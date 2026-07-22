package router

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/logmon"
)

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
	w := httptest.NewRecorder()

	// Shorten the cadence so the test does not sleep 37s of real time;
	// production defaults (20s/15s) are restored on cleanup.
	origQuiet, origInterval := pingQuietDelay, pingInterval
	pingQuietDelay, pingInterval = 100*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { pingQuietDelay, pingInterval = origQuiet, origInterval })

	pw := newPingWriter(logger, "test-model", w)
	defer pw.stop()

	// llama-server's httplib sends SSE headers immediately at accept.
	pw.Header().Set("Content-Type", "text/event-stream")
	pw.WriteHeader(http.StatusOK)

	// Then the body goes silent (task queued / prefilling / decode-starved).
	// Wait past pingQuietDelay plus one pingInterval of margin.
	time.Sleep(pingQuietDelay + pingInterval + 2*time.Second)

	body := w.Body.String()
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
	w := httptest.NewRecorder()

	// Shorten the cadence so the test does not sleep 37s of real time;
	// production defaults (20s/15s) are restored on cleanup.
	origQuiet, origInterval := pingQuietDelay, pingInterval
	pingQuietDelay, pingInterval = 100*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { pingQuietDelay, pingInterval = origQuiet, origInterval })

	pw := newPingWriter(logger, "test-model", w)
	defer pw.stop()

	pw.Header().Set("Content-Type", "text/event-stream")
	pw.WriteHeader(http.StatusOK)
	if _, err := pw.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	before := w.Body.Len()

	time.Sleep(pingQuietDelay + pingInterval + 2*time.Second)

	body := w.Body.String()[before:]
	if !strings.Contains(body, `"type": "ping"`) && !strings.Contains(body, `"type":"ping"`) {
		t.Errorf("expected ping events during mid-stream silence, got %d new bytes: %q", len(body), body)
	}
}
