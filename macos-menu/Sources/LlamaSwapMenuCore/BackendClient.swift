import Foundation
import Combine

public final class BackendClient: ObservableObject {
    private let baseURL: URL
    public let bars: [BarMetric]
    private var cancellables = Set<AnyCancellable>()
    private var eventSession: URLSession?
    /// Requests currently tracked in flight, keyed by id, kept in sync with
    /// llama-swap's own inflightTracker via the "inflight" SSE stream's
    /// snapshot/upsert/remove operations (see applyInflightEntries).
    private var inflightEntries: [String: InflightRequestEntry] = [:]
    /// Per-request throughput classification, shared vocabulary with
    /// llama-cm's cm-menu (see SessionThroughput.swift).
    private let throughputTracker = SessionThroughputTracker()
    /// Session titles cm-menu publishes; the proxy cannot know them (see
    /// SessionTitleStore).
    private let titleStore: SessionTitleStore

    @Published public var menuState = MenuState() {
        didSet { writeDebugSnapshot() }
    }

    /// Resolves the llama-swap base URL. The parent llama-swap process passes
    /// its own listen address via LLAMA_SWAP_MENU_BASE_URL when it launches
    /// this helper; the fallback matches llama-swap's default listen port.
    /// A value without an http(s) scheme would still parse as a URL (host
    /// becomes the scheme) and silently break every request, so anything
    /// malformed falls back to the default.
    ///
    /// Public because `InstanceGuard` keys its single-instance lock on the same
    /// value: resolving the backend twice by different rules would let two
    /// helpers polling one backend take two different locks.
    public static func defaultBaseURL() -> URL {
        let env = ProcessInfo.processInfo.environment["LLAMA_SWAP_MENU_BASE_URL"] ?? ""
        if env.hasPrefix("http://") || env.hasPrefix("https://"), let url = URL(string: env) {
            return url
        }
        return URL(string: "http://127.0.0.1:8080")!
    }

    /// sessionTitlesPath overrides where cm-menu's published titles are read
    /// from; the default is the one path cm-menu writes. Tests point it at a
    /// temporary file.
    public init(baseURL: URL? = nil, sessionTitlesPath: String? = nil,
                laneLingerSeconds: TimeInterval = BackendClient.defaultLaneLingerSeconds) {
        self.baseURL = baseURL ?? Self.defaultBaseURL()
        self.laneLingerSeconds = laneLingerSeconds
        self.titleStore = SessionTitleStore(path: sessionTitlesPath)
        self.bars = BarMetric.parseList(ProcessInfo.processInfo.environment["LLAMA_SWAP_MENU_BARS"])
        // Size the initial bar values to the configured bar count so the icon
        // renders the right number of (empty) bars before the first poll.
        self.menuState.barValues = Array(repeating: 0, count: self.bars.count)
        startPolling()
        startEventSource()
    }

    private func writeDebugSnapshot() {
        guard ProcessInfo.processInfo.environment["LLAMA_MENU_DEBUG_STATE"] == "1" else { return }
        let path = "/tmp/llama-swap-menu-state.json"
        if let data = try? JSONEncoder().encode(menuState),
           let text = String(data: data, encoding: .utf8) {
            try? text.write(toFile: path, atomically: true, encoding: .utf8)
        }
    }

    private func startPolling() {
        Timer.publish(every: 2.0, on: .main, in: .common)
            .autoconnect()
            .sink { [weak self] _ in
                self?.fetchPerformance()
                self?.fetchMetrics()
            }
            .store(in: &cancellables)
    }

    private func fetchPerformance() {
        let url = baseURL.appendingPathComponent("/api/performance")
        URLSession.shared.dataTask(with: url) { [weak self] data, _, _ in
            guard let self, let data else { return }
            if let resp = try? JSONDecoder().decode(PerformanceResponse.self, from: data) {
                let values = self.bars.map { Self.value(for: $0, from: resp) }
                DispatchQueue.main.async {
                    // Mutate via a single assignment so the didSet observer
                    // (debug snapshot writer) fires once per poll, not once
                    // per property.
                    var state = self.menuState
                    state.barValues = values
                    state.backendOnline = true
                    self.menuState = state
                }
            }
        }.resume()
    }

    /// Extracts the latest normalized 0...1 reading for one bar metric.
    private static func value(for metric: BarMetric, from resp: PerformanceResponse) -> Double {
        switch metric {
        case .gpu:
            return (resp.gpuStats.last?.gpuUtilPct ?? 0) / 100.0
        case .vram:
            return (resp.gpuStats.last?.memUtilPct ?? 0) / 100.0
        case .cpu:
            guard let cores = resp.sysStats.last?.cpuUtilPerCore, !cores.isEmpty else { return 0 }
            return cores.reduce(0, +) / Double(cores.count) / 100.0
        case .ram:
            guard let sys = resp.sysStats.last, sys.memTotalMB > 0 else { return 0 }
            return Double(sys.memUsedMB) / Double(sys.memTotalMB)
        }
    }

    private func fetchMetrics() {
        // GET /api/metrics/activity is paginated (sqlite-backed, default page
        // size 25) - counting its "data" array would report a page size, not
        // a true total. /api/metrics/stats carries the aggregate total_requests
        // count directly.
        let url = baseURL.appendingPathComponent("/api/metrics/stats")
        URLSession.shared.dataTask(with: url) { [weak self] data, _, _ in
            guard let data else { return }
            if let stats = try? JSONDecoder().decode(ActivityStats.self, from: data) {
                DispatchQueue.main.async {
                    self?.menuState.completed = stats.totalRequests
                }
            }
        }.resume()
    }

