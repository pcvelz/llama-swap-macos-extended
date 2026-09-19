import XCTest
@testable import LlamaSwapMenuCore

/// The memory brake's admission gate in the menu (llama-cm incident
/// 2026-09-18, Event 8): after a brake kill, local-model loads are held until
/// file-backed memory drains below `drainBelowGB`. The menu says so, with the
/// live value, straight from the contract's `memoryBrake` - and says nothing
/// when the gate is open.
final class MemoryBrakeHoldTests: XCTestCase {

    private func snapshot(memoryBrake: String) throws -> SessionsSnapshot {
        let json = """
        {"schema":"llama-swap.sessions/v1","generatedAt":"2026-09-19T14:45:00Z",
         "resident":null,"queue":{"waiting":0,"byTier":{}},"cooldown":null,
         "memoryBrake":\(memoryBrake),"sessions":[]}
        """.data(using: .utf8)!
        return try JSONDecoder().decode(SessionsSnapshot.self, from: json)
    }

    func testHoldingRendersTheDrainGateWithTheLiveValue() throws {
        let s = try snapshot(memoryBrake: """
        {"enabled":true,"holding":true,"remainingSeconds":0,"fileBackedGB":36.12,"drainBelowGB":10}
        """)
        XCTAssertEqual(MenuState.memoryBrakeLabel(s.memoryBrake),
                       "Memory brake: loads held until file-backed < 10 GB (now 36.1 GB)")
    }

    func testOpenGateRendersNothing() throws {
        let s = try snapshot(memoryBrake: """
        {"enabled":true,"holding":false,"remainingSeconds":0}
        """)
        XCTAssertNil(MenuState.memoryBrakeLabel(s.memoryBrake))
        XCTAssertNil(MenuState.memoryBrakeLabel(nil))
    }

    /// A server from before the drain gate sends no drain fields: still
    /// decodes, and a hold still shows (without numbers).
    func testOlderServerWithoutDrainFieldsStillShowsTheHold() throws {
        let s = try snapshot(memoryBrake: """
        {"enabled":true,"holding":true,"remainingSeconds":125}
        """)
        XCTAssertEqual(MenuState.memoryBrakeLabel(s.memoryBrake), "Memory brake: loads held")
    }
}
