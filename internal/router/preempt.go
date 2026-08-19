package router

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"sync/atomic"
)

// preemptResponseWriter wraps a granted request's ResponseWriter so a
// server-side preemption (scheduler.HandlerReq.Preempt, see fifo.go
// tryPreempt) can surface as a 503 with `X-LlamaSwap-Preempted: 1` +
// `Retry-After` instead of whatever the upstream reverse proxy would
// otherwise write once its context is cancelled (typically a bare 502).
//
// This is best-effort by design (docs/intent/llama-swap-tiers.md, "Booted
// back into the queue" mechanics): it only rewrites the FIRST WriteHeader (or
// implicit-200 first Write) call. If the victim's handler already streamed a
// response before the preempt landed, the stream is simply aborted by the
// context cancel — there is nothing left to rewrite.
//
// v2 (docs/intent/llama-swap-tiers.md "Known v1 limitations" -> "Transparent
// server-side replay of not-yet-streamed requests"): when replayWanted is
// non-nil, that FIRST WriteHeader call — the exact "preempted, zero bytes
// reached the client yet" moment — is swallowed instead of turned into a 503:
// nothing is committed to the real client connection, and replayWanted is set
// so internal/router/base.go ServeHTTP knows to re-submit a fresh attempt
// rather than returning this response. A request that already streamed
// anything never reaches this branch again (wroteHeader is already true), so
// it keeps the v1 abort-in-place behavior unchanged, exactly as before.
type preemptResponseWriter struct {
	http.ResponseWriter
	preempted *atomic.Bool
	// replayWanted, when non-nil, marks this attempt eligible for a
	// transparent replay (see the type doc above). nil means "not eligible
	// this attempt" (non-preemptible tier, or the replay budget/deadline is
	// exhausted — see base.go maxReplays/maxReplayHeld) — the writer then
	// falls back to the v1 cancel+503 exactly as before.
	replayWanted *atomic.Bool
	wroteHeader  bool
}

// newPreemptResponseWriter wraps w only when preempted is a real flag (a nil
// *atomic.Bool means this request was granted without tiered-queue
// bookkeeping, e.g. a test using a bare Effects fake — pass w through
// untouched rather than risk a nil-pointer Load()). replayWanted is passed
// through as-is (nil is a valid, common value — see the type doc).
func newPreemptResponseWriter(w http.ResponseWriter, preempted *atomic.Bool, replayWanted *atomic.Bool) http.ResponseWriter {
	if preempted == nil {
		return w
	}
	return &preemptResponseWriter{ResponseWriter: w, preempted: preempted, replayWanted: replayWanted}
}

// writePreemptGiveUp writes the canonical "llama-swap gave up on this request"
// response: 503 + X-LlamaSwap-Preempted + Retry-After. It is the single place
// that shape is defined, so the two stages that can give up on a request agree
// byte for byte:
//
//   - the SERVE stage, where preemptResponseWriter rewrites a preemption
//     victim's first WriteHeader (below);
//   - the PARK stage, where base.go ServeHTTP abandons a request that has been
//     waiting for a slot past parkGiveUpBudget (llama-cm incident
//     2026-08-18-cq27-background-admission-park-bare-502-retry-storm).
//
// Callers must only invoke this on a response that has NOT committed a status
// line yet - writing it over a committed response (e.g. one already emitting
// pingWriter keepalives) would be ignored by net/http and log a superfluous
// WriteHeader warning.
func writePreemptGiveUp(w http.ResponseWriter) {
	w.Header().Set("X-LlamaSwap-Preempted", "1")
	w.Header().Set("Retry-After", "1")
	w.WriteHeader(http.StatusServiceUnavailable)
}

func (p *preemptResponseWriter) WriteHeader(code int) {
	if p.wroteHeader {
		p.ResponseWriter.WriteHeader(code)
		return
	}
	p.wroteHeader = true
	if p.preempted.Load() {
		if p.replayWanted != nil {
			p.replayWanted.Store(true)
			return
		}
		writePreemptGiveUp(p.ResponseWriter)
		return
	}
	p.ResponseWriter.WriteHeader(code)
}

func (p *preemptResponseWriter) Write(b []byte) (int, error) {
	if !p.wroteHeader {
		p.WriteHeader(http.StatusOK)
	}
	if p.replayWanted != nil && p.replayWanted.Load() {
		// The header above was swallowed for a replay (see WriteHeader) —
		// discard any trailing body the upstream still tries to flush (e.g.
		// a reverse proxy writing an error body right after WriteHeader) so
		// this attempt truly reaches the client as nothing at all.
		return len(b), nil
	}
	return p.ResponseWriter.Write(b)
}

// Flush forwards to the wrapped writer's Flusher when present. http.ResponseWriter
// method promotion does not cover optional interfaces like http.Flusher, so this
// must be implemented explicitly or SSE/streaming responses through a preempted
// request would silently stop flushing.
func (p *preemptResponseWriter) Flush() {
	if f, ok := p.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards to the wrapped writer's http.Hijacker when present, same
// rationale as Flush: method promotion does not cover optional interfaces, so
// without this a preempted request wrapping a hijackable connection (e.g. a
// WebSocket upgrade) would silently lose hijack support and every caller
// downstream would see http.ErrNotSupported instead of the real behavior.
func (p *preemptResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := p.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

// ReadFrom forwards to the wrapped writer's io.ReaderFrom when present (the
// sendfile/splice fast path net/http's reverse proxy and file servers use).
// Falls back to a plain io.Copy through Write when the wrapped writer does
// not implement it, so callers that type-assert io.ReaderFrom still get
// correct (if slower) behavior instead of a broken fast path.
func (p *preemptResponseWriter) ReadFrom(r io.Reader) (int64, error) {
	if rf, ok := p.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	// io.Copy(p, r) would recurse into this very method (p also implements
	// io.ReaderFrom) - wrap p behind a plain io.Writer so io.Copy takes its
	// ordinary Write loop instead.
	return io.Copy(struct{ io.Writer }{p}, r)
}
