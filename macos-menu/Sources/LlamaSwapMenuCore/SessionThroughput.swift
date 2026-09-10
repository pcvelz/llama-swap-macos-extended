import Foundation

/// One polled sample of a single in-flight request's response-byte count,
/// used to tell DECODE apart from FLAT across two polls of the same request.
/// Mirrors the two-sample state file llama-cm's llama/lib/session-throughput.sh
/// keeps per session (`<resp_bytes> <sample_epoch> <first_byte_epoch>`); here
/// it lives in memory, keyed by request id, because the menu bar's poll loop
/// is one long-lived process rather than a daemon calling out per sample.
public struct ThroughputSample: Equatable {
    public var respBytes: Int64
    public var sampledAt: Date
    /// Pinned the first time respBytes > 0 is observed, then carried forward
    /// unchanged while the stream continues - anchors FLAT's "N seconds since
    /// first byte" clock, same as the bash reader's first_byte_epoch.
    public var firstByteAt: Date?
    /// The elapsed_ms this sample carried - the PREFILL-advance proxy (see
    /// classify's resp_bytes==0 branch). This reader has no access to the
    /// child llama-server's prefill-progress log the bash reader
    /// (session-throughput.sh's _stp_child_progress) consults, only the
    /// /api/events inflight entry, so a still-climbing elapsed_ms across two
    /// polls is the best available live signal that the request is still
    /// open and advancing rather than dead past the elapsed budget.
    public var elapsedMs: Int64

    public init(respBytes: Int64, sampledAt: Date, firstByteAt: Date?, elapsedMs: Int64 = 0) {
        self.respBytes = respBytes
        self.sampledAt = sampledAt
        self.firstByteAt = firstByteAt
        self.elapsedMs = elapsedMs
    }
}

/// The subset of llama-cm's session-throughput.sh KEYWORDS this reader can
/// derive from the live /api/events inflight entry. PARKED is derived from
/// metadata.slot_granted: a GRANTED (actively prefilling) in-flight request
/// carries `metadata.slot_granted == "1"`; a queued request has no
/// slot_granted key at all - see BackendClient.applyInflightEntries. NONE and
/// UNKNOWN are still NOT modeled here: a request only appears in this
/// reader's input at all while it is tracked in-flight, so NONE/UNKNOWN (no
/// data / session absent) do not apply.
public enum ThroughputWord: String {
    case prefill = "PREFILL"
    case decode = "DECODE"
    case flat = "FLAT"
    case parked = "PARKED"
    /// Between turns: the lane has NO request on the proxy right now because
    /// the client is running a tool locally (1-3s for an ordinary Bash/Read
    /// call). Not a backend state at all - it is the honest word for a row
    /// that BackendClient is holding on screen across a turn boundary rather
    /// than letting it vanish and re-appear. Never render DECODE/PREFILL for
    /// such a row: nothing is being computed for it.
    case turn = "TURN"
}

/// Classifies one polled sample of a single in-flight request using the SAME
/// vocabulary and thresholds as llama-cm's llama/lib/session-throughput.sh,
/// so the menu bar and cm-menu can never disagree about a request that is
/// actually alive. See that script's header for the full reasoning; the
/// mechanics are reproduced here rather than re-derived.
public enum SessionThroughput {
    /// Mirrors SESSION_THROUGHPUT_PREFILL_BUDGET_S's default (900s).
    public static let prefillBudgetSeconds: TimeInterval = 900
    /// Mirrors SESSION_THROUGHPUT_FLAT_S's default (60s).
    public static let flatWindowSeconds: TimeInterval = 60

