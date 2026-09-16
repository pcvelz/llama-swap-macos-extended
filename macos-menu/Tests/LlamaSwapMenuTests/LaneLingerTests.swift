import XCTest
@testable import LlamaSwapMenuCore

/// Witnessed on the live box 2026-09-09 (/slots sampled at 1Hz): a Claude
/// Code session in its tool loop issues ONE proxy request per turn - 3-10s of
/// prefill+decode, the request ends, then 1-3s of nothing while the client
/// runs the tool locally, then the next request arrives. Session rows are one
/// row per LANE and, per applyInflightEntries, "a lane drops out only when it
/// has no request left", so every turn boundary removes the lane's only
/// request, the row VANISHES, and 1-3s later it is re-added and re-sorted.
/// The user sees rows disappearing and reappearing on every single turn.
///
/// Contract (lane linger): when a lane's last request is removed the row
/// stays put for `laneLingerSeconds`, keeping its position, origin, model,
/// tier, title and parent/agent, and renders an honest between-turns word -
/// never a fake DECODE/FLOWING. A new request for the same lane inside the
/// window reuses the row in place. Only a genuinely departed session (silent
/// past the window) loses its row.
///
/// The earlier flicker fix (SlotReadoutHoldTests / WaitingHoldTests) held the
/// RATE readout only; row PRESENCE had no hold at all.
final class LaneLingerTests: XCTestCase {

    private var stub: StubBackend!

    override func setUpWithError() throws {
        stub = try StubBackend()
    }

    override func tearDown() {
        stub.stop()
        stub = nil
    }

    @discardableResult
    private func waitUntil(_ timeout: TimeInterval = 6, _ cond: () -> Bool) -> Bool {
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if cond() { return true }
            RunLoop.main.run(until: Date().addingTimeInterval(0.02))
        }
        return cond()
    }

    private func makeClient(laneLingerSeconds: TimeInterval = BackendClient.defaultLaneLingerSeconds) -> BackendClient {
        // Hold 0: these cases pin the RAW lane word; the visual hold on top
        // of it is RowWordSteadinessTests' contract.
        let client = BackendClient(baseURL: stub.baseURL, laneLingerSeconds: laneLingerSeconds,
                                   rowWordHoldSeconds: 0)
        XCTAssertTrue(waitUntil { self.stub.hasEventClient },
                      "client never opened the /api/events stream")
        return client
    }

    /// One request per turn in one lane, plus a second lane that never goes
    /// away (so an order regression is observable). id 200 finishes, the gap
    /// is the client running a tool, id 201 is the next turn.
    private func req(_ id: String, session: String, bytes: Int) -> String {
        """
        {"id":"\(id)","timestamp":"2026-09-09T09:00:00Z","model":"cq35",\
        "req_path":"/v1/messages","method":"POST",\
        "req_headers":{"User-Agent":"claude-code/1.0"},"remote_ip":"127.0.0.1",\
        "resp_headers":{},"resp_bytes":\(bytes),"elapsed_ms":900,\
        "metadata":{"session_id":"\(session)","tier":"priority","slot_granted":"1"}}
        """
    }

    private let laneA = "3bf85c8c-1234-4321-aaaa-bbbbccccdddd"
    private let laneB = "934b47c6-9143-464f-8846-49099f0a295b"

    func testLaneRowSurvivesTheGapBetweenTurnsAndIsReusedInPlace() {
        let client = makeClient()

        // Two lanes in flight: A first, then B. A's row is index 0.
        stub.pushEvent(type: "inflight", inner: """
        {"total":2,"granted":2,"operation":"snapshot","requests":[\
        \(req("200", session: laneA, bytes: 4000)),\
        \(req("300", session: laneB, bytes: 4000))]}
        """)
        XCTAssertTrue(waitUntil { client.menuState.sessionRows.count == 2 },
                      "expected 2 lane rows, got \(client.menuState.sessionRows.map(\.displayLine))")
        let before = client.menuState.sessionRows[0]
        XCTAssertEqual(before.origin, "3bf85c8c")

        // Turn boundary: lane A's ONLY request ends. The client is now
        // running a tool for 1-3s. The row must stay.
        stub.pushEvent(type: "inflight", inner: "{\"total\":1,\"operation\":\"remove\",\"id\":\"200\"}")
        RunLoop.main.run(until: Date().addingTimeInterval(0.5))

        let during = client.menuState.sessionRows
        XCTAssertEqual(during.count, 2,
                       "lane A must LINGER across the between-turns gap, got \(during.map(\.displayLine))")
        XCTAssertEqual(during.first?.origin, "3bf85c8c", "the lingering row keeps its position (first)")
        XCTAssertEqual(during.first?.model, before.model, "the lingering row keeps its model")
        XCTAssertEqual(during.first?.tier, before.tier, "the lingering row keeps its tier")
        XCTAssertEqual(during.first?.word, "TURN",
                       "a lingering lane must say it is between turns, never a fake DECODE/FLOWING; got '\(during.first?.word ?? "<none>")'")

        // Next turn, same lane, new request id: reuse in place - exactly one
        // row for the lane, still first, no removal/re-append/re-sort.
        stub.pushEvent(type: "inflight", inner: """
        {"total":2,"operation":"upsert","request":\(req("201", session: laneA, bytes: 10))}
        """)
        XCTAssertTrue(waitUntil { client.menuState.sessionRows.first?.id == "201" },
                      "the next turn must land in the SAME lane row, got \(client.menuState.sessionRows.map(\.displayLine))")
        let after = client.menuState.sessionRows
        XCTAssertEqual(after.count, 2, "no duplicate row for the reused lane, got \(after.map(\.displayLine))")
        XCTAssertEqual(after.map(\.origin), ["3bf85c8c", "934b47c6"], "order must not move across a turn boundary")
        XCTAssertNotEqual(after.first?.word, "TURN", "an in-flight request ends the linger word")
    }

    /// The other half of the contract: a session that genuinely LEFT the box
    /// must lose its row once the window expires - and the expiry has to fire
    /// on its own, with no further SSE events to drive it.
    func testLaneRowDisappearsAfterTheLingerWindowWithNoFurtherEvents() {
        // Short injected window: the production default is 10s and no test
        // should sleep it.
        let client = makeClient(laneLingerSeconds: 0.6)
        stub.pushEvent(type: "inflight", inner: """
        {"total":1,"granted":1,"operation":"snapshot","requests":[\
        \(req("400", session: laneA, bytes: 4000))]}
        """)
        XCTAssertTrue(waitUntil { client.menuState.sessionRows.count == 1 })

        stub.pushEvent(type: "inflight", inner: "{\"total\":0,\"operation\":\"remove\",\"id\":\"400\"}")
        RunLoop.main.run(until: Date().addingTimeInterval(0.1))
        XCTAssertEqual(client.menuState.sessionRows.count, 1, "row must linger right after the removal")

        // No further events at all: the window must expire by itself.
        XCTAssertTrue(waitUntil(6) { client.menuState.sessionRows.isEmpty },
                      "the row must be dropped once the linger window expires, got \(client.menuState.sessionRows.map(\.displayLine))")
    }
}
