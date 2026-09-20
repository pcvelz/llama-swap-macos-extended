package swaputil

import (
	"context"
	"testing"
	"time"
)

func contextBackground() context.Context { return context.Background() }

// feed records n responses of the same size on one session.
func feed(lt *LoopTracker, session string, n int, tokens int64) {
	for i := 0; i < n; i++ {
		lt.Record(session, tokens)
	}
}

// Strike n is reached at n * runBar: 20 -> 1, 40 -> 2, 60 -> 3, and the
// ladder is capped at the configured strike count however long the loop runs.
func TestLoopTracker_StrikeThresholds(t *testing.T) {
	clk := time.Unix(1000, 0)
	lt := newTestLoopTrackerGuard(DefaultLoopGuard(), &clk)

	feed(lt, "s", 19, 48)
	if _, ok := lt.Penalty("s"); ok {
		t.Fatalf("no strike may exist below the run bar")
	}

	feed(lt, "s", 1, 48) // run 20
	p, ok := lt.Penalty("s")
	if !ok || p.Strike != 1 || p.Strikes != 3 {
		t.Fatalf("Penalty=%+v ok=%v want strike 1 of 3 at run 20", p, ok)
	}
	// Strike 1 costs 0 seconds: a flag, no hold. This is exactly phase-1
	// behaviour - served, earns no fresh grace.
	if p.Held || p.HeldForever {
		t.Fatalf("strike 1 (penaltySeconds 0) must carry no hold: %+v", p)
	}
	if p.TypicalTokens != 48 || p.UniformRun != 20 {
		t.Fatalf("Penalty=%+v want uniformRun 20, typicalTokens 48", p)
	}

	feed(lt, "s", 20, 48) // run 40
	p, _ = lt.Penalty("s")
	if p.Strike != 2 || !p.Held || p.HeldForever {
		t.Fatalf("Penalty=%+v want strike 2 with a TIMED hold", p)
	}
	if want := clk.Add(900 * time.Second); !p.Until.Equal(want) {
		t.Fatalf("Until=%v want %v (900s)", p.Until, want)
	}

	feed(lt, "s", 20, 48) // run 60
	p, _ = lt.Penalty("s")
	// NO INFINITE DEFAULT (2026-09-20): the shipped top strike is an hour,
	// not "until a human clicks".
	if p.Strike != 3 || !p.Held || p.HeldForever {
		t.Fatalf("Penalty=%+v want strike 3 with a TIMED hold, not a forever hold", p)
	}
	if want := clk.Add(3600 * time.Second); !p.Until.Equal(want) {
		t.Fatalf("Until=%v want %v (3600s)", p.Until, want)
	}

	// Capped: the ladder has 3 rungs, however long the loop goes on.
	feed(lt, "s", 40, 48)
	p, _ = lt.Penalty("s")
	if p.Strike != 3 {
		t.Fatalf("strike=%d want the ladder capped at 3", p.Strike)
	}
}

// THE FALSE POSITIVE (2026-09-20). A Hermes session (ef043b3f) made 45
// requests that each decoded for 142-159s and emitted ~1083 tokens, +-2 -
// uniform because the client caps output, not because it was looping - and
// the guard held it for an hour against an empty box. Above the output
// ceiling, so: run 0, never looping, never a strike.
func TestLoopTracker_LargeUniformOutputsAreNotALoop(t *testing.T) {
	clk := time.Unix(1000, 0)
	lt := newTestLoopTrackerGuard(DefaultLoopGuard(), &clk)

	// The captured shape: 45 responses, 1081-1085 tokens.
	sizes := []int64{1083, 1081, 1084, 1082, 1085}
	for i := 0; i < 45; i++ {
		lt.Record("ef043b3f", sizes[i%len(sizes)])
	}

	if got := lt.Run("ef043b3f"); got > 1 {
		t.Fatalf("Run=%d want 0: a capped 1083-token response is productive work, not loop evidence", got)
	}
	if lt.Looping("ef043b3f") {
		t.Fatalf("a session emitting ~1083 tokens a turn must never read as looping")
	}
	if p, ok := lt.Penalty("ef043b3f"); ok {
		t.Fatalf("Penalty=%+v want none: a healthy session must never earn a strike", p)
	}
	if held, _, _ := lt.Held("ef043b3f"); held {
		t.Fatalf("a healthy session must never be held")
	}
}

