package server

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/cache"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"github.com/tidwall/gjson"
)

type TokenMetrics = store.TokenMetrics
type ActivityLogEntry = store.ActivityLogEntry

// activityTierKey is the request-metadata key carrying the entry-point tier
// name (swaputil.TierFromContext), e.g. "default" or a configured tier's key.
// Upstream moved ActivityLogEntry into internal/store, which has no Tier
// column, so our per-tier attribution rides on upstream's request-metadata bag
// instead of a dedicated field. Always populated - an untagged request
// resolves to swaputil.DefaultTier.Name ("default").
const activityTierKey = "tier"

// ActivityLogEvent carries a single activity log entry to event subscribers.
type ActivityLogEvent struct {
	Metrics ActivityLogEntry
}

func (e ActivityLogEvent) Type() uint32 {
	return swaputil.ActivityLogEventID
}

// metricsMonitor parses upstream responses for token statistics, stores
// activity in a store, and (when captures are enabled) stores
// zstd+CBOR-compressed request/response captures in a sized in-memory cache.
type metricsMonitor struct {
	store          *store.Store
	maxMetrics     int
	logger         *logmon.Monitor
	enableCaptures bool
	captureCache   *cache.Cache // zstd-compressed CBOR of ReqRespCapture
}

func newMetricsMonitor(logger *logmon.Monitor, maxMetrics int, captureBufferMB int, st *store.Store) *metricsMonitor {
	if maxMetrics <= 0 {
		maxMetrics = 1000
	}
	mm := &metricsMonitor{
		logger:         logger,
		store:          st,
		maxMetrics:     maxMetrics,
		enableCaptures: captureBufferMB > 0,
	}
	if captureBufferMB > 0 {
		mm.captureCache = cache.New(captureBufferMB * 1024 * 1024)
	}
	return mm
}

// queueMetrics persists a metric and returns the store-assigned row. It
// deliberately does not take the request context: record runs after the
// handler returns, and an aborted request (canceled context) must still be
// recorded — that is exactly when the error entry matters.
func (mp *metricsMonitor) queueMetrics(metric ActivityLogEntry) (ActivityLogEntry, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stored, err := mp.store.InsertActivity(ctx, metric)
	if err != nil {
		mp.warnf("failed to persist activity metric: %v", err)
		return ActivityLogEntry{}, false
	}
	if mp.store.IsInMemory() {
		if err := mp.store.PruneActivity(ctx, mp.maxMetrics); err != nil {
			mp.warnf("failed to prune activity metrics: %v", err)
		}
	}
	return stored, true
}

// emitMetric publishes an ActivityLogEvent for the given metric.
func (mp *metricsMonitor) emitMetric(metric ActivityLogEntry) {
	event.Emit(ActivityLogEvent{Metrics: metric})
}

func (mp *metricsMonitor) overlayCaptureState(entries []ActivityLogEntry) {
	if mp.captureCache == nil {
		for i := range entries {
			entries[i].HasCapture = false
		}
		return
	}
	for i := range entries {
		entries[i].HasCapture = mp.captureCache.Has(entries[i].ID)
	}
}

func (mp *metricsMonitor) warnf(format string, args ...any) {
	if mp.logger != nil {
		mp.logger.Warnf(format, args...)
	}
}

func (mp *metricsMonitor) Close() error {
	return nil
}

