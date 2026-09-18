import XCTest
@testable import LlamaSwapMenuCore

/// Pins the HARD RULE (user, 2026-09-18): whenever slots are shown, the
/// waiting counter must be in parity with them. Since 2026-09-18 this is no
/// longer a client-side computation at all: `menuState.waiting`/
/// `waitingByTier` are copied straight from the session-state contract's
/// `queue.waiting`/`queue.byTier` (BackendClient.applySessionsSnapshot), and
/// `menuState.sessionRows` is copied from the SAME "sessions" event's
/// `sessions[]` in the same assignment - there is no second, independent
/// count of PARKED rows anywhere in this client to disagree with the
/// contract's own count.
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
        let parkedCount = client.menuState.sessionRows.filter { $0.phase == "PARKED" }.count
        XCTAssertEqual(client.menuState.waiting, parkedCount,
                        "waiting must equal the number of PARKED rows shown", line: line)
        if !client.menuState.waitingByTier.isEmpty {
            let sum = client.menuState.waitingByTier.values.reduce(0, +)
            XCTAssertEqual(sum, client.menuState.waiting,
                            "waitingByTier must sum to waiting - they must never disagree", line: line)
        }
    }

    private func sessionsBody(waiting: Int, byTier: [String: Int], sessions: String) -> String {
        let byTierJSON = byTier.map { "\"\($0.key)\":\($0.value)" }.joined(separator: ",")
        return """
        {"schema":"llama-swap.sessions/v1","generatedAt":"2026-09-18T00:00:00Z",\
        "resident":null,"queue":{"waiting":\(waiting),"byTier":{\(byTierJSON)}},\
        "cooldown":null,"memoryBrake":{"enabled":true,"holding":false,"remainingSeconds":0},\
        "sessions":[\(sessions)]}
        """
    }

    private func session(id: String, phase: String, tier: String = "default", priority: Int = 0) -> String {
        """
        {"sessionId":"\(id)","sessionShort":"\(String(id.prefix(8)))","requestId":"r-\(id)",\
        "model":"cq35","alias":"cq35","tier":"\(tier)","priority":\(priority),"phase":"\(phase)",\
        "parkReason":null,"slot":null,\
        "context":{"used":0,"cached":0,"processed":0,"decoded":0,"promptTotal":0,"window":262144},\
        "progress":null,"rate":{"kind":null,"tokensPerSecond":null,"windowSeconds":30.0},\
        "elapsedMs":0,"phaseSinceMs":0,"respTokens":0}
        """
    }

    func testIdleBoxShowsZeroWaiting() {
        let client = makeClient()
        stub.pushEvent(type: "sessions", inner: sessionsBody(waiting: 0, byTier: ["default": 0], sessions: ""))
        XCTAssertTrue(waitUntil { client.menuState.sessionRows.isEmpty })
        assertParity(client)
        XCTAssertEqual(client.menuState.waiting, 0)
        XCTAssertEqual(client.menuState.waitingSummary, "0 waiting")
    }

    /// A granted request (HOT/DECODE) is a slot, never "waiting".
    func testRunningSessionNeverCountsAsWaiting() {
        let client = makeClient()
        stub.pushEvent(type: "sessions", inner: sessionsBody(
            waiting: 0, byTier: ["default": 0],
            sessions: session(id: "aaaaaaaa", phase: "DECODE")))
        XCTAssertTrue(waitUntil { client.menuState.sessionRows.count == 1 })
        assertParity(client)
        XCTAssertEqual(client.menuState.waiting, 0)
    }

    func testParkedRequestGrantedDropsWaitingAtOnce() {
        let client = makeClient()
        stub.pushEvent(type: "sessions", inner: sessionsBody(
            waiting: 1, byTier: ["default": 1],
            sessions: session(id: "bbbbbbbb", phase: "PARKED")))
        XCTAssertTrue(waitUntil { client.menuState.sessionRows.first?.phase == "PARKED" })
        assertParity(client)
        XCTAssertEqual(client.menuState.waiting, 1)

        stub.pushEvent(type: "sessions", inner: sessionsBody(
            waiting: 0, byTier: ["default": 0],
            sessions: session(id: "bbbbbbbb", phase: "DECODE")))
        XCTAssertTrue(waitUntil { client.menuState.sessionRows.first?.phase != "PARKED" })
        assertParity(client)
        XCTAssertEqual(client.menuState.waiting, 0,
                        "a grant must clear waiting at once, not hold an old peak")
    }

    func testTwoTiersBreakdownMatchesParkedRowsPerTier() {
        let client = makeClient()
        stub.pushEvent(type: "sessions", inner: sessionsBody(
            waiting: 2, byTier: ["default": 1, "priority": 1],
            sessions: [
                session(id: "cccccccc", phase: "PARKED", tier: "default"),
                session(id: "dddddddd", phase: "PARKED", tier: "priority", priority: 10),
            ].joined(separator: ",")))
        XCTAssertTrue(waitUntil { client.menuState.sessionRows.count == 2 })
        assertParity(client)
        XCTAssertEqual(client.menuState.waiting, 2)
        XCTAssertEqual(client.menuState.waitingByTier["default"], 1)
        XCTAssertEqual(client.menuState.waitingByTier["priority"], 1)
        XCTAssertEqual(client.menuState.waitingSummary, "default 1, priority 1 waiting")

        // The priority request is granted: waiting drops to 1, entirely on
        // the default tier - immediately, no hold.
        stub.pushEvent(type: "sessions", inner: sessionsBody(
            waiting: 1, byTier: ["default": 1, "priority": 0],
            sessions: [
                session(id: "cccccccc", phase: "PARKED", tier: "default"),
                session(id: "dddddddd", phase: "DECODE", tier: "priority", priority: 10),
            ].joined(separator: ",")))
        XCTAssertTrue(waitUntil {
            client.menuState.sessionRows.first(where: { $0.sessionShort == "dddddddd" })?.phase != "PARKED"
        })
        assertParity(client)
        XCTAssertEqual(client.menuState.waiting, 1)
    }
}
