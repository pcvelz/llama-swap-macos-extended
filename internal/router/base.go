package router

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/router/scheduler"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// maxReplays caps how many times a single client request may be
// transparently re-enqueued after a zero-byte preemption (see ServeHTTP)
// before falling back to the v1 cancel+503
// (docs/intent/llama-swap-tiers.md "Known v1 limitations" -> v2). A hard
// ceiling independent of maxReplayHeld: a pathological rapid-fire preemption
// loop (each attempt preempted within milliseconds of being granted) would
// otherwise hold the connection — and this model's serving slot, which is only
// released when ServeHTTP finally returns — for
// effectively unbounded wall-clock time without ever approaching the
// held-time budget below.
const maxReplays = 5

// maxReplayHeld caps the total wall-clock time (measured once, across every
// attempt — not restarted per attempt) a request may be held for transparent
// replay before falling back to the v1 503. Deliberately under the
// interactive client's own ~300s zero-byte abort ceiling (llama-cm
// llama/llama-swap.yaml, "THE 300s TIME-TO-FIRST-BYTE CEILING" note) so a
// request that eventually gives up still has time left to receive and act on
// the v1 503 before the client's OWN timeout fires first and aborts the
// connection out from under us.
//
// A var, not a const, purely so tests can shorten it (the sequenced pinger
// below keys its eligibility-surrender moment off this same deadline).
var maxReplayHeld = 270 * time.Second

// parkGiveUpBudget bounds how long a PREEMPTIBLE request may sit parked
// waiting to be granted a serving slot before llama-swap gives up on it
// itself and answers with the canonical preempt-503 (see writePreemptGiveUp).
//
// WHY (llama-cm incident
// 2026-08-18-cq27-background-admission-park-bare-502-retry-storm): the park
// had no server-side time bound at all. A background-tier request behind a
// monopolized slot simply waited until the CLIENT's own ~300s zero-byte abort
// cancelled its context, at which point ServeHTTP returned BARE - writing
// nothing. That rendered in the access log as `502 0` (the reverse proxy's own
// status) or `200 0` (nothing wrote a status at all), always at a metronomic
// 5m00, and Claude Code 0s-retried it into a self-sustaining storm. Answering
// with a real 503 + Retry-After BEFORE the client's ceiling is what makes the
// client back off instead.
//
// Set to maxReplayHeld: the same "comfortably under the client's ~300s
// zero-byte abort" reasoning applies verbatim, and a request that has been
// parked this long has already blown the replay budget anyway. A var, not a
// const, purely so tests can shorten it.
var parkGiveUpBudget = maxReplayHeld

type shutdownReq struct {
	timeout time.Duration
	respond chan error
}

type unloadReq struct {
	targets []string
	timeout time.Duration
	respond chan struct{}
}

// baseRouter owns the channels, run-loop, and process machinery shared by every
// concrete router. Concrete routers embed *baseRouter and supply a
// scheduler.Swapper describing how eviction sets are decided. baseRouter
// implements scheduler.Effects so the scheduler can call back for side-effects.
type baseRouter struct {
	name      string
	config    config.Config
	processes map[string]process.Process
	logger    *logmon.Monitor
	schedule  scheduler.Scheduler

	// shutdownCtx governs the request machinery: cancelling it tells grant()
	// and ServeHTTP to stop granting and reject callers. It is deliberately
	// separate from procCtx — see procCtx below.
	shutdownCtx  context.Context
	shutdownFn   context.CancelFunc
	shuttingDown atomic.Bool

	// procCtx is the parent context for every managed process and governs
	// process lifetime only. handleShutdown stops processes gracefully via
	// Stop() and cancels procCtx afterwards, so teardown is never a context
	// cancel racing the graceful path (which collapsed the grace to 100ms and
	// let the caller return before children were reaped — see process run loop).
	procCtx    context.Context
	procCancel context.CancelFunc

	handlerCh   chan scheduler.HandlerReq
	cancelCh    chan scheduler.HandlerReq
	shutdownCh  chan shutdownReq
	unloadCh    chan unloadReq
	swapDoneCh  chan scheduler.SwapDone
	serveDoneCh chan scheduler.ServeDoneEvent

	runDone chan struct{}

	// testProcessed, when non-nil, receives one event after each handlerReq
	// or swapDone has been fully processed by run(). Tests use it to wait
	// for run() to reach a deterministic state without sleeping. serveDone
	// events are intentionally NOT signalled here so test event counts
	// remain stable.
	testProcessed chan struct{}

	// pinned maps a pinned model ID to its lease deadline. A zero time.Time
	// means a permanent pin (the pre-lease behavior, still used when a pin
	// request carries no ttl). A non-zero deadline is a lease: IsPinned
	// treats a deadline that has passed as NOT pinned even before this
	// entry is swept out of the map, so enforcement never depends on sweep
	// timing (crash-safety: a client that dies without unpinning must never
	// wedge the box).
	pinnedMu sync.RWMutex
	pinned   map[string]time.Time

	// graceTick, when > 0, arms a periodic OnTick in the run loop so swap-grace
	// deferrals are re-evaluated during pure idle (when no serve/swap event
	// fires). 0 disables the ticker entirely — zero overhead when no model has a
	// swap-grace configured.
	graceTick time.Duration
}

func newBaseRouter(
	name string,
	conf config.Config,
	processes map[string]process.Process,
	logger *logmon.Monitor,
	planner scheduler.Swapper,
) (*baseRouter, error) {
	shutdownCtx, shutdownFn := context.WithCancel(context.Background())
	procCtx, procCancel := context.WithCancel(context.Background())
	b := &baseRouter{
		name:        name,
		config:      conf,
		processes:   processes,
		logger:      logger,
		shutdownCtx: shutdownCtx,
		shutdownFn:  shutdownFn,
		procCtx:     procCtx,
		procCancel:  procCancel,
		handlerCh:   make(chan scheduler.HandlerReq),
		cancelCh:    make(chan scheduler.HandlerReq),
		shutdownCh:  make(chan shutdownReq),
		unloadCh:    make(chan unloadReq),
		swapDoneCh:  make(chan scheduler.SwapDone),
		serveDoneCh: make(chan scheduler.ServeDoneEvent),
		runDone:     make(chan struct{}),
		pinned:      make(map[string]time.Time),
	}

	// Arm the run-loop ticker only when at least one model has a swap-grace
	// configured, so the default (upstream) config pays no ticker overhead.
	// The scheduler derives the per-model grace durations from the same
	// config (see scheduler.NewFIFO).
	for _, mc := range conf.Models {
		if mc.SwapGraceSeconds > 0 {
			b.graceTick = time.Second
			break
		}
	}

	sched, err := scheduler.New(conf, name, logger, planner, b)
	if err != nil {
		return nil, err
	}
	b.schedule = sched
	return b, nil
}

