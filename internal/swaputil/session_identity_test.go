package swaputil

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// TestSessionMetadata_ClientFamily pins the closed set of client families a
// queue row may show. The session header is decisive; everything else is a
// best-effort User-Agent guess that must land on "other" rather than leaking
// a raw agent string into the UI.
func TestSessionMetadata_ClientFamily(t *testing.T) {
	const uuid = "cf006070-986b-4150-ac78-308a899c3321"

	cases := []struct {
		name      string
		userAgent string
		sessionID string
		want      string
	}{
		{"session header wins over any agent", "curl/8.7.1", uuid, "claude-code"},
		{"claude cli agent", "claude-cli/2.1.252 (external, cli)", "", "claude-code"},
		{"python sdk", "Anthropic/Python 0.40.0", "", "python-sdk"},
		{"curl", "curl/8.7.1", "", "curl"},
		{"hermes any case", "Hermes-Desktop/1.4", "", "hermes"},
		{"unknown agent", "Mozilla/5.0", "", "other"},
		{"no agent at all", "", "", "other"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, _ := http.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
			r.Header.Set("Content-Type", "application/json")
			if c.userAgent != "" {
				r.Header.Set("User-Agent", c.userAgent)
			}
			if c.sessionID != "" {
				r.Header.Set("X-Claude-Code-Session-Id", c.sessionID)
			}
			got, err := extractContext(r)
			if err != nil {
				t.Fatalf("extractContext: %v", err)
			}
			if got.Metadata["client"] != c.want {
				t.Errorf("client = %q, want %q", got.Metadata["client"], c.want)
			}
		})
	}
}

// TestSessionMetadata_ClientFamilyFromUserID covers the fallback identity
// channel: a session id recovered from metadata.user_id (older CLI builds)
// does not set the session HEADER, so the family must come from the agent -
// which for those builds is still claude-cli.
func TestSessionMetadata_ClientFamilyFromUserID(t *testing.T) {
	body := `{"model":"m","metadata":{"user_id":"user_abc_account_def_session_11111111-1111-1111-1111-111111111111"}}`
	r, _ := http.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("User-Agent", "claude-cli/2.0.0")

	got, err := extractContext(r)
	if err != nil {
		t.Fatalf("extractContext: %v", err)
	}
	if got.Metadata["session_id"] != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("session_id = %q, want the user_id segment", got.Metadata["session_id"])
	}
	if got.Metadata["client"] != "claude-code" {
		t.Errorf("client = %q, want %q", got.Metadata["client"], "claude-code")
	}
}

// TestSessionMetadata_ParentSessionID pins the dispatch-parent channel: a full
// uuid and the short 8-hex form a dispatcher may only have are both accepted,
// anything else is dropped rather than shown verbatim.
func TestSessionMetadata_ParentSessionID(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   string
	}{
		{"full uuid", "cf006070-986b-4150-ac78-308a899c3321", "cf006070-986b-4150-ac78-308a899c3321"},
		{"short 8-hex form", "cf006070", "cf006070"},
		{"longer hex run", "cf006070986b", "cf006070986b"},
		{"too short", "cf0060", ""},
		{"hostile value", "'; DROP TABLE sessions;--", ""},
		{"absent", "", ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, _ := http.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
			r.Header.Set("Content-Type", "application/json")
			if c.header != "" {
				r.Header.Set("X-Claude-Code-Parent-Session-Id", c.header)
			}
			got, err := extractContext(r)
			if err != nil {
				t.Fatalf("extractContext: %v", err)
			}
			if c.want == "" {
				if v, ok := got.Metadata["parent_session_id"]; ok {
					t.Errorf("parent_session_id = %q, want key absent", v)
				}
				return
			}
			if got.Metadata["parent_session_id"] != c.want {
				t.Errorf("parent_session_id = %q, want %q", got.Metadata["parent_session_id"], c.want)
			}
		})
	}
}

// TestSessionMetadata_ParentSessionIDOnGET checks the parent channel works on
// a body-less request too - the header check needs no body.
func TestSessionMetadata_ParentSessionIDOnGET(t *testing.T) {
	r, _ := http.NewRequest(http.MethodGet, "/v1/models?model=m", nil)
	r.Header.Set("X-Claude-Code-Parent-Session-Id", "cf006070")

	got, err := extractContext(r)
	if err != nil {
		t.Fatalf("extractContext: %v", err)
	}
	if got.Metadata["parent_session_id"] != "cf006070" {
		t.Errorf("parent_session_id = %q, want %q", got.Metadata["parent_session_id"], "cf006070")
	}
}

// TestShortestAlias covers the display-name rule: shortest alias wins, ties
// break lexicographically so a reload cannot flip the answer, and a model
// with no alias falls back to its own id.
func TestShortestAlias(t *testing.T) {
	cfg := config.Config{
		Models: map[string]config.ModelConfig{
			"Qwen3-35B-A3B-Q4":  {Aliases: []string{"qwen35-instruct", "cq35", "cq35h"}},
			"tie":               {Aliases: []string{"zzzz", "aaaa"}},
			"no-aliases":        {},
			"empty-alias-entry": {Aliases: []string{"", "cq27"}},
		},
	}

	cases := []struct {
		modelID string
		want    string
	}{
		{"Qwen3-35B-A3B-Q4", "cq35"},
		{"tie", "aaaa"},
		{"no-aliases", "no-aliases"},
		{"empty-alias-entry", "cq27"},
		{"not-in-config", "not-in-config"},
	}

	for _, c := range cases {
		t.Run(c.modelID, func(t *testing.T) {
			if got := ShortestAlias(cfg, c.modelID); got != c.want {
				t.Errorf("ShortestAlias(%q) = %q, want %q", c.modelID, got, c.want)
			}
		})
	}
}

