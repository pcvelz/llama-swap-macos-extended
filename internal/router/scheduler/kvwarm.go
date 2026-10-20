package scheduler

import (
	"sort"
	"strings"
	"time"
)

// The slot cooldown (llama-cm docs/intent/cooldown.md; incident 2026-10-06-kv-
// pool-purge-pingpong-every-turn-re-prefills).
//
// THE GAP. kvAdmit counts only requests IN FLIGHT. A child served with
// --kv-unified keeps every idle slot's prompt in the same pool, so when session
// A (198k tokens) finishes a turn its prompt stays resident while kvAdmit sees
// an empty pool. Session B (97k, parked behind A) was granted the instant A's
// turn ended; its prefill ran out of cells at ~64k and llama-server purged A's
// slot (try_clear_id) to go on. A's tool result came back seconds later, parked
// behind B, and re-prefilled all 198k once B was done - which purged B. Live on
// cq27 2026-10-05/06: 34 of 34 turns were full re-prefills (198k at ~50 tok/s =
// 67 min, 97k = 22 min) and each session got one turn per ~90 minutes.
//
// THE COOLDOWN. When a session completes a request its prompt stays in its
// slot, and for the model's swapGraceSeconds - the SAME duration as the model
// cooldown, there is one - that slot is COOLING: another session's request is
// held only if granting it would DISPLACE a cooling slot, either by tokens
// (in-flight KV + cooling prompts + the request > kvPoolTokens) or by slot
// count (in-flight requests + cooling sessions >= concurrencyLimit, the
// child's --parallel). A request that fits beside the cooling slots - tokens
// AND a free slot - is granted at once, so parallel sessions keep working.
// Every completion with a session restarts that session's cooldown, except a
// looping one (ServeDoneEvent.Looping: the loop guard's verdict), which lets it
// run down - the same rule the model level applies in OnServeDone.
//
// THE EXITS. Only two, and no clock: FinishCooldown (the menu click) ends every
// slot cooldown once (kvWarmEndCooldowns, on the same run-loop consumption as
// the model finish), and a request whose tier Preempts and strictly outranks
// the cooling session's recorded tier is not held by that session. There is no
// quantum and no ceiling: a session that never goes quiet keeps its slot, and
// abuse is bounded by the loop guard / yield floor / penalty hold instead.
//
// A model without swapGraceSeconds has no cooldown at either level. A session's
// own subagent lanes ("<session>/<agent>") are never held behind it: the parent
// cannot return while its subagent runs, so holding the subagent would only
// delay the same purge by a window.

// kvResidentForget drops an idle residency nobody displaced: its session has
// not come back for this long, so the entry is only bookkeeping.
const kvResidentForget = time.Hour

// kvResident is one session lane's slot in a model, as the scheduler believes
// it: created at the first grant of a request carrying a session, dropped when
// another grant displaces it or its model is (re)loaded.
type kvResident struct {
	session string
	// tokens is the estimate of the session's last request - the footprint it
	// leaves in the pool once that request ends.
	tokens int
	// busy counts the session's requests currently granted. A busy residency
	// is already counted in inFlight/kvInFlight, never twice.
	busy int
	// lastEnd is when the last request ended.
	lastEnd time.Time
	// warmUntil is when the cooldown ends: the last NON-looping completion
	// plus the model's swap grace. Zero = not cooling (never completed, a
	// finish ended it, or only looping completions so far).
	warmUntil time.Time
	// tierRank is the rank of the tier the session last ran under, for the
	// strictly-outranks bypass.
	tierRank int
}

// sessionRoot strips a subagent suffix: "<session>/<agent>" -> "<session>".
func sessionRoot(session string) string {
	if i := strings.IndexByte(session, '/'); i >= 0 {
		return session[:i]
	}
	return session
}

// shortSession is the log form of a lane key: the first 8 characters of the
// session id, the subagent suffix dropped.
func shortSession(session string) string {
	if session == "" {
		return "(none)"
	}
	root := sessionRoot(session)
	sub := root != session
	if len(root) > 8 {
		root = root[:8]
	}
	if sub {
		return root + "/sub"
	}
	return root
}

// kvWarmCooling: r is idle and inside its cooldown.
func kvWarmCooling(r *kvResident, now time.Time) bool {
	return r.busy == 0 && now.Before(r.warmUntil)
}

// kvWarmSorted returns model's residencies in a deterministic order (oldest
// completion first, session id as the tie-break), so every decision and every
// API read is independent of map iteration order.
func (s *FIFO) kvWarmSorted(model string) []*kvResident {
	rs := make([]*kvResident, 0, len(s.kvResidents[model]))
	for _, r := range s.kvResidents[model] {
		rs = append(rs, r)
	}
	sort.Slice(rs, func(i, j int) bool {
		if !rs[i].lastEnd.Equal(rs[j].lastEnd) {
			return rs[i].lastEnd.Before(rs[j].lastEnd)
		}
		return rs[i].session < rs[j].session
	})
	return rs
}

