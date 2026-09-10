package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// ---------------------------------------------------------------------------
// helpers

const (
	affSessionA = "11111111-2222-3333-4444-555555555555"
	affSessionB = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
)

// affinityConfig builds a config with model "m" opted in and model "plain"
// not opted in.
func affinityConfig() config.Config {
	return config.Config{Models: map[string]config.ModelConfig{
		"m":     {SlotAffinity: true},
		"plain": {},
	}}
}

// newAffinityStore returns a store that is NOT subscribed to the event bus,
// so unit tests never race with other tests' process events. Both
// small-request thresholds are zeroed so the existing tiny-body,
// low-token-count fixtures below keep exercising learn/inject unchanged;
// tests of the thresholds themselves build their own store.
func newAffinityStore(cfg config.Config) *slotAffinityStore {
	s := newSlotAffinityStore(cfg)
	s.Close()
	s.minLearnInputTokens = 0
	s.minInjectBodyBytes = 0
	return s
}

// affinityRequest builds a JSON POST for model carrying a Claude Code style
// metadata.user_id with the given session uuid (empty = no metadata) and any
// extra top-level JSON fields.
func affinityRequest(model, session, extra string) *http.Request {
	body := `{"model":"` + model + `"`
	if session != "" {
		body += `,"metadata":{"user_id":"user_x_account_y_session_` + session + `"}`
	}
	if extra != "" {
		body += "," + extra
	}
	body += "}"
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

// runAffinityMiddleware runs the middleware and returns what the downstream
// handler saw: the raw body, the shared-context data, and the recorder.
func runAffinityMiddleware(t *testing.T, store *slotAffinityStore, cfg config.Config, r *http.Request) (body []byte, data swaputil.ReqContextData, w *httptest.ResponseRecorder, called bool) {
	t.Helper()
	final := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
		body, _ = io.ReadAll(r.Body)
		data, _ = swaputil.ReadContext(r.Context())
	})
	w = httptest.NewRecorder()
	CreateSlotAffinityMiddleware(store, cfg)(final).ServeHTTP(w, r)
	return body, data, w, called
}

// affinityTestServer wires a full Server (real middleware chain, stub router)
// with cfg in place BEFORE the affinity store is built, unlike
// selectorTestServer which swaps cfg after construction. Both small-request
// thresholds are zeroed for the same reason as newAffinityStore; tests that
// exercise the thresholds restore them afterward.
func affinityTestServer(t *testing.T, cfg config.Config, local *stubRouter) *Server {
	t.Helper()
	s := newTestServer(local, newStubRouter(nil, ""))
	s.slotAffinity.Close()
	s.cfg = cfg
	s.slotAffinity = newSlotAffinityStore(cfg)
	s.slotAffinity.minLearnInputTokens = 0
	s.slotAffinity.minInjectBodyBytes = 0
	s.metrics.affinity = s.slotAffinity
	s.routes()
	t.Cleanup(func() {
		s.slotAffinity.Close()
		s.store.Close()
	})
	return s
}

// ---------------------------------------------------------------------------
// store

func TestSlotAffinityStore_LearnAndLookup(t *testing.T) {
	s := newAffinityStore(affinityConfig())
	s.learn("m", affSessionA, 3)
	slot, ok := s.lookup("m", affSessionA)
	require.True(t, ok)
	assert.Equal(t, 3, slot)
}

func TestSlotAffinityStore_LookupUnknownSession(t *testing.T) {
	s := newAffinityStore(affinityConfig())
	s.learn("m", affSessionA, 3)
	_, ok := s.lookup("m", affSessionB)
	assert.False(t, ok)
}

func TestSlotAffinityStore_LearnIgnoredWhenModelNotOptedIn(t *testing.T) {
	s := newAffinityStore(affinityConfig())
	s.learn("plain", affSessionA, 3)
	s.learn("unknown", affSessionA, 3)
	_, ok := s.lookup("plain", affSessionA)
	assert.False(t, ok)
	assert.Equal(t, 0, s.sessionCount("plain"))
	assert.Equal(t, 0, s.sessionCount("unknown"))
}

func TestSlotAffinityStore_LearnIgnoresEmptySessionAndNegativeSlot(t *testing.T) {
	s := newAffinityStore(affinityConfig())
	s.learn("m", "", 3)
	s.learn("m", affSessionA, -1)
	assert.Equal(t, 0, s.sessionCount("m"))
	_, ok := s.lookup("m", "")
	assert.False(t, ok)
}

func TestSlotAffinityStore_LearnOverwritesSlot(t *testing.T) {
	s := newAffinityStore(affinityConfig())
	s.learn("m", affSessionA, 1)
	s.learn("m", affSessionA, 4)
	slot, ok := s.lookup("m", affSessionA)
	require.True(t, ok)
	assert.Equal(t, 4, slot)
	assert.Equal(t, 1, s.sessionCount("m"))
}

