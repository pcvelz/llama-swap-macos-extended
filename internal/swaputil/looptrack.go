package swaputil

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Loop tracking (fork). WHY it exists, 2026-09-19: a Claude Code session
// (934159af, cq27) sat in ONE turn for 4.5h making 792 consecutive tool
// calls. Every /v1/messages response emitted 46-49 output tokens - 86 of 86
// requests inside one 30-minute window - one request every ~20s. swap-grace
// (swapGraceSeconds) is an idle-STREAK requirement, so that cadence reset the
// resident's idle clock forever and two sessions parked for ANOTHER model
// (cq35) starved for hours. The obvious valve, swapStarvationSeconds, is
// deliberately OFF (-1) in production because it cannot tell a productive
// live session from this one, and on 2026-09-08 it swapped out a live
// interactive session (see internal/router/scheduler/fifo.go, withinGrace /
// deferredByGrace).
//
// The missing discriminator is "is this session in a degenerate loop?".
// LoopTracker answers it from the ONE signal the proxy already measures per
// request: the real output-token count (InflightRequestEntry.RespTokens,
// counted from content_block_delta events). A session whose last N finished
// responses were all the same size to within a couple of tokens is looping;
// anything with varied response sizes is productive and keeps its full
// protection.
//
// Two consequences, in escalating order:
//
//  1. A looping session's completion does not earn the resident FRESH
//     swap-grace (phase 1). It is still served in full.
//  2. Once its run reaches a STRIKE (n * runBar), the configured penalty for
//     that strike applies: flag only, a timed hold, or held until an operator
//     un-penalizes it. A held session's NEXT requests are PARKED, never
//     refused - "a session is always promised a turn" is a user ruling, so
//     the penalty delays work, it never destroys it, and the request that
//     earned the strike is never cut mid-flight.
//
// A hold deliberately does NOT reset the run: "the agent rebounding on the
// box is undesired" (user ruling). A session that comes back and keeps
// emitting the same thing climbs to the next strike instead of restarting
// from zero.

const (
	// DefaultLoopRunBar is how many consecutive same-size responses make a
	// session a loop, and DefaultLoopTolerance is the max spread (max - min)
	// inside that run. UNPROVEN TUNABLES: both were read off ONE captured
	// loop (2026-09-19, 86/86 requests at 46-49 tokens) and have never been
	// measured against healthy traffic. 20 is ~7 minutes at the observed
	// ~20s cadence, i.e. well past any legitimate run of same-length turns.
	// They are the FALLBACK only: production reads config.LoopGuardConfig
	// (`loopGuard:` in the YAML) precisely because numbers this unproven must
	// be tunable without a rebuild.
	DefaultLoopRunBar    = 20
	DefaultLoopTolerance = int64(2)

	// DefaultMaxLoopTokens is the OUTPUT CEILING: a response larger than this
	// is productive work and can never be loop evidence, however uniform it
	// is. WHY (2026-09-20): the guard held a HEALTHY session (ef043b3f) whose
	// 45 responses were all ~1083 tokens +-2 - uniform because the client
	// caps output, not because it was looping - for an hour against an empty
	// box. Size was always the tell: the real loop emitted 46-49 tokens a
	// turn, roughly 4% of that.
	DefaultMaxLoopTokens = int64(256)

	// loopHistoryMin is the FLOOR on one session's ring. The real bound is
	// runBar * strikes + 1 (computed per tracker), so the top strike is
	// always reachable; this floor only keeps a tiny configuration from
	// shrinking the window below anything useful.
	loopHistoryMin = 64

	// loopSessionTTL drops a session that has not completed a request for
	// this long, so the map cannot grow forever across a box's uptime. Pruned
	// lazily on Record - there is no goroutine here to sweep on a timer, and
	// a stale entry costs nothing until it is read. A session currently
	// HELD is exempt: its whole point is that it is not sending requests.
	loopSessionTTL = 30 * time.Minute

	// PenaltyReasonLoop is the only reason a penalty exists today. Carried in
	// the published state so a later reason (say, a byte-budget breach) is an
	// added value rather than a schema change.
	PenaltyReasonLoop = "loop"
)

