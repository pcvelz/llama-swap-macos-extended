package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// TestBaseRouter_StatusGET_NeverPreemptsSlotHolders: a GET to a status path
// (/slots, /props, /metrics, /health — what internal/server handleUpstream
// forwards for /upstream/<model>/...) runs no inference and holds no serving
// slot, so it must be served immediately at the concurrency cap WITHOUT
// booting the lower-rank holders. llama-cm incident
// 2026-07-05-cq27-toolgap-eviction-reprefill-storm (2026-09-01 follow-up):
// every status poll
// on the main listener arrived as a rank-0 DefaultTier request, hit
// atCapacity, and tryPreempt cancelled the interactive background sessions
// mid-prefill — 146 of 153 preemptions on 2026-09-01 had such a GET within 3s.
func TestBaseRouter_StatusGET_NeverPreemptsSlotHolders(t *testing.T) {
	m1 := newFakeProcess("m1")
	m1.markReady()

	release := make(chan struct{})
	var victimBooted atomic.Bool
	m1.serveFunc = func(w http.ResponseWriter, r *http.Request, callNum int) bool {
		if r.Header.Get("X-Role") != "victim" {
			return false // status GET: default 200 "ok:m1"
		}
		select {
		case <-r.Context().Done():
			victimBooted.Store(true)
			w.WriteHeader(http.StatusBadGateway)
		case <-release:
			w.WriteHeader(http.StatusOK)
		}
		return true
	}

	conf := config.Config{HealthCheckTimeout: 5}
	conf.Models = map[string]config.ModelConfig{"m1": {ConcurrencyLimit: 1}}
	b := newTestBaseWithConfig(t, conf, map[string]process.Process{"m1": m1}, &stubPlanner{})

	victimReq := buildReplayReq("m1", "victim", replayTierBackground)
	victimRec := httptest.NewRecorder()
	victimDone := make(chan struct{})
	go func() {
		defer close(victimDone)
		b.ServeHTTP(victimRec, victimReq)
	}()
	waitSignal(t, m1.serveStarted, "victim request start")

	// Mirror handleUpstream: model pinned in the context, no body, main
	// listener tier (DefaultTier, rank 0 — strictly above background).
	getReq := httptest.NewRequest("GET", "/slots", nil)
	ctx := swaputil.SetContext(context.Background(), swaputil.ReqContextData{
		Model: "m1", ModelID: "m1", Metadata: make(map[string]string),
	})
	getReq = getReq.WithContext(swaputil.WithTier(ctx, replayTierDefault))
	getRec := httptest.NewRecorder()
	getDone := make(chan struct{})
	go func() {
		defer close(getDone)
		b.ServeHTTP(getRec, getReq)
	}()

	select {
	case <-getDone:
	case <-time.After(3 * time.Second):
		t.Fatal("status GET never completed — it parked behind the slot holder instead of bypassing the cap")
	}
	if getRec.Code != http.StatusOK || !strings.HasPrefix(getRec.Body.String(), "ok:m1") {
		t.Fatalf("status GET = %d %q, want 200 ok:m1", getRec.Code, getRec.Body.String())
	}

	// The holder must still be streaming untouched.
	close(release)
	select {
	case <-victimDone:
	case <-time.After(3 * time.Second):
		t.Fatal("victim never completed after release")
	}
	if victimBooted.Load() {
		t.Fatalf("status GET preempted the background slot holder — a slot-free request must never boot a stream")
	}
	if victimRec.Code != http.StatusOK {
		t.Fatalf("victim status = %d, want 200 (served untouched)", victimRec.Code)
	}
}
