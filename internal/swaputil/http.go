package swaputil

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type contextkey struct {
	name string
}

type ReqContextData struct {
	ApiKey           string
	Model            string
	ModelID          string
	Streaming        bool
	SendLoadingState bool
	// Metadata is a request-scoped key/value bag that handlers may mutate
	// while processing. The metrics middleware copies it into ActivityLogEntry.
	Metadata map[string]string

	// Tier is the entry-point tier this request arrived through (see tier.go).
	// Set from the request context at extraction time, which is itself tagged
	// by the listener (llama-swap.go) before routing ever sees the request.
	// DefaultTier for every request on the main listener / when no tiers are
	// configured.
	Tier Tier

	// Body is the raw request body buffered once by extractContext (nil for
	// GET requests, which carry no body). Downstream middleware (filters,
	// metrics) reuse this instead of re-reading r.Body, so the body is only
	// buffered into memory once per request rather than once per middleware.
	// A middleware that mutates the body (e.g. filters.go rewriting JSON
	// params) must call SetContext with an updated copy so later middleware
	// observes the mutated bytes.
	Body []byte
}

const MaxMultiPartSize = 32 << 20

var (
	ReqContextKey        = &contextkey{"context"}
	ErrNoModelInContext  = fmt.Errorf("no model in request context")
	ErrNoRouterFound     = fmt.Errorf("no router found for model")
	ErrNoPeerModelFound  = fmt.Errorf("peer model not found")
	ErrNoLocalModelFound = fmt.Errorf("local model not found")
	// ErrModelNotLoaded answers a slot-free status read (GET/HEAD) whose model
	// is known but not resident and not loading. Such a read must never be
	// the request that queues or wins a swap.
	ErrModelNotLoaded = fmt.Errorf("model not loaded")
)

// IsWebSocketUpgrade reports whether r contains a valid websocket protocol
// upgrade request. Header token comparisons are case-insensitive and support
// comma-separated or repeated header values.
func IsWebSocketUpgrade(r *http.Request) bool {
	return headerContainsToken(r.Header.Values("Connection"), "upgrade") &&
		headerContainsToken(r.Header.Values("Upgrade"), "websocket")
}

// IsStatusRead reports whether r only observes a model (GET/HEAD /slots,
// /props, /metrics, /health) rather than using it. Such a read must never
// queue a swap, restart the cooldown, or reset the idle TTL; one predicate
// for all three keeps the cooldown clock and the unload clock agreeing on
// what counts as use. A websocket upgrade is a GET but is a real session.
// count_tokens is a POST and part of a real turn, so it is use.
func IsStatusRead(r *http.Request) bool {
	return (r.Method == http.MethodGet || r.Method == http.MethodHead) && !IsWebSocketUpgrade(r)
}

