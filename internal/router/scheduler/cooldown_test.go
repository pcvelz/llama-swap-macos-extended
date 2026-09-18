package scheduler

import (
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/process"
)

// cooldownFixture: a resident "a" with a 60s swap-grace, two stopped siblings
// "b" and "c" that both evict "a", and a controllable clock. "a" has just
// served one request and gone idle at t=1000.
func cooldownFixture(t *testing.T) (*FIFO, *fakeEffects, *time.Time) {
	t.Helper()
	eff := newFakeEffects()
	eff.states["a"] = process.StateReady
	eff.states["b"] = process.StateStopped
	eff.states["c"] = process.StateStopped
	clk := time.Unix(1000, 0)
	grace := map[string]time.Duration{"a": 60 * time.Second}
	planner := &runningFilterPlanner{evict: map[string][]string{"b": {"a"}, "c": {"a"}}}
	s := newFIFOGrace(planner, eff, grace, &clk)
	s.OnRequest(req("a"))
	s.OnServeDone(ServeDoneEvent{ModelID: "a"})
	return s, eff, &clk
}

// The cooldown is a property of the RESIDENT model, not of each model that
// happens to be queued behind it. Two different models parked behind the same
// cooling resident is ONE cooldown. Witnessed 2026-09-10 as two menu rows,
// "cq35h waiting for cq35" and "cq27 waiting for cq35", both at 6:50 - an
// impossible state for a machine that cools ONE model down.
func TestFIFO_Cooldown_IsSingleton(t *testing.T) {
	s, _, clk := cooldownFixture(t)

	*clk = clk.Add(10 * time.Second)
	s.OnRequest(reqCh("b"))
	s.OnRequest(reqCh("c"))

	cd := s.Cooldown()
	if cd == nil {
		t.Fatalf("Cooldown()=nil want the single cooldown on a")
	}
	if cd.EvicteeModel != "a" {
		t.Fatalf("cooldown.EvicteeModel=%q want a", cd.EvicteeModel)
	}
	if cd.NextModel != "b" {
		t.Fatalf("cooldown.NextModel=%q want b (oldest queued cross-model request)", cd.NextModel)
	}
	if cd.Waiting != 2 {
		t.Fatalf("cooldown.Waiting=%d want 2 (every request parked behind the cooldown)", cd.Waiting)
	}
	if cd.RemainingSeconds <= 0 || cd.RemainingSeconds > 50 {
		t.Fatalf("cooldown.RemainingSeconds=%d want in (0,50]", cd.RemainingSeconds)
	}
}

// A status read (GET /slots, /props, /metrics - ConcurrencyExempt) on a model
// that is not resident must never be queued as a swap request: it neither
// starts a cooldown, nor inflates the count of what is waiting, nor can it be
// the request that wins the swap. It is refused at admission. Witnessed
// 2026-09-10: the menu-bar helper's 2s /upstream/<parked-model>/slots polls
// were most of "waiting" (5+3 shown for two real sessions).
func TestFIFO_Cooldown_ExemptStatusReadNeverQueuesSwap(t *testing.T) {
	s, eff, clk := cooldownFixture(t)

	*clk = clk.Add(10 * time.Second)
	poll := reqCh("b")
	poll.ConcurrencyExempt = true
	poll.StatusRead = true
	s.OnRequest(poll)

	if err := admitErr(t, poll); err == nil {
		t.Fatalf("exempt status read on a non-resident model must be refused at admission, got nil")
	}
	if len(s.queued) != 0 {
		t.Fatalf("exempt status read must never be queued, queue len=%d", len(s.queued))
	}
	// The resident is still idle inside its own grace (2026-09-18: the
	// no-waiter cooldown is now always published), but a status read must
	// never turn that into a "waiting for" cooldown - no NextModel, no
	// Waiting.
	if cd := s.Cooldown(); cd == nil || cd.NextModel != "" || cd.Waiting != 0 {
		t.Fatalf("a status read must not start a waiting cooldown, got %+v", cd)
	}
	if got := eff.startsFor("b"); got != 0 {
		t.Fatalf("a status read must never start a swap, StartSwap(b)=%d", got)
	}
}

// The state machine: resident serves -> cooldown runs -> cooldown expires ->
// the swap to the next model proceeds and the cooldown is gone.
func TestFIFO_Cooldown_ExpiresThenSwapProceeds(t *testing.T) {
	s, eff, clk := cooldownFixture(t)

	*clk = clk.Add(10 * time.Second)
	s.OnRequest(reqCh("b"))
	if s.Cooldown() == nil {
		t.Fatalf("expected a cooldown while a is inside its grace")
	}
	if got := eff.startsFor("b"); got != 0 {
		t.Fatalf("swap must not start during cooldown, StartSwap(b)=%d", got)
	}

	*clk = clk.Add(51 * time.Second) // 61s idle > 60s grace
	s.OnTick()

	if got := eff.startsFor("b"); got != 1 {
		t.Fatalf("swap to b must start once the cooldown expired, StartSwap(b)=%d", got)
	}
	if cd := s.Cooldown(); cd != nil {
		t.Fatalf("cooldown must be gone once the swap proceeds, got %+v", cd)
	}
}

