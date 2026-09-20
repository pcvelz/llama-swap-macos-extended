package scheduler

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
)

// newFIFOGraceLogged is newFIFOGrace with its log captured, so the
// `loopguard:` lines can be asserted. The 2026-09-20 incident produced NOT
// ONE log line while a session sat parked for an hour; the lines are part of
// the fix, so they are tested like any other behaviour.
func newFIFOGraceLogged(planner Swapper, eff Effects, grace map[string]time.Duration, clk *time.Time, log io.Writer) *FIFO {
	models := make(map[string]config.ModelConfig, len(grace))
	for id, g := range grace {
		models[id] = config.ModelConfig{SwapGraceSeconds: int(g / time.Second)}
	}
	s := NewFIFO("test", logmon.NewWriter(log), planner, config.FifoConfig{}, models, eff)
	s.now = func() time.Time { return *clk }
	return s
}

// THE CAPTURED SHAPE (2026-09-20). A penalized session's request must never
// sit held while the queue is otherwise empty: the box had zero other waiters
// at every one of the session's last 16 completions, the resident
// idle-unloaded underneath the held request, and a human had to release it
// after an hour. With the contention gate the request is never held at all -
// and if it somehow were, one tick releases it.
func TestFIFO_NoHoldWhenNobodyIsWaiting(t *testing.T) {
	s, eff, clk := penaltyFixture(t)
	g := &fakeGate{held: true, forever: true, clk: clk}

	// Empty box, empty queue: the strike stands, the hold does not.
	s.OnRequest(penaltyReq("a", g))
	if len(s.penalized) != 0 {
		t.Fatalf("penalized=%+v want empty: nobody is waiting, so there is nobody to protect", s.penalized)
	}
	if got := eff.served("a"); got != 1 {
		t.Fatalf("served(a)=%d want 1: a penalized session on an EMPTY box is served normally", got)
	}

	// The resident then idle-unloads, exactly as it did in the incident. The
	// session must still not be held on its next attempt.
	s.OnServeDone(ServeDoneEvent{ModelID: "a", Looping: true})
	*clk = clk.Add(time.Hour)
	s.OnTick()
	s.OnRequest(penaltyReq("a", g))
	if len(s.penalized) != 0 {
		t.Fatalf("penalized=%+v want empty an hour later on a still-empty box", s.penalized)
	}
	if got := eff.served("a"); got != 2 {
		t.Fatalf("served(a)=%d want 2", got)
	}
}

// Belt and braces for the same rule: even if a request IS in FIFO.penalized
// (it was held while somebody waited), it can never remain there for more
// than one tick once the queue empties.
func TestFIFO_PenalizedReleasedWithinOneTickWhenQueueEmpties(t *testing.T) {
	eff := newFakeEffects()
	eff.states["a"] = process.StateReady
	eff.states["c"] = process.StateStopped
	clk := time.Unix(1000, 0)
	s := newFIFOGrace(&runningFilterPlanner{}, eff, map[string]time.Duration{"a": 60 * time.Second}, &clk)
	g := &fakeGate{held: true, forever: true, clk: &clk}

	// A waiter exists, so the hold is real.
	contend(s)
	s.OnRequest(penaltyReq("a", g))
	if len(s.penalized) != 1 {
		t.Fatalf("setup: penalized=%+v want the request held", s.penalized)
	}

	// The waiter's swap completes and it is served: the box is empty again.
	eff.states["c"] = process.StateReady
	s.OnSwapDone(SwapDone{ModelID: "c"})
	s.OnServeDone(ServeDoneEvent{ModelID: "c"})

	s.OnTick()
	if len(s.penalized) != 0 {
		t.Fatalf("penalized=%+v want empty ONE tick after the queue drained", s.penalized)
	}
	if got := eff.served("a"); got != 1 {
		t.Fatalf("served(a)=%d want 1: the held request is released, not left parked", got)
	}
}

// The gate is symmetric: contention appears -> the session's NEXT request is
// held again; the waiter goes away -> it is released on the next tick,
// without any timer elapsing.
func TestFIFO_HoldFollowsContention(t *testing.T) {
	s, eff, clk := penaltyFixture(t)
	// A long timed hold, so nothing in this test can be explained by the
	// timer running out.
	g := &fakeGate{held: true, until: clk.Add(time.Hour), clk: clk}

	// 1. No contention: served.
	s.OnRequest(penaltyReq("a", g))
	if got := eff.served("a"); got != 1 {
		t.Fatalf("served(a)=%d want 1 with nobody waiting", got)
	}
	s.OnServeDone(ServeDoneEvent{ModelID: "a", Looping: true})

	// 2. Contention appears: the NEXT request is held.
	contend(s)
	s.OnRequest(penaltyReq("a", g))
	if len(s.penalized) != 1 {
		t.Fatalf("penalized=%+v want the next request held once somebody waits", s.penalized)
	}
	if got := eff.served("a"); got != 1 {
		t.Fatalf("served(a)=%d want still 1 while held", got)
	}

	// 3. The waiter is served and leaves; the hold has not elapsed.
	eff.states["c"] = process.StateReady
	s.OnSwapDone(SwapDone{ModelID: "c"})
	s.OnServeDone(ServeDoneEvent{ModelID: "c"})
	*clk = clk.Add(time.Second)
	s.OnTick()

	if held, _, _ := g.gate()(); !held {
		t.Fatalf("the test is meaningless if the timer elapsed: the hold must still be live")
	}
	if len(s.penalized) != 0 {
		t.Fatalf("penalized=%+v want empty once the waiter left", s.penalized)
	}
	if got := eff.served("a"); got != 2 {
		t.Fatalf("served(a)=%d want 2: released without the timer elapsing", got)
	}
}

