import XCTest
@testable import LlamaSwapMenuCore

/// Pins the PENALIZED row contract (llama-cm docs/intent/session-state-
/// contract.md, schema "llama-swap.sessions/v1", additive `phase ==
/// "PENALIZED"` + `penalty` box):
///
///   1. `ContractSession`/`PenaltyInfo` decode the penalty box, including a
///      null `remainingSeconds` (a HELD final strike) and an absent
///      `penalty` on an older/odd server.
///   2. `SessionRow.displayLine` renders the user-approved PENALIZED shape,
///      timed and held, and falls back to the generic (verbatim) path when
///      `phase == "PENALIZED"` but `penalty` is missing (invariant 5).
///   3. The loop-detector evidence (`uniformRun`/`typicalTokens`) surfaces
///      only in the tooltip, never in the row text.
///   4. A PENALIZED row never counts toward `waiting` (WaitingParityTests
///      covers the general parity invariant; this file adds the PENALIZED
///      case).
///   5. Clicking a PENALIZED row POSTs `/api/sessions/<full sessionId>/
///      unpenalize` - NEVER `<id>`, which is the request id whenever the
///      session still has one PARKED (the common case: the client keeps
///      retrying) - clears the hold optimistically, and restores it on a
///      failed POST - the same optimistic-clear/restore contract as
///      `finishCooldown` (GraceFinishClickTests) and `cancelInflight`
///      (CancelInflightClickTests).
final class PenalizedRowTests: XCTestCase {

    // MARK: - decode

    private func contractSession(_ json: String) throws -> ContractSession {
        try JSONDecoder().decode(ContractSession.self, from: json.data(using: .utf8)!)
    }

    func testDecodesTimedPenalty() throws {
        let entry = try contractSession("""
        {"sessionId":"934159af-0000-0000-0000-000000000000","sessionShort":"934159af",
         "requestId":null,"model":"cq35","alias":"cq35","tier":"default","priority":0,
         "phase":"PENALIZED","parkReason":null,"slot":null,
         "context":{"used":228100,"cached":0,"processed":0,"decoded":0,"promptTotal":0,"window":262100},
         "progress":null,"rate":{"kind":null,"tokensPerSecond":null,"windowSeconds":30.0},
         "elapsedMs":0,"phaseSinceMs":0,"respTokens":0,
         "looping":true,"uniformRun":86,
         "penalty":{"reason":"loop","strike":2,"strikes":3,"remainingSeconds":760,
                    "uniformRun":86,"typicalTokens":48}}
        """)
        XCTAssertEqual(entry.phase, "PENALIZED")
        XCTAssertEqual(entry.looping, true)
        XCTAssertEqual(entry.uniformRun, 86)
        let penalty = try XCTUnwrap(entry.penalty)
        XCTAssertEqual(penalty.reason, "loop")
        XCTAssertEqual(penalty.strike, 2)
        XCTAssertEqual(penalty.strikes, 3)
        XCTAssertEqual(penalty.remainingSeconds, 760)
        XCTAssertEqual(penalty.uniformRun, 86)
        XCTAssertEqual(penalty.typicalTokens, 48)
    }

    func testDecodesHeldPenaltyWithNullRemainingSeconds() throws {
        let entry = try contractSession("""
        {"sessionId":"934159af-0000-0000-0000-000000000000","sessionShort":"934159af",
         "requestId":null,"model":"cq35","alias":"cq35","tier":"default","priority":0,
         "phase":"PENALIZED","parkReason":null,"slot":null,
         "context":{"used":228100,"cached":0,"processed":0,"decoded":0,"promptTotal":0,"window":262100},
         "progress":null,"rate":{"kind":null,"tokensPerSecond":null,"windowSeconds":30.0},
         "elapsedMs":0,"phaseSinceMs":0,"respTokens":0,
         "penalty":{"reason":"loop","strike":3,"strikes":3,"remainingSeconds":null,
                    "uniformRun":86,"typicalTokens":48}}
        """)
        let penalty = try XCTUnwrap(entry.penalty)
        XCTAssertNil(penalty.remainingSeconds)
    }

    /// An older/odd server that predates `looping`/`uniformRun`/`penalty`:
    /// none of the new fields are required, and decoding must not fail.
    func testAbsentPenaltyFieldsDecodeToNilFalseZero() throws {
        let entry = try contractSession("""
        {"sessionId":"x","sessionShort":"x","requestId":null,"model":"m","alias":"a",
         "tier":"default","priority":0,"phase":"PENALIZED","parkReason":null,"slot":null,
         "context":{"used":0,"cached":0,"processed":0,"decoded":0,"promptTotal":0,"window":1},
         "progress":null,"rate":{"kind":null,"tokensPerSecond":null,"windowSeconds":30.0},
         "elapsedMs":0,"phaseSinceMs":0,"respTokens":0}
        """)
        XCTAssertNil(entry.looping)
        XCTAssertNil(entry.uniformRun)
        XCTAssertNil(entry.penalty)
    }

