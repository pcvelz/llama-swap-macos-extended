package scheduler

import "time"

// driveContinuousA replays the 2026-07-15 shape: a serves back-to-back with a
// 2s gap so a's idle streak never reaches its grace, while b sits parked.
// Returns the clock at which b's swap started, or zero if it never did.
// (The swapStarvationSeconds valve tests that used this were removed
// 2026-10-18 with the valve; the helper stays for the cooldown tests.)
func driveContinuousA(s *FIFO, eff *fakeEffects, clk *time.Time, cycles int) time.Time {
	for cycle := 0; cycle < cycles; cycle++ {
		*clk = clk.Add(2 * time.Second)
		s.OnTick()
		if eff.startsFor("b") == 1 {
			return *clk
		}
		s.OnRequest(req("a"))
		*clk = clk.Add(18 * time.Second)
		s.OnServeDone(ServeDoneEvent{ModelID: "a"})
		if eff.startsFor("b") == 1 {
			return *clk
		}
	}
	return time.Time{}
}
