package router

// peerStallGuard reclaims a serving slot from a client that has STOPPED
// READING its response.
//
// THE PROBLEM. llama-swap holds a request open for as long as it takes, on
// purpose (llama-cm docs/intent/llama-swap-backend.md, "You are held, not
// refused"). Cancellation therefore has exactly one trigger today: the request
// context, which fires when the client's connection is CLOSED. A client that
// neither reads nor closes - the case that motivated this, a Claude Code
// session SIGSTOPped by cm-menu's session-freeze - never trips it. Its socket
// stays open, its receive window stays shut, and the slot it holds stays held
// until the process is thawed. The keepalive pinger does not help: it writes to
// the same doomed connection, so it merely blocks alongside everything else.
//
// THE SIGNAL, and the only one this file is allowed to act on: a write to the
// client that cannot make progress. Not elapsed request time, not token rate,
// not queue depth - a request that is parked for hours or streaming one token a
// minute is the product working, and a blanket http.Server WriteTimeout would
// break it. What separates a frozen peer from a slow one is that a slow peer
// still DRAINS ITS SOCKET, so kernel buffering makes writes on this side return
// immediately.
//
// THE MECHANISM. Each write to the client is bracketed by an absolute write
// deadline of exactly the stall budget, installed through
// http.ResponseController and cleared again the moment the write returns.
// Deadlines are absolute, so leaving one installed would fire on the NEXT write
// of a perfectly healthy but legitimately silent stream - hence set-and-clear
// around every write rather than once per request.
//
// WHY THE DEADLINE IS THE WHOLE BUDGET AND NOT A SHORT PROBE. Once a write
// deadline expires on a net/http connection the connection is finished: the
// error is latched in the connection's bufio.Writer and every later write fails
// immediately. So an expired deadline cannot be used to sample and retry - it
// is a one-shot verdict, and the only defensible verdict to attach to it is the
// full budget.
//
// WHAT RECLAIM DOES. It cancels the request context. That is deliberately the
// SAME path a real client disconnect already takes today, so the scheduler
// releases the slot, the upstream inference is aborted, and no other component
// has to learn a new outcome. Status codes, bodies and retry semantics for
// healthy clients are untouched.
//
// ---------------------------------------------------------------------------
// SECOND VERDICT: NO FORWARD PROGRESS (slot stall)
//
// User ruling (llama-cm llama/incidents/2026-08-28-idle-cc-sessions-pin-ram-no-
// freeze.md, "USER RULING (2026-09-01)"): "if it doesn't generate tokens, that
// should be the prime reason to evict the slot - above all the layers." A slot
// is worth reclaiming when the request holding it has gone flat, whatever the
// cause: frozen client, wedged upstream, or a stall neither end notices.
//
// THE COUNTER. The proxy's in-process measure of forward progress is upstream
// BODY BYTES DELIVERED, tracked by pingWriter (lastBodyWrite). Bytes only flow
// on this path when the child produces tokens, so a byte gap IS a flat token
// counter. Evidence for why the budget must be generous rather than a rate: a
// thermally throttled box decoding at 5-10 tok/s writes every 100-200 ms,
// nowhere near any budget here. SLOW IS NOT STUCK, and this must never become a
// rate threshold - same bar and same reasoning as pingZeroBudget in pinging.go
// and llama/queue/loop-turnaround-probe.sh in llama-cm.
//
// PREFILL IS PROGRESS, and it is deliberately OUT OF SCOPE for this guard. A
// request that has not yet produced its first byte may be legitimately
// prefilling - at 40 tok/s a large context takes many minutes with the byte
// counter honestly flat the whole time - so reaping on a flat counter before
// the first byte would kill exactly the healthy case the ruling protects. The
// slot-stall clock therefore ARMS ON THE FIRST BODY BYTE and is inert before
// it. The pre-first-byte window keeps its existing, separately @user-gated
// owner: pingZeroBudget.
//
// THE TRAP WE DID NOT WALK INTO. The obvious way to watch prefill directly is
// the child's /slots (n_prompt_tokens_processed) or its /metrics. Both queue a
// task onto llama-server's inference queue, so both are STARVED precisely when
// the box is loaded, which is the only time this feature matters. Witnessed on
// the live backend: repeated `GET /upstream/<model>/slots ... 499 0 ... 1.0s` in
// the proxy log, i.e. the poller giving up after a second. Any progress signal
// built on polling those endpoints fails exactly when it is needed. That is why
// this guard reads a counter the proxy already owns in-process and never polls
// the child.

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mostlygeek/llama-swap/internal/logmon"
)

