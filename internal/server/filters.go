package server

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// CreateFilterMiddleware returns middleware that applies per-model request-body
// filters to JSON requests before they are forwarded upstream:
//
//   - UseModelName rewrite (issue #69)
//   - StripParams removal (issue #174)
//   - SetParams injection (issue #453)
//   - SetParamsByID per-alias overrides
//   - image content blocks replaced by a text note for models whose declared
//     capabilities lack image input (filters_image.go)
//
// Non-JSON requests (GET, multipart forms) pass through untouched. The buffered
// body is re-attached with Content-Length / Transfer-Encoding cleanup so the
// downstream reverse proxy forwards the correct bytes (see issue #11).
func CreateFilterMiddleware(cfg config.Config) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.Contains(r.Header.Get("Content-Type"), "application/json") {
				next.ServeHTTP(w, r)
				return
			}

			data, err := swaputil.FetchContext(r, cfg)
			if err != nil {
				swaputil.SendError(w, r, swaputil.ErrNoModelInContext)
				return
			}

			useModelName, filters, ok := resolveFilters(cfg, data.Model)
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			textOnly := modelRejectsImages(cfg, data.Model)

			// FetchContext already buffered the body once; reuse it instead of
			// reading r.Body again here. Fall back to a direct read only if
			// nothing was buffered upstream (e.g. an empty body).
			body := data.Body
			if body == nil {
				var err error
				body, err = io.ReadAll(r.Body)
				if err != nil {
					swaputil.SendResponse(w, r, http.StatusBadRequest, "could not read request body")
					return
				}
			}

			body, err = applyFilters(body, data.Model, useModelName, filters)
			if err != nil {
				swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
				return
			}
			if textOnly {
				body, err = replaceImageBlocks(body, data.Model)
				if err != nil {
					swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
					return
				}
			}

			r.Body = io.NopCloser(bytes.NewReader(body))
			r.Header.Del("Transfer-Encoding")
			r.Header.Set("Content-Length", strconv.Itoa(len(body)))
			r.ContentLength = int64(len(body))

			// Persist the filtered body on the shared context so downstream
			// middleware (metrics captures) observes the mutated bytes instead
			// of re-reading r.Body itself.
			data.Body = body
			*r = *r.WithContext(swaputil.SetContext(r.Context(), data))

			next.ServeHTTP(w, r)
		})
	}
}

