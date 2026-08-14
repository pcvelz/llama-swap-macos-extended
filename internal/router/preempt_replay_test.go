package router

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// Transparent-replay tests (docs/intent/llama-swap-tiers.md "Known v1
// limitations" -> v2): a preemption victim that has written ZERO response
// bytes is silently re-submitted by baseRouter.ServeHTTP instead of being
// handed the v1 cancel+503. These drive the real FIFO scheduler (same as
// base_test.go) through a real KV-admission same-model preemption
// (scheduler/fifo.go's "SAME-model preemption" branch), so the preemption
// itself is genuine, not simulated.

var (
	replayTierBackground = swaputil.Tier{Name: "background", Rank: -10, Preemptible: true}
	replayTierDefault    = swaputil.Tier{Name: "default", Rank: 0}
)

// newReplayTestBase mirrors newTestBase (base_test.go) but accepts a
// FifoConfig, so these tests can configure KVPoolTokens to force a
// deterministic same-model preemption.
func newReplayTestBase(t *testing.T, processes map[string]process.Process, fifoCfg config.FifoConfig) *baseRouter {
	t.Helper()
	conf := config.Config{HealthCheckTimeout: 5}
	conf.Routing.Scheduler.Settings.Fifo = fifoCfg
	b, err := newBaseRouter("test", conf, processes, logmon.NewWriter(io.Discard), &stubPlanner{})
	if err != nil {
		t.Fatalf("newBaseRouter: %v", err)
	}
	b.testProcessed = make(chan struct{}, 64)
	go b.run()
	t.Cleanup(func() {
		if !b.shuttingDown.Load() {
			_ = b.Shutdown(time.Second)
		}
	})
	return b
}

// buildReplayReq constructs a POST /v1/chat/completions request for model,
// tagged with tier (as a tier-listener would) and an optional X-Role header
// so a shared fakeProcess.serveFunc can script per-caller behavior.
func buildReplayReq(model, role string, tier swaputil.Tier) *http.Request {
	body := fmt.Sprintf(`{"model":%q}`, model)
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if role != "" {
		r.Header.Set("X-Role", role)
	}
	ctx := swaputil.WithTier(context.Background(), tier)
	return r.WithContext(ctx)
}

// TestBaseRouter_PreemptReplay_ZeroBytesReplaysAndServes: a background-tier
// victim granted first, then preempted by a default-tier arrival before
// writing anything, is transparently re-submitted and eventually served —
// the client sees exactly one successful response, never a 503.
func TestBaseRouter_PreemptReplay_ZeroBytesReplaysAndServes(t *testing.T) {
	m1 := newFakeProcess("m1")
	m1.markReady()

	m1.serveFunc = func(w http.ResponseWriter, r *http.Request, callNum int) bool {
		if r.Header.Get("X-Role") == "victim" && callNum == 1 {
			// The victim's FIRST attempt: block until preempted, then mirror
			// a real reverse proxy aborting its upstream call — writes ITS
			// OWN failure status, which preemptResponseWriter must intercept.
			<-r.Context().Done()
			w.WriteHeader(http.StatusBadGateway)
			return true
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "served:%s:%d", r.Header.Get("X-Role"), callNum)
		return true
	}

	fifoCfg := config.FifoConfig{KVPoolTokens: map[string]int{"m1": 5}}
	b := newReplayTestBase(t, map[string]process.Process{"m1": m1}, fifoCfg)

	victimReq := buildReplayReq("m1", "victim", replayTierBackground)
	victimRec := httptest.NewRecorder()
	victimDone := make(chan struct{})
	go func() {
		defer close(victimDone)
		b.ServeHTTP(victimRec, victimReq)
	}()

	// Let the victim get admitted and start blocking before sending the
	// rival — otherwise the rival might win the race and there would be
	// nothing to preempt.
	select {
	case <-m1.serveStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("victim never reached ServeHTTP")
	}

	rivalReq := buildReplayReq("m1", "rival", replayTierDefault)
	rivalRec := httptest.NewRecorder()
	rivalDone := make(chan struct{})
	go func() {
		defer close(rivalDone)
		b.ServeHTTP(rivalRec, rivalReq)
	}()

	select {
	case <-rivalDone:
	case <-time.After(2 * time.Second):
		t.Fatal("rival never completed — victim was not preempted (KV admission didn't fire)")
	}
	if rivalRec.Code != http.StatusOK {
		t.Fatalf("rival status = %d, want 200", rivalRec.Code)
	}

	select {
	case <-victimDone:
	case <-time.After(2 * time.Second):
		t.Fatal("victim's ServeHTTP never returned — transparent replay did not complete")
	}

	if victimRec.Header().Get("X-LlamaSwap-Preempted") != "" {
		t.Fatalf("victim response carries X-LlamaSwap-Preempted — a v1 503 leaked to the client instead of a transparent replay")
	}
	if victimRec.Code != http.StatusOK {
		t.Fatalf("victim status = %d, want 200 (single successful response after replay)", victimRec.Code)
	}
	body := victimRec.Body.String()
	if !strings.HasPrefix(body, "served:victim:") {
		t.Fatalf("victim body = %q, want a real served:victim:N response from the retried attempt", body)
	}

	if calls := m1.serveCalls.Load(); calls < 3 {
		t.Fatalf("fakeProcess.ServeHTTP called %d times, want >= 3 (victim attempt 1 + rival + victim attempt 2)", calls)
	}
}

