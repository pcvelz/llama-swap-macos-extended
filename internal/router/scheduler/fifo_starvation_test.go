package scheduler

import (
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/process"
)

// Regression test for the 2026-07-15 production starvation (llama-cm incident
// 2026-07-07-llamaswap-drain-wedge-empty-200, evening follow-up): swap-grace is
// an idle-STREAK requirement, so a continuous consumer of the resident model
// (request cadence < grace) resets the idle clock forever and a parked
// cross-model request never gets its swap — witnessed live: a ~20s-cadence
// /v1/messages client pinned cq27 (grace 600s) indefinitely while a cq35
// request sat parked 13+ minutes until manually flushed.
//
// Contract under test (the starvation valve): once a deferred request has
// itself waited >= the evictee's grace, the grace is deemed served — the
// resident model got its full protection window — and the swap must proceed at
// the next drain gap, regardless of how recently the evictee served traffic.
func TestFIFO_SwapGraceStarvationValve(t *testing.T) {
	eff := newFakeEffects()
	eff.states["a"] = process.StateReady
	eff.states["b"] = process.StateStopped
	clk := time.Unix(1000, 0)
	grace := map[string]time.Duration{"a": 60 * time.Second}
	s := newFIFOGrace(&stubPlanner{evict: map[string][]string{"b": {"a"}}}, eff, grace, &clk)

	// a serves and finishes — idle clock starts.
	s.OnRequest(req("a"))
	s.OnServeDone(ServeDoneEvent{ModelID: "a"})

	// b arrives 10s later: within a's grace — deferred.
	clk = clk.Add(10 * time.Second)
	s.OnRequest(reqCh("b"))
	if got := eff.startsFor("b"); got != 0 {
		t.Fatalf("b should be deferred within a's grace; got %d swap starts", got)
	}

	// Continuous a traffic: a new ~18s request every 20s. Each completion
	// resets a's idle clock, so a's idle streak NEVER reaches 60s.
	// b keeps waiting; after b has waited >= 60s (a's grace), the valve must
	// open and the next drain gap must start b's swap.
	deadlineReached := false
	for cycle := 0; cycle < 10; cycle++ {
		clk = clk.Add(2 * time.Second) // idle gap between a requests
		s.OnTick()
		if eff.startsFor("b") == 1 {
			deadlineReached = true
			break
		}
		s.OnRequest(req("a")) // next a request (fast path, a busy again)
		clk = clk.Add(18 * time.Second)
		s.OnServeDone(ServeDoneEvent{ModelID: "a"}) // a idle again, clock reset
		if eff.startsFor("b") == 1 {
			deadlineReached = true
			break
		}
	}

	if !deadlineReached {
		t.Fatalf("STARVATION: b waited %s (grace is 60s) under continuous a traffic and never got its swap",
			clk.Sub(time.Unix(1010, 0)))
	}

	// The valve must not fire EARLY: b's swap may start only once b has waited
	// at least a's full grace (60s) since its first deferral at t0+10.
	waited := clk.Sub(time.Unix(1010, 0))
	if waited < 60*time.Second {
		t.Fatalf("valve opened too early: b waited only %s (< 60s grace)", waited)
	}
	last := eff.starts[len(eff.starts)-1]
	if !containsString(last.evict, "a") {
		t.Fatalf("swap for b should evict a; got evict=%v", last.evict)
	}
}
