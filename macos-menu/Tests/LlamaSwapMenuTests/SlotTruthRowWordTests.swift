import XCTest
@testable import LlamaSwapMenuCore

/// User report 2026-09-16 (screenshot, two cq35h sessions on a --parallel 2
/// box): "[399b4775] cq35h · TURN · 106.6k · 20.0 t/s" above
/// "[e221c13d] cq35h · DECODE · 67.2k" - the row that was NOT on a slot
/// carried a rate, the row that WAS decoding carried none, and the user saw a
/// session shown as parked/between turns while its llama.cpp slot was
/// generating. Live capture the same minute (/slots at 2s + /api/events,
/// ~/.claude/logs/evidence-2026-09-16-turn-row-serialized-lanes/): each
/// session holds a stable slot_affinity (399b4775 -> 0, e221c13d -> 1), and
/// the proxy's per-request flags lag the slot around every hand-off (a lane's
/// next request arrives kv-parked while its previous one is still on the
/// slot; a grant is stamped slot_granted=1 one upsert before kv_parked is
/// cleared).
///
/// Contract: the llama.cpp slot is the truth for "is this session active".
/// - A lane whose own slot (slot_affinity) is processing, and which holds a
///   granted request, renders an active word (DECODE/PREFILL) with its live
///   readout - never PARKED - even if its NEWEST request is still flagged
///   parked.
/// - Two lanes each decoding on their own slot both render active.
/// - A lane lingering between turns (TURN) is on no slot, so it shows no
///   rate: a stale "t/s" there reads as generation that is not happening.
final class SlotTruthRowWordTests: XCTestCase {

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

    private func makeClient(laneLingerSeconds: TimeInterval = BackendClient.defaultLaneLingerSeconds) -> BackendClient {
        let client = BackendClient(baseURL: stub.baseURL, laneLingerSeconds: laneLingerSeconds)
        XCTAssertTrue(waitUntil { self.stub.hasEventClient },
                      "client never opened the /api/events stream")
        return client
    }

    /// /slots whose `decoding` slots advance n_decoded on every poll, so the
    /// client measures a real rate; other slots are idle.
    private func serveSlots(decoding: Set<Int>) {
        let lock = NSLock()
        var polls = 0
        stub.responder = { _, path in
            guard path.hasSuffix("/slots") else { return (200, "{}") }
            lock.lock(); polls += 1; let n = polls * 40; lock.unlock()
            let slots = [0, 1].map { id -> String in
                let busy = decoding.contains(id)
                return """
                {"id":\(id),"is_processing":\(busy),"n_prompt_tokens":\(id == 0 ? 106600 : 67200),\
                "n_prompt_tokens_processed":\(busy ? 150 : 0),"n_decoded":\(busy ? n : 0)}
                """
            }
            return (200, """
            {"models":[{"model":"Qwen3.6-35B-A3B-APEX-I-Balanced-384K","state":"ready",\
            "slots":[\(slots.joined(separator: ","))]}]}
            """)
        }
    }

    private func req(_ id: String, session: String, affinity: String, _ meta: String) -> String {
        """
        {"id":"\(id)","timestamp":"2026-09-16T17:03:00Z",\
        "model":"Qwen3.6-35B-A3B-APEX-I-Balanced-384K",\
        "req_path":"/v1/messages","method":"POST",\
        "req_headers":{"User-Agent":"claude-cli/2.1.273"},"remote_ip":"127.0.0.1",\
        "resp_headers":{},"resp_bytes":0,"elapsed_ms":4000,\
        "metadata":{"session_id":"\(session)","model_alias":"cq35h",\
        "slot_affinity":"\(affinity)",\(meta)}}
        """
    }

    private let sessionA = "399b4775-a452-4b0b-b003-197d2dab155b"
    private let sessionB = "e221c13d-1604-400d-b656-13a836f3131e"

    private func rowDump(_ client: BackendClient) -> String {
        client.menuState.sessionRows.map { "\($0.origin)=\($0.word)|\($0.detail ?? "<nil>")" }.joined(separator: ", ")
    }

