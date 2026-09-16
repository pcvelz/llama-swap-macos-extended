import XCTest
@testable import LlamaSwapMenuCore

/// User report 2026-09-16 20:40 (master-template / master-template-2, two
/// cq35h sessions): the rows flip PREFILL / DECODE / TURN "like an arcade
/// machine" while the model itself runs without a hiccup. Live observation of
/// the real BackendClient against :8001 (LiveRowWordObserverTests, 95 s) next
/// to /slots at 1 Hz: session 5cee4df5 ran a healthy tool loop - one request
/// per turn, 100-800 prompt tokens prefilled in under a second, 25-45 t/s
/// decode for 2-20 s, a 1-3 s tool gap - and its row changed word on every
/// one of those boundaries:
///   DECODE | 26.2k -> PREFILL | 26.2k -> TURN | 26.2k -> PREFILL | 26.2k ->
///   DECODE | 26.2k -> DECODE | 26.4k · 27.1 t/s -> TURN | 26.6k -> DECODE ...
/// Every word was momentarily true; the row as a whole was noise, and every
/// status consumer re-alerts on each flip.
///
/// Contract: between the three "active" words (PREFILL, DECODE, TURN) a row
/// only changes word once the new word has held for rowWordHoldSeconds. A
/// sub-second prefill at a turn boundary and a 1-3 s tool gap never reach the
/// screen; a real long prefill or a real idle lane still does, a hold later.
/// The rate readout is held the same way, so a new request's bare total does
/// not blank a steady "· 25 t/s". PARKED / FLAT and the rest are not held:
/// they surface at once.
final class RowWordSteadinessTests: XCTestCase {

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

    private func serveDecodingSlot0() {
        let lock = NSLock()
        var polls = 0
        stub.responder = { _, path in
            guard path.hasSuffix("/slots") else { return (200, "{}") }
            lock.lock(); polls += 1; let n = polls * 40; lock.unlock()
            return (200, """
            {"models":[{"model":"Qwen3.6-35B-A3B-APEX-I-Balanced-384K","state":"ready","slots":[\
            {"id":0,"is_processing":true,"n_prompt_tokens":26200,"n_prompt_tokens_processed":64,"n_decoded":\(n)},\
            {"id":1,"is_processing":false,"n_prompt_tokens":0,"n_prompt_tokens_processed":0,"n_decoded":0}]}]}
            """)
        }
    }

    private func req(_ id: String) -> String {
        """
        {"id":"\(id)","timestamp":"2026-09-16T18:40:00Z",\
        "model":"Qwen3.6-35B-A3B-APEX-I-Balanced-384K",\
        "req_path":"/v1/messages","method":"POST",\
        "req_headers":{"User-Agent":"claude-cli/2.1.273"},"remote_ip":"127.0.0.1",\
        "resp_headers":{},"resp_bytes":1800,"elapsed_ms":4000,\
        "metadata":{"session_id":"5cee4df5-fe96-4c23-9ddb-3996cfb75ecc","model_alias":"cq35h",\
        "slot_affinity":"0","slot_granted":"1"}}
        """
    }