func TestSlotAffinityStore_BoundedEvictsLeastRecentlySeen(t *testing.T) {
	s := newAffinityStore(affinityConfig())
	s.maxSessions = 3
	now := time.Unix(1000, 0)
	s.now = func() time.Time { now = now.Add(time.Second); return now }

	s.learn("m", "s1", 1)
	s.learn("m", "s2", 2)
	s.learn("m", "s3", 3)
	assert.Equal(t, 3, s.sessionCount("m"))

	s.learn("m", "s4", 4) // over cap: s1 is the oldest
	assert.Equal(t, 3, s.sessionCount("m"))
	_, ok := s.lookup("m", "s1")
	assert.False(t, ok, "oldest entry must be evicted")
	for _, id := range []string{"s2", "s3", "s4"} {
		_, ok := s.lookup("m", id)
		assert.True(t, ok, "%s must survive", id)
	}
}

func TestSlotAffinityStore_LookupRefreshesRecency(t *testing.T) {
	s := newAffinityStore(affinityConfig())
	s.maxSessions = 2
	now := time.Unix(1000, 0)
	s.now = func() time.Time { now = now.Add(time.Second); return now }

	s.learn("m", "s1", 1)
	s.learn("m", "s2", 2)
	s.lookup("m", "s1") // touch s1, s2 is now the oldest
	s.learn("m", "s3", 3)

	_, ok := s.lookup("m", "s2")
	assert.False(t, ok, "s2 (least recently seen) must be evicted")
	_, ok = s.lookup("m", "s1")
	assert.True(t, ok, "s1 was touched by lookup and must survive")
}

func TestSlotAffinityStore_ReLearnDoesNotEvict(t *testing.T) {
	s := newAffinityStore(affinityConfig())
	s.maxSessions = 2
	s.learn("m", "s1", 1)
	s.learn("m", "s2", 2)
	s.learn("m", "s1", 5) // update in place, must not evict s2
	assert.Equal(t, 2, s.sessionCount("m"))
	_, ok := s.lookup("m", "s2")
	assert.True(t, ok)
}

func TestSlotAffinityStore_ForgetDropsOnlyThatModel(t *testing.T) {
	cfg := affinityConfig()
	cfg.Models["n"] = config.ModelConfig{SlotAffinity: true}
	s := newAffinityStore(cfg)
	s.learn("m", affSessionA, 1)
	s.learn("n", affSessionA, 2)

	s.forget("m")
	_, ok := s.lookup("m", affSessionA)
	assert.False(t, ok)
	slot, ok := s.lookup("n", affSessionA)
	require.True(t, ok)
	assert.Equal(t, 2, slot)
}

func TestSlotAffinityStore_ProcessStateChangeEvictsOnStop(t *testing.T) {
	for _, state := range []string{"stopping", "stopped", "shutdown"} {
		t.Run(state, func(t *testing.T) {
			s := newAffinityStore(affinityConfig())
			s.learn("m", affSessionA, 1)
			s.onProcessStateChange(swaputil.ProcessStateChangeEvent{ProcessName: "m", OldState: "ready", NewState: state})
			assert.Equal(t, 0, s.sessionCount("m"))
		})
	}
}

func TestSlotAffinityStore_ProcessStateChangeKeepsOnRunningStates(t *testing.T) {
	for _, state := range []string{"starting", "ready"} {
		t.Run(state, func(t *testing.T) {
			s := newAffinityStore(affinityConfig())
			s.learn("m", affSessionA, 1)
			s.onProcessStateChange(swaputil.ProcessStateChangeEvent{ProcessName: "m", NewState: state})
			assert.Equal(t, 1, s.sessionCount("m"))
		})
	}
}

func TestSlotAffinityStore_ProcessStateChangeOtherModelUntouched(t *testing.T) {
	s := newAffinityStore(affinityConfig())
	s.learn("m", affSessionA, 1)
	s.onProcessStateChange(swaputil.ProcessStateChangeEvent{ProcessName: "other", NewState: "stopped"})
	assert.Equal(t, 1, s.sessionCount("m"))
}

func TestSlotAffinityStore_EventBusEvictsOnStop(t *testing.T) {
	s := newSlotAffinityStore(affinityConfig())
	defer s.Close()
	s.learn("m", affSessionA, 1)

	event.Emit(swaputil.ProcessStateChangeEvent{ProcessName: "m", OldState: "ready", NewState: "stopped"})
	assert.Eventually(t, func() bool { return s.sessionCount("m") == 0 }, 2*time.Second, 5*time.Millisecond)
}

func TestSlotAffinityStore_CloseUnsubscribes(t *testing.T) {
	s := newSlotAffinityStore(affinityConfig())
	s.Close()
	s.Close() // idempotent
	s.learn("m", affSessionA, 1)

	event.Emit(swaputil.ProcessStateChangeEvent{ProcessName: "m", OldState: "ready", NewState: "stopped"})
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, 1, s.sessionCount("m"), "a closed store must not react to process events")
}

func TestSlotAffinityStore_LearnFromEntry(t *testing.T) {
	s := newAffinityStore(affinityConfig())
	s.learnFromEntry(ActivityLogEntry{
		Model:          "m",
		RespStatusCode: http.StatusOK,
		Metadata:       map[string]string{"session_id": affSessionA, "slot_id": "2"},
	})
	slot, ok := s.lookup("m", affSessionA)
	require.True(t, ok)
	assert.Equal(t, 2, slot)
}

func TestSlotAffinityStore_LearnFromEntryIgnoresNon200(t *testing.T) {
	s := newAffinityStore(affinityConfig())
	s.learnFromEntry(ActivityLogEntry{
		Model:          "m",
		RespStatusCode: http.StatusBadGateway,
		Metadata:       map[string]string{"session_id": affSessionA, "slot_id": "2"},
	})
	assert.Equal(t, 0, s.sessionCount("m"))
}

