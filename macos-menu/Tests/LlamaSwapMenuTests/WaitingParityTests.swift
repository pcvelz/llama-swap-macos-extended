import XCTest
@testable import LlamaSwapMenuCore

/// Pins the HARD RULE (user, 2026-09-18): whenever slots are shown, the
/// waiting counter must be in parity with them - derived from the SAME
/// snapshot that produces sessionRows, never smoothed independently. Waiting
/// counts PARKED requests only (queued, not in a slot); a granted request is
/// shown as a slot, never as "waiting".
///
/// This replaces the old peak-hold anti-flap (MenuState.waitingHold /
/// holdWaiting), which let "N waiting" keep showing a stale peak for up to
/// 600s after the real queue drained to 0 and every slot went idle -
/// "waiting" and "Queue: idle" visibly disagreeing. There is no longer any
/// smoothing to disagree: menuState.waiting/waitingByTier are recomputed from
/// menuState.sessionRows every time sessionRows changes (BackendClient's
/// publishHeldRows -> applyWaitingParity), so the two can never diverge.
final class WaitingParityTests: XCTestCase {

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

    private func makeClient() -> BackendClient {
        let client = BackendClient(baseURL: stub.baseURL)
        XCTAssertTrue(waitUntil { self.stub.hasEventClient },
                      "client never opened the /api/events stream")
        return client
    }

    /// Asserts the invariant itself, generically: waiting must always equal
    /// the number of PARKED rows currently shown, and waitingByTier's sum
    /// must equal waiting whenever it is populated.
    private func assertParity(_ client: BackendClient, line: UInt = #line) {
        let parkedCount = client.menuState.sessionRows.filter { $0.word == "PARKED" }.count
        XCTAssertEqual(client.menuState.waiting, parkedCount,
                        "waiting must equal the number of PARKED rows shown", line: line)
        if !client.menuState.waitingByTier.isEmpty {
            let sum = client.menuState.waitingByTier.values.reduce(0, +)
            XCTAssertEqual(sum, client.menuState.waiting,
                            "waitingByTier must sum to waiting - they must never disagree", line: line)
        }
    }

    // Nothing queued, nothing running: idle box shows 0 waiting, matching
    // "Queue: idle" and no slots.
    func testIdleBoxShowsZeroWaiting() {
        let client = makeClient()
        stub.pushEvent(type: "inflight", inner: """
        {"total":0,"operation":"snapshot","requests":[],"queue":[]}
        """)
        XCTAssertTrue(waitUntil { client.menuState.sessionRows.isEmpty })
        assertParity(client)
        XCTAssertEqual(client.menuState.waiting, 0)
        XCTAssertEqual(client.menuState.waitingSummary, "0 waiting")
    }

    // A short request that never parked (granted immediately) finishing must
    // never have counted as "waiting", before or after it completes -
    // the old Total-based count conflated running + parked, which is
    // exactly the divergence this rule closes.
    func testShortGrantedTurnFinishingNeverCountsAsWaiting() {
        let client = makeClient()
        stub.pushEvent(type: "inflight", inner: """
        {"total":1,"operation":"snapshot",\
        "requests":[{"id":"1","timestamp":"2026-09-18T00:00:00Z","model":"cq35",\
        "req_path":"/v1/messages","method":"POST","req_headers":{},"remote_ip":"127.0.0.1",\
        "resp_headers":{},"resp_bytes":200,"elapsed_ms":300,\
        "metadata":{"session_id":"aaaaaaaa-0000","slot_granted":"1"}}],"queue":[]}
        """)
        XCTAssertTrue(waitUntil { client.menuState.sessionRows.count == 1 })
        assertParity(client)
        XCTAssertEqual(client.menuState.waiting, 0, "a granted request is a slot, never waiting")

        stub.pushEvent(type: "inflight", inner: """
        {"total":0,"operation":"remove","id":"1"}
        """)
        // The lane may linger for a beat rendering its last row (turn
        // boundary grace, see BackendClient.defaultLaneLingerSeconds) - it
        // was never PARKED, so parity must hold at once regardless.
        XCTAssertTrue(waitUntil { client.menuState.sessionRows.first?.word != "PARKED" })
        assertParity(client)
        XCTAssertEqual(client.menuState.waiting, 0, "finishing must not leave a stale waiting count behind")
    }

