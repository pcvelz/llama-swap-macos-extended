import XCTest
@testable import LlamaSwapMenuCore

/// A PARKED row says WHY. `SessionRow.parkPhrase` renders the contract's
/// `parkReason` (session-state-contract.md's documented vocabulary) as a
/// plain phrase after the phase, the way a slot readout follows PREFILL/
/// DECODE. Vocabulary from the by-hand ledger of 2026-09-10 (llama-cm
/// docs/research/2026-09-10-cooldown-dogfood-ledger.md): the user's
/// screenshot row "[9f3c6151] cq27 · PARKED" with no reason was O2's cap park
/// and could not be told from a cooldown park.
final class ParkReasonTests: XCTestCase {

    func testParkPhraseMapsTheDocumentedVocabulary() {
        XCTAssertEqual(SessionRow.parkPhrase("cap"), "slots full")
        XCTAssertEqual(SessionRow.parkPhrase("kv"), "kv pool")
        XCTAssertEqual(SessionRow.parkPhrase("busy"), "resident busy")
        XCTAssertEqual(SessionRow.parkPhrase("cooldown"), "cooldown")
        XCTAssertEqual(SessionRow.parkPhrase("loading"), "loading")
        XCTAssertEqual(SessionRow.parkPhrase("rank"), "behind higher rank")
        XCTAssertEqual(SessionRow.parkPhrase("swap-collision"), "another swap in flight")
        XCTAssertEqual(SessionRow.parkPhrase("memory-brake"), "memory brake")
        XCTAssertNil(SessionRow.parkPhrase(nil))
        XCTAssertNil(SessionRow.parkPhrase(""))
    }

    /// Invariant 5 (session-state-contract.md): an unknown value renders
    /// verbatim, never dropped as if it were absent.
    func testUnknownParkReasonRendersVerbatim() {
        XCTAssertEqual(SessionRow.parkPhrase("some-future-reason"), "some-future-reason")
    }

    func testParkedRowNamesTheReasonInDisplayLine() {
        let row = SessionRow(
            id: "3", sessionShort: "fd998b45", model: "cq35h", alias: "cq35h", tier: "-",
            priority: 0, phase: "PARKED", parkReason: "cap",
            context: ContractContext(used: 0, cached: 0, processed: 0, decoded: 0, promptTotal: 0, window: 262144),
            rate: ContractRate(kind: nil, tokensPerSecond: nil, windowSeconds: 30))
        XCTAssertTrue(row.displayLine.contains("PARKED (slots full)"),
                      "row must name the park reason, got '\(row.displayLine)'")
    }
}
