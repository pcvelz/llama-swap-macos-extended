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
	"github.com/mostlygeek/llama-swap/internal/swaputil"
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
		if data, ok := swaputil.ReadContext(r.Context()); ok {
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

// TestServer_RewriteMultipartModel / boundaryOf were dropped in the upstream
// merge: the multipart model rewrite moved out of this package into
// swaputil.ReplaceRequestModel, and upstream carries the test there.
func TestServer_ResolveFilters_QualifiedPeer(t *testing.T) {
	want := config.Filters{StripParams: "temperature"}
	cfg := config.Config{Peers: config.PeerDictionaryConfig{
		"remote": {
			Models:  []string{"org/model"},
			Filters: want,
		},
	}}

	useModelName, got, ok := resolveFilters(cfg, "remote/org/model")
	if !ok {
		t.Fatal("qualified peer filters were not resolved")
	}
	if useModelName != "" {
		t.Fatalf("useModelName = %q, want empty for peer", useModelName)
	}
	if got.StripParams != want.StripParams {
		t.Fatalf("StripParams = %q, want %q", got.StripParams, want.StripParams)
	}
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

	var gotModel, gotFilename, gotFileBody string
	var gotContext swaputil.ReqContextData
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(swaputil.MaxMultiPartSize); err != nil {
			t.Errorf("ParseMultipartForm: %v", err)
			return
		}
		gotModel = r.MultipartForm.Value["model"][0]
		fileHeader := r.MultipartForm.File["file"][0]
		gotFilename = fileHeader.Filename
		file, err := fileHeader.Open()
		if err != nil {
			t.Errorf("open file: %v", err)
			return
		}
		data, err := io.ReadAll(file)
		file.Close()
		if err != nil {
			t.Errorf("read file: %v", err)
			return
		}
		gotFileBody = string(data)
		gotContext, _ = swaputil.ReadContext(r.Context())
	})
	CreateFormFilterMiddleware(cfg)(final).ServeHTTP(httptest.NewRecorder(), r)

	if gotModel != "whisper-large-v3" {
		t.Errorf("model rewritten to %q, want whisper-large-v3", gotModel)
	}
	if gotFilename != "a.wav" {
		t.Errorf("filename = %q, want a.wav", gotFilename)
	}
	if gotFileBody != "xx" {
		t.Errorf("file body = %q, want xx", gotFileBody)
	}
	if gotContext.Model != "whisper" || gotContext.ModelID != "whisper" {
		t.Errorf("request context = %+v, want original whisper model", gotContext)
	}
}
