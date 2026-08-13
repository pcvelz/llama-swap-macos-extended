package server

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/shared"
)

// apiUnloadTimeout is used by the API endpoints to stop processes
const apiUnloadTimeout = 10 * time.Second

// modelRecord is one entry in the OpenAI-compatible /v1/models listing.
type modelRecord struct {
	ID                  string         `json:"id"`
	Object              string         `json:"object"`
	Created             int64          `json:"created"`
	OwnedBy             string         `json:"owned_by"`
	Name                string         `json:"name,omitempty"`
	Description         string         `json:"description,omitempty"`
	Architecture        map[string]any `json:"architecture,omitempty"`
	Capabilities        map[string]any `json:"capabilities,omitempty"`
	SupportedParameters []string       `json:"supported_parameters,omitempty"`
	ContextLength       int            `json:"context_length,omitempty"`
	MaxContextLength    int            `json:"max_context_length,omitempty"`
	Meta                map[string]any `json:"meta,omitempty"`
}

// cappedMetadataKeys are top-level /v1/models fields produced by the
// capabilities renderer. If a model's metadata block defines any of these
// keys, the renderer's values win and the metadata keys are dropped.
var cappedMetadataKeys = map[string]struct{}{
	"architecture":         {},
	"capabilities":         {},
	"supported_parameters": {},
	"context_length":       {},
	"max_context_length":   {},
}

// renderCapabilities converts a model's capabilities config into additional
// /v1/models fields. Returns zero values when caps.Empty() is true.
func renderCapabilities(caps config.ModelCapConfig) (arch map[string]any, capsMap map[string]any, params []string, ctxLen int) {
	if caps.Empty() {
		return
	}

	hasIn := len(caps.In) > 0
	hasOut := len(caps.Out) > 0

	if hasIn || hasOut {
		arch = make(map[string]any)
	}
	if hasIn {
		arch["input_modalities"] = caps.In
	}
	if hasOut {
		arch["output_modalities"] = caps.Out
	}
	if hasIn && hasOut {
		arch["modality"] = strings.Join(caps.In, "+") + "->" + strings.Join(caps.Out, "+")
	}

	// Build capabilities map only if there's something to put in it.
	if hasIn || hasOut || caps.Tools || caps.Reranker {
		capsMap = make(map[string]any)
	}

	if hasIn {
		if contains(caps.In, "image") {
			capsMap["vision"] = true
		}
	}
	if hasIn && hasOut {
		if contains(caps.In, "audio") && contains(caps.Out, "text") {
			capsMap["audio_transcriptions"] = true
		}
		if contains(caps.In, "text") && contains(caps.Out, "audio") {
			capsMap["audio_speech"] = true
		}
		if contains(caps.In, "text") && contains(caps.Out, "image") {
			capsMap["image_generation"] = true
		}
		if contains(caps.In, "image") && contains(caps.Out, "image") {
			capsMap["image_to_image"] = true
		}
	}

	if caps.Tools {
		capsMap["function_calling"] = true
		params = []string{"tools", "tool_choice"}
	}

	if caps.Reranker {
		capsMap["reranker"] = true
	}

	if caps.Context > 0 {
		ctxLen = caps.Context
	}

	return
}

// contains reports whether s is present in ss.
func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// filterCappedMetadata returns metadata with renderer-owned keys removed.
func filterCappedMetadata(md map[string]any) map[string]any {
	if len(md) == 0 {
		return nil
	}
	filtered := make(map[string]any, len(md))
	for k, v := range md {
		if _, capped := cappedMetadataKeys[k]; !capped {
			filtered[k] = v
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	return filtered
}

// handleListModels serves the OpenAI-compatible model listing: local models
// (with optional aliases) plus peer models. When the request is identified as
// coming from an Anthropic client the SAME record set is encoded in the
// Anthropic model-list envelope instead, so Claude Code's gateway discovery can
// populate its /model picker. Identification: an `anthropic-version` header, OR
// a User-Agent starting with "claude-code" (case-insensitive prefix).
// WHY the UA check: Claude Code's actual gateway discovery call sends neither
// anthropic-version nor x-api-key (witnessed 2026-07-22 in the llama-swap log:
// "GET /v1/models" from UA "claude-code/2.1.217" returned the 919-byte OpenAI
// envelope where the Anthropic envelope is 1533 bytes) - its User-Agent is the
// only stable discriminator, and no OpenAI consumer of this endpoint
// (curl/llama.cpp clients) identifies as claude-code. Negotiation deliberately
// does NOT key on x-api-key: keyed OpenAI consumers may also send it, so OpenAI
// clients keep getting the byte-identical OpenAI envelope.
func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	records := s.collectModelRecords()

	// Echo the Origin so browser clients can read the listing.
	if origin := r.Header.Get("Origin"); origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", origin)
	}

	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("anthropic-version") != "" ||
		strings.HasPrefix(strings.ToLower(r.Header.Get("User-Agent")), "claude-code") {
		s.writeAnthropicModelList(w, records)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   records,
	})
}