func TestSlotAffinityStore_LearnFromEntryIgnoresIncompleteMetadata(t *testing.T) {
	cases := map[string]map[string]string{
		"nil metadata":     nil,
		"no slot_id":       {"session_id": affSessionA},
		"no session_id":    {"slot_id": "2"},
		"non-numeric slot": {"session_id": affSessionA, "slot_id": "two"},
		"empty session_id": {"session_id": "", "slot_id": "2"},
	}
	for name, md := range cases {
		t.Run(name, func(t *testing.T) {
			s := newAffinityStore(affinityConfig())
			s.learnFromEntry(ActivityLogEntry{Model: "m", RespStatusCode: http.StatusOK, Metadata: md})
			assert.Equal(t, 0, s.sessionCount("m"))
		})
	}
}

func TestSlotAffinityStore_LearnFromEntryIgnoresSmallPrompt(t *testing.T) {
	// Default thresholds this time: a housekeeping-sized response must not
	// teach the store a slot for the session's full turns.
	s := newSlotAffinityStore(affinityConfig())
	defer s.Close()

	s.learnFromEntry(ActivityLogEntry{
		Model:          "m",
		RespStatusCode: http.StatusOK,
		Tokens:         TokenMetrics{InputTokens: 742},
		Metadata:       map[string]string{"session_id": affSessionA, "slot_id": "2"},
	})
	_, ok := s.lookup("m", affSessionA)
	assert.False(t, ok, "a housekeeping-sized response must not be learned from")

	s.learnFromEntry(ActivityLogEntry{
		Model:          "m",
		RespStatusCode: http.StatusOK,
		Tokens:         TokenMetrics{InputTokens: 12000},
		Metadata:       map[string]string{"session_id": affSessionA, "slot_id": "2"},
	})
	slot, ok := s.lookup("m", affSessionA)
	require.True(t, ok, "a full-turn-sized response must be learned from")
	assert.Equal(t, 2, slot)
}

func TestSlotAffinityStore_AssignSpreadsAcrossSlots(t *testing.T) {
	s := newAffinityStore(affinityConfig())
	slot, ok := s.assign("m", "A", 2)
	require.True(t, ok)
	assert.Equal(t, 0, slot)

	slot, ok = s.assign("m", "B", 2)
	require.True(t, ok)
	assert.Equal(t, 1, slot)

	slot, ok = s.assign("m", "C", 2)
	require.True(t, ok, "tie between slots must break to the lowest id")
	assert.Equal(t, 0, slot)

	slot, ok = s.assign("m", "A", 2)
	require.True(t, ok)
	assert.Equal(t, 0, slot, "an already-assigned session must not be reassigned")
}

func TestSlotAffinityStore_AssignIgnoresStaleSessions(t *testing.T) {
	s := newAffinityStore(affinityConfig())
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }

	slot, ok := s.assign("m", "A", 2)
	require.True(t, ok)
	assert.Equal(t, 0, slot)

	now = now.Add(20 * time.Minute) // A's entry ages out of the active window

	slot, ok = s.assign("m", "B", 2)
	require.True(t, ok, "A no longer counts against slot 0")
	assert.Equal(t, 0, slot)

	slot, ok = s.assign("m", "C", 2)
	require.True(t, ok)
	assert.Equal(t, 1, slot)
}

func TestSlotAffinityStore_AssignRefusesSingleSlotAndDisabled(t *testing.T) {
	s := newAffinityStore(affinityConfig())
	_, ok := s.assign("m", affSessionA, 1)
	assert.False(t, ok, "a single-slot child has nothing to pin")

	_, ok = s.assign("plain", affSessionA, 4)
	assert.False(t, ok, "model did not opt in")
}

func TestSlotAffinityStore_NilSafe(t *testing.T) {
	var s *slotAffinityStore
	assert.NotPanics(t, func() {
		s.learn("m", affSessionA, 1)
		_, ok := s.lookup("m", affSessionA)
		assert.False(t, ok)
		s.forget("m")
		assert.Equal(t, 0, s.sessionCount("m"))
		s.learnFromEntry(ActivityLogEntry{Model: "m", RespStatusCode: 200, Metadata: map[string]string{"session_id": affSessionA, "slot_id": "1"}})
		s.Close()
	})
}

// ---------------------------------------------------------------------------
// middleware

func TestSlotAffinityMiddleware_InjectsLearnedSlot(t *testing.T) {
	cfg := affinityConfig()
	s := newAffinityStore(cfg)
	s.learn("m", affSessionA, 3)

	r := affinityRequest("m", affSessionA, `"temperature":0.5`)
	body, data, _, called := runAffinityMiddleware(t, s, cfg, r)
	require.True(t, called)

	assert.Equal(t, int64(3), gjson.GetBytes(body, "id_slot").Int())
	assert.Equal(t, 0.5, gjson.GetBytes(body, "temperature").Float(), "other fields preserved")
	assert.Equal(t, "m", gjson.GetBytes(body, "model").String())
	assert.Equal(t, strconv.Itoa(len(body)), r.Header.Get("Content-Length"))
	assert.Equal(t, int64(len(body)), r.ContentLength)
	assert.True(t, bytes.Equal(body, data.Body), "shared-context Body must carry the injected bytes")
	assert.Equal(t, "3", data.Metadata[slotAffinityMetadataKey])
	assert.Equal(t, affSessionA, data.Metadata["session_id"], "existing metadata preserved")
}