    private func startEventSource() {
        eventSession?.invalidateAndCancel()
        let delegate = EventSourceDelegate { [weak self] data in
            self?.handleEvent(data)
        }
        delegate.reconnect = { [weak self] in
            self?.startEventSource()
        }
        let config = URLSessionConfiguration.default
        config.timeoutIntervalForRequest = TimeInterval.greatestFiniteMagnitude
        let session = URLSession(configuration: config, delegate: delegate, delegateQueue: .main)
        eventSession = session
        let url = baseURL.appendingPathComponent("/api/events")
        var request = URLRequest(url: url)
        request.setValue("text/event-stream", forHTTPHeaderField: "Accept")
        session.dataTask(with: request).resume()
    }

    /// Latest llama-server slot snapshots by model id, plus the last token
    /// counter sampled per request id. Main-thread only, like `menuState`.
    private var slotCache: [String: [SlotInfo]] = [:]
    private var slotSamples: [String: SlotSample] = [:]
    private var slotTimer: Timer?

    private struct SlotInfo: Decodable {
        // llama-server's /slots wraps next_token in a single-element array,
        // not a bare object; decoding it as an object throws on every
        // response and silently drops the whole payload upstream (`try?`).
        struct NextToken: Decodable { let n_decoded: Int? }
        let id: Int
        let is_processing: Bool
        let n_prompt_tokens: Int?
        let n_prompt_tokens_processed: Int?
        let next_token: [NextToken]?

        var nextTokenInfo: NextToken? { next_token?.first }
    }

    /// Tracks BOTH slot counters independently rather than a single `value`
    /// keyed to the byte-heuristic word - see slotDetail's header comment for
    /// why trusting only one of them prints a false "0.0 t/s".
    private struct SlotSample {
        var prefillProcessed: Int
        var decoded: Int
        var at: Date
        /// When either counter last MOVED. The rate is measured over this
        /// span, not over the poll gap: llama-server advances
        /// n_prompt_tokens_processed once per ubatch (2048 tokens, ~10s at
        /// 240 tok/s on cq35h, measured 2026-09-08), so a 2s poll that lands
        /// on the step used to print 2048/2s = ~1000 t/s for one tick and
        /// nothing for the next four.
        var lastChangeAt: Date
    }

    /// How long the last real rate keeps showing while the slot still
    /// reports is_processing and its counters have not moved. Sized for the
    /// prefill step cadence above (one step per ubatch, 10s+ on a slow
    /// prefill) with headroom; a slot that has genuinely stopped drops
    /// is_processing and never reaches this hold at all, so the only reading
    /// this can prolong is a wedged slot's, and 30s is a fair time to keep
    /// calling that "still going".
    private static let processingRateHoldSeconds: TimeInterval = 30.0

    /// The last rate text actually computed from a moving counter, plus when
    /// it was computed - lets a brief poll-to-poll gap with no delta (a
    /// single stalled tick, not a real stop) keep showing the last real
    /// number instead of flickering to a bare total and back (user report
    /// 2026-09-08: "if token/sec is not there for a few 100ms, it's
    /// flickering gone, then back"). Kept separate from `slotSamples`, which
    /// must keep updating every tick regardless so the NEXT delta is correct.
    private struct SlotRateHold {
        var text: String
        var at: Date
    }
    private var slotRateHolds: [String: SlotRateHold] = [:]
    /// The last WHOLE readout ("98.9k · 12.4 t/s") a lane rendered, and when.
    /// The rate hold above only runs once a /slots row was joined to the
    /// lane; when the join itself fails the entire readout used to drop in
    /// one go (user report 2026-09-08, second round: the token total and the
    /// rate flicker off together). Two live producers of a failed join: the
    /// fallback join needs exactly one processing slot, so every small
    /// background request on the other slot (pii-detect curls, ~3s each)
    /// blanks the row; and at a turn boundary the slot cache is wiped and the
    /// next poll takes 5-13s against a prefilling child. A reading a few
    /// seconds stale is allowed; a blank is not.
    private var slotReadoutHolds: [String: SlotRateHold] = [:]
    /// How long a stale readout (the rate, or the whole "total · rate" line)
    /// keeps showing across quiet or failed ticks before it is finally
    /// dropped - long enough to ride out one missed 2s poll tick or one ~3s
    /// background request on the peer slot, short enough that a genuinely
    /// stopped slot doesn't show a fake reading forever. One window for both
    /// holds on purpose: the user sees one readout, not two fields.
    private static let rateHoldSeconds: TimeInterval = 5.0

    /// How long a lane's ROW stays on screen after its last request is
    /// removed. A Claude Code session in its tool loop has exactly one
    /// request on the proxy per turn: it ends, the client runs a tool for
    /// 1-3s (measured on the live box 2026-09-09, /slots at 1Hz), then the
    /// next turn's request arrives. Without this window the lane empties, the
    /// row vanishes, and it is re-appended and re-sorted a beat later - rows
    /// disappearing and reappearing on every single turn. 10s covers an
    /// ordinary Bash/Read tool call with headroom; a longer absence means the
    /// session genuinely left the box and the row SHOULD go, so this is
    /// deliberately not generous. The holds above cover the readout NUMBERS
    /// across the same gap; this one covers the row's existence.
    public static let defaultLaneLingerSeconds: TimeInterval = 10.0
    /// Injectable so tests can exercise expiry without sleeping the real
    /// window; production always uses the default above.
    public let laneLingerSeconds: TimeInterval

