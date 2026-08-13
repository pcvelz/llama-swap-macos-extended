package scheduler

import (
	"io"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/shared"
)

// Tiered-queue tests (docs/intent/llama-swap-tiers.md, llama-cm). These drive
// the same fakeEffects/stubPlanner harness as fifo_test.go, so a FIFO
// scheduler's tiered behavior is exercised exactly the way its untiered
// behavior already is: directly, synchronously, on the run-loop methods.

var (
	tierPriority = shared.Tier{Name: "priority", Rank: 10, Preempts: true}
	tierBg       = shared.Tier{Name: "background", Rank: -10, Preemptible: true}
	// tierSilent has a rank below the default tier but sets neither
	// Preempts nor Preemptible — used to prove a non-preemptible victim
	// survives an arrival that doesn't carry `preempts: true`.
	tierSilent = shared.Tier{Name: "silent", Rank: -5}
)

// tierReq builds a HandlerReq tagged with tier and a unique Respond channel
// (so queue/barrier logic that compares by channel identity never confuses
// two distinct requests for the same model).
func tierReq(model string, tier shared.Tier) HandlerReq {
	return HandlerReq{Model: model, Respond: make(chan HandlerResp, 1), Tier: tier}
}

// grantedTierReq is like tierReq but also wires a Preempt handle, so it can be
// granted (via the fast path) and later found by the preemption branch as a
// candidate victim. booted flips true the moment Preempt is invoked.
func grantedTierReq(model string, tier shared.Tier) (req HandlerReq, booted *bool) {
	booted = new(bool)
	req = tierReq(model, tier)
	req.Preempt = func() { *booted = true }
	return req, booted
}

// ---- Rank-ordered enqueue -------------------------------------------------

// TestFIFO_TierEnqueueOrder verifies the queue orders by tier rank DESC first,
// falling back to the existing per-model priority for ties on rank, and
// arrival (FIFO) order for ties on both.
func TestFIFO_TierEnqueueOrder(t *testing.T) {
	eff := newFakeEffects()
	for _, m := range []string{"z", "p", "d1", "d2", "b"} {
		eff.states[m] = process.StateStopped
	}
	// z's swap evicts every other model, so any request that arrives while z
	// is loading collides with z's in-flight swap and parks in the queue —
	// the same trick TestFIFO_PriorityQueueOrder uses.
	planner := &stubPlanner{evict: map[string][]string{"z": {"p", "d1", "d2", "b"}}}
	cfg := config.FifoConfig{Priority: map[string]int{"d1": 1, "d2": 5}}
	s := NewFIFO("test", logmon.NewWriter(io.Discard), planner, cfg, nil, eff)

	s.OnRequest(tierReq("z", shared.DefaultTier)) // StartSwap(z, [...])

	// Arrive out of rank order; d1 before d2 exercises the priority
	// tie-break within the same (default) rank.
	s.OnRequest(tierReq("b", tierBg))
	s.OnRequest(tierReq("d1", shared.DefaultTier))
	s.OnRequest(tierReq("d2", shared.DefaultTier))
	s.OnRequest(tierReq("p", tierPriority))

	got := make([]string, len(s.queued))
	for i, q := range s.queued {
		got[i] = q.Model
	}
	want := []string{"p", "d2", "d1", "b"}
	if len(got) != len(want) {
		t.Fatalf("queue=%v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("queue=%v want %v", got, want)
		}
	}
}

// ---- Rank barrier -----------------------------------------------------

