package router

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// A request that was cut (a preemption, a stall reclaim, a client that hung up)
// is not necessarily gone from the engine that was serving it.
//
// llama-server frees the slot at once, and for the cases where it does not the
// slot table (slotbind.go) holds the cut request's place until the child's
// /slots reads idle. An engine with no slot endpoint cannot be asked. Splash
// keeps a cancelled request in its bounded queue until it next looks at the
// connection, seconds later, and answers every arrival in the meantime with a
// 503 and a Retry-After. The scheduler cannot know: it granted the next request
// the moment the cut one was released.
//
// That refusal is not an answer to the request, it is the engine saying "still
// busy with the one you cut". Handing it to the client was wrong twice over:
//
//   - the request that cut the victim (a priority arrival) got a 503 after 4 ms,
//     so the preemption bought it nothing;
//   - the victim's replay, granted the instant the arrival's 503 freed the slot,
//     was refused too, and because the victim's status line was long committed
//     the refusal's JSON body was written into its event stream as the "result":
//     a client holding a message_start and some pings, then a line that is not a
//     frame, then the end of the stream.
//
// So a retryable refusal is held back while it is plausibly the cut request's
// shadow, and the same grant forwards the request again. The grant matters: the
// retry runs inside the slot the scheduler gave this request, so a queued replay
// cannot be granted behind its back and race the arrival for the engine, which
// would let the replay win the freed place and have the arrival boot it again.
//
// Bounds: only a refusal that follows a cut on the same model (cutLingerWindow),
// only 503 and 529, only before this attempt wrote a byte, only for a request
// whose body can be sent again, and for at most refusalRetryBudget per grant.
// Past the budget the next answer is delivered as the child gave it.

var (
	// cutLingerWindow: a refusal counts as the cut request's shadow only when a
	// request on the same model was cut this recently.
	cutLingerWindow = 60 * time.Second
	// refusalRetryBudget: the longest one grant keeps retrying.
	refusalRetryBudget = 60 * time.Second
	// The cadence between tries: the child's Retry-After when it gives seconds,
	// else refusalRetryDefault, always inside [refusalRetryMin, refusalRetryMax].
	refusalRetryDefault = 500 * time.Millisecond
	refusalRetryMin     = 250 * time.Millisecond
	refusalRetryMax     = 2 * time.Second
)

// retryableRefusal: the statuses a child uses for "busy, ask again shortly".
func retryableRefusal(code int) bool {
	return code == http.StatusServiceUnavailable || code == 529
}

// parseRetryAfter reads a Retry-After given in seconds; an HTTP date or an
// absent header reads as zero (use the default cadence).
func parseRetryAfter(v string) time.Duration {
	s, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || s < 0 {
		return 0
	}
	return time.Duration(s) * time.Second
}

// refusalWait is how long to wait before the next try.
func refusalWait(retryAfter time.Duration) time.Duration {
	w := retryAfter
	if w <= 0 {
		w = refusalRetryDefault
	}
	if w < refusalRetryMin {
		w = refusalRetryMin
	}
	if w > refusalRetryMax {
		w = refusalRetryMax
	}
	return w
}

// cutStamps records when a request on each model was last cut.
type cutStamps struct {
	mu sync.Mutex
	at map[string]time.Time
}

func (c *cutStamps) note(model string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at == nil {
		c.at = map[string]time.Time{}
	}
	c.at[model] = time.Now()
}

// since reports how long ago a request on model was last cut, and false when
// none ever was.
func (c *cutStamps) since(model string) (time.Duration, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.at[model]
	if !ok {
		return 0, false
	}
	return time.Since(t), true
}

// refusalWriter sits next to the reverse proxy for one forwarding attempt that
// may be retried. A retryable refusal as the attempt's status is withheld: its
// status line, headers and body never reach the writers behind it, and
// refused() tells the caller to try again. Anything else passes through
// untouched.
//
// The header map is the client's own, shared along the whole chain, and the
// reverse proxy has already copied the refusal's headers into it by the time it
// writes the status (Content-Length and Content-Type of a small JSON error,
// Retry-After). They are taken back out, or the answer that finally succeeds
// would inherit a Content-Length that belongs to the refusal.
type refusalWriter struct {
	http.ResponseWriter
	snap       http.Header // the client-facing headers before the attempt started
	decided    bool        // the final status was seen
	wasRefused atomic.Bool
	retryAfter time.Duration
	status     int
}

func newRefusalWriter(w http.ResponseWriter) *refusalWriter {
	return &refusalWriter{ResponseWriter: w, snap: w.Header().Clone()}
}

func (r *refusalWriter) refused() bool { return r.wasRefused.Load() }

func (r *refusalWriter) WriteHeader(code int) {
	// 1xx are interim responses, not the attempt's answer.
	if !r.decided && code >= http.StatusOK {
		r.decided = true
		if retryableRefusal(code) {
			r.status = code
			r.retryAfter = parseRetryAfter(r.Header().Get("Retry-After"))
			h := r.Header()
			for k := range h {
				delete(h, k)
			}
			for k, v := range r.snap {
				h[k] = v
			}
			r.wasRefused.Store(true)
			return
		}
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *refusalWriter) Write(b []byte) (int, error) {
	if r.wasRefused.Load() {
		return len(b), nil
	}
	r.decided = true // an implicit 200
	return r.ResponseWriter.Write(b)
}

// Flush, Hijack and ReadFrom forward the optional interfaces the wrapped writer
// has, as preemptResponseWriter does; method promotion does not cover them.
func (r *refusalWriter) Flush() {
	if r.wasRefused.Load() {
		return
	}
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *refusalWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := r.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

func (r *refusalWriter) ReadFrom(src io.Reader) (int64, error) {
	if r.wasRefused.Load() {
		return io.Copy(io.Discard, src)
	}
	r.decided = true
	if rf, ok := r.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(src)
	}
	return io.Copy(struct{ io.Writer }{r.ResponseWriter}, src)
}