// The ceiling BREAKS a run rather than merely not extending it, and the
// breaking response counts toward clearRequests forgiveness: one real turn is
// the strongest evidence of recovery there is.
func TestLoopTracker_OutputCeilingBreaksTheRun(t *testing.T) {
	clk := time.Unix(1000, 0)
	g := DefaultLoopGuard()
	g.ClearRequests = 2
	lt := newTestLoopTrackerGuard(g, &clk)

	feed(lt, "s", 25, 48) // a real loop: strike 2
	if _, ok := lt.Penalty("s"); !ok {
		t.Fatalf("setup: the small-token loop should have earned a strike")
	}

	lt.Record("s", g.MaxLoopTokens+1)
	if got := lt.Run("s"); got != 0 {
		t.Fatalf("Run=%d want 0: an over-ceiling response breaks the run outright", got)
	}
	lt.Record("s", g.MaxLoopTokens*4)
	if p, ok := lt.Penalty("s"); ok {
		t.Fatalf("Penalty=%+v: two real turns must forgive the session (clearRequests 2)", p)
	}
}

// The ceiling is a strict threshold: a response AT the ceiling is still loop
// evidence, only one ABOVE it is productive.
func TestLoopTracker_OutputCeilingIsInclusive(t *testing.T) {
	clk := time.Unix(1000, 0)
	g := DefaultLoopGuard()
	g.MaxLoopTokens = 100
	lt := newTestLoopTrackerGuard(g, &clk)

	feed(lt, "at-ceiling", 20, 100)
	if !lt.Looping("at-ceiling") {
		t.Fatalf("a response AT the ceiling is still loop evidence (run=%d)", lt.Run("at-ceiling"))
	}
	feed(lt, "over-ceiling", 20, 101)
	if lt.Looping("over-ceiling") {
		t.Fatalf("a response ABOVE the ceiling must never be loop evidence")
	}
}

// A hold does NOT reset the run: "the agent rebounding on the box is
// undesired" (user ruling). A session that comes back and keeps emitting the
// same thing climbs to the next strike rather than starting over.
func TestLoopTracker_HoldDoesNotResetTheRun(t *testing.T) {
	clk := time.Unix(1000, 0)
	lt := newTestLoopTrackerGuard(DefaultLoopGuard(), &clk)

	feed(lt, "s", 40, 48) // strike 2, 900s hold
	clk = clk.Add(901 * time.Second)
	if held, _, _ := lt.Held("s"); held {
		t.Fatalf("the timed hold must have elapsed")
	}
	if p, _ := lt.Penalty("s"); p.Strike != 2 {
		t.Fatalf("an elapsed hold must keep the STRIKE; got %+v", p)
	}
	// It comes back and keeps looping: one more step is all it takes.
	feed(lt, "s", 20, 48)
	p, _ := lt.Penalty("s")
	if p.Strike != 3 || !p.Held {
		t.Fatalf("Penalty=%+v want the next strike, not a restart", p)
	}
}

// -1 remains legal CONFIGURATION even though it is no longer a default: a
// deployment that wants "held until a human looks" can still say so.
func TestLoopTracker_ForeverHoldIsStillConfigurable(t *testing.T) {
	clk := time.Unix(1000, 0)
	g := DefaultLoopGuard()
	g.PenaltySeconds = []int{0, 900, -1}
	lt := newTestLoopTrackerGuard(g, &clk)

	feed(lt, "s", 60, 48)
	p, _ := lt.Penalty("s")
	if p.Strike != 3 || !p.Held || !p.HeldForever || !p.Until.IsZero() {
		t.Fatalf("Penalty=%+v want strike 3 HELD until un-penalized", p)
	}
	clk = clk.Add(100 * time.Hour)
	if held, _, _ := lt.Held("s"); !held {
		t.Fatalf("a -1 hold must never be released by time")
	}
}

