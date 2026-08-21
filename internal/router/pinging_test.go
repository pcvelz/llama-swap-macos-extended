package router

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/logmon"
)

// syncRecorder is a minimal concurrency-safe http.ResponseWriter for the
// pingWriter tests.
//
// WHY it exists: these tests deliberately let the ping goroutine write to the
// response while the test goroutine reads what it wrote. httptest.Recorder
// records into a plain bytes.Buffer with no locking, so every such read is a
// genuine data race — `go test -race` (what `make test-all` and BOTH CI lanes
// run) fails on it, even though `go test` without -race passes. That is exactly
// how it reached main: verified locally without -race, red on Linux and Windows.
//
// This is a harness fix, not a behaviour fix. Production is unaffected:
// pingWriter.mu serializes all access to the real ResponseWriter, and the only
// unsynchronized reader was the test itself.
type syncRecorder struct {
	// hdr is not lock-guarded because callers mutate the returned map directly,
	// exactly as with httptest.Recorder. Safe here: headers are set during test
	// setup, before pingQuietDelay can elapse and start the ping goroutine.
	hdr http.Header

	mu     sync.Mutex
	body   bytes.Buffer
	status int
}

func newSyncRecorder() *syncRecorder {
	return &syncRecorder{hdr: make(http.Header)}
}

func (r *syncRecorder) Header() http.Header { return r.hdr }

func (r *syncRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.Write(p)
}

func (r *syncRecorder) WriteHeader(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = code
}

// bodyString and bodyLen are the only sanctioned way for a test goroutine to
// observe the body: they take the same lock the ping goroutine writes under.
func (r *syncRecorder) bodyString() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.String()
}

func (r *syncRecorder) bodyLen() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.Len()
}

// TestPingWriter_PingsContinueAfterEarlyHeaders reproduces the 2026-07-22
// compact-stall finding (llama-cm incident
// 2026-07-09-compact-stall-cold-cache-swap-churn, follow-up 2026-07-22):
//
// llama-server (httplib) commits "200 + text/event-stream" response headers
// immediately at request accept — BEFORE task scheduling, prefill, or the
// first generated token. pingWriter treats that bare WriteHeader(200) as
// "upstream started" and permanently disarms, so a request that then sits
// silent for minutes (queued task, 163k prefill, decode starvation under a
// concurrent prefill) delivers ZERO bytes of body and is killed by the
// client's ~300s body timeout (witnessed as six metronomic 5m0.06s aborts).
//
// The pinger's contract is to keep byte-less waits alive; headers alone do
// not feed a client's body-timeout watchdog. Expected behaviour: ping events
// keep flowing during body silence that follows early upstream headers.
func TestPingWriter_PingsContinueAfterEarlyHeaders(t *testing.T) {
	if testing.Short() {
		t.Skip("waits >pingQuietDelay of real time")
	}
	logger := logmon.NewWriter(io.Discard)
	w := newSyncRecorder()

	// Shorten the cadence so the test does not sleep 37s of real time;
	// production defaults (20s/15s) are restored on cleanup.
	origQuiet, origInterval := pingQuietDelay, pingInterval
	pingQuietDelay, pingInterval = 100*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { pingQuietDelay, pingInterval = origQuiet, origInterval })

	pw := newPingWriter(logger, "test-model", w, true)
	defer pw.stop()

	// llama-server's httplib sends SSE headers immediately at accept.
	pw.Header().Set("Content-Type", "text/event-stream")
	pw.WriteHeader(http.StatusOK)

	// Then the body goes silent (task queued / prefilling / decode-starved).
	// Wait past pingQuietDelay plus one pingInterval of margin.
	time.Sleep(pingQuietDelay + pingInterval + 2*time.Second)

	body := w.bodyString()
	if !strings.Contains(body, `"type": "ping"`) && !strings.Contains(body, `"type":"ping"`) {
		t.Errorf("expected ping events during post-header body silence, got %d bytes of body: %q", len(body), body)
	}
}

// TestPingWriter_PingsResumeDuringMidStreamSilence: same contract one level
// deeper — after a first real body byte (e.g. llama-server's initial chunk),
// a long silence gap must also be bridged by pings, because the client's
// body timeout resets per byte and a single early byte followed by minutes
// of silence still kills the stream.
func TestPingWriter_PingsResumeDuringMidStreamSilence(t *testing.T) {
	if testing.Short() {
		t.Skip("waits >pingQuietDelay of real time")
	}
	logger := logmon.NewWriter(io.Discard)
	w := newSyncRecorder()

	// Shorten the cadence so the test does not sleep 37s of real time;
	// production defaults (20s/15s) are restored on cleanup.
	origQuiet, origInterval := pingQuietDelay, pingInterval
	pingQuietDelay, pingInterval = 100*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { pingQuietDelay, pingInterval = origQuiet, origInterval })

	pw := newPingWriter(logger, "test-model", w, true)
	defer pw.stop()

	pw.Header().Set("Content-Type", "text/event-stream")
	pw.WriteHeader(http.StatusOK)
	if _, err := pw.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	before := w.bodyLen()

	time.Sleep(pingQuietDelay + pingInterval + 2*time.Second)

	body := w.bodyString()[before:]
	if !strings.Contains(body, `"type": "ping"`) && !strings.Contains(body, `"type":"ping"`) {
		t.Errorf("expected ping events during mid-stream silence, got %d new bytes: %q", len(body), body)
	}
}
