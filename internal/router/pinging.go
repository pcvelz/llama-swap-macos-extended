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
// A QUEUE HOLDING IS THE MECHANISM WORKING. With two serving slots and three
// sessions the third MUST wait, and the fork's answer to that wait is to HOLD
// THE CONNECTION AND KEEP THE SESSION INFORMED rather than refuse it (llama-cm
// docs/intent/llama-swap-backend.md, "You are held, not refused"). So a request
// still parked in the scheduler queue is pinged for as long as it waits; the
// zero-output budget below applies only once it actually holds a slot. The
// accepted cost is that the first ping commits the response, so a parked
// request can no longer be transparently replayed.
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
	"sync/atomic"
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
	// pingZeroBudget: how long a stream that HOLDS A SERVING SLOT may emit
	// pings having produced ZERO upstream body bytes before the pinger ends it
	// with a readable SSE error. The clock starts at slotGranted, not at
	// request arrival - see that method for why the parked stretch is excluded.
	//
	// WHY THIS EXISTS: pings feed the client's byte watchdog but carry no
	// content, so a granted slot that never produces a token is held open
	// forever and the client cannot tell it apart from a slow one.
	// armParkGiveUp in base.go does not cover this case: it returns early when
	// responseCommitted is true, and the first ping IS the commit. So a
	// request the pinger has taken over gets no give-up at all and falls
	// through to the pre-fix bare return - the `200 0` / `502 0` signature
	// from llama-cm incident
	// 2026-08-18-cq27-background-admission-park-bare-502-retry-storm.
	//
	// ZERO IS THE BAR, and it is @user-gated: this fires only when NOTHING has
	// been written. One body byte disarms it permanently. An agent may not
	// turn this into a rate threshold ("too few tokens per second") because a
	// rate would be more elegant - a slow session is a working session, and
	// the user has accepted 40-minute turns. If evidence later suggests zero
	// is the wrong bar, that is a finding to REPORT, not a change to make.
	// Same bar, same reasoning as llama/queue/loop-turnaround-probe.sh in
	// llama-cm.
	//
	// Set to maxReplayHeld (270s) rather than introducing a second, different
	// deadline number.
	pingZeroBudget = maxReplayHeld
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

	// slotStart is the moment the scheduler granted this request a serving
	// slot, or the zero time while it is still parked in the queue. It is the
	// clock pingZeroBudget measures against.
	slotStart time.Time

	// allowed gates whether tryPing may emit anything at all. Default true
	// (unchanged behaviour for every request that is not replay-eligible); set
	// false at construction for a request that may still be transparently
	// replayed, since the first ping commits a 200 and a replay requires a
	// response with zero committed bytes. base.go flips it back to true at the
	// eligibility-surrender moment. Atomic rather than mu-guarded so the flip
	// never has to take the writer lock, and checked INSIDE tryPing so a
	// suppressed tick simply re-arms and re-evaluates one interval later - a
	// mid-wait flip needs no wakeup plumbing.
	allowed atomic.Bool

	// stall bounds how long a single write to the client may block, so a peer
	// that has stopped reading gives its slot back instead of pinning it. Nil
	// or zero-timeout = disabled; see peerstall.go.
	stall *peerStallGuard

	stopOnce sync.Once
	stopCh   chan struct{}
	// loopDone closes when the ping goroutine has returned. stop() only
	// SIGNALS the goroutine; a caller that needs to know it is actually gone
	// (rather than merely fenced off the writer) waits on this.
	loopDone chan struct{}
	// wakeCh re-evaluates the ping deadline immediately instead of at the next
	// scheduled tick. Buffered 1 and non-blocking so a signaller never waits.
	wakeCh chan struct{}
}

func newPingWriter(logger *logmon.Monitor, model string, w http.ResponseWriter, allowed bool, stall *peerStallGuard) *pingWriter {
	pw := &pingWriter{writer: w, logger: logger, model: model, start: time.Now(), stall: stall,
		stopCh: make(chan struct{}), wakeCh: make(chan struct{}, 1), loopDone: make(chan struct{})}
	pw.allowed.Store(allowed)
	go pw.loop()
	return pw
}