// collectModelRecords builds the sorted OpenAI modelRecord list shared by both
// listing envelopes (extraction keeps the OpenAI encoding path byte-identical).
func (s *Server) collectModelRecords() []modelRecord {
	created := time.Now().Unix()
	data := make([]modelRecord, 0, len(s.cfg.Models))

	newRecord := func(id, name, description string, metadata map[string]any, caps config.ModelCapConfig) modelRecord {
		rec := modelRecord{
			ID:          id,
			Object:      "model",
			Created:     created,
			OwnedBy:     "llama-swap",
			Name:        strings.TrimSpace(name),
			Description: strings.TrimSpace(description),
		}
		rec.Architecture, rec.Capabilities, rec.SupportedParameters, rec.ContextLength = renderCapabilities(caps)
		rec.MaxContextLength = rec.ContextLength
		if !caps.Empty() {
			metadata = filterCappedMetadata(metadata)
		}
		if len(metadata) > 0 {
			rec.Meta = map[string]any{"llamaswap": metadata}
		}
		return rec
	}

	for id, mc := range s.cfg.Models {
		if mc.Unlisted {
			continue
		}
		data = append(data, newRecord(id, mc.Name, mc.Description, mc.Metadata, mc.Capabilities))

		if s.cfg.IncludeAliasesInList {
			for _, alias := range mc.Aliases {
				if alias := strings.TrimSpace(alias); alias != "" {
					data = append(data, newRecord(alias, mc.Name, mc.Description, mc.Metadata, mc.Capabilities))
				}
			}
		}
	}

	for peerID, peer := range s.cfg.Peers {
		for _, modelID := range peer.Models {
			data = append(data, newRecord(modelID, peerID+": "+modelID, "", map[string]any{"peerID": peerID}, config.ModelCapConfig{}))
		}
	}

	sort.Slice(data, func(i, j int) bool { return data[i].ID < data[j].ID })
	return data
}

// anthropicModelEntry is one model in the Anthropic /v1/models envelope — the
// shape Claude Code's gateway model discovery renders in its /model picker
// (verified against macaz-cli internal/gateway/server.go:430).
type anthropicModelEntry struct {
	ID              string   `json:"id"`
	Type            string   `json:"type"`
	DisplayName     string   `json:"display_name"`
	CreatedAt       string   `json:"created_at"`
	Description     string   `json:"description,omitempty"`
	Default         *bool    `json:"default,omitempty"`
	Efforts         []string `json:"efforts,omitempty"`
	InputModalities []string `json:"input_modalities,omitempty"`
}

// writeAnthropicModelList encodes the sorted records in the Anthropic
// model-list envelope: {"data":[...], "has_more":false, "first_id", "last_id"}.
func (s *Server) writeAnthropicModelList(w http.ResponseWriter, data []modelRecord) {
	entries := make([]anthropicModelEntry, 0, len(data))
	// seen maps a row's public ID to its index in entries, so an alias row
	// already emitted from the record list (includeAliasesInList) can be
	// REPLACED below by the metadata-enriched version rather than duplicated.
	seen := make(map[string]int, len(data))
	recByID := make(map[string]modelRecord, len(data))
	for _, rec := range data {
		recByID[rec.ID] = rec
		seen[rec.ID] = len(entries)
		entries = append(entries, s.anthropicEntry(rec.ID, rec, rec.ID))
	}

	// Claude Code's /model picker only surfaces discovered models whose IDs
	// start with "claude" (macaz-cli prefixes every public ID with
	// "claude-macaz-" for exactly this reason), so our raw GGUF IDs
	// (Qwen3.6-...) are dropped by the picker. llama-swap already routes
	// /v1/messages by alias (the claude-haiku-* rewrite works this way), so
	// emitting one row per alias whose ID starts with "claude"
	// (case-insensitive) — inheriting the PARENT model's metadata — makes a
	// picked claude-* alias reach the right child with ZERO routing changes.
	// Non-claude shell aliases (cq35 etc.) are deliberately skipped so they
	// stay out of the picker. Model IDs are iterated sorted for determinism
	// (Go map order is random).
	modelIDs := make([]string, 0, len(s.cfg.Models))
	for id := range s.cfg.Models {
		modelIDs = append(modelIDs, id)
	}
	sort.Strings(modelIDs)
	for _, id := range modelIDs {
		mc := s.cfg.Models[id]
		if mc.Unlisted {
			continue
		}
		rec, ok := recByID[id]
		if !ok {
			continue
		}
		for _, alias := range mc.Aliases {
			alias = strings.TrimSpace(alias)
			if alias == "" {
				continue
			}
			if !strings.HasPrefix(strings.ToLower(alias), "claude") {
				continue
			}
			if idx, dup := seen[alias]; dup {
				// Alias already listed via the record pass (includeAliasesInList):
				// enrich in place with the parent's Anthropic extras.
				entries[idx] = s.anthropicEntry(alias, rec, id)
				continue
			}
			seen[alias] = len(entries)
			entries = append(entries, s.anthropicEntry(alias, rec, id))
		}
	}

	// WHY a final full sort by id: alias rows are appended after the record
	// pass, so re-sorting keeps entry order — and therefore first_id/last_id —
	// deterministic regardless of how many alias rows were emitted.
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })

	resp := map[string]any{
		"data":     entries,
		"has_more": false, // single-shot listing; llama-swap never paginates
	}
	if len(entries) > 0 {
		resp["first_id"] = entries[0].ID
		resp["last_id"] = entries[len(entries)-1].ID
	}
	json.NewEncoder(w).Encode(resp)
}

