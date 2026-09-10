package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// reasonRecorder is metadataRecorder plus a per-key emit count, so a test can
// assert a reason is stamped ONCE per change and not re-emitted on every
// drain tick (each SetMetadata is an SSE upsert; a 1 Hz re-stamp of an
// unchanged reason would be a 1 Hz event storm per parked request).
type reasonRecorder struct {
	mu     sync.Mutex
	values map[string]string
	counts map[string]int
}

func newReasonRecorder() *reasonRecorder {
	return &reasonRecorder{values: map[string]string{}, counts: map[string]int{}}
}

func (r *reasonRecorder) ctx() context.Context {
	setter := swaputil.InflightMetadataSetter(func(key, value string) {
		r.mu.Lock()
		r.values[key] = value
		r.counts[key]++
		r.mu.Unlock()
	})
	return swaputil.WithInflightMetadataSetter(context.Background(), setter)
}

func (r *reasonRecorder) reason() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.values["park_reason"]
}

func (r *reasonRecorder) emits() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counts["park_reason"]
}

// Every park the scheduler decides names its reason on the live in-flight
// entry (`park_reason`), so a PARKED row can say WHY. The reasons are the
// states observed by hand on 2026-09-10 (llama-cm
// docs/research/2026-09-10-cooldown-dogfood-ledger.md): CAP (O2), KV (O7),
// BUSY (O9), COOLDOWN (O10), LOADING (O12).

func TestFIFO_ParkReason_Cap(t *testing.T) {
	s, _ := newFIFOWithLimit(t, "a", 1)
	s.OnRequest(req("a")) // takes the only slot

	rec := newReasonRecorder()
	r := reqCh("a")
	r.Ctx = rec.ctx()
	s.OnRequest(r)

	if got := rec.reason(); got != ParkCap {
		t.Fatalf("park_reason=%q want %q", got, ParkCap)
	}
}

func TestFIFO_ParkReason_KV(t *testing.T) {
	eff := newFakeEffects()
	eff.states["cq35"] = process.StateReady
	s := newFIFOKV(eff, map[string]int{"cq35": 100})
	s.OnRequest(reqTokens("cq35", 60))

	rec := newReasonRecorder()
	r := reqTokens("cq35", 60)
	r.Ctx = rec.ctx()
	s.OnRequest(r)

	if got := rec.reason(); got != ParkKV {
		t.Fatalf("park_reason=%q want %q", got, ParkKV)
	}
}

func TestFIFO_ParkReason_BusyThenCooldown_OneEmitPerChange(t *testing.T) {
	eff := newFakeEffects()
	eff.states["a"] = process.StateReady
	eff.states["b"] = process.StateStopped
	clk := time.Unix(1000, 0)
	grace := map[string]time.Duration{"a": 60 * time.Second}
	s := newFIFOGrace(&runningFilterPlanner{evict: map[string][]string{"b": {"a"}}}, eff, grace, &clk)

	s.OnRequest(req("a")) // a is busy

	rec := newReasonRecorder()
	r := reqCh("b")
	r.Ctx = rec.ctx()
	s.OnRequest(r)
	if got := rec.reason(); got != ParkBusy {
		t.Fatalf("park_reason=%q want %q while the resident is in flight", got, ParkBusy)
	}

	// Ticks while nothing changes must not re-stamp.
	s.OnTick()
	s.OnTick()
	if got := rec.emits(); got != 1 {
		t.Fatalf("park_reason emitted %d times while unchanged, want 1", got)
	}

	// The resident drains: the same queued request is now held by the
	// cooldown, and the reason changes exactly once.
	s.OnServeDone(ServeDoneEvent{ModelID: "a"})
	if got := rec.reason(); got != ParkCooldown {
		t.Fatalf("park_reason=%q want %q once the resident is idle inside its grace", got, ParkCooldown)
	}
	if got := rec.emits(); got != 2 {
		t.Fatalf("park_reason emitted %d times, want 2 (busy, cooldown)", got)
	}

	// The cooldown expires: the swap starts and the request joins it.
	clk = clk.Add(61 * time.Second)
	s.OnTick()
	if got := rec.reason(); got != ParkLoading {
		t.Fatalf("park_reason=%q want %q while b loads", got, ParkLoading)
	}

	// Granted: the reason is cleared (empty value deletes the key).
	eff.states["b"] = process.StateReady
	s.OnSwapDone(SwapDone{ModelID: "b"})
	if got := rec.reason(); got != "" {
		t.Fatalf("park_reason=%q want cleared once granted", got)
	}
}

func TestFIFO_ParkReason_LoadingWhenJoiningActiveSwap(t *testing.T) {
	eff := newFakeEffects()
	eff.states["a"] = process.StateReady
	eff.states["b"] = process.StateStopped
	s := newFIFO(&runningFilterPlanner{evict: map[string][]string{"b": {"a"}}}, eff)

	s.OnRequest(reqCh("b")) // a idle, no grace -> swap starts
	if got := eff.startsFor("b"); got != 1 {
		t.Fatalf("StartSwap(b)=%d want 1", got)
	}

	rec := newReasonRecorder()
	r := reqCh("b")
	r.Ctx = rec.ctx()
	s.OnRequest(r) // joins the in-flight swap
	if got := rec.reason(); got != ParkLoading {
		t.Fatalf("park_reason=%q want %q", got, ParkLoading)
	}
}
