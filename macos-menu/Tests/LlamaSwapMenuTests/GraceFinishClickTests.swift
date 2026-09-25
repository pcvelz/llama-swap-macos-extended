import XCTest
@testable import LlamaSwapMenuCore

/// Pins the cooldown-row click contract (MenuView's "Cooldown: X (m:ss),
/// then Y" Button -> BackendClient.finishCooldown):
///
///   1. The click POSTs /api/swap-grace/finish - no model in the path, the
///      cooldown is a singleton on the resident.
///   2. The row clears optimistically the instant the click lands. The click
///      is the operator saying "end it"; making them wait for the next
///      swapGrace SSE tick reads as a dead click (witnessed 2026-09-09: the
///      click fired, the backend complied, and the row still LOOKED stuck).
///   3. A failed POST is never silent: the row comes back and the menu's
///      error line says the finish did not happen. Fire-and-forget was the
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

    private let cooldown = CooldownRow(evicteeModel: "cq35", nextModel: "cq27",
                                       waiting: 1, remainingSeconds: 300, slots: [])

    func testClickPostsFinishAndClearsRowImmediately() {
        stub.responder = { _, _ in (200, "{}") }
        let client = makeClient()
        client.menuState.cooldown = cooldown

        client.finishCooldown()

        // Optimistic clear: synchronous with the click, no SSE round-trip.
        XCTAssertNil(client.menuState.cooldown,
                     "clicked cooldown must clear the row at once, not on the next SSE tick")

        XCTAssertTrue(waitUntil {
            self.stub.recorded.contains(where: { $0.method == "POST" && $0.path == "/api/swap-grace/finish" })
        }, "expected POST /api/swap-grace/finish, got \(stub.recorded)")
    }

    func testFailedClickRestoresRowAndSurfacesError() {
        stub.responder = { _, path in
            if path == "/api/swap-grace/finish" { return (500, "boom") }
            return (200, "{}")
        }
        let client = makeClient()
        client.menuState.cooldown = cooldown

        client.finishCooldown()

        XCTAssertTrue(waitUntil { client.menuState.lastSwitchError != nil },
                      "a failed finish must surface in the menu, not fail silently")
        XCTAssertTrue(waitUntil { client.menuState.cooldown != nil },
                      "a failed finish must restore the row (the cooldown is still running)")
    }
}
