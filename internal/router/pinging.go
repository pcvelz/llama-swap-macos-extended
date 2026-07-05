package router

// pingWriter keeps silent Anthropic (/v1/messages) streams alive while a
// request is parked behind a swap, a model is loading, or a long prefill has
// not yet produced its first byte.
//
// WHY: clients abort streams that receive zero bytes for ~300s (Claude Code /
// undici headers+body timeouts; raise-only, not fully env-controllable). A
// request parked at llama-swap receives NOTHING, so every wait longer than
// ~5 minutes died client-side and re-queued, producing retry storms and
// permanently-held concurrency permits (2026-07-05 forensics, llama-cm
// docs/research/2026-07-05-cq27-stall-forensics.md). The OpenAI path already
// had loadingWriter; the Anthropic path had nothing because loadingWriter's
// SSE shape is OpenAI-specific.
//
// BEHAVIOUR: lazy. If the upstream produces its first byte within
// pingQuietDelay the wrapper is a pure passthrough (real status code, zero
// overhead). After pingQuietDelay of total silence it commits 200 + SSE
// headers and emits an Anthropic-legal `ping` event every pingInterval until
// the first upstream byte, then steps aside permanently. If the upstream
// fails AFTER pings started (status line already committed), the failure is
// mapped to an Anthropic SSE `error` event, which clients parse and retry.
import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/logmon"
)

const (
	// pingQuietDelay: silence budget before the first ping. Well under the
	// ~300s client zero-byte timeouts, well over any healthy TTFB.
	pingQuietDelay = 20 * time.Second
	// pingInterval: cadence after the first ping. Anthropic pings are legal
	// at any frequency; 15s comfortably feeds byte-level watchdogs.
	pingInterval = 15 * time.Second
)

func isAnthropicStreamPath(path string) bool {
	return strings.HasPrefix(path, "/v1/messages") && !strings.HasSuffix(path, "/count_tokens")
}

type pingWriter struct {
	writer http.ResponseWriter
	logger *logmon.Monitor
	model  string

	// mu serializes all writer access and guards the state below. Once
	// upstreamStarted or stopped is set, the ping goroutine never touches the
	// writer again (same fence contract as loadingWriter.release).
	mu              sync.Mutex
	upstreamStarted bool
	pingsStarted    bool
	errored         bool
	stopped         bool

	stopOnce sync.Once
	stopCh   chan struct{}
}

func newPingWriter(logger *logmon.Monitor, model string, w http.ResponseWriter) *pingWriter {
	pw := &pingWriter{writer: w, logger: logger, model: model, stopCh: make(chan struct{})}
	go pw.loop()
	return pw
}

func (pw *pingWriter) loop() {
	timer := time.NewTimer(pingQuietDelay)
	defer timer.Stop()
	select {
	case <-pw.stopCh:
		return
	case <-timer.C:
	}
	if !pw.emitPing() {
		return
	}
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-pw.stopCh:
			return
		case <-ticker.C:
			if !pw.emitPing() {
				return
			}
		}
	}
}

// emitPing writes one Anthropic SSE ping event; the first call commits
// 200 + SSE headers. Returns false when pinging must end (upstream started,
// writer stopped/errored, or the client is gone).
func (pw *pingWriter) emitPing() bool {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	if pw.upstreamStarted || pw.stopped || pw.errored {
		return false
	}
	if !pw.pingsStarted {
		h := pw.writer.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		pw.writer.WriteHeader(http.StatusOK)
		pw.pingsStarted = true
		pw.logger.Infof("<%s> keepalive pings started (stream silent > %s: parked, loading or prefilling)",
			pw.model, pingQuietDelay)
	}
	if _, err := fmt.Fprint(pw.writer, "event: ping\ndata: {\"type\": \"ping\"}\n\n"); err != nil {
		pw.logger.Debugf("<%s> keepalive ping write failed (client likely disconnected): %v", pw.model, err)
		return false
	}
	if f, ok := pw.writer.(http.Flusher); ok {
		f.Flush()
	}
	return true
}

// Write: the first upstream byte permanently ends pinging.
func (pw *pingWriter) Write(p []byte) (int, error) {
	pw.mu.Lock()
	pw.upstreamStarted = true
	errored := pw.errored
	pw.mu.Unlock()
	if errored {
		// An SSE error event already terminated this stream semantically;
		// swallow the raw error body that follows a mapped WriteHeader.
		return len(p), nil
	}
	return pw.writer.Write(p)
}

func (pw *pingWriter) WriteHeader(code int) {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	pw.upstreamStarted = true
	if !pw.pingsStarted {
		pw.writer.WriteHeader(code)
		return
	}
	// Status line already committed as 200 for the ping stream: map late
	// failures to an Anthropic SSE error event the client can parse and retry.
	if code >= http.StatusBadRequest && !pw.errored {
		pw.errored = true
		fmt.Fprintf(pw.writer,
			"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"llama-swap: upstream failed with status %d during load/prefill\"}}\n\n",
			code)
		if f, ok := pw.writer.(http.Flusher); ok {
			f.Flush()
		}
		pw.logger.Warnf("<%s> upstream status %d after keepalive pings started; mapped to SSE error event",
			pw.model, code)
	}
}

func (pw *pingWriter) Header() http.Header { return pw.writer.Header() }

func (pw *pingWriter) Flush() {
	if f, ok := pw.writer.(http.Flusher); ok {
		f.Flush()
	}
}

// stop fences the ping goroutine off from the ResponseWriter. Must run before
// ServeHTTP returns (a use-after-return Flush panics on the recycled writer).
// Idempotent.
func (pw *pingWriter) stop() {
	pw.stopOnce.Do(func() { close(pw.stopCh) })
	pw.mu.Lock()
	pw.stopped = true
	pw.mu.Unlock()
}
