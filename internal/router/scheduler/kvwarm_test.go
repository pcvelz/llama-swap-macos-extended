package scheduler

import (
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/process"
)

// The slot cooldown (kvwarm.go), on the geometry of the 2026-10-06 cq27 thrash
// scaled to a pool of 1000: session A's prompt 198k and session B's 97k of a
// 262144 pool become 750 and 370 - together 1120, so the two cannot share the
// pool and every grant of one purges the other. The cooldown is the model's
// swapGraceSeconds (kvwGrace); the contract tests for exits, looping and slot
// count are in cooldown_unified_test.go.
const (
	kvwPool  = 1000
	kvwA     = 750
	kvwB     = 370
	kvwGrace = 600
)

type kvWarmRig struct {
	t     *testing.T
	eff   *fakeEffects
	s     *FIFO
	clock time.Time
}

func newKVWarmRig(t *testing.T, pool int) *kvWarmRig {
	t.Helper()
	withLargePrefillThreshold(t, 100)
	eff := newFakeEffects()
	eff.states["m"] = process.StateReady
	r := &kvWarmRig{t: t, eff: eff, clock: time.Unix(1_800_000_000, 0)}
	pools := map[string]int{}
	if pool > 0 {
		pools["m"] = pool
	}
	// WHY a swap grace: the slot cooldown IS swapGraceSeconds, and a model
	// without one has no cooldown at all.
	r.s = newFIFOKVModels(eff, pools, map[string]config.ModelConfig{"m": {SwapGraceSeconds: kvwGrace}})
	r.s.now = func() time.Time { return r.clock }
	return r
}

func (r *kvWarmRig) req(session string, tokens int) {
	r.s.OnRequest(HandlerReq{Model: "m", EstimatedTokens: tokens, Session: session})
}

func (r *kvWarmRig) done(session string, tokens int) {
	r.s.OnServeDone(ServeDoneEvent{ModelID: "m", EstimatedTokens: tokens, Session: session})
}

func (r *kvWarmRig) advance(d time.Duration) {
	r.clock = r.clock.Add(d)
	r.s.OnTick()
}

func (r *kvWarmRig) wantServed(want int, why string) {
	r.t.Helper()
	if got := r.eff.served("m"); got != want {
		r.t.Fatalf("%s: served=%d want %d", why, got, want)
	}
}

func (r *kvWarmRig) wantParked(reason string) {
	r.t.Helper()
	if len(r.s.queued) != 1 || r.s.queued[0].parkReason != reason {
		var got []string
		for _, q := range r.s.queued {
			got = append(got, q.Session+":"+q.parkReason)
		}
		r.t.Fatalf("queue = %v, want exactly one request parked %q", got, reason)
	}
}

// The incident: A's tool loop must keep its cache. B, parked behind A's turn,
// is NOT granted the moment that turn ends; A's next turn, seconds later, is.
func TestKVWarm_ToolLoopTurnKeepsItsCache(t *testing.T) {
	r := newKVWarmRig(t, kvwPool)
	r.req("A", kvwA)
	r.wantServed(1, "A alone")
	r.req("B", kvwB)
	r.wantParked(ParkKV)

	r.done("A", kvwA)
	r.wantServed(1, "B granted on A's turn boundary would purge A's cooling cache")
	r.wantParked(ParkKVWarm)

	r.advance(5 * time.Second) // the client runs a Bash tool
	r.req("A", kvwA+10)
	r.wantServed(2, "A's own next turn reuses its cache and must be granted past B")
	r.advance(time.Second)
	r.wantParked(ParkKV) // B now waits on A in flight
}

// A paused session does not keep the pool past its cooldown: once the grace
// lapses with no turn from A, B goes on the next tick, and A's cache is gone.
func TestKVWarm_CooldownLapseHandsOver(t *testing.T) {
	r := newKVWarmRig(t, kvwPool)
	r.req("A", kvwA)
	r.req("B", kvwB)
	r.done("A", kvwA)
	r.wantParked(ParkKVWarm)

	r.advance(kvwGrace*time.Second - time.Second)
	r.wantServed(1, "still inside A's cooldown")
	r.advance(2 * time.Second)
	r.wantServed(2, "A's cooldown lapsed: B must be granted")
	if _, ok := r.s.kvResidents["m"]["A"]; ok {
		t.Fatal("B's grant purges A's cache in the child; A must no longer be tracked as resident")
	}
}

