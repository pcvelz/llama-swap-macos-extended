import Foundation

/// Holds a session row's word steady across the sub-second and few-second
/// boundaries of a healthy tool loop, so the menu shows what a lane is DOING
/// rather than every instant of it.
///
/// WHY (user report 2026-09-16 20:40, "an arcade machine flip-flopping"): a
/// Claude Code tool loop issues one request per turn; each starts with a
/// prefill under a second, decodes for 2-20 s, then leaves a 1-3 s tool gap.
/// BackendClient classifies every one of those instants correctly, and the
/// row therefore changed word every 5-10 s (PREFILL -> DECODE -> TURN ->
/// PREFILL ...) and dropped its rate at each new request, while the model ran
/// without a hiccup. Status consumers re-alert on every flip.
///
/// Rule: among the three ACTIVE words (PREFILL, DECODE, TURN) a row shows a
/// new word only after it has persisted for `holdSeconds`; until then it keeps
/// the word it was showing. The rate is held the same way: a raw readout
/// without "t/s" does not replace a held readout that had one while the hold
/// runs. Every other word (PARKED, FLAT, ...) is shown at once and resets the
/// hold - those are state changes a user must see, and FLAT already carries
/// its own 60 s window (SessionThroughput.flatWindowSeconds).
///
/// Rows are keyed by lane identity as the user reads it (origin + agent), not
/// by request id, which changes at every turn.
public final class RowWordStabilizer {
    public let holdSeconds: TimeInterval

    private static let activeWords: Set<String> = [
        ThroughputWord.prefill.rawValue, ThroughputWord.decode.rawValue, ThroughputWord.turn.rawValue,
    ]

    private struct Shown {
        var word: String
        var detail: String?
        /// When `detail` last carried a rate; bounds how long it may stand in
        /// for a raw readout without one.
        var rateAt: Date?
    }
    private var shown: [String: Shown] = [:]
    /// A different active word waiting out the hold, and since when.
    private var pending: [String: (word: String, since: Date)] = [:]

    /// True while some row is waiting out a hold. The caller must re-apply
    /// after the hold even if no new data arrives: a lane that went idle
    /// produces no further events, so its TURN would otherwise never show.
    public var hasPending: Bool { !pending.isEmpty }

    public init(holdSeconds: TimeInterval) {
        self.holdSeconds = holdSeconds
    }

    public func apply(_ rows: [SessionRow], now: Date) -> [SessionRow] {
        // A hold of zero is "show every instant": nothing to remember.
        guard holdSeconds > 0 else {
            shown.removeAll()
            pending.removeAll()
            return rows
        }
        var seen = Set<String>()
        let out = rows.map { row -> SessionRow in
            let key = Self.key(for: row)
            seen.insert(key)
            let raw = row.word
            let hasRate = row.detail?.contains("t/s") == true

            // @user-gated: the hold below is a user-requested visual staleness
            // (2026-09-16); an agent may not shorten, bypass or remove it.
            guard let prev = shown[key],
                  Self.activeWords.contains(raw), Self.activeWords.contains(prev.word) else {
                // First sight, or a non-active word on either side: no hold.
                shown[key] = Shown(word: raw, detail: row.detail, rateAt: hasRate ? now : nil)
                pending[key] = nil
                return row
            }

            var word = prev.word
            if raw == prev.word {
                pending[key] = nil
            } else if let p = pending[key], p.word == raw {
                if now.timeIntervalSince(p.since) >= holdSeconds {
                    word = raw
                    pending[key] = nil
                }
            } else {
                pending[key] = (raw, now)
            }

            var detail = row.detail
            var rateAt = hasRate ? now : prev.rateAt
            if !hasRate, word == prev.word, let at = prev.rateAt,
               now.timeIntervalSince(at) <= holdSeconds {
                // Keep the steady readout while the hold runs; a new request's
                // bare total (no sample yet) is not news.
                detail = prev.detail
            } else if !hasRate {
                rateAt = nil
            }
            shown[key] = Shown(word: word, detail: detail, rateAt: rateAt)
            guard word != raw || detail != row.detail else { return row }
            return SessionRow(id: row.id, origin: row.origin, model: row.model, tier: row.tier,
                              word: word, detail: detail, hasSession: row.hasSession,
                              title: row.title, parent: row.parent, agent: row.agent)
        }
        shown = shown.filter { seen.contains($0.key) }
        pending = pending.filter { seen.contains($0.key) }
        return out
    }

    static func key(for row: SessionRow) -> String {
        // A row without a session is its own lane (BackendClient.laneKey
        // "req:<id>"): two session-less curls share an origin label, and
        // keying them together would hand one row the other's word.
        guard row.hasSession else { return "req:" + row.id }
        return row.origin + ">" + (row.agent ?? "")
    }
}
