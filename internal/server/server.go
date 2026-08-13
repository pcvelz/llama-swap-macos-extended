package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/perf"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/router"
	"github.com/mostlygeek/llama-swap/internal/shared"
)

// Server owns the HTTP mux, cross-cutting middleware, and the local/peer model
// dispatch. It supersedes router.Server: it builds the local and peer routers
// directly and dispatches between them itself.
type Server struct {
	cfg config.Config

	muxlog      *logmon.Monitor
	proxylog    *logmon.Monitor
	upstreamlog *logmon.Monitor

	perf     *perf.Monitor
	inflight *inflightCounter
	metrics  *metricsMonitor
	build    BuildInfo

	local router.LocalRouter
	peer  router.Router

	mux     *http.ServeMux
	handler http.Handler

	shutdownCtx  context.Context
	shutdownFn   context.CancelFunc
	shuttingDown atomic.Bool
}

// modelPostJSONRoutes are endpoints with a model id in the JSON request body.
var modelPostJSONRoutes = []string{
	"/v1/chat/completions",
	"/v1/responses",
	"/v1/completions",
	"/v1/messages",
	"/v1/messages/count_tokens",
	"/v1/embeddings",
	"/reranking",
	"/rerank",
	"/v1/rerank",
	"/v1/reranking",
	"/infill",
	"/completion",
	"/v1/audio/speech",
	"/v1/audio/voices",
	"/v1/images/generations",
	"/sdapi/v1/txt2img",
	"/sdapi/v1/img2img",

	// versionless routes, the /v/ is stripped before the request is forwarded upstream
	// see issue #728
	"/v/chat/completions",
	"/v/responses",
	"/v/completions",
	"/v/messages",
	"/v/messages/count_tokens",
	"/v/embeddings",
	"/v/rerank",
	"/v/reranking",
}

// modelPostFormRoutes are multipart/form-data endpoints with a model id in the form data
var modelPostFormRoutes = []string{
	"/v1/audio/transcriptions",
	"/v1/images/edits",
}

// modelGetRoutes are model-dispatched GET endpoints (the model arrives as a
// query parameter).
var modelGetRoutes = []string{
	"/v1/audio/voices",
	"/sdapi/v1/loras",
}

// BuildInfo carries version metadata surfaced by GET /api/version.
type BuildInfo struct {
	Version string
	Commit  string
	Date    string
}

func New(cfg config.Config, muxlog *logmon.Monitor, proxylog *logmon.Monitor, upstreamlog *logmon.Monitor, perfMon *perf.Monitor, build BuildInfo) (*Server, error) {
	var local router.LocalRouter
	var err error

	switch cfg.Routing.Router.Use {
	case "matrix":
		local, err = router.NewMatrix(cfg, proxylog, upstreamlog)
		if err != nil {
			return nil, fmt.Errorf("creating matrix router: %w", err)
		}
	default: // "group"
		local, err = router.NewGroup(cfg, proxylog, upstreamlog)
		if err != nil {
			return nil, fmt.Errorf("creating group router: %w", err)
		}
	}

	peer, err := router.NewPeer(cfg, proxylog)
	if err != nil {
		return nil, fmt.Errorf("creating peer router: %w", err)
	}

	shutdownCtx, shutdownFn := context.WithCancel(context.Background())
	s := &Server{
		cfg:         cfg,
		muxlog:      muxlog,
		proxylog:    proxylog,
		upstreamlog: upstreamlog,
		perf:        perfMon,
		inflight:    newInflightCounter(tierNames(cfg.Tiers)),
		metrics:     newMetricsMonitor(proxylog, cfg.MetricsMaxInMemory, cfg.CaptureBuffer),
		build:       build,
		local:       local,
		peer:        peer,
		shutdownCtx: shutdownCtx,
		shutdownFn:  shutdownFn,
	}
	s.routes()
	s.startPreload()
	return s, nil
}