// Loop penalty event kinds, emitted to the observer (SetPenaltyObserver) for
// the debug history's "penalty" events.
const (
	PenaltyEventStrike     = "strike"
	PenaltyEventHoldStart  = "hold-start"
	PenaltyEventHoldEnd    = "hold-end"
	PenaltyEventUnpenalize = "unpenalize"
)

// LoopGuard is the tracker's configuration, mirroring config.LoopGuardConfig
// (which this package must not import: config imports swaputil, not the
// reverse).
type LoopGuard struct {
	RunBar          int
	ToleranceTokens int64
	// Strikes is how many strikes exist; PenaltySeconds carries one entry per
	// strike: 0 = flag only, N > 0 = hold N seconds, -1 = hold until
	// un-penalized. A short list is padded with 0 (flag only) rather than
	// panicking - config.load rejects the mismatch long before this.
	Strikes        int
	PenaltySeconds []int
	// ClearRequests is how many consecutive recorded requests with a run
	// below RunBar wipe the session's strikes and any hold.
	ClearRequests int
	// MaxLoopTokens is the OUTPUT CEILING: a response larger than this is
	// productive work and BREAKS the trailing run (see runLocked). 0 falls
	// back to DefaultMaxLoopTokens.
	MaxLoopTokens int64
}

// DefaultLoopGuard mirrors config.DefaultLoopGuardConfig for callers that
// have no config in scope (tests, bare trackers).
func DefaultLoopGuard() LoopGuard {
	return LoopGuard{
		RunBar:          DefaultLoopRunBar,
		ToleranceTokens: DefaultLoopTolerance,
		Strikes:         3,
		PenaltySeconds:  []int{0, 900, 3600},
		ClearRequests:   10,
		MaxLoopTokens:   DefaultMaxLoopTokens,
	}
}

// LoopPenaltyEvent is one transition in a session's penalty state, handed to
// the observer so the debug history can log it. Emitted AFTER the tracker's
// lock is released, so an observer may call back into anything.
type LoopPenaltyEvent struct {
	SessionID string
	// Kind is one of the PenaltyEvent* constants.
	Kind   string
	Reason string
	Strike int
	// HoldSeconds is the hold this strike carries: 0 = none, N = N seconds,
	// -1 = until un-penalized. Only meaningful on strike / hold-start.
	HoldSeconds int
	// UniformRun is the trailing run at the moment of the event, and
	// TypicalTokens its median - the "what did it keep emitting" a log line
	// needs to be actionable rather than just alarming.
	UniformRun    int
	TypicalTokens int64
	// Source describes where an action came from (e.g. "menu-click", the HTTP
	// User-Agent). Only populated on un-penalize and cancel events. Empty for
	// mechanical events like strike / hold-start / hold-end.
	Source string
	// UserAgent is the HTTP User-Agent of the caller, logged alongside Source
	// so a log line names who did what (e.g. "llama-swap-menu" vs "curl/7.0").
	UserAgent string
}

// PenaltyState is a session's published penalty. Zero value means "no
// penalty"; callers get ok=false instead.
type PenaltyState struct {
	Reason  string
	Strike  int
	Strikes int
	// Held reports an ACTIVE hold. A strike whose penalty is 0 is a flag with
	// Held false - real, published, and costing the session nothing but its
	// swap-grace refresh.
	Held bool
	// HeldForever is the -1 penalty: no timer will ever release it, only
	// Unpenalize.
	HeldForever bool
	// Until is when a timed hold ends; zero when not held or held forever.
	Until time.Time
	// UniformRun is the trailing run, TypicalTokens its median.
	UniformRun    int
	TypicalTokens int64
}

// LoopTracker records finished responses per session and answers whether a
// session is in a degenerate loop, plus what penalty it has earned. Safe for
// concurrent use: Record runs on the serving hot path (one call per completed
// request) while the verdict/penalty reads come from the session-state loop,
// the scheduler's admission path and arbitrary HTTP goroutines.
//
// A NIL *LoopTracker is the DISABLED tracker (config loopGuard.enabled:
// false): every method is a nil-safe no-op returning the "productive,
// unpenalized" answer, so a deployment that turns the guard off gets
// byte-for-byte the pre-2026-09-19 behaviour without a second code path
// anywhere.
type LoopTracker struct {
	guard      LoopGuard
	historyMax int

	mu       sync.Mutex
	sessions map[string]*loopHistory

	// observerMu guards observer, which is installed once at construction
	// time but from a different goroutine than the Record calls that fire it.
	observerMu sync.RWMutex
	observer   func(LoopPenaltyEvent)

	// now is the clock; overridable in tests.
	now func() time.Time
}

