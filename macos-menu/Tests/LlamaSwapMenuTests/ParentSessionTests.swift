// @user-gated: user ruling - every row shows tier + rank and a dispatch shows its parent session

import XCTest
@testable import LlamaSwapMenuCore

/// Pins the parent-session display contract: when a dispatch carries
/// X-Claude-Code-Parent-Session-Id, the bracket reads `[<parentShort> >
/// <childShort>]`; without it the bracket stays `[<sessionShort>]`. Also
/// verifies that every row shape (including PENALIZED) ends with the tier +
/// rank segment.
final class ParentSessionTests: XCTestCase {

    // MARK: - Bracket format

    func testDirectRowBracketWithoutParent() {
        let row = SessionRow(
            id: "r-1", sessionId: "sess-aaaaaaaa", sessionShort: "aaaaaaaa",
            parentSessionShort: nil, model: "cq35", alias: "cq35", tier: "default",
            priority: 0, phase: "DECODE",
            context: ContractContext(used: 50_000, cached: 40_000, processed: 10_000, decoded: 1200, promptTotal: 100_000, window: 262_144),
            rate: ContractRate(kind: "decode", tokensPerSecond: 45.2, windowSeconds: 30))
        XCTAssertEqual(row.displayLine, "[aaaaaaaa] cq35 · DECODE · 50.0k/262.1k · decode 45.2 t/s · default P0")
    }

    func testDispatchedRowBracketWithParent() {
        let row = SessionRow(
            id: "r-dispatch", sessionId: "child-0001", sessionShort: "child001",
            parentSessionShort: "parent01", model: "cq35", alias: "cq35", tier: "default",
            priority: 0, phase: "PARKED", parkReason: "kv",
            context: ContractContext(used: 0, cached: 0, processed: 0, decoded: 0, promptTotal: 0, window: 262_144),
            rate: ContractRate(kind: nil, tokensPerSecond: nil, windowSeconds: 30))
        XCTAssertEqual(row.displayLine, "[parent01 > child001] cq35 · PARKED (kv pool) · 0/262.1k · default P0")
    }

    func testDispatchedRowBracketWithShortParent() {
        let row = SessionRow(
            id: "r-dispatch", sessionId: "child-0002", sessionShort: "child002",
            parentSessionShort: "p1234567", model: "cq27", alias: "cq27", tier: "priority",
            priority: 10, phase: "LOADING",
            context: ContractContext(used: 0, cached: 0, processed: 0, decoded: 0, promptTotal: 0, window: 262_144),
            rate: ContractRate(kind: nil, tokensPerSecond: nil, windowSeconds: 30))
        XCTAssertEqual(row.displayLine, "[p1234567 > child002] cq27 · LOADING · 0/262.1k · priority P10")
    }

    // MARK: - PENALIZED shape includes tier + rank

    func testPenalizedRowShowsTierAndRank() {
        let row = SessionRow(
            id: "x", sessionId: "sess-penalized", sessionShort: "penalized",
            parentSessionShort: nil, model: "cq35", alias: "cq35", tier: "default",
            priority: 0, phase: "PENALIZED",
            context: ContractContext(used: 200_000, cached: 0, processed: 0, decoded: 0, promptTotal: 0, window: 262_144),
            rate: ContractRate(kind: nil, tokensPerSecond: nil, windowSeconds: 30),
            penalty: PenaltyInfo(reason: "loop", strike: 2, strikes: 3, remainingSeconds: 760, uniformRun: 86, typicalTokens: 48))
        XCTAssertTrue(row.displayLine.hasSuffix("· default P0"))
    }

    func testPenalizedRowWithParentShowsBracketAndTierRank() {
        let row = SessionRow(
            id: "x", sessionId: "child-0003", sessionShort: "child003",
            parentSessionShort: "parent02", model: "cq35", alias: "cq35", tier: "default",
            priority: 0, phase: "PENALIZED",
            context: ContractContext(used: 200_000, cached: 0, processed: 0, decoded: 0, promptTotal: 0, window: 262_144),
            rate: ContractRate(kind: nil, tokensPerSecond: nil, windowSeconds: 30),
            penalty: PenaltyInfo(reason: "loop", strike: 3, strikes: 3, remainingSeconds: nil, uniformRun: 86, typicalTokens: 48))
        XCTAssertTrue(row.displayLine.hasPrefix("[parent02 > child003]"))
        XCTAssertTrue(row.displayLine.hasSuffix("· held · default P0"))
    }

    // MARK: - ContractSession decoding

