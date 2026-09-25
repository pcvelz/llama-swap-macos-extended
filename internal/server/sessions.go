package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/membrake"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/router/scheduler"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// Session state (fork). The contract is owned by llama-cm:
// docs/intent/session-state-contract.md (llama-swap.sessions/v1). Session
// state is computed ONCE here and every client only renders it: phase,
// context, progress and rate are never derived client-side.
//
// One loop (sessionsHub.run) ticks at 1 Hz. Each tick polls the /slots of
// every READY local child DIRECTLY on the child's own URL (childProxyURL):
// the poll never enters the router or the process proxy, so it cannot move a
// TTL or idle clock, cannot queue a swap and cannot load a model (the § 17
// status-read rule). With no model ready, nothing is polled. The poll result
// is folded with the in-flight entries, the slot-affinity store's hot slots,
// the scheduler's park reasons, the cooldown and the memory brake into ONE
// cached snapshot. GET /api/sessions serves that cache; /api/events pushes
// the same body as a "sessions" event on change, at most once per second.

const sessionsSchema = "llama-swap.sessions/v1"

// sessionsRateWindow is the sliding window rates are measured over.
const sessionsRateWindow = 30 * time.Second

// sessionsIdleRetention is how long a known session stays listed as IDLE
// after it stops holding anything.
const sessionsIdleRetention = 60 * time.Second

// sessionsPollTimeout bounds one child /slots read so a stalled child cannot
// stretch the 1 Hz tick.
const sessionsPollTimeout = 900 * time.Millisecond

const (
	phaseParked  = "PARKED"
	phaseLoading = "LOADING"
	phasePrefill = "PREFILL"
	phaseDecode  = "DECODE"
	phaseHot     = "HOT"
	phaseIdle    = "IDLE"
	// phasePenalized is the loop guard's hold (2026-09-19 phase 2). It
	// REPLACES whatever the row would otherwise be, and applies only while
	// the session is ACTUALLY being held - i.e. it has a request the
	// scheduler parked as `penalized` (2026-09-20 phase 3: with the
	// contention gate, having strikes no longer means being held). A
	// penalized session with no live request keeps its IDLE/HOT row, exempt
	// from the 60s drop, so it stays visible and clickable between retries.
	phasePenalized = "PENALIZED"
)

type sessionsBody struct {
	Schema      string             `json:"schema"`
	GeneratedAt string             `json:"generatedAt"`
	Resident    *sessionsResident  `json:"resident"`
	Queue       sessionsQueue      `json:"queue"`
	Cooldown    *swaputil.Cooldown `json:"cooldown"`
	MemoryBrake membrake.Status    `json:"memoryBrake"`
	Sessions    []sessionEntry     `json:"sessions"`
}

type sessionsResident struct {
	Model  string `json:"model"`
	Alias  string `json:"alias"`
	State  string `json:"state"`
	Window int    `json:"window"`
	Slots  int    `json:"slots"`
}

type sessionsQueue struct {
	Waiting int            `json:"waiting"`
	ByTier  map[string]int `json:"byTier"`
}

type sessionContext struct {
	Used        int `json:"used"`
	Cached      int `json:"cached"`
	Processed   int `json:"processed"`
	Decoded     int `json:"decoded"`
	PromptTotal int `json:"promptTotal"`
	Window      int `json:"window"`
}

type sessionRate struct {
	Kind            *string  `json:"kind"`
	TokensPerSecond *float64 `json:"tokensPerSecond"`
	WindowSeconds   float64  `json:"windowSeconds"`
}

