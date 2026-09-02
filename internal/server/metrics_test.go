package server

import (
	"bytes"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"github.com/tidwall/gjson"
)

func TestServer_ParseMetrics_ChatCompletions(t *testing.T) {
	body := `{"usage":{"prompt_tokens":12,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":4}}}`
	parsed := gjson.Parse(body)
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), parsed.Get("timings"), parsed.Get("metrics"), parsed.Get("id_slot"))
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
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), parsed.Get("timings"), parsed.Get("metrics"), parsed.Get("id_slot"))
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

func TestServer_ProcessStreamingResponse_VLLMMetrics(t *testing.T) {
	body := []byte(`data: {"id":"chatcmpl-b7a832cea986aea4","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":14,"total_tokens":166,"completion_tokens":152},"metrics":{"time_to_first_token_ms":70,"mean_itl_ms":10,"tokens_per_second":24.116032676555495}}

data: [DONE]
`)
	entry, err := processStreamingResponse("m", time.Now(), body)
	if err != nil {
		t.Fatalf("processStreamingResponse: %v", err)
	}
	if entry.Tokens.InputTokens != 14 || entry.Tokens.OutputTokens != 152 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
	if entry.Tokens.CachedTokens != -1 {
		t.Errorf("CachedTokens = %d, want -1", entry.Tokens.CachedTokens)
	}
	if got, want := entry.Tokens.PromptPerSecond, 200.0; math.Abs(got-want) > 1e-9 {
		t.Errorf("PromptPerSecond = %v, want %v", got, want)
	}
	if entry.Tokens.TokensPerSecond != 100 {
		t.Errorf("TokensPerSecond = %v, want 100", entry.Tokens.TokensPerSecond)
	}
}

func TestServer_ParseMetrics_VLLMMetrics(t *testing.T) {
	body := `{"id":"chatcmpl-abc123","object":"chat.completion","usage":{"prompt_tokens":42,"completion_tokens":128,"total_tokens":170,"prompt_tokens_details":{"cached_tokens":20}},"metrics":{"time_to_first_token_ms":85.2,"generation_time_ms":1240.5,"queue_time_ms":12.3,"mean_itl_ms":9.1,"tokens_per_second":103.2}}`
	parsed := gjson.Parse(body)
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), parsed.Get("timings"), parsed.Get("metrics"), parsed.Get("id_slot"))
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if entry.Tokens.InputTokens != 42 || entry.Tokens.OutputTokens != 128 || entry.Tokens.CachedTokens != 20 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
	if got, want := entry.Tokens.PromptPerSecond, float64(42-20)/(85.2/1000); math.Abs(got-want) > 1e-9 {
		t.Errorf("PromptPerSecond = %v, want %v", got, want)
	}
	if got, want := entry.Tokens.TokensPerSecond, 1000/9.1; math.Abs(got-want) > 1e-9 {
		t.Errorf("TokensPerSecond = %v, want %v", got, want)
	}
}

func TestServer_ProcessStreamingResponse_NoData(t *testing.T) {
	if _, err := processStreamingResponse("m", time.Now(), []byte("data: [DONE]\n\n")); err == nil {
		t.Fatal("expected error for stream with no usage data")
	}
}

// TestServer_ParseMetrics_SlotID covers stamping slot_id onto the parsed
// entry's Metadata when the child llama-server response carries a top-level
// id_slot, so a reader can tell which slot served a given request.
func TestServer_ParseMetrics_SlotID(t *testing.T) {
	body := `{"id_slot":3,"timings":{"prompt_n":1,"predicted_n":1}}`
	parsed := gjson.Parse(body)
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), parsed.Get("timings"), parsed.Get("metrics"), parsed.Get("id_slot"))
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if entry.Metadata["slot_id"] != "3" {
		t.Errorf("slot_id = %q, want %q", entry.Metadata["slot_id"], "3")
	}
}

