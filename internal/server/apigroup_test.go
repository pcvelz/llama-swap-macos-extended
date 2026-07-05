package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
)

func TestServer_InflightMiddleware(t *testing.T) {
	c := &inflightCounter{}
	mw := CreateInflightMiddleware(c)

	var duringRequest int64
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		duringRequest = c.Current()
	}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	if duringRequest != 1 {
		t.Errorf("counter during request = %d, want 1", duringRequest)
	}
	if got := c.Current(); got != 0 {
		t.Errorf("counter after request = %d, want 0", got)
	}
}

func TestServer_APIVersion(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	s.build = BuildInfo{Version: "1.2.3", Commit: "deadbeef", Date: "2026-05-19"}

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/version", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["version"] != "1.2.3" || got["commit"] != "deadbeef" || got["build_date"] != "2026-05-19" {
		t.Errorf("body = %v", got)
	}
}

func TestServer_APIMetrics_Empty(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/metrics", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if body := strings.TrimSpace(w.Body.String()); body != "[]" {
		t.Errorf("body = %q, want []", body)
	}
}

func TestServer_APIPerformance_Unavailable(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/performance", nil))

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestServer_APIEvents_InitialPayload(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx)
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		s.ServeHTTP(w, req)
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after context cancel")
	}

	body := w.Body.String()
	for _, want := range []string{`"type":"modelStatus"`, `"type":"inflight"`, `"type":"logData"`} {
		if !strings.Contains(body, want) {
			t.Errorf("initial SSE payload missing %s; body=%q", want, body)
		}
	}
}

func TestServer_ModelStatus_TTLAndLastUse(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	local := newStubRouter(nil, "")
	local.lastUseMap = map[string]time.Time{"m1": now}
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{
		"m1": {UnloadAfter: 120},
	}}
	models := s.modelStatus()
	if len(models) != 1 {
		t.Fatalf("len=%d want 1", len(models))
	}
	m := models[0]
	if m.TTL != 120 {
		t.Errorf("TTL=%d want 120", m.TTL)
	}
	if !m.LastUse.Equal(now) {
		t.Errorf("LastUse=%v want %v", m.LastUse, now)
	}
}

func TestServer_PinUnpin(t *testing.T) {
	local := newStubRouter([]string{"m1"}, "")
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"m1": {}}}

	pin := httptest.NewRequest(http.MethodPost, "/api/models/pin/m1", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, pin)
	if w.Code != http.StatusOK {
		t.Fatalf("pin status=%d body=%q", w.Code, w.Body.String())
	}
	if !local.IsPinned("m1") {
		t.Errorf("m1 not pinned after POST /api/models/pin/m1")
	}
	// modelStatus must reflect the pinned state.
	if m := s.modelStatus(); len(m) != 1 || !m[0].Pinned {
		t.Errorf("modelStatus pinned=%v want true", m)
	}

	unpin := httptest.NewRequest(http.MethodPost, "/api/models/unpin/m1", nil)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, unpin)
	if w.Code != http.StatusOK {
		t.Fatalf("unpin status=%d body=%q", w.Code, w.Body.String())
	}
	if local.IsPinned("m1") {
		t.Errorf("m1 still pinned after POST /api/models/unpin/m1")
	}
}

func TestServer_Pin_UnknownModel404(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{}}

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/models/pin/nope", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("pin unknown model status=%d want 404", w.Code)
	}
}
