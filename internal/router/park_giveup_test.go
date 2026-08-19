package router

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// Park-give-up tests (llama-cm incident
// 2026-08-18-cq27-background-admission-park-bare-502-retry-storm): a
// preemptible request that parks waiting for a busy model's only slot used to
// return BARE - writing nothing at all - once the client's own ~300s zero-byte
// abort cancelled its context. llama-swap logged that as `502 0` (the reverse
// proxy's own status) or `200 0` (nothing wrote a status, so the access log
// defaulted), both at a metronomic 5m00, and the client 0s-retried into a
// self-sustaining storm.
//
// The fix bounds the park itself with parkGiveUpBudget and answers with the
// canonical preempt-503 BEFORE the client's abort, so the client backs off.
// These tests drive the real FIFO scheduler through a real concurrency-cap
// park (scheduler/fifo.go atCapacity -> enqueue), so the park is genuine.
//
// Assertions are deliberately POSITIVE - "a real 503 carrying
// X-LlamaSwap-Preempted" - never "not a 502": half the production population
// rendered as `200 0`, so a not-502 assertion would pass on a still-bare
// give-up.

// newParkTestBase builds a router whose single model has concurrencyLimit 1,
// so the second request for it is guaranteed to park in the scheduler queue.
// extra, when non-nil, may further adjust the model config (e.g. arm
// SendLoadingState for the pingWriter test).
func newParkTestBase(t *testing.T, m *fakeProcess, extra func(*config.ModelConfig)) *baseRouter {
	t.Helper()
	return newParkTestBaseLogged(t, m, extra, io.Discard)
}

// syncBuf is a concurrency-safe io.Writer so a test can read the router's log
// output while the router is still running.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func newParkTestBaseLogged(t *testing.T, m *fakeProcess, extra func(*config.ModelConfig), logTo io.Writer) *baseRouter {
	t.Helper()
	mc := config.ModelConfig{ConcurrencyLimit: 1}
	if extra != nil {
		extra(&mc)
	}
	conf := config.Config{HealthCheckTimeout: 5, Models: map[string]config.ModelConfig{m.id: mc}}
	b, err := newBaseRouter("test", conf, map[string]process.Process{m.id: m}, logmon.NewWriter(logTo), &stubPlanner{})
	if err != nil {
		t.Fatalf("newBaseRouter: %v", err)
	}
	// Deliberately NOT wiring testProcessed: baseRouter.notifyProcessed does a
	// BLOCKING send on it, so a router that outlives its 64-slot buffer wedges
	// its own run loop when nothing drains it. These tests synchronise on the
	// requests themselves, so leaving it nil removes that landmine - the
	// rendezvous-race test alone drives several hundred scheduler events.
	go b.run()
	t.Cleanup(func() {
		if !b.shuttingDown.Load() {
			_ = b.Shutdown(time.Second)
		}
	})
	return b
}

// shortenParkBudget shrinks the give-up budget for the duration of a test so
// the 270s production deadline does not have to be waited out.
func shortenParkBudget(t *testing.T, d time.Duration) {
	t.Helper()
	orig := parkGiveUpBudget
	parkGiveUpBudget = d
	t.Cleanup(func() { parkGiveUpBudget = orig })
}

// buildParkReq builds a request for model tagged with tier. path/body let the
// pingWriter test send a streaming Anthropic request instead of the default
// non-streaming one (the production victim population is exactly the
// non-streaming/no-keepalive calls).
func buildParkReq(model, role string, tier swaputil.Tier, path, body string) *http.Request {
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if role != "" {
		r.Header.Set("X-Role", role)
	}
	return r.WithContext(swaputil.WithTier(context.Background(), tier))
}

// startHog occupies the model's only slot until the returned release func is
// called, and waits until it is genuinely inside ServeHTTP before returning.
func startHog(t *testing.T, b *baseRouter, m *fakeProcess) (release func()) {
	t.Helper()
	hogGate := make(chan struct{})
	m.serveFunc = func(w http.ResponseWriter, r *http.Request, callNum int) bool {
		if r.Header.Get("X-Role") == "hog" {
			<-hogGate
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "hog-ok")
			return true
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "served")
		return true
	}

	go b.ServeHTTP(httptest.NewRecorder(),
		buildParkReq(m.id, "hog", replayTierDefault, "/v1/chat/completions", fmt.Sprintf(`{"model":%q}`, m.id)))

	select {
	case <-m.serveStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("hog never reached ServeHTTP - the slot was never occupied")
	}

	closed := false
	return func() {
		if !closed {
			closed = true
			close(hogGate)
		}
	}
}

