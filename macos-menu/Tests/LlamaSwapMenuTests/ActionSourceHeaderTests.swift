import XCTest
@testable import LlamaSwapMenuCore

/// @user-gated: user ruling - every menu action states its source
///
/// Pins that BackendClient sends `X-Action-Source: menu-click` on the two
/// destructive POST endpoints (unpenalize and cancelInflight) so the server
/// can log where each action came from rather than guessing "by operator".
final class ActionSourceHeaderTests: XCTestCase {

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

    // MARK: - unpenalize header

    func testUnpenalizeSendsActionSourceHeader() {
        stub.responder = { _, _ in (200, "{}") }
        let client = makeClient()
        let sessionId = "934159af-0000-0000-0000-000000000000"
        client.unpenalize(sessionId: sessionId)

        XCTAssertTrue(waitUntil {
            self.stub.recorded.contains(where: { $0.path.hasSuffix("/unpenalize") })
        }, "expected POST /api/sessions/<id>/unpenalize, got \(stub.recorded)")

        let unpenReq = stub.recorded.first(where: { $0.path.hasSuffix("/unpenalize") })
        XCTAssertNotNil(unpenReq, "must have recorded the unpenalize request")
        XCTAssertEqual(unpenReq!.headers["X-Action-Source"], "menu-click",
                       "unpenalize must carry X-Action-Source: menu-click so the server logs the source")
    }

    // MARK: - cancelInflight header

    func testCancelInflightSendsActionSourceHeader() {
        stub.responder = { _, _ in (200, "{}") }
        let client = makeClient()

        client.cancelInflight(id: "req-abc")

        XCTAssertTrue(waitUntil {
            self.stub.recorded.contains(where: { $0.path.hasPrefix("/api/inflight/") && $0.path.hasSuffix("/cancel") })
        }, "expected POST /api/inflight/req-abc/cancel, got \(stub.recorded)")

        let cancelReq = stub.recorded.first(where: { $0.path.hasPrefix("/api/inflight/") && $0.path.hasSuffix("/cancel") })
        XCTAssertNotNil(cancelReq, "must have recorded the cancel request")
        XCTAssertEqual(cancelReq!.headers["X-Action-Source"], "menu-click",
                       "cancelInflight must carry X-Action-Source: menu-click so the server logs the source")
    }
}