    /// Lanes with no in-flight request that are still being rendered, with
    /// the row as it last looked and the moment its last request went away.
    private var lingeringLanes: [String: (row: SessionRow, since: Date)] = [:]
    /// The last row each lane rendered, so a lane that empties has something
    /// to linger WITH (position, origin, model, tier, title, parent/agent).
    private var lastRowByLane: [String: SessionRow] = [:]
    /// Fires the expiry sweep even when no further SSE event arrives - a
    /// session that left the box produces no events at all, so an
    /// event-driven sweep alone would strand its row forever.
    private var lingerTimer: Timer?

    private final class EventSourceDelegate: NSObject, URLSessionDataDelegate {
        var onEvent: (String) -> Void
        var reconnect: (() -> Void)?
        private var lineBuffer = Data()
        private var eventBuffer = ""

        init(onEvent: @escaping (String) -> Void) {
            self.onEvent = onEvent
        }

        func urlSession(_ session: URLSession, dataTask: URLSessionDataTask, didReceive data: Data) {
            lineBuffer.append(data)
            while let range = lineBuffer.range(of: Data("\n".utf8)) {
                let lineData = lineBuffer.subdata(in: lineBuffer.startIndex..<range.lowerBound)
                lineBuffer.removeSubrange(lineBuffer.startIndex...range.upperBound.advanced(by: -1))
                var line = String(data: lineData, encoding: .utf8) ?? ""
                if line.hasSuffix("\r") { line.removeLast() }
                if line.hasPrefix("data:") {
                    eventBuffer.append(String(line.dropFirst(5)))
                } else if line.isEmpty {
                    let payload = eventBuffer
                    eventBuffer = ""
                    if !payload.isEmpty {
                        onEvent(payload)
                    }
                }
            }
        }

        func urlSession(_ session: URLSession, task: URLSessionTask, didCompleteWithError error: Error?) {
            DispatchQueue.main.asyncAfter(deadline: .now() + 2.0) { [weak self] in
                self?.reconnect?()
            }
        }
    }

    private func handleEvent(_ data: String) {
        guard let payload = data.data(using: .utf8),
              let envelope = try? JSONDecoder().decode(EventEnvelope.self, from: payload) else { return }
        switch envelope.type {
        case "modelStatus":
            if let inner = envelope.data.data(using: .utf8),
               let models = try? JSONDecoder().decode([ModelRow].self, from: inner) {
                // Already on main: the SSE session's delegateQueue is .main.
                var state = self.menuState
                state.models = models
                // The switch is done the moment the chosen model is serving.
                if let pending = state.pendingModelID,
                   models.first(where: { $0.id == pending })?.state == "ready" {
                    state.pendingModelID = nil
                    state.lastSwitchError = nil
                }
                self.menuState = state
            }
        case "inflight":
            if let inner = envelope.data.data(using: .utf8),
               let stats = try? JSONDecoder().decode(InFlightStats.self, from: inner) {
                applyInflightEntries(stats)
                menuState.applyInflight(total: stats.total, byTier: stats.byTier ?? [:])
            }
        case "swapGrace":
            if let inner = envelope.data.data(using: .utf8),
               let payload = try? JSONDecoder().decode(CooldownPayload.self, from: inner) {
                // A countdown that went UP is a restart (the resident finished
                // a turn inside its grace); remember when, so the row can say
                // "restarted 0:31 ago" instead of silently showing a bigger
                // number than a moment ago.
                if let cd = payload.cooldown,
                   MenuState.cooldownRestarted(previous: menuState.cooldown?.remainingSeconds, current: cd.remainingSeconds) {
                    menuState.cooldownRestartedAt = Date()
                }
                if payload.cooldown == nil { menuState.cooldownRestartedAt = nil }
                menuState.cooldown = payload.cooldown
            }
        default:
            break
        }
    }

