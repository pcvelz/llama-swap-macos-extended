package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/membrake"
)

// /api/events carries the memory brake state so the menu can show the drain
// gate: "memory brake: loads held until file-backed < 10 GB (now 36.0 GB)".
func TestServer_APIEvents_MemoryBrakeHold(t *testing.T) {
	old := membrake.DefaultHold
	defer func() { membrake.DefaultHold = old }()
	membrake.DefaultHold = &membrake.Hold{}
	membrake.DefaultHold.Set(&membrake.Event{Killed: []string{"cq27(pgid 4242)"}, GrowthGB: 6.6}, 10<<30)

	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	ctx, cancelReq := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		s.ServeHTTP(w, req)
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	cancelReq()
	<-done

	body := w.Body.String()
	for _, want := range []string{`"type":"memoryBrake"`, `\"holding\":true`, `\"drainBelowGB\":10`, `\"fileBackedGB\":`, `cq27(pgid 4242)`} {
		if !strings.Contains(body, want) {
			t.Errorf("memoryBrake SSE payload missing %s; body=%q", want, body)
		}
	}
}
