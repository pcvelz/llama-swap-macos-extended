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
	"github.com/mostlygeek/llama-swap/internal/shared"
	"github.com/tidwall/gjson"
)

func TestServer_ApplyFilters(t *testing.T) {
	t.Run("useModelName rewrite", func(t *testing.T) {
		out, err := applyFilters([]byte(`{"model":"alias","temp":1}`), "alias", "real-model", config.Filters{})
		if err != nil {
			t.Fatalf("applyFilters: %v", err)
		}
		if got := gjson.GetBytes(out, "model").String(); got != "real-model" {
			t.Errorf("model = %q, want real-model", got)
		}
	})

	t.Run("strip and set params", func(t *testing.T) {
		f := config.Filters{
			StripParams: "temperature",
			SetParams:   map[string]any{"top_p": 0.9},
		}
		out, err := applyFilters([]byte(`{"model":"m","temperature":0.7}`), "m", "", f)
		if err != nil {
			t.Fatalf("applyFilters: %v", err)
		}
		if gjson.GetBytes(out, "temperature").Exists() {
			t.Error("temperature should be stripped")
		}
		if got := gjson.GetBytes(out, "top_p").Float(); got != 0.9 {
			t.Errorf("top_p = %v, want 0.9", got)
		}
	})

	t.Run("setParamsByID overrides setParams", func(t *testing.T) {
		f := config.Filters{
			SetParams:     map[string]any{"top_p": 0.5},
			SetParamsByID: map[string]map[string]any{"alias": {"top_p": 0.1}},
		}
		out, err := applyFilters([]byte(`{"model":"alias"}`), "alias", "", f)
		if err != nil {
			t.Fatalf("applyFilters: %v", err)
		}
		if got := gjson.GetBytes(out, "top_p").Float(); got != 0.1 {
			t.Errorf("top_p = %v, want 0.1", got)
		}
	})
}

// TestServer_FilterMiddleware_SharedBodyBuffer verifies filters.go consumes
// the request body FetchContext already buffered (rather than re-reading
// r.Body itself), that the filtered result reaches the downstream handler
// with identical io semantics to before, and that the shared context Body is
// updated to the filtered bytes so later middleware (metrics captures) sees
// the mutated body instead of the original.
func TestServer_FilterMiddleware_SharedBodyBuffer(t *testing.T) {
	cfg := config.Config{Models: map[string]config.ModelConfig{
		"alias": {
			UseModelName: "real-model",
			Filters: config.ModelFilters{Filters: config.Filters{
				SetParams: map[string]any{"top_p": 0.9},
			}},
		},
	}}

	reqJSON := `{"model":"alias","temp":1}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqJSON))
	r.Header.Set("Content-Type", "application/json")

	var gotBody []byte
	var gotCtxBody []byte
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		if data, ok := shared.ReadContext(r.Context()); ok {
			gotCtxBody = data.Body
		}
	})

	CreateFilterMiddleware(cfg)(final).ServeHTTP(httptest.NewRecorder(), r)

	if got := gjson.GetBytes(gotBody, "model").String(); got != "real-model" {
		t.Errorf("downstream model = %q, want real-model", got)
	}
	if got := gjson.GetBytes(gotBody, "top_p").Float(); got != 0.9 {
		t.Errorf("downstream top_p = %v, want 0.9", got)
	}
	if gotCtxBody == nil {
		t.Fatal("expected shared context Body to be set after filtering")
	}
	if !bytes.Equal(gotBody, gotCtxBody) {
		t.Errorf("shared context Body = %q, does not match filtered r.Body %q", gotCtxBody, gotBody)
	}
}

func TestServer_RewriteMultipartModel(t *testing.T) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("model", "old-name")
	mw.WriteField("language", "en")
	fw, _ := mw.CreateFormFile("file", "audio.wav")
	fw.Write([]byte("RIFFdata"))
	mw.Close()

	r := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		t.Fatalf("ParseMultipartForm: %v", err)
	}

	body, contentType, err := rewriteMultipartModel(r.MultipartForm, "new-name")
	if err != nil {
		t.Fatalf("rewriteMultipartModel: %v", err)
	}

	parsed, err := multipart.NewReader(bytes.NewReader(body), boundaryOf(t, contentType)).ReadForm(32 << 20)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if got := parsed.Value["model"][0]; got != "new-name" {
		t.Errorf("model = %q, want new-name", got)
	}
	if got := parsed.Value["language"][0]; got != "en" {
		t.Errorf("language = %q, want en", got)
	}
	fh := parsed.File["file"][0]
	f, _ := fh.Open()
	data, _ := io.ReadAll(f)
	f.Close()
	if string(data) != "RIFFdata" {
		t.Errorf("file data = %q, want RIFFdata", data)
	}
}

func boundaryOf(t *testing.T, contentType string) string {
	t.Helper()
	_, params, ok := strings.Cut(contentType, "boundary=")
	if !ok {
		t.Fatalf("no boundary in %q", contentType)
	}
	return params
}

func TestServer_FormFilterMiddleware(t *testing.T) {
	cfg := config.Config{Models: map[string]config.ModelConfig{
		"whisper": {UseModelName: "whisper-large-v3"},
	}}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("model", "whisper")
	fw, _ := mw.CreateFormFile("file", "a.wav")
	fw.Write([]byte("xx"))
	mw.Close()

	r := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())

	var gotModel string
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(32 << 20)
		gotModel = r.MultipartForm.Value["model"][0]
	})
	CreateFormFilterMiddleware(cfg)(final).ServeHTTP(httptest.NewRecorder(), r)

	if gotModel != "whisper-large-v3" {
		t.Errorf("model rewritten to %q, want whisper-large-v3", gotModel)
	}
}