func TestSlotAffinityMiddleware_NoInjectWhenDisabled(t *testing.T) {
	cfg := affinityConfig()
	s := newAffinityStore(cfg)
	// Bypass the learn() gate to prove the inject side checks opt-in on its own.
	s.byModel["plain"] = map[string]slotAffinityEntry{affSessionA: {slot: 3}}

	r := affinityRequest("plain", affSessionA, "")
	body, data, _, called := runAffinityMiddleware(t, s, cfg, r)
	require.True(t, called)
	assert.False(t, gjson.GetBytes(body, "id_slot").Exists())
	_, stamped := data.Metadata[slotAffinityMetadataKey]
	assert.False(t, stamped)
}

func TestSlotAffinityMiddleware_NoInjectWithoutPriorSlot(t *testing.T) {
	cfg := affinityConfig()
	s := newAffinityStore(cfg)

	r := affinityRequest("m", affSessionA, "")
	body, data, _, called := runAffinityMiddleware(t, s, cfg, r)
	require.True(t, called)
	assert.False(t, gjson.GetBytes(body, "id_slot").Exists())
	_, stamped := data.Metadata[slotAffinityMetadataKey]
	assert.False(t, stamped)
}

func TestSlotAffinityMiddleware_NoInjectWithoutSessionID(t *testing.T) {
	cfg := affinityConfig()
	s := newAffinityStore(cfg)
	s.learn("m", affSessionA, 3)

	r := affinityRequest("m", "", "")
	body, _, _, called := runAffinityMiddleware(t, s, cfg, r)
	require.True(t, called)
	assert.False(t, gjson.GetBytes(body, "id_slot").Exists())
}

func TestSlotAffinityMiddleware_OverridesClientIDSlot(t *testing.T) {
	cfg := affinityConfig()
	s := newAffinityStore(cfg)
	s.learn("m", affSessionA, 3)

	r := affinityRequest("m", affSessionA, `"id_slot":7`)
	body, data, _, called := runAffinityMiddleware(t, s, cfg, r)
	require.True(t, called)
	assert.Equal(t, int64(3), gjson.GetBytes(body, "id_slot").Int(), "learned slot wins over the client's")
	assert.Equal(t, "3", data.Metadata[slotAffinityMetadataKey])
}

func TestSlotAffinityMiddleware_ClientIDSlotPassesThroughWhenNothingLearned(t *testing.T) {
	cfg := affinityConfig()
	s := newAffinityStore(cfg)

	r := affinityRequest("m", affSessionA, `"id_slot":7`)
	body, _, _, called := runAffinityMiddleware(t, s, cfg, r)
	require.True(t, called)
	assert.Equal(t, int64(7), gjson.GetBytes(body, "id_slot").Int())
}

func TestSlotAffinityMiddleware_NoInjectForSmallBody(t *testing.T) {
	// Default thresholds: a learned slot exists, but a housekeeping-sized
	// body must pass through untouched, while a body padded past the
	// threshold gets the injection.
	cfg := affinityConfig()
	s := newSlotAffinityStore(cfg)
	defer s.Close()
	s.learn("m", affSessionA, 3)

	r := affinityRequest("m", affSessionA, "")
	body, data, _, called := runAffinityMiddleware(t, s, cfg, r)
	require.True(t, called)
	assert.False(t, gjson.GetBytes(body, "id_slot").Exists(), "small body must not receive the learned slot")
	_, stamped := data.Metadata[slotAffinityMetadataKey]
	assert.False(t, stamped)

	pad := `"pad":"` + strings.Repeat("x", 17000) + `"`
	r = affinityRequest("m", affSessionA, pad)
	body, data, _, called = runAffinityMiddleware(t, s, cfg, r)
	require.True(t, called)
	assert.Equal(t, int64(3), gjson.GetBytes(body, "id_slot").Int(), "body above the threshold gets the learned slot")
	assert.Equal(t, "3", data.Metadata[slotAffinityMetadataKey])
}

func TestSlotAffinityMiddleware_AssignsOnFirstRequest(t *testing.T) {
	cfg := config.Config{Models: map[string]config.ModelConfig{
		"m": {SlotAffinity: true, ConcurrencyLimit: 2},
	}}
	s := newAffinityStore(cfg)

	r := affinityRequest("m", affSessionA, "")
	body, data, _, called := runAffinityMiddleware(t, s, cfg, r)
	require.True(t, called)
	assert.Equal(t, int64(0), gjson.GetBytes(body, "id_slot").Int())
	assert.Equal(t, "0", data.Metadata[slotAffinityMetadataKey])

	r = affinityRequest("m", affSessionB, "")
	body, data, _, called = runAffinityMiddleware(t, s, cfg, r)
	require.True(t, called)
	assert.Equal(t, int64(1), gjson.GetBytes(body, "id_slot").Int())
	assert.Equal(t, "1", data.Metadata[slotAffinityMetadataKey])

	// Second request for A: the assigned slot is reused, not reassigned.
	r = affinityRequest("m", affSessionA, "")
	body, _, _, called = runAffinityMiddleware(t, s, cfg, r)
	require.True(t, called)
	assert.Equal(t, int64(0), gjson.GetBytes(body, "id_slot").Int())
}

