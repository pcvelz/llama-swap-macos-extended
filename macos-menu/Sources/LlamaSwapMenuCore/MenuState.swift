import Foundation

public struct MenuState: Encodable {
    public var backendOnline = false
    public var completed = 0
    public var waiting = 0
    /// Per-tier waiting breakdown (docs/intent/llama-swap-tiers.md, llama-cm).
    /// Populated only when more than one tier is currently in play among the
    /// rows shown; empty otherwise, so a single-tier deployment renders
    /// exactly as before tiers existed.
    ///
    /// HARD RULE (user, 2026-09-18): waiting must always be in parity with
    /// the slots shown - both are derived from the same sessionRows snapshot
    /// (BackendClient.applyWaitingParity), counting PARKED rows only. There
    /// is deliberately no independent smoothing/anti-flap hold here anymore
    /// (the old 600s peak-hold let "N waiting" show a stale peak with
    /// "Queue: idle" and no PARKED rows in sight) - if a hold is ever wanted
    /// again it must apply to waiting and waitingByTier from the exact same
    /// state so the two can never disagree.
    public var waitingByTier: [String: Int] = [:]
    public var models: [ModelRow] = []
    /// The model the user last picked. Kept after the switch completes: it is
    /// what makes `activeModelID` keep tracking their choice once several models
    /// are resident again.
    public var chosenModelID: String?
    /// Set while the chosen model's switch is still running; drives the
    /// half-bullet so a click that takes tens of seconds is visible.
    public var pendingModelID: String?
    /// Why the last switch failed, or nil. Shown in the menu.
    public var lastSwitchError: String?
    /// Values 0...1 for the configured bar metrics, in configuration order.
    public var barValues: [Double] = [0, 0]

    /// One row per currently in-flight request, derived from the /api/events
    /// "inflight" snapshot/upsert/remove stream by BackendClient - the same
    /// per-session throughput vocabulary as llama-cm's cm-menu (see
    /// SessionThroughput.swift), so the two can never disagree.
    public var sessionRows: [SessionRow] = []
    /// The scheduler's ordered wait list (llama-swap's own authoritative
    /// "no slot granted yet" data, internal/swaputil/events.go QueueEntry) -
    /// never inferred, since the live inflight entry carries no per-request
    /// slot signal (see SessionThroughput.swift's header).
    public var queueRows: [QueueRow] = []

    /// The current swap-grace cooldown (llama-cm llama-swap.yaml
    /// swapGraceSeconds; see fifo.go's grace/idleSince/withinGrace): the
    /// resident model is idle inside its grace while queued requests for
    /// another model wait for it. ONE state on the resident, never one per
    /// waiting model (2026-09-10: two "waiting for cq35" rows). Pushed by the
    /// "swapGrace" SSE event/GET /api/swap-grace (BackendClient.handleEvent);
    /// nil when nothing is held.
    public var cooldown: CooldownRow? = nil

    /// Seconds since the cooldown last RESTARTED (a completed turn of the
    /// resident restarts its grace; between two screenshots on 2026-09-10 the
    /// countdown went 8:52 -> 9:41). Set by BackendClient when a swapGrace
    /// event's remaining goes UP; nil until that has happened once.
    public var cooldownRestartedAt: Date? = nil

    /// A restart is a countdown that went up: the resident finished a turn
    /// inside its own grace. A one-second wobble from tick alignment is not.
    public static func cooldownRestarted(previous: Int?, current: Int) -> Bool {
        guard let previous else { return false }
        return current > previous + 1
    }

    /// The resident's own row while it cools down - the cooldown is a state
    /// of the loaded model, so it is rendered ON that model's line, not as a
    /// separate section: "cooldown 9:41 for [17426df4], then cq35 · 5 waiting",
    /// with "(restarted 0:31 ago)" after the countdown once a turn of the
    /// resident has restarted it. Never "X waiting for <loaded model>".
    ///
    /// A no-waiter cooldown (2026-09-18: the resident is idle inside its own
    /// grace with nothing cross-model queued behind it - CooldownRow.nextModel
    /// empty) omits the ", then <next> · N waiting" suffix entirely: there is
    /// no swap pending, so naming one and counting zero waiters would read as
    /// a real wait that isn't happening.
    public static func cooldownLabel(_ cd: CooldownRow, next: String, restartedAgo: Int? = nil) -> String {
        var s = "cooldown \(CompactFormatter.countdown(cd.remainingSeconds))"
        if let restartedAgo { s += " (restarted \(CompactFormatter.countdown(restartedAgo)) ago)" }
        let owners = hotSlots(cd).map { "[\(String($0.sessionId.prefix(8)))]" }
        if !owners.isEmpty { s += " for " + owners.joined(separator: " ") }
        if !cd.nextModel.isEmpty {
            s += ", then \(next) · \(cd.waiting) waiting"
        }
        return s
    }

