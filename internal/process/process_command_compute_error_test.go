package process

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// newComputeErrorTestProcess builds a ProcessCommand whose proxy logger
// writes to a capturable syncBuffer, so tests can assert on the
// "child error-state: ... restarting child" log line rather than just the
// resulting state transition. Needs a real child process (simple-responder)
// so run()'s Stop() path has something real to tear down; requests are
// proxied to the mock upstreamURL, matching the TTL tests' pattern.
func newComputeErrorTestProcess(t *testing.T, upstreamURL string) (*ProcessCommand, *syncBuffer) {
	t.Helper()
	skipIfNoSimpleResponder(t)
	cmd, _ := simpleResponderCmd(t, "-silent")

	logBuf := &syncBuffer{}
	proxyLogger := logmon.NewWriter(logBuf)
	procLogger := logmon.NewWriter(io.Discard)
	p, err := New(context.Background(), t.Name(), config.ModelConfig{
		Cmd:                cmd,
		Proxy:              upstreamURL,
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 10,
	}, procLogger, proxyLogger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p, logBuf
}

// stateTransitionRecorder mutex-guards the transitions slice: event delivery
// is asynchronous (see internal/event's dispatcher), so a handler goroutine
// can still be appending while the test reads without this.
type stateTransitionRecorder struct {
	mu          sync.Mutex
	transitions []swaputil.ProcessStateChangeEvent
}

func (r *stateTransitionRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.transitions)
}

func (r *stateTransitionRecorder) countReadyToStopping() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.transitions {
		if e.OldState == string(StateReady) && e.NewState == string(StateStopping) {
			n++
		}
	}
	return n
}

func (r *stateTransitionRecorder) snapshot() []swaputil.ProcessStateChangeEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]swaputil.ProcessStateChangeEvent, len(r.transitions))
	copy(out, r.transitions)
	return out
}

// subscribeStateTransitions records every ProcessStateChangeEvent for this
// test's process, so a test can assert exactly how many times a restart
// (Ready -> Stopping -> Stopped) actually fired.
func subscribeStateTransitions(t *testing.T, name string) *stateTransitionRecorder {
	t.Helper()
	r := &stateTransitionRecorder{}
	cancel := event.On(func(e swaputil.ProcessStateChangeEvent) {
		if e.ProcessName != name {
			return
		}
		r.mu.Lock()
		r.transitions = append(r.transitions, e)
		r.mu.Unlock()
	})
	t.Cleanup(cancel)
	return r
}

// computeErrorResponder returns an httptest handler that always answers /health
// with 200 and drains statuses/bodies from queue in order for every other
// request (repeating the last entry once the queue is exhausted).
func computeErrorResponder(queue []struct {
	status int
	body   string
}) http.HandlerFunc {
	var n int
	var mu sync.Mutex
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		mu.Lock()
		idx := n
		if idx >= len(queue) {
			idx = len(queue) - 1
		}
		n++
		mu.Unlock()
		entry := queue[idx]
		w.WriteHeader(entry.status)
		_, _ = w.Write([]byte(entry.body))
	}
}

// TestProcessCommand_ComputeErrorRestartsAfterThreeConsecutive covers (a):
// three consecutive upstream 500 "Compute error" responses (the Metal-OOM
// error-state signature witnessed in production) must trigger exactly one
// restart of the child, via the same Stop() path the TTL/unload logic uses.
func TestProcessCommand_ComputeErrorRestartsAfterThreeConsecutive(t *testing.T) {
	origThreshold := computeErrorRestartThreshold
	computeErrorRestartThreshold = 3
	t.Cleanup(func() { computeErrorRestartThreshold = origThreshold })

	mock := httptest.NewServer(computeErrorResponder([]struct {
		status int
		body   string
	}{
		{http.StatusInternalServerError, `{"error":"Compute error"}`},
		{http.StatusInternalServerError, `{"error":"Compute error"}`},
		{http.StatusInternalServerError, `{"error":"Compute error"}`},
	}))
	t.Cleanup(mock.Close)

	p, logBuf := newComputeErrorTestProcess(t, mock.URL)
	t.Cleanup(func() {
		if p.State() != StateStopped && p.State() != StateShutdown {
			p.Stop(testStopTimeout)
		}
	})

	transitions := subscribeStateTransitions(t, t.Name())

	runErr := runAsync(t, p)

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest("POST", "/v1/messages", nil)
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("request %d: expected 500, got %d", i, rr.Code)
		}
		if !strings.Contains(rr.Body.String(), "Compute error") {
			t.Fatalf("request %d: expected client to still receive the full body, got %q", i, rr.Body.String())
		}
	}

	// The restart runs asynchronously (see restartOnComputeError) so the
	// response above returns immediately; wait for the child to actually
	// stop.
	waitForState(t, p, StateStopped)

	select {
	case <-runErr:
	case <-time.After(testReturnTimeout):
		t.Fatal("Run() did not return after compute-error-triggered stop")
	}

	// Event delivery is asynchronous; give the dispatcher a moment to catch
	// up to the Stopped state we already observed via polling above.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && transitions.count() < 4 {
		time.Sleep(testPollInterval)
	}

	if got := transitions.countReadyToStopping(); got != 1 {
		t.Errorf("expected exactly 1 Ready->Stopping transition (one restart), got %d: %v", got, transitions.snapshot())
	}

	logs := logBuf.String()
	if !strings.Contains(logs, "child error-state") || !strings.Contains(logs, "restarting child") {
		t.Errorf("expected a child error-state restart log line, got: %q", logs)
	}
	if !strings.Contains(logs, "consecutive_compute_errors=3") {
		t.Errorf("expected log line to report consecutive_compute_errors=3, got: %q", logs)
	}
}