    // MARK: - displayLine

    /// `id` is deliberately a REQUEST id, distinct from `sessionId` - the
    /// common shape for a PENALIZED row that still has one PARKED request
    /// (the client keeps retrying) - so a test that accidentally posts `id`
    /// instead of `sessionId` fails loudly rather than passing by
    /// coincidence.
    private func row(strike: Int, remainingSeconds: Int?, parkReason: String? = nil) -> SessionRow {
        SessionRow(
            id: "req-999", sessionId: "934159af-0000-0000-0000-000000000000", sessionShort: "934159af",
            model: "cq35", alias: "ALIAS", tier: "default",
            priority: 0, phase: "PENALIZED", parkReason: parkReason,
            context: ContractContext(used: 228_100, cached: 0, processed: 0, decoded: 0, promptTotal: 0, window: 262_100),
            rate: ContractRate(kind: nil, tokensPerSecond: nil, windowSeconds: 30),
            penalty: PenaltyInfo(reason: "loop", strike: strike, strikes: 3,
                                  remainingSeconds: remainingSeconds, uniformRun: 86, typicalTokens: 48))
    }

    func testDisplayLineTimedPenalty() {
        XCTAssertEqual(row(strike: 2, remainingSeconds: 760).displayLine,
                       "[934159af] ALIAS · PENALIZED (loop 2/3) · 228.1k/262.1k · 12:40")
    }

    func testDisplayLineHeldPenalty() {
        XCTAssertEqual(row(strike: 3, remainingSeconds: nil).displayLine,
                       "[934159af] ALIAS · PENALIZED (loop 3/3) · 228.1k/262.1k · held")
    }

    /// The common PENALIZED shape: a request is still PARKED under park
    /// reason "penalized" while the hold is in effect. The park phrase must
    /// NOT be appended a second time alongside the PENALIZED segment - the
    /// line is exactly the same as the no-request case.
    func testDisplayLineWithRequestAndParkReasonDoesNotDoubleUpTheReason() {
        XCTAssertEqual(row(strike: 2, remainingSeconds: 760, parkReason: "penalized").displayLine,
                       "[934159af] ALIAS · PENALIZED (loop 2/3) · 228.1k/262.1k · 12:40")
    }

    /// `phase == "PENALIZED"` without a `penalty` box falls back to the
    /// generic path: PENALIZED renders verbatim, never dropped or guessed at
    /// (invariant 5).
    func testPenalizedWithoutPenaltyFallsBackToVerbatimGenericPath() {
        let row = SessionRow(
            id: "x", sessionShort: "aaaaaaaa", model: "cq35", alias: "cq35", tier: "default",
            priority: 0, phase: "PENALIZED",
            context: ContractContext(used: 0, cached: 0, processed: 0, decoded: 0, promptTotal: 0, window: 262_144),
            rate: ContractRate(kind: nil, tokensPerSecond: nil, windowSeconds: 30))
        XCTAssertEqual(row.displayLine, "[aaaaaaaa] cq35 · PENALIZED · 0/262.1k")
    }

    /// Strike-1 (looping, not yet PENALIZED) renders exactly as today - no
    /// looping segment is ever added to a non-PENALIZED row.
    func testLoopingStrikeOneRendersUnchanged() throws {
        let entry = try contractSession("""
        {"sessionId":"x","sessionShort":"aaaaaaaa","requestId":"r-1","model":"cq35","alias":"cq35",
         "tier":"default","priority":0,"phase":"DECODE","parkReason":null,"slot":null,
         "context":{"used":0,"cached":0,"processed":0,"decoded":0,"promptTotal":0,"window":262144},
         "progress":null,"rate":{"kind":null,"tokensPerSecond":null,"windowSeconds":30.0},
         "elapsedMs":0,"phaseSinceMs":0,"respTokens":0,"looping":true,"uniformRun":12}
        """)
        let row = SessionRow(contract: entry)
        XCTAssertEqual(row.displayLine, "[aaaaaaaa] cq35 · DECODE · 0/262.1k")
    }

    // MARK: - tooltip

    func testTooltipCarriesEvidenceNotInTheRowText() {
        let r = row(strike: 2, remainingSeconds: 760)
        XCTAssertEqual(r.penaltyTooltip, "86 requests in a row, ~48 tokens each")
        XCTAssertFalse(r.displayLine.contains("86"), "evidence belongs in the tooltip, not the row")
        XCTAssertFalse(r.displayLine.contains("48"), "evidence belongs in the tooltip, not the row")
    }

    func testNoTooltipWithoutPenalty() {
        let row = SessionRow(
            id: "x", sessionShort: "aaaaaaaa", model: "cq35", alias: "cq35", tier: "default",
            priority: 0, phase: "DECODE",
            context: ContractContext(used: 0, cached: 0, processed: 0, decoded: 0, promptTotal: 0, window: 262_144),
            rate: ContractRate(kind: nil, tokensPerSecond: nil, windowSeconds: 30))
        XCTAssertNil(row.penaltyTooltip)
    }