    /// Turn boundary: the lane's previous request (granted) is still on slot 0
    /// while its next request has already arrived kv-parked. The newest
    /// request is what the row shows - it must not read PARKED while the
    /// lane's own slot is generating.
    func testLaneWhoseSlotIsDecodingIsNotShownParked() {
        serveSlots(decoding: [0])
        let client = makeClient()
        stub.pushEvent(type: "inflight", inner: """
        {"total":2,"operation":"snapshot","requests":[\
        \(req("600", session: sessionA, affinity: "0", "\"slot_granted\":\"1\"")),\
        \(req("601", session: sessionA, affinity: "0", "\"kv_parked\":\"1\",\"park_reason\":\"kv\""))]}
        """)
        let ok = waitUntil {
            let row = client.menuState.sessionRows.first
            return row?.word == ThroughputWord.decode.rawValue && row?.detail?.contains("t/s") == true
        }
        XCTAssertTrue(ok, "lane decoding on its own slot must read DECODE with a rate, got \(rowDump(client))")
    }

    /// The grant is stamped slot_granted=1 one upsert BEFORE kv_parked is
    /// cleared (internal/router/base.go). A request that holds its slot and is
    /// decoding is active, whatever the stale flag says.
    func testGrantedRequestStillFlaggedKvParkedShowsActive() {
        serveSlots(decoding: [1])
        let client = makeClient()
        stub.pushEvent(type: "inflight", inner: """
        {"total":1,"operation":"snapshot","requests":[\
        \(req("603", session: sessionB, affinity: "1", "\"kv_parked\":\"1\",\"slot_granted\":\"1\""))]}
        """)
        let ok = waitUntil {
            let row = client.menuState.sessionRows.first
            return row?.word == ThroughputWord.decode.rawValue && row?.detail?.contains("t/s") == true
        }
        XCTAssertTrue(ok, "granted request decoding on its slot must read DECODE with a rate, got \(rowDump(client))")
    }

    /// --parallel 2, both sessions on their own slot, both decoding: both
    /// rows active, each with its own rate.
    func testTwoLanesDecodingOnTheirOwnSlotsBothShowActive() {
        serveSlots(decoding: [0, 1])
        let client = makeClient()
        stub.pushEvent(type: "inflight", inner: """
        {"total":2,"operation":"snapshot","requests":[\
        \(req("610", session: sessionA, affinity: "0", "\"slot_granted\":\"1\"")),\
        \(req("611", session: sessionB, affinity: "1", "\"slot_granted\":\"1\""))]}
        """)
        let ok = waitUntil {
            let rows = client.menuState.sessionRows
            return rows.count == 2 && rows.allSatisfy {
                $0.word == ThroughputWord.decode.rawValue && $0.detail?.contains("t/s") == true
            }
        }
        XCTAssertTrue(ok, "both decoding lanes must read DECODE with a rate, got \(rowDump(client))")
        // Each row carries its OWN slot's total (n_prompt_tokens + n_decoded,
        // so the exact decimal moves with the poll count: compare prefixes).
        let totals = client.menuState.sessionRows.map { $0.detail?.components(separatedBy: " · ").first ?? "" }
        XCTAssertTrue(totals[0].hasPrefix("10") && totals[1].hasPrefix("6"),
                      "each row must carry its own slot's total, got \(rowDump(client))")
    }

    /// The screenshot's TURN row: its request is gone, it holds no slot, so
    /// it must not keep printing the rate it had while it was decoding.
    func testLingeringTurnRowShowsNoRate() {
        serveSlots(decoding: [0])
        let client = makeClient()
        stub.pushEvent(type: "inflight", inner: """
        {"total":1,"operation":"snapshot","requests":[\
        \(req("620", session: sessionA, affinity: "0", "\"slot_granted\":\"1\""))]}
        """)
        XCTAssertTrue(waitUntil { client.menuState.sessionRows.first?.detail?.contains("t/s") == true },
                      "granted row never earned a rate, got \(rowDump(client))")
        // The live proxy upserts the entry as bytes stream, so the lane's
        // remembered row (what it lingers WITH) is recorded while it carries
        // the rate. Without this event the test never reaches the live shape.
        stub.pushEvent(type: "inflight", inner: """
        {"total":1,"operation":"upsert","request":\
        \(req("620", session: sessionA, affinity: "0", "\"slot_granted\":\"1\""))}
        """)
        RunLoop.main.run(until: Date().addingTimeInterval(0.3))
        serveSlots(decoding: [])
        stub.pushEvent(type: "inflight", inner: """
        {"total":0,"operation":"remove","id":"620"}
        """)
        XCTAssertTrue(waitUntil { client.menuState.sessionRows.first?.word == ThroughputWord.turn.rawValue },
                      "lane never lingered as TURN, got \(rowDump(client))")
        let detail = client.menuState.sessionRows.first?.detail ?? ""
        XCTAssertFalse(detail.contains("t/s"), "a TURN row is on no slot and must show no rate, got '\(detail)'")
    }
}
