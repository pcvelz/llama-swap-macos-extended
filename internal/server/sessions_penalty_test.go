package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

const penSession = "934159af-1111-2222-3333-444455556666"

// penaltyTracker builds a loop tracker on a controllable clock, with a short
// ladder so a test does not have to record 60 responses.
func penaltyTracker(clk *time.Time, penalties ...int) *swaputil.LoopTracker {
	g := swaputil.LoopGuard{RunBar: 3, ToleranceTokens: 2, Strikes: len(penalties), PenaltySeconds: penalties, ClearRequests: 5}
	return swaputil.NewLoopTrackerWithClock(g, func() time.Time { return *clk })
}

func feedSession(lt *swaputil.LoopTracker, session string, n int) {
	for i := 0; i < n; i++ {
		lt.Record(session, 48)
	}
}

// A strike that carries NO hold is published (it is real state an operator
// must see before it escalates) but leaves the phase alone.
func TestSessions_PenaltyFlagOnlyKeepsThePhase(t *testing.T) {
	b := newSessionsBuilder(sessionsTestConfig())
	now := fixtureClock(t, "2026-09-19T12:00:00.000+02:00")
	clk := now
	lt := penaltyTracker(&clk, 0, 900, -1)
	feedSession(lt, penSession, 3) // strike 1, no hold

	got := b.build(penaltyInput(now, lt, decodeRequest(now)))
	row := rowFor(t, got, penSession)
	if row.Phase != phaseDecode {
		t.Fatalf("phase=%s want %s: a flag-only strike must not change the phase", row.Phase, phaseDecode)
	}
	if row.Penalty == nil || row.Penalty.Strike != 1 || row.Penalty.Strikes != 3 {
		t.Fatalf("penalty=%+v want strike 1 of 3", row.Penalty)
	}
	if row.Penalty.RemainingSeconds == nil || *row.Penalty.RemainingSeconds != 0 {
		t.Fatalf("remainingSeconds=%v want 0 for a strike with no hold", row.Penalty.RemainingSeconds)
	}
	if row.Penalty.TypicalTokens != 48 || row.Penalty.UniformRun != 3 {
		t.Fatalf("penalty=%+v want uniformRun 3, typicalTokens 48", row.Penalty)
	}
	if row.Penalty.Held {
		t.Fatalf("penalty.held=true for a strike that carries no hold")
	}
	assertSessionsInvariants(t, got)
}

// A timed hold: phase PENALIZED and a counting-down remainingSeconds.
func TestSessions_PenaltyTimedHoldCountsDown(t *testing.T) {
	b := newSessionsBuilder(sessionsTestConfig())
	now := fixtureClock(t, "2026-09-19T12:00:00.000+02:00")
	clk := now
	lt := penaltyTracker(&clk, 0, 900, -1)
	feedSession(lt, penSession, 6) // strike 2 -> 900s hold

	// The scheduler is ACTUALLY holding it: a request parked as `penalized`.
	// Since phase 3 that park reason, not the strike, is what makes the row
	// PENALIZED - the contention gate admits a penalized session normally
	// whenever nobody else is waiting.
	got := b.build(penaltyInput(now.Add(140*time.Second), lt, heldRequest(now)))
	row := rowFor(t, got, penSession)
	if row.Phase != phasePenalized {
		t.Fatalf("phase=%s want %s", row.Phase, phasePenalized)
	}
	if !row.Penalty.Held {
		t.Fatalf("penalty.held=false on a row the scheduler is holding")
	}
	if row.Penalty.RemainingSeconds == nil || *row.Penalty.RemainingSeconds != 760 {
		t.Fatalf("remainingSeconds=%v want 760", row.Penalty.RemainingSeconds)
	}
	assertSessionsInvariants(t, got)
}

