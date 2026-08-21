package router

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
)

// P4 tests: deadline-aware admission (feature A) and the sequenced pinger
// (feature B).
//
// Context (llama-cm incident
// 2026-08-18-cq27-granted-request-zero-byte-502-at-response-header-ceiling):
// the park-giveup fix answers the NEVER-GRANTED class. What remained was the
// GRANTED-BUT-SLOW class - a background request whose prefill cannot possibly
// produce a byte before the client's ~300s zero-byte wall. Serving it burns a
// slot for minutes and still ends in a client-side abort; parking it ends in a
// give-up. Refusing it up front with the canonical preempt-503 is the only
// outcome that leaves the client with something to back off on.
//
// Assertion style is inherited from park_giveup_test.go and is deliberately
// POSITIVE ("a real 503 carrying X-LlamaSwap-Preempted", "the body contains a
// ping event"), never "not a 502" - half the production population rendered as
// `200 0`, so a not-502 assertion would pass on a still-bare failure.
// testProcessed is likewise left nil in the helper below (see
// newParkTestBaseLogged for why the 64-slot channel is a landmine).

// newDeadlineTestBase builds a router with one model, full control over its
// ModelConfig (prefill rate, concurrency, loading state) and over the FIFO
// config (KVPoolTokens, for the tests that need a real preemption).
func newDeadlineTestBase(t *testing.T, m *fakeProcess, mc config.ModelConfig, fifoCfg config.FifoConfig, logTo io.Writer) *baseRouter {
	t.Helper()
	conf := config.Config{HealthCheckTimeout: 5, Models: map[string]config.ModelConfig{m.id: mc}}
	conf.Routing.Scheduler.Settings.Fifo = fifoCfg
	b, err := newBaseRouter("test", conf, map[string]process.Process{m.id: m}, logmon.NewWriter(logTo), &stubPlanner{})
	if err != nil {
		t.Fatalf("newBaseRouter: %v", err)
	}
	go b.run()
	t.Cleanup(func() {
		if !b.shuttingDown.Load() {
			_ = b.Shutdown(time.Second)
		}
	})
	return b
}

// shortenDeadlineBudget shrinks the client-budget model for the duration of a
// test. Note it does NOT touch parkGiveUpBudget: leaving the park budget at its
// production 270s is what proves a fast 503 came from the deadline refusal and
// not from a park give-up.
func shortenDeadlineBudget(t *testing.T, d time.Duration) {
	t.Helper()
	orig := deadlineBudget
	deadlineBudget = d
	t.Cleanup(func() { deadlineBudget = orig })
}

// shortenReplayHeld shrinks the replay held-time deadline, which is also the
// moment the sequenced pinger is unmuted.
func shortenReplayHeld(t *testing.T, d time.Duration) {
	t.Helper()
	orig := maxReplayHeld
	maxReplayHeld = d
	t.Cleanup(func() { maxReplayHeld = orig })
}

// shortenPingCadence makes pings observable inside a test's lifetime.
func shortenPingCadence(t *testing.T, d time.Duration) {
	t.Helper()
	origQuiet, origInterval := pingQuietDelay, pingInterval
	pingQuietDelay, pingInterval = d, d
	t.Cleanup(func() { pingQuietDelay, pingInterval = origQuiet, origInterval })
}

// statusCode is the locked reader for syncRecorder's status (see
// pinging_test.go for why these tests may not read the recorder directly).
func (r *syncRecorder) statusCode() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

// bigBody returns a JSON body for model whose length makes
// swaputil.EstimateTokens (len/4) return approximately tokens.
func bigBody(model string, tokens int) string {
	head := fmt.Sprintf(`{"model":%q,"pad":"`, model)
	tail := `"}`
	pad := tokens*4 - len(head) - len(tail)
	if pad < 0 {
		pad = 0
	}
	return head + strings.Repeat("x", pad) + tail
}

