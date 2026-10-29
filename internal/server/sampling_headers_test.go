package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/tidwall/gjson"
)

func samplingCfg(optIn bool) config.Config {
	mc := config.ModelConfig{Cmd: "llama-server"}
	if optIn {
		mc.Metadata = map[string]any{"samplingHeaders": true}
	}
	return config.Config{Models: map[string]config.ModelConfig{
		"llama-model": mc,
		"splash":      {Cmd: "splash-serve.py"}, // never opted in: an engine without these fields
	}}
}

func headers(pairs ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(pairs); i += 2 {
		h.Set(pairs[i], pairs[i+1])
	}
	return h
}

func TestServer_SamplingHeaders_TemperatureOverrides(t *testing.T) {
	cfg := samplingCfg(true)
	out, err := applySamplingHeadersForModel(cfg, "llama-model",
		headers("X-Temperature", "0.3"),
		[]byte(`{"model":"llama-model","temperature":1.0}`))
	if err != nil {
		t.Fatalf("applySamplingHeadersForModel: %v", err)
	}
	if got := gjson.GetBytes(out, "temperature").Float(); got != 0.3 {
		t.Errorf("temperature = %v, want 0.3 (header overrides the body)", got)
	}
}

func TestServer_SamplingHeaders_PenaltiesAreFillOnly(t *testing.T) {
	cfg := samplingCfg(true)
	out, err := applySamplingHeadersForModel(cfg, "llama-model",
		headers("X-Presence-Penalty", "1.5", "X-Repetition-Penalty", "1.2"),
		[]byte(`{"model":"llama-model","presence_penalty":-0.5}`))
	if err != nil {
		t.Fatalf("applySamplingHeadersForModel: %v", err)
	}
	if got := gjson.GetBytes(out, "presence_penalty").Float(); got != -0.5 {
		t.Errorf("presence_penalty = %v, want the body's -0.5 (fill-only)", got)
	}
	if got := gjson.GetBytes(out, "repeat_penalty").Float(); got != 1.2 {
		t.Errorf("repeat_penalty = %v, want 1.2 (filled, body had none)", got)
	}
}

func TestServer_SamplingHeaders_RangeBoundaries(t *testing.T) {
	cfg := samplingCfg(true)
	cases := []struct {
		header, value, bodyField string
		wantSet                  bool
	}{
		{"X-Temperature", "2", "temperature", true},     // top of range
		{"X-Temperature", "0", "temperature", false},    // 0 excluded
		{"X-Temperature", "-0.1", "temperature", false}, // below range
		{"X-Temperature", "2.1", "temperature", false},  // above range
		{"X-Presence-Penalty", "-2", "presence_penalty", true},
		{"X-Presence-Penalty", "2", "presence_penalty", true},
		{"X-Presence-Penalty", "-2.1", "presence_penalty", false},
		{"X-Repetition-Penalty", "3", "repeat_penalty", true},
		{"X-Repetition-Penalty", "0", "repeat_penalty", false},
		{"X-Repetition-Penalty", "3.5", "repeat_penalty", false},
	}
	for _, c := range cases {
		out, err := applySamplingHeadersForModel(cfg, "llama-model",
			headers(c.header, c.value), []byte(`{"model":"llama-model"}`))
		if err != nil {
			t.Fatalf("%s=%s: %v", c.header, c.value, err)
		}
		got := gjson.GetBytes(out, c.bodyField)
		if c.wantSet && !got.Exists() {
			t.Errorf("%s=%s: %s not set, want set", c.header, c.value, c.bodyField)
		}
		if !c.wantSet && got.Exists() {
			t.Errorf("%s=%s: %s set to %v, want ignored", c.header, c.value, c.bodyField, got.Value())
		}
	}
}

func TestServer_SamplingHeaders_GarbageIgnored(t *testing.T) {
	cfg := samplingCfg(true)
	for _, v := range []string{"abc", "", "  ", "NaN", "Inf", "-Inf", "1e", "0x1", "1,5"} {
		out, err := applySamplingHeadersForModel(cfg, "llama-model",
			headers("X-Temperature", v), []byte(`{"model":"llama-model","temperature":0.8}`))
		if err != nil {
			t.Fatalf("X-Temperature=%q: a header must never fail a request: %v", v, err)
		}
		if got := gjson.GetBytes(out, "temperature").Float(); got != 0.8 {
			t.Errorf("X-Temperature=%q: temperature = %v, want the body's 0.8", v, got)
		}
	}
}