// loopHistory is one session's per-request memory: the response sizes, the
// strike ladder position, and any active hold.
type loopHistory struct {
	tokens []int64
	last   time.Time
	// strike is the highest strike this session has reached, 0 for none. It
	// only ever falls via clearStreak or Unpenalize.
	strike int
	// heldUntil / heldForever describe the ACTIVE hold, if any.
	heldUntil   time.Time
	heldForever bool
	// clearStreak counts consecutive recorded requests whose trailing run was
	// below the run bar. At ClearRequests the session is forgiven.
	clearStreak int
}

// NewLoopTracker builds a tracker from g. Fields left at zero fall back to
// the package defaults so a miswired caller cannot accidentally declare every
// session a loop, or invent a strike ladder nobody configured.
func NewLoopTracker(g LoopGuard) *LoopTracker {
	if g.RunBar <= 0 {
		g.RunBar = DefaultLoopRunBar
	}
	if g.ToleranceTokens < 0 {
		g.ToleranceTokens = DefaultLoopTolerance
	}
	if g.Strikes < 1 {
		g.Strikes = 1
	}
	if g.ClearRequests < 1 {
		g.ClearRequests = DefaultLoopGuard().ClearRequests
	}
	if g.MaxLoopTokens < 1 {
		g.MaxLoopTokens = DefaultMaxLoopTokens
	}
	// The ring must be able to HOLD the top strike's run, or that strike
	// could never be reached however long the loop went on.
	historyMax := g.RunBar*g.Strikes + 1
	if historyMax < loopHistoryMin {
		historyMax = loopHistoryMin
	}
	return &LoopTracker{
		guard:      g,
		historyMax: historyMax,
		sessions:   map[string]*loopHistory{},
		now:        time.Now,
	}
}

// NewLoopTrackerWithClock is NewLoopTracker on a caller-supplied clock. Every
// deadline in here (the hold, the idle TTL) is wall-clock, so tests outside
// this package need a way to move time without sleeping.
func NewLoopTrackerWithClock(g LoopGuard, now func() time.Time) *LoopTracker {
	t := NewLoopTracker(g)
	if now != nil {
		t.now = now
	}
	return t
}

// SetPenaltyObserver installs the sink for LoopPenaltyEvents (the debug
// history). Called once at server construction.
func (t *LoopTracker) SetPenaltyObserver(f func(LoopPenaltyEvent)) {
	if t == nil {
		return
	}
	t.observerMu.Lock()
	t.observer = f
	t.observerMu.Unlock()
}

// emit hands events to the observer. ALWAYS called after t.mu is released:
// an observer is arbitrary code (it writes the debug history, which has its
// own lock) and must never run under the hot path's lock.
func (t *LoopTracker) emit(events []LoopPenaltyEvent) {
	if len(events) == 0 {
		return
	}
	t.observerMu.RLock()
	obs := t.observer
	t.observerMu.RUnlock()
	if obs == nil {
		return
	}
	for _, ev := range events {
		obs(ev)
	}
}

// Record appends one finished request's REAL output-token count to sessionID's
// history and advances the strike ladder. Callers must only pass requests that
// actually produced model output for a session (POST /v1/messages with a
// session id and RespTokens > 0): a status read, a count_tokens call or a
// timed-out retry carries no turn, and a run of their zero counts would fake a
// perfect loop - see internal/server/inflight.go recordLoopLocked, which owns
// that gate. An empty sessionID is ignored: a request with no session cannot
// be a looping session.
func (t *LoopTracker) Record(sessionID string, respTokens int64) {
	if t == nil || sessionID == "" {
		return
	}
	now := t.now()

	t.mu.Lock()
	h, ok := t.sessions[sessionID]
	if !ok {
		h = &loopHistory{}
		t.sessions[sessionID] = h
	}
	h.tokens = append(h.tokens, respTokens)
	if len(h.tokens) > t.historyMax {
		h.tokens = h.tokens[len(h.tokens)-t.historyMax:]
	}
	h.last = now

	events := t.advanceLocked(sessionID, h, now)
	t.pruneLocked(now)
	t.mu.Unlock()

	t.emit(events)
}

