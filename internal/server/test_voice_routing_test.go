package server

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// TestServer_VoiceTranscriptionProxiesToEndpoint proves that when
// TranscriptionEndpoint is set, a /v1/audio/transcriptions request is
// reverse-proxied directly to that endpoint, bypassing the model dispatch
// chain. The upstream STT service receives the original multipart body and
// its response is streamed back to the client. This is the fix for voice
// dictation not working on cq* models: Claude Code sends model=<chat-model>
// in the form; without the endpoint proxy the audio would land on a text-only
// llama-server that cannot transcribe.
func TestServer_VoiceTranscriptionProxiesToEndpoint(t *testing.T) {
	// Standalone STT service (simulates whisper-server /inference).
	var gotBody []byte
	var gotContentType string
	stt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotContentType = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"text":"hello world"}`))
	}))
	defer stt.Close()

	local := newStubRouter([]string{"cq27"}, "")
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		t.Error("model dispatch chain must NOT be called when TranscriptionEndpoint is set")
	}

	cfg := config.Config{HealthCheckTimeout: 15}
	cfg.TranscriptionEndpoint = stt.URL + "/inference"
	cfg.Models = map[string]config.ModelConfig{
		"cq27": {
			Cmd:   "echo ready",
			Proxy: "http://localhost:8080",
		},
	}

	s := newTestServerWithConfig(cfg, local, newStubRouter(nil, ""))

	// Build a multipart transcription request with model=cq27 (the chat model).
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "test.wav")
	fw.Write([]byte("fake audio data"))
	mw.WriteField("model", "cq27")
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"text":"hello world"`) {
		t.Errorf("response missing transcription text: %s", body)
	}
	// WHY: the STT endpoint must receive the multipart body with its original
	// Content-Type (including boundary). If the proxy mangled the body or
	// dropped the boundary, whisper-server would fail to parse the form.
	if !strings.Contains(gotContentType, "multipart/form-data") {
		t.Errorf("upstream Content-Type=%q, want multipart/form-data", gotContentType)
	}
	if len(gotBody) == 0 {
		t.Error("upstream received empty body; multipart form was not forwarded")
	}
}

// TestServer_VoiceTranscriptionCapabilityAdvertised proves that when
// TranscriptionEndpoint is set, /v1/models advertises audio_transcriptions:true
// on all models. Claude Code uses this flag to decide whether to enable the
// hold-space voice button for a given model. Without it, voice is silently
// disabled on cq* sessions even though the backend can handle transcription.
func TestServer_VoiceTranscriptionCapabilityAdvertised(t *testing.T) {
	cfg := config.Config{HealthCheckTimeout: 15}
	cfg.TranscriptionEndpoint = "http://localhost:8099/inference"
	cfg.Models = map[string]config.ModelConfig{
		"cq27": {
			Cmd:   "echo ready",
			Proxy: "http://localhost:8080",
		},
	}

	s := newTestServerWithConfig(cfg, newStubRouter([]string{"cq27"}, ""), newStubRouter(nil, ""))

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}

	body, _ := io.ReadAll(w.Result().Body)
	// WHY: with TranscriptionEndpoint set, EVERY model advertises
	// audio_transcriptions because the form filter reverse-proxies all
	// transcription traffic to the endpoint regardless of which chat model
	// the session is using.
	if !bytes.Contains(body, []byte(`"audio_transcriptions":true`)) {
		t.Errorf("/v1/models response missing audio_transcriptions:true:\n%s", body)
	}
}

// TestServer_VoiceTranscriptionNoEndpointFallsThrough proves that when
// TranscriptionEndpoint is NOT set, transcription requests go through the
// normal model dispatch chain (upstream behaviour). This guards against the
// endpoint proxy accidentally intercepting requests when no endpoint is
// configured.
func TestServer_VoiceTranscriptionNoEndpointFallsThrough(t *testing.T) {
	dispatched := false
	local := newStubRouter([]string{"cq27"}, "")
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		dispatched = true
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"text":"from dispatch"}`))
	}

	cfg := config.Config{HealthCheckTimeout: 15}
	// No TranscriptionEndpoint set.
	cfg.Models = map[string]config.ModelConfig{
		"cq27": {
			Cmd:   "echo ready",
			Proxy: "http://localhost:8080",
		},
	}

	s := newTestServerWithConfig(cfg, local, newStubRouter(nil, ""))

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "test.wav")
	fw.Write([]byte("fake audio data"))
	mw.WriteField("model", "cq27")
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	if !dispatched {
		t.Error("model dispatch chain was not called; TranscriptionEndpoint is empty so request should fall through")
	}
}