func headerContainsToken(values []string, token string) bool {
	for _, value := range values {
		for part := range strings.SplitSeq(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// ShouldIgnoreWebsocket reports whether r is a websocket request whose local
// model configuration opts out of websocket lifecycle activity.
func ShouldIgnoreWebsocket(r *http.Request, cfg config.Config) bool {
	if !IsWebSocketUpgrade(r) {
		return false
	}
	data, err := FetchContext(r, cfg)
	if err != nil {
		return false
	}
	mc, ok := cfg.Models[data.ModelID]
	return ok && mc.Compat.IgnoreWebsockets
}

func SendError(w http.ResponseWriter, r *http.Request, err error) {
	var httpErr HTTPError
	if errors.As(err, &httpErr) {
		for k, v := range httpErr.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(httpErr.StatusCode())
		w.Write(httpErr.Body())
		return
	}

	switch {
	case errors.Is(err, ErrNoModelInContext):
		SendResponse(w, r, http.StatusNotFound, "no model id could be identified")
	case errors.Is(err, ErrNoPeerModelFound):
		SendResponse(w, r, http.StatusNotFound, "no peer found for requested model")
	case errors.Is(err, ErrNoLocalModelFound):
		SendResponse(w, r, http.StatusNotFound, "no local server found for requested model")
	case errors.Is(err, ErrNoRouterFound):
		SendResponse(w, r, http.StatusNotFound, "no router for requested model")
	case errors.Is(err, ErrModelNotLoaded):
		// A status read on a model that is not resident: 503 rather than a
		// queued swap, so a poller learns "not loaded" in microseconds and
		// never occupies the scheduler queue (2026-09-10).
		SendResponse(w, r, http.StatusServiceUnavailable, "model not loaded; status reads do not trigger a load")
	default:
		SendResponse(w, r, http.StatusInternalServerError, fmt.Sprintf("unspecific error: %v", err))
	}
}

// SendResponse detects what content type the client prefers and returns an error response in that format.
func SendResponse(w http.ResponseWriter, r *http.Request, status int, message string) {
	acceptHeader := r.Header.Get("Accept")
	if strings.Contains(acceptHeader, "text/plain") {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(status)
		w.Write([]byte(fmt.Sprintf("llama-swap: %s", message)))
		return
	}

	if strings.Contains(acceptHeader, "text/html") {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(status)
		w.Write([]byte(fmt.Sprintf(`<html><body><h1>llama-swap</h1><p>%s</p></body></html>`, html.EscapeString(message))))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	resp, err := json.Marshal(map[string]string{"src": "llama-swap", "error": message})
	if err != nil {
		w.Write([]byte(`{"src":"llama-swap", "error": "failed to marshal response"}`))
		return
	}
	w.Write(resp)
}

// FetchContext will attempt to get the model id from the context, then
// from an /upstream/<model> path prefix, then from the request body/query.
// If it extracts the model it will store it in the context for downstream
// handlers. An error will be returned when a model cannot be identified.
func FetchContext(r *http.Request, cfg config.Config) (ReqContextData, error) {
	data, ok := ReadContext(r.Context())
	if ok {
		return data, nil
	}

	if strings.HasPrefix(r.URL.Path, "/upstream/") {
		if data, ok := extractUpstreamContext(r, cfg); ok {
			StampModelAlias(&data, cfg)
			*r = *r.WithContext(SetContext(r.Context(), data))
			return data, nil
		}
		return ReqContextData{}, ErrNoModelInContext
	}

	if data, err := extractContext(r); err == nil && data.Model != "" {
		realName, _ := cfg.RealModelName(data.Model)
		if realName == "" {
			realName = data.Model
		}
		data.ModelID = realName
		if mc, ok := cfg.Models[realName]; ok {
			data.SendLoadingState = mc.SendLoadingState != nil && *mc.SendLoadingState
		}
		StampModelAlias(&data, cfg)
		*r = *r.WithContext(SetContext(r.Context(), data))
		return data, nil
	}

	return ReqContextData{}, ErrNoModelInContext
}

// StampModelAlias records the resolved model's display alias on data's
// metadata bag. Called once per request, at the point the model id is known
// and before the context is stored, so every consumer of the bag (in-flight
// entries, activity log) reads the same answer without repeating the lookup.
// Re-stamped when a later layer REwrites ModelID (server.go's resident-alias
// resolution), since the alias must name the model that actually serves.
func StampModelAlias(data *ReqContextData, cfg config.Config) {
	if data.ModelID == "" {
		return
	}
	if data.Metadata == nil {
		data.Metadata = map[string]string{}
	}
	data.Metadata["model_alias"] = ShortestAlias(cfg, data.ModelID)
}

// EstimateTokens gives a cheap, conservative estimate of a request's context
// size in tokens, from its already-buffered body — used only by KV-aware
// admission (config.FifoConfig.KVPoolTokens / router/scheduler.FIFO.kvAdmit).
// It is intentionally NOT a real tokenizer: this only needs to be a rough,
// fast proxy the scheduler can use to decide whether to park a request behind
// another, not an exact count.
//
// Rule: len(body) / 4. Four bytes/token is roughly the middle of the range
// for mixed English prose (~4 chars/token) and code (denser, closer to
// ~3 chars/token — lots of short symbols and whitespace). Dividing by 4
// slightly UNDERestimates token count for code-heavy bodies and slightly
// OVERestimates it for prose-heavy ones; there is no single constant that is
// exact for both. This is a known, accepted limitation: the estimate only
// needs to keep two large concurrent requests from being admitted together
// when they'd actually oversubscribe the KV pool, and gets checked against
// reality by whatever headroom the configured kvPoolTokens leaves under the
// server's real --ctx-size. An empty/absent body (GET requests) estimates 0,
// which never blocks admission on its own.
func EstimateTokens(body []byte) int {
	return len(body) / 4
}

// ExtractModel returns the model name encoded in a request without caching
// request data in its context.
func ExtractModel(r *http.Request) (string, error) {
	data, err := extractContext(r)
	return data.Model, err
}

// ReplaceRequestModel replaces model with replacement wherever the request
// encodes its model ID. It returns a request whose cached model context has
// been invalidated so downstream handlers resolve the replacement normally.
func ReplaceRequestModel(r *http.Request, model, replacement string) (*http.Request, error) {
	if strings.HasPrefix(r.URL.Path, "/upstream/") {
		upstreamPath := strings.TrimPrefix(r.PathValue("upstreamPath"), "/")
		if upstreamPath != model && !strings.HasPrefix(upstreamPath, model+"/") {
			return r, nil
		}

		// Preserve the client's escaping of the path after the model name before
		// URL.Path is rewritten below.
		escapedRemaining := EscapedPathSuffix(r.URL.EscapedPath(), "/upstream/"+model)

		remainingPath := strings.TrimPrefix(upstreamPath, model)
		rewrittenPath := replacement + remainingPath
		if replacement == "" {
			rewrittenPath = ""
		}
		r.SetPathValue("upstreamPath", rewrittenPath)
		r.URL.Path = "/upstream/" + rewrittenPath
		r.URL.RawPath = ""
		if replacement != "" && escapedRemaining != "" {
			prefix := (&url.URL{Path: "/upstream/" + replacement}).EscapedPath()
			r.URL.RawPath = prefix + escapedRemaining
		}
		return invalidateRequestContext(r), nil
	}

	current, err := ExtractModel(r)
	if err != nil {
		return r, err
	}
	if current != model {
		return r, nil
	}

	if r.Method == http.MethodGet {
		query := r.URL.Query()
		query.Set("model", replacement)
		r.URL.RawQuery = query.Encode()
		return invalidateRequestContext(r), nil
	}

	contentType := r.Header.Get("Content-Type")
	switch {
	case strings.Contains(contentType, "application/json"):
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return r, fmt.Errorf("could not read request body")
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		body, err = sjson.SetBytes(body, "model", replacement)
		if err != nil {
			return r, fmt.Errorf("could not rewrite model in JSON body: %w", err)
		}
		replaceRequestBody(r, body)
	case strings.Contains(contentType, "multipart/form-data"):
		if err := r.ParseMultipartForm(MaxMultiPartSize); err != nil {
			return r, fmt.Errorf("could not parse multipart form: %w", err)
		}
		form := r.MultipartForm
		defer form.RemoveAll()
		body, rewrittenContentType, err := replaceMultipartModel(form, replacement)
		if err != nil {
			return r, err
		}
		r.MultipartForm = nil
		r.Form = nil
		r.PostForm = nil
		r.Header.Set("Content-Type", rewrittenContentType)
		replaceRequestBody(r, body)
	case strings.Contains(contentType, "application/x-www-form-urlencoded"):
		if err := r.ParseForm(); err != nil {
			return r, fmt.Errorf("could not parse form: %w", err)
		}
		r.PostForm.Set("model", replacement)
		replaceRequestBody(r, []byte(r.PostForm.Encode()))
	default:
		if err := r.ParseForm(); err != nil {
			return r, fmt.Errorf("could not parse form: %w", err)
		}
		r.PostForm.Set("model", replacement)
		replaceRequestBody(r, []byte(r.PostForm.Encode()))
	}

	return invalidateRequestContext(r), nil
}

func invalidateRequestContext(r *http.Request) *http.Request {
	if _, ok := ReadContext(r.Context()); !ok {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), ReqContextKey, struct{}{}))
}

func replaceRequestBody(r *http.Request, body []byte) {
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.Header.Del("Transfer-Encoding")
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))
	r.ContentLength = int64(len(body))
}

