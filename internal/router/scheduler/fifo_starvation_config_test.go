package scheduler

import (
	"io"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
)

// Regression tests for llama-cm incident
// 2026-09-08-swap-grace-starvation-valve-hardcoded-evicts-hot-cq35h: the
// starvation valve in withinGrace opened the instant the resident cq35h drained
// (0s idle, swapGraceSeconds 600, model pinned) because the parked cq27
// requester had been waiting > 600s. The valve threshold was hardcoded to the
// evictee's grace; the operator had no knob to lengthen or disable it.
//
// Contract under test: the global `swapStarvationSeconds` sets the valve.
//   0 / absent -> valve opens after the evictee's grace (the historic default)
//   -1         -> valve never opens: a live resident is never evicted mid-cadence
//   N > 0      -> valve opens once the requester has waited N seconds

func newFIFOStarve(planner Swapper, eff Effects, grace map[string]time.Duration, starve int, clk *time.Time) *FIFO {
	models := make(map[string]config.ModelConfig, len(grace))
	for id, g := range grace {
		models[id] = config.ModelConfig{SwapGraceSeconds: int(g / time.Second)}
	}
	s := NewFIFO("test", logmon.NewWriter(io.Discard), planner, config.FifoConfig{}, models, eff)
	s.SetSwapStarvationSeconds(starve)
	s.now = func() time.Time { return *clk }
	return s
}

// driveContinuousA replays the 2026-07-15 shape: a serves back-to-back with a
// 2s gap so a's idle streak never reaches its grace, while b sits parked.
// Returns the clock at which b's swap started, or zero if it never did.
func driveContinuousA(s *FIFO, eff *fakeEffects, clk *time.Time, cycles int) time.Time {
	for cycle := 0; cycle < cycles; cycle++ {
		*clk = clk.Add(2 * time.Second)
		s.OnTick()
		if eff.startsFor("b") == 1 {
			return *clk
		}
		s.OnRequest(req("a"))
		*clk = clk.Add(18 * time.Second)
		s.OnServeDone(ServeDoneEvent{ModelID: "a"})
		if eff.startsFor("b") == 1 {
			return *clk
		}
	}
	return time.Time{}
}

func TestFIFO_SwapStarvationValveDisabled(t *testing.T) {
	eff := newFakeEffects()
	eff.states["a"] = process.StateReady
	eff.states["b"] = process.StateStopped
	clk := time.Unix(1000, 0)
	grace := map[string]time.Duration{"a": 60 * time.Second}
	s := newFIFOStarve(&stubPlanner{evict: map[string][]string{"b": {"a"}}}, eff, grace, -1, &clk)

	s.OnRequest(req("a"))
	s.OnServeDone(ServeDoneEvent{ModelID: "a"})
	clk = clk.Add(10 * time.Second)
	s.OnRequest(reqCh("b"))

	// 10 cycles = 200s of continuous a traffic, far past a's 60s grace.
	if at := driveContinuousA(s, eff, &clk, 10); !at.IsZero() {
		t.Fatalf("swapStarvationSeconds=-1 must never open the valve; b's swap started after %s", at.Sub(time.Unix(1010, 0)))
	}

	// Once a genuinely goes idle for its full grace, b must still be served.
	clk = clk.Add(61 * time.Second)
	s.OnTick()
	if eff.startsFor("b") != 1 {
		t.Fatalf("b must get its swap once a has been idle for its full grace; starts=%d", eff.startsFor("b"))
	}
}

func TestFIFO_SwapStarvationValveExplicitSeconds(t *testing.T) {
	eff := newFakeEffects()
	eff.states["a"] = process.StateReady
	eff.states["b"] = process.StateStopped
	clk := time.Unix(1000, 0)
	grace := map[string]time.Duration{"a": 60 * time.Second}
	// Valve at 30s: shorter than the grace, so it must open BEFORE b has waited 60s.
	s := newFIFOStarve(&stubPlanner{evict: map[string][]string{"b": {"a"}}}, eff, grace, 30, &clk)

	s.OnRequest(req("a"))
	s.OnServeDone(ServeDoneEvent{ModelID: "a"})
	clk = clk.Add(10 * time.Second)
	s.OnRequest(reqCh("b"))

	at := driveContinuousA(s, eff, &clk, 10)
	if at.IsZero() {
		t.Fatalf("swapStarvationSeconds=30 must open the valve; b never got its swap")
	}
	waited := at.Sub(time.Unix(1010, 0))
	if waited < 30*time.Second || waited >= 60*time.Second {
		t.Fatalf("valve must open once b waited >= 30s and before the 60s grace; opened after %s", waited)
	}
}

func TestFIFO_SwapStarvationValveDefaultInheritsGrace(t *testing.T) {
	eff := newFakeEffects()
	eff.states["a"] = process.StateReady
	eff.states["b"] = process.StateStopped
	clk := time.Unix(1000, 0)
	grace := map[string]time.Duration{"a": 60 * time.Second}
	s := newFIFOStarve(&stubPlanner{evict: map[string][]string{"b": {"a"}}}, eff, grace, 0, &clk)

	s.OnRequest(req("a"))
	s.OnServeDone(ServeDoneEvent{ModelID: "a"})
	clk = clk.Add(10 * time.Second)
	s.OnRequest(reqCh("b"))

	at := driveContinuousA(s, eff, &clk, 10)
	if at.IsZero() {
		t.Fatalf("swapStarvationSeconds=0 must keep the historic valve (= grace); b never got its swap")
	}
	if waited := at.Sub(time.Unix(1010, 0)); waited < 60*time.Second {
		t.Fatalf("default valve opened before the 60s grace: %s", waited)
	}
}