// record parses a completed response body and stores/emits an activity entry.
// Successful requests store a zstd+CBOR capture (when enabled) with cf
// controlling which parts are retained. Failed (non-200) requests capture the
// request only and set ErrorMsg to a description of the failure, so the error
// can be inspected without storing unreadable raw response bytes.
// reqCapture is a zstd+CBOR-compressed ReqRespCapture (request fields only,
// see CreateMetricsMiddleware) produced before dispatch; storeCapture
// decompresses it and merges in the response fields before the final store.
func (mp *metricsMonitor) record(modelID string, r *http.Request, recorder *responseBodyCopier, cf captureFields, reqCapture []byte) {
	tm := ActivityLogEntry{
		Timestamp:       time.Now(),
		Model:           modelID,
		ReqPath:         r.URL.Path,
		RespContentType: recorder.Header().Get("Content-Type"),
		RespStatusCode:  recorder.Status(),
		DurationMs:      int(time.Since(recorder.StartTime()).Milliseconds()),
	}

	if ctxData, ok := swaputil.ReadContext(r.Context()); ok && len(ctxData.Metadata) > 0 {
		tm.Metadata = make(map[string]string, len(ctxData.Metadata))
		for k, v := range ctxData.Metadata {
			tm.Metadata[k] = v
		}
	}
	// Tier attribution (docs/intent/llama-swap-tiers.md). TierFromContext
	// falls back to swaputil.DefaultTier for any request that never passed
	// through tier-wrapping; only a NON-default tier is stamped, so a config
	// with no `tiers:` block keeps byte-identical activity rows - the same
	// "single listener sees no tier surface" rule the ByTier breakdown follows.
	if tier := swaputil.TierFromContext(r.Context()).Name; tier != swaputil.DefaultTier.Name {
		if tm.Metadata == nil {
			tm.Metadata = make(map[string]string, 1)
		}
		tm.Metadata[activityTierKey] = tier
	}
	if selectorID := selectorFromContext(r.Context()); selectorID != "" {
		if tm.Metadata == nil {
			tm.Metadata = make(map[string]string)
		}
		tm.Metadata["selector"] = selectorID
	}

	queueAndEmit := func() {
		stored, ok := mp.queueMetrics(tm)
		if !ok {
			return
		}
		tm = stored
		mp.emitMetric(tm)
	}

	if recorder.Status() != http.StatusOK {
		mp.logger.Warnf("non-200 response, recording partial metrics: status=%d, path=%s", recorder.Status(), r.URL.Path)
		decoded, decErr := mp.decodeResponseBody(recorder, r.URL.Path)
		tm.ErrorMsg = failedErrorMessage(recorder.Status(), decoded, decErr)
		stored, ok := mp.queueMetrics(tm)
		if !ok {
			return
		}
		tm = stored
		// Capture the request only; the failure is surfaced via ErrorMsg
		// rather than storing the (possibly undisplayable) response body.
		tm.HasCapture = mp.storeCapture(tm.ID, r, recorder, cf&^captureRespBody, reqCapture, nil)
		mp.emitMetric(tm)
		return
	}

	body := recorder.body.Bytes()
	if len(body) == 0 {
		mp.logger.Warn("metrics: empty body, recording minimal metrics")
		queueAndEmit()
		return
	}

	if encoding := recorder.Header().Get("Content-Encoding"); encoding != "" {
		decoded, err := decompressBody(body, encoding)
		if err != nil {
			mp.logger.Warnf("metrics: decompression failed: %v, path=%s, recording minimal metrics", err, r.URL.Path)
			tm.ErrorMsg = fmt.Sprintf("response decompression failed: %v", err)
			queueAndEmit()
			return
		}
		body = decoded
	}

	if strings.Contains(recorder.Header().Get("Content-Type"), "text/event-stream") {
		if parsed, err := processStreamingResponse(modelID, recorder.StartTime(), body); err != nil {
			mp.logger.Warnf("error processing streaming response: %v, path=%s, recording minimal metrics", err, r.URL.Path)
		} else {
			tm.Tokens = parsed.Tokens
			tm.DurationMs = parsed.DurationMs
		}
	} else if gjson.ValidBytes(body) {
		parsed := gjson.ParseBytes(body)
		usage := parsed.Get("usage")
		timings := parsed.Get("timings")
		responseMetrics := parsed.Get("metrics")

		// /infill responses are arrays; timings live in the last element (#463).
		if strings.HasPrefix(r.URL.Path, "/infill") {
			if arr := parsed.Array(); len(arr) > 0 {
				timings = arr[len(arr)-1].Get("timings")
			}
		}

		if usage.Exists() || timings.Exists() || responseMetrics.Exists() {
			if parsedMetrics, err := parseMetrics(modelID, recorder.StartTime(), usage, timings, responseMetrics); err != nil {
				mp.logger.Warnf("error parsing metrics: %v, path=%s, recording minimal metrics", err, r.URL.Path)
			} else {
				tm.Tokens = parsedMetrics.Tokens
				tm.DurationMs = parsedMetrics.DurationMs
			}
		}
	} else {
		mp.logger.Warnf("metrics: invalid JSON in response body path=%s, recording minimal metrics", r.URL.Path)
	}

	stored, ok := mp.queueMetrics(tm)
	if !ok {
		return
	}
	tm = stored
	tm.HasCapture = mp.storeCapture(tm.ID, r, recorder, cf, reqCapture, body)
	mp.emitMetric(tm)
}

