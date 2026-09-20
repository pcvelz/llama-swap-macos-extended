package swaputil

import (
	"context"
	"testing"
	"time"
)

// newTestLoopTracker builds a tracker on a controllable clock (*clk) with the
// default strike ladder.
func newTestLoopTracker(runBar int, tolerance int64, clk *time.Time) *LoopTracker {
	g := DefaultLoopGuard()
	g.RunBar, g.ToleranceTokens = runBar, tolerance
	return newTestLoopTrackerGuard(g, clk)
}

func newTestLoopTrackerGuard(g LoopGuard, clk *time.Time) *LoopTracker {
	t := NewLoopTracker(g)
	t.now = func() time.Time { return *clk }
	return t
}

func recordAll(t *LoopTracker, session string, tokens []int64) {
	for _, n := range tokens {
		t.Record(session, n)
	}
}

// The real capture: session 934159af's 30 consecutive /v1/messages responses
// from the 2026-09-19 loop (46-49 output tokens each, one every ~20s).
var loopCapture = []int64{48, 48, 48, 47, 48, 47, 47, 48, 47, 48, 49, 47, 48, 48, 49, 48, 48, 48, 48, 49, 48, 47, 48, 48, 47, 48, 47, 47, 48, 47}

func TestLoopTracker_RealCaptureIsALoop(t *testing.T) {
	clk := time.Unix(1000, 0)
	lt := newTestLoopTracker(DefaultLoopRunBar, DefaultLoopTolerance, &clk)
	recordAll(lt, "934159af", loopCapture)

	if got := lt.Run("934159af"); got != len(loopCapture) {
		t.Fatalf("Run=%d want %d", got, len(loopCapture))
	}
	if !lt.Looping("934159af") {
		t.Fatalf("the captured loop must read as looping")
	}
}

// Healthy traffic: real turns vary wildly in output size, so the trailing run
// never gets off the ground. This is the control that keeps a productive
// session's full swap-grace protection.
func TestLoopTracker_VariedSizesAreNotALoop(t *testing.T) {
	clk := time.Unix(1000, 0)
	lt := newTestLoopTracker(DefaultLoopRunBar, DefaultLoopTolerance, &clk)
	varied := []int64{48, 412, 96, 1530, 61, 233, 48, 880, 47, 305}
	for i := 0; i < 3; i++ {
		recordAll(lt, "healthy", varied)
	}
	if lt.Looping("healthy") {
		t.Fatalf("varied response sizes must not read as looping (Run=%d)", lt.Run("healthy"))
	}
}

// Below the bar: uniform, but not yet long enough to be a loop.
func TestLoopTracker_ShortUniformRunIsBelowTheBar(t *testing.T) {
	clk := time.Unix(1000, 0)
	lt := newTestLoopTracker(DefaultLoopRunBar, DefaultLoopTolerance, &clk)
	for i := 0; i < 10; i++ {
		lt.Record("short", 48)
	}
	if got := lt.Run("short"); got != 10 {
		t.Fatalf("Run=%d want 10", got)
	}
	if lt.Looping("short") {
		t.Fatalf("10 uniform responses must not clear a bar of %d", DefaultLoopRunBar)
	}
}

// Recovery: the run is TRAILING, so one real turn after a long loop clears the
// verdict immediately - the session gets its protection back at once.
func TestLoopTracker_RecoveryResetsTheRun(t *testing.T) {
	clk := time.Unix(1000, 0)
	lt := newTestLoopTracker(DefaultLoopRunBar, DefaultLoopTolerance, &clk)
	for i := 0; i < 25; i++ {
		lt.Record("recovered", 48)
	}
	if !lt.Looping("recovered") {
		t.Fatalf("25 uniform responses should be looping first")
	}
	// A different size, still UNDER the output ceiling, so this is purely
	// about the spread breaking the run (the over-ceiling case is
	// TestLoopTracker_OutputCeilingBreaksTheRun).
	lt.Record("recovered", 120)
	if got := lt.Run("recovered"); got != 1 {
		t.Fatalf("Run=%d want 1 after one differently-sized response", got)
	}
	if lt.Looping("recovered") {
		t.Fatalf("a recovered session must not read as looping")
	}
}

func TestLoopTracker_EmptySessionIDIgnored(t *testing.T) {
	clk := time.Unix(1000, 0)
	lt := newTestLoopTracker(DefaultLoopRunBar, DefaultLoopTolerance, &clk)
	for i := 0; i < 30; i++ {
		lt.Record("", 48)
	}
	if got := lt.Run(""); got != 0 {
		t.Fatalf("Run(\"\")=%d want 0", got)
	}
	if lt.Looping("") {
		t.Fatalf("the empty session must never read as looping")
	}
	lt.mu.Lock()
	n := len(lt.sessions)
	lt.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d sessions tracked, want 0", n)
	}
}

// The map must not grow forever: a session with no completion for the TTL is
// dropped on the next Record by anyone.
func TestLoopTracker_PrunesIdleSessions(t *testing.T) {
	clk := time.Unix(1000, 0)
	lt := newTestLoopTracker(DefaultLoopRunBar, DefaultLoopTolerance, &clk)
	recordAll(lt, "old", loopCapture)
	if !lt.Looping("old") {
		t.Fatalf("setup: old should be looping")
	}

	clk = clk.Add(loopSessionTTL + time.Second)
	lt.Record("fresh", 100)

	if got := lt.Run("old"); got != 0 {
		t.Fatalf("Run(old)=%d want 0 after the TTL", got)
	}
	lt.mu.Lock()
	_, stillThere := lt.sessions["old"]
	n := len(lt.sessions)
	lt.mu.Unlock()
	if stillThere || n != 1 {
		t.Fatalf("old session not pruned (present=%v, tracked=%d)", stillThere, n)
	}
}

// The ring is bounded but never drops anything a verdict needs: it must
// always be able to hold the TOP strike's run (runBar * strikes).
func TestLoopTracker_HistoryIsBounded(t *testing.T) {
	clk := time.Unix(1000, 0)
	lt := newTestLoopTracker(DefaultLoopRunBar, DefaultLoopTolerance, &clk)
	max := lt.historyMax
	if max < DefaultLoopRunBar*DefaultLoopGuard().Strikes {
		t.Fatalf("historyMax=%d cannot hold the top strike's run", max)
	}
	for i := 0; i < max*3; i++ {
		lt.Record("long", 48)
	}
	lt.mu.Lock()
	n := len(lt.sessions["long"].tokens)
	lt.mu.Unlock()
	if n != max {
		t.Fatalf("history len=%d want %d", n, max)
	}
	if got := lt.Run("long"); got != max {
		t.Fatalf("Run=%d want %d", got, max)
	}
}

func TestLoopVerdictContext(t *testing.T) {
	if _, ok := LoopVerdictFromContext(context.Background()); ok {
		t.Fatalf("a bare context must carry no verdict")
	}
	ctx := WithLoopVerdict(context.Background(), func() bool { return true })
	verdict, ok := LoopVerdictFromContext(ctx)
	if !ok || !verdict() {
		t.Fatalf("verdict not carried through the context (ok=%v)", ok)
	}
}