func (b *baseRouter) notifyProcessed() {
	if b.testProcessed != nil {
		b.testProcessed <- struct{}{}
	}
}

func (b *baseRouter) run() {
	defer close(b.runDone)

	// A nil channel blocks forever in select, so when no swap-grace is configured
	// (graceTick == 0) the tick case never fires and adds zero overhead.
	var tickC <-chan time.Time
	if b.graceTick > 0 {
		ticker := time.NewTicker(b.graceTick)
		defer ticker.Stop()
		tickC = ticker.C
	}

	for {
		select {
		case req := <-b.shutdownCh:
			b.handleShutdown(req)
			return

		case req := <-b.handlerCh:
			b.schedule.OnRequest(req)
			b.notifyProcessed()

		case req := <-b.cancelCh:
			b.schedule.OnCancel(req)
			b.notifyProcessed()

		case req := <-b.unloadCh:
			b.schedule.OnUnload(req.targets, req.timeout)
			close(req.respond)
			b.notifyProcessed()

		case ev := <-b.swapDoneCh:
			b.schedule.OnSwapDone(ev)
			b.notifyProcessed()

		case ev := <-b.serveDoneCh:
			b.schedule.OnServeDone(ev)

		case <-tickC:
			b.schedule.OnTick()
		}
	}
}

// grant sends a response back to the caller of ServeHTTP and tells us
// whether the caller was still there to receive it.
//
// Each ServeHTTP creates a fresh, UNBUFFERED respond channel and parks in
// a select waiting on it. "Unbuffered" is the important word: a send only
// completes when the other side is actively receiving. So if this send
// succeeds, we know for a fact the caller picked up the response and will
// act on it. If the caller has already given up (its request context was
// cancelled, e.g. the HTTP client disconnected) or the router is shutting
// down, the send never lands, one of the other select cases fires, and we
// report back that the grant did NOT happen.
//
// That distinction matters for in-flight bookkeeping — see GrantServe.
func (b *baseRouter) grant(req scheduler.HandlerReq, resp scheduler.HandlerResp) bool {
	select {
	case req.Respond <- resp:
		return true
	case <-req.Ctx.Done():
		return false
	case <-b.shutdownCtx.Done():
		return false
	}
}

// cancelParked tells the run loop to prune hr from the scheduler's queue and
// from every in-flight swap's waiter list. Every path on which ServeHTTP walks
// away from a request it already handed to the scheduler must call this, or
// drainQueue would eventually start a model load for a caller that is gone.
func (b *baseRouter) cancelParked(hr scheduler.HandlerReq) {
	select {
	case b.cancelCh <- hr:
	case <-b.shutdownCtx.Done():
	}
}

// armParkGiveUp returns the channel that fires when a parked request has
// waited out parkGiveUpBudget, or nil when this request must never be given up
// on. Nil disables the case entirely (a receive on a nil channel blocks
// forever), so the select degrades to exactly its previous shape.
//
// Eligibility is deliberately narrow - this is the preemption core, and the
// population that actually suffers the bare give-up is small and specific:
//
//   - tier.Preemptible only. A non-preemptible (interactive) request is not
//     what the incident observed (100% of the occurrences were
//     tier=background) and giving up on one would be a visible regression for
//     paid interactive traffic.
//   - responseCommitted must be false. A streaming Anthropic request gets a
//     pingWriter (see isAnthropicStreamPath above) whose SSE keepalives have
//     long since committed a 200 and real bytes by the time this budget would
//     expire. Those requests are NOT the zero-byte victim class - the pings are
//     what keep the client from aborting at ~300s in the first place - so
//     giving up on them would abort a request that is currently kept alive and
//     eventually served, and the 503 could not even be written over the
//     committed status line.
//
// The deadline measures from replayStart (request arrival), so it is one
// budget across every replay attempt rather than a fresh one per attempt -
// the same accounting maxReplayHeld uses.
func armParkGiveUp(tier swaputil.Tier, responseCommitted bool, replayStart time.Time) (<-chan time.Time, func()) {
	if !tier.Preemptible || responseCommitted {
		return nil, func() {}
	}
	remaining := parkGiveUpBudget - time.Since(replayStart)
	if remaining < 0 {
		remaining = 0
	}
	t := time.NewTimer(remaining)
	return t.C, func() { t.Stop() }
}

// deadlineBudget is how much of the client's ~300s zero-byte wall a request
// may actually spend, measured from arrival. Identical construction to
// maxReplayHeld/parkGiveUpBudget: the wall minus a ~30s safety margin, so a
// refusal still reaches the client before its own abort fires. Its own var
// (rather than an alias of parkGiveUpBudget) so a test can shorten the park
// budget and the deadline budget independently and tell the two give-up
// classes apart.
var deadlineBudget = maxReplayHeld

// defaultPrefillRate is the conservative prompt-prefill throughput assumed for
// a model with no config.ModelConfig.PrefillTokensPerSecond entry, in tokens
// per second. 75 t/s is the low end of the cq27-class deep-prefill range
// measured on the production box (75-80 t/s); assuming the LOW end makes the
// estimated prefill time an OVER-estimate, which is the safe direction for a
// refusal decision only in the sense that it refuses earlier - it is
// deliberately paired with the narrow eligibility below so that
// over-refusal can never touch interactive traffic.
const defaultPrefillRate = 75.0

// prefillRate resolves the per-model prefill throughput used by
// deadlineRefuse. Unconfigured (or non-positive) falls back to
// defaultPrefillRate; there is deliberately NO measured/EMA estimator here.
func (b *baseRouter) prefillRate(modelID string) float64 {
	if mc, _, ok := b.config.FindConfig(modelID); ok && mc.PrefillTokensPerSecond > 0 {
		return mc.PrefillTokensPerSecond
	}
	return defaultPrefillRate
}