// TestBaseRouter_ParkedPreemptibleRequest_GetsCanonical503: THE CORE FIX. A
// background-tier request parked behind a monopolized slot must receive a real
// 503 with X-LlamaSwap-Preempted + Retry-After when the park budget expires -
// not a bare return. RED before the fix: ServeHTTP blocks until the test
// context/timeout, then returns having written nothing.
func TestBaseRouter_ParkedPreemptibleRequest_GetsCanonical503(t *testing.T) {
	shortenParkBudget(t, 300*time.Millisecond)

	m1 := newFakeProcess("m1")
	m1.markReady()
	logs := &syncBuf{}
	b := newParkTestBaseLogged(t, m1, nil, logs)
	release := startHog(t, b, m1)
	defer release()

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.ServeHTTP(rec, buildParkReq("m1", "parked", replayTierBackground,
			"/v1/chat/completions", `{"model":"m1"}`))
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("parked request never gave up - it is still waiting for the slot (the bare-park bug)")
	}

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("parked request status = %d, want 503 - a bare give-up renders as 502 0 or 200 0 in production", rec.Code)
	}
	if got := rec.Header().Get("X-LlamaSwap-Preempted"); got != "1" {
		t.Fatalf("X-LlamaSwap-Preempted = %q, want \"1\" - the client must see the canonical preempt-503, not any 503", got)
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Fatal("Retry-After missing - it is what makes the client back off instead of 0s-retrying")
	}
	if calls := m1.serveCalls.Load(); calls != 1 {
		t.Fatalf("fakeProcess.ServeHTTP called %d times, want exactly 1 (the hog only) - the parked request must never have been granted", calls)
	}
	// Locks in WHICH wait actually parks. The incident doc originally placed
	// the park at the admission handshake; fifo.go OnRequest admits before it
	// consults capacity (and Admit is buffered), so the real wait is the GRANT
	// handshake. If a future refactor moves the park, this assertion fails
	// loudly rather than letting the give-up quietly cover the wrong select.
	if got := logs.String(); !strings.Contains(got, "stage=grant") {
		t.Fatalf("give-up log does not report stage=grant - the park is not where the fix assumes.\nlogs:\n%s", got)
	}
}

// TestBaseRouter_GrantedRequest_ServesPastGiveUpBudget: THE TRAP GUARD. The
// budget bounds only the PARK. A request that won the slot must stream past it
// untouched - putting the deadline on reqCtx instead would kill a healthy
// long-running generation at 270s, a strictly worse bug than the one being
// fixed.
func TestBaseRouter_GrantedRequest_ServesPastGiveUpBudget(t *testing.T) {
	shortenParkBudget(t, 200*time.Millisecond)

	m1 := newFakeProcess("m1")
	m1.markReady()
	m1.serveFunc = func(w http.ResponseWriter, r *http.Request, callNum int) bool {
		// Serve for well over the budget, exactly like a real long generation.
		select {
		case <-time.After(1 * time.Second):
		case <-r.Context().Done():
			t.Errorf("granted request's context was cancelled mid-serve - the give-up deadline leaked onto reqCtx")
			return true
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "long-generation")
		return true
	}

	b := newParkTestBase(t, m1, nil)

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.ServeHTTP(rec, buildParkReq("m1", "solo", replayTierBackground,
			"/v1/chat/completions", `{"model":"m1"}`))
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("granted request never completed")
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("granted request status = %d, want 200 - it held the slot and must not be given up", rec.Code)
	}
	if rec.Header().Get("X-LlamaSwap-Preempted") != "" {
		t.Fatal("granted request carries X-LlamaSwap-Preempted - the park deadline fired on a request that was already serving")
	}
	if got := rec.Body.String(); got != "long-generation" {
		t.Fatalf("granted request body = %q, want %q (full response, uncut)", got, "long-generation")
	}
}