    /// Keeps `inflightEntries` in sync with one "inflight" event's operation
    /// (snapshot replaces the whole set; upsert/remove touch one id - mirrors
    /// internal/server/inflight.go's own tracker), then rederives the two
    /// presentation lists every event carries: sessionRows (one per in-flight
    /// request, with a throughput word) and queueRows (the scheduler's own
    /// wait list, always re-stamped fresh on every event regardless of
    /// operation - internal/server/inflight.go withCountsLocked).
    private func applyInflightEntries(_ stats: InFlightStats) {
        switch stats.operation {
        case "snapshot":
            inflightEntries = Dictionary(uniqueKeysWithValues: (stats.requests ?? []).map { ($0.id, $0) })
        case "upsert":
            if let req = stats.request { inflightEntries[req.id] = req }
        case "remove":
            if let rid = stats.id { inflightEntries.removeValue(forKey: rid) }
        default:
            break
        }

        let now = Date()
        throughputTracker.prune(keeping: Set(inflightEntries.keys))
        // One stat (and at most one read) per event, not per row - see
        // SessionTitleStore.refresh.
        titleStore.refresh()
        // ONE ROW PER LANE, in first-seen order. A Claude Code client keeps
        // two requests open on the proxy around every turn boundary (the
        // finished one is removed a beat after the next arrives), so a
        // per-request list shows two rows per lane and re-sorts on every
        // arrival and removal - the list read as flickering with a parent
        // and two subagents. The lane's newest request is what the row shows;
        // a lane drops out only when it has no request left.
        var newestByLane: [String: InflightRequestEntry] = [:]
        var oldestIDByLane: [String: Int] = [:]
        for entry in inflightEntries.values {
            let lane = Self.laneKey(for: entry)
            let id = Int(entry.id) ?? 0
            oldestIDByLane[lane] = min(oldestIDByLane[lane] ?? id, id)
            if let current = newestByLane[lane], (Int(current.id) ?? 0) >= id { continue }
            newestByLane[lane] = entry
        }
        // LANE LINGER. A lane that just lost its last request is between
        // turns, not gone: keep it in laneOrder (so it holds its position and
        // nothing re-sorts) with the row it last rendered, until the window
        // expires. A lane whose request came back stops lingering at once and
        // is rendered from live data again - the row is reused in place, so
        // the user never sees a removal or an append. Applies to "snapshot"
        // too: a snapshot that no longer lists a lane is the same event as a
        // remove, just delivered wholesale.
        for lane in laneOrder where newestByLane[lane] == nil && lingeringLanes[lane] == nil {
            guard let row = lastRowByLane[lane] else { continue }
            lingeringLanes[lane] = (row: row, since: now)
        }
        for lane in newestByLane.keys { lingeringLanes[lane] = nil }
        expireLingeringLanes(now: now)
        laneOrder.removeAll { newestByLane[$0] == nil && lingeringLanes[$0] == nil }
        // A lane new to the list joins in the order it first APPEARED (its
        // oldest open request), not by its newest request - otherwise a
        // parent whose next turn arrived after its subagent's would jump
        // below that subagent.
        for lane in newestByLane.keys.sorted(by: { oldestIDByLane[$0]! < oldestIDByLane[$1]! })
        where !laneOrder.contains(lane) {
            laneOrder.append(lane)
        }
        menuState.sessionRows = laneOrder
            .compactMap { lane -> SessionRow? in
                // A lingering lane has no entry to classify: re-render the
                // row it last showed, with the honest between-turns word and
                // whatever readout it carried. Its position in laneOrder is
                // untouched, so the row does not move.
                guard let entry = newestByLane[lane] else {
                    guard let held = lingeringLanes[lane]?.row else { return nil }
                    return SessionRow(
                        id: held.id, origin: held.origin, model: held.model,
                        tier: held.tier, word: ThroughputWord.turn.rawValue,
                        detail: held.detail, hasSession: held.hasSession,
                        title: held.title, parent: held.parent, agent: held.agent)
                }
                let meta = entry.metadata ?? [:]
                let sessionID = meta["session_id"]
                let origin = SessionOrigin.label(
                    sessionID: sessionID, client: meta["client"],
                    userAgent: entry.reqHeaders?["User-Agent"])
                // model_alias is the operator-facing name for the model that
                // actually serves; the raw model id is the fallback for an
                // entry from a proxy that predates the key.
                let model = meta["model_alias"] ?? entry.model
                let row = { (word: String, detail: String?) in
                    SessionRow(
                        id: entry.id, origin: origin, model: model,
                        tier: meta["tier"] ?? "-", word: word, detail: detail,
                        hasSession: !(sessionID ?? "").isEmpty,
                        title: self.titleStore.title(forSessionID: sessionID),
                        parent: meta["parent_session_id"].map { String($0.prefix(8)) },
                        agent: meta["agent_id"].map { String($0.prefix(8)) })
                }
                // PARKED WINS BEFORE ANY BYTE HEURISTIC. Two things park a
                // request: kv-admission holding it (metadata.kv_parked), and
                // the scheduler not having granted it a slot yet (no
                // metadata.slot_granted - a granted request carries "1"). A
                // parked request emits no output bytes, and on the Anthropic
                // path may still emit keepalives, so a byte heuristic reads
                // it as FLAT, i.e. stalled, when it is merely waiting. These
                // rows also skip the byte-sample tracker, so a still-parked
                // request never seeds a stale sample that would misclassify
                // it the moment it is granted and starts producing bytes.
                guard meta["kv_parked"] != "1", meta["slot_granted"] == "1" else {
                    // The reason the scheduler stamped (park_reason), so the
                    // row says "PARKED · slots full" / "· cooldown" / "·
                    // loading" instead of a bare PARKED nobody can act on.
                    return row(ThroughputWord.parked.rawValue,
                               MenuState.parkDetail(reason: meta["park_reason"], kvParked: meta["kv_parked"] == "1"))
                }
                let word = throughputTracker.word(
                    forRequestID: entry.id, respBytes: entry.respBytes, elapsedMs: entry.elapsedMs, now: now)
                return row(word.rawValue, nil)
            }
        menuState.queueRows = (stats.queue ?? []).map {
            QueueRow(position: $0.position, tier: $0.tier, model: $0.model)
        }
        // Rows are built with a nil detail above; fill them from the slot
        // cache / readout holds right away so a turn boundary (new request
        // id, same lane) re-shows the lane's last readout instead of a blank
        // until the next /slots poll answers.
        refreshSlotDetails(now: now)
        recordLaneRows()
        scheduleLingerSweep()
        syncSlotPolling()
    }

