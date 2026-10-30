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
    /// no PARKED rows in sight) - if a hold is ever wanted
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

    /// One row per session in the session-state contract's `sessions[]`
    /// (llama-cm docs/intent/session-state-contract.md), pushed by the
    /// "sessions" SSE event and decoded straight into `SessionRow` - see
    /// SessionStateContract.swift. Sorted by the server, active-first (DECODE,
    /// then PREFILL/LOADING, then PARKED, then the rest; within a phase
    /// priority descending, then elapsedMs descending); this menu renders that
    /// order as given, never re-sorting it. The PARKED rows ARE the wait list:
    /// their relative order is the pick order (tier rank descending - priority,
    /// default, background - then the order the scheduler will grant), so the
    /// first PARKED row is the next request a slot takes. `queueSummary` below
    /// restates that order as one line (user ruling 2026-09-25: the summary
    /// line stays).
    public var sessionRows: [SessionRow] = []

    /// The current swap-grace cooldown (llama-cm llama-swap.yaml
    /// swapGraceSeconds; see fifo.go's grace/idleSince/withinGrace): the
    /// resident model is idle inside its grace while queued requests for
    /// another model wait for it. ONE state on the resident, never one per
    /// waiting model (2026-09-10: two "waiting for cq35" rows). Pushed by the
    /// "swapGrace" SSE event/GET /api/swap-grace (BackendClient.handleEvent);
    /// nil when nothing is held.
    public var cooldown: CooldownRow? = nil

    /// The memory brake's state from the session-state contract's
    /// `memoryBrake`, copied by BackendClient.applySessionsSnapshot.
    public var memoryBrake: MemoryBrakeInfo? = nil

    /// The brake's admission-gate line, nil while the gate is open. After a
    /// brake kill the killed model's weights linger as file cache (Event 8,
    /// 2026-09-19: 36 GB with nothing loaded), so loads are held until it
    /// drains; a request parked behind it shows as loading, and this line
    /// says why it does not move.
    public static func memoryBrakeLabel(_ mb: MemoryBrakeInfo?) -> String? {
        guard let mb, mb.holding else { return nil }
        guard let below = mb.drainBelowGB, let now = mb.fileBackedGB else {
            return "Memory brake: loads held"
        }
        let level = below == below.rounded() ? "\(Int(below))" : String(format: "%.1f", below)
        return "Memory brake: loads held until file-backed < \(level) GB (now \(String(format: "%.1f", now)) GB)"
    }

    /// The resident's own row while it cools down - the cooldown is a state
    /// of the loaded model, so it is rendered ON that model's line, not as a
    /// separate section: "cooldown 9:41 for [17426df4], then cq35 · 5 waiting"
    /// - remaining time plus the existing owner/next/waiting suffixes only,
    /// never a "(restarted x:xx ago)" note. Never "X waiting for <loaded
    /// model>".
    ///
    /// A no-waiter cooldown (2026-09-18: the resident is idle inside its own
    /// grace with nothing cross-model queued behind it - CooldownRow.nextModel
    /// empty) omits the ", then <next> · N waiting" suffix entirely: there is
    /// no swap pending, so naming one and counting zero waiters would read as
    /// a real wait that isn't happening.
    public static func cooldownLabel(_ cd: CooldownRow, next: String) -> String {
        var s = "cooldown \(CompactFormatter.countdown(cd.remainingSeconds))"
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

    /// One hot-slot line under the cooldown row: which session the slot is
    /// being kept warm for (the cooldown's whole purpose - a session's slot
    /// and KV cache survive a tool call or an AskUserQuestion pause), shown
    /// like an active slot would be. Session ids are shown by their first 8
    /// characters, the same short form the session rows use.
    public static func hotSlotLabel(_ s: HotSlotRow) -> String {
        "  slot \(s.slot) · [\(String(s.sessionId.prefix(8)))] · hot, idle \(CompactFormatter.countdown(s.idleSeconds))"
    }

    /// The "Queue: idle" line the design calls for when nothing is parked,
    /// else one summary per queued entry - built straight from the contract's
    /// own `sessions[]` (PARKED rows), the single source `sessionRows`
    /// already comes from. There is no second "scheduler queue" list to keep
    /// in sync with it: llama-swap's PARKED entries ARE the wait list, in the
    /// order it already sorted them (priority descending, then elapsedMs
    /// descending within the PARKED phase - session-state-contract.md
    /// § "sessions"). Restored by
    /// user ruling 2026-09-25 after 40db173 removed it; do not remove again
    /// without the user.
    public static func queueSummary(_ rows: [SessionRow]) -> String {
        let parked = rows.filter { $0.phase == "PARKED" }
        guard !parked.isEmpty else { return "Queue: idle" }
        return parked.enumerated()
            .map { i, row in "\(i + 1). \(row.tier)/\(row.alias.isEmpty ? row.model : row.alias)" }
            .joined(separator: ", ")
    }

    /// Derived, never stored: storing it meant recomputing at three call sites
    /// with inputs that drifted apart.
    public var activeModelID: String? {
        MenuState.activeModel(in: models, preferring: chosenModelID)
    }

    /// The waiting-count row text: a per-tier breakdown ("priority 2, default
    /// 1 waiting") when more than one tier actually HAS a waiter, otherwise
    /// the plain "N waiting" string unchanged from before tiers existed.
    ///
    /// `waitingByTier` is the contract's `queue.byTier` verbatim, which lists
    /// "every configured tier present, zero included" (session-state-
    /// contract.md) - so with three configured tiers and one waiter it always
    /// has 3 entries, not 1. The breakdown is only useful once more than one
    /// of them is actually nonzero; filtering here is display choice, not a
    /// second count (the numbers themselves are still the contract's own).
    public var waitingSummary: String {
        let nonZero = waitingByTier.filter { $0.value > 0 }
        if nonZero.count > 1 {
            let parts = nonZero.keys.sorted().map { "\($0) \(nonZero[$0] ?? 0)" }
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
        case sessionRows, cooldown
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

/// One row of the session-state contract's `sessions[]`
/// (docs/intent/session-state-contract.md, llama-cm), rendered as-is: every
/// field below is copied from the wire body by `SessionRow.init(contract:)`
/// (SessionStateContract.swift) and `displayLine` only formats them (k-units,
/// a percent sign, "N.n t/s") - it never derives a number the body does
/// not already contain (invariant 3).
public struct SessionRow: Identifiable, Encodable, Equatable {
    public let id: String
    /// The FULL session id (`ContractSession.sessionId`), verbatim -
    /// distinct from `id`, which is `requestId ?? sessionId ?? sessionShort`
    /// and so is the REQUEST id whenever one is present (the common case for
    /// a PENALIZED session: the server publishes PENALIZED both with no
    /// request and with one PARKED under park reason "penalized", because
    /// the client keeps retrying). `unpenalize` needs the session id
    /// specifically - the endpoint is keyed on the session, not the request
    /// - so this field exists precisely so that click never sends `id` by
    /// mistake. Defaults to "" for the memberwise init's existing callers
    /// that predate this field; empty means "unknown, do not act on it".
    public let sessionId: String
    public let sessionShort: String
    /// The dispatching parent's short id (8 hex), set when the row represents
    /// a dispatched child. nil for direct CLI, curl, or non-dispatched rows.
    public let parentSessionShort: String?
    public let model: String
    public let alias: String
    public let tier: String
    /// The tier's configured rank; default tier 0, background negative.
    /// Reserved by the contract for a future per-session override - this
    /// menu only carries it.
    public let priority: Int
    /// `PARKED`, `LOADING`, `PREFILL`, `DECODE`, `HOT`, `IDLE`, or any value
    /// the server has not documented yet - rendered verbatim either way
    /// (invariant 5), never mapped through a Swift enum that could reject it.
    public let phase: String
    /// The contract's optional `display.phase` (raw `phase` is kept in
    /// `phase`; machine logic reads that). nil when the server sent no
    /// `display` object or no `display.phase` - the renderer then falls back
    /// to the raw phase. Never mutated by anything but the row's init.
    public let displayPhase: String?
    /// The retained readout's `context.used` (`display.contextUsed`) - the
    /// last raw DECODE's number, rendered in place of `context.used` while
    /// the retention holds. nil on rows without a `display` readout.
    public let displayContextUsed: Int?
    /// The retained readout's rate (`display.rate`) - the last raw DECODE's
    /// rate, rendered in place of `rate` while the retention holds. nil when
    /// the server sent none (including servers that predate the readout).
    public let displayRate: ContractRate?
    /// Set only while `phase == "PARKED"`. Same verbatim rule as `phase`.
    public let parkReason: String?
    public let context: ContractContext
    /// PREFILL only, 0...1; nil otherwise.
    public let progress: Double?
    public let rate: ContractRate
    /// Present only for a PENALIZED row's hold detail (SessionStateContract's
    /// `PenaltyInfo`); nil for every other phase, and nil for a PENALIZED row
    /// from a server that predates the penalty box (invariant 5 fallback:
    /// see `displayLine`).
    public let penalty: PenaltyInfo?
    /// The server-rendered row text (`ContractSession.line`); nil on a server
    /// that predates it. `displayLine` prints it verbatim.
    public let line: String?

    public init(id: String, sessionId: String = "", sessionShort: String, parentSessionShort: String? = nil, model: String, alias: String, tier: String,
                priority: Int, phase: String, displayPhase: String? = nil, displayContextUsed: Int? = nil, displayRate: ContractRate? = nil,
                parkReason: String? = nil,
                context: ContractContext, progress: Double? = nil, rate: ContractRate,
                penalty: PenaltyInfo? = nil, line: String? = nil) {
        self.id = id
        self.sessionId = sessionId
        self.sessionShort = sessionShort
        self.parentSessionShort = parentSessionShort
        self.model = model
        self.alias = alias
        self.tier = tier
        self.priority = priority
        self.phase = phase
        self.displayPhase = displayPhase
        self.displayContextUsed = displayContextUsed
        self.displayRate = displayRate
        self.parkReason = parkReason
        self.context = context
        self.progress = progress
        self.rate = rate
        self.penalty = penalty
        self.line = line
    }

    /// The row as the menu shows it: the server's `line`, VERBATIM. The
    /// server renders each row's text once (fork `rowLine`) and every menu -
    /// this dropdown and cm-menu - prints it, so no client formats a row.
    /// A server that predates `line` gets only the minimal
    /// `[<bracket>] <alias> · <raw phase>`, never a client-side reconstruction.
    public var displayLine: String {
        if let line { return line }
        let bracket: String
        if let parentSessionShort, !parentSessionShort.isEmpty {
            bracket = "\(parentSessionShort) > \(sessionShort.isEmpty ? "-" : sessionShort)"
        } else {
            bracket = sessionShort.isEmpty ? "-" : sessionShort
        }
        return "[\(bracket)] \(alias.isEmpty ? model : alias) · \(phase)"
    }

    /// The loop detector's evidence behind a PENALIZED hold, shown only in
    /// the menu item's tooltip (not the row text itself) - nil whenever
    /// `penalty` is absent.
    public var penaltyTooltip: String? {
        guard let penalty else { return nil }
        return "\(penalty.uniformRun) requests in a row, ~\(penalty.typicalTokens) tokens each"
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

/// The "swapGrace" SSE event's payload / GET /api/swap-grace's body -
/// internal/server/apigroup.go handleAPISwapGrace and the SSE sibling event
/// in handleAPIEvents both wrap the single cooldown (or null) in a
/// "cooldown" key.
struct CooldownPayload: Codable {
    let cooldown: CooldownRow?
}
