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
///   - the tier renders by NAME (priority / background), default omitted
///     (testTierRendering); PARKED rows keep the server's queue order and
///     no separate queue-order line exists (testParkedRowsKeepServerOrder,
///     testMenuHasNoQueueSummaryLine).
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
        XCTAssertEqual(parked.displayLine, "[a1b2c3d4] cq27 · PARKED (kv pool) · 0/262.1k · priority")

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

    // MARK: - tier rendering

    private func row(tier: String, priority: Int, alias: String = "cq27", short: String = "ee000000",
                     phase: String = "PARKED") throws -> SessionRow {
        let json = """
        {"sessionId":"\(short)-0000-0000-0000-000000000000","sessionShort":"\(short)",
         "requestId":null,"model":"\(alias)","alias":"\(alias)","tier":"\(tier)","priority":\(priority),
         "phase":"\(phase)","parkReason":null,"slot":null,
         "context":{"used":100,"cached":100,"processed":0,"decoded":0,"promptTotal":100,"window":262144},
         "progress":null,"rate":{"kind":null,"tokensPerSecond":null,"windowSeconds":30.0},
         "elapsedMs":0,"phaseSinceMs":0,"respTokens":0}
        """.data(using: .utf8)!
        return SessionRow(contract: try JSONDecoder().decode(ContractSession.self, from: json))
    }

    func testTierRendering() throws {
        let snapshot = try decode("decode-parked-hot")
        let rows = snapshot.sessions.map(SessionRow.init(contract:))

        // There are exactly three tiers; a raw rank like "P10" reads as if
        // there were more layers, so the tier NAME renders instead.
        let priorityTier = try XCTUnwrap(rows.first { $0.phase == "PARKED" })
        XCTAssertEqual(priorityTier.tier, "priority")
        XCTAssertTrue(priorityTier.displayLine.hasSuffix(" · priority"),
                      "tier name must render, got '\(priorityTier.displayLine)'")
        XCTAssertNil(priorityTier.displayLine.range(of: #"\bP-?\d+\b"#, options: .regularExpression),
                     "no P<rank> token may render, got '\(priorityTier.displayLine)'")

        // The default tier is the common case: nothing rendered for it.
        let defaultTier = try XCTUnwrap(rows.first { $0.phase == "DECODE" })
        XCTAssertFalse(defaultTier.displayLine.contains("default"))
        XCTAssertFalse(defaultTier.displayLine.contains("P0"))

        let background = try row(tier: "background", priority: -10, phase: "IDLE")
        XCTAssertTrue(background.displayLine.hasSuffix(" · background"),
                      "got '\(background.displayLine)'")
        XCTAssertNil(background.displayLine.range(of: #"\bP-?\d+\b"#, options: .regularExpression),
                     "no P-10 token may render, got '\(background.displayLine)'")
    }

    /// The tier field decides, not the number: a default-tier row with an
    /// unexpected nonzero rank prints no rank, and an unknown tier name
    /// renders verbatim (invariant 5).
    func testTierNameIsDerivedFromTierNotPriority() throws {
        let oddRank = try row(tier: "default", priority: 10)
        XCTAssertFalse(oddRank.displayLine.contains("P10"))
        XCTAssertFalse(oddRank.displayLine.contains("priority"),
                       "default tier must not render as priority just because rank is 10")
        let unknown = try row(tier: "batch", priority: -5)
        XCTAssertTrue(unknown.displayLine.hasSuffix(" · batch"), "got '\(unknown.displayLine)'")
    }

    // MARK: - queue order is the row order

    /// The separate "1. a, 2. b" queue line is gone: the PARKED rows are the
    /// queue, shown in the server's order (first in line on top) - the menu
    /// must not re-sort them.
    func testParkedRowsKeepServerOrder() throws {
        let first = try row(tier: "priority", priority: 10, alias: "cq35", short: "aaaaaaaa")
        let second = try row(tier: "background", priority: -10, alias: "cq27", short: "bbbbbbbb")
        let third = try row(tier: "background", priority: -10, alias: "cq35", short: "cccccccc")

        var state = MenuState()
        state.sessionRows = [first, second, third]
        XCTAssertEqual(state.sessionRows.map(\.sessionShort), ["aaaaaaaa", "bbbbbbbb", "cccccccc"])

        // Through the real decode path the order is the wire order too.
        let snapshot = try decode("decode-parked-hot")
        let wire = snapshot.sessions.map(\.sessionShort)
        XCTAssertEqual(snapshot.sessions.map(SessionRow.init(contract:)).map(\.sessionShort), wire)
    }

    /// The parked list as shown IS the pick order: the server's sessions[]
    /// order (tier rank descending, then the order the scheduler will grant),
    /// so the top row is the next request a slot takes. Background rows here
    /// arrived FIRST (largest elapsedMs) and are listed LAST by the server; a
    /// menu that re-sorted by age, alias, id or tier name would reorder them.
    func testParkedRowsRenderInServerPickOrder() throws {
        func session(_ short: String, _ alias: String, _ tier: String, _ priority: Int, _ elapsedMs: Int) -> String {
            """
            {"sessionId":"\(short)-0000-0000-0000-000000000000","sessionShort":"\(short)",
             "requestId":null,"model":"\(alias)","alias":"\(alias)","tier":"\(tier)","priority":\(priority),
             "phase":"PARKED","parkReason":"cap","slot":null,
             "context":{"used":0,"cached":0,"processed":0,"decoded":0,"promptTotal":0,"window":262144},
             "progress":null,"rate":{"kind":null,"tokensPerSecond":null,"windowSeconds":30.0},
             "elapsedMs":\(elapsedMs),"phaseSinceMs":0,"respTokens":0}
            """
        }
        // Server order: priority, default, default, background, background.
        // Names sort the opposite way alphabetically (z.. first) and the
        // background rows are the oldest, so any client-side sort shows.
        let body = """
        {"schema":"llama-swap.sessions/v1","generatedAt":"2026-09-19T00:00:00Z",
         "resident":null,"queue":{"waiting":5,"byTier":{"priority":1,"default":2,"background":2}},
         "cooldown":null,
         "sessions":[
           \(session("zzzzzzzz", "cq35", "priority", 10, 1_000)),
           \(session("yyyyyyyy", "cq27", "default", 0, 9_000)),
           \(session("xxxxxxxx", "cq35", "default", 0, 4_000)),
           \(session("bbbbbbbb", "cq27", "background", -10, 90_000)),
           \(session("aaaaaaaa", "cq35", "background", -10, 80_000))
         ]}
        """.data(using: .utf8)!
        let snapshot = try JSONDecoder().decode(SessionsSnapshot.self, from: body)
        let serverOrder = snapshot.sessions.map(\.sessionShort)

        var state = MenuState()
        state.sessionRows = snapshot.sessions.map(SessionRow.init(contract:))
        // What MenuView's ForEach walks, top to bottom.
        let shown = state.sessionRows.filter { $0.phase == "PARKED" }

        XCTAssertEqual(shown.map(\.sessionShort), serverOrder, "menu must render sessions[] order as-is")
        XCTAssertEqual(shown.map(\.tier),
                       ["priority", "default", "default", "background", "background"],
                       "higher tiers on top, background last")
        XCTAssertEqual(shown.first?.sessionShort, "zzzzzzzz", "top row is the next request a slot takes")
        XCTAssertEqual(shown.map(\.displayLine), [
            "[zzzzzzzz] cq35 · PARKED (slots full) · 0/262.1k · priority",
            "[yyyyyyyy] cq27 · PARKED (slots full) · 0/262.1k",
            "[xxxxxxxx] cq35 · PARKED (slots full) · 0/262.1k",
            "[bbbbbbbb] cq27 · PARKED (slots full) · 0/262.1k · background",
            "[aaaaaaaa] cq35 · PARKED (slots full) · 0/262.1k · background",
        ])
    }

    /// MenuView is SwiftUI and cannot be rendered in a unit test, so this is
    /// a source guard: the view must not build a queue-order summary line.
    func testMenuHasNoQueueSummaryLine() throws {
        let dir = URL(fileURLWithPath: String(#filePath))
            .deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
        let view = try String(contentsOf: dir.appendingPathComponent("Sources/LlamaSwapMenuCore/MenuView.swift"))
        let state = try String(contentsOf: dir.appendingPathComponent("Sources/LlamaSwapMenuCore/MenuState.swift"))
        XCTAssertFalse(view.contains("queueSummary"), "MenuView must not render a queue-order line")
        XCTAssertFalse(state.contains("queueSummary"), "queueSummary must be removed")
        XCTAssertFalse(view.contains("Queue: idle"), "no dangling idle line")
    }

    // MARK: - empty box

    func testEmptyBoxDecodesToNoRowsAndNoWaiting() throws {
        let snapshot = try decode("empty-box")
        XCTAssertNil(snapshot.resident)
        XCTAssertEqual(snapshot.sessions.count, 0)
        XCTAssertEqual(snapshot.queue.waiting, 0)

        let rows = snapshot.sessions.map(SessionRow.init(contract:))
        XCTAssertTrue(rows.isEmpty)
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
