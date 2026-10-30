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
///   - EVERY row shows its tier NAME and its rank, including the default
///     tier ("default P0" / "priority P10" / "background P-10") - user
///     ruling 2026-09-25, restoring what 40db173 removed (testTierRendering);
///     PARKED rows keep the server's queue order (testParkedRowsKeepServerOrder,
///     testParkedRowsRenderInServerPickOrder) and the "Queue: idle" /
///     numbered summary line is back (testQueueSummaryLine).
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
                       "[69699f8b] cq27 · PREFILL · 92.2k/262.1k · 90.0% · 50.7 t/s · default P0",
                       "every segment must come straight from the body: k-units, one-decimal "
                       + "percent and rate are formatting, not derivation")
    }

    func testHotAndParkedRowsRenderTheirOwnFields() throws {
        let snapshot = try decode("decode-parked-hot")
        let rows = snapshot.sessions.map(SessionRow.init(contract:))
        XCTAssertEqual(rows.count, 3)

        let parked = try XCTUnwrap(rows.first { $0.phase == "PARKED" })
        XCTAssertEqual(parked.parkReason, "kv")
        XCTAssertEqual(parked.displayLine, "[a1b2c3d4] cq27 · PARKED (kv pool) · 0/262.1k · priority P10")

        let decode = try XCTUnwrap(rows.first { $0.phase == "DECODE" })
        XCTAssertEqual(decode.displayLine, "[69699f8b] cq27 · DECODE · 102.6k/262.1k · 7.1 t/s · default P0")

        let hot = try XCTUnwrap(rows.first { $0.phase == "HOT" })
        // HOT carries no rate and no progress: neither segment appears, and
        // nothing here invents one.
        XCTAssertEqual(hot.displayLine, "[0f0e0d0c] cq27 · HOT · 31.8k/262.1k · default P0")
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
        {"line":"[\(short)] \(alias) · \(phase) · 100/262.1k · \(tier) P\(priority)",
         "sessionId":"\(short)-0000-0000-0000-000000000000","sessionShort":"\(short)",
         "requestId":null,"model":"\(alias)","alias":"\(alias)","tier":"\(tier)","priority":\(priority),
         "phase":"\(phase)","parkReason":null,"slot":null,
         "context":{"used":100,"cached":100,"processed":0,"decoded":0,"promptTotal":100,"window":262144},
         "progress":null,"rate":{"kind":null,"tokensPerSecond":null,"windowSeconds":30.0},
         "elapsedMs":0,"phaseSinceMs":0,"respTokens":0}
        """.data(using: .utf8)!
        return SessionRow(contract: try JSONDecoder().decode(ContractSession.self, from: json))
    }

    // @user-gated 2026-09-25: user ruling - every row shows tier + rank incl. default, and the queue summary line stays. Removed once by 40db173; do not change without the user.
    func testTierRendering() throws {
        let snapshot = try decode("decode-parked-hot")
        let rows = snapshot.sessions.map(SessionRow.init(contract:))

        // Every row shows BOTH the tier name and its rank, including the
        // default tier - a "<tier> P<priority>" segment is never dropped.
        let priorityTier = try XCTUnwrap(rows.first { $0.phase == "PARKED" })
        XCTAssertEqual(priorityTier.tier, "priority")
        XCTAssertEqual(priorityTier.priority, 10)
        XCTAssertTrue(priorityTier.displayLine.hasSuffix(" · priority P10"),
                      "tier + rank must render, got '\(priorityTier.displayLine)'")

        // The default tier renders too: "default P0", never omitted.
        let defaultTier = try XCTUnwrap(rows.first { $0.phase == "DECODE" })
        XCTAssertEqual(defaultTier.tier, "default")
        XCTAssertEqual(defaultTier.priority, 0)
        XCTAssertTrue(defaultTier.displayLine.hasSuffix(" · default P0"),
                      "default tier must render its own row too, got '\(defaultTier.displayLine)'")

        let background = try row(tier: "background", priority: -10, phase: "IDLE")
        XCTAssertTrue(background.displayLine.hasSuffix(" · background P-10"),
                      "got '\(background.displayLine)'")
    }

    /// @user-gated 2026-09-25: user ruling - every row shows tier + rank incl. default, and the queue summary line stays. Removed once by 40db173; do not change without the user.
    /// An unknown tier name still renders verbatim, with its own priority
    /// (invariant 5) - the segment is always "<tier> P<priority>" straight
    /// from the contract fields, never derived or filtered.
    func testUnknownTierRendersVerbatimWithItsRank() throws {
        let unknown = try row(tier: "batch", priority: -5)
        XCTAssertTrue(unknown.displayLine.hasSuffix(" · batch P-5"), "got '\(unknown.displayLine)'")
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
            {"line":"[\(short)] \(alias) · PARKED (slots full) · 0/262.1k · \(tier) P\(priority)",
             "sessionId":"\(short)-0000-0000-0000-000000000000","sessionShort":"\(short)",
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
            "[zzzzzzzz] cq35 · PARKED (slots full) · 0/262.1k · priority P10",
            "[yyyyyyyy] cq27 · PARKED (slots full) · 0/262.1k · default P0",
            "[xxxxxxxx] cq35 · PARKED (slots full) · 0/262.1k · default P0",
            "[bbbbbbbb] cq27 · PARKED (slots full) · 0/262.1k · background P-10",
            "[aaaaaaaa] cq35 · PARKED (slots full) · 0/262.1k · background P-10",
        ])
    }

    // @user-gated 2026-09-25: user ruling - every row shows tier + rank incl. default, and the queue summary line stays. Removed once by 40db173; do not change without the user.
    /// "Queue: idle" when nothing is parked, else the parked rows in server
    /// order as "1. <tier>/<alias>, 2. ...". Restored after 40db173 removed
    /// it; MenuView must actually render it too (checked via source guard,
    /// SwiftUI views cannot be rendered in a unit test).
    func testQueueSummaryLine() throws {
        XCTAssertEqual(MenuState.queueSummary([]), "Queue: idle")

        let first = try row(tier: "priority", priority: 10, alias: "cq35", short: "aaaaaaaa")
        let second = try row(tier: "background", priority: -10, alias: "cq27", short: "bbbbbbbb")
        XCTAssertEqual(MenuState.queueSummary([first, second]), "1. priority/cq35, 2. background/cq27")

        let dir = URL(fileURLWithPath: String(#filePath))
            .deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
        let view = try String(contentsOf: dir.appendingPathComponent("Sources/LlamaSwapMenuCore/MenuView.swift"))
        XCTAssertTrue(view.contains("MenuState.queueSummary(state.sessionRows)"),
                     "MenuView must render the queue-order summary line")
    }

    // @user-gated: user ruling - the queue summary line lists exactly the requests that wait for a slot, default tier included
    /// The summary is the WAIT list: one parked default request reads as that
    /// one entry, and a request that is running (not PARKED) never appears in
    /// it, even when it sits in the same row list.
    func testQueueSummaryListsOnlyWaitingRequests() throws {
        let waiting = try row(tier: "default", priority: 0, alias: "cq35", short: "cccccccc")
        XCTAssertEqual(MenuState.queueSummary([waiting]), "1. default/cq35")

        let running = try row(tier: "priority", priority: 10, alias: "cq27", short: "dddddddd", phase: "DECODE")
        XCTAssertEqual(MenuState.queueSummary([running, waiting]), "1. default/cq35")
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
        {"line":"[yyyyyyyy] a · WARMING · 0/1 · default P0","sessionId":"y","sessionShort":"yyyyyyyy","requestId":null,"model":"m","alias":"a",
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

    // MARK: - display.phase (renderer rule, fork 9dfdc32)

    /// Shared body for both display cases: identical except for the
    /// optional `display` object.
    private func retentionRowJSON(displayKey: String, line: String) -> Data {
        // displayKey is a pre-quoted fragment like `"display":{"phase":"DECODE"},`
        // (kept out of the multiline literal: a quote-terminated interpolation
        // on its own line trips the multiline-literal parser).
        """
        {"line":"\(line)","sessionId":"z","sessionShort":"zzzzzzzz","requestId":null,"model":"m","alias":"a",
         "tier":"default","priority":0,"phase":"IDLE",\(displayKey)"parkReason":null,"slot":null,
         "context":{"used":0,"cached":0,"processed":0,"decoded":0,"promptTotal":0,"window":1},
         "progress":null,"rate":{"kind":null,"tokensPerSecond":null,"windowSeconds":30.0},
         "elapsedMs":0,"phaseSinceMs":0,"respTokens":0}
        """.data(using: .utf8)!
    }

    /// A row with `display.phase` present renders the DISPLAYED phase while
    /// the raw `phase` stays what the machine readers use.
    func testDisplayPhaseRendersWhenPresent() throws {
        let entry = try JSONDecoder().decode(ContractSession.self,
                                             from: retentionRowJSON(displayKey: "\"display\":{\"phase\":\"DECODE\"},", line: "[zzzzzzzz] a · DECODE · 0/1 · default P0"))
        let row = SessionRow(contract: entry)
        XCTAssertEqual(row.phase, "IDLE", "the raw phase must stay IDLE for machine readers")
        XCTAssertEqual(row.displayPhase, "DECODE")
        XCTAssertTrue(row.displayLine.contains("· DECODE ·"),
                      "the rendered phase segment must show display.phase, got '\(row.displayLine)'")
        XCTAssertFalse(row.displayLine.contains("IDLE"),
                       "the raw phase must not appear in the render, got '\(row.displayLine)'")
    }

    /// A row without `display` decodes (the key is optional) and renders
    /// the raw phase unchanged - servers predating 9dfdc32 are unaffected.
    func testDisplayPhaseAbsentFallsBackToRawPhase() throws {
        let entry = try JSONDecoder().decode(ContractSession.self,
                                             from: retentionRowJSON(displayKey: "", line: "[zzzzzzzz] a · IDLE · 0/1 · default P0"))
        let row = SessionRow(contract: entry)
        XCTAssertNil(entry.display)
        XCTAssertNil(row.displayPhase)
        XCTAssertEqual(row.phase, "IDLE")
        XCTAssertTrue(row.displayLine.contains("· IDLE ·"),
                      "without display the raw phase renders, got '\(row.displayLine)'")
    }

    /// The retention readout (2026-10-30 incident): `display.phase=DECODE`
    /// now travels with `display.contextUsed` and `display.rate` - the last
    /// real DECODE sample - because a live observer saw the phase hold while
    /// the raw HOT `context.used` fell to 0 and the rate went absent. The
    /// menu renders the DISPLAY numbers verbatim: `context.window` stays the
    /// denominator and nothing is recomputed client-side (invariant 3); the
    /// raw fields stay raw for machine readers.
    func testDisplayPhaseRetainedReadoutRendersVerbatim() throws {
        let json = """
        {"line":"[zzzzzzzz] a · DECODE · 100.8k/262.1k · 21.4 t/s · default P0","sessionId":"z","sessionShort":"zzzzzzzz","requestId":null,"model":"m","alias":"a",
         "tier":"default","priority":0,"phase":"HOT",
         "display":{"phase":"DECODE","contextUsed":100809,
           "rate":{"kind":"decode","tokensPerSecond":21.4,"windowSeconds":30.0}},
         "parkReason":null,"slot":null,
         "context":{"used":0,"cached":0,"processed":0,"decoded":0,"promptTotal":41976,"window":262144},
         "progress":null,"rate":{"kind":null,"tokensPerSecond":null,"windowSeconds":30.0},
         "elapsedMs":0,"phaseSinceMs":0,"respTokens":0}
        """.data(using: .utf8)!
        let entry = try JSONDecoder().decode(ContractSession.self, from: json)
        let row = SessionRow(contract: entry)

        // Raw values stay raw for machine readers - the override is
        // render-only:
        XCTAssertEqual(row.phase, "HOT")
        XCTAssertEqual(row.context.used, 0)
        XCTAssertNil(row.rate.kind)
        XCTAssertEqual(row.displayPhase, "DECODE")

        // The used/rate segments render the DISPLAY readout while
        // context.window remains the raw denominator:
        XCTAssertEqual(row.displayLine,
                       "[zzzzzzzz] a · DECODE · 100.8k/262.1k · 21.4 t/s · default P0",
                       "the retained readout renders verbatim; no client-side calculation, "
                       + "got '\(row.displayLine)'")
    }

    /// Leak case 1 (2026-10-30 incident): a retained DECODE row sitting on a
    /// raw PREFILL must NOT print the raw prefill progress percent - the
    /// presentation is DECODE, and DECODE rows never carry progress. The raw
    /// fields still decode unchanged for machine readers.
    func testDisplayPhaseRetainedDecodeHidesRawPrefillProgress() throws {
        let json = """
        {"line":"[zzzzzzzz] a · DECODE · 100.8k/262.1k · 21.4 t/s · default P0","sessionId":"z","sessionShort":"zzzzzzzz","requestId":"r2","model":"m","alias":"a",
         "tier":"default","priority":0,"phase":"PREFILL",
         "display":{"phase":"DECODE","contextUsed":100809,
           "rate":{"kind":"decode","tokensPerSecond":21.4,"windowSeconds":30.0}},
         "parkReason":null,"slot":0,
         "context":{"used":10,"cached":38000,"processed":2960,"decoded":0,"promptTotal":90000,"window":262144},
         "progress":0.4182,"rate":{"kind":"prefill","tokensPerSecond":4120.0,"windowSeconds":30.0},
         "elapsedMs":4000,"phaseSinceMs":0,"respTokens":0}
        """.data(using: .utf8)!
        let entry = try JSONDecoder().decode(ContractSession.self, from: json)
        let row = SessionRow(contract: entry)

        // Raw stays raw:
        XCTAssertEqual(row.phase, "PREFILL")
        XCTAssertEqual(row.progress, 0.4182)
        XCTAssertEqual(row.rate.kind, "prefill")

        // The render shows DECODE with the retained readout and NO progress:
        XCTAssertEqual(row.displayLine,
                       "[zzzzzzzz] a · DECODE · 100.8k/262.1k · 21.4 t/s · default P0",
                       "a retained DECODE must not leak the raw PREFILL progress, "
                       + "got '\(row.displayLine)'")
        XCTAssertFalse(row.displayLine.contains("41.8%"),
                       "the raw prefill percent must not appear, got '\(row.displayLine)'")
    }

    /// The exact zero-first-PREFILL case (2026-10-30 incident, menu side):
    /// the next turn's first poll arrives before the child has processed
    /// anything, so the raw PREFILL carries context.used 0 and decoded 0
    /// (its own rate not yet measured) while the retention still displays
    /// DECODE with the last raw DECODE's readout. The raw zeros stay on the
    /// raw fields, and NO zero leaks into the rendered row: the used segment
    /// shows the retained 41.5k, never "0/262.1k".
    func testDisplayPhaseRetainedDecodeCoversZeroFirstPrefill() throws {
        let json = """
        {"line":"[zzzzzzzz] a · DECODE · 41.5k/262.1k · 58.0 t/s · default P0","sessionId":"z","sessionShort":"zzzzzzzz","requestId":"r2","model":"m","alias":"a",
         "tier":"default","priority":0,"phase":"PREFILL",
         "display":{"phase":"DECODE","contextUsed":41530,
           "rate":{"kind":"decode","tokensPerSecond":58.0,"windowSeconds":30.0}},
         "parkReason":null,"slot":0,
         "context":{"used":0,"cached":0,"processed":0,"decoded":0,"promptTotal":90000,"window":262144},
         "progress":0.0,"rate":{"kind":"prefill","tokensPerSecond":null,"windowSeconds":30.0},
         "elapsedMs":0,"phaseSinceMs":0,"respTokens":0}
        """.data(using: .utf8)!
        let entry = try JSONDecoder().decode(ContractSession.self, from: json)
        let row = SessionRow(contract: entry)

        // Raw stays raw for machine readers - the whole point of the case:
        XCTAssertEqual(row.phase, "PREFILL")
        XCTAssertEqual(row.context.used, 0)
        XCTAssertEqual(row.context.decoded, 0)

        // The rendered row shows the retained DECODE readout and no zero:
        XCTAssertEqual(row.displayLine,
                       "[zzzzzzzz] a · DECODE · 41.5k/262.1k · 58.0 t/s · default P0",
                       "the retained readout must cover the zero-first PREFILL, "
                       + "got '\(row.displayLine)'")
        XCTAssertFalse(row.displayLine.contains("0/262.1k"),
                       "no zero may leak into the rendered row, got '\(row.displayLine)'")
    }

    /// Leak case 2 (2026-10-30 incident): a retained DECODE row sitting on a
    /// raw PARKED must NOT append the raw park-reason suffix - "DECODE (lane)"
    /// would leak the phase being overridden. Without `display` the same row
    /// keeps its raw PARKED presentation (reason included), so the gate is
    /// the DISPLAYED phase, not the presence of parkReason.
    func testDisplayPhaseRetainedDecodeHidesRawParkReasonSuffix() throws {
        let json = """
        {"line":"[zzzzzzzz] a · DECODE · 100.8k/262.1k · 21.4 t/s · default P0","sessionId":"z","sessionShort":"zzzzzzzz","requestId":"r2","model":"m","alias":"a",
         "tier":"default","priority":0,"phase":"PARKED",
         "display":{"phase":"DECODE","contextUsed":100809,
           "rate":{"kind":"decode","tokensPerSecond":21.4,"windowSeconds":30.0}},
         "parkReason":"lane","slot":null,
         "context":{"used":0,"cached":0,"processed":0,"decoded":0,"promptTotal":0,"window":262144},
         "progress":null,"rate":{"kind":null,"tokensPerSecond":null,"windowSeconds":30.0},
         "elapsedMs":4000,"phaseSinceMs":0,"respTokens":0,
         "waitingFor":{"session":"s2","until":""}}
        """.data(using: .utf8)!
        let entry = try JSONDecoder().decode(ContractSession.self, from: json)
        let row = SessionRow(contract: entry)

        // Raw stays raw, including the park reason:
        XCTAssertEqual(row.phase, "PARKED")
        XCTAssertEqual(row.parkReason, "lane")

        // The render shows plain DECODE - no "(lane)" suffix:
        XCTAssertEqual(row.displayLine,
                       "[zzzzzzzz] a · DECODE · 100.8k/262.1k · 21.4 t/s · default P0",
                       "a retained DECODE must not leak the raw PARKED reason, "
                       + "got '\(row.displayLine)'")
    }

    /// Leak case 3: a retained DECODE row whose last raw DECODE never
    /// measured a rate (kind=decode on the first sample,
    /// tokensPerSecond still nil, so the server omits `display.rate`).
    /// The row sits on a raw PREFILL carrying a REAL measured prefill
    /// rate - and that raw rate must NOT render: a DECODE presentation
    /// shows `display.rate` ONLY; absent means no rate segment at all.
    /// `display.contextUsed` still renders.
    func testDisplayPhaseRetainedDecodeWithoutDisplayRateShowsNoRate() throws {
        let json = """
        {"line":"[zzzzzzzz] a · DECODE · 100.8k/262.1k · default P0","sessionId":"z","sessionShort":"zzzzzzzz","requestId":"r2","model":"m","alias":"a",
         "tier":"default","priority":0,"phase":"PREFILL",
         "display":{"phase":"DECODE","contextUsed":100809},
         "parkReason":null,"slot":0,
         "context":{"used":20000,"cached":0,"processed":20000,"decoded":0,"promptTotal":90000,"window":262144},
         "progress":0.2222,"rate":{"kind":"prefill","tokensPerSecond":10000.0,"windowSeconds":30.0},
         "elapsedMs":2000,"phaseSinceMs":0,"respTokens":0}
        """.data(using: .utf8)!
        let entry = try JSONDecoder().decode(ContractSession.self, from: json)
        let row = SessionRow(contract: entry)

        // Raw stays raw for machine readers:
        XCTAssertEqual(row.phase, "PREFILL")
        XCTAssertEqual(row.rate.kind, "prefill")
        XCTAssertEqual(row.rate.tokensPerSecond, 10000.0)

        // The render shows DECODE with the retained contextUsed and NO
        // rate segment - the raw prefill rate must not leak into the
        // DECODE presentation:
        XCTAssertEqual(row.displayLine,
                       "[zzzzzzzz] a · DECODE · 100.8k/262.1k · default P0",
                       "a retained DECODE without display.rate must show no rate at all, "
                       + "got '\(row.displayLine)'")
        XCTAssertFalse(row.displayLine.contains("t/s"),
                       "no rate may render without display.rate on a DECODE presentation, "
                       + "got '\(row.displayLine)'")
    }

    /// Outside retained DECODE the existing raw behavior stands: a raw
    /// PREFILL row with no display object renders its own measured rate.
    func testRawPrefillRowWithoutDisplayKeepsRenderingItsRate() throws {
        let json = """
        {"line":"[zzzzzzzz] a · PREFILL · 20.0k/262.1k · 22.2% · 10000.0 t/s · default P0","sessionId":"z","sessionShort":"zzzzzzzz","requestId":"r2","model":"m","alias":"a",
         "tier":"default","priority":0,"phase":"PREFILL",
         "parkReason":null,"slot":0,
         "context":{"used":20000,"cached":0,"processed":20000,"decoded":0,"promptTotal":90000,"window":262144},
         "progress":0.2222,"rate":{"kind":"prefill","tokensPerSecond":10000.0,"windowSeconds":30.0},
         "elapsedMs":2000,"phaseSinceMs":0,"respTokens":0}
        """.data(using: .utf8)!
        let entry = try JSONDecoder().decode(ContractSession.self, from: json)
        let row = SessionRow(contract: entry)
        XCTAssertEqual(row.displayLine,
                       "[zzzzzzzz] a · PREFILL · 20.0k/262.1k · 22.2% · 10000.0 t/s · default P0",
                       "a raw PREFILL row renders its own rate, got '\(row.displayLine)'")
    }

    /// The server's no-zero rule (2026-10-30): a raw 0 t/s arrives with a
    /// display.rate - the last measured rate of that kind, or one with no
    /// number. On EVERY phase, display.rate wins over the raw rate, so the
    /// menu never prints "0.0 t/s".
    func testDisplayRateReplacesRawZeroRateOnAnyPhase() throws {
        func row(_ display: String, line: String) throws -> SessionRow {
            let json = """
            {"line":"\(line)","sessionId":"z","sessionShort":"zzzzzzzz","requestId":"r2","model":"m","alias":"a",
             "tier":"default","priority":0,"phase":"PREFILL",
             "display":\(display),
             "parkReason":null,"slot":0,
             "context":{"used":20000,"cached":0,"processed":20000,"decoded":0,"promptTotal":90000,"window":262144},
             "progress":0.2222,"rate":{"kind":"prefill","tokensPerSecond":0.0,"windowSeconds":30.0},
             "elapsedMs":2000,"phaseSinceMs":0,"respTokens":0}
            """.data(using: .utf8)!
            return SessionRow(contract: try JSONDecoder().decode(ContractSession.self, from: json))
        }
        let last = try row(#"{"phase":"PREFILL","rate":{"kind":"prefill","tokensPerSecond":611.2,"windowSeconds":30.0}}"#,
                       line: "[zzzzzzzz] a · PREFILL · 20.0k/262.1k · 22.2% · 611.2 t/s · default P0")
        XCTAssertEqual(last.rate.tokensPerSecond, 0.0, "raw stays raw")
        XCTAssertEqual(last.displayLine,
                       "[zzzzzzzz] a · PREFILL · 20.0k/262.1k · 22.2% · 611.2 t/s · default P0",
                       "display.rate wins on a PREFILL row, got '\(last.displayLine)'")
        let none = try row(#"{"phase":"PREFILL","rate":{"kind":"prefill","tokensPerSecond":null,"windowSeconds":30.0}}"#,
                       line: "[zzzzzzzz] a · PREFILL · 20.0k/262.1k · 22.2% · default P0")
        XCTAssertFalse(none.displayLine.contains("t/s"),
                       "a display.rate with no number shows no rate, never 0.0, got '\(none.displayLine)'")
    }

    /// The no-zero context: past the decode retention a PARKED row of a
    /// session that had real context carries display {phase: PARKED,
    /// contextUsed} - the raw word with the last real context.
    func testDisplayContextUsedRendersUnderRawWord() throws {
        let json = """
        {"line":"[zzzzzzzz] a · PARKED (kv pool) · 41.5k/262.1k · default P0","sessionId":"z","sessionShort":"zzzzzzzz","requestId":"r3","model":"m","alias":"a",
         "tier":"default","priority":0,"phase":"PARKED",
         "display":{"phase":"PARKED","contextUsed":41530},
         "parkReason":"kv","slot":null,
         "context":{"used":0,"cached":0,"processed":0,"decoded":0,"promptTotal":0,"window":262144},
         "progress":null,"rate":{"kind":null,"tokensPerSecond":null,"windowSeconds":30.0},
         "elapsedMs":2000,"phaseSinceMs":0,"respTokens":0}
        """.data(using: .utf8)!
        let row = SessionRow(contract: try JSONDecoder().decode(ContractSession.self, from: json))
        XCTAssertTrue(row.displayLine.contains("41.5k/262.1k"),
                      "the last real context renders, never 0/262.1k, got '\(row.displayLine)'")
        XCTAssertTrue(row.displayLine.contains("PARKED"), "the raw word renders, got '\(row.displayLine)'")
    }
}
