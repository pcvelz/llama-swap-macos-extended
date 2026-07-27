import XCTest
@testable import LlamaSwapMenuCore

/// Validates the menu's model-switch path by driving
/// `BackendClient.load(modelID:)`, the function `MenuView`'s per-model Button
/// calls.
final class ModelSwitchTests: XCTestCase {

    private var stub: StubBackend!

    override func setUpWithError() throws {
        stub = try StubBackend()
    }

    override func tearDown() {
        stub.stop()
        stub = nil
    }

    /// Spins the main run loop until `cond` holds or the deadline passes.
    /// BackendClient publishes every state change on the main queue, so the
    /// loop has to actually run for the client to make progress.
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

    /// Builds the client and waits for its `/api/events` stream to attach, so
    /// pushed model-status events cannot race the connect.
    private func makeClient() -> BackendClient {
        let client = BackendClient(baseURL: stub.baseURL)
        XCTAssertTrue(waitUntil { self.stub.hasEventClient },
                      "client never opened the /api/events stream")
        return client
    }

    // MARK: - wire contract

    /// A click must reach llama-swap as `POST /upstream/<model>/`. GET is
    /// deliberately rejected upstream (it would let health-pollers trigger
    /// loads), so the method and the trailing slash are both load-bearing.
    func testClickPostsUpstreamLoadForTargetModel() {
        stub.responder = { _, _ in (200, "{}") }
        let client = makeClient()

        client.load(modelID: "cq35")

        XCTAssertTrue(waitUntil {
            self.stub.recorded.contains(StubBackend.Recorded(method: "POST", path: "/upstream/cq35/"))
        }, "expected POST /upstream/cq35/, got \(stub.recorded)")
    }

    // MARK: - the reported defect

    /// The reported bug. The stub parks an upstream load for cq35 - accepted,
    /// never answered, no state change - until the incumbent is unloaded, which
    /// is how llama-swap behaves while cq27 still has in-flight requests. See
    /// `BackendClient.load` for why.
    func testClickSwitchesModelEvenWhileIncumbentIsBusy() {
        var incumbentLoaded = true

        stub.responder = { [weak self] method, path in
            guard let self else { return (200, "{}") }
            if method == "POST", path == "/api/models/unload/cq27" {
                incumbentLoaded = false
                DispatchQueue.main.async { self.stub.pushModelStatus([("cq27", "stopped")]) }
                return (200, "unloaded")
            }
            if method == "POST", path == "/upstream/cq35/" {
                if incumbentLoaded {
                    // Busy incumbent: llama-swap parks the request. No reply,
                    // no state change - the failure mode being tested.
                    return (-1, "")
                }
                DispatchQueue.main.async {
                    self.stub.pushModelStatus([("cq27", "stopped"), ("cq35", "ready")])
                }
                return (200, "{}")
            }
            return (200, "{}")
        }

        let client = makeClient()
        stub.pushModelStatus([("cq27", "ready")])
        XCTAssertTrue(waitUntil { client.menuState.activeModelID == "cq27" },
                      "precondition: cq27 should be the active model")

        client.load(modelID: "cq35")

        XCTAssertTrue(waitUntil(10) { client.menuState.activeModelID == "cq35" },
                      "clicking cq35 must make it the active model; still \(client.menuState.activeModelID ?? "nil"). requests: \(stub.recorded)")
    }

    /// The click must be visible immediately. Loading a model takes tens of
    /// seconds; without a pending marker the menu looks like the click did
    /// nothing, which is how the original bug presented.
    func testClickMarksTargetPendingRightAway() {
        stub.responder = { _, _ in (-1, "") }
        let client = makeClient()

        client.load(modelID: "cq35")

        XCTAssertTrue(waitUntil(2) { client.menuState.pendingModelID == "cq35" },
                      "target should be marked pending as soon as it is clicked")
    }

    /// Once the target is ready the pending marker clears, so the menu stops
    /// showing a switch that already finished.
    func testPendingClearsWhenTargetBecomesReady() {
        stub.responder = { _, _ in (200, "{}") }
        let client = makeClient()

        client.load(modelID: "cq35")
        XCTAssertTrue(waitUntil(2) { client.menuState.pendingModelID == "cq35" })

        stub.pushModelStatus([("cq35", "ready")])

        XCTAssertTrue(waitUntil { client.menuState.pendingModelID == nil },
                      "pending marker should clear once cq35 reports ready")
        XCTAssertEqual(client.menuState.activeModelID, "cq35")
    }

    /// llama-swap can hold more than one model ready at a time, so "first ready
    /// row" just reports config order. The model the user chose wins - and keeps
    /// winning on later events, after the pending marker has cleared.
    func testChosenModelWinsWhenSeveralModelsAreReady() {
        stub.responder = { _, _ in (200, "{}") }
        let client = makeClient()

        stub.pushModelStatus([("cq27", "ready")])
        XCTAssertTrue(waitUntil { client.menuState.activeModelID == "cq27" })

        client.load(modelID: "cq35")
        stub.pushModelStatus([("cq27", "ready"), ("cq35", "ready")])

        XCTAssertTrue(waitUntil { client.menuState.activeModelID == "cq35" },
                      "the clicked model should be reported active, not the first ready row")

        // A second event after pending cleared must not fall back to config order.
        XCTAssertTrue(waitUntil { client.menuState.pendingModelID == nil })
        stub.pushModelStatus([("cq27", "ready"), ("cq35", "ready")])
        RunLoop.main.run(until: Date().addingTimeInterval(0.3))
        XCTAssertEqual(client.menuState.activeModelID, "cq35",
                       "active model regressed to config order once pending cleared")
    }

    /// Clicking the model that is already serving must be a no-op. It must not
    /// evict a second resident model (pin / keep-warm) as if that were the
    /// incumbent being replaced.
    func testClickingActiveModelDoesNotUnloadAnything() {
        stub.responder = { _, _ in (200, "{}") }
        let client = makeClient()

        stub.pushModelStatus([("cq27", "ready"), ("cq35", "ready")])
        XCTAssertTrue(waitUntil { client.menuState.models.count == 2 })

        client.load(modelID: "cq27")
        RunLoop.main.run(until: Date().addingTimeInterval(0.5))

        XCTAssertFalse(stub.recorded.contains { $0.path.hasPrefix("/api/models/unload") },
                       "re-clicking the active model must not unload anything: \(stub.recorded)")
        XCTAssertNil(client.menuState.pendingModelID,
                     "a no-op click must not show a pending switch")
        XCTAssertEqual(client.menuState.activeModelID, "cq27")
    }

    /// Two quick clicks on different models: the first switch must not resurrect
    /// itself after the second has taken over.
    func testSecondClickSupersedesTheFirst() {
        stub.responder = { method, path in
            // Hold the unload so the first switch is still mid-flight when the
            // second click lands.
            if method == "POST", path.hasPrefix("/api/models/unload") { return (-1, "") }
            return (200, "{}")
        }
        let client = makeClient()
        stub.pushModelStatus([("cq27", "ready")])
        XCTAssertTrue(waitUntil { client.menuState.activeModelID == "cq27" })

        client.load(modelID: "cq35")
        client.load(modelID: "cq24")
        RunLoop.main.run(until: Date().addingTimeInterval(0.5))

        XCTAssertEqual(client.menuState.pendingModelID, "cq24")
        XCTAssertFalse(stub.recorded.contains { $0.path == "/upstream/cq35/" },
                       "the superseded switch must not issue its load: \(stub.recorded)")
    }
}