// deadlineRefuse answers the GRANTED-BUT-SLOW class: a background request whose
// prefill demonstrably cannot finish inside the client's remaining zero-byte
// budget. Serving it (or parking it toward a give-up) burns a slot for minutes
// and still ends in a client-side abort, so llama-swap refuses it up front with
// the canonical preempt-503 - the same shape, and the same "make the client
// back off instead of 0s-retrying" reasoning, as the park give-up above.
//
// Eligibility mirrors armParkGiveUp EXACTLY and for the same two reasons:
//
//   - tier.Preemptible only. A non-preemptible (interactive) request is NEVER
//     deadline-refused, however large its context: refusing paid interactive
//     traffic on an ESTIMATE would be a far worse regression than the
//     starvation this fixes.
//   - the response must still be uncommitted (no keepalive pings, no loading
//     writer). A committed response cannot carry a 503, and a keepalive-armed
//     request is not racing a zero-byte wall in the first place.
//
// Returns true when it has written the refusal and the caller must return.
func (b *baseRouter) deadlineRefuse(w http.ResponseWriter, data swaputil.ReqContextData, responseCommitted bool, estimatedTokens int, replayStart time.Time) bool {
	if !data.Tier.Preemptible || responseCommitted {
		return false
	}
	estSeconds := float64(estimatedTokens) / b.prefillRate(data.ModelID)
	remainingSeconds := (deadlineBudget - time.Since(replayStart)).Seconds()
	if estSeconds <= remainingSeconds {
		return false
	}
	b.logger.Infof("deadline-refuse: tier=%s model=%s est_tokens=%d est_s=%.0f remaining_s=%.0f",
		data.Tier.Name, data.ModelID, estimatedTokens, estSeconds, remainingSeconds)
	writePreemptGiveUp(w)
	return true
}

// giveUpParked is the shared body of both park-stage give-ups. Order is
// load-bearing:
//
//  1. reqCancel FIRST. baseRouter.grant blocks on an UNBUFFERED send to
//     hr.Respond and escapes only via hr.Ctx.Done() or shutdown. The instant
//     this goroutine stops receiving on Respond while its context is still
//     alive, a concurrent grant from the run loop would block there forever -
//     deadlocking the whole router, every model, every tier. Cancelling first
//     makes grant return false, which also makes scheduler.FIFO.grantHandler
//     skip its inFlight++/KV reservation, so no counter is stranded either.
//     (The send itself cannot have been lost: an unbuffered send completes
//     only when a receiver takes it, so if select chose this case instead,
//     nothing was handed over.)
//  2. Write the canonical 503 - before the client's own ~300s zero-byte abort,
//     which is the whole point: the client sees a real status with Retry-After
//     and backs off instead of 0s-retrying into a storm. This comes BEFORE the
//     prune because the prune blocks on the run loop: if the loop is itself
//     wedged (the only condition under which the admission wait can park at
//     all), pruning first would hold the 503 hostage to the very wedge the
//     client is timing out on. Step 1 already guarantees a late grant returns
//     false, so the two are safely swappable.
//  3. Prune from the scheduler, so drainQueue never starts a model load for a
//     caller that has been answered and left.
func (b *baseRouter) giveUpParked(w http.ResponseWriter, hr scheduler.HandlerReq, reqCancel context.CancelFunc, stage string, data swaputil.ReqContextData, replayStart time.Time) {
	reqCancel()
	b.logger.Infof("park-giveup: tier=%s model=%s stage=%s held=%.0fs",
		data.Tier.Name, data.ModelID, stage, time.Since(replayStart).Seconds())
	writePreemptGiveUp(w)
	b.cancelParked(hr)
}

// ModelState implements scheduler.Effects.
func (b *baseRouter) ModelState(modelID string) (process.ProcessState, bool) {
	p, ok := b.processes[modelID]
	if !ok {
		var zero process.ProcessState
		return zero, false
	}
	return p.State(), true
}

// StartSwap implements scheduler.Effects, launching the swap goroutine.
func (b *baseRouter) StartSwap(modelID string, evict []string) {
	go b.doSwap(modelID, evict)
}

// GrantError implements scheduler.Effects.
func (b *baseRouter) GrantError(req scheduler.HandlerReq, err error) {
	b.grant(req, scheduler.HandlerResp{Err: err})
}

// GrantServe implements scheduler.Effects. It hands the caller a wrapped
// p.ServeHTTP (via trackedServe) so the run loop hears about the request
// finishing, and reports whether the caller received it. The scheduler bumps
// its in-flight count only on a true return: if grant() returns false the
// caller already walked away and trackedServe will never run, so no matching
// decrement will ever arrive — incrementing would strand the counter at >0 and
// the router would never again be willing to evict this model.
func (b *baseRouter) GrantServe(req scheduler.HandlerReq, modelID string) bool {
	p := b.processes[modelID]
	return b.grant(req, scheduler.HandlerResp{HandleFunc: b.trackedServe(modelID, p, req.EstimatedTokens, req.Preempted, req.ReplayWanted)})
}

// StopProcesses implements scheduler.Effects, stopping the named processes in
// parallel and blocking until all have stopped.
func (b *baseRouter) StopProcesses(timeout time.Duration, ids []string) {
	var wg sync.WaitGroup
	for _, id := range ids {
		p, ok := b.processes[id]
		if !ok {
			continue
		}
		wg.Add(1)
		go func(id string, p process.Process) {
			defer wg.Done()
			if err := p.Stop(timeout); err != nil {
				b.logger.Warnf("%s: stopping %s failed: %v", b.name, id, err)
			}
		}(id, p)
	}
	wg.Wait()
}

