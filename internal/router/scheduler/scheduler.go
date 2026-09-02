// Package scheduler contains the request-scheduling strategies used by the
// router's baseRouter. A Scheduler owns the queue, in-flight tracking, and the
// decision tree for when to start a swap versus queue a request. The baseRouter
// owns the channels, run loop, and process machinery, and exposes the
// side-effects a scheduler needs through the Effects interface.
//
// Splitting these apart lets the scheduling strategy be swapped out
// independently of both the process machinery (baseRouter) and the eviction
// policy (Swapper). FIFO is the first and currently only implementation.
package scheduler

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// ErrModelNotFound is granted to callers whose model is not handled by this
// router. It is an alias for swaputil.ErrNoLocalModelFound.
var ErrModelNotFound = swaputil.ErrNoLocalModelFound

// Swapper is the eviction policy: it decides which running models must be
// stopped before a target can serve. It is orthogonal to the scheduling
// strategy — any Scheduler works with any Swapper.
type Swapper interface {
	// EvictionFor returns running model IDs that must be stopped before
	// target can serve. running is the complete set the scheduler considers
	// live: every process that is not stopped, unioned with the targets of
	// in-flight swaps the scheduler has already committed to (which are not yet
	// visible in process state). The planner does not inspect process state
	// itself. Pure decision; must not log.
	EvictionFor(target string, running []string) []string

	// OnSwapStart runs once at the start of every swap, with the same running
	// set EvictionFor was given for this decision. Planners may log their
	// decision here at whatever verbosity they choose.
	OnSwapStart(target string, running []string)
}

// Scheduler decides what happens to each event the router's run loop receives.
// All methods run on that single run-loop goroutine, so implementations need no
// internal locking for their own state.
type Scheduler interface {
	// OnRequest handles one incoming ServeHTTP request.
	OnRequest(req HandlerReq)
	// OnCancel handles a request whose client has disconnected before it was
	// granted. The scheduler must remove the request from its queue and from
	// any in-flight swap's waiters so it never triggers a model load or grant
	// for a caller that is no longer there.
	OnCancel(req HandlerReq)
	// OnSwapDone handles a swap goroutine reporting completion.
	OnSwapDone(ev SwapDone)
	// OnServeDone handles a tracked ServeHTTP finishing (in-flight decrement).
	OnServeDone(ev ServeDoneEvent)
	// OnTick is a periodic nudge (armed by the router only when a swap-grace is
	// configured) that re-evaluates the queue, letting a grace-deferred swap
	// proceed once its evictee has been idle long enough. No other event fires
	// during pure idle, so without this the deferred request would wait forever.
	OnTick()
	// OnUnload reconciles scheduler state for an unload, stops the targeted
	// processes via Effects, and drains the queue. It must block until the
	// targeted processes have stopped.
	OnUnload(targets []string, timeout time.Duration)
	// OnShutdown grants err to every waiter the scheduler still holds (active
	// swap waiters and queued requests). Process teardown is the baseRouter's
	// responsibility.
	OnShutdown(err error)
}

// Effects is implemented by the baseRouter. The scheduler calls back through it
// for every side-effect: inspecting process state, launching swaps, responding
// to callers, and stopping processes.
type Effects interface {
	// ModelState returns the current state of a model's process. ok is false
	// when the model is not handled by this router.
	ModelState(modelID string) (process.ProcessState, bool)
	// RunningModels returns the state of every process that is not stopped or
	// shut down, keyed by model ID. The scheduler uses it to build the running
	// set it hands the Swapper.
	RunningModels() map[string]process.ProcessState
	// StartSwap launches the swap goroutine for modelID, stopping evict first.
	StartSwap(modelID string, evict []string)
	// GrantError responds to a caller with an error.
	GrantError(req HandlerReq, err error)
	// GrantServe hands a caller the wrapped handler for modelID and reports
	// whether the caller was still there to receive it. The scheduler bumps
	// its in-flight count only when this returns true.
	GrantServe(req HandlerReq, modelID string) bool
	// StopProcesses stops the named processes in parallel and blocks until all
	// have stopped. Unknown IDs are skipped.
	StopProcesses(timeout time.Duration, ids []string)
}

// New returns a Scheduler selected by conf.Routing.Scheduler.Use, configured
// from conf and bound to the given planner and effects. Currently only "fifo"
// (the default) is supported.
func New(conf config.Config, name string, logger *logmon.Monitor, planner Swapper, eff Effects) (Scheduler, error) {
	use := conf.Routing.Scheduler.Use
	if use == "" {
		use = "fifo"
	}
	switch use {
	case "fifo":
		return NewFIFO(name, logger, planner, conf.Routing.Scheduler.Settings.Fifo, conf.Models, eff), nil
	default:
		return nil, fmt.Errorf("unsupported scheduler type: %q", use)
	}
}