func replaceMultipartModel(form *multipart.Form, replacement string) ([]byte, string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	for key, values := range form.Value {
		for _, value := range values {
			if key == "model" {
				value = replacement
			}
			field, err := mw.CreateFormField(key)
			if err != nil {
				return nil, "", fmt.Errorf("error recreating form field %s: %w", key, err)
			}
			if _, err := field.Write([]byte(value)); err != nil {
				return nil, "", fmt.Errorf("error writing form field %s: %w", key, err)
			}
		}
	}

	for key, headers := range form.File {
		for _, fh := range headers {
			part, err := mw.CreateFormFile(key, fh.Filename)
			if err != nil {
				return nil, "", fmt.Errorf("error recreating form file %s: %w", key, err)
			}
			file, err := fh.Open()
			if err != nil {
				return nil, "", fmt.Errorf("error opening uploaded file %s: %w", key, err)
			}
			if _, err := io.Copy(part, file); err != nil {
				file.Close()
				return nil, "", fmt.Errorf("error copying file data %s: %w", key, err)
			}
			file.Close()
		}
	}

	if err := mw.Close(); err != nil {
		return nil, "", fmt.Errorf("error finalizing multipart form: %w", err)
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}

// extractUpstreamContext resolves the model from an /upstream/<model>/... path.
func extractUpstreamContext(r *http.Request, cfg config.Config) (ReqContextData, bool) {
	searchName, realName, _, found := FindModelInPath(cfg, strings.TrimPrefix(r.URL.Path, "/upstream"))
	if !found {
		return ReqContextData{}, false
	}
	return ReqContextData{
		Model:            searchName,
		ModelID:          realName,
		ApiKey:           ExtractAPIKey(r),
		Streaming:        r.URL.Query().Get("stream") == "true",
		SendLoadingState: sendLoadingState(cfg, realName),
		Metadata:         make(map[string]string),
		Tier:             TierFromContext(r.Context()),
	}, true
}

// sendLoadingState reports whether the configured model wants loading-state SSEs.
func sendLoadingState(cfg config.Config, modelID string) bool {
	if mc, ok := cfg.Models[modelID]; ok {
		return mc.SendLoadingState != nil && *mc.SendLoadingState
	}
	return false
}

// FindModelInPath walks a slash-separated path, building up segments until one
// matches a configured model. This resolves model names that contain slashes
// (e.g. "author/model"). Returns the matched name, its real model ID, the
// remaining path, and whether a match was found.
func FindModelInPath(cfg config.Config, path string) (searchName, realName, remainingPath string, found bool) {
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

		if modelID, ok := cfg.ResolveBaseModel(name); ok {
			searchName = name
			realName = modelID
			remainingPath = "/" + strings.Join(parts[i+1:], "/")
			found = true
		}
	}

	return
}