// A request to the resident DURING the cooldown restarts it from the next
// idle moment (the resident is in use again). FinishCooldown ends it
// deliberately and the swap proceeds at the next tick.
func TestFIFO_Cooldown_ResidentUseRestarts_FinishEnds(t *testing.T) {
	s, eff, clk := cooldownFixture(t)

	*clk = clk.Add(10 * time.Second)
	s.OnRequest(reqCh("b"))
	before := s.Cooldown().RemainingSeconds

	*clk = clk.Add(20 * time.Second)
	s.OnRequest(req("a")) // resident used again
	s.OnServeDone(ServeDoneEvent{ModelID: "a"})
	after := s.Cooldown()
	if after == nil || after.RemainingSeconds <= before-20 {
		t.Fatalf("resident use must restart the cooldown: before=%d after=%+v", before, after)
	}

	s.FinishCooldown()
	s.OnTick()
	if got := eff.startsFor("b"); got != 1 {
		t.Fatalf("FinishCooldown must let the swap proceed at the next tick, StartSwap(b)=%d", got)
	}
	if cd := s.Cooldown(); cd != nil {
		t.Fatalf("cooldown must be gone after finish, got %+v", cd)
	}
}

// A no-waiter cooldown is reported when the resident is idle inside its own
// grace even though nothing cross-model is queued (2026-09-18): the model's
// slots and KV cache are hot and cooling exactly as they are with a waiter,
// so the row must show too - just without NextModel/Waiting, since there is
// no swap pending and nothing to finish.
func TestFIFO_Cooldown_NoWaiterWhileIdleInGrace(t *testing.T) {
	s, _, clk := cooldownFixture(t)
	*clk = clk.Add(10 * time.Second)
	s.OnTick() // the run-loop grace ticker's periodic re-publish, simulated
	cd := s.Cooldown()
	if cd == nil {
		t.Fatalf("Cooldown()=nil want a no-waiter cooldown while a is idle inside its grace")
	}
	if cd.EvicteeModel != "a" {
		t.Fatalf("cooldown.EvicteeModel=%q want a", cd.EvicteeModel)
	}
	if cd.NextModel != "" {
		t.Fatalf("cooldown.NextModel=%q want empty (nothing queued)", cd.NextModel)
	}
	if cd.Waiting != 0 {
		t.Fatalf("cooldown.Waiting=%d want 0 (nothing queued)", cd.Waiting)
	}
	if cd.RemainingSeconds <= 0 || cd.RemainingSeconds > 50 {
		t.Fatalf("cooldown.RemainingSeconds=%d want in (0,50]", cd.RemainingSeconds)
	}

	s.OnRequest(req("a")) // same-model request: still no waiter, cooldown restarts
	s.OnServeDone(ServeDoneEvent{ModelID: "a"})
	s.OnTick()
	if cd := s.Cooldown(); cd == nil || cd.NextModel != "" {
		t.Fatalf("Cooldown()=%+v want a no-waiter cooldown for a same-model request", cd)
	}
}

// Once the resident's grace has fully elapsed with nothing queued, there is
// nothing left to cool down: no slots/KV are being protected past that point
// (the process's own idle-TTL unload takes it from there).
func TestFIFO_Cooldown_NilOnceGraceFullyElapsed(t *testing.T) {
	s, _, clk := cooldownFixture(t)
	*clk = clk.Add(61 * time.Second) // grace is 60s
	s.OnTick()
	if cd := s.Cooldown(); cd != nil {
		t.Fatalf("Cooldown()=%+v want nil once grace has fully elapsed", cd)
	}
}

// FinishCooldown on a no-waiter cooldown is harmless: there is no queued swap
// to end, so the one-shot flag is simply dropped at the next tick and the
// no-waiter row keeps counting down on its own.
func TestFIFO_Cooldown_FinishOnNoWaiterIsHarmless(t *testing.T) {
	s, eff, clk := cooldownFixture(t)
	*clk = clk.Add(10 * time.Second)
	s.OnTick()
	if s.Cooldown() == nil {
		t.Fatalf("expected a no-waiter cooldown before FinishCooldown")
	}

	s.FinishCooldown()
	s.OnTick()

	if cd := s.Cooldown(); cd == nil || cd.EvicteeModel != "a" {
		t.Fatalf("Cooldown()=%+v want the no-waiter cooldown to survive a no-op finish", cd)
	}
	if got := eff.startsFor("b"); got != 0 {
		t.Fatalf("no swap must start, StartSwap(b)=%d", got)
	}
}
