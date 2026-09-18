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
	"github.com/mostlygeek/llama-swap/internal/swaputil"
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