// TestBaseRouter_OverBudgetPreemptible_RefusedImmediately: THE CORE OF FEATURE
// A. A background request whose estimated prefill cannot finish inside the
// client's remaining budget is refused NOW, with the canonical preempt-503, and
// never reaches the model at all. RED before the fix: it is admitted, holds a
// slot for the whole prefill, and the client aborts zero-byte anyway.
func TestBaseRouter_OverBudgetPreemptible_RefusedImmediately(t *testing.T) {
	shortenDeadlineBudget(t, 2*time.Second)

	m1 := newFakeProcess("m1")
	m1.markReady()
	logs := &syncBuf{}
	// 1 token/s: 100 estimated tokens is 100s of prefill against a 2s budget.
	mc := config.ModelConfig{ConcurrencyLimit: 1, PrefillTokensPerSecond: 1}
	b := newDeadlineTestBase(t, m1, mc, config.FifoConfig{}, logs)

	rec := httptest.NewRecorder()
	start := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.ServeHTTP(rec, buildParkReq("m1", "over-budget", replayTierBackground,
			"/v1/chat/completions", bigBody("m1", 100)))
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("over-budget request was not refused - it is parked or serving toward a doomed prefill")
	}
	elapsed := time.Since(start)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 - the client must get a real status to back off on", rec.Code)
	}
	if got := rec.Header().Get("X-LlamaSwap-Preempted"); got != "1" {
		t.Fatalf("X-LlamaSwap-Preempted = %q, want \"1\" - the refusal must be the canonical preempt-503", got)
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Fatal("Retry-After missing - it is what makes the client back off instead of 0s-retrying")
	}
	// FAST is the whole point: this must be an up-front refusal, not a park
	// that eventually gives up (parkGiveUpBudget is untouched at 270s here).
	if elapsed > time.Second {
		t.Fatalf("refusal took %v - a deadline refusal must be immediate, not the result of a park", elapsed)
	}
	if calls := m1.serveCalls.Load(); calls != 0 {
		t.Fatalf("fakeProcess.ServeHTTP called %d times, want 0 - a doomed request must never be granted a slot", calls)
	}
	if got := logs.String(); !strings.Contains(got, "deadline-refuse: tier=background model=m1 est_tokens=100 est_s=100") {
		t.Fatalf("deadline-refuse log line missing or malformed.\nlogs:\n%s", got)
	}
}

// TestBaseRouter_WithinBudgetPreemptible_ServesNormally: the trap guard on
// feature A. A background request that CAN finish inside the budget is
// untouched - the refusal must key on the estimate, not on the tier.
func TestBaseRouter_WithinBudgetPreemptible_ServesNormally(t *testing.T) {
	shortenDeadlineBudget(t, 60*time.Second)

	m1 := newFakeProcess("m1")
	m1.markReady()
	// 100 t/s against ~100 estimated tokens is ~1s of prefill: comfortably
	// inside the 60s budget.
	mc := config.ModelConfig{ConcurrencyLimit: 1, PrefillTokensPerSecond: 100}
	b := newDeadlineTestBase(t, m1, mc, config.FifoConfig{}, io.Discard)

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.ServeHTTP(rec, buildParkReq("m1", "within-budget", replayTierBackground,
			"/v1/chat/completions", bigBody("m1", 100)))
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("within-budget request never completed")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 - a request that fits the budget must serve", rec.Code)
	}
	if rec.Header().Get("X-LlamaSwap-Preempted") != "" {
		t.Fatal("within-budget request carries X-LlamaSwap-Preempted - the deadline refused a request that had time")
	}
	if calls := m1.serveCalls.Load(); calls != 1 {
		t.Fatalf("fakeProcess.ServeHTTP called %d times, want 1", calls)
	}
}

