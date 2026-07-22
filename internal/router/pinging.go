package router

// pingWriter keeps silent Anthropic (/v1/messages) streams alive while a
// request is parked behind a swap, a model is loading, or a long prefill has
// not yet produced its first byte — and during ANY later body-silence gap
// for the stream's whole lifetime.
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
// 2026-07-22 finding (llama-cm incident 2026-07-09-compact-stall follow-up):
// llama-server (httplib) commits "200 + text/event-stream" response headers
// IMMEDIATELY at request accept — before task scheduling, prefill, or the
// first token. The original design disarmed the pinger on the first upstream
// WriteHeader/Write, which made it dead code for every request that actually
// reached llama-server: headers went out, the pinger stepped aside, and the
// ensuing minutes of body silence (queued task, 163k prefill, decode
// starvation) delivered zero bytes — clients still died at their ~300s BODY
// timeout, which response headers alone do not feed (six metronomic 5m0.06s
// aborts witnessed).
//
// BEHAVIOUR: continuous silence-gap pinging. A ping event is emitted whenever
// no upstream BODY byte has been written for pingQuietDelay (first gap after
// start or after an upstream write) / pingInterval (subsequent pings while
// the gap continues) — regardless of whether headers or earlier body bytes
// already flowed. Upstream headers pass through untouched and do NOT disarm
// the pinger. Pinging stops only at stop() (ServeHTTP return) or after an
// SSE error event was emitted. If the upstream fails AFTER pings started
// (status line already committed), the failure is mapped to an Anthropic SSE
// `error` event, which clients parse and retry.
import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/logmon"
)

// Package-level vars (not consts) so tests can shorten the cadence; the
// production defaults are unchanged.
var (
	// pingQuietDelay: silence budget before the first ping of a gap. Well
	// under the ~300s client zero-byte timeouts, well over any healthy TTFB.
	pingQuietDelay = 20 * time.Second
	// pingInterval: cadence after the first ping of a gap. Anthropic pings
	// are legal at any frequency; 15s comfortably feeds byte-level watchdogs.
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
	// stopped is set, the ping goroutine never touches the writer again
	// (same fence contract as loadingWriter.release).
	mu       sync.Mutex
	stopped  bool
	errored  bool
	start    time.Time
	lastPing time.Time

	// headersSent: upstream (or the pinger) already committed a status line,
	// so the pinger must not WriteHeader again — only append ping events.
	// WHY: a bare upstream WriteHeader(200) is llama-server accepting the
	// request, not the stream becoming alive; it must not disarm pinging.
	headersSent bool
	// pingsStarted: the pinger committed the 200 + SSE status line itself;
	// late upstream failures must be mapped to an SSE error event.
	pingsStarted bool

	// wroteBody / lastBodyWrite / tail track upstream body activity so the
	// pinger can measure silence gaps and respect SSE event boundaries.
	wroteBody     bool
	pingedThisGap bool
	lastBodyWrite time.Time
	tail          string // trailing bytes (<=2) of the last upstream Write

	stopOnce sync.Once
	stopCh   chan struct{}
}

func newPingWriter(logger *logmon.Monitor, model string, w http.ResponseWriter) *pingWriter {
	pw := &pingWriter{writer: w, logger: logger, model: model, start: time.Now(), stopCh: make(chan struct{})}
	go pw.loop()
	return pw
}

// loop wakes at each computed ping deadline for the stream's whole lifetime.
// tryPing reports how long to sleep until the next candidate deadline, or
// false when pinging must end (writer stopped/errored, client gone).
func (pw *pingWriter) loop() {
	timer := time.NewTimer(pingQuietDelay)
	defer timer.Stop()
	for {
		select {
		case <-pw.stopCh:
			return
		case <-timer.C:
		}
		wait, ok := pw.tryPing()
		if !ok {
			return
		}
		timer.Reset(wait)
	}
}