// TestBaseRouter_PreemptReplay_BytesAlreadyWrittenUnaffected: a victim that
// already wrote real response bytes before being preempted is NOT replayed —
// zero behavior change from v1. preempt.go's existing doc comment already
// establishes that v1 itself never synthesizes a 503 once bytes have
// streamed (there is nothing left to rewrite); this test proves the v2
// replay path does not change that either — the connection just carries
// whatever was already written, and the process is called exactly once.
func TestBaseRouter_PreemptReplay_BytesAlreadyWrittenUnaffected(t *testing.T) {
	m1 := newFakeProcess("m1")
	m1.markReady()

	m1.serveFunc = func(w http.ResponseWriter, r *http.Request, callNum int) bool {
		if r.Header.Get("X-Role") == "victim" {
			// Stream real bytes FIRST (preempted is still false at this
			// point), then wait to be preempted. No further write happens
			// after the cancel — matching a real proxy whose headers/body
			// are already committed.
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "partial")
			<-r.Context().Done()
			return true
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "rival-ok")
		return true
	}

	fifoCfg := config.FifoConfig{KVPoolTokens: map[string]int{"m1": 5}}
	b := newReplayTestBase(t, map[string]process.Process{"m1": m1}, fifoCfg)

	victimReq := buildReplayReq("m1", "victim", replayTierBackground)
	victimRec := httptest.NewRecorder()
	victimDone := make(chan struct{})
	go func() {
		defer close(victimDone)
		b.ServeHTTP(victimRec, victimReq)
	}()

	select {
	case <-m1.serveStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("victim never reached ServeHTTP")
	}

	rivalReq := buildReplayReq("m1", "rival", replayTierDefault)
	rivalRec := httptest.NewRecorder()
	rivalDone := make(chan struct{})
	go func() {
		defer close(rivalDone)
		b.ServeHTTP(rivalRec, rivalReq)
	}()

	select {
	case <-rivalDone:
	case <-time.After(2 * time.Second):
		t.Fatal("rival never completed — victim was not preempted")
	}

	select {
	case <-victimDone:
	case <-time.After(2 * time.Second):
		t.Fatal("victim's ServeHTTP never returned")
	}

	if victimRec.Code != http.StatusOK {
		t.Fatalf("victim status = %d, want 200 (its own first write, unaffected by the later preempt)", victimRec.Code)
	}
	if victimRec.Body.String() != "partial" {
		t.Fatalf("victim body = %q, want %q — bytes already streamed must survive untouched", victimRec.Body.String(), "partial")
	}
	if calls := m1.serveCalls.Load(); calls != 2 {
		t.Fatalf("fakeProcess.ServeHTTP called %d times, want exactly 2 (victim once + rival once) — a replay must not have happened", calls)
	}
}

// TestBaseRouter_PreemptReplay_CapFallsBackTo503: once a victim has been
// transparently replayed maxReplays times, the NEXT preemption falls back to
// the v1 cancel+503 exactly as before this change.
func TestBaseRouter_PreemptReplay_CapFallsBackTo503(t *testing.T) {
	m1 := newFakeProcess("m1")
	m1.markReady()

	victimGranted := make(chan struct{}, maxReplays+2)
	m1.serveFunc = func(w http.ResponseWriter, r *http.Request, callNum int) bool {
		if r.Header.Get("X-Role") == "victim" {
			victimGranted <- struct{}{}
			<-r.Context().Done()
			w.WriteHeader(http.StatusBadGateway)
			return true
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "rival-ok")
		return true
	}

	fifoCfg := config.FifoConfig{KVPoolTokens: map[string]int{"m1": 5}}
	b := newReplayTestBase(t, map[string]process.Process{"m1": m1}, fifoCfg)

	victimReq := buildReplayReq("m1", "victim", replayTierBackground)
	victimRec := httptest.NewRecorder()
	victimDone := make(chan struct{})
	go func() {
		defer close(victimDone)
		b.ServeHTTP(victimRec, victimReq)
	}()

	// Preempt the victim maxReplays+1 times in total: the first maxReplays
	// preemptions must each be transparently replayed; the (maxReplays+1)th
	// must fall back to the v1 503, since replayCount < maxReplays is false
	// by then (see base.go ServeHTTP's replayEligible computation).
	for i := 0; i < maxReplays+1; i++ {
		select {
		case <-victimGranted:
		case <-time.After(2 * time.Second):
			t.Fatalf("round %d: victim was never (re-)granted", i+1)
		}

		rivalReq := buildReplayReq("m1", "rival", replayTierDefault)
		rivalRec := httptest.NewRecorder()
		rivalDone := make(chan struct{})
		go func() {
			defer close(rivalDone)
			b.ServeHTTP(rivalRec, rivalReq)
		}()
		select {
		case <-rivalDone:
		case <-time.After(2 * time.Second):
			t.Fatalf("round %d: rival never completed — victim was not preempted", i+1)
		}
		if rivalRec.Code != http.StatusOK {
			t.Fatalf("round %d: rival status = %d, want 200", i+1, rivalRec.Code)
		}
	}

	select {
	case <-victimDone:
	case <-time.After(2 * time.Second):
		t.Fatal("victim's ServeHTTP never returned after the replay cap should have tripped")
	}

	if victimRec.Code != http.StatusServiceUnavailable {
		t.Fatalf("victim status = %d, want 503 (replay cap exhausted, v1 fallback)", victimRec.Code)
	}
	if got := victimRec.Header().Get("X-LlamaSwap-Preempted"); got != "1" {
		t.Fatalf("victim X-LlamaSwap-Preempted = %q, want %q", got, "1")
	}
}