// TestProcessCommand_ComputeErrorResetsOnSuccess covers (b): two consecutive
// Compute errors followed by a successful response must reset the streak, so
// two MORE Compute errors afterwards (4 total, never 3 in a row) do not
// trigger a restart.
func TestProcessCommand_ComputeErrorResetsOnSuccess(t *testing.T) {
	origThreshold := computeErrorRestartThreshold
	computeErrorRestartThreshold = 3
	t.Cleanup(func() { computeErrorRestartThreshold = origThreshold })

	mock := httptest.NewServer(computeErrorResponder([]struct {
		status int
		body   string
	}{
		{http.StatusInternalServerError, `{"error":"Compute error"}`},
		{http.StatusInternalServerError, `{"error":"Compute error"}`},
		{http.StatusOK, `{"ok":true}`},
		{http.StatusInternalServerError, `{"error":"Compute error"}`},
		{http.StatusInternalServerError, `{"error":"Compute error"}`},
	}))
	t.Cleanup(mock.Close)

	p, _ := newComputeErrorTestProcess(t, mock.URL)
	t.Cleanup(func() {
		if p.State() != StateStopped && p.State() != StateShutdown {
			p.Stop(testStopTimeout)
		}
	})

	_ = runAsync(t, p)

	wantCodes := []int{http.StatusInternalServerError, http.StatusInternalServerError, http.StatusOK, http.StatusInternalServerError, http.StatusInternalServerError}
	for i, want := range wantCodes {
		req := httptest.NewRequest("POST", "/v1/messages", nil)
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)
		if rr.Code != want {
			t.Fatalf("request %d: expected %d, got %d", i, want, rr.Code)
		}
	}

	// Give any (incorrectly) triggered async restart a moment to happen, then
	// assert the process never left StateReady.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if got := p.State(); got != StateReady {
			t.Fatalf("process left StateReady (got %s) after a reset streak that never reached the threshold", got)
		}
		time.Sleep(testPollInterval)
	}
}

// TestProcessCommand_ComputeErrorPlainFailureDoesNotCount covers (c): plain
// 500 responses without the "Compute error" signature must never trigger a
// restart, however many arrive in a row.
func TestProcessCommand_ComputeErrorPlainFailureDoesNotCount(t *testing.T) {
	origThreshold := computeErrorRestartThreshold
	computeErrorRestartThreshold = 3
	t.Cleanup(func() { computeErrorRestartThreshold = origThreshold })

	mock := httptest.NewServer(computeErrorResponder([]struct {
		status int
		body   string
	}{
		{http.StatusInternalServerError, `{"error":"some other failure"}`},
		{http.StatusInternalServerError, `{"error":"some other failure"}`},
		{http.StatusInternalServerError, `{"error":"some other failure"}`},
	}))
	t.Cleanup(mock.Close)

	p, _ := newComputeErrorTestProcess(t, mock.URL)
	t.Cleanup(func() {
		if p.State() != StateStopped && p.State() != StateShutdown {
			p.Stop(testStopTimeout)
		}
	})

	_ = runAsync(t, p)

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest("POST", "/v1/messages", nil)
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("request %d: expected 500, got %d", i, rr.Code)
		}
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if got := p.State(); got != StateReady {
			t.Fatalf("process left StateReady (got %s) after plain 500s with no Compute error signature", got)
		}
		time.Sleep(testPollInterval)
	}
}