// advanceLocked applies one recorded request to the strike ladder and returns
// the events it produced. Callers must hold t.mu.
func (t *LoopTracker) advanceLocked(sessionID string, h *loopHistory, now time.Time) []LoopPenaltyEvent {
	run := runLocked(h.tokens, t.guard.ToleranceTokens, t.guard.MaxLoopTokens)

	if run < t.guard.RunBar {
		// Below the bar: this is evidence of recovery. Enough of it in a row
		// and the session is forgiven outright.
		h.clearStreak++
		if h.clearStreak < t.guard.ClearRequests || h.strike == 0 {
			return nil
		}
		strike := h.strike
		held := !h.heldUntil.IsZero() || h.heldForever
		h.strike, h.clearStreak = 0, 0
		h.heldUntil, h.heldForever = time.Time{}, false
		events := []LoopPenaltyEvent{}
		if held {
			events = append(events, LoopPenaltyEvent{SessionID: sessionID, Kind: PenaltyEventHoldEnd,
				Reason: PenaltyReasonLoop, Strike: strike, UniformRun: run})
		}
		return events
	}

	// At or above the bar: the run itself names the strike (20 -> 1,
	// 40 -> 2, 60 -> 3), capped at the configured ladder length. A run that
	// merely continues inside the same step changes nothing.
	h.clearStreak = 0
	target := run / t.guard.RunBar
	if target > t.guard.Strikes {
		target = t.guard.Strikes
	}
	if target <= h.strike {
		return nil
	}
	h.strike = target
	hold := t.holdSecondsFor(target)
	typical := medianOfRun(h.tokens, t.guard.ToleranceTokens, t.guard.MaxLoopTokens)
	events := []LoopPenaltyEvent{{SessionID: sessionID, Kind: PenaltyEventStrike,
		Reason: PenaltyReasonLoop, Strike: target, HoldSeconds: hold, UniformRun: run, TypicalTokens: typical}}
	switch {
	case hold < 0:
		h.heldForever, h.heldUntil = true, time.Time{}
	case hold > 0:
		h.heldForever, h.heldUntil = false, now.Add(time.Duration(hold)*time.Second)
	default:
		// Flag only: no hold at all, and an earlier hold is NOT extended by a
		// strike that costs nothing.
		return events
	}
	return append(events, LoopPenaltyEvent{SessionID: sessionID, Kind: PenaltyEventHoldStart,
		Reason: PenaltyReasonLoop, Strike: target, HoldSeconds: hold, UniformRun: run, TypicalTokens: typical})
}

// holdSecondsFor returns strike n's configured cost. A ladder shorter than
// the strike count (rejected by config.load, still possible for a
// hand-built LoopGuard) reads as flag-only rather than panicking.
func (t *LoopTracker) holdSecondsFor(strike int) int {
	if strike < 1 || strike > len(t.guard.PenaltySeconds) {
		return 0
	}
	return t.guard.PenaltySeconds[strike-1]
}

// pruneLocked drops sessions idle for longer than loopSessionTTL. A HELD
// session is exempt: not sending requests is precisely what the hold makes it
// do, so ageing it out would silently un-penalize it. Callers must hold t.mu.
func (t *LoopTracker) pruneLocked(now time.Time) {
	for id, h := range t.sessions {
		if h.heldForever || (!h.heldUntil.IsZero() && now.Before(h.heldUntil)) {
			continue
		}
		if now.Sub(h.last) >= loopSessionTTL {
			delete(t.sessions, id)
		}
	}
}

