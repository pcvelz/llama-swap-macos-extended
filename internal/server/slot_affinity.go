package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Slot affinity (fork, per-model opt-in `slotAffinity: true`).
//
// llama.cpp picks the slot for a new request by longest-common-prefix over
// the slots' cached prompts (or by LRU when nothing matches). With several
// Claude Code sessions sharing one child, that heuristic can move a session
// to a different slot mid-conversation, throwing away the KV prefix the
// previous slot still holds and re-prefilling from scratch. llama.cpp honours
// an explicit `id_slot` in the request body, so the proxy can pin a session
// to a slot:
//
//   - ASSIGN (primary mechanism): every Claude Code session talks to the
//     child over /v1/messages, and llama-server's Anthropic-format
//     serializers (to_json_anthropic, to_json_anthropic_stream) never emit
//     id_slot in the response - so on that path nothing is ever learned from
//     the child, and a learn-only design is inert for the traffic this
//     feature exists for. Instead, CreateSlotAffinityMiddleware assigns a
//     slot itself on a session's first full-size request: least-loaded among
//     the slots with any session active in the last
//     slotAffinityActiveWindow, ties to the lowest slot id. concurrencyLimit
//     is the slot count.
//   - LEARN (refinement only): metrics.record() still parses the child's
//     `id_slot` from the response where the format carries it (fork commit
//     b44aae2, activity entries carry slot_id) - true today for OpenAI-format
//     paths. When the entry also carries a session_id (from metadata.user_id)
//     and the model opted in, the pair is remembered in slotAffinityStore,
//     refining or confirming whatever assign() picked.
//   - INJECT: CreateSlotAffinityMiddleware runs after the filters and before
//     metrics. For an opted-in model it looks up a learned slot for the
//     request's session first, falling back to assign() when nothing was
//     learned yet; either way it sets `id_slot` in the JSON body sent
//     upstream and stamps `slot_affinity` on the request metadata so the
//     activity entry shows what was injected.
//
// Policies, all deliberate:
//   - Busy slot: always inject the learned slot. llama.cpp queues the request
//     on a busy slot; no /slots probe per request (an extra round trip and a
//     race for nothing).
//   - Client-supplied id_slot: OVERRIDDEN once a slot has been learned.
//     Behind a proxy the client cannot know the child's slot layout, so a
//     client value is at best stale. Before anything is learned the client's
//     value passes through untouched.
//   - Bounded: at most slotAffinityMaxSessions sessions per model; the
//     least-recently-seen entry is evicted on overflow.
//   - Eviction on model stop: a ProcessStateChangeEvent into "stopped" (swap,
//     unload, crash) drops the model's whole map - the new process has fresh,
//     empty slots and any old slot number would be a lie.
//   - Peers and /upstream passthrough are never touched.
//   - Small-request exclusion: a Claude Code session reuses the same
//     session_id for its full turns (>= ~12k input tokens, request body
//     >= ~50 KB, carrying the whole system prompt and tool schema) and for
//     tiny housekeeping calls on the same session (~300-800 input tokens, a
//     few KB body). A housekeeping call must neither LEARN nor INJECT: if it
//     learned, the next full turn would be pinned to a slot sized for a
//     housekeeping-sized KV cache; if it injected, it would land on the full
//     turn's slot and truncate that slot's KV cache. slotAffinityMinLearnInputTokens
//     (2048 input tokens) and slotAffinityMinInjectBodyBytes (16384 body
//     bytes) each sit strictly between the two populations, so every
//     housekeeping call falls below both thresholds and every full turn
//     clears them, with headroom on both sides.

// slotAffinityMaxSessions bounds the per-model session map. Claude Code
// sessions on one box number in the tens; 256 leaves a wide margin without
// letting a scripted client with rotating session ids grow the map forever.
const slotAffinityMaxSessions = 256

// slotAffinityMetadataKey is the request-metadata key stamped with the
// injected slot, so the activity entry records "llama-swap asked for slot N".
const slotAffinityMetadataKey = "slot_affinity"

