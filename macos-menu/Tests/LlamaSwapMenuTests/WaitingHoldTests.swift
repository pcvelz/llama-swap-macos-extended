import XCTest
@testable import LlamaSwapMenuCore

/// The anti-flap display hold: waiting counts never flap 0↔1 on short
/// requests (llama-cm docs/intent/llama-swap-backend.md § Slot stability).
final class WaitingHoldTests: XCTestCase {

    func testDropIsHeldThenExpires() {
        var state = MenuState()
        let t0 = Date()

        state.applyInflight(total: 1, byTier: ["default": 1, "priority": 0], now: t0)
        XCTAssertEqual(state.waitingByTier["default"], 1)

        // Short request finishes: raw drops to 0 but the display holds the peak.
        state.applyInflight(total: 0, byTier: ["default": 0, "priority": 0], now: t0.addingTimeInterval(1))
        XCTAssertEqual(state.waiting, 1)
        XCTAssertEqual(state.waitingByTier["default"], 1)

        // After the hold expires without a re-peak, the drop applies.
        let later = t0.addingTimeInterval(MenuState.waitingHold + 2)
        state.applyInflight(total: 0, byTier: ["default": 0, "priority": 0], now: later)
        XCTAssertEqual(state.waiting, 0)
        XCTAssertEqual(state.waitingByTier["default"], 0)
    }
}