// Run returns the TRAILING run length: walking back from the NEWEST record,
// how many consecutive records keep (max - min) <= tolerance. Trailing, not
// longest, on purpose - a session that looped earlier and then recovered must
// read as not looping the moment one differently-sized response lands.
func (t *LoopTracker) Run(sessionID string) int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	h, ok := t.sessions[sessionID]
	if !ok {
		return 0
	}
	return runLocked(h.tokens, t.guard.ToleranceTokens, t.guard.MaxLoopTokens)
}

// runLocked is the trailing-run computation over a raw history.
//
// The OUTPUT CEILING is applied here and nowhere else: a record above
// maxTokens is productive work, so the walk STOPS at it without counting it.
// That is what makes a session whose every response is capped at ~1083 tokens
// read as run 0 rather than as a perfect loop (2026-09-20, ef043b3f), and it
// is also why such a response counts toward ClearRequests - advanceLocked
// sees a run below the bar and treats it as evidence of recovery.
func runLocked(tokens []int64, tolerance, maxTokens int64) int {
	if len(tokens) == 0 {
		return 0
	}
	lo, hi := tokens[len(tokens)-1], tokens[len(tokens)-1]
	run := 0
	for i := len(tokens) - 1; i >= 0; i-- {
		v := tokens[i]
		if v > maxTokens {
			break
		}
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
		if hi-lo > tolerance {
			break
		}
		run++
	}
	return run
}

// medianOfRun returns the median of the trailing run - the session's
// "typical" response size, published so a reader can see WHAT it kept
// emitting, not just that it did.
func medianOfRun(tokens []int64, tolerance, maxTokens int64) int64 {
	run := runLocked(tokens, tolerance, maxTokens)
	if run == 0 {
		return 0
	}
	vals := append([]int64(nil), tokens[len(tokens)-run:]...)
	sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
	return vals[len(vals)/2]
}

// Looping reports whether sessionID's trailing run has reached the run bar.
func (t *LoopTracker) Looping(sessionID string) bool {
	if t == nil {
		return false
	}
	return t.Run(sessionID) >= t.guard.RunBar
}

// Held is the scheduler's admission gate: may a request from this session be
// granted right now? A timed hold that has elapsed is released here (lazily -
// there is no timer goroutine), which is why this takes the write lock.
func (t *LoopTracker) Held(sessionID string) (held bool, until time.Time, forever bool) {
	if t == nil || sessionID == "" {
		return false, time.Time{}, false
	}
	now := t.now()
	t.mu.Lock()
	h, ok := t.sessions[sessionID]
	if !ok {
		t.mu.Unlock()
		return false, time.Time{}, false
	}
	events := expireHoldLocked(sessionID, h, now)
	forever = h.heldForever
	held = h.heldForever || !h.heldUntil.IsZero()
	if !h.heldForever {
		until = h.heldUntil
	}
	t.mu.Unlock()
	t.emit(events)
	return held, until, forever
}

// expireHoldLocked releases a timed hold whose deadline has passed, keeping
// the STRIKE (only clearRequests or Unpenalize clears that). Callers must
// hold t.mu.
func expireHoldLocked(sessionID string, h *loopHistory, now time.Time) []LoopPenaltyEvent {
	if h.heldForever || h.heldUntil.IsZero() || now.Before(h.heldUntil) {
		return nil
	}
	h.heldUntil = time.Time{}
	return []LoopPenaltyEvent{{SessionID: sessionID, Kind: PenaltyEventHoldEnd,
		Reason: PenaltyReasonLoop, Strike: h.strike}}
}

// Penalty returns sessionID's published penalty state, or ok=false when the
// session has no strike. Like Held, it expires an elapsed timed hold.
func (t *LoopTracker) Penalty(sessionID string) (PenaltyState, bool) {
	if t == nil || sessionID == "" {
		return PenaltyState{}, false
	}
	now := t.now()
	t.mu.Lock()
	h, ok := t.sessions[sessionID]
	if !ok || h.strike == 0 {
		t.mu.Unlock()
		return PenaltyState{}, false
	}
	events := expireHoldLocked(sessionID, h, now)
	out := PenaltyState{
		Reason:        PenaltyReasonLoop,
		Strike:        h.strike,
		Strikes:       t.guard.Strikes,
		Held:          h.heldForever || !h.heldUntil.IsZero(),
		HeldForever:   h.heldForever,
		UniformRun:    runLocked(h.tokens, t.guard.ToleranceTokens, t.guard.MaxLoopTokens),
		TypicalTokens: medianOfRun(h.tokens, t.guard.ToleranceTokens, t.guard.MaxLoopTokens),
	}
	if !h.heldForever {
		out.Until = h.heldUntil
	}
	t.mu.Unlock()
	t.emit(events)
	return out, true
}