// EscapedPathSuffix removes decodedPrefix from escapedPath while leaving the
// remaining percent-encoding untouched. Decoded paths cannot identify the
// boundary alone: a model name such as "author/model" may have arrived as the
// single escaped segment "author%2Fmodel".
func EscapedPathSuffix(escapedPath, decodedPrefix string) string {
	rawIndex, prefixIndex := 0, 0
	for rawIndex < len(escapedPath) && prefixIndex < len(decodedPrefix) {
		end := rawIndex + 1
		if escapedPath[rawIndex] == '%' {
			end = rawIndex + 3
		} else {
			_, size := utf8.DecodeRuneInString(escapedPath[rawIndex:])
			end = rawIndex + size
		}
		if end > len(escapedPath) {
			return ""
		}

		decoded, err := url.PathUnescape(escapedPath[rawIndex:end])
		if err != nil || !strings.HasPrefix(decodedPrefix[prefixIndex:], decoded) {
			return ""
		}
		rawIndex = end
		prefixIndex += len(decoded)
	}
	if prefixIndex != len(decodedPrefix) {
		return ""
	}
	return escapedPath[rawIndex:]
}

func SetContext(ctx context.Context, data ReqContextData) context.Context {
	return context.WithValue(ctx, ReqContextKey, data)
}