type sessionEntry struct {
	SessionID    string         `json:"sessionId"`
	SessionShort string         `json:"sessionShort"`
	RequestID    *string        `json:"requestId"`
	Model        string         `json:"model"`
	Alias        string         `json:"alias"`
	Tier         string         `json:"tier"`
	Priority     int            `json:"priority"`
	Phase        string         `json:"phase"`
	ParkReason   *string        `json:"parkReason"`
	Slot         *int           `json:"slot"`
	Context      sessionContext `json:"context"`
	Progress     *float64       `json:"progress"`
	Rate         sessionRate    `json:"rate"`
	ElapsedMs    int64          `json:"elapsedMs"`
	PhaseSinceMs int64          `json:"phaseSinceMs"`
	RespTokens   int64          `json:"respTokens"`
	// ParentSessionId / ParentSessionShort carry the dispatching session's id
	// from X-Claude-Code-Parent-Session-Id. When present, a renderer can show
	// which interactive Claude Code session fired a headless dispatch row;
	// absent on rows that are not dispatched children (direct CLI, curl, etc).
	ParentSessionId    string `json:"parentSessionId,omitempty"`
	ParentSessionShort string `json:"parentSessionShort,omitempty"`
	// Looping / UniformRun expose the loop verdict (swaputil.LoopTracker) for
	// this session: UniformRun is the trailing run of near-identical output
	// sizes, Looping whether it cleared the bar. Purely observational - the
	// scheduler reads the tracker itself. Additive to llama-swap.sessions/v1
	// and `omitempty` so a session with no run (every session on a box that
	// has served nothing, and every golden fixture) serialises exactly as it
	// did before (2026-09-19).
	Looping    bool `json:"looping,omitempty"`
	UniformRun int  `json:"uniformRun,omitempty"`
	// Penalty is the loop guard's strike/hold for this session, omitted when
	// it has none. Present even for a strike that carries NO hold (the phase
	// is then unchanged): the strike is real state an operator must be able
	// to see before it escalates.
	Penalty *sessionPenalty `json:"penalty,omitempty"`
}

// sessionPenalty is the published penalty. RemainingSeconds is a POINTER so
// a hold with no deadline ("held until un-penalized") serialises as JSON
// null - the one state a countdown cannot express.
type sessionPenalty struct {
	Reason           string `json:"reason"`
	Strike           int    `json:"strike"`
	Strikes          int    `json:"strikes"`
	RemainingSeconds *int   `json:"remainingSeconds"`
	UniformRun       int    `json:"uniformRun"`
	TypicalTokens    int64  `json:"typicalTokens"`
	// Held reports that the session is being held RIGHT NOW, as opposed to
	// merely carrying strikes. The two came apart in phase 3: with the
	// contention gate a session inside a penalty window is admitted normally
	// whenever nobody else is waiting (2026-09-20), so "has strikes" no
	// longer implies "is being held" and a client must be able to tell them
	// apart.
	Held bool `json:"held"`
}

// childSlot is one child /slots row, only the fields the contract folds.
type childSlot struct {
	ID                     int
	NCtx                   int
	IsProcessing           bool
	NPromptTokens          int
	NPromptTokensCache     int
	NPromptTokensProcessed int
	NDecoded               int
}

// parseChildSlots decodes a llama-server /slots body, tolerating unknown
// fields, the bare-array form, next_token as an object or a one-element
// array, and a missing n_prompt_tokens_cache (older builds: 0).
func parseChildSlots(body []byte) ([]childSlot, bool) {
	type rawSlot struct {
		ID                     int             `json:"id"`
		NCtx                   int             `json:"n_ctx"`
		IsProcessing           bool            `json:"is_processing"`
		NPromptTokens          int             `json:"n_prompt_tokens"`
		NPromptTokensCache     int             `json:"n_prompt_tokens_cache"`
		NPromptTokensProcessed int             `json:"n_prompt_tokens_processed"`
		NextToken              json.RawMessage `json:"next_token"`
	}
	var wrap struct {
		Slots []rawSlot `json:"slots"`
	}
	var slots []rawSlot
	if err := json.Unmarshal(body, &wrap); err == nil && wrap.Slots != nil {
		slots = wrap.Slots
	} else if err := json.Unmarshal(body, &slots); err != nil || slots == nil {
		return nil, false
	}
	type nextToken struct {
		NDecoded int `json:"n_decoded"`
	}
	out := make([]childSlot, len(slots))
	for i, sl := range slots {
		out[i] = childSlot{
			ID:                     sl.ID,
			NCtx:                   sl.NCtx,
			IsProcessing:           sl.IsProcessing,
			NPromptTokens:          sl.NPromptTokens,
			NPromptTokensCache:     sl.NPromptTokensCache,
			NPromptTokensProcessed: sl.NPromptTokensProcessed,
		}
		var arr []nextToken
		var one nextToken
		if json.Unmarshal(sl.NextToken, &arr) == nil && len(arr) > 0 {
			out[i].NDecoded = arr[0].NDecoded
		} else if json.Unmarshal(sl.NextToken, &one) == nil {
			out[i].NDecoded = one.NDecoded
		}
	}
	return out, true
}

