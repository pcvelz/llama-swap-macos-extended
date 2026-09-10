package scheduler

import (
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/process"
)

// runningFilterPlanner is stubPlanner's evict-per-target map, but only
// returns entries that are actually in the running set handed to
// EvictionFor. Bidirectional configs (a evicts b, b evicts a) need it: plain
// stubPlanner ignores `running` entirely, so it would report BOTH directions
// as always colliding even while the "wrong" side is stopped.
type runningFilterPlanner struct {
	evict map[string][]string
}

func (p *runningFilterPlanner) EvictionFor(target string, running []string) []string {
	want := p.evict[target]
	if len(want) == 0 {
		return nil
	}
	live := make(map[string]struct{}, len(running))
	for _, m := range running {
		live[m] = struct{}{}
	}
	var out []string
	for _, w := range want {
		if _, ok := live[w]; ok {
			out = append(out, w)
		}
	}
	return out
}

func (p *runningFilterPlanner) OnSwapStart(string, []string) {}

// TestFIFO_FinishCooldown_OneShot asserts a finish is consumed by the ONE
// cooldown it ends and does not silently bypass a LATER cooldown: after b
// loads on the finished cooldown, a request back to a while b is inside ITS
// grace is held normally.
func TestFIFO_FinishCooldown_OneShot(t *testing.T) {
	eff := newFakeEffects()
	eff.states["a"] = process.StateReady
	eff.states["b"] = process.StateStopped
	clk := time.Unix(1000, 0)
	grace := map[string]time.Duration{"a": 60 * time.Second, "b": 60 * time.Second}
	s := newFIFOGrace(&runningFilterPlanner{evict: map[string][]string{"b": {"a"}, "a": {"b"}}}, eff, grace, &clk)

	s.OnRequest(req("a"))
	s.OnServeDone(ServeDoneEvent{ModelID: "a"})
	clk = clk.Add(10 * time.Second)
	s.OnRequest(reqCh("b"))
	if s.Cooldown() == nil {
		t.Fatalf("expected a cooldown on a")
	}

	s.FinishCooldown()
	s.OnTick()
	if got := eff.startsFor("b"); got != 1 {
		t.Fatalf("StartSwap(b)=%d want 1 after finish", got)
	}
	// b becomes resident and serves; a goes away.
	eff.states["a"] = process.StateStopped
	eff.states["b"] = process.StateReady
	s.OnSwapDone(SwapDone{ModelID: "b"})
	s.OnServeDone(ServeDoneEvent{ModelID: "b"})

	// A request back to a while b is inside its grace must be HELD: the
	// earlier finish was consumed.
	clk = clk.Add(5 * time.Second)
	s.OnRequest(reqCh("a"))
	if got := eff.startsFor("a"); got != 0 {
		t.Fatalf("StartSwap(a)=%d want 0: the earlier finish must not bypass b's cooldown", got)
	}
	cd := s.Cooldown()
	if cd == nil || cd.EvicteeModel != "b" || cd.NextModel != "a" {
		t.Fatalf("Cooldown()=%+v want evictee=b next=a", cd)
	}
}

// A finish that lands while nothing is held is not left armed against the
// next cooldown: the flag is only consulted while a grace is actually
// deferring, and cleared then.
func TestFIFO_FinishCooldown_IgnoredWhenNothingHeld(t *testing.T) {
	eff := newFakeEffects()
	eff.states["a"] = process.StateReady
	eff.states["b"] = process.StateStopped
	clk := time.Unix(1000, 0)
	grace := map[string]time.Duration{"a": 60 * time.Second}
	s := newFIFOGrace(&runningFilterPlanner{evict: map[string][]string{"b": {"a"}}}, eff, grace, &clk)

	s.OnRequest(req("a"))
	s.OnServeDone(ServeDoneEvent{ModelID: "a"})
	s.FinishCooldown() // nothing held yet

	// The grace runs out on its own; the stale finish must not have been
	// consumed by that (nothing to consume) and must not leak: a fresh
	// cooldown on the next idle period is honoured.
	clk = clk.Add(70 * time.Second)
	s.OnTick()
	s.OnRequest(req("a"))
	s.OnServeDone(ServeDoneEvent{ModelID: "a"})
	clk = clk.Add(5 * time.Second)
	s.OnRequest(reqCh("b"))
	if got := eff.startsFor("b"); got != 0 {
		t.Fatalf("StartSwap(b)=%d want 0: a finish clicked with nothing held must not pre-empt a later cooldown", got)
	}
}
