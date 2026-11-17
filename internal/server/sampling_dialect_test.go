package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/tidwall/gjson"
)

func samplingCfgMode(mode any) config.Config {
	return config.Config{Models: map[string]config.ModelConfig{
		"m": {Cmd: "x", Metadata: map[string]any{"samplingHeaders": mode}},
	}}
}

func TestServer_SamplingHeaders_EngineDialect(t *testing.T) {
	cases := []struct {
		mode     any
		wantKeys []string
		notKeys  []string
	}{
		{true, []string{"repeat_penalty"}, []string{"repetition_penalty"}},
		{"true", []string{"repeat_penalty"}, []string{"repetition_penalty"}},
		{"llamacpp", []string{"repeat_penalty"}, []string{"repetition_penalty"}},
		{"splash", []string{"repetition_penalty"}, []string{"repeat_penalty"}},
		{"Splash", []string{"repetition_penalty"}, []string{"repeat_penalty"}},
		{"both", []string{"repeat_penalty", "repetition_penalty"}, nil},
	}
	for _, c := range cases {
		out, err := applySamplingHeadersForModel(samplingCfgMode(c.mode), "m",
			headers("X-Repetition-Penalty", "1.2", "X-Presence-Penalty", "0.5", "X-Temperature", "0.9"),
			[]byte(`{"model":"m"}`))
		if err != nil {
			t.Fatalf("%v: %v", c.mode, err)
		}
		for _, k := range c.wantKeys {
			if got := gjson.GetBytes(out, k).Float(); got != 1.2 {
				t.Errorf("mode %v: %s = %v, want 1.2 (%s)", c.mode, k, got, out)
			}
		}
		for _, k := range c.notKeys {
			if gjson.GetBytes(out, k).Exists() {
				t.Errorf("mode %v: %s written, want absent (%s)", c.mode, k, out)
			}
		}
		if gjson.GetBytes(out, "presence_penalty").Float() != 0.5 || gjson.GetBytes(out, "temperature").Float() != 0.9 {
			t.Errorf("mode %v: presence/temperature not written: %s", c.mode, out)
		}
	}
}

func TestServer_SamplingHeaders_DialectFillOnlyUsesItsOwnKey(t *testing.T) {
	// a client body that already carries the engine's own repetition key keeps it
	out, _ := applySamplingHeadersForModel(samplingCfgMode("splash"), "m",
		headers("X-Repetition-Penalty", "1.2"),
		[]byte(`{"model":"m","repetition_penalty":1.05}`))
	if got := gjson.GetBytes(out, "repetition_penalty").Float(); got != 1.05 {
		t.Errorf("repetition_penalty = %v, want the body's 1.05", got)
	}
}

func TestServer_SamplingHeaders_OffValuesNeverRewrite(t *testing.T) {
	for _, mode := range []any{false, "off", "nonsense", 3} {
		out, _ := applySamplingHeadersForModel(samplingCfgMode(mode), "m",
			headers("X-Temperature", "0.5"), []byte(`{"model":"m"}`))
		if gjson.GetBytes(out, "temperature").Exists() {
			t.Errorf("mode %v rewrote the body: %s", mode, out)
		}
	}
}

// The header wins over a static set-if-undefined default in setParams (the
// header is applied first), a client body value wins over both, and a hard
// setParams key is untouched by the ordering.
func TestServer_SamplingHeaders_HeaderBeatsSoftSetParamsDefault(t *testing.T) {
	yaml := `
models:
  m:
    cmd: llama-server --port ${PORT}
    metadata:
      samplingHeaders: splash
    filters:
      setParams:
        "temperature?": 1.0
        "presence_penalty?": 0
        "top_k?": 20
        "repetition_penalty?": 1.0
        timings_per_token: true
`
	cfg, err := config.LoadConfigFromReader(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	run := func(h map[string]string, body string) []byte {
		r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		for k, v := range h {
			r.Header.Set(k, v)
		}
		var down []byte
		final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { down, _ = io.ReadAll(r.Body) })
		CreateFilterMiddleware(cfg)(final).ServeHTTP(httptest.NewRecorder(), r)
		return down
	}

	// no header, no client value: the static defaults apply
	d := run(nil, `{"model":"m"}`)
	if gjson.GetBytes(d, "temperature").Float() != 1.0 || gjson.GetBytes(d, "top_k").Int() != 20 ||
		!gjson.GetBytes(d, "presence_penalty").Exists() || gjson.GetBytes(d, "presence_penalty").Float() != 0 {
		t.Errorf("static defaults missing: %s", d)
	}

	// headers win over the static defaults
	d = run(map[string]string{"X-Temperature": "0.7", "X-Presence-Penalty": "1.5", "X-Repetition-Penalty": "1.1"}, `{"model":"m"}`)
	if gjson.GetBytes(d, "temperature").Float() != 0.7 || gjson.GetBytes(d, "presence_penalty").Float() != 1.5 ||
		gjson.GetBytes(d, "repetition_penalty").Float() != 1.1 {
		t.Errorf("headers did not beat the static defaults: %s", d)
	}
	if !gjson.GetBytes(d, "timings_per_token").Bool() {
		t.Errorf("hard setParams key lost: %s", d)
	}

	// a client's own body value beats the static default
	d = run(nil, `{"model":"m","temperature":0.2,"top_k":5}`)
	if gjson.GetBytes(d, "temperature").Float() != 0.2 || gjson.GetBytes(d, "top_k").Int() != 5 {
		t.Errorf("client body value overwritten by a soft default: %s", d)
	}
}
