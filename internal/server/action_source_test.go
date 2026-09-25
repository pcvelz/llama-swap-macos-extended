package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// @user-gated: user ruling - every menu action states its source
func TestServer_UnpenalizeLogsSourceWithHeader(t *testing.T) {
	s := newTestServer(newStubRouter([]string{"cq35"}, ""), newStubRouter(nil, ""))
	s.inflight.setLoopGuard(config.LoopGuardConfig{Enabled: true, RunBar: 3, ToleranceTokens: 2, Strikes: 1, PenaltySeconds: []int{-1}, ClearRequests: 5})
	s.inflight.loops.SetPenaltyObserver(s.logPenaltyEvent)
	feedSession(s.inflight.loops, penSession, 3)

	// With the header: log must say "via menu-click" and include the User-Agent.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/"+penSession+"/unpenalize", nil)
	req.Header.Set("X-Action-Source", "menu-click")
	req.Header.Set("User-Agent", "llama-swap-menu/1.0")
	s.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", w.Code)
	}
	logs := string(s.proxylog.GetHistory())
	t.Logf("proxylog contents: %q", logs)
	if !strings.Contains(logs, "un-penalized via menu-click (llama-swap-menu/1.0)") {
		t.Fatalf("proxylog must contain 'un-penalized via menu-click (llama-swap-menu/1.0)', got:\n%s", logs)
	}
}

// @user-gated: user ruling - every menu action states its source
func TestServer_UnpenalizeLogsSourceWithoutHeader(t *testing.T) {
	s := newTestServer(newStubRouter([]string{"cq35"}, ""), newStubRouter(nil, ""))
	s.inflight.setLoopGuard(config.LoopGuardConfig{Enabled: true, RunBar: 3, ToleranceTokens: 2, Strikes: 1, PenaltySeconds: []int{-1}, ClearRequests: 5})
	s.inflight.loops.SetPenaltyObserver(s.logPenaltyEvent)
	feedSession(s.inflight.loops, penSession, 3)

	// Without the header: log must say "via API" and include the User-Agent.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/"+penSession+"/unpenalize", nil)
	req.Header.Set("User-Agent", "curl/8.0")
	s.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", w.Code)
	}
	logs := string(s.proxylog.GetHistory())
	t.Logf("proxylog contents: %q", logs)
	if !strings.Contains(logs, "un-penalized via API (curl/8.0)") {
		t.Fatalf("proxylog must contain 'un-penalized via API (curl/8.0)', got:\n%s", logs)
	}
}

// TestServer_CancelInflightLogsSourceWithHeader creates a REAL in-flight
// request, cancels it with the X-Action-Source header, and asserts the cancel
// log line carries source + user-agent.
func TestServer_CancelInflightLogsSourceWithHeader(t *testing.T) {
	local := newStubRouter([]string{"cq35"}, "ok")
	s := newTestServer(local, newStubRouter(nil, ""))

	idCh := make(chan string, 1)
	done := make(chan struct{})
	mw := CreateInflightMiddleware(s.inflight, config.Config{})
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := s.inflight.Current()
		if len(current.Requests) != 1 {
			t.Errorf("inflight requests = %d, want 1", len(current.Requests))
			return
		}
		idCh <- current.Requests[0].ID
		<-r.Context().Done() // block until cancelled
		close(done)
	}))

	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		req.Header.Set("User-Agent", "test-client")
		req = req.WithContext(swaputil.SetContext(req.Context(), swaputil.ReqContextData{ModelID: "cq35"}))
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}()

	var inflightID string
	select {
	case inflightID = <-idCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for inflight ID")
	}

	// Cancel with the header.
	w := httptest.NewRecorder()
	cancelReq := httptest.NewRequest(http.MethodPost, "/api/inflight/"+inflightID+"/cancel", nil)
	cancelReq.Header.Set("X-Action-Source", "menu-click")
	cancelReq.Header.Set("User-Agent", "llama-swap-menu/1.0")
	s.ServeHTTP(w, cancelReq)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", w.Code)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("request context was not canceled")
	}

	logs := string(s.proxylog.GetHistory())
	t.Logf("proxylog contents: %q", logs)
	if !strings.Contains(logs, "cancelled via menu-click (llama-swap-menu/1.0)") {
		t.Fatalf("proxylog must contain 'cancelled via menu-click (llama-swap-menu/1.0)', got:\n%s", logs)
	}
}

// TestServer_CancelInflightLogsSourceWithoutHeader creates a REAL in-flight
// request, cancels it without the header, and asserts the cancel log line says
// "via API" with the User-Agent.
func TestServer_CancelInflightLogsSourceWithoutHeader(t *testing.T) {
	local := newStubRouter([]string{"cq35"}, "ok")
	s := newTestServer(local, newStubRouter(nil, ""))

	idCh := make(chan string, 1)
	done := make(chan struct{})
	mw := CreateInflightMiddleware(s.inflight, config.Config{})
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := s.inflight.Current()
		if len(current.Requests) != 1 {
			t.Errorf("inflight requests = %d, want 1", len(current.Requests))
			return
		}
		idCh <- current.Requests[0].ID
		<-r.Context().Done() // block until cancelled
		close(done)
	}))

	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		req.Header.Set("User-Agent", "test-client")
		req = req.WithContext(swaputil.SetContext(req.Context(), swaputil.ReqContextData{ModelID: "cq35"}))
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}()

	var inflightID string
	select {
	case inflightID = <-idCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for inflight ID")
	}

	w := httptest.NewRecorder()
	cancelReq := httptest.NewRequest(http.MethodPost, "/api/inflight/"+inflightID+"/cancel", nil)
	cancelReq.Header.Set("User-Agent", "curl/8.0")
	s.ServeHTTP(w, cancelReq)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", w.Code)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("request context was not canceled")
	}

	logs := string(s.proxylog.GetHistory())
	t.Logf("proxylog contents: %q", logs)
	if !strings.Contains(logs, "cancelled via API (curl/8.0)") {
		t.Fatalf("proxylog must contain 'cancelled via API (curl/8.0)', got:\n%s", logs)
	}
}