    /// Remembers each rendered row against its lane so a lane that empties
    /// has something to linger WITH. A lane already lingering is skipped: its
    /// stored row is the last LIVE one, and re-storing the TURN row would
    /// make that word stick to the lane after its next turn starts.
    private func recordLaneRows() {
        guard laneOrder.count == menuState.sessionRows.count else { return }
        for (lane, row) in zip(laneOrder, menuState.sessionRows) where lingeringLanes[lane] == nil {
            lastRowByLane[lane] = row
        }
        let known = Set(laneOrder)
        lastRowByLane = lastRowByLane.filter { known.contains($0.key) }
    }

    /// Drops lanes whose linger window has run out. Silent when nothing has
    /// expired, so it is safe to call on every event and every sweep tick.
    private func expireLingeringLanes(now: Date) {
        lingeringLanes = lingeringLanes.filter { now.timeIntervalSince($0.value.since) <= laneLingerSeconds }
    }

    /// Expiry must fire with NO further SSE events: a session that left the
    /// box stops producing events entirely, so an event-driven sweep alone
    /// would strand its row on screen forever. The timer only exists while
    /// something is lingering, and stops as soon as the last one resolves.
    private func scheduleLingerSweep() {
        guard !lingeringLanes.isEmpty else {
            lingerTimer?.invalidate()
            lingerTimer = nil
            return
        }
        guard lingerTimer == nil else { return }
        // Sub-window ticks so a row disappears close to the window rather
        // than up to a whole window late. .common for the same reason as the
        // slot timer: an open menu's tracking loop does not run .default.
        let interval = max(0.2, laneLingerSeconds / 4)
        let timer = Timer(timeInterval: interval, repeats: true) { [weak self] _ in
            self?.sweepLingeringLanes()
        }
        RunLoop.main.add(timer, forMode: .common)
        lingerTimer = timer
    }

    private func sweepLingeringLanes() {
        let before = Set(lingeringLanes.keys)
        expireLingeringLanes(now: Date())
        defer { scheduleLingerSweep() }
        guard before != Set(lingeringLanes.keys) else { return }
        guard laneOrder.count == menuState.sessionRows.count else { return }
        let live = Set(inflightEntries.values.map { Self.laneKey(for: $0) })
        var keptLanes: [String] = []
        var keptRows: [SessionRow] = []
        for (lane, row) in zip(laneOrder, menuState.sessionRows)
        where live.contains(lane) || lingeringLanes[lane] != nil {
            keptLanes.append(lane)
            keptRows.append(row)
        }
        laneOrder = keptLanes
        menuState.sessionRows = keptRows
        let known = Set(laneOrder)
        lastRowByLane = lastRowByLane.filter { known.contains($0.key) }
    }

    /// Poll `/slots` only while there is inflight work; one GET per distinct
    /// model per tick. Everything lands back on main before touching state.
    private func syncSlotPolling() {
        guard !inflightEntries.isEmpty else {
            DispatchQueue.main.async {
                self.slotTimer?.invalidate()
                self.slotTimer = nil
                self.slotCache.removeAll()
                self.slotSamples.removeAll()
                self.slotRateHolds.removeAll()
                // slotReadoutHolds is deliberately NOT cleared here: the
                // inflight set is empty for an instant at every turn
                // boundary, and the hold is what carries the lane's readout
                // across that gap until the next turn's first poll answers.
                // Entries age out through the hold window on their own.
            }
            return
        }
        guard slotTimer == nil else { return }
        // NOT Timer.scheduledTimer: that schedules on the run loop's .default
        // mode only, which an open NSStatusItem menu's tracking loop does not
        // run (.eventTracking instead) - the exact moment someone is looking
        // at the menu to see the rate, its ticks stop firing. .common covers
        // both.
        let timer = Timer(timeInterval: 2.0, repeats: true) { [weak self] _ in
            self?.pollSlots()
        }
        RunLoop.main.add(timer, forMode: .common)
        slotTimer = timer
        pollSlots()
    }

    /// First-seen order of the lanes currently rendered; see applyInflightEntries.
    private var laneOrder: [String] = []

    /// The identity a menu row stands for: a Claude Code session, or one of
    /// its Agent-tool subagents (same session_id, own agent_id). A request
    /// without a session is its own lane, so anonymous rows still render.
    static func laneKey(for entry: InflightRequestEntry) -> String {
        let meta = entry.metadata ?? [:]
        guard let session = meta["session_id"], !session.isEmpty else { return "req:" + entry.id }
        if let agent = meta["agent_id"], !agent.isEmpty { return session + "/" + agent }
        return session
    }

    /// The model whose /slots answer for this entry. A resident-alias request
    /// (claude-haiku-*, default) keeps the alias as entry.model, and an alias
    /// has no /upstream route - polling it 404s every tick and the row's slot
    /// readout vanishes. The proxy stamps metadata.resolved_model with the
    /// model that serves; follow it when present.
    static func slotModel(for entry: InflightRequestEntry) -> String {
        entry.metadata?["resolved_model"] ?? entry.model
    }