// slotAffinityMinLearnInputTokens is the minimum InputTokens a finished
// response needs before its (session, slot) pair is learned. Claude Code
// housekeeping calls run ~300-800 input tokens; full turns run >= ~12k. 2048
// sits well above the housekeeping ceiling and well below the smallest full
// turn, so only full turns teach the store a slot.
const slotAffinityMinLearnInputTokens = 2048

// slotAffinityMinInjectBodyBytes is the minimum request body size, in bytes,
// before a learned slot is injected. Housekeeping bodies run a few KB; full
// turns run >= ~50 KB once the system prompt and tool schema are included.
// 16384 sits between the two, so a housekeeping call passes through
// untouched instead of stealing the full turn's slot.
const slotAffinityMinInjectBodyBytes = 16384

// slotAffinityActiveWindow bounds how far back assign() looks when counting
// each slot's current load: a session whose entry is older than this no
// longer counts as occupying its slot, so a long-finished conversation
// cannot permanently skew the least-loaded pick.
const slotAffinityActiveWindow = 15 * time.Minute

// slotAffinityLiveWindow is how long after its last request started or ended
// a session still counts as LIVE for slot placement. A Claude Code tool loop
// is silent on the proxy only while the client runs a tool: seconds for a
// Read/Bash call, a couple of minutes for a build or a test run. Beyond that
// the session is paused (the user is reading, a question is open) and its
// slot may go to a session that is actually working - but only when no slot
// is free (assign/repair below prefer a slot with no live session at all).
// The 15-min activeWindow is kept for what it was built for (hotSlots, the
// cooldown's per-slot display); it is far too long to decide placement: it
// counted a session silent for 4 minutes as occupying its slot, tied it with
// a live one and put a newcomer on the live session's slot (incident llama-cm
// 2026-09-16-two-live-sessions-pinned-same-slot-cache-thrash).
const slotAffinityLiveWindow = 2 * time.Minute

type slotAffinityEntry struct {
	slot int
	seen time.Time
	// assigned is when this session was first placed on its current slot.
	// Repair moves the NEWER of two live sessions sharing a slot, so the
	// longer conversation - usually the bigger KV prefix - keeps its cache.
	assigned time.Time
	// inflight counts this lane's full-turn requests currently being served
	// through the middleware; lastEnd is when the last one finished. A
	// 5-minute decode must count as live although seen (its start) is old.
	inflight int
	lastEnd  time.Time
}

// liveLocked reports whether e's session is working right now: a request in
// flight, or one started or finished within liveWindow. Called with mu held.
func (s *slotAffinityStore) liveLocked(e slotAffinityEntry, now time.Time) bool {
	if e.inflight > 0 {
		return true
	}
	last := e.seen
	if e.lastEnd.After(last) {
		last = e.lastEnd
	}
	return now.Sub(last) <= s.liveWindow
}

// slotLoadLocked describes one slot for placement: how many OTHER sessions
// (not exclude) are live on it, and when any session last used it (zero =
// never). Called with mu held.
func (s *slotAffinityStore) slotLoadLocked(sessions map[string]slotAffinityEntry, exclude string, nSlots int, now time.Time) (live []int, lastUsed []time.Time) {
	live = make([]int, nSlots)
	lastUsed = make([]time.Time, nSlots)
	for key, e := range sessions {
		if e.slot < 0 || e.slot >= nSlots {
			continue
		}
		last := e.seen
		if e.lastEnd.After(last) {
			last = e.lastEnd
		}
		if last.After(lastUsed[e.slot]) {
			lastUsed[e.slot] = last
		}
		if key != exclude && s.liveLocked(e, now) {
			live[e.slot]++
		}
	}
	return live, lastUsed
}