// TestFIFO_TierRankBarrier verifies that a lower-rank arrival is never
// granted while a strictly higher-rank request is still queued, even when
// the lower-rank arrival would otherwise take the immediate fast path — and
// that it IS granted, in the same drain, once the higher-rank request stops
// blocking the queue.
func TestFIFO_TierRankBarrier(t *testing.T) {
	eff := newFakeEffects()
	eff.states["busy"] = process.StateReady
	eff.states["hi"] = process.StateReady
	eff.states["lo"] = process.StateReady
	planner := &stubPlanner{evict: map[string][]string{"hi": {"busy"}}}
	s := NewFIFO("test", logmon.NewWriter(io.Discard), planner, config.FifoConfig{}, nil, eff)

	// "busy" goes in-flight via the ordinary fast path.
	s.OnRequest(tierReq("busy", shared.DefaultTier))
	if eff.served("busy") != 1 {
		t.Fatalf("busy should be served immediately")
	}

	// "hi" (priority) needs to evict "busy", which is in-flight -> queues.
	s.OnRequest(tierReq("hi", tierPriority))
	if len(s.queued) != 1 || s.queued[0].Model != "hi" {
		t.Fatalf("queue=%v want [hi]", s.queued)
	}

	// "lo" (default) is ready with nothing to evict — normally an instant
	// fast-path grant — but "hi" is still queued at a strictly higher rank,
	// so the barrier must defer it too.
	s.OnRequest(tierReq("lo", shared.DefaultTier))
	if eff.served("lo") != 0 {
		t.Fatalf("lo must not be served while a higher-rank request is queued")
	}
	if len(s.queued) != 2 || s.queued[1].Model != "lo" {
		t.Fatalf("queue=%v want [hi lo]", s.queued)
	}

	// "busy" finishes -> hi can now proceed (starts its swap); the barrier
	// lifts in the SAME drain pass and lo is granted right after.
	s.OnServeDone(ServeDoneEvent{ModelID: "busy"})

	if got := eff.startsFor("hi"); got != 1 {
		t.Errorf("StartSwap(hi)=%d want 1", got)
	}
	if eff.served("lo") != 1 {
		t.Errorf("lo should be served once the rank barrier lifts")
	}
	if len(s.queued) != 0 {
		t.Errorf("queue=%v want empty", s.queued)
	}
}

// ---- Preemption matrix -------------------------------------------------

// TestFIFO_Preempt_DefaultBootsBackground: a default-tier arrival boots a
// running background-tier request (background is Preemptible).
func TestFIFO_Preempt_DefaultBootsBackground(t *testing.T) {
	eff := newFakeEffects()
	eff.states["bg"] = process.StateReady
	eff.states["x"] = process.StateReady
	planner := &stubPlanner{evict: map[string][]string{"x": {"bg"}}}
	s := NewFIFO("test", logmon.NewWriter(io.Discard), planner, config.FifoConfig{}, nil, eff)

	bgReq, bgBooted := grantedTierReq("bg", tierBg)
	s.OnRequest(bgReq)
	if eff.served("bg") != 1 {
		t.Fatalf("bg should be granted")
	}

	s.OnRequest(tierReq("x", shared.DefaultTier))
	if !*bgBooted {
		t.Errorf("default arrival should preempt the running background request")
	}
	if len(s.queued) != 1 || s.queued[0].Model != "x" {
		t.Fatalf("queue=%v want [x] (preemption is a cancel, not a synchronous release)", s.queued)
	}

	// The booted request's HTTP handler eventually aborts and reports
	// ServeDone, just like any other exit path from trackedServe.
	s.OnServeDone(ServeDoneEvent{ModelID: "bg"})
	if got := eff.startsFor("x"); got != 1 {
		t.Errorf("StartSwap(x)=%d want 1 once the preempted victim clears", got)
	}
}

// TestFIFO_Preempt_PriorityBootsBackgroundAndDefault: a priority-tier arrival
// (preempts: true) boots BOTH a running background request (preemptible) and
// a running default request (not preemptible on its own, but preempts:true
// overrides that).
func TestFIFO_Preempt_PriorityBootsBackgroundAndDefault(t *testing.T) {
	eff := newFakeEffects()
	eff.states["bg"] = process.StateReady
	eff.states["def"] = process.StateReady
	eff.states["y"] = process.StateReady
	planner := &stubPlanner{evict: map[string][]string{"y": {"bg", "def"}}}
	s := NewFIFO("test", logmon.NewWriter(io.Discard), planner, config.FifoConfig{}, nil, eff)

	bgReq, bgBooted := grantedTierReq("bg", tierBg)
	defReq, defBooted := grantedTierReq("def", shared.DefaultTier)
	s.OnRequest(bgReq)
	s.OnRequest(defReq)
	if eff.served("bg") != 1 || eff.served("def") != 1 {
		t.Fatalf("bg and def should both be granted")
	}

	s.OnRequest(tierReq("y", tierPriority))
	if !*bgBooted {
		t.Errorf("priority arrival should preempt the running background request")
	}
	if !*defBooted {
		t.Errorf("priority arrival (preempts:true) should also preempt the running default request")
	}
}