// CreateFormFilterMiddleware returns middleware that applies the UseModelName
// rewrite (issue #69) to multipart/form-data requests before they are forwarded
// upstream. JSON-body filters (StripParams, SetParams) do not apply to form
// endpoints; only the "model" field is rewritten.
//
// Non-multipart requests pass through untouched. When a rewrite is needed the
// form is reconstructed and re-attached with Content-Type / Content-Length
// cleanup so the downstream reverse proxy forwards the correct bytes.
//
// Transcription routing (2026-10-04): when cfg.TranscriptionEndpoint is set,
// POST /v1/audio/transcriptions is reverse-proxied directly to that endpoint,
// bypassing the model dispatch chain entirely. This is architecturally correct
// for standalone STT services (whisper.cpp) that share no KV cache, swap-grace,
// or slot-affinity machinery with the resident chat model. The request never
// enters the FIFO scheduler, so there is no queue wait or model-load wait.
func CreateFormFilterMiddleware(cfg config.Config) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.Contains(r.Header.Get("Content-Type"), "multipart/form-data") {
				next.ServeHTTP(w, r)
				return
			}

			// WHY: transcription requests to a standalone STT endpoint bypass the
			// model dispatch chain entirely. The request is reverse-proxied
			// directly to the configured endpoint; no model rewrite, no scheduler
			// admission, no KV pool interaction. This runs BEFORE FetchContext so
			// a missing model in the form (e.g. Claude Code sends model=<chat-model>
			// which is valid for dispatch but irrelevant here) does not short-circuit.
			if cfg.TranscriptionEndpoint != "" && r.URL.Path == "/v1/audio/transcriptions" {
				proxyTranscription(w, r, cfg.TranscriptionEndpoint)
				return
			}

			data, err := swaputil.FetchContext(r, cfg)
			if err != nil {
				swaputil.SendError(w, r, swaputil.ErrNoModelInContext)
				return
			}

			useModelName, _, ok := resolveFilters(cfg, data.Model)
			if !ok || useModelName == "" {
				// WHY: even when the requested model has no filter config, a
				// transcription request must still be rewritten if a dedicated
				// transcription model is configured. Without this branch voice
				// dictation on cq* sessions routes to the text-only chat model
				// and silently fails (2026-10-04 incident).
				if cfg.TranscriptionModel != "" && r.URL.Path == "/v1/audio/transcriptions" {
					updated, err := swaputil.ReplaceRequestModel(r, data.Model, cfg.TranscriptionModel)
					if err != nil {
						swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
						return
					}
					// WHY: the dispatch reads data.ModelID from the context, not
					// the request body. Rewriting only the form field leaves the
					// context pointing at the original chat model, so the audio
					// still lands on the text-only LLM. Update both.
					data.Model = cfg.TranscriptionModel
					data.ModelID = cfg.TranscriptionModel
					newCtx := swaputil.SetContext(r.Context(), data)
					updated = updated.WithContext(newCtx)
					next.ServeHTTP(w, updated)
					return
				}
				next.ServeHTTP(w, r)
				return
			}

			// WHY: /v1/audio/transcriptions always routes to the dedicated
			// transcription model (whisper), regardless of which chat model the
			// session is using. Claude Code sends model=<current-chat-model> in
			// the multipart form; without this rewrite the audio lands on a
			// text-only llama-server that cannot transcribe.
			if cfg.TranscriptionModel != "" && r.URL.Path == "/v1/audio/transcriptions" {
				useModelName = cfg.TranscriptionModel
			}

			updated, err := swaputil.ReplaceRequestModel(r, data.Model, useModelName)
			if err != nil {
				swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
				return
			}

			// WHY: for transcription requests the context MUST point at the
			// whisper model, not the original chat model. The dispatch reads
			// data.ModelID from the context to decide which child serves the
			// request; leaving it on cq27 sends audio to a text-only LLM.
			// For non-transcription rewrites we keep the original context so
			// routing and metrics still identify the configured model.
			if cfg.TranscriptionModel != "" && r.URL.Path == "/v1/audio/transcriptions" {
				data.Model = useModelName
				data.ModelID = useModelName
				newCtx := swaputil.SetContext(r.Context(), data)
				updated = updated.WithContext(newCtx)
			} else {
				// UseModelName changes only the model name sent upstream. Keep the
				// original request context so routing and metrics still identify
				// the configured model.
				updated = updated.WithContext(r.Context())
			}
			next.ServeHTTP(w, updated)
		})
	}
}

// proxyTranscription reverse-proxies a multipart transcription request directly
// to the configured STT endpoint (e.g. whisper-server /inference). The request
// body is forwarded verbatim; the response is streamed back to the client.
// This bypasses the model dispatch chain entirely - no scheduler admission,
// no KV pool interaction, no swap-grace wait.
func proxyTranscription(w http.ResponseWriter, r *http.Request, endpoint string) {
	target, err := url.Parse(endpoint)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadGateway, "invalid transcription endpoint: "+err.Error())
		return
	}

	// Buffer the multipart body so we can set Content-Length on the proxied request.
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "could not read transcription body: "+err.Error())
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

	proxiedReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target.String(), bytes.NewReader(bodyBytes))
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadGateway, "could not create proxied request: "+err.Error())
		return
	}

	// Forward the multipart Content-Type (with its boundary) verbatim.
	proxiedReq.Header.Set("Content-Type", r.Header.Get("Content-Type"))
	proxiedReq.ContentLength = int64(len(bodyBytes))

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(proxiedReq)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadGateway, "transcription endpoint unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()

	// Copy response headers and status.
	for key, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// modelRejectsImages reports whether a requested LOCAL model has declared