// THE FALSE POSITIVE, published side (2026-09-20). A session with strikes
// that is being SERVED (the contention gate admitted it - nobody else is
// waiting) must keep its real phase and report held:false. Showing it as
// PENALIZED while it decodes is how an hour of held-against-an-empty-box went
// unnoticed.
func TestSessions_StrikesWithoutAHoldAreNotPenalizedPhase(t *testing.T) {
	b := newSessionsBuilder(sessionsTestConfig())
	now := fixtureClock(t, "2026-09-20T12:00:00.000+02:00")
	clk := now
	lt := penaltyTracker(&clk, 0, 900, 3600)
	feedSession(lt, penSession, 9) // strike 3, the tracker considers it held

	// ...but the scheduler admitted it: a granted, decoding request.
	got := b.build(penaltyInput(now, lt, decodeRequest(now)))
	row := rowFor(t, got, penSession)
	if row.Phase != phaseDecode {
		t.Fatalf("phase=%s want %s: a session being SERVED is not PENALIZED", row.Phase, phaseDecode)
	}
	if row.Penalty == nil || row.Penalty.Held {
		t.Fatalf("penalty=%+v want the strike published with held:false", row.Penalty)
	}
	assertSessionsInvariants(t, got)
}

// The HELD penalty (-1) must serialise remainingSeconds as JSON null: it is
// the one state a countdown cannot express.
func TestSessions_PenaltyHeldSerialisesNullRemaining(t *testing.T) {
	b := newSessionsBuilder(sessionsTestConfig())
	now := fixtureClock(t, "2026-09-19T12:00:00.000+02:00")
	clk := now
	lt := penaltyTracker(&clk, 0, 900, -1)
	feedSession(lt, penSession, 9) // strike 3 -> HELD

	got := b.build(penaltyInput(now, lt, decodeRequest(now)))
	row := rowFor(t, got, penSession)
	if row.Penalty.RemainingSeconds != nil {
		t.Fatalf("remainingSeconds=%v want nil (JSON null) for a HELD penalty", *row.Penalty.RemainingSeconds)
	}
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"remainingSeconds":null`) {
		t.Fatalf("row JSON must carry remainingSeconds:null, got %s", raw)
	}
	if !strings.Contains(string(raw), `"reason":"loop"`) {
		t.Fatalf("row JSON must carry the penalty reason, got %s", raw)
	}
}

// The row must survive between the client's retries: no in-flight request at
// all and far past the 60s IDLE drop, it is still listed and still carries
// its penalty, so an operator can find it and click un-penalize. Its phase is
// IDLE rather than PENALIZED because nothing is being held at that instant -
// there is no request to hold.
func TestSessions_PenalizedRowSurvivesWithoutARequest(t *testing.T) {
	b := newSessionsBuilder(sessionsTestConfig())
	now := fixtureClock(t, "2026-09-19T12:00:00.000+02:00")
	clk := now
	lt := penaltyTracker(&clk, 0, -1)
	feedSession(lt, penSession, 3) // strike 1 of this ladder: no hold yet

	// The session is seen once with a request, then never again.
	b.build(penaltyInput(now, lt, decodeRequest(now)))
	feedSession(lt, penSession, 3) // strike 2 -> HELD

	later := now.Add(10 * time.Minute) // far past sessionsIdleRetention
	clk = later
	got := b.build(penaltyInput(later, lt))
	row := rowFor(t, got, penSession)
	if row.Phase != phaseIdle {
		t.Fatalf("phase=%s want %s: nothing is being held while it has no request", row.Phase, phaseIdle)
	}
	if row.Penalty == nil || row.Penalty.Strike != 2 {
		t.Fatalf("penalty=%+v want strike 2 still published on the surviving row", row.Penalty)
	}
	if row.Penalty.Held {
		t.Fatalf("penalty.held=true with no request to hold")
	}
}

// Invariant 2 must hold exactly with a penalized session present: a PENALIZED
// row is never counted as waiting (it is not in the queue at all - the
// scheduler keeps it in a separate list).
func TestSessions_PenalizedRowIsNeverCountedAsWaiting(t *testing.T) {
	b := newSessionsBuilder(sessionsTestConfig())
	now := fixtureClock(t, "2026-09-19T12:00:00.000+02:00")
	clk := now
	lt := penaltyTracker(&clk, -1)
	feedSession(lt, penSession, 3) // HELD

	// The held session's request would otherwise be a PARKED row; a second,
	// innocent session really is parked.
	held := swaputil.InflightRequestEntry{
		ID: "r-1", Model: sessModel, Timestamp: now.Add(-time.Second),
		Metadata: map[string]string{"session_id": penSession, "park_reason": "penalized", "tier": "default"},
	}
	parked := swaputil.InflightRequestEntry{
		ID: "r-2", Model: sessModel, Timestamp: now.Add(-time.Second),
		Metadata: map[string]string{"session_id": "innocent-session", "park_reason": "cooldown", "tier": "priority"},
	}
	got := b.build(penaltyInput(now, lt, held, parked))

	if got.Queue.Waiting != 1 {
		t.Fatalf("queue.waiting=%d want 1: only the innocent parked session counts", got.Queue.Waiting)
	}
	if got.Queue.ByTier["priority"] != 1 || got.Queue.ByTier["default"] != 0 {
		t.Fatalf("byTier=%v want priority 1, default 0", got.Queue.ByTier)
	}
	row := rowFor(t, got, penSession)
	if row.Phase != phasePenalized || row.ParkReason != nil {
		t.Fatalf("held row=%+v want PENALIZED with no park reason", row)
	}
	assertSessionsInvariants(t, got)
}

// POST /api/sessions/{id}/unpenalize clears the penalty. Always 200, and a
// no-op for an id the tracker never saw.
func TestServer_SessionsUnpenalize(t *testing.T) {
	s := newTestServer(newStubRouter([]string{"cq35"}, ""), newStubRouter(nil, ""))
	s.inflight.setLoopGuard(config.LoopGuardConfig{Enabled: true, RunBar: 3, ToleranceTokens: 2, Strikes: 1, PenaltySeconds: []int{-1}, ClearRequests: 5})
	feedSession(s.inflight.loops, penSession, 3)
	if held, _, _ := s.inflight.loops.Held(penSession); !held {
		t.Fatalf("setup: the session should be held")
	}

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/sessions/"+penSession+"/unpenalize", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if held, _, _ := s.inflight.loops.Held(penSession); held {
		t.Fatalf("the session must be released")
	}
	if got := s.inflight.loops.Run(penSession); got != 0 {
		t.Fatalf("Run=%d want 0: un-penalize forgets the history", got)
	}

	// Unknown id: still 200, still a no-op.
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/sessions/no-such-session/unpenalize", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("unknown id: status=%d want 200", w.Code)
	}
}

// The state trace names held sessions, which otherwise have no in-flight
// request to show: an idle box with no reason is the thing this prevents.
func TestStateTrace_NamesPenalizedSessions(t *testing.T) {
	base := traceSnapshot{Resident: "cq27", ResidentState: process.StateReady}
	if got := traceLine(base); strings.Contains(got, "penalized=") {
		t.Fatalf("a line with nothing held must keep its exact old shape: %q", got)
	}
	base.Penalized = []string{penSession}
	if got := traceLine(base); !strings.HasSuffix(got, " penalized=[934159af]") {
		t.Fatalf("line=%q want a trailing penalized=[934159af]", got)
	}
}

// --- helpers ---

func penaltyInput(now time.Time, lt *swaputil.LoopTracker, reqs ...swaputil.InflightRequestEntry) sessionsInput {
	return sessionsInput{
		Now:         now,
		Running:     map[string]process.ProcessState{sessModel: process.StateReady},
		Requests:    reqs,
		Slots:       map[string][]childSlot{sessModel: {{ID: 0, NCtx: 262144, IsProcessing: true, NPromptTokens: 100, NPromptTokensCache: 100, NDecoded: 5}}},
		MemoryBrake: enabledBrake(),
		Loops:       lt,
	}
}

// heldRequest is a request the scheduler parked as `penalized` - the live
// evidence that the session is being held right now.
func heldRequest(now time.Time) swaputil.InflightRequestEntry {
	return swaputil.InflightRequestEntry{
		ID: "r-9", Model: sessModel, Timestamp: now.Add(-2 * time.Second),
		Metadata: map[string]string{"session_id": penSession, "park_reason": "penalized", "tier": "default"},
	}
}

func decodeRequest(now time.Time) swaputil.InflightRequestEntry {
	return swaputil.InflightRequestEntry{
		ID: "r-1", Model: sessModel, Timestamp: now.Add(-2 * time.Second), RespTokens: 5,
		Metadata: map[string]string{"session_id": penSession, "slot_granted": "1", "slot_id": "0", "tier": "default"},
	}
}

func rowFor(t *testing.T, b sessionsBody, sessionID string) sessionEntry {
	t.Helper()
	for _, row := range b.Sessions {
		if row.SessionID == sessionID {
			return row
		}
	}
	t.Fatalf("no row for session %s in %+v", sessionID, b.Sessions)
	return sessionEntry{}
}