// storeCapture assembles a ReqRespCapture for id, honoring the captureFields
// mask, and stores it when captures are enabled. body is the response body to
// capture (already decompressed by the caller); pass nil to omit it. Returns
// true if a capture was stored.
//
// reqCapture is the pre-compressed request-side capture handed over by
// CreateMetricsMiddleware (fork commit 1f9ac8f): the raw request body is
// compressed once at dispatch time instead of being pinned in memory for the
// whole - potentially hours long - streamed response, so only the small blob
// survives until here, where it is decompressed and merged with the response
// fields. The captureReqBody/captureReqHeaders mask was already applied when
// the blob was built.
func (mp *metricsMonitor) storeCapture(id int, r *http.Request, recorder *responseBodyCopier, cf captureFields, reqCapture []byte, body []byte) bool {
	if !mp.enableCaptures {
		return false
	}
	capture := ReqRespCapture{
		ID:      id,
		ReqPath: r.URL.Path,
	}
	if len(reqCapture) > 0 {
		if partial, err := decompressCapture(reqCapture); err == nil {
			capture.ReqHeaders = partial.ReqHeaders
			capture.ReqBody = partial.ReqBody
		} else {
			mp.logger.Warnf("failed to decompress request capture: %v, path=%s", err, r.URL.Path)
		}
	}
	if cf&captureRespHeaders != 0 {
		capture.RespHeaders = headerMap(recorder.Header())
		redactHeaders(capture.RespHeaders)
		delete(capture.RespHeaders, "Content-Encoding")
	}
	if cf&captureRespBody != 0 {
		capture.RespBody = body
	}
	return mp.addCapture(capture)
}

// decodeResponseBody returns the buffered response body, decompressing it when
// the upstream set a Content-Encoding we recognize. On decompression failure it
// logs a warning and returns an error so the caller can record a description
// (via ErrorMsg) instead of storing unreadable raw bytes.
func (mp *metricsMonitor) decodeResponseBody(recorder *responseBodyCopier, path string) ([]byte, error) {
	body := recorder.body.Bytes()
	if len(body) == 0 {
		return nil, nil
	}
	encoding := recorder.Header().Get("Content-Encoding")
	if encoding == "" {
		return body, nil
	}
	decoded, err := decompressBody(body, encoding)
	if err != nil {
		mp.logger.Warnf("metrics: response decompression failed: %v, path=%s", err, path)
		return nil, err
	}
	return decoded, nil
}

// errorMessagePaths lists JSON paths where a human-readable error message can
// live across OpenAI- and llama.cpp-style error responses.
var errorMessagePaths = []string{"error.message", "error", "message", "detail"}

// extractErrorMessage pulls a human-readable error string from a JSON error
// response. Returns "" if no message is found or the body is not valid JSON.
func extractErrorMessage(body []byte) string {
	if !gjson.ValidBytes(body) {
		return ""
	}
	parsed := gjson.ParseBytes(body)
	for _, path := range errorMessagePaths {
		v := parsed.Get(path)
		if v.Exists() && v.Type == gjson.String {
			if s := strings.TrimSpace(v.String()); s != "" {
				return s
			}
		}
	}
	return ""
}