func TestSlotAffinityMiddleware_AliasResolvesToRealModel(t *testing.T) {
	// Aliases are indexed at load time, so build the config the real way.
	cfg, err := config.LoadConfigFromReader(strings.NewReader(`
models:
  m:
    cmd: echo ${PORT}
    aliases: [m-alias]
    slotAffinity: true
`))
	require.NoError(t, err)
	s := newAffinityStore(cfg)
	s.learn("m", affSessionA, 2)

	r := affinityRequest("m-alias", affSessionA, "")
	body, _, _, called := runAffinityMiddleware(t, s, cfg, r)
	require.True(t, called)
	assert.Equal(t, int64(2), gjson.GetBytes(body, "id_slot").Int())
}

func TestSlotAffinityMiddleware_PassthroughNonJSON(t *testing.T) {
	cfg := affinityConfig()
	s := newAffinityStore(cfg)
	s.learn("m", affSessionA, 3)

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("model=m"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	body, _, _, called := runAffinityMiddleware(t, s, cfg, r)
	require.True(t, called)
	assert.Equal(t, "model=m", string(body))
}

func TestSlotAffinityMiddleware_PassthroughGET(t *testing.T) {
	cfg := affinityConfig()
	s := newAffinityStore(cfg)
	s.learn("m", affSessionA, 3)

	r := httptest.NewRequest(http.MethodGet, "/v1/chat/completions?model=m", nil)
	r.Header.Set("Content-Type", "application/json")
	_, _, _, called := runAffinityMiddleware(t, s, cfg, r)
	assert.True(t, called)
}

func TestSlotAffinityMiddleware_PassthroughNilStore(t *testing.T) {
	cfg := affinityConfig()
	r := affinityRequest("m", affSessionA, "")
	body, _, _, called := runAffinityMiddleware(t, nil, cfg, r)
	require.True(t, called)
	assert.False(t, gjson.GetBytes(body, "id_slot").Exists())
}

func TestSlotAffinityMiddleware_PassthroughInvalidJSON(t *testing.T) {
	cfg := affinityConfig()
	s := newAffinityStore(cfg)
	s.learn("m", affSessionA, 3)

	// A pre-set context lets FetchContext succeed while the body is garbage.
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{not json"))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(swaputil.SetContext(r.Context(), swaputil.ReqContextData{
		Model: "m", ModelID: "m",
		Metadata: map[string]string{"session_id": affSessionA},
	}))
	body, _, _, called := runAffinityMiddleware(t, s, cfg, r)
	require.True(t, called)
	assert.Equal(t, "{not json", string(body))
}

func TestSlotAffinityMiddleware_UnknownModelPassesThrough(t *testing.T) {
	// FetchContext resolves an unknown model id to itself and leaves the 404
	// to the dispatcher; the middleware must not get in the way of that.
	cfg := affinityConfig()
	s := newAffinityStore(cfg)

	r := affinityRequest("nope", affSessionA, "")
	body, _, _, called := runAffinityMiddleware(t, s, cfg, r)
	require.True(t, called)
	assert.False(t, gjson.GetBytes(body, "id_slot").Exists())
}

// ---------------------------------------------------------------------------
// metrics.record learns

func TestMetricsMonitor_RecordLearnsSlotAffinity(t *testing.T) {
	cfg := affinityConfig()
	mm := newTestMetricsMonitor(t, nil, 10, 0)
	mm.affinity = newAffinityStore(cfg)

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	r = r.WithContext(swaputil.SetContext(r.Context(), swaputil.ReqContextData{
		ModelID:  "m",
		Metadata: map[string]string{"session_id": affSessionA},
	}))
	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.WriteHeader(http.StatusOK)
	copier.Write([]byte(`{"id_slot":5,"usage":{"prompt_tokens":1,"completion_tokens":2}}`))

	mm.record("m", r, copier, 0, nil)

	slot, ok := mm.affinity.lookup("m", affSessionA)
	require.True(t, ok)
	assert.Equal(t, 5, slot)
}

func TestMetricsMonitor_RecordLearnsFromStreamingSlotID(t *testing.T) {
	cfg := affinityConfig()
	mm := newTestMetricsMonitor(t, nil, 10, 0)
	mm.affinity = newAffinityStore(cfg)

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	r = r.WithContext(swaputil.SetContext(r.Context(), swaputil.ReqContextData{
		ModelID:  "m",
		Metadata: map[string]string{"session_id": affSessionA},
	}))
	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.Header().Set("Content-Type", "text/event-stream")
	copier.WriteHeader(http.StatusOK)
	copier.Write([]byte("data: {\"id_slot\":1,\"choices\":[]}\n\ndata: {\"id_slot\":1,\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1},\"timings\":{\"prompt_n\":1,\"predicted_n\":1}}\n\ndata: [DONE]\n\n"))

	mm.record("m", r, copier, 0, nil)

	slot, ok := mm.affinity.lookup("m", affSessionA)
	require.True(t, ok)
	assert.Equal(t, 1, slot)
}