// Timed holds expire lazily on read - there is no timer goroutine.
func TestLoopTracker_TimedHoldExpires(t *testing.T) {
	clk := time.Unix(1000, 0)
	lt := newTestLoopTrackerGuard(DefaultLoopGuard(), &clk)
	feed(lt, "s", 40, 48)

	held, until, forever := lt.Held("s")
	if !held || forever || !until.Equal(clk.Add(900*time.Second)) {
		t.Fatalf("Held=(%v,%v,%v) want a 900s timed hold", held, until, forever)
	}
	clk = clk.Add(899 * time.Second)
	if held, _, _ := lt.Held("s"); !held {
		t.Fatalf("the hold must survive to its deadline")
	}
	clk = clk.Add(2 * time.Second)
	if held, _, _ := lt.Held("s"); held {
		t.Fatalf("the hold must release once its deadline passes")
	}
}

// Recovery: clearRequests consecutive below-bar records wipe the strikes AND
// any hold.
func TestLoopTracker_ClearRequestsForgivesTheSession(t *testing.T) {
	clk := time.Unix(1000, 0)
	g := DefaultLoopGuard()
	g.ClearRequests = 4
	lt := newTestLoopTrackerGuard(g, &clk)

	feed(lt, "s", 60, 48) // strike 3, HELD
	if held, _, _ := lt.Held("s"); !held {
		t.Fatalf("setup: the session should be held")
	}

	// Real, varied work. Each record breaks the run, so each one counts
	// toward the clear streak.
	for i, n := range []int64{900, 120, 430} {
		lt.Record("s", n)
		if _, ok := lt.Penalty("s"); !ok {
			t.Fatalf("forgiven after only %d of 4 clearing requests", i+1)
		}
	}
	lt.Record("s", 77)
	if p, ok := lt.Penalty("s"); ok {
		t.Fatalf("Penalty=%+v ok=%v want the session forgiven after 4", p, ok)
	}
	if held, _, _ := lt.Held("s"); held {
		t.Fatalf("forgiving a session must release its hold too")
	}
}

// The streak must be CONSECUTIVE: a relapse into the run resets it.
func TestLoopTracker_ClearStreakIsConsecutive(t *testing.T) {
	clk := time.Unix(1000, 0)
	g := DefaultLoopGuard()
	g.ClearRequests = 3
	lt := newTestLoopTrackerGuard(g, &clk)

	feed(lt, "s", 20, 48) // strike 1
	lt.Record("s", 500)
	lt.Record("s", 600)
	// A relapse: this alone cannot rebuild a run of 20, but it must reset the
	// clearing streak so the next two records do not forgive the session.
	feed(lt, "s", 20, 48)
	lt.Record("s", 700)
	lt.Record("s", 800)
	if _, ok := lt.Penalty("s"); !ok {
		t.Fatalf("a relapse must restart the clearing streak")
	}
}

// Un-penalize forgets the session ENTIRELY: it must not be one uniform
// response away from its old strike.
func TestLoopTracker_UnpenalizeForgetsHistory(t *testing.T) {
	clk := time.Unix(1000, 0)
	lt := newTestLoopTrackerGuard(DefaultLoopGuard(), &clk)
	feed(lt, "s", 60, 48)

	lt.Unpenalize("s")
	if _, ok := lt.Penalty("s"); ok {
		t.Fatalf("un-penalize must clear the penalty")
	}
	if held, _, _ := lt.Held("s"); held {
		t.Fatalf("un-penalize must release the hold")
	}
	if got := lt.Run("s"); got != 0 {
		t.Fatalf("Run=%d want 0: the history must be forgotten", got)
	}
	// One more identical response must NOT put it straight back on strike 3.
	lt.Record("s", 48)
	if _, ok := lt.Penalty("s"); ok {
		t.Fatalf("one response after un-penalize must not re-penalize")
	}
	lt.Unpenalize("unknown-session") // no-op, must not panic
}

