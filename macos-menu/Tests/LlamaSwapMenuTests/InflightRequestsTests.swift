import XCTest
@testable import LlamaSwapMenuCore

/// Wire-level pins for the individual-request half of the /api/events
/// "inflight" union payload (internal/swaputil/events.go
/// InFlightRequestsEvent.Requests/.Request/.Queue) - the fields
/// MetricsTests.swift's existing InFlightStats test intentionally leaves
/// undecoded. These feed BackendClient.menuState.sessionRows / .queueRows,
/// the per-session throughput rows the menu bar renders alongside cm-menu's
/// unified view (llama-cm docs/intent/cm-driver.md).
final class InflightRequestsTests: XCTestCase {

    // MARK: - Codable-level pins (a trimmed real shape - see /tmp/iso-events.txt
    // for the live capture this mirrors, structurally confirmed against
    // internal/server/inflight.go + internal/swaputil/events.go)

    func testSnapshotDecodesRequestsWithMetadataAndQueue() throws {
        let json = """
        {"total":2,"byTier":{"default":1,"priority":1},"granted":1,"operation":"snapshot",\
        "requests":[\
        {"id":"41","timestamp":"2026-09-02T13:21:00Z","model":"cq35",\
        "req_path":"/v1/messages","method":"POST",\
        "req_headers":{"User-Agent":"claude-code/1.0"},"remote_ip":"127.0.0.1",\
        "resp_headers":{},"resp_bytes":1200,"elapsed_ms":4500,\
        "metadata":{"session_id":"3bf85c8c-1234-4321-aaaa-bbbbccccdddd","client_user_id":"user_x_session_3bf85c8c-1234-4321-aaaa-bbbbccccdddd"}},\
        {"id":"42","timestamp":"2026-09-02T13:21:05Z","model":"cq27",\
        "req_path":"/v1/messages","method":"POST",\
        "req_headers":{"User-Agent":"Hermes Desktop/2.0"},"remote_ip":"127.0.0.1",\
        "resp_headers":{},"resp_bytes":0,"elapsed_ms":100}\
        ],\
        "queue":[{"position":1,"tier":"priority","model":"cq35","arrived":"2026-09-02T13:21:06Z"}]}
        """.data(using: .utf8)!

        let stats = try JSONDecoder().decode(InFlightStats.self, from: json)
        XCTAssertEqual(stats.operation, "snapshot")
        XCTAssertEqual(stats.requests?.count, 2)

        let first = try XCTUnwrap(stats.requests?.first { $0.id == "41" })
        XCTAssertEqual(first.model, "cq35")
        XCTAssertEqual(first.reqPath, "/v1/messages")
        XCTAssertEqual(first.respBytes, 1200)
        XCTAssertEqual(first.elapsedMs, 4500)
        XCTAssertEqual(first.metadata?["session_id"], "3bf85c8c-1234-4321-aaaa-bbbbccccdddd")
        XCTAssertEqual(first.reqHeaders?["User-Agent"], "claude-code/1.0")

        let second = try XCTUnwrap(stats.requests?.first { $0.id == "42" })
        XCTAssertNil(second.metadata?["session_id"], "a request with no session metadata must decode without one, not crash")

        let queue = try XCTUnwrap(stats.queue)
        XCTAssertEqual(queue.count, 1)
        XCTAssertEqual(queue[0].position, 1)
        XCTAssertEqual(queue[0].tier, "priority")
        XCTAssertEqual(queue[0].model, "cq35")
    }

    func testUpsertDecodesSingleRequestAndRemoveDecodesID() throws {
        let upsertJSON = """
        {"total":1,"operation":"upsert",\
        "request":{"id":"7","timestamp":"2026-09-02T13:00:00Z","model":"cq35",\
        "req_path":"/v1/messages","method":"POST","req_headers":{},"remote_ip":"127.0.0.1",\
        "resp_headers":{},"resp_bytes":0,"elapsed_ms":10}}
        """.data(using: .utf8)!
        let upsert = try JSONDecoder().decode(InFlightStats.self, from: upsertJSON)
        XCTAssertEqual(upsert.operation, "upsert")
        XCTAssertEqual(upsert.request?.id, "7")
        XCTAssertNil(upsert.requests)

        let removeJSON = """
        {"total":0,"operation":"remove","id":"7"}
        """.data(using: .utf8)!
        let remove = try JSONDecoder().decode(InFlightStats.self, from: removeJSON)
        XCTAssertEqual(remove.operation, "remove")
        XCTAssertEqual(remove.id, "7")
    }

