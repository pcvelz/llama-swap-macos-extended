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

// The cooldown is just the cooldown (user ruling), at model level AND slot
// level, with ONE duration: the model's swapGraceSeconds. A resident model, or
// a session's slot, inside its cooldown stays put. The only exits are
// FinishCooldown (one-shot, ends the model cooldown AND the slot cooldowns) and
// a higher-rank arrival whose tier Preempts and strictly outranks the protected
// traffic. Protection means "not DISPLACED": a request that FITS beside the
// cooling slot is granted at once; only one whose grant would displace a
// cooling slot's KV is held. A looping session does not refresh its cooldown.
//
// Slot scenarios run on the kvwarm geometry (kvwarm_test.go): pool 1000,
// A=750 and B=370 cannot share it, A=500 and B=300 can.

const (
	cdPool  = 1000
	cdBig   = kvwA // 750
	cdOver  = kvwB // 370: overflows the pool beside cdBig
	cdFitA  = 500
	cdFitB  = 300 // fits the pool beside cdFitA
	cdSessA = "c83f32d8-5a1e-4c2b-9d47-0e6f1a2b3c4d"
	cdSessB = "7be1a094-2f33-4e58-8c10-91d5e6a7b8c9"
	cdSessC = "5d0c2e71-8a64-4f19-b3a2-6c7d8e9f0a1b"
	cdGrace = 600
)

type cooldownRig struct {
	t     *testing.T
	eff   *fakeEffects
	s     *FIFO
	start time.Time
	clock time.Time
}

// newCooldownRig builds one pool model "m" with the given swap and slot grace.
// maxParallelLargePrefill is 1 (the cq27 value); newCooldownRigLarge sets it.
func newCooldownRig(t *testing.T, swapGrace, slotGrace int) *cooldownRig {
	t.Helper()
	return newCooldownRigLarge(t, swapGrace, slotGrace, 1)
}

// Real values (llama-cm llama/llama-swap.yaml): concurrencyLimit 2 everywhere;
// cq27 (Qwen3.8-27B) maxParallelLargePrefill 1, slotGraceSeconds 600; cq35
// maxParallelLargePrefill 2, slotGraceSeconds 0; swapGraceSeconds 600 global.
// Token sizes are scaled down (pool 262144 -> 1000, threshold 8192 -> 100).
// slotGrace is NOT read: slotGraceSeconds was removed 2026-10-18 (the config
// loader rejects > 0) and the scheduler has no such input; the parameter only
// names the production shape so a test can prove the shape behaves the same.
func newCooldownRigLarge(t *testing.T, swapGrace, slotGrace, maxLarge int) *cooldownRig {
	t.Helper()
	withLargePrefillThreshold(t, 100)
	eff := newFakeEffects()
	eff.states["m"] = process.StateReady
	models := map[string]config.ModelConfig{
		"m": {ConcurrencyLimit: 2, MaxParallelLargePrefill: maxLarge, SwapGraceSeconds: swapGrace},
	}
	cfg := config.FifoConfig{KVPoolTokens: map[string]int{"m": cdPool}}
	t0 := time.Unix(1_800_000_000, 0)
	r := &cooldownRig{t: t, eff: eff, start: t0, clock: t0}
	r.s = NewFIFO("test", logmon.NewWriter(io.Discard), &stubPlanner{}, cfg, models, eff)
	r.s.now = func() time.Time { return r.clock }
	return r
}

func (r *cooldownRig) req(session string, tokens int, tier swaputil.Tier) {
	r.s.OnRequest(HandlerReq{Model: "m", Session: session, EstimatedTokens: tokens, Tier: tier})
}

func (r *cooldownRig) done(session string, tokens int, looping bool) {
	r.s.OnServeDone(ServeDoneEvent{ModelID: "m", Session: session, EstimatedTokens: tokens, Tier: swaputil.DefaultTier, Looping: looping})
}

func (r *cooldownRig) advance(d time.Duration) {
	r.clock = r.clock.Add(d)
	r.s.OnTick()
}

func (r *cooldownRig) wantServed(want int, why string) {
	r.t.Helper()
	if got := r.eff.served("m"); got != want {
		r.t.Fatalf("%s: served=%d want %d (queued=%d)", why, got, want, len(r.s.queued))
	}
}

// --- model level -----------------------------------------------------------

