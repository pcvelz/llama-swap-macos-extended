import XCTest
@testable import LlamaSwapMenuCore

/// User report 2026-09-08 (fourth round): with one parent session and two
/// haiku subagents in flight, every row in the menu printed the SAME token
/// total and rate - "97.6k · 36.9 t/s" on the PREFILL row and on both
/// PARKED rows. Live /slots at that instant: slot 0 processing at
/// n_prompt_tokens 97613, slot 1 idle. The parked rows were showing slot 0's
/// counters, not their own (they have none - a parked request holds no slot).
///
/// Root cause: BackendClient.joinedSlot joins a row to a slot by
/// metadata.slot_affinity FIRST, and the proxy stamps slot_affinity on every
/// request at admission time, parked or granted. All requests of a lane (and,
/// today, all requests full stop) carry affinity 0, so a parked row joins the
/// busy slot and borrows its readout. The row classifier already knows the
/// request is parked (kv_parked == "1", or no slot_granted); the slot join
/// never consulted those flags.
///
/// Contract: a row whose request is parked (kv_parked == "1") or not yet
/// granted (slot_granted != "1") renders NO slot readout - it is not on a
/// slot, so there is nothing truthful to print. Only the granted row shows
/// the slot's total and rate.
final class ParkedRowSlotJoinTests: XCTestCase {

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

    /// Three requests, all slot_affinity "0" exactly as the proxy stamps them
    /// (captured 2026-09-08 19:04, ids 925/926/927): one granted, two parked
    /// (one by kv-admission, one never granted). /slots: slot 0 processing at
    /// 97613 prompt tokens, slot 1 idle at a stale 65047.
    func testParkedRowsDoNotBorrowTheGrantedSlotsReadout() {
        stub.responder = { _, path in
            guard path.hasSuffix("/slots") else { return (200, "{}") }
            return (200, """
            [{"id":0,"is_processing":true,"n_prompt_tokens":97613,\
            "n_prompt_tokens_processed":97613,"next_token":[{"n_decoded":0}]},\
            {"id":1,"is_processing":false,"n_prompt_tokens":65047,\
            "n_prompt_tokens_processed":65047,"next_token":[{"n_decoded":0}]}]
            """)
        }

        let client = makeClient()
        // Three distinct sessions so each request is its own lane and its
        // own row (one-row-per-lane would otherwise fold them).
        func req(_ id: String, _ session: String, _ meta: String) -> String {
            """
            {"id":"\(id)","timestamp":"2026-09-08T17:04:00Z",\
            "model":"Qwen3.6-35B-A3B-APEX-I-Balanced-384K",\
            "req_path":"/v1/messages","method":"POST",\
            "req_headers":{"User-Agent":"claude-cli/2.1.263"},"remote_ip":"127.0.0.1",\
            "resp_headers":{},"resp_bytes":0,"elapsed_ms":6000,\
            "metadata":{"session_id":"\(session)","model_alias":"cq35h",\
            "slot_affinity":"0",\(meta)}}
            """
        }
        let inner = """
        {"total":3,"granted":1,"operation":"snapshot","requests":[\
        \(req("925", "aaaaaaaa-0000-4000-8000-000000000001", "\"slot_granted\":\"1\"")),\
        \(req("926", "bbbbbbbb-0000-4000-8000-000000000002", "\"kv_parked\":\"1\"")),\
        \(req("927", "cccccccc-0000-4000-8000-000000000003", "\"kv_parked\":\"1\""))]}
        """
        stub.pushEvent(type: "inflight", inner: inner)

        XCTAssertTrue(waitUntil(6) { client.menuState.sessionRows.count == 3 },
                      "expected three rows, got \(client.menuState.sessionRows.count)")
        // The granted row must join slot 0 and show its total.
        XCTAssertTrue(waitUntil(6) {
            client.menuState.sessionRows.first(where: { $0.id == "925" })?.detail?.contains("97.6k") == true
        }, "granted row never showed slot 0's total, got \(client.menuState.sessionRows.map { "\($0.id)=\($0.detail ?? "<nil>")" })")

        // Let a couple more polls land so any wrong join has had every chance
        // to print.
        RunLoop.main.run(until: Date().addingTimeInterval(0.5))
        for id in ["926", "927"] {
            let row = client.menuState.sessionRows.first(where: { $0.id == id })
            XCTAssertEqual(row?.word, ThroughputWord.parked.rawValue, "row \(id) must classify PARKED")
            XCTAssertNil(row?.detail,
                         "parked row \(id) holds no slot and must print no readout, got '\(row?.detail ?? "<nil>")'")
        }
    }

    /// User report 2026-09-08 19:25, on the build with the guard above: a
    /// row reading `PARKED · 109.6k · 37.5 t/s`. The lane had just been
    /// granted (readout real), then its next request arrived parked. The
    /// parked entry gets no slot join, but the lane's last-good READOUT HOLD
    /// (added for the flicker fix) still carried the old numbers for its 5 s
    /// window. Turns rotate every 6-12 s, so a parked row showed a stale
    /// rate about half the time. Contract: a PARKED entry drops the lane's
    /// hold at once - stale is allowed across a failed join, never across a
    /// known park.
    func testLaneGoingParkedDropsTheHeldReadoutImmediately() {
        stub.responder = { _, path in
            guard path.hasSuffix("/slots") else { return (200, "{}") }
            return (200, """
            [{"id":0,"is_processing":true,"n_prompt_tokens":109600,\
            "n_prompt_tokens_processed":109600,"next_token":[{"n_decoded":40}]}]
            """)
        }
        let client = makeClient()
        func req(_ id: String, _ meta: String) -> String {
            """
            {"id":"\(id)","timestamp":"2026-09-08T17:25:00Z",\
            "model":"Qwen3.6-35B-A3B-APEX-I-Balanced-384K",\
            "req_path":"/v1/messages","method":"POST",\
            "req_headers":{"User-Agent":"claude-cli/2.1.263"},"remote_ip":"127.0.0.1",\
            "resp_headers":{},"resp_bytes":0,"elapsed_ms":3000,\
            "metadata":{"session_id":"a1728760-0000-4000-8000-000000000001",\
            "agent_id":"af5219ebebc84f896","model_alias":"cq35h","slot_affinity":"0",\(meta)}}
            """
        }
        // Granted request: the lane earns a real readout.
        stub.pushEvent(type: "inflight", inner: """
        {"total":1,"granted":1,"operation":"snapshot","requests":[\(req("940", "\"slot_granted\":\"1\""))]}
        """)
        XCTAssertTrue(waitUntil(6) {
            client.menuState.sessionRows.first?.detail?.contains("109.6k") == true
        }, "granted row never showed its readout, got \(client.menuState.sessionRows.first?.detail ?? "<nil>")")

        // Same lane, next request, parked. The row must go bare at once.
        stub.pushEvent(type: "inflight", inner: """
        {"total":1,"granted":0,"operation":"snapshot","requests":[\(req("941", "\"kv_parked\":\"1\""))]}
        """)
        XCTAssertTrue(waitUntil(6) { client.menuState.sessionRows.first?.id == "941" })
        RunLoop.main.run(until: Date().addingTimeInterval(0.3))
        let row = client.menuState.sessionRows.first
        XCTAssertEqual(row?.word, ThroughputWord.parked.rawValue)
        XCTAssertNil(row?.detail, "a parked lane must not keep showing its previous request's readout, got '\(row?.detail ?? "<nil>")'")
    }
}
