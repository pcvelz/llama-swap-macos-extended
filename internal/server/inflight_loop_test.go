package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// track adds one request with the given method/path and session id, gives it
// respTokens of real output, and completes it - the shape Remove() feeds to
// the loop tracker (2026-09-19).
func track(t *testing.T, tracker *inflightTracker, method, path, sessionID string, respTokens int64) {
	t.Helper()
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	id := tracker.Add(httptest.NewRequest(method, path, nil), cancel)
	if sessionID != "" {
		tracker.SetMetadata(id, "session_id", sessionID)
	}
	tracker.mu.Lock()
	tracker.requests[id].entry.RespTokens = respTokens
	tracker.mu.Unlock()
	tracker.Remove(id)
}

// A run of near-identical /v1/messages turns on one session is a loop.
func TestInflightTracker_RecordsLoopOnMessagesCompletions(t *testing.T) {
	tracker := newInflightTracker()
	for i := 0; i < 25; i++ {
		track(t, tracker, http.MethodPost, "/v1/messages", "934159af", 48)
	}
	if !tracker.loops.Looping("934159af") {
		t.Fatalf("25 uniform /v1/messages turns must read as looping (run=%d)", tracker.loops.Run("934159af"))
	}
}

// Everything that is not a real turn must stay out of the history: a run of
// their zero token counts would fake a perfect loop on a healthy session.
func TestInflightTracker_SkipsNonTurnCompletions(t *testing.T) {
	tracker := newInflightTracker()
	for i := 0; i < 25; i++ {
		track(t, tracker, http.MethodGet, "/slots", "poller", 0)                     // status read
		track(t, tracker, http.MethodPost, "/v1/messages/count_tokens", "poller", 0) // tokenize-only
		track(t, tracker, http.MethodPost, "/v1/messages", "", 48)                   // no session id
	}
	if tracker.loops.Run("poller") != 0 {
		t.Fatalf("status reads / count_tokens must not be recorded (run=%d)", tracker.loops.Run("poller"))
	}
	if tracker.loops.Looping("") {
		t.Fatalf("a request with no session id must never produce a verdict")
	}
}

// A request that produced NO output says nothing about what the session
// generates. The session that gets starved is the one that parks, times out
// and retries - a run of identical zero-token 499s - so counting those would
// make the victim read as the looper (review finding, 2026-09-19).
func TestInflightTracker_ZeroTokenCompletionsAreNotRecorded(t *testing.T) {
	tracker := newInflightTracker()
	for i := 0; i < 25; i++ {
		track(t, tracker, http.MethodPost, "/v1/messages", "starved", 0)
	}
	if got := tracker.loops.Run("starved"); got != 0 {
		t.Fatalf("Run=%d want 0: zero-token completions must not be recorded", got)
	}
	if tracker.loops.Looping("starved") {
		t.Fatalf("a starved, zero-output session must never read as looping")
	}
}

// Zeros interleaved in a real run are simply absent from the history: they
// neither extend it nor break it.
func TestInflightTracker_ZeroTokensDoNotDisturbARun(t *testing.T) {
	tracker := newInflightTracker()
	for i := 0; i < 19; i++ {
		track(t, tracker, http.MethodPost, "/v1/messages", "mixed", 48)
		track(t, tracker, http.MethodPost, "/v1/messages", "mixed", 0) // a timed-out retry
	}
	if got := tracker.loops.Run("mixed"); got != 19 {
		t.Fatalf("Run=%d want 19: the zeros must neither extend nor break the run", got)
	}
	if tracker.loops.Looping("mixed") {
		t.Fatalf("19 real responses must still be below the bar of 20")
	}
	track(t, tracker, http.MethodPost, "/v1/messages", "mixed", 47)
	if !tracker.loops.Looping("mixed") {
		t.Fatalf("the 20th real response must reach the bar")
	}
}