// anthropicEntry builds one envelope row. id is the row's public ID (model ID
// or alias); rec carries the shared name/description/created fields; parentID
// keys the per-model config metadata lookup (== id for plain model rows, the
// owning model's ID for alias rows).
func (s *Server) anthropicEntry(id string, rec modelRecord, parentID string) anthropicModelEntry {
	e := anthropicModelEntry{
		ID:   id,
		Type: "model",
		// Fallback chain: explicit metadata override, then the configured
		// display name, then the raw row ID.
		DisplayName: rec.Name,
		// Anthropic uses a string timestamp, not a unix int.
		CreatedAt:   time.Unix(rec.Created, 0).UTC().Format(time.RFC3339),
		Description: rec.Description,
	}
	// Per-model config carries the Anthropic extras, looked up by parent model
	// ID inside the encoder (rather than threading ModelConfig through
	// modelRecord) so the OpenAI record shape stays untouched. Guard with
	// ok: peer models have no local config — omit extras for those.
	if mc, ok := s.cfg.Models[parentID]; ok {
		if v, ok := mc.Metadata["anthropic_display_name"].(string); ok && v != "" {
			e.DisplayName = v
		}
		// Never default `default` to true silently — only emit when the
		// operator explicitly set anthropic_default.
		if v, ok := mc.Metadata["anthropic_default"].(bool); ok {
			e.Default = &v
		}
		// yaml decodes a sequence into []any — convert and skip non-string
		// entries rather than failing the whole listing.
		if raw, ok := mc.Metadata["anthropic_efforts"].([]any); ok {
			for _, item := range raw {
				if str, ok := item.(string); ok {
					e.Efforts = append(e.Efforts, str)
				}
			}
		}
		if len(mc.Capabilities.In) > 0 {
			e.InputModalities = mc.Capabilities.In
		}
	}
	if e.DisplayName == "" {
		e.DisplayName = id
	}
	return e
}