    public static func classify(
        respBytes: Int64,
        elapsedMs: Int64,
        previous: ThroughputSample?,
        now: Date
    ) -> (word: ThroughputWord, sample: ThroughputSample) {
        var firstByteAt = previous?.firstByteAt
        if respBytes > 0 && firstByteAt == nil {
            firstByteAt = now
        }

        let word: ThroughputWord
        if respBytes == 0 {
            // PREFILL-advance proxy: no child-log progress signal is
            // available here (see ThroughputSample.elapsedMs), so a request
            // still tracked in-flight with elapsed_ms having grown since the
            // last poll is treated as advancing and reads PREFILL regardless
            // of the elapsed budget - mirrors the bash reader's "an
            // ADVANCING signature overrides the elapsed clock" rule, one
            // signal poorer. Only when elapsed_ms is unchanged (or there is
            // no previous sample yet) does the pure elapsed-budget rule
            // apply, same as before this fold.
            if let previous, previous.respBytes == 0, elapsedMs > previous.elapsedMs {
                word = .prefill
            } else {
                let elapsedSeconds = Double(elapsedMs) / 1000.0
                word = elapsedSeconds >= prefillBudgetSeconds ? .flat : .prefill
            }
        } else if let previous, respBytes < previous.respBytes {
            // Bytes went DOWN: not a stall, a NEW request reusing this
            // tracking key. Treated as a fresh stream (re-pin first byte).
            firstByteAt = now
            word = .decode
        } else if let previous, respBytes > previous.respBytes {
            word = .decode
        } else if let previous, respBytes == previous.respBytes,
                  let pinnedFirstByte = firstByteAt,
                  now.timeIntervalSince(pinnedFirstByte) >= flatWindowSeconds {
            word = .flat
        } else {
            // The one genuinely ambiguous case: bytes present, neither "rose"
            // nor "sustained no motion" is proven yet (no previous sample, or
            // still within the flat grace window). DECODE is the least wrong
            // of the three keywords - see session-throughput.sh's header.
            word = .decode
        }

        return (word, ThroughputSample(respBytes: respBytes, sampledAt: now, firstByteAt: firstByteAt, elapsedMs: elapsedMs))
    }
}

/// Keeps one ThroughputSample per in-flight request id across polls, so
/// BackendClient can call `word(forRequestID:...)` once per event without
/// hand-rolling the previous-sample bookkeeping itself. Request ids are
/// unique per request (llama-swap's inflightTracker.nextID), so a dropped
/// byte count can never happen mid-stream here the way it can when a bash
/// reader keys on a session id spanning several requests - kept anyway
/// (see SessionThroughput.classify) for parity with the shared vocabulary.
public final class SessionThroughputTracker {
    private var samples: [String: ThroughputSample] = [:]

    public init() {}

    public func word(forRequestID id: String, respBytes: Int64, elapsedMs: Int64, now: Date = Date()) -> ThroughputWord {
        let result = SessionThroughput.classify(
            respBytes: respBytes, elapsedMs: elapsedMs, previous: samples[id], now: now)
        samples[id] = result.sample
        return result.word
    }

    /// Drops samples for requests no longer in flight, so a long-lived menu
    /// bar process never accumulates one entry per request ever seen.
    public func prune(keeping ids: Set<String>) {
        samples = samples.filter { ids.contains($0.key) }
    }
}

/// Derives the origin label a session row shows: session_id first 8 chars
/// (mirrors cm-menu's short-id convention), else "hermes" when the User-Agent
/// identifies the Hermes Desktop client, else the User-Agent's first token,
/// else "unknown" - a row is never left blank.
public enum SessionOrigin {
    /// Preferred entry point: the proxy now classifies the caller itself
    /// (metadata.client - claude-code/hermes/curl/python-sdk/other), so a
    /// session-less row names a client FAMILY instead of whatever the
    /// User-Agent happened to start with. "other" carries no more information
    /// than the agent string does, so it defers to the agent path below,
    /// which also covers entries from a proxy predating the client key.
    public static func label(sessionID: String?, client: String?, userAgent: String?) -> String {
        if let sessionID, !sessionID.isEmpty {
            return String(sessionID.prefix(8))
        }
        if let client, !client.isEmpty, client != "other" {
            return client
        }
        return label(sessionID: nil, userAgent: userAgent)
    }

    public static func label(sessionID: String?, userAgent: String?) -> String {
        if let sessionID, !sessionID.isEmpty {
            return String(sessionID.prefix(8))
        }
        if let userAgent, userAgent.localizedCaseInsensitiveContains("hermes") {
            return "hermes"
        }
        if let userAgent {
            let trimmed = userAgent.trimmingCharacters(in: .whitespaces)
            if let firstToken = trimmed.split(separator: " ").first {
                return String(firstToken)
            }
        }
        return "unknown"
    }
}