// loopGuard.enabled:false must be a true off switch: nothing recorded, every
// verdict false, i.e. byte-for-byte the pre-2026-09-19 behaviour.
func TestInflightTracker_LoopGuardDisabled(t *testing.T) {
	tracker := newInflightTracker()
	tracker.setLoopGuard(config.LoopGuardConfig{Enabled: false, RunBar: 20, ToleranceTokens: 2})
	for i := 0; i < 50; i++ {
		track(t, tracker, http.MethodPost, "/v1/messages", "934159af", 48)
	}
	if tracker.loops.Looping("934159af") || tracker.loops.Run("934159af") != 0 {
		t.Fatalf("a disabled loop guard must never report a loop")
	}
}

// The configured thresholds are the ones that count, not the package
// defaults: an operator lowering runBar must see the verdict sooner.
func TestInflightTracker_LoopGuardHonoursConfiguredThresholds(t *testing.T) {
	tracker := newInflightTracker()
	tracker.setLoopGuard(config.LoopGuardConfig{Enabled: true, RunBar: 5, ToleranceTokens: 0})
	for i := 0; i < 5; i++ {
		track(t, tracker, http.MethodPost, "/v1/messages", "tight", 48)
	}
	if !tracker.loops.Looping("tight") {
		t.Fatalf("runBar 5 must be reached after 5 identical responses")
	}
	// tolerance 0: one token of variation breaks the run.
	track(t, tracker, http.MethodPost, "/v1/messages", "tight", 49)
	if tracker.loops.Looping("tight") {
		t.Fatalf("toleranceTokens 0 must not tolerate a 1-token spread")
	}
}

// The upstream middleware strips /upstream/<model>, so the recorded path is
// the bare /v1/messages - both front doors must count the same turns.
func TestInflightTracker_LoopVerdictThroughContext(t *testing.T) {
	tracker := newInflightTracker()
	for i := 0; i < 25; i++ {
		track(t, tracker, http.MethodPost, "/v1/messages", "looper", 48)
	}

	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	id := tracker.Add(httptest.NewRequest(http.MethodPost, "/v1/messages", nil), cancel)
	verdict := tracker.loopVerdictFor(id)
	if verdict() {
		t.Fatalf("a request with no session id stamped yet must read as productive")
	}
	// The session id is stamped after Add() in production; the verdict reads
	// it lazily, so it must flip without rebinding.
	tracker.SetMetadata(id, "session_id", "looper")
	if !verdict() {
		t.Fatalf("verdict must see the session id stamped after Add()")
	}
	tracker.Remove(id)
	if verdict() {
		t.Fatalf("a finished request has no live entry, so no session and no verdict")
	}
}

// CheckYieldFloor scans in-flight requests and cancels glacial ones.
func TestInflightTracker_YieldFloorCanclesGlacial(t *testing.T) {
	tracker := newInflightTracker()
	tracker.setLoopGuard(config.LoopGuardConfig{
		Enabled:             true,
		RunBar:              999, // disable loop detection
		YieldFloorTokPerMin: 10,
		YieldFloorMinutes:   2, // short window for the test
	})

	// Add an in-flight request with a session id.
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	id := tracker.Add(req, cancel)
	tracker.SetMetadata(id, "session_id", "yield-test")

	// Simulate a glacial request that has been running for 30 minutes with
	// only 2 tokens = 0.067 tok/min — far below the 10 tok/min floor.
	// We fake this by backdating the entry's Timestamp and capping RespTokens.
	tracker.mu.Lock()
	tracker.requests[id].entry.Timestamp = time.Now().Add(-30 * time.Minute)
	tracker.requests[id].entry.RespTokens = 2
	tracker.mu.Unlock()

	cancelled, events := tracker.CheckYieldFloor()
	if len(cancelled) != 1 {
		t.Fatalf("cancelled=%v want [id], got %d events", cancelled, len(events))
	}
	if len(events) != 1 || events[0].Reason != swaputil.PenaltyReasonYield {
		t.Fatalf("events[0].Reason=%q want %q", events[0].Reason, swaputil.PenaltyReasonYield)
	}

	// A healthy request should NOT be cancelled.
	_, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	id2 := tracker.Add(req2, cancel2)
	tracker.SetMetadata(id2, "session_id", "healthy-test")
	tracker.mu.Lock()
	tracker.requests[id2].entry.RespTokens = 300 // healthy rate
	tracker.mu.Unlock()

	cancelled2, _ := tracker.CheckYieldFloor()
	if len(cancelled2) != 0 {
		t.Fatalf("healthy request should not be cancelled, got %v", cancelled2)
	}
}

