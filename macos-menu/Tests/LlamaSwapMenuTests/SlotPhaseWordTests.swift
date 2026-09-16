import XCTest
@testable import LlamaSwapMenuCore

/// User report 2026-09-08 19:40: a row read `PREFILL · 110k · 38 t/s` while
/// the slot was plainly DECODING (n_decoded climbing). The PREFILL/DECODE
/// word comes from the byte heuristic in SessionThroughput (resp_bytes
/// flowing = DECODE, none yet = PREFILL). On the Anthropic streaming path
/// the proxy's resp_bytes for a request can lag or sit at 0 for a while
/// after decode has begun, so the heuristic says PREFILL while the joined
/// /slots row shows the decode counter moving. The rate already prefers
/// whichever slot counter moved (slotDetail); the WORD ignored that and kept
/// contradicting the number next to it.
///
/// Contract: once a row is joined to a slot and one of its counters moved,
/// the slot decides the phase word - decode counter moved -> DECODE, prefill
/// counter moved -> PREFILL. The byte heuristic still rules when there is no
/// slot join (no readout at all), and PARKED / FLAT are never overridden: a
/// parked row has no slot, and FLAT is a stall verdict the slot cannot see.
final class SlotPhaseWordTests: XCTestCase {

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

    private func grantedRequest(respBytes: Int) -> String {
        """
        {"total":1,"granted":1,"operation":"snapshot","requests":[\
        {"id":"950","timestamp":"2026-09-08T17:40:00Z","model":"cq35",\
        "req_path":"/v1/messages","method":"POST",\
        "req_headers":{"User-Agent":"claude-cli/2.1.263"},"remote_ip":"127.0.0.1",\
        "resp_headers":{},"resp_bytes":\(respBytes),"elapsed_ms":4000,\
        "metadata":{"session_id":"e411b774-0000-4000-8000-000000000001",\
        "slot_granted":"1","slot_affinity":"0"}}]}
        """
    }

    /// resp_bytes stays 0 (heuristic: PREFILL) while the slot's n_decoded
    /// climbs 100 -> 300 across polls. The word must follow the slot.
    func testDecodeCounterMovingOverridesPrefillWord() {
        let lock = NSLock()
        var polls = 0
        stub.responder = { _, path in
            guard path.hasSuffix("/slots") else { return (200, "{}") }
            lock.lock(); polls += 1; let n = polls; lock.unlock()
            let decoded = n <= 1 ? 100 : 300
            return (200, """
            {"models":[{"model":"cq35","state":"ready","slots":[\
            {"id":0,"is_processing":true,"n_prompt_tokens":110000,\
            "n_prompt_tokens_processed":110000,"n_decoded":\(decoded)}]}]}
            """)
        }
        let client = makeClient()
        stub.pushEvent(type: "inflight", inner: grantedRequest(respBytes: 0))

        XCTAssertTrue(waitUntil(6) { client.menuState.sessionRows.first?.word == ThroughputWord.prefill.rawValue },
                      "before any slot motion the byte heuristic's PREFILL stands")
        XCTAssertTrue(waitUntil(8) {
            client.menuState.sessionRows.first?.detail?.contains("t/s") == true
        }, "expected a rate once the decode counter moved, got \(client.menuState.sessionRows.first?.detail ?? "<nil>")")
        let row = client.menuState.sessionRows.first
        XCTAssertEqual(row?.word, ThroughputWord.decode.rawValue,
                       "decode counter moved, the word must say DECODE, got '\(row?.word ?? "<nil>")' next to '\(row?.detail ?? "<nil>")'")
    }

    /// The mirror case from the earlier report: bytes present (heuristic:
    /// DECODE) while only the prefill counter moves. The word must say
    /// PREFILL.
    func testPrefillCounterMovingOverridesDecodeWord() {
        let lock = NSLock()
        var polls = 0
        stub.responder = { _, path in
            guard path.hasSuffix("/slots") else { return (200, "{}") }
            lock.lock(); polls += 1; let n = polls; lock.unlock()
            let processed = n <= 1 ? 15000 : 17048
            return (200, """
            {"models":[{"model":"cq35","state":"ready","slots":[\
            {"id":0,"is_processing":true,"n_prompt_tokens":40000,\
            "n_prompt_tokens_processed":\(processed),"n_decoded":0}]}]}
            """)
        }
        let client = makeClient()
        stub.pushEvent(type: "inflight", inner: grantedRequest(respBytes: 40))

        XCTAssertTrue(waitUntil(8) {
            client.menuState.sessionRows.first?.detail?.contains("t/s") == true
        }, "expected a rate once the prefill counter moved, got \(client.menuState.sessionRows.first?.detail ?? "<nil>")")
        XCTAssertEqual(client.menuState.sessionRows.first?.word, ThroughputWord.prefill.rawValue,
                       "prefill counter moved, the word must say PREFILL")
    }
}