    /// The slots the cooldown is actually protecting: only those a session
    /// owns. A free slot is protected by nothing and is not shown (a "slot 1 ·
    /// free" line under a cooldown read as two slots in cooldown, 2026-09-10).
    public static func hotSlots(_ cd: CooldownRow) -> [HotSlotRow] {
        cd.slots.filter { !$0.sessionId.isEmpty }
    }

    /// The phrase a PARKED row shows after the word: the scheduler's
    /// `park_reason` (internal/router/scheduler Park* constants) in words.
    /// `kv_parked` is the older flag for the same KV case. Unknown or absent
    /// renders nothing - never a guess.
    public static func parkDetail(reason: String?, kvParked: Bool) -> String? {
        switch reason ?? "" {
        case "cap": return "slots full"
        case "kv": return "kv pool"
        case "busy": return "resident busy"
        case "cooldown": return "cooldown"
        case "loading": return "loading"
        case "rank": return "behind higher rank"
        case "swap-collision": return "another swap in flight"
        default: return kvParked ? "kv pool" : nil
        }
    }

    /// One hot-slot line under the cooldown row: which session the slot is
    /// being kept warm for (the cooldown's whole purpose - a session's slot
    /// and KV cache survive a tool call or an AskUserQuestion pause), shown
    /// like an active slot would be. Session ids are shown by their first 8
    /// characters, the same short form the session rows use.
    public static func hotSlotLabel(_ s: HotSlotRow) -> String {
        "  slot \(s.slot) · [\(String(s.sessionId.prefix(8)))] · hot, idle \(CompactFormatter.countdown(s.idleSeconds))"
    }

    /// The "Queue: idle" line the design calls for when nothing is parked,
    /// else one summary per queued entry.
    public static func queueSummary(_ rows: [QueueRow]) -> String {
        guard !rows.isEmpty else { return "Queue: idle" }
        return rows
            .sorted { $0.position < $1.position }
            .map { "\($0.position). \($0.tier)/\($0.model)" }
            .joined(separator: ", ")
    }

    /// Derived, never stored: storing it meant recomputing at three call sites
    /// with inputs that drifted apart.
    public var activeModelID: String? {
        MenuState.activeModel(in: models, preferring: chosenModelID)
    }

    /// The waiting-count row text: a per-tier breakdown ("priority 2, default
    /// 1 waiting") when more than one tier is in play, otherwise the plain
    /// "N waiting" string unchanged from before tiers existed.
    public var waitingSummary: String {
        if waitingByTier.count > 1 {
            let parts = waitingByTier.keys.sorted().map { "\($0) \(waitingByTier[$0] ?? 0)" }
            return parts.joined(separator: ", ") + " waiting"
        }
        return "\(waiting) waiting"
    }

    /// Several models can be ready at once, so "first ready row" is just config
    /// order. The user's chosen model wins; a starting model wins only when
    /// nothing is ready yet.
    static func activeModel(in models: [ModelRow], preferring chosen: String?) -> String? {
        if let chosen, models.first(where: { $0.id == chosen })?.state == "ready" {
            return chosen
        }
        if let ready = models.first(where: { $0.state == "ready" }) { return ready.id }
        return models.first { $0.state == "starting" }?.id
    }

    // Hand-written so the debug snapshot still carries the derived activeModelID.
    private enum CodingKeys: String, CodingKey {
        case backendOnline, completed, waiting, waitingByTier, models, chosenModelID
        case pendingModelID, lastSwitchError, barValues, activeModelID
        case sessionRows, queueRows, cooldown
    }

    public func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(backendOnline, forKey: .backendOnline)
        try c.encode(completed, forKey: .completed)
        try c.encode(waiting, forKey: .waiting)
        try c.encode(waitingByTier, forKey: .waitingByTier)
        try c.encode(models, forKey: .models)
        try c.encode(barValues, forKey: .barValues)
        try c.encodeIfPresent(chosenModelID, forKey: .chosenModelID)
        try c.encodeIfPresent(pendingModelID, forKey: .pendingModelID)
        try c.encodeIfPresent(lastSwitchError, forKey: .lastSwitchError)
        try c.encodeIfPresent(activeModelID, forKey: .activeModelID)
        try c.encode(sessionRows, forKey: .sessionRows)
        try c.encode(queueRows, forKey: .queueRows)
        try c.encodeIfPresent(cooldown, forKey: .cooldown)
    }
}