// CheckYieldFloor with zero tokens (prefill) does not cancel.
func TestInflightTracker_YieldFloorSkipsPrefill(t *testing.T) {
	tracker := newInflightTracker()
	tracker.setLoopGuard(config.LoopGuardConfig{
		Enabled:             true,
		RunBar:              999,
		YieldFloorTokPerMin: 10,
		YieldFloorMinutes:   2,
	})

	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	id := tracker.Add(req, cancel)
	tracker.SetMetadata(id, "session_id", "prefill-test")
	// Zero tokens: still in prefill.
	tracker.mu.Lock()
	tracker.requests[id].entry.RespTokens = 0
	tracker.mu.Unlock()

	cancelled, _ := tracker.CheckYieldFloor()
	if len(cancelled) != 0 {
		t.Fatalf("prefill (zero tokens) should not be cancelled, got %v", cancelled)
	}
}

// 2026-10-19 (W fea5693f, G 620c7b25): a request that waited parked and then
// prefilled for more than the yield window was cancelled the instant its FIRST
// token streamed, because yield was measured from arrival. Waiting is not low
// yield: the window starts at the first real output token.
func TestInflightTracker_YieldFloorIgnoresWaitBeforeFirstToken(t *testing.T) {
	tracker := newInflightTracker()
	tracker.setLoopGuard(config.LoopGuardConfig{
		Enabled:             true,
		RunBar:              999,
		YieldFloorTokPerMin: 10,
		YieldFloorMinutes:   15, // the live value
	})

	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	id := tracker.Add(req, cancel)
	tracker.SetMetadata(id, "session_id", "fea5693f-dd88-47d1-a37e-d5e2e2ed0d61")
	// Arrived 17 minutes ago (8 parked + 9 cold prefill, as witnessed).
	tracker.mu.Lock()
	tracker.requests[id].entry.Timestamp = time.Now().Add(-17 * time.Minute)
	tracker.mu.Unlock()

	// The first real output token arrives now.
	tracker.AddResponseBytes(id, []byte("event: content_block_delta\n"+
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"I\"}}\n\n"))

	cancelled, events := tracker.CheckYieldFloor()
	if len(cancelled) != 0 || len(events) != 0 {
		t.Fatalf("a request that just produced its first token after a long wait must not be struck; cancelled=%v events=%d", cancelled, len(events))
	}

	// A request that has been DECODING glacially for longer than the window is
	// still struck: the floor keeps its job, only the clock start moved.
	_, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	id2 := tracker.Add(httptest.NewRequest(http.MethodPost, "/v1/messages", nil), cancel2)
	tracker.SetMetadata(id2, "session_id", "glacial-decoder")
	tracker.AddResponseBytes(id2, []byte("event: content_block_delta\n"+
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"a\"}}\n\n"))
	tracker.mu.Lock()
	tracker.requests[id2].entry.Timestamp = time.Now().Add(-40 * time.Minute)
	tracker.requests[id2].firstOutputAt = time.Now().Add(-20 * time.Minute)
	tracker.mu.Unlock()
	cancelled2, _ := tracker.CheckYieldFloor()
	if len(cancelled2) != 1 || cancelled2[0] != id2 {
		t.Fatalf("a request decoding 1 token in 20 min must be struck; cancelled=%v", cancelled2)
	}
}
