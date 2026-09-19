package router

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/membrake"
	"github.com/mostlygeek/llama-swap/internal/process"
)

// After a memory brake event, a load of a local model is PARKED until the
// brake opens the gate (file-backed drained, so swap-grace, the kv-admission
// parker or a waiting client cannot reload straight into the killed model's
// leftover file cache), and then proceeds normally - it is never failed.
func TestMemoryBrakeHoldParksLoadUntilExpiry(t *testing.T) {
	old := membrake.DefaultHold
	defer func() { membrake.DefaultHold = old }()
	membrake.DefaultHold = &membrake.Hold{}
	hold := 400 * time.Millisecond
	membrake.DefaultHold.Set(&membrake.Event{}, 10<<30)
	time.AfterFunc(hold, membrake.DefaultHold.Release)

	b := newFakeProcess("b")
	b.autoReady = true
	conf := config.Config{
		HealthCheckTimeout: 5,
		Routing: groupRouting(map[string]config.GroupConfig{
			"g": {Swap: true, Exclusive: true, Members: []string{"b"}},
		}),
	}
	g := newTestGroup(t, conf, map[string]process.Process{"b": b})

	start := time.Now()
	done := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		g.ServeHTTP(w, newRequest("b"))
		done <- w.Code
	}()

	time.Sleep(hold / 2)
	if n := b.runCalls.Load(); n != 0 {
		t.Fatalf("model was started during the memory brake hold (runCalls=%d)", n)
	}

	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("status=%d after the hold, want 200", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request never completed after the hold expired")
	}
	if el := time.Since(start); el < hold-50*time.Millisecond {
		t.Fatalf("load proceeded after %v, before the %v hold expired", el, hold)
	}
	if n := b.runCalls.Load(); n != 1 {
		t.Fatalf("runCalls=%d after the hold, want 1", n)
	}
}