// failedErrorMessage builds a human-readable description for a non-200 response.
// It prefers an error message parsed from the (decompressed) body and falls back
// to the HTTP status text. A non-nil decErr indicates the body could not be
// decoded, in which case the decode error is described instead.
func failedErrorMessage(status int, body []byte, decErr error) string {
	const maxLen = 500
	if decErr != nil {
		return fmt.Sprintf("response decode failed: %v", decErr)
	}
	if msg := extractErrorMessage(body); msg != "" {
		if len(msg) > maxLen {
			msg = msg[:maxLen] + "..."
		}
		return msg
	}
	if text := http.StatusText(status); text != "" {
		return fmt.Sprintf("%d %s", status, text)
	}
	return fmt.Sprintf("HTTP %d", status)
}

// usagePaths lists the JSON paths where a per-event usage object can live.
var usagePaths = []string{"usage", "response.usage", "message.usage"}

// extractUsageTokens reads input/output/cached token counts from a usage
// gjson.Result, handling the field-name differences across endpoints.
func extractUsageTokens(usage gjson.Result) (input, output, cached int64, ok bool) {
	cached = -1
	if !usage.Exists() {
		return
	}

	if v := usage.Get("prompt_tokens"); v.Exists() {
		input = v.Int()
		ok = true
	} else if v := usage.Get("input_tokens"); v.Exists() {
		input = v.Int()
		ok = true
	}

	if v := usage.Get("completion_tokens"); v.Exists() {
		output = v.Int()
		ok = true
	} else if v := usage.Get("output_tokens"); v.Exists() {
		output = v.Int()
		ok = true
	}

	if v := usage.Get("cache_read_input_tokens"); v.Exists() {
		cached = v.Int()
		ok = true
	} else if v := usage.Get("input_tokens_details.cached_tokens"); v.Exists() {
		cached = v.Int()
		ok = true
	} else if v := usage.Get("prompt_tokens_details.cached_tokens"); v.Exists() {
		cached = v.Int()
		ok = true
	}
	return
}

func processStreamingResponse(modelID string, start time.Time, body []byte) (ActivityLogEntry, error) {
	var (
		inputTokens, outputTokens int64
		cachedTokens              int64 = -1
		hasAny                    bool
		timings                   gjson.Result
		responseMetrics           gjson.Result
	)

	prefix := []byte("data:")
	for offset := 0; offset < len(body); {
		nl := bytes.IndexByte(body[offset:], '\n')
		var line []byte
		if nl == -1 {
			line = body[offset:]
			offset = len(body)
		} else {
			line = body[offset : offset+nl]
			offset += nl + 1
		}

		line = bytes.TrimSpace(line)
		if len(line) == 0 || !bytes.HasPrefix(line, prefix) {
			continue
		}
		data := bytes.TrimSpace(line[len(prefix):])
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		if !gjson.ValidBytes(data) {
			continue
		}
		parsed := gjson.ParseBytes(data)

		for _, path := range usagePaths {
			u := parsed.Get(path)
			if !u.Exists() {
				continue
			}
			i, o, c, ok := extractUsageTokens(u)
			if !ok {
				continue
			}
			hasAny = true
			if i > 0 {
				inputTokens = i
			}
			if o > 0 {
				outputTokens = o
			}
			if c >= 0 {
				cachedTokens = c
			}
		}
		if t := parsed.Get("timings"); t.Exists() {
			timings = t
			hasAny = true
		}
		if m := parsed.Get("metrics"); m.Exists() {
			responseMetrics = m
			hasAny = true
		}
	}

	if !hasAny {
		return ActivityLogEntry{}, fmt.Errorf("no valid JSON data found in stream")
	}

	return buildMetrics(modelID, start, inputTokens, outputTokens, cachedTokens, timings, responseMetrics), nil
}

