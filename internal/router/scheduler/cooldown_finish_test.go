package scheduler

import (
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/process"
)

// TestFIFO_Cooldown_StaleFinishBeforeFirstServe: a finish clicked while no
// cooldown is held must not pre-empt a LATER cooldown that begins on the
// resident's next served request. The stale intent is dropped by the next
// tick with nothing queued (the one-shot discipline), so when the resident
// serves again and goes idle inside a fresh grace, a cross-model request is
// held normally.
func TestFIFO_Cooldown_StaleFinishBeforeFirstServe(t *testing.T) {
	eff := newFakeEffects()
	eff.states["a"] = process.StateReady
	eff.states["b"] = process.StateStopped
	clk := time.Unix(1000, 0)
	grace := map[string]time.Duration{"a": 60 * time.Second}
	s := newFIFOGrace(&runningFilterPlanner{evict: map[string][]string{"b": {"a"}}}, eff, grace, &clk)

	// Nothing has been served yet: no idleSince, no cooldown.
	s.FinishCooldown() // stale click with nothing held
	if cd := s.Cooldown(); cd != nil {
		t.Fatalf("Cooldown()=%+v want nil before anything is served", cd)
	}

	// A tick with nothing queued drops the stale intent (one-shot).
	s.OnTick()

	// The resident serves and goes idle inside a fresh grace.
	s.OnRequest(req("a"))
	s.OnServeDone(ServeDoneEvent{ModelID: "a"})
	if cd := s.Cooldown(); cd == nil || cd.NextModel != "" {
		t.Fatalf("Cooldown()=%+v want a fresh no-waiter cooldown", cd)
	}

	// A cross-model request must be HELD by the fresh grace, not bypassed
	// by the stale finish.
	s.OnRequest(reqCh("b"))
	if got := eff.startsFor("b"); got != 0 {
		t.Fatalf("StartSwap(b)=%d want 0: a stale finish must not pre-empt the later cooldown", got)
	}
	if cd := s.Cooldown(); cd == nil || cd.NextModel != "b" {
		t.Fatalf("Cooldown()=%+v want evictee=a next=b still held", cd)
	}
}