// A HELD session is exempt from the idle TTL: not sending requests is exactly
// what the hold makes it do, so ageing it out would silently un-penalize it.
func TestLoopTracker_HeldSessionSurvivesTheTTL(t *testing.T) {
	clk := time.Unix(1000, 0)
	g := DefaultLoopGuard()
	// A hold that outlasts the TTL by a wide margin, so this test is about
	// the prune and not about the hold expiring.
	g.PenaltySeconds = []int{0, 900, -1}
	lt := newTestLoopTrackerGuard(g, &clk)
	feed(lt, "s", 60, 48) // HELD

	clk = clk.Add(loopSessionTTL * 4)
	lt.Record("someone-else", 100) // drives the prune
	if held, _, _ := lt.Held("s"); !held {
		t.Fatalf("a HELD session must not be aged out of the tracker")
	}
	if got := lt.HeldSessions(); len(got) != 1 || got[0] != "s" {
		t.Fatalf("HeldSessions=%v want [s]", got)
	}
}

// The observer sees every transition, and never under the tracker's lock (it
// is free to call back into anything, which this test does).
func TestLoopTracker_PenaltyObserverSeesEveryTransition(t *testing.T) {
	clk := time.Unix(1000, 0)
	lt := newTestLoopTrackerGuard(DefaultLoopGuard(), &clk)
	var kinds []string
	lt.SetPenaltyObserver(func(ev LoopPenaltyEvent) {
		lt.Run(ev.SessionID) // would deadlock if emitted under t.mu
		kinds = append(kinds, ev.Kind)
	})

	feed(lt, "s", 20, 48) // strike 1, no hold
	feed(lt, "s", 20, 48) // strike 2 + hold-start
	clk = clk.Add(901 * time.Second)
	lt.Held("s")       // expires the hold -> hold-end
	lt.Unpenalize("s") // -> unpenalize

	want := []string{PenaltyEventStrike, PenaltyEventStrike, PenaltyEventHoldStart, PenaltyEventHoldEnd, PenaltyEventUnpenalize}
	if len(kinds) != len(want) {
		t.Fatalf("events=%v want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("events=%v want %v", kinds, want)
		}
	}
}

// The disabled tracker answers "productive, unpenalized" to everything.
func TestLoopTracker_NilTrackerIsInert(t *testing.T) {
	var lt *LoopTracker
	lt.Record("s", 48)
	lt.Unpenalize("s")
	lt.SetPenaltyObserver(func(LoopPenaltyEvent) {})
	if held, _, _ := lt.Held("s"); held {
		t.Fatalf("a disabled tracker must never hold anything")
	}
	if _, ok := lt.Penalty("s"); ok {
		t.Fatalf("a disabled tracker must report no penalty")
	}
	if lt.HeldSessions() != nil {
		t.Fatalf("a disabled tracker holds nobody")
	}
}

func TestPenaltyGateContext(t *testing.T) {
	if _, ok := PenaltyGateFromContext(contextBackground()); ok {
		t.Fatalf("a bare context must carry no penalty gate")
	}
	ctx := WithPenaltyGate(contextBackground(), func() (bool, time.Time, bool) { return true, time.Time{}, true })
	gate, ok := PenaltyGateFromContext(ctx)
	if !ok {
		t.Fatalf("gate not carried through the context")
	}
	held, _, forever := gate()
	if !held || !forever {
		t.Fatalf("gate()=(%v,_,%v) want held forever", held, forever)
	}
}
