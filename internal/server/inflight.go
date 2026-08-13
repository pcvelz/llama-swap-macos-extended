package server

import (
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/shared"
)

// inflightCounter tracks the number of in-flight model-dispatched requests,
// both overall and per tier (docs/intent/llama-swap-tiers.md). The per-tier
// map is guarded by mu since it is read/written far less often than total is
// incremented; total stays a lock-free atomic for the hot path.
type inflightCounter struct {
	total atomic.Int64

	mu      sync.Mutex
	perTier map[string]int64
}

// newInflightCounter seeds perTier with "default" plus every configured tier
// name at zero, so the ByTier breakdown reflects CONFIGURATION (">1 tier
// configured") rather than only tiers that have happened to see traffic yet.
func newInflightCounter(tierNames []string) *inflightCounter {
	c := &inflightCounter{perTier: make(map[string]int64, len(tierNames)+1)}
	c.perTier[shared.DefaultTier.Name] = 0
	for _, name := range tierNames {
		c.perTier[name] = 0
	}
	return c
}

func (c *inflightCounter) Increment() int64 { return c.total.Add(1) }
func (c *inflightCounter) Decrement() int64 { return c.total.Add(-1) }
func (c *inflightCounter) Current() int64   { return c.total.Load() }

// incrementTier bumps tier's count and returns the current total.
func (c *inflightCounter) incrementTier(tier string) int64 {
	c.mu.Lock()
	if c.perTier == nil {
		c.perTier = make(map[string]int64)
	}
	c.perTier[tier]++
	c.mu.Unlock()
	return c.Increment()
}

// decrementTier drops tier's count (never below zero) and returns the current total.
func (c *inflightCounter) decrementTier(tier string) int64 {
	c.mu.Lock()
	if c.perTier != nil && c.perTier[tier] > 0 {
		c.perTier[tier]--
	}
	c.mu.Unlock()
	return c.Decrement()
}

// tierSnapshot returns a copy of the per-tier counts. Only ever has more than
// one entry when `tiers:` is configured; single-listener deployments see at
// most {"default": N}.
func (c *inflightCounter) tierSnapshot() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int, len(c.perTier))
	for k, v := range c.perTier {
		out[k] = int(v)
	}
	return out
}

// CreateInflightMiddleware returns middleware that increments the counter on
// entry and decrements on exit, emitting an InFlightRequestsEvent for each.
// The event's ByTier breakdown is populated only when more than one tier has
// ever been seen, so a config with no `tiers:` block emits byte-identical
// {Total} payloads.
func CreateInflightMiddleware(c *inflightCounter) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tier := shared.TierFromContext(r.Context()).Name
			total := c.incrementTier(tier)
			event.Emit(shared.InFlightRequestsEvent{Total: int(total), ByTier: byTierOrNil(c.tierSnapshot())})
			defer func() {
				total := c.decrementTier(tier)
				event.Emit(shared.InFlightRequestsEvent{Total: int(total), ByTier: byTierOrNil(c.tierSnapshot())})
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// byTierOrNil returns snapshot unless it carries at most one tier, in which
// case it returns nil so single-tier deployments never surface a ByTier
// breakdown (docs/intent/llama-swap-tiers.md, "Inflight/menu" section).
func byTierOrNil(snapshot map[string]int) map[string]int {
	if len(snapshot) <= 1 {
		return nil
	}
	return snapshot
}