// modelContinuousFixture: a (60s grace) served and idle, b (evicts a) queued.
func modelContinuousFixture(t *testing.T) (*FIFO, *fakeEffects, *time.Time) {
	t.Helper()
	eff := newFakeEffects()
	eff.states["a"] = process.StateReady
	eff.states["b"] = process.StateStopped
	eff.states["c"] = process.StateStopped
	clk := time.Unix(1000, 0)
	grace := map[string]time.Duration{"a": 60 * time.Second}
	// NO SetSwapStarvationSeconds: the default IS the contract.
	s := newFIFOGrace(&stubPlanner{evict: map[string][]string{"b": {"a"}, "c": {"a"}}}, eff, grace, &clk)

	s.OnRequest(req("a"))
	s.OnServeDone(ServeDoneEvent{ModelID: "a"})
	clk = clk.Add(10 * time.Second)
	s.OnRequest(reqCh("b"))
	return s, eff, &clk
}

func TestCooldownUnified_ModelNoStarvationValveByDefault(t *testing.T) {
	s, eff, clk := modelContinuousFixture(t)

	// 10 cycles = 200s of continuous a traffic, far past a's 60s grace.
	if at := driveContinuousA(s, eff, clk, 10); !at.IsZero() {
		t.Fatalf("the cooldown has no timeout valve: b's swap started %s into continuous a traffic", at.Sub(time.Unix(1010, 0)))
	}
	if got := eff.startsFor("b"); got != 0 {
		t.Fatalf("b must not start while a is in cooldown, starts=%d", got)
	}

	s.FinishCooldown()
	s.OnTick()
	if got := eff.startsFor("b"); got != 1 {
		t.Fatalf("FinishCooldown must end the model cooldown and let b swap, starts=%d", got)
	}
}

func TestCooldownUnified_ModelPriorityArrivalBreaks(t *testing.T) {
	s, eff, clk := modelContinuousFixture(t)

	// A few cycles of continuous a traffic, inside any valve that exists today,
	// so this stays GREEN before and after the valve is deleted.
	if at := driveContinuousA(s, eff, clk, 2); !at.IsZero() {
		t.Fatalf("b started during the cooldown at %s", at.Sub(time.Unix(1010, 0)))
	}
	s.OnRequest(tierReq("c", tierPriority))
	if got := eff.startsFor("c"); got != 1 {
		t.Fatalf("a Preempts arrival must break the model cooldown at once, StartSwap(c)=%d", got)
	}
}

// --- slot level ------------------------------------------------------------

// The slot cooldown IS swapGraceSeconds (slotGraceSeconds unset): not the 120s
// kv-warm window.
func TestCooldownUnified_SlotWindowIsTheCooldown(t *testing.T) {
	r := newCooldownRig(t, cdGrace, 0)

	r.req(cdSessA, cdBig, swaputil.DefaultTier)
	r.wantServed(1, "A alone")
	r.done(cdSessA, cdBig, false)

	r.advance(150 * time.Second) // past kvWarmWindow, inside swapGraceSeconds
	r.req(cdSessB, cdOver, swaputil.DefaultTier)
	r.wantServed(1, "B must park: A is 150s into a 600s cooldown and B would displace its KV")

	r.advance(451 * time.Second) // A idle 601s
	r.wantServed(2, "B granted once A's 600s cooldown is over")
}

// No quantum (30m) and no ceiling (90m): only Finish or a priority arrival.
func TestCooldownUnified_SlotNoQuantumNoCeiling(t *testing.T) {
	r := newCooldownRig(t, cdGrace, 0)

	r.req(cdSessA, cdBig, swaputil.DefaultTier)
	r.wantServed(1, "A alone")
	r.req(cdSessB, cdOver, swaputil.DefaultTier) // parks behind A in flight
	r.done(cdSessA, cdBig, false)

	// 12 cycles of 510s = 102 minutes of A turns with gaps < 600s.
	for i := 0; i < 12; i++ {
		r.advance(500 * time.Second)
		r.wantServed(1+i, "B must stay parked while A keeps refreshing its cooldown")
		r.req(cdSessA, cdBig, swaputil.DefaultTier)
		r.wantServed(2+i, "A's own continuation inside its cooldown is granted")
		r.advance(10 * time.Second)
		r.done(cdSessA, cdBig, false)
	}
	if r.clock.Sub(r.start) <= 90*time.Minute {
		t.Fatalf("scenario must outlast the 90m ceiling")
	}
	r.advance(time.Second)
	r.wantServed(13, "B still parked after 90+ minutes: no quantum, no ceiling")

	r.s.FinishCooldown()
	r.s.OnTick()
	r.wantServed(14, "FinishCooldown ends the slot cooldown and grants B")
}