func TestMetricsMonitor_RecordDoesNotLearnOnFailure(t *testing.T) {
	cfg := affinityConfig()
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 10, 0)
	mm.affinity = newAffinityStore(cfg)

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	r = r.WithContext(swaputil.SetContext(r.Context(), swaputil.ReqContextData{
		ModelID:  "m",
		Metadata: map[string]string{"session_id": affSessionA},
	}))
	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.WriteHeader(http.StatusBadGateway)
	copier.Write([]byte(`{"id_slot":5,"error":"boom"}`))

	mm.record("m", r, copier, 0, nil)
	assert.Equal(t, 0, mm.affinity.sessionCount("m"))
}

// ---------------------------------------------------------------------------
// end to end through the real middleware chain

func TestServer_SlotAffinity_LearnThenInject(t *testing.T) {
	cfg := affinityConfig()
	local := newStubRouter([]string{"m", "plain"}, "")
	var bodies [][]byte
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id_slot":2,"usage":{"prompt_tokens":2,"completion_tokens":3}}`))
	}
	s := affinityTestServer(t, cfg, local)

	// Turn 1: nothing learned yet, no id_slot goes upstream.
	w := httptest.NewRecorder()
	s.ServeHTTP(w, affinityRequest("m", affSessionA, ""))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Len(t, bodies, 1)
	assert.False(t, gjson.GetBytes(bodies[0], "id_slot").Exists())

	// Turn 2, same session: the slot the child reported is injected.
	w = httptest.NewRecorder()
	s.ServeHTTP(w, affinityRequest("m", affSessionA, ""))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Len(t, bodies, 2)
	assert.Equal(t, int64(2), gjson.GetBytes(bodies[1], "id_slot").Int())

	// A different session on the same model is not pinned.
	w = httptest.NewRecorder()
	s.ServeHTTP(w, affinityRequest("m", affSessionB, ""))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Len(t, bodies, 3)
	assert.False(t, gjson.GetBytes(bodies[2], "id_slot").Exists())

	// Activity: entry 1 carries slot_id=2; entry 2 carries slot_affinity=2
	// equal to entry 1's slot_id (the handoff's verification rule).
	entries := metricsEntries(t, s.metrics)
	require.Len(t, entries, 3)
	byID := map[int]ActivityLogEntry{}
	for _, e := range entries {
		byID[e.ID] = e
	}
	first, second := byID[1], byID[2]
	assert.Equal(t, "2", first.Metadata["slot_id"])
	_, stamped := first.Metadata[slotAffinityMetadataKey]
	assert.False(t, stamped, "turn 1 had nothing to inject")
	assert.Equal(t, first.Metadata["slot_id"], second.Metadata[slotAffinityMetadataKey])
	assert.Equal(t, affSessionA, second.Metadata["session_id"])
}

func TestServer_SlotAffinity_AssignWithoutLearn(t *testing.T) {
	// The stub upstream returns an Anthropic-shaped body with no id_slot at
	// all, mirroring llama-server's real Anthropic-format serializers: this
	// must not stop the middleware from pinning a slot, because assignment
	// (not learning from the response) is the primary mechanism.
	cfg := config.Config{Models: map[string]config.ModelConfig{
		"m": {SlotAffinity: true, ConcurrencyLimit: 2},
	}}
	local := newStubRouter([]string{"m"}, "")
	var bodies [][]byte
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"type":"message","usage":{"input_tokens":5,"output_tokens":3}}`))
	}
	s := affinityTestServer(t, cfg, local)

	w := httptest.NewRecorder()
	s.ServeHTTP(w, affinityRequest("m", affSessionA, ""))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Len(t, bodies, 1)
	assert.Equal(t, int64(0), gjson.GetBytes(bodies[0], "id_slot").Int(), "the very first request must already be assigned a slot")

	w = httptest.NewRecorder()
	s.ServeHTTP(w, affinityRequest("m", affSessionA, ""))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Len(t, bodies, 2)
	assert.Equal(t, int64(0), gjson.GetBytes(bodies[1], "id_slot").Int(), "the same session must keep its assigned slot")
}

func TestServer_SlotAffinity_HousekeepingDoesNotSteal(t *testing.T) {
	// affinityTestServer zeroes both thresholds; restore the defaults here so
	// this test exercises the real small-request exclusion end to end.
	cfg := affinityConfig()
	local := newStubRouter([]string{"m"}, "")
	var bodies [][]byte
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, b)
		w.Header().Set("Content-Type", "application/json")
		if len(b) >= slotAffinityMinInjectBodyBytes {
			w.Write([]byte(`{"id_slot":0,"usage":{"prompt_tokens":12000,"completion_tokens":3}}`))
		} else {
			w.Write([]byte(`{"id_slot":1,"usage":{"prompt_tokens":500,"completion_tokens":3}}`))
		}
	}
	s := affinityTestServer(t, cfg, local)
	s.slotAffinity.minLearnInputTokens = slotAffinityMinLearnInputTokens
	s.slotAffinity.minInjectBodyBytes = slotAffinityMinInjectBodyBytes

	bigPad := `"pad":"` + strings.Repeat("x", 17000) + `"`

	// Big turn 1: learns slot 0 (nothing injected yet, turn 1 of the session).
	w := httptest.NewRecorder()
	s.ServeHTTP(w, affinityRequest("m", affSessionA, bigPad))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Len(t, bodies, 1)
	assert.False(t, gjson.GetBytes(bodies[0], "id_slot").Exists())
	slot, ok := s.slotAffinity.lookup("m", affSessionA)
	require.True(t, ok)
	assert.Equal(t, 0, slot)

	// Small housekeeping call, same session: must carry no id_slot upstream
	// and must not disturb the learned slot.
	w = httptest.NewRecorder()
	s.ServeHTTP(w, affinityRequest("m", affSessionA, ""))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Len(t, bodies, 2)
	assert.False(t, gjson.GetBytes(bodies[1], "id_slot").Exists(), "housekeeping call must not receive the learned slot")
	slot, ok = s.slotAffinity.lookup("m", affSessionA)
	require.True(t, ok)
	assert.Equal(t, 0, slot, "housekeeping response must not overwrite the learned slot")

	// Big turn again: the learned slot is injected.
	w = httptest.NewRecorder()
	s.ServeHTTP(w, affinityRequest("m", affSessionA, bigPad))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Len(t, bodies, 3)
	assert.Equal(t, int64(0), gjson.GetBytes(bodies[2], "id_slot").Int())
}

func TestServer_SlotAffinity_DisabledModelNeverInjects(t *testing.T) {
	cfg := affinityConfig()
	local := newStubRouter([]string{"m", "plain"}, "")
	var bodies [][]byte
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id_slot":2,"usage":{"prompt_tokens":2,"completion_tokens":3}}`))
	}
	s := affinityTestServer(t, cfg, local)

	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, affinityRequest("plain", affSessionA, ""))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	}
	require.Len(t, bodies, 2)
	assert.False(t, gjson.GetBytes(bodies[1], "id_slot").Exists())
	assert.Equal(t, 0, s.slotAffinity.sessionCount("plain"))
}

