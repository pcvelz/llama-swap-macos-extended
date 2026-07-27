import XCTest
@testable import LlamaSwapMenuCore

/// End-to-end validation against a real llama-swap instance.
///
/// This is the test that answers "does clicking another model actually make
/// that model active in the box?". It simulates the click by calling
/// `BackendClient.load(modelID:)` - the same function `MenuView`'s Button
/// invokes - and then requires TWO independent confirmations:
///
///   1. the menu's own state (`activeModelID`), fed by the `/api/events` SSE
///      stream, flips to the target; and
///   2. llama-swap's `/running` endpoint reports the target as `ready`.
///
/// Skipped unless explicitly enabled, because it swaps models on a live
/// backend:
///
///   LLAMA_MENU_LIVE=1 \
///   LLAMA_MENU_LIVE_URL=http://localhost:8001 \
///   LLAMA_MENU_LIVE_TO=<target alias or id> \
///   swift test --filter LiveModelSwitchTests
final class LiveModelSwitchTests: XCTestCase {

    private func env(_ key: String) -> String? {
        let v = ProcessInfo.processInfo.environment[key]
        return (v?.isEmpty ?? true) ? nil : v
    }

    @discardableResult
    private func waitUntil(_ timeout: TimeInterval, _ cond: () -> Bool) -> Bool {
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if cond() { return true }
            RunLoop.main.run(until: Date().addingTimeInterval(0.1))
        }
        return cond()
    }

    /// Resolves a user-facing alias (e.g. "cq35") to the real model ID using the
    /// same alias list the menu renders labels from.
    private func resolve(_ needle: String, in models: [ModelRow]) -> String? {
        if let exact = models.first(where: { $0.id == needle }) { return exact.id }
        return models.first { ($0.aliases ?? []).contains(needle) }?.id
    }

    /// Independent confirmation straight from llama-swap, bypassing the menu's
    /// own view of the world.
    private func backendReports(ready modelID: String, baseURL: URL) -> Bool {
        guard let data = try? Data(contentsOf: baseURL.appendingPathComponent("/running")),
              let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
              let running = obj["running"] as? [[String: Any]] else { return false }
        return running.contains {
            ($0["model"] as? String) == modelID && ($0["state"] as? String) == "ready"
        }
    }

    func testClickingAnotherModelMakesItActiveOnTheRealBackend() throws {
        try XCTSkipUnless(env("LLAMA_MENU_LIVE") == "1",
                          "live backend test; set LLAMA_MENU_LIVE=1 to run")

        let baseURL = URL(string: env("LLAMA_MENU_LIVE_URL") ?? "http://localhost:8001")!
        let target = try XCTUnwrap(env("LLAMA_MENU_LIVE_TO"),
                                   "set LLAMA_MENU_LIVE_TO to the model to switch to")

        let client = BackendClient(baseURL: baseURL)

        // The menu learns the model list from the same SSE stream it uses for
        // the bullet, so wait for that rather than querying a side channel.
        XCTAssertTrue(waitUntil(30) { !client.menuState.models.isEmpty },
                      "no modelStatus received from \(baseURL) - is llama-swap running?")

        let targetID = try XCTUnwrap(resolve(target, in: client.menuState.models),
                                     "model '\(target)' not in \(client.menuState.models.map(\.id))")
        let before = client.menuState.activeModelID
        print("[live] active before click: \(before ?? "none") -> clicking \(targetID)")
        try XCTSkipIf(before == targetID, "\(targetID) is already active; nothing to switch")

        // The click.
        client.load(modelID: targetID)

        // Model loads on this box take tens of seconds; allow a generous window.
        let deadline = TimeInterval(env("LLAMA_MENU_LIVE_TIMEOUT").flatMap(Double.init) ?? 300)

        XCTAssertTrue(waitUntil(deadline) { client.menuState.activeModelID == targetID },
                      "menu still shows \(client.menuState.activeModelID ?? "nil") after clicking \(targetID)")

        XCTAssertTrue(waitUntil(60) { self.backendReports(ready: targetID, baseURL: baseURL) },
                      "llama-swap /running does not report \(targetID) as ready")

        XCTAssertNil(client.menuState.pendingModelID,
                     "pending marker should have cleared once the switch completed")
        print("[live] active after click: \(client.menuState.activeModelID ?? "none") - confirmed ready by /running")
    }
}