// sessionsInput is everything one build folds. Injected so the builder is a
// pure fold that tests drive with fixed inputs.
type sessionsInput struct {
	Now         time.Time
	Running     map[string]process.ProcessState
	Requests    []swaputil.InflightRequestEntry
	Slots       map[string][]childSlot
	Hot         map[string][]swaputil.HotSlot
	Cooldown    *swaputil.Cooldown
	MemoryBrake membrake.Status
	// Loops is the box's loop tracker, read (never written) to stamp the
	// looping/uniformRun fields onto every row that has a session id. nil in
	// tests that drive the fold directly, which then leaves both fields at
	// their zero values.
	Loops *swaputil.LoopTracker
}

type phaseMark struct {
	phase string
	since time.Time
}

type rateSample struct {
	at        time.Time
	requestID string
	kind      string
	processed int
	decoded   int
}

type idleMark struct {
	since time.Time
	entry sessionEntry
}

// sessionsBuilder holds the cross-tick memory a single snapshot cannot carry:
// when each row entered its phase, the rate samples, and the known sessions
// for IDLE retention. Not safe for concurrent use; sessionsHub serializes.
type sessionsBuilder struct {
	cfg    config.Config
	phases map[string]phaseMark
	rates  map[string][]rateSample
	known  map[string]sessionEntry
	idle   map[string]idleMark
}

func newSessionsBuilder(cfg config.Config) *sessionsBuilder {
	return &sessionsBuilder{
		cfg:    cfg,
		phases: map[string]phaseMark{},
		rates:  map[string][]rateSample{},
		known:  map[string]sessionEntry{},
		idle:   map[string]idleMark{},
	}
}

func (b *sessionsBuilder) alias(model string) string {
	if mc, ok := b.cfg.Models[model]; ok && len(mc.Aliases) > 0 {
		return mc.Aliases[0]
	}
	return ""
}

func (b *sessionsBuilder) tierRank(tier string) int {
	if tc, ok := b.cfg.Tiers[tier]; ok {
		return tc.Rank
	}
	return 0
}

