import XCTest
@testable import LlamaSwapMenuCore

/// Pins the menu bar as a pure consumer of the session-state contract
/// (llama-cm docs/intent/session-state-contract.md, schema
/// "llama-swap.sessions/v1"). The three fixtures are the canonical bodies
/// the contract doc ships for client tests, copied verbatim into
/// Fixtures/ - no field here is edited by hand.
///
/// What this file pins, one per contract requirement in the dispatch:
///   - rows render sessionShort/alias/phase/context/progress/rate/priority
///     straight from the wire body (testRowsRenderTheFixtureFieldsVerbatim,
///     testHotAndParkedRowsRenderTheirOwnFields).
///   - the waiting counter comes from body.queue, and is naturally in parity
///     with the PARKED rows because both come from the same decode
///     (testWaitingCounterParityFromTheSameSnapshot).
///   - priority renders compactly, 0 omitted (testPriorityRendering).
///   - the empty box decodes to zero rows and zero waiting
///     (testEmptyBoxDecodesToNoRowsAndNoWaiting).
///   - unknown fields, an unknown phase and an unknown parkReason never
///     fail decoding and render verbatim (invariant 5).
final class SessionStateContractTests: XCTestCase {

    private func fixture(_ name: String) -> Data {
        let dir = URL(fileURLWithPath: String(#filePath)).deletingLastPathComponent()
        let url = dir.appendingPathComponent("Fixtures/\(name).json")
        return try! Data(contentsOf: url)
    }

    private func decode(_ name: String) throws -> SessionsSnapshot {
        try JSONDecoder().decode(SessionsSnapshot.self, from: fixture(name))
    }

    // MARK: - rows render straight from the contract

    func testRowsRenderTheFixtureFieldsVerbatim() throws {
        let snapshot = try decode("prefill-cache-resumed")
        let rows = snapshot.sessions.map(SessionRow.init(contract:))
        XCTAssertEqual(rows.count, 1)
        let row = rows[0]

        XCTAssertEqual(row.sessionShort, "69699f8b")
        XCTAssertEqual(row.alias, "cq27")
        XCTAssertEqual(row.phase, "PREFILL")
        XCTAssertEqual(row.context.used, 92170)
        XCTAssertEqual(row.context.window, 262144)
        XCTAssertEqual(row.rate.kind, "prefill")
        XCTAssertEqual(row.rate.tokensPerSecond, 50.7)
        XCTAssertEqual(row.priority, 0)

        // context.used == cached + processed + decoded (invariant 1) - the
        // client never recomputes this, but the fixture's own numbers must
        // add up, or a client rendering `used` verbatim would be lying too.
        XCTAssertEqual(row.context.used, row.context.cached + row.context.processed + row.context.decoded)

        XCTAssertEqual(row.displayLine,
                       "[69699f8b] cq27 · PREFILL · 92.2k/262.1k · 90.0% · prefill 50.7 t/s",
                       "every segment must come straight from the body: k-units, one-decimal "
                       + "percent and rate are formatting, not derivation")
    }

    func testHotAndParkedRowsRenderTheirOwnFields() throws {
        let snapshot = try decode("decode-parked-hot")
        let rows = snapshot.sessions.map(SessionRow.init(contract:))
        XCTAssertEqual(rows.count, 3)

        let parked = try XCTUnwrap(rows.first { $0.phase == "PARKED" })
        XCTAssertEqual(parked.parkReason, "kv")
        XCTAssertEqual(parked.displayLine, "[a1b2c3d4] cq27 · PARKED (kv pool) · 0/262.1k · P10")

        let decode = try XCTUnwrap(rows.first { $0.phase == "DECODE" })
        XCTAssertEqual(decode.displayLine, "[69699f8b] cq27 · DECODE · 102.6k/262.1k · decode 7.1 t/s")

        let hot = try XCTUnwrap(rows.first { $0.phase == "HOT" })
        // HOT carries no rate and no progress: neither segment appears, and
        // nothing here invents one.
        XCTAssertEqual(hot.displayLine, "[0f0e0d0c] cq27 · HOT · 31.8k/262.1k")
    }

    // MARK: - the waiting counter comes from body.queue

    func testWaitingCounterParityFromTheSameSnapshot() throws {
        let snapshot = try decode("decode-parked-hot")
        // The contract's own count.
        XCTAssertEqual(snapshot.queue.waiting, 1)
        XCTAssertEqual(snapshot.queue.byTier["priority"], 1)

        // The rows built from the SAME decode: parity holds because there is
        // only one source, not because a client recomputed a matching number.
        let rows = snapshot.sessions.map(SessionRow.init(contract:))
        let parkedCount = rows.filter { $0.phase == "PARKED" }.count
        XCTAssertEqual(snapshot.queue.waiting, parkedCount)
    }

    // MARK: - priority rendering

    func testPriorityRendering() throws {
        let snapshot = try decode("decode-parked-hot")
        let rows = snapshot.sessions.map(SessionRow.init(contract:))

        let priorityTen = try XCTUnwrap(rows.first { $0.phase == "PARKED" })
        XCTAssertEqual(priorityTen.priority, 10)
        XCTAssertTrue(priorityTen.displayLine.hasSuffix("P10"), "P10 must render, got '\(priorityTen.displayLine)'")

        // Default-tier priority 0 is the common case; printing "P0" on every
        // row would be constant noise for zero information, so it is
        // omitted rather than shown.
        let defaultTier = try XCTUnwrap(rows.first { $0.phase == "DECODE" })
        XCTAssertEqual(defaultTier.priority, 0)
        XCTAssertFalse(defaultTier.displayLine.contains("P0"),
                       "priority 0 (the default tier) must not print a P0 segment")

        // A background-tier session (negative priority) renders "P-10", not
        // dropped and not "P10" - constructed inline (not a fixture edit)
        // since neither canonical fixture carries a negative priority.
        let backgroundJSON = """
        {"sessionId":"ee000000-0000-0000-0000-000000000000","sessionShort":"ee000000",
         "requestId":null,"model":"cq27","alias":"cq27","tier":"background","priority":-10,
         "phase":"IDLE","parkReason":null,"slot":null,
         "context":{"used":100,"cached":100,"processed":0,"decoded":0,"promptTotal":100,"window":262144},
         "progress":null,"rate":{"kind":null,"tokensPerSecond":null,"windowSeconds":30.0},
         "elapsedMs":0,"phaseSinceMs":0,"respTokens":0}
        """.data(using: .utf8)!
        let background = try JSONDecoder().decode(ContractSession.self, from: backgroundJSON)
        let backgroundRow = SessionRow(contract: background)
        XCTAssertTrue(backgroundRow.displayLine.hasSuffix("P-10"),
                      "a background (negative) priority must render as P-10, got '\(backgroundRow.displayLine)'")
    }

    // MARK: - empty box

    func testEmptyBoxDecodesToNoRowsAndNoWaiting() throws {
        let snapshot = try decode("empty-box")
        XCTAssertNil(snapshot.resident)
        XCTAssertEqual(snapshot.sessions.count, 0)
        XCTAssertEqual(snapshot.queue.waiting, 0)

        let rows = snapshot.sessions.map(SessionRow.init(contract:))
        XCTAssertTrue(rows.isEmpty)
        XCTAssertEqual(MenuState.queueSummary(rows), "Queue: idle")
    }

    // MARK: - invariant 5: unknown fields, unknown phase/parkReason

    /// An unrecognized top-level key and an unrecognized session-entry key
    /// must not fail decoding - Swift's synthesized Decodable already drops
    /// them, this just pins that it stays true for this type.
    func testUnknownFieldsAreIgnoredNotFatal() throws {
        let json = """
        {"schema":"llama-swap.sessions/v1","generatedAt":"2026-09-18T00:00:00Z",
         "resident":null,"queue":{"waiting":0,"byTier":{}},"cooldown":null,
         "memoryBrake":{"enabled":true,"holding":false,"remainingSeconds":0},
         "aFieldFromTheFuture":42,
         "sessions":[{"sessionId":"x","sessionShort":"x","requestId":null,"model":"m","alias":"a",
         "tier":"default","priority":0,"phase":"PREFILL","parkReason":null,"slot":null,
         "context":{"used":0,"cached":0,"processed":0,"decoded":0,"promptTotal":0,"window":1},
         "progress":null,"rate":{"kind":null,"tokensPerSecond":null,"windowSeconds":30.0},
         "elapsedMs":0,"phaseSinceMs":0,"respTokens":0,"aSessionFieldFromTheFuture":"ignored"}]}
        """.data(using: .utf8)!
        let snapshot = try JSONDecoder().decode(SessionsSnapshot.self, from: json)
        XCTAssertEqual(snapshot.sessions.count, 1)
    }

    /// A phase the client predates must still decode and render exactly as
    /// given, never rejected and never guessed at.
    func testUnknownPhaseRendersVerbatim() throws {
        let json = """
        {"sessionId":"y","sessionShort":"yyyyyyyy","requestId":null,"model":"m","alias":"a",
         "tier":"default","priority":0,"phase":"WARMING","parkReason":null,"slot":null,
         "context":{"used":0,"cached":0,"processed":0,"decoded":0,"promptTotal":0,"window":1},
         "progress":null,"rate":{"kind":null,"tokensPerSecond":null,"windowSeconds":30.0},
         "elapsedMs":0,"phaseSinceMs":0,"respTokens":0}
        """.data(using: .utf8)!
        let entry = try JSONDecoder().decode(ContractSession.self, from: json)
        let row = SessionRow(contract: entry)
        XCTAssertEqual(row.phase, "WARMING")
        XCTAssertTrue(row.displayLine.contains("WARMING"))
    }
}