// kvWarmBlocker returns the cooling residency that granting req now would
// displace, or nil when req may go. Called only after kvAdmit has admitted req.
func (s *FIFO) kvWarmBlocker(req HandlerReq) *kvResident {
	if req.StatusRead || req.ConcurrencyExempt {
		return nil
	}
	if len(s.kvResidents[req.Model]) == 0 {
		return nil
	}
	now := s.now()
	root := sessionRoot(req.Session)
	cooling := 0
	warm := 0
	var blocker *kvResident
	for _, r := range s.kvWarmSorted(req.Model) {
		// WHY: a session's own lanes reuse its own slot - nothing of theirs is
		// displaced.
		if req.Session != "" && sessionRoot(r.session) == root {
			continue
		}
		if !kvWarmCooling(r, now) {
			continue
		}
		// WHY: a Preempts tier that strictly outranks the cooling session may
		// take its slot (the one early exit besides the menu finish); same
		// contract as the model level's rank-aware grace break.
		if req.Tier.Preempts && req.Tier.Rank > r.tierRank {
			continue
		}
		cooling++
		warm += r.tokens
		if blocker == nil || r.tokens > blocker.tokens {
			blocker = r
		}
	}
	if blocker == nil {
		return nil
	}
	// WHY pool: the request would not fit beside the cooling prompts; the
	// child would purge one to make room.
	if pool := s.cfg.KVPoolTokens[req.Model]; pool > 0 && s.kvInFlight[req.Model]+warm+req.EstimatedTokens > pool {
		return blocker
	}
	// WHY slots: tokens fit, but every slot is in flight or cooling, so the
	// request would land on a cooling slot and overwrite its KV. Only when
	// concurrencyLimit is set explicitly (it is the --parallel slot count).
	if slots := s.slotCount[req.Model]; slots > 0 && s.inFlight[req.Model]+cooling >= slots {
		return blocker
	}
	return nil
}

// logKVWarmHold logs a hold once per request (on the park-reason change, not
// on every once-a-second drain pass).
func (s *FIFO) logKVWarmHold(req HandlerReq, r *kvResident) {
	if req.parkReason == ParkKVWarm {
		return
	}
	now := s.now()
	s.logger.Infof("cooldown: holding %s request of session %s est=%d: granting it would displace session %s's cooling slot (%d tok, cooling %s more) inflight=%d pool=%d",
		req.Model, shortSession(req.Session), req.EstimatedTokens, shortSession(r.session), r.tokens,
		r.warmUntil.Sub(now).Round(time.Second),
		s.kvInFlight[req.Model], s.cfg.KVPoolTokens[req.Model])
}

// kvWarmOnGrant records a granted request: one with a session makes (or keeps)
// its session resident and busy; then every idle residency that can no longer
// share the pool and the slots with what is now in flight is dropped -
// llama-server will purge it to make room.
func (s *FIFO) kvWarmOnGrant(req HandlerReq, modelID string) {
	// WHY: no swap grace = no cooldown at either level, nothing to track.
	if s.grace[modelID] <= 0 || req.StatusRead || req.ConcurrencyExempt {
		return
	}
	if req.Session != "" {
		if s.kvResidents[modelID] == nil {
			s.kvResidents[modelID] = map[string]*kvResident{}
		}
		r := s.kvResidents[modelID][req.Session]
		if r == nil {
			r = &kvResident{session: req.Session}
			s.kvResidents[modelID][req.Session] = r
		}
		r.busy++
		r.tokens = req.EstimatedTokens
		r.tierRank = req.Tier.Rank
		// warmUntil is deliberately kept: a looping completion must leave the
		// cooldown running down from the last productive one.
	}
	s.kvWarmDisplace(modelID, req.Session)
}

// kvWarmDisplace drops idle residencies, oldest first, until in-flight plus
// what stays resident fits the pool and the slots. A grant that reaches here
// either fit beside every cooling slot (so only expired entries go) or was
// allowed past the hold by a Preempts outrank (so the outranked slot goes).
func (s *FIFO) kvWarmDisplace(modelID, by string) {
	pool := s.cfg.KVPoolTokens[modelID]
	slots := s.slotCount[modelID]
	var idle []*kvResident
	tokens := s.kvInFlight[modelID]
	for _, r := range s.kvWarmSorted(modelID) {
		if r.busy == 0 {
			idle = append(idle, r)
			tokens += r.tokens
		}
	}
	count := s.inFlight[modelID] + len(idle)
	for _, r := range idle {
		overPool := pool > 0 && tokens > pool
		overSlots := slots > 0 && count > slots
		if !overPool && !overSlots {
			break
		}
		tokens -= r.tokens
		count--
		delete(s.kvResidents[modelID], r.session)
		s.logger.Infof("cooldown: %s session %s slot (%d tok) displaced by session %s: in-flight %d does not fit beside it (pool %d, slots %d)",
			modelID, shortSession(r.session), r.tokens, shortSession(by), s.kvInFlight[modelID], pool, slots)
	}
}