func ReadContext(ctx context.Context) (ReqContextData, bool) {
	data, ok := ctx.Value(ReqContextKey).(ReqContextData)
	return data, ok
}

// SetReqData attaches a key/value pair to the request context's metadata map.
// The metadata map must already exist in the context's ReqContextData; callers
// should ensure FetchContext has run or initialize the map themselves.
// It returns an error for nil contexts or contexts without request data.
func SetReqData(ctx context.Context, key, value string) error {
	if ctx == nil {
		return fmt.Errorf("cannot set request metadata on nil context")
	}
	data, ok := ReadContext(ctx)
	if !ok {
		return fmt.Errorf("no request context data found")
	}
	if data.Metadata == nil {
		return fmt.Errorf("no metadata map in request context")
	}
	data.Metadata[key] = value
	return nil
}

// sessionUserIDPattern matches the session_<uuid> segment inside Claude
// Code's metadata.user_id field (e.g.
// "user_abc123_account_def456_session_<uuid>"). Matching only that segment,
// rather than the whole field, keeps extraction working even if the
// surrounding segments' shape drifts.
var sessionUserIDPattern = regexp.MustCompile(`session_([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})`)

// sessionIDFromUserID extracts the session uuid from a Claude Code
// metadata.user_id value, returning "" when userID contains no
// session_<uuid> segment (including when userID itself is empty).
func sessionIDFromUserID(userID string) string {
	m := sessionUserIDPattern.FindStringSubmatch(userID)
	if m == nil {
		return ""
	}
	return m[1]
}

// claudeCodeSessionHeader is the request header current Claude Code CLI
// builds (live-verified 2026-09-02 against claude-cli/2.1.252) use to carry
// their session identity as a bare uuid - the channel this fork's original
// metadata.user_id extraction missed entirely, which is why in-flight
// entries surfaced a nil Metadata map for real traffic.
const claudeCodeSessionHeader = "X-Claude-Code-Session-Id"

// bareSessionUUIDPattern validates claudeCodeSessionHeader's value before
// trusting it as a session id, so a malformed or hostile header value is
// dropped rather than propagated into Metadata/logs/UI as-is.
var bareSessionUUIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// sessionIDFromHeader returns the session uuid carried by
// claudeCodeSessionHeader, or "" when the header is absent or is not a bare
// uuid.
func sessionIDFromHeader(r *http.Request) string {
	v := strings.TrimSpace(r.Header.Get(claudeCodeSessionHeader))
	if !bareSessionUUIDPattern.MatchString(v) {
		return ""
	}
	return v
}

// claudeCodeParentSessionHeader carries the id of the session that DISPATCHED
// a headless run (llama-cm's llama/dispatch and
// llama/providers/llamacpp/agentic.sh set it). Without it a dispatched child
// is just another anonymous request on the queue; with it a renderer can say
// which interactive session is waiting on that row.
const claudeCodeParentSessionHeader = "X-Claude-Code-Parent-Session-Id"

// parentSessionIDPattern validates claudeCodeParentSessionHeader's value.
// Deliberately looser than bareSessionUUIDPattern: a dispatcher may only have
// the SHORT form of its parent's id to hand (the 8-hex prefix every renderer
// displays), so a hex run of at least 8 characters is accepted alongside a
// full uuid. Anything else is dropped rather than propagated into
// Metadata/logs/UI as-is.
var parentSessionIDPattern = regexp.MustCompile(`^([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}|[0-9a-fA-F]{8,})$`)

// parentSessionIDFromHeader returns the dispatching session's id carried by
// claudeCodeParentSessionHeader, or "" when the header is absent or malformed.
func parentSessionIDFromHeader(r *http.Request) string {
	v := strings.TrimSpace(r.Header.Get(claudeCodeParentSessionHeader))
	if !parentSessionIDPattern.MatchString(v) {
		return ""
	}
	return v
}

