package process

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
)

// TestProcessCommand_UpstreamDisconnectEmitsSSEError reproduces the 2026-07-22
// compact-stall finding (llama-cm incident
// 2026-07-09-compact-stall-cold-cache-swap-churn, follow-up 2026-07-22):
//
// When the upstream llama-server connection breaks mid-SSE-stream (witnessed:
// KV-exhaustion abort of two oversubscribed 163k requests at 14:06:34, and the
// GGML_ASSERT crash at 14:32:47), the reverse proxy panics with
// http.ErrAbortHandler, handlerFn recovers — and then simply returns. Go
// closes the chunked response CLEANLY, so the client receives a well-formed
// HTTP 200 whose SSE body just silently lacks any terminal event. Anthropic
// clients (claude-cli) neither error nor back off on that: the compact UI
// hangs at "95%" and the client blind-retries (witnessed as "no valid JSON
// data found in stream" + 200 with a pings-only 195-byte body).
//
// Contract under test: a mid-stream upstream disconnection on an SSE response
// must terminate the client stream SEMANTICALLY — with an Anthropic SSE
// `error` event — not just close the socket as if the stream completed.
// Sibling of TestProcessCommand_ReverseProxyPanicIsRecovered (which only
// asserts the recover log line).
func TestProcessCommand_UpstreamDisconnectEmitsSSEError(t *testing.T) {
	skipIfNoSimpleResponder(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		// Emit valid SSE headers + a partial event stream, then slam the
		// connection shut mid-stream (the KV-exhaustion / GGML-crash shape).
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("upstream: hijack not supported")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("upstream: hijack: %v", err)
			return
		}
		_, _ = conn.Write([]byte(
			"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n" +
				"3c\r\nevent: message_start\ndata: {\"type\":\"message_start\"}\n\n\r\n"))
		_ = conn.Close()
	}))
	t.Cleanup(upstream.Close)

	proxyLogger := logmon.NewWriter(io.Discard)
	procLogger := logmon.NewWriter(io.Discard)

	cmd, _ := simpleResponderCmd(t, "-silent")
	p, err := New(context.Background(), t.Name(), config.ModelConfig{
		Cmd:                cmd,
		Proxy:              upstream.URL,
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 10,
	}, procLogger, proxyLogger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { p.Stop(testStopTimeout) })

	_ = runAsync(t, p)

	// Real server wrapper so httputil.ReverseProxy raises ErrAbortHandler on
	// the copy error (needs http.ServerContextKey), same as the sibling test.
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	resp, err := http.Get(front.URL + "/v1/messages")
	if err != nil {
		// A transport-level error would at least make the client retry; the
		// witnessed production failure is the silent-success shape, so a
		// hard error here would technically pass the contract. Report it so
		// the fix keeps the response parseable instead.
		t.Fatalf("client saw transport error instead of SSE error event: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if !strings.Contains(string(body), "event: error") {
		t.Errorf("upstream died mid-SSE but client body has no terminal SSE error event; client sees a cleanly-completed stream and hangs/blind-retries.\nbody (%d bytes): %q", len(body), body)
	}
}