func shortOf(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func strPtr(s string) *string { return &s }

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

func residentStateName(st process.ProcessState) string {
	switch st {
	case process.StateReady:
		return "ready"
	case process.StateStarting:
		return "loading"
	case process.StateStopping:
		return "stopping"
	}
	return string(st)
}

// pickResident chooses the loaded local model: ready first, then loading,
// then stopping; ties by id so the choice is stable.
func pickResident(running map[string]process.ProcessState) (string, process.ProcessState) {
	order := map[process.ProcessState]int{process.StateReady: 0, process.StateStarting: 1, process.StateStopping: 2}
	best, bestState, bestRank := "", process.ProcessState(""), 99
	for id, st := range running {
		r, ok := order[st]
		if !ok {
			continue
		}
		if r < bestRank || (r == bestRank && id < best) {
			best, bestState, bestRank = id, st, r
		}
	}
	return best, bestState
}

func (b *sessionsBuilder) window(in sessionsInput, model string) int {
	for _, sl := range in.Slots[model] {
		if sl.NCtx > 0 {
			return sl.NCtx
		}
	}
	return 0
}

func findSlot(slots []childSlot, id int) (childSlot, bool) {
	for _, sl := range slots {
		if sl.ID == id {
			return sl, true
		}
	}
	return childSlot{}, false
}

// requestRank orders the in-flight requests of one session: the request on
// a slot represents the session over a loading one over a parked one.
func requestRank(m map[string]string) int {
	if m["slot_granted"] == "1" && m["kv_parked"] != "1" {
		return 0
	}
	return 1
}

func (b *sessionsBuilder) build(in sessionsInput) sessionsBody {
	now := in.Now
	body := sessionsBody{
		Schema:      sessionsSchema,
		GeneratedAt: now.Format("2006-01-02T15:04:05.000Z07:00"),
		Cooldown:    in.Cooldown,
		MemoryBrake: in.MemoryBrake,
		Sessions:    []sessionEntry{},
		Queue:       sessionsQueue{ByTier: map[string]int{swaputil.DefaultTier.Name: 0}},
	}
	for name := range b.cfg.Tiers {
		body.Queue.ByTier[name] = 0
	}

	if id, st := pickResident(in.Running); id != "" {
		n := b.cfg.Models[id].ConcurrencyLimit
		if n <= 0 {
			n = len(in.Slots[id])
		}
		body.Resident = &sessionsResident{Model: id, Alias: b.alias(id), State: residentStateName(st), Window: b.window(in, id), Slots: n}
	}

	// One row per session: the most advanced request represents it, then
	// the oldest. A request without a session is its own row.
	chosen := map[string]swaputil.InflightRequestEntry{}
	var order []string
	for _, r := range in.Requests {
		key := "req:" + r.ID
		if sid := r.Metadata["session_id"]; sid != "" {
			key = sid
		}
		cur, ok := chosen[key]
		if !ok {
			order = append(order, key)
			chosen[key] = r
			continue
		}
		ra, rb := requestRank(r.Metadata), requestRank(cur.Metadata)
		if ra < rb || (ra == rb && r.Timestamp.Before(cur.Timestamp)) {
			chosen[key] = r
		}
	}

	held := heldNow(in)
	present := map[string]bool{}
	for _, key := range order {
		r := chosen[key]
		row := b.requestRow(in, r)
		// Before stampPhaseSince, so a held row's phase clock measures the
		// PENALIZED phase rather than the phase it was displaced from.
		b.applyLoopState(in, &row, held)
		b.stampPhaseSince(key, &row, now, r.Timestamp)
		b.stampRate(key, &row, now, in, r)
		present[key] = true
		body.Sessions = append(body.Sessions, row)
	}

	// HOT: a session with no request that still owns a slot and its cache.
	for model, hots := range in.Hot {
		for _, h := range hots {
			if h.SessionID == "" || present[h.SessionID] {
				continue
			}
			row := b.hotRow(in, model, h)
			b.applyLoopState(in, &row, held)
			present[h.SessionID] = true
			delete(b.phases, h.SessionID)
			delete(b.rates, h.SessionID)
			body.Sessions = append(body.Sessions, row)
		}
	}

	// IDLE retention for known sessions that hold nothing any more.
	for key, e := range b.known {
		if !present[key] {
			if _, ok := b.idle[key]; !ok {
				b.idle[key] = idleMark{since: now, entry: e}
			}
		}
	}
	b.known = map[string]sessionEntry{}
	for _, row := range body.Sessions {
		if row.SessionID != "" {
			b.known[row.SessionID] = row
			delete(b.idle, row.SessionID)
		}
	}
	for key, m := range b.idle {
		// A PENALIZED session is exempt from the 60s drop: a hold stops it
		// sending requests, and the row has to stay visible (and clickable,
		// for un-penalize) between the client's retries. Keyed on having a
		// strike rather than on being held right now, so the row does not
		// blink out every time the contention gate admits it. Everything else
		// ages out as before.
		if now.Sub(m.since) >= sessionsIdleRetention && !b.sessionPenalized(in, key) {
			delete(b.idle, key)
			continue
		}
		row := b.idleRow(in, m, now)
		b.applyLoopState(in, &row, held)
		body.Sessions = append(body.Sessions, row)
	}

	// Drop per-key memory for rows that are gone.
	for key := range b.phases {
		if !present[key] {
			delete(b.phases, key)
		}
	}
	for key := range b.rates {
		if !present[key] {
			delete(b.rates, key)
		}
	}

	// Invariant 2 (waiting == count(PARKED)) is counted AFTER applyLoopState
	// has replaced a held row's phase with PENALIZED, so a penalized row is
	// never counted as waiting - it is not in the queue at all (the scheduler
	// keeps it in a separate list, see FIFO.penalized).
	for _, row := range body.Sessions {
		if row.Phase == phaseParked {
			body.Queue.Waiting++
			body.Queue.ByTier[row.Tier]++
		}
	}
	sort.SliceStable(body.Sessions, func(i, j int) bool {
		a, c := body.Sessions[i], body.Sessions[j]
		if a.Priority != c.Priority {
			return a.Priority > c.Priority
		}
		if a.ElapsedMs != c.ElapsedMs {
			return a.ElapsedMs > c.ElapsedMs
		}
		return a.SessionShort < c.SessionShort
	})
	return body
}

// heldNow is the set of sessions the scheduler is ACTUALLY holding right now:
// those with an in-flight request parked as `penalized`.
//
// The tracker alone cannot answer this since phase 3. With the contention
// gate a session inside a penalty window is admitted normally whenever nobody
// else is waiting (2026-09-20), so tracker.Held is "is inside a penalty
// window", while the park reason on a live request is the scheduler's own
// record of a request it actually held. The park reason is therefore the
// authority for the PENALIZED phase, and the thing that stops the menu
// showing a session as held while it is happily decoding.
func heldNow(in sessionsInput) map[string]bool {
	out := map[string]bool{}
	for _, r := range in.Requests {
		if sid := r.Metadata["session_id"]; sid != "" && r.Metadata["park_reason"] == scheduler.ParkPenalized {
			out[sid] = true
		}
	}
	return out
}

// sessionPenalized reports whether sessionID carries any strike - the
// condition for keeping its row alive past the IDLE drop, so the row is still
// there (and clickable) when the operator goes looking.
func (b *sessionsBuilder) sessionPenalized(in sessionsInput, sessionID string) bool {
	if in.Loops == nil || sessionID == "" {
		return false
	}
	_, ok := in.Loops.Penalty(sessionID)
	return ok
}

// applyLoopState stamps the loop verdict and any penalty onto one row, and
// replaces the phase with PENALIZED while the session is held. Stamped on
// EVERY row with a session id whatever its phase: a looping or penalized
// session is just as worth seeing while it is PARKED or IDLE as mid-DECODE
// (2026-09-19).
func (b *sessionsBuilder) applyLoopState(in sessionsInput, row *sessionEntry, held map[string]bool) {
	if in.Loops == nil || row.SessionID == "" {
		return
	}
	row.UniformRun = in.Loops.Run(row.SessionID)
	row.Looping = in.Loops.Looping(row.SessionID)

	p, ok := in.Loops.Penalty(row.SessionID)
	if !ok {
		return
	}
	entry := &sessionPenalty{
		Reason:        p.Reason,
		Strike:        p.Strike,
		Strikes:       p.Strikes,
		UniformRun:    p.UniformRun,
		TypicalTokens: p.TypicalTokens,
		Held:          held[row.SessionID],
	}
	// remainingSeconds stays JSON null for a hold with no deadline; a strike
	// that carries no hold reports 0, which is exactly what it costs.
	if !p.HeldForever {
		remaining := 0
		if !p.Until.IsZero() {
			if d := p.Until.Sub(in.Now); d > 0 {
				remaining = int(d.Round(time.Second) / time.Second)
			}
		}
		entry.RemainingSeconds = &remaining
	}
	row.Penalty = entry
	// PENALIZED only while the session is ACTUALLY being held. A session that
	// has strikes but is being served (nobody else waiting, see the
	// contention gate) keeps its real phase and simply carries the penalty
	// object with held:false.
	if entry.Held {
		row.Phase = phasePenalized
		// A penalized row is not parked behind anything in the queue, so the
		// park reason it may have carried would be a lie.
		row.ParkReason = nil
	}
}

func (b *sessionsBuilder) baseRow(model, sessionID, tier string) sessionEntry {
	if tier == "" {
		tier = swaputil.DefaultTier.Name
	}
	return sessionEntry{
		SessionID:    sessionID,
		SessionShort: shortOf(sessionID),
		Model:        model,
		Alias:        b.alias(model),
		Tier:         tier,
		Priority:     b.tierRank(tier),
		Rate:         sessionRate{WindowSeconds: sessionsRateWindow.Seconds()},
	}
}

func (b *sessionsBuilder) requestRow(in sessionsInput, r swaputil.InflightRequestEntry) sessionEntry {
	m := r.Metadata
	row := b.baseRow(r.Model, m["session_id"], m["tier"])
	row.RequestID = strPtr(r.ID)
	if row.SessionShort == "" {
		row.SessionShort = shortOf(r.ID)
	}
	row.ElapsedMs = in.Now.Sub(r.Timestamp).Milliseconds()
	if row.ElapsedMs < 0 {
		row.ElapsedMs = 0
	}
	row.RespTokens = r.RespTokens
	row.Context.Window = b.window(in, r.Model)

	if pid := m["parent_session_id"]; pid != "" {
		row.ParentSessionId = pid
		row.ParentSessionShort = shortOf(pid)
	}

	if requestRank(m) != 0 {
		row.Phase = phaseParked
		reason := m["park_reason"]
		if reason == "" && m["kv_parked"] == "1" {
			reason = "kv"
		}
		if reason != "" {
			row.ParkReason = strPtr(reason)
		}
		return row
	}
	if in.Running[r.Model] != process.StateReady {
		row.Phase = phaseLoading
		return row
	}
	row.Phase = phasePrefill
	slotStr := m["slot_id"]
	if slotStr == "" {
		slotStr = m[slotAffinityMetadataKey]
	}
	if n, err := strconv.Atoi(slotStr); err == nil {
		row.Slot = &n
		if sl, ok := findSlot(in.Slots[r.Model], n); ok {
			if sl.NCtx > 0 {
				row.Context.Window = sl.NCtx
			}
			// A slot not processing still shows its PREVIOUS request's
			// counters: they do not belong to this request yet.
			if sl.IsProcessing {
				row.Context.Cached = sl.NPromptTokensCache
				row.Context.Processed = sl.NPromptTokensProcessed
				row.Context.Decoded = sl.NDecoded
				row.Context.PromptTotal = sl.NPromptTokens
			}
		}
	}
	// n_prompt_tokens only counts what the child has reached so far (cache
	// included), so it reads ~100% all through a prefill. The router's
	// arrival-time estimate of the whole prompt (est_tokens) is the real
	// denominator; the child's count wins once it has passed the estimate.
	if est, err := strconv.Atoi(m["est_tokens"]); err == nil && est > row.Context.PromptTotal {
		row.Context.PromptTotal = est
	}
	c := &row.Context
	c.Used = c.Cached + c.Processed + c.Decoded
	if c.Decoded > 0 {
		row.Phase = phaseDecode
	} else if c.PromptTotal > 0 {
		p := round(float64(c.Cached+c.Processed)/float64(c.PromptTotal), 4)
		if p > 1 {
			p = 1
		}
		row.Progress = &p
	}
	return row
}

// hotRow: the slot's KV cache holds the session's last prompt, so the whole
// prompt reads as cached (contract fixture decode-parked-hot.json).
func (b *sessionsBuilder) hotRow(in sessionsInput, model string, h swaputil.HotSlot) sessionEntry {
	tier := ""
	if prev, ok := b.known[h.SessionID]; ok {
		tier = prev.Tier
	}
	row := b.baseRow(model, h.SessionID, tier)
	row.Phase = phaseHot
	slot := h.Slot
	row.Slot = &slot
	row.PhaseSinceMs = int64(h.IdleSeconds) * 1000
	row.Context.Window = b.window(in, model)
	if sl, ok := findSlot(in.Slots[model], h.Slot); ok {
		if sl.NCtx > 0 {
			row.Context.Window = sl.NCtx
		}
		row.Context.Cached = sl.NPromptTokens
		row.Context.PromptTotal = sl.NPromptTokens
		row.Context.Used = sl.NPromptTokens
	}
	return row
}

func (b *sessionsBuilder) idleRow(in sessionsInput, m idleMark, now time.Time) sessionEntry {
	row := b.baseRow(m.entry.Model, m.entry.SessionID, m.entry.Tier)
	row.Phase = phaseIdle
	row.PhaseSinceMs = now.Sub(m.since).Milliseconds()
	row.Context.Window = b.window(in, m.entry.Model)
	return row
}

func (b *sessionsBuilder) stampPhaseSince(key string, row *sessionEntry, now, arrival time.Time) {
	mark, ok := b.phases[key]
	switch {
	case !ok:
		mark = phaseMark{phase: row.Phase, since: arrival}
	case mark.phase != row.Phase:
		mark = phaseMark{phase: row.Phase, since: now}
	}
	b.phases[key] = mark
	row.PhaseSinceMs = now.Sub(mark.since).Milliseconds()
	if row.PhaseSinceMs < 0 {
		row.PhaseSinceMs = 0
	}
}

// stampRate measures PREFILL on processed and DECODE on decoded, over the
// sliding window. The history restarts on a new request, a phase change or
// a counter going backwards, so a reset can never read negative or spike.
func (b *sessionsBuilder) stampRate(key string, row *sessionEntry, now time.Time, in sessionsInput, r swaputil.InflightRequestEntry) {
	var kind string
	switch row.Phase {
	case phasePrefill:
		kind = "prefill"
	case phaseDecode:
		kind = "decode"
	default:
		delete(b.rates, key)
		return
	}
	if row.Slot == nil || row.Context.PromptTotal == 0 && row.Context.Decoded == 0 {
		delete(b.rates, key)
		return
	}
	row.Rate.Kind = strPtr(kind)
	s := rateSample{at: now, requestID: r.ID, kind: kind, processed: row.Context.Processed, decoded: row.Context.Decoded}
	hist := b.rates[key]
	if n := len(hist); n > 0 {
		last := hist[n-1]
		if last.requestID != s.requestID || last.kind != s.kind || s.processed < last.processed || s.decoded < last.decoded {
			hist = nil
		} else if !s.at.After(last.at) {
			hist = hist[:n-1]
		}
	}
	hist = append(hist, s)
	cutoff := now.Add(-sessionsRateWindow)
	for len(hist) > 2 && hist[0].at.Before(cutoff) {
		hist = hist[1:]
	}
	if len(hist) > 1 && hist[0].at.Before(cutoff) {
		hist = hist[1:]
	}
	b.rates[key] = hist
	if len(hist) < 2 {
		return
	}
	first := hist[0]
	dt := s.at.Sub(first.at).Seconds()
	if dt <= 0 {
		return
	}
	delta := s.processed - first.processed
	if kind == "decode" {
		delta = s.decoded - first.decoded
	}
	v := round(float64(delta)/dt, 1)
	row.Rate.TokensPerSecond = &v
}

// sessionsThrottle pushes a body when it changed (generatedAt ignored) and
// no push happened in the last minInterval.
type sessionsThrottle struct {
	minInterval time.Duration
	lastAt      time.Time
	lastKey     string
	sent        bool
}

func sessionsChangeKey(b sessionsBody) string {
	b.GeneratedAt = ""
	raw, _ := json.Marshal(b)
	return string(raw)
}

func (t *sessionsThrottle) offer(now time.Time, b sessionsBody, emit func(sessionsBody)) {
	key := sessionsChangeKey(b)
	if t.sent && key == t.lastKey {
		return
	}
	if t.sent && now.Sub(t.lastAt) < t.minInterval {
		return
	}
	t.sent, t.lastAt, t.lastKey = true, now, key
	emit(b)
}

// SessionsEvent carries a new session-state snapshot to /api/events.
type SessionsEvent struct {
	Body sessionsBody
}

func (e SessionsEvent) Type() uint32 { return swaputil.SessionsEventID }

// sessionsHub owns the builder, the 1 Hz loop and the cached snapshot.
type sessionsHub struct {
	s        *Server
	mu       sync.Mutex
	builder  *sessionsBuilder
	throttle *sessionsThrottle
	cur      atomic.Pointer[sessionsBody]
	lastPoll map[string][]childSlot
}

func newSessionsHub(s *Server) *sessionsHub {
	return &sessionsHub{
		s:        s,
		builder:  newSessionsBuilder(s.cfg),
		throttle: &sessionsThrottle{minInterval: time.Second},
	}
}

func (h *sessionsHub) run(ctx context.Context) {
	h.tick(time.Now(), true)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			h.tick(now, true)
		}
	}
}