// TestServer_ParseMetrics_NoSlotID covers a response with no id_slot: the
// key must stay absent, not an empty string, so downstream callers can tell
// "unknown slot" apart from "slot 0".
func TestServer_ParseMetrics_NoSlotID(t *testing.T) {
	body := `{"timings":{"prompt_n":1,"predicted_n":1}}`
	parsed := gjson.Parse(body)
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), parsed.Get("timings"), parsed.Get("metrics"), parsed.Get("id_slot"))
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if _, ok := entry.Metadata["slot_id"]; ok {
		t.Errorf("slot_id key should be absent, got %q", entry.Metadata["slot_id"])
	}
}

// TestServer_ProcessStreamingResponse_SlotID covers extracting id_slot from a
// streamed response's final chunk, mirroring how llama.cpp repeats id_slot on
// every SSE chunk.
func TestServer_ProcessStreamingResponse_SlotID(t *testing.T) {
	body := []byte("data: {\"choices\":[{}],\"id_slot\":2}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":15,\"completion_tokens\":33},\"id_slot\":2}\n\n" +
		"data: [DONE]\n\n")
	entry, err := processStreamingResponse("m", time.Now(), body)
	if err != nil {
		t.Fatalf("processStreamingResponse: %v", err)
	}
	if entry.Metadata["slot_id"] != "2" {
		t.Errorf("slot_id = %q, want %q", entry.Metadata["slot_id"], "2")
	}
}

func TestMetricsMonitor_RecordMetadata(t *testing.T) {
	mm := newTestMetricsMonitor(t, nil, 10, 0)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"usage":{}}`))
	r = r.WithContext(swaputil.SetContext(r.Context(), swaputil.ReqContextData{
		ModelID:  "m",
		Metadata: map[string]string{"client": "web", "trace": "abc"},
	}))

	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.WriteHeader(http.StatusOK)
	copier.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":2}}`))

	mm.record("m", r, copier, 0, nil)

	entries := metricsEntries(t, mm)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if entries[0].Metadata["client"] != "web" {
		t.Errorf("client = %q, want web", entries[0].Metadata["client"])
	}
	if entries[0].Metadata["trace"] != "abc" {
		t.Errorf("trace = %q, want abc", entries[0].Metadata["trace"])
	}
}

