package router

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/logmon"
)

// syncRecorder is a minimal concurrency-safe http.ResponseWriter for the
// pingWriter tests.
//
// WHY it exists: these tests deliberately let the ping goroutine write to the
// response while the test goroutine reads what it wrote. httptest.Recorder
// records into a plain bytes.Buffer with no locking, so every such read is a
// genuine data race — `go test -race` (what `make test-all` and BOTH CI lanes
// run) fails on it, even though `go test` without -race passes. That is exactly
// how it reached main: verified locally without -race, red on Linux and Windows.
//
// This is a harness fix, not a behaviour fix. Production is unaffected:
// pingWriter.mu serializes all access to the real ResponseWriter, and the only
// unsynchronized reader was the test itself.
type syncRecorder struct {
	// hdr is not lock-guarded because callers mutate the returned map directly,
	// exactly as with httptest.Recorder. Safe here: headers are set during test
	// setup, before pingQuietDelay can elapse and start the ping goroutine.
	hdr http.Header

	mu     sync.Mutex
	body   bytes.Buffer
	status int
}

func newSyncRecorder() *syncRecorder {
	return &syncRecorder{hdr: make(http.Header)}
}

func (r *syncRecorder) Header() http.Header { return r.hdr }

func (r *syncRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.Write(p)
}

func (r *syncRecorder) WriteHeader(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = code
}

// bodyString and bodyLen are the only sanctioned way for a test goroutine to
// observe the body: they take the same lock the ping goroutine writes under.
func (r *syncRecorder) bodyString() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.String()
}

func (r *syncRecorder) bodyLen() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.Len()
}

// TestPingWriter_PingsContinueAfterEarlyHeaders reproduces the 2026-07-22
// compact-stall finding (llama-cm incident
// 2026-07-09-compact-stall-cold-cache-swap-churn, follow-up 2026-07-22):
//
// llama-server (httplib) commits "200 + text/event-stream" response headers
// immediately at request accept — BEFORE task scheduling, prefill, or the
// first generated token. pingWriter treats that bare WriteHeader(200) as
// "upstream started" and permanently disarms, so a request that then sits
// silent for minutes (queued task, 163k prefill, decode starvation under a
// concurrent prefill) delivers ZERO bytes of body and is killed by the
// client's ~300s body timeout (witnessed as six metronomic 5m0.06s aborts).
//
// The pinger's contract is to keep byte-less waits alive; headers alone do
// not feed a client's body-timeout watchdog. Expected behaviour: ping events
// keep flowing during body silence that follows early upstream headers.
func TestPingWriter_PingsContinueAfterEarlyHeaders(t *testing.T) {
	if testing.Short() {
		t.Skip("waits >pingQuietDelay of real time")
	}
	logger := logmon.NewWriter(io.Discard)
	w := newSyncRecorder()

	// Shorten the cadence so the test does not sleep 37s of real time;
	// production defaults (20s/15s) are restored on cleanup.
	origQuiet, origInterval := pingQuietDelay, pingInterval
	pingQuietDelay, pingInterval = 100*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { pingQuietDelay, pingInterval = origQuiet, origInterval })

	pw := newPingWriter(logger, "test-model", w, true, nil)
	defer pw.stop()

	// llama-server's httplib sends SSE headers immediately at accept.
	pw.Header().Set("Content-Type", "text/event-stream")
	pw.WriteHeader(http.StatusOK)

	// Then the body goes silent (task queued / prefilling / decode-starved).
	// Wait past pingQuietDelay plus one pingInterval of margin.
	time.Sleep(pingQuietDelay + pingInterval + 2*time.Second)

	body := w.bodyString()
	if !strings.Contains(body, `"type": "ping"`) && !strings.Contains(body, `"type":"ping"`) {
		t.Errorf("expected ping events during post-header body silence, got %d bytes of body: %q", len(body), body)
	}
}

// TestPingWriter_PingsResumeDuringMidStreamSilence: same contract one level
// deeper — after a first real body byte (e.g. llama-server's initial chunk),
// a long silence gap must also be bridged by pings, because the client's
// body timeout resets per byte and a single early byte followed by minutes
// of silence still kills the stream.
func TestPingWriter_PingsResumeDuringMidStreamSilence(t *testing.T) {
	if testing.Short() {
		t.Skip("waits >pingQuietDelay of real time")
	}
	logger := logmon.NewWriter(io.Discard)
	w := newSyncRecorder()

	// Shorten the cadence so the test does not sleep 37s of real time;
	// production defaults (20s/15s) are restored on cleanup.
	origQuiet, origInterval := pingQuietDelay, pingInterval
	pingQuietDelay, pingInterval = 100*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { pingQuietDelay, pingInterval = origQuiet, origInterval })

	pw := newPingWriter(logger, "test-model", w, true, nil)
	defer pw.stop()

	pw.Header().Set("Content-Type", "text/event-stream")
	pw.WriteHeader(http.StatusOK)
	if _, err := pw.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	before := w.bodyLen()

	time.Sleep(pingQuietDelay + pingInterval + 2*time.Second)

	body := w.bodyString()[before:]
	if !strings.Contains(body, `"type": "ping"`) && !strings.Contains(body, `"type":"ping"`) {
		t.Errorf("expected ping events during mid-stream silence, got %d new bytes: %q", len(body), body)
	}
}

