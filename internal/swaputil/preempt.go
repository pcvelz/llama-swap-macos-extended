package swaputil

import (
	"context"
	"sync/atomic"
)

// PreemptHandle is the per-request handle a tiered-queue preemption cancels
// through: setting Flag then calling Cancel unwinds whatever is currently
// holding this request up, whether that is the per-model concurrency
// semaphore (internal/server/concurrency.go) or a granted FIFO-scheduler slot
// (internal/router/scheduler/fifo.go tryPreempt via internal/router/base.go
// ServeHTTP) - both layers preempt through the exact same Flag+Cancel pair,
// so a preempted caller always sees the 503 + X-LlamaSwap-Preempted response
// (internal/router/preempt.go), regardless of which layer was holding it up.
//
// Created once per request, as early as possible - before
// CreateConcurrencyMiddleware's per-model semaphore ever blocks - and carried
// in the request context (see WithPreemptHandle / PreemptHandleFromContext).
// See docs/intent/llama-swap-tiers.md (llama-cm) "Known soft spot" for why
// this exists: without it, same-model contention at the semaphore stage was
// invisible to tier ranks entirely.
type PreemptHandle struct {
	Flag   *atomic.Bool
	Cancel context.CancelFunc

	// Parent is the context Cancel's own context was derived from (the
	// arriving request's context BEFORE this handle wrapped it) - still
	// alive after Cancel fires, since cancellation only propagates downward
	// to children, never back up to the parent it was built from.
	//
	// WHY this exists: Cancel is one-shot - once it fires, the context it
	// cancelled reports Done() forever, so it cannot be reused to detect a
	// LATER event. A transparent replay attempt (internal/router/base.go
	// ServeHTTP, docs/intent/llama-swap-tiers.md "Known v1 limitations" -> v2)
	// needs a fresh, live context for each retry after the first is
	// preempted, so it derives that retry's context from Parent instead of
	// from Cancel's spent one. Nil for a PreemptHandle built without this
	// field (e.g. bare test harnesses) - callers must treat that as "this
	// handle cannot seed a replay attempt", not an error.
	Parent context.Context
}

type preemptHandleContextKey struct{}

// WithPreemptHandle tags ctx with h.
func WithPreemptHandle(ctx context.Context, h *PreemptHandle) context.Context {
	return context.WithValue(ctx, preemptHandleContextKey{}, h)
}

// PreemptHandleFromContext returns the handle tagged onto ctx by
// WithPreemptHandle, if any. Absent for requests that never passed through
// the middleware that creates one (e.g. direct callers/tests that construct
// a bare context) - callers must treat that as "this request cannot be
// tracked as a preemption victim", not an error.
func PreemptHandleFromContext(ctx context.Context) (*PreemptHandle, bool) {
	h, ok := ctx.Value(preemptHandleContextKey{}).(*PreemptHandle)
	return h, ok
}

// CanPreempt reports whether an arrival on tier `arrival` may boot a running
// request on tier `victim`, per the tiered-queue preemption rule
// (docs/intent/llama-swap-tiers.md "Preemption rule"):
//
//	rank(victim) < rank(arrival) AND (victim.Preemptible OR arrival.Preempts)
//
// Shared by every layer that can preempt: the FIFO scheduler
// (internal/router/scheduler/fifo.go tryPreempt) and the per-model
// concurrency semaphore (internal/server/concurrency.go) both apply this one
// rule, so a request is never preempted by one layer under a different
// standard than the other.
func CanPreempt(victim, arrival Tier) bool {
	if victim.Rank >= arrival.Rank {
		return false
	}
	return victim.Preemptible || arrival.Preempts
}