type peerStallGuard struct {
	// timeout is the stall budget. Zero means the guard is disabled and every
	// write goes through untouched - byte-identical pre-feature behaviour,
	// including the absence of the write serialization below.
	timeout time.Duration
	// slotTimeout is the no-forward-progress budget. Zero disables the second
	// verdict independently of the first: the two knobs are separate config
	// entries because they answer different questions (is the CLIENT reading
	// vs is the REQUEST advancing) and an operator may well want one without
	// the other.
	slotTimeout time.Duration
	logger      *logmon.Monitor
	model       string

	// mu serializes deadline-install / write / deadline-clear. Two goroutines
	// write to the client on this path (the upstream body copy and the
	// keepalive pinger) and they share ONE connection deadline, so an
	// unserialized pair would let the pinger clear the body write's deadline
	// and vice versa. It also makes the two writers mutually exclusive on the
	// socket, which they always should have been.
	mu sync.Mutex

	// cancel is the current attempt's request-cancel func. A pointer swapped
	// per replay attempt (see base.go): each attempt gets a fresh context, and
	// cancelling a spent one would be a no-op.
	cancel atomic.Pointer[context.CancelFunc]

	// fired latches the verdict so a stalled stream logs and cancels once,
	// however many writers pile up behind the same dead connection - and so the
	// two verdicts never both fire on one request. Whichever notices first
	// wins; the second would cancel an already-cancelled context and put a
	// second, contradictory line in the log.
	fired atomic.Bool
}

func newPeerStallGuard(logger *logmon.Monitor, model string, timeout, slotTimeout time.Duration) *peerStallGuard {
	return &peerStallGuard{timeout: timeout, slotTimeout: slotTimeout, logger: logger, model: model}
}

// slotStallTimeout is the no-forward-progress budget, or zero when that verdict
// is disabled. Nil-safe so pingWriter can call it without a guard.
func (g *peerStallGuard) slotStallTimeout() time.Duration {
	if g == nil {
		return 0
	}
	return g.slotTimeout
}

// setCancel installs the cancel func for the attempt now in flight.
func (g *peerStallGuard) setCancel(fn context.CancelFunc) {
	if g == nil || fn == nil {
		return
	}
	g.cancel.Store(&fn)
}

// stalled reports whether this guard has already reclaimed. Callers use it to
// stop writing: after a reclaim the connection is dead and further writes are
// noise.
func (g *peerStallGuard) stalled() bool {
	return g != nil && g.fired.Load()
}

// write runs one write to the client under the stall budget. fn must perform
// the whole client-visible operation, INCLUDING its flush: for a small payload
// such as a keepalive ping the buffered write never blocks, only the flush that
// drains it to the socket does.
//
// Nil-receiver and zero-timeout are both "disabled" and pass straight through.
func (g *peerStallGuard) write(w http.ResponseWriter, fn func() (int, error)) (int, error) {
	if g == nil || g.timeout <= 0 {
		return fn()
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	rc := http.NewResponseController(w)
	// A chain that cannot carry a deadline (a test recorder, a hijacked
	// connection, an upstream wrapper without Unwrap) fails OPEN: an unguarded
	// write is exactly what this proxy did before the guard existed. It must
	// never turn into a refusal.
	armed := rc.SetWriteDeadline(time.Now().Add(g.timeout)) == nil

	started := time.Now()
	n, err := fn()
	blocked := time.Since(started)

	if armed {
		// Clear before evaluating: even on the reclaim path the response still
		// unwinds through other writers, and an absolute deadline left in place
		// would make their errors unreadable.
		_ = rc.SetWriteDeadline(time.Time{})
	}

	// Two ways the verdict shows up. The error is the direct one. The elapsed
	// check is not redundant: http.ResponseController.Flush reports no error
	// when it resolves to a plain http.Flusher (the common case in this chain),
	// so a flush that burned the entire budget can return nil. Either way, a
	// write that occupied the whole budget is the verdict.
	//
	// No SSE error frame is written here, unlike the two pingWriter verdicts:
	// the whole point of THIS verdict is that a write to this peer already
	// burned the entire stall budget without landing, so attempting one more
	// write is not a courtesy, it is another write that will not land either -
	// the client is not reading, not merely silent.
	if armed && (isWriteDeadlineErr(err) || blocked >= g.timeout) {
		g.reclaim(blocked, w)
	}
	return n, err
}

// terminationMarker is implemented by response writers that can record why
// the proxy itself ended a stream (as opposed to the request finishing, or
// the client disconnecting), for the access log. Declared locally rather than
// added to swaputil: a type assertion against a same-shaped interface works
// against any concrete type regardless of which package declares it, so
// internal/server's statusRecorder can implement this method without either
// package importing the other.
type terminationMarker interface {
	MarkTermination(reason string)
}

// findTerminationMarker walks w's Unwrap chain - the SAME chain
// http.ResponseController already walks to reach the write deadline above -
// looking for a writer that can record the termination reason. Fails open,
// same contract as the deadline arming: a chain that never reaches one (a test
// writer, a wrapper without Unwrap) is simply skipped rather than treated as
// an error.
func findTerminationMarker(w http.ResponseWriter) (terminationMarker, bool) {
	for w != nil {
		if marker, ok := w.(terminationMarker); ok {
			return marker, true
		}
		unwrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return nil, false
		}
		w = unwrapper.Unwrap()
	}
	return nil, false
}