func parseMetrics(modelID string, start time.Time, usage, timings, responseMetrics gjson.Result) (ActivityLogEntry, error) {
	input, output, cached, _ := extractUsageTokens(usage)
	return buildMetrics(modelID, start, input, output, cached, timings, responseMetrics), nil
}

// buildMetrics composes an ActivityLogEntry from accumulated token counts and
// optional llama-server timings (which override input/output and provide rates)
// or vLLM response metrics.
func buildMetrics(modelID string, start time.Time, inputTokens, outputTokens, cachedTokens int64, timings, responseMetrics gjson.Result) ActivityLogEntry {
	wallDurationMs := int(time.Since(start).Milliseconds())
	durationMs := wallDurationMs
	tokensPerSecond := -1.0
	promptPerSecond := -1.0
	draftTokens := -1
	draftAccTokens := -1

	if timings.Exists() {
		inputTokens = timings.Get("prompt_n").Int()
		outputTokens = timings.Get("predicted_n").Int()
		promptPerSecond = timings.Get("prompt_per_second").Float()
		tokensPerSecond = timings.Get("predicted_per_second").Float()
		timingsDurationMs := int(timings.Get("prompt_ms").Float() + timings.Get("predicted_ms").Float())
		if timingsDurationMs > durationMs {
			durationMs = timingsDurationMs
		}
		if cachedValue := timings.Get("cache_n"); cachedValue.Exists() {
			cachedTokens = cachedValue.Int()
		}
		if timings.Get("draft_n").Exists() && timings.Get("draft_n_accepted").Exists() {
			draftTokens = int(timings.Get("draft_n").Int())
			draftAccTokens = int(timings.Get("draft_n_accepted").Int())
		}
	}
	if timeToFirstToken := responseMetrics.Get("time_to_first_token_ms"); timeToFirstToken.Exists() && timeToFirstToken.Float() > 0 {
		cachedForRate := cachedTokens
		if cachedForRate < 0 {
			cachedForRate = 0
		}
		promptPerSecond = float64(inputTokens-cachedForRate) / (timeToFirstToken.Float() / 1000)
	}
	if meanInterTokenLatency := responseMetrics.Get("mean_itl_ms"); meanInterTokenLatency.Exists() && meanInterTokenLatency.Float() > 0 {
		tokensPerSecond = 1000 / meanInterTokenLatency.Float()
	}

	return ActivityLogEntry{
		Timestamp: time.Now(),
		Model:     modelID,
		Tokens: TokenMetrics{
			CachedTokens:    int(cachedTokens),
			DraftTokens:     draftTokens,
			DraftAccTokens:  draftAccTokens,
			InputTokens:     int(inputTokens),
			OutputTokens:    int(outputTokens),
			PromptPerSecond: promptPerSecond,
			TokensPerSecond: tokensPerSecond,
		},
		DurationMs: durationMs,
	}
}

// decompressBody decompresses the body based on the Content-Encoding header.
func decompressBody(body []byte, encoding string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "gzip":
		reader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		return io.ReadAll(reader)
	case "deflate":
		reader := flate.NewReader(bytes.NewReader(body))
		defer reader.Close()
		return io.ReadAll(reader)
	default:
		return body, nil
	}
}

// filterAcceptEncoding filters Accept-Encoding to only gzip/deflate so response
// bodies remain decompressible for metrics parsing.
func filterAcceptEncoding(acceptEncoding string) string {
	if acceptEncoding == "" {
		return ""
	}

	supported := map[string]bool{"gzip": true, "deflate": true}
	var filtered []string
	for part := range strings.SplitSeq(acceptEncoding, ",") {
		encoding, _, _ := strings.Cut(strings.TrimSpace(part), ";")
		if supported[strings.ToLower(encoding)] {
			filtered = append(filtered, strings.TrimSpace(part))
		}
	}
	return strings.Join(filtered, ", ")
}

