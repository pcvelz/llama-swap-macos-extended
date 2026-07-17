package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
)

// TestServer_MetricsMiddleware_RequestCaptureAfterEarlyRelease exercises
// CreateMetricsMiddleware end to end: the request body/headers are buffered
// and pre-compressed before dispatch (dropping the raw reqBody/reqHeaders
// references), and record() must still decompress and merge them into a
// correct, retrievable capture once the response has streamed.
func TestServer_MetricsMiddleware_RequestCaptureAfterEarlyRelease(t *testing.T) {
	cfg := config.Config{}
	mm := newMetricsMonitor(logmon.NewWriter(io.Discard), 100, 5)
	if !mm.enableCaptures {
		t.Fatal("captures should be enabled with non-zero buffer")
	}

	reqBody := `{"model":"m","prompt":"hello"}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer secret-token")

	var downstreamSawBody string
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		downstreamSawBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"usage":{"prompt_tokens":3,"completion_tokens":5}}`))
	})

	CreateMetricsMiddleware(mm, cfg)(final).ServeHTTP(httptest.NewRecorder(), r)

	if downstreamSawBody != reqBody {
		t.Fatalf("downstream handler saw body %q, want %q", downstreamSawBody, reqBody)
	}

	metrics := mm.getMetrics()
	if len(metrics) != 1 {
		t.Fatalf("got %d metrics entries, want 1", len(metrics))
	}
	entry := metrics[0]
	if entry.Tokens.InputTokens != 3 || entry.Tokens.OutputTokens != 5 {
		t.Fatalf("tokens = %+v, want input=3 output=5", entry.Tokens)
	}
	if !entry.HasCapture {
		t.Fatal("expected HasCapture=true")
	}

	capture := mm.getCaptureByID(entry.ID)
	if capture == nil {
		t.Fatal("expected a stored capture")
	}
	if string(capture.ReqBody) != reqBody {
		t.Errorf("capture.ReqBody = %q, want %q", capture.ReqBody, reqBody)
	}
	if got := capture.ReqHeaders["Authorization"]; got != "[REDACTED]" {
		t.Errorf("Authorization header = %q, want [REDACTED]", got)
	}
	if got := capture.ReqHeaders["Content-Type"]; got != "application/json" {
		t.Errorf("Content-Type header = %q, want application/json", got)
	}
	if string(capture.RespBody) != `{"usage":{"prompt_tokens":3,"completion_tokens":5}}` {
		t.Errorf("capture.RespBody = %q, unexpected", capture.RespBody)
	}
}

// TestServer_MetricsMiddleware_CapturesDisabled ensures no capture work
// (compression, decompression, storage) happens when captures are off.
func TestServer_MetricsMiddleware_CapturesDisabled(t *testing.T) {
	cfg := config.Config{}
	mm := newMetricsMonitor(logmon.NewWriter(io.Discard), 100, 0)
	if mm.enableCaptures {
		t.Fatal("captures should be disabled with zero buffer")
	}

	reqBody := `{"model":"m"}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	r.Header.Set("Content-Type", "application/json")

	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	})

	CreateMetricsMiddleware(mm, cfg)(final).ServeHTTP(httptest.NewRecorder(), r)

	metrics := mm.getMetrics()
	if len(metrics) != 1 {
		t.Fatalf("got %d metrics entries, want 1", len(metrics))
	}
	if metrics[0].HasCapture {
		t.Fatal("HasCapture should be false when captures are disabled")
	}
}
