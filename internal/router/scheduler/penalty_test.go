package scheduler

import (
	"errors"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/process"
)

// fakeGate is a controllable loop-guard penalty gate: the test flips held /
// until, exactly as the tracker would when a hold starts, elapses or is
// manually un-penalized.
type fakeGate struct {
	held    bool
	until   time.Time
	forever bool
	clk     *time.Time
}

// gate returns the callback a HandlerReq carries. It expires a timed hold the
// way swaputil.LoopTracker.Held does, so the scheduler sees the same shape it
// sees in production.
func (g *fakeGate) gate() func() (bool, time.Time, bool) {
	return func() (bool, time.Time, bool) {
		if g.held && !g.forever && !g.until.IsZero() && !g.clk.Before(g.until) {
			g.held = false
		}
		return g.held, g.until, g.forever
	}
}

// penaltyReq is reqCh plus a penalty gate.
func penaltyReq(model string, g *fakeGate) HandlerReq {
	r := reqCh(model)
	r.PenaltyGate = g.gate()
	return r
}

// penaltyFixture: resident "a" (60s grace), a stopped "b" that evicts it, a
// stopped "c" that evicts nothing (the contention waiter, see contend), and a
// controllable clock.
func penaltyFixture(t *testing.T) (*FIFO, *fakeEffects, *time.Time) {
	t.Helper()
	eff := newFakeEffects()
	eff.states["a"] = process.StateReady
	eff.states["b"] = process.StateStopped
	eff.states["c"] = process.StateStopped
	clk := time.Unix(1000, 0)
	grace := map[string]time.Duration{"a": 60 * time.Second}
	s := newFIFOGrace(&runningFilterPlanner{evict: map[string][]string{"b": {"a"}}}, eff, grace, &clk)
	s.SetSwapStarvationSeconds(-1)
	return s, eff, &clk
}

// contend puts ANOTHER session on the scheduler and leaves it there, so the
// contention gate (2026-09-20, holdOnlyWhenContended) is satisfied: a hold
// only exists to protect somebody else, so without this nothing is ever held.
//
// The waiter is a request for "c", which evicts nothing and is stopped: it
// starts a swap and stays in s.active because the test never sends its
// SwapDone. That makes it durable across any amount of clock movement - a
// queued-behind-grace waiter would evaporate the moment a test advanced time
// past the grace - and it collides with nothing, so it neither blocks nor
// delays the requests under test.
func contend(s *FIFO) {
	s.OnRequest(reqCh("c"))
}

// A held request is PARKED with reason `penalized`, never granted and never
// refused: "a session is always promised a turn" (user ruling 2026-09-19).
// It is released on the first OnTick after its hold elapses - no new arrival
// needed, because a timed hold fires no event of its own.
func TestFIFO_PenalizedRequestHeldThenReleasedOnTick(t *testing.T) {
	s, eff, clk := penaltyFixture(t)
	contend(s) // somebody else is waiting, so the hold has a purpose
	g := &fakeGate{held: true, until: clk.Add(30 * time.Second), clk: clk}

	req := penaltyReq("a", g)
	s.OnRequest(req)
	if err := admitErr(t, req); err != nil {
		t.Fatalf("a penalized request must still be ADMITTED, got %v", err)
	}
	if got := eff.served("a"); got != 0 {
		t.Fatalf("served(a)=%d want 0 while held", got)
	}
	if len(s.penalized) != 1 || s.penalized[0].parkReason != ParkPenalized {
		t.Fatalf("penalized=%+v want one request parked as %q", s.penalized, ParkPenalized)
	}
	if len(s.queued) != 0 {
		t.Fatalf("a penalized request must not occupy a queue position: %+v", s.queued)
	}

	// Ticks before the deadline change nothing.
	*clk = clk.Add(29 * time.Second)
	s.OnTick()
	if got := eff.served("a"); got != 0 {
		t.Fatalf("served(a)=%d want 0 one second before the hold ends", got)
	}

	*clk = clk.Add(2 * time.Second)
	s.OnTick()
	if got := eff.served("a"); got != 1 {
		t.Fatalf("served(a)=%d want 1 on the first tick after the hold elapsed", got)
	}
	if len(s.penalized) != 0 {
		t.Fatalf("released request still in the penalized list: %+v", s.penalized)
	}
	if got := eff.errored(""); got != 0 {
		t.Fatalf("a penalty must never produce an error grant; got %d", got)
	}
}

// The -1 penalty: no timer will ever release it. Only un-penalize does, and
// the scheduler observes that within one tick because it re-reads the gate.
func TestFIFO_PenalizedForeverReleasesOnlyOnUnpenalize(t *testing.T) {
	s, eff, clk := penaltyFixture(t)
	contend(s) // the hold needs somebody to protect
	g := &fakeGate{held: true, forever: true, clk: clk}

	s.OnRequest(penaltyReq("a", g))
	for i := 0; i < 100; i++ {
		*clk = clk.Add(time.Minute)
		s.OnTick()
	}
	if got := eff.served("a"); got != 0 {
		t.Fatalf("served(a)=%d want 0: a HELD session must never be released by time", got)
	}

	// The menu's click.
	g.held, g.forever = false, false
	s.OnTick()
	if got := eff.served("a"); got != 1 {
		t.Fatalf("served(a)=%d want 1 after un-penalize", got)
	}
}