// TestMetricsMonitor_RecordSlotID covers record() merging the slot_id parsed
// out of the child response body into the stored entry's Metadata, alongside
// (not clobbering) the request-context keys like tier/selector.
func TestMetricsMonitor_RecordSlotID(t *testing.T) {
	mm := newTestMetricsMonitor(t, nil, 10, 0)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"usage":{}}`))
	r = r.WithContext(swaputil.SetContext(r.Context(), swaputil.ReqContextData{
		ModelID:  "m",
		Metadata: map[string]string{"client": "web"},
	}))

	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.WriteHeader(http.StatusOK)
	copier.Write([]byte(`{"id_slot":5,"usage":{"prompt_tokens":1,"completion_tokens":2}}`))

	mm.record("m", r, copier, 0, nil)

	entries := metricsEntries(t, mm)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if entries[0].Metadata["slot_id"] != "5" {
		t.Errorf("slot_id = %q, want %q", entries[0].Metadata["slot_id"], "5")
	}
	if entries[0].Metadata["client"] != "web" {
		t.Errorf("client = %q, want web (slot_id merge must not clobber context metadata)", entries[0].Metadata["client"])
	}
}

func TestMetricsMonitor_RecordFailedRequestCapture(t *testing.T) {
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 10, 5)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqHeaders := map[string]string{"content-type": "application/json"}

	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.Header().Set("Content-Type", "application/json")
	copier.WriteHeader(http.StatusBadGateway)
	copier.Write([]byte(`{"error":{"message":"model unavailable"}}`))

	reqBody := []byte(`{"model":"m","messages":[]}`)
	mm.record("m", r, copier, captureAll, testReqCapture(t, reqBody, reqHeaders))

	entries := metricsEntries(t, mm)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	entry := entries[0]
	if entry.RespStatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", entry.RespStatusCode, http.StatusBadGateway)
	}
	if entry.ErrorMsg != "model unavailable" {
		t.Errorf("error_msg = %q, want extracted message", entry.ErrorMsg)
	}
	if !entry.HasCapture {
		t.Fatal("failed request should capture the request so it can be inspected")
	}

	got := mm.getCaptureByID(entry.ID)
	if got == nil {
		t.Fatal("capture not found")
	}
	if string(got.ReqBody) != `{"model":"m","messages":[]}` {
		t.Errorf("req body = %q", got.ReqBody)
	}
	if len(got.RespBody) != 0 {
		t.Errorf("resp body stored for failed request (len=%d); want none", len(got.RespBody))
	}
	if got.RespHeaders["Content-Type"] != "application/json" {
		t.Errorf("resp Content-Type = %q", got.RespHeaders["Content-Type"])
	}
}

func TestMetricsMonitor_RecordFailedRequestStatusFallback(t *testing.T) {
	// Non-JSON error body: ErrorMsg falls back to the HTTP status text.
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 10, 5)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.WriteHeader(http.StatusBadGateway)
	copier.Write([]byte("<html>upstream down</html>"))

	mm.record("m", r, copier, captureAll, nil)

	entries := metricsEntries(t, mm)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if entries[0].ErrorMsg != "502 Bad Gateway" {
		t.Errorf("error_msg = %q, want status text", entries[0].ErrorMsg)
	}
}

func TestMetricsMonitor_RecordFailedRequestCaptureDisabled(t *testing.T) {
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 10, 0) // captures disabled
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.WriteHeader(http.StatusInternalServerError)
	copier.Write([]byte(`{"error":"boom"}`))

	mm.record("m", r, copier, captureAll, testReqCapture(t, []byte("req"), nil))

	entries := metricsEntries(t, mm)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if entries[0].HasCapture {
		t.Fatal("captures disabled, HasCapture should be false")
	}
	// ErrorMsg is independent of whether captures are enabled.
	if entries[0].ErrorMsg != "boom" {
		t.Errorf("error_msg = %q, want boom", entries[0].ErrorMsg)
	}
	if mm.getCaptureByID(entries[0].ID) != nil {
		t.Fatal("no capture should be stored when disabled")
	}
}

func TestMetricsMonitor_RecordDecompressionFailureSetsErrorMsg(t *testing.T) {
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 10, 5)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.Header().Set("Content-Encoding", "gzip")
	copier.WriteHeader(http.StatusOK)
	copier.Write([]byte("not-really-gzip"))

	mm.record("m", r, copier, captureAll, testReqCapture(t, []byte("req"), nil))

	entries := metricsEntries(t, mm)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if entries[0].ErrorMsg == "" {
		t.Fatal("expected ErrorMsg for decompression failure")
	}
	// Raw bytes must not be stored when the body could not be decoded.
	if entries[0].HasCapture {
		t.Fatal("decompression failure should not store a capture")
	}
}

func TestMetricsMonitor_DecodeResponseBody(t *testing.T) {
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 10, 5)

	// No Content-Encoding: body returned unchanged.
	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.Write([]byte("plain"))
	got, err := mm.decodeResponseBody(copier, "/p")
	if err != nil || string(got) != "plain" {
		t.Fatalf("plain body = %q, err = %v", got, err)
	}

	// Bogus gzip payload: returns an error and no body (no raw bytes kept).
	w2 := httptest.NewRecorder()
	copier2 := newBodyCopier(w2)
	copier2.Header().Set("Content-Encoding", "gzip")
	copier2.Write([]byte("not-really-gzip"))
	got, err = mm.decodeResponseBody(copier2, "/p")
	if err == nil {
		t.Fatal("expected decompression error")
	}
	if got != nil {
		t.Errorf("expected nil body on failure, got %q", got)
	}
}

func TestServer_ExtractErrorMessage(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"openai object", `{"error":{"message":"rate limited"}}`, "rate limited"},
		{"string error", `{"error":"bad request"}`, "bad request"},
		{"message field", `{"message":"nope"}`, "nope"},
		{"detail field", `{"detail":"oops"}`, "oops"},
		{"object error ignored", `{"error":{"code":42}}`, ""},
		{"no error", `{"usage":{}}`, ""},
		{"invalid json", `not-json`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractErrorMessage([]byte(tc.body)); got != tc.want {
				t.Errorf("extractErrorMessage = %q, want %q", got, tc.want)
			}
		})
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
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), timings, parsed.Get("metrics"), parsed.Get("id_slot"))
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if entry.Tokens.InputTokens != 5 || entry.Tokens.OutputTokens != 9 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
}

// TestServer_MetricsMiddleware_UpstreamAudioCaptureSkipsRespBody verifies that
// an /upstream/<model>/v1/audio/speech request uses the path-specific capture
// mask (headers only) rather than falling back to captureAll.
func TestServer_MetricsMiddleware_UpstreamAudioCaptureSkipsRespBody(t *testing.T) {
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 100, 5)
	cfg := config.Config{Models: map[string]config.ModelConfig{"m1": {}}}

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("BINARY-AUDIO-DATA"))
	})
	handler := CreateMetricsMiddleware(mm, cfg)(inner)

	req := httptest.NewRequest(http.MethodPost, "/upstream/m1/v1/audio/speech", strings.NewReader(`{"model":"m1"}`))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	entries := metricsEntries(t, mm)
	if len(entries) == 0 {
		t.Fatal("no metrics recorded")
	}
	last := entries[len(entries)-1]
	if !last.HasCapture {
		t.Fatal("expected capture to be stored")
	}
	cap := mm.getCaptureByID(last.ID)
	if cap == nil {
		t.Fatal("capture not found")
	}
	if len(cap.RespBody) != 0 {
		t.Errorf("RespBody stored for /upstream audio route (len=%d); want path-specific mask to skip body", len(cap.RespBody))
	}
	if len(cap.RespHeaders) == 0 {
		t.Error("RespHeaders not stored; want captureRespHeaders mask")
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
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 100, 5)

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

	metrics := metricsEntries(t, mm)
	if len(metrics) != 1 {
		t.Fatalf("got %d metrics entries, want 1", len(metrics))
	}
	entry := metrics[0]
	if entry.Tokens.InputTokens != 11 || entry.Tokens.OutputTokens != 22 {
		t.Fatalf("tokens = %+v, want input=11 output=22 (non-streaming response over the tee cap must not be truncated)", entry.Tokens)
	}
}

// TestServer_MetricsMiddleware_UpstreamCapturesRequestBody covers the seam
// between upstream's /upstream metering (#858) and our pre-compressed request
// capture (fork commit 1f9ac8f): swaputil.extractUpstreamContext resolves the
// model from the URL and never buffers the body, so ReqContextData.Body is nil
// on this route and the middleware must fall back to reading r.Body itself -
// otherwise /upstream captures would silently hold no request body, and the
// downstream handler must still see the bytes.
func TestServer_MetricsMiddleware_UpstreamCapturesRequestBody(t *testing.T) {
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 100, 5)
	cfg := config.Config{Models: map[string]config.ModelConfig{"m1": {}}}

	reqBody := `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`
	var downstreamSaw string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		downstreamSaw = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"usage":{"prompt_tokens":2,"completion_tokens":3}}`))
	})

	req := httptest.NewRequest(http.MethodPost, "/upstream/m1/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	CreateMetricsMiddleware(mm, cfg)(inner).ServeHTTP(httptest.NewRecorder(), req)

	if downstreamSaw != reqBody {
		t.Fatalf("downstream handler saw body %q, want %q (body must be restored after the capture read)", downstreamSaw, reqBody)
	}

	entries := metricsEntries(t, mm)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	capture := mm.getCaptureByID(entries[0].ID)
	if capture == nil {
		t.Fatal("capture not found")
	}
	if string(capture.ReqBody) != reqBody {
		t.Errorf("capture.ReqBody = %q, want %q", capture.ReqBody, reqBody)
	}
}

// testReqCapture builds the pre-compressed request-side capture blob that
// CreateMetricsMiddleware hands to metricsMonitor.record (fork commit 1f9ac8f:
// the request body is compressed once at dispatch time instead of being pinned
// in memory for the whole streamed response).
func testReqCapture(t *testing.T, body []byte, headers map[string]string) []byte {
	t.Helper()
	if len(body) == 0 && len(headers) == 0 {
		return nil
	}
	compressed, _, err := compressCapture(&ReqRespCapture{ReqBody: body, ReqHeaders: headers})
	if err != nil {
		t.Fatalf("compressCapture: %v", err)
	}
	return compressed
}
