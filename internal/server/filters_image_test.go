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

// runImageFilter pushes one JSON body through CreateFilterMiddleware for the
// given model config and returns the body the downstream handler received.
func runImageFilter(t *testing.T, mc config.ModelConfig, path, reqJSON string) []byte {
	t.Helper()
	cfg := config.Config{Models: map[string]config.ModelConfig{"m": mc}}
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(reqJSON))
	r.Header.Set("Content-Type", "application/json")
	var got []byte
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
	})
	CreateFilterMiddleware(cfg)(final).ServeHTTP(httptest.NewRecorder(), r)
	return got
}

// A model whose declared capabilities do not include image input cannot be
// handed an image block: llama-server without an mmproj answers
// `500 image input is not supported - hint: ... you may need to provide the
// mmproj`, and because the block then sits in the client's history every
// retry fails the same way (a Claude Code cq27 session that Read a screenshot
// was wedged this way on 2026-09-06). The proxy replaces such blocks with a
// text placeholder so the request can proceed and the client can recover.
func TestServer_FilterMiddleware_ImageBlocksReplacedForTextOnlyModel(t *testing.T) {
	textOnly := config.ModelConfig{Capabilities: config.ModelCapConfig{Context: 262144}}

	t.Run("anthropic image block incl. nested tool_result", func(t *testing.T) {
		body := `{"model":"m","messages":[
			{"role":"user","content":[
				{"type":"text","text":"look"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"t1","content":[
					{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}
				]}
			]}
		]}`
		got := runImageFilter(t, textOnly, "/v1/messages", body)
		if strings.Contains(string(got), `"type":"image"`) {
			t.Fatalf("image block still present in downstream body: %s", got)
		}
		if typ := gjson.GetBytes(got, "messages.0.content.1.type").String(); typ != "text" {
			t.Errorf("messages.0.content.1.type = %q, want text", typ)
		}
		if txt := gjson.GetBytes(got, "messages.0.content.1.text").String(); !strings.Contains(txt, "image omitted") {
			t.Errorf("placeholder text = %q, want it to say the image was omitted", txt)
		}
		if typ := gjson.GetBytes(got, "messages.1.content.0.content.0.type").String(); typ != "text" {
			t.Errorf("nested tool_result image type = %q, want text", typ)
		}
		if txt := gjson.GetBytes(got, "messages.0.content.0.text").String(); txt != "look" {
			t.Errorf("sibling text block changed: %q", txt)
		}
	})

	t.Run("openai image_url block", func(t *testing.T) {
		body := `{"model":"m","messages":[{"role":"user","content":[
			{"type":"text","text":"look"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}
		]}]}`
		got := runImageFilter(t, textOnly, "/v1/chat/completions", body)
		if strings.Contains(string(got), `"type":"image_url"`) {
			t.Fatalf("image_url block still present in downstream body: %s", got)
		}
		if typ := gjson.GetBytes(got, "messages.0.content.1.type").String(); typ != "text" {
			t.Errorf("messages.0.content.1.type = %q, want text", typ)
		}
	})

	t.Run("vision model keeps image blocks", func(t *testing.T) {
		vision := config.ModelConfig{Capabilities: config.ModelCapConfig{In: []string{"text", "image"}, Out: []string{"text"}}}
		body := `{"model":"m","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"x"}}]}]}`
		got := runImageFilter(t, vision, "/v1/messages", body)
		if !strings.Contains(string(got), `"type":"image"`) {
			t.Fatalf("vision model lost its image block: %s", got)
		}
	})

	t.Run("undeclared capabilities pass through", func(t *testing.T) {
		body := `{"model":"m","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"x"}}]}]}`
		got := runImageFilter(t, config.ModelConfig{}, "/v1/messages", body)
		if !strings.Contains(string(got), `"type":"image"`) {
			t.Fatalf("model with no declared capabilities lost its image block: %s", got)
		}
	})

	t.Run("string content untouched", func(t *testing.T) {
		body := `{"model":"m","messages":[{"role":"user","content":"plain image talk"}]}`
		got := runImageFilter(t, textOnly, "/v1/messages", body)
		if s := gjson.GetBytes(got, "messages.0.content").String(); s != "plain image talk" {
			t.Errorf("string content changed: %q", s)
		}
	})
}
