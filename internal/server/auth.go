package server

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/shared"
)

// CreateAuthMiddleware returns middleware that validates API keys when the
// config declares any. It accepts the key via Authorization: Bearer,
// Authorization: Basic (password field), or x-api-key. When no keys are
// configured the middleware is a pass-through.
func CreateAuthMiddleware(cfg config.Config) chain.Middleware {
	keys := cfg.RequiredAPIKeys
	return func(next http.Handler) http.Handler {
		if len(keys) == 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			provided := shared.ExtractAPIKey(r)

			valid := false
			for _, key := range keys {
				if provided == key {
					valid = true
					break
				}
			}
			if !valid {
				w.Header().Set("WWW-Authenticate", `Basic realm="llama-swap"`)
				shared.SendResponse(w, r, http.StatusUnauthorized, "unauthorized: invalid or missing API key")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// CreateRequestContextMiddleware returns middleware that extracts model and
// auth info from the request into the context. Requests where no model can be
// identified are rejected with a 404.
//
// It also tags the request with a fresh shared.PreemptHandle wrapping a
// cancellable child of the request's context - as early as possible, before
// CreateConcurrencyMiddleware's per-model semaphore can ever block on it.
// Every layer downstream that can hold a request up (the semaphore, the FIFO
// scheduler) reuses this SAME handle instead of minting its own, so a
// preemption at any layer cancels through the one Flag+Cancel pair
// (docs/intent/llama-swap-tiers.md "Known soft spot"; see
// internal/router/base.go ServeHTTP, which reads this handle back out of the
// context).
func CreateRequestContextMiddleware(cfg config.Config) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			data, err := shared.FetchContext(r, cfg)
			if err != nil {
				shared.SendError(w, r, shared.ErrNoModelInContext)
				return
			}
			_ = data

			parent := r.Context()
			ctx, cancel := context.WithCancel(parent)
			// Parent (the pre-wrap context) stays alive after Cancel fires -
			// see shared.PreemptHandle.Parent for why a transparent replay
			// attempt needs it.
			handle := &shared.PreemptHandle{Flag: new(atomic.Bool), Cancel: cancel, Parent: parent}
			*r = *r.WithContext(shared.WithPreemptHandle(ctx, handle))

			next.ServeHTTP(w, r)
		})
	}
}

// CreateCORSMiddleware returns middleware that answers OPTIONS preflight
// requests with permissive CORS headers (see issues #81, #77, #42). Non-OPTIONS
// requests pass through untouched.
func CreateCORSMiddleware() chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodOptions {
				next.ServeHTTP(w, r)
				return
			}

			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			if headers := r.Header.Get("Access-Control-Request-Headers"); headers != "" {
				w.Header().Set("Access-Control-Allow-Headers", sanitizeAccessControlRequestHeaderValues(headers))
			} else {
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Accept, X-Requested-With")
			}
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
		})
	}
}

func isTokenChar(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z':
	case r >= 'A' && r <= 'Z':
	case r >= '0' && r <= '9':
	case strings.ContainsRune("!#$%&'*+-.^_`|~", r):
	default:
		return false
	}
	return true
}

// sanitizeAccessControlRequestHeaderValues drops any header names that contain
// characters outside the HTTP token grammar before echoing them back.
func sanitizeAccessControlRequestHeaderValues(headerValues string) string {
	parts := strings.Split(headerValues, ",")
	valid := make([]string, 0, len(parts))

	for _, p := range parts {
		v := strings.TrimSpace(p)
		if v == "" {
			continue
		}

		validPart := true
		for _, c := range v {
			if !isTokenChar(c) {
				validPart = false
				break
			}
		}
		if validPart {
			valid = append(valid, v)
		}
	}

	return strings.Join(valid, ", ")
}
