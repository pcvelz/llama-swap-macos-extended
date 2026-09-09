import XCTest
@testable import LlamaSwapMenuCore

/// Pins the menu bar's per-request throughput classification to the SAME
/// vocabulary and thresholds as llama-cm's llama/lib/session-throughput.sh
/// (PREFILL_BUDGET_S=900, FLAT_S=60) so the menu bar and cm-menu can never
/// disagree about whether a request is alive. PARKED is not modeled here:
/// the live /api/events inflight entry never carries a slot_id (only the
/// POST-HOC completed activity entry does, per internal/server/metrics.go
/// mergeSlotID) - see SessionThroughput.swift's header for the full note.
final class SessionThroughputTests: XCTestCase {

    private let t0 = Date(timeIntervalSince1970: 1_800_000_000)

    // MARK: - zero bytes (PREFILL / FLAT-past-budget)

    func testZeroBytesWithinPrefillBudgetIsPrefill() {
        let result = SessionThroughput.classify(
            respBytes: 0, elapsedMs: 60_000, previous: nil, now: t0)
        XCTAssertEqual(result.word, .prefill)
    }

    func testZeroBytesPastPrefillBudgetIsFlat() {
        let result = SessionThroughput.classify(
            respBytes: 0, elapsedMs: 901_000, previous: nil, now: t0)
        XCTAssertEqual(result.word, .flat)
    }

    // MARK: - PREFILL-advance proxy (elapsed_ms climbing across polls)

    func testZeroBytesPastPrefillBudgetWithClimbingElapsedIsStillPrefill() {
        let prev = ThroughputSample(respBytes: 0, sampledAt: t0, firstByteAt: nil, elapsedMs: 900_000)
        let result = SessionThroughput.classify(
            respBytes: 0, elapsedMs: 930_000, previous: prev, now: t0.addingTimeInterval(30))
        XCTAssertEqual(result.word, .prefill, "elapsed_ms grew since the last poll - treated as an advancing prefill regardless of the elapsed budget")
    }

    func testZeroBytesPastPrefillBudgetWithStalledElapsedIsFlat() {
        let prev = ThroughputSample(respBytes: 0, sampledAt: t0, firstByteAt: nil, elapsedMs: 901_000)
        let result = SessionThroughput.classify(
            respBytes: 0, elapsedMs: 901_000, previous: prev, now: t0.addingTimeInterval(30))
        XCTAssertEqual(result.word, .flat, "elapsed_ms unchanged since last poll - no advance signal, falls back to the elapsed budget")
    }

    // MARK: - the ambiguous first-observation case

    func testFirstObservationWithBytesIsDecode() {
        let result = SessionThroughput.classify(
            respBytes: 500, elapsedMs: 2_000, previous: nil, now: t0)
        XCTAssertEqual(result.word, .decode)
        XCTAssertEqual(result.sample.firstByteAt, t0, "first byte epoch must be pinned on first sight")
    }

    // MARK: - rising bytes

    func testRisingBytesIsDecode() {
        let prev = ThroughputSample(respBytes: 500, sampledAt: t0, firstByteAt: t0)
        let result = SessionThroughput.classify(
            respBytes: 800, elapsedMs: 7_000, previous: prev, now: t0.addingTimeInterval(5))
        XCTAssertEqual(result.word, .decode)
        XCTAssertEqual(result.sample.firstByteAt, t0, "first byte epoch carries forward unchanged while the stream continues")
    }

    // MARK: - flat bytes

    func testFlatBytesWithinFlatWindowIsStillDecode() {
        let prev = ThroughputSample(respBytes: 500, sampledAt: t0, firstByteAt: t0)
        let result = SessionThroughput.classify(
            respBytes: 500, elapsedMs: 30_000, previous: prev, now: t0.addingTimeInterval(30))
        XCTAssertEqual(result.word, .decode, "no stall proven yet - DECODE is the least wrong verdict")
    }

    func testFlatBytesPastFlatWindowIsFlat() {
        let prev = ThroughputSample(respBytes: 500, sampledAt: t0, firstByteAt: t0)
        let result = SessionThroughput.classify(
            respBytes: 500, elapsedMs: 61_000, previous: prev, now: t0.addingTimeInterval(61))
        XCTAssertEqual(result.word, .flat)
    }

    // MARK: - a lower byte count means a NEW request reused the tracking key

    func testDroppedBytesRepinsFirstByteAndReadsDecode() {
        let prev = ThroughputSample(respBytes: 800, sampledAt: t0, firstByteAt: t0)
        let now = t0.addingTimeInterval(120)
        let result = SessionThroughput.classify(
            respBytes: 100, elapsedMs: 1_000, previous: prev, now: now)
        XCTAssertEqual(result.word, .decode)
        XCTAssertEqual(result.sample.firstByteAt, now, "a byte-count drop must re-pin first_byte rather than stay stale")
    }
}

/// Pins the origin label derivation: session_id first 8 chars, else "hermes"
/// when the User-Agent identifies as Hermes, else the User-Agent's first
/// token, else "unknown" - never leaves a row blank.
final class SessionOriginTests: XCTestCase {

    func testSessionIDIsTruncatedToEightChars() {
        let origin = SessionOrigin.label(
            sessionID: "3bf85c8c-1234-4321-aaaa-bbbbccccdddd", userAgent: "claude-code/1.0")
        XCTAssertEqual(origin, "3bf85c8c")
    }

    func testHermesUserAgentWinsOverGenericFallback() {
        let origin = SessionOrigin.label(sessionID: nil, userAgent: "Hermes Desktop/2.0 (macOS)")
        XCTAssertEqual(origin, "hermes")
    }

    func testGenericUserAgentUsesFirstToken() {
        let origin = SessionOrigin.label(
            sessionID: nil, userAgent: "llama-swap-menu (unknown version) CFNetwork/3896.100.1.1.1 Darwin/27.0.0")
        XCTAssertEqual(origin, "llama-swap-menu")
    }

    func testNoSignalReadsUnknown() {
        let origin = SessionOrigin.label(sessionID: nil, userAgent: nil)
        XCTAssertEqual(origin, "unknown")
    }
}