    private func pollSlots() {
        // Only entries that hold a slot have slots to read. A PARKED entry's
        // model is by definition not resident; polling its /slots is answered
        // 503 by the proxy now, and before that fix every such poll sat in
        // the scheduler queue as a swap request for 2-4s (2026-09-10). Not
        // sending it at all keeps the swap log clean.
        let models = Set(inflightEntries.values.filter { !Self.isParked($0) }.map { Self.slotModel(for: $0) })
        for model in models {
            let escaped = model.addingPercentEncoding(withAllowedCharacters: .urlPathAllowed) ?? model
            let url = baseURL.appendingPathComponent("/upstream/\(escaped)/slots")
            URLSession.shared.dataTask(with: url) { [weak self] data, _, _ in
                guard let self, let data,
                      let slots = try? JSONDecoder().decode([SlotInfo].self, from: data) else { return }
                DispatchQueue.main.async {
                    self.slotCache[model] = slots
                    self.refreshSlotDetails()
                }
            }.resume()
        }
    }

    /// Which slot counter last MOVED for a lane: .decode when n_decoded
    /// advanced, .prefill when n_prompt_tokens_processed did. Set by
    /// freshSlotDetail on the tick it measures a rate, cleared with the other
    /// per-lane state when the lane parks. This is the slot's own account of
    /// the phase, and it outranks the byte heuristic's PREFILL/DECODE guess:
    /// on the Anthropic streaming path resp_bytes can sit at 0 for a while
    /// after decode has begun (user report 2026-09-08 19:40, "PREFILL · 110k ·
    /// 38 t/s" while the decode counter climbed), and a keepalive byte can
    /// flip it to DECODE while the slot still prefills (the earlier report).
    /// Either way the word contradicted the rate next to it, which is
    /// measured from the counter that actually moved.
    private var slotPhases: [String: ThroughputWord] = [:]

    /// Rewrites the trailing numbers on already-classified rows and lets the
    /// slot's own phase (slotPhases) correct a PREFILL/DECODE word. PARKED and
    /// FLAT are never touched: a parked row has no slot, and FLAT is a stall
    /// verdict from the byte clock that a moving counter would have
    /// prevented anyway. Tracker samples are left exactly as events made them.
    private func refreshSlotDetails(now: Date = Date()) {
        menuState.sessionRows = menuState.sessionRows.map { row in
            guard let entry = inflightEntries[row.id] else { return row }
            // A PARKED row's detail is its park reason, not a slot readout:
            // keep it, there is no slot to read for it.
            let detail = row.word == ThroughputWord.parked.rawValue
                ? row.detail
                : slotDetail(for: entry, word: row.word, now: now)
            var word = row.word
            if word == ThroughputWord.prefill.rawValue || word == ThroughputWord.decode.rawValue,
               let phase = slotPhases[Self.laneKey(for: entry)] {
                word = phase.rawValue
            }
            return SessionRow(id: row.id, origin: row.origin, model: row.model,
                              tier: row.tier, word: word,
                              detail: detail,
                              hasSession: row.hasSession, title: row.title, parent: row.parent,
                              agent: row.agent)
        }
    }

    /// The one definition of "this request holds no slot": kv-admission is
    /// holding it, or the scheduler has not granted it yet. Same flags the
    /// PARKED classification in applyInflightEntries uses; the slot join and
    /// the readout hold must agree with the word or a row contradicts itself.
    static func isParked(_ entry: InflightRequestEntry) -> Bool {
        let meta = entry.metadata ?? [:]
        return meta["kv_parked"] == "1" || meta["slot_granted"] != "1"
    }

    private func joinedSlot(for entry: InflightRequestEntry) -> SlotInfo? {
        guard let slots = slotCache[Self.slotModel(for: entry)] else { return nil }
        let meta = entry.metadata ?? [:]
        // A parked request holds no slot, so there is nothing truthful to
        // join. The proxy stamps slot_affinity on EVERY request of a lane at
        // admission, parked or granted, and a parked subagent turn shares its
        // lane's affinity with the parent's granted turn - so without this
        // guard the affinity branch below joins the parked row to the busy
        // slot and prints the OTHER request's total and rate (user report
        // 2026-09-08, fourth round: three rows all reading "97.6k · 36.9 t/s"
        // with one slot processing and one idle). The fallback branch has the
        // same hole whenever exactly one slot is processing. Same flags as
        // the PARKED classification in applyInflightEntries.
        guard !Self.isParked(entry) else { return nil }
        if let affinity = meta["slot_affinity"], let id = Int(affinity),
           let slot = slots.first(where: { $0.id == id }) { return slot }
        if let sid = meta["slot_id"], let id = Int(sid),
           let slot = slots.first(where: { $0.id == id }) { return slot }
        let processing = slots.filter { $0.is_processing }
        return processing.count == 1 ? processing[0] : nil
    }