    // MARK: - integration: pushed SSE events land in menuState.sessionRows/.queueRows

    private var stub: StubBackend!

    override func setUpWithError() throws {
        stub = try StubBackend()
    }

    override func tearDown() {
        stub.stop()
        stub = nil
    }

    @discardableResult
    private func waitUntil(_ timeout: TimeInterval = 6, _ cond: () -> Bool) -> Bool {
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if cond() { return true }
            RunLoop.main.run(until: Date().addingTimeInterval(0.02))
        }
        return cond()
    }

    private func makeClient(laneLingerSeconds: TimeInterval = BackendClient.defaultLaneLingerSeconds) -> BackendClient {
        let client = BackendClient(baseURL: stub.baseURL, laneLingerSeconds: laneLingerSeconds)
        XCTAssertTrue(waitUntil { self.stub.hasEventClient },
                      "client never opened the /api/events stream")
        return client
    }

    func testPushedSnapshotPopulatesSessionRowsAndQueueRows() {
        let client = makeClient()
        let inner = """
        {"total":1,"granted":1,"operation":"snapshot",\
        "requests":[{"id":"41","timestamp":"2026-09-02T13:21:00Z","model":"cq35",\
        "req_path":"/v1/messages","method":"POST",\
        "req_headers":{"User-Agent":"claude-code/1.0"},"remote_ip":"127.0.0.1",\
        "resp_headers":{},"resp_bytes":0,"elapsed_ms":500,\
        "metadata":{"session_id":"3bf85c8c-1234-4321-aaaa-bbbbccccdddd"}}],\
        "queue":[{"position":1,"tier":"priority","model":"cq27","arrived":"2026-09-02T13:21:06Z"}]}
        """
        stub.pushEvent(type: "inflight", inner: inner)

        XCTAssertTrue(waitUntil { client.menuState.sessionRows.count == 1 },
                      "expected 1 session row, got \(client.menuState.sessionRows.count)")
        let row = client.menuState.sessionRows[0]
        XCTAssertEqual(row.origin, "3bf85c8c")
        XCTAssertEqual(row.model, "cq35")
        XCTAssertEqual(row.word, "PARKED")

        XCTAssertEqual(client.menuState.queueRows.count, 1)
        XCTAssertEqual(client.menuState.queueRows[0].tier, "priority")
        XCTAssertEqual(client.menuState.queueRows[0].model, "cq27")
    }

    func testGrantedRequestWithNoBytesYetIsPrefillNotParked() {
        let client = makeClient()
        let inner = """
        {"total":1,"granted":1,"operation":"snapshot",\
        "requests":[{"id":"41","timestamp":"2026-09-02T13:21:00Z","model":"cq35",\
        "req_path":"/v1/messages","method":"POST",\
        "req_headers":{"User-Agent":"claude-code/1.0"},"remote_ip":"127.0.0.1",\
        "resp_headers":{},"resp_bytes":0,"elapsed_ms":500,\
        "metadata":{"session_id":"3bf85c8c-1234-4321-aaaa-bbbbccccdddd","slot_granted":"1"}}],\
        "queue":[]}
        """
        stub.pushEvent(type: "inflight", inner: inner)

        XCTAssertTrue(waitUntil { client.menuState.sessionRows.count == 1 },
                      "expected 1 session row, got \(client.menuState.sessionRows.count)")
        let row = client.menuState.sessionRows[0]
        XCTAssertEqual(row.word, "PREFILL", "a granted request with no bytes yet is still prefilling, not parked")
    }

    func testParkedRequestTransitionsToDecodeOnceGranted() {
        let client = makeClient()
        let parkedInner = """
        {"total":1,"granted":0,"operation":"snapshot",\
        "requests":[{"id":"55","timestamp":"2026-09-02T13:21:00Z","model":"cq35",\
        "req_path":"/v1/messages","method":"POST",\
        "req_headers":{"User-Agent":"claude-code/1.0"},"remote_ip":"127.0.0.1",\
        "resp_headers":{},"resp_bytes":0,"elapsed_ms":500,\
        "metadata":{"session_id":"3bf85c8c-1234-4321-aaaa-bbbbccccdddd"}}],\
        "queue":[]}
        """
        stub.pushEvent(type: "inflight", inner: parkedInner)
        XCTAssertTrue(waitUntil { client.menuState.sessionRows.first?.word == "PARKED" },
                      "expected PARKED before slot_granted, got \(client.menuState.sessionRows.first?.word ?? "<none>")")

        let grantedInner = """
        {"total":1,"granted":1,"operation":"snapshot",\
        "requests":[{"id":"55","timestamp":"2026-09-02T13:21:00Z","model":"cq35",\
        "req_path":"/v1/messages","method":"POST",\
        "req_headers":{"User-Agent":"claude-code/1.0"},"remote_ip":"127.0.0.1",\
        "resp_headers":{},"resp_bytes":800,"elapsed_ms":600,\
        "metadata":{"session_id":"3bf85c8c-1234-4321-aaaa-bbbbccccdddd","slot_granted":"1"}}],\
        "queue":[]}
        """
        stub.pushEvent(type: "inflight", inner: grantedInner)
        XCTAssertTrue(waitUntil { client.menuState.sessionRows.first?.word == "DECODE" },
                      "expected a clean PARKED->DECODE transition once granted with bytes, got \(client.menuState.sessionRows.first?.word ?? "<none>")")
    }

    /// A removed request no longer clears its row INSTANTLY: the lane lingers
    /// for a window first, because a Claude Code client's turn boundary
    /// removes the lane's only request for 1-3s while it runs a tool (see
    /// LaneLingerTests). The row must still be gone once the window expires -
    /// that is what this test pins, with a short injected window.
    func testRemoveOperationLingersThenClearsSessionRow() {
        let client = makeClient(laneLingerSeconds: 0.6)
        let upsertInner = """
        {"total":1,"operation":"upsert",\
        "request":{"id":"9","timestamp":"2026-09-02T13:21:00Z","model":"cq35",\
        "req_path":"/v1/messages","method":"POST","req_headers":{},"remote_ip":"127.0.0.1",\
        "resp_headers":{},"resp_bytes":50,"elapsed_ms":100}}
        """
        stub.pushEvent(type: "inflight", inner: upsertInner)
        XCTAssertTrue(waitUntil { client.menuState.sessionRows.count == 1 })

        let removeInner = """
        {"total":0,"operation":"remove","id":"9"}
        """
        stub.pushEvent(type: "inflight", inner: removeInner)
        RunLoop.main.run(until: Date().addingTimeInterval(0.1))
        XCTAssertEqual(client.menuState.sessionRows.first?.word, "TURN",
                       "the lane lingers as between-turns first, got \(client.menuState.sessionRows.map(\.displayLine))")
        XCTAssertTrue(waitUntil { client.menuState.sessionRows.isEmpty },
                      "a removed request must not outlive the linger window")
        XCTAssertTrue(client.menuState.queueRows.isEmpty)
    }

    func testEmptyQueueRendersAsIdleAtTheViewLayer() {
        let client = makeClient()
        XCTAssertEqual(client.menuState.queueRows.count, 0)
        XCTAssertEqual(MenuState.queueSummary(client.menuState.queueRows), "Queue: idle")
    }

    /// Slot polling for a resident-alias row. The in-flight entry keeps the id
    /// the caller asked for (claude-haiku-4-5-20251001), which never shows up
    /// as a "model" key in /api/slots - only the resident model it resolved
    /// to does, so joining on the alias id finds nothing and the row loses
    /// its slot readout (witnessed 2026-09-08, "slots disappear" while only a
    /// subagent turn is in flight). The proxy stamps metadata.resolved_model;
    /// the join must follow it, and the row must carry the agent's short id.
    func testSlotPollFollowsResolvedModelNotTheAliasId() {
        stub.responder = { _, path in
            if path == "/api/slots" {
                return (200, """
                {"models":[{"model":"Qwen3.6-35B-A3B-APEX-I-Balanced-384K","state":"ready","slots":[]}]}
                """)
            }
            return (200, "{}")
        }
        let client = makeClient()
        let inner = """
        {"total":1,"granted":1,"operation":"snapshot",\
        "requests":[{"id":"151","timestamp":"2026-09-08T10:10:03Z","model":"claude-haiku-4-5-20251001",\
        "req_path":"/v1/messages","method":"POST",\
        "req_headers":{"User-Agent":"claude-cli/2.1.263"},"remote_ip":"127.0.0.1",\
        "resp_headers":{},"resp_bytes":0,"elapsed_ms":500,\
        "metadata":{"session_id":"934b47c6-9143-464f-8846-49099f0a295b","agent_id":"a4c4e94e633cf6841",\
        "resolved_model":"Qwen3.6-35B-A3B-APEX-I-Balanced-384K","slot_granted":"1"}}]}
        """
        stub.pushEvent(type: "inflight", inner: inner)

        XCTAssertTrue(waitUntil {
            self.stub.recorded.contains { $0.path == "/api/slots" }
        }, "slot poll must hit the single /api/slots endpoint; recorded: \(stub.recorded.map(\.path))")
        XCTAssertEqual(client.menuState.sessionRows.first?.agent, "a4c4e94e")
    }

    /// One row per LANE (session, or session+agent), not per request. A Claude
    /// Code client keeps two requests open on the proxy for a moment at every
    /// turn boundary (the finished one is removed a beat after the next one
    /// arrives), so a per-request list shows 5 rows for 3 lanes and re-sorts
    /// on every arrival and removal - the list the user saw "flickering,
    /// jumping around" on 2026-09-08. The row shows the lane's NEWEST request;
    /// lanes keep the order they were first seen, parent before its subagents.
    func testOneRowPerLaneNewestRequestWinsAndOrderIsStable() {
        let client = makeClient()
        func req(_ id: String, _ agent: String?, _ bytes: Int, granted: Bool) -> String {
            let agentKey = agent.map { ",\"agent_id\":\"\($0)\"" } ?? ""
            let grant = granted ? ",\"slot_granted\":\"1\"" : ",\"kv_parked\":\"1\""
            return """
            {"id":"\(id)","timestamp":"2026-09-08T10:37:00Z","model":"cq35h",\
            "req_path":"/v1/messages","method":"POST","req_headers":{},"remote_ip":"127.0.0.1",\
            "resp_headers":{},"resp_bytes":\(bytes),"elapsed_ms":500,\
            "metadata":{"session_id":"934b47c6-9143-464f-8846-49099f0a295b"\(agentKey)\(grant)}}
            """
        }
        // 5 requests, 3 lanes: main (67 finishing, 69 new), a7f3ebca (68), aa715a39 (70 old, 71 new).
        let snapshot = """
        {"total":5,"operation":"snapshot","requests":[\
        \(req("67", nil, 2983, granted: true)),\
        \(req("68", "a7f3ebca633cf6841", 3, granted: true)),\
        \(req("69", nil, 0, granted: false)),\
        \(req("70", "aa715a39633cf6841", 0, granted: false)),\
        \(req("71", "aa715a39633cf6841", 0, granted: false))\
        ]}
        """
        stub.pushEvent(type: "inflight", inner: snapshot)
        XCTAssertTrue(waitUntil { client.menuState.sessionRows.count == 3 },
                      "expected 3 lane rows, got \(client.menuState.sessionRows.map(\.displayLine))")
        let rows = client.menuState.sessionRows
        XCTAssertEqual(rows.map { $0.agent ?? "main" }, ["main", "a7f3ebca", "aa715a39"])
        XCTAssertEqual(rows[0].id, "69", "the lane row is its newest request")
        XCTAssertEqual(rows[2].id, "71")

        // The old main request is removed and a new subagent request arrives:
        // the order must not move.
        stub.pushEvent(type: "inflight", inner: "{\"total\":4,\"operation\":\"remove\",\"id\":\"67\"}")
        stub.pushEvent(type: "inflight", inner:
            "{\"total\":5,\"operation\":\"upsert\",\"request\":\(req("72", "a7f3ebca633cf6841", 0, granted: false))}")
        XCTAssertTrue(waitUntil { client.menuState.sessionRows.first { $0.agent == "a7f3ebca" }?.id == "72" })
        XCTAssertEqual(client.menuState.sessionRows.map { $0.agent ?? "main" }, ["main", "a7f3ebca", "aa715a39"])
    }
}