// tryPing emits one ping event if the stream has been body-silent long
// enough and sits at an SSE event boundary. Returns the delay until the next
// candidate deadline, or false to terminate the ping loop.
func (pw *pingWriter) tryPing() (time.Duration, bool) {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	if pw.stopped || pw.errored {
		// WHY: stop() fences the goroutine off the writer before ServeHTTP
		// returns; an emitted SSE error event semantically ends the stream.
		return 0, false
	}

	now := time.Now()
	// Measure the gap from the last upstream body byte (or stream start);
	// after the first ping of a gap, subsequent pings ride pingInterval.
	base := pw.start
	delay := pingQuietDelay
	if pw.wroteBody {
		base = pw.lastBodyWrite
	}
	if pw.pingedThisGap {
		base = pw.lastPing
		delay = pingInterval
	}
	if remaining := base.Add(delay).Sub(now); remaining > 0 {
		// Not silent long enough yet (upstream wrote since the last wake);
		// WHY: re-arm for the exact deadline instead of a fixed tick, so a
		// busy stream never sees a ping and a quiet one is pinged promptly.
		return remaining, true
	}

	// SSE-boundary safety: a ping injected between events (or mid-event)
	// corrupts the client's event parser. Only emit when nothing has been
	// written yet or the last upstream bytes closed an event ("\n\n").
	// WHY skip instead of forcing: a mid-event tail means upstream is
	// actively streaming — the client watchdog is already fed, so dropping
	// this tick and re-checking one interval later costs nothing.
	if pw.wroteBody && !strings.HasSuffix(pw.tail, "\n\n") {
		return pingInterval, true
	}

	if !pw.headersSent {
		h := pw.writer.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		pw.writer.WriteHeader(http.StatusOK)
		pw.headersSent = true
		pw.pingsStarted = true
		pw.logger.Infof("<%s> keepalive pings started (stream silent > %s: parked, loading or prefilling)",
			pw.model, pingQuietDelay)
	}
	if _, err := fmt.Fprint(pw.writer, "event: ping\ndata: {\"type\": \"ping\"}\n\n"); err != nil {
		pw.logger.Debugf("<%s> keepalive ping write failed (client likely disconnected): %v", pw.model, err)
		return 0, false
	}
	if f, ok := pw.writer.(http.Flusher); ok {
		f.Flush()
	}
	pw.lastPing = now
	pw.pingedThisGap = true
	return pingInterval, true
}

// Write forwards upstream body bytes and records the write time plus the
// trailing bytes, so the pinger can measure the silence gap and never inject
// a ping mid-event.
func (pw *pingWriter) Write(p []byte) (int, error) {
	pw.mu.Lock()
	pw.wroteBody = true
	pw.lastBodyWrite = time.Now()
	// WHY reset the gap threshold: fresh upstream bytes already fed the
	// client's watchdog, so the next ping is due a full quiet delay later.
	pw.pingedThisGap = false
	if len(p) >= 2 {
		pw.tail = string(p[len(p)-2:])
	} else {
		// Short writes (e.g. a single byte) append to the previous tail so
		// boundary detection still sees the true last two bytes.
		pw.tail = (pw.tail + string(p))
		if len(pw.tail) > 2 {
			pw.tail = pw.tail[len(pw.tail)-2:]
		}
	}
	errored := pw.errored
	pw.mu.Unlock()
	if errored {
		// An SSE error event already terminated this stream semantically;
		// swallow the raw error body that follows a mapped WriteHeader.
		return len(p), nil
	}
	return pw.writer.Write(p)
}

// WriteHeader forwards the upstream status line. It never disarms pinging:
// llama-server commits 200 + SSE headers at request accept, minutes before
// the first body byte, and headers alone do not feed client body timeouts.
func (pw *pingWriter) WriteHeader(code int) {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	if !pw.pingsStarted {
		// WHY: pass the real status through (pings commit their own 200 only
		// when they win the race); remember it so tryPing never double-commits.
		if !pw.headersSent {
			pw.writer.WriteHeader(code)
			pw.headersSent = true
		}
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
