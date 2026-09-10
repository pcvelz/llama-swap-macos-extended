import XCTest
@testable import LlamaSwapMenuCore

/// A PARKED row says WHY. The proxy stamps `park_reason` on a queued
/// request's live entry (internal/router/scheduler Park* constants), and the
/// row renders it as a plain phrase after the word, the way a slot readout
/// follows PREFILL/DECODE. Vocabulary from the by-hand ledger of 2026-09-10
/// (llama-cm docs/research/2026-09-10-cooldown-dogfood-ledger.md): the
/// user's screenshot row "[9f3c6151] cq27 · PARKED" with no reason was O2's
/// cap park and could not be told from a cooldown park.
final class ParkReasonTests: XCTestCase {

    func testParkDetailPhrases() {
        XCTAssertEqual(MenuState.parkDetail(reason: "cap", kvParked: false), "slots full")
        XCTAssertEqual(MenuState.parkDetail(reason: "kv", kvParked: false), "kv pool")
        XCTAssertEqual(MenuState.parkDetail(reason: nil, kvParked: true), "kv pool")
        XCTAssertEqual(MenuState.parkDetail(reason: "busy", kvParked: false), "resident busy")
        XCTAssertEqual(MenuState.parkDetail(reason: "cooldown", kvParked: false), "cooldown")
        XCTAssertEqual(MenuState.parkDetail(reason: "loading", kvParked: false), "loading")
        XCTAssertEqual(MenuState.parkDetail(reason: "rank", kvParked: false), "behind higher rank")
        XCTAssertEqual(MenuState.parkDetail(reason: "swap-collision", kvParked: false), "another swap in flight")
        // An unknown/absent reason renders as nothing rather than a guess.
        XCTAssertNil(MenuState.parkDetail(reason: nil, kvParked: false))
        XCTAssertNil(MenuState.parkDetail(reason: "", kvParked: false))
    }

    private var stub: StubBackend!

    override func setUpWithError() throws { stub = try StubBackend() }
    override func tearDown() { stub.stop(); stub = nil }

    private func waitUntil(_ timeout: TimeInterval = 5, _ cond: () -> Bool) -> Bool {
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if cond() { return true }
            RunLoop.main.run(until: Date().addingTimeInterval(0.02))
        }
        return cond()
    }

    func testParkedRowCarriesTheReason() {
        stub.responder = { _, _ in (200, "{}") }
        let client = BackendClient(baseURL: stub.baseURL)
        XCTAssertTrue(waitUntil { self.stub.hasEventClient })

        let inner = """
        {"total":1,"granted":0,"operation":"snapshot",\
        "requests":[{"id":"3","timestamp":"2026-09-10T09:05:02Z","model":"cq35h",\
        "req_path":"/v1/messages","method":"POST",\
        "req_headers":{"User-Agent":"claude-code/1.0"},"remote_ip":"127.0.0.1",\
        "resp_headers":{},"resp_bytes":72,"elapsed_ms":17000,\
        "metadata":{"session_id":"fd998b45-7c70-4870-bd01-4213196f432f","slot_affinity":"1","park_reason":"cap"}}]}
        """
        stub.pushEvent(type: "inflight", inner: inner)

        XCTAssertTrue(waitUntil { client.menuState.sessionRows.first?.word == "PARKED" },
                      "expected a PARKED row, got \(client.menuState.sessionRows)")
        let line = client.menuState.sessionRows.first?.displayLine ?? ""
        XCTAssertTrue(line.contains("PARKED · slots full"), "row must name the park reason, got '\(line)'")
    }
}
