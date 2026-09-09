import XCTest
@testable import LlamaSwapMenuCore

/// Pins the row grammar llama-cm's session-identity contract fixes for both
/// renderers (docs/intent/session-identity-contract.md). A row must let an
/// operator answer, at a glance: which session, which model, doing what, in
/// which phase - and whether it is a dispatched child.
final class SessionRowDisplayTests: XCTestCase {
    func testSessionRowShowsBracketedIdAliasTitleAndWord() {
        let row = SessionRow(id: "41", origin: "3bf85c8c", model: "cq35", tier: "-",
                             word: "DECODE", hasSession: true, title: "llama-cm queue work")
        XCTAssertEqual(row.displayLine, "[3bf85c8c] cq35 · llama-cm queue work · DECODE")
    }

    func testRowWithoutSessionShowsClientFamilyUnbracketed() {
        let row = SessionRow(id: "41", origin: "hermes", model: "cq35", tier: "-",
                             word: "PREFILL", hasSession: false)
        XCTAssertEqual(row.displayLine, "hermes · cq35 · PREFILL")
    }

    func testDispatchedChildNamesItsParent() {
        let row = SessionRow(id: "41", origin: "3bf85c8c", model: "cq27", tier: "-",
                             word: "PARKED", hasSession: true, title: "subagent run",
                             parent: "aa11bb22")
        XCTAssertEqual(row.displayLine, "[3bf85c8c] cq27 · subagent run · PARKED (child of aa11bb22)")
    }

    /// A row with no title known must not render an empty segment: cm-menu may
    /// not have published one yet, and the rest of the row still has to read.
    func testMissingTitleDropsTheSegment() {
        let row = SessionRow(id: "41", origin: "3bf85c8c", model: "cq35", tier: "-",
                             word: "FLAT", hasSession: true)
        XCTAssertEqual(row.displayLine, "[3bf85c8c] cq35 · FLAT")
    }

    func testTrailingDetailFollowsTheWord() {
        let row = SessionRow(id: "41", origin: "3bf85c8c", model: "cq35", tier: "-",
                             word: "DECODE", detail: "98.9k · 12.4 t/s",
                             hasSession: true, title: "review the fork")
        XCTAssertEqual(row.displayLine, "[3bf85c8c] cq35 · review the fork · DECODE · 98.9k · 12.4 t/s")
    }
}

/// Pins the title channel: cm-menu writes the file, this reader shows the
/// first 20 characters, and nothing else derives a title.
final class SessionTitleStoreTests: XCTestCase {
    private func writeTitles(_ contents: String) throws -> String {
        let path = NSTemporaryDirectory() + "session-titles-\(UUID().uuidString).tsv"
        try contents.write(toFile: path, atomically: true, encoding: .utf8)
        addTeardownBlock { try? FileManager.default.removeItem(atPath: path) }
        return path
    }

    func testLooksUpByShortIdAndTruncatesToTwentyCharacters() throws {
        let path = try writeTitles("3bf85c8c\tfix the queue starvation bug\naa11bb22\tshort one\n")
        let store = SessionTitleStore(path: path)
        store.refresh()

        XCTAssertEqual(store.title(forSessionID: "3bf85c8c"), "fix the queue starva")
        XCTAssertEqual(store.title(forSessionID: "aa11bb22"), "short one")
    }

    /// The proxy reports a FULL session uuid; the file is keyed by the short
    /// form, so the lookup has to bridge the two.
    func testFullSessionUUIDResolvesAgainstShortKeys() throws {
        let path = try writeTitles("3bf85c8c\tqueue work\n")
        let store = SessionTitleStore(path: path)
        store.refresh()

        XCTAssertEqual(store.title(forSessionID: "3bf85c8c-1234-4321-aaaa-bbbbccccdddd"), "queue work")
    }

    func testUnknownSessionAndMissingFileYieldNoTitle() {
        let store = SessionTitleStore(path: NSTemporaryDirectory() + "does-not-exist-\(UUID().uuidString).tsv")
        store.refresh()

        XCTAssertNil(store.title(forSessionID: "3bf85c8c"))
        XCTAssertNil(store.title(forSessionID: nil))
    }