func TestCooldownUnified_SlotPriorityArrivalBreaks(t *testing.T) {
	r := newCooldownRig(t, cdGrace, 0)

	r.req(cdSessA, cdBig, swaputil.DefaultTier)
	r.done(cdSessA, cdBig, false)
	r.advance(10 * time.Second)
	r.req(cdSessB, cdOver, swaputil.DefaultTier)
	r.wantServed(1, "default-tier B parks behind A's cooldown")

	r.req(cdSessC, cdOver, tierPriority)
	r.wantServed(2, "a Preempts arrival that outranks A is granted despite A's cooldown")
}

// A cold LARGE request that FITS beside the cooling slot runs at once: the
// cooldown protects against displacement, it does not serialise sessions.
func TestCooldownUnified_SlotFittingSessionRunsInParallel(t *testing.T) {
	for _, tc := range []struct {
		name                string
		slotGrace, maxLarge int
	}{
		{"cq27-shape-swap600-slot600-large1", 600, 1},
		{"cq35-shape-swap600-slot0-large2", 0, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCooldownRigLarge(t, cdGrace, tc.slotGrace, tc.maxLarge)

			r.req(cdSessA, cdFitA, swaputil.DefaultTier)
			r.wantServed(1, "A alone")
			r.done(cdSessA, cdFitA, false)

			r.advance(10 * time.Second)
			r.req(cdSessB, cdFitB, swaputil.DefaultTier)
			r.wantServed(2, "B fits beside A's cooling KV (500+300<=1000): granted immediately")
			if _, ok := r.s.kvResidents["m"][cdSessA]; !ok {
				t.Fatalf("A's cooling KV was displaced by a request that fits beside it")
			}
		})
	}
}

// A looping session does not refresh its slot cooldown: it runs down from the
// last NON-looping completion.
func TestCooldownUnified_SlotLoopingDoesNotRefresh(t *testing.T) {
	r := newCooldownRig(t, cdGrace, 0)

	r.req(cdSessA, cdBig, swaputil.DefaultTier)
	r.done(cdSessA, cdBig, false) // the last productive completion, t0

	served := 1
	for i := 0; i < 19; i++ { // looping turns every 30s, the last ending at t0+570
		r.advance(5 * time.Second)
		r.req(cdSessA, cdBig, swaputil.DefaultTier)
		served++
		if i == 0 {
			r.req(cdSessB, cdOver, swaputil.DefaultTier) // overflows: parks
		}
		r.wantServed(served, "A's looping turn is granted, B stays parked")
		r.advance(25 * time.Second)
		r.done(cdSessA, cdBig, true)
		r.wantServed(served, "B stays parked inside the 600s measured from t0")
	}
	if got := r.clock.Sub(r.start); got != 570*time.Second {
		t.Fatalf("fixture drift: clock at t0+%s want t0+570s", got)
	}

	r.advance(31 * time.Second) // t0+601s; the last looping completion was 31s ago
	r.wantServed(served+1, "B granted: 601s since A's last non-looping completion (looping must not refresh)")
}

// A finish with nothing cooling is spent: it must not pre-end a later cooldown.
func TestCooldownUnified_SlotFinishIsOneShot(t *testing.T) {
	r := newCooldownRig(t, cdGrace, 0)

	r.s.FinishCooldown()
	r.advance(time.Second) // the run loop's next decision consumes it

	r.req(cdSessA, cdBig, swaputil.DefaultTier)
	r.wantServed(1, "A alone")
	r.done(cdSessA, cdBig, false)
	r.advance(10 * time.Second)
	r.req(cdSessB, cdOver, swaputil.DefaultTier)
	r.wantServed(1, "B parks: the earlier finish was spent on nothing and must not end this cooldown")
}

// Slot-count displacement: with concurrencyLimit 2 (--parallel 2) and both
// slots holding a cooling session's KV, a third session fits the pool TOKENS
// but has no free slot - granting it lands on a cooling slot and overwrites its
// KV. It parks until a slot's cooldown ends, a finish, or a Preempts outrank.
func TestCooldownUnified_SlotCountDisplacement_cq35Shape(t *testing.T) {
	slotCountDisplacement(t, 0, 2)
}

func TestCooldownUnified_SlotCountDisplacement_cq27Shape(t *testing.T) {
	slotCountDisplacement(t, 600, 1)
}