// kvWarmOnDone starts (or, for a non-looping completion, restarts) the
// session's cooldown when its request ends. Must run before the drain that
// follows the completion.
func (s *FIFO) kvWarmOnDone(ev ServeDoneEvent) {
	g := s.grace[ev.ModelID]
	if ev.Session == "" || g <= 0 {
		return
	}
	r := s.kvResidents[ev.ModelID][ev.Session]
	if r == nil {
		// WHY nothing is created here: every tracked grant made a record, so a
		// missing one means a reload (kvWarmForget) or an exempt request. A
		// reloaded child has empty slots, and the session's next grant
		// recreates the record.
		return
	}
	if r.busy > 0 {
		r.busy--
	}
	if r.busy > 0 {
		return
	}
	// WHY: X-Slot-Cooldown: off (user ruling 2026-10-19/20 - a subagent
	// dispatch gets no slot cooldown; llama-cm's agentic.sh sends the header).
	// The session's slot does not cool after this request, so a waiting
	// request may take it at once. The residency stays as displaceable KV:
	// kvWarmDisplace drops it when a grant no longer fits beside it. Witnessed
	// without this (llama-swap.log 2026-10-20 06:48:05): a dispatch running its
	// own tools between turns kept its slot cooling while the GPU sat idle and
	// 7 dispatches plus 2 interactive sessions waited.
	if ev.SlotCooldownOff {
		return
	}
	now := s.now()
	r.tokens = ev.EstimatedTokens
	r.lastEnd = now
	r.tierRank = ev.Tier.Rank
	// WHY not for a looping completion: a degenerate loop must not earn fresh
	// protection by repeating itself (same rule as the model level), so its
	// cooldown runs down from the last productive completion.
	if !ev.Looping {
		r.warmUntil = now.Add(g)
	}
}

// EndSlotCooldown implements SlotCooldownControl: it ends ONE session's slot
// cooldown now (POST /api/sessions/{id}/slot-cooldown, the per-session sibling
// of FinishCooldown). The residency stays as displaceable KV - only its cooling
// protection goes. A no-op when the session is not resident or not cooling.
func (s *FIFO) EndSlotCooldown(session string) {
	for _, rs := range s.kvResidents {
		if r, ok := rs[session]; ok && kvWarmCooling(r, s.now()) {
			r.warmUntil = time.Time{}
		}
	}
}

// kvWarmEndCooldowns ends every slot cooldown (FinishCooldown). The residencies
// stay as bookkeeping - they are still displaceable KV - but nothing is cooling.
func (s *FIFO) kvWarmEndCooldowns() {
	for _, rs := range s.kvResidents {
		for _, r := range rs {
			r.warmUntil = time.Time{}
		}
	}
}

// kvWarmPrune forgets residencies idle past kvResidentForget whose cooldown is
// over: the session has not come back, so the entry is only bookkeeping.
func (s *FIFO) kvWarmPrune() {
	if len(s.kvResidents) == 0 {
		return
	}
	now := s.now()
	for model, rs := range s.kvResidents {
		for sess, r := range rs {
			if r.busy == 0 && !kvWarmCooling(r, now) && !r.lastEnd.IsZero() && now.Sub(r.lastEnd) > kvResidentForget {
				delete(rs, sess)
			}
		}
		if len(rs) == 0 {
			delete(s.kvResidents, model)
		}
	}
}

// kvWarmForget drops a model's residencies: its process is being (re)started
// or stopped, and a fresh child has empty slots.
func (s *FIFO) kvWarmForget(models ...string) {
	for _, m := range models {
		delete(s.kvResidents, m)
	}
}

// SlotWarmUntil implements SlotWarmReporter: per session lane, when its slot
// cooldown ends (last non-looping completion + swap grace), as RFC3339, for
// sessions cooling now. Deterministic: models are walked in name order and a
// lane cooling on several models reports the latest end.
func (s *FIFO) SlotWarmUntil() map[string]string {
	out := make(map[string]string)
	latest := make(map[string]time.Time)
	now := s.now()
	models := make([]string, 0, len(s.kvResidents))
	for m := range s.kvResidents {
		models = append(models, m)
	}
	sort.Strings(models)
	for _, m := range models {
		for _, r := range s.kvWarmSorted(m) {
			if kvWarmCooling(r, now) && r.warmUntil.After(latest[r.session]) {
				latest[r.session] = r.warmUntil
			}
		}
	}
	for sess, until := range latest {
		out[sess] = until.Format(time.RFC3339)
	}
	return out
}