// Two prompts that share the pool never wait on each other.
func TestKVWarm_FitsTogetherNeverHolds(t *testing.T) {
	r := newKVWarmRig(t, kvwPool)
	r.req("A", 400)
	r.done("A", 400)
	r.req("B", 300)
	r.wantServed(2, "400 resident + 300 fits 1000")
	if len(r.s.queued) != 0 {
		t.Fatalf("nothing may be queued, got %d", len(r.s.queued))
	}
}

// A session's own subagent is never held behind it: the parent cannot come
// back while the subagent runs.
func TestKVWarm_OwnSubagentNotHeld(t *testing.T) {
	r := newKVWarmRig(t, kvwPool)
	r.req("A", kvwA)
	r.done("A", kvwA)
	r.req("A/agent-1", kvwB)
	r.wantServed(2, "the parent's own subagent")
}

// A request without session metadata cannot hold a cache warm, but is held
// back from purging one.
func TestKVWarm_SessionlessRequest(t *testing.T) {
	r := newKVWarmRig(t, kvwPool)
	r.req("", kvwA)
	r.done("", kvwA)
	r.req("B", kvwA)
	r.wantServed(2, "a sessionless prompt is never warm")

	r.done("B", kvwA)
	r.req("", kvwB)
	r.wantServed(2, "a sessionless request must not purge B's cooling cache")
	r.wantParked(ParkKVWarm)
}

// Inert without a KV budget for the model (and without an explicit
// concurrencyLimit there is no slot count to displace either).
func TestKVWarm_InertWithoutPool(t *testing.T) {
	r := newKVWarmRig(t, 0)
	r.req("A", kvwA)
	r.done("A", kvwA)
	r.req("B", kvwA)
	r.wantServed(2, "no kvPoolTokens: no hold")
}

// A model with no swapGraceSeconds has no cooldown at either level: the
// session's slot is not protected after its turn, whatever the pool says.
func TestKVWarm_NoGraceNoCooldown(t *testing.T) {
	withLargePrefillThreshold(t, 100)
	eff := newFakeEffects()
	eff.states["m"] = process.StateReady
	s := newFIFOKVModels(eff, map[string]int{"m": kvwPool}, map[string]config.ModelConfig{"m": {}})
	s.OnRequest(HandlerReq{Model: "m", EstimatedTokens: kvwA, Session: "A"})
	s.OnServeDone(ServeDoneEvent{ModelID: "m", EstimatedTokens: kvwA, Session: "A"})
	s.OnRequest(HandlerReq{Model: "m", EstimatedTokens: kvwB, Session: "B"})
	if got := eff.served("m"); got != 2 {
		t.Fatalf("no swapGraceSeconds: B must not be held for A's slot, served=%d want 2", got)
	}
}

// Requests that waited out a model swap are granted by OnSwapDone, which used
// to check only the concurrency cap: after every swap to cq27 its two large
// sessions (198k + 98k on a 262144 pool) prefilled together (live 2026-10-06
// 19:44). The pool and the large-prefill cap apply to swap waiters too.
func TestKVWarm_SwapWaitersRespectThePool(t *testing.T) {
	withLargePrefillThreshold(t, 100)
	eff := newFakeEffects()
	eff.states["m"] = process.StateStopped
	s := newFIFOKVModels(eff, map[string]int{"m": kvwPool},
		map[string]config.ModelConfig{"m": {ConcurrencyLimit: 2, MaxParallelLargePrefill: 2}})

	s.OnRequest(HandlerReq{Model: "m", EstimatedTokens: kvwA, Session: "A"}) // starts the swap
	s.OnRequest(HandlerReq{Model: "m", EstimatedTokens: kvwB, Session: "B"}) // joins it
	eff.states["m"] = process.StateReady
	s.OnSwapDone(SwapDone{ModelID: "m"})

	if got := eff.served("m"); got != 1 {
		t.Fatalf("both swap waiters granted (served=%d want 1): %d + %d overflows pool %d", got, kvwA, kvwB, kvwPool)
	}
	if len(s.queued) != 1 || s.queued[0].parkReason != ParkKV {
		t.Fatalf("the second waiter must wait in the queue parked on kv, queue=%d", len(s.queued))
	}
}

// A reload starts with empty slots: an unload forgets every residency.
func TestKVWarm_UnloadForgetsResidency(t *testing.T) {
	r := newKVWarmRig(t, kvwPool)
	r.req("A", kvwA)
	r.done("A", kvwA)
	r.s.OnUnload([]string{"m"}, time.Second)
	r.eff.states["m"] = process.StateReady
	r.req("B", kvwA)
	r.wantServed(2, "after an unload A's cache is gone; B must not be held for it")
}
