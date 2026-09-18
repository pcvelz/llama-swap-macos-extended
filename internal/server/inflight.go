package server

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

const inflightUpdateInterval = 250 * time.Millisecond
const inflightOutboxSize = 128

const (
	inflightOperationSnapshot = "snapshot"
	inflightOperationUpsert   = "upsert"
	inflightOperationRemove   = "remove"
)

type inflightStartContextKey struct{}

func markInflightStart(r *http.Request) *http.Request {
	if _, ok := r.Context().Value(inflightStartContextKey{}).(time.Time); ok {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), inflightStartContextKey{}, time.Now()))
}

func inflightStart(r *http.Request) time.Time {
	if started, ok := r.Context().Value(inflightStartContextKey{}).(time.Time); ok {
		return started
	}
	return time.Now()
}

// inflightTracker tracks in-flight model-dispatched requests and their
// cancellable contexts.
type inflightTracker struct {
	nextID atomic.Uint64

	// mu serializes state mutations and their corresponding outbox writes so
	// request updates keep the same order in which they were applied.
	mu       sync.RWMutex
	requests map[string]*inflightRequest

	// tierNames are the configured tier names (excluding the implicit
	// "default"), seeded at construction so the ByTier breakdown reflects
	// CONFIGURATION (">1 tier configured") rather than only tiers that have
	// happened to see traffic yet. See docs/intent/llama-swap-tiers.md.
	tierNames []string

	updates          chan swaputil.InFlightRequestsEvent
	needsSnapshot    atomic.Bool
	publisherRunning atomic.Bool
	publish          func(swaputil.InFlightRequestsEvent)
}

type inflightRequest struct {
	entry       swaputil.InflightRequestEntry
	cancel      context.CancelFunc
	lastEmitted time.Time
	timer       *time.Timer
	// tier is the entry-point tier this request arrived through, captured at
	// Add() time so the per-tier breakdown survives context rewrites.
	tier string
	// respCarry holds the trailing partial SSE line left over from the
	// previous AddResponseBytes chunk. Response bytes arrive in arbitrary
	// chunks NOT aligned to SSE event boundaries, so a `content_block_delta`
	// event line can straddle two writes; carrying the unterminated tail lets
	// the next chunk complete and count it. Bounded by respCarryMax so a
	// pathological unterminated line can never buffer the whole stream.
	respCarry []byte
}

// newInflightTracker builds a tracker. tierNames are the configured tier names
// (variadic so the many single-tier test call sites stay unchanged); the
// implicit "default" tier is always seeded.
func newInflightTracker(tierNames ...string) *inflightTracker {
	return newInflightTrackerWithPublisher(inflightOutboxSize, func(update swaputil.InFlightRequestsEvent) {
		event.Emit(update)
	}, tierNames...)
}

func newInflightTrackerWithPublisher(size int, publish func(swaputil.InFlightRequestsEvent), tierNames ...string) *inflightTracker {
	t := &inflightTracker{
		requests:  make(map[string]*inflightRequest),
		updates:   make(chan swaputil.InFlightRequestsEvent, size),
		publish:   publish,
		tierNames: tierNames,
	}
	return t
}

