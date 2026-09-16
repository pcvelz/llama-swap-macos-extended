import XCTest
@testable import LlamaSwapMenuCore

/// User report 2026-09-08 (second round, after the rate hold landed): BOTH
/// the token total ("85.6k") and the rate ("t/s") still flicker off and back
/// on together. The rate hold only runs once a /slots row was JOINED to the
/// session's row; when the join itself fails the whole readout is dropped
/// in one go. Two live producers of a failed join:
///
///  1. The fallback join (no slot_affinity / slot_id metadata) requires
///     exactly ONE processing slot. Every small background request on the
///     other slot (pii-detect curls run every few minutes, ~3s each) makes
///     it two, the join returns nil, and the row loses its readout until
///     that request finishes.
///  2. At a turn boundary the inflight set is empty for an instant, the slot
///     cache is wiped, and the next turn's first poll has to complete (5-13s
///     against a prefilling child, measured in the proxy log) before the
///     readout comes back.
///
/// Contract: a row keeps showing its last good readout for a few seconds of
/// failed joins - stale is allowed, blank is not - and only after the hold
/// window does the readout disappear.
final class SlotReadoutHoldTests: XCTestCase {

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

    private func pollCount(_ lock: NSLock, _ read: () -> Int) -> Int {
        lock.lock(); defer { lock.unlock() }
        return read()
    }

    private func makeClient() -> BackendClient {
        let client = BackendClient(baseURL: stub.baseURL)
        XCTAssertTrue(waitUntil { self.stub.hasEventClient },
                      "client never opened the /api/events stream")
        return client
    }

    /// Polls 1-2: one processing slot, decoded 100 -> 300, so the row shows
    /// "15.3k · N t/s". Poll 3+: a second slot starts processing (the
    /// background curl), the fallback join can no longer pick a slot. The
    /// readout must survive that tick unchanged.
    func testSecondProcessingSlotKeepsLastReadoutInsteadOfBlanking() {
        let lock = NSLock()
        var slotPollCount = 0
        stub.responder = { _, path in
            guard path.hasSuffix("/slots") else { return (200, "{}") }
            lock.lock()
            slotPollCount += 1
            let n = slotPollCount
            lock.unlock()
            let decoded = n <= 1 ? 100 : 300
            let session = """
            {"id":1,"is_processing":true,"n_prompt_tokens":15000,\
            "n_prompt_tokens_processed":15000,"n_decoded":\(decoded)}
            """
            let peer = """
            {"id":0,"is_processing":true,"n_prompt_tokens":700,\
            "n_prompt_tokens_processed":700,"n_decoded":5}
            """
            let slots = n <= 2 ? "[\(session)]" : "[\(session),\(peer)]"
            return (200, """
            {"models":[{"model":"cq35","state":"ready","slots":\(slots)}]}
            """)
        }

        let client = makeClient()
        let inner = """
        {"total":1,"granted":1,"operation":"snapshot",\
        "requests":[{"id":"92","timestamp":"2026-09-08T10:00:00Z","model":"cq35",\
        "req_path":"/v1/messages","method":"POST",\
        "req_headers":{"User-Agent":"claude-code/1.0"},"remote_ip":"127.0.0.1",\
        "resp_headers":{},"resp_bytes":40,"elapsed_ms":500,\
        "metadata":{"session_id":"3bf85c8c-1234-4321-aaaa-bbbbccccdddd","slot_granted":"1"}}]}
        """
        stub.pushEvent(type: "inflight", inner: inner)

        XCTAssertTrue(waitUntil(6) { self.pollCount(lock, { slotPollCount }) >= 2 })
        XCTAssertTrue(waitUntil(6) {
            client.menuState.sessionRows.first?.detail?.contains("t/s") == true
        }, "expected a full readout after the second poll, got \(client.menuState.sessionRows.first?.detail ?? "<none>")")
        let good = client.menuState.sessionRows.first?.detail ?? "<none>"

        // Third poll: two processing slots, the join fails.
        XCTAssertTrue(waitUntil(6) { self.pollCount(lock, { slotPollCount }) >= 3 })
        RunLoop.main.run(until: Date().addingTimeInterval(0.3))
        let detail = client.menuState.sessionRows.first?.detail ?? "<none>"
        XCTAssertEqual(detail, good,
                       "a failed slot join within the hold window must keep the last readout, got '\(detail)'")
        XCTAssertTrue(detail.contains("15.3k"), "the token total must not blank, got '\(detail)'")
    }

