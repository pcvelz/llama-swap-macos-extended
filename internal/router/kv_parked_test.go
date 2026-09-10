package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// TestBaseRouter_GrantClearsKVParkedMetadata pins the end of a KV park: the
// scheduler stamps kv_parked while a request waits on admission
// (scheduler.markKVParked), and the grant must clear it. Left behind, the
// marker would hold the row at PARKED for the rest of a request that is in
// fact serving. An empty value is the delete signal - see
// internal/server/inflight.go SetMetadata.
func TestBaseRouter_GrantClearsKVParkedMetadata(t *testing.T) {
	a := newFakeProcess("a")
	a.autoReady = true

	b := newTestBase(t, map[string]process.Process{"a": a}, &stubPlanner{})

	var mu sync.Mutex
	calls := map[string]string{}
	setter := swaputil.InflightMetadataSetter(func(key, value string) {
		mu.Lock()
		calls[key] = value
		mu.Unlock()
	})
	ctx := swaputil.WithInflightMetadataSetter(context.Background(), setter)

	w := httptest.NewRecorder()
	b.ServeHTTP(w, newRequestCtx(ctx, "a"))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	mu.Lock()
	got, ok := calls["kv_parked"]
	mu.Unlock()
	if !ok {
		t.Fatalf("grant must clear kv_parked, no call recorded (setter calls: %v)", calls)
	}
	if got != "" {
		t.Errorf(`kv_parked cleared with %q, want "" (the delete signal)`, got)
	}
}