// localPeerHandler dispatches a model-routed request to the local or peer
// router. The model is resolved once via shared.FetchContext.
func (s *Server) localPeerHandler(w http.ResponseWriter, r *http.Request) {
	stripVersionPrefix(r)

	data, err := shared.FetchContext(r, s.cfg)
	if err != nil {
		shared.SendError(w, r, shared.ErrNoModelInContext)
		return
	}

	switch {
	case s.local.Handles(data.ModelID):
		s.proxylog.Debugf("dispatch: using local process for model: %s", data.ModelID)
		s.local.ServeHTTP(w, r)
	case s.peer.Handles(data.ModelID):
		s.proxylog.Debugf("dispatch: using peer for model: %s", data.ModelID)
		s.peer.ServeHTTP(w, r)
	default:
		// Resident-alias resolution: ids matching cfg.ResidentAliases (e.g.
		// "claude-haiku-*" — Claude Code housekeeping and subagent
		// orchestrator turns — or "default" — hook-layer analyze calls) are
		// served by whichever local model is ALREADY resident. Only ready
		// processes qualify: resolving to a non-resident model would trigger
		// a load/swap, i.e. exactly the standing cross-model eviction
		// pressure a static alias was rejected for. With nothing resident we
		// fall through to the 404, which stays a cheap no-op for idle-time
		// housekeeping. The rewritten context is what the local router's
		// scheduler reads (FetchContext returns the stored context first),
		// so FIFO ordering and KV admission apply to the RESOLVED model; the
		// earlier per-model concurrency/filter middleware saw the alias id
		// and deliberately did not apply — those are keyed to real model
		// blocks, which an alias-only id never is.
		if resolved, ok := resolveResidentAlias(s.cfg, s.local.RunningModels(), data.Model); ok {
			s.proxylog.Debugf("dispatch: resident alias %s -> %s", data.Model, resolved)
			data.ModelID = resolved
			*r = *r.WithContext(shared.SetContext(r.Context(), data))
			s.local.ServeHTTP(w, r)
			return
		}
		shared.SendError(w, r, router.ErrNoRouterFound)
	}
}

// resolveResidentAlias maps a request whose model id matches a configured
// residentAliases pattern to the currently-resident local model. Returns
// false when the id matches no pattern or no local process is ready — it
// never causes a load. Pure function over its inputs so it is testable
// without a Server.
func resolveResidentAlias(cfg config.Config, running map[string]process.ProcessState, requested string) (string, bool) {
	if !cfg.MatchesResidentAlias(requested) {
		return "", false
	}
	ready := make([]string, 0, len(running))
	for id, state := range running {
		if state == process.StateReady {
			ready = append(ready, id)
		}
	}
	if len(ready) == 0 {
		return "", false
	}
	// Deterministic tie-break when several models are resident (possible in
	// multi-group configs): smallest id. Single-resident deployments never
	// hit this.
	sort.Strings(ready)
	return ready[0], true
}

// tierNames returns the configured tier names (excluding the implicit
// default), used to seed inflightCounter.perTier at construction.
func tierNames(tiers map[string]config.TierConfig) []string {
	if len(tiers) == 0 {
		return nil
	}
	names := make([]string, 0, len(tiers))
	for name := range tiers {
		names = append(names, name)
	}
	return names
}

// stripVersionPrefix rewrites versionless /v/... requests to their /... form
// before forwarding upstream (issue #728).
func stripVersionPrefix(r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/v/") {
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/v")
	}
}

