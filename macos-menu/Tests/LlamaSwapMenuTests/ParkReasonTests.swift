import XCTest
@testable import LlamaSwapMenuCore

/// A PARKED row says WHY - but the wording is the SERVER's. The park-reason
/// vocabulary ("slots full", "kv pool", ...) lives in the fork's `parkPhrase`
/// (internal/server/sessions_line.go) with its own table tests; the dropdown
/// prints the row's `line` verbatim and never maps a reason itself.
final class ParkReasonTests: XCTestCase {

    private func parked(line: String?) -> SessionRow {
        SessionRow(
            id: "3", sessionShort: "fd998b45", model: "cq35h", alias: "cq35h", tier: "-",
            priority: 0, phase: "PARKED", parkReason: "cap",
            context: ContractContext(used: 0, cached: 0, processed: 0, decoded: 0, promptTotal: 0, window: 262144),
            rate: ContractRate(kind: nil, tokensPerSecond: nil, windowSeconds: 30),
            line: line)
    }

    func testParkedRowPrintsTheServerLineVerbatim() {
        let line = "[fd998b45] cq35h · PARKED (slots full) · 0/262.1k · - P0"
        XCTAssertEqual(parked(line: line).displayLine, line)
    }

    /// An older server sends no `line`: the minimal fallback names the raw
    /// phase only - the client does not translate the park reason.
    func testParkedRowWithoutServerLineFallsBackToTheRawPhase() {
        XCTAssertEqual(parked(line: nil).displayLine, "[fd998b45] cq35h · PARKED")
    }
}