// snapshot is the cached body; a zero body with the schema before the first
// tick.
func (h *sessionsHub) snapshot() sessionsBody {
	if b := h.cur.Load(); b != nil {
		return *b
	}
	return sessionsBody{}
}

// pollSlots reads /slots from every READY local child in parallel, directly
// on the child's own URL. Nothing is read when no model is ready.
func (h *sessionsHub) pollSlots(running map[string]process.ProcessState) map[string][]childSlot {
	out := map[string][]childSlot{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for id, st := range running {
		if st != process.StateReady {
			continue
		}
		url := childProxyURL(h.s.cfg, id)
		if url == "" {
			continue
		}
		wg.Add(1)
		go func(id, url string) {
			defer wg.Done()
			slots, err := fetchChildSlots(url)
			if err == nil {
				observeSlots(h.s.local, id, slots)
			}
			if err != nil {
				// A busy child answers /slots only between batches, so a
				// long prefill ubatch outlasts the timeout. Keep the last
				// good reading: an empty one would show the session as
				// cached=0 used=0 and spike the rate on the next hit.
				prev, ok := h.lastPoll[id]
				if !ok {
					return
				}
				slots = prev
			}
			mu.Lock()
			out[id] = slots
			mu.Unlock()
		}(id, url)
	}
	wg.Wait()
	return out
}

// slotObserver is the router's slot table (internal/router/slotbind.go): it
// binds each granted request to an upstream slot of its own and needs the
// child's own busy map to keep the cap on the upstream's reality.
type slotObserver interface {
	ObserveSlots(modelID string, busy []bool)
}

// observeSlots hands one FRESH reading (the child answered) to the router.
// A kept-over reading is never passed: a busy child answers /slots only
// between batches, and a stale "idle" would start the phantom clock on a
// slot that is in fact working.
func observeSlots(local any, model string, slots []childSlot) {
	obs, ok := local.(slotObserver)
	if !ok {
		return
	}
	n := 0
	for _, sl := range slots {
		if sl.ID+1 > n {
			n = sl.ID + 1
		}
	}
	busy := make([]bool, n)
	for _, sl := range slots {
		if sl.ID >= 0 {
			busy[sl.ID] = sl.IsProcessing
		}
	}
	obs.ObserveSlots(model, busy)
}

func fetchChildSlots(base string) ([]childSlot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), sessionsPollTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/slots", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("/slots -> HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	slots, ok := parseChildSlots(raw)
	if !ok {
		return nil, fmt.Errorf("unparseable /slots response")
	}
	return slots, nil
}