// TestPingWriter_ZeroBudgetEndsStreamReadably covers the hole armParkGiveUp
// leaves open (llama-cm task #11): that give-up returns early when
// responseCommitted is true, and the FIRST PING IS THE COMMIT. So a request
// the pinger has taken over never gets a park-stage give-up, and before this
// budget existed it was held open indefinitely on content-free pings.
//
// The contract: a stream with ZERO upstream body bytes must END with a body
// the client can read, not stay open forever and not return bare. A bare
// return is the `200 0` / `502 0` signature from llama-cm incident
// 2026-08-18-cq27-background-admission-park-bare-502-retry-storm, which
// Claude Code 0s-retried into a self-sustaining storm.
func TestPingWriter_ZeroBudgetEndsStreamReadably(t *testing.T) {
	if testing.Short() {
		t.Skip("waits >pingQuietDelay of real time")
	}
	logger := logmon.NewWriter(io.Discard)
	w := newSyncRecorder()

	origQuiet, origInterval, origBudget := pingQuietDelay, pingInterval, pingZeroBudget
	pingQuietDelay, pingInterval = 50*time.Millisecond, 50*time.Millisecond
	pingZeroBudget = 400 * time.Millisecond
	t.Cleanup(func() {
		pingQuietDelay, pingInterval, pingZeroBudget = origQuiet, origInterval, origBudget
	})

	pw := newPingWriter(logger, "test-model", w, true, nil)
	defer pw.stop()
	// The budget is scoped to a request that HOLDS A SLOT (see slotGranted);
	// a still-parked request is held, not ended.
	pw.slotGranted()

	// Never write a body byte: this is the granted-but-produces-nothing case.
	time.Sleep(pingZeroBudget + 6*pingInterval)

	body := w.bodyString()
	if !strings.Contains(body, `"type":"error"`) {
		t.Errorf("zero-output stream did not end with a readable SSE error event; body=%q", body)
	}
	// overloaded_error is the one Anthropic error type that means "this was
	// never started, retrying is safe" - the client keys on it to back off
	// rather than fail the turn.
	if !strings.Contains(body, "overloaded_error") {
		t.Errorf("error event should carry overloaded_error so the client knows a retry is safe; body=%q", body)
	}
}

// TestPingWriter_ZeroBudgetDisarmedByOneByte is the @user-gated half: ZERO is
// the bar. One body byte means the stream is working, however slowly, and the
// budget must never fire again. A slow session is a working session - the user
// has accepted 40-minute turns - so this must not become a rate threshold.
func TestPingWriter_ZeroBudgetDisarmedByOneByte(t *testing.T) {
	if testing.Short() {
		t.Skip("waits >pingQuietDelay of real time")
	}
	logger := logmon.NewWriter(io.Discard)
	w := newSyncRecorder()

	origQuiet, origInterval, origBudget := pingQuietDelay, pingInterval, pingZeroBudget
	pingQuietDelay, pingInterval = 50*time.Millisecond, 50*time.Millisecond
	pingZeroBudget = 300 * time.Millisecond
	t.Cleanup(func() {
		pingQuietDelay, pingInterval, pingZeroBudget = origQuiet, origInterval, origBudget
	})

	pw := newPingWriter(logger, "test-model", w, true, nil)
	defer pw.stop()
	pw.slotGranted()

	// One real body byte, closing an SSE event, then silence far beyond the
	// budget. This is a slow turn, not a stuck one.
	_, _ = pw.Write([]byte("data: {}\n\n"))
	time.Sleep(pingZeroBudget + 8*pingInterval)

	body := w.bodyString()
	if strings.Contains(body, `"type":"error"`) {
		t.Errorf("budget fired on a stream that HAD produced output - zero is the bar, not a rate; body=%q", body)
	}
	if !strings.Contains(body, "ping") {
		t.Errorf("expected pings to keep bridging silence on a working stream; body=%q", body)
	}
}