    /// Renders the trailing "context · rate" readout for one row's slot.
    ///
    /// Two corrections here, both against real evidence (user report
    /// 2026-09-08, "... · DECODE · 15.5k · 0.0 t/s" with the total visibly
    /// rising between ticks):
    ///
    /// 1. Which /slots counter feeds the rate used to be picked solely from
    ///    `word`, the byte-heuristic classification from SessionThroughput
    ///    (SessionThroughput.swift). On the Anthropic path a keepalive byte
    ///    can flip that heuristic to DECODE while the slot itself is still
    ///    PREFILLING (n_prompt_tokens/n_prompt_tokens_processed climbing,
    ///    n_decoded pinned at 0) - the rate then sampled the flat counter
    ///    (decoded) while the displayed total (built from BOTH counters) kept
    ///    rising from the other one, printing a literal "0.0 t/s" next to a
    ///    rising total: a contradiction. Fix: track both counters every tick
    ///    and report whichever one actually advanced, ignoring `word` for
    ///    this choice - `word` still drives which KEYWORD the row shows, just
    ///    not which counter backs its rate.
    /// 2. Samples used to be keyed by `entry.id`, but a row now shows one LANE
    ///    (applyInflightEntries' one-row-per-lane), whose id is the lane's
    ///    NEWEST request and therefore changes at every turn boundary -
    ///    every boundary reset the sample and printed a bare total with no
    ///    rate for one tick. Fix: key by lane instead, so the sample survives
    ///    a request handoff within the same lane.
    private func slotDetail(for entry: InflightRequestEntry, word: String, now: Date) -> String? {
        let lane = Self.laneKey(for: entry)
        // A PARKED entry is a KNOWN absence from the slot, not a failed join,
        // so the lane's last-good readout must not ride the hold here: with
        // turns rotating every 6-12 s the hold kept a stale "109.6k · 37.5
        // t/s" on a PARKED row for 5 s of every park (user report 2026-09-08
        // 19:25). Dropping the hold (and the rate sample, whose counters
        // restart on the next grant anyway) makes the row go bare the moment
        // the lane parks; the next grant rebuilds both from a real join.
        if Self.isParked(entry) {
            slotReadoutHolds[lane] = nil
            slotRateHolds[lane] = nil
            slotSamples[lane] = nil
            slotPhases[lane] = nil
            return nil
        }
        guard let fresh = freshSlotDetail(for: entry, lane: lane, now: now) else {
            // Failed join: keep the last good readout inside the hold window
            // rather than blanking the row - see slotReadoutHolds.
            guard let held = slotReadoutHolds[lane],
                  now.timeIntervalSince(held.at) <= Self.rateHoldSeconds else { return nil }
            return held.text
        }
        slotReadoutHolds[lane] = SlotRateHold(text: fresh, at: now)
        return fresh
    }

    private func freshSlotDetail(for entry: InflightRequestEntry, lane: String, now: Date) -> String? {
        guard let slot = joinedSlot(for: entry) else { return nil }
        let prompt = slot.n_prompt_tokens ?? 0
        let decoded = slot.nextTokenInfo?.n_decoded ?? 0
        let context = CompactFormatter.tokens(prompt + decoded)
        guard slot.is_processing else { return context }
        let prefillProcessed = slot.n_prompt_tokens_processed ?? prompt
        guard let previous = slotSamples[lane] else {
            slotSamples[lane] = SlotSample(prefillProcessed: prefillProcessed, decoded: decoded,
                                           at: now, lastChangeAt: now)
            return context
        }
        let decodedDelta = decoded - previous.decoded
        let prefillDelta = prefillProcessed - previous.prefillProcessed
        // A negative delta means the slot was reused by a fresh request
        // (counters restarted): re-baseline, show no rate until it moves.
        if decodedDelta < 0 || prefillDelta < 0 {
            slotSamples[lane] = SlotSample(prefillProcessed: prefillProcessed, decoded: decoded,
                                           at: now, lastChangeAt: now)
            // The new request's phase is unknown until a counter moves; do
            // not let the previous request's phase label it.
            slotPhases[lane] = nil
            return context
        }
        // Decode motion wins when both counters move in one tick: a slot that
        // has produced a token is past prefill, whatever the prefill counter's
        // final catch-up step says.
        if decodedDelta > 0 {
            slotPhases[lane] = .decode
        } else if prefillDelta > 0 {
            slotPhases[lane] = .prefill
        }
        let moved = decodedDelta > 0 || prefillDelta > 0
        let span = now.timeIntervalSince(previous.lastChangeAt)
        slotSamples[lane] = SlotSample(prefillProcessed: prefillProcessed, decoded: decoded,
                                       at: now, lastChangeAt: moved ? now : previous.lastChangeAt)
        // No motion this tick (or a poll landed within the same instant as the
        // step): the slot is still processing, so keep the last real rate.
        guard moved, span > 0.25 else { return heldRateDetail(lane: lane, context: context, now: now) }
        // Prefer whichever counter actually moved, measured over the time
        // since it LAST moved - see SlotSample.lastChangeAt.
        let delta = decodedDelta > 0 ? decodedDelta : prefillDelta
        let freshRate = CompactFormatter.rate(Double(delta) / span)
        slotRateHolds[lane] = SlotRateHold(text: freshRate, at: now)
        return "\(context) · \(freshRate)"
    }

    /// Falls back to the last real rate for `lane` while it is still within
    /// the hold window, rather than dropping straight to a bare total the
    /// moment one tick shows no delta - see slotRateHolds' header comment.
    private func heldRateDetail(lane: String, context: String, now: Date) -> String {
        // Only reached while the slot reports is_processing, so the longer
        // processing hold applies - see processingRateHoldSeconds.
        guard let held = slotRateHolds[lane],
              now.timeIntervalSince(held.at) <= Self.processingRateHoldSeconds else {
            return context
        }
        return "\(context) · \(held.text)"
    }

