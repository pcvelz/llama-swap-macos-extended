package scheduler

import (
	"sync/atomic"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// Incident llama-cm 2026-09-25 (phantom holder, fault 2): the preemption
// bookkeeping dropped the LAST granted entry on any completion, not the one
// that finished. After X finished, X's stale handle stayed in the victim list
// and the still-running Y dropped out of it: the next preemption "booted" X
// (a no-op on a finished request), freed nothing, and could never reach Y.
func TestFIFO_ServeDoneDropsTheFinishedHoldersEntry(t *testing.T) {
	s, _ := newFIFOWithLimit(t, "a", 2)

	x, bootedX := grantedTierReq("a", tierBg)
	x.Preempted = new(atomic.Bool)
	y, bootedY := grantedTierReq("a", tierBg)
	y.Preempted = new(atomic.Bool)
	s.OnRequest(x)
	s.OnRequest(y)

	// X was granted FIRST and finishes first.
	s.OnServeDone(ServeDoneEvent{ModelID: "a", Holder: x.Preempted})

	arrival := req("a")
	arrival.Tier = swaputil.DefaultTier
	s.OnRequest(arrival)
	// One place is free after X finished: the arrival is granted, nothing
	// needs booting yet.
	if *bootedX || *bootedY {
		t.Fatalf("booted a holder although a place was free (X=%v Y=%v)", *bootedX, *bootedY)
	}
	// A second arrival must boot the RUNNING lower-rank holder Y.
	arrival2 := req("a")
	arrival2.Tier = swaputil.DefaultTier
	s.OnRequest(arrival2)
	if *bootedX {
		t.Fatalf("preemption fired the finished request X's stale handle (Y booted=%v): the victim list kept X and lost Y", *bootedY)
	}
	if !*bootedY {
		t.Fatalf("the running background holder Y was not booted for a default-tier arrival at the cap")
	}
}

// A phantom holder (still served, but its upstream slot reads idle) gives its
// cap place back once; its eventual completion must not give it back again.
func TestFIFO_CapReleaseOnlyGivesThePlaceBackExactlyOnce(t *testing.T) {
	s, eff := newFIFOWithLimit(t, "a", 2)

	p := req("a")
	p.Preempted = new(atomic.Bool)
	q := req("a")
	q.Preempted = new(atomic.Bool)
	s.OnRequest(p)
	s.OnRequest(q)

	r := req("a")
	s.OnRequest(r)
	assertParkedAtCapacity(t, s, r)

	s.OnServeDone(ServeDoneEvent{ModelID: "a", Holder: p.Preempted, CapReleaseOnly: true})
	if got := eff.served("a"); got != 3 {
		t.Fatalf("served(a)=%d after the phantom's cap release, want 3 (the parked request takes the freed place)", got)
	}

	// The phantom's request finally ends: its place was already given back.
	s.OnServeDone(ServeDoneEvent{ModelID: "a", Holder: p.Preempted, CapReleased: true})
	if got := s.inFlight["a"]; got != 2 {
		t.Fatalf("inFlight(a)=%d after the phantom finished, want 2 (Q and R still hold their places)", got)
	}
	sreq := req("a")
	s.OnRequest(sreq)
	assertParkedAtCapacity(t, s, sreq)
}