/// The current cooldown - mirrors internal/swaputil/events.go Cooldown
/// exactly (field names match, so the default synthesized Codable decodes
/// it with no CodingKeys needed).
public struct CooldownRow: Codable, Equatable {
    public let evicteeModel: String
    public let nextModel: String
    public let waiting: Int
    public let remainingSeconds: Int
    public let slots: [HotSlotRow]

    public init(evicteeModel: String, nextModel: String, waiting: Int, remainingSeconds: Int, slots: [HotSlotRow]) {
        self.evicteeModel = evicteeModel
        self.nextModel = nextModel
        self.waiting = waiting
        self.remainingSeconds = remainingSeconds
        self.slots = slots
    }
}

/// One slot of the cooling resident and the session it is kept warm for -
/// mirrors internal/swaputil/events.go HotSlot.
public struct HotSlotRow: Identifiable, Codable, Equatable {
    public var id: Int { slot }
    public let slot: Int
    public let sessionId: String
    public let idleSeconds: Int

    public init(slot: Int, sessionId: String, idleSeconds: Int) {
        self.slot = slot
        self.sessionId = sessionId
        self.idleSeconds = idleSeconds
    }
}

/// One in-flight request rendered as a menu row: who it belongs to, which
/// model it hit, and its throughput classification (SessionThroughput.swift).
public struct SessionRow: Identifiable, Encodable, Equatable {
    public let id: String
    public let origin: String
    public let model: String
    /// "-" when the live entry carries no per-request tier (the common case
    /// today - see SessionThroughput.swift's header on why tier isn't
    /// threaded onto the live inflight entry's metadata).
    public let tier: String
    public let word: String
    /// Optional trailing readout, e.g. "98.9k · 12.4 t/s"; nil when no slot can
    /// be joined confidently for this request.
    public let detail: String?
    /// True when the proxy identified a Claude Code session behind this
    /// request (metadata.session_id). Decides whether `origin` renders as a
    /// bracketed session id or as a bare client-family name - the two cases
    /// the row grammar distinguishes.
    public let hasSession: Bool
    /// What that session is working on, from cm-menu's published titles
    /// (SessionTitleStore); nil when no title is known.
    public let title: String?
    /// Short id of the session that DISPATCHED this one, when the request is
    /// a headless child (metadata.parent_session_id).
    public let parent: String?
    /// Short id of the Agent-tool SUBAGENT making this turn
    /// (metadata.agent_id); nil for the session's own turns. A subagent
    /// reuses its parent's session id, so without this the parent's turn and
    /// its subagent's render as identical rows.
    public let agent: String?

    public init(id: String, origin: String, model: String, tier: String, word: String,
                detail: String? = nil, hasSession: Bool = false, title: String? = nil,
                parent: String? = nil, agent: String? = nil) {
        self.id = id
        self.origin = origin
        self.model = model
        self.tier = tier
        self.word = word
        self.detail = detail
        self.hasSession = hasSession
        self.title = title
        self.parent = parent
        self.agent = agent
    }

    /// The row as the menu shows it, per llama-cm's session-identity contract
    /// (docs/intent/session-identity-contract.md):
    ///
    ///   `[<sid8>] <model_alias> · <title20> · <WORD>`
    ///   `[<sid8> > <agent8>] <model_alias> · ...` for a subagent's turn
    ///   `<client> · <model_alias> · <WORD>` when no session is identified
    ///   ... ` (child of <parent8>)` when the request was dispatched
    ///
    /// The trailing token/rate readout, when one could be joined, follows the
    /// word as its own segment. Segments the request cannot supply (no title,
    /// no slot join) are dropped rather than shown empty, so a sparse row
    /// stays readable instead of collapsing into separators.
    public var displayLine: String {
        var segments: [String] = []
        if hasSession {
            let bracket = agent.map { "\(origin) > \($0)" } ?? origin
            segments.append("[\(bracket)] \(model)")
        } else {
            segments.append("\(origin) · \(model)")
        }
        if let title, !title.isEmpty { segments.append(title) }
        segments.append(word)
        if let detail, !detail.isEmpty { segments.append(detail) }
        var line = segments.joined(separator: " · ")
        if let parent, !parent.isEmpty {
            line += " (child of \(parent))"
        }
        return line
    }
}

/// Compact menu numbers: 98.9k / 1.2M for token counts, one decimal for t/s.
public enum CompactFormatter {
    public static func tokens(_ n: Int) -> String {
        if n >= 1_000_000 { return String(format: "%.1fM", Double(n) / 1_000_000) }
        if n >= 1_000 { return String(format: "%.1fk", Double(n) / 1_000) }
        return "\(n)"
    }