    /// A 1-3 s tool gap between two turns: the row stays DECODE with its rate.
    func testToolGapShorterThanTheHoldKeepsTheRowSteady() {
        serveDecodingSlot0()
        let client = BackendClient(baseURL: stub.baseURL)
        XCTAssertTrue(waitUntil { self.stub.hasEventClient })
        stub.pushEvent(type: "inflight", inner: """
        {"total":1,"operation":"snapshot","requests":[\(req("42"))]}
        """)
        stub.pushEvent(type: "inflight", inner: """
        {"total":1,"operation":"upsert","request":\(req("42"))}
        """)
        XCTAssertTrue(waitUntil {
            let row = client.menuState.sessionRows.first
            return row?.word == ThroughputWord.decode.rawValue && row?.detail?.contains("t/s") == true
        }, "lane never showed DECODE with a rate")
        stub.pushEvent(type: "inflight", inner: """
        {"total":1,"operation":"upsert","request":\(req("42"))}
        """)
        RunLoop.main.run(until: Date().addingTimeInterval(0.3))

        // Turn ends; the next request arrives 2 s later, as in the live tool loop.
        stub.pushEvent(type: "inflight", inner: """
        {"total":0,"operation":"remove","id":"42"}
        """)
        var seen: [String] = []
        let gapEnd = Date().addingTimeInterval(2.0)
        while Date() < gapEnd {
            RunLoop.main.run(until: Date().addingTimeInterval(0.05))
            if let row = client.menuState.sessionRows.first { seen.append("\(row.word)|\(row.detail ?? "-")") }
        }
        stub.pushEvent(type: "inflight", inner: """
        {"total":1,"operation":"snapshot","requests":[\(req("43"))]}
        """)
        let after = Date().addingTimeInterval(1.5)
        while Date() < after {
            RunLoop.main.run(until: Date().addingTimeInterval(0.05))
            if let row = client.menuState.sessionRows.first { seen.append("\(row.word)|\(row.detail ?? "-")") }
        }
        let flips = seen.filter { !$0.hasPrefix(ThroughputWord.decode.rawValue + "|") }
        XCTAssertTrue(flips.isEmpty, "a 2 s tool gap must not change the row's word, saw \(Array(Set(flips)))")
        let bare = seen.filter { !$0.contains("t/s") }
        XCTAssertTrue(bare.isEmpty, "a 2 s tool gap must not blank the rate, saw \(Array(Set(bare)))")
    }

    /// A lane that really went quiet still reads TURN - one hold later.
    func testRealIdleStillSurfacesAfterTheHold() {
        serveDecodingSlot0()
        let client = BackendClient(baseURL: stub.baseURL, rowWordHoldSeconds: 1.0)
        XCTAssertTrue(waitUntil { self.stub.hasEventClient })
        stub.pushEvent(type: "inflight", inner: """
        {"total":1,"operation":"snapshot","requests":[\(req("50"))]}
        """)
        XCTAssertTrue(waitUntil { client.menuState.sessionRows.first?.word == ThroughputWord.decode.rawValue })
        stub.pushEvent(type: "inflight", inner: """
        {"total":0,"operation":"remove","id":"50"}
        """)
        XCTAssertTrue(waitUntil(5) { client.menuState.sessionRows.first?.word == ThroughputWord.turn.rawValue },
                      "an idle lane must still read TURN once the hold has passed, got \(client.menuState.sessionRows.map { $0.word })")
    }

    /// Pure: a sub-hold PREFILL flash at a turn boundary never reaches the row.
    func testStabilizerHidesAShortPrefillFlash() {
        let s = RowWordStabilizer(holdSeconds: 5)
        let t0 = Date(timeIntervalSince1970: 1_000)
        func row(_ word: String, _ detail: String?) -> SessionRow {
            SessionRow(id: "1", origin: "5cee4df5", model: "cq35h", tier: "-", word: word, detail: detail, hasSession: true)
        }
        XCTAssertEqual(s.apply([row("DECODE", "26.4k · 27.1 t/s")], now: t0).first?.word, "DECODE")
        let flash = s.apply([row("PREFILL", "26.4k")], now: t0.addingTimeInterval(0.6)).first
        XCTAssertEqual(flash?.word, "DECODE")
        XCTAssertEqual(flash?.detail, "26.4k · 27.1 t/s")
        XCTAssertEqual(s.apply([row("DECODE", "26.5k")], now: t0.addingTimeInterval(1.2)).first?.word, "DECODE")
        // A PREFILL that persists past the hold is real and shows.
        _ = s.apply([row("PREFILL", "40.0k")], now: t0.addingTimeInterval(2))
        XCTAssertEqual(s.apply([row("PREFILL", "42.0k")], now: t0.addingTimeInterval(7.5)).first?.word, "PREFILL")
        // PARKED is never held.
        XCTAssertEqual(s.apply([row("PARKED", "kv pool")], now: t0.addingTimeInterval(7.6)).first?.word, "PARKED")
    }
}
