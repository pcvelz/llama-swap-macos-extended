package router

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/logmon"
)

// The two tests that matter run over a REAL TCP connection, because the thing
// under test is a kernel-level property: whether a write to a peer that has
// stopped draining its socket eventually blocks. A recorder cannot reproduce
// that, and a test that fakes it would prove nothing about production.
//
// The frozen client is simulated the way SIGSTOP presents on the wire: the
// client connects, reads the first bytes, then stops calling Read while keeping
// the connection open. With a small receive buffer the window closes within a
// few kilobytes and the server's writes start blocking.

// stallTestServer wires a pingWriter + peerStallGuard over a real
// httptest.Server and reports whether the request context was cancelled.
type stallTestServer struct {
	srv *httptest.Server
	// cancelled closes when the guard cancelled the handler's context.
	cancelled chan struct{}
	// writeErr is the error the last body write returned, if any.
	writeErr atomic.Pointer[error]
	// served closes when the handler returns.
	served chan struct{}
}

// newStallTestServer starts a server whose handler streams `chunk` repeatedly
// until its context is cancelled, guarded by a peerStallGuard with the given
// budget.
func newStallTestServer(t *testing.T, budget time.Duration, chunk []byte) *stallTestServer {
	t.Helper()
	logger := logmon.NewWriter(io.Discard)

	st := &stallTestServer{
		cancelled: make(chan struct{}),
		served:    make(chan struct{}),
	}

	st.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(st.served)

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		// slotTimeout 0: these tests isolate the peer verdict. The
		// no-forward-progress verdict has its own tests in slotstall_test.go.
		guard := newPeerStallGuard(logger, "stall-model", budget, 0)
		guard.setCancel(cancel)
		pw := newPingWriter(logger, "stall-model", w, true, guard)
		// waitLoop, not just stop: the ping goroutine read the package-level
		// pacing vars at startup, and a neighbouring test in this package
		// writes them. Joining it here is the happens-before edge that keeps
		// -race honest.
		defer func() { pw.stop(); pw.waitLoop() }()

		go func() {
			<-ctx.Done()
			close(st.cancelled)
		}()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		pw.Flush()

		// Stream until the peer stops accepting bytes. A live client keeps this
		// loop going forever, which is exactly the "slow but alive" case: it
		// must NOT be reaped.
		for ctx.Err() == nil {
			if _, err := pw.Write(chunk); err != nil {
				st.writeErr.Store(&err)
				return
			}
			pw.Flush()
		}
	}))
	t.Cleanup(st.srv.Close)
	return st
}

// dialAndReadOnce opens a raw connection, sends the request, reads at least one
// byte of the response, and returns the connection WITHOUT reading further.
// Closing is left to the caller so the socket stays open (a closed socket is
// the case that already works today and is not what this tests).
func dialAndReadOnce(t *testing.T, srv *httptest.Server, rcvBuf int) net.Conn {
	t.Helper()

	addr := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	// A small receive buffer closes the window after a few KB instead of after
	// the default hundreds of KB, so the stall shows up in test time rather
	// than in minutes of streaming.
	if tcp, ok := conn.(*net.TCPConn); ok {
		if err := tcp.SetReadBuffer(rcvBuf); err != nil {
			t.Fatalf("SetReadBuffer: %v", err)
		}
	}

	if _, err := fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatalf("write request: %v", err)
	}

	one := make([]byte, 1)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(one); err != nil {
		t.Fatalf("read first byte: %v", err)
	}
	conn.SetReadDeadline(time.Time{})
	return conn
}

// TestPeerStall_FrozenPeerReclaimsSlot is the reproduction: a client that
// accepts the response, reads the first bytes, then stops reading (SIGSTOP)
// must have its request cancelled - which is what returns the serving slot to
// the scheduler - within the configured budget.
func TestPeerStall_FrozenPeerReclaimsSlot(t *testing.T) {
	const budget = 2 * time.Second

	// 4 KB chunks fill a small receive window quickly.
	st := newStallTestServer(t, budget, []byte(strings.Repeat("x", 4096)))
	conn := dialAndReadOnce(t, st.srv, 2048)
	defer conn.Close()

	// From here the client is "frozen": it never reads again, and it never
	// closes. Nothing in llama-swap before this change ends such a request.
	select {
	case <-st.cancelled:
	case <-time.After(budget + 15*time.Second):
		t.Fatal("frozen peer was not reclaimed: request context never cancelled")
	}

	select {
	case <-st.served:
	case <-time.After(10 * time.Second):
		t.Fatal("handler did not return after the reclaim")
	}
}

