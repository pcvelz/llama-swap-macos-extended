package scheduler

import (
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/process"
)

// A status read (GET /slots, /props, /metrics) on the READY resident must not
// restart its swap-grace: driven by hand on the isolated box 2026-09-10
// (llama-cm docs/research/2026-09-10-cooldown-dogfood-ledger.md O16), a
// 20 s cooldown sat at 12 s for 72 seconds because a /slots read every 8 s
// completed as a normal request and reset idleSince. Every /slots poller
// (the menu-bar helper, cm-menu's activity reader) therefore held the
// resident inside its grace for as long as it was watched.
func TestFIFO_Cooldown_StatusReadOnResidentDoesNotRestartIt(t *testing.T) {
	s, eff, clk := cooldownFixture(t)

	*clk = clk.Add(10 * time.Second)
	s.OnRequest(reqCh("b"))
	before := s.Cooldown()
	if before == nil {
		t.Fatalf("expected a cooldown on a")
	}

	// A status read on the resident is served (a is ready) and completes.
	*clk = clk.Add(5 * time.Second)
	read := req("a")
	read.ConcurrencyExempt = true
	read.StatusRead = true
	s.OnRequest(read)
	if got := eff.served("a"); got != 2 {
		t.Fatalf("served(a)=%d want 2 (the read is served immediately)", got)
	}
	s.OnServeDone(ServeDoneEvent{ModelID: "a", StatusRead: true})
	s.OnTick() // the snapshot refreshes on the tick, as in production

	after := s.Cooldown()
	if after == nil {
		t.Fatalf("cooldown vanished on a status read")
	}
	if after.RemainingSeconds != before.RemainingSeconds-5 {
		t.Fatalf("remaining=%d want %d: a status read must not restart the cooldown", after.RemainingSeconds, before.RemainingSeconds-5)
	}

	// And the swap still proceeds when the ORIGINAL grace elapses.
	*clk = clk.Add(46 * time.Second) // 61 s idle since the real turn
	s.OnTick()
	if got := eff.startsFor("b"); got != 1 {
		t.Fatalf("StartSwap(b)=%d want 1 once the original grace elapsed", got)
	}
}

// A status read never counts as in-flight work either: a resident with only
// a status read outstanding is idle for the swap (and for capacity).
func TestFIFO_StatusReadDoesNotCountAsInFlight(t *testing.T) {
	s, _ := newFIFOWithLimit(t, "a", 1)
	_ = process.StateReady

	read := req("a")
	read.ConcurrencyExempt = true
	read.StatusRead = true
	s.OnRequest(read)
	if s.inFlight["a"] != 0 {
		t.Fatalf("inFlight[a]=%d want 0 for a status read", s.inFlight["a"])
	}
	s.OnServeDone(ServeDoneEvent{ModelID: "a", StatusRead: true})
	if s.inFlight["a"] != 0 {
		t.Fatalf("inFlight[a]=%d want 0 after the read completes", s.inFlight["a"])
	}
}
