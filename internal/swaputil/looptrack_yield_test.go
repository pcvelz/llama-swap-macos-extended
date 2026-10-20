package swaputil

import (
	"testing"
	"time"
)

// newTestYieldTracker builds a tracker with yield floor enabled and a
// controllable clock. runBar is set high so loop detection never fires;
// only the yield floor matters in these tests.
func newTestYieldTracker(runBar int, clk *time.Time) *LoopTracker {
	g := DefaultLoopGuard()
	g.RunBar = runBar
	if g.RunBar <= 0 {
		g.RunBar = DefaultLoopRunBar
	}
	t := NewLoopTracker(g)
	t.now = func() time.Time { return *clk }
	return t
}

// --- yield floor breach fires a strike + logs the event ---

func TestYieldFloor_BreachFiresStrike(t *testing.T) {
	clk := time.Unix(1000, 0)
	lt := newTestYieldTracker(DefaultLoopRunBar, &clk)

	// First check: 5 min in, 2 tokens = 0.4 tok/min, below floor of 10.
	// Sets the breach timer but no strike yet (5 < 15 min window).
	clk = clk.Add(5 * time.Minute)
	strike, _ := lt.CheckYieldFloor("glacial", 5*time.Minute, 2)
	if strike {
		t.Fatalf("no strike at 5 min: breach timer starts but window hasn't elapsed")
	}

	// Advance clock past the 15-min breach window. The request has been
	// running for 20 min total with only 2 tokens = 0.1 tok/min.
	clk = clk.Add(16 * time.Minute) // now at t=1020 min
	strike, ev := lt.CheckYieldFloor("glacial", 20*time.Minute, 2)
	if !strike {
		t.Fatalf("expected a yield strike after 21 min of breach (5+16)")
	}
	if ev.Reason != PenaltyReasonYield {
		t.Fatalf("ev.Reason=%q want %q", ev.Reason, PenaltyReasonYield)
	}
	if ev.Kind != PenaltyEventStrike {
		t.Fatalf("ev.Kind=%q want %q", ev.Kind, PenaltyEventStrike)
	}
	if ev.Strike != 1 {
		t.Fatalf("ev.Strike=%d want 1", ev.Strike)
	}
	if ev.HoldSeconds != 0 {
		t.Fatalf("ev.HoldSeconds=%d want 0 (yield is flag-only at strike 1)", ev.HoldSeconds)
	}
}

// --- above-floor request is never struck ---

func TestYieldFloor_AboveFloorNeverStrikes(t *testing.T) {
	clk := time.Unix(1000, 0)
	lt := newTestYieldTracker(DefaultLoopRunBar, &clk)

	// 50 tokens in 10 minutes = 5 tok/min — below floor of 10 but elapsed
	// (10 min) < window (15 min), so no strike.
	clk = clk.Add(10 * time.Minute)
	strike, _ := lt.CheckYieldFloor("slow", 10*time.Minute, 50)
	if strike {
		t.Fatalf("elapsed < window: no strike yet")
	}

	// Now test a truly healthy rate: 500 tokens in 10 min = 50 tok/min.
	clk2 := time.Unix(2000, 0)
	lt2 := newTestYieldTracker(DefaultLoopRunBar, &clk2)
	clk2 = clk2.Add(10 * time.Minute)
	strike, _ = lt2.CheckYieldFloor("healthy", 10*time.Minute, 500)
	if strike {
		t.Fatalf("a healthy 50 tok/min session must never be struck")
	}

	// Even at the exact floor (10 tok/min = 100 tokens in 10 min): yield is
	// BELOW floor only when strictly less than. At exactly the floor it is fine.
	strike, _ = lt2.CheckYieldFloor("at-floor", 10*time.Minute, 100)
	if strike {
		t.Fatalf("yield AT the floor must not be below it")
	}
}

// --- yield recovers before window expires: no strike ---

