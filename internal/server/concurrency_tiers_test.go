package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/shared"
)

// tieredConcurrencyTestReq builds a request tagged with both a Tier (as the
// real request-context middleware would set from the arrival listener) and a
// shared.PreemptHandle (as CreateRequestContextMiddleware creates before
// CreateConcurrencyMiddleware ever runs) — mirroring the real modelChain
// wiring in server.go, not just the bare ReqContextData the pre-tiers tests
// in concurrency_test.go use.
func tieredConcurrencyTestReq(model string, tier shared.Tier) (*http.Request, *shared.PreemptHandle) {
	r := httptest.NewRequest("GET", "/v1/chat/completions", nil)
	ctx := shared.SetContext(r.Context(), shared.ReqContextData{Model: model, ModelID: model, Tier: tier})
	ctx, cancel := context.WithCancel(ctx)
	handle := &shared.PreemptHandle{Flag: new(atomic.Bool), Cancel: cancel}
	ctx = shared.WithPreemptHandle(ctx, handle)
	return r.WithContext(ctx), handle
}

// (a) A priority-tier arrival preempts a background-tier holder at
// concurrencyLimit 1: the background holder's Flag is set and its context is
// cancelled, and the priority arrival proceeds once the victim's handler
// unwinds and releases the permit.
func TestServer_ConcurrencyMiddleware_PriorityPreemptsBackgroundHolder(t *testing.T) {
	cfg := config.Config{
		Models: map[string]config.ModelConfig{
			"m1": {ConcurrencyLimit: 1},
		},
	}

	background := shared.Tier{Name: "background", Rank: -10, Preemptible: true}
	priority := shared.Tier{Name: "priority", Rank: 10, Preempts: true}

	entered := make(chan string, 2)
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		role := r.Header.Get("X-Role")
		entered <- role
		if role == "background" {
			// Blocks until preempted; a well-behaved victim unwinds on
			// context cancellation, exactly like a real upstream proxy
			// handler would (see router.trackedServe).
			<-r.Context().Done()
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	h := CreateConcurrencyMiddleware(cfg)(final)

	bgReq, bgHandle := tieredConcurrencyTestReq("m1", background)
	bgReq.Header.Set("X-Role", "background")
	bgRec := httptest.NewRecorder()
	bgDone := make(chan struct{})
	go func() {
		defer close(bgDone)
		h.ServeHTTP(bgRec, bgReq)
	}()

	if role := <-entered; role != "background" {
		t.Fatalf("first handler entered = %q, want background", role)
	}

	if bgHandle.Flag.Load() {
		t.Fatal("background holder preempted before any priority arrival")
	}

	prReq, _ := tieredConcurrencyTestReq("m1", priority)
	prReq.Header.Set("X-Role", "priority")
	prRec := httptest.NewRecorder()
	prDone := make(chan struct{})
	go func() {
		defer close(prDone)
		h.ServeHTTP(prRec, prReq)
	}()

	select {
	case <-bgDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for background holder to be preempted and unwind")
	}
	if !bgHandle.Flag.Load() {
		t.Fatal("background holder's Preempted flag was never set")
	}
	if bgRec.Code != http.StatusServiceUnavailable {
		t.Fatalf("background holder status = %d, want 503 (cancelled context)", bgRec.Code)
	}

	if role := <-entered; role != "priority" {
		t.Fatalf("second handler entered = %q, want priority", role)
	}

	select {
	case <-prDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for priority arrival to be granted the freed permit")
	}
	if prRec.Code != http.StatusOK {
		t.Fatalf("priority arrival status = %d, want 200", prRec.Code)
	}
}

// (b) An equal-rank arrival must NOT preempt: the holder's Flag stays unset
// and its context is never cancelled while the arrival waits for the permit
// the ordinary way.
func TestServer_ConcurrencyMiddleware_EqualRankDoesNotPreempt(t *testing.T) {
	cfg := config.Config{
		Models: map[string]config.ModelConfig{
			"m1": {ConcurrencyLimit: 1},
		},
	}

	// preempts:true does not matter here — same rank never crosses, per
	// shared.CanPreempt (rank(victim) >= rank(arrival) always disqualifies).
	priority := shared.Tier{Name: "priority", Rank: 10, Preempts: true}

	// Buffered so a second (unexpected, in the passing case never reached
	// while release is closed) entry never blocks the handler goroutine.
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	})
	h := CreateConcurrencyMiddleware(cfg)(final)

	firstReq, firstHandle := tieredConcurrencyTestReq("m1", priority)
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		h.ServeHTTP(httptest.NewRecorder(), firstReq)
	}()
	<-entered

	secondReq, _ := tieredConcurrencyTestReq("m1", priority)
	w2 := httptest.NewRecorder()
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		h.ServeHTTP(w2, secondReq)
	}()

	select {
	case <-secondDone:
		t.Fatal("equal-rank arrival must not be granted while the slot is held")
	case <-time.After(50 * time.Millisecond):
	}

	if firstHandle.Flag.Load() {
		t.Fatal("equal-rank arrival preempted the first holder; must never cross equal ranks")
	}
	select {
	case <-firstReq.Context().Done():
		t.Fatal("first holder's context was cancelled by an equal-rank arrival")
	default:
	}

	close(release)
	<-firstDone
	<-secondDone
	if w2.Code != http.StatusOK {
		t.Fatalf("queued equal-rank request status = %d, want 200 after the slot freed normally", w2.Code)
	}
}

// (c) Default-tier-only (zero-config) behavior is unchanged: two DefaultTier
// arrivals never preempt each other, even though both now carry a real
// shared.PreemptHandle (as production requests do once
// CreateRequestContextMiddleware runs). The second still just queues behind
// the first and completes once it releases — byte-identical to
// TestServer_ConcurrencyMiddleware_QueuesOverLimit in concurrency_test.go,
// which additionally covers requests with no tier/handle at all.
func TestServer_ConcurrencyMiddleware_DefaultTierOnlyUnchanged(t *testing.T) {
	cfg := config.Config{
		Models: map[string]config.ModelConfig{
			"m1": {ConcurrencyLimit: 1},
		},
	}

	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	})
	h := CreateConcurrencyMiddleware(cfg)(final)

	firstReq, firstHandle := tieredConcurrencyTestReq("m1", shared.DefaultTier)
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		h.ServeHTTP(httptest.NewRecorder(), firstReq)
	}()
	<-entered

	secondReq, _ := tieredConcurrencyTestReq("m1", shared.DefaultTier)
	w2 := httptest.NewRecorder()
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		h.ServeHTTP(w2, secondReq)
	}()

	select {
	case <-secondDone:
		t.Fatal("second default-tier request completed while the slot was still held; want it queued")
	default:
	}

	if firstHandle.Flag.Load() {
		t.Fatal("default-tier arrival preempted another default-tier holder; zero-config must never preempt")
	}

	close(release)
	<-firstDone
	<-secondDone
	if w2.Code != http.StatusOK {
		t.Fatalf("queued request status = %d, want 200 after slot freed", w2.Code)
	}
}
