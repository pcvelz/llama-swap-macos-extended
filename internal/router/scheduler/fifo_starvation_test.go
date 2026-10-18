package scheduler

import (
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/process"
)

// The starvation valve was removed 2026-10-18 (llama-cm docs/intent/cooldown.md:
// a cooldown ends only by the menu finish or a higher-rank arrival), so the old
// valve tests are gone. What stays is the other half of the contract: a
// cooldown is not eternal - once the resident has genuinely been idle for its
// full grace, the parked swap proceeds on its own, with no finish and no valve.
func TestFIFO_SwapGraceExpiresAfterFullIdleGrace(t *testing.T) {
	eff := newFakeEffects()
	eff.states["a"] = process.StateReady
	eff.states["b"] = process.StateStopped
	clk := time.Unix(1000, 0)
	grace := map[string]time.Duration{"a": 60 * time.Second}
	s := newFIFOGrace(&stubPlanner{evict: map[string][]string{"b": {"a"}}}, eff, grace, &clk)

	s.OnRequest(req("a"))
	s.OnServeDone(ServeDoneEvent{ModelID: "a"})
	clk = clk.Add(10 * time.Second)
	s.OnRequest(reqCh("b"))

	// 10 cycles = 200s of continuous a traffic, far past a's 60s grace.
	if at := driveContinuousA(s, eff, &clk, 10); !at.IsZero() {
		t.Fatalf("b's swap started %s into continuous a traffic", at.Sub(time.Unix(1010, 0)))
	}

	// Once a genuinely goes idle for its full grace, b must be served.
	clk = clk.Add(61 * time.Second)
	s.OnTick()
	if eff.startsFor("b") != 1 {
		t.Fatalf("b must get its swap once a has been idle for its full grace; starts=%d", eff.startsFor("b"))
	}
}