// responseTeeCapBytes bounds how much of a streamed SSE response
// responseBodyCopier keeps buffered for metrics parsing. Without a bound, a
// long-lived SSE stream (a request can sit parked in llama-swap's queue and
// then stream for a very long time) grows this buffer without limit, an
// OOM-scale footprint under concurrent long streams. processStreamingResponse
// only needs the LAST usage/timings-bearing SSE event (it keeps overwriting
// as it scans forward), so keeping just the most recent responseTeeCapBytes
// bytes preserves metrics correctness for that path; only the (unbounded)
// capture-storage fidelity for the response body on streams that exceed the
// cap is traded away - the stored capture body is truncated to the same tail
// window used for parsing.
//
// This cap applies ONLY to uncompressed text/event-stream responses (see
// selectBuffer below). mp.record's non-streaming branch does a single
// gjson.ParseBytes over the WHOLE body from offset 0 - capping that buffer
// would hand it a truncated mid-document fragment (invalid JSON) and silently
// zero out token metrics for any non-streaming response over the cap. Plain
// JSON responses are also bounded by normal sizes anyway; the memory problem
// this cap solves is specifically long-lived SSE streams. Compressed SSE
// (Content-Encoding set) is likewise left uncapped: decompressBody needs a
// complete gzip/deflate stream, so a truncated tail would corrupt it too.
const responseTeeCapBytes = 16 * 1024 * 1024 // 16MB

// cappedTailBuffer is an io.Writer that retains only the most recently written
// responseTeeCapBytes bytes, discarding older bytes from the front once the
// cap is reached. Most SSE responses are KB-to-few-MB, well under the cap, so
// the backing array is grown lazily via append (Go's normal geometric
// growth) rather than eagerly allocated at the full cap size - eagerly
// allocating 16MB per stream would itself be the large-transient-allocation
// pattern this cap exists to avoid, multiplied by every concurrent stream
// (golang/go#47656). Only once cumulative writes reach the cap does the
// buffer switch to fixed-size ring semantics, so a write after that point
// costs O(len(p)), not O(cap).
type cappedTailBuffer struct {
	buf     []byte // grows via append while !wrapped; fixed len == cap once wrapped
	cap     int
	pos     int  // next write position within buf (ring mode only; see Write)
	wrapped bool // true once buf has reached cap bytes at least once
}

func newCappedTailBuffer(capBytes int) *cappedTailBuffer {
	return &cappedTailBuffer{cap: capBytes}
}

func (c *cappedTailBuffer) Write(p []byte) (int, error) {
	total := len(p)
	if total == 0 || c.cap <= 0 {
		return total, nil
	}

	if !c.wrapped {
		room := c.cap - len(c.buf)
		if len(p) <= room {
			// Still below the cap: grow the backing array like a normal
			// append-based buffer, no ring bookkeeping needed yet.
			c.buf = append(c.buf, p...)
			c.pos = len(c.buf)
			if c.pos == c.cap {
				c.wrapped = true
				c.pos = 0
			}
			return total, nil
		}
		// This write crosses the cap: fill the remaining growing capacity
		// (buf becomes exactly cap bytes long, matching ring-mode's fixed
		// size invariant), then fall through to ring-mode for the rest of p.
		c.buf = append(c.buf, p[:room]...)
		p = p[room:]
		c.wrapped = true
		c.pos = 0
	}

	// Ring mode: c.buf is a fixed-size (cap-length) circular buffer.
	n := len(p)
	if n == 0 {
		return total, nil
	}
	if n >= c.cap {
		copy(c.buf, p[n-c.cap:])
		c.pos = 0
		return total, nil
	}
	end := c.pos + n
	if end <= c.cap {
		copy(c.buf[c.pos:end], p)
		c.pos = end % c.cap
	} else {
		first := c.cap - c.pos
		copy(c.buf[c.pos:], p[:first])
		copy(c.buf, p[first:])
		c.pos = n - first
	}
	return total, nil
}

