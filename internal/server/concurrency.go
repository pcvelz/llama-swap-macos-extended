package server

import (
	"net/http"
	"strings"
	"sync"

	"golang.org/x/sync/semaphore"

	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/shared"
)

// defaultConcurrencyLimit caps simultaneous in-flight requests per model when
// the model config leaves concurrencyLimit unset. Matches the legacy
// proxy.Process default.
const defaultConcurrencyLimit = 10

// concurrencyHolder is one request currently holding a permit on a model's
// semaphore, tracked so a higher-rank arrival that cannot immediately acquire
// a permit can find and preempt an eligible holder (docs/intent/
// llama-swap-tiers.md "Known soft spot": this semaphore sits AHEAD of the
// tier-aware FIFO scheduler, so without this tracking it had no rank
// awareness at all).
type concurrencyHolder struct {
	tier   shared.Tier
	handle *shared.PreemptHandle
}

// concurrencyHolders is a per-model registry of current permit holders,
// guarded by a mutex. This layer runs outside the FIFO scheduler's single
// run-loop goroutine, so a lock is fine and expected here (unlike
// scheduler.FIFO, which is deliberately lock-free).
type concurrencyHolders struct {
	mu      sync.Mutex
	byModel map[string][]concurrencyHolder
}

func newConcurrencyHolders() *concurrencyHolders {
	return &concurrencyHolders{byModel: make(map[string][]concurrencyHolder)}
}

// register records handle as a current holder for modelID. A nil handle
// (requests that never passed through CreateRequestContextMiddleware, e.g.
// direct callers/tests) is never trackable as a preemption victim, so it is
// silently skipped rather than stored.
func (h *concurrencyHolders) register(modelID string, tier shared.Tier, handle *shared.PreemptHandle) {
	if handle == nil {
		return
	}
	h.mu.Lock()
	h.byModel[modelID] = append(h.byModel[modelID], concurrencyHolder{tier: tier, handle: handle})
	h.mu.Unlock()
}

// unregister drops the holder entry matching handle for modelID (a no-op if
// it was never registered, e.g. handle is nil).
func (h *concurrencyHolders) unregister(modelID string, handle *shared.PreemptHandle) {
	if handle == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	list := h.byModel[modelID]
	for i, holder := range list {
		if holder.handle == handle {
			h.byModel[modelID] = append(list[:i:i], list[i+1:]...)
			break
		}
	}
	if len(h.byModel[modelID]) == 0 {
		delete(h.byModel, modelID)
	}
}

// preempt boots every currently-registered holder of modelID that
// shared.CanPreempt allows arrival to evict — the SAME rule
// scheduler.FIFO.tryPreempt applies, just one layer earlier (before the
// semaphore, rather than after it).
func (h *concurrencyHolders) preempt(modelID string, arrival shared.Tier) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, holder := range h.byModel[modelID] {
		if holder.handle == nil || !shared.CanPreempt(holder.tier, arrival) {
			continue
		}
		holder.handle.Flag.Store(true)
		holder.handle.Cancel()
	}
}

// CreateConcurrencyMiddleware returns middleware that limits simultaneous
// model-dispatched requests per model. Each model gets a semaphore sized to
// its concurrencyLimit (or defaultConcurrencyLimit). A request that cannot
// immediately acquire a slot is rejected with 429. Models without a local
// config entry (e.g. peer-routed models) are not limited.
//
// Tier-aware preemption (docs/intent/llama-swap-tiers.md "Known soft spot"):
// with concurrencyLimit 1, a priority-tier arrival used to park on this
// semaphore behind a background-tier holder with no rank awareness — tiers
// were inert for same-model contention, the dominant production case. Now a
// request that cannot immediately acquire a permit boots any eligible
// lower-rank holder (same rule as scheduler.FIFO.tryPreempt) before falling
// through to the existing blocking acquire; the booted holder's handler
// unwinds via its cancelled context, releasing the permit. When no tiers are
// configured every request is shared.DefaultTier (rank 0), so no victim is
// ever eligible and TryAcquire-then-Acquire behaves identically to a bare
// Acquire — zero-config behavior is unchanged.
func CreateConcurrencyMiddleware(cfg config.Config) chain.Middleware {
	semaphores := make(map[string]*semaphore.Weighted, len(cfg.Models))
	for id, mc := range cfg.Models {
		limit := defaultConcurrencyLimit
		if mc.ConcurrencyLimit > 0 {
			limit = mc.ConcurrencyLimit
		}
		semaphores[id] = semaphore.NewWeighted(int64(limit))
	}
	holders := newConcurrencyHolders()

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// count_tokens is tokenize-only (no slot, near-free). Exempt it so
			// client turn-boundary fan-outs (15-30 parallel calls) never burn
			// permits or eat 429s (2026-07-05: they starved the pool and
			// bounced real turns).
			if strings.HasSuffix(r.URL.Path, "/count_tokens") {
				next.ServeHTTP(w, r)
				return
			}

			data, err := shared.FetchContext(r, cfg)
			if err != nil {
				shared.SendError(w, r, shared.ErrNoModelInContext)
				return
			}

			// fall through for peer models
			sem, ok := semaphores[data.ModelID]
			if !ok {
				next.ServeHTTP(w, r)
				return
			}

			// The handle CreateRequestContextMiddleware tagged onto this
			// request earlier in the chain (nil for direct callers/tests
			// that skip that middleware — see concurrencyHolders.register).
			handle, _ := shared.PreemptHandleFromContext(r.Context())

			if !sem.TryAcquire(1) {
				// Couldn't acquire immediately: boot any eligible lower-rank
				// holder, then fall through to the existing blocking
				// Acquire — booting is a server-side cancel, not a
				// synchronous release, so this request still has to wait
				// for the victim's handler to actually unwind.
				holders.preempt(data.ModelID, data.Tier)

				// Over-capacity requests WAIT for a permit instead of bouncing
				// with 429. Rationale (2026-07-05): permits are held for a
				// request's entire lifetime including time parked in the swap
				// queue, so bursts of parked requests exhausted the pool and
				// real turns got instant 429s ("should just let the agent
				// wait"). Blocking on the request context queues fairly, still
				// caps concurrent child inference, and releases automatically
				// when the client disconnects. Keepalive pings
				// (router/pinging.go) keep waiting streams alive meanwhile.
				if err := sem.Acquire(r.Context(), 1); err != nil {
					// client gave up while queued for a permit
					return
				}
			}
			defer sem.Release(1)

			holders.register(data.ModelID, data.Tier, handle)
			defer holders.unregister(data.ModelID, handle)

			next.ServeHTTP(w, r)
		})
	}
}
