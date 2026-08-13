package router

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/router/scheduler"
	"github.com/mostlygeek/llama-swap/internal/shared"
)

// maxReplays caps how many times a single client request may be
// transparently re-enqueued after a zero-byte preemption (see ServeHTTP)
// before falling back to the v1 cancel+503
// (docs/intent/llama-swap-tiers.md "Known v1 limitations" -> v2). A hard
// ceiling independent of maxReplayHeld: a pathological rapid-fire preemption
// loop (each attempt preempted within milliseconds of being granted) would
// otherwise hold the connection — and this model's concurrency-semaphore
// permit, which is only released when ServeHTTP finally returns — for
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
const maxReplayHeld = 270 * time.Second

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
// scheduler.Factory (which captures their scheduler.Swapper) describing how
// requests are scheduled and how their eviction set is decided. baseRouter
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
	newSched scheduler.Factory,
) *baseRouter {
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
	b.schedule = newSched(name, logger, b)
	return b
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

	target := b.processes[modelID]
	if target.State() == process.StateStopped {
		go func() {
			if err := target.Run(timeout); err != nil {
				b.logger.Warnf("%s: running %s exited: %v", b.name, modelID, err)
			}
		}()
	}

	err := target.WaitReady(b.shutdownCtx)

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

func (b *baseRouter) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if b.shuttingDown.Load() {
		shared.SendError(w, req, fmt.Errorf("%s is shutting down", b.name))
		return
	}

	data, err := shared.FetchContext(req, b.config)
	if err != nil {
		shared.SendError(w, req, err)
		return
	}

	// Anthropic-path keepalive: /v1/messages streams that stay byte-less while
	// parked behind a swap, loading, or mid-prefill get killed by client-side
	// zero-byte timeouts (~300s). Wrap the writer so silent waits emit legal
	// SSE ping events; fast requests pass through untouched (see pinging.go).
	if data.Streaming && data.SendLoadingState && isAnthropicStreamPath(req.URL.Path) {
		pw := newPingWriter(b.logger, data.ModelID, w)
		defer pw.stop()
		w = pw
	}

	// arrivalCtx anchors every attempt below (see the per-attempt context
	// derivation for why attempt 1 alone may reuse a shared.PreemptHandle
	// context directly, while replay attempts never do). origBody is the
	// body shared.FetchContext already buffered once at arrival — reused
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

	for {
		attempt := replayCount + 1

		// A cancellable derivative of the request's own context: once
		// granted, this becomes the context p.ServeHTTP actually serves
		// under (req is rebound to it below), so the FIFO scheduler's
		// preemption branch can server-side-abort a running request by
		// calling reqCancel through hr.Preempt. Deriving from arrivalCtx
		// means an ordinary client disconnect still cancels it too —
		// preemption is layered on top of that existing mechanism, not a
		// replacement for it.
		//
		// Attempt 1 reuses the shared.PreemptHandle from
		// CreateRequestContextMiddleware exactly as before this change (the
		// "Known soft spot" fix): a same-model preemption at the semaphore
		// stage (internal/server/concurrency.go) and this scheduler-stage
		// preemption then cancel through the exact same Flag+Cancel pair.
		// That handle's own context is one-shot — once Cancel fires it
		// reports Done() forever — so it cannot seed a REPLAY attempt.
		// Attempts 2+ instead derive a fresh context from handle.Parent (the
		// context that existed before the handle wrapped it, still alive
		// after Cancel fires — see shared.PreemptHandle.Parent) when a
		// handle was used, or from arrivalCtx directly otherwise (that
		// branch never cancels arrivalCtx itself, only its own child, so
		// arrivalCtx stays live for every attempt already).
		//
		// Known tradeoff: a same-model semaphore-triggered preemption that
		// arrives while we're on a replay attempt (2+) no longer interrupts
		// it — the semaphore's registered handle is the ORIGINAL one, spent
		// after its first Cancel. Only a FIFO-scheduler preemption is
		// observed on replay attempts, since hr.Preempt is rebuilt fresh
		// every attempt below. Bounded either way by the replay cap.
		var reqCtx context.Context
		var reqCancel context.CancelFunc
		var preempted *atomic.Bool
		if attempt == 1 {
			if handle, ok := shared.PreemptHandleFromContext(arrivalCtx); ok {
				reqCtx = arrivalCtx
				reqCancel = handle.Cancel
				preempted = handle.Flag
			} else {
				reqCtx, reqCancel = context.WithCancel(arrivalCtx)
				preempted = new(atomic.Bool)
			}
		} else if handle, ok := shared.PreemptHandleFromContext(arrivalCtx); ok && handle.Parent != nil {
			reqCtx, reqCancel = context.WithCancel(handle.Parent)
			preempted = new(atomic.Bool)
		} else {
			reqCtx, reqCancel = context.WithCancel(arrivalCtx)
			preempted = new(atomic.Bool)
		}
		defer reqCancel()

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
		// (already buffered once by shared.FetchContext for every JSON POST,
		// replay-eligible or not) is retained for this attempt's lifetime —
		// interactive request bodies can run multi-MB, so this scales with
		// (concurrent preemptible requests) x (body size), not with replay
		// count, and is bounded by the same concurrency limits that already
		// bound in-flight request memory today.
		var replayWanted *atomic.Bool
		replayEligible := data.Tier.Preemptible && replayCount < maxReplays && time.Since(replayStart) < maxReplayHeld
		if replayEligible {
			replayWanted = new(atomic.Bool)
		}

		hr := scheduler.HandlerReq{
			Model: data.ModelID,
			Ctx:   reqCtx,
			// Unbuffered: a successful send on Respond proves the waiter is
			// alive and consuming. grant() relies on this to avoid handing a
			// handleFunc to a cancelled waiter and leaking the inFlight count.
			Respond:    make(chan scheduler.HandlerResp),
			PositionCh: make(chan int, 1),
			// Inert unless the target model has a KVPoolTokens budget configured
			// (see scheduler.FIFO.kvAdmit) — see shared.EstimateTokens for the
			// estimation rule.
			EstimatedTokens: shared.EstimateTokens(origBody),
			// Tier is DefaultTier for every request on the main listener / when no
			// `tiers:` block is configured (see shared.Tier).
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
			shared.SendError(w, attemptReq, fmt.Errorf("%s is shutting down", b.name))
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

		var resp scheduler.HandlerResp
		select {
		case resp = <-hr.Respond:
			finishLoading()
		case <-attemptReq.Context().Done():
			finishLoading()
			// Notify the scheduler so it can prune this request from its queue
			// and swap waiters. Without this, a queued request whose client left
			// would sit in the scheduler until drainQueue eventually starts a
			// wasted model load for it.
			select {
			case b.cancelCh <- hr:
			case <-b.shutdownCtx.Done():
			}
			return
		case <-b.shutdownCtx.Done():
			finishLoading()
			shared.SendError(w, attemptReq, fmt.Errorf("%s is shutting down", b.name))
			return
		}

		if resp.Err != nil {
			shared.SendError(w, attemptReq, resp.Err)
			return
		}
		resp.HandleFunc(w, attemptReq)

		if replayWanted != nil && replayWanted.Load() {
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