// tierSnapshotLocked returns the per-tier in-flight counts: "default" plus
// every configured tier seeded at zero, then the live requests counted by the
// tier they arrived on. Callers must hold t.mu (read or write).
func (t *inflightTracker) tierSnapshotLocked() map[string]int {
	counts := make(map[string]int, len(t.tierNames)+1)
	counts[swaputil.DefaultTier.Name] = 0
	for _, name := range t.tierNames {
		counts[name] = 0
	}
	for _, req := range t.requests {
		tier := req.tier
		if tier == "" {
			tier = swaputil.DefaultTier.Name
		}
		counts[tier]++
	}
	return counts
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

// withCountsLocked stamps the aggregate Total (and, when tiers are configured,
// the ByTier breakdown) onto an outgoing event. Our menu-bar/tray helpers read
// those two fields from the "inflight" SSE payload; upstream's UI reads the
// Operation/Requests fields on the same event. Callers must hold t.mu.
func (t *inflightTracker) withCountsLocked(update swaputil.InFlightRequestsEvent) swaputil.InFlightRequestsEvent {
	update.Total = len(t.requests)
	update.ByTier = byTierOrNil(t.tierSnapshotLocked())
	return update
}

func (t *inflightTracker) Add(r *http.Request, cancel context.CancelFunc) string {
	id := strconv.FormatUint(t.nextID.Add(1), 10)
	entry := swaputil.InflightRequestEntry{
		ID:          id,
		Timestamp:   inflightStart(r),
		ReqPath:     r.URL.Path,
		Method:      r.Method,
		ReqHeaders:  headerMap(r.Header),
		RemoteIP:    clientIP(r),
		RespHeaders: map[string]string{},
	}
	redactHeaders(entry.ReqHeaders)
	data, hasCtx := swaputil.ReadContext(r.Context())
	if hasCtx {
		entry.Model = data.ModelID
		entry.Metadata = copyMetadata(data.Metadata)
	}
	// Stamp the same tier tierSnapshotLocked's ByTier breakdown counts by
	// (req.tier below) onto the live entry's own Metadata, so a subscriber
	// reading entry.Metadata directly (rather than cross-referencing
	// ByTier) can see which tier a request arrived on without waiting for
	// the request to complete.
	tier := swaputil.TierFromContext(r.Context()).Name
	if tier != "" {
		if entry.Metadata == nil {
			entry.Metadata = map[string]string{}
		}
		entry.Metadata["tier"] = tier
	}
	// Stamp the same child slot the affinity middleware injected at admission
	// (slot_affinity.go), so a renderer joining this row to
	// /upstream/<model>/slots has the number from the FIRST event instead of
	// waiting for the response's id_slot (SetSlotID) - which on the Anthropic
	// streaming path only arrives once the whole stream has been read. The
	// response stamp still runs and wins when the child reassigns mid-stream.
	if hasCtx {
		if slot := data.Metadata["slot_affinity"]; slot != "" {
			entry.Metadata["slot_id"] = slot
		}
	}

	t.mu.Lock()
	req := &inflightRequest{
		entry:       entry,
		cancel:      cancel,
		lastEmitted: time.Now(),
		tier:        tier,
	}
	t.requests[id] = req
	t.enqueueLocked(upsertInflightEvent(req.entry))
	t.mu.Unlock()
	return id
}

func (t *inflightTracker) Remove(id string) {
	t.mu.Lock()
	req, ok := t.requests[id]
	if ok {
		delete(t.requests, id)
		if req.timer != nil {
			req.timer.Stop()
		}
		t.enqueueLocked(swaputil.InFlightRequestsEvent{Operation: inflightOperationRemove, ID: id})
	}
	t.mu.Unlock()
}

// SetSlotID stamps the child's serving slot number onto the LIVE in-flight
// entry for id and re-emits an upsert so subscribers see it before the
// request completes. This is the live twin of the post-hoc `slot_id` that
// record() lands on the COMPLETED activity entry from the response's
// id_slot (metrics.go): a renderer joining /api/events rows to
// /upstream/<model>/slots needs the slot number WHILE the request is in
// flight, and the affinity middleware's own stamp only covers opted-in
// models. Idempotent - re-stamping the same number emits nothing. A no-op
// once the request has already been Remove()d.
func (t *inflightTracker) SetSlotID(id string, slot int) {
	value := strconv.Itoa(slot)
	t.mu.Lock()
	req, ok := t.requests[id]
	if !ok {
		t.mu.Unlock()
		return
	}
	if req.entry.Metadata["slot_id"] == value {
		t.mu.Unlock()
		return
	}
	if req.entry.Metadata == nil {
		req.entry.Metadata = map[string]string{}
	}
	req.entry.Metadata["slot_id"] = value
	req.lastEmitted = time.Now()
	t.enqueueLocked(upsertInflightEvent(req.entry))
	t.mu.Unlock()
}

// SetMetadata stamps a single key/value pair onto the LIVE in-flight entry
// for id, if it is still tracked, and re-emits an upsert so subscribers see
// it before the request completes. This is the update-by-id path a request
// mid-flight uses to record something that only becomes known partway
// through - e.g. slot_granted, stamped by the scheduler's grant (see
// swaputil.InflightMetadataSetter / internal/router/base.go's
// pw.slotGranted() call site) - as distinct from entry.Metadata's initial
// contents, which are frozen from the request context's bag at Add() time.
// A no-op once the request has already been Remove()d.
//
// An EMPTY value DELETES the key. A renderer reads these keys as flags
// (kv_parked, slot_granted) and treats presence as the signal, so a state
// that has ended must leave no key behind rather than an empty string a
// consumer would have to special-case.
func (t *inflightTracker) SetMetadata(id, key, value string) {
	t.mu.Lock()
	req, ok := t.requests[id]
	if !ok {
		t.mu.Unlock()
		return
	}
	if req.entry.Metadata == nil {
		if value == "" {
			t.mu.Unlock()
			return
		}
		req.entry.Metadata = map[string]string{}
	}
	if value == "" {
		delete(req.entry.Metadata, key)
	} else {
		req.entry.Metadata[key] = value
	}
	req.lastEmitted = time.Now()
	t.enqueueLocked(upsertInflightEvent(req.entry))
	t.mu.Unlock()
}

func (t *inflightTracker) SetResponseHeaders(id string, headers http.Header) {
	values := headerMap(headers)
	redactHeaders(values)

	t.mu.Lock()
	req, ok := t.requests[id]
	if ok {
		req.entry.RespHeaders = values
		req.lastEmitted = time.Now()
		t.enqueueLocked(upsertInflightEvent(req.entry))
	}
	t.mu.Unlock()
}

// respCarryMax bounds the split-frame carry buffer. A real Anthropic SSE line
// (a ping, a content_block_delta event line, or its data line) is far smaller;
// the cap only exists so a pathological unterminated line cannot grow the
// carry without limit - it never buffers the whole stream.
const respCarryMax = 64 * 1024

// contentBlockDeltaEvent is the SSE event name that carries REAL Anthropic
// model output. Keepalive events are `event: ping`; matching the event line
// exactly (not a substring of the whole chunk) keeps the count ping-immune and
// avoids double-counting the "content_block_delta" that also appears in the
// event's own data line.
var contentBlockDeltaEvent = []byte("content_block_delta")

// AddResponseBytes records the bytes just written to the client for id. data
// is exactly what reached the socket (data[:n] from the ResponseWriter), so
// len(data) is the byte delta for RespBytes AND the payload scanned for real
// output tokens. Scanning happens on the serving hot path: it is a single
// forward pass over the chunk (plus a small carry) and never blocks or buffers
// the whole stream.
func (t *inflightTracker) AddResponseBytes(id string, data []byte) {
	total := len(data)
	if total <= 0 {
		return
	}

	t.mu.Lock()
	req, ok := t.requests[id]
	if !ok {
		t.mu.Unlock()
		return
	}
	req.entry.RespBytes += int64(total)
	// Count REAL output (content_block_delta events) separately from RespBytes,
	// which still counts EVERYTHING including keepalive pings - the two fields
	// are what let a reader tell a producing slot from an only-pinged one.
	deltas, carry := scanRealOutputDeltas(req.respCarry, data)
	req.respCarry = carry
	req.entry.RespTokens += deltas

	now := time.Now()
	remaining := inflightUpdateInterval - now.Sub(req.lastEmitted)
	if remaining <= 0 && req.timer == nil {
		req.lastEmitted = now
		t.enqueueLocked(upsertInflightEvent(req.entry))
		t.mu.Unlock()
		return
	}
	if req.timer == nil {
		if remaining < 0 {
			remaining = 0
		}
		req.timer = time.AfterFunc(remaining, func() { t.emitPending(id) })
	}
	t.mu.Unlock()
}

// scanRealOutputDeltas counts complete `event: content_block_delta` SSE lines
// across carry (the unterminated tail from the previous chunk) followed by
// data (this chunk), and returns that count plus the new unterminated tail to
// carry forward. It makes ONE forward pass, never buffers more than one
// partial line, and treats every non-delta line (pings, message_start,
// content_block_start/stop, message_delta, message_stop, data lines) as
// nothing - so the count is immune to keepalive ping bytes.
func scanRealOutputDeltas(carry, data []byte) (deltas int64, newCarry []byte) {
	buf := append(carry, data...) // carry's backing array is ours; data is copied in, not aliased
	start := 0
	for {
		nl := bytes.IndexByte(buf[start:], '\n')
		if nl < 0 {
			break
		}
		if isContentBlockDeltaEventLine(buf[start : start+nl]) {
			deltas++
		}
		start += nl + 1
	}
	remaining := buf[start:]
	if len(remaining) > respCarryMax {
		// An unterminated line has outgrown any real SSE frame; keep only the
		// tail so a delta event straddling the drop boundary can still match,
		// and never let the carry pin the whole stream.
		remaining = remaining[len(remaining)-respCarryMax:]
	}
	return deltas, append([]byte(nil), remaining...)
}

// isContentBlockDeltaEventLine reports whether one SSE line (no trailing
// newline) is the event line `event: content_block_delta`. It tolerates an
// optional trailing CR and the space-optional `event:` form, and matches the
// event name EXACTLY so the "content_block_delta" substring inside the event's
// own data line is not miscounted.
func isContentBlockDeltaEventLine(line []byte) bool {
	line = bytes.TrimSuffix(line, []byte("\r"))
	rest, ok := bytes.CutPrefix(line, []byte("event:"))
	if !ok {
		return false
	}
	return bytes.Equal(bytes.TrimSpace(rest), contentBlockDeltaEvent)
}

func (t *inflightTracker) emitPending(id string) {
	t.mu.Lock()
	req, ok := t.requests[id]
	if !ok {
		t.mu.Unlock()
		return
	}
	req.timer = nil
	req.lastEmitted = time.Now()
	t.enqueueLocked(upsertInflightEvent(req.entry))
	t.mu.Unlock()
}

// enqueueLocked adds an update without allowing event-bus backpressure to
// block the request path. On overflow, a later snapshot replaces any dropped
// incremental updates with the tracker's authoritative state.
func (t *inflightTracker) enqueueLocked(update swaputil.InFlightRequestsEvent) {
	update = t.withCountsLocked(update)
	select {
	case t.updates <- update:
	default:
		t.needsSnapshot.Store(true)
	}
	t.startPublisher()
}

func (t *inflightTracker) startPublisher() {
	if t.publisherRunning.CompareAndSwap(false, true) {
		go t.publishUpdates()
	}
}

func (t *inflightTracker) publishUpdates() {
	for {
		select {
		case update := <-t.updates:
			t.publish(refreshInflightElapsed(update))
			t.publishRecoverySnapshots()
		default:
			t.publisherRunning.Store(false)
			// An enqueue racing with the transition to idle either starts a new
			// publisher or leaves work here for this publisher to reclaim.
			if len(t.updates) > 0 && t.publisherRunning.CompareAndSwap(false, true) {
				continue
			}
			return
		}
	}
}

func (t *inflightTracker) publishRecoverySnapshots() {
	for t.needsSnapshot.Swap(false) {
		t.discardQueuedUpdates()
		t.publish(t.Current())
	}
}

func (t *inflightTracker) discardQueuedUpdates() {
	for {
		select {
		case <-t.updates:
		default:
			return
		}
	}
}

func (t *inflightTracker) Cancel(id string) bool {
	t.mu.RLock()
	req, ok := t.requests[id]
	t.mu.RUnlock()
	if !ok {
		return false
	}
	req.cancel()
	return true
}

func (t *inflightTracker) Current() swaputil.InFlightRequestsEvent {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.withCountsLocked(swaputil.InFlightRequestsEvent{
		Operation: inflightOperationSnapshot,
		Requests:  t.snapshotLocked(),
	})
}

func (t *inflightTracker) snapshotLocked() []swaputil.InflightRequestEntry {
	requests := make([]swaputil.InflightRequestEntry, 0, len(t.requests))
	for _, req := range t.requests {
		requests = append(requests, copyInflightEntry(req.entry))
	}
	sort.Slice(requests, func(i, j int) bool {
		if requests[i].Timestamp.Equal(requests[j].Timestamp) {
			return requests[i].ID < requests[j].ID
		}
		return requests[i].Timestamp.Before(requests[j].Timestamp)
	})
	return requests
}

func upsertInflightEvent(entry swaputil.InflightRequestEntry) swaputil.InFlightRequestsEvent {
	entry = copyInflightEntry(entry)
	return swaputil.InFlightRequestsEvent{Operation: inflightOperationUpsert, Request: &entry}
}

func copyInflightEntry(entry swaputil.InflightRequestEntry) swaputil.InflightRequestEntry {
	entry.Metadata = copyMetadata(entry.Metadata)
	entry.ReqHeaders = copyStringMap(entry.ReqHeaders)
	entry.RespHeaders = copyStringMap(entry.RespHeaders)
	setInflightElapsed(&entry)
	return entry
}

func refreshInflightElapsed(update swaputil.InFlightRequestsEvent) swaputil.InFlightRequestsEvent {
	if update.Request != nil {
		entry := *update.Request
		setInflightElapsed(&entry)
		update.Request = &entry
	}
	for i := range update.Requests {
		setInflightElapsed(&update.Requests[i])
	}
	return update
}

func setInflightElapsed(entry *swaputil.InflightRequestEntry) {
	elapsed := time.Since(entry.Timestamp)
	if elapsed < 0 {
		elapsed = 0
	}
	entry.ElapsedMs = elapsed.Milliseconds()
}

func copyStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func copyMetadata(metadata map[string]string) map[string]string {
	if len(metadata) == 0 {
		return nil
	}
	out := make(map[string]string, len(metadata))
	for k, v := range metadata {
		out[k] = v
	}
	return out
}

type inflightResponseWriter struct {
	http.ResponseWriter
	tracker     *inflightTracker
	id          string
	wroteHeader bool
}

func (w *inflightResponseWriter) WriteHeader(statusCode int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.tracker.SetResponseHeaders(w.id, w.Header())
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *inflightResponseWriter) Write(data []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(data)
	// Pass the bytes actually written (data[:n]) so the tracker counts the same
	// n toward RespBytes as before AND can scan them for real output tokens.
	w.tracker.AddResponseBytes(w.id, data[:n])
	return n, err
}

// MarkStatus forwards a recorded-only status to the wrapped writer. The
// tracker records bytes and headers rather than a status code, so there is
// nothing to update here.
func (w *inflightResponseWriter) MarkStatus(code int) {
	if marker, ok := w.ResponseWriter.(swaputil.StatusMarker); ok {
		marker.MarkStatus(code)
	}
}

// WroteHeader reports whether a response status reached the client.
func (w *inflightResponseWriter) WroteHeader() bool { return w.wroteHeader }

func (w *inflightResponseWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *inflightResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hijacker, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hijacker.Hijack()
	}
	return nil, nil, fmt.Errorf("underlying ResponseWriter does not support hijacking")
}