// allowPings unmutes a pinger that was created suppressed. Idempotent, and
// safe to call from a timer goroutine.
//
// It wakes the loop rather than letting it notice at its next scheduled tick:
// the unmute moment is maxReplayHeld, already close to the client's ~300s
// zero-byte abort, so an extra pingInterval of silence eats most of what is
// left of that margin.
func (pw *pingWriter) allowPings() {
	pw.allowed.Store(true)
	pw.wake()
}

// wake nudges the ping loop to re-evaluate now. Never blocks.
func (pw *pingWriter) wake() {
	select {
	case pw.wakeCh <- struct{}{}:
	default:
	}
}

// slotGranted records that the scheduler handed this request a serving slot,
// which starts the pingZeroBudget clock.
//
// WHY THE PARKED STRETCH IS EXCLUDED: measuring the budget from request
// arrival made it fire on a request that was simply third in an honest queue.
// With two slots and multi-minute turns a wait past 270s is the mechanism
// working, and ending that stream is a refusal dressed as a safety net - the
// session sees an error and retries, which is the retry ladder the pinger
// exists to prevent. The condition the budget was written for is the one its
// own message states, "it held a slot without emitting a token", and that can
// only be judged from the grant onwards.
//
// Called again on each replay attempt: a fresh grant is a fresh chance to
// produce a token, and the replay path is separately capped by maxReplays and
// maxReplayHeld.
func (pw *pingWriter) slotGranted() {
	pw.mu.Lock()
	pw.slotStart = time.Now()
	pw.mu.Unlock()
	pw.wake()
}

// pingsCommitted reports whether this writer has committed the response itself
// with a ping (status line + at least one ping event). base.go uses it to
// refuse a replay on an already-committed stream.
func (pw *pingWriter) pingsCommitted() bool {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	return pw.pingsStarted
}

// loop wakes at each computed ping deadline for the stream's whole lifetime.
// tryPing reports how long to sleep until the next candidate deadline, or
// false when pinging must end (writer stopped/errored, client gone).
func (pw *pingWriter) loop() {
	defer close(pw.loopDone)
	timer := time.NewTimer(pingQuietDelay)
	defer timer.Stop()
	for {
		select {
		case <-pw.stopCh:
			return
		case <-pw.wakeCh:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
		}
		wait, ok := pw.tryPing()
		if !ok {
			return
		}
		timer.Reset(wait)
	}
}