func TestServer_SlotAffinity_ForgottenAfterProcessStop(t *testing.T) {
	cfg := affinityConfig()
	local := newStubRouter([]string{"m"}, "")
	var bodies [][]byte
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id_slot":2,"usage":{"prompt_tokens":2,"completion_tokens":3}}`))
	}
	s := affinityTestServer(t, cfg, local)

	s.ServeHTTP(httptest.NewRecorder(), affinityRequest("m", affSessionA, ""))
	require.Equal(t, 1, s.slotAffinity.sessionCount("m"))

	// The model is swapped out: its process reports "stopped".
	event.Emit(swaputil.ProcessStateChangeEvent{ProcessName: "m", OldState: "ready", NewState: "stopped"})
	require.Eventually(t, func() bool { return s.slotAffinity.sessionCount("m") == 0 }, 2*time.Second, 5*time.Millisecond)

	s.ServeHTTP(httptest.NewRecorder(), affinityRequest("m", affSessionA, ""))
	require.Len(t, bodies, 2)
	assert.False(t, gjson.GetBytes(bodies[1], "id_slot").Exists(), "a fresh process has fresh slots; nothing may be injected")
}

func TestServer_SlotAffinity_FiltersRunBeforeInjection(t *testing.T) {
	// stripParams: id_slot must NOT strip the injected value, because the
	// affinity middleware runs after the filters.
	cfg := config.Config{Models: map[string]config.ModelConfig{
		"m": {
			SlotAffinity: true,
			Filters:      config.ModelFilters{Filters: config.Filters{StripParams: "id_slot"}},
		},
	}}
	local := newStubRouter([]string{"m"}, "")
	var bodies [][]byte
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id_slot":4,"usage":{"prompt_tokens":2,"completion_tokens":3}}`))
	}
	s := affinityTestServer(t, cfg, local)

	// Turn 1 with a client id_slot: stripped by the filter (nothing learned).
	s.ServeHTTP(httptest.NewRecorder(), affinityRequest("m", affSessionA, `"id_slot":9`))
	require.Len(t, bodies, 1)
	assert.False(t, gjson.GetBytes(bodies[0], "id_slot").Exists())

	// Turn 2: the learned slot survives the strip filter.
	s.ServeHTTP(httptest.NewRecorder(), affinityRequest("m", affSessionA, ""))
	require.Len(t, bodies, 2)
	assert.Equal(t, int64(4), gjson.GetBytes(bodies[1], "id_slot").Int())
}

func TestConfig_SlotAffinityYAML(t *testing.T) {
	cfg, err := config.LoadConfigFromReader(strings.NewReader(`
models:
  pinned:
    cmd: echo ${PORT}
    slotAffinity: true
  free:
    cmd: echo ${PORT}
`))
	require.NoError(t, err)
	assert.True(t, cfg.Models["pinned"].SlotAffinity)
	assert.False(t, cfg.Models["free"].SlotAffinity, "default must be off")
}

// ---------------------------------------------------------------------------
// subagent lanes and resident-alias turns (2026-09-08)
//
// A Claude Code subagent (Agent tool) reuses its parent's session id and adds
// X-Claude-Code-Agent-Id. Its turns are full-size (13-19k input tokens, 130 KB
// bodies) and run CONCURRENTLY with the parent's own turns, so keying affinity
// on the session id alone pins parent and child to one slot while the other
// slot idles. Witnessed 2026-09-08 on cq35h session 934b47c6: parent turns
// carried slot_affinity=0, the agent's turns carried no slot_affinity at all
// (they arrive under the resident alias claude-haiku-4-5-20251001, which is
// not a model block, so the middleware declined before the alias resolved).