// isWriteDeadlineErr reports whether err is the expired write deadline rather
// than an ordinary disconnect (ECONNRESET, EPIPE). Both end the request, but
// only the deadline means "the peer was alive and silently not reading", which
// is the thing worth naming in the log.
func isWriteDeadlineErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// reclaim logs the peer-stalled verdict once and cancels the request. w is
// the writer the stalled write was attempted on, threaded through only so the
// access log can record why (see MarkTermination on findTerminationMarker);
// nil is fine when a caller (a test) has no writer to offer.
func (g *peerStallGuard) reclaim(blocked time.Duration, w http.ResponseWriter) {
	g.fire("peer-stalled", blocked, w,
		"client accepted no bytes for the whole budget")
}

// reclaimStalledSlot logs the slot-stalled verdict once and cancels the
// request: a stream that PRODUCED at least one body byte and then went flat.
// Same reclaim path as the peer verdict by design: the scheduler, the child,
// and every observer should see one outcome shape, not two.
//
// Callers must have established that the request already produced a body byte
// (see the PREFILL IS PROGRESS note in the file header). This function does not
// re-check that, because the only counter it could re-check it against is the
// one the caller is holding a lock on.
func (g *peerStallGuard) reclaimStalledSlot(flat time.Duration, w http.ResponseWriter) {
	g.fire("slot-stalled", flat, w,
		"no upstream token reached the client for the whole budget")
}

// reclaimZeroOutputSlot logs the zero-output-budget verdict once and cancels
// the request: a stream that held a slot and produced NOTHING, ever - the
// pre-first-byte counterpart to reclaimStalledSlot. Kept as its own verdict
// name (rather than folding into reclaimStalledSlot) because "never started"
// and "went flat mid-stream" are different facts for an operator reading the
// access log, even though the cancel mechanics are identical.
func (g *peerStallGuard) reclaimZeroOutputSlot(zero time.Duration, w http.ResponseWriter) {
	g.fire("zero-output-budget", zero, w,
		"no output was produced within the budget")
}

// fire is the single reclaim path every verdict goes through: latch, log one
// greppable line, record why on the access log line via w, cancel. Cancelling
// is what actually frees the scheduler slot and aborts the upstream
// generation - the log line alone changes nothing.
func (g *peerStallGuard) fire(verdict string, elapsed time.Duration, w http.ResponseWriter, why string) {
	if g == nil || !g.fired.CompareAndSwap(false, true) {
		return
	}
	g.logger.Warnf("%s: reclaimed slot model=%s after=%s (%s; cancelling the request as if it had disconnected)",
		verdict, g.model, elapsed.Round(time.Second), why)
	// The pinger already committed a plain 200, so nothing about the access
	// log's status line tells an operator this stream was cut by the proxy
	// rather than finishing normally or the client leaving - see log.go
	// CreateRequestLogMiddleware's `cut=` field. MarkTermination reaches
	// statusRecorder through the same Unwrap chain used above for the write
	// deadline, not through the MarkStatus-style forwarding each wrapper in
	// between would otherwise need to grow - see findTerminationMarker.
	if marker, ok := findTerminationMarker(w); ok {
		marker.MarkTermination(verdict)
	}
	if fn := g.cancel.Load(); fn != nil {
		(*fn)()
	}
}
