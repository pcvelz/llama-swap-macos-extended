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

// Slot cooldown tests at production-sized token counts (10000 per large turn,
// the default 8192 large-prefill threshold). The slot cooldown IS the model's
// swapGraceSeconds (slotGraceSeconds was removed 2026-10-18, see
// cooldown_unified_test.go and llama-cm docs/intent/cooldown.md): a session
// that finished a turn keeps its slot for that long, and another session's
// request is held only if granting it would displace that slot - here by
// pool tokens: 10000 + 10000 overflows slotPool, 10000 + 4000 does not.

const slotPool = 15000

// slotGraceRig sets up a FIFO with a controllable clock and a per-model
// swapGraceSeconds, for deterministic slot-cooldown tests.
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
		"m": {ConcurrencyLimit: 2, SwapGraceSeconds: graceSec},
	}
	cfg := config.FifoConfig{KVPoolTokens: map[string]int{"m": slotPool}}
	r := &slotGraceRig{t: t, eff: eff, clock: time.Unix(1_000_000, 0)}
	s := NewFIFO("test", logmon.NewWriter(io.Discard), &stubPlanner{}, cfg, models, eff)
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

// --- Test: a request that would displace the cooling slot parks ---

func TestSlotGrace_ColdParksWhileWarm(t *testing.T) {
	r := newSlotGraceRig(t, 60) // 60s cooldown

	// Session A makes a large turn and finishes.
	r.coldReq("A", 10000)
	r.wantServed(1, "A's first request")
	r.warmDone("A", 10000)

	// Session B sends a request that overflows the pool beside A's cooling
	// prompt (10s elapsed).
	r.advance(10 * time.Second)
	r.coldReq("B", 10000)
	r.wantServed(1, "B must park: granting it would displace A's cooling slot")
	r.wantQueuedCount(1, "one request queued")
	if r.s.queued[0].parkReason != ParkKVWarm {
		t.Fatalf("park_reason=%q want %q", r.s.queued[0].parkReason, ParkKVWarm)
	}
}

// --- Test: warm continuation admitted inside the cooldown ---

func TestSlotGrace_WarmContinuationAdmitted(t *testing.T) {
	r := newSlotGraceRig(t, 60)

	r.coldReq("A", 10000)
	r.wantServed(1, "A's first request")
	r.warmDone("A", 10000)

	// B parks.
	r.advance(10 * time.Second)
	r.coldReq("B", 10000)
	r.wantQueuedCount(1, "B parked behind A's cooldown")

	// A's own continuation arrives: must be admitted despite B queued.
	r.coldReq("A", 5000)
	r.wantServed(2, "A's warm continuation is admitted")
}

// --- Test: request admitted after the cooldown ends ---

func TestSlotGrace_WindowExpiryReleasesQueue(t *testing.T) {
	r := newSlotGraceRig(t, 60)

	r.coldReq("A", 10000)
	r.wantServed(1, "A's first request")
	r.warmDone("A", 10000)

	r.advance(10 * time.Second)
	r.coldReq("B", 10000)
	r.wantQueuedCount(1, "B parked")

	// Advance past the cooldown.
	r.advance(55 * time.Second)
	r.wantServed(2, "B admitted after A's cooldown ended")
	r.wantQueuedCount(0, "queue drained")
}

// --- Test: a request that fits beside the cooling slot is not held ---

func TestSlotGrace_FittingRequestNotHeld(t *testing.T) {
	r := newSlotGraceRig(t, 60)

	r.coldReq("A", 10000)
	r.warmDone("A", 10000)

	r.advance(10 * time.Second)
	r.coldReq("B", 4000)
	r.wantServed(2, "10000 cooling + 4000 fits the 15000 pool: B runs beside A")
	if _, ok := r.s.kvResidents["m"]["A"]; !ok {
		t.Fatal("A's cooling slot was displaced by a request that fits beside it")
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
	r.wantQueuedCount(1, "B parked behind A's cooldown")

	// C arrives on a higher-rank tier: must bypass the cooldown.
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
		t.Fatalf("served=%d want 2 (C bypasses the cooldown)", got)
	}
	r.wantQueuedCount(1, "B still queued behind C")
}

// --- Test: no swap grace, no cooldown ---

func TestSlotGrace_InertAtZero(t *testing.T) {
	r := newSlotGraceRig(t, 0) // no swapGraceSeconds: no cooldown at either level

	r.coldReq("A", 10000)
	r.wantServed(1, "A's first request")
	r.warmDone("A", 10000)

	// B is admitted normally - nothing is cooling.
	r.advance(1 * time.Second)
	r.coldReq("B", 10000)
	r.wantServed(2, "B admitted: no swapGraceSeconds means plain FIFO")
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

	// B arrives: A is still cooling (10s total < 60s) and 8000 + 10000
	// overflows the pool.
	r.advance(5 * time.Second)
	r.coldReq("B", 10000)
	r.wantQueuedCount(1, "B parked: A's cooldown not yet over")
}

// --- Test: the sessions API reads the unified cooling record ---

func TestSlotGrace_SlotWarmUntilReportsCoolingSessions(t *testing.T) {
	r := newSlotGraceRig(t, 60)

	r.coldReq("A", 10000)
	if got := r.s.SlotWarmUntil(); len(got) != 0 {
		t.Fatalf("a session in flight is not cooling: %v", got)
	}
	r.warmDone("A", 10000)
	want := r.clock.Add(60 * time.Second).Format(time.RFC3339)
	if got := r.s.SlotWarmUntil()["A"]; got != want {
		t.Fatalf("SlotWarmUntil[A]=%q want %q (last completion + swap grace)", got, want)
	}
	r.advance(61 * time.Second)
	if got := r.s.SlotWarmUntil(); len(got) != 0 {
		t.Fatalf("an expired cooldown must not be reported: %v", got)
	}
}
