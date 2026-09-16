import XCTest
@testable import LlamaSwapMenuCore

/// Pins the "0.0 t/s next to a rising token total" bug (user report
/// 2026-09-08): a session row showed `... · DECODE · 15.5k · 0.0 t/s` while
/// the token total kept climbing between ticks. Root cause: BackendClient's
/// slotDetail(for:word:now:) picks which /slots counter to sample (decoded
/// vs prompt-processed) purely from the byte-heuristic `word`
/// (SessionThroughput.classify), not from what the slot itself is actually
/// doing. A keepalive byte on the Anthropic path can flip the heuristic word
/// to DECODE while the slot is still PREFILLING (n_prompt_tokens climbing,
/// n_decoded pinned at 0) - the rate metric then samples the flat counter
/// (decoded) while the total is built from the climbing one (prompt tokens),
/// producing a literal 0.0 next to a rising total.
final class SlotRateReadoutTests: XCTestCase {

    private var stub: StubBackend!

    override func setUpWithError() throws {
        stub = try StubBackend()
    }

    override func tearDown() {
        stub.stop()
        stub = nil
    }

    @discardableResult
    private func waitUntil(_ timeout: TimeInterval = 8, _ cond: () -> Bool) -> Bool {
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if cond() { return true }
            RunLoop.main.run(until: Date().addingTimeInterval(0.02))
        }
        return cond()
    }

    private func makeClient() -> BackendClient {
        let client = BackendClient(baseURL: stub.baseURL)
        XCTAssertTrue(waitUntil { self.stub.hasEventClient },
                      "client never opened the /api/events stream")
        return client
    }

    /// Two /slots polls, 2s apart: n_prompt_tokens climbs by 400 tokens
    /// (still prefilling), n_decoded stays 0 throughout. The row's
    /// byte-heuristic word is DECODE (a keepalive byte read as motion), so
    /// the buggy code samples n_decoded (flat) instead of the counter that is
    /// actually moving, and prints "0.0 t/s" while the displayed total rises.
    func testRisingPromptTokensWhileWordSaysDecodeDoesNotPrintZeroRate() {
        let lock = NSLock()
        var slotPollCount = 0
        stub.responder = { _, path in
            guard path.hasSuffix("/slots") else { return (200, "{}") }
            lock.lock()
            slotPollCount += 1
            let n = slotPollCount
            lock.unlock()
            // First poll: prompt at 15000. Second+ poll: prompt at 15400,
            // 400 tokens further along, decoded still 0 throughout.
            let prompt = n <= 1 ? 15000 : 15400
            return (200, """
            {"models":[{"model":"cq35","state":"ready","slots":[\
            {"id":0,"is_processing":true,"n_prompt_tokens":\(prompt),\
            "n_prompt_tokens_processed":\(prompt),"n_decoded":0}]}]}
            """)
        }

        let client = makeClient()
        // resp_bytes > 0 with no previous sample classifies as DECODE (the
        // ambiguous first-observation case in SessionThroughput.classify) -
        // exactly the keepalive-read-as-motion path from the bug report.
        let inner = """
        {"total":1,"granted":1,"operation":"snapshot",\
        "requests":[{"id":"90","timestamp":"2026-09-08T10:00:00Z","model":"cq35",\
        "req_path":"/v1/messages","method":"POST",\
        "req_headers":{"User-Agent":"claude-code/1.0"},"remote_ip":"127.0.0.1",\
        "resp_headers":{},"resp_bytes":40,"elapsed_ms":500,\
        "metadata":{"session_id":"3bf85c8c-1234-4321-aaaa-bbbbccccdddd","slot_granted":"1"}}]}
        """
        stub.pushEvent(type: "inflight", inner: inner)

        XCTAssertTrue(waitUntil { client.menuState.sessionRows.first?.word == "DECODE" },
                      "expected DECODE from the byte heuristic, got \(client.menuState.sessionRows.first?.word ?? "<none>")")

        // Wait for the first slot sample to seed (no rate on the very first
        // sample), then for the second poll's detail to land.
        XCTAssertTrue(waitUntil { self.pollCount(lock, { slotPollCount }) >= 1 })
        XCTAssertTrue(waitUntil(6) { self.pollCount(lock, { slotPollCount }) >= 2 },
                      "expected a second /slots poll within the wait window")

        XCTAssertTrue(waitUntil(6) {
            client.menuState.sessionRows.first?.detail?.contains("15.4k") == true
        }, "expected the total to reflect the second poll's 15400 tokens, got \(client.menuState.sessionRows.first?.detail ?? "<none>")")

        let detail = client.menuState.sessionRows.first?.detail ?? "<none>"
        XCTAssertFalse(detail.contains("0.0 t/s"),
                        "the total rose by 400 tokens between polls - the rate must not read 0.0 t/s; got '\(detail)'")
    }

    private func pollCount(_ lock: NSLock, _ read: () -> Int) -> Int {
        lock.lock(); defer { lock.unlock() }
        return read()
    }

    /// User report 2026-09-08: "if token/sec is not there for a few 100ms,
    /// it's flickering gone, then back" during a real, live decode. A single
    /// /slots poll that lands on a quiet instant (n_decoded happens not to
    /// have advanced between two 2s polls, e.g. between-token scheduling
    /// jitter) used to drop the rate to a bare total for that tick, then
    /// bring it back the next - a visible flicker for a rate the user could
    /// see was genuinely still live. The rate must instead keep showing the
    /// last real number for a few seconds of quiet before it is dropped.
    func testBriefQuietTickKeepsShowingTheLastRealRateInsteadOfFlickering() {
        let lock = NSLock()
        var slotPollCount = 0
        stub.responder = { _, path in
            guard path.hasSuffix("/slots") else { return (200, "{}") }
            lock.lock()
            slotPollCount += 1
            let n = slotPollCount
            lock.unlock()
            // Poll 1: 100 decoded. Poll 2: 300 (a real rate appears). Poll 3
            // and later: still 300 - one quiet tick in a live decode.
            let decoded = n <= 1 ? 100 : 300
            return (200, """
            {"models":[{"model":"cq35","state":"ready","slots":[\
            {"id":0,"is_processing":true,"n_prompt_tokens":15000,\
            "n_prompt_tokens_processed":15000,"n_decoded":\(decoded)}]}]}
            """)
        }

        let client = makeClient()
        let inner = """
        {"total":1,"granted":1,"operation":"snapshot",\
        "requests":[{"id":"91","timestamp":"2026-09-08T10:00:00Z","model":"cq35",\
        "req_path":"/v1/messages","method":"POST",\
        "req_headers":{"User-Agent":"claude-code/1.0"},"remote_ip":"127.0.0.1",\
        "resp_headers":{},"resp_bytes":40,"elapsed_ms":500,\
        "metadata":{"session_id":"3bf85c8c-1234-4321-aaaa-bbbbccccdddd","slot_granted":"1"}}]}
        """
        stub.pushEvent(type: "inflight", inner: inner)

        XCTAssertTrue(waitUntil(6) { self.pollCount(lock, { slotPollCount }) >= 2 })
        XCTAssertTrue(waitUntil(6) {
            client.menuState.sessionRows.first?.detail?.contains("t/s") == true
        }, "expected a rate after the second poll, got \(client.menuState.sessionRows.first?.detail ?? "<none>")")

        // The quiet third poll must not drop the rate.
        XCTAssertTrue(waitUntil(6) { self.pollCount(lock, { slotPollCount }) >= 3 })
        RunLoop.main.run(until: Date().addingTimeInterval(0.3))
        let detail = client.menuState.sessionRows.first?.detail ?? "<none>"
        XCTAssertTrue(detail.contains("t/s"),
                      "one quiet /slots tick must keep the last real rate, got '\(detail)'")
    }
}
