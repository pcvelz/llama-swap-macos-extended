package router

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// The same replay scenario as admission_replay_test.go, through the real chain:
// a client on a real HTTP server, the real scheduler, the real ProcessCommand
// whose httputil.ReverseProxy talks to a fake Anthropic upstream. Nothing here
// writes the proxy's terminal error event by hand: when the upstream request is
// cancelled mid-stream the reverse proxy panics with http.ErrAbortHandler, the
// process recovers it and ends the SSE response with that event, and the test
// shows what the client is left holding. The fake-process tests cannot show it:
// they only write the event because a comment says the proxy does.

// childCmd returns a command line for a throwaway child (the simple-responder
// test helper) whose own port nobody uses: the ProcessCommand only needs a live
// process and a health endpoint, and its proxy URL points at the fake upstream.
func childCmd(t *testing.T) string {
	t.Helper()
	name := fmt.Sprintf("simple-responder_%s_%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		name = "simple-responder.exe"
	}
	path := filepath.Join("..", "..", "build", name)
	if _, err := os.Stat(path); err != nil {
		t.Skipf("simple-responder not found at %s, run `make simple-responder`", path)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return fmt.Sprintf("%s -port %d -silent", filepath.ToSlash(path), port)
}

type proxyChain struct {
	front      *httptest.Server
	logs       *syncBuf
	victimRuns atomic.Int32
	rivalRuns  atomic.Int32
}

// chainShape is what the fake upstream's victim sends before it waits to be
// booted.
type chainShape int

const (
	// shapeAdmission: ping, message_start, ping (an engine that speaks the
	// Anthropic stream natively, admitting the request).
	shapeAdmission chainShape = iota
	// shapeContent: shapeAdmission plus the first content block.
	shapeContent
	// shapeComments: one SSE comment and nothing else (llama-server before its
	// first token).
	shapeComments
)

// newProxyChain builds the chain. The fake upstream plays the same victim as
// replayScenario: its first frames (per shape), then it waits to be cancelled;
// the replayed attempt (and a victim that is never booted) finishes its message.
func newProxyChain(t *testing.T, shape chainShape) *proxyChain {
	t.Helper()
	shortenPingCadence(t, time.Hour) // only upstream bytes count
	c := &proxyChain{logs: &syncBuf{}}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		flush := func() {
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		switch r.Header.Get("X-Role") {
		case "victim":
			attempt := c.victimRuns.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if shape == shapeComments {
				fmt.Fprint(w, ":\n\n")
				flush()
				if attempt == 1 {
					<-r.Context().Done()
					return
				}
				fmt.Fprint(w, msgStartFrame("msg_B")+blockStartFrame+deltaFrame("hello")+blockStopFrame+msgDeltaFrame+msgStopFrame)
				flush()
				return
			}
			fmt.Fprint(w, frPing+msgStartFrame(map[int32]string{1: "msg_A", 2: "msg_B"}[attempt])+frPing)
			flush()
			if attempt == 1 {
				if shape == shapeContent {
					fmt.Fprint(w, blockStartFrame+deltaFrame("par"))
					flush()
				}
				<-r.Context().Done() // the proxy cancels the upstream request when the victim is booted
				return
			}
			fmt.Fprint(w, blockStartFrame+deltaFrame("hello")+blockStopFrame+msgDeltaFrame+msgStopFrame)
			flush()
		case "rival":
			c.rivalRuns.Add(1)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, "rival-ok")
		}
	}))
	t.Cleanup(upstream.Close)

	sendLoading := true
	mc := config.ModelConfig{
		Cmd:                childCmd(t),
		Proxy:              upstream.URL,
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 10,
		ConcurrencyLimit:   1,
		SendLoadingState:   &sendLoading,
	}
	p, err := process.New(context.Background(), "alpha", mc, logmon.NewWriter(io.Discard), logmon.NewWriter(io.Discard))
	if err != nil {
		t.Fatalf("process.New: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(2 * time.Second) })
	go func() { _ = p.Run(10 * time.Second) }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.WaitReady(ctx); err != nil {
		t.Fatalf("the child never became ready: %v", err)
	}

	conf := config.Config{HealthCheckTimeout: 5, Models: map[string]config.ModelConfig{"alpha": mc}}
	b, err := newBaseRouter("test", conf, map[string]process.Process{"alpha": p}, logmon.NewWriter(c.logs), &stubPlanner{})
	if err != nil {
		t.Fatalf("newBaseRouter: %v", err)
	}
	go b.run()
	t.Cleanup(func() {
		if !b.shuttingDown.Load() {
			_ = b.Shutdown(2 * time.Second)
		}
	})

	// A real server, so the request context carries http.ServerContextKey and
	// httputil.ReverseProxy raises ErrAbortHandler on a cancelled copy exactly
	// as it does in production.
	c.front = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tier := replayTierDefault
		if r.Header.Get("X-Test-Tier") == "priority" {
			tier = tierPriorityRank10
		}
		b.ServeHTTP(w, r.WithContext(swaputil.WithTier(r.Context(), tier)))
	}))
	t.Cleanup(c.front.Close)
	return c
}

