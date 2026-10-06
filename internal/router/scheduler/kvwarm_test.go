package scheduler

import (
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/process"
)

// The KV warm hold (kvwarm.go), on the geometry of the 2026-10-06 cq27 thrash
// scaled to a pool of 1000: session A's prompt 198k and session B's 97k of a
// 262144 pool become 750 and 370 - together 1120, so the two cannot share the
// pool and every grant of one purges the other.
const (
	kvwPool = 1000
	kvwA    = 750
	kvwB    = 370
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
	r.s = newFIFOKV(eff, pools)
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
	r.wantServed(1, "B granted on A's turn boundary would purge A's warm cache")
	r.wantParked(ParkKVWarm)

	r.advance(5 * time.Second) // the client runs a Bash tool
	r.req("A", kvwA+10)
	r.wantServed(2, "A's own next turn reuses its cache and must be granted past B")
	r.advance(time.Second)
	r.wantParked(ParkKV) // B now waits on A in flight
}

// A paused session does not keep the pool: once the window lapses with no
// turn from A, B goes on the next tick, and A's cache is gone.
func TestKVWarm_WindowLapseHandsOver(t *testing.T) {
	r := newKVWarmRig(t, kvwPool)
	r.req("A", kvwA)
	r.req("B", kvwB)
	r.done("A", kvwA)
	r.wantParked(ParkKVWarm)

	r.advance(kvWarmWindow - time.Second)
	r.wantServed(1, "still inside A's window")
	r.advance(2 * time.Second)
	r.wantServed(2, "A's window lapsed: B must be granted")
	if _, ok := r.s.kvResidents["m"]["A"]; ok {
		t.Fatal("B's grant purges A's cache in the child; A must no longer be tracked as resident")
	}
}

// Nobody waits forever: once B has been held a quantum, the hold ends at A's
// next turn boundary - after A got a quantum of warm turns.
func TestKVWarm_QuantumBoundsTheHold(t *testing.T) {
	r := newKVWarmRig(t, kvwPool)
	r.req("A", kvwA)
	r.req("B", kvwB)
	r.done("A", kvwA) // the quantum starts at this first warm hold

	turns := 0
	for i := 0; i < 100 && r.eff.served("m") == 1+turns; i++ {
		r.advance(10 * time.Second) // tool runs
		r.req("A", kvwA)
		if r.eff.served("m") != 1+turns+1 {
			break // A's turn was not granted: the hold is over
		}
		turns++
		r.advance(110 * time.Second) // A's warm turn
		r.done("A", kvwA)
	}
	if got, want := r.eff.served("m"), 1+turns+1; got != want {
		t.Fatalf("after a quantum of A turns B must be granted: served=%d want %d (A turns %d)", got, want, turns)
	}
	if b := r.eff.lastServeReq.Session; b != "B" {
		t.Fatalf("the grant that ended the hold must be B's, got session %q", b)
	}
	if wantMin := int(kvWarmQuantum / (2 * time.Minute)); turns < wantMin {
		t.Fatalf("A must get a quantum of warm turns before handing over: got %d want >= %d", turns, wantMin)
	}
	if wantMax := int(kvWarmQuantum/(2*time.Minute)) + 1; turns > wantMax {
		t.Fatalf("the hold must end once the quantum is spent: A got %d turns, want <= %d", turns, wantMax)
	}
}

// The quantum clock starts at the first WARM hold: B parked behind A's long
// cold prefill must not spend A's quantum before A's first warm turn.
func TestKVWarm_ColdPrefillDoesNotSpendTheQuantum(t *testing.T) {
	r := newKVWarmRig(t, kvwPool)
	r.req("A", kvwA)
	r.req("B", kvwB)
	r.advance(67 * time.Minute) // A's cold re-prefill of a 198k prompt
	r.done("A", kvwA)
	r.wantParked(ParkKVWarm)
	r.advance(10 * time.Second)
	r.req("A", kvwA+5)
	r.wantServed(2, "A's first warm turn after a long cold prefill")
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
	r.wantServed(2, "a sessionless request must not purge B's warm cache")
	r.wantParked(ParkKVWarm)
}

// Inert without a KV budget for the model.
func TestKVWarm_InertWithoutPool(t *testing.T) {
	r := newKVWarmRig(t, 0)
	r.req("A", kvwA)
	r.done("A", kvwA)
	r.req("B", kvwA)
	r.wantServed(2, "no kvPoolTokens: no hold")
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
