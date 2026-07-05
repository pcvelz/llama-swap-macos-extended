package server

import (
	"net/http"
	"strings"

	"golang.org/x/sync/semaphore"

	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/shared"
)

// defaultConcurrencyLimit caps simultaneous in-flight requests per model when
// the model config leaves concurrencyLimit unset. Matches the legacy
// proxy.Process default.
const defaultConcurrencyLimit = 10

// CreateConcurrencyMiddleware returns middleware that limits simultaneous
// model-dispatched requests per model. Each model gets a semaphore sized to
// its concurrencyLimit (or defaultConcurrencyLimit). A request that cannot
// immediately acquire a slot is rejected with 429. Models without a local
// config entry (e.g. peer-routed models) are not limited.
func CreateConcurrencyMiddleware(cfg config.Config) chain.Middleware {
	semaphores := make(map[string]*semaphore.Weighted, len(cfg.Models))
	for id, mc := range cfg.Models {
		limit := defaultConcurrencyLimit
		if mc.ConcurrencyLimit > 0 {
			limit = mc.ConcurrencyLimit
		}
		semaphores[id] = semaphore.NewWeighted(int64(limit))
	}

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
			defer sem.Release(1)
			next.ServeHTTP(w, r)
		})
	}
}
