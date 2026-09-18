import Foundation

/// Decodable mirror of the session-state contract (llama-cm
/// docs/intent/session-state-contract.md, schema "llama-swap.sessions/v1").
///
/// This is the ONLY place session phase, context and rate are read from.
/// llama-swap computes them once from the child's `/slots` poll, its
/// scheduler and the slot-affinity store; every client (this menu, the
/// Claude Code status line, cm-menu) only renders what is here. No property
/// in this file is ever derived from another field client-side - each one is
/// copied straight out of the wire body.
///
/// Every type below relies on Swift's synthesized `Decodable`: a JSON key
/// with no matching property is silently ignored (invariant 5, "clients
/// ignore unknown fields"), and adding a field to the contract never breaks
/// an older client.
public struct SessionsSnapshot: Decodable {
    public let schema: String
    public let generatedAt: String
    public let resident: ResidentInfo?
    public let queue: QueueCounts
    /// Also carried by the dedicated "swapGrace" SSE event, which already
    /// drives `MenuState.cooldown` (including the restart-detection logic in
    /// BackendClient.handleEvent) - this menu keeps using that single path
    /// rather than a second writer of the same field, so decoding this
    /// property is enough; nothing here re-applies it.
    public let cooldown: CooldownRow?
    public let memoryBrake: MemoryBrakeInfo?
    public let sessions: [ContractSession]
}

public struct ResidentInfo: Decodable {
    public let model: String
    public let alias: String
    public let state: String
    public let window: Int
    public let slots: Int
}

/// `queue.waiting`/`queue.byTier` from the contract - the ONLY source for
/// `MenuState.waiting`/`waitingByTier`. There is deliberately no client-side
/// recount of PARKED rows here: the contract already guarantees invariant 2
/// (waiting == count(phase == PARKED)) server-side, so copying this struct
/// straight into MenuState satisfies the parity rule by construction rather
/// than by a second computation that could drift from the first.
public struct QueueCounts: Decodable, Equatable {
    public let waiting: Int
    public let byTier: [String: Int]
}

public struct MemoryBrakeInfo: Decodable {
    public let enabled: Bool
    public let holding: Bool
    public let remainingSeconds: Int
}

/// One `sessions[]` entry. `phase` and `parkReason` are plain `String`, not a
/// Swift enum: invariant 5 requires an unknown value to render verbatim
/// rather than fail to decode or fall back to a guess, which a `String`
/// naturally satisfies with no extra handling.
public struct ContractSession: Decodable {
    public let sessionId: String
    public let sessionShort: String
    public let requestId: String?
    public let model: String
    public let alias: String
    public let tier: String
    public let priority: Int
    public let phase: String
    public let parkReason: String?
    public let slot: Int?
    public let context: ContractContext
    public let progress: Double?
    public let rate: ContractRate
    public let elapsedMs: Int
    public let phaseSinceMs: Int
    public let respTokens: Int
}

public struct ContractContext: Codable, Equatable {
    public let used: Int
    public let cached: Int
    public let processed: Int
    public let decoded: Int
    public let promptTotal: Int
    public let window: Int
}

public struct ContractRate: Codable, Equatable {
    public let kind: String?
    public let tokensPerSecond: Double?
    public let windowSeconds: Double
}

extension SessionRow {
    /// The one place a `ContractSession` becomes the row the menu renders.
    /// Every displayed number (context, progress, rate, priority) is copied
    /// from `entry` untouched - see SessionRow.displayLine for the formatting
    /// (k-units, percent, "kind N.n t/s") that invariant 3 still allows.
    public init(contract entry: ContractSession) {
        // requestId identifies the in-flight request cancelInflight targets;
        // a HOT/IDLE row has none, so it falls back to the session (still
        // unique) and finally the short id, so `id` is never empty.
        let id = entry.requestId ?? (entry.sessionId.isEmpty ? entry.sessionShort : entry.sessionId)
        self.init(
            id: id,
            sessionShort: entry.sessionShort,
            model: entry.model,
            alias: entry.alias,
            tier: entry.tier,
            priority: entry.priority,
            phase: entry.phase,
            parkReason: entry.parkReason,
            context: entry.context,
            progress: entry.progress,
            rate: entry.rate)
    }
}