// post sends one request through the front server and streams the response
// body into sink until it ends; done closes when it has.
func (c *proxyChain) post(t *testing.T, path, role, tier, body string, sink *syncRecorder) (done chan struct{}) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, c.front.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Role", role)
	req.Header.Set("X-Test-Tier", tier)
	done = make(chan struct{})
	go func() {
		defer close(done)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			fmt.Fprintf(sink, "[client error: %v]", err)
			return
		}
		defer resp.Body.Close()
		sink.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(sink, resp.Body)
	}()
	return done
}

func TestRealProxyChain_AdmissionFrames_ReplayedBeforeContentAbortedAfter(t *testing.T) {
	attempt1 := frPing + msgStartFrame("msg_A") + frPing
	attempt2 := frPing + msgStartFrame("msg_B") + frPing + blockStartFrame + deltaFrame("hello") + blockStopFrame + msgDeltaFrame + msgStopFrame

	run := func(t *testing.T, shape chainShape, until string) (*proxyChain, *syncRecorder) {
		c := newProxyChain(t, shape)
		victim, rival := newSyncRecorder(), newSyncRecorder()
		victimDone := c.post(t, "/v1/messages", "victim", "default", `{"model":"alpha","stream":true}`, victim)
		waitForBody(t, victim, until)
		rivalDone := c.post(t, "/v1/chat/completions", "rival", "priority", `{"model":"alpha"}`, rival)
		awaitDone(t, "victim", victimDone, c.logs)
		awaitDone(t, "rival", rivalDone, c.logs)
		if rival.statusCode() != http.StatusOK || rival.bodyString() != "rival-ok" {
			t.Fatalf("rival status=%d body=%q, want 200 rival-ok\nlogs:\n%s", rival.statusCode(), rival.bodyString(), c.logs.String())
		}
		return c, victim
	}

	t.Run("booted before the first content delta: replayed", func(t *testing.T) {
		c, victim := run(t, shapeAdmission, attempt1)

		want := attempt1 + dropFrame(attempt2, msgStartFrame("msg_B"))
		if got := victim.bodyString(); got != want {
			t.Fatalf("victim stream:\n got %q\nwant %q\nlogs:\n%s", got, want, c.logs.String())
		}
		if got := c.victimRuns.Load(); got != 2 {
			t.Fatalf("the upstream saw %d victim requests, want 2 (the cut attempt and its replay)", got)
		}
		if !strings.Contains(c.logs.String(), "preempt-replay: tier=default model=alpha attempt=1") {
			t.Fatalf("no replay line in the log:\n%s", c.logs.String())
		}
	})

	t.Run("booted after the first content delta: aborted", func(t *testing.T) {
		c, victim := run(t, shapeContent, attempt1+blockStartFrame+deltaFrame("par"))

		want := attempt1 + blockStartFrame + deltaFrame("par") + proxyAbortFrame
		if got := victim.bodyString(); got != want {
			t.Fatalf("victim stream:\n got %q\nwant %q\nlogs:\n%s", got, want, c.logs.String())
		}
		if got := c.victimRuns.Load(); got != 1 {
			t.Fatalf("the upstream saw %d victim requests, want exactly 1: a stream that carried content is never replayed", got)
		}
	})

	// llama-server's shape: SSE comments until the first token. Commit 333cf30
	// meant this victim to be replayed too, but through the real proxy the
	// cancel's terminal error event reached the client first and ended the
	// stream (on the pristine tree: the upstream saw one victim request and the
	// client read ":\n\n\n\nevent: error ... upstream disconnected mid-stream").
	t.Run("a comments-only victim booted before its first token: replayed", func(t *testing.T) {
		c, victim := run(t, shapeComments, ":\n\n")

		want := ":\n\n" + ":\n\n" + msgStartFrame("msg_B") + blockStartFrame + deltaFrame("hello") + blockStopFrame + msgDeltaFrame + msgStopFrame
		if got := victim.bodyString(); got != want {
			t.Fatalf("victim stream:\n got %q\nwant %q\nlogs:\n%s", got, want, c.logs.String())
		}
		if got := c.victimRuns.Load(); got != 2 {
			t.Fatalf("the upstream saw %d victim requests, want 2 (the cut attempt and its replay)", got)
		}
	})
}
