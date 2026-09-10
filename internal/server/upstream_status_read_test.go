package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/process"
)

// A status read (GET/HEAD) under /upstream/<model>/ for a local model that is
// not READY must be answered here without dispatching into the router: once
// dispatched, a read on a non-resident model becomes a queued swap request.
// It inflated the swap-grace "waiting" count and could have won the swap
// (menu-bar helper 2s /slots polls on parked models, 2026-09-10). This
// generalises the existing GET/HEAD root-path guard to every GET/HEAD path.
func TestServer_HandleUpstream_StatusReadOnNotReadyModelNeverDispatches(t *testing.T) {
	local := newStubRouter([]string{"m1"}, "")
	serveCalls := 0
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		serveCalls++
		w.WriteHeader(http.StatusOK)
	}
	local.running = map[string]process.ProcessState{} // m1 not loaded
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"m1": {}}}
	s.routes()

	for _, p := range []string{"/upstream/m1/slots", "/upstream/m1/props", "/upstream/m1/metrics", "/upstream/m1/health"} {
		for _, m := range []string{http.MethodGet, http.MethodHead} {
			w := httptest.NewRecorder()
			s.ServeHTTP(w, httptest.NewRequest(m, p, nil))
			if w.Code != http.StatusServiceUnavailable {
				t.Errorf("%s %s: status=%d want 503 (model not loaded, no dispatch)", m, p, w.Code)
			}
		}
	}
	if serveCalls != 0 {
		t.Fatalf("router.ServeHTTP called %d times for status reads on a not-ready model, want 0", serveCalls)
	}

	// A POST (real inference) on the same not-ready model still dispatches:
	// it is what is allowed to queue a load.
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/upstream/m1/v1/chat/completions", nil))
	if serveCalls != 1 {
		t.Fatalf("POST on a not-ready model must dispatch (serveCalls=%d want 1)", serveCalls)
	}

	// Once READY the same reads proxy normally.
	local.running["m1"] = process.StateReady
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/upstream/m1/slots", nil))
	if w.Code != http.StatusOK || serveCalls != 2 {
		t.Fatalf("ready model: status=%d serveCalls=%d want 200/2", w.Code, serveCalls)
	}
}
