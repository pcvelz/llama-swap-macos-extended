package scheduler

import (
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// defaultConcurrencyLimit caps simultaneous in-flight requests per model when
// the model config leaves concurrencyLimit unset.
const defaultConcurrencyLimit = 10

// grantedReq is one currently-granted (in-flight) request's tier bookkeeping,
// tracked alongside FIFO.inFlight so the preemption branch can find victims
// for an arrival that cannot be granted because of in-flight work. Order
// within a model's slice carries no meaning; entries are removed on any
// OnServeDone for that model, not necessarily the one that produced them —
// exactly mirroring the existing inFlight counter's per-model granularity.
type grantedReq struct {
	tier    swaputil.Tier
	preempt func()
}

// activeSwap tracks one in-flight swap and the callers waiting on it.
type activeSwap struct {
	modelID string
	evict   []string
	waiters []HandlerReq
}

// FIFO is the default scheduler. Requests are handled in a first-in, first-out order.
// To reduce swapping requests for a model that is already running will be handled
// immediately by the running process.
//
// Requests into this schedule are handled like this:
//
// A B C A B C --> A A B B C C
//
// The strategy is simple and reduces the number of swaps required.
type FIFO struct {
	name    string
	logger  *logmon.Monitor
	planner Swapper
	cfg     config.FifoConfig
	effects Effects

	// limits is the per-model concurrency cap (config concurrencyLimit,
	// defaultConcurrencyLimit when unset). Upstream enforces it by rejecting
	// over-limit arrivals with 429 before they can start a loading stream;
	// this fork instead PARKS them in the queue — see atCapacity.
	limits   map[string]int
	active   map[string]*activeSwap
	inFlight map[string]int
	queued   []HandlerReq

	// granted tracks, per model, the tier + preempt handle of every currently
	// granted (in-flight) request — the tiered-queue counterpart to inFlight,
	// which only counts requests. Consulted by the preemption branch when an
	// arrival cannot be granted because of in-flight work; see tryPreempt.
	granted map[string][]*grantedReq

	// kvInFlight tracks, per model, the sum of EstimatedTokens across every
	// currently-granted request — the KV-aware admission counterpart to
	// inFlight (which only counts requests, not their size). Only consulted
	// for models with a positive cfg.KVPoolTokens entry; see kvAdmit.
	kvInFlight map[string]int

	// grace maps a model ID to its swap-grace duration: a request for a
	// different model is held in the queue until this model has been idle
	// (in-flight 0) for at least this long before it may be evicted. Absent or
	// <= 0 means no grace for that model.
	grace map[string]time.Duration
	// idleSince records when each model last became idle — its in-flight dropped
	// to 0, or it just became ready. Grace is measured from this. A busy model
	// (in-flight > 0) is already deferred by the in-flight check before the grace
	// check runs, so a stale idleSince on a busy model is harmless.
	idleSince map[string]time.Time
	// graceWait records, per REQUESTED model, when its oldest queued request
	// was FIRST deferred by a swap-grace. It powers the starvation valve in
	// withinGrace: grace is an idle-STREAK requirement, so a continuous
	// consumer of the resident model (cadence < grace) resets idleSince
	// forever and a parked cross-model request would starve indefinitely
	// (witnessed 2026-07-15: a ~20s-cadence client pinned a 600s-grace model;
	// the parked request waited 13+ min until manually flushed). Once the
	// deferred request has itself waited >= the evictee's grace, the evictee
	// has had its full protection window and the swap proceeds at the next
	// drain gap. Cleared when the model's swap starts or its queue empties.
	graceWait map[string]time.Time
	// now is the clock; overridable in tests.
	now func() time.Time

	// nextArrivalSeq hands out HandlerReq.arrivalSeq — see its doc comment.
	// Plain uint64, not atomic: only ever touched on the run loop.
	nextArrivalSeq uint64
}

// NewFIFO builds a FIFO scheduler. Both per-model tables are derived from
// models: the concurrency limit (ConcurrencyLimit, falling back to
// defaultConcurrencyLimit) and the swap-grace window (SwapGraceSeconds; absent
// or 0 means no grace, which is upstream's behaviour).
func NewFIFO(name string, logger *logmon.Monitor, planner Swapper, cfg config.FifoConfig, models map[string]config.ModelConfig, eff Effects) *FIFO {
	limits := make(map[string]int, len(models))
	grace := make(map[string]time.Duration, len(models))
	for id, mc := range models {
		limit := defaultConcurrencyLimit
		if mc.ConcurrencyLimit > 0 {
			limit = mc.ConcurrencyLimit
		}
		limits[id] = limit
		if mc.SwapGraceSeconds > 0 {
			grace[id] = time.Duration(mc.SwapGraceSeconds) * time.Second
		}
	}

	return &FIFO{
		name:       name,
		logger:     logger,
		planner:    planner,
		cfg:        cfg,
		effects:    eff,
		limits:     limits,
		active:     make(map[string]*activeSwap),
		inFlight:   make(map[string]int),
		granted:    make(map[string][]*grantedReq),
		kvInFlight: make(map[string]int),
		grace:      grace,
		idleSince:  make(map[string]time.Time),
		graceWait:  make(map[string]time.Time),
		now:        time.Now,
	}
}

// OnRequest decides what to do with one incoming ServeHTTP request. It never
// blocks indefinitely: any work that has to wait (starting a process, stopping
// siblings, waiting for ready) is deferred to a swap goroutine and reported back
// via OnSwapDone.
//
// The decision tree, in order:
//
//  1. Unknown model — reject the admission handshake with ErrModelNotFound
//     (before any loading stream can start) and move on.
//     1b. Every other request is admitted immediately: the per-model concurrency
//     cap parks over-capacity requests in the queue rather than rejecting
//     them with a 429 (see atCapacity).
//  2. Rank barrier: never grant an arrival while a strictly higher-rank
//     request is still queued. This applies to every remaining branch below
//     (joining/starting a swap, fast-path serve) so background work only
//     proceeds once the priority + default queues are empty. Checked BEFORE
//     the swap-join branch (3) — joining an in-flight swap for the same
//     model is itself a grant (the joiner is served the moment the swap
//     completes) and must not skip the barrier either.
//  3. A swap to the same model is already in flight — attach this waiter so
//     one swap serves all callers that asked for the same model.
//  4. Fast path — the target process is already ready, the planner sees
//     nothing to evict, and no in-flight swap is evicting it. Hand back its
//     ServeHTTP immediately.
//  5. Would collide with an in-flight swap (we'd stop their target, or they're
//     stopping us) — park in the queue for OnSwapDone to drain.
//  6. Would evict a process that is still handling requests — park in the
//     queue. OnServeDone will retry when the busy process drains.
//  7. Otherwise — start a new swap. This may run in parallel with other active
//     swaps when their evict sets don't intersect.
func (s *FIFO) OnRequest(req HandlerReq) {
	// (1) Unknown model.
	state, ok := s.effects.ModelState(req.Model)
	if !ok {
		s.logger.Debugf("%s: model %s not handled by this router", s.name, req.Model)
		s.rejectAdmission(req, ErrModelNotFound)
		return
	}

	// (1b) Admit before anything else can commit a response stream. A false
	// return means the caller is already gone; it never reaches the queue.
	if !s.admit(req) {
		return
	}

	// Stamp this request's arrival order the one time it first enters the
	// scheduler — see HandlerReq.arrivalSeq. Every later re-enqueue of this
	// same value (OnSwapDone's cap re-queue, drainQueue) carries this stamp
	// forward untouched.
	s.nextArrivalSeq++
	req.arrivalSeq = s.nextArrivalSeq

	// (2) Rank barrier — see decision-tree doc above. Must run before (3) so
	// joining an in-flight swap cannot bypass it.
	if s.blockedByRankBarrier(req) {
		s.logger.Debugf("%s: queuing request for model %s (rank barrier: higher-rank request queued)", s.name, req.Model)
		s.enqueue(req)
		return
	}

	// (3) Join an in-flight swap for the same model.
	if sw, ok := s.active[req.Model]; ok {
		s.logger.Debugf("%s: joining in-flight swap for model %s (%d waiters)", s.name, req.Model, len(sw.waiters)+1)
		sw.waiters = append(sw.waiters, req)
		return
	}

	running := s.runningSet(req.Model)
	evict := s.planner.EvictionFor(req.Model, running)

	// (3c) Per-model concurrency cap: the model is already serving as many
	// requests as it is allowed to. Park in the queue and wait instead of
	// bouncing the caller with a 429 (see atCapacity), booting an eligible
	// lower-rank holder first if this arrival outranks one — the same
	// same-model preemption the deleted semaphore layer performed.
	if s.atCapacity(req) {
		if s.tryPreempt(req, []string{req.Model}, 1) {
			s.logger.Debugf("%s: preempting same-model in-flight request(s) on %s for higher-rank arrival at concurrency cap", s.name, req.Model)
		}
		s.logger.Debugf("%s: queuing request for model %s (concurrency limit %d reached)", s.name, req.Model, s.limit(req.Model))
		s.enqueue(req)
		return
	}

	// (4) Fast path: ready, nothing to evict, and nobody is evicting us.
	if state == process.StateReady && len(evict) == 0 && !collidesWith(req.Model, evict, s.active) {
		if !s.kvAdmit(req.Model, req.EstimatedTokens) {
			// SAME-model preemption: the pool is held by in-flight requests on
			// req's own model, so those are the victims — without this, a
			// higher-rank arrival could never boot a lower-rank request of the
			// same model (the evict set is empty on this branch).
			if s.tryPreempt(req, []string{req.Model}, 1) {
				s.logger.Debugf("%s: preempting same-model in-flight request(s) on %s for higher-rank arrival", s.name, req.Model)
			}
			s.logKVParked(req.Model, req.EstimatedTokens)
			s.enqueue(req)
			return
		}
		s.logger.Debugf("%s: fast-path serving model %s (already ready)", s.name, req.Model)
		s.grantHandler(req, req.Model)
		return
	}

	// (5) Collision with an in-flight swap — queue.
	if collidesWith(req.Model, evict, s.active) {
		s.logger.Debugf("%s: queuing request for model %s (collides with in-flight swap)", s.name, req.Model)
		s.enqueue(req)
		return
	}

	// (6) Would evict a busy process — try to preempt it (see tryPreempt),
	// then queue either way: preemption is a server-side cancel, not a
	// synchronous release, so req must wait for the victim's own OnServeDone
	// to arrive before drainQueue can grant it.
	if conflictsWithInFlight(evict, s.inFlight) {
		if s.tryPreempt(req, evict, 0) {
			s.logger.Debugf("%s: preempting in-flight request(s) blocking model %s for higher-rank arrival", s.name, req.Model)
		}
		s.logger.Debugf("%s: queuing request for model %s (would evict in-flight process)", s.name, req.Model)
		s.enqueue(req)
		return
	}

	// (6b) Would evict a model still inside its swap-grace window — keep it
	// resident and queue the request. The grace lets a recently-hot model ride
	// out brief request gaps (an agent pausing to run a tool) instead of being
	// evicted the instant it drains. OnServeDone (new traffic) or OnTick (pure
	// idle) retries; the swap proceeds once the grace elapses.
	if s.withinGrace(req.Model, evict) {
		s.noteGraceDeferral(req.Model)
		s.logger.Debugf("%s: queuing request for model %s (evictee still within swap-grace)", s.name, req.Model)
		s.enqueue(req)
		return
	}

	// (7) Start a new (possibly parallel) swap.
	s.logger.Debugf("%s: starting swap for model %s, evicting %v", s.name, req.Model, evict)
	s.startSwap(req, evict, running)
}

// OnCancel removes a request whose client has disconnected from the queue and
// from every in-flight swap's waiters. If the request was the sole waiter of an
// active swap, the swap goroutine is left to complete on its own — OnSwapDone
// will find no waiters and simply clean up. This prevents drainQueue from ever
// starting a model load for a caller that is no longer there.
func (s *FIFO) OnCancel(req HandlerReq) {
	removed := false

	// Prune from the queue.
	if len(s.queued) > 0 {
		kept := s.queued[:0]
		for _, q := range s.queued {
			if q.Respond == req.Respond {
				removed = true
				continue
			}
			kept = append(kept, q)
		}
		s.queued = kept
	}

	// Prune from any active swap's waiters.
	for _, sw := range s.active {
		filtered := sw.waiters[:0]
		for _, w := range sw.waiters {
			if w.Respond == req.Respond {
				removed = true
				continue
			}
			filtered = append(filtered, w)
		}
		sw.waiters = filtered
	}

	if removed {
		// If that was the LAST queued request for this model, drop its
		// starvation-valve reference: a stale graceWait would let a future,
		// unrelated request for the same model bypass the evictee's grace
		// instantly (the valve must measure THAT request's own wait).
		stillQueued := false
		for _, q := range s.queued {
			if q.Model == req.Model {
				stillQueued = true
				break
			}
		}
		if !stillQueued {
			delete(s.graceWait, req.Model)
		}
		s.logger.Debugf("%s: cancelled request for model %s pruned from scheduler", s.name, req.Model)
		broadcastQueuePositions(s.queued)
	}
}

// OnSwapDone fans the result out to every waiter that joined this swap, removes
// the swap from the active map, then walks the queue once, promoting any items
// that no longer collide with the remaining active set. FIFO order is preserved:
// items still blocked stay in place.
func (s *FIFO) OnSwapDone(ev SwapDone) {
	sw, ok := s.active[ev.ModelID]
	if !ok {
		return
	}
	delete(s.active, ev.ModelID)

	// A freshly-ready model starts its idle clock now. If waiters are granted
	// below it goes busy immediately and the grace check won't apply until it
	// drains again; if there are none it sits idle and the grace runs from here.
	if ev.Err == nil {
		s.idleSince[ev.ModelID] = s.now()
	}

	for _, w := range sw.waiters {
		if ev.Err != nil {
			s.effects.GrantError(w, ev.Err)
			continue
		}
		// Waiters joined this swap before the concurrency cap could apply
		// (branch (2) of OnRequest runs first), so honour it here: anything
		// over the freshly-ready model's allowance goes back in the queue
		// instead of being granted. Upstream keeps the same invariant by
		// rejecting the excess waiter with a 429 at admission time.
		if s.atCapacity(w) {
			s.logger.Debugf("%s: swap waiter for model %s re-queued (concurrency limit %d reached)", s.name, ev.ModelID, s.limit(ev.ModelID))
			s.enqueue(w)
			continue
		}
		s.grantHandler(w, ev.ModelID)
	}

	s.drainQueue()
}

// OnServeDone decrements the per-model in-flight count and releases this
// request's KV-admission reservation (if any). It retries the queue whenever
// something a parked request might now fit in was freed: the in-flight count
// dropping to zero (a swap may now be possible), a KV reservation being
// released (a parallel-slot request may now fit the pool even while a sibling
// request for the same model is still in flight), or simply a serving slot
// coming free under the per-model concurrency cap (atCapacity) — the last of
// which is true of every serve-done, so a non-empty queue is always re-walked.
func (s *FIFO) OnServeDone(ev ServeDoneEvent) {
	s.inFlight[ev.ModelID]--
	// Drop one granted-tracking entry for this model, mirroring the inFlight
	// decrement above. Which specific entry is immaterial — only the count of
	// currently-granted requests (and their tiers, for future preemption
	// decisions) matters, not which physical request this ServeDoneEvent
	// belongs to.
	if g := s.granted[ev.ModelID]; len(g) > 0 {
		s.granted[ev.ModelID] = g[:len(g)-1]
		if len(s.granted[ev.ModelID]) == 0 {
			delete(s.granted, ev.ModelID)
		}
	}
	wentIdle := s.inFlight[ev.ModelID] <= 0
	if wentIdle {
		delete(s.inFlight, ev.ModelID)
		// Model just went idle — (re)start its swap-grace clock so a competing
		// request waits the full grace from this moment before evicting it.
		s.idleSince[ev.ModelID] = s.now()
	}

	kvReleased := s.releaseKV(ev.ModelID, ev.EstimatedTokens)

	// A serving slot always came free here, so a request parked purely by the
	// concurrency cap must get another chance; drainQueue is a no-op on an
	// empty queue.
	if wentIdle || kvReleased || len(s.queued) > 0 {
		s.drainQueue()
	}
}

// OnTick re-evaluates the queue on a timer. It is the only thing that lets a
// grace-deferred swap proceed once its evictee has gone idle and STAYS idle:
// no serve or swap event fires during pure idle, so without this nudge the
// deferred request would wait forever. The router arms it only when a swap-grace
// is configured; it is a no-op when the queue is empty.
func (s *FIFO) OnTick() {
	s.drainQueue()
}

// withinGrace reports whether any model in evict is still inside its swap-grace
// window: it is idle now (a busy evictee is already deferred by the in-flight
// check that runs before this) but has been idle for less than its configured
// grace. A model with no recorded idle time (ready but never served under this
// scheduler) is treated as evictable. grace <= 0 means no grace for that model.
//
// Starvation valve: the idle-streak requirement alone lets a continuous
// consumer of the evictee (request cadence < grace) defer reqModel FOREVER —
// idleSince resets on every completion. So a deferral is honoured only until
// reqModel's oldest queued request has itself waited the evictee's full grace
// (measured from its first deferral, tracked in graceWait): at that point the
// evictee has had every bit of protection the grace promises, and the swap
// proceeds at the next drain gap. reqModel must be the REQUESTED model whose
// deferral is being decided; callers record graceWait on a true return.
func (s *FIFO) withinGrace(reqModel string, evict []string) bool {
	now := s.now()
	for _, m := range evict {
		g := s.grace[m]
		if g <= 0 {
			continue
		}
		since, ok := s.idleSince[m]
		if !ok {
			continue
		}
		if now.Sub(since) < g {
			if w, ok := s.graceWait[reqModel]; ok && now.Sub(w) >= g {
				continue // valve open: requester already waited m's full grace
			}
			return true
		}
	}
	return false
}

// noteGraceDeferral records the FIRST time a request for reqModel was deferred
// by a swap-grace, so the starvation valve has a fixed reference point.
func (s *FIFO) noteGraceDeferral(reqModel string) {
	if _, ok := s.graceWait[reqModel]; !ok {
		s.graceWait[reqModel] = s.now()
	}
}

// OnUnload reconciles router-owned state with the impending Stop, performs the
// Stop (synchronously, via Effects) so callers of Unload remain blocked until
// each targeted process has exited, then drains the queue.
func (s *FIFO) OnUnload(targets []string, timeout time.Duration) {
	unloadErr := fmt.Errorf("%s: model unloaded", s.name)

	targetSet := make(map[string]bool, len(targets))
	for _, id := range targets {
		targetSet[id] = true
	}

	// Release waiters of any in-flight swap whose target is being unloaded.
	// The swap goroutine itself is left to finish on its own; when its
	// SwapDone arrives, OnSwapDone will find no entry in active and drop it.
	for id := range targetSet {
		sw, ok := s.active[id]
		if !ok {
			continue
		}
		for _, w := range sw.waiters {
			s.effects.GrantError(w, unloadErr)
		}
		delete(s.active, id)
	}

	// Drop queued requests addressed to unloaded models. Requests for other
	// models stay queued and may benefit from drainQueue at the end.
	if len(s.queued) > 0 {
		kept := s.queued[:0]
		for _, w := range s.queued {
			if targetSet[w.Model] {
				s.effects.GrantError(w, unloadErr)
				continue
			}
			kept = append(kept, w)
		}
		s.queued = kept
	}

	// Stop the targeted processes. Done synchronously so Unload's caller can
	// rely on "after Unload returns, the process is stopped". inFlight is
	// intentionally NOT cleared here: each dying handler will fire its tracked
	// serve and reach OnServeDone in the normal way.
	s.effects.StopProcesses(timeout, targets)

	// Removing entries from active above may have unblocked queued requests
	// that previously collided with the now-cancelled swaps.
	s.drainQueue()
}

// OnShutdown grants err to every waiter still held by the scheduler.
func (s *FIFO) OnShutdown(err error) {
	for _, sw := range s.active {
		for _, w := range sw.waiters {
			s.effects.GrantError(w, err)
		}
	}
	for _, w := range s.queued {
		s.effects.GrantError(w, err)
	}
}

// grantHandler hands the caller a tracked handler for modelID and, only if the
// caller was still there to receive it, bumps the in-flight count. Incrementing
// when the grant failed would strand the counter and block future evictions.
// The same "only on true return" rule applies to the KV reservation: a caller
// that already walked away will never produce a matching OnServeDone, so
// reserving its estimate would strand that budget forever too.
func (s *FIFO) grantHandler(req HandlerReq, modelID string) {
	// Nil Ctx only happens in tests that build a bare HandlerReq; skip rather
	// than panic in ctx.Value.
	if req.Ctx != nil {
		if err := swaputil.SetReqData(req.Ctx, "fifo_priority", strconv.Itoa(s.cfg.Priority[req.Model])); err != nil {
			s.logger.Debugf("failed to set fifo_priority metadata: %v", err)
		}
	}

	if s.effects.GrantServe(req, modelID) {
		s.inFlight[modelID]++
		if req.EstimatedTokens > 0 {
			s.kvInFlight[modelID] += req.EstimatedTokens
		}
		// Track tier + preempt handle for the preemption branch. req.Preempt
		// is nil for callers that don't wire tiered-queue support (e.g. tests
		// against a bare fakeEffects) — harmless to skip tracking those; they
		// simply can never be preemption victims.
		if req.Preempt != nil {
			s.granted[modelID] = append(s.granted[modelID], &grantedReq{tier: req.Tier, preempt: req.Preempt})
		}
	}
}

// admit performs upstream's pre-stream admission handshake: the caller parks
// on req.Admit until the scheduler answers, so any rejection is written as a
// plain HTTP error before a loading/SSE stream can be committed (upstream
// #889). It returns false only when the caller has already gone away.
//
// Fork divergence: upstream ALSO rejects here with a 429 once a model is at
// its concurrency limit. This fork never does — over-capacity requests wait
// (see atCapacity). Rationale (2026-07-05, re-homed from the deleted
// internal/server concurrency middleware): a slot is held for a request's
// entire lifetime including time parked behind a swap, so bursts of parked
// requests exhausted the pool and real turns got instant 429s. Waiting queues
// fairly, still caps concurrent child inference, and releases automatically
// when the client disconnects (keepalive pings in router/pinging.go keep the
// waiting stream alive meanwhile).
func (s *FIFO) admit(req HandlerReq) bool {
	return sendAdmission(req, nil)
}

// rejectAdmission answers the handshake with err. Used only for failures that
// must be visible before any stream starts (unknown model).
func (s *FIFO) rejectAdmission(req HandlerReq, err error) {
	sendAdmission(req, err)
}

func sendAdmission(req HandlerReq, err error) bool {
	if req.Admit == nil {
		return true
	}
	done := reqDone(req)
	select {
	case <-done:
		return false
	default:
	}
	select {
	case req.Admit <- err:
		return true
	case <-done:
		return false
	}
}

func reqDone(req HandlerReq) <-chan struct{} {
	if req.Ctx == nil {
		return nil
	}
	return req.Ctx.Done()
}

// atCapacity reports whether req's model is already serving its full
// concurrency allowance. Requests marked ConcurrencyExempt (tokenize-only
// count_tokens calls) are never held back here.
func (s *FIFO) atCapacity(req HandlerReq) bool {
	if req.ConcurrencyExempt {
		return false
	}
	return s.inFlight[req.Model] >= s.limit(req.Model)
}

// limit returns the per-model concurrency cap, defaulting to
// defaultConcurrencyLimit when the model has no explicit entry.
func (s *FIFO) limit(modelID string) int {
	if l, ok := s.limits[modelID]; ok {
		return l
	}
	return defaultConcurrencyLimit
}

// tryPreempt looks for granted (in-flight) requests on models in evict that
// block req from proceeding, and boots the ones the preemption rule allows:
// rank(victim) < rank(req) AND (victim.Preemptible OR req.Preempts). Booting
// is a server-side context cancel (see scheduler.HandlerReq.Preempt /
// trackedServe) — not synchronous — so this only fires the cancels; the
// caller must still queue req and wait for the victims' OnServeDone to arrive
// before a retry can succeed. Returns whether anything was booted.
//
// limit bounds how many victims may be booted; 0 means every eligible one.
// A same-model arrival blocked by the concurrency cap or the KV pool needs
// exactly ONE slot, so those callers pass 1: booting every eligible holder
// on a --parallel 2 box cancelled two interactive prefills to seat one
// request (llama-cm incident 2026-07-05-cq27-toolgap-eviction-reprefill-storm,
// 2026-09-01 follow-up). The swap path (a different model must be evicted)
// keeps 0: the swap cannot start until that model's in-flight count is zero,
// so every holder has to go.
func (s *FIFO) tryPreempt(req HandlerReq, evict []string, limit int) bool {
	booted := 0
	for _, m := range evict {
		victims := s.granted[m]
		if len(victims) == 0 {
			continue
		}
		for _, v := range victims {
			if limit > 0 && booted >= limit {
				return true
			}
			if !swaputil.CanPreempt(v.tier, req.Tier) {
				continue // rank(victim) >= rank(arrival), or victim isn't eligible
			}
			if v.preempt != nil {
				v.preempt()
				booted++
			}
		}
	}
	return booted > 0
}

// kvAdmit reports whether a request estimated at `estimate` tokens for model
// may be forwarded immediately under KV-aware admission
// (config.FifoConfig.KVPoolTokens). Fail-open by design:
//   - no (or non-positive) budget configured for the model -> always admit
//     (today's behavior, admission control off).
//   - nothing currently in flight for the model -> always admit, even if
//     estimate alone exceeds the whole pool. A request must never be
//     rejected outright for being "too big" — only ever parked behind
//     others; when it's the only one, there's nothing for it to collide
//     with, so let it through and let llama.cpp be the final arbiter.
//   - otherwise -> admit only if the combined estimate still fits the pool.
func (s *FIFO) kvAdmit(model string, estimate int) bool {
	pool := s.cfg.KVPoolTokens[model]
	if pool <= 0 {
		return true
	}
	inflight := s.kvInFlight[model]
	if inflight <= 0 {
		return true
	}
	return inflight+estimate <= pool
}

// releaseKV returns estimate tokens to model's KV budget and reports whether
// it actually released anything (used by OnServeDone to decide whether a
// parked request might now fit). A no-op for requests KV admission was never
// tracking (estimate <= 0, or nothing currently reserved for the model — e.g.
// admission is disabled for it).
func (s *FIFO) releaseKV(model string, estimate int) bool {
	if estimate <= 0 || s.kvInFlight[model] <= 0 {
		return false
	}
	s.kvInFlight[model] -= estimate
	if s.kvInFlight[model] < 0 {
		s.kvInFlight[model] = 0
	}
	s.logger.Infof("kv-admission: released %s est=%d inflight=%d pool=%d",
		model, estimate, s.kvInFlight[model], s.cfg.KVPoolTokens[model])
	return true
}

// logKVParked logs the one required observability line for a request parked
// by KV-aware admission.
func (s *FIFO) logKVParked(model string, estimate int) {
	s.logger.Infof("kv-admission: parked %s est=%d inflight=%d pool=%d",
		model, estimate, s.kvInFlight[model], s.cfg.KVPoolTokens[model])
}

// startSwap records the swap as active and launches it via Effects. running is
// the set EvictionFor saw, forwarded to OnSwapStart so the planner logs against
// the same picture it decided on.
func (s *FIFO) startSwap(initial HandlerReq, evict, running []string) {
	// The wait is over — drop the starvation-valve reference so a FUTURE
	// request for this model starts a fresh grace wait of its own.
	delete(s.graceWait, initial.Model)
	s.active[initial.Model] = &activeSwap{
		modelID: initial.Model,
		evict:   evict,
		waiters: []HandlerReq{initial},
	}
	s.planner.OnSwapStart(initial.Model, running)
	s.effects.StartSwap(initial.Model, evict)
}

// enqueue inserts req into the queue in order: tier rank DESC first (see
// swaputil.Tier / docs/intent/llama-swap-tiers.md), then the existing per-model
// priority (also DESC), then arrival (FIFO) order — by req.arrivalSeq, NOT by
// call order — for ties on both keys. It goes just before the first queued
// item whose (rank, priority) key is strictly lower than req's, OR whose key
// is tied but which arrived later (a higher arrivalSeq), so higher-rank/
// higher-priority requests are serviced first while equal-key requests keep
// their TRUE arrival order even when one of them is a re-enqueue (OnSwapDone's
// cap re-queue, drainQueue) that lands here well after a later arrival was
// already queued — see fifo_arrival_order_test.go. Priorities come from the
// FifoConfig; unlisted models default to 0.
func (s *FIFO) enqueue(req HandlerReq) {
	rank := req.Tier.Rank
	p := s.cfg.Priority[req.Model]
	i := len(s.queued)
	for j, q := range s.queued {
		if q.Tier.Rank < rank {
			i = j
			break
		}
		if q.Tier.Rank == rank {
			qp := s.cfg.Priority[q.Model]
			if qp < p {
				i = j
				break
			}
			if qp == p && q.arrivalSeq > req.arrivalSeq {
				i = j
				break
			}
		}
	}
	s.queued = append(s.queued, HandlerReq{})
	copy(s.queued[i+1:], s.queued[i:])
	s.queued[i] = req
	broadcastQueuePositions(s.queued)
}

// blockedByRankBarrier reports whether any OTHER queued request has strictly
// higher tier rank than req — the rank barrier: a queued (or about-to-be-
// queued) request is never granted while a strictly higher-rank request is
// still queued, so background work only proceeds once the priority + default
// queues are empty. Compares against every entry rather than relying on queue
// order alone, since a request identical to req (down to its Respond channel)
// must never block itself.
func (s *FIFO) blockedByRankBarrier(req HandlerReq) bool {
	for _, q := range s.queued {
		if q.Respond == req.Respond {
			continue
		}
		if q.Tier.Rank > req.Tier.Rank {
			return true
		}
	}
	return false
}

// drainQueue walks the queued requests in order, re-running the OnRequest
// decision tree against the (now smaller) active set. Items that can now start
// or join become satisfied; items still blocked remain queued in original order
// so they get another chance on the next swap completion.
func (s *FIFO) drainQueue() {
	if len(s.queued) == 0 {
		return
	}
	pending := s.queued
	var remaining []HandlerReq

	// Rank barrier while draining: pending is already ordered rank DESC (see
	// enqueue), so once an item is genuinely stuck (collision, in-flight
	// conflict, or grace), no STRICTLY lower-rank item may be granted this
	// pass either — otherwise it would jump the queue ahead of a still-queued
	// higher-rank request. barrierArmed/barrierRank record the highest rank
	// seen stuck so far; items whose own rank is >= barrierRank are unaffected
	// (the barrier only blocks strictly lower ranks).
	barrierArmed := false
	barrierRank := 0
	stick := func(req HandlerReq) {
		remaining = append(remaining, req)
		if !barrierArmed || req.Tier.Rank > barrierRank {
			barrierArmed = true
			barrierRank = req.Tier.Rank
		}
	}

	for _, req := range pending {
		if barrierArmed && req.Tier.Rank < barrierRank {
			remaining = append(remaining, req)
			continue
		}

		state, ok := s.effects.ModelState(req.Model)
		if !ok {
			s.effects.GrantError(req, ErrModelNotFound)
			continue
		}
		if sw, ok := s.active[req.Model]; ok {
			s.logger.Debugf("%s: queued request for model %s now joining in-flight swap", s.name, req.Model)
			sw.waiters = append(sw.waiters, req)
			continue
		}
		running := s.runningSet(req.Model)
		evict := s.planner.EvictionFor(req.Model, running)
		// Concurrency cap, mirroring the OnRequest branch: the model is still
		// serving its full allowance, so this request keeps waiting.
		if s.atCapacity(req) {
			if s.tryPreempt(req, []string{req.Model}, 1) {
				s.logger.Debugf("%s: preempting same-model in-flight request(s) on %s for queued higher-rank request at concurrency cap", s.name, req.Model)
			}
			stick(req)
			continue
		}
		if state == process.StateReady && len(evict) == 0 && !collidesWith(req.Model, evict, s.active) {
			if !s.kvAdmit(req.Model, req.EstimatedTokens) {
				// Same-model preemption, mirroring the OnRequest fast-path
				// branch (tryPreempt is idempotent per victim — a repeat
				// drain pass re-cancels an already-cancelled context, a no-op).
				if s.tryPreempt(req, []string{req.Model}, 1) {
					s.logger.Debugf("%s: preempting same-model in-flight request(s) on %s for queued higher-rank request", s.name, req.Model)
				}
				s.logKVParked(req.Model, req.EstimatedTokens)
				stick(req)
				continue
			}
			s.logger.Debugf("%s: queued request for model %s now served fast-path", s.name, req.Model)
			s.grantHandler(req, req.Model)
			continue
		}
		if collidesWith(req.Model, evict, s.active) {
			stick(req)
			continue
		}
		if conflictsWithInFlight(evict, s.inFlight) {
			if s.tryPreempt(req, evict, 0) {
				s.logger.Debugf("%s: preempting in-flight request(s) blocking queued model %s", s.name, req.Model)
			}
			stick(req)
			continue
		}
		if s.withinGrace(req.Model, evict) {
			s.noteGraceDeferral(req.Model)
			stick(req)
			continue
		}
		s.logger.Debugf("%s: queued request for model %s now starting swap, evicting %v", s.name, req.Model, evict)
		s.startSwap(req, evict, running)
	}
	s.queued = remaining
	broadcastQueuePositions(s.queued)
}

// runningSet is the live model set handed to the Swapper: every process the
// baseRouter reports as running, unioned with the targets of in-flight swaps
// (excluding excludeActive, the model whose own swap is being decided — its
// in-flight entry must not count as "already running"). The result is sorted so
// eviction decisions derived from it are deterministic.
func (s *FIFO) runningSet(excludeActive string) []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(id string) {
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	for id := range s.effects.RunningModels() {
		add(id)
	}
	for _, id := range activeTargets(s.active, excludeActive) {
		add(id)
	}
	sort.Strings(out)
	return out
}

// activeTargets returns the IDs of every in-flight swap target except exclude.
// The planner uses this to account for models committed to but not yet reflected
// in process state.
func activeTargets(active map[string]*activeSwap, exclude string) []string {
	if len(active) == 0 {
		return nil
	}
	out := make([]string, 0, len(active))
	for id := range active {
		if id == exclude {
			continue
		}
		out = append(out, id)
	}
	return out
}

// collidesWith reports whether a new swap with this target and evict set can
// safely run alongside the currently active swaps. Same-target callers should
// JOIN (handled before this) — they do not collide with themselves.
func collidesWith(target string, evict []string, active map[string]*activeSwap) bool {
	for id, sw := range active {
		if id == target {
			continue
		}
		if containsString(evict, id) {
			return true
		}
		if containsString(sw.evict, target) {
			return true
		}
		if slicesOverlap(evict, sw.evict) {
			return true
		}
	}
	return false
}

// slicesOverlap reports whether xs and ys share any common element.
func slicesOverlap(xs, ys []string) bool {
	for _, x := range xs {
		if containsString(ys, x) {
			return true
		}
	}
	return false
}

// conflictsWithInFlight reports whether any model in evict is still handling
// requests. Stopping a busy process would cancel its callers' connections, so
// the scheduler defers the swap until those callers finish.
func conflictsWithInFlight(evict []string, inFlight map[string]int) bool {
	for _, m := range evict {
		if inFlight[m] > 0 {
			return true
		}
	}
	return false
}

func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// broadcastQueuePositions sends each queued request its current 1-indexed
// position. Sends are non-blocking: if the channel is full, the old value is
// drained first so the consumer always sees the latest position.
func broadcastQueuePositions(queued []HandlerReq) {
	for i, req := range queued {
		pos := i + 1
		select {
		case req.PositionCh <- pos:
		default:
			select {
			case <-req.PositionCh:
			default:
			}
			select {
			case req.PositionCh <- pos:
			default:
			}
		}
	}
}
