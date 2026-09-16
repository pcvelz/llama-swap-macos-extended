package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/process"
)

// TestParseUpstreamSlots pins the two /slots body shapes (the current
// {"slots":[...]} object and the older bare array) and the n_decoded join out
// of next_token[0] - a renderer computing phase/rate off this endpoint must
// get the decode counter, not a zero.
func TestParseUpstreamSlots(t *testing.T) {
	wrapped := []byte(`{"slots":[
		{"id":0,"is_processing":true,"n_prompt_tokens":12000,"n_prompt_tokens_processed":12000,"next_token":[{"n_decoded":340}]},
		{"id":1,"is_processing":false,"n_prompt_tokens":0,"n_prompt_tokens_processed":0}
	]}`)
	slots, ok := parseUpstreamSlots(wrapped)
	if !ok {
		t.Fatal("wrapped form must parse")
	}
	if len(slots) != 2 {
		t.Fatalf("slots = %d, want 2", len(slots))
	}
	if slots[0].ID != 0 || !slots[0].IsProcessing || slots[0].NPromptTokensProcessed != 12000 || slots[0].NDecoded != 340 {
		t.Errorf("slot 0 = %+v, want processing with n_decoded=340", slots[0])
	}
	if slots[1].IsProcessing || slots[1].NDecoded != 0 {
		t.Errorf("slot 1 = %+v, want idle with zero counters", slots[1])
	}

	bare := []byte(`[{"id":2,"is_processing":true,"n_prompt_tokens":5,"n_prompt_tokens_processed":3,"next_token":[{"n_decoded":7}]}]`)
	slots, ok = parseUpstreamSlots(bare)
	if !ok {
		t.Fatal("bare-array form must parse")
	}
	if len(slots) != 1 || slots[0].NDecoded != 7 {
		t.Errorf("bare slots = %+v, want one slot with n_decoded=7", slots)
	}

	if _, ok := parseUpstreamSlots([]byte(`not json`)); ok {
		t.Error("garbage body must not parse")
	}
}

// TestChildProxyURL pins macro substitution on the model's proxy value: a
// literal URL passes through, ${PORT}-style placeholders resolve from the
// model macros, and an unresolvable placeholder yields "" (the endpoint then
// reports an error rather than dialing garbage).
func TestChildProxyURL(t *testing.T) {
	cfg := config.Config{Models: map[string]config.ModelConfig{
		"literal": {Proxy: "http://127.0.0.1:5801"},
		"macroed": {
			Proxy:  "http://localhost:${PORT}",
			Macros: config.MacroList{{Name: "PORT", Value: 5802}},
		},
		"unresolved": {Proxy: "http://localhost:${NOPE}"},
		"empty":      {},
	}}
	if got := childProxyURL(cfg, "literal"); got != "http://127.0.0.1:5801" {
		t.Errorf("literal = %q", got)
	}
	if got := childProxyURL(cfg, "macroed"); got != "http://localhost:5802" {
		t.Errorf("macroed = %q, want the PORT macro substituted", got)
	}
	if got := childProxyURL(cfg, "unresolved"); got != "" {
		t.Errorf("unresolved = %q, want empty", got)
	}
	if got := childProxyURL(cfg, "empty"); got != "" {
		t.Errorf("empty = %q, want empty", got)
	}
	if got := childProxyURL(cfg, "absent"); got != "" {
		t.Errorf("absent model = %q, want empty", got)
	}
}

// TestServer_HandleAPISlots pins the endpoint's contract: one entry per
// RUNNING model (stopped models are absent), slots fetched from the child for
// ready models with a declared ConcurrencyLimit, an error surfaced when the
// child cannot be read, and no fetch attempted for a model without a
// declared slot count.
func TestServer_HandleAPISlots(t *testing.T) {
	// A fake child answering /slots like llama.cpp does.
	child := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/slots" {
			t.Errorf("child got path %q, want /slots", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"slots":[{"id":0,"is_processing":true,"n_prompt_tokens":100,"n_prompt_tokens_processed":90,"next_token":[{"n_decoded":42}]}]}`))
	}))
	defer child.Close()

	local := newStubRouter(nil, "")
	local.running = map[string]process.ProcessState{
		"m-ready":   process.StateReady,
		"m-start":   process.StateStarting,
		"m-nolimit": process.StateReady,
	}
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{
		"m-ready":   {Proxy: child.URL, ConcurrencyLimit: 2},
		"m-start":   {Proxy: child.URL, ConcurrencyLimit: 1},
		"m-nolimit": {Proxy: child.URL},
	}}

	req := httptest.NewRequest(http.MethodGet, "/api/slots", nil)
	w := httptest.NewRecorder()
	s.handleAPISlots(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body struct {
		Models []apiModelSlots `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Models) != 3 {
		t.Fatalf("models = %+v, want the three running models", body.Models)
	}
	byID := map[string]apiModelSlots{}
	for _, m := range body.Models {
		byID[m.Model] = m
	}

	ready := byID["m-ready"]
	if ready.State != "ready" || len(ready.Slots) != 1 || ready.Error != "" {
		t.Errorf("m-ready = %+v, want one slot and no error", ready)
	}
	if got := ready.Slots[0]; got.ID != 0 || !got.IsProcessing || got.NDecoded != 42 || got.NPromptTokensProcessed != 90 {
		t.Errorf("m-ready slot = %+v, want the child's counters", got)
	}

	starting := byID["m-start"]
	if len(starting.Slots) != 0 {
		t.Errorf("m-start slots = %+v, want none while not ready", starting.Slots)
	}

	nolimit := byID["m-nolimit"]
	if len(nolimit.Slots) != 0 || nolimit.Error != "" {
		t.Errorf("m-nolimit = %+v, want no slots and no error (no declared geometry)", nolimit)
	}
}

// TestServer_HandleAPISlots_ChildDown pins the degraded shape: a child that
// cannot be read surfaces an error on its model's row rather than failing the
// whole endpoint - the renderer keeps every other model's slots.
func TestServer_HandleAPISlots_ChildDown(t *testing.T) {
	local := newStubRouter(nil, "")
	local.running = map[string]process.ProcessState{"m-down": process.StateReady}
	s := newTestServer(local, newStubRouter(nil, ""))
	// A proxy pointing at a closed port: the fetch must fail fast.
	s.cfg = config.Config{Models: map[string]config.ModelConfig{
		"m-down": {Proxy: "http://127.0.0.1:1", ConcurrencyLimit: 2},
	}}

	req := httptest.NewRequest(http.MethodGet, "/api/slots", nil)
	w := httptest.NewRecorder()
	s.handleAPISlots(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even with a dead child", w.Code)
	}
	var body struct {
		Models []apiModelSlots `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Models) != 1 || body.Models[0].Error == "" {
		t.Errorf("models = %+v, want one model with a fetch error", body.Models)
	}
}
