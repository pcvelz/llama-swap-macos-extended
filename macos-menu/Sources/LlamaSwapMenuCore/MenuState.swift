import Foundation

public struct MenuState: Encodable {
    public var backendOnline = false
    public var completed = 0
    public var waiting = 0
    /// Per-tier waiting breakdown (docs/intent/llama-swap-tiers.md, llama-cm).
    /// Populated only when the backend's inflight event carried more than one
    /// tier; empty otherwise, so a single-listener backend renders exactly as
    /// before tiers existed.
    public var waitingByTier: [String: Int] = [:]
    /// When each waiting count ("" = total) last peaked, for the anti-flap hold.
    private var heldSince: [String: Date] = [:]

    /// Mirrors the config's swapGraceSeconds (600) — anti-flap display hold;
    /// slot stability: llama-cm docs/intent/llama-swap-backend.md § Slot stability.
    static let waitingHold: TimeInterval = 600
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

    /// Applies an inflight event through the anti-flap hold: rises show
    /// immediately (and refresh the peak timestamp); drops only apply once the
    /// count has not re-peaked for waitingHold. No flapping — slot stability.
    public mutating func applyInflight(total: Int, byTier: [String: Int], now: Date = Date()) {
        waiting = holdWaiting(key: "", held: waiting, raw: total, now: now)
        var merged = byTier
        for key in waitingByTier.keys where merged[key] == nil { merged[key] = 0 }
        guard !merged.isEmpty else { return }
        var held: [String: Int] = [:]
        for (key, raw) in merged {
            held[key] = holdWaiting(key: key, held: waitingByTier[key] ?? 0, raw: raw, now: now)
        }
        waitingByTier = held
    }

    private mutating func holdWaiting(key: String, held: Int, raw: Int, now: Date) -> Int {
        if raw >= held || now.timeIntervalSince(heldSince[key] ?? .distantPast) > MenuState.waitingHold {
            heldSince[key] = now
            return raw
        }
        return held
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

struct ActivityLogEntry: Codable {}

struct EventEnvelope: Codable {
    let type: String
    let data: String
}

struct InFlightStats: Codable {
    let total: Int
    let byTier: [String: Int]?
}
