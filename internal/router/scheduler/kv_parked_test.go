package scheduler

import (
	"context"
	"sync"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// metadataRecorder collects what a scheduler stamps onto a request's live
// in-flight entry, standing in for internal/server's inflightTracker (which
// the scheduler cannot import - the import direction runs server -> router).
type metadataRecorder struct {
	mu    sync.Mutex
	calls map[string]string
}

func newMetadataRecorder() *metadataRecorder {
	return &metadataRecorder{calls: map[string]string{}}
}

func (m *metadataRecorder) ctx() context.Context {
	setter := swaputil.InflightMetadataSetter(func(key, value string) {
		m.mu.Lock()
		m.calls[key] = value
		m.mu.Unlock()
	})
	return swaputil.WithInflightMetadataSetter(context.Background(), setter)
}

func (m *metadataRecorder) get(key string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.calls[key]
	return v, ok
}

// TestFIFO_KVAdmission_ParkStampsKVParkedMetadata pins the renderer-facing
// half of KV admission: a request the pool parks produces no bytes, which
// every byte heuristic reads as a stall, so the park itself must be visible
// on the live in-flight entry.
func TestFIFO_KVAdmission_ParkStampsKVParkedMetadata(t *testing.T) {
	eff := newFakeEffects()
	eff.states["cq35"] = process.StateReady
	s := newFIFOKV(eff, map[string]int{"cq35": 100})

	rec := newMetadataRecorder()
	s.OnRequest(reqTokens("cq35", 60)) // nothing in flight -> admitted

	parked := reqTokens("cq35", 60) // 60+60 = 120 > 100 -> must park
	parked.Ctx = rec.ctx()
	s.OnRequest(parked)

	if len(s.queued) != 1 {
		t.Fatalf("second request should have parked, queued=%d", len(s.queued))
	}
	if got, ok := rec.get("kv_parked"); !ok || got != "1" {
		t.Errorf(`kv_parked = %q (present=%v), want "1"`, got, ok)
	}
}

// TestFIFO_KVAdmission_AdmittedRequestIsNotMarkedParked guards the other
// direction: a request that sails through admission must carry no park
// marker at all, or every row would read PARKED.
func TestFIFO_KVAdmission_AdmittedRequestIsNotMarkedParked(t *testing.T) {
	eff := newFakeEffects()
	eff.states["cq35"] = process.StateReady
	s := newFIFOKV(eff, map[string]int{"cq35": 100})

	rec := newMetadataRecorder()
	admitted := reqTokens("cq35", 30)
	admitted.Ctx = rec.ctx()
	s.OnRequest(admitted)

	if got := eff.served("cq35"); got != 1 {
		t.Fatalf("request should have been admitted, served=%d", got)
	}
	if got, ok := rec.get("kv_parked"); ok {
		t.Errorf("kv_parked = %q, want no stamp for an admitted request", got)
	}
}

// TestFIFO_KVAdmission_DrainRepark keeps the marker honest across a drain
// pass: a queued request the drain re-parks is stamped again, so a row that
// stayed parked cannot silently lose its marker.
func TestFIFO_KVAdmission_DrainRepark(t *testing.T) {
	eff := newFakeEffects()
	eff.states["cq35"] = process.StateReady
	s := newFIFOKV(eff, map[string]int{"cq35": 100})

	s.OnRequest(reqTokens("cq35", 60))
	s.OnRequest(reqTokens("cq35", 60)) // parks, holding the queue

	rec := newMetadataRecorder()
	third := reqTokens("cq35", 60)
	third.Ctx = rec.ctx()
	s.OnRequest(third)

	// Freeing one request's budget drains the queue; the third still does not
	// fit behind the second, so the drain pass re-parks it.
	s.OnServeDone(ServeDoneEvent{ModelID: "cq35", EstimatedTokens: 60})

	if got, ok := rec.get("kv_parked"); !ok || got != "1" {
		t.Errorf(`kv_parked = %q (present=%v), want "1" after a drain re-park`, got, ok)
	}
}

// TestMarkKVParked_NoSetterIsNoOp covers a request that never passed through
// the inflight middleware (bare test harnesses, skipped paths): there is
// nothing to stamp, and that must not panic.
func TestMarkKVParked_NoSetterIsNoOp(t *testing.T) {
	markKVParked(HandlerReq{Model: "cq35"})
	markKVParked(HandlerReq{Model: "cq35", Ctx: context.Background()})
}
