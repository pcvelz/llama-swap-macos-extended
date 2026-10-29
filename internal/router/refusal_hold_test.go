package router

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// A real engine does not always let go of a request the moment its client is
// cut. Splash (bounded queue of one) keeps a cancelled request until it next
// looks at the connection, and refuses every arrival in the meantime with a 503
// and a Retry-After. A priority arrival that boots a streaming victim therefore
// meets that refusal, and so does the victim's replay.
//
// These tests play that engine through the real chain (real server, scheduler,
// ProcessCommand and reverse proxy), the way the afternoon's failure ran.

// splashRefusalBody is the shape of the engine's overload answer.
const splashRefusalBody = `{"type":"error","error":{"type":"overloaded_error","message":"request queue is full"}}`

// lingeringEngine admits one request at a time. A request it admitted and that
// was then cancelled stays in its queue for lag.
type lingeringEngine struct {
	lag time.Duration

	mu       sync.Mutex
	occupied bool
	admitted []string // roles, in the order the engine took them
	refused  []string // roles, in the order the engine turned them away
	victims  int
	timers   []*time.Timer
}

func newLingeringEngine(t *testing.T, lag time.Duration) *lingeringEngine {
	t.Helper()
	e := &lingeringEngine{lag: lag}
	t.Cleanup(func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		for _, tm := range e.timers {
			tm.Stop()
		}
	})
	return e
}

func (e *lingeringEngine) admit(role string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.occupied {
		e.refused = append(e.refused, role)
		return false
	}
	e.occupied = true
	e.admitted = append(e.admitted, role)
	return true
}

func (e *lingeringEngine) release() {
	e.mu.Lock()
	e.occupied = false
	e.mu.Unlock()
}

// releaseAfterLag is the engine noticing, late, that the client of a cancelled
// request is gone.
func (e *lingeringEngine) releaseAfterLag() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.timers = append(e.timers, time.AfterFunc(e.lag, e.release))
}

func (e *lingeringEngine) occupy() {
	e.mu.Lock()
	e.occupied = true
	e.mu.Unlock()
}

func (e *lingeringEngine) roles() (admitted, refused []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.admitted...), append([]string(nil), e.refused...)
}

func (e *lingeringEngine) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		flush := func() {
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		role := r.Header.Get("X-Role")
		if !e.admit(role) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, splashRefusalBody)
			return
		}
		switch role {
		case "victim":
			e.mu.Lock()
			e.victims++
			attempt := e.victims
			e.mu.Unlock()
			// Splash announces the message and then reports its prefill progress
			// with pings, long before the first token.
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, msgStartFrame(map[int]string{1: "msg_A", 2: "msg_B"}[attempt])+frPing+frPing)
			flush()
			if attempt == 1 {
				<-r.Context().Done() // booted: the proxy cancels this request
				e.releaseAfterLag()
				return
			}
			fmt.Fprint(w, blockStartFrame+deltaFrame("hello")+blockStopFrame+msgDeltaFrame+msgStopFrame)
			flush()
			e.release()
		default:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, "rival-ok")
			e.release()
		}
	})
}

// newLingerChain wires the real router, process and reverse proxy in front of
// handler. The returned proxyChain only fills the fields post() and the logs need.
func newLingerChain(t *testing.T, handler http.Handler) *proxyChain {
	t.Helper()
	shortenPingCadence(t, time.Hour) // only upstream bytes count
	c := &proxyChain{logs: &syncBuf{}}
	upstream := httptest.NewServer(handler)
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

// fastRefusalRetry makes the retry cadence and budget small enough for a test.
func fastRefusalRetry(t *testing.T, cadence, budget time.Duration) {
	t.Helper()
	oldDef, oldMin, oldMax, oldBudget := refusalRetryDefault, refusalRetryMin, refusalRetryMax, refusalRetryBudget
	refusalRetryDefault, refusalRetryMin, refusalRetryMax, refusalRetryBudget = cadence, cadence, cadence, budget
	t.Cleanup(func() {
		refusalRetryDefault, refusalRetryMin, refusalRetryMax, refusalRetryBudget = oldDef, oldMin, oldMax, oldBudget
	})
}

// lingerAttempt1 is what the engine sends the victim on admission: the message,
// then two progress pings.
var lingerAttempt1 = msgStartFrame("msg_A") + frPing + frPing

// sseParsable reports a line of body that no SSE parser could read as a field:
// every non-blank line must be a comment, an event line or a data line.
func sseParsable(body string) (badLine string, ok bool) {
	for _, line := range strings.Split(body, "\n") {
		if line == "" || strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event: ") || strings.HasPrefix(line, "data: ") {
			continue
		}
		return line, false
	}
	return "", true
}