    public static func rate(_ tokensPerSecond: Double) -> String {
        String(format: "%.1f t/s", tokensPerSecond)
    }

    /// "4:12" style countdown for a swap-grace hold's remaining seconds.
    /// Clamped at 0 so a stale/negative reading (the hold ending between the
    /// last SSE tick and render) never prints a negative countdown.
    public static func countdown(_ seconds: Int) -> String {
        let s = max(0, seconds)
        return String(format: "%d:%02d", s / 60, s % 60)
    }
}

/// One parked entry from the scheduler's own wait list (llama-swap's
/// QueueEntry), 1-indexed by grant order.
public struct QueueRow: Identifiable, Encodable, Equatable {
    public let id: String
    public let position: Int
    public let tier: String
    public let model: String

    public init(position: Int, tier: String, model: String) {
        self.id = "\(position)-\(tier)-\(model)"
        self.position = position
        self.tier = tier
        self.model = model
    }
}

public struct ModelRow: Codable, Identifiable {
    public let id: String
    public let name: String
    let description: String
    public let state: String
    let unlisted: Bool
    let peerID: String?
    public let aliases: [String]?
    let capabilities: [String: Bool]?
}

struct PerformanceResponse: Codable {
    let gpuStats: [GPUStat]
    let sysStats: [SysStat]

    enum CodingKeys: String, CodingKey {
        case gpuStats = "gpu_stats"
        case sysStats = "sys_stats"
    }
}

struct GPUStat: Codable {
    let gpuUtilPct: Double
    let memUtilPct: Double

    enum CodingKeys: String, CodingKey {
        case gpuUtilPct = "gpu_util_pct"
        case memUtilPct = "mem_util_pct"
    }
}

struct SysStat: Codable {
    let cpuUtilPerCore: [Double]
    let memTotalMB: Int
    let memUsedMB: Int

    enum CodingKeys: String, CodingKey {
        case cpuUtilPerCore = "cpu_util_per_core"
        case memTotalMB = "mem_total_mb"
        case memUsedMB = "mem_used_mb"
    }
}

/// Decodes GET /api/metrics/stats — the aggregate activity-stats object that
/// replaced the old bare-array GET /api/metrics endpoint. Only the total
/// request count is needed for the "N completed" badge; the histogram fields
/// are intentionally left undecoded.
struct ActivityStats: Codable {
    let totalRequests: Int

    enum CodingKeys: String, CodingKey {
        case totalRequests = "total_requests"
    }
}

struct EventEnvelope: Codable {
    let type: String
    let data: String
}

struct InFlightStats: Codable {
    let total: Int
    let byTier: [String: Int]?
    /// "snapshot" | "upsert" | "remove" (internal/server/inflight.go
    /// inflightOperation*). Optional so the pre-merge minimal payload this
    /// struct originally decoded still parses.
    let operation: String?
    /// Present on a "snapshot" event: every currently tracked request.
    let requests: [InflightRequestEntry]?
    /// Present on an "upsert" event: the one request that changed.
    let request: InflightRequestEntry?
    /// Present on a "remove" event: the id of the request that finished.
    let id: String?
    /// The scheduler's ordered wait list, first-in-line first; nil when no
    /// tier reporter is wired (single-tier deployments).
    let queue: [QueueEntry]?
}

/// Mirrors internal/swaputil/events.go InflightRequestEntry - only the
/// fields the menu bar's session rows need (id, model, req_path, resp_bytes,
/// elapsed_ms, metadata, and req_headers for the User-Agent origin fallback).
/// Unmodeled fields (timestamp, method, resp_headers, remote_ip) are ignored
/// by Codable, not decoded.
struct InflightRequestEntry: Codable {
    let id: String
    let model: String
    let reqPath: String
    let respBytes: Int64
    let elapsedMs: Int64
    let metadata: [String: String]?
    let reqHeaders: [String: String]?

    enum CodingKeys: String, CodingKey {
        case id, model, metadata
        case reqPath = "req_path"
        case respBytes = "resp_bytes"
        case elapsedMs = "elapsed_ms"
        case reqHeaders = "req_headers"
    }
}

/// Mirrors internal/swaputil/events.go QueueEntry. `arrived` is intentionally
/// left undecoded - the menu only needs position/tier/model.
struct QueueEntry: Codable {
    let position: Int
    let tier: String
    let model: String
}

/// The "swapGrace" SSE event's payload / GET /api/swap-grace's body -
/// internal/server/apigroup.go handleAPISwapGrace and the SSE sibling event
/// in handleAPIEvents both wrap the single cooldown (or null) in a
/// "cooldown" key.
struct CooldownPayload: Codable {
    let cooldown: CooldownRow?
}