// TestPingWriter_ParkedStreamIsHeldNotRefused is the @user-approved promise
// made mechanical (llama-cm docs/intent/llama-swap-backend.md, "You are held,
// not refused"): a request that is still PARKED in the scheduler queue is
// pinged through its whole wait, however long that wait is.
//
// The defect it locks out: pingZeroBudget used to measure from request
// arrival, so a session that was simply third in an honest queue - two serving
// slots, three sessions, measured ~374s to a slot - was handed an SSE error at
// 270s and started a retry ladder. A queue holding is the mechanism working,
// not a stream to end.
func TestPingWriter_ParkedStreamIsHeldNotRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("waits >pingQuietDelay of real time")
	}
	logger := logmon.NewWriter(io.Discard)
	w := newSyncRecorder()

	origQuiet, origInterval, origBudget := pingQuietDelay, pingInterval, pingZeroBudget
	pingQuietDelay, pingInterval = 50*time.Millisecond, 50*time.Millisecond
	pingZeroBudget = 300 * time.Millisecond
	t.Cleanup(func() {
		pingQuietDelay, pingInterval, pingZeroBudget = origQuiet, origInterval, origBudget
	})

	pw := newPingWriter(logger, "test-model", w, true, nil)
	defer pw.stop()

	// No slotGranted: this request never left the queue. Wait far past the
	// budget it would have blown under the arrival-keyed clock.
	time.Sleep(pingZeroBudget + 8*pingInterval)

	body := w.bodyString()
	if strings.Contains(body, `"type":"error"`) {
		t.Errorf("a parked request was REFUSED after %s of waiting - it must be held and pinged for as long as the queue holds it; body=%q", pingZeroBudget, body)
	}
	if !strings.Contains(body, "ping") {
		t.Errorf("a parked request must be pinged through its wait, not left silent; body=%q", body)
	}
}

// TestPingWriter_ZeroBudgetStartsAtSlotGrant pins the clock the budget runs
// on. Time spent parked must not be charged against it: only the stretch after
// the scheduler granted a slot counts, which is the condition the budget's own
// error message states ("it held a slot without emitting a token").
func TestPingWriter_ZeroBudgetStartsAtSlotGrant(t *testing.T) {
	if testing.Short() {
		t.Skip("waits >pingQuietDelay of real time")
	}
	logger := logmon.NewWriter(io.Discard)
	w := newSyncRecorder()

	origQuiet, origInterval, origBudget := pingQuietDelay, pingInterval, pingZeroBudget
	pingQuietDelay, pingInterval = 50*time.Millisecond, 50*time.Millisecond
	pingZeroBudget = 400 * time.Millisecond
	t.Cleanup(func() {
		pingQuietDelay, pingInterval, pingZeroBudget = origQuiet, origInterval, origBudget
	})

	pw := newPingWriter(logger, "test-model", w, true, nil)
	defer pw.stop()

	// A long park, then the grant. Under an arrival-keyed clock the budget is
	// already spent at this point.
	time.Sleep(pingZeroBudget + 4*pingInterval)
	pw.slotGranted()

	// Immediately after the grant the stream must still be alive: the budget
	// restarts here.
	time.Sleep(2 * pingInterval)
	if body := w.bodyString(); strings.Contains(body, `"type":"error"`) {
		t.Errorf("the budget charged the parked stretch against the grant - stream ended %s after being granted a slot; body=%q", 2*pingInterval, body)
	}

	// And a slot genuinely held without output still ends readably.
	time.Sleep(pingZeroBudget + 6*pingInterval)
	if body := w.bodyString(); !strings.Contains(body, `"type":"error"`) {
		t.Errorf("a granted slot that produced nothing for %s must end with a readable SSE error; body=%q", pingZeroBudget, body)
	}
}

// TestPingWriter_AllowPingsWakesImmediately covers the unmute latency. A
// replay-eligible request is muted until maxReplayHeld (270s), already close
// to the client's ~300s zero-byte abort, so noticing the unmute only at the
// next scheduled tick spends most of the remaining margin on silence.
func TestPingWriter_AllowPingsWakesImmediately(t *testing.T) {
	if testing.Short() {
		t.Skip("waits >pingQuietDelay of real time")
	}
	logger := logmon.NewWriter(io.Discard)
	w := newSyncRecorder()

	origQuiet, origInterval := pingQuietDelay, pingInterval
	pingQuietDelay = 20 * time.Millisecond
	pingInterval = 2 * time.Second
	t.Cleanup(func() { pingQuietDelay, pingInterval = origQuiet, origInterval })

	pw := newPingWriter(logger, "test-model", w, false, nil)
	defer pw.stop()

	// Let the muted loop park on its next tick, a full pingInterval away.
	time.Sleep(200 * time.Millisecond)
	if w.bodyLen() != 0 {
		t.Fatalf("a muted pinger wrote before being unmuted; body=%q", w.bodyString())
	}

	pw.allowPings()
	time.Sleep(300 * time.Millisecond)
	if body := w.bodyString(); !strings.Contains(body, "ping") {
		t.Errorf("unmute was not picked up promptly - the pinger waited for its next %s tick while the client's abort ceiling approached; body=%q", pingInterval, body)
	}
}
