package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/tidwall/gjson"
)

func TestServer_ParseMetrics_ChatCompletions(t *testing.T) {
	body := `{"usage":{"prompt_tokens":12,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":4}}}`
	parsed := gjson.Parse(body)
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), parsed.Get("timings"))
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if entry.Tokens.InputTokens != 12 || entry.Tokens.OutputTokens != 7 || entry.Tokens.CachedTokens != 4 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
}

func TestServer_ParseMetrics_Timings(t *testing.T) {
	body := `{"timings":{"prompt_n":20,"predicted_n":50,"prompt_per_second":100.0,"predicted_per_second":40.0,"prompt_ms":200,"predicted_ms":1250,"cache_n":8}}`
	parsed := gjson.Parse(body)
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), parsed.Get("timings"))
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if entry.Tokens.InputTokens != 20 || entry.Tokens.OutputTokens != 50 || entry.Tokens.CachedTokens != 8 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
	if entry.Tokens.TokensPerSecond != 40.0 || entry.Tokens.PromptPerSecond != 100.0 {
		t.Fatalf("rates = %+v", entry.Tokens)
	}
	if entry.DurationMs != 1450 {
		t.Fatalf("DurationMs = %d, want 1450", entry.DurationMs)
	}
}

func TestServer_ProcessStreamingResponse(t *testing.T) {
	body := []byte("data: {\"choices\":[{}]}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":15,\"completion_tokens\":33}}\n\n" +
		"data: [DONE]\n\n")
	entry, err := processStreamingResponse("m", time.Now(), body)
	if err != nil {
		t.Fatalf("processStreamingResponse: %v", err)
	}
	if entry.Tokens.InputTokens != 15 || entry.Tokens.OutputTokens != 33 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
}

func TestServer_ProcessStreamingResponse_NoData(t *testing.T) {
	if _, err := processStreamingResponse("m", time.Now(), []byte("data: [DONE]\n\n")); err == nil {
		t.Fatal("expected error for stream with no usage data")
	}
}

// TestServer_ResponseBodyCopier_CapsBufferedTail streams more than
// responseTeeCapBytes through a responseBodyCopier for an uncompressed SSE
// response and asserts the client still receives every byte untouched while
// the internal metrics-parsing buffer never grows past the cap and retains
// exactly the tail window. Only text/event-stream responses are capped - see
// TestServer_ResponseBodyCopier_NonStreamingOverCap_NotTruncated below for the
// non-streaming case, which must stay unbounded.
func TestServer_ResponseBodyCopier_CapsBufferedTail(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "text/event-stream")
	bc := newBodyCopier(rec)
	bc.WriteHeader(http.StatusOK)

	const extra = 4096
	total := responseTeeCapBytes + extra
	chunk := bytes.Repeat([]byte("x"), 4096)

	var allWritten bytes.Buffer
	written := 0
	for written < total {
		n := len(chunk)
		if written+n > total {
			n = total - written
		}
		if _, err := bc.Write(chunk[:n]); err != nil {
			t.Fatalf("Write: %v", err)
		}
		allWritten.Write(chunk[:n])
		written += n
	}

	if got := rec.Body.Len(); got != total {
		t.Fatalf("client received %d bytes, want %d (tee must pass everything through untouched)", got, total)
	}
	if !bytes.Equal(rec.Body.Bytes(), allWritten.Bytes()) {
		t.Fatal("client bytes diverge from what was written")
	}

	if got := bc.body.Len(); got > responseTeeCapBytes {
		t.Fatalf("internal buffer len = %d, want <= %d", got, responseTeeCapBytes)
	}
	if got := bc.body.Len(); got != responseTeeCapBytes {
		t.Fatalf("internal buffer len = %d, want exactly %d once the cap is exceeded", got, responseTeeCapBytes)
	}

	wantTail := allWritten.Bytes()[allWritten.Len()-responseTeeCapBytes:]
	if !bytes.Equal(bc.body.Bytes(), wantTail) {
		t.Fatal("buffered tail does not match the most recently written bytes")
	}
}

