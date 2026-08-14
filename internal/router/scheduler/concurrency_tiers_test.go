package scheduler

import (
	"sync/atomic"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// These cover the tier-aware side of the per-model concurrency cap. They were
// re-homed from internal/server/concurrency_tiers_test.go when upstream moved
// the concurrency limits out of the server middleware and into the scheduler
// (upstream #849): the "Known soft spot" in docs/intent/llama-swap-tiers.md was
// that same-model contention — the dominant production case — used to be
// resolved by a rank-blind semaphore. It is now resolved here, by the same
// swaputil.CanPreempt rule the eviction-driven preemption branch uses.

// tieredReq builds an admitted-style HandlerReq carrying a tier and a preempt
// handle, the way baseRouter.ServeHTTP does for a real request.
func tieredReq(model string, tier swaputil.Tier) (HandlerReq, *atomic.Bool) {
	r := req(model)
	preempted := new(atomic.Bool)
	r.Tier = tier
	r.Preempted = preempted
	r.Preempt = func() { preempted.Store(true) }
	return r, preempted
}

// (a) A priority-tier arrival preempts a background-tier holder at
// concurrencyLimit 1: the holder's Preempted flag is set (its serving context
// is cancelled in production), and the arrival parks until the victim's
// OnServeDone frees the slot.
func TestFIFO_ConcurrencyTiers_PriorityPreemptsBackgroundHolder(t *testing.T) {
	s, eff := newFIFOWithLimit(t, "m1", 1)

	background := swaputil.Tier{Name: "background", Rank: -10, Preemptible: true}
	priority := swaputil.Tier{Name: "priority", Rank: 10, Preempts: true}

	bg, bgPreempted := tieredReq("m1", background)
	s.OnRequest(bg)
	if got := eff.served("m1"); got != 1 {
		t.Fatalf("served(m1)=%d want 1", got)
	}
	if bgPreempted.Load() {
		t.Fatal("background holder preempted before any priority arrival")
	}

	pr, _ := tieredReq("m1", priority)
	s.OnRequest(pr)
	assertParkedAtCapacity(t, s, pr)
	if !bgPreempted.Load() {
		t.Fatal("background holder was not preempted by the higher-rank arrival")
	}

	// The victim's handler unwinds; its slot frees and the arrival is served.
	s.OnServeDone(ServeDoneEvent{ModelID: "m1"})
	if got := eff.served("m1"); got != 2 {
		t.Fatalf("served(m1)=%d want 2 once the preempted holder released its slot", got)
	}
}

// (b) An equal-rank arrival must NOT preempt: the holder keeps running and the
// arrival waits for the slot the ordinary way.
func TestFIFO_ConcurrencyTiers_EqualRankDoesNotPreempt(t *testing.T) {
	s, eff := newFIFOWithLimit(t, "m1", 1)

	// Preempts:true does not matter here — equal ranks never cross, per
	// swaputil.CanPreempt.
	priority := swaputil.Tier{Name: "priority", Rank: 10, Preempts: true}

	first, firstPreempted := tieredReq("m1", priority)
	s.OnRequest(first)
	if got := eff.served("m1"); got != 1 {
		t.Fatalf("served(m1)=%d want 1", got)
	}

	second, _ := tieredReq("m1", priority)
	s.OnRequest(second)
	assertParkedAtCapacity(t, s, second)
	if firstPreempted.Load() {
		t.Fatal("equal-rank arrival preempted the holder; must never cross equal ranks")
	}
	if got := eff.served("m1"); got != 1 {
		t.Fatalf("served(m1)=%d want 1 while the slot is still held", got)
	}

	s.OnServeDone(ServeDoneEvent{ModelID: "m1"})
	if got := eff.served("m1"); got != 2 {
		t.Fatalf("served(m1)=%d want 2 after the slot freed normally", got)
	}
}

// (c) Zero-config (default-tier only) behaviour is unchanged: two DefaultTier
// requests never preempt each other; the second simply waits.
func TestFIFO_ConcurrencyTiers_DefaultTierOnlyUnchanged(t *testing.T) {
	s, eff := newFIFOWithLimit(t, "m1", 1)

	first, firstPreempted := tieredReq("m1", swaputil.DefaultTier)
	s.OnRequest(first)

	second, _ := tieredReq("m1", swaputil.DefaultTier)
	s.OnRequest(second)
	assertParkedAtCapacity(t, s, second)

	if firstPreempted.Load() {
		t.Fatal("default-tier arrival preempted another default-tier holder; zero-config must never preempt")
	}

	s.OnServeDone(ServeDoneEvent{ModelID: "m1"})
	if got := eff.served("m1"); got != 2 {
		t.Fatalf("served(m1)=%d want 2 after the slot freed", got)
	}
}