// The core of the phase-2 ruling: a penalized request must be INERT. It must
// not block, outrank or delay another session's request - not for its own
// model and not for another - and the cross-model swap the looper was
// starving must go through while it sits there.
func TestFIFO_PenalizedRequestDoesNotBlockAnyoneElse(t *testing.T) {
	s, eff, clk := penaltyFixture(t)
	contend(s) // the hold needs somebody to protect
	g := &fakeGate{held: true, forever: true, clk: clk}

	// The looper is held on the resident.
	s.OnRequest(penaltyReq("a", g))

	// Another session's request for the SAME model is served immediately.
	s.OnRequest(req("a"))
	if got := eff.served("a"); got != 1 {
		t.Fatalf("served(a)=%d want 1: a held request must not delay another session", got)
	}
	s.OnServeDone(ServeDoneEvent{ModelID: "a"})

	// And the starved cross-model request gets its swap once a's grace runs
	// out, with the held request neither deferring it nor naming a cooldown.
	*clk = clk.Add(61 * time.Second)
	s.OnRequest(reqCh("b"))
	if got := eff.startsFor("b"); got != 1 {
		t.Fatalf("StartSwap(b)=%d want 1: the held looper must not defer the swap it was starving", got)
	}
	if cd := s.Cooldown(); cd != nil && cd.NextModel == "a" {
		t.Fatalf("a held request must never name itself as the cooldown's next model: %+v", cd)
	}
	if got := eff.served("a"); got != 1 {
		t.Fatalf("served(a)=%d: the held request must still not have been granted", got)
	}
}

// A penalized request must not appear in the cooldown at all: not as
// NextModel, not in Waiting. Otherwise the menu would show the box cooling
// down "for" the session it is deliberately holding.
func TestFIFO_PenalizedRequestIsNotInTheCooldown(t *testing.T) {
	s, _, clk := penaltyFixture(t)
	contend(s) // the hold needs somebody to protect
	g := &fakeGate{held: true, forever: true, clk: clk}

	// a has served and gone idle; its grace is running.
	s.OnRequest(req("a"))
	s.OnServeDone(ServeDoneEvent{ModelID: "a"})
	*clk = clk.Add(10 * time.Second)

	// A held request for the OTHER model would, if queued, be the cooldown's
	// NextModel with Waiting 1.
	s.OnRequest(penaltyReq("b", g))

	cd := s.Cooldown()
	if cd != nil && (cd.NextModel != "" || cd.Waiting != 0) {
		t.Fatalf("Cooldown()=%+v: a held request must add neither NextModel nor Waiting", cd)
	}
}

// Status reads and count_tokens are never held: they hold no slot, answer a
// poller rather than the looping agent, and a status read is never allowed to
// queue at all (2026-09-10).
func TestFIFO_PenaltySkipsStatusReadsAndCountTokens(t *testing.T) {
	s, eff, clk := penaltyFixture(t)
	g := &fakeGate{held: true, forever: true, clk: clk}

	read := penaltyReq("a", g)
	read.StatusRead = true
	s.OnRequest(read)

	count := penaltyReq("a", g)
	count.ConcurrencyExempt = true
	s.OnRequest(count)

	if len(s.penalized) != 0 {
		t.Fatalf("penalized=%+v want empty: reads and count_tokens are exempt", s.penalized)
	}
	if got := eff.served("a"); got != 2 {
		t.Fatalf("served(a)=%d want 2", got)
	}
}

// A cancelled client must not come back to life when its hold ends.
func TestFIFO_PenalizedRequestPrunedOnCancel(t *testing.T) {
	s, eff, clk := penaltyFixture(t)
	contend(s) // the hold needs somebody to protect
	g := &fakeGate{held: true, until: clk.Add(10 * time.Second), clk: clk}

	req := penaltyReq("a", g)
	s.OnRequest(req)
	s.OnCancel(req)
	if len(s.penalized) != 0 {
		t.Fatalf("penalized=%+v want empty after cancel", s.penalized)
	}

	*clk = clk.Add(20 * time.Second)
	s.OnTick()
	if got := eff.served("a"); got != 0 {
		t.Fatalf("served(a)=%d want 0: a cancelled request must never be released", got)
	}
}

// Shutdown must not leave a held caller hanging.
func TestFIFO_PenalizedRequestGetsShutdownError(t *testing.T) {
	s, eff, clk := penaltyFixture(t)
	contend(s) // the hold needs somebody to protect
	g := &fakeGate{held: true, forever: true, clk: clk}
	s.OnRequest(penaltyReq("a", g))

	s.OnShutdown(errors.New("shutting down"))
	// Filtered by model so the contention waiter's own shutdown error (on
	// "c") cannot be mistaken for the held caller's.
	if got := eff.errored("a"); got != 1 {
		t.Fatalf("errored(a)=%d want 1: a held caller must be released at shutdown", got)
	}
}