// TestPeerStall_SlowButAliveReaderNotReaped is the guard on the guard: a client
// that drains slowly - the population llama-swap exists to hold, not refuse -
// must survive well past the budget. If this ever goes red, the feature has
// become a request timeout and must be reverted, not tuned.
func TestPeerStall_SlowButAliveReaderNotReaped(t *testing.T) {
	const budget = 500 * time.Millisecond

	st := newStallTestServer(t, budget, []byte(strings.Repeat("y", 256)))
	conn := dialAndReadOnce(t, st.srv, 2048)
	defer conn.Close()

	// Drain a little, slowly, for many multiples of the budget. Each read
	// reopens the window, so no single server write ever blocks for a whole
	// budget even though the client is far slower than the server.
	stopDraining := make(chan struct{})
	var drainErr atomic.Pointer[error]
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 128)
		for {
			select {
			case <-stopDraining:
				return
			default:
			}
			conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			if _, err := conn.Read(buf); err != nil {
				drainErr.Store(&err)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	select {
	case <-st.cancelled:
		t.Fatal("a slow but alive reader was reaped; the guard has become a request timeout")
	case <-time.After(10 * budget):
	}

	close(stopDraining)
	wg.Wait()

	if p := drainErr.Load(); p != nil {
		t.Fatalf("slow reader saw an error while the server was supposed to keep streaming: %v", *p)
	}
	if p := st.writeErr.Load(); p != nil {
		t.Fatalf("server write failed against a live reader: %v", *p)
	}
}

// TestPeerStall_DisabledLeavesFrozenPeerAlone pins the config contract: with
// the guard off, the pre-feature behaviour is restored exactly - the frozen
// peer keeps its slot. This is what makes the feature safely revertible from
// yaml alone.
func TestPeerStall_DisabledLeavesFrozenPeerAlone(t *testing.T) {
	// budget 0 == disabled (config.PeerStallConfig.StallTimeout returns 0 for
	// both enabled:false and timeoutSeconds:0).
	st := newStallTestServer(t, 0, []byte(strings.Repeat("z", 4096)))
	conn := dialAndReadOnce(t, st.srv, 2048)
	defer conn.Close()

	select {
	case <-st.cancelled:
		t.Fatal("guard cancelled with a zero budget; disabled must mean untouched")
	case <-time.After(3 * time.Second):
	}
}

// blockingWriter is a fake ResponseWriter used for the unit-level checks below,
// where a real socket would only add nondeterminism. It cannot carry a write
// deadline, which is itself the case being pinned in the fail-open test.
type blockingWriter struct {
	hdr     http.Header
	release chan struct{}
}

func (b *blockingWriter) Header() http.Header { return b.hdr }
func (b *blockingWriter) WriteHeader(int)     {}
func (b *blockingWriter) Write(p []byte) (int, error) {
	<-b.release
	return len(p), nil
}

// TestPeerStallGuard_FailsOpenWithoutDeadlineSupport: a writer chain that
// cannot carry a deadline must behave exactly as it did before the guard
// existed - the write proceeds, and no reclaim is ever declared, no matter how
// long it takes. Failing closed here would turn every unsupported chain into a
// refusal.
func TestPeerStallGuard_FailsOpenWithoutDeadlineSupport(t *testing.T) {
	logger := logmon.NewWriter(io.Discard)
	guard := newPeerStallGuard(logger, "m", 50*time.Millisecond, 0)

	cancelled := false
	guard.setCancel(func() { cancelled = true })

	w := &blockingWriter{hdr: make(http.Header), release: make(chan struct{})}
	time.AfterFunc(300*time.Millisecond, func() { close(w.release) })

	n, err := guard.write(w, func() (int, error) { return w.Write([]byte("hello")) })
	if err != nil || n != 5 {
		t.Fatalf("write = (%d, %v), want (5, nil)", n, err)
	}
	if guard.stalled() || cancelled {
		t.Fatal("guard reclaimed on a chain with no deadline support; it must fail open")
	}
}

// TestPeerStallGuard_ReclaimsOnce pins the latch: however many writers pile up
// behind one dead connection, the reclaim logs and cancels exactly once.
func TestPeerStallGuard_ReclaimsOnce(t *testing.T) {
	logger := logmon.NewWriter(io.Discard)
	guard := newPeerStallGuard(logger, "m", time.Second, 0)

	var cancels atomic.Int32
	guard.setCancel(func() { cancels.Add(1) })

	guard.reclaim(time.Second, nil)
	guard.reclaim(time.Second, nil)
	guard.reclaim(time.Second, nil)

	if got := cancels.Load(); got != 1 {
		t.Fatalf("cancel called %d times, want 1", got)
	}
	if !guard.stalled() {
		t.Fatal("guard should report stalled after a reclaim")
	}
}

// TestIsWriteDeadlineErr separates the peer-stopped-reading verdict from an
// ordinary disconnect. Both end the request; only the first is this feature's
// business, and mislabelling a reset as a stall would put a misleading line in
// the log an operator greps.
func TestIsWriteDeadlineErr(t *testing.T) {
	if !isWriteDeadlineErr(os.ErrDeadlineExceeded) {
		t.Error("os.ErrDeadlineExceeded should be a write-deadline error")
	}
	if !isWriteDeadlineErr(fmt.Errorf("wrapped: %w", os.ErrDeadlineExceeded)) {
		t.Error("a wrapped deadline error should still be recognised")
	}
	if isWriteDeadlineErr(nil) {
		t.Error("nil is not a deadline error")
	}
	if isWriteDeadlineErr(errors.New("connection reset by peer")) {
		t.Error("an ordinary disconnect is not a stall")
	}
}