// TestServer_CappedTailBuffer_NoEagerFullCapAlloc guards against
// re-introducing an eager full-cap allocation: a typical small SSE stream
// (well under responseTeeCapBytes) must grow its backing array lazily
// (Go's normal append growth), not allocate the full 16MB cap up front -
// that would recreate, per concurrent stream, exactly the large-transient-
// allocation footprint this cap exists to eliminate.
func TestServer_CappedTailBuffer_NoEagerFullCapAlloc(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "text/event-stream")
	bc := newBodyCopier(rec)
	bc.WriteHeader(http.StatusOK)

	const streamSize = 64 * 1024 // typical small SSE stream, far under the cap
	chunk := bytes.Repeat([]byte("y"), 4096)
	written := 0
	for written < streamSize {
		n := len(chunk)
		if written+n > streamSize {
			n = streamSize - written
		}
		if _, err := bc.Write(chunk[:n]); err != nil {
			t.Fatalf("Write: %v", err)
		}
		written += n
	}

	ctb, ok := bc.body.(*cappedTailBuffer)
	if !ok {
		t.Fatalf("body is %T, want *cappedTailBuffer (text/event-stream should select the capped buffer)", bc.body)
	}
	if got := ctb.Len(); got != streamSize {
		t.Fatalf("Len() = %d, want %d", got, streamSize)
	}
	if got := cap(ctb.buf); got >= responseTeeCapBytes {
		t.Fatalf("backing array cap = %d, must stay well below the %d-byte tee cap for a %d-byte stream (no eager full-cap allocation)", got, responseTeeCapBytes, streamSize)
	}
	// Generous slack for Go's slice growth factor - still an order of
	// magnitude below the 16MB cap for a 64KB stream.
	if got := cap(ctb.buf); got > streamSize*4 {
		t.Fatalf("backing array cap = %d grew far beyond bytes written (%d); expected lazy, proportional growth", got, streamSize)
	}
}

// TestServer_ResponseBodyCopier_UnderCapKeepsEverything ensures small
// responses (the common case) are not affected by the cap at all.
func TestServer_ResponseBodyCopier_UnderCapKeepsEverything(t *testing.T) {
	rec := httptest.NewRecorder()
	bc := newBodyCopier(rec)

	payload := []byte(`{"usage":{"prompt_tokens":1,"completion_tokens":2}}`)
	if _, err := bc.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if !bytes.Equal(bc.body.Bytes(), payload) {
		t.Fatalf("buffered body = %q, want %q", bc.body.Bytes(), payload)
	}
	if bc.body.Len() != len(payload) {
		t.Fatalf("body.Len() = %d, want %d", bc.body.Len(), len(payload))
	}
}

// TestServer_ResponseBodyCopier_NonStreamingOverCap_NotTruncated is the
// regression test for the defect where a non-streaming JSON response larger
// than responseTeeCapBytes got its metrics-parsing buffer capped to a
// truncated tail (an invalid mid-document JSON fragment), silently zeroing
// out token metrics. The usage object sits at the START of the document,
// followed by > cap bytes of padding, so a tail-only buffer would have missed
// it entirely. Runs the full CreateMetricsMiddleware chain (not just
// responseBodyCopier in isolation) so it exercises exactly the code path
// mp.record uses.
func TestServer_ResponseBodyCopier_NonStreamingOverCap_NotTruncated(t *testing.T) {
	cfg := config.Config{}
	mm := newMetricsMonitor(logmon.NewWriter(io.Discard), 100, 5)

	reqBody := `{"model":"m"}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	r.Header.Set("Content-Type", "application/json")

	padding := strings.Repeat("a", responseTeeCapBytes+4096)
	respBody := `{"usage":{"prompt_tokens":11,"completion_tokens":22},"padding":"` + padding + `"}`

	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(respBody))
	})

	CreateMetricsMiddleware(mm, cfg)(final).ServeHTTP(httptest.NewRecorder(), r)

	metrics := mm.getMetrics()
	if len(metrics) != 1 {
		t.Fatalf("got %d metrics entries, want 1", len(metrics))
	}
	entry := metrics[0]
	if entry.Tokens.InputTokens != 11 || entry.Tokens.OutputTokens != 22 {
		t.Fatalf("tokens = %+v, want input=11 output=22 (non-streaming response over the tee cap must not be truncated)", entry.Tokens)
	}
}

func TestServer_ParseMetrics_Infill(t *testing.T) {
	// /infill responses are arrays; timings live in the last element.
	body := `[{"content":"a"},{"content":"b","timings":{"prompt_n":5,"predicted_n":9,"prompt_ms":10,"predicted_ms":20}}]`
	parsed := gjson.Parse(body)
	timings := parsed.Get("timings")
	if arr := parsed.Array(); len(arr) > 0 {
		timings = arr[len(arr)-1].Get("timings")
	}
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), timings)
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if entry.Tokens.InputTokens != 5 || entry.Tokens.OutputTokens != 9 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
}