    /// User report 2026-09-08 (third round): "**k now seems stable, but
    /// token/sec is still flickering". Measured on the live box: during
    /// prefill llama-server advances n_prompt_tokens_processed once per
    /// ubatch (2048 tokens), about every 10s, while the menu polls every 2s.
    /// A fresh rate therefore appears every ~5 polls; the 5s rate hold
    /// expired in between, blanking the rate for the last ~3 polls of every
    /// step. And the rate printed AT the step was 2048 over the 2s poll gap,
    /// ~1000 t/s, four times the real throughput.
    ///
    /// Shape: P advances by 2048 at poll 2 and again at poll 6 (8s later),
    /// flat in between. Contract: the rate stays on screen through polls
    /// 3-5 (is_processing is true throughout), and the rate printed at poll
    /// 6 is 2048 tokens over the 8s since the previous step, not over one
    /// 2s poll gap.
    func testPrefillStepCadenceKeepsRateAndMeasuresItOverTheStep() {
        let lock = NSLock()
        var slotPollCount = 0
        stub.responder = { _, path in
            guard path.hasSuffix("/slots") else { return (200, "{}") }
            lock.lock()
            slotPollCount += 1
            let n = slotPollCount
            lock.unlock()
            let processed = n <= 1 ? 1000 : (n <= 5 ? 3048 : 5096)
            return (200, """
            {"models":[{"model":"cq35","state":"ready","slots":[\
            {"id":0,"is_processing":true,"n_prompt_tokens":20000,\
            "n_prompt_tokens_processed":\(processed),"n_decoded":0}]}]}
            """)
        }

        let client = makeClient()
        let inner = """
        {"total":1,"granted":1,"operation":"snapshot",\
        "requests":[{"id":"93","timestamp":"2026-09-08T10:00:00Z","model":"cq35",\
        "req_path":"/v1/messages","method":"POST",\
        "req_headers":{"User-Agent":"claude-code/1.0"},"remote_ip":"127.0.0.1",\
        "resp_headers":{},"resp_bytes":40,"elapsed_ms":500,\
        "metadata":{"session_id":"3bf85c8c-1234-4321-aaaa-bbbbccccdddd","slot_granted":"1"}}]}
        """
        stub.pushEvent(type: "inflight", inner: inner)

        XCTAssertTrue(waitUntil(6) { self.pollCount(lock, { slotPollCount }) >= 2 })
        XCTAssertTrue(waitUntil(6) {
            client.menuState.sessionRows.first?.detail?.contains("t/s") == true
        }, "expected a rate at the first step, got \(client.menuState.sessionRows.first?.detail ?? "<none>")")

        // Poll 5 lands ~6s after the step at poll 2: past the old 5s hold.
        XCTAssertTrue(waitUntil(12) { self.pollCount(lock, { slotPollCount }) >= 5 })
        RunLoop.main.run(until: Date().addingTimeInterval(0.3))
        let midStep = client.menuState.sessionRows.first?.detail ?? "<none>"
        XCTAssertTrue(midStep.contains("t/s"),
                      "the rate must stay on screen between prefill steps while is_processing, got '\(midStep)'")

        // Poll 6: the next step. 2048 tokens over the ~8s since poll 2.
        XCTAssertTrue(waitUntil(6) { self.pollCount(lock, { slotPollCount }) >= 6 })
        RunLoop.main.run(until: Date().addingTimeInterval(0.3))
        let atStep = client.menuState.sessionRows.first?.detail ?? "<none>"
        let rate = Self.rateValue(in: atStep)
        XCTAssertNotNil(rate, "expected a rate at the second step, got '\(atStep)'")
        if let rate {
            XCTAssertLessThan(rate, 500,
                              "rate must be measured over the whole step (~2048/8s = 256 t/s), not the 2s poll gap (~1024); got '\(atStep)'")
            XCTAssertGreaterThan(rate, 150, "rate implausibly low for 2048 tokens over ~8s; got '\(atStep)'")
        }
    }

    private static func rateValue(in detail: String) -> Double? {
        guard let range = detail.range(of: " t/s") else { return nil }
        let head = detail[..<range.lowerBound]
        guard let last = head.split(separator: " ").last else { return nil }
        return Double(last)
    }
}