// The afternoon's failure. A default-tier session turn is streaming from an
// engine that announced the message; a priority request arrives and boots it.
// The engine keeps the cancelled request for a while, so it refuses the arrival
// and then the victim's replay.
//
// Wanted: the arrival is held by the proxy (it cut the victim, so the refusal is
// the victim's shadow) and served once the engine lets go, the victim's replay
// queues behind the arrival and then continues the message, and no refusal
// reaches either client.
func TestRealProxyChain_CutRequestLingersInTheEngine_BothSurvive(t *testing.T) {
	fastRefusalRetry(t, 15*time.Millisecond, 30*time.Second)
	eng := newLingeringEngine(t, 400*time.Millisecond)
	c := newLingerChain(t, eng.handler())

	victim, rival := newSyncRecorder(), newSyncRecorder()
	victimDone := c.post(t, "/v1/messages", "victim", "default", `{"model":"alpha","stream":true}`, victim)
	waitForBody(t, victim, lingerAttempt1)
	rivalDone := c.post(t, "/v1/messages", "rival", "priority", `{"model":"alpha"}`, rival)
	awaitDone(t, "victim", victimDone, c.logs)
	awaitDone(t, "rival", rivalDone, c.logs)

	if rival.statusCode() != http.StatusOK || rival.bodyString() != "rival-ok" {
		t.Fatalf("the arrival got status=%d body=%q, want 200 rival-ok: the refusal was the cut request's shadow and must not reach it\nlogs:\n%s",
			rival.statusCode(), rival.bodyString(), c.logs.String())
	}
	attempt2 := frPing + frPing + blockStartFrame + deltaFrame("hello") + blockStopFrame + msgDeltaFrame + msgStopFrame
	want := lingerAttempt1 + attempt2
	if got := victim.bodyString(); got != want {
		t.Fatalf("the victim's stream:\n got %q\nwant %q\nlogs:\n%s", got, want, c.logs.String())
	}
	if bad, ok := sseParsable(victim.bodyString()); !ok {
		t.Fatalf("a line that is not an SSE field reached the victim: %q", bad)
	}

	admitted, refused := eng.roles()
	if got := strings.Join(admitted, ","); got != "victim,rival,victim" {
		t.Fatalf("the engine took %q, want victim,rival,victim: the replay must wait behind the arrival that booted it", got)
	}
	if len(refused) == 0 {
		t.Fatal("the engine never refused anyone: the scenario did not reproduce the lingering request")
	}
	for _, role := range refused {
		if role != "rival" {
			t.Fatalf("the engine refused a %q request; only the arrival may meet the lingering one (replay raced it): %v", role, refused)
		}
	}
	for _, line := range []string{"refusal-hold: model=alpha status=503", "refusal-hold: model=alpha the child took the request after"} {
		if !strings.Contains(c.logs.String(), line) {
			t.Fatalf("no %q line in the log:\n%s", line, c.logs.String())
		}
	}
	t.Logf("proxy log:\n%s", c.logs.String())
}

// An engine that never lets go: the hold is bounded, and what the clients are
// told stays well formed. The arrival (nothing sent yet) gets the engine's own
// refusal; the victim's stream (status line long committed) ends with an SSE
// error event, never with the refusal's JSON as a line of the stream.
func TestRealProxyChain_EngineNeverLetsGo_RefusalNeverBecomesAStreamLine(t *testing.T) {
	fastRefusalRetry(t, 15*time.Millisecond, 300*time.Millisecond)
	eng := newLingeringEngine(t, time.Hour)
	c := newLingerChain(t, eng.handler())

	victim, rival := newSyncRecorder(), newSyncRecorder()
	victimDone := c.post(t, "/v1/messages", "victim", "default", `{"model":"alpha","stream":true}`, victim)
	waitForBody(t, victim, lingerAttempt1)
	rivalDone := c.post(t, "/v1/messages", "rival", "priority", `{"model":"alpha"}`, rival)
	awaitDone(t, "rival", rivalDone, c.logs)
	awaitDone(t, "victim", victimDone, c.logs)

	if rival.statusCode() != http.StatusServiceUnavailable || rival.bodyString() != splashRefusalBody {
		t.Fatalf("the arrival got status=%d body=%q, want the engine's own 503 once the hold is spent\nlogs:\n%s",
			rival.statusCode(), rival.bodyString(), c.logs.String())
	}
	body := victim.bodyString()
	if !strings.HasPrefix(body, lingerAttempt1) || !strings.Contains(body, "event: error\ndata: ") || !strings.HasSuffix(body, "\n\n") {
		t.Fatalf("the victim's stream should be its admission frames and then an SSE error event, got %q\nlogs:\n%s", body, c.logs.String())
	}
	if bad, ok := sseParsable(body); !ok {
		t.Fatalf("a line that is not an SSE field reached the victim: %q\nstream: %q", bad, body)
	}
	if strings.Contains(body, splashRefusalBody+"\n") || strings.Contains(body, "\n"+splashRefusalBody) {
		t.Fatalf("the engine's raw refusal was written into the stream: %q", body)
	}
}