    public func unloadAll() {
        var request = URLRequest(url: baseURL.appendingPathComponent("/api/models/unload"))
        request.httpMethod = "POST"
        URLSession.shared.dataTask(with: request) { _, _, _ in }.resume()
    }

    /// Ends the current cooldown immediately - the menu's cooldown row click.
    /// The row clears optimistically the moment the click lands (the click IS
    /// the operator ending the cooldown; waiting for the next swapGrace SSE
    /// tick reads as a dead click), and SSE re-adds it within ~1s if the
    /// cooldown genuinely persists. A failed POST restores the row and
    /// surfaces the error: a dead click and a successful one must never look
    /// identical (witnessed 2026-09-09). There is no model to name: the
    /// cooldown is a singleton on the resident.
    public func finishCooldown() {
        let removed = menuState.cooldown
        menuState.cooldown = nil

        var request = URLRequest(url: baseURL.appendingPathComponent("/api/swap-grace/finish"))
        request.httpMethod = "POST"
        URLSession.shared.dataTask(with: request) { [weak self] _, response, error in
            let status = (response as? HTTPURLResponse)?.statusCode ?? 0
            let failed = error != nil || status < 200 || status >= 300
            guard failed, let self else { return }
            DispatchQueue.main.async {
                // Restore only if SSE has not already re-published a cooldown
                // in the meantime.
                if self.menuState.cooldown == nil {
                    self.menuState.cooldown = removed
                }
                self.menuState.lastSwitchError = "finish cooldown failed"
            }
        }.resume()
    }

    /// Switches the backend to `modelID`. This is the menu's click handler:
    /// `MenuView`'s per-model Button calls exactly this.
    ///
    /// A bare `POST /upstream/<model>/` is not enough: it enters the scheduler as
    /// ordinary traffic, and llama-swap will not evict a model that still has
    /// in-flight requests or is inside its swap-grace window, so on a busy backend
    /// it parks in the queue until the client timeout and nothing changes. Picking
    /// a model is a command, so the incumbent is unloaded first - which does cancel
    /// its in-flight work, the same as "Unload All".
    public func load(modelID: String) {
        DispatchQueue.main.async { [weak self] in
            guard let self else { return }
            var state = self.menuState

            // Already serving: nothing to switch. Returning here also protects a
            // second resident model (pin/keep-warm) from being evicted as the
            // "incumbent" of a no-op click.
            guard state.models.first(where: { $0.id == modelID })?.state != "ready" else {
                state.chosenModelID = modelID
                self.menuState = state
                return
            }

            state.chosenModelID = modelID
            state.pendingModelID = modelID
            state.lastSwitchError = nil
            self.menuState = state
            self.armSwitchTimeout(for: modelID)

            let incumbent = state.models.first {
                $0.id != modelID && ($0.state == "ready" || $0.state == "starting")
            }?.id

            guard let incumbent else {
                self.postUpstreamLoad(modelID)
                return
            }
            self.unload(modelID: incumbent) { [weak self] in
                // A newer click supersedes this one; without the guard both loads
                // race, and a switch back to `incumbent` would be undone here.
                guard let self, self.menuState.pendingModelID == modelID else { return }
                self.postUpstreamLoad(modelID)
            }
        }
    }

    /// Fails the switch if the model never reports ready. Covers a load that is
    /// accepted at the HTTP level but whose process crash-loops or stalls, which
    /// would otherwise leave the half-bullet up forever.
    private func armSwitchTimeout(for modelID: String) {
        DispatchQueue.main.asyncAfter(deadline: .now() + 300) { [weak self] in
            guard let self, self.menuState.pendingModelID == modelID else { return }
            self.menuState.pendingModelID = nil
            self.menuState.lastSwitchError = "\(modelID) did not become ready"
        }
    }

    /// Unloads one model. Synchronous server-side: llama-swap has stopped the
    /// process by the time this responds, which is what frees the follow-up load
    /// to start instead of parking behind the incumbent.
    private func unload(modelID: String, completion: @escaping () -> Void) {
        var request = URLRequest(url: baseURL.appendingPathComponent("/api/models/unload/\(modelID)"))
        request.httpMethod = "POST"
        request.timeoutInterval = 120
        URLSession.shared.dataTask(with: request) { _, _, _ in
            // Proceed even if the unload failed - the load is still worth trying,
            // and the pending marker surfaces a switch that never completes.
            DispatchQueue.main.async { completion() }
        }.resume()
    }

    /// Triggers the load. POST because GET /upstream/<model>/ answers 503 while the
    /// model is stopped (a guard against health-pollers eager-reloading it).
    ///
    /// The status code is NOT the success signal: llama-server's root handler
    /// answers this empty POST with 404/415 even on a successful load. Only
    /// transport failures are reported; success is decided by the modelStatus
    /// stream clearing `pendingModelID`.
    private func postUpstreamLoad(_ modelID: String) {
        var request = URLRequest(url: baseURL.appendingPathComponent("/upstream/\(modelID)/"))
        request.httpMethod = "POST"
        request.timeoutInterval = 300
        URLSession.shared.dataTask(with: request) { [weak self] _, _, error in
            guard let self, let error else { return }
            DispatchQueue.main.async {
                guard self.menuState.pendingModelID == modelID else { return }
                self.menuState.pendingModelID = nil
                self.menuState.lastSwitchError = error.localizedDescription
            }
        }.resume()
    }
}
