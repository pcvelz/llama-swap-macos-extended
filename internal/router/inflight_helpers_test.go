package router

// Shared fixtures for the admission/replay/refusal-hold tests
// (admission_replay_test.go, admission_replay_proxy_test.go,
// refusal_hold_test.go, refusal_writer_test.go). They were originally defined
// in preempt_inflight_test.go, which is parked together with the unfinished
// preemptInFlight/scheduler work; the tests that need them stay, so the
// helpers live here, in a _test file, until that work returns.

import (
	"io"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

var tierPriorityRank10 = swaputil.Tier{Name: "priority", Rank: 10, Preempts: true}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func awaitDone(t *testing.T, who string, ch <-chan struct{}, logs *syncBuf) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never finished.\nlogs:\n%s", who, logs.String())
	}
}

// newInFlightTestBase builds a router with one model and the prefill-stall
// config fixed at construction (the run loop reads the config from its first
// tick, so it must not be edited afterwards).
func newInFlightTestBase(t *testing.T, proc *fakeProcess, mc config.ModelConfig, prefill config.PrefillStallConfig, logTo io.Writer) *baseRouter {
	t.Helper()
	conf := config.Config{HealthCheckTimeout: 5, Models: map[string]config.ModelConfig{proc.id: mc}, PrefillStall: prefill}
	b, err := newBaseRouter("test", conf, map[string]process.Process{proc.id: proc}, logmon.NewWriter(logTo), &stubPlanner{})
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
