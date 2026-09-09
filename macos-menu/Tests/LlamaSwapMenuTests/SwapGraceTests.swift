import XCTest
@testable import LlamaSwapMenuCore

/// Wire-level pins for the swap-grace surface: the "swapGrace" SSE event's
/// payload (mirrors internal/swaputil/events.go GraceHold, pushed by
/// internal/server/apigroup.go handleAPIEvents/handleAPISwapGrace) and the
/// countdown formatting the menu row renders it with.
final class SwapGraceTests: XCTestCase {

    func testGraceHoldRowDecodesFieldsAndDerivesID() throws {
        let json = """
        {"requestedModel":"cq35","evicteeModel":"cq35h","waiting":2,"remainingSeconds":252}
        """.data(using: .utf8)!

        let hold = try JSONDecoder().decode(GraceHoldRow.self, from: json)
        XCTAssertEqual(hold.requestedModel, "cq35")
        XCTAssertEqual(hold.evicteeModel, "cq35h")
        XCTAssertEqual(hold.waiting, 2)
        XCTAssertEqual(hold.remainingSeconds, 252)
        XCTAssertEqual(hold.id, "cq35->cq35h", "id must distinguish holds with the same requested model but a different evictee")
    }

    func testSwapGracePayloadDecodesEmptyAndNonEmptyLists() throws {
        let empty = try JSONDecoder().decode(SwapGracePayload.self, from: Data("""
        {"holds":[]}
        """.utf8))
        XCTAssertEqual(empty.holds.count, 0)

        let nonEmpty = try JSONDecoder().decode(SwapGracePayload.self, from: Data("""
        {"holds":[{"requestedModel":"cq27","evicteeModel":"cq35h","waiting":1,"remainingSeconds":9}]}
        """.utf8))
        XCTAssertEqual(nonEmpty.holds.count, 1)
        XCTAssertEqual(nonEmpty.holds[0].requestedModel, "cq27")
    }

    func testCountdownFormatsMinutesAndSeconds() {
        XCTAssertEqual(CompactFormatter.countdown(252), "4:12")
        XCTAssertEqual(CompactFormatter.countdown(9), "0:09")
        XCTAssertEqual(CompactFormatter.countdown(0), "0:00")
        // A stale/negative reading (the hold ended between the last SSE tick
        // and render) must clamp to zero, never print a negative countdown.
        XCTAssertEqual(CompactFormatter.countdown(-3), "0:00")
    }

    func testMenuStateGraceHoldsDefaultsEmptyAndIsSettable() {
        var state = MenuState()
        XCTAssertTrue(state.graceHolds.isEmpty)

        state.graceHolds = [GraceHoldRow(requestedModel: "cq35", evicteeModel: "cq35h", waiting: 1, remainingSeconds: 60)]
        XCTAssertEqual(state.graceHolds.count, 1)
        XCTAssertEqual(state.graceHolds[0].id, "cq35->cq35h")
    }
}