func TestServer_SamplingHeaders_ModelWithoutOptInUntouched(t *testing.T) {
	cfg := samplingCfg(true) // "splash" has no metadata key
	for _, model := range []string{"splash", "unknown-model"} {
		out, err := applySamplingHeadersForModel(cfg, model,
			headers("X-Temperature", "0.2", "X-Repetition-Penalty", "1.1"),
			[]byte(`{"model":"`+model+`"}`))
		if err != nil {
			t.Fatalf("%s: %v", model, err)
		}
		if gjson.GetBytes(out, "temperature").Exists() || gjson.GetBytes(out, "repeat_penalty").Exists() {
			t.Errorf("%s: body rewritten although it never opted in: %s", model, out)
		}
	}
}

func TestServer_SamplingHeaders_AliasResolvesToOptedInModel(t *testing.T) {
	// A static alias resolves through the config loader (the unexported
	// aliases map is only populated there), so this goes through a real yaml
	// load.
	yaml := `
models:
  llama-model:
    cmd: llama-server --port ${PORT}
    aliases: [llama-alias]
    metadata:
      samplingHeaders: true
`
	cfg, err := config.LoadConfigFromReader(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("LoadConfigFromReader: %v", err)
	}
	out, err := applySamplingHeadersForModel(cfg, "llama-alias",
		headers("X-Temperature", "0.4"), []byte(`{"model":"llama-alias"}`))
	if err != nil {
		t.Fatalf("applySamplingHeadersForModel: %v", err)
	}
	if got := gjson.GetBytes(out, "temperature").Float(); got != 0.4 {
		t.Errorf("temperature = %v, want 0.4 via the alias", got)
	}
	// and the yaml metadata opt-in reads through the same key
	if !modelWantsSamplingHeaders(cfg.Models["llama-model"]) {
		t.Error("metadata.samplingHeaders: true did not opt the model in through config.Load")
	}
}

func TestServer_SamplingHeaders_MiddlewareEndToEnd(t *testing.T) {
	cfg := samplingCfg(true)

	run := func(method, path string, headers map[string]string, body string) (downstream []byte, contentLength string) {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		var gotCL string
		final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			downstream, _ = io.ReadAll(r.Body)
			gotCL = r.Header.Get("Content-Length")
		})
		rec := httptest.NewRecorder()
		CreateFilterMiddleware(cfg)(final).ServeHTTP(rec, r)
		return downstream, gotCL
	}

	// opted-in model, completion endpoint: rewritten, Content-Length matches
	down, cl := run(http.MethodPost, "/v1/chat/completions",
		map[string]string{"X-Temperature": "0.5", "X-Presence-Penalty": "0.25"},
		`{"model":"llama-model","temperature":1}`)
	if got := gjson.GetBytes(down, "temperature").Float(); got != 0.5 {
		t.Errorf("downstream temperature = %v, want 0.5", got)
	}
	if got := gjson.GetBytes(down, "presence_penalty").Float(); got != 0.25 {
		t.Errorf("downstream presence_penalty = %v, want 0.25", got)
	}
	if cl != strconv.Itoa(len(down)) {
		t.Errorf("Content-Length = %s, want %d (the rewritten body)", cl, len(down))
	}

	// non-JSON GET: untouched, no rewrite path at all
	down, _ = run(http.MethodGet, "/v1/models", map[string]string{"X-Temperature": "0.5"}, "")
	if len(down) != 0 {
		t.Errorf("GET body = %q, want empty passthrough", down)
	}

	// opted-in model, endpoint outside the three: untouched
	down, _ = run(http.MethodPost, "/v1/embeddings",
		map[string]string{"X-Temperature": "0.5"}, `{"model":"llama-model","input":"hi"}`)
	if gjson.GetBytes(down, "temperature").Exists() {
		t.Errorf("/v1/embeddings rewritten: %s", down)
	}

	// Splash-like model: untouched even on /v1/messages
	down, _ = run(http.MethodPost, "/v1/messages",
		map[string]string{"X-Temperature": "0.5"}, `{"model":"splash"}`)
	if gjson.GetBytes(down, "temperature").Exists() {
		t.Errorf("non-opted-in model rewritten on /v1/messages: %s", down)
	}

	// /v1/messages on the opted-in model: rewritten
	down, _ = run(http.MethodPost, "/v1/messages",
		map[string]string{"X-Temperature": "0.7"}, `{"model":"llama-model"}`)
	if got := gjson.GetBytes(down, "temperature").Float(); got != 0.7 {
		t.Errorf("/v1/messages temperature = %v, want 0.7", got)
	}

	// /v1/completions on the opted-in model: rewritten
	down, _ = run(http.MethodPost, "/v1/completions",
		map[string]string{"X-Repetition-Penalty": "1.3"}, `{"model":"llama-model"}`)
	if got := gjson.GetBytes(down, "repeat_penalty").Float(); got != 1.3 {
		t.Errorf("/v1/completions repeat_penalty = %v, want 1.3", got)
	}
}