// TestBaseRouter_PingWriterArmed_NotGivenUp: THE REGRESSION GUARD. A streaming
// Anthropic /v1/messages request gets a pingWriter (base.go, isAnthropicStreamPath)
// that emits SSE keepalives while parked, so it has ALREADY committed a status
// line and real bytes and is never the zero-byte victim this fix targets.
// Firing the give-up on it would abort a request that is currently kept alive
// and eventually served - and its 503 could not even be written. The deadline
// must arm only while the response is still uncommitted.
func TestBaseRouter_PingWriterArmed_NotGivenUp(t *testing.T) {
	shortenParkBudget(t, 200*time.Millisecond)
	// Make the keepalive fire well inside the shortened budget so the response
	// is genuinely committed by the time the deadline would have expired.
	origQuiet, origInterval := pingQuietDelay, pingInterval
	pingQuietDelay, pingInterval = 20*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { pingQuietDelay, pingInterval = origQuiet, origInterval })

	m1 := newFakeProcess("m1")
	m1.markReady()
	sendLoading := true
	b := newParkTestBase(t, m1, func(mc *config.ModelConfig) { mc.SendLoadingState = &sendLoading })
	release := startHog(t, b, m1)

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.ServeHTTP(rec, buildParkReq("m1", "streamer", replayTierBackground,
			"/v1/messages", `{"model":"m1","stream":true}`))
	}()

	// Well past the budget: a wrongly-armed deadline would have fired by now.
	time.Sleep(1 * time.Second)

	select {
	case <-done:
		t.Fatalf("pinged streaming request was given up (status %d) - it is kept alive by SSE pings and must keep waiting for the slot", rec.Code)
	default:
	}

	// Releasing the hog frees the slot; the parked streamer must then be served.
	release()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("streaming request never completed after the slot freed")
	}
	if rec.Header().Get("X-LlamaSwap-Preempted") != "" {
		t.Fatal("streaming request carries X-LlamaSwap-Preempted - the give-up fired on a keepalive-protected request")
	}
}

// TestBaseRouter_ParkGiveUp_NoInFlightLeak: the give-up path must prune the
// request from the scheduler (cancelCh) exactly like a client disconnect does.
// If it did not, the model's inFlight/queue bookkeeping would drift and the
// freed slot would never be handed to the next waiter - a leak that degrades
// into a deadlock over a long run.
func TestBaseRouter_ParkGiveUp_NoInFlightLeak(t *testing.T) {
	shortenParkBudget(t, 200*time.Millisecond)

	m1 := newFakeProcess("m1")
	m1.markReady()
	b := newParkTestBase(t, m1, nil)
	release := startHog(t, b, m1)

	// Three parked requests all give up.
	var dones []chan struct{}
	for i := 0; i < 3; i++ {
		d := make(chan struct{})
		dones = append(dones, d)
		go func() {
			defer close(d)
			rec := httptest.NewRecorder()
			b.ServeHTTP(rec, buildParkReq("m1", "parked", replayTierBackground,
				"/v1/chat/completions", `{"model":"m1"}`))
		}()
	}
	for i, d := range dones {
		select {
		case <-d:
		case <-time.After(5 * time.Second):
			t.Fatalf("parked request %d never gave up", i)
		}
	}

	// Free the slot; a fresh request must be served immediately, proving the
	// three give-ups released their queue/inFlight bookkeeping.
	release()
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
		t.Fatal("a fresh request could not be served after the give-ups - scheduler bookkeeping leaked")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("fresh request status = %d, want 200", rec.Code)
	}
}