// writeSSEErrorLocked emits one Anthropic SSE error event to the client and
// marks the stream errored, so no later writer on this pingWriter (a body
// byte still in flight, a second verdict) mistakes it for open. Shared by
// every proxy-side verdict that ends a stream the pinger has taken over (the
// zero-progress budget and the mid-stream no-forward-progress budget), so the
// frame shape can never drift between them.
//
// Caller must hold pw.mu and must have already checked !pw.errored - this
// does not re-check, because two verdicts racing to call it would otherwise
// both flush a frame onto one stream.
func (pw *pingWriter) writeSSEErrorLocked(errType, message string) {
	pw.errored = true
	fmt.Fprintf(pw.writer,
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"%s\",\"message\":\"%s\"}}\n\n",
		errType, message)
	if f, ok := pw.writer.(http.Flusher); ok {
		f.Flush()
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

	if !pw.allowed.Load() {
		// Muted while this request may still be transparently replayed (see
		// the allowed field). Re-arm rather than terminate: eligibility is
		// surrendered on a timer/attempt boundary, and this tick is how the
		// pinger notices.
		return pingInterval, true
	}

	now := time.Now()

	// Zero-progress budget. Checked before the silence-gap arithmetic because
	// this ends the stream rather than pinging it: a stream that has produced
	// no body byte at all within the budget is not slow, it is one the client
	// can never make sense of. Inert while slotStart is zero - a request still
	// parked in the queue is held for as long as the queue holds it.
	if !pw.wroteBody && pw.pingsStarted && !pw.slotStart.IsZero() &&
		now.Sub(pw.slotStart) >= pingZeroBudget && !pw.errored {
		pw.writeSSEErrorLocked("overloaded_error", fmt.Sprintf(
			"llama-swap: no output was produced in %.0fs and the stream was ended so this request can be retried. It held a slot without emitting a token.",
			pingZeroBudget.Seconds()))
		pw.logger.Warnf("<%s> ping budget: zero body bytes after %s, ended stream with SSE error",
			pw.model, pingZeroBudget)
		// AND give the slot back. Ending the stream only tells the CLIENT to
		// retry; without this the original request stays in flight, still
		// holding the slot the retry now has to queue behind - the failure the
		// 2026-09-01 ruling names. The client-visible behaviour above is
		// unchanged; this only stops the abandoned request from occupying a
		// slot nobody is reading any more.
		pw.stall.reclaimZeroOutputSlot(now.Sub(pw.slotStart), pw.writer)
		return 0, false
	}

	// No-forward-progress budget. The counter is upstream body bytes, so a gap
	// since lastBodyWrite IS a flat token counter (see peerstall.go, SECOND
	// VERDICT). Gated on wroteBody: before the first byte the request may be
	// legitimately prefilling, which this guard must never reap - that window
	// belongs to the zero-progress budget above.
	//
	// Placed here, with the zero budget, because everything below returns early
	// on a stream that is merely mid-gap or mid-SSE-event; a check further down
	// would be unreachable for most of the population it exists for.
	if slotBudget := pw.stall.slotStallTimeout(); slotBudget > 0 && pw.wroteBody && !pw.errored &&
		!pw.slotStart.IsZero() && now.Sub(pw.lastBodyWrite) >= slotBudget {
		// Write the client's ending BEFORE cancelling, same as the zero-progress
		// budget above. The cancel below unwinds the request through
		// httputil.ReverseProxy's ErrorHandler
		// (internal/process/process_command.go newProxyErrorHandler), which
		// returns WITHOUT WRITING once swaputil.ResponseStarted(w) is true -
		// guaranteed here, since pingsStarted already committed the 200. That
		// path does NOT "own what the client is told" for a stream the pinger
		// has taken over; left to it alone the client sees a bare chunked
		// termination with no terminal event to parse or retry on. Writing here
		// first is safe precisely because this ends the stream immediately
		// after (return 0, false) - there is no later write left to race it.
		pw.writeSSEErrorLocked("api_error", fmt.Sprintf(
			"llama-swap: no upstream token reached the client for %s and the stream was ended so this request can be retried. It went flat mid-stream.",
			slotBudget))
		pw.logger.Warnf("<%s> slot stall: no upstream body bytes for %s, ended stream with SSE error",
			pw.model, slotBudget)
		pw.stall.reclaimStalledSlot(now.Sub(pw.lastBodyWrite), pw.writer)
		return 0, false
	}

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
	// Write AND flush inside the stall guard: a 40-byte ping never blocks in
	// the buffered write, only in the flush that drains it to the socket, so
	// guarding the write alone would watch the wrong half.
	if _, err := pw.stall.write(pw.writer, func() (int, error) {
		n, err := fmt.Fprint(pw.writer, "event: ping\ndata: {\"type\": \"ping\"}\n\n")
		if err != nil {
			return n, err
		}
		if f, ok := pw.writer.(http.Flusher); ok {
			f.Flush()
		}
		return n, nil
	}); err != nil {
		pw.logger.Debugf("<%s> keepalive ping write failed (client likely disconnected): %v", pw.model, err)
		return 0, false
	}
	if pw.stall.stalled() {
		// The guard cancelled the request and the connection is spent (an
		// expired write deadline is latched by net/http). Stop pinging a peer
		// that is provably not listening.
		return 0, false
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
	// The body copy is where a frozen peer actually shows up: token deltas fill
	// the socket buffer in seconds, whereas 40-byte pings would take an hour to
	// do it. Guarding this write is what makes the reclaim timely.
	return pw.stall.write(pw.writer, func() (int, error) { return pw.writer.Write(p) })
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

// waitLoop blocks until the ping goroutine has returned. stop() must have been
// called first or this never returns.
//
// Production does not need it - stop()'s fence is enough to hand the
// ResponseWriter back safely. It exists so a test can establish a
// happens-before edge with the goroutine before touching the package-level
// pacing vars it read at startup; without that edge those tests are a genuine
// data race under -race, not a false positive.
func (pw *pingWriter) waitLoop() { <-pw.loopDone }