// A REAL small-token loop on an otherwise empty box keeps its phase-1
// treatment - strikes are recorded and it earns no fresh swap-grace - but
// nothing is ever held, because there is nobody to hold it for. Withholding
// grace costs the looper nothing while it is alone; it only matters the
// moment somebody else shows up, which is exactly when the hold starts too.
func TestFIFO_LooperAloneKeepsGraceWithheldButIsNeverHeld(t *testing.T) {
	s, eff, clk := penaltyFixture(t)
	g := &fakeGate{held: true, until: clk.Add(time.Hour), clk: clk}

	// The looper serves and completes repeatedly, flagged Looping, on an
	// empty box. a's idle mark must NOT be refreshed by those completions.
	s.OnRequest(penaltyReq("a", g))
	s.OnServeDone(ServeDoneEvent{ModelID: "a", Looping: true})
	firstIdle := s.idleSince["a"]

	for i := 0; i < 5; i++ {
		*clk = clk.Add(20 * time.Second)
		s.OnTick()
		s.OnRequest(penaltyReq("a", g))
		s.OnServeDone(ServeDoneEvent{ModelID: "a", Looping: true})
	}

	if !s.idleSince["a"].Equal(firstIdle) {
		t.Fatalf("idleSince moved to %v (was %v): a looping session must not earn fresh grace", s.idleSince["a"], firstIdle)
	}
	if len(s.penalized) != 0 {
		t.Fatalf("penalized=%+v want empty: nobody was ever waiting", s.penalized)
	}
	if got := eff.served("a"); got != 6 {
		t.Fatalf("served(a)=%d want 6: every request must have been served on the empty box", got)
	}
}

// holdOnlyWhenContended: false restores the unconditional phase-2 behaviour
// for a deployment that wants it.
func TestFIFO_ContentionGateCanBeDisabled(t *testing.T) {
	s, eff, clk := penaltyFixture(t)
	s.SetHoldOnlyWhenContended(false)
	g := &fakeGate{held: true, forever: true, clk: clk}

	s.OnRequest(penaltyReq("a", g))
	if len(s.penalized) != 1 {
		t.Fatalf("penalized=%+v want the request held on an empty box with the gate off", s.penalized)
	}
	if got := eff.served("a"); got != 0 {
		t.Fatalf("served(a)=%d want 0", got)
	}
	s.OnTick()
	if len(s.penalized) != 1 {
		t.Fatalf("penalized=%+v want it still held after a tick", s.penalized)
	}
}

// Every hold and release is logged at INFO with a greppable prefix.
func TestFIFO_PenaltyDecisionsAreLogged(t *testing.T) {
	eff := newFakeEffects()
	eff.states["a"] = process.StateReady
	eff.states["c"] = process.StateStopped
	clk := time.Unix(1000, 0)
	var log bytes.Buffer
	s := newFIFOGraceLogged(&runningFilterPlanner{}, eff, map[string]time.Duration{"a": 60 * time.Second}, &clk, &log)
	g := &fakeGate{held: true, until: clk.Add(30 * time.Second), clk: &clk}

	contend(s)
	s.OnRequest(penaltyReq("a", g))
	if got := log.String(); !strings.Contains(got, "loopguard: holding request for model a") {
		t.Fatalf("no hold line logged; log was:\n%s", got)
	}

	clk = clk.Add(31 * time.Second)
	s.OnTick()
	if got := log.String(); !strings.Contains(got, "loopguard: released request for model a (hold ended)") {
		t.Fatalf("no release line logged; log was:\n%s", got)
	}
}

// The no-contention release says SO, rather than claiming a timer ended.
func TestFIFO_NoContentionReleaseIsLoggedAsSuch(t *testing.T) {
	eff := newFakeEffects()
	eff.states["a"] = process.StateReady
	eff.states["c"] = process.StateStopped
	clk := time.Unix(1000, 0)
	var log bytes.Buffer
	s := newFIFOGraceLogged(&runningFilterPlanner{}, eff, map[string]time.Duration{"a": 60 * time.Second}, &clk, &log)
	g := &fakeGate{held: true, forever: true, clk: &clk}

	contend(s)
	s.OnRequest(penaltyReq("a", g))
	eff.states["c"] = process.StateReady
	s.OnSwapDone(SwapDone{ModelID: "c"})
	s.OnServeDone(ServeDoneEvent{ModelID: "c"})
	s.OnTick()

	if got := log.String(); !strings.Contains(got, "loopguard: released request for model a (no contention)") {
		t.Fatalf("release reason not logged; log was:\n%s", got)
	}
}