// TestBaseRouter_NonPreemptible_NeverDeadlineRefused: THE REGRESSION GUARD.
// Interactive (non-preemptible) traffic is never refused on an ESTIMATE,
// however large its context - refusing paid interactive requests would be a far
// worse regression than the background starvation this fixes. Same
// over-budget input as the core test, opposite outcome.
func TestBaseRouter_NonPreemptible_NeverDeadlineRefused(t *testing.T) {
	shortenDeadlineBudget(t, 2*time.Second)

	m1 := newFakeProcess("m1")
	m1.markReady()
	mc := config.ModelConfig{ConcurrencyLimit: 1, PrefillTokensPerSecond: 1}
	b := newDeadlineTestBase(t, m1, mc, config.FifoConfig{}, io.Discard)

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.ServeHTTP(rec, buildParkReq("m1", "interactive", replayTierDefault,
			"/v1/chat/completions", bigBody("m1", 100)))
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("non-preemptible request never completed")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 - a non-preemptible request must never be deadline-refused", rec.Code)
	}
	if rec.Header().Get("X-LlamaSwap-Preempted") != "" {
		t.Fatal("non-preemptible request carries X-LlamaSwap-Preempted - the deadline refusal leaked onto interactive traffic")
	}
}

// TestBaseRouter_SequencedPinger_SilentWhileReplayEligible: THE CORE OF FEATURE
// B. Pinging and transparent replay are mutually exclusive by construction (the
// first ping commits a 200; a replay requires zero committed bytes), so the
// resolution is SEQUENCING: silence while the request may still be replayed,
// pings from the moment that eligibility is surrendered. RED before the fix:
// the pinger starts at pingQuietDelay regardless, permanently disqualifying the
// request from the replay it is eligible for.
func TestBaseRouter_SequencedPinger_SilentWhileReplayEligible(t *testing.T) {
	const held = 600 * time.Millisecond
	shortenReplayHeld(t, held)
	shortenPingCadence(t, 30*time.Millisecond)

	m1 := newFakeProcess("m1")
	m1.markReady()
	sendLoading := true
	mc := config.ModelConfig{ConcurrencyLimit: 1, SendLoadingState: &sendLoading}
	b := newDeadlineTestBase(t, m1, mc, config.FifoConfig{}, io.Discard)
	release := startHog(t, b, m1)

	rec := newSyncRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.ServeHTTP(rec, buildParkReq("m1", "streamer", replayTierBackground,
			"/v1/messages", `{"model":"m1","stream":true}`))
	}()

	// Well past pingQuietDelay but well inside the replay held-time: the
	// request is still replay-eligible, so it must have emitted NOTHING.
	time.Sleep(held / 2)
	if got := rec.bodyString(); got != "" {
		t.Fatalf("replay-eligible request emitted %q while still eligible - a committed ping forecloses the replay", got)
	}
	if code := rec.statusCode(); code != 0 {
		t.Fatalf("replay-eligible request committed status %d while still eligible", code)
	}

	// Past the held-time: eligibility is surrendered, so the pinger must take
	// over and keep the still-parked request alive.
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(rec.bodyString(), "event: ping") {
		if time.Now().After(deadline) {
			t.Fatalf("no ping after replay eligibility was surrendered - the request is silent into the client's wall.\nbody: %q", rec.bodyString())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// And it still completes once the slot frees.
	release()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("streaming request never completed after the slot freed")
	}
	if !strings.Contains(rec.bodyString(), "served") {
		t.Fatalf("streaming request body = %q, want the served payload after the pings", rec.bodyString())
	}
}