// TestBaseRouter_ParkGiveUp_BudgetSpansReplayAttempts: the budget is ONE
// allowance measured from arrival, not a fresh one per replay attempt.
// Otherwise a request that is preempted and re-parked repeatedly could outlive
// the client's ~300s ceiling several times over and still never receive a 503 -
// the exact failure the budget exists to prevent.
//
// Shape: the victim is granted, spends real time on attempt 1, is preempted by
// a higher-rank rival (real KV-admission preemption), replays, and then parks
// behind that rival. If the budget restarted per attempt the give-up would land
// at roughly attempt-1-time + budget; it must land at budget total.
func TestBaseRouter_ParkGiveUp_BudgetSpansReplayAttempts(t *testing.T) {
	const budget = 600 * time.Millisecond
	const attempt1Work = 300 * time.Millisecond
	shortenParkBudget(t, budget)

	m1 := newFakeProcess("m1")
	m1.markReady()

	rivalGate := make(chan struct{})
	m1.serveFunc = func(w http.ResponseWriter, r *http.Request, callNum int) bool {
		switch r.Header.Get("X-Role") {
		case "victim":
			// Attempt 1: burn real budget, then wait to be preempted. Mirrors a
			// reverse proxy writing its own failure status on abort, which
			// preemptResponseWriter swallows for the replay.
			time.Sleep(attempt1Work)
			<-r.Context().Done()
			w.WriteHeader(http.StatusBadGateway)
			return true
		case "rival":
			// Hold the slot so the victim's replay attempt parks.
			<-rivalGate
			w.WriteHeader(http.StatusOK)
			return true
		}
		w.WriteHeader(http.StatusOK)
		return true
	}

	fifoCfg := config.FifoConfig{KVPoolTokens: map[string]int{"m1": 5}}
	b := newReplayTestBase(t, map[string]process.Process{"m1": m1}, fifoCfg)
	defer close(rivalGate)

	rec := httptest.NewRecorder()
	start := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.ServeHTTP(rec, buildReplayReq("m1", "victim", replayTierBackground))
	}()

	select {
	case <-m1.serveStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("victim never reached ServeHTTP")
	}

	go b.ServeHTTP(httptest.NewRecorder(), buildReplayReq("m1", "rival", replayTierDefault))

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("victim never gave up")
	}
	elapsed := time.Since(start)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("victim status = %d, want 503 after its budget ran out across attempts", rec.Code)
	}
	// Per-attempt accounting would land at >= attempt1Work + budget.
	if elapsed >= attempt1Work+budget {
		t.Fatalf("gave up after %v, but the whole budget is %v - the budget restarted on the replay attempt instead of spanning both",
			elapsed, budget)
	}
}

// TestBaseRouter_ParkGiveUp_GrantRendezvousRace: the highest-stakes
// interleaving. When the slot frees at the same instant the budget expires,
// select may take either case. baseRouter.grant is blocked on an UNBUFFERED
// send at that moment, so a give-up that mishandled it would strand the run
// loop. Every iteration must end in exactly one of "served" or "given up", and
// the router must still work afterwards.
func TestBaseRouter_ParkGiveUp_GrantRendezvousRace(t *testing.T) {
	const budget = 15 * time.Millisecond
	shortenParkBudget(t, budget)

	m1 := newFakeProcess("m1")
	m1.markReady()
	b := newParkTestBase(t, m1, nil)

	for i := 0; i < 100; i++ {
		hogGate := make(chan struct{})
		m1.serveFunc = func(w http.ResponseWriter, r *http.Request, callNum int) bool {
			if r.Header.Get("X-Role") == "hog" {
				<-hogGate
			}
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "ok")
			return true
		}

		hogDone := make(chan struct{})
		go func() {
			defer close(hogDone)
			b.ServeHTTP(httptest.NewRecorder(), buildParkReq("m1", "hog", replayTierDefault,
				"/v1/chat/completions", `{"model":"m1"}`))
		}()

		rec := httptest.NewRecorder()
		victimDone := make(chan struct{})
		go func() {
			defer close(victimDone)
			b.ServeHTTP(rec, buildParkReq("m1", "parked", replayTierBackground,
				"/v1/chat/completions", `{"model":"m1"}`))
		}()

		// Free the slot right around the budget so grant and the timer race.
		time.AfterFunc(budget, func() { close(hogGate) })

		select {
		case <-victimDone:
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d: victim neither served nor gave up - the run loop is wedged", i)
		}
		select {
		case <-hogDone:
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d: hog never completed - the run loop is wedged", i)
		}

		if rec.Code != http.StatusOK && rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("iteration %d: victim status = %d, want 200 (won the race) or 503 (gave up)", i, rec.Code)
		}
	}

	// The router must still serve normally after 100 races.
	m1.serveFunc = nil
	rec := httptest.NewRecorder()
	after := make(chan struct{})
	go func() {
		defer close(after)
		b.ServeHTTP(rec, buildParkReq("m1", "after", replayTierDefault,
			"/v1/chat/completions", `{"model":"m1"}`))
	}()
	select {
	case <-after:
	case <-time.After(5 * time.Second):
		t.Fatal("router could not serve after the race loop - state leaked")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("post-race request status = %d, want 200", rec.Code)
	}
}
