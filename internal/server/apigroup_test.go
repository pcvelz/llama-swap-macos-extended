package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/cache"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/hw"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

func TestServer_InflightMiddleware_AddsAndRemovesEntriesAroundRequestHandling(t *testing.T) {
	tracker := newInflightTracker()
	mw := CreateInflightMiddleware(tracker, config.Config{})

	var duringRequest swaputil.InFlightRequestsEvent
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		duringRequest = tracker.Current()
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	started := time.Now().Add(-time.Second)
	req = req.WithContext(context.WithValue(req.Context(), inflightStartContextKey{}, started))
	req.Header.Set("User-Agent", "test-agent")
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	req = req.WithContext(swaputil.SetContext(req.Context(), swaputil.ReqContextData{
		Model:    "requested-model",
		ModelID:  "resolved-model",
		Metadata: map[string]string{"source": "test"},
	}))

	handler.ServeHTTP(httptest.NewRecorder(), req)

	if len(duringRequest.Requests) != 1 {
		t.Fatalf("inflight requests during request = %d, want 1", len(duringRequest.Requests))
	}
	entry := duringRequest.Requests[0]
	if entry.ID == "" || entry.Model != "resolved-model" || entry.Method != http.MethodPost || entry.ReqPath != "/v1/chat/completions" {
		t.Errorf("inflight entry = %+v", entry)
	}
	if !entry.Timestamp.Equal(started) {
		t.Errorf("timestamp = %v, want request start %v", entry.Timestamp, started)
	}
	if entry.ElapsedMs < 1000 {
		t.Errorf("elapsed ms = %d, want at least 1000", entry.ElapsedMs)
	}
	if entry.Metadata["source"] != "test" {
		t.Errorf("metadata = %v, want source=test", entry.Metadata)
	}
	if entry.RemoteIP != "203.0.113.9" {
		t.Errorf("remote ip = %q, want 203.0.113.9", entry.RemoteIP)
	}
	if entry.ReqHeaders["User-Agent"] != "test-agent" || entry.ReqHeaders["Authorization"] != "[REDACTED]" {
		t.Errorf("request headers = %v", entry.ReqHeaders)
	}
	if got := tracker.Current(); len(got.Requests) != 0 {
		t.Errorf("inflight after request = %+v, want empty", got)
	}
}

// TestServer_InflightTracker_PerTierCounts pins the tiers feature's in-flight
// accounting (docs/intent/llama-swap-tiers.md): every emitted event carries the
// aggregate Total, and the ByTier breakdown appears only when more than one
// tier is configured — the macOS menu-bar helper and internal/tray read exactly
// those two fields off the "inflight" SSE payload.
func TestServer_InflightTracker_PerTierCounts(t *testing.T) {
	t.Run("no tiers configured: Total only, no ByTier", func(t *testing.T) {
		tracker := newInflightTracker()
		mw := CreateInflightMiddleware(tracker, config.Config{})

		var during swaputil.InFlightRequestsEvent
		mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			during = tracker.Current()
		})).ServeHTTP(httptest.NewRecorder(), inflightTierReq(swaputil.DefaultTier))

		if during.Total != 1 {
			t.Errorf("Total = %d, want 1", during.Total)
		}
		if during.ByTier != nil {
			t.Errorf("ByTier = %v, want nil when only the default tier exists", during.ByTier)
		}
	})

	t.Run("tiers configured: per-tier breakdown", func(t *testing.T) {
		tracker := newInflightTracker("priority", "background")
		mw := CreateInflightMiddleware(tracker, config.Config{})

		var during swaputil.InFlightRequestsEvent
		mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			during = tracker.Current()
		})).ServeHTTP(httptest.NewRecorder(), inflightTierReq(swaputil.Tier{Name: "priority", Rank: 10}))

		if during.Total != 1 {
			t.Errorf("Total = %d, want 1", during.Total)
		}
		want := map[string]int{"default": 0, "priority": 1, "background": 0}
		if len(during.ByTier) != len(want) {
			t.Fatalf("ByTier = %v, want %v", during.ByTier, want)
		}
		for name, count := range want {
			if during.ByTier[name] != count {
				t.Errorf("ByTier[%q] = %d, want %d (full map %v)", name, during.ByTier[name], count, during.ByTier)
			}
		}

		// Counts drop back to zero once the request completes.
		after := tracker.Current()
		if after.Total != 0 || after.ByTier["priority"] != 0 {
			t.Errorf("after request: Total=%d ByTier=%v, want 0 everywhere", after.Total, after.ByTier)
		}
	})
}