// Unpenalize is the manual escape hatch behind POST
// /api/sessions/{id}/unpenalize (the menu's click, and the ONLY place a human
// is in the chain). It forgets the session ENTIRELY - strikes, hold and
// response history - so the session starts clean rather than one uniform
// response away from its old strike. A no-op for an unknown session; that is
// deliberate, the endpoint answers 200 either way so a click never has to
// race the tracker's own TTL. The source string (e.g. "menu-click") describes
// where the action came from; empty means the caller did not provide one.
func (t *LoopTracker) Unpenalize(sessionID, source, userAgent string) {
	if t == nil || sessionID == "" {
		return
	}
	t.mu.Lock()
	h, ok := t.sessions[sessionID]
	if !ok {
		t.mu.Unlock()
		return
	}
	strike := h.strike
	delete(t.sessions, sessionID)
	t.mu.Unlock()
	t.emit([]LoopPenaltyEvent{{SessionID: sessionID, Kind: PenaltyEventUnpenalize,
		Reason: PenaltyReasonLoop, Strike: strike, Source: source, UserAgent: userAgent}})
}

// HeldSessions returns the session ids with an ACTIVE hold, sorted, for the
// state trace (a held session with no in-flight request is invisible in the
// box array otherwise). Read-only: it does not expire holds, so a merely
// stale line can never be the thing that releases a session.
func (t *LoopTracker) HeldSessions() []string {
	if t == nil {
		return nil
	}
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	for id, h := range t.sessions {
		if h.heldForever || (!h.heldUntil.IsZero() && now.Before(h.heldUntil)) {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// LoopVerdict answers "is the session behind the CURRENT request in a
// degenerate loop?" It is a callback rather than a value because the verdict
// must be read at completion time (the session's history has grown since the
// request started) and because the router cannot import internal/server,
// where the tracker lives - the same import-cycle constraint that
// InflightMetadataSetter documents.
type LoopVerdict func() bool

// PenaltyGate answers "is the session behind the CURRENT request held right
// now?", read by the scheduler at admission and again on every tick while the
// request is parked. Same context-carried-callback shape, and for the same
// import-cycle reason, as LoopVerdict. heldForever distinguishes the -1
// penalty (no timer will release it) from a countdown.
type PenaltyGate func() (held bool, until time.Time, heldForever bool)

type loopVerdictContextKey struct{}

type penaltyGateContextKey struct{}

// WithLoopVerdict tags ctx with verdict.
func WithLoopVerdict(ctx context.Context, verdict LoopVerdict) context.Context {
	return context.WithValue(ctx, loopVerdictContextKey{}, verdict)
}

// LoopVerdictFromContext returns the verdict tagged onto ctx by
// WithLoopVerdict, if any. Absent for requests that never passed through the
// inflight middleware (bare test harnesses, skipped paths) - callers must
// treat that as "unknown, assume productive", never as an error.
func LoopVerdictFromContext(ctx context.Context) (LoopVerdict, bool) {
	verdict, ok := ctx.Value(loopVerdictContextKey{}).(LoopVerdict)
	return verdict, ok
}

// WithPenaltyGate tags ctx with gate.
func WithPenaltyGate(ctx context.Context, gate PenaltyGate) context.Context {
	return context.WithValue(ctx, penaltyGateContextKey{}, gate)
}

// PenaltyGateFromContext returns the gate tagged onto ctx by WithPenaltyGate,
// if any. Absent means "this request cannot be held" - the fail-open answer,
// matching LoopVerdictFromContext.
func PenaltyGateFromContext(ctx context.Context) (PenaltyGate, bool) {
	gate, ok := ctx.Value(penaltyGateContextKey{}).(PenaltyGate)
	return gate, ok
}