// HandlerReq is one in-flight ServeHTTP request waiting for a routing decision.
type HandlerReq struct {
	Model string
	Ctx   context.Context
	// Admit carries the pre-stream admission answer: nil once the scheduler
	// has accepted the request, or an error the caller must return BEFORE any
	// loading stream is committed (upstream #889). Buffered by the caller.
	Admit      chan error
	Respond    chan HandlerResp
	PositionCh chan int
	// EstimatedTokens is a conservative estimate of this request's context
	// size, used only by KV-aware admission (config.FifoConfig.KVPoolTokens).
	// baseRouter.ServeHTTP populates it from the buffered request body; see
	// swaputil.EstimateTokens for the estimation rule. 0 for requests with no
	// body (e.g. GET) or when the target model has no KV budget configured —
	// either way it is inert unless KVPoolTokens > 0 for the model.
	EstimatedTokens int

	// Tier is the entry-point tier this request arrived through (see
	// swaputil.Tier). swaputil.DefaultTier for every request on the main
	// listener / when no `tiers:` block is configured — see
	// docs/intent/llama-swap-tiers.md (llama-cm) for the full design.
	Tier swaputil.Tier

	// ConcurrencyExempt marks a request that must not be held back by the
	// per-model concurrency cap (FIFO.atCapacity): tokenize-only
	// count_tokens calls, which hold no model slot. Set by
	// baseRouter.ServeHTTP; false for every ordinary inference request.
	ConcurrencyExempt bool

	// Preempted, when non-nil, is set to true by Preempt just before it
	// cancels this request's serving context. Only populated on requests
	// baseRouter has granted (nil while merely queued); trackedServe reads it
	// to tell a genuine preemption apart from an ordinary client disconnect or
	// shutdown when deciding whether to inject the 503 + headers response.
	Preempted *atomic.Bool

	// Preempt, when non-nil, server-side-cancels this GRANTED request so its
	// caller sees a 503 + `X-LlamaSwap-Preempted: 1` + `Retry-After` (best
	// effort — see trackedServe) and can retry, re-entering the queue through
	// its own tier port. Only the FIFO scheduler's preemption branch in
	// OnRequest/drainQueue calls this, and only on requests it is currently
	// tracking as granted. nil while merely queued (there is nothing running
	// yet to preempt).
	Preempt func()

	// ReplayWanted, when non-nil, marks this attempt eligible for a
	// transparent server-side replay if it is preempted before producing any
	// response body byte (docs/intent/llama-swap-tiers.md "Known v1
	// limitations" -> v2): internal/router/preempt.go's
	// preemptResponseWriter sets it to true instead of writing the v1
	// cancel+503, and internal/router/base.go ServeHTTP re-submits a fresh
	// attempt when it sees that. nil means "this attempt falls back to the
	// v1 503 if preempted" — the default, byte-identical to pre-replay
	// behavior.
	ReplayWanted *atomic.Bool

	// arrivalSeq is a monotonically increasing sequence number FIFO.OnRequest
	// assigns to a request the one time it first enters the scheduler — never
	// reassigned on a later re-enqueue (OnSwapDone's cap re-queue, drainQueue),
	// since those calls pass along the SAME HandlerReq value. enqueue uses it
	// to break ties within a (tier rank, priority) group by true arrival
	// order, which no other field can reliably do (there is no wall-clock
	// arrival timestamp on this struct) — see fifo_arrival_order_test.go.
	arrivalSeq uint64
}

// HandlerResp is the routing decision returned to a HandlerReq's caller: either
// a handler to serve with, or an error.
type HandlerResp struct {
	HandleFunc http.HandlerFunc
	Err        error
}

// SwapDone is reported by a swap goroutine when its target is ready (or failed).
type SwapDone struct {
	ModelID string
	Err     error
}

// ServeDoneEvent is reported when a tracked ServeHTTP handler returns. This
// fires on EVERY exit path from p.ServeHTTP — success, client abort, or
// upstream error — because trackedServe's release is a deferred send, not a
// success-only callback. That makes OnServeDone the single release point for
// KV-aware admission's EstimatedTokens accounting too: whatever tokens were
// reserved at grant time are always released here, regardless of how the
// request ended.
type ServeDoneEvent struct {
	ModelID string
	// EstimatedTokens is the same estimate the originating HandlerReq carried
	// at grant time, echoed back so the scheduler can release exactly what it
	// reserved. 0 when KV admission wasn't in play for this request.
	EstimatedTokens int
}
