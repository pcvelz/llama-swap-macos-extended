package scheduler

import (
	"sort"
	"strings"
	"time"
)

// KV warm hold (llama-cm incident 2026-10-06-kv-pool-purge-pingpong-every-turn-
// re-prefills).
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
// THE HOLD. When a session finishes a LARGE request its prompt is resident, and
// for kvWarmWindow - the gap a Claude Code tool loop leaves while the client
// runs a tool - it is WARM. While a session is warm, another session's request
// whose grant would push in-flight + warm tokens over the pool is parked
// (ParkKVWarm): granting it would purge the warm cache. The warm session's own
// next turn is admitted without counting its own residency (it reuses that
// cache) and costs only the delta prefill.
//
// FAIRNESS. The hold is bounded by kvWarmQuantum, measured from the moment the
// residency first made another session wait (in flight or warm). Once spent,
// the warm session's next completion does not renew it and a hold already
// running ends at once, so the waiter is granted at the next turn boundary. A
// waiter waits at most one quantum plus one turn ("a session is always
// promised a turn"). The roles then swap, and the new holder earns its own
// quantum: amortising one cold prefill over a quantum of warm turns instead of
// paying it on every turn.
//
// INERT unless the model has a kvPoolTokens budget and the two prompts cannot
// share the pool: sessions that fit together never wait on each other. A
// session's own subagent lanes ("<session>/<agent>") are never held behind it:
// the parent cannot return while its subagent runs, so holding the subagent
// would only delay the same purge by a window.

// kvWarmWindow is how long after a large turn a session's prompt counts as
// warm. Same reasoning as the slot-affinity live window (internal/server
// slot_affinity.go slotAffinityLiveWindow): a tool loop is silent on the proxy
// for seconds on a Read/Bash call and a couple of minutes on a build or a test
// run; longer than that the session is paused and a waiter gets the pool.
var kvWarmWindow = 120 * time.Second

// kvWarmQuantum bounds how long one session's residency may keep another
// waiting. A switch costs the incoming session a cold prefill of up to an hour
// on a 200k prompt, so the quantum must be long enough to amortise it.
var kvWarmQuantum = 30 * time.Minute

// kvWarmCeiling ends any hold this long after it first made another session
// wait, cut retries of a cold prefill included: the waiter's worst case.
var kvWarmCeiling = 90 * time.Minute

// kvResidentForget drops an idle residency nobody displaced: its session has
// not come back for this long, so the entry is only bookkeeping.
const kvResidentForget = time.Hour