// TestServer_APIEvents_InflightPayloadKeepsTotalAndByTier is the wire-format
// gate for the "inflight" SSE event. Consumers outside this repo read the
// payload's `total` (and `byTier`) keys directly and default to 0 on a parse
// miss rather than erroring - llama-cm's cm-menu cold-start gate treats a 0 as
// "box is empty" and fires an armed session, so a silently unpopulated total is
// worse than a hard failure. Upstream now marshals the whole event struct, so
// this asserts the emitted JSON still carries our two keys alongside upstream's
// operation/requests fields.
func TestServer_APIEvents_InflightPayloadKeepsTotalAndByTier(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	// Two configured tiers: the ByTier breakdown is emitted (a single-tier
	// deployment deliberately omits it - see byTierOrNil).
	s.inflight = newInflightTracker("priority", "background")

	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.inflight.Add(inflightTierReq(swaputil.Tier{Name: "priority", Rank: 10}), cancel)

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
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after context cancel")
	}

	body := w.Body.String()
	// The payload is a JSON string inside the envelope, so the keys arrive
	// backslash-escaped.
	for _, want := range []string{`\"total\":1`, `\"byTier\":`, `\"priority\":1`, `\"default\":0`} {
		if !strings.Contains(body, want) {
			t.Errorf("inflight SSE payload missing %s; body=%q", want, body)
		}
	}
	t.Logf("emitted inflight payload: %s", inflightPayloadJSON(t, s))
}

// inflightPayloadJSON returns exactly the bytes sendInFlight marshals, for the
// log line above (and as a readable record of the wire shape).
func inflightPayloadJSON(t *testing.T, s *Server) string {
	t.Helper()
	j, err := json.Marshal(s.inflight.Current())
	if err != nil {
		t.Fatalf("marshal inflight event: %v", err)
	}
	return string(j)
}

// inflightTierReq builds a model-dispatched request tagged with tier.
func inflightTierReq(tier swaputil.Tier) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx := swaputil.WithTier(req.Context(), tier)
	ctx = swaputil.SetContext(ctx, swaputil.ReqContextData{Model: "m1", ModelID: "m1"})
	return req.WithContext(ctx)
}

func TestServer_InflightMiddleware_IgnoresConfiguredWebsocket(t *testing.T) {
	tracker := newInflightTracker()
	cfg := config.Config{Models: map[string]config.ModelConfig{
		"m1": {Compat: config.CompatConfig{IgnoreWebsockets: true}},
	}}
	var duringRequest swaputil.InFlightRequestsEvent
	handler := CreateInflightMiddleware(tracker, cfg)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		duringRequest = tracker.Current()
		w.WriteHeader(http.StatusSwitchingProtocols)
	}))

	req := httptest.NewRequest(http.MethodGet, "/props?model=m1", nil)
	req.Header.Set("Connection", "keep-alive, Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req = req.WithContext(swaputil.SetContext(req.Context(), swaputil.ReqContextData{Model: "m1", ModelID: "m1"}))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if len(duringRequest.Requests) != 0 {
		t.Fatalf("inflight during ignored websocket = %+v, want empty", duringRequest)
	}
}

