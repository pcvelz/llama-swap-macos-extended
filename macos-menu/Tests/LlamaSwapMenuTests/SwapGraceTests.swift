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

    func testCooldownLabelRendersOnTheResidentRow() {
        // The cooldown is the loaded model's own state: its row reads
        // "cooldown m:ss for [owner], then <next> · N waiting" - never
        // "X waiting for <loaded model>", and never a separate section.
        let cd = CooldownRow(evicteeModel: "cq27", nextModel: "cq35", waiting: 5, remainingSeconds: 581,
                             slots: [HotSlotRow(slot: 0, sessionId: "17426df4-aaaa", idleSeconds: 31),
                                     HotSlotRow(slot: 1, sessionId: "", idleSeconds: 0)])
        XCTAssertEqual(MenuState.cooldownLabel(cd, next: "cq35"),
                       "cooldown 9:41 for [17426df4], then cq35 · 5 waiting")
        XCTAssertEqual(MenuState.cooldownLabel(cd, next: "cq35", restartedAgo: 31),
                       "cooldown 9:41 (restarted 0:31 ago) for [17426df4], then cq35 · 5 waiting")
        let nobody = CooldownRow(evicteeModel: "cq35", nextModel: "cq27", waiting: 1, remainingSeconds: 9, slots: [])
        XCTAssertEqual(MenuState.cooldownLabel(nobody, next: "cq27"),
                       "cooldown 0:09, then cq27 · 1 waiting")
    }

    func testOnlyOwnedSlotsAreListedUnderTheCooldown() {
        // A free slot is protected by nothing; listing it read as two slots
        // in cooldown (2026-09-10 screenshot: "slot 0 hot", "slot 1 free").
        let cd = CooldownRow(evicteeModel: "cq27", nextModel: "cq35", waiting: 1, remainingSeconds: 60,
                             slots: [HotSlotRow(slot: 0, sessionId: "17426df4-aaaa", idleSeconds: 31),
                                     HotSlotRow(slot: 1, sessionId: "", idleSeconds: 0)])
        let hot = MenuState.hotSlots(cd)
        XCTAssertEqual(hot.map(\.slot), [0])
        XCTAssertEqual(MenuState.hotSlotLabel(hot[0]), "  slot 0 · [17426df4] · hot, idle 0:31")
    }

    func testRestartIsACountdownThatWentUp() {
        // 8:52 -> 9:41 between two screenshots: the resident finished a turn
        // inside its grace. A one-second wobble is not a restart.
        XCTAssertTrue(MenuState.cooldownRestarted(previous: 532, current: 581))
        XCTAssertFalse(MenuState.cooldownRestarted(previous: 532, current: 531))
        XCTAssertFalse(MenuState.cooldownRestarted(previous: 532, current: 533))
        XCTAssertFalse(MenuState.cooldownRestarted(previous: nil, current: 600))
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