// CreateInflightMiddleware returns middleware that tracks model-dispatched
// requests until downstream handling completes.
func CreateInflightMiddleware(t *inflightTracker, cfg config.Config) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if swaputil.ShouldIgnoreWebsocket(r, cfg) {
				next.ServeHTTP(w, r)
				return
			}

			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()

			r = r.WithContext(ctx)
			id := t.Add(r, cancel)
			defer t.Remove(id)
			// Carry an update-by-id callback bound to this request's id so
			// code deep in the router (which cannot import inflightTracker -
			// see swaputil.InflightMetadataSetter) can stamp things onto the
			// LIVE entry that only become known partway through, e.g.
			// slot_granted at the scheduler's grant.
			r = r.WithContext(swaputil.WithInflightMetadataSetter(r.Context(), func(key, value string) {
				t.SetMetadata(id, key, value)
			}))

			next.ServeHTTP(&inflightResponseWriter{ResponseWriter: w, tracker: t, id: id}, r)
		})
	}
}

// CreateUpstreamInflightMiddleware tracks /upstream/<model>/<path> requests
// only when the stripped upstream path is one of the model-dispatched
// inference endpoints.
func CreateUpstreamInflightMiddleware(t *inflightTracker, cfg config.Config) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.URL.Path, "/upstream/") {
				next.ServeHTTP(w, r)
				return
			}
			r = markInflightStart(r)

			_, _, remainingPath, found := swaputil.FindModelInPath(cfg, strings.TrimPrefix(r.URL.Path, "/upstream"))
			if !found || !isModelDispatchedRequest(r.Method, remainingPath) {
				next.ServeHTTP(w, r)
				return
			}

			if _, err := swaputil.FetchContext(r, cfg); err != nil {
				next.ServeHTTP(w, r)
				return
			}
			if swaputil.ShouldIgnoreWebsocket(r, cfg) {
				next.ServeHTTP(w, r)
				return
			}

			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()

			r = r.WithContext(ctx)
			tracked := r.Clone(ctx)
			tracked.URL.Path = remainingPath
			id := t.Add(tracked, cancel)
			defer t.Remove(id)
			// See the matching comment in CreateInflightMiddleware above.
			r = r.WithContext(swaputil.WithInflightMetadataSetter(r.Context(), func(key, value string) {
				t.SetMetadata(id, key, value)
			}))

			next.ServeHTTP(&inflightResponseWriter{ResponseWriter: w, tracker: t, id: id}, r)
		})
	}
}

func isModelDispatchedRequest(method, path string) bool {
	switch method {
	case http.MethodPost:
		for _, p := range modelPostJSONRoutes {
			if p == path {
				return true
			}
		}
		for _, p := range modelPostFormRoutes {
			if p == path {
				return true
			}
		}
	case http.MethodGet:
		for _, p := range modelGetRoutes {
			if p == path {
				return true
			}
		}
	}
	return false
}