func TestServer_InflightMiddleware_StreamsResponseUpdates(t *testing.T) {
	events := make(chan swaputil.InFlightRequestsEvent, 8)
	tracker := newInflightTrackerWithPublisher(8, func(update swaputil.InFlightRequestsEvent) {
		events <- update
	})

	release := make(chan struct{})
	done := make(chan struct{})
	handler := CreateInflightMiddleware(tracker, config.Config{})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Set-Cookie", "secret=value")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello"))
		w.(http.Flusher).Flush()
		<-release
	}))

	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req = req.WithContext(swaputil.SetContext(req.Context(), swaputil.ReqContextData{ModelID: "m1"}))
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}()

	_ = waitInflightEvent(t, events, inflightOperationUpsert)
	headers := waitInflightEvent(t, events, inflightOperationUpsert)
	if headers.Request == nil || headers.Request.RespHeaders["Content-Type"] != "text/event-stream" {
		t.Fatalf("response headers event = %+v", headers)
	}
	if headers.Request.RespHeaders["Set-Cookie"] != "[REDACTED]" {
		t.Errorf("response headers = %v", headers.Request.RespHeaders)
	}

	bytesUpdate := waitInflightEvent(t, events, inflightOperationUpsert)
	if bytesUpdate.Request == nil || bytesUpdate.Request.RespBytes != 5 {
		t.Errorf("response bytes event = %+v, want 5", bytesUpdate.Request)
	}

	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return")
	}
	removed := waitInflightEvent(t, events, inflightOperationRemove)
	if removed.ID == "" {
		t.Error("remove event missing id")
	}
}

func TestServer_InflightEventPayloadIncludesRequestEntries(t *testing.T) {
	events := make(chan swaputil.InFlightRequestsEvent, 4)
	tracker := newInflightTrackerWithPublisher(4, func(update swaputil.InFlightRequestsEvent) {
		events <- update
	})

	release := make(chan struct{})
	done := make(chan struct{})
	handler := CreateInflightMiddleware(tracker, config.Config{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))

	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodGet, "/props?model=m1", nil)
		req = req.WithContext(swaputil.SetContext(req.Context(), swaputil.ReqContextData{
			Model:   "m1",
			ModelID: "m1",
		}))
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}()

	added := waitInflightEvent(t, events, inflightOperationUpsert)
	if added.Request == nil {
		t.Fatal("added request is nil")
	}
	if added.Request.Model != "m1" || added.Request.ReqPath != "/props" {
		t.Errorf("added request = %+v", added.Request)
	}

	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return")
	}
	removed := waitInflightEvent(t, events, inflightOperationRemove)
	if removed.ID != added.Request.ID {
		t.Errorf("removed id = %q, want %q", removed.ID, added.Request.ID)
	}
}

func TestServer_InflightTracker_OutboxDoesNotBlockRequests(t *testing.T) {
	publishStarted := make(chan struct{})
	releasePublisher := make(chan struct{})
	published := make(chan swaputil.InFlightRequestsEvent, 4)
	var blockFirst sync.Once

	tracker := newInflightTrackerWithPublisher(1, func(update swaputil.InFlightRequestsEvent) {
		blockFirst.Do(func() {
			close(publishStarted)
			<-releasePublisher
		})
		published <- update
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	id := tracker.Add(req, func() {})
	select {
	case <-publishStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("publisher did not start")
	}

	// Fill the one-item outbox while the publisher is blocked, then overflow
	// it with the removal. The request path must still return immediately.
	tracker.SetResponseHeaders(id, http.Header{"Content-Type": {"text/event-stream"}})
	removed := make(chan struct{})
	go func() {
		tracker.Remove(id)
		close(removed)
	}()
	select {
	case <-removed:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Remove blocked on a busy publisher")
	}

	close(releasePublisher)
	_ = waitInflightEvent(t, published, inflightOperationUpsert)
	recovered := waitInflightEvent(t, published, inflightOperationSnapshot)
	if len(recovered.Requests) != 0 {
		t.Errorf("recovery snapshot = %+v, want no requests", recovered.Requests)
	}
}

func TestServer_InflightCancelByIDCancelsRequestContext(t *testing.T) {
	tracker := newInflightTracker()
	idCh := make(chan string, 1)
	done := make(chan struct{})
	handler := CreateInflightMiddleware(tracker, config.Config{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := tracker.Current()
		if len(current.Requests) != 1 {
			t.Errorf("inflight requests = %d, want 1", len(current.Requests))
			return
		}
		idCh <- current.Requests[0].ID
		<-r.Context().Done()
		close(done)
	}))

	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req = req.WithContext(swaputil.SetContext(req.Context(), swaputil.ReqContextData{ModelID: "m1"}))
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}()

	var id string
	select {
	case id = <-idCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for inflight id")
	}
	if !tracker.Cancel(id) {
		t.Fatalf("Cancel(%q) = false, want true", id)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("request context was not canceled")
	}
	waitInflightTrackerCount(t, tracker, 0)
}