// capabilities that leave out image input. Undeclared capabilities and peers
// return false: the proxy only acts on an explicit statement, never a guess
// (see replaceImageBlocks).
func modelRejectsImages(cfg config.Config, requested string) bool {
	realName, found := cfg.RealModelName(requested)
	if !found {
		return false
	}
	caps := cfg.Models[realName].Capabilities
	return !caps.Empty() && !contains(caps.In, "image")
}

// resolveFilters returns the filter settings for a requested model. UseModelName
// only applies to local models; peers carry filters but no name rewrite.
func resolveFilters(cfg config.Config, requested string) (useModelName string, filters config.Filters, ok bool) {
	if realName, found := cfg.RealModelName(requested); found {
		mc := cfg.Models[realName]
		return mc.UseModelName, mc.Filters.Filters, true
	}
	if peerID, _, found := cfg.ResolvePeerModel(requested); found {
		return "", cfg.Peers[peerID].Filters, true
	}
	return "", config.Filters{}, false
}

// residentAliasModelName returns body with its model field set to the resolved
// model's useModelName, for a resident-alias request (an id such as
// claude-haiku-* that dispatch maps to whichever local model is resident).
// CreateFilterMiddleware ran before that mapping, found no model block for the
// alias id and applied nothing. stripParams and setParams stay unapplied on
// purpose (see dispatch), but the name rewrite cannot: an engine that checks
// the model field (Splash answers 404 model_not_found to any name it does not
// serve) would refuse every alias turn. changed is false when the model sets
// no useModelName or the body is not JSON.
func residentAliasModelName(cfg config.Config, resolved, contentType string, body []byte) (rewritten []byte, changed bool, err error) {
	useModelName := cfg.Models[resolved].UseModelName
	if useModelName == "" || len(body) == 0 || !strings.Contains(contentType, "application/json") {
		return body, false, nil
	}
	if rewritten, err = sjson.SetBytes(body, "model", useModelName); err != nil {
		return nil, false, fmt.Errorf("error rewriting model name in JSON: %w", err)
	}
	return rewritten, true, nil
}

// applyFilters rewrites the JSON body in place. Order matches the legacy
// ProxyManager: useModelName, stripParams, setParams, then setParamsByID (which
// can override setParams).
func applyFilters(body []byte, requested, useModelName string, f config.Filters) ([]byte, error) {
	var err error

	if useModelName != "" {
		if body, err = sjson.SetBytes(body, "model", useModelName); err != nil {
			return nil, fmt.Errorf("error rewriting model name in JSON: %w", err)
		}
	}

	for _, param := range f.SanitizedStripParams() {
		if body, err = sjson.DeleteBytes(body, param); err != nil {
			return nil, fmt.Errorf("error stripping parameter %s from request", param)
		}
	}

	setParams, setKeys, setSoft := f.SanitizedSetParams()
	byID, byIDKeys, byIDSoft := f.SanitizedSetParamsByID(requested)

	// Set-if-undefined keys ("key?", issue #1052) only fill parameters the body
	// does not carry at that point in the pipeline. Filters apply like a pipe —
	// stripParams | setParams | setParamsByID — so a stripped key counts as
	// undefined, and a key an earlier stage set counts as defined (a "key?" in
	// setParamsByID is a no-op when setParams already set it).
	for _, key := range setKeys {
		if setSoft[key] && gjson.GetBytes(body, key).Exists() {
			continue
		}
		if body, err = sjson.SetBytes(body, key, setParams[key]); err != nil {
			return nil, fmt.Errorf("error setting parameter %s in request", key)
		}
	}

	for _, key := range byIDKeys {
		if byIDSoft[key] && gjson.GetBytes(body, key).Exists() {
			continue
		}
		if body, err = sjson.SetBytes(body, key, byID[key]); err != nil {
			return nil, fmt.Errorf("error setting parameter %s in request", key)
		}
	}

	return body, nil
}
