import XCTest
@testable import LlamaSwapMenuCore

/// Validates `BackendClient.fetchMetrics` against the API split introduced by
/// the upstream merge (GET /api/metrics/stats, an aggregate object, replacing
/// the removed bare-array GET /api/metrics) and the merged /api/events
/// "inflight" union payload. Wire-level (and Codable-level) tests matter here
/// because every failure mode in this path is silent: a wrong path 404s to
/// nothing visible, an object-vs-array mismatch just leaves `completed` at 0
/// forever, and a paginated page-count would read as a plausible-but-wrong
/// total - none of them throw anything a human would notice.
final class MetricsTests: XCTestCase {

    private var stub: StubBackend!

    override func setUpWithError() throws {
        stub = try StubBackend()
    }

    override func tearDown() {
        stub.stop()
        stub = nil
    }

    /// Spins the main run loop until `cond` holds or the deadline passes.
    /// BackendClient publishes every state change on the main queue, so the
    /// loop has to actually run for the client to make progress.
    @discardableResult
    private func waitUntil(_ timeout: TimeInterval = 6,
                           _ cond: () -> Bool) -> Bool {
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if cond() { return true }
            RunLoop.main.run(until: Date().addingTimeInterval(0.02))
        }
        return cond()
    }

    /// Builds the client and waits for its `/api/events` stream to attach, so
    /// pushed events cannot race the connect.
    private func makeClient() -> BackendClient {
        let client = BackendClient(baseURL: stub.baseURL)
        XCTAssertTrue(waitUntil { self.stub.hasEventClient },
                      "client never opened the /api/events stream")
        return client
    }

    // MARK: - wire contract (fetchMetrics -> GET /api/metrics/stats)

    /// The happy path: a realistic /api/metrics/stats body (the real
    /// store.ActivityStats field set from internal/store/store.go) must
    /// populate `completed` from total_requests, and the client must hit the
    /// new path - not the removed GET /api/metrics.
    func testFetchMetricsReadsTotalRequestsFromStatsEndpoint() {
        stub.responder = { method, path in
            if method == "GET", path == "/api/metrics/stats" {
                return (200, """
                {"total_requests":42,"total_input_tokens":1000,"total_output_tokens":2000,\
                "total_cache_tokens":500,"prompt_histogram":null,"gen_histogram":null}
                """)
            }
            return (200, "{}")
        }
        let client = makeClient()

        XCTAssertTrue(waitUntil { client.menuState.completed == 42 },
                      "expected completed == 42, got \(client.menuState.completed)")
        XCTAssertTrue(stub.recorded.contains(StubBackend.Recorded(method: "GET", path: "/api/metrics/stats")),
                      "client must poll the new /api/metrics/stats path")
        XCTAssertFalse(stub.recorded.contains { $0.path == "/api/metrics" },
                       "client must not poll the removed bare GET /api/metrics path")
    }

    /// The negative case for the exact bug this merge could reintroduce: if
    /// the client (or a future regression) were pointed at the paginated
    /// /api/metrics/activity shape instead, naively counting array entries or
    /// reading a pagination "total" field would report a page size or a stale
    /// count instead of the true aggregate. Serving that shape at
    /// /api/metrics/stats must leave `completed` at 0 (the ActivityStats
    /// decode fails - total_requests is a required key that shape lacks -
    /// so the poll is silently skipped rather than adopting 25 (page length)
    /// or 612 (the pagination "total" field)).
    func testFetchMetricsDoesNotCountPaginatedActivityPage() {
        stub.responder = { method, path in
            if method == "GET", path == "/api/metrics/stats" {
                let rows = (0..<25).map { "{\"id\":\($0)}" }.joined(separator: ",")
                return (200, "{\"data\":[\(rows)],\"page\":1,\"limit\":25,\"total\":612,\"total_pages\":25}")
            }
            return (200, "{}")
        }
        let client = makeClient()

        // Give the poller several rounds (it fires every 2s) to make sure the
        // value settles rather than just racing a transient default.
        RunLoop.main.run(until: Date().addingTimeInterval(3))
        XCTAssertEqual(client.menuState.completed, 0,
                       "must not mistake the paginated page/array shape for a request total")
    }

    // MARK: - Codable-level pins

    /// Direct decode check pinned to the real upstream field name
    /// (store.ActivityStats.TotalRequests -> json:"total_requests"),
    /// including the histogram fields our client does not model - those must
    /// not derail decoding.
    func testActivityStatsDecodesRealFieldNamesToleratingExtraKeys() throws {
        let json = """
        {"total_requests":137,"total_input_tokens":50000,"total_output_tokens":80000,\
        "total_cache_tokens":12000,\
        "prompt_histogram":{"bins":[1,2,3],"min":0,"max":10,"binSize":1,"p50":2,"p95":8,"p99":9},\
        "gen_histogram":null}
        """.data(using: .utf8)!
        let stats = try JSONDecoder().decode(ActivityStats.self, from: json)
        XCTAssertEqual(stats.totalRequests, 137)
    }

    /// Direct decode check for the merged /api/events "inflight" union
    /// payload: our total/byTier fields plus upstream's operation/requests/
    /// request/id in the same object (internal/swaputil/events.go
    /// InFlightRequestsEvent). This was previously verified only by
    /// inspection (Codable ignores unrecognized keys by default); pinning it
    /// here means a future upstream merge that changes the union shape again
    /// fails a test instead of silently zeroing the waiting count.
    func testInFlightStatsDecodesUnionPayloadIgnoringUpstreamExtraFields() throws {
        let json = """
        {"total":3,"byTier":{"default":2,"priority":1},"operation":"snapshot",\
        "requests":[{"id":"abc123","timestamp":"2026-08-14T00:00:00Z","model":"cq35",\
        "req_path":"/v1/chat/completions","method":"POST","req_headers":{},\
        "remote_ip":"127.0.0.1","resp_headers":{},"resp_bytes":0,"elapsed_ms":120}],\
        "id":"abc123"}
        """.data(using: .utf8)!
        let stats = try JSONDecoder().decode(InFlightStats.self, from: json)
        XCTAssertEqual(stats.total, 3)
        XCTAssertEqual(stats.byTier, ["default": 2, "priority": 1])
    }
}