func waitInflightTrackerCount(t *testing.T, tracker *inflightTracker, total int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if got := tracker.Current(); len(got.Requests) == total {
			return
		}
		select {
		case <-deadline:
			got := tracker.Current()
			t.Fatalf("inflight total = %d, want %d", len(got.Requests), total)
		case <-tick.C:
		}
	}
}

func waitInflightEvent(t *testing.T, events <-chan swaputil.InFlightRequestsEvent, operation string) swaputil.InFlightRequestsEvent {
	t.Helper()
	timer := time.After(2 * time.Second)
	for {
		select {
		case got := <-events:
			if got.Operation == operation {
				return got
			}
		case <-timer:
			t.Fatalf("timed out waiting for inflight operation %q", operation)
		}
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

func TestServer_APIHardware(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	s.hardware = &hw.HardwareSnapshot{
		SchemaVersion: hw.SchemaVersion,
		CapturedAt:    time.Date(2026, time.August, 3, 12, 0, 0, 0, time.UTC),
		Capture: hw.HardwareCapture{
			Scope:    hw.CaptureScopeInferenceHost,
			Method:   hw.CaptureMethodDetected,
			Detector: &hw.DetectorInfo{Name: "llama-swap", Version: "246"},
		},
		Architecture:    hw.Architecture{Name: "x86_64"},
		OperatingSystem: hw.OperatingSystem{Family: "linux"},
		Environment:     hw.ExecutionEnvironment{Kind: "unknown"},
		Memory:          hw.SystemMemory{CapacityBytes: 1024},
		Accelerators:    []hw.Accelerator{},
	}

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/hardware", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var got hw.HardwareSnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.SchemaVersion != 1 || got.Memory.CapacityBytes != 1024 || got.Accelerators == nil {
		t.Errorf("body = %+v", got)
	}
}

func TestServer_APIHardwareUnavailable(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/hardware", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

func TestServer_APIMetricsActivity_Empty(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/metrics/activity", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var page store.ActivityPage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if page.Total != 0 || len(page.Data) != 0 {
		t.Errorf("page = %+v, want empty", page)
	}
}

func TestServer_APIMetricsActivity(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	s.metrics.enableCaptures = true
	s.metrics.captureCache = cache.New(1024 * 1024)

	storedM1, ok := s.metrics.queueMetrics(ActivityLogEntry{
		Timestamp: time.Unix(1, 0),
		Model:     "m1",
		ReqPath:   "/v1/chat/completions",
		Tokens:    TokenMetrics{InputTokens: 1, OutputTokens: 2},
	})
	if !ok {
		t.Fatal("queueMetrics m1 failed")
	}
	if ok := s.metrics.addCapture(ReqRespCapture{ID: storedM1.ID, ReqPath: "/v1/chat/completions"}); !ok {
		t.Fatal("addCapture failed")
	}
	if _, ok := s.metrics.queueMetrics(ActivityLogEntry{
		Timestamp: time.Unix(2, 0),
		Model:     "m2",
		ReqPath:   "/v1/chat/completions",
		Tokens:    TokenMetrics{InputTokens: 3, OutputTokens: 4},
	}); !ok {
		t.Fatal("queueMetrics m2 failed")
	}

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/metrics/activity?model=m1&limit=10&page=1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", w.Code, w.Body.String())
	}
	var page store.ActivityPage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if page.Total != 1 || len(page.Data) != 1 {
		t.Fatalf("page = %+v", page)
	}
	if page.Data[0].ID != storedM1.ID || !page.Data[0].HasCapture {
		t.Fatalf("entry = %+v", page.Data[0])
	}
}