// claudeCodeAgentHeader carries the id of the Agent-tool SUBAGENT making a
// request. Claude Code sets it on every request a subagent issues while
// keeping claudeCodeSessionHeader at the PARENT's id (live-verified 2026-09-08
// against claude-cli/2.1.263: session 934b47c6, agent a4c4e94e633cf6841). It
// is the only wire-level signal that separates a subagent's turn from its
// parent's - without it a renderer prints the parent's id on both, and slot
// affinity keys both onto one lane.
const claudeCodeAgentHeader = "X-Claude-Code-Agent-Id"

// agentIDPattern validates claudeCodeAgentHeader's value: live ids are a
// 17-hex run, and the 8-hex prefix a renderer displays is accepted too (same
// shape rule as parentSessionIDPattern's short form). Anything else is dropped
// rather than propagated into Metadata/logs/UI as-is.
var agentIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8,}$`)

// agentIDFromHeader returns the subagent id carried by claudeCodeAgentHeader,
// or "" when the header is absent or malformed.
func agentIDFromHeader(r *http.Request) string {
	v := strings.TrimSpace(r.Header.Get(claudeCodeAgentHeader))
	if !agentIDPattern.MatchString(v) {
		return ""
	}
	return v
}

// Client families reported in the `client` metadata key. The set is closed so
// a renderer can switch on it instead of displaying a raw user-agent string,
// which is what a session-less queue row used to surface.
const (
	clientClaudeCode = "claude-code"
	clientHermes     = "hermes"
	clientCurl       = "curl"
	clientPythonSDK  = "python-sdk"
	clientOther      = "other"
)

// clientFamily classifies who is behind a request. A Claude Code session
// header is decisive - the CLI sets it and no other client sends it - so
// hasSession short-circuits the User-Agent sniffing, which is only a
// best-effort family guess for everything else.
func clientFamily(r *http.Request, hasSession bool) string {
	if hasSession {
		return clientClaudeCode
	}
	ua := strings.TrimSpace(r.Header.Get("User-Agent"))
	switch {
	case strings.HasPrefix(ua, "claude-cli"):
		return clientClaudeCode
	case strings.Contains(ua, "Anthropic/Python"):
		return clientPythonSDK
	case strings.HasPrefix(ua, "curl/"):
		return clientCurl
	case strings.Contains(strings.ToLower(ua), "hermes"):
		return clientHermes
	default:
		return clientOther
	}
}

// sessionMetadata builds the session-identity metadata bag for a request: the
// session_id/client_user_id pair, the dispatching parent_session_id, and the
// client family. It applies to every request shape (GET query, JSON body,
// form body) since the header checks need no body at all. The
// claudeCodeSessionHeader takes priority as the current, authoritative
// channel; the metadata.user_id session_<uuid> segment (userID, "" when the
// caller has no JSON body to inspect) is kept as a fallback for whichever
// older CLI builds still rely on it instead.
func sessionMetadata(r *http.Request, userID string) map[string]string {
	metadata := make(map[string]string)
	sessionID := sessionIDFromHeader(r)
	fromHeader := sessionID != ""
	if fromHeader {
		metadata["session_id"] = sessionID
		if userID != "" {
			metadata["client_user_id"] = userID
		}
	} else if sessionID = sessionIDFromUserID(userID); sessionID != "" {
		metadata["session_id"] = sessionID
		metadata["client_user_id"] = userID
	}
	if parentID := parentSessionIDFromHeader(r); parentID != "" {
		metadata["parent_session_id"] = parentID
	}
	if agentID := agentIDFromHeader(r); agentID != "" {
		metadata["agent_id"] = agentID
	}
	metadata["client"] = clientFamily(r, fromHeader)
	return metadata
}

// ShortestAlias returns the shortest alias configured for modelID, falling
// back to modelID itself when the model has no alias (or is not in the config
// at all). The shortest alias is the name an operator types and reads
// (`cq35`), where the model id is a repo/quant path no queue row has room
// for. Ties break lexicographically so the answer is stable across reloads:
// config.ModelConfig.Aliases keeps YAML order, which says nothing about
// which of two equal-length aliases should win.
func ShortestAlias(cfg config.Config, modelID string) string {
	mc, ok := cfg.Models[modelID]
	if !ok {
		return modelID
	}
	best := ""
	for _, alias := range mc.Aliases {
		if alias == "" {
			continue
		}
		if best == "" || len(alias) < len(best) || (len(alias) == len(best) && alias < best) {
			best = alias
		}
	}
	if best == "" {
		return modelID
	}
	return best
}

// extractContext pulls fields from an HTTP request into a ReqContextData,
// returning whatever is available. For GET requests it reads query parameters.
// For POST requests it inspects Content-Type and parses JSON,
// multipart/form-data, or application/x-www-form-urlencoded bodies. The
// request body is always restored before returning. An error is returned only
// for I/O or parse failures, not for missing fields.
func extractContext(r *http.Request) (ReqContextData, error) {

	apiKey := ExtractAPIKey(r)

	if r.Method == http.MethodGet {
		q := r.URL.Query()
		return ReqContextData{
			Model:     q.Get("model"),
			Streaming: q.Get("stream") == "true",
			ApiKey:    apiKey,
			Metadata:  sessionMetadata(r, ""),
			Tier:      TierFromContext(r.Context()),
		}, nil
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		return ReqContextData{}, fmt.Errorf("error reading request body: %w", err)
	}
	defer func() {
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	}()

	contentType := r.Header.Get("Content-Type")

	if strings.Contains(contentType, "application/json") {
		// Claude Code carries its session identity via the
		// claudeCodeSessionHeader request header (current CLI builds) or,
		// as a fallback, metadata.user_id in the JSON body (e.g.
		// "user_..._account_..._session_<uuid>"); see sessionMetadata.
		userID := gjson.GetBytes(bodyBytes, "metadata.user_id").String()
		return ReqContextData{
			Model:     gjson.GetBytes(bodyBytes, "model").String(),
			Streaming: gjson.GetBytes(bodyBytes, "stream").Bool(),
			ApiKey:    apiKey,
			Metadata:  sessionMetadata(r, userID),
			Tier:      TierFromContext(r.Context()),
			Body:      bodyBytes,
		}, nil
	}

	// Form parsers read from r.Body, so feed them a fresh reader over the
	// buffered bytes. The deferred restore above will reset r.Body again
	// after parsing.
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	if strings.Contains(contentType, "multipart/form-data") {
		if err := r.ParseMultipartForm(MaxMultiPartSize); err != nil {
			return ReqContextData{}, fmt.Errorf("error parsing multipart form: %w", err)
		}
	} else {
		if err := r.ParseForm(); err != nil {
			return ReqContextData{}, fmt.Errorf("error parsing form: %w", err)
		}
	}

	return ReqContextData{
		Model:     r.FormValue("model"),
		Streaming: r.FormValue("stream") == "true",
		ApiKey:    apiKey,
		Metadata:  sessionMetadata(r, ""),
		Tier:      TierFromContext(r.Context()),
		Body:      bodyBytes,
	}, nil
}

// extractAPIKey pulls a candidate API key from the request, preferring Basic,
// then Bearer, then x-api-key.
func ExtractAPIKey(r *http.Request) string {
	var bearerKey, basicKey string
	if auth := r.Header.Get("Authorization"); auth != "" {
		scheme, credentials, ok := strings.Cut(auth, " ")
		if ok {
			switch strings.ToLower(scheme) {
			case "bearer":
				bearerKey = credentials
			case "basic":
				if decoded, err := base64.StdEncoding.DecodeString(credentials); err == nil {
					if parts := strings.SplitN(string(decoded), ":", 2); len(parts) == 2 {
						basicKey = parts[1] // password field is the API key
					}
				}
			}
		}
	}

	switch {
	case basicKey != "":
		return basicKey
	case bearerKey != "":
		return bearerKey
	default:
		return r.Header.Get("x-api-key")
	}
}