// kvResident is one session's prompt in a model's KV pool, as the scheduler
// believes it: created at the grant of the session's first large request,
// dropped when another grant displaces it or its model is (re)loaded.
type kvResident struct {
	session string
	// tokens is the estimate of the session's last large request - what it
	// leaves resident once that request ends.
	tokens int
	// busy counts the session's large requests currently granted. A busy
	// residency is already counted in kvInFlight, never twice.
	busy int
	// lastEnd is when the last large request ended; warmUntil is when the
	// hold lapses (zero = not protected, e.g. quantum spent).
	lastEnd   time.Time
	warmUntil time.Time
	// contendedSince is when another session first waited on this residency
	// (zero = nobody waiting); kvWarmCeiling runs from here. quantumSince is
	// the first COMPLETED turn after that (a cut retry is still the cold
	// prefill); kvWarmQuantum runs from there. lastCut: the last request ended
	// cut.
	contendedSince time.Time
	quantumSince   time.Time
	lastCut        bool
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

func (s *FIFO) kvWarmQuantumSpent(r *kvResident, now time.Time) bool {
	if r.contendedSince.IsZero() {
		return false
	}
	if now.Sub(r.contendedSince) >= kvWarmCeiling {
		return true
	}
	return !r.quantumSince.IsZero() && now.Sub(r.quantumSince) >= kvWarmQuantum
}

// kvWarmProtected: r is idle, inside its window and its quantum.
func (s *FIFO) kvWarmProtected(r *kvResident, now time.Time) bool {
	return r.busy == 0 && now.Before(r.warmUntil) && !s.kvWarmQuantumSpent(r, now)
}

// kvWarmBlocker returns the warm residency that granting req now would purge,
// or nil when req may go. Called only after kvAdmit has admitted req.
func (s *FIFO) kvWarmBlocker(req HandlerReq) *kvResident {
	if req.StatusRead || req.ConcurrencyExempt {
		return nil
	}
	pool := s.cfg.KVPoolTokens[req.Model]
	if pool <= 0 {
		return nil
	}
	now := s.now()
	root := sessionRoot(req.Session)
	warm := 0
	var blocker *kvResident
	for _, r := range s.kvResidents[req.Model] {
		if (req.Session != "" && sessionRoot(r.session) == root) || !s.kvWarmProtected(r, now) {
			continue
		}
		warm += r.tokens
		if blocker == nil || r.tokens > blocker.tokens {
			blocker = r
		}
	}
	if blocker == nil || s.kvInFlight[req.Model]+warm+req.EstimatedTokens <= pool {
		return nil
	}
	return blocker
}

// logKVWarmHold logs a hold once per request (on the park-reason change, not
// on every once-a-second drain pass).
func (s *FIFO) logKVWarmHold(req HandlerReq, r *kvResident) {
	if req.parkReason == ParkKVWarm {
		return
	}
	now := s.now()
	quantumLeft := kvWarmQuantum
	if !r.quantumSince.IsZero() {
		quantumLeft -= now.Sub(r.quantumSince)
	}
	s.logger.Infof("kv-warm: holding %s request of session %s est=%d: granting it would purge session %s's warm cache (%d tok, warm %s more, quantum %s left) inflight=%d pool=%d",
		req.Model, shortSession(req.Session), req.EstimatedTokens, shortSession(r.session), r.tokens,
		r.warmUntil.Sub(now).Round(time.Second), quantumLeft.Round(time.Second),
		s.kvInFlight[req.Model], s.cfg.KVPoolTokens[req.Model])
}

// kvWarmOnGrant records a granted request: a large one with a session makes
// (or keeps) its session resident and busy; then every idle residency of
// another session that can no longer share the pool with what is now in
// flight is dropped - llama-server will purge it to make room.
func (s *FIFO) kvWarmOnGrant(req HandlerReq, modelID string) {
	if s.cfg.KVPoolTokens[modelID] <= 0 || req.StatusRead || req.ConcurrencyExempt {
		return
	}
	if req.Session != "" && req.EstimatedTokens >= largePrefillThreshold {
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
		r.warmUntil = time.Time{}
	}
	s.kvWarmDisplace(modelID, req.Session)
}

// kvWarmDisplace drops idle residencies, oldest first, until in-flight plus
// what stays resident fits the pool.
func (s *FIFO) kvWarmDisplace(modelID, by string) {
	pool := s.cfg.KVPoolTokens[modelID]
	var idle []*kvResident
	total := s.kvInFlight[modelID]
	for _, r := range s.kvResidents[modelID] {
		if r.busy == 0 {
			idle = append(idle, r)
			total += r.tokens
		}
	}
	sort.Slice(idle, func(i, j int) bool { return idle[i].lastEnd.Before(idle[j].lastEnd) })
	for _, r := range idle {
		if total <= pool {
			break
		}
		total -= r.tokens
		delete(s.kvResidents[modelID], r.session)
		s.logger.Infof("kv-warm: %s session %s cache (%d tok) displaced by session %s: in-flight %d does not fit beside it in pool %d",
			modelID, shortSession(r.session), r.tokens, shortSession(by), s.kvInFlight[modelID], pool)
	}
}

// kvWarmOnDone marks the session's prompt resident and warm when its large
// request ends - unless its quantum is spent, then the pool is handed over.
// Must run before the drain that follows the completion.
func (s *FIFO) kvWarmOnDone(ev ServeDoneEvent) {
	if ev.Session == "" || ev.EstimatedTokens < largePrefillThreshold || s.cfg.KVPoolTokens[ev.ModelID] <= 0 {
		return
	}
	r := s.kvResidents[ev.ModelID][ev.Session]
	if r == nil {
		// Displaced while busy cannot happen (only idle entries are dropped);
		// reached after a reload cleared the table mid-request. Its prompt
		// is in the fresh child now.
		if s.kvResidents[ev.ModelID] == nil {
			s.kvResidents[ev.ModelID] = map[string]*kvResident{}
		}
		r = &kvResident{session: ev.Session, busy: 1}
		s.kvResidents[ev.ModelID][ev.Session] = r
	}
	if r.busy > 0 {
		r.busy--
	}
	if r.busy > 0 {
		return
	}
	now := s.now()
	r.tokens = ev.EstimatedTokens
	r.lastEnd = now
	r.lastCut = ev.Cut
	if !ev.Cut && !r.contendedSince.IsZero() && r.quantumSince.IsZero() {
		r.quantumSince = now
	}
	if s.kvWarmQuantumSpent(r, now) {
		r.warmUntil = time.Time{}
		s.logger.Infof("kv-warm: %s session %s quantum spent (another session waiting %s): not holding its cache, the waiter goes next",
			ev.ModelID, shortSession(ev.Session), now.Sub(r.contendedSince).Round(time.Second))
		return
	}
	r.warmUntil = now.Add(kvWarmWindow)
}

// kvWarmContention re-reads, from the queue, who is waiting on which
// residency. The quantum clock starts at the first WARM hold (ParkKVWarm) - not
// at a plain in-flight park, or a 67-minute cold prefill would spend the whole
// quantum before the session got a single warm turn - keeps running while
// another session waits for any KV reason, and resets once nobody does. Also
// forgets residencies idle past kvResidentForget.
func (s *FIFO) kvWarmContention() {
	if len(s.kvResidents) == 0 {
		return
	}
	now := s.now()
	for model, rs := range s.kvResidents {
		for sess, r := range rs {
			if r.busy == 0 && !r.lastEnd.IsZero() && now.Sub(r.lastEnd) > kvResidentForget {
				delete(rs, sess)
				continue
			}
			heldWarm, waiting := false, false
			for _, q := range s.queued {
				if q.Model != model || (q.parkReason != ParkKV && q.parkReason != ParkKVWarm) {
					continue
				}
				if q.Session != "" && sessionRoot(q.Session) == sessionRoot(r.session) {
					continue
				}
				waiting = true
				if q.parkReason == ParkKVWarm {
					heldWarm = true
				}
			}
			switch {
			case !waiting:
				r.contendedSince = time.Time{}
				r.quantumSince = time.Time{}
			case heldWarm && r.contendedSince.IsZero() && s.kvWarmProtected(r, now):
				r.contendedSince = now
				if !r.lastCut {
					r.quantumSince = now
				}
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
