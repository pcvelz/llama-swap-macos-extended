import XCTest
@testable import LlamaSwapMenuCore

/// Pins the request-row click contract (MenuView's per-request Button ->
/// BackendClient.cancelInflight):
///
///   1. The click POSTs /api/inflight/<id>/cancel - the fork's existing
///      cancel endpoint; for a PARKED request the cancelled context drops it
///      out of the scheduler's queue, i.e. the row is evicted.
///   2. The row clears optimistically the instant the click lands, same
///      contract as the cooldown row (GraceFinishClickTests).
///   3. A failed POST is never silent: the rows come back and the menu's
///      error line says the evict did not happen.
final class CancelInflightClickTests: XCTestCase {

    private var stub: StubBackend!

    override func setUpWithError() throws {
        stub = try StubBackend()
    }

    override func tearDown() {
        stub.stop()
        stub = nil
    }

    @discardableResult
    private func waitUntil(_ timeout: TimeInterval = 5,
                           _ cond: () -> Bool) -> Bool {
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

    private let rows = [
        SessionRow(id: "41", origin: "17426df4", model: "cq35", tier: "-",
                   word: "PARKED", hasSession: true),
        SessionRow(id: "42", origin: "9bc0de12", model: "cq35", tier: "-",
                   word: "DECODE", hasSession: true),
    ]

    func testClickPostsCancelAndClearsRowImmediately() {
        stub.responder = { _, _ in (200, "{}") }
        let client = makeClient()
        client.rawRows = rows

        client.cancelInflight(id: "41")

        // Optimistic clear: synchronous with the click, no SSE round-trip.
        XCTAssertFalse(client.menuState.sessionRows.contains(where: { $0.id == "41" }),
                       "clicked row must clear at once, not on the next SSE tick")
        XCTAssertTrue(client.menuState.sessionRows.contains(where: { $0.id == "42" }),
                      "the click evicts only its own row")

        XCTAssertTrue(waitUntil {
            self.stub.recorded.contains(StubBackend.Recorded(
                method: "POST", path: "/api/inflight/41/cancel"))
        }, "expected POST /api/inflight/41/cancel, got \(stub.recorded)")
    }

    func testFailedClickRestoresRowsAndSurfacesError() {
        stub.responder = { _, path in
            if path.hasPrefix("/api/inflight/") { return (500, "boom") }
            return (200, "{}")
        }
        let client = makeClient()
        client.rawRows = rows

        client.cancelInflight(id: "41")

        XCTAssertTrue(waitUntil { client.menuState.lastSwitchError != nil },
                      "a failed evict must surface in the menu, not fail silently")
        XCTAssertTrue(waitUntil {
            client.menuState.sessionRows.contains(where: { $0.id == "41" })
        }, "a failed evict must restore the row (the request is still queued)")
    }
}
