import XCTest
@testable import LlamaSwapMenuCore

/// Pins the cooldown-row click contract (MenuView's "Cooldown: X waiting for
/// Y" Button -> BackendClient.finishGrace):
///
///   1. The click POSTs /api/swap-grace/finish/<requestedModel> - the model
///      WAITING, not the evictee.
///   2. The row clears optimistically the instant the click lands. The click
///      is the operator saying "unarm this"; making them wait for the next
///      swapGrace SSE tick reads as a dead click (witnessed 2026-09-09: the
///      click fired, the backend complied, and the row still LOOKED stuck).
///   3. A failed POST is never silent: the row comes back and the menu's
///      error line says the unarm did not happen. Fire-and-forget was the
///      original sin - a dead click and a successful one looked identical.
final class GraceFinishClickTests: XCTestCase {

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

    private let hold = GraceHoldRow(requestedModel: "cq27", evicteeModel: "cq35",
                                    waiting: 1, remainingSeconds: 300)

    func testClickPostsFinishForRequestedModelAndClearsRowImmediately() {
        stub.responder = { _, _ in (200, "{}") }
        let client = makeClient()
        client.menuState.graceHolds = [hold]

        client.finishGrace(reqModel: hold.requestedModel)

        // Optimistic clear: synchronous with the click, no SSE round-trip.
        XCTAssertTrue(client.menuState.graceHolds.isEmpty,
                      "clicked hold must clear the row at once, not on the next SSE tick")

        XCTAssertTrue(waitUntil {
            self.stub.recorded.contains(StubBackend.Recorded(
                method: "POST", path: "/api/swap-grace/finish/cq27"))
        }, "expected POST /api/swap-grace/finish/cq27, got \(stub.recorded)")
    }

    func testFailedClickRestoresRowAndSurfacesError() {
        stub.responder = { _, path in
            if path.hasPrefix("/api/swap-grace/finish/") { return (500, "boom") }
            return (200, "{}")
        }
        let client = makeClient()
        client.menuState.graceHolds = [hold]

        client.finishGrace(reqModel: hold.requestedModel)

        XCTAssertTrue(waitUntil { client.menuState.lastSwitchError != nil },
                      "a failed unarm must surface in the menu, not fail silently")
        XCTAssertTrue(waitUntil { !client.menuState.graceHolds.isEmpty },
                      "a failed unarm must restore the row (the hold is still armed)")
    }
}