func slotCountDisplacement(t *testing.T, slotGrace, maxLarge int) {
	const tok = 300 // 300+300+200 <= cdPool: tokens are never the constraint

	// bothCooling: A done at t0, C done at t0+100s, B parked at t0+110s.
	bothCooling := func(t *testing.T) *cooldownRig {
		r := newCooldownRigLarge(t, cdGrace, slotGrace, maxLarge)
		r.req(cdSessA, tok, swaputil.DefaultTier)
		r.done(cdSessA, tok, false)
		r.advance(100 * time.Second)
		r.req(cdSessC, tok, swaputil.DefaultTier)
		r.done(cdSessC, tok, false)
		r.advance(10 * time.Second)
		r.wantServed(2, "A and C served")
		r.req(cdSessB, 200, swaputil.DefaultTier)
		r.wantServed(2, "B must park: it fits the pool tokens but both slots hold a cooling KV")
		return r
	}

	t.Run("parks-until-a-cooldown-ends", func(t *testing.T) {
		r := bothCooling(t)
		r.advance(480 * time.Second) // t0+590: A idle 590s, still cooling
		r.wantServed(2, "B still parked before any cooldown ends")
		r.advance(11 * time.Second) // t0+601: A's cooldown over, C's (t0+700) is not
		r.wantServed(3, "B granted onto A's slot once A passes 600s idle")
	})

	t.Run("finish-releases", func(t *testing.T) {
		r := bothCooling(t)
		r.s.FinishCooldown()
		r.s.OnTick()
		r.wantServed(3, "FinishCooldown ends the slot cooldowns and grants B")
	})

	t.Run("preempts-outranks", func(t *testing.T) {
		r := bothCooling(t)
		r.req("a41e6c02-93bd-4d7f-8e15-2f0b7c6d5a39", 200, tierPriority)
		r.wantServed(3, "a Preempts arrival that outranks the cooling sessions is granted")
	})

	t.Run("one-cooling-leaves-a-free-slot", func(t *testing.T) {
		r := newCooldownRigLarge(t, cdGrace, slotGrace, maxLarge)
		r.req(cdSessA, tok, swaputil.DefaultTier)
		r.done(cdSessA, tok, false)
		r.advance(10 * time.Second)
		r.req(cdSessB, 200, swaputil.DefaultTier)
		r.wantServed(2, "B granted: only one slot is cooling, a free slot exists")
	})
}

// Acceptance target. cq35 shape (concurrencyLimit 2, maxParallelLargePrefill 2,
// slotGraceSeconds 0, swapGraceSeconds 600, a pool that fits both prompts): TWO
// sessions run large turns at the same time, also while one of them is in its
// cooldown between turns. cq27 shape (maxParallelLargePrefill 1,
// slotGraceSeconds 600): ONE large turn in flight at a time, and a cooling
// session's slot is not displaced.
func TestCooldownUnified_TwoSessionsParallel_cq35Shape(t *testing.T) {
	r := newCooldownRigLarge(t, cdGrace, 0, 2)

	r.req(cdSessA, cdFitA, swaputil.DefaultTier)
	r.req(cdSessB, cdFitB, swaputil.DefaultTier)
	r.wantServed(2, "two large turns in flight at the same time")

	// A ends its turn and cools; B keeps working; A's next turn runs beside B.
	r.done(cdSessA, cdFitA, false)
	r.advance(20 * time.Second)
	r.req(cdSessA, cdFitA, swaputil.DefaultTier)
	r.wantServed(3, "A's next turn runs beside B's in-flight turn")

	// B ends and cools while A runs: B's next turn is granted beside A.
	r.done(cdSessB, cdFitB, false)
	r.advance(20 * time.Second)
	r.req(cdSessB, cdFitB, swaputil.DefaultTier)
	r.wantServed(4, "B's next turn runs beside A while B was cooling")
}

func TestCooldownUnified_OneSessionAtATime_cq27Shape(t *testing.T) {
	r := newCooldownRigLarge(t, cdGrace, 600, 1)

	r.req(cdSessA, cdFitA, swaputil.DefaultTier)
	r.req(cdSessB, cdFitB, swaputil.DefaultTier)
	r.wantServed(1, "maxParallelLargePrefill 1: only one large turn in flight")

	// A ends: B is granted into the free slot; A's cooling KV is not displaced.
	r.done(cdSessA, cdFitA, false)
	r.wantServed(2, "B runs once A's turn ends")
	if _, ok := r.s.kvResidents["m"][cdSessA]; !ok {
		t.Fatalf("A's cooling slot was displaced by B")
	}

	// A's next turn waits for B's large prefill (one at a time).
	r.advance(20 * time.Second)
	r.req(cdSessA, cdFitA, swaputil.DefaultTier)
	r.wantServed(2, "A's next turn parks behind B's in-flight large turn")
}