// trackedServe is the wrapper that closes the loop on in-flight tracking.
// It runs p.ServeHTTP normally; the only added behaviour is a deferred
// send on serveDoneCh after the handler returns. That send is what tells
// the run loop "this model now has one fewer request in flight — go look
// at the queue again, you may be able to start a swap you previously had
// to defer." estimatedTokens echoes back the KV-admission reservation (see
// scheduler.FIFO.grantHandler / releaseKV) this request holds, if any, so the
// scheduler can release exactly what it reserved — on every exit path from
// p.ServeHTTP, since this send is unconditional in the deferred func, not
// gated on success.
//
// The select on shutdownCtx.Done() is a release valve: if the router is
// already shutting down, nobody is reading serveDoneCh, so we drop the
// notification rather than blocking the HTTP goroutine forever.
// preempted, when non-nil, is the same flag scheduler.HandlerReq.Preempt sets
// just before it cancels this request's serving context (see fifo.go
// tryPreempt). trackedServe wraps w so that cancel surfaces as a 503 +
// X-LlamaSwap-Preempted header — best effort, see preempt.go — instead of
// whatever the upstream reverse proxy would otherwise write. replayWanted,
// when non-nil, is the same flag scheduler.HandlerReq.ReplayWanted carries —
// see preemptResponseWriter's type doc for the v2 replay behavior it enables.
func (b *baseRouter) trackedServe(modelID string, p process.Process, estimatedTokens int, preempted *atomic.Bool, replayWanted *atomic.Bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			select {
			case b.serveDoneCh <- scheduler.ServeDoneEvent{ModelID: modelID, EstimatedTokens: estimatedTokens}:
			case <-b.shutdownCtx.Done():
			}
		}()
		p.ServeHTTP(newPreemptResponseWriter(w, preempted, replayWanted), r)
	}
}

// Eviction is a LAST resort, never routine: unloading a resident model forces a
// full re-prefill downstream (llama-cm docs/intent/llama-swap-backend.md § Slot
// stability). Only reach here after drain + swapGraceSeconds have run out.
func (b *baseRouter) doSwap(modelID string, toStop []string) {
	timeout := b.healthCheckTimeout()

	var wg sync.WaitGroup
	for _, mID := range toStop {
		wg.Add(1)
		go func(p process.Process, id string) {
			defer wg.Done()
			// Swap evictions were previously log-silent at INFO, making
			// residency changes invisible (2026-07-05 forensics).
			b.logger.Infof("%s: swap eviction: unloading <%s> to make room for <%s>", b.name, id, modelID)
			if err := p.Stop(timeout); err != nil {
				b.logger.Warnf("%s: stopping %s failed: %v", b.name, id, err)
			}
		}(b.processes[mID], mID)
	}
	wg.Wait()

	// EnsureReady rather than a State() check followed by Run: the router must
	// not assume anything about the process. Deciding out here means acting on
	// a snapshot that the process's own run loop can invalidate at any moment —
	// a TTL unload landing in that window used to leave the swap waiting on a
	// process nobody was ever going to start (issue #946). EnsureReady makes
	// the same decision inside the process, where the state is owned.
	target := b.processes[modelID]
	err := target.EnsureReady(b.shutdownCtx, timeout)
	if err != nil && b.shutdownCtx.Err() == nil {
		// Quiet during shutdown: every in-flight swap fails at once there, and
		// that is expected rather than worth a warning per model.
		b.logger.Warnf("%s: starting %s failed: %v", b.name, modelID, err)
	}

	select {
	case b.swapDoneCh <- scheduler.SwapDone{ModelID: modelID, Err: err}:
	case <-b.shutdownCtx.Done():
	}
}