func TestServer_APIMetricsStats(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	for _, entry := range []ActivityLogEntry{
		{Timestamp: time.Unix(1, 0), Model: "m1", Tokens: TokenMetrics{InputTokens: 1, OutputTokens: 2, CachedTokens: 1, PromptPerSecond: 10, TokensPerSecond: 20}},
		{Timestamp: time.Unix(2, 0), Model: "m1", Tokens: TokenMetrics{InputTokens: 3, OutputTokens: 4, PromptPerSecond: 30, TokensPerSecond: 40}},
		{Timestamp: time.Unix(3, 0), Model: "m2", Tokens: TokenMetrics{InputTokens: 5, OutputTokens: 6, PromptPerSecond: 50}},
	} {
		if _, ok := s.metrics.queueMetrics(entry); !ok {
			t.Fatal("queueMetrics failed")
		}
	}

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/metrics/stats?model=m1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", w.Code, w.Body.String())
	}
	var stats store.ActivityStats
	if err := json.Unmarshal(w.Body.Bytes(), &stats); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if stats.TotalRequests != 2 || stats.TotalInputTokens != 4 || stats.TotalOutputTokens != 6 || stats.TotalCacheTokens != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	if stats.PromptHistogram == nil || stats.GenerationHistogram == nil {
		t.Fatalf("expected histograms: %+v", stats)
	}
}

func TestServer_APICancelInflight(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	ctx, cancel := context.WithCancel(context.Background())
	id := s.inflight.Add(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx), cancel)
	defer s.inflight.Remove(id)

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/inflight/"+id+"/cancel", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%q", w.Code, w.Body.String())
	}
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("cancel endpoint did not cancel request context")
	}

	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/inflight/missing/cancel", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d, want 404", w.Code)
	}
}

func TestServer_InflightMetricsRecordsCompletedOnce(t *testing.T) {
	local := newStubRouter([]string{"m1"}, `{"usage":{"prompt_tokens":1,"completion_tokens":2}}`)
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = configWithModels("m1")

	w := httptest.NewRecorder()
	s.ServeHTTP(w, chatRequest("m1"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", w.Code, w.Body.String())
	}
	if got := s.inflight.Current(); len(got.Requests) != 0 {
		t.Fatalf("inflight total after request = %d, want 0", len(got.Requests))
	}
	gotMetrics := metricsEntries(t, s.metrics)
	if len(gotMetrics) != 1 {
		t.Fatalf("metrics len = %d, want 1", len(gotMetrics))
	}
	if gotMetrics[0].Model != "m1" || gotMetrics[0].Tokens.InputTokens != 1 || gotMetrics[0].Tokens.OutputTokens != 2 {
		t.Errorf("metric = %+v", gotMetrics[0])
	}
}

func configWithModels(models ...string) config.Config {
	cfg := config.Config{Models: make(map[string]config.ModelConfig, len(models))}
	for _, model := range models {
		cfg.Models[model] = config.ModelConfig{}
	}
	return cfg
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
	s.cfg.UI.Activity.SessionID = []string{"X-Trace-ID"}

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
	for _, want := range []string{`"type":"modelStatus"`, `"type":"inflight"`, `"type":"uiConfig"`, `"type":"profileChanged"`, `"type":"logData"`, `X-Trace-ID`} {
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