// pickSlotLocked chooses the placement for a session: the fewest live
// sessions first; among equals the slot used longest ago (a never-used slot
// first), because the most recently used slot most likely still holds a
// conversation that will be back; remaining ties to the lowest id. Called
// with mu held.
func pickSlotLocked(live []int, lastUsed []time.Time) int {
	best := 0
	for id := 1; id < len(live); id++ {
		switch {
		case live[id] < live[best]:
			best = id
		case live[id] == live[best] && lastUsed[id].Before(lastUsed[best]):
			best = id
		}
	}
	return best
}

// slotAffinityStore remembers, per model, which slot last served each
// session. Safe for concurrent use.
type slotAffinityStore struct {
	mu                  sync.Mutex
	cfg                 config.Config
	maxSessions         int
	minLearnInputTokens int
	minInjectBodyBytes  int
	activeWindow        time.Duration
	liveWindow          time.Duration
	byModel             map[string]map[string]slotAffinityEntry
	now                 func() time.Time
	unsubscribe         context.CancelFunc
	// resolveResident maps a resident-alias id (claude-*, default) to the
	// model that will serve it. The middleware runs BEFORE localPeerHandler
	// resolves the alias, so without this seam a Claude Code subagent's
	// turn - which always arrives under claude-haiku-* - is "not a model
	// block" here and gets no lane at all; it then lands on whichever slot
	// llama.cpp picks, typically its parent's, and truncates that KV cache
	// (incident llama-cm 2026-09-08-subagent-turns-invisible-share-parent-
	// slot-lane). Wired by the server to resolveResidentAlias; nil in unit
	// tests that do not exercise aliases.
	resolveResident func(requested string) (string, bool)
}

// resolvedModelMetadataKey names the model that actually serves a
// resident-alias request. The in-flight entry keeps the id the caller asked
// for (that is what the row must show as "what was requested"), but a
// renderer polling /upstream/<model>/slots needs the real model: the alias
// has no upstream route and 404s (witnessed as the menu's slot readouts
// vanishing whenever only a subagent turn was in flight).
const resolvedModelMetadataKey = "resolved_model"

// affinityLaneKey is the identity a slot lane is keyed on. A Claude Code
// subagent reuses its parent's session id (metadata.session_id) and adds
// metadata.agent_id; its full turns run concurrently with the parent's, so
// keying on the session id alone would pin both onto one slot and leave the
// other idle. The parent's own turns carry no agent_id and keep the bare
// session key, so nothing changes for a session without subagents.
func affinityLaneKey(metadata map[string]string) string {
	sessionID := metadata["session_id"]
	if sessionID == "" {
		return ""
	}
	if agentID := metadata["agent_id"]; agentID != "" {
		return sessionID + "/" + agentID
	}
	return sessionID
}

// newSlotAffinityStore builds a store for cfg and subscribes it to process
// state changes so a stopped model forgets its slots. Close() unsubscribes.
func newSlotAffinityStore(cfg config.Config) *slotAffinityStore {
	s := &slotAffinityStore{
		cfg:                 cfg,
		maxSessions:         slotAffinityMaxSessions,
		minLearnInputTokens: slotAffinityMinLearnInputTokens,
		minInjectBodyBytes:  slotAffinityMinInjectBodyBytes,
		activeWindow:        slotAffinityActiveWindow,
		liveWindow:          slotAffinityLiveWindow,
		byModel:             make(map[string]map[string]slotAffinityEntry),
		now:                 time.Now,
	}
	s.unsubscribe = event.On(func(e swaputil.ProcessStateChangeEvent) {
		s.onProcessStateChange(e)
	})
	return s
}

// Close drops the process-event subscription.
func (s *slotAffinityStore) Close() {
	if s == nil || s.unsubscribe == nil {
		return
	}
	s.unsubscribe()
	s.unsubscribe = nil
}

// enabled reports whether modelID (a real model id, not an alias) opted in.
func (s *slotAffinityStore) enabled(modelID string) bool {
	if s == nil {
		return false
	}
	mc, ok := s.cfg.Models[modelID]
	return ok && mc.SlotAffinity
}