    /// A title may contain tabs; a session id may not. Splitting on the first
    /// tab only keeps such a title intact instead of truncating it there.
    func testParseSplitsOnTheFirstTabOnly() {
        let parsed = SessionTitleStore.parse("3bf85c8c\ta\ttitle\twith\ttabs\n\nbroken-line\n")
        XCTAssertEqual(parsed["3bf85c8c"], "a\ttitle\twith\ttabs")
        XCTAssertNil(parsed["broken-line"])
    }

    /// cm-menu rewrites the file whole on every refresh, so a rename must be
    /// picked up rather than served from the cached read.
    func testRefreshPicksUpARewrittenFile() throws {
        let path = try writeTitles("3bf85c8c\tfirst title\n")
        let store = SessionTitleStore(path: path)
        store.refresh()
        XCTAssertEqual(store.title(forSessionID: "3bf85c8c"), "first title")

        // Force a distinct modification date: the store skips an unchanged
        // file, and a same-second rewrite would otherwise be indistinguishable.
        try "3bf85c8c\tsecond title\n".write(toFile: path, atomically: true, encoding: .utf8)
        try FileManager.default.setAttributes([.modificationDate: Date().addingTimeInterval(5)],
                                              ofItemAtPath: path)
        store.refresh()
        XCTAssertEqual(store.title(forSessionID: "3bf85c8c"), "second title")
    }
}

/// End-to-end pins through a stub backend: the identity keys the fork now
/// stamps must reach the rendered row, and a KV-parked request must read
/// PARKED no matter what its byte counts say.
final class SessionIdentityRowTests: XCTestCase {
    private var stub: StubBackend!
    private var titlesPath: String!

    override func setUpWithError() throws {
        stub = try StubBackend()
        titlesPath = NSTemporaryDirectory() + "session-titles-\(UUID().uuidString).tsv"
    }

    override func tearDown() {
        stub.stop()
        stub = nil
        if let titlesPath { try? FileManager.default.removeItem(atPath: titlesPath) }
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

    private func makeClient() -> BackendClient {
        let client = BackendClient(baseURL: stub.baseURL, sessionTitlesPath: titlesPath)
        XCTAssertTrue(waitUntil { self.stub.hasEventClient },
                      "client never opened the /api/events stream")
        return client
    }

    /// kv-admission holds a request that has ALREADY been granted a slot and
    /// may have produced bytes; the byte heuristic would call that DECODE.
    /// PARKED has to win.
    func testKVParkedWinsOverBytesAndSlotGrant() {
        let client = makeClient()
        let inner = """
        {"total":1,"granted":1,"operation":"snapshot",\
        "requests":[{"id":"41","timestamp":"2026-09-02T13:21:00Z","model":"Qwen3-35B",\
        "req_path":"/v1/messages","method":"POST",\
        "req_headers":{"User-Agent":"claude-cli/2.1.252"},"remote_ip":"127.0.0.1",\
        "resp_headers":{},"resp_bytes":4096,"elapsed_ms":9000,\
        "metadata":{"session_id":"3bf85c8c-1234-4321-aaaa-bbbbccccdddd","slot_granted":"1","kv_parked":"1"}}],\
        "queue":[]}
        """
        stub.pushEvent(type: "inflight", inner: inner)

        XCTAssertTrue(waitUntil { client.menuState.sessionRows.first?.word == "PARKED" },
                      "kv_parked must read PARKED, got \(client.menuState.sessionRows.first?.word ?? "<none>")")
    }

    func testRowCarriesAliasTitleAndParent() throws {
        try "3bf85c8c\tfix the queue starvation bug\n".write(toFile: titlesPath, atomically: true, encoding: .utf8)
        let client = makeClient()
        let inner = """
        {"total":1,"granted":1,"operation":"snapshot",\
        "requests":[{"id":"41","timestamp":"2026-09-02T13:21:00Z","model":"Qwen3-35B-A3B-Q4",\
        "req_path":"/v1/messages","method":"POST",\
        "req_headers":{"User-Agent":"claude-cli/2.1.252"},"remote_ip":"127.0.0.1",\
        "resp_headers":{},"resp_bytes":800,"elapsed_ms":9000,\
        "metadata":{"session_id":"3bf85c8c-1234-4321-aaaa-bbbbccccdddd","slot_granted":"1",\
        "model_alias":"cq35","client":"claude-code",\
        "parent_session_id":"aa11bb22-3333-4444-5555-666677778888"}}],\
        "queue":[]}
        """
        stub.pushEvent(type: "inflight", inner: inner)

        XCTAssertTrue(waitUntil { client.menuState.sessionRows.count == 1 },
                      "expected 1 session row, got \(client.menuState.sessionRows.count)")
        let row = client.menuState.sessionRows[0]
        XCTAssertEqual(row.model, "cq35", "the row must show the alias, not the model id")
        XCTAssertEqual(row.origin, "3bf85c8c")
        XCTAssertTrue(row.hasSession)
        XCTAssertEqual(row.title, "fix the queue starva")
        XCTAssertEqual(row.parent, "aa11bb22")
        XCTAssertEqual(row.displayLine,
                       "[3bf85c8c] cq35 · fix the queue starva · DECODE (child of aa11bb22)")
    }

    func testSessionLessRowNamesTheClientFamily() {
        let client = makeClient()
        let inner = """
        {"total":1,"granted":1,"operation":"snapshot",\
        "requests":[{"id":"41","timestamp":"2026-09-02T13:21:00Z","model":"Qwen3-27B",\
        "req_path":"/v1/messages","method":"POST",\
        "req_headers":{"User-Agent":"curl/8.7.1"},"remote_ip":"127.0.0.1",\
        "resp_headers":{},"resp_bytes":800,"elapsed_ms":9000,\
        "metadata":{"slot_granted":"1","model_alias":"cq27","client":"curl"}}],\
        "queue":[]}
        """
        stub.pushEvent(type: "inflight", inner: inner)

        XCTAssertTrue(waitUntil { client.menuState.sessionRows.count == 1 },
                      "expected 1 session row, got \(client.menuState.sessionRows.count)")
        let row = client.menuState.sessionRows[0]
        XCTAssertFalse(row.hasSession)
        XCTAssertEqual(row.displayLine, "curl · cq27 · DECODE")
    }
}

/// Pins the client-family label: the proxy classifies the caller now, so a
/// session-less row names a family rather than a raw agent string.
final class SessionOriginClientTests: XCTestCase {
    func testSessionIDStillWinsOverClient() {
        let origin = SessionOrigin.label(
            sessionID: "3bf85c8c-1234-4321-aaaa-bbbbccccdddd", client: "claude-code", userAgent: "curl/8.7.1")
        XCTAssertEqual(origin, "3bf85c8c")
    }