// tick builds one snapshot. poll=false reuses the last poll (no child read).
func (h *sessionsHub) tick(now time.Time, poll bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.s
	running := s.local.RunningModels()
	if poll || h.lastPoll == nil {
		if poll {
			h.lastPoll = h.pollSlots(running)
		} else {
			h.lastPoll = map[string][]childSlot{}
		}
	}
	hot := map[string][]swaputil.HotSlot{}
	if s.slotAffinity != nil {
		for id := range running {
			if mc, ok := s.cfg.Models[id]; ok && mc.ConcurrencyLimit > 0 && s.slotAffinity.enabled(id) {
				hot[id] = s.slotAffinity.hotSlots(id, mc.ConcurrencyLimit)
			}
		}
	}
	body := h.builder.build(sessionsInput{
		Now:         now,
		Running:     running,
		Requests:    s.inflight.Current().Requests,
		Slots:       h.lastPoll,
		Hot:         hot,
		Cooldown:    s.currentCooldown(),
		MemoryBrake: membrake.CurrentStatus(),
		Loops:       s.inflight.loops,
	})
	h.cur.Store(&body)
	if s.debugHistory != nil {
		s.debugHistory.record(now, body)
	}
	h.throttle.offer(now, body, func(b sessionsBody) { event.Emit(SessionsEvent{Body: b}) })
}

// handleAPISessions serves the cached session-state snapshot. It never
// proxies to a child: the 1 Hz loop is the only /slots reader.
func (s *Server) handleAPISessions(w http.ResponseWriter, r *http.Request) {
	if s.sessions == nil || s.sessions.cur.Load() == nil {
		swaputil.SendResponse(w, r, http.StatusServiceUnavailable, "session state not built yet")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.sessions.snapshot())
}