func (b *baseRouter) handleShutdown(req shutdownReq) {
	shutdownErr := fmt.Errorf("%s is shutting down", b.name)

	// Cancel shutdownCtx first so any waiter that is currently parked on
	// its respond channel can exit via its own shutdownCtx.Done() branch.
	// The OnShutdown grants below then either land (waiter happened to receive
	// before noticing shutdown) or fall through immediately via grant's
	// shutdownCtx case — either way the waiter sees a non-OK response.
	// This does NOT touch processes: their lifetime is procCtx, cancelled
	// only after the graceful Stop() calls below have reaped them.
	b.shutdownFn()

	b.schedule.OnShutdown(shutdownErr)

	stopTimeout := req.timeout
	if stopTimeout <= 0 {
		stopTimeout = b.healthCheckTimeout()
	}

	var wg sync.WaitGroup
	for i, p := range b.processes {
		wg.Add(1)
		go func(id string, p process.Process) {
			defer wg.Done()
			if err := p.Stop(stopTimeout); err != nil {
				b.logger.Warnf("%s failed to stop process %s: %v", b.name, id, err)
			}
		}(i, p)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	if req.timeout > 0 {
		select {
		case <-done:
		case <-time.After(req.timeout):
			<-done
		}
	} else {
		<-done
	}

	// Every process is stopped (children reaped via Stop()). Cancel procCtx so
	// the process run-loop goroutines exit; they are already StateStopped, so
	// this is a clean no-op kill rather than a forced teardown.
	b.procCancel()

	req.respond <- nil
}

func (b *baseRouter) healthCheckTimeout() time.Duration {
	t := time.Duration(b.config.HealthCheckTimeout) * time.Second
	if t <= 0 {
		return 30 * time.Second
	}
	return t
}

// unloadTimeout returns the graceful stop timeout for a model. Config parsing
// guarantees both the global and per-model unloadTimeout are populated (a zero
// model value is rewritten to the global default on parse), so no zero handling
// is needed here.
func (b *baseRouter) unloadTimeout(modelID string) time.Duration {
	if mc, ok := b.config.Models[modelID]; ok {
		return time.Duration(mc.UnloadTimeout) * time.Second
	}
	return time.Duration(b.config.UnloadTimeout) * time.Second
}

func (b *baseRouter) Handles(model string) bool {
	_, ok := b.processes[model]
	return ok
}

func (b *baseRouter) ProcessLogger(modelID string) (*logmon.Monitor, bool) {
	if p, ok := b.processes[modelID]; ok {
		return p.Logger(), true
	}
	return nil, false
}

func (b *baseRouter) ProcessLastUse(modelID string) (time.Time, bool) {
	p, ok := b.processes[modelID]
	if !ok {
		return time.Time{}, false
	}
	return p.LastUse(), true
}

// Pin marks modelID as permanently pinned (no expiry) - the pre-lease
// behavior. Equivalent to PinWithTTL(modelID, 0).
func (b *baseRouter) Pin(modelID string) {
	b.PinWithTTL(modelID, 0)
}

// PinWithTTL pins modelID and returns the deadline that was stored (the
// zero time.Time for a permanent pin, when ttl <= 0). Re-pinning an
// already-pinned model refreshes its deadline - this is the lease-refresh
// path a client re-sends periodically to keep its session's model
// resident, and it must stay cheap/race-safe since it can fire often.
func (b *baseRouter) PinWithTTL(modelID string, ttl time.Duration) time.Time {
	b.pinnedMu.Lock()
	defer b.pinnedMu.Unlock()
	var deadline time.Time
	if ttl > 0 {
		deadline = time.Now().Add(ttl)
	}
	b.pinned[modelID] = deadline
	return deadline
}

func (b *baseRouter) Unpin(modelID string) {
	b.pinnedMu.Lock()
	defer b.pinnedMu.Unlock()
	delete(b.pinned, modelID)
}

// IsPinned reports whether modelID is currently pinned. A leased pin whose
// deadline has passed is reported as unpinned even if it has not yet been
// swept from the map - callers must never depend on sweep cadence for
// correctness - and this call opportunistically performs that sweep so an
// abandoned lease does not linger in the map indefinitely.
func (b *baseRouter) IsPinned(modelID string) bool {
	b.pinnedMu.RLock()
	deadline, ok := b.pinned[modelID]
	b.pinnedMu.RUnlock()
	if !ok {
		return false
	}
	if deadline.IsZero() || time.Now().Before(deadline) {
		return true
	}

	// Lease expired: sweep it under the write lock so a client that died
	// without unpinning cannot wedge the box - after this, normal idle-TTL
	// rules apply to the model like any other unpinned process.
	b.pinnedMu.Lock()
	defer b.pinnedMu.Unlock()
	// Re-check under the write lock: Pin/PinWithTTL may have refreshed the
	// lease between the RUnlock above and this Lock.
	if d, ok := b.pinned[modelID]; ok && !d.IsZero() && !time.Now().Before(d) {
		delete(b.pinned, modelID)
	}
	return false
}

// PinExpiry reports the pin state and lease deadline for modelID: pinned is
// false when the model is not pinned or its lease has expired (in which case
// the entry is swept, same as IsPinned); deadline is the zero time.Time for
// a permanent pin.
func (b *baseRouter) PinExpiry(modelID string) (deadline time.Time, pinned bool) {
	if !b.IsPinned(modelID) {
		return time.Time{}, false
	}
	b.pinnedMu.RLock()
	defer b.pinnedMu.RUnlock()
	return b.pinned[modelID], true
}

// wirePinCallbacks wires the allowIdleEvict callback on every ProcessCommand
// so the TTL goroutine consults the pinned map (via IsPinned, which also
// sweeps expired leases) before evicting. This is the only per-second
// consultation point for most models, which is why IsPinned itself performs
// the sweep rather than requiring a separate goroutine.
func (b *baseRouter) wirePinCallbacks() {
	for id, p := range b.processes {
		id := id
		if pc, ok := p.(*process.ProcessCommand); ok {
			pc.SetAllowIdleEvict(func() bool {
				return !b.IsPinned(id)
			})
		}
	}
}

// RunningModels returns the current state of every process that is not stopped
// or shut down. The processes map keys are fixed at construction and State()
// is a snapshot, so this is safe to call without the run loop.
func (b *baseRouter) RunningModels() map[string]process.ProcessState {
	running := make(map[string]process.ProcessState)
	for id, p := range b.processes {
		st := p.State()
		if st == process.StateStopped || st == process.StateShutdown {
			continue
		}
		running[id] = st
	}
	return running
}

// Unload stops the named models, or every running model when none are named.
// It blocks until each targeted process has stopped.
//
// The request is funneled through the run loop so eviction is coordinated
// with the rest of the router's state: pending swap waiters for an
// unloaded model are released with an error, queued requests for unloaded
// models are dropped, and any deferred swaps that were waiting on those
// models become eligible to start.
//
// In-flight requests being served by an unloaded process are not waited
// for — Stop kills the upstream, those callers see whatever error the
// reverse proxy surfaces and may retry. Their trackedServe defers fire
// normally and decrement inFlight as the dying handlers return.
//
// A timeout <= 0 unloads each targeted model with its configured
// unloadTimeout: targets sharing a timeout are stopped in parallel within one
// unload request, and the requests are processed smallest timeout first. The
// requests are sequential, so a hung stop on a large model (long timeouts
// usually mean multi-node unloads) cannot delay reclaiming the quick ones
// queued behind it. A positive timeout overrides the configured values and
// stops every target with that timeout.
func (b *baseRouter) Unload(timeout time.Duration, models ...string) {
	targets := models
	if len(targets) == 0 {
		targets = make([]string, 0, len(b.processes))
		for id := range b.processes {
			targets = append(targets, id)
		}
	}
	if len(targets) == 0 {
		return
	}

	if timeout > 0 {
		b.sendUnload(targets, timeout)
		return
	}
	buckets := make(map[time.Duration][]string)
	for _, id := range targets {
		t := b.unloadTimeout(id)
		buckets[t] = append(buckets[t], id)
	}
	timeouts := make([]time.Duration, 0, len(buckets))
	for t := range buckets {
		timeouts = append(timeouts, t)
	}
	sort.Slice(timeouts, func(i, j int) bool { return timeouts[i] < timeouts[j] })
	for _, t := range timeouts {
		b.sendUnload(buckets[t], t)
	}
}

// sendUnload funnels one unload request through the run loop and blocks until
// the scheduler has stopped the targeted processes.
func (b *baseRouter) sendUnload(targets []string, timeout time.Duration) {
	req := unloadReq{targets: targets, timeout: timeout, respond: make(chan struct{})}
	select {
	case b.unloadCh <- req:
	case <-b.runDone:
		return
	}
	<-req.respond
}

func (b *baseRouter) Shutdown(timeout time.Duration) error {
	if !b.shuttingDown.CompareAndSwap(false, true) {
		return fmt.Errorf("%s shutdown already in progress", b.name)
	}
	req := shutdownReq{timeout: timeout, respond: make(chan error, 1)}
	select {
	case b.shutdownCh <- req:
	case <-b.runDone:
		return nil
	}
	return <-req.respond
}

// isCountTokensPath reports whether p is a tokenize-only count_tokens request
// (Anthropic /v1/messages/count_tokens and its versioned variants). These do no
// generation and hold no model slot, so they are exempt from the per-model
// concurrency cap — see HandlerReq.ConcurrencyExempt.
func isCountTokensPath(p string) bool {
	return strings.HasSuffix(p, "/count_tokens")
}

// isSlotFreeRequest reports whether r can be served without occupying a
// model serving slot, which is exactly the population that must bypass the
// per-model concurrency cap (HandlerReq.ConcurrencyExempt).
//
// Two classes qualify:
//   - count_tokens (see isCountTokensPath);
//   - any GET/HEAD. llama-server runs inference only on POST; its GET
//     surface (/slots, /props, /metrics, /health, /models, the UI) is
//     read-only status, which is what /upstream/<model>/... polls forward.
//
// WHY this matters beyond queueing: at capacity a non-exempt arrival takes
// the scheduler's preemption branch (scheduler.FIFO.tryPreempt), and a
// main-listener poll carries the default tier, rank 0 — strictly above the
// background tier every interactive cq* session rides. Without this rule a
// status poll cancelled live prefills for a request that never needed a
// slot (llama-cm incident 2026-07-05-cq27-toolgap-eviction-reprefill-storm,
// 2026-09-01 follow-up).
func isSlotFreeRequest(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	return isCountTokensPath(r.URL.Path)
}

func (b *baseRouter) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if b.shuttingDown.Load() {
		swaputil.SendError(w, req, fmt.Errorf("%s is shutting down", b.name))
		return
	}

	data, err := swaputil.FetchContext(req, b.config)
	if err != nil {
		swaputil.SendError(w, req, err)
		return
	}

	// Ignored websocket connections are deliberately kept outside the
	// scheduler: they cannot start or queue a model, consume concurrency, or
	// prevent another request from swapping the process out. A process may stop
	// immediately after this readiness check; dropping that websocket is the
	// intended tradeoff of opting out of lifecycle tracking.
	if swaputil.ShouldIgnoreWebsocket(req, b.config) {
		p, ok := b.processes[data.ModelID]
		if !ok {
			swaputil.SendError(w, req, scheduler.ErrModelNotFound)
			return
		}
		if p.State() != process.StateReady {
			swaputil.SendResponse(w, req, http.StatusConflict,
				fmt.Sprintf("model %s is not loaded; ignored websocket requests cannot start it", data.ModelID))
			return
		}
		p.ServeHTTP(w, req)
		return
	}

	// Anthropic-path keepalive: /v1/messages streams that stay byte-less while
	// parked behind a swap, loading, or mid-prefill get killed by client-side
	// zero-byte timeouts (~300s). Wrap the writer so silent waits emit legal
	// SSE ping events; fast requests pass through untouched (see pinging.go).
	// keepaliveArmed doubles as "this response will have committed a status
	// line and real bytes long before any park budget expires" - see
	// armParkGiveUp for why that disqualifies the request from being given up
	// on.
	//
	// SEQUENCING (not wiring): pinging and transparent replay are mutually
	// exclusive by construction - the first ping commits a 200, and a replay
	// requires a response with zero committed bytes. So a request that is still
	// replay-ELIGIBLE stays silent, and the pinger is unmuted the moment that
	// eligibility is surrendered: from then on nothing can replay this request
	// and keeping it alive past the client's wall is strictly better than
	// silence. Replay eligibility at arrival is exactly tier.Preemptible (see
	// replayEligible in the loop: replayCount is 0 and no time has passed yet),
	// so that is the mute condition here.
	keepaliveArmed := false
	var pw *pingWriter
	var stall *peerStallGuard
	if data.Streaming && data.SendLoadingState && isAnthropicStreamPath(req.URL.Path) {
		// Dead-peer slot reclaim rides on the pingWriter because that is the
		// wrapper that already owns every byte going to the client on this
		// path - and because /v1/messages streams ARE the population that can
		// be frozen mid-turn while holding a slot. config.PeerStallConfig
		// decides whether it is armed at all (see peerstall.go). The same guard
		// carries the no-forward-progress verdict (config.SlotStallConfig), so
		// both reclaim reasons share one latch and one cancel.
		stall = newPeerStallGuard(b.logger, data.ModelID,
			b.config.PeerStall.StallTimeout(), b.config.SlotStall.StallTimeout())
		pw = newPingWriter(b.logger, data.ModelID, w, !data.Tier.Preemptible, stall)
		defer pw.stop()
		w = pw
		keepaliveArmed = true
		if data.Tier.Preemptible {
			// The TIME-based half of the surrender. The count-based half is
			// re-evaluated at the top of every attempt (see replayEligible
			// below), but a request parked on attempt 1 never re-enters that
			// code, so its maxReplayHeld expiry would otherwise be unobservable
			// and the request would stay silent straight through the client's
			// wall - a regression on exactly the population the pinger exists
			// for. The timer fires at the same instant replayEligible would
			// start reporting false; the pinger picks it up on its next tick.
			surrender := time.AfterFunc(maxReplayHeld, pw.allowPings)
			defer surrender.Stop()
		}
	}

	// arrivalCtx anchors every attempt below (see the per-attempt context
	// derivation for why attempt 1 alone may reuse a swaputil.PreemptHandle
	// context directly, while replay attempts never do). origBody is the
	// body swaputil.FetchContext already buffered once at arrival — reused
	// verbatim on every replay attempt so a retry sends the client's EXACT
	// original bytes, never a second read of an already-drained r.Body.
	arrivalCtx := req.Context()
	origBody := data.Body

	// replayStart/replayCount implement the transparent-replay cap
	// (docs/intent/llama-swap-tiers.md "Known v1 limitations" -> v2, the
	// documented later enhancement over the v1 cancel+503): a preemption
	// victim that has written zero response bytes is, within budget,
	// silently re-submitted instead of being handed a 503. See
	// maxReplays/maxReplayHeld above for the two independent caps.
	replayStart := time.Now()
	replayCount := 0

	// Estimated once from the arrival body (identical on every replay attempt -
	// origBody is the same bytes) and used both for KV admission and for the
	// deadline check below. See swaputil.EstimateTokens for the estimation rule.
	estimatedTokens := swaputil.EstimateTokens(origBody)

	for {
		attempt := replayCount + 1

		// DEADLINE-AWARE ADMISSION, on the UNCONDITIONAL path: every attempt
		// passes through here before it is handed to the scheduler at all, so
		// this cannot hide behind a feature that is off in production. It
		// deliberately does NOT live in scheduler.FIFO.kvAdmit: that gate is
		// inert for any model without a kvPoolTokens budget (pool=0 is what the
		// production cq27 runs with), and a check that only fires for configured
		// pools is exactly the defect class this is answering.
		if b.deadlineRefuse(w, data, keepaliveArmed, estimatedTokens, replayStart) {
			return
		}

		// A cancellable derivative of the request's own context: once
		// granted, this becomes the context p.ServeHTTP actually serves
		// under (req is rebound to it below), so the FIFO scheduler's
		// preemption branch can server-side-abort a running request by
		// calling reqCancel through hr.Preempt. Deriving from arrivalCtx
		// means an ordinary client disconnect still cancels it too —
		// preemption is layered on top of that existing mechanism, not a
		// replacement for it.
		//
		// Attempt 1 reuses the swaputil.PreemptHandle from
		// CreateRequestContextMiddleware exactly as before this change (the
		// "Known soft spot" fix): any preemption raised outside this loop and
		// this scheduler-stage preemption then cancel through the exact same
		// Flag+Cancel pair. (Upstream moved the per-model concurrency cap into
		// the scheduler, so same-model contention is now resolved by
		// scheduler.FIFO.tryPreempt rather than by a separate semaphore layer.)
		// That handle's own context is one-shot — once Cancel fires it
		// reports Done() forever — so it cannot seed a REPLAY attempt.
		// Attempts 2+ instead derive a fresh context from handle.Parent (the
		// context that existed before the handle wrapped it, still alive
		// after Cancel fires — see swaputil.PreemptHandle.Parent) when a
		// handle was used, or from arrivalCtx directly otherwise (that
		// branch never cancels arrivalCtx itself, only its own child, so
		// arrivalCtx stays live for every attempt already).
		//
		// Known tradeoff: a preemption raised through the ORIGINAL handle
		// while we're on a replay attempt (2+) no longer interrupts it — that
		// handle is spent after its first Cancel. Only a FIFO-scheduler
		// preemption is
		// observed on replay attempts, since hr.Preempt is rebuilt fresh
		// every attempt below. Bounded either way by the replay cap.
		var reqCtx context.Context
		var reqCancel context.CancelFunc
		var preempted *atomic.Bool
		if attempt == 1 {
			if handle, ok := swaputil.PreemptHandleFromContext(arrivalCtx); ok {
				reqCtx = arrivalCtx
				reqCancel = handle.Cancel
				preempted = handle.Flag
			} else {
				reqCtx, reqCancel = context.WithCancel(arrivalCtx)
				preempted = new(atomic.Bool)
			}
		} else if handle, ok := swaputil.PreemptHandleFromContext(arrivalCtx); ok && handle.Parent != nil {
			reqCtx, reqCancel = context.WithCancel(handle.Parent)
			preempted = new(atomic.Bool)
		} else {
			reqCtx, reqCancel = context.WithCancel(arrivalCtx)
			preempted = new(atomic.Bool)
		}
		defer reqCancel()
		// Hand the stall guard THIS attempt's cancel: each replay attempt runs
		// under a fresh context, and cancelling a spent one would free nothing.
		stall.setCancel(reqCancel)

		attemptReq := req.WithContext(reqCtx)
		if attempt > 1 {
			// Replay: hand the upstream the client's original bytes again —
			// see origBody above.
			attemptReq.Body = io.NopCloser(bytes.NewReader(origBody))
		}

		// replayEligible: this attempt may transparently retry instead of
		// 503ing the client if it is preempted before producing any body
		// byte. Scope: only tiers marked Preemptible (today, just
		// "background" — docs/intent/llama-swap-tiers.md) — a
		// non-preemptible request is never a preemption victim in the first
		// place, so this check is inert for it either way; making it
		// explicit keeps that inertness obvious rather than implicit.
		// Bounded by maxReplays/maxReplayHeld so a pathological hot-loop of
		// same-model contention still falls back to the v1 503 well under
		// the client's own ~300s abort ceiling. Memory tradeoff: origBody
		// (already buffered once by swaputil.FetchContext for every JSON POST,
		// replay-eligible or not) is retained for this attempt's lifetime —
		// interactive request bodies can run multi-MB, so this scales with
		// (concurrent preemptible requests) x (body size), not with replay
		// count, and is bounded by the same concurrency limits that already
		// bound in-flight request memory today.
		var replayWanted *atomic.Bool
		replayEligible := data.Tier.Preemptible && replayCount < maxReplays && time.Since(replayStart) < maxReplayHeld
		if replayEligible {
			replayWanted = new(atomic.Bool)
		} else if pw != nil {
			// Eligibility surrendered (replay budget or held-time exhausted):
			// nothing can transparently retry this request any more, so the
			// keepalive pings that would have blocked a replay are now the only
			// thing standing between it and the client's zero-byte wall. See
			// the sequencing note at the pingWriter construction above.
			pw.allowPings()
		}

		hr := scheduler.HandlerReq{
			Model: data.ModelID,
			Ctx:   reqCtx,
			// Unbuffered: a successful send on Respond proves the waiter is
			// alive and consuming. grant() relies on this to avoid handing a
			// handleFunc to a cancelled waiter and leaking the inFlight count.
			Admit:      make(chan error, 1),
			Respond:    make(chan scheduler.HandlerResp),
			PositionCh: make(chan int, 1),
			// count_tokens is tokenize-only (no model slot, near-free), so it is
			// exempt from the per-model concurrency cap the scheduler enforces
			// (see scheduler.FIFO.atCapacity). Client turn-boundary fan-outs fire
			// 15-30 of these in parallel; making them wait for a serving slot
			// starved the pool and bounced real turns (2026-07-05). Re-homed here
			// from the deleted internal/server concurrency middleware, which
			// exempted the same path from its semaphore.
			ConcurrencyExempt: isSlotFreeRequest(attemptReq),
			// Inert unless the target model has a KVPoolTokens budget configured
			// (see scheduler.FIFO.kvAdmit) — see swaputil.EstimateTokens for the
			// estimation rule.
			EstimatedTokens: estimatedTokens,
			// Tier is DefaultTier for every request on the main listener / when no
			// `tiers:` block is configured (see swaputil.Tier).
			Tier:         data.Tier,
			Preempted:    preempted,
			ReplayWanted: replayWanted,
			Preempt: func() {
				preempted.Store(true)
				reqCancel()
			},
		}

		select {
		case b.handlerCh <- hr:
		case <-attemptReq.Context().Done():
			return
		case <-b.shutdownCtx.Done():
			swaputil.SendError(w, attemptReq, fmt.Errorf("%s is shutting down", b.name))
			return
		}

		// Admission handshake (upstream #889): the scheduler answers here
		// BEFORE any loading stream is started, so a pre-stream rejection
		// (unknown model) can still be written as a plain HTTP error rather
		// than injected into an already-committed SSE body. Our fork does not
		// reject on concurrency here — over-capacity requests park in the
		// scheduler queue and wait (see scheduler.FIFO.atCapacity).
		admitGiveUp, stopAdmitGiveUp := armParkGiveUp(data.Tier, keepaliveArmed, replayStart)
		defer stopAdmitGiveUp()

		var admissionErr error
		select {
		case admissionErr = <-hr.Admit:
		case <-admitGiveUp:
			// Bounded for the same reason as the grant wait below. In practice
			// the scheduler answers this handshake immediately (fifo.go
			// OnRequest admits before it ever consults capacity, and Admit is
			// buffered), so this case is a belt-and-braces twin of the real
			// park rather than the path the incident observed.
			b.giveUpParked(w, hr, reqCancel, "admission", data, replayStart)
			return
		case <-attemptReq.Context().Done():
			b.cancelParked(hr)
			return
		case <-b.shutdownCtx.Done():
			swaputil.SendError(w, attemptReq, fmt.Errorf("%s is shutting down", b.name))
			return
		}
		if admissionErr != nil {
			swaputil.SendError(w, attemptReq, admissionErr)
			return
		}

		isModelReady := false
		if p, ok := b.processes[data.ModelID]; ok {
			isModelReady = p.State() == process.StateReady
		}
		// shouldShowLoading is attempt-1-only: loadingWriter commits its own
		// SSE 200 status line the moment it starts (see loading.go), so
		// re-running it on a replay attempt would try to write a second
		// status line on a writer that already committed one. The OpenAI
		// loading path (isLoadingPath) is disjoint from the Anthropic
		// pingWriter path replay actually targets in production, so this is
		// a no-op restriction for the tiers use case and simply keeps the
		// loading indicator best-effort (as it already is) rather than
		// risking a panic on retry.
		shouldShowLoading := attempt == 1 && data.Streaming && data.SendLoadingState && isLoadingPath(attemptReq.URL.Path) && !isModelReady

		var lw *loadingWriter
		cancelLoad := func() {}
		if shouldShowLoading {
			var swapCtx context.Context
			swapCtx, cancelLoad = context.WithCancel(attemptReq.Context())
			lw = newLoadingWriter(b.logger, data.ModelID, w, attemptReq)
			go lw.start(swapCtx)
			go func() {
				for {
					select {
					case pos := <-hr.PositionCh:
						lw.setUpdate(fmt.Sprintf("Queue position: #%d", pos))
					case <-swapCtx.Done():
						return
					}
				}
			}()
		}

		// finishLoading stops the loading stream and fences its goroutine off from
		// the ResponseWriter before the real handler (or ServeHTTP's return)
		// reclaims it. release() must run even when waitForCompletion times out:
		// otherwise a still-streaming goroutine flushes a finalized response and
		// panics on the recycled *bufio.Writer.
		finishLoading := func() {
			cancelLoad()
			if lw != nil {
				lw.waitForCompletion(1 * time.Second)
				lw.release()
			}
		}

		// THE park. Over-capacity requests wait here (fifo.go atCapacity ->
		// enqueue) until the scheduler grants them a slot; until this fix that
		// wait had no server-side bound at all and a starved background request
		// simply returned bare at the client's ~300s abort. lw != nil means the
		// OpenAI loading stream already committed a status line, which
		// disqualifies this request from the give-up for the same reason
		// keepaliveArmed does.
		grantGiveUp, stopGrantGiveUp := armParkGiveUp(data.Tier, keepaliveArmed || lw != nil, replayStart)
		defer stopGrantGiveUp()

		var resp scheduler.HandlerResp
		select {
		case resp = <-hr.Respond:
			finishLoading()
		case <-grantGiveUp:
			finishLoading()
			b.giveUpParked(w, hr, reqCancel, "grant", data, replayStart)
			return
		case <-attemptReq.Context().Done():
			finishLoading()
			// Notify the scheduler so it can prune this request from its queue
			// and swap waiters. Without this, a queued request whose client left
			// would sit in the scheduler until drainQueue eventually starts a
			// wasted model load for it.
			b.cancelParked(hr)
			return
		case <-b.shutdownCtx.Done():
			finishLoading()
			swaputil.SendError(w, attemptReq, fmt.Errorf("%s is shutting down", b.name))
			return
		}

		if resp.Err != nil {
			swaputil.SendError(w, attemptReq, resp.Err)
			return
		}
		if pw != nil {
			// The park is over: this request holds a serving slot. That starts
			// pingZeroBudget, which is deliberately inert for the parked
			// stretch so a session that is merely third in the queue is held
			// and pinged rather than ended with an error it must retry. See
			// pingWriter.slotGranted.
			pw.slotGranted()
		}
		resp.HandleFunc(w, attemptReq)

		if replayWanted != nil && replayWanted.Load() {
			// GUARD on the pinger/replay interaction. preemptResponseWriter
			// tracks its OWN wroteHeader, so it will happily swallow a first
			// WriteHeader for a replay even on a response whose status line the
			// pinger already committed - and replaying then means the client
			// gets a second response body on a stream it is already reading.
			// The sequencing above makes this unreachable in the common case
			// (an eligible request is muted, so it cannot have pinged); it is
			// reachable only in the narrow window where eligibility was
			// surrendered by time AFTER this attempt's replayWanted was
			// created. End the stream the way a committed stream must be ended -
			// as an SSE error event (pingWriter.WriteHeader maps it) - instead
			// of replaying.
			if pw != nil && pw.pingsCommitted() {
				b.logger.Warnf("preempt-replay: tier=%s model=%s attempt=%d suppressed - keepalive pings already committed the response",
					data.Tier.Name, data.ModelID, attempt)
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			replayCount++
			b.logger.Infof("preempt-replay: tier=%s model=%s attempt=%d held=%.0fs",
				data.Tier.Name, data.ModelID, replayCount, time.Since(replayStart).Seconds())
			continue
		}
		if replayCount > 0 {
			outcome := "served"
			if preempted.Load() {
				// This final attempt WAS preempted too, but landed outside
				// replay eligibility (cap or deadline tripped) and fell back
				// to the v1 503 inside preemptResponseWriter.
				outcome = "given-up"
			}
			b.logger.Infof("preempt-replay: tier=%s model=%s %s attempt=%d held=%.0fs",
				data.Tier.Name, data.ModelID, outcome, attempt, time.Since(replayStart).Seconds())
		}
		return
	}
}
