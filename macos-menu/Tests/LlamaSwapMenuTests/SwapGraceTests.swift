import XCTest
@testable import LlamaSwapMenuCore

/// Wire-level pins for the cooldown surface: the "swapGrace" SSE event's
/// payload (mirrors internal/swaputil/events.go Cooldown, pushed by
/// internal/server/apigroup.go handleAPIEvents/handleAPISwapGrace), the
/// single menu row it renders as, and the hot-slot rows shown beneath it.
///
/// The cooldown is ONE state on the resident model (it is cooling down; the
/// swap to the next model waits), never one row per waiting model
/// (2026-09-10: "cq35h waiting for cq35" + "cq27 waiting for cq35").
final class SwapGraceTests: XCTestCase {

    func testCooldownRowDecodesSingletonWithHotSlots() throws {
        let json = """
        {"evicteeModel":"cq35","nextModel":"cq35h","waiting":2,"remainingSeconds":252,
         "slots":[{"slot":0,"sessionId":"725558cb-aaaa","idleSeconds":35},{"slot":1,"sessionId":"","idleSeconds":0}]}
        """.data(using: .utf8)!

        let cd = try JSONDecoder().decode(CooldownRow.self, from: json)
        XCTAssertEqual(cd.evicteeModel, "cq35")
        XCTAssertEqual(cd.nextModel, "cq35h")
        XCTAssertEqual(cd.waiting, 2)
        XCTAssertEqual(cd.remainingSeconds, 252)
        XCTAssertEqual(cd.slots.count, 2)
        XCTAssertEqual(cd.slots[0].sessionId, "725558cb-aaaa")
    }

    func testCooldownPayloadDecodesNullAndPresent() throws {
        let none = try JSONDecoder().decode(CooldownPayload.self, from: Data("""
        {"cooldown":null}
        """.utf8))
        XCTAssertNil(none.cooldown)

        let some = try JSONDecoder().decode(CooldownPayload.self, from: Data("""
        {"cooldown":{"evicteeModel":"cq35","nextModel":"cq27","waiting":1,"remainingSeconds":9,"slots":[]}}
        """.utf8))
        XCTAssertEqual(some.cooldown?.nextModel, "cq27")
    }

    func testCooldownLabelNamesTheCoolingModelThenTheNextOne() {
        let cd = CooldownRow(evicteeModel: "cq35", nextModel: "cq35h", waiting: 2, remainingSeconds: 265, slots: [])
        // The model that IS loaded is the one cooling down; the row must never
        // read as "waiting for <loaded model>".
        XCTAssertEqual(MenuState.cooldownLabel(cd, resident: "cq35", next: "cq35h"),
                       "Cooldown: cq35 (4:25), then cq35h · 2 waiting")
        let one = CooldownRow(evicteeModel: "cq35", nextModel: "cq27", waiting: 1, remainingSeconds: 9, slots: [])
        XCTAssertEqual(MenuState.cooldownLabel(one, resident: "cq35", next: "cq27"),
                       "Cooldown: cq35 (0:09), then cq27 · 1 waiting")
    }

    func testHotSlotLabelShowsOwningSessionAndIdleTime() {
        // A slot kept warm for a session across a tool call / AskUserQuestion
        // pause must stay visible like an active slot, with its owner.
        let owned = HotSlotRow(slot: 0, sessionId: "725558cb-1234-5678", idleSeconds: 35)
        XCTAssertEqual(MenuState.hotSlotLabel(owned), "  slot 0 · [725558cb] · hot, idle 0:35")
        let free = HotSlotRow(slot: 1, sessionId: "", idleSeconds: 0)
        XCTAssertEqual(MenuState.hotSlotLabel(free), "  slot 1 · free")
    }

    func testCountdownFormatsMinutesAndSeconds() {
        XCTAssertEqual(CompactFormatter.countdown(252), "4:12")
        XCTAssertEqual(CompactFormatter.countdown(9), "0:09")
        XCTAssertEqual(CompactFormatter.countdown(0), "0:00")
        XCTAssertEqual(CompactFormatter.countdown(-3), "0:00")
    }

    func testMenuStateCooldownDefaultsNilAndIsSettable() {
        var state = MenuState()
        XCTAssertNil(state.cooldown)
        state.cooldown = CooldownRow(evicteeModel: "cq35", nextModel: "cq35h", waiting: 1, remainingSeconds: 60, slots: [])
        XCTAssertEqual(state.cooldown?.evicteeModel, "cq35")
    }
}