// affinityHeaderRequest is affinityRequest with the identity carried the way
// current Claude Code builds carry it: headers, not metadata.user_id.
func affinityHeaderRequest(model, session, agent string, extra string) *http.Request {
	r := affinityRequest(model, "", extra)
	r.Header.Set("X-Claude-Code-Session-Id", session)
	if agent != "" {
		r.Header.Set("X-Claude-Code-Agent-Id", agent)
	}
	return r
}

func TestSlotAffinityMiddleware_SubagentGetsOwnLane(t *testing.T) {
	cfg := config.Config{Models: map[string]config.ModelConfig{
		"m": {SlotAffinity: true, ConcurrencyLimit: 2},
	}}
	s := newAffinityStore(cfg)

	// Parent's full turn: first lane on the model, lowest slot.
	body, data, _, called := runAffinityMiddleware(t, s, cfg, affinityHeaderRequest("m", affSessionA, "", ""))
	require.True(t, called)
	require.True(t, gjson.GetBytes(body, "id_slot").Exists(), "parent turn must be pinned")
	parentSlot := gjson.GetBytes(body, "id_slot").Int()
	assert.Equal(t, strconv.FormatInt(parentSlot, 10), data.Metadata[slotAffinityMetadataKey])

	// Subagent's full turn on the SAME session: must get the other slot, not
	// ride the parent's lane (that is what serialises them on one slot).
	body, data, _, called = runAffinityMiddleware(t, s, cfg, affinityHeaderRequest("m", affSessionA, "a4c4e94e633cf6841", ""))
	require.True(t, called)
	require.True(t, gjson.GetBytes(body, "id_slot").Exists(), "subagent turn must be pinned too")
	agentSlot := gjson.GetBytes(body, "id_slot").Int()
	assert.NotEqual(t, parentSlot, agentSlot, "subagent must not share the parent's slot")
	assert.Equal(t, strconv.FormatInt(agentSlot, 10), data.Metadata[slotAffinityMetadataKey])

	// And the parent keeps its own lane on the next turn.
	body, _, _, _ = runAffinityMiddleware(t, s, cfg, affinityHeaderRequest("m", affSessionA, "", ""))
	assert.Equal(t, parentSlot, gjson.GetBytes(body, "id_slot").Int(), "parent lane must be stable")
}

func TestSlotAffinityMiddleware_ResidentAliasTurnIsPinnedOnResolvedModel(t *testing.T) {
	cfg := config.Config{
		Models: map[string]config.ModelConfig{
			"m": {SlotAffinity: true, ConcurrencyLimit: 2},
		},
		ResidentAliases: []string{"claude-*"},
	}
	s := newAffinityStore(cfg)
	// The server wires this to resolveResidentAlias over the local router's
	// running models; the unit test stands in for "m is resident".
	s.resolveResident = func(requested string) (string, bool) {
		if cfg.MatchesResidentAlias(requested) {
			return "m", true
		}
		return "", false
	}

	r := affinityHeaderRequest("claude-haiku-4-5-20251001", affSessionA, "a4c4e94e633cf6841", "")
	body, data, _, called := runAffinityMiddleware(t, s, cfg, r)
	require.True(t, called)
	assert.True(t, gjson.GetBytes(body, "id_slot").Exists(), "a resident-alias turn must be pinned like any full turn")
	_, stamped := data.Metadata[slotAffinityMetadataKey]
	assert.True(t, stamped)
	assert.Equal(t, "m", data.Metadata["resolved_model"], "the resolved model must be visible to renderers")
}

// Full chain: a subagent turn under the resident alias reaches the child with
// id_slot injected, and the LIVE in-flight entry (what /api/events publishes
// to the menu and cm-menu) carries agent_id, resolved_model and slot_affinity
// while the request is being served.
func TestServer_ResidentAliasSubagentTurn_LiveEntryCarriesLaneAndResolvedModel(t *testing.T) {
	cfg, err := loadResidentAliasConfig(`
models:
  modelA:
    cmd: echo a ${PORT}
    slotAffinity: true
    concurrencyLimit: 2
residentAliases:
  - "claude-*"
`)
	require.NoError(t, err)

	var seenBody []byte
	var live swaputil.InflightRequestEntry
	local := newStubRouter([]string{"modelA"}, "")
	local.running = map[string]process.ProcessState{"modelA": process.StateReady}
	s := affinityTestServer(t, cfg, local)
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		seenBody, _ = io.ReadAll(r.Body)
		for _, e := range s.inflight.Current().Requests {
			live = e
		}
		w.WriteHeader(http.StatusOK)
	}

	body := `{"model":"claude-haiku-4-5-20251001","messages":[{"role":"user","content":"` +
		strings.Repeat("x", 20000) + `"}]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Claude-Code-Session-Id", affSessionA)
	r.Header.Set("X-Claude-Code-Agent-Id", "a4c4e94e633cf6841")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.True(t, gjson.GetBytes(seenBody, "id_slot").Exists(), "child must receive id_slot")
	assert.Equal(t, "a4c4e94e633cf6841", live.Metadata["agent_id"])
	assert.Equal(t, "modelA", live.Metadata["resolved_model"])
	assert.NotEmpty(t, live.Metadata[slotAffinityMetadataKey])
}