// TestFetchContext_StampsModelAlias pins that the alias reaches the metadata
// bag every consumer reads (in-flight entries, activity log), for a request
// that names the model by alias and for one that names the id outright.
func TestFetchContext_StampsModelAlias(t *testing.T) {
	cfg := config.Config{
		Models: map[string]config.ModelConfig{
			"Qwen3-35B-A3B-Q4": {Aliases: []string{"qwen35-instruct", "cq35"}},
			"plain":            {},
		},
	}

	t.Run("requested by id", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"Qwen3-35B-A3B-Q4"}`))
		r.Header.Set("Content-Type", "application/json")
		data, err := FetchContext(r, cfg)
		if err != nil {
			t.Fatalf("FetchContext: %v", err)
		}
		if data.Metadata["model_alias"] != "cq35" {
			t.Errorf("model_alias = %q, want %q", data.Metadata["model_alias"], "cq35")
		}
	})

	// Requesting by ALIAS only resolves through the loader-built alias map
	// (config.aliases is populated by LoadConfigFromReader, not by a
	// hand-built Config), so this case loads real YAML: the alias the caller
	// typed must be resolved to the model id first and the row must then show
	// the SHORTEST alias, not whichever one was typed.
	t.Run("requested by alias resolves then shortens", func(t *testing.T) {
		loaded, err := config.LoadConfigFromReader(strings.NewReader(`
models:
  Qwen3-35B-A3B-Q4:
    cmd: echo ${PORT}
    aliases:
      - qwen35-instruct
      - cq35
`))
		if err != nil {
			t.Fatalf("LoadConfigFromReader: %v", err)
		}
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen35-instruct"}`))
		r.Header.Set("Content-Type", "application/json")
		data, err := FetchContext(r, loaded)
		if err != nil {
			t.Fatalf("FetchContext: %v", err)
		}
		if data.ModelID != "Qwen3-35B-A3B-Q4" {
			t.Fatalf("ModelID = %q, want the alias resolved to the model id", data.ModelID)
		}
		if data.Metadata["model_alias"] != "cq35" {
			t.Errorf("model_alias = %q, want %q", data.Metadata["model_alias"], "cq35")
		}
	})

	t.Run("model without aliases falls back to its id", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"plain"}`))
		r.Header.Set("Content-Type", "application/json")
		data, err := FetchContext(r, cfg)
		if err != nil {
			t.Fatalf("FetchContext: %v", err)
		}
		if data.Metadata["model_alias"] != "plain" {
			t.Errorf("model_alias = %q, want %q", data.Metadata["model_alias"], "plain")
		}
	})

	t.Run("upstream path", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/upstream/Qwen3-35B-A3B-Q4/v1/chat/completions", strings.NewReader(`{}`))
		data, err := FetchContext(r, cfg)
		if err != nil {
			t.Fatalf("FetchContext: %v", err)
		}
		if data.Metadata["model_alias"] != "cq35" {
			t.Errorf("model_alias = %q, want %q", data.Metadata["model_alias"], "cq35")
		}
	})
}

// TestSessionMetadata_AgentID pins the subagent channel: Claude Code sends
// X-Claude-Code-Agent-Id on every request an Agent-tool subagent makes, while
// reusing the PARENT's X-Claude-Code-Session-Id. Without this key a renderer
// cannot tell a subagent's turn from its parent's, and slot affinity keys
// both onto one lane (witnessed 2026-09-08 on the cq35h dogfood session
// 934b47c6: agent a4c4e94e633cf6841). Hex runs of at least 8 characters are
// accepted (live ids are 17 hex); anything else is dropped, never shown.
func TestSessionMetadata_AgentID(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   string
	}{
		{"live 17-hex id", "a4c4e94e633cf6841", "a4c4e94e633cf6841"},
		{"short 8-hex form", "a4c4e94e", "a4c4e94e"},
		{"too short", "a4c4e9", ""},
		{"hostile value", "'; DROP TABLE agents;--", ""},
		{"absent", "", ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, _ := http.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-Claude-Code-Session-Id", "cf006070-986b-4150-ac78-308a899c3321")
			if c.header != "" {
				r.Header.Set("X-Claude-Code-Agent-Id", c.header)
			}
			got, err := extractContext(r)
			if err != nil {
				t.Fatalf("extractContext: %v", err)
			}
			if c.want == "" {
				if v, ok := got.Metadata["agent_id"]; ok {
					t.Errorf("agent_id = %q, want key absent", v)
				}
				return
			}
			if got.Metadata["agent_id"] != c.want {
				t.Errorf("agent_id = %q, want %q", got.Metadata["agent_id"], c.want)
			}
			if got.Metadata["session_id"] != "cf006070-986b-4150-ac78-308a899c3321" {
				t.Errorf("session_id must stay the parent's, got %q", got.Metadata["session_id"])
			}
		})
	}
}