// learn remembers slot for (modelID, sessionID). A no-op for models that did
// not opt in, or when either key is empty.
func (s *slotAffinityStore) learn(modelID, sessionID string, slot int) {
	if !s.enabled(modelID) || sessionID == "" || slot < 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sessions := s.byModel[modelID]
	if sessions == nil {
		sessions = make(map[string]slotAffinityEntry)
		s.byModel[modelID] = sessions
	}
	now := s.now()
	e, exists := sessions[sessionID]
	if !exists && len(sessions) >= s.maxSessions {
		s.evictOldestLocked(sessions)
	}
	if !exists || e.slot != slot {
		e.assigned = now
	}
	e.slot, e.seen = slot, now
	sessions[sessionID] = e
}

// evictOldestLocked removes the least-recently-seen entry. Called with mu
// held; O(n) over a map bounded at maxSessions, which is fine at 256.
func (s *slotAffinityStore) evictOldestLocked(sessions map[string]slotAffinityEntry) {
	var (
		oldestKey string
		oldest    time.Time
		first     = true
	)
	for k, e := range sessions {
		if first || e.seen.Before(oldest) {
			oldestKey, oldest, first = k, e.seen, false
		}
	}
	if !first {
		delete(sessions, oldestKey)
	}
}

// lookup returns the learned slot for (modelID, sessionID) and refreshes its
// recency. ok is false when nothing was learned or the model did not opt in.
func (s *slotAffinityStore) lookup(modelID, sessionID string) (slot int, ok bool) {
	if !s.enabled(modelID) || sessionID == "" {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sessions := s.byModel[modelID]
	e, ok := sessions[sessionID]
	if !ok {
		return 0, false
	}
	e.seen = s.now()
	sessions[sessionID] = e
	return e.slot, true
}

// assign picks a slot for (modelID, sessionID) when none has been learned or
// assigned yet, and remembers the pick like learn() does. Refuses when the
// model did not opt in, sessionID is empty, or nSlots <= 1 (a single-slot
// child has nothing to pin). If the session already has an entry (from a
// prior learn or assign), that entry's recency is refreshed and its slot is
// returned unchanged. Otherwise the pick is the least-loaded slot: among
// slot ids in [0, nSlots), count sessions currently mapped to each slot
// whose last-seen time is within activeWindow of now, and take the lowest
// count, ties to the lowest slot id, so a stale conversation cannot keep
// occupying its slot's load forever. A slot learned from the child that is
// >= nSlots (e.g. after a config change shrank the pool) still counts
// against its own id rather than crashing on the out-of-range value.
func (s *slotAffinityStore) assign(modelID, sessionID string, nSlots int) (slot int, ok bool) {
	if !s.enabled(modelID) || sessionID == "" || nSlots <= 1 {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	sessions := s.byModel[modelID]
	if sessions == nil {
		sessions = make(map[string]slotAffinityEntry)
		s.byModel[modelID] = sessions
	}
	if e, exists := sessions[sessionID]; exists {
		e.seen = now
		sessions[sessionID] = e
		return e.slot, true
	}

	// Placement on what is LIVE, not on 15-min history - see
	// slotAffinityLiveWindow and pickSlotLocked.
	live, lastUsed := s.slotLoadLocked(sessions, sessionID, nSlots, now)
	best := pickSlotLocked(live, lastUsed)

	if len(sessions) >= s.maxSessions {
		s.evictOldestLocked(sessions)
	}
	sessions[sessionID] = slotAffinityEntry{slot: best, seen: now, assigned: now}
	return best, true
}

// lookupRepair is lookup for a request about to be served: it returns the
// session's slot and refreshes its recency like lookup, but first repairs a
// collision. When another LIVE session shares this session's slot and was
// placed there first, while some slot has no live session at all, this
// session moves to that slot: one re-prefill once, instead of both sessions
// throwing away each other's KV prefix on every turn switch (incident llama-cm
// 2026-09-16-two-live-sessions-pinned-same-slot-cache-thrash). The older
// session never moves, so the pair cannot ping-pong, and nothing moves when
// every slot is live - there is nowhere better to go.
func (s *slotAffinityStore) lookupRepair(modelID, sessionID string, nSlots int) (slot int, ok bool) {
	if !s.enabled(modelID) || sessionID == "" {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sessions := s.byModel[modelID]
	e, ok := sessions[sessionID]
	if !ok {
		return 0, false
	}
	now := s.now()
	if nSlots > 1 && e.slot >= 0 && e.slot < nSlots {
		sharedWithOlder := false
		for key, other := range sessions {
			if key == sessionID || other.slot != e.slot || !s.liveLocked(other, now) {
				continue
			}
			// Equal placement times break by key so exactly one side moves.
			if other.assigned.Before(e.assigned) || (other.assigned.Equal(e.assigned) && key < sessionID) {
				sharedWithOlder = true
				break
			}
		}
		if sharedWithOlder {
			live, lastUsed := s.slotLoadLocked(sessions, sessionID, nSlots, now)
			if target := pickSlotLocked(live, lastUsed); live[target] == 0 && target != e.slot {
				e.slot = target
				e.assigned = now
			}
		}
	}
	e.seen = now
	sessions[sessionID] = e
	return e.slot, true
}

// begin and end bracket a full-turn request of sessionID being served, so a
// long stream keeps its session live (liveLocked) however long ago it began.
func (s *slotAffinityStore) begin(modelID, sessionID string) {
	s.adjustInflight(modelID, sessionID, +1)
}

func (s *slotAffinityStore) end(modelID, sessionID string) {
	s.adjustInflight(modelID, sessionID, -1)
}

func (s *slotAffinityStore) adjustInflight(modelID, sessionID string, delta int) {
	if s == nil || sessionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byModel[modelID][sessionID]
	if !ok {
		// Forgotten meanwhile (model stopped, entry evicted): nothing to track.
		return
	}
	e.inflight += delta
	if e.inflight < 0 {
		e.inflight = 0
	}
	if delta < 0 {
		e.lastEnd = s.now()
	}
	s.byModel[modelID][sessionID] = e
}

// hotSlots reports, for the cooling resident modelID, which session each of
// its nSlots slots is being kept warm for: the most recently seen session
// mapped to that slot within activeWindow, or an empty SessionID when no
// live lane owns it. This is what the cooldown exists to protect (a
// session's slot and KV cache across a tool call or an AskUserQuestion
// pause), so the menu must show it like an active slot. Always returns one
// entry per slot id in [0, nSlots), in slot order, so the display is stable.
func (s *slotAffinityStore) hotSlots(modelID string, nSlots int) []swaputil.HotSlot {
	if nSlots <= 0 {
		return []swaputil.HotSlot{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	cutoff := now.Add(-s.activeWindow)
	out := make([]swaputil.HotSlot, nSlots)
	for i := range out {
		out[i].Slot = i
	}
	for sessionID, e := range s.byModel[modelID] {
		if e.slot < 0 || e.slot >= nSlots || !e.seen.After(cutoff) {
			continue
		}
		idle := int(now.Sub(e.seen) / time.Second)
		if cur := out[e.slot]; cur.SessionID == "" || idle < cur.IdleSeconds {
			out[e.slot] = swaputil.HotSlot{Slot: e.slot, SessionID: sessionID, IdleSeconds: idle}
		}
	}
	return out
}

// scratchSlot picks the slot a request WITHOUT a session lane (housekeeping
// calls, background curls, probes) should run on: the slot that no active
// session lane owns. Without this the child picks by least-recently-used,
// which is exactly the idle session's slot - its whole context is then
// replaced by a ~1k-token prompt. Among slot ids in [0, nSlots) the pick is
// the one with the fewest active lanes, ties to the HIGHEST id, so with one
// interactive session on slot 0 every housekeeping call lands on slot 1.
// Nothing is remembered: the caller is not a lane. Refuses (ok=false) when
// the model did not opt in or has a single slot.
func (s *slotAffinityStore) scratchSlot(modelID string, nSlots int) (slot int, ok bool) {
	if !s.enabled(modelID) || nSlots <= 1 {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// Live sessions first (same liveness as placement), then the 15-min
	// activity count, ties to the highest id: a housekeeping call must avoid
	// a working session's slot above all, and a paused one's where it can.
	now := s.now()
	live, _ := s.slotLoadLocked(s.byModel[modelID], "", nSlots, now)
	counts := make(map[int]int, nSlots)
	cutoff := now.Add(-s.activeWindow)
	for _, e := range s.byModel[modelID] {
		if e.seen.After(cutoff) {
			counts[e.slot]++
		}
	}
	best := nSlots - 1
	for id := nSlots - 2; id >= 0; id-- {
		if live[id] < live[best] || (live[id] == live[best] && counts[id] < counts[best]) {
			best = id
		}
	}
	return best, true
}

// forget drops every learned slot for modelID.
func (s *slotAffinityStore) forget(modelID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byModel, modelID)
}

// sessionCount reports how many sessions are remembered for modelID.
func (s *slotAffinityStore) sessionCount(modelID string) int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byModel[modelID])
}

// onProcessStateChange forgets a model's slots when its process leaves the
// running states. "stopping" is enough: from that point the old slots are
// gone and any request routed after it lands on a fresh process. Comparing
// against the string keeps this package free of an internal/process import
// (swaputil.ProcessStateChangeEvent carries states as strings for the same
// reason).
func (s *slotAffinityStore) onProcessStateChange(e swaputil.ProcessStateChangeEvent) {
	switch e.NewState {
	case "stopping", "stopped", "shutdown":
		s.forget(e.ProcessName)
	}
}

// learnFromEntry feeds a finished activity entry into the store: the entry's
// model plus its session_id and slot_id metadata. Entries without either key
// are ignored, as are non-200 responses (their slot_id, if any, is not
// something the child actually served) and entries whose InputTokens falls
// below minLearnInputTokens (a housekeeping call on the same session must
// not teach the store the wrong slot for that session's full turns).
func (s *slotAffinityStore) learnFromEntry(entry ActivityLogEntry) {
	if s == nil || entry.RespStatusCode != http.StatusOK {
		return
	}
	if entry.Tokens.InputTokens < s.minLearnInputTokens {
		return
	}
	// Same lane key as the inject side, so a subagent's learned slot never
	// overwrites its parent's.
	sessionID := affinityLaneKey(entry.Metadata)
	slotStr, ok := entry.Metadata["slot_id"]
	if sessionID == "" || !ok {
		return
	}
	slot, err := strconv.Atoi(slotStr)
	if err != nil {
		return
	}
	s.learn(entry.Model, sessionID, slot)
}

// CreateSlotAffinityMiddleware returns middleware that assigns or reuses a
// pinned id_slot and injects it into JSON bodies for opted-in local models.
// It must run AFTER CreateFilterMiddleware (so stripParams cannot undo the
// injection and the buffered body is the filtered one) and BEFORE
// CreateMetricsMiddleware (so the capture shows the body the child actually
// received). A session's slot is looked up first (from a prior learn or
// assign); when nothing is known yet, one is assigned on the spot from the
// model's concurrencyLimit. Non-JSON requests, models that did not opt in,
// requests without a session id, models with no usable slot count, and
// bodies below minInjectBodyBytes (housekeeping calls) pass through
// untouched.
func CreateSlotAffinityMiddleware(store *slotAffinityStore, cfg config.Config) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if store == nil || r.Method != http.MethodPost ||
				!strings.Contains(r.Header.Get("Content-Type"), "application/json") {
				next.ServeHTTP(w, r)
				return
			}

			data, err := swaputil.FetchContext(r, cfg)
			if err != nil {
				swaputil.SendError(w, r, swaputil.ErrNoModelInContext)
				return
			}

			body := data.Body
			if body == nil {
				body, err = io.ReadAll(r.Body)
				if err != nil {
					swaputil.SendResponse(w, r, http.StatusBadRequest, "could not read request body")
					return
				}
				// Hand the bytes back in case we pass through below.
				r.Body = io.NopCloser(bytes.NewReader(body))
			}

			// A resident-alias id (claude-*, default) is not a model block;
			// resolve it to the model that will serve it so the lane lives
			// on that model. Stamped on the context AND the live in-flight
			// entry whether or not a slot gets injected, so a renderer can
			// poll the real model's /slots for this row.
			modelID := data.ModelID
			if _, isModel := cfg.Models[modelID]; !isModel && store.resolveResident != nil {
				if resolved, ok := store.resolveResident(modelID); ok {
					modelID = resolved
					if data.Metadata == nil {
						data.Metadata = make(map[string]string, 2)
					}
					data.Metadata[resolvedModelMetadataKey] = resolved
					data.Body = body
					*r = *r.WithContext(swaputil.SetContext(r.Context(), data))
					stampInflightMetadata(r, resolvedModelMetadataKey, resolved)
				}
			}

			nSlots := 0
			if mc, exists := cfg.Models[modelID]; exists {
				nSlots = mc.ConcurrencyLimit
			}
			var (
				slot      int
				ok        bool
				sessionID string
			)
			if len(body) < store.minInjectBodyBytes {
				// A body this small is a housekeeping call (a session's own
				// title/summary call, a background curl, a probe), not a
				// full turn. It must neither learn nor receive a session's
				// slot - and it must not be left to the child's LRU pick
				// either, which is exactly the idle session's slot: the
				// child then replaces that session's whole context with a
				// 900-token prompt. Pin it to the scratch slot instead.
				slot, ok = store.scratchSlot(modelID, nSlots)
			} else {
				sessionID = affinityLaneKey(data.Metadata)
				slot, ok = store.lookupRepair(modelID, sessionID, nSlots)
				if !ok {
					slot, ok = store.assign(modelID, sessionID, nSlots)
				}
			}
			if !ok {
				next.ServeHTTP(w, r)
				return
			}

			if !gjson.ValidBytes(body) {
				next.ServeHTTP(w, r)
				return
			}

			body, err = sjson.SetBytes(body, "id_slot", slot)
			if err != nil {
				swaputil.SendResponse(w, r, http.StatusInternalServerError, "error injecting id_slot: "+err.Error())
				return
			}

			r.Body = io.NopCloser(bytes.NewReader(body))
			r.Header.Del("Transfer-Encoding")
			r.Header.Set("Content-Length", strconv.Itoa(len(body)))
			r.ContentLength = int64(len(body))

			if data.Metadata == nil {
				data.Metadata = make(map[string]string, 1)
			}
			data.Metadata[slotAffinityMetadataKey] = strconv.Itoa(slot)
			data.Body = body
			*r = *r.WithContext(swaputil.SetContext(r.Context(), data))
			// The activity entry gets this from the context at serve-done;
			// the LIVE entry (/api/events, what the menu joins /slots on)
			// was published before this middleware ran and only learns it
			// through the setter.
			stampInflightMetadata(r, slotAffinityMetadataKey, strconv.Itoa(slot))

			// A full turn keeps its session live for placement while it is
			// served, however long it streams (sessionID is empty for the
			// scratch path, which is not a lane).
			if sessionID != "" {
				store.begin(modelID, sessionID)
				defer store.end(modelID, sessionID)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// stampInflightMetadata writes key=value onto the request's LIVE in-flight
// entry. Silently a no-op for a request that never passed the inflight
// middleware (bare test harnesses) - the same contract as markKVParked in
// the scheduler.
func stampInflightMetadata(r *http.Request, key, value string) {
	if setter, ok := swaputil.InflightMetadataSetterFromContext(r.Context()); ok {
		setter(key, value)
	}
}
