package scheduler

import (
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// graceRankFixture: resident "a" with a 60s swap-grace, two stopped siblings
// "b" and "c" that both evict "a", and a controllable clock. "a" has just
// served one request and gone idle at t=1000. The fixture grants a default-tier
// request to "a" so lastServedTier["a"] is set, mirroring the common case where
// the resident has been serving before entering grace.
func graceRankFixture(t *testing.T) (*FIFO, *fakeEffects, *time.Time) {
	t.Helper()
	eff := newFakeEffects()
	eff.states["a"] = process.StateReady
	eff.states["b"] = process.StateStopped
	eff.states["c"] = process.StateStopped
	clk := time.Unix(1000, 0)
	grace := map[string]time.Duration{"a": 60 * time.Second}
	planner := &runningFilterPlanner{evict: map[string][]string{"b": {"a"}, "c": {"a"}}}
	s := newFIFOGrace(planner, eff, grace, &clk)

	// Grant a default-tier request to "a" so the fixture has a lastServedTier.
	aReq := tierReq("a", swaputil.DefaultTier)
	s.OnRequest(aReq)
	s.OnServeDone(ServeDoneEvent{ModelID: "a"})
	return s, eff, &clk
}

// noLastServedFixture is like graceRankFixture but skips the initial grant so
// lastServedTier["a"] is never recorded — testing case (d). The model is
// pre-set as ready with idleSince already advanced, simulating a freshly
// swapped-in process that has not yet served a request.
func noLastServedFixture(t *testing.T) (*FIFO, *fakeEffects, *time.Time) {
	t.Helper()
	eff := newFakeEffects()
	eff.states["a"] = process.StateReady
	eff.states["b"] = process.StateStopped
	eff.states["c"] = process.StateStopped
	clk := time.Unix(1000, 0)
	grace := map[string]time.Duration{"a": 60 * time.Second}
	planner := &runningFilterPlanner{evict: map[string][]string{"b": {"a"}, "c": {"a"}}}
	s := newFIFOGrace(planner, eff, grace, &clk)

	// Pre-set idleSince so the model appears to have just gone idle at t=1000,
	// but do NOT call grantHandler (which would record lastServedTier). This
	// simulates a freshly swapped-in process that has not yet served any request.
	s.idleSince["a"] = clk

	return s, eff, &clk
}

// (a) priority arrival (rank 10, preempts:true) vs default last-served -> grace
// broken, swap starts immediately. User ruling 2026-09-14: rank must be TOTAL.
func TestFIFO_Grace_PriorityBreaksDefaultGrace(t *testing.T) {
	s, eff, clk := graceRankFixture(t)

	*clk = clk.Add(10 * time.Second)
	s.OnRequest(tierReq("b", tierBg))
	if s.Cooldown() == nil {
		t.Fatalf("expected a cooldown on a while the low-rank b waits")
	}
	if got := eff.startsFor("b"); got != 0 {
		t.Fatalf("low-rank b must not start during the cooldown, StartSwap(b)=%d", got)
	}

	// Priority arrival for a different evictee: breaks grace immediately.
	s.OnRequest(tierReq("c", tierPriority))
	if got := eff.startsFor("c"); got != 1 {
		t.Fatalf("priority c must start immediately (grace broken), StartSwap(c)=%d", got)
	}
}

// (b) default arrival (rank 0, preempts:false) vs background last-served
// (rank -10, preemptible:true) -> grace HOLDS. This is the cq35h regression
// guard: a non-preempting arrival must not break grace even if its rank is
// strictly above the last-served tier. swapStarvationSeconds:-1 protects this.
func TestFIFO_Grace_DefaultHoldsAgainstBackgroundGrace(t *testing.T) {
	s, eff, clk := graceRankFixture(t)

	*clk = clk.Add(10 * time.Second)
	s.OnRequest(tierReq("b", tierBg))
	if s.Cooldown() == nil {
		t.Fatalf("expected a cooldown on a while the low-rank b waits")
	}

	// Default-tier arrival: rank 0 > -10 but Preempts:false -> grace HOLDS.
	s.OnRequest(tierReq("c", swaputil.DefaultTier))
	if got := eff.startsFor("c"); got != 0 {
		t.Fatalf("default c must hold against background grace (Preempts:false), StartSwap(c)=%d", got)
	}

	// Proceeds once the grace elapses.
	*clk = clk.Add(51 * time.Second)
	s.OnTick()
	if got := eff.startsFor("c"); got != 1 {
		t.Fatalf("c must start once grace expired, StartSwap(c)=%d", got)
	}
}

// (c) equal-rank arrival -> grace holds. Within one rank, FIFO order and the
// resident's grace both stand.
func TestFIFO_Grace_EqualRankHolds(t *testing.T) {
	s, eff, clk := graceRankFixture(t)

	*clk = clk.Add(10 * time.Second)
	// Last-served was default (rank 0). A mid-rank arrival outranks it.
	// But a same-rank arrival as the queued evictee does not break grace.
	s.OnRequest(tierReq("b", swaputil.Tier{Name: "mid", Rank: 5}))
	if got := eff.startsFor("b"); got != 0 {
		t.Fatalf("b must not start during cooldown, StartSwap(b)=%d", got)
	}

	// Same-rank c: rank not strictly greater -> grace holds.
	s.OnRequest(tierReq("c", swaputil.Tier{Name: "mid", Rank: 5}))
	if got := eff.startsFor("c"); got != 0 {
		t.Fatalf("same-rank c must hold, StartSwap(c)=%d", got)
	}

	// Grace elapses: b goes first (FIFO within rank).
	*clk = clk.Add(51 * time.Second)
	s.OnTick()
	if got := eff.startsFor("b"); got != 1 {
		t.Fatalf("b must start first when grace expires, StartSwap(b)=%d", got)
	}
}

// (c-lower) lower-rank arrival -> grace holds. A background request arriving
// behind a default-tier cooldown must not break it.
func TestFIFO_Grace_LowerRankHolds(t *testing.T) {
	s, eff, clk := graceRankFixture(t)

	*clk = clk.Add(10 * time.Second)
	s.OnRequest(tierReq("b", swaputil.Tier{Name: "mid", Rank: 5}))
	if got := eff.startsFor("b"); got != 0 {
		t.Fatalf("b must not start during cooldown, StartSwap(b)=%d", got)
	}

	// Background arrival (rank -10) vs default last-served (rank 0):
	// rank is LOWER -> grace holds regardless of Preempts.
	s.OnRequest(tierReq("c", tierBg))
	if got := eff.startsFor("c"); got != 0 {
		t.Fatalf("lower-rank c must hold, StartSwap(c)=%d", got)
	}
}

// (d) no lastServedTier recorded -> grace holds. A freshly swapped-in model
// that has not yet served a real request should not have its grace broken.
func TestFIFO_Grace_NoLastServedHolds(t *testing.T) {
	s, eff, clk := noLastServedFixture(t)

	*clk = clk.Add(10 * time.Second)
	// Priority arrival: no lastServedTier for "a" -> grace holds.
	s.OnRequest(tierReq("b", tierPriority))
	if got := eff.startsFor("b"); got != 0 {
		t.Fatalf("priority b must hold when no lastServedTier, StartSwap(b)=%d", got)
	}

	// Proceeds once the grace elapses.
	*clk = clk.Add(51 * time.Second)
	s.OnTick()
	if got := eff.startsFor("b"); got != 1 {
		t.Fatalf("b must start once grace expired, StartSwap(b)=%d", got)
	}
}

// DrainQueue path: a request queued by the rank barrier re-runs deferredByGrace
// on the next drain against the live lastServedTier state. A lower-rank evictee
// that served after the original decision still causes an overtake; one that has
// been replaced does not.
func TestFIFO_Grace_DrainQueueRereadsLiveState(t *testing.T) {
	s, eff, clk := graceRankFixture(t)

	*clk = clk.Add(10 * time.Second)
	// Low-rank b parks behind the resident's cooldown.
	s.OnRequest(tierReq("b", tierBg))
	if s.Cooldown() == nil {
		t.Fatalf("expected a cooldown on a while the low-rank b waits")
	}

	// A top-rank request to "a" itself arrives: fast-path grant, makes the
	// resident busy again. It finishes immediately, updating lastServedTier["a"]
	// to priority tier and restarting a fresh idle streak (and grace).
	s.OnRequest(tierReq("a", tierPriority))
	if got := eff.served("a"); got != 2 { // fixture request + this one
		t.Fatalf("top-rank a must be granted fast-path, served(a)=%d", got)
	}
	s.OnServeDone(ServeDoneEvent{ModelID: "a"})

	// Now a top-rank b arrives while the resident is idle inside its FRESH
	// grace with lastServedTier["a"] = priority: since b IS priority tier,
	// rank is NOT strictly greater -> grace HOLDS.
	s.OnRequest(tierReq("b", tierPriority))
	if got := eff.startsFor("b"); got != 0 {
		t.Fatalf("priority b vs priority last-served must hold (equal rank), StartSwap(b)=%d", got)
	}
}

// The same arrival with NO lower-rank traffic queued behind the resident is
// still held by the grace: lastServedTier["a"] = default (rank 0), and the
// priority arrival has rank 10 > 0 with Preempts:true — so grace breaks.
// A freshly swapped-in model with no lower-rank traffic in the queue should
// not hold a higher-prio request (no cooldown "in effect"). The swap proceeds
// immediately, and once it completes the idle clock resets for whatever comes next.
func TestFIFO_Grace_HeldWhenNoLowerRankQueued(t *testing.T) {
	s, eff, clk := graceRankFixture(t)

	*clk = clk.Add(10 * time.Second)
	s.OnRequest(tierReq("c", tierPriority))
	if got := eff.startsFor("c"); got != 1 {
		t.Fatalf("high-rank c must start immediately (no queued traffic to protect), StartSwap(c)=%d", got)
	}

	// The cooldown is gone because the swap started.
	if cd := s.Cooldown(); cd != nil {
		t.Fatalf("expected no cooldown once c's swap started, got %+v", cd)
	}
}

// DrainQueueHonoursGraceAfterOvertake: a request queued by the rank barrier
// re-runs deferredByGrace on the next drain. If the higher-rank waiter has
// overtaken and started its swap by then, lastServedTier reflects the new
// resident's tier — so the barrier-blocked request must honour that grace.
func TestFIFO_Grace_DrainQueueHonoursGraceAfterOvertake(t *testing.T) {
	s, eff, clk := graceRankFixture(t)

	*clk = clk.Add(10 * time.Second)
	// Low-rank b parks behind the resident's cooldown.
	s.OnRequest(tierReq("b", tierBg))
	if s.Cooldown() == nil {
		t.Fatalf("expected a cooldown on a while the low-rank b waits")
	}

	// Same-rank c arrives: held by the grace (same rank is not strictly lower,
	// so no override), queued behind b in FIFO order.
	s.OnRequest(tierReq("c", tierBg))
	if got := eff.startsFor("c"); got != 0 {
		t.Fatalf("same-rank c must not overtake b's hold, StartSwap(c)=%d", got)
	}

	// A request to the resident forces a drain: it is served fast-path and its
	// completion updates lastServedTier["a"] to priority. The drain re-runs the
	// decision tree for b and c against the live state: both same rank, grace
	// still holding (fresh idle streak).
	s.OnRequest(tierReq("a", tierPriority))
	if got := eff.served("a"); got != 2 {
		t.Fatalf("top-rank a must be granted fast-path, served(a)=%d", got)
	}
	s.OnServeDone(ServeDoneEvent{ModelID: "a"})

	if got := eff.startsFor("b"); got != 0 {
		t.Fatalf("b must stay held after the drain, StartSwap(b)=%d", got)
	}

	// Grace elapses: b goes first (FIFO within the rank).
	*clk = clk.Add(61 * time.Second)
	s.OnTick()
	if got := eff.startsFor("b"); got != 1 {
		t.Fatalf("b must start first when the grace expires, StartSwap(b)=%d", got)
	}
}
