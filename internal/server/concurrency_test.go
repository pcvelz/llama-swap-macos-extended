package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/shared"
)

func concurrencyTestReq(model string) *http.Request {
	r := httptest.NewRequest("GET", "/v1/chat/completions", nil)
	return r.WithContext(shared.SetContext(r.Context(), shared.ReqContextData{Model: model, ModelID: model}))
}

func TestServer_ConcurrencyMiddleware_QueuesOverLimit(t *testing.T) {
	cfg := config.Config{
		Models: map[string]config.ModelConfig{
			"m1": {ConcurrencyLimit: 1},
		},
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		<-release
		w.WriteHeader(http.StatusOK)
	})
	h := CreateConcurrencyMiddleware(cfg)(final)

	// First request occupies the only slot.
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		h.ServeHTTP(httptest.NewRecorder(), concurrencyTestReq("m1"))
	}()
	<-entered

	// Second concurrent request QUEUES (no 429) and completes once the slot
	// frees - over-capacity requests wait instead of bouncing.
	secondDone := make(chan struct{})
	w2 := httptest.NewRecorder()
	go func() {
		defer close(secondDone)
		h.ServeHTTP(w2, concurrencyTestReq("m1"))
	}()
	select {
	case <-secondDone:
		t.Fatal("second request completed while the slot was still held; want it queued")
	default:
	}

	close(release)
	<-firstDone
	<-secondDone
	if w2.Code != http.StatusOK {
		t.Fatalf("queued request status = %d, want 200 after slot freed", w2.Code)
	}
}

func TestServer_ConcurrencyMiddleware_CanceledWhileQueued(t *testing.T) {
	cfg := config.Config{
		Models: map[string]config.ModelConfig{
			"m1": {ConcurrencyLimit: 1},
		},
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	calls := make(chan struct{}, 8)
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls <- struct{}{}
		once.Do(func() { close(entered) })
		<-release
		w.WriteHeader(http.StatusOK)
	})
	h := CreateConcurrencyMiddleware(cfg)(final)

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		h.ServeHTTP(httptest.NewRecorder(), concurrencyTestReq("m1"))
	}()
	<-entered

	// Second request cancels while queued: it must return WITHOUT reaching
	// the inner handler.
	ctx, cancel := context.WithCancel(context.Background())
	r2 := httptest.NewRequest("GET", "/v1/chat/completions", nil)
	r2 = r2.WithContext(shared.SetContext(ctx, shared.ReqContextData{Model: "m1", ModelID: "m1"}))
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		h.ServeHTTP(httptest.NewRecorder(), r2)
	}()
	cancel()
	<-secondDone

	close(release)
	<-firstDone
	if len(calls) != 1 {
		t.Fatalf("inner handler calls = %d, want 1 (canceled request must not run)", len(calls))
	}
}

func TestServer_ConcurrencyMiddleware_CountTokensBypassesLimit(t *testing.T) {
	cfg := config.Config{
		Models: map[string]config.ModelConfig{
			"m1": {ConcurrencyLimit: 1},
		},
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		if r.URL.Path != "/v1/messages/count_tokens" {
			<-release
		}
		w.WriteHeader(http.StatusOK)
	})
	h := CreateConcurrencyMiddleware(cfg)(final)

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		h.ServeHTTP(httptest.NewRecorder(), concurrencyTestReq("m1"))
	}()
	<-entered

	// count_tokens ignores the exhausted pool entirely.
	rc := httptest.NewRequest("POST", "/v1/messages/count_tokens", nil)
	rc = rc.WithContext(shared.SetContext(rc.Context(), shared.ReqContextData{Model: "m1", ModelID: "m1"}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, rc)
	if w.Code != http.StatusOK {
		t.Fatalf("count_tokens status = %d, want 200 while pool exhausted", w.Code)
	}

	close(release)
	<-firstDone
}

func TestServer_ConcurrencyMiddleware_UnconfiguredModelPassesThrough(t *testing.T) {
	cfg := config.Config{Models: map[string]config.ModelConfig{}}

	called := 0
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		w.WriteHeader(http.StatusOK)
	})
	h := CreateConcurrencyMiddleware(cfg)(final)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, concurrencyTestReq("peer-model"))
	if w.Code != http.StatusOK || called != 1 {
		t.Fatalf("unconfigured model: status=%d called=%d, want 200/1", w.Code, called)
	}
}
