package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/membrake"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/router/scheduler"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"github.com/stretchr/testify/require"
)

// The session-state contract (llama-swap.sessions/v1) is owned by llama-cm:
// docs/intent/session-state-contract.md. The golden tests below read its
// canonical fixtures from the sibling checkout rather than a copy, so the
// fork can never drift from the contract silently.
const sessionsFixtureDir = "../../../llama-cm/docs/intent/session-state-contract.fixtures"

const sessModel = "Qwen3.8-27B-UD-Q5_K_XL"

func sessionsTestConfig() config.Config {
	return config.Config{
		Models: map[string]config.ModelConfig{
			sessModel: {Aliases: []string{"cq27"}, ConcurrencyLimit: 2},
		},
		Tiers: map[string]config.TierConfig{
			"priority":   {Listen: "127.0.0.1:8002", Rank: 10},
			"background": {Listen: "127.0.0.1:8003", Rank: -10},
		},
		// What LoadConfigFromReader seeds when the sessions block is absent
		// (config.DefaultSessionsConfig), so the retention tests exercise the
		// window a real box runs.
		Sessions: config.DefaultSessionsConfig(),
	}
}

func loadSessionsFixture(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(sessionsFixtureDir, name))
	if err != nil {
		t.Skipf("contract fixture not available (%v); llama-cm checkout expected next to the fork", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return out
}

func bodyAsMap(t *testing.T, body sessionsBody) map[string]any {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func assertGolden(t *testing.T, got sessionsBody, fixture string) {
	t.Helper()
	want := loadSessionsFixture(t, fixture)
	have := bodyAsMap(t, got)
	if !reflect.DeepEqual(have, want) {
		h, _ := json.MarshalIndent(have, "", "  ")
		w, _ := json.MarshalIndent(want, "", "  ")
		t.Fatalf("body != fixture %s\n got: %s\nwant: %s", fixture, h, w)
	}
	assertSessionsInvariants(t, got)
}

// assertSessionsInvariants pins the contract's invariants 1, 2 and 4 on any
// body the server builds.
func assertSessionsInvariants(t *testing.T, b sessionsBody) {
	t.Helper()
	if b.Schema != "llama-swap.sessions/v1" {
		t.Errorf("schema=%q", b.Schema)
	}
	parked := 0
	perTier := map[string]int{}
	for _, s := range b.Sessions {
		c := s.Context
		if c.Used != c.Cached+c.Processed+c.Decoded {
			t.Errorf("%s: used %d != cached %d + processed %d + decoded %d", s.SessionShort, c.Used, c.Cached, c.Processed, c.Decoded)
		}
		if s.Phase == "PARKED" {
			parked++
			perTier[s.Tier]++
		} else if s.ParkReason != nil {
			t.Errorf("%s: parkReason set on phase %s", s.SessionShort, s.Phase)
		}
		if s.Rate.TokensPerSecond != nil && *s.Rate.TokensPerSecond < 0 {
			t.Errorf("%s: negative rate %v", s.SessionShort, *s.Rate.TokensPerSecond)
		}
	}
	if b.Queue.Waiting != parked {
		t.Errorf("queue.waiting=%d, PARKED rows=%d", b.Queue.Waiting, parked)
	}
	for tier, n := range b.Queue.ByTier {
		if perTier[tier] != n {
			t.Errorf("byTier[%s]=%d, PARKED rows on that tier=%d", tier, n, perTier[tier])
		}
	}
	for tier, n := range perTier {
		if _, ok := b.Queue.ByTier[tier]; !ok {
			t.Errorf("byTier missing tier %s with %d parked", tier, n)
		}
	}
}

func fixtureClock(t *testing.T, stamp string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func enabledBrake() membrake.Status { return membrake.Status{Enabled: true} }

func TestSessions_Golden_EmptyBox(t *testing.T) {
	b := newSessionsBuilder(sessionsTestConfig())
	now := fixtureClock(t, "2026-09-18T16:30:33.000+02:00")
	got := b.build(sessionsInput{Now: now, MemoryBrake: enabledBrake()})
	assertGolden(t, got, "empty-box.json")
}

func TestSessions_Golden_PrefillCacheResumed(t *testing.T) {
	b := newSessionsBuilder(sessionsTestConfig())
	now := fixtureClock(t, "2026-09-18T19:27:03.412+02:00")
	req := swaputil.InflightRequestEntry{
		ID: "r-41", Model: sessModel, Timestamp: now.Add(-118400 * time.Millisecond),
		Metadata: map[string]string{"session_id": "69699f8b-0b1e-4c3a-9f1e-2d7c5a1b9e00", "slot_granted": "1", "slot_id": "0", "tier": "default", "est_tokens": "102400"},
	}
	running := map[string]process.ProcessState{sessModel: process.StateReady}
	// The child's n_prompt_tokens is what it has REACHED (cache + processed),
	// not the whole prompt: measured 2026-09-18 at 92,170 of ~102K. Progress
	// must come from the router's est_tokens, or it reads ~100% all prefill.
	slotsAt := func(processed int) map[string][]childSlot {
		return map[string][]childSlot{sessModel: {
			{ID: 0, NCtx: 262144, IsProcessing: true, NPromptTokens: 79603 + processed, NPromptTokensCache: 79603, NPromptTokensProcessed: processed},
			{ID: 1, NCtx: 262144},
		}}
	}
	// Two poll samples 30 s apart: 1521 prompt tokens computed -> 50.7 t/s.
	// The 79,603 cached tokens must never enter the rate.
	b.build(sessionsInput{Now: now.Add(-30 * time.Second), Running: running, Requests: []swaputil.InflightRequestEntry{req}, Slots: slotsAt(11046), MemoryBrake: enabledBrake()})
	got := b.build(sessionsInput{Now: now, Running: running, Requests: []swaputil.InflightRequestEntry{req}, Slots: slotsAt(12567), MemoryBrake: enabledBrake()})
	assertGolden(t, got, "prefill-cache-resumed.json")
}

func TestSessions_Golden_DecodeParkedHot(t *testing.T) {
	b := newSessionsBuilder(sessionsTestConfig())
	now := fixtureClock(t, "2026-09-18T19:40:11.009+02:00")
	arrival := now.Add(-61 * time.Second)
	decode := swaputil.InflightRequestEntry{
		ID: "r-55", Model: sessModel, Timestamp: arrival, RespTokens: 96,
		Metadata: map[string]string{"session_id": "69699f8b-0b1e-4c3a-9f1e-2d7c5a1b9e00", "slot_granted": "1", "slot_id": "0", "tier": "default"},
	}
	parked := swaputil.InflightRequestEntry{
		ID: "r-57", Model: sessModel, Timestamp: now.Add(-4200 * time.Millisecond),
		Metadata: map[string]string{"session_id": "a1b2c3d4-1111-2222-3333-444455556666", "park_reason": "kv", "kv_parked": "1", "tier": "priority"},
	}
	running := map[string]process.ProcessState{sessModel: process.StateReady}
	hot := map[string][]swaputil.HotSlot{sessModel: {
		{Slot: 0, SessionID: "69699f8b-0b1e-4c3a-9f1e-2d7c5a1b9e00"},
		{Slot: 1, SessionID: "0f0e0d0c-aaaa-bbbb-cccc-dddddddddddd", IdleSeconds: 95},
	}}
	slots := func(processed, decoded int) map[string][]childSlot {
		return map[string][]childSlot{sessModel: {
			{ID: 0, NCtx: 262144, IsProcessing: true, NPromptTokens: 102200, NPromptTokensCache: 102200 - processed, NPromptTokensProcessed: processed, NDecoded: decoded},
			{ID: 1, NCtx: 262144, NPromptTokens: 31844, NPromptTokensCache: 31000, NPromptTokensProcessed: 844, NDecoded: 12},
		}}
	}
	in := func(at time.Time, reqs []swaputil.InflightRequestEntry, s map[string][]childSlot) sessionsInput {
		return sessionsInput{Now: at, Running: running, Requests: reqs, Slots: s, Hot: hot, MemoryBrake: enabledBrake()}
	}
	b.build(in(arrival.Add(2*time.Second), []swaputil.InflightRequestEntry{decode}, slots(0, 0)))
	b.build(in(arrival.Add(3*time.Second), []swaputil.InflightRequestEntry{decode}, slots(0, 1)))
	b.build(in(arrival.Add(31*time.Second), []swaputil.InflightRequestEntry{decode}, slots(0, 199)))
	got := b.build(in(now, []swaputil.InflightRequestEntry{decode, parked}, slots(0, 412)))
	assertGolden(t, got, "decode-parked-hot.json")
}

// Invariant 4: a new request on the same session resets processed; the rate
// must restart from null, never go negative or spike.
func TestSessions_RateResetOnNewRequest(t *testing.T) {
	b := newSessionsBuilder(sessionsTestConfig())
	t0 := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	running := map[string]process.ProcessState{sessModel: process.StateReady}
	mk := func(id string) swaputil.InflightRequestEntry {
		return swaputil.InflightRequestEntry{ID: id, Model: sessModel, Timestamp: t0,
			Metadata: map[string]string{"session_id": "sess-aaaaaaaa", "slot_granted": "1", "slot_id": "0"}}
	}
	sl := func(cache, processed int) map[string][]childSlot {
		return map[string][]childSlot{sessModel: {{ID: 0, NCtx: 1000, IsProcessing: true, NPromptTokens: 90000, NPromptTokensCache: cache, NPromptTokensProcessed: processed}}}
	}
	b.build(sessionsInput{Now: t0, Running: running, Requests: []swaputil.InflightRequestEntry{mk("1")}, Slots: sl(0, 1000)})
	b.build(sessionsInput{Now: t0.Add(time.Second), Running: running, Requests: []swaputil.InflightRequestEntry{mk("1")}, Slots: sl(0, 1200)})
	// New request: 80k cached, processed back to 10.
	got := b.build(sessionsInput{Now: t0.Add(2 * time.Second), Running: running, Requests: []swaputil.InflightRequestEntry{mk("2")}, Slots: sl(80000, 10)})
	assertSessionsInvariants(t, got)
	if r := got.Sessions[0].Rate.TokensPerSecond; r != nil {
		t.Fatalf("rate after a new request = %v, want null until two samples of the new request", *r)
	}
	got = b.build(sessionsInput{Now: t0.Add(3 * time.Second), Running: running, Requests: []swaputil.InflightRequestEntry{mk("2")}, Slots: sl(80000, 110)})
	if r := got.Sessions[0].Rate.TokensPerSecond; r == nil || *r != 100 {
		t.Fatalf("rate = %v, want 100 (processed delta only, cached excluded)", r)
	}
	// Same request id but processed went backwards (child restarted the
	// prompt): must not produce a negative rate.
	got = b.build(sessionsInput{Now: t0.Add(4 * time.Second), Running: running, Requests: []swaputil.InflightRequestEntry{mk("2")}, Slots: sl(0, 5)})
	assertSessionsInvariants(t, got)
	if r := got.Sessions[0].Rate.TokensPerSecond; r != nil {
		t.Fatalf("rate after processed reset = %v, want null", *r)
	}
}

// An entry with no session id gets sessionShort from its request id; a
// finished session stays IDLE for 60 s, then is dropped.
func TestSessions_IdleRetentionAndShort(t *testing.T) {
	b := newSessionsBuilder(sessionsTestConfig())
	t0 := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	running := map[string]process.ProcessState{sessModel: process.StateReady}
	withSess := swaputil.InflightRequestEntry{ID: "7", Model: sessModel, Timestamp: t0,
		Metadata: map[string]string{"session_id": "abcdef0123456789", "park_reason": "busy"}}
	anon := swaputil.InflightRequestEntry{ID: "request-long-id", Model: sessModel, Timestamp: t0}
	got := b.build(sessionsInput{Now: t0, Running: running, Requests: []swaputil.InflightRequestEntry{withSess, anon}})
	assertSessionsInvariants(t, got)
	shorts := map[string]bool{}
	for _, s := range got.Sessions {
		shorts[s.SessionShort] = true
	}
	if !shorts["abcdef01"] || !shorts["request-"] {
		t.Fatalf("sessionShort values %v, want abcdef01 and request-", shorts)
	}
	got = b.build(sessionsInput{Now: t0.Add(10 * time.Second), Running: running})
	if len(got.Sessions) != 1 || got.Sessions[0].Phase != "IDLE" || got.Sessions[0].RequestID != nil {
		t.Fatalf("after finish want one IDLE row for the known session, got %+v", got.Sessions)
	}
	if got.Sessions[0].PhaseSinceMs != 0 {
		t.Errorf("IDLE phaseSinceMs=%d want 0 at the moment it went idle", got.Sessions[0].PhaseSinceMs)
	}
	got = b.build(sessionsInput{Now: t0.Add(69 * time.Second), Running: running})
	if len(got.Sessions) != 1 || got.Sessions[0].PhaseSinceMs != 59000 {
		t.Fatalf("IDLE row must survive 59 s, got %+v", got.Sessions)
	}
	got = b.build(sessionsInput{Now: t0.Add(71 * time.Second), Running: running})
	if len(got.Sessions) != 0 {
		t.Fatalf("IDLE row must be dropped after 60 s, got %+v", got.Sessions)
	}
}

// A granted request whose model is still loading reads LOADING.
func TestSessions_LoadingPhase(t *testing.T) {
	b := newSessionsBuilder(sessionsTestConfig())
	t0 := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	req := swaputil.InflightRequestEntry{ID: "1", Model: sessModel, Timestamp: t0,
		Metadata: map[string]string{"session_id": "s1", "slot_granted": "1"}}
	got := b.build(sessionsInput{Now: t0, Running: map[string]process.ProcessState{sessModel: process.StateStarting}, Requests: []swaputil.InflightRequestEntry{req}})
	if got.Sessions[0].Phase != "LOADING" || got.Resident == nil || got.Resident.State != "loading" {
		t.Fatalf("got phase %s resident %+v", got.Sessions[0].Phase, got.Resident)
	}
}

// Invariant 5 on the parser side: unknown child fields are tolerated, and a
// child without n_prompt_tokens_cache reads 0.
func TestSessions_ParseChildSlotsTolerant(t *testing.T) {
	body := `{"slots":[{"id":1,"n_ctx":4096,"is_processing":true,"n_prompt_tokens":50,"n_prompt_tokens_processed":20,"next_token":[{"n_decoded":3,"new_thing":1}],"brand_new":{"x":1}}]}`
	slots, ok := parseChildSlots([]byte(body))
	if !ok || len(slots) != 1 {
		t.Fatalf("parse failed: %v %v", ok, slots)
	}
	s := slots[0]
	if s.ID != 1 || s.NCtx != 4096 || !s.IsProcessing || s.NPromptTokens != 50 || s.NPromptTokensCache != 0 || s.NPromptTokensProcessed != 20 || s.NDecoded != 3 {
		t.Fatalf("parsed %+v", s)
	}
	// Bare-array form from an older child.
	if slots, ok := parseChildSlots([]byte(`[{"id":0,"n_prompt_tokens_cache":7,"next_token":{"n_decoded":2}}]`)); !ok || slots[0].NPromptTokensCache != 7 || slots[0].NDecoded != 2 {
		t.Fatalf("bare array / object next_token: %v %+v", ok, slots)
	}
}

// The poller is a status read: it GETs the child's /slots directly and never
// dispatches through the router (which is what moves TTL/idle clocks and
// loads models), and it polls nothing when no model is ready.
func TestSessions_PollerNeverTouchesRouter(t *testing.T) {
	var childHits atomic.Int32
	child := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/slots" {
			t.Errorf("child got %s %s, want GET /slots", r.Method, r.URL.Path)
		}
		childHits.Add(1)
		w.Write([]byte(`{"slots":[{"id":0,"n_ctx":8192}]}`))
	}))
	defer child.Close()

	local := newStubRouter([]string{"m1"}, "")
	var routerCalls atomic.Int32
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) { routerCalls.Add(1) }
	local.running = map[string]process.ProcessState{}
	cfg := config.Config{Models: map[string]config.ModelConfig{"m1": {Proxy: child.URL, ConcurrencyLimit: 1}}}
	s := newTestServerWithConfig(cfg, local, newStubRouter(nil, ""))
	hub := newSessionsHub(s)
	s.sessions = hub

	hub.tick(time.Now(), true)
	if childHits.Load() != 0 {
		t.Fatalf("poller hit the child %d times with no model ready", childHits.Load())
	}

	local.running = map[string]process.ProcessState{"m1": process.StateReady}
	hub.tick(time.Now(), true)
	if childHits.Load() != 1 {
		t.Fatalf("child hits=%d want 1 for one ready model", childHits.Load())
	}
	if routerCalls.Load() != 0 {
		t.Fatalf("poller dispatched %d requests through the router (would move TTL/idle clocks)", routerCalls.Load())
	}
	if n := len(s.inflight.Current().Requests); n != 0 {
		t.Fatalf("poller created %d in-flight entries", n)
	}
	if hub.snapshot().Resident == nil || hub.snapshot().Resident.Window != 8192 {
		t.Fatalf("resident not folded from the poll: %+v", hub.snapshot().Resident)
	}

	// GET /api/sessions serves the cache: no further child hit.
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"schema":"llama-swap.sessions/v1"`) {
		t.Fatalf("GET /api/sessions: %d %s", w.Code, w.Body.String())
	}
	if childHits.Load() != 1 || routerCalls.Load() != 0 {
		t.Fatalf("GET /api/sessions proxied: child=%d router=%d", childHits.Load(), routerCalls.Load())
	}
}

// Parent session id carried by X-Claude-Code-Parent-Session-Id surfaces on the
// row: parentSessionId is the full id, parentSessionShort the first 8 chars.
// Absent when the header was not set (no change to existing rows).
func TestSessions_ParentSessionId(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	running := map[string]process.ProcessState{sessModel: process.StateReady}

	t.Run("dispatched request carries parent", func(t *testing.T) {
		b := newSessionsBuilder(sessionsTestConfig())
		req := swaputil.InflightRequestEntry{
			ID: "r-dispatch", Model: sessModel, Timestamp: now,
			Metadata: map[string]string{
				"session_id":        "2679c154-0000-0000-0000-000000000001",
				"parent_session_id": "3f9cb5bb-aaaa-bbbb-cccc-dddddddddddd",
				"park_reason":       "kv",
				"kv_parked":         "1",
				"tier":              "default",
			},
		}
		got := b.build(sessionsInput{Now: now, Running: running, Requests: []swaputil.InflightRequestEntry{req}, MemoryBrake: enabledBrake()})
		assertSessionsInvariants(t, got)
		require.Len(t, got.Sessions, 1)
		s := got.Sessions[0]
		if s.ParentSessionId != "3f9cb5bb-aaaa-bbbb-cccc-dddddddddddd" {
			t.Errorf("ParentSessionId = %q, want %q", s.ParentSessionId, "3f9cb5bb-aaaa-bbbb-cccc-dddddddddddd")
		}
		if s.ParentSessionShort != "3f9cb5bb" {
			t.Errorf("ParentSessionShort = %q, want %q", s.ParentSessionShort, "3f9cb5bb")
		}
	})

	t.Run("no parent header leaves fields empty", func(t *testing.T) {
		b := newSessionsBuilder(sessionsTestConfig())
		req := swaputil.InflightRequestEntry{
			ID: "r-direct", Model: sessModel, Timestamp: now,
			Metadata: map[string]string{
				"session_id": "abcdef01-0000-0000-0000-000000000002",
				"tier":       "default",
			},
		}
		got := b.build(sessionsInput{Now: now, Running: running, Requests: []swaputil.InflightRequestEntry{req}, MemoryBrake: enabledBrake()})
		assertSessionsInvariants(t, got)
		require.Len(t, got.Sessions, 1)
		s := got.Sessions[0]
		if s.ParentSessionId != "" {
			t.Errorf("ParentSessionId = %q, want empty", s.ParentSessionId)
		}
		if s.ParentSessionShort != "" {
			t.Errorf("ParentSessionShort = %q, want empty", s.ParentSessionShort)
		}
	})

	t.Run("short parent id (8 hex)", func(t *testing.T) {
		b := newSessionsBuilder(sessionsTestConfig())
		req := swaputil.InflightRequestEntry{
			ID: "r-short-parent", Model: sessModel, Timestamp: now,
			Metadata: map[string]string{
				"session_id":        "cccccccc-0000-0000-0000-000000000003",
				"parent_session_id": "abcd1234",
				"tier":              "default",
			},
		}
		got := b.build(sessionsInput{Now: now, Running: running, Requests: []swaputil.InflightRequestEntry{req}, MemoryBrake: enabledBrake()})
		assertSessionsInvariants(t, got)
		require.Len(t, got.Sessions, 1)
		s := got.Sessions[0]
		if s.ParentSessionId != "abcd1234" {
			t.Errorf("ParentSessionId = %q, want %q", s.ParentSessionId, "abcd1234")
		}
		if s.ParentSessionShort != "abcd1234" {
			t.Errorf("ParentSessionShort = %q, want %q", s.ParentSessionShort, "abcd1234")
		}
	})

	t.Run("short parent from full uuid uses first 8", func(t *testing.T) {
		b := newSessionsBuilder(sessionsTestConfig())
		req := swaputil.InflightRequestEntry{
			ID: "r-full-parent", Model: sessModel, Timestamp: now,
			Metadata: map[string]string{
				"session_id":        "dddddddd-0000-0000-0000-000000000004",
				"parent_session_id": "ef012345-aaaa-bbbb-cccc-dddddddddddd",
				"tier":              "default",
			},
		}
		got := b.build(sessionsInput{Now: now, Running: running, Requests: []swaputil.InflightRequestEntry{req}, MemoryBrake: enabledBrake()})
		assertSessionsInvariants(t, got)
		require.Len(t, got.Sessions, 1)
		s := got.Sessions[0]
		if s.ParentSessionId != "ef012345-aaaa-bbbb-cccc-dddddddddddd" {
			t.Errorf("ParentSessionId = %q, want %q", s.ParentSessionId, "ef012345-aaaa-bbbb-cccc-dddddddddddd")
		}
		if s.ParentSessionShort != "ef012345" {
			t.Errorf("ParentSessionShort = %q, want %q", s.ParentSessionShort, "ef012345")
		}
	})
}

// The "sessions" push fires on change and at most once per second.
func TestSessions_EventThrottle(t *testing.T) {
	var emitted []sessionsBody
	th := &sessionsThrottle{minInterval: time.Second}
	t0 := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	a := sessionsBody{Schema: sessionsSchema, GeneratedAt: "x", Sessions: []sessionEntry{}}
	bdy := sessionsBody{Schema: sessionsSchema, GeneratedAt: "y", Sessions: []sessionEntry{{SessionID: "s"}}}
	emit := func(b sessionsBody) { emitted = append(emitted, b) }

	th.offer(t0, a, emit)                            // first: emits
	th.offer(t0.Add(1500*time.Millisecond), a, emit) // unchanged (generatedAt ignored): no emit
	th.offer(t0.Add(1600*time.Millisecond), bdy, emit)
	th.offer(t0.Add(1700*time.Millisecond), a, emit) // changed but within 1 s of the last push: held
	th.offer(t0.Add(2700*time.Millisecond), a, emit) // 1.1 s later: the held change goes out
	if len(emitted) != 3 {
		t.Fatalf("emitted %d events, want 3 (first, change, throttled change)", len(emitted))
	}
	if len(emitted[2].Sessions) != 0 {
		t.Fatalf("third push should carry the latest body")
	}
}

// /api/events carries a "sessions" event with the snapshot body.
func TestSessions_SSEEvent(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	s.sessions = newSessionsHub(s)
	s.sessions.tick(time.Now(), false)
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
	if body := w.Body.String(); !strings.Contains(body, `"type":"sessions"`) || !strings.Contains(body, `llama-swap.sessions/v1`) {
		t.Fatalf("no sessions event in SSE stream: %q", body)
	}
}

// Active-first order: a session the backend is serving (DECODE/PREFILL) is
// listed above a PARKED one even when the parked request is older, because
// the menu's Queue block reads top-down as "what is running now". Within a
// phase the old order (priority desc, then elapsed desc) still holds.
func TestSessions_ActiveFirstOrder(t *testing.T) {
	b := newSessionsBuilder(sessionsTestConfig())
	now := fixtureClock(t, "2026-10-16T12:00:00.000+02:00")
	running := map[string]process.ProcessState{sessModel: process.StateReady}
	parked := swaputil.InflightRequestEntry{
		ID: "r-park", Model: sessModel, Timestamp: now.Add(-2787 * time.Second),
		Metadata: map[string]string{"session_id": "97c5c88f-0000-0000-0000-000000000001", "park_reason": "busy", "tier": "default"},
	}
	decode := swaputil.InflightRequestEntry{
		ID: "r-dec", Model: sessModel, Timestamp: now.Add(-55 * time.Second),
		Metadata: map[string]string{"session_id": "9f562e73-0000-0000-0000-000000000002", "slot_granted": "1", "slot_id": "0", "tier": "default"},
	}
	slots := map[string][]childSlot{sessModel: {
		{ID: 0, NCtx: 262144, IsProcessing: true, NPromptTokens: 92254, NPromptTokensCache: 92254, NPromptTokensProcessed: 0, NDecoded: 40},
	}}
	got := b.build(sessionsInput{Now: now, Running: running, Requests: []swaputil.InflightRequestEntry{parked, decode}, Slots: slots, MemoryBrake: enabledBrake()})
	if len(got.Sessions) < 2 {
		t.Fatalf("want 2 session rows, got %d", len(got.Sessions))
	}
	if got.Sessions[0].Phase != phaseDecode || got.Sessions[1].Phase != phaseParked {
		t.Fatalf("want DECODE then PARKED, got %s then %s", got.Sessions[0].Phase, got.Sessions[1].Phase)
	}
}

// One-lane backend (Splash shim): slot affinity is off at nSlots<=1, so the
// request carries no slot_id. With exactly one slot and exactly one admitted
// session the binding is deterministic: that session owns slot 0.
func oneLaneReq(id, sess string, granted bool, t0 time.Time) swaputil.InflightRequestEntry {
	md := map[string]string{"session_id": sess}
	if granted {
		md["slot_granted"] = "1"
	}
	return swaputil.InflightRequestEntry{ID: id, Model: sessModel, Timestamp: t0, Metadata: md}
}

func oneLaneSlots(processed, decoded int) map[string][]childSlot {
	return map[string][]childSlot{sessModel: {{ID: 0, NCtx: 262144, IsProcessing: true, NPromptTokens: 9000, NPromptTokensProcessed: processed, NDecoded: decoded}}}
}

func TestSessions_OneLaneSoleAdmittedBindsSlot0(t *testing.T) {
	b := newSessionsBuilder(sessionsTestConfig())
	t0 := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	running := map[string]process.ProcessState{sessModel: process.StateReady}
	reqs := []swaputil.InflightRequestEntry{oneLaneReq("1", "sess-aaaaaaaa", true, t0)}
	got := b.build(sessionsInput{Now: t0, Running: running, Requests: reqs, Slots: oneLaneSlots(2000, 0)})
	row := got.Sessions[0]
	if row.Slot == nil || *row.Slot != 0 {
		t.Fatalf("slot = %v, want 0", row.Slot)
	}
	if row.Context.Window != 262144 || row.Context.Processed != 2000 || row.Context.Used != 2000 {
		t.Fatalf("context = %+v", row.Context)
	}
	if row.Phase != "PREFILL" || row.Progress == nil {
		t.Fatalf("phase=%s progress=%v", row.Phase, row.Progress)
	}
	// First poll: no rate yet (null, not 0).
	if row.Rate.TokensPerSecond != nil {
		t.Fatalf("first-poll rate = %v, want null", *row.Rate.TokensPerSecond)
	}
	// Second poll 2 s later: 1000 more prompt tokens -> 500 tok/s prefill.
	got = b.build(sessionsInput{Now: t0.Add(2 * time.Second), Running: running, Requests: reqs, Slots: oneLaneSlots(3000, 0)})
	row = got.Sessions[0]
	if row.Rate.Kind == nil || *row.Rate.Kind != "prefill" || row.Rate.TokensPerSecond == nil || *row.Rate.TokensPerSecond != 500 {
		t.Fatalf("prefill rate = %+v", row.Rate)
	}
	// Decoding starts: phase follows the slot, decode rate from decoded delta.
	got = b.build(sessionsInput{Now: t0.Add(3 * time.Second), Running: running, Requests: reqs, Slots: oneLaneSlots(9000, 10)})
	got = b.build(sessionsInput{Now: t0.Add(5 * time.Second), Running: running, Requests: reqs, Slots: oneLaneSlots(9000, 50)})
	row = got.Sessions[0]
	if row.Phase != "DECODE" || row.Rate.Kind == nil || *row.Rate.Kind != "decode" || row.Rate.TokensPerSecond == nil || *row.Rate.TokensPerSecond != 20 {
		t.Fatalf("decode row = phase %s rate %+v", row.Phase, row.Rate)
	}
	if row.Context.Decoded != 50 || row.Context.Used != 9050 {
		t.Fatalf("decode context = %+v", row.Context)
	}
	assertSessionsInvariants(t, got)
}

func TestSessions_OneLaneTwoAdmittedUnchanged(t *testing.T) {
	b := newSessionsBuilder(sessionsTestConfig())
	t0 := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	running := map[string]process.ProcessState{sessModel: process.StateReady}
	reqs := []swaputil.InflightRequestEntry{
		oneLaneReq("1", "sess-aaaaaaaa", true, t0),
		oneLaneReq("2", "sess-bbbbbbbb", true, t0),
	}
	got := b.build(sessionsInput{Now: t0, Running: running, Requests: reqs, Slots: oneLaneSlots(2000, 0)})
	for _, row := range got.Sessions {
		if row.Slot != nil || row.Context.Processed != 0 {
			t.Fatalf("%s bound with two admitted: slot=%v ctx=%+v", row.SessionShort, row.Slot, row.Context)
		}
	}
}

// Decode retention (PF-C, 2026-10-25): a session chaining short tool turns
// reads DECODE -> IDLE/PREFILL -> DECODE at the 1 Hz poll. Within
// sessionsDecodeRetention of the last DECODE poll a young PREFILL or IDLE row
// keeps DISPLAYING DECODE; the raw phase, counters and PhaseSinceMs are
// untouched (machine readers read `phase` as truth), and a phase that lasts
// longer than the retention shows as itself from that moment on.
func TestSessions_DecodeRetentionHoldsAcrossShortPrefill(t *testing.T) {
	b := newSessionsBuilder(sessionsTestConfig())
	t0 := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	running := map[string]process.ProcessState{sessModel: process.StateReady}
	mk := func(id string, sess string, at time.Time) swaputil.InflightRequestEntry {
		return swaputil.InflightRequestEntry{ID: id, Model: sessModel, Timestamp: at,
			Metadata: map[string]string{"session_id": sess, "slot_granted": "1", "slot_id": "0"}}
	}
	sl := func(processed, decoded int) map[string][]childSlot {
		return map[string][]childSlot{sessModel: {{ID: 0, NCtx: 262144, IsProcessing: true, NPromptTokens: 90000, NPromptTokensProcessed: processed, NDecoded: decoded}}}
	}
	check := func(got sessionsBody, wantPhase, wantDisplay string) {
		if len(got.Sessions) != 1 {
			t.Fatalf("want one row, got %+v", got.Sessions)
		}
		row := got.Sessions[0]
		if row.Phase != wantPhase {
			t.Fatalf("raw phase = %s, want %s (display-only retention must never touch it)", row.Phase, wantPhase)
		}
		disp := ""
		if row.Display != nil {
			disp = row.Display.Phase
		}
		if disp != wantDisplay {
			t.Fatalf("display.phase = %q, want %q (raw %s)", disp, wantDisplay, row.Phase)
		}
	}

	// Poll 1: s1 decodes its first turn.
	got := b.build(sessionsInput{Now: t0, Running: running, Requests: []swaputil.InflightRequestEntry{mk("r1", "s1", t0.Add(-5*time.Second))}, Slots: sl(100, 40)})
	check(got, "DECODE", "")
	if got.Sessions[0].Context.Decoded != 40 || got.Sessions[0].Rate.Kind == nil || *got.Sessions[0].Rate.Kind != "decode" {
		t.Fatalf("decode row = ctx %+v rate %+v", got.Sessions[0].Context, got.Sessions[0].Rate)
	}

	// Poll 2 (+2 s): the request finished; raw IDLE, display still DECODE.
	got = b.build(sessionsInput{Now: t0.Add(2 * time.Second), Running: running})
	check(got, "IDLE", "DECODE")
	// PhaseSinceMs is stamped from the builder's own memory (idleMark.since),
	// never by the retention - pin it at its pre-retention value.
	if got.Sessions[0].PhaseSinceMs != 0 {
		t.Fatalf("IDLE phaseSinceMs=%d, want 0 (retention must not touch it)", got.Sessions[0].PhaseSinceMs)
	}

	// Poll 3 (+4 s): the next turn of s1 is in PREFILL (NDecoded 0); s2, which
	// was never in DECODE, prefills beside it. Display holds for s1 only.
	reqs := []swaputil.InflightRequestEntry{mk("r2", "s1", t0.Add(4*time.Second)), mk("r3", "s2", t0.Add(4*time.Second))}
	got = b.build(sessionsInput{Now: t0.Add(4 * time.Second), Running: running, Requests: reqs, Slots: sl(10, 0)})
	bySess := map[string]sessionEntry{}
	for _, r := range got.Sessions {
		bySess[r.SessionID] = r
	}
	if len(bySess) != 2 {
		t.Fatalf("want two rows, got %+v", got.Sessions)
	}
	r1, ok1 := bySess["s1"]
	r2, ok2 := bySess["s2"]
	if !ok1 || !ok2 {
		t.Fatalf("rows missing: %+v", got.Sessions)
	}
	if r1.Phase != "PREFILL" || (r1.Display == nil || r1.Display.Phase != "DECODE") {
		t.Fatalf("s1 at +4s: raw %s display %+v, want PREFILL / DECODE", r1.Phase, r1.Display)
	}
	if r1.Context.Decoded != 0 {
		t.Fatalf("s1 prefill context decoded=%d, want 0", r1.Context.Decoded)
	}
	if r2.Phase != "PREFILL" || r2.Display != nil {
		t.Fatalf("s2 at +4s: raw %s display %+v, want PREFILL / absent (never decoded)", r2.Phase, r2.Display)
	}

	// Poll 4 (+12 s after the last DECODE): s1's request is STILL in PREFILL.
	// The window is now measured from the last DECODE and configured at 30 s,
	// so 12 s is still INSIDE it and the display holds - the old behaviour
	// (a prefill past 8 s shows itself) is superseded by
	// TestSessions_DecodeRetentionReleasedPastWindow, which takes the release
	// past the configured window. (s2 finishes here and keeps its IDLE row
	// for the 60 s known-session retention; it was never in DECODE, so it has
	// nothing to retain.)
	s1req := []swaputil.InflightRequestEntry{mk("r2", "s1", t0.Add(4*time.Second))}
	got = b.build(sessionsInput{Now: t0.Add(12 * time.Second), Running: running, Requests: s1req, Slots: sl(10, 0)})
	bySess = map[string]sessionEntry{}
	for _, r := range got.Sessions {
		bySess[r.SessionID] = r
	}
	r1, ok1 = bySess["s1"]
	if !ok1 {
		t.Fatalf("s1 row missing, got %+v", got.Sessions)
	}
	if r1.Phase != "PREFILL" || r1.Display == nil || r1.Display.Phase != "DECODE" {
		t.Fatalf("s1 at +12s: raw %s display %+v, want PREFILL / DECODE (inside the 30 s window)", r1.Phase, r1.Display)
	}

	// A session IDLE 34 s after its last DECODE is past the window: IDLE in
	// both fields. (s2's finished request leaves it an IDLE row too - the
	// known-session retention is 60 s; only s1's display is asserted here.)
	got = b.build(sessionsInput{Now: t0.Add(34 * time.Second), Running: running})
	bySess = map[string]sessionEntry{}
	for _, r := range got.Sessions {
		bySess[r.SessionID] = r
	}
	r1, ok1 = bySess["s1"]
	if !ok1 {
		t.Fatalf("s1 row missing, got %+v", got.Sessions)
	}
	if r1.Phase != "IDLE" || r1.Display != nil {
		t.Fatalf("s1 idle 34s after decode: raw %s display %+v, want IDLE / absent (past the window)", r1.Phase, r1.Display)
	}
	assertSessionsInvariants(t, got)
}

// The retention's own clock (lastDecode) must not outlive the row it belongs
// to. A session that leaves the snapshot entirely - a crash, a model swap, a
// client that dies between turns - stops being polled, so its entry would
// otherwise sit in the map forever: one permanent entry per dead session, and
// nothing anywhere else bounds that map. Once the session is absent from the
// body for a full retention window its memory is dropped with the rest.
func TestSessions_DecodeRetentionMemoryDroppedWithDeadSession(t *testing.T) {
	b := newSessionsBuilder(sessionsTestConfig())
	t0 := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	running := map[string]process.ProcessState{sessModel: process.StateReady}
	mk := func(id string, sess string, at time.Time) swaputil.InflightRequestEntry {
		return swaputil.InflightRequestEntry{ID: id, Model: sessModel, Timestamp: at,
			Metadata: map[string]string{"session_id": sess, "slot_granted": "1", "slot_id": "0"}}
	}
	sl := func(processed, decoded int) map[string][]childSlot {
		return map[string][]childSlot{sessModel: {{ID: 0, NCtx: 262144, IsProcessing: true, NPromptTokens: 90000, NPromptTokensProcessed: processed, NDecoded: decoded}}}
	}

	got := b.build(sessionsInput{Now: t0, Running: running, Requests: []swaputil.InflightRequestEntry{mk("r1", "s1", t0.Add(-5*time.Second))}, Slots: sl(100, 40)})
	if len(b.lastDecode) != 1 {
		t.Fatalf("lastDecode = %v after a DECODE poll, want one entry", b.lastDecode)
	}

	// s1 vanishes from the backend: no requests, no slots, and its IDLE row
	// ages out of the body after sessionsIdleRetention.
	for d := time.Second; d <= sessionsIdleRetention+sessionsDecodeRetention; d += time.Second {
		got = b.build(sessionsInput{Now: t0.Add(d), Running: running})
	}
	for _, row := range got.Sessions {
		if row.SessionID == "s1" {
			t.Fatalf("s1 still in the body after both retentions: %+v", got.Sessions)
		}
	}
	if _, ok := b.lastDecode["s1"]; ok {
		t.Fatalf("lastDecode kept an entry for a session gone from the body: %v", b.lastDecode)
	}
	assertSessionsInvariants(t, got)
}

// The window bounds the retention on its own clock: a chained session does
// NOT carry display DECODE forever. A PREFILL that starts inside the window
// shows itself once the window since the last DECODE has elapsed, however
// fresh the phase itself is (the pre-2026-10-27 rule also required the phase
// to be younger than the window; the window now counts from the last DECODE,
// which is the bound that actually matches a two-session lane gap).
func TestSessions_DecodeRetentionEndsAtWindowDuringLongPrefill(t *testing.T) {
	b := newSessionsBuilder(sessionsTestConfig())
	t0 := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	running := map[string]process.ProcessState{sessModel: process.StateReady}
	mk := func(id string, sess string, at time.Time) swaputil.InflightRequestEntry {
		return swaputil.InflightRequestEntry{ID: id, Model: sessModel, Timestamp: at,
			Metadata: map[string]string{"session_id": sess, "slot_granted": "1", "slot_id": "0"}}
	}
	decodeSlots := map[string][]childSlot{sessModel: {{ID: 0, NCtx: 262144, IsProcessing: true, NPromptTokens: 90000, NPromptTokensProcessed: 100, NDecoded: 40}}}
	prefillSlots := map[string][]childSlot{sessModel: {{ID: 0, NCtx: 262144, IsProcessing: true, NPromptTokens: 90000, NPromptTokensProcessed: 10, NDecoded: 0}}}

	// Poll 1 (+0): s1 decodes turn 1.
	got := b.build(sessionsInput{Now: t0, Running: running, Requests: []swaputil.InflightRequestEntry{mk("r1", "s1", t0.Add(-5*time.Second))}, Slots: decodeSlots})
	if len(got.Sessions) != 1 || got.Sessions[0].Phase != "DECODE" || got.Sessions[0].Display != nil {
		t.Fatalf("poll 1: %+v, want one DECODE row without display", got.Sessions)
	}

	// Poll 2 (+3 s): turn 2 arrives in PREFILL, 3 s after the last DECODE ->
	// display holds.
	got = b.build(sessionsInput{Now: t0.Add(3 * time.Second), Running: running, Requests: []swaputil.InflightRequestEntry{mk("r2", "s1", t0.Add(3*time.Second))}, Slots: prefillSlots})
	if len(got.Sessions) != 1 || got.Sessions[0].Phase != "PREFILL" || got.Sessions[0].Display == nil || got.Sessions[0].Display.Phase != "DECODE" {
		t.Fatalf("poll 2: %+v, want PREFILL / display DECODE", got.Sessions)
	}

	// Poll 3 (+31 s): the SAME request is still prefilling - 28 s of phase,
	// and 31 s since the last DECODE, i.e. past the 30 s window. The raw
	// phase stays PREFILL and the display is gone: a session genuinely stuck
	// in prefill surfaces again.
	got = b.build(sessionsInput{Now: t0.Add(31 * time.Second), Running: running, Requests: []swaputil.InflightRequestEntry{mk("r2", "s1", t0.Add(3*time.Second))}, Slots: prefillSlots})
	if len(got.Sessions) != 1 || got.Sessions[0].Phase != "PREFILL" || got.Sessions[0].Display != nil {
		t.Fatalf("poll 3: %+v, want PREFILL / display absent (window 30 s from the last DECODE)", got.Sessions)
	}
	assertSessionsInvariants(t, got)
}

// --- decode retention round 2 (2026-10-27) ---
//
// With two sessions sharing one lane the gap between one session's turns is
// IDLE (the tool call) plus PARKED (the next turn queued behind the other
// session) plus PREFILL, and that whole gap routinely outlasts 8 s. The
// retention therefore measures from the LAST RAW DECODE (not from the start
// of the current phase), covers PARKED as well as IDLE and PREFILL, and the
// window is configuration: sessions.decodeRetentionSeconds, default 30 s,
// with the 8 s constant as the fallback when the config leaves it unset.
// Raw phase, counters, PhaseSinceMs and the queue tallies stay untouched
// throughout; a session that never decoded, a LOADING row and any penalized
// row are never retained.

// retentionPolls drives one builder through a DECODE poll (an admitted
// request decoding) and a second poll `later` later carrying the NEXT request
// (parked unless the caller's metadata says otherwise, so its raw phase is
// PARKED, PREFILL or IDLE per `slots`/extraMeta). It returns that second
// poll's single row for s1, `later` after the last DECODE.
func retentionPolls(t *testing.T, b *sessionsBuilder, t0 time.Time, later time.Duration, extraMeta map[string]string, slots map[string][]childSlot) sessionEntry {
	t.Helper()
	running := map[string]process.ProcessState{sessModel: process.StateReady}
	decodeSlots := map[string][]childSlot{sessModel: {{ID: 0, NCtx: 262144, IsProcessing: true, NPromptTokens: 90000, NPromptTokensProcessed: 100, NDecoded: 40}}}
	if got := b.build(sessionsInput{Now: t0, Running: running, Requests: []swaputil.InflightRequestEntry{retentionReq("r1", t0.Add(-5*time.Second), nil)}, Slots: decodeSlots}); len(got.Sessions) != 1 || got.Sessions[0].Phase != "DECODE" {
		t.Fatalf("setup poll: %+v, want one DECODE row", got.Sessions)
	}
	got := b.build(sessionsInput{Now: t0.Add(later), Running: running, Requests: []swaputil.InflightRequestEntry{retentionReq("r2", t0.Add(later), extraMeta)}, Slots: slots})
	if len(got.Sessions) != 1 {
		t.Fatalf("second poll: %+v, want one row", got.Sessions)
	}
	return got.Sessions[0]
}

// retentionReq is one in-flight request for s1: admitted (slot_granted) by
// default, parked when the caller adds a park_reason (which the scheduler
// only ever stamps on a request it has NOT granted a slot to).
func retentionReq(id string, now time.Time, extraMeta map[string]string) swaputil.InflightRequestEntry {
	md := map[string]string{"session_id": "s1"}
	if _, parked := extraMeta["park_reason"]; !parked {
		md["slot_granted"] = "1"
		md["slot_id"] = "0"
	}
	for k, v := range extraMeta {
		md[k] = v
	}
	return swaputil.InflightRequestEntry{ID: id, Model: sessModel, Timestamp: now, Metadata: md}
}

// PARKED inside the window keeps displaying DECODE: the session's turn is
// queued behind another session on the same lane, which is exactly the state
// the menu used to show as a bare PARKED row.
func TestSessions_DecodeRetentionHoldsAcrossParked(t *testing.T) {
	b := newSessionsBuilder(sessionsTestConfig())
	t0 := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	parkedSlots := map[string][]childSlot{sessModel: {{ID: 0, NCtx: 262144, IsProcessing: true, NPromptTokens: 90000, NPromptTokensProcessed: 100, NDecoded: 40}}}
	row := retentionPolls(t, b, t0, 4*time.Second, map[string]string{"park_reason": "lane", "waiting_for_session": "s2"}, parkedSlots)
	if row.Phase != "PARKED" {
		t.Fatalf("raw phase = %s, want PARKED (display-only retention must never touch it)", row.Phase)
	}
	if row.Display == nil || row.Display.Phase != "DECODE" {
		t.Fatalf("display = %+v, want DECODE for a PARKED row 4 s after the last DECODE", row.Display)
	}
	if row.WaitingFor == nil || row.WaitingFor.Session != "s2" {
		t.Fatalf("waitingFor = %+v, want the park's own waitingFor kept intact", row.WaitingFor)
	}
	assertSessionsInvariants(t, b.build(sessionsInput{Now: t0.Add(4 * time.Second), Running: map[string]process.ProcessState{sessModel: process.StateReady}}))
}

// HOT inside the window keeps displaying DECODE: with slot affinity a session
// between turns has no request in flight but still owns its lane, which reads
// HOT rather than IDLE.
func TestSessions_DecodeRetentionHoldsAcrossHot(t *testing.T) {
	b := newSessionsBuilder(sessionsTestConfig())
	t0 := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	running := map[string]process.ProcessState{sessModel: process.StateReady}
	decodeSlots := map[string][]childSlot{sessModel: {{ID: 0, NCtx: 262144, IsProcessing: true, NPromptTokens: 90000, NPromptTokensProcessed: 100, NDecoded: 40}}}
	b.build(sessionsInput{Now: t0, Running: running, Requests: []swaputil.InflightRequestEntry{retentionReq("r1", t0.Add(-5*time.Second), nil)}, Slots: decodeSlots})
	hot := map[string][]swaputil.HotSlot{sessModel: {{Slot: 0, SessionID: "s1", IdleSeconds: 4}}}
	got := b.build(sessionsInput{Now: t0.Add(4 * time.Second), Running: running, Hot: hot})
	if len(got.Sessions) != 1 {
		t.Fatalf("sessions = %+v, want one HOT row", got.Sessions)
	}
	row := got.Sessions[0]
	if row.Phase != "HOT" {
		t.Fatalf("raw phase = %s, want HOT (display-only retention must never touch it)", row.Phase)
	}
	if row.Display == nil || row.Display.Phase != "DECODE" {
		t.Fatalf("display = %+v, want DECODE for a HOT row 4 s after the last DECODE", row.Display)
	}
	late := b.build(sessionsInput{Now: t0.Add(35 * time.Second), Running: running, Hot: map[string][]swaputil.HotSlot{sessModel: {{Slot: 0, SessionID: "s1", IdleSeconds: 35}}}})
	if len(late.Sessions) != 1 || late.Sessions[0].Display != nil {
		t.Fatalf("past the window = %+v, want the raw HOT row with no display override", late.Sessions)
	}
}

// A PREFILL older than the old 8 s constant but inside the window keeps
// displaying DECODE: the window is measured from the last raw DECODE, not
// from the start of the current phase.
func TestSessions_DecodeRetentionPrefillOlderThan8sInsideWindow(t *testing.T) {
	b := newSessionsBuilder(sessionsTestConfig())
	t0 := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	prefillSlots := map[string][]childSlot{sessModel: {{ID: 0, NCtx: 262144, IsProcessing: true, NPromptTokens: 90000, NPromptTokensProcessed: 10, NDecoded: 0}}}
	row := retentionPolls(t, b, t0, 12*time.Second, nil, prefillSlots)
	if row.Phase != "PREFILL" {
		t.Fatalf("raw phase = %s, want PREFILL", row.Phase)
	}
	if row.RequestID == nil || *row.RequestID != "r2" {
		t.Fatalf("requestID = %v, want r2 (the phase under test is the NEW request's)", row.RequestID)
	}
	if row.Display == nil || row.Display.Phase != "DECODE" {
		t.Fatalf("display = %+v, want DECODE 12 s after the last DECODE (window 30 s)", row.Display)
	}
}

// Past the window the display is released: the row shows its raw phase again.
func TestSessions_DecodeRetentionReleasedPastWindow(t *testing.T) {
	b := newSessionsBuilder(sessionsTestConfig())
	t0 := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	parkedSlots := map[string][]childSlot{sessModel: {{ID: 0, NCtx: 262144, IsProcessing: true, NPromptTokens: 90000, NPromptTokensProcessed: 100, NDecoded: 40}}}
	row := retentionPolls(t, b, t0, 35*time.Second, map[string]string{"park_reason": "lane"}, parkedSlots)
	if row.Phase != "PARKED" {
		t.Fatalf("raw phase = %s, want PARKED", row.Phase)
	}
	if row.Display != nil {
		t.Fatalf("display = %+v, want none 35 s after the last DECODE (window 30 s)", row.Display)
	}
}

// A penalized row is never retained - neither the scheduler-held row the
// builder already rewrote to PENALIZED, nor a struck-but-admitted row that
// still reads PARKED. A penalty is something an operator has to see.
func TestSessions_DecodeRetentionNeverPenalized(t *testing.T) {
	t0 := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	clk := t0
	lt := penaltyTracker(&clk, 0, 900)
	feedSession(lt, "s1", 6) // strike 2 -> a 900 s hold exists

	for _, tc := range []struct {
		name  string
		meta  map[string]string
		phase string
	}{
		{"held by the scheduler", map[string]string{"park_reason": scheduler.ParkPenalized}, "PENALIZED"},
		{"struck but admitted", map[string]string{"park_reason": "lane"}, "PARKED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newSessionsBuilder(sessionsTestConfig())
			decodeSlots := map[string][]childSlot{sessModel: {{ID: 0, NCtx: 262144, IsProcessing: true, NPromptTokens: 90000, NPromptTokensProcessed: 100, NDecoded: 40}}}
			running := map[string]process.ProcessState{sessModel: process.StateReady}
			if got := b.build(sessionsInput{Now: t0, Running: running, Loops: lt, Requests: []swaputil.InflightRequestEntry{retentionReq("r1", t0.Add(-5*time.Second), nil)}, Slots: decodeSlots}); len(got.Sessions) != 1 || got.Sessions[0].Phase != "DECODE" {
				t.Fatalf("setup poll: %+v, want one DECODE row", got.Sessions)
			}
			at := t0.Add(4 * time.Second)
			got := b.build(sessionsInput{Now: at, Running: running, Loops: lt, Requests: []swaputil.InflightRequestEntry{retentionReq("r2", at, tc.meta)}, Slots: decodeSlots})
			row := rowFor(t, got, "s1")
			if row.Phase != tc.phase {
				t.Fatalf("raw phase = %s, want %s", row.Phase, tc.phase)
			}
			if row.Display != nil {
				t.Fatalf("display = %+v, want none for a penalized row (%s)", row.Display, tc.name)
			}
			assertSessionsInvariants(t, got)
		})
	}
}

// The window is configuration: sessions.decodeRetentionSeconds overrides the
// default, and 0 turns the retention off entirely.
func TestSessions_DecodeRetentionConfiguredWindow(t *testing.T) {
	cfg := sessionsTestConfig()
	cfg.Sessions.DecodeRetentionSeconds = 4

	b := newSessionsBuilder(cfg)
	t0 := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	parkedSlots := map[string][]childSlot{sessModel: {{ID: 0, NCtx: 262144, IsProcessing: true, NPromptTokens: 90000, NPromptTokensProcessed: 100, NDecoded: 40}}}
	row := retentionPolls(t, b, t0, 3*time.Second, map[string]string{"park_reason": "lane"}, parkedSlots)
	if row.Display == nil || row.Display.Phase != "DECODE" {
		t.Fatalf("display = %+v, want DECODE inside the configured 4 s window", row.Display)
	}

	b = newSessionsBuilder(cfg)
	row = retentionPolls(t, b, t0, 6*time.Second, map[string]string{"park_reason": "lane"}, parkedSlots)
	if row.Display != nil {
		t.Fatalf("display = %+v, want none past the configured 4 s window", row.Display)
	}

	off := sessionsTestConfig()
	off.Sessions.DecodeRetentionSeconds = 0
	b = newSessionsBuilder(off)
	row = retentionPolls(t, b, t0, 2*time.Second, map[string]string{"park_reason": "lane"}, parkedSlots)
	if row.Display != nil {
		t.Fatalf("display = %+v, want none with the retention configured off", row.Display)
	}
}

// --- decode retention readout (2026-10-30 incident) ---
//
// The retention is a READOUT, not just a phase sticker: a live 1 Hz observer
// saw display.phase hold DECODE while the raw HOT context.used fell from
// 100809 to 0 and the rate went absent (raw IDLE then also read 0). For the
// whole configured window the display object must therefore also carry the
// LAST DECODE sample's context.used and rate (llama-swap.sessions/v1 shape:
// display.contextUsed plus display.rate in the top-level rate's shape). The
// top-level raw rate/context keep reading the raw phase - machine readers
// (lane governor, liveness monitor, health watch) are never rewritten.

// decodeWithRate drives two DECODE polls of one request, one second apart,
// so the row carries a MEASURED decode rate, and leaves the builder holding
// its sample as the last DECODE. Every retention assertion below compares
// against that sample: context.used = 38000 cached + 2960 processed + 570
// decoded = 41530, rate = (570-512)/1s = 58.0 tok/s kind decode.
func decodeWithRate(t *testing.T, b *sessionsBuilder, t0 time.Time) {
	t.Helper()
	running := map[string]process.ProcessState{sessModel: process.StateReady}
	dec := func(n int) map[string][]childSlot {
		return map[string][]childSlot{sessModel: {{ID: 0, NCtx: 262144, IsProcessing: true,
			NPromptTokens: 90000, NPromptTokensCache: 38000, NPromptTokensProcessed: 2960, NDecoded: n}}}
	}
	got := b.build(sessionsInput{Now: t0, Running: running,
		Requests: []swaputil.InflightRequestEntry{retentionReq("r1", t0.Add(-5*time.Second), nil)}, Slots: dec(512)})
	if len(got.Sessions) != 1 || got.Sessions[0].Phase != "DECODE" {
		t.Fatalf("setup poll 1: %+v, want one DECODE row", got.Sessions)
	}
	got = b.build(sessionsInput{Now: t0.Add(time.Second), Running: running,
		Requests: []swaputil.InflightRequestEntry{retentionReq("r1", t0.Add(-5*time.Second), nil)}, Slots: dec(570)})
	if len(got.Sessions) != 1 {
		t.Fatalf("setup poll 2: %+v, want one row", got.Sessions)
	}
	row := got.Sessions[0]
	if row.Rate.Kind == nil || *row.Rate.Kind != "decode" || row.Rate.TokensPerSecond == nil || *row.Rate.TokensPerSecond != 58.0 {
		t.Fatalf("setup poll 2 rate = %+v, want decode 58.0 t/s", row.Rate)
	}
	if row.Context.Used != 41530 {
		t.Fatalf("setup poll 2 context.used = %d, want 41530", row.Context.Used)
	}
}

// rowWireMap is the row of one session in the body's WIRE shape (JSON keys,
// numbers as float64) - the shape machine clients and fixtures actually see.
func rowWireMap(t *testing.T, got sessionsBody, session string) map[string]any {
	t.Helper()
	m := bodyAsMap(t, got)
	rows, ok := m["sessions"].([]any)
	if !ok {
		t.Fatalf("wire sessions = %v, want a row list", m["sessions"])
	}
	for _, r := range rows {
		rm, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("wire session row = %v (%T), want an object", r, r)
		}
		if rm["sessionId"] == session {
			return rm
		}
	}
	t.Fatalf("session %s missing from wire body: %+v", session, got.Sessions)
	return nil
}

// retainedDisplay pins the readout contract on the wire body: inside the
// retention window a non-DECODE row's display object carries phase DECODE,
// the LAST DECODE sample's contextUsed and that sample's rate (same shape as
// the top-level rate). It returns the raw row map for raw-truthfulness
// assertions by the caller.
func retainedDisplay(t *testing.T, got sessionsBody, session string, wantUsed, wantTPS float64) map[string]any {
	t.Helper()
	row := rowWireMap(t, got, session)
	disp, ok := row["display"].(map[string]any)
	if !ok {
		t.Fatalf("%s: display = %v, want the retained readout {phase, contextUsed, rate}", session, row["display"])
	}
	if disp["phase"] != "DECODE" {
		t.Fatalf("%s: display.phase = %v, want DECODE", session, disp["phase"])
	}
	if v := disp["contextUsed"]; v != wantUsed {
		t.Fatalf("%s: display.contextUsed = %v, want %v (the last DECODE context.used)", session, v, wantUsed)
	}
	drate, ok := disp["rate"].(map[string]any)
	if !ok {
		t.Fatalf("%s: display.rate = %v, want the last DECODE rate in the top-level rate's shape {kind, tokensPerSecond, windowSeconds}", session, disp["rate"])
	}
	if drate["kind"] != "decode" || drate["tokensPerSecond"] != wantTPS {
		t.Fatalf("%s: display.rate = %v, want {kind:decode, tokensPerSecond:%v}", session, drate, wantTPS)
	}
	return row
}

// noDisplay pins the other side: no retention, no display key at all.
func noDisplay(t *testing.T, got sessionsBody, session string) map[string]any {
	t.Helper()
	row := rowWireMap(t, got, session)
	if _, ok := row["display"]; ok {
		t.Fatalf("%s: display = %v, want no display override", session, row["display"])
	}
	return row
}

func rawUsed(t *testing.T, row map[string]any) any {
	t.Helper()
	ctx, ok := row["context"].(map[string]any)
	if !ok {
		t.Fatalf("wire context = %v, want an object", row["context"])
	}
	return ctx["used"]
}

func rawRateKind(t *testing.T, row map[string]any) any {
	t.Helper()
	rate, ok := row["rate"].(map[string]any)
	if !ok {
		t.Fatalf("wire rate = %v, want an object", row["rate"])
	}
	return rate["kind"]
}

// IDLE then PREFILL inside the window expose the retained contextUsed and
// decode rate; the raw row reads its own phase truthfully; a session that
// never decoded gets no display at all; past the window the display is gone.
func TestSessions_DecodeRetentionRetainsRateAndContextUsed(t *testing.T) {
	b := newSessionsBuilder(sessionsTestConfig())
	t0 := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	running := map[string]process.ProcessState{sessModel: process.StateReady}
	decodeWithRate(t, b, t0) // last DECODE at t0+1s: used 41530, decode 58.0 t/s

	// +2 s: the request finished. Raw IDLE reads used 0 with no rate kind;
	// the display keeps DECODE with the retained readout.
	got := b.build(sessionsInput{Now: t0.Add(2 * time.Second), Running: running})
	row := retainedDisplay(t, got, "s1", 41530, 58.0)
	if v := rawRateKind(t, row); v != nil {
		t.Fatalf("raw rate.kind = %v, want nil on the IDLE row (the display override must never rewrite the raw fields)", v)
	}
	if v := rawUsed(t, row); v != float64(0) {
		t.Fatalf("raw context.used = %v, want 0 while display.contextUsed holds the retained 41530", v)
	}
	assertSessionsInvariants(t, got)

	// +4 s: s1's next turn is in PREFILL beside s2, which never decoded.
	prefill := map[string][]childSlot{sessModel: {{ID: 0, NCtx: 262144, IsProcessing: true, NPromptTokens: 90000, NPromptTokensProcessed: 10}}}
	mk := func(id, sess string, at time.Time) swaputil.InflightRequestEntry {
		return swaputil.InflightRequestEntry{ID: id, Model: sessModel, Timestamp: at,
			Metadata: map[string]string{"session_id": sess, "slot_granted": "1", "slot_id": "0"}}
	}
	got = b.build(sessionsInput{Now: t0.Add(4 * time.Second), Running: running,
		Requests: []swaputil.InflightRequestEntry{mk("r2", "s1", t0.Add(4*time.Second)), mk("r3", "s2", t0.Add(4*time.Second))}, Slots: prefill})
	row = retainedDisplay(t, got, "s1", 41530, 58.0)
	if v := rawUsed(t, row); v != float64(10) {
		t.Fatalf("s1 raw context.used = %v, want 10 (the young PREFILL's own counters)", v)
	}
	if v := rawRateKind(t, row); v != "prefill" {
		t.Fatalf("s1 raw rate.kind = %v, want prefill while display.rate holds the retained decode rate", v)
	}
	s2row := noDisplay(t, got, "s2")
	if v := rawUsed(t, s2row); v != float64(10) {
		t.Fatalf("s2 raw context.used = %v, want 10", v)
	}
	assertSessionsInvariants(t, got)

	// +34 s: past the 30 s window the overrides are gone; both rows are bare
	// IDLE rows reading their raw (zero) counters.
	got = b.build(sessionsInput{Now: t0.Add(34 * time.Second), Running: running})
	noDisplay(t, got, "s1")
	s2row = noDisplay(t, got, "s2")
	if v := rawUsed(t, s2row); v != float64(0) {
		t.Fatalf("s2 raw context.used = %v, want 0", v)
	}
	assertSessionsInvariants(t, got)
}

// The exact zero-first-PREFILL case: the next turn's FIRST poll lands before
// the child has processed anything, so the raw PREFILL row reads
// context.used 0 and decoded 0 (its own rate still unmeasured). Inside the
// window the display must still hold the last DECODE's readout (used 41530,
// 58.0 t/s) - the raw zeros stay on the raw fields and no zero leaks into
// the retained readout.
func TestSessions_DecodeRetentionCoversZeroFirstPrefill(t *testing.T) {
	b := newSessionsBuilder(sessionsTestConfig())
	t0 := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	running := map[string]process.ProcessState{sessModel: process.StateReady}
	decodeWithRate(t, b, t0) // last DECODE at t0+1s: used 41530, decode 58.0 t/s

	// +3 s (2 s after the last DECODE, inside the 30 s window): the slot is
	// processing the new prompt but has processed NOTHING yet - every raw
	// counter is zero.
	zeroPrefill := map[string][]childSlot{sessModel: {{ID: 0, NCtx: 262144, IsProcessing: true,
		NPromptTokens: 90000, NPromptTokensProcessed: 0, NDecoded: 0}}}
	got := b.build(sessionsInput{Now: t0.Add(3 * time.Second), Running: running,
		Requests: []swaputil.InflightRequestEntry{retentionReq("r2", t0.Add(3*time.Second), nil)}, Slots: zeroPrefill})
	if len(got.Sessions) != 1 || got.Sessions[0].Phase != "PREFILL" {
		t.Fatalf("poll = %+v, want one raw PREFILL row", got.Sessions)
	}
	row := retainedDisplay(t, got, "s1", 41530, 58.0)
	if v := rawUsed(t, row); v != float64(0) {
		t.Fatalf("raw context.used = %v, want 0 (the zero-first PREFILL's own counters stay raw)", v)
	}
	ctx := row["context"].(map[string]any)
	if v := ctx["decoded"]; v != float64(0) {
		t.Fatalf("raw context.decoded = %v, want 0", v)
	}
	assertSessionsInvariants(t, got)
}

// PARKED and HOT rows inside the window carry the same retained readout - HOT
// being exactly the witnessed case: the slot binds the lane but the child no
// longer reports the prompt, so the raw context.used has fallen to 0 while
// the display must keep the last DECODE's 41530 / 58.0 t/s. Raw context.window
// stays the denominator throughout.
func TestSessions_DecodeRetentionRetainsReadoutAcrossParkedAndHot(t *testing.T) {
	b := newSessionsBuilder(sessionsTestConfig())
	t0 := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	running := map[string]process.ProcessState{sessModel: process.StateReady}
	decodeWithRate(t, b, t0) // last DECODE at t0+1s

	// +4 s: the next turn is PARKED behind another session on the lane.
	got := b.build(sessionsInput{Now: t0.Add(4 * time.Second), Running: running,
		Requests: []swaputil.InflightRequestEntry{
			retentionReq("r2", t0.Add(4*time.Second), map[string]string{"park_reason": "lane", "waiting_for_session": "s2"})}})
	row := retainedDisplay(t, got, "s1", 41530, 58.0)
	if v := rawUsed(t, row); v != float64(0) {
		t.Fatalf("PARKED raw context.used = %v, want 0 (a parked row carries no slot counters)", v)
	}
	wf, ok := row["waitingFor"].(map[string]any)
	if !ok || wf["session"] != "s2" {
		t.Fatalf("waitingFor = %v, want the park's own {session:s2} kept intact", row["waitingFor"])
	}
	assertSessionsInvariants(t, got)

	// +8 s (7 s after the last DECODE): HOT - the slot still binds the lane
	// but the child reports an empty prompt: the witnessed shape.
	hotSlots := map[string][]childSlot{sessModel: {{ID: 0, NCtx: 262144}}}
	got = b.build(sessionsInput{Now: t0.Add(8 * time.Second), Running: running,
		Hot: map[string][]swaputil.HotSlot{sessModel: {{Slot: 0, SessionID: "s1", IdleSeconds: 7}}}, Slots: hotSlots})
	row = retainedDisplay(t, got, "s1", 41530, 58.0)
	if v := rawUsed(t, row); v != float64(0) {
		t.Fatalf("HOT raw context.used = %v, want 0 (the witnessed drop; the display readout must cover it)", v)
	}
	ctx := row["context"].(map[string]any)
	if v := ctx["window"]; v != float64(262144) {
		t.Fatalf("HOT raw context.window = %v, want 262144 (the denominator stays raw)", v)
	}
	assertSessionsInvariants(t, got)

	// +35 s: past the window the HOT row shows itself with no override.
	got = b.build(sessionsInput{Now: t0.Add(35 * time.Second), Running: running,
		Hot: map[string][]swaputil.HotSlot{sessModel: {{Slot: 0, SessionID: "s1", IdleSeconds: 34}}}, Slots: hotSlots})
	noDisplay(t, got, "s1")
	assertSessionsInvariants(t, got)
}

// The uncovered edge: stampRate sets kind=decode on the FIRST DECODE sample
// while tokensPerSecond is still nil (two samples are needed to measure). A
// retained row downstream must not expose that incomplete rate as
// display.rate - the kind is real but the number was never measured - nor may
// the raw PREFILL's own rate render through a fallback. display.contextUsed
// still persists; the raw fields stay raw.
func TestSessions_DecodeRetentionOmitsRateWhenLastDecodeUnmeasured(t *testing.T) {
	b := newSessionsBuilder(sessionsTestConfig())
	t0 := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	running := map[string]process.ProcessState{sessModel: process.StateReady}

	// One DECODE poll: kind=decode is set but tokensPerSecond stays nil -
	// the sample is the only one in the window's history.
	decodeSlots := map[string][]childSlot{sessModel: {{ID: 0, NCtx: 262144, IsProcessing: true,
		NPromptTokens: 90000, NPromptTokensCache: 38000, NPromptTokensProcessed: 2960, NDecoded: 570}}}
	got := b.build(sessionsInput{Now: t0, Running: running,
		Requests: []swaputil.InflightRequestEntry{retentionReq("r1", t0.Add(-5*time.Second), nil)}, Slots: decodeSlots})
	if len(got.Sessions) != 1 || got.Sessions[0].Rate.Kind == nil || *got.Sessions[0].Rate.Kind != "decode" || got.Sessions[0].Rate.TokensPerSecond != nil {
		t.Fatalf("setup poll: rate = %+v, want kind=decode with tokensPerSecond nil (first sample, not measured)", got.Sessions[0].Rate)
	}
	if got.Sessions[0].Context.Used != 41530 {
		t.Fatalf("setup poll context.used = %d, want 41530", got.Sessions[0].Context.Used)
	}

	// Next turn: PREFILL. Two polls one second apart measure a real prefill
	// rate; both are inside the retention window of the lone DECODE.
	mk := func(id, sess string, at time.Time) swaputil.InflightRequestEntry {
		return swaputil.InflightRequestEntry{ID: id, Model: sessModel, Timestamp: at,
			Metadata: map[string]string{"session_id": sess, "slot_granted": "1", "slot_id": "0"}}
	}
	prefillSlots := func(processed int) map[string][]childSlot {
		return map[string][]childSlot{sessModel: {{ID: 0, NCtx: 262144, IsProcessing: true,
			NPromptTokens: 90000, NPromptTokensProcessed: processed}}}
	}
	got = b.build(sessionsInput{Now: t0.Add(time.Second), Running: running,
		Requests: []swaputil.InflightRequestEntry{mk("r2", "s1", t0.Add(time.Second))}, Slots: prefillSlots(10000)})
	got = b.build(sessionsInput{Now: t0.Add(2 * time.Second), Running: running,
		Requests: []swaputil.InflightRequestEntry{mk("r2", "s1", t0.Add(time.Second))}, Slots: prefillSlots(20000)})

	row := rowWireMap(t, got, "s1")
	disp, ok := row["display"].(map[string]any)
	if !ok {
		t.Fatalf("s1: display = %v, want the retained DECODE display", row["display"])
	}
	if disp["phase"] != "DECODE" {
		t.Fatalf("s1: display.phase = %v, want DECODE", disp["phase"])
	}
	if v := disp["contextUsed"]; v != float64(41530) {
		t.Fatalf("s1: display.contextUsed = %v, want 41530 (retained even without a measured rate)", v)
	}
	if _, ok := disp["rate"]; ok {
		t.Fatalf("s1: display.rate = %v, want NO rate object (the last DECODE never measured one; "+
			"a kind without tokensPerSecond must not be exposed)", disp["rate"])
	}
	raw, ok := row["rate"].(map[string]any)
	if !ok {
		t.Fatalf("s1: wire rate = %v, want an object", row["rate"])
	}
	if raw["kind"] != "prefill" || raw["tokensPerSecond"] != float64(10000) {
		t.Fatalf("s1: raw rate = %v, want {kind:prefill, tokensPerSecond:10000} unchanged by the retention", raw)
	}
	assertSessionsInvariants(t, got)
}

// retention=0 turns the whole override off: not just the phase sticker, the
// readout too - the wire row carries no display key at all.
func TestSessions_DecodeRetentionZeroOmitsReadout(t *testing.T) {
	cfg := sessionsTestConfig()
	cfg.Sessions.DecodeRetentionSeconds = 0
	b := newSessionsBuilder(cfg)
	t0 := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	running := map[string]process.ProcessState{sessModel: process.StateReady}
	decodeWithRate(t, b, t0)

	got := b.build(sessionsInput{Now: t0.Add(2 * time.Second), Running: running})
	row := noDisplay(t, got, "s1")
	if v := rawRateKind(t, row); v != nil {
		t.Fatalf("raw rate.kind = %v, want nil (retention=0 must not touch the raw fields either)", v)
	}
	assertSessionsInvariants(t, got)
}