// runningModel is one entry in the /running listing.
type runningModel struct {
	Model       string `json:"model"`
	State       string `json:"state"`
	Cmd         string `json:"cmd"`
	Proxy       string `json:"proxy"`
	TTL         int    `json:"ttl"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// handleUnload stops every running local process. Peer models are remote and
// unaffected.
func (s *Server) handleUnload(w http.ResponseWriter, r *http.Request) {
	s.local.Unload(apiUnloadTimeout)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

// handleRunning lists local processes that are not stopped, joining each model
// ID against its config for the cmd/proxy/ttl/name/description metadata.
func (s *Server) handleRunning(w http.ResponseWriter, r *http.Request) {
	states := s.local.RunningModels()
	list := make([]runningModel, 0, len(states))
	for id, state := range states {
		mc := s.cfg.Models[id]
		list = append(list, runningModel{
			Model:       id,
			State:       string(state),
			Cmd:         mc.Cmd,
			Proxy:       mc.Proxy,
			TTL:         mc.UnloadAfter,
			Name:        mc.Name,
			Description: mc.Description,
		})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Model < list[j].Model })

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"running": list})
}

// discardResponseWriter satisfies http.ResponseWriter for preload requests,
// dropping the body while capturing the status code.
type discardResponseWriter struct {
	header http.Header
	status int
}

func (d *discardResponseWriter) Header() http.Header {
	if d.header == nil {
		d.header = make(http.Header)
	}
	return d.header
}

func (d *discardResponseWriter) Write(p []byte) (int, error) { return len(p), nil }

func (d *discardResponseWriter) WriteHeader(status int) { d.status = status }

// startPreload fires a background GET / at every model named in
// Hooks.OnStartup.Preload so they are warm before the first real request.
// Preload names are already resolved to real model IDs by config loading.
func (s *Server) startPreload() {
	models := s.cfg.Hooks.OnStartup.Preload
	if len(models) == 0 {
		return
	}
	go func() {
		for _, modelID := range models {
			if !s.local.Handles(modelID) {
				s.proxylog.Warnf("preload: model %s is not a local model, skipping", modelID)
				continue
			}
			s.proxylog.Infof("preloading model: %s", modelID)

			req, err := http.NewRequestWithContext(s.shutdownCtx, http.MethodGet, "/", nil)
			if err != nil {
				continue
			}
			req = req.WithContext(shared.SetContext(req.Context(), shared.ReqContextData{Model: modelID, ModelID: modelID}))

			dw := &discardResponseWriter{status: http.StatusOK}
			s.local.ServeHTTP(dw, req)

			success := dw.status < http.StatusBadRequest
			if !success {
				s.proxylog.Errorf("failed to preload model %s: status %d", modelID, dw.status)
			}
			event.Emit(shared.ModelPreloadedEvent{ModelName: modelID, Success: success})
		}
	}()
}

// handleMetrics serves Prometheus-format performance metrics. Returns 503 when
// performance monitoring is disabled.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if s.perf == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("# performance monitor not available\n"))
		return
	}
	s.perf.MetricsHandler().ServeHTTP(w, r)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func handleRootRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/ui", http.StatusFound)
}

func handleUpstreamRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/ui/models", http.StatusFound)
}

// handleUpstream proxies ANY request under /upstream/<model>/<path> directly to
// the model's process, bypassing model dispatch by body/query inspection.
func (s *Server) handleUpstream(w http.ResponseWriter, r *http.Request) {
	upstreamPath := r.PathValue("upstreamPath")

	searchName, modelID, remainingPath, found := findModelInPath(s.cfg, "/"+upstreamPath)
	if !found {
		shared.SendResponse(w, r, http.StatusNotFound, "model not found")
		return
	}

	// Redirect /upstream/model to /upstream/model/ so relative URLs in upstream
	// responses resolve. 301 for GET/HEAD, 308 otherwise to preserve the method.
	if remainingPath == "/" && !strings.HasSuffix(r.URL.Path, "/") {
		newPath := "/upstream/" + searchName + "/"
		if r.URL.RawQuery != "" {
			newPath += "?" + r.URL.RawQuery
		}
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			http.Redirect(w, r, newPath, http.StatusMovedPermanently)
		} else {
			http.Redirect(w, r, newPath, http.StatusPermanentRedirect)
		}
		return
	}

	// Strip the /upstream/<model> prefix before forwarding.
	r.URL.Path = remainingPath
	// Pin the resolved model so the router skips body/query extraction.
	*r = *r.WithContext(shared.SetContext(r.Context(), shared.ReqContextData{Model: searchName, ModelID: modelID}))

	switch {
	case s.local.Handles(modelID):
		// Guard: GET and HEAD to /upstream/<model>/ (root path) must NOT trigger a
		// model load. They only return useful content once the model is already
		// running — the 415 response from llama-server's root is load-cycle noise,
		// not a real inference path. Allowing GET to trigger loads lets any external
		// health-poller (Python-urllib, curl, browser prefetch) accidentally eager-
		// reload an idle model that should stay stopped until a real POST arrives.
		//
		// POST/PUT/DELETE always pass through — they carry real inference payloads
		// and must queue a load if the model is stopped.
		//
		// The web UI's "Load" button and the macOS menu helper are expected to call
		// POST /upstream/<model>/ (updated alongside this guard).
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && remainingPath == "/" {
			states := s.local.RunningModels()
			if st, ok := states[modelID]; !ok || st != process.StateReady {
				shared.SendResponse(w, r, http.StatusServiceUnavailable,
					"model not loaded — use POST /upstream/<model>/ to trigger a load")
				return
			}
		}
		s.local.ServeHTTP(w, r)
	case s.peer.Handles(modelID):
		s.peer.ServeHTTP(w, r)
	default:
		shared.SendResponse(w, r, http.StatusNotFound, "no router for model "+modelID)
	}
}

// findModelInPath walks a slash-separated path, building up segments until one
// matches a configured model. This resolves model names that contain slashes
// (e.g. "author/model"). Returns the matched name, its real model ID, the
// remaining path, and whether a match was found.
func findModelInPath(cfg config.Config, path string) (searchName, realName, remainingPath string, found bool) {
	parts := strings.Split(strings.TrimSpace(path), "/")
	name := ""

	for i, part := range parts {
		if part == "" {
			continue
		}
		if name == "" {
			name = part
		} else {
			name = name + "/" + part
		}

		if modelID, ok := cfg.RealModelName(name); ok {
			return name, modelID, "/" + strings.Join(parts[i+1:], "/"), true
		}
	}

	return "", "", "", false
}