    // MARK: - unpenalize click

    private var stub: StubBackend!

    override func setUpWithError() throws {
        stub = try StubBackend()
    }

    override func tearDown() {
        stub.stop()
        stub = nil
    }

    @discardableResult
    private func waitUntil(_ timeout: TimeInterval = 5, _ cond: () -> Bool) -> Bool {
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if cond() { return true }
            RunLoop.main.run(until: Date().addingTimeInterval(0.02))
        }
        return cond()
    }

    private func makeClient() -> BackendClient {
        let client = BackendClient(baseURL: stub.baseURL)
        XCTAssertTrue(waitUntil { self.stub.hasEventClient },
                      "client never opened the /api/events stream")
        return client
    }

    func testClickPostsUnpenalizeWithFullSessionIdNotRequestId() {
        stub.responder = { _, _ in (200, "{}") }
        let client = makeClient()
        let penalized = row(strike: 2, remainingSeconds: 760)
        XCTAssertNotEqual(penalized.id, penalized.sessionId,
                          "test fixture must exercise id != sessionId, the common PENALIZED shape")
        client.menuState.sessionRows = [penalized]

        client.unpenalize(sessionId: penalized.sessionId)

        // Optimistic clear: synchronous with the click, no SSE round-trip.
        XCTAssertFalse(client.menuState.sessionRows.contains(where: { $0.phase == "PENALIZED" }),
                       "clicked row must stop reading PENALIZED at once")

        XCTAssertTrue(waitUntil {
            self.stub.recorded.contains(StubBackend.Recorded(
                method: "POST", path: "/api/sessions/\(penalized.sessionId)/unpenalize"))
        }, "expected POST /api/sessions/<full sessionId>/unpenalize, got \(stub.recorded)")
        XCTAssertFalse(self.stub.recorded.contains(StubBackend.Recorded(
            method: "POST", path: "/api/sessions/\(penalized.id)/unpenalize")),
            "must never POST the request id's path")
    }

    /// Decoded straight off the wire with a non-null `requestId` (the PARKED
    /// retry present alongside the hold): the click still POSTs the full
    /// `sessionId` path, not the request id `SessionRow.id` resolves to.
    func testDecodedRowWithRequestIdStillPostsSessionIdPath() throws {
        let entry = try contractSession("""
        {"sessionId":"934159af-0000-0000-0000-000000000000","sessionShort":"934159af",
         "requestId":"req-999","model":"cq35","alias":"cq35","tier":"default","priority":0,
         "phase":"PENALIZED","parkReason":"penalized","slot":null,
         "context":{"used":228100,"cached":0,"processed":0,"decoded":0,"promptTotal":0,"window":262100},
         "progress":null,"rate":{"kind":null,"tokensPerSecond":null,"windowSeconds":30.0},
         "elapsedMs":0,"phaseSinceMs":0,"respTokens":0,
         "penalty":{"reason":"loop","strike":2,"strikes":3,"remainingSeconds":760,
                    "uniformRun":86,"typicalTokens":48}}
        """)
        let penalized = SessionRow(contract: entry)
        XCTAssertEqual(penalized.id, "req-999", "id resolves to the request id when one is present")
        XCTAssertEqual(penalized.sessionId, "934159af-0000-0000-0000-000000000000")

        stub.responder = { _, _ in (200, "{}") }
        let client = makeClient()
        client.menuState.sessionRows = [penalized]

        client.unpenalize(sessionId: penalized.sessionId)

        XCTAssertTrue(waitUntil {
            self.stub.recorded.contains(StubBackend.Recorded(
                method: "POST", path: "/api/sessions/934159af-0000-0000-0000-000000000000/unpenalize"))
        }, "expected POST to the full sessionId path, got \(stub.recorded)")
        XCTAssertFalse(self.stub.recorded.contains(StubBackend.Recorded(
            method: "POST", path: "/api/sessions/req-999/unpenalize")),
            "must never POST the request id's path - that endpoint call would silently no-op")
    }

    func testFailedClickRestoresRowAndSurfacesError() {
        stub.responder = { _, path in
            if path.hasSuffix("/unpenalize") { return (500, "boom") }
            return (200, "{}")
        }
        let client = makeClient()
        let penalized = row(strike: 2, remainingSeconds: 760)
        client.menuState.sessionRows = [penalized]

        client.unpenalize(sessionId: penalized.sessionId)

        XCTAssertTrue(waitUntil { client.menuState.lastSwitchError != nil },
                      "a failed unpenalize must surface in the menu, not fail silently")
        XCTAssertTrue(waitUntil {
            client.menuState.sessionRows.contains(where: { $0.phase == "PENALIZED" })
        }, "a failed unpenalize must restore the hold (it is still in effect)")
    }
}