func TestYieldFloor_RecoveryBeforeWindow(t *testing.T) {
	clk := time.Unix(1000, 0)
	lt := newTestYieldTracker(DefaultLoopRunBar, &clk)

	// Start glacial at t=5 min.
	clk = clk.Add(5 * time.Minute)
	strike, _ := lt.CheckYieldFloor("recovering", 5*time.Minute, 2) // 0.4 tok/min
	if strike {
		t.Fatalf("setup: should not have struck yet (5 < 15 min window)")
	}

	// Yield recovers ABOVE floor before the 15-min window expires.
	clk = clk.Add(3 * time.Minute)                                  // t=8 min
	strike, _ = lt.CheckYieldFloor("recovering", 8*time.Minute, 90) // ~11.25 tok/min — above floor
	if strike {
		t.Fatalf("recovery above floor must reset the breach timer")
	}

	// Even if yield drops again, it needs another full window.
	// Use elapsed < 15 min so no instant-strike fires.
	clk = clk.Add(5 * time.Minute)                                   // t=13 min
	strike, _ = lt.CheckYieldFloor("recovering", 13*time.Minute, 80) // ~6.15 tok/min — below floor but only 5 min breach
	if strike {
		t.Fatalf("a fresh breach needs a full yieldFloorMinutes window")
	}
}

// --- no tokens produced: yield is zero, treated as recovered (no data) ---

func TestYieldFloor_ZeroTokensIsNotABreach(t *testing.T) {
	clk := time.Unix(1000, 0)
	lt := newTestYieldTracker(DefaultLoopRunBar, &clk)

	// A request that has been running for a while but produced zero tokens:
	// InflightYield returns 0 when produced <= 0, and CheckYieldFloor treats
	// yield == 0 as "recovered" (reset breach timer). This prevents striking
	// requests that are still in prefill.
	clk = clk.Add(30 * time.Minute)
	strike, _ := lt.CheckYieldFloor("prefill", 30*time.Minute, 0)
	if strike {
		t.Fatalf("zero tokens must not be struck (still in prefill)")
	}
}

// --- nil tracker is inert ---

func TestYieldFloor_NilTrackerIsInert(t *testing.T) {
	var lt *LoopTracker
	strike, ev := lt.CheckYieldFloor("any", 30*time.Minute, 5)
	if strike {
		t.Fatalf("nil tracker must never strike")
	}
	if ev.SessionID != "" {
		t.Fatalf("nil tracker must return zero event")
	}
}

// --- duplicate strikes within the same window are suppressed ---

func TestYieldFloor_NoDuplicateStrikes(t *testing.T) {
	clk := time.Unix(1000, 0)
	lt := newTestYieldTracker(DefaultLoopRunBar, &clk)

	// First strike at t=21 min (5+16).
	clk = clk.Add(5 * time.Minute)  // t=5
	clk = clk.Add(16 * time.Minute) // t=21
	strike1, _ := lt.CheckYieldFloor("persistent", 21*time.Minute, 5)
	if !strike1 {
		t.Fatalf("expected first strike")
	}

	// Second call: same glacial request, but only 2 min since last strike.
	clk = clk.Add(2 * time.Minute) // t=23
	strike2, _ := lt.CheckYieldFloor("persistent", 23*time.Minute, 5)
	if strike2 {
		t.Fatalf("a second strike must not fire within the same window")
	}

	// After a full window since the last strike, a new one is allowed.
	clk = clk.Add(16 * time.Minute) // t=39, 18 min since last strike
	strike3, _ := lt.CheckYieldFloor("persistent", 39*time.Minute, 5)
	if !strike3 {
		t.Fatalf("a fresh window should allow a new strike")
	}
}

// --- yield floor config defaults ---

func TestYieldFloor_Defaults(t *testing.T) {
	g := DefaultLoopGuard()
	if g.YieldFloorTokPerMin != DefaultYieldFloorTokPerMin {
		t.Fatalf("YieldFloorTokPerMin default = %v, want %v", g.YieldFloorTokPerMin, DefaultYieldFloorTokPerMin)
	}
	if g.YieldFloorMinutes != DefaultYieldFloorMinutes {
		t.Fatalf("YieldFloorMinutes default = %d, want %d", g.YieldFloorMinutes, DefaultYieldFloorMinutes)
	}

	lt := NewLoopTracker(g)
	if lt.YieldFloorTokPerMin() != DefaultYieldFloorTokPerMin {
		t.Fatalf("effective floor = %v, want %v", lt.YieldFloorTokPerMin(), DefaultYieldFloorTokPerMin)
	}
	if lt.YieldFloorMinutes() != DefaultYieldFloorMinutes {
		t.Fatalf("effective minutes = %d, want %d", lt.YieldFloorMinutes(), DefaultYieldFloorMinutes)
	}
}

// --- yield floor with custom config ---