// TestFIFO_Preempt_NeverCrossesEqualRank: preemption never crosses equal
// ranks, even when both sides carry preempts/preemptible flags.
func TestFIFO_Preempt_NeverCrossesEqualRank(t *testing.T) {
	eff := newFakeEffects()
	eff.states["p1"] = process.StateReady
	eff.states["p2"] = process.StateReady
	planner := &stubPlanner{evict: map[string][]string{"p2": {"p1"}}}
	s := NewFIFO("test", logmon.NewWriter(io.Discard), planner, config.FifoConfig{}, nil, eff)

	victimTier := shared.Tier{Name: "priority", Rank: 10, Preempts: true, Preemptible: true}
	p1Req, p1Booted := grantedTierReq("p1", victimTier)
	s.OnRequest(p1Req)
	if eff.served("p1") != 1 {
		t.Fatalf("p1 should be granted")
	}

	s.OnRequest(tierReq("p2", shared.Tier{Name: "priority", Rank: 10, Preempts: true}))
	if *p1Booted {
		t.Errorf("preemption must never cross equal ranks")
	}
	if got := eff.startsFor("p2"); got != 0 {
		t.Errorf("StartSwap(p2)=%d want 0 (p1 was never booted, so p2 stays blocked)", got)
	}
}

// TestFIFO_Preempt_NonPreemptibleVictimSurvives: a victim that is neither
// preemptible nor targeted by a preempts:true arrival is never booted, even
// when its rank is strictly lower than the arrival's.
func TestFIFO_Preempt_NonPreemptibleVictimSurvives(t *testing.T) {
	eff := newFakeEffects()
	eff.states["s1"] = process.StateReady
	eff.states["d1"] = process.StateReady
	planner := &stubPlanner{evict: map[string][]string{"d1": {"s1"}}}
	s := NewFIFO("test", logmon.NewWriter(io.Discard), planner, config.FifoConfig{}, nil, eff)

	s1Req, s1Booted := grantedTierReq("s1", tierSilent) // rank -5, no preemptible flag
	s.OnRequest(s1Req)
	if eff.served("s1") != 1 {
		t.Fatalf("s1 should be granted")
	}

	// default (rank 0, no preempts flag) arrives; s1's rank (-5) is strictly
	// lower, but neither side opts in to preemption.
	s.OnRequest(tierReq("d1", shared.DefaultTier))
	if *s1Booted {
		t.Errorf("a non-preemptible victim must survive a non-preempts arrival")
	}
	if got := eff.startsFor("d1"); got != 0 {
		t.Errorf("StartSwap(d1)=%d want 0 (still blocked by the surviving victim)", got)
	}
}

// TestFIFO_Preempt_SameModelKVParked: preemption also fires on the KV-admission
// branch, where the evict set is empty because arrival and victim share ONE
// model — the headline tier scenario (a background session hogging the model a
// priority dispatch needs). Without same-model preemption a higher-rank
// arrival parked by kvAdmit could never boot the lower-rank request holding
// the pool.
func TestFIFO_Preempt_SameModelKVParked(t *testing.T) {
	eff := newFakeEffects()
	eff.states["m"] = process.StateReady
	planner := &stubPlanner{}
	cfg := config.FifoConfig{KVPoolTokens: map[string]int{"m": 100}}
	s := NewFIFO("test", logmon.NewWriter(io.Discard), planner, cfg, nil, eff)

	bgReq, bgBooted := grantedTierReq("m", tierBg)
	bgReq.EstimatedTokens = 80
	s.OnRequest(bgReq)
	if eff.served("m") != 1 {
		t.Fatalf("bg request should be granted (80 <= 100 pool)")
	}

	prioReq := tierReq("m", tierPriority)
	prioReq.EstimatedTokens = 80
	s.OnRequest(prioReq)
	if !*bgBooted {
		t.Errorf("priority arrival parked by kvAdmit should preempt the same-model background request")
	}
	if len(s.queued) != 1 {
		t.Fatalf("queue=%v want the parked priority request (cancel is async)", s.queued)
	}

	// Victim's handler aborts -> ServeDone releases its KV reservation -> the
	// parked priority request is granted in the drain.
	s.OnServeDone(ServeDoneEvent{ModelID: "m", EstimatedTokens: 80})
	if eff.served("m") != 2 {
		t.Errorf("served(m)=%d want 2 once the preempted victim releases the pool", eff.served("m"))
	}
	if len(s.queued) != 0 {
		t.Errorf("queue=%v want empty", s.queued)
	}
}