    func testContractSessionDecodesParentFields() throws {
        let json = """
        {
            "sessionId": "child-uuid-0001",
            "sessionShort": "child001",
            "requestId": "r-dispatch",
            "model": "cq35",
            "alias": "cq35",
            "tier": "default",
            "priority": 0,
            "phase": "PARKED",
            "parkReason": null,
            "slot": null,
            "context": {"used": 0, "cached": 0, "processed": 0, "decoded": 0, "promptTotal": 0, "window": 262144},
            "progress": null,
            "rate": {"kind": null, "tokensPerSecond": null, "windowSeconds": 30},
            "elapsedMs": 120000,
            "phaseSinceMs": 120000,
            "respTokens": 0,
            "parentSessionId": "parent-uuid-full",
            "parentSessionShort": "parent01"
        }
        """
        let data = json.data(using: .utf8)!
        let session = try JSONDecoder().decode(ContractSession.self, from: data)
        XCTAssertEqual(session.parentSessionId, "parent-uuid-full")
        XCTAssertEqual(session.parentSessionShort, "parent01")
    }

    func testContractSessionIgnoresParentFieldsWhenAbsent() throws {
        let json = """
        {
            "sessionId": "direct-session-id",
            "sessionShort": "direct01",
            "requestId": null,
            "model": "cq27",
            "alias": "cq27",
            "tier": "default",
            "priority": 0,
            "phase": "DECODE",
            "parkReason": null,
            "slot": 0,
            "context": {"used": 50000, "cached": 40000, "processed": 10000, "decoded": 1200, "promptTotal": 100000, "window": 262144},
            "progress": null,
            "rate": {"kind": "decode", "tokensPerSecond": 45.2, "windowSeconds": 30},
            "elapsedMs": 60000,
            "phaseSinceMs": 60000,
            "respTokens": 1200
        }
        """
        let data = json.data(using: .utf8)!
        let session = try JSONDecoder().decode(ContractSession.self, from: data)
        XCTAssertNil(session.parentSessionId)
        XCTAssertNil(session.parentSessionShort)
    }

    // MARK: - SessionRow init(contract:) propagates parent

    func testContractInitPropagatesParentFields() throws {
        let json = """
        {
            "sessionId": "child-uuid-0002",
            "sessionShort": "child002",
            "requestId": "r-dispatch",
            "model": "cq35",
            "alias": "cq35",
            "tier": "default",
            "priority": 0,
            "phase": "PARKED",
            "parkReason": null,
            "slot": null,
            "context": {"used": 0, "cached": 0, "processed": 0, "decoded": 0, "promptTotal": 0, "window": 262144},
            "progress": null,
            "rate": {"kind": null, "tokensPerSecond": null, "windowSeconds": 30},
            "elapsedMs": 120000,
            "phaseSinceMs": 120000,
            "respTokens": 0,
            "parentSessionId": "parent-uuid-full",
            "parentSessionShort": "parent02"
        }
        """
        let data = json.data(using: .utf8)!
        let contract = try JSONDecoder().decode(ContractSession.self, from: data)
        let row = SessionRow(contract: contract)
        XCTAssertEqual(row.parentSessionShort, "parent02")
        XCTAssertTrue(row.displayLine.hasPrefix("[parent02 > child002]"))
    }

    // MARK: - Empty bracket fallback

    func testEmptySessionShortShowsDash() {
        let row = SessionRow(
            id: "req-anonymous", sessionId: "", sessionShort: "",
            parentSessionShort: nil, model: "cq35", alias: "cq35", tier: "default",
            priority: 0, phase: "IDLE",
            context: ContractContext(used: 0, cached: 0, processed: 0, decoded: 0, promptTotal: 0, window: 262_144),
            rate: ContractRate(kind: nil, tokensPerSecond: nil, windowSeconds: 30))
        XCTAssertTrue(row.displayLine.hasPrefix("[-]"))
    }

    func testDispatchedAnonymousShowsParentWithDashChild() {
        let row = SessionRow(
            id: "req-anon-dispatch", sessionId: "", sessionShort: "",
            parentSessionShort: "parent03", model: "cq35", alias: "cq35", tier: "default",
            priority: 0, phase: "PARKED", parkReason: "rank",
            context: ContractContext(used: 0, cached: 0, processed: 0, decoded: 0, promptTotal: 0, window: 262_144),
            rate: ContractRate(kind: nil, tokensPerSecond: nil, windowSeconds: 30))
        XCTAssertEqual(row.displayLine, "[parent03 > -] cq35 · PARKED (behind higher rank) · 0/262.1k · default P0")
    }
}