func TestYieldFloor_CustomConfig(t *testing.T) {
	clk := time.Unix(1000, 0)
	g := DefaultLoopGuard()
	g.RunBar = 999            // disable loop detection entirely
	g.YieldFloorTokPerMin = 5 // very low floor
	g.YieldFloorMinutes = 3   // short window for the test

	lt := NewLoopTracker(g)
	clkVal := clk
	lt.now = func() time.Time { return clkVal }
	if lt.YieldFloorTokPerMin() != 5 {
		t.Fatalf("custom floor = %v, want 5", lt.YieldFloorTokPerMin())
	}

	// First call: elapsed=1 min < window=3. Sets breach timer, no strike.
	clkVal = clkVal.Add(1 * time.Minute)
	strike, _ := lt.CheckYieldFloor("custom", 1*time.Minute, 3) // 3 tok/min < floor of 5
	if strike {
		t.Fatalf("elapsed < window: no strike yet")
	}

	// Advance past the 3-min breach window.
	clkVal = clkVal.Add(4 * time.Minute) // now t=1005, breach lasted 4 min >= 3 min window
	strike, ev := lt.CheckYieldFloor("custom", 5*time.Minute, 3)
	if !strike {
		t.Fatalf("expected strike at custom floor 5 tok/min after breach >= 3 min")
	}
	if ev.Reason != PenaltyReasonYield {
		t.Fatalf("ev.Reason=%q want %q", ev.Reason, PenaltyReasonYield)
	}
}

// --- yield floor disabled when minutes == 0 (after default applied) ---

func TestYieldFloor_MinutesZeroUsesDefault(t *testing.T) {
	g := DefaultLoopGuard()
	g.RunBar = 999
	g.YieldFloorMinutes = 0 // should fall back to default 15

	lt := NewLoopTracker(g)
	if lt.YieldFloorMinutes() != DefaultYieldFloorMinutes {
		t.Fatalf("zero minutes should fall back to %d, got %d", DefaultYieldFloorMinutes, lt.YieldFloorMinutes())
	}
}

// --- yield floor disabled when tokPerMin is zero (after default applied) ---

func TestYieldFloor_TokPerMinZeroUsesDefault(t *testing.T) {
	clk := time.Unix(1000, 0)
	g := DefaultLoopGuard()
	g.RunBar = 999
	g.YieldFloorTokPerMin = 0 // should fall back to default

	lt := NewLoopTracker(g)
	if lt.YieldFloorTokPerMin() != DefaultYieldFloorTokPerMin {
		t.Fatalf("zero tok/min should fall back to %v, got %v", DefaultYieldFloorTokPerMin, lt.YieldFloorTokPerMin())
	}

	// Even a glacial request at the default floor should not strike because
	// we haven't advanced the clock far enough for the 15 min window.
	clk = clk.Add(5 * time.Minute)
	strike, _ := lt.CheckYieldFloor("glacial", 5*time.Minute, 2)
	if strike {
		t.Fatalf("5 min < 15 min default window: should not strike")
	}
}

// --- InflightYield computes correctly ---

func TestInflightYield_Computation(t *testing.T) {
	g := DefaultLoopGuard()
	lt := NewLoopTracker(g)

	tests := []struct {
		name     string
		elapsed  time.Duration
		produced int64
		want     float64
	}{
		{"10tok/min", time.Minute, 10, 10},
		{"5tok/min", 2 * time.Minute, 10, 5},
		{"10tok/min-fast", 30 * time.Second, 5, 10},
		{"600tok/hr", time.Hour, 600, 10},    // 600 tokens in 1 hour = 10 tok/min
		{"zero-elapsed", 0, 100, 0},          // zero elapsed
		{"zero-produced", time.Minute, 0, 0}, // zero produced
	}

	for _, tc := range tests {
		got := lt.InflightYield(tc.elapsed, tc.produced)
		if got != tc.want {
			t.Fatalf("%s: InflightYield(%v, %d) = %v, want %v", tc.name, tc.elapsed, tc.produced, got, tc.want)
		}
	}
}

// --- nil LoopTracker: InflightYield and yieldFloorMinutes are inert ---

func TestNilTracker_YieldMethods(t *testing.T) {
	var lt *LoopTracker
	if lt.InflightYield(time.Minute, 100) != 0 {
		t.Fatalf("nil tracker InflightYield must return 0")
	}
	if lt.YieldFloorTokPerMin() != 0 {
		t.Fatalf("nil tracker YieldFloorTokPerMin must return 0")
	}
	if lt.YieldFloorMinutes() != 0 {
		t.Fatalf("nil tracker YieldFloorMinutes must return 0")
	}
}