// routes builds the mux, registers every route, and wraps the mux with the
// global CORS middleware.
func (s *Server) routes() {

	authMW := CreateAuthMiddleware(s.cfg)
	modelChain := chain.New(
		authMW,
		CreateRequestContextMiddleware(s.cfg),
		// Inflight counting runs BEFORE the per-model concurrency semaphore so
		// a request parked waiting for a permit is counted as "waiting" in the
		// per-tier numbers the menu bar shows (docs/intent/llama-swap-tiers.md
		// "Known soft spot") — previously it only counted once the semaphore
		// had already been acquired, hiding same-model semaphore contention
		// from the tier breakdown entirely.
		CreateInflightMiddleware(s.inflight),
		CreateConcurrencyMiddleware(s.cfg),
		CreateFilterMiddleware(s.cfg),
		CreateFormFilterMiddleware(s.cfg),
		CreateMetricsMiddleware(s.metrics, s.cfg),
	)
	// Custom endpoints only need auth.
	apiChain := chain.New(authMW)

	mux := http.NewServeMux()
	dispatch := http.HandlerFunc(s.localPeerHandler)

	for _, path := range modelPostJSONRoutes {
		mux.Handle("POST "+path, modelChain.Then(dispatch))
	}
	for _, path := range modelPostFormRoutes {
		mux.Handle("POST "+path, modelChain.Then(dispatch))
	}
	for _, path := range modelGetRoutes {
		mux.Handle("GET "+path, modelChain.Then(dispatch))
	}

	// llama-swap API + custom endpoints.
	mux.Handle("GET /v1/models", apiChain.ThenFunc(s.handleListModels))
	mux.Handle("GET /logs", apiChain.ThenFunc(s.handleLogs))
	mux.Handle("GET /logs/stream", apiChain.ThenFunc(s.handleLogStream))
	mux.Handle("GET /logs/stream/{logMonitorID...}", apiChain.ThenFunc(s.handleLogStream))

	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("GET /wol-health", handleHealth)
	mux.HandleFunc("GET /{$}", handleRootRedirect)

	// Embedded UI.
	mux.Handle("GET /ui/", chain.New(authMW).ThenFunc(s.handleUI))
	mux.HandleFunc("GET /favicon.ico", s.handleFavicon)

	// Prometheus metrics (wrapped by apiChain, matches the legacy endpoint).
	mux.Handle("GET /metrics", apiChain.ThenFunc(s.handleMetrics))

	// Operations endpoints.
	mux.Handle("GET /unload", apiChain.ThenFunc(s.handleUnload))
	mux.Handle("GET /running", apiChain.ThenFunc(s.handleRunning))

	// Upstream passthrough.
	mux.HandleFunc("GET /upstream", handleUpstreamRedirect)
	mux.Handle("/upstream/{upstreamPath...}", apiChain.ThenFunc(s.handleUpstream))

	// API group (API-key protected) consumed by the UI.
	mux.Handle("POST /api/models/unload", apiChain.ThenFunc(s.handleAPIUnloadAll))
	mux.Handle("POST /api/models/unload/{model...}", apiChain.ThenFunc(s.handleAPIUnloadModel))
	mux.Handle("GET /api/events", apiChain.ThenFunc(s.handleAPIEvents))
	mux.Handle("GET /api/metrics", apiChain.ThenFunc(s.handleAPIMetrics))
	mux.Handle("GET /api/performance", apiChain.ThenFunc(s.handleAPIPerformance))
	mux.Handle("GET /api/version", apiChain.ThenFunc(s.handleAPIVersion))
	mux.Handle("GET /api/captures/{id}", apiChain.ThenFunc(s.handleAPICapture))
	mux.Handle("POST /api/models/pin/{model}", apiChain.ThenFunc(s.handleAPIPin))
	mux.Handle("POST /api/models/unpin/{model}", apiChain.ThenFunc(s.handleAPIUnpin))

	s.mux = mux
	s.handler = chain.New(CreateRequestLogMiddleware(s.proxylog), CreateCORSMiddleware()).Then(mux)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// CloseStreams cancels long-lived response streams (Server-Sent Events) so a
// graceful httpServer.Shutdown can drain without blocking on them. It does not
// tear down routers; call Shutdown for that. Safe to call repeatedly.
func (s *Server) CloseStreams() {
	s.shutdownFn()
}

// Shutdown stops the local and peer routers in parallel. It is idempotent;
// repeated calls return nil without re-running shutdown.
//
// Callers must drain inflight HTTP requests (httpServer.Shutdown) before
// calling this, otherwise inflight requests 502 when their processes are torn
// down. Call CloseStreams before httpServer.Shutdown so SSE streams do not
// block the drain.
func (s *Server) Shutdown(timeout time.Duration) error {
	if !s.shuttingDown.CompareAndSwap(false, true) {
		return nil
	}
	s.shutdownFn()

	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error

	for _, rt := range []router.Router{s.local, s.peer} {
		if rt == nil {
			continue
		}
		wg.Add(1)
		go func(rt router.Router) {
			defer wg.Done()
			if err := rt.Shutdown(timeout); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}(rt)
	}

	wg.Wait()
	return errors.Join(errs...)
}
