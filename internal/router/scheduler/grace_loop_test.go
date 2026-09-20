package scheduler

import (
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/process"
)

// loopFixture is the production shape of the 2026-09-19 incident: resident "a"
// with a 300s swap-grace, a stopped "b" that evicts it, and the starvation
// valve OFF (-1) exactly as the live config has it. "a" has just served one
// ordinary request and gone idle at t=1000.
func loopFixture(t *testing.T) (*FIFO, *fakeEffects, *time.Time) {
	t.Helper()
	eff := newFakeEffects()
	eff.states["a"] = process.StateReady
	eff.states["b"] = process.StateStopped
	clk := time.Unix(1000, 0)
	grace := map[string]time.Duration{"a": 300 * time.Second}
	s := newFIFOGrace(&runningFilterPlanner{evict: map[string][]string{"b": {"a"}}}, eff, grace, &clk)
	s.SetSwapStarvationSeconds(-1) // valve OFF, as in production
	s.OnRequest(req("a"))
	s.OnServeDone(ServeDoneEvent{ModelID: "a"})
	return s, eff, &clk
}

// The 2026-09-19 fix: a completion from a session in a degenerate loop does
// NOT restart the resident's swap-grace clock. Session 934159af sat in one
// turn for 4.5h at a ~20s cadence, resetting a's idle clock forever, while
// sessions parked for another model starved for hours. With the loop verdict
// the existing idle reference is left alone, so the grace runs down and the
// swap proceeds at the next drain gap.
func TestFIFO_LoopingCompletionsDoNotRefreshGrace(t *testing.T) {
	s, eff, clk := loopFixture(t)

	// b arrives 10s in: inside a's grace, deferred.
	*clk = clk.Add(10 * time.Second)
	s.OnRequest(reqCh("b"))
	if got := eff.startsFor("b"); got != 0 {
		t.Fatalf("b should be deferred inside a's grace; got %d swap starts", got)
	}

	// The loop: a request on a every 20s, each completion flagged Looping.
	for cycle := 0; cycle < 40 && eff.startsFor("b") == 0; cycle++ {
		*clk = clk.Add(2 * time.Second)
		s.OnTick()
		s.OnRequest(req("a"))
		*clk = clk.Add(18 * time.Second)
		s.OnServeDone(ServeDoneEvent{ModelID: "a", Looping: true})
	}

	if got := eff.startsFor("b"); got != 1 {
		t.Fatalf("STARVATION: b never got its swap under a looping session (starts=%d, waited %s)",
			got, clk.Sub(time.Unix(1010, 0)))
	}
	// Not EARLY either: the resident keeps its full 300s of protection,
	// measured from the last non-looping idle mark at t=1000.
	if elapsed := clk.Sub(time.Unix(1000, 0)); elapsed < 300*time.Second {
		t.Fatalf("swap started after only %s; a's 300s grace must still be honoured in full", elapsed)
	}
	last := eff.starts[len(eff.starts)-1]
	if !containsString(last.evict, "a") {
		t.Fatalf("swap for b should evict a; got evict=%v", last.evict)
	}
}

// The control, and the thing that must never regress: the SAME cadence with
// Looping=false keeps deferring forever with the valve off. That is the
// 2026-09-08 protection (a live interactive session was swapped out by the
// blunt valve), and the loop verdict is the only thing allowed to bypass it.
func TestFIFO_ProductiveCompletionsKeepFullProtection(t *testing.T) {
	s, eff, clk := loopFixture(t)

	*clk = clk.Add(10 * time.Second)
	s.OnRequest(reqCh("b"))

	for cycle := 0; cycle < 40; cycle++ {
		*clk = clk.Add(2 * time.Second)
		s.OnTick()
		s.OnRequest(req("a"))
		*clk = clk.Add(18 * time.Second)
		s.OnServeDone(ServeDoneEvent{ModelID: "a"})
	}

	if got := eff.startsFor("b"); got != 0 {
		t.Fatalf("a productive session at a %s-wide cadence was evicted after %s (starts=%d): the 2026-09-08 protection is gone",
			20*time.Second, clk.Sub(time.Unix(1000, 0)), got)
	}
}

// The cooldown snapshot derives from idleSince, so with the clock frozen by a
// looping session it must simply COUNT DOWN rather than sit at the full grace.
func TestFIFO_CooldownCountsDownUnderALoop(t *testing.T) {
	s, _, clk := loopFixture(t)

	*clk = clk.Add(10 * time.Second)
	s.OnRequest(reqCh("b"))
	cd := s.Cooldown()
	if cd == nil || cd.EvicteeModel != "a" || cd.NextModel != "b" {
		t.Fatalf("Cooldown()=%+v want evictee=a next=b", cd)
	}
	if cd.RemainingSeconds != 290 {
		t.Fatalf("RemainingSeconds=%d want 290", cd.RemainingSeconds)
	}

	// One looping turn later the countdown has advanced by the wall time,
	// not been reset by the completion.
	*clk = clk.Add(2 * time.Second)
	s.OnTick()
	s.OnRequest(req("a"))
	*clk = clk.Add(18 * time.Second)
	s.OnServeDone(ServeDoneEvent{ModelID: "a", Looping: true})

	cd = s.Cooldown()
	if cd == nil || cd.RemainingSeconds != 270 {
		t.Fatalf("Cooldown()=%+v want RemainingSeconds=270 after 30s of looping traffic", cd)
	}
}

// A looping completion on a model with NO idle reference yet must still set
// one: withinGrace treats a model without an idleSince entry as evictable, so
// skipping the write would drop the protection entirely instead of freezing
// it.
func TestFIFO_LoopingCompletionStillSeedsIdleSince(t *testing.T) {
	eff := newFakeEffects()
	eff.states["a"] = process.StateReady
	eff.states["b"] = process.StateStopped
	clk := time.Unix(1000, 0)
	grace := map[string]time.Duration{"a": 300 * time.Second}
	s := newFIFOGrace(&runningFilterPlanner{evict: map[string][]string{"b": {"a"}}}, eff, grace, &clk)
	s.SetSwapStarvationSeconds(-1)

	// No OnSwapDone and no earlier serve: a has never been marked idle.
	delete(s.idleSince, "a")
	s.OnRequest(req("a"))
	s.OnServeDone(ServeDoneEvent{ModelID: "a", Looping: true})

	if _, ok := s.idleSince["a"]; !ok {
		t.Fatalf("a ready model must never be left without an idle reference")
	}
	clk = clk.Add(10 * time.Second)
	s.OnRequest(reqCh("b"))
	if got := eff.startsFor("b"); got != 0 {
		t.Fatalf("StartSwap(b)=%d want 0: a's grace must hold from the seeded idle mark", got)
	}
}
