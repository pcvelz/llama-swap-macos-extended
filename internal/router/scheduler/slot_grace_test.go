package scheduler

import (
	"io"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// slotGraceRig sets up a FIFO with a controllable clock and per-model
// slotGraceSeconds, for deterministic slot-grace tests.
type slotGraceRig struct {
	t     *testing.T
	eff   *fakeEffects
	s     *FIFO
	clock time.Time
}

func newSlotGraceRig(t *testing.T, graceSec int) *slotGraceRig {
	t.Helper()
	eff := newFakeEffects()
	eff.states["m"] = process.StateReady
	models := map[string]config.ModelConfig{
		"m": {ConcurrencyLimit: 2, SlotGraceSeconds: graceSec},
	}
	r := &slotGraceRig{t: t, eff: eff, clock: time.Unix(1_000_000, 0)}
	s := NewFIFO("test", logmon.NewWriter(io.Discard), &stubPlanner{}, config.FifoConfig{}, models, eff)
	r.s = s
	s.now = func() time.Time { return r.clock }
	return r
}

func (r *slotGraceRig) coldReq(session string, tokens int) {
	r.s.OnRequest(HandlerReq{
		Model:           "m",
		Session:         session,
		EstimatedTokens: tokens,
		Tier:            swaputil.DefaultTier,
	})
}

func (r *slotGraceRig) warmDone(session string, tokens int) {
	r.s.OnServeDone(ServeDoneEvent{
		ModelID:         "m",
		Session:         session,
		EstimatedTokens: tokens,
	})
}

func (r *slotGraceRig) advance(d time.Duration) {
	r.clock = r.clock.Add(d)
	r.s.OnTick()
}

func (r *slotGraceRig) wantServed(want int, why string) {
	r.t.Helper()
	if got := r.eff.served("m"); got != want {
		r.t.Fatalf("%s: served=%d want %d", why, got, want)
	}
}

func (r *slotGraceRig) wantQueuedCount(want int, why string) {
	r.t.Helper()
	if got := len(r.s.queued); got != want {
		r.t.Fatalf("%s: queue len=%d want %d", why, got, want)
	}
}

// --- Test: cold request parks while another session is slot-warm ---

func TestSlotGrace_ColdParksWhileWarm(t *testing.T) {
	r := newSlotGraceRig(t, 60) // 60s grace

	// Session A makes a large turn and finishes.
	r.coldReq("A", 10000)
	r.wantServed(1, "A's first request")
	r.warmDone("A", 10000)

	// Session B sends a cold large request while A is still warm (10s elapsed).
	r.advance(10 * time.Second)
	r.coldReq("B", 10000)
	r.wantServed(1, "B must park: A is slot-warm")
	r.wantQueuedCount(1, "one request queued")
	if len(r.s.queued) > 0 && r.s.queued[0].parkReason != ParkKVWarm {
		t.Fatalf("park_reason=%q want %q", func() string {
			if len(r.s.queued) > 0 {
				return r.s.queued[0].parkReason
			}
			return ""
		}(), ParkKVWarm)
	}
}

// --- Test: warm continuation admitted inside window ---

func TestSlotGrace_WarmContinuationAdmitted(t *testing.T) {
	r := newSlotGraceRig(t, 60)

	r.coldReq("A", 10000)
	r.wantServed(1, "A's first request")
	r.warmDone("A", 10000)

	// B parks.
	r.advance(10 * time.Second)
	r.coldReq("B", 10000)
	r.wantQueuedCount(1, "B parked behind A's warm window")

	// A's own continuation arrives: must be admitted despite B queued.
	r.coldReq("A", 5000)
	r.wantServed(2, "A's warm continuation is admitted")
}

// --- Test: request admitted after window expiry ---

func TestSlotGrace_WindowExpiryReleasesQueue(t *testing.T) {
	r := newSlotGraceRig(t, 60)

	r.coldReq("A", 10000)
	r.wantServed(1, "A's first request")
	r.warmDone("A", 10000)

	r.advance(10 * time.Second)
	r.coldReq("B", 10000)
	r.wantQueuedCount(1, "B parked")

	// Advance past the grace window.
	r.advance(55 * time.Second)
	r.wantServed(2, "B admitted after A's slot-warm window expired")
	r.wantQueuedCount(0, "queue drained")
}

// --- Test: warm cache not displaced inside window ---

func TestSlotGrace_KVCacheProtectedInsideWindow(t *testing.T) {
	// This test asserts that the KV displacement logic does not purge a cache
	// whose owning session is inside its slot-grace window. Inside the window
	// the newcomer parks instead.
	eff := newFakeEffects()
	eff.states["m"] = process.StateReady
	models := map[string]config.ModelConfig{
		"m": {ConcurrencyLimit: 2, SlotGraceSeconds: 60},
	}
	cfg := config.FifoConfig{KVPoolTokens: map[string]int{"m": 25000}}
	s := NewFIFO("test", logmon.NewWriter(io.Discard), &stubPlanner{}, cfg, models, eff)
	clock := time.Unix(1_000_000, 0)
	s.now = func() time.Time { return clock }

	// A makes a large turn and finishes.
	s.OnRequest(HandlerReq{Model: "m", Session: "A", EstimatedTokens: 10000, Tier: swaputil.DefaultTier})
	if got := eff.served("m"); got != 1 {
		t.Fatalf("served=%d want 1 (A's first request)", got)
	}
	s.OnServeDone(ServeDoneEvent{ModelID: "m", EstimatedTokens: 10000, Session: "A", Tier: swaputil.DefaultTier})

	// B arrives while A is warm (10s elapsed < 60s grace): must park.
	clock = clock.Add(10 * time.Second)
	s.OnRequest(HandlerReq{Model: "m", Session: "B", EstimatedTokens: 10000, Tier: swaputil.DefaultTier})
	if got := eff.served("m"); got != 1 {
		t.Fatalf("served=%d want 1 (B parked behind A's warm window)", got)
	}
	if len(s.queued) != 1 || s.queued[0].parkReason != ParkKVWarm {
		t.Fatalf("B must park: served=%d, queue=%d reason=%q", eff.served("m"), len(s.queued), func() string {
			if len(s.queued) > 0 {
				return s.queued[0].parkReason
			}
			return ""
		}())
	}

	// Advance past the grace window: B is admitted.
	clock = clock.Add(60 * time.Second)
	s.OnTick()
	if got := eff.served("m"); got != 2 {
		t.Fatalf("served=%d want 2 (B admitted after window)", got)
	}

	// A's residency should still exist (was protected during the hold).
	if _, ok := s.kvResidents["m"]["A"]; !ok {
		t.Fatal("A's KV cache was displaced while A was inside its slot-grace window")
	}
}

// --- Test: higher-rank cold request admitted despite the hold ---

func TestSlotGrace_HigherRankBypassesHold(t *testing.T) {
	r := newSlotGraceRig(t, 60)

	r.coldReq("A", 10000)
	r.wantServed(1, "A's first request")
	r.warmDone("A", 10000)

	r.advance(10 * time.Second)
	r.coldReq("B", 10000)
	r.wantQueuedCount(1, "B parked behind A's warm window")

	// C arrives on a higher-rank tier: must bypass the slot-grace hold.
	priorityTier := swaputil.Tier{Name: "priority", Rank: 10, Preempts: true}
	cReq := HandlerReq{
		Model:           "m",
		Session:         "C",
		EstimatedTokens: 10000,
		Tier:            priorityTier,
	}
	r.s.OnRequest(cReq)

	// C is served (bypasses the hold). B stays queued.
	if got := r.eff.served("m"); got != 2 {
		t.Fatalf("served=%d want 2 (C bypasses slot-grace hold)", got)
	}
	r.wantQueuedCount(1, "B still queued behind C")
}

// --- Test: feature inert at 0 ---

func TestSlotGrace_InertAtZero(t *testing.T) {
	r := newSlotGraceRig(t, 0) // grace off

	r.coldReq("A", 10000)
	r.wantServed(1, "A's first request")
	r.warmDone("A", 10000)

	// B is admitted normally — no slot-grace hold.
	r.advance(1 * time.Second)
	r.coldReq("B", 10000)
	r.wantServed(2, "B admitted: slotGraceSeconds=0 means plain FIFO")
	r.wantQueuedCount(0, "nothing parked")
}

// --- Test: warm session's own continuation not counted as cold ---

func TestSlotGrace_WarmSessionNotBlockedByItself(t *testing.T) {
	r := newSlotGraceRig(t, 60)

	// A starts and finishes two turns back-to-back.
	r.coldReq("A", 10000)
	r.wantServed(1, "A's first request")
	r.warmDone("A", 10000)

	r.advance(5 * time.Second)
	r.coldReq("A", 8000)
	r.wantServed(2, "A's second warm turn")
	r.warmDone("A", 8000)

	// B arrives: A is still warm (10s total < 60s grace).
	r.advance(5 * time.Second)
	r.coldReq("B", 10000)
	r.wantQueuedCount(1, "B parked: A's window not yet expired")
}