    // A parked request being granted a slot must drop out of "waiting"
    // IMMEDIATELY - no anti-flap hold letting the old peak linger.
    func testParkedRequestGrantedDropsWaitingAtOnce() {
        let client = makeClient()
        stub.pushEvent(type: "inflight", inner: """
        {"total":1,"operation":"snapshot",\
        "requests":[{"id":"7","timestamp":"2026-09-18T00:00:00Z","model":"cq35",\
        "req_path":"/v1/messages","method":"POST","req_headers":{},"remote_ip":"127.0.0.1",\
        "resp_headers":{},"resp_bytes":0,"elapsed_ms":50,\
        "metadata":{"session_id":"bbbbbbbb-0000"}}],"queue":[]}
        """)
        XCTAssertTrue(waitUntil { client.menuState.sessionRows.first?.word == "PARKED" })
        assertParity(client)
        XCTAssertEqual(client.menuState.waiting, 1)

        stub.pushEvent(type: "inflight", inner: """
        {"total":1,"operation":"snapshot",\
        "requests":[{"id":"7","timestamp":"2026-09-18T00:00:00Z","model":"cq35",\
        "req_path":"/v1/messages","method":"POST","req_headers":{},"remote_ip":"127.0.0.1",\
        "resp_headers":{},"resp_bytes":300,"elapsed_ms":80,\
        "metadata":{"session_id":"bbbbbbbb-0000","slot_granted":"1"}}],"queue":[]}
        """)
        XCTAssertTrue(waitUntil { client.menuState.sessionRows.first?.word != "PARKED" })
        assertParity(client)
        XCTAssertEqual(client.menuState.waiting, 0,
                        "a grant must clear waiting at once, not hold the old peak")
    }

    // Two tiers, one parked request on each: the breakdown must match the
    // PARKED rows exactly, per tier, and sum to the total.
    func testTwoTiersBreakdownMatchesParkedRowsPerTier() {
        let client = makeClient()
        stub.pushEvent(type: "inflight", inner: """
        {"total":2,"byTier":{"default":1,"priority":1},"operation":"snapshot",\
        "requests":[\
        {"id":"10","timestamp":"2026-09-18T00:00:00Z","model":"cq35",\
        "req_path":"/v1/messages","method":"POST","req_headers":{},"remote_ip":"127.0.0.1",\
        "resp_headers":{},"resp_bytes":0,"elapsed_ms":50,\
        "metadata":{"session_id":"cccccccc-0000","tier":"default"}},\
        {"id":"11","timestamp":"2026-09-18T00:00:05Z","model":"cq27",\
        "req_path":"/v1/messages","method":"POST","req_headers":{},"remote_ip":"127.0.0.1",\
        "resp_headers":{},"resp_bytes":0,"elapsed_ms":40,\
        "metadata":{"session_id":"dddddddd-0000","tier":"priority"}}\
        ],"queue":[]}
        """)
        XCTAssertTrue(waitUntil { client.menuState.sessionRows.count == 2 })
        assertParity(client)
        XCTAssertEqual(client.menuState.waiting, 2)
        XCTAssertEqual(client.menuState.waitingByTier["default"], 1)
        XCTAssertEqual(client.menuState.waitingByTier["priority"], 1)
        XCTAssertEqual(client.menuState.waitingSummary, "default 1, priority 1 waiting")

        // The priority request is granted: waiting drops to 1, entirely on
        // the default tier - immediately, no hold.
        stub.pushEvent(type: "inflight", inner: """
        {"total":2,"byTier":{"default":1,"priority":0},"operation":"upsert",\
        "request":{"id":"11","timestamp":"2026-09-18T00:00:05Z","model":"cq27",\
        "req_path":"/v1/messages","method":"POST","req_headers":{},"remote_ip":"127.0.0.1",\
        "resp_headers":{},"resp_bytes":300,"elapsed_ms":90,\
        "metadata":{"session_id":"dddddddd-0000","tier":"priority","slot_granted":"1"}}}
        """)
        XCTAssertTrue(waitUntil {
            client.menuState.sessionRows.first(where: { $0.id == "11" })?.word != "PARKED"
        })
        assertParity(client)
        XCTAssertEqual(client.menuState.waiting, 1)
    }
}
