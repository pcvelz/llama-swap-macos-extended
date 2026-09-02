package scheduler

import (
	"testing"

	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// TestFIFO_Preempt_BootsOnlyEnoughToFreeOneSlot: a single higher-rank arrival
// at the concurrency cap needs exactly ONE slot, so it may boot exactly ONE
// eligible lower-rank holder. Booting every eligible holder on the model
// (llama-cm incident 2026-07-05-cq27-toolgap-eviction-reprefill-storm,
// 2026-09-01 follow-up)
// turned each rank-0 arrival on a 2-slot box into a double eviction: both
// interactive cq* sessions lost their prefill for a request that could only
// ever occupy one of the two slots.
func TestFIFO_Preempt_BootsOnlyEnoughToFreeOneSlot(t *testing.T) {
	s, eff := newFIFOWithLimit(t, "a", 2)

	bg1, booted1 := grantedTierReq("a", tierBg)
	bg2, booted2 := grantedTierReq("a", tierBg)
	s.OnRequest(bg1)
	s.OnRequest(bg2)
	if got := eff.served("a"); got != 2 {
		t.Fatalf("served(a)=%d want 2 (both background holders granted)", got)
	}

	arrival := req("a")
	arrival.Tier = swaputil.DefaultTier
	s.OnRequest(arrival)
	assertParkedAtCapacity(t, s, arrival)

	booted := 0
	if *booted1 {
		booted++
	}
	if *booted2 {
		booted++
	}
	if booted != 1 {
		t.Fatalf("booted %d background holders for ONE default-tier arrival, want exactly 1 (one slot needed, one victim)", booted)
	}
}