// postCancellable is post() with a client that can hang up.
func (c *proxyChain) postCancellable(t *testing.T, path, role, tier, body string, sink *syncRecorder) (hangUp context.CancelFunc, done chan struct{}) {
	t.Helper()
	ctx, hangUp := context.WithCancel(context.Background())
	t.Cleanup(hangUp)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.front.URL+path, strings.NewReader(body))
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
	return hangUp, done
}

// A request that is itself being held can be booted by a higher tier. It must
// not be lost: its cancel comes out of the hold the way any booted request's
// does, and it is replayed behind the arrival.
//
// The cut here is a client that hangs up on a streaming request (not a
// preemption), which the engine also keeps for a while.
func TestRealProxyChain_BootedWhileHeld_IsReplayedNotLost(t *testing.T) {
	fastRefusalRetry(t, 15*time.Millisecond, 30*time.Second)
	eng := newLingeringEngine(t, 600*time.Millisecond)
	c := newLingerChain(t, eng.handler())

	gone := newSyncRecorder()
	hangUp, goneDone := c.postCancellable(t, "/v1/messages", "victim", "default", `{"model":"alpha","stream":true}`, gone)
	waitForBody(t, gone, lingerAttempt1)
	hangUp()
	awaitDone(t, "the client that hung up", goneDone, c.logs)

	// Default tier, while the engine still holds the request the client left.
	d := newSyncRecorder()
	dDone := c.post(t, "/v1/messages", "d", "default", `{"model":"alpha"}`, d)
	waitFor(t, "the hold on the default-tier request to start", func() bool {
		return strings.Contains(c.logs.String(), "refusal-hold: model=alpha status=503")
	})

	// A higher tier arrives and boots it out of the hold.
	p := newSyncRecorder()
	pDone := c.post(t, "/v1/messages", "p", "priority", `{"model":"alpha"}`, p)
	awaitDone(t, "the priority request", pDone, c.logs)
	awaitDone(t, "the default-tier request", dDone, c.logs)

	for name, rec := range map[string]*syncRecorder{"priority": p, "default": d} {
		if rec.statusCode() != http.StatusOK || rec.bodyString() != "rival-ok" {
			t.Fatalf("%s request: status=%d body=%q, want 200 rival-ok\nlogs:\n%s", name, rec.statusCode(), rec.bodyString(), c.logs.String())
		}
	}
	admitted, refused := eng.roles()
	if got := strings.Join(admitted, ","); got != "victim,p,d" {
		t.Fatalf("the engine took %q, want victim,p,d: the booted request waits behind the arrival and is then served\nlogs:\n%s", got, c.logs.String())
	}
	var sawD, sawP bool
	for _, role := range refused {
		sawD = sawD || role == "d"
		sawP = sawP || role == "p"
	}
	if !sawD || !sawP {
		t.Fatalf("refused roles %v, want both the held request and the arrival to have met the lingering one", refused)
	}
	if !strings.Contains(c.logs.String(), "preempt-replay: tier=default model=alpha attempt=1") {
		t.Fatalf("the booted request was not replayed:\n%s", c.logs.String())
	}
}

// A refusal with no cut request behind it is the engine's real answer, not a
// shadow: it is delivered at once and the request is not retried.
func TestRealProxyChain_RefusalWithoutACutIsDeliveredAtOnce(t *testing.T) {
	fastRefusalRetry(t, 15*time.Millisecond, 30*time.Second)
	eng := newLingeringEngine(t, time.Hour)
	eng.occupy() // someone else's request
	c := newLingerChain(t, eng.handler())

	rival := newSyncRecorder()
	done := c.post(t, "/v1/messages", "rival", "default", `{"model":"alpha"}`, rival)
	awaitDone(t, "request", done, c.logs)

	if rival.statusCode() != http.StatusServiceUnavailable || rival.bodyString() != splashRefusalBody {
		t.Fatalf("status=%d body=%q, want the engine's 503 as it came", rival.statusCode(), rival.bodyString())
	}
	if _, refused := eng.roles(); len(refused) != 1 {
		t.Fatalf("the engine was asked %d times, want exactly once: nothing was cut, so nothing is retried", len(refused))
	}
	if strings.Contains(c.logs.String(), "refusal-hold:") {
		t.Fatalf("a refusal-hold line without a cut:\n%s", c.logs.String())
	}
}