// TestBaseRouter_PingedRequest_NeverReplays: the interaction guard. A request
// whose pings already committed the response must NOT be transparently replayed
// - preemptResponseWriter keeps its own wroteHeader, so it would happily
// swallow a header and ask for a replay on a stream the client is already
// reading, which would deliver a second response body on that same stream. The
// stream is ended as an SSE error event instead, and the model is entered
// exactly once for this caller.
func TestBaseRouter_PingedRequest_NeverReplays(t *testing.T) {
	// Surrender eligibility almost immediately so the victim is a PINGED
	// request by the time the rival preempts it, while its attempt still holds
	// a live replayWanted flag - the exact window the guard covers.
	shortenReplayHeld(t, 50*time.Millisecond)
	shortenPingCadence(t, 30*time.Millisecond)

	m1 := newFakeProcess("m1")
	m1.markReady()
	var victimCalls atomic.Int32
	m1.serveFunc = func(w http.ResponseWriter, r *http.Request, callNum int) bool {
		if r.Header.Get("X-Role") == "victim" {
			victimCalls.Add(1)
			// Mirror a reverse proxy aborting its upstream call on preemption:
			// it writes its OWN failure status, which preemptResponseWriter
			// intercepts as the replay signal.
			<-r.Context().Done()
			w.WriteHeader(http.StatusBadGateway)
			return true
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "rival-ok")
		return true
	}

	sendLoading := true
	mc := config.ModelConfig{SendLoadingState: &sendLoading}
	logs := &syncBuf{}
	// Pool 8: the victim (est 7) is admitted alone, and the rival (est 3)
	// pushes the model over, forcing a real same-model preemption.
	b := newDeadlineTestBase(t, m1, mc, config.FifoConfig{KVPoolTokens: map[string]int{"m1": 8}}, logs)

	rec := newSyncRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.ServeHTTP(rec, buildParkReq("m1", "victim", replayTierBackground,
			"/v1/messages", `{"model":"m1","stream":true}`))
	}()

	// Wait until the victim has genuinely committed its response with a ping.
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(rec.bodyString(), "event: ping") {
		if time.Now().After(deadline) {
			t.Fatalf("victim never pinged, so this test is not exercising the guard.\nbody: %q", rec.bodyString())
		}
		time.Sleep(10 * time.Millisecond)
	}

	go b.ServeHTTP(httptest.NewRecorder(), buildParkReq("m1", "rival", replayTierDefault,
		"/v1/chat/completions", `{"model":"m1"}`))

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("victim never finished - it is stuck in a replay it should never have started")
	}

	if got := victimCalls.Load(); got != 1 {
		t.Fatalf("victim entered the model %d times, want exactly 1 - a pinged request must never replay", got)
	}
	if body := rec.bodyString(); !strings.Contains(body, "event: error") {
		t.Fatalf("victim body = %q, want a terminal SSE error event - a committed stream must be ended, not silently abandoned", body)
	}
	if got := logs.String(); !strings.Contains(got, "suppressed - keepalive pings already committed the response") {
		t.Fatalf("the replay-suppression log line is missing.\nlogs:\n%s", got)
	}
}

// TestBaseRouter_DeadlineRefuse_NoLeak: refusals must leave no scheduler
// bookkeeping behind. Same shape as the park-give-up no-leak test: several
// requests are refused, then a fresh one must be served immediately.
func TestBaseRouter_DeadlineRefuse_NoLeak(t *testing.T) {
	shortenDeadlineBudget(t, 2*time.Second)

	m1 := newFakeProcess("m1")
	m1.markReady()
	mc := config.ModelConfig{ConcurrencyLimit: 1, PrefillTokensPerSecond: 1}
	b := newDeadlineTestBase(t, m1, mc, config.FifoConfig{}, io.Discard)

	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		d := make(chan struct{})
		go func() {
			defer close(d)
			b.ServeHTTP(rec, buildParkReq("m1", "over-budget", replayTierBackground,
				"/v1/chat/completions", bigBody("m1", 100)))
		}()
		select {
		case <-d:
		case <-time.After(5 * time.Second):
			t.Fatalf("refusal %d never completed", i)
		}
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("refusal %d status = %d, want 503", i, rec.Code)
		}
	}

	rec := httptest.NewRecorder()
	fresh := make(chan struct{})
	go func() {
		defer close(fresh)
		b.ServeHTTP(rec, buildParkReq("m1", "fresh", replayTierDefault,
			"/v1/chat/completions", `{"model":"m1"}`))
	}()
	select {
	case <-fresh:
	case <-time.After(5 * time.Second):
		t.Fatal("a fresh request could not be served after the refusals - scheduler bookkeeping leaked")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("fresh request status = %d, want 200", rec.Code)
	}
}