    func testClientFamilyLabelsASessionLessRow() {
        XCTAssertEqual(SessionOrigin.label(sessionID: nil, client: "curl", userAgent: "curl/8.7.1"), "curl")
        XCTAssertEqual(SessionOrigin.label(sessionID: nil, client: "python-sdk", userAgent: nil), "python-sdk")
    }

    /// "other" says no more than the agent string does, so the agent path
    /// still gets its turn - as it does for an entry with no client key at all.
    func testOtherAndMissingClientFallBackToTheUserAgent() {
        XCTAssertEqual(SessionOrigin.label(sessionID: nil, client: "other", userAgent: "Hermes Desktop/2.0"), "hermes")
        XCTAssertEqual(SessionOrigin.label(sessionID: nil, client: nil, userAgent: "llama-swap-menu 1.0 (macOS)"), "llama-swap-menu")
        XCTAssertEqual(SessionOrigin.label(sessionID: nil, client: nil, userAgent: nil), "unknown")
    }
}

/// A subagent (Agent tool) reuses its parent's session id; the proxy now
/// carries metadata.agent_id from X-Claude-Code-Agent-Id. The row must name
/// the agent so an operator can tell the parent's turn from its subagent's
/// (2026-09-08: three "[934b47c6] claude-haiku..." rows, one of them the
/// parent, none distinguishable).
final class SubagentRowDisplayTests: XCTestCase {
    func testSubagentRowNamesItsAgentInsideTheBracket() {
        let row = SessionRow(id: "41", origin: "934b47c6", model: "cq35h", tier: "-",
                             word: "PREFILL", hasSession: true, title: "summarize chat",
                             agent: "a4c4e94e")
        XCTAssertEqual(row.displayLine, "[934b47c6 > a4c4e94e] cq35h · summarize chat · PREFILL")
    }

    func testParentRowStaysUnchangedWithoutAgent() {
        let row = SessionRow(id: "40", origin: "934b47c6", model: "cq35h", tier: "-",
                             word: "DECODE", hasSession: true, agent: nil)
        XCTAssertEqual(row.displayLine, "[934b47c6] cq35h · DECODE")
    }
}