// Bytes returns the retained bytes in chronological order (oldest first). If
// the cap was reached, this is the tail window; otherwise it is everything
// written so far.
func (c *cappedTailBuffer) Bytes() []byte {
	if c.buf == nil {
		return nil
	}
	if !c.wrapped {
		return c.buf[:c.pos]
	}
	out := make([]byte, c.cap)
	copy(out, c.buf[c.pos:])
	copy(out[c.cap-c.pos:], c.buf[:c.pos])
	return out
}

// Len returns the number of bytes retained (capped at c.cap).
func (c *cappedTailBuffer) Len() int {
	if !c.wrapped {
		return c.pos
	}
	return c.cap
}

// teeBuffer is the interface responseBodyCopier.body satisfies, whichever
// concrete buffer selectBuffer picks: an unbounded *bytes.Buffer for
// non-streaming/compressed responses, or a bounded *cappedTailBuffer for
// uncompressed SSE streams.
type teeBuffer interface {
	io.Writer
	Bytes() []byte
	Len() int
}

// responseBodyCopier tees the upstream response to the client while buffering
// it for metrics parsing. Status defaults to 200 until WriteHeader is called.
// The buffer implementation (bounded vs unbounded) is chosen lazily in
// selectBuffer, once response headers are available - see responseTeeCapBytes.
type responseBodyCopier struct {
	http.ResponseWriter
	body        teeBuffer
	tee         io.Writer
	status      int
	wroteHeader bool
	start       time.Time
}

func newBodyCopier(w http.ResponseWriter) *responseBodyCopier {
	return &responseBodyCopier{
		ResponseWriter: w,
		// Default to an unbounded buffer so recorder.body is always safe to
		// call Bytes()/Len() on, even for a handler that never writes a body
		// (WriteHeader/selectBuffer never runs). selectBuffer may still swap
		// this out for a capped tail buffer once headers are known - safe
		// because that only happens before any bytes are written to it.
		body:   &bytes.Buffer{},
		status: http.StatusOK,
		start:  time.Now(),
	}
}

// selectBuffer picks the buffer implementation once response headers are
// final (called from WriteHeader, before it forwards to the underlying
// writer). Only uncompressed text/event-stream responses get the bounded
// tail buffer; everything else (including compressed SSE, which
// decompressBody needs whole) gets an unbounded buffer, matching pre-cap
// behavior for those cases.
func (w *responseBodyCopier) selectBuffer() {
	h := w.Header()
	streaming := strings.Contains(h.Get("Content-Type"), "text/event-stream") && h.Get("Content-Encoding") == ""
	if streaming {
		w.body = newCappedTailBuffer(responseTeeCapBytes)
	} else {
		w.body = &bytes.Buffer{}
	}
	w.tee = io.MultiWriter(w.ResponseWriter, w.body)
}

func (w *responseBodyCopier) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	// On a protocol upgrade (e.g. websocket) the body is raw framed data, not a
	// metrics-parseable response, so write straight to the client without
	// buffering a copy we can't use.
	if w.status == http.StatusSwitchingProtocols {
		return w.ResponseWriter.Write(b)
	}
	return w.tee.Write(b)
}

func (w *responseBodyCopier) WriteHeader(statusCode int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = statusCode
	w.selectBuffer()
	w.ResponseWriter.WriteHeader(statusCode)
}

// Flush forwards to the underlying writer so streaming responses still flush.
func (w *responseBodyCopier) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards to the underlying writer so httputil.ReverseProxy can take
// over the connection for websocket upgrades.
func (w *responseBodyCopier) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, fmt.Errorf("underlying ResponseWriter does not support hijacking")
}

func (w *responseBodyCopier) Status() int          { return w.status }
func (w *responseBodyCopier) StartTime() time.Time { return w.start }
