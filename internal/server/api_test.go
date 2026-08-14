package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

func TestServer_HandleListModels(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	s.cfg = config.Config{
		Models: map[string]config.ModelConfig{
			"visible": {Name: "Visible", Description: "a model"},
			"hidden":  {Unlisted: true},
		},
		Peers: config.PeerDictionaryConfig{
			"peer1": {Models: []string{"remote-model"}},
		},
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Origin", "http://example.com")
	s.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "http://example.com" {
		t.Errorf("Access-Control-Allow-Origin = %q", got)
	}

	var resp struct {
		Data []modelRecord `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ids := map[string]bool{}
	for _, m := range resp.Data {
		ids[m.ID] = true
	}
	if !ids["visible"] || !ids["peer1/remote-model"] {
		t.Errorf("missing expected models: %v", ids)
	}
	if ids["hidden"] {
		t.Error("unlisted model should not appear")
	}
}

func TestServer_HandleListModels_PeerNamespaces(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	s.cfg = config.Config{
		Models: map[string]config.ModelConfig{"shared": {}},
		Peers: config.PeerDictionaryConfig{
			"cuda":  {Models: []string{"shared"}},
			"strix": {Models: []string{"shared"}},
		},
	}

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	var resp struct {
		Data []modelRecord `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ids := make(map[string]bool)
	for _, model := range resp.Data {
		ids[model.ID] = true
	}
	for _, id := range []string{"shared", "cuda/shared", "strix/shared"} {
		if !ids[id] {
			t.Errorf("missing model %q from %v", id, ids)
		}
	}
	if len(ids) != 3 {
		t.Errorf("model IDs = %v, want exactly local and two qualified peers", ids)
	}

	statusIDs := make(map[string]bool)
	for _, model := range s.modelStatus() {
		statusIDs[model.Id] = true
	}
	for _, id := range []string{"shared", "cuda/shared", "strix/shared"} {
		if !statusIDs[id] {
			t.Errorf("missing status model %q from %v", id, statusIDs)
		}
	}
}

func TestServer_HandleListModels_Aliases(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	s.cfg = config.Config{
		IncludeAliasesInList: true,
		Models: map[string]config.ModelConfig{
			"real": {Aliases: []string{"nick"}},
		},
	}

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	var resp struct {
		Data []modelRecord `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	ids := map[string]bool{}
	for _, m := range resp.Data {
		ids[m.ID] = true
	}
	if !ids["real"] || !ids["nick"] {
		t.Errorf("expected alias entry; got %v", ids)
	}
}

func TestServer_HandleListModels_Status(t *testing.T) {
	local := newStubRouter(nil, "")
	local.running = map[string]process.ProcessState{"loaded-model": process.StateReady}
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{
		IncludeAliasesInList: true,
		Models: map[string]config.ModelConfig{
			"loaded-model":   {Aliases: []string{"loaded-alias"}},
			"unloaded-model": {},
		},
		Peers: config.PeerDictionaryConfig{
			"peer1": {Models: []string{"remote-model"}},
		},
	}

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	var resp struct {
		Data []modelRecord `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	statuses := map[string]string{}
	for _, m := range resp.Data {
		statuses[m.ID], _ = m.Status["value"].(string)
	}

	if statuses["loaded-model"] != "loaded" {
		t.Errorf("loaded-model status = %q, want loaded", statuses["loaded-model"])
	}
	if statuses["loaded-alias"] != "loaded" {
		t.Errorf("loaded-alias status = %q, want loaded", statuses["loaded-alias"])
	}
	if statuses["unloaded-model"] != "unloaded" {
		t.Errorf("unloaded-model status = %q, want unloaded", statuses["unloaded-model"])
	}
	if statuses["peer1/remote-model"] != "unloaded" {
		t.Errorf("peer1/remote-model status = %q, want unloaded", statuses["peer1/remote-model"])
	}
}

func TestServer_FindModelInPath(t *testing.T) {
	cfg := config.Config{Models: map[string]config.ModelConfig{
		"author":       {},
		"author/model": {},
		"simple":       {},
	}}

	cases := []struct {
		path      string
		wantName  string
		wantRem   string
		wantFound bool
	}{
		{"/simple/v1/chat", "simple", "/v1/chat", true},
		{"/author/model/v1/chat", "author/model", "/v1/chat", true},
		{"/author/model", "author/model", "/", true},
		{"/author/v1/chat", "author", "/v1/chat", true},
		{"/missing/v1", "", "", false},
		{"/", "", "", false},
	}
	for _, c := range cases {
		name, _, rem, found := swaputil.FindModelInPath(cfg, c.path)
		if found != c.wantFound || name != c.wantName || (found && rem != c.wantRem) {
			t.Errorf("FindModelInPath(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.path, name, rem, found, c.wantName, c.wantRem, c.wantFound)
		}
	}
}

func TestServer_HandleUpstream(t *testing.T) {
	local := newStubRouter([]string{"m1"}, "upstream-body")
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"m1": {}}}

	t.Run("proxies to local", func(t *testing.T) {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/upstream/m1/v1/chat", nil))
		if w.Code != http.StatusOK || w.Body.String() != "upstream-body" {
			t.Errorf("status=%d body=%q", w.Code, w.Body.String())
		}
	})

	t.Run("redirects bare model path", func(t *testing.T) {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/upstream/m1", nil))
		if w.Code != http.StatusMovedPermanently {
			t.Errorf("status = %d, want 301", w.Code)
		}
	})

	t.Run("unknown model 404", func(t *testing.T) {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/upstream/nope/v1", nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", w.Code)
		}
	})
}

func TestServer_HandleComfyUI(t *testing.T) {
	local := newStubRouter([]string{config.ComfyUIModelID}, "")
	var gotPath string
	var gotQuery string
	var gotContext swaputil.ReqContextData
	serveCalls := 0
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		serveCalls++
		gotPath = r.URL.EscapedPath()
		gotQuery = r.URL.RawQuery
		gotContext, _ = swaputil.ReadContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{config.ComfyUIModelID: {}}}
	s.routes()

	t.Run("redirects bare path", func(t *testing.T) {
		for _, tt := range []struct {
			method string
			status int
		}{
			{method: http.MethodGet, status: http.StatusMovedPermanently},
			{method: http.MethodHead, status: http.StatusMovedPermanently},
			{method: http.MethodPost, status: http.StatusPermanentRedirect},
		} {
			w := httptest.NewRecorder()
			s.ServeHTTP(w, httptest.NewRequest(tt.method, "/comfyui?token=value", nil))
			if w.Code != tt.status {
				t.Errorf("%s status=%d want %d", tt.method, w.Code, tt.status)
			}
			if got := w.Header().Get("Location"); got != "/comfyui/?token=value" {
				t.Errorf("%s Location=%q want /comfyui/?token=value", tt.method, got)
			}
		}
	})

	t.Run("only root starts unloaded model", func(t *testing.T) {
		local.running = nil

		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/comfyui/?token=value", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("root status=%d want 200 body=%q", w.Code, w.Body.String())
		}
		if gotPath != "/" || gotQuery != "token=value" {
			t.Errorf("root path=%q query=%q want path=/ query=token=value", gotPath, gotQuery)
		}

		before := serveCalls
		w = httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/comfyui/api/prompt", nil))
		if w.Code != http.StatusConflict {
			t.Fatalf("subpath status=%d want 409 body=%q", w.Code, w.Body.String())
		}
		if serveCalls != before {
			t.Fatal("unloaded model received a non-root ComfyUI request")
		}
		if !strings.Contains(w.Body.String(), "only /comfyui/ can start it") {
			t.Errorf("body=%q missing root-path explanation", w.Body.String())
		}

		local.running = map[string]process.ProcessState{config.ComfyUIModelID: process.StateStarting}
		w = httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/comfyui/api/prompt", nil))
		if w.Code != http.StatusConflict {
			t.Fatalf("starting model status=%d want 409 body=%q", w.Code, w.Body.String())
		}
		if serveCalls != before {
			t.Fatal("starting model received a non-root ComfyUI request")
		}
	})

	t.Run("proxies fixed model and preserves escaped path", func(t *testing.T) {
		local.running = map[string]process.ProcessState{config.ComfyUIModelID: process.StateReady}
		for _, tt := range []struct {
			name      string
			target    string
			wantPath  string
			wantQuery string
		}{
			{
				name:      "encoded slash",
				target:    "/comfyui/api/userdata/workflows%2Fexample.json?preview=1",
				wantPath:  "/api/userdata/workflows%2Fexample.json",
				wantQuery: "preview=1",
			},
			{
				name:     "double encoded slash",
				target:   "/comfyui/api/userdata/workflows%252Fexample.json",
				wantPath: "/api/userdata/workflows%252Fexample.json",
			},
			{
				name:     "utf8 and encoded slash",
				target:   "/comfyui/api/%E2%9C%93%2Ffile.json",
				wantPath: "/api/%E2%9C%93%2Ffile.json",
			},
		} {
			t.Run(tt.name, func(t *testing.T) {
				w := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, tt.target, nil)
				s.ServeHTTP(w, req)
				if w.Code != http.StatusOK {
					t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
				}
				if gotPath != tt.wantPath {
					t.Errorf("path=%q want %q", gotPath, tt.wantPath)
				}
				if gotQuery != tt.wantQuery {
					t.Errorf("query=%q want %q", gotQuery, tt.wantQuery)
				}
			})
		}
		if gotContext.Model != config.ComfyUIModelID || gotContext.ModelID != config.ComfyUIModelID {
			t.Errorf("context=%+v want model %s", gotContext, config.ComfyUIModelID)
		}
	})
}

func TestServer_HandleComfyUI_RequiresExactLocalModel(t *testing.T) {
	tests := []struct {
		name  string
		cfg   config.Config
		local *stubRouter
		peer  *stubRouter
	}{
		{
			name:  "missing model",
			cfg:   config.Config{},
			local: newStubRouter(nil, ""),
			peer:  newStubRouter(nil, ""),
		},
		{
			name:  "peer model",
			cfg:   config.Config{Models: map[string]config.ModelConfig{}},
			local: newStubRouter(nil, ""),
			peer:  newStubRouter([]string{config.ComfyUIModelID}, "peer"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(tt.local, tt.peer)
			s.cfg = tt.cfg
			s.routes()
			w := httptest.NewRecorder()
			s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/comfyui/", nil))
			if w.Code != http.StatusNotFound {
				t.Fatalf("status=%d want 404 body=%q", w.Code, w.Body.String())
			}
		})
	}
}

func TestServer_HandleComfyUI_UsesAuthentication(t *testing.T) {
	local := newStubRouter([]string{config.ComfyUIModelID}, "ok")
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{
		RequiredAPIKeys: []string{"secret"},
		Models:          map[string]config.ModelConfig{config.ComfyUIModelID: {}},
	}
	s.routes()

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/comfyui/", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d want 401", w.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/comfyui/", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("authenticated status=%d want 200 body=%q", w.Code, w.Body.String())
	}
}

func TestProxy_HandleUpstreamPreservesEscapedPath(t *testing.T) {
	tests := []struct {
		name   string
		models []string
		target string
		want   string
	}{
		{
			name:   "encoded slash in resource path",
			models: []string{"m1"},
			target: "/upstream/m1/api/userdata/workflows%2Fexample.json",
			want:   "/api/userdata/workflows%2Fexample.json",
		},
		{
			name:   "multi-segment model name",
			models: []string{"author/model"},
			target: "/upstream/author/model/api/x%2Fy",
			want:   "/api/x%2Fy",
		},
		{
			name:   "encoded separator in multi-segment model name",
			models: []string{"author/model"},
			target: "/upstream/author%2Fmodel/api/x%2Fy",
			want:   "/api/x%2Fy",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			local := newStubRouter(tt.models, "")
			var got string
			local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
				got = r.URL.EscapedPath()
				w.WriteHeader(http.StatusOK)
			}
			s := newTestServer(local, newStubRouter(nil, ""))
			models := make(map[string]config.ModelConfig, len(tt.models))
			for _, model := range tt.models {
				models[model] = config.ModelConfig{}
			}
			s.cfg = config.Config{Models: models}
			s.routes()

			w := httptest.NewRecorder()
			s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, tt.target, nil))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body=%q)", w.Code, w.Body.String())
			}
			if got != tt.want {
				t.Errorf("upstream escaped path = %q, want %q", got, tt.want)
			}
		})
	}
}

func upstreamMetricsServer(t *testing.T, response string) *Server {
	t.Helper()
	cfg := config.Config{Models: map[string]config.ModelConfig{"m1": {}}}
	proxylog := logmon.NewWriter(io.Discard)
	s := &Server{
		cfg:         cfg,
		muxlog:      logmon.NewWriter(io.Discard),
		proxylog:    proxylog,
		upstreamlog: logmon.NewWriter(io.Discard),
		inflight:    newInflightTracker(),
		metrics:     newTestMetricsMonitor(t, proxylog, 10, 0),
		local:       newStubRouter([]string{"m1"}, response),
		peer:        newStubRouter(nil, ""),
	}
	s.routes()
	return s
}

func TestServer_HandleUpstream_IgnorePaths(t *testing.T) {
	// Compile a pattern that matches static asset suffixes.
	pattern := regexp.MustCompile(`.*\.(js|json|css|png|gif|jpg|jpeg|txt)$`)

	t.Run("matched path, model not loaded, returns 409", func(t *testing.T) {
		local := newStubRouter([]string{"m1"}, "upstream-body")
		// running is nil/empty: model is not in RunningModels() => not loaded.
		s := newTestServer(local, newStubRouter(nil, ""))
		s.cfg = config.Config{
			Models: map[string]config.ModelConfig{"m1": {}},
			Upstream: config.UpstreamConfig{
				IgnorePaths: []*regexp.Regexp{pattern},
			},
		}

		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/upstream/m1/foo.js", nil))

		if w.Code != http.StatusConflict {
			t.Fatalf("status = %d, want %d (body=%q)", w.Code, http.StatusConflict, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "not loaded") {
			t.Errorf("body = %q, want it to contain 'not loaded'", w.Body.String())
		}
	})

	t.Run("matched path, model already loaded, serves normally", func(t *testing.T) {
		local := newStubRouter([]string{"m1"}, "upstream-body")
		local.running = map[string]process.ProcessState{"m1": process.StateReady}
		s := newTestServer(local, newStubRouter(nil, ""))
		s.cfg = config.Config{
			Models: map[string]config.ModelConfig{"m1": {}},
			Upstream: config.UpstreamConfig{
				IgnorePaths: []*regexp.Regexp{pattern},
			},
		}

		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/upstream/m1/foo.js", nil))

		if w.Code != http.StatusOK || w.Body.String() != "upstream-body" {
			t.Fatalf("status=%d body=%q, want 200 'upstream-body'", w.Code, w.Body.String())
		}
	})

	t.Run("non-matched path, model not loaded, serves normally", func(t *testing.T) {
		local := newStubRouter([]string{"m1"}, "upstream-body")
		s := newTestServer(local, newStubRouter(nil, ""))
		s.cfg = config.Config{
			Models: map[string]config.ModelConfig{"m1": {}},
			Upstream: config.UpstreamConfig{
				IgnorePaths: []*regexp.Regexp{pattern},
			},
		}

		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/upstream/m1/v1/chat/completions", nil))

		if w.Code != http.StatusOK || w.Body.String() != "upstream-body" {
			t.Fatalf("status=%d body=%q, want 200 'upstream-body'", w.Code, w.Body.String())
		}
	})

	t.Run("matched path, peer model, serves normally", func(t *testing.T) {
		// Peer routers do not appear via RunningModels on the local router;
		// they should fall through to normal dispatch without 409.
		local := newStubRouter(nil, "")
		peer := newStubRouter([]string{"m1"}, "peer-body")
		s := newTestServer(local, peer)
		s.cfg = config.Config{
			Models: map[string]config.ModelConfig{"m1": {}},
			Upstream: config.UpstreamConfig{
				IgnorePaths: []*regexp.Regexp{pattern},
			},
		}

		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/upstream/m1/foo.js", nil))

		if w.Code != http.StatusOK || w.Body.String() != "peer-body" {
			t.Fatalf("status=%d body=%q, want 200 'peer-body'", w.Code, w.Body.String())
		}
	})
}

func TestServer_HandleUpstream_MetricsRecordsSupportedPath(t *testing.T) {
	resp := `{"usage":{"prompt_tokens":3,"completion_tokens":5}}`
	s := upstreamMetricsServer(t, resp)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/upstream/m1/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	s.ServeHTTP(w, req)

	if w.Code != http.StatusOK || w.Body.String() != resp {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	entries := metricsEntries(t, s.metrics)
	if len(entries) != 1 {
		t.Fatalf("want 1 metrics entry, got %d", len(entries))
	}
	if entries[0].Model != "m1" {
		t.Errorf("model = %q, want m1", entries[0].Model)
	}
	if entries[0].ReqPath != "/v1/chat/completions" {
		t.Errorf("req_path = %q, want /v1/chat/completions", entries[0].ReqPath)
	}
	if entries[0].Tokens.InputTokens != 3 || entries[0].Tokens.OutputTokens != 5 {
		t.Errorf("tokens = %+v, want input=3 output=5", entries[0].Tokens)
	}
}

func TestServer_HandleUpstream_MetricsSkipsUnsupportedPath(t *testing.T) {
	s := upstreamMetricsServer(t, "ok")

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/upstream/m1/probe", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	s.ServeHTTP(w, req)

	if w.Code != http.StatusOK || w.Body.String() != "ok" {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if len(metricsEntries(t, s.metrics)) != 0 {
		t.Errorf("want no metrics entries for unsupported path, got %d", len(metricsEntries(t, s.metrics)))
	}
}

func TestServer_HandleUpstream_MetricsSkipsGET(t *testing.T) {
	s := upstreamMetricsServer(t, `{"usage":{}}`)

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/upstream/m1/v1/chat/completions", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	if len(metricsEntries(t, s.metrics)) != 0 {
		t.Errorf("want no metrics entries for GET upstream, got %d", len(metricsEntries(t, s.metrics)))
	}
}

func TestServer_HandleUpstream_InflightTracksSupportedPaths(t *testing.T) {
	for _, tc := range []struct {
		name     string
		method   string
		path     string
		wantPath string
	}{
		{
			name:     "post inference",
			method:   http.MethodPost,
			path:     "/upstream/m1/v1/chat/completions",
			wantPath: "/v1/chat/completions",
		},
		{
			name:     "get model endpoint",
			method:   http.MethodGet,
			path:     "/upstream/m1/props",
			wantPath: "/props",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local := newStubRouter([]string{"m1"}, "ok")
			var s *Server
			var during swaputil.InFlightRequestsEvent
			local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
				during = s.inflight.Current()
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("ok"))
			}
			s = upstreamInflightServer(t, local, config.ModelConfig{})

			w := httptest.NewRecorder()
			s.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`)))
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
			}
			if len(during.Requests) != 1 {
				t.Fatalf("inflight during request = %+v, want 1 request", during)
			}
			entry := during.Requests[0]
			if entry.Model != "m1" || entry.Method != tc.method || entry.ReqPath != tc.wantPath {
				t.Errorf("inflight entry = %+v, want model=m1 method=%s path=%s", entry, tc.method, tc.wantPath)
			}
			if got := s.inflight.Current(); len(got.Requests) != 0 {
				t.Errorf("inflight after request = %d, want 0", len(got.Requests))
			}
		})
	}
}

func TestServer_HandleUpstream_InflightSkipsUnsupportedPath(t *testing.T) {
	local := newStubRouter([]string{"m1"}, "ok")
	var s *Server
	var during swaputil.InFlightRequestsEvent
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		during = s.inflight.Current()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}
	s = upstreamInflightServer(t, local, config.ModelConfig{})

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/upstream/m1/probe", strings.NewReader(`{}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if len(during.Requests) != 0 {
		t.Fatalf("inflight during unsupported path = %+v, want empty", during)
	}
}

func TestServer_HandleUpstream_InflightIgnoresConfiguredWebsocket(t *testing.T) {
	local := newStubRouter([]string{"m1"}, "ok")
	var s *Server
	var during swaputil.InFlightRequestsEvent
	local.serveHTTP = func(w http.ResponseWriter, _ *http.Request) {
		during = s.inflight.Current()
		w.WriteHeader(http.StatusSwitchingProtocols)
	}
	s = upstreamInflightServer(t, local, config.ModelConfig{
		Compat: config.CompatConfig{IgnoreWebsockets: true},
	})

	req := httptest.NewRequest(http.MethodGet, "/upstream/m1/props", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	if len(during.Requests) != 0 {
		t.Fatalf("inflight during ignored websocket = %+v, want empty", during)
	}
}

func upstreamInflightServer(t *testing.T, local *stubRouter, mc config.ModelConfig) *Server {
	t.Helper()
	cfg := config.Config{Models: map[string]config.ModelConfig{"m1": mc}}
	proxylog := logmon.NewWriter(io.Discard)
	s := &Server{
		cfg:         cfg,
		muxlog:      logmon.NewWriter(io.Discard),
		proxylog:    proxylog,
		upstreamlog: logmon.NewWriter(io.Discard),
		inflight:    newInflightTracker(),
		metrics:     newTestMetricsMonitor(t, proxylog, 10, 0),
		local:       local,
		peer:        newStubRouter(nil, ""),
	}
	s.routes()
	return s
}

func TestServer_HandleMetrics_Unavailable(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestServer_Redirects(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))

	for path, want := range map[string]string{"/": "/ui", "/upstream": "/ui/models"} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusFound {
			t.Errorf("%s: status = %d, want 302", path, w.Code)
		}
		if got := w.Header().Get("Location"); got != want {
			t.Errorf("%s: Location = %q, want %q", path, got, want)
		}
	}
}

func TestServer_HandleListModels_Capabilities(t *testing.T) {
	newServer := func(mc config.ModelConfig) *Server {
		s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
		s.cfg = config.Config{Models: map[string]config.ModelConfig{"m": mc}}
		return s
	}
	getModel := func(t *testing.T, s *Server) modelRecord {
		t.Helper()
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
		var resp struct {
			Data []modelRecord `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.Data) != 1 {
			t.Fatalf("expected 1 model, got %d", len(resp.Data))
		}
		return resp.Data[0]
	}

	t.Run("all_fields", func(t *testing.T) {
		m := getModel(t, newServer(config.ModelConfig{
			Capabilities: config.ModelCapConfig{
				In:      []string{"text", "image"},
				Out:     []string{"text", "audio"},
				Tools:   true,
				Context: 100000,
			},
		}))
		if m.Architecture == nil {
			t.Fatal("architecture is nil")
		}
		if !anySliceStrEqual(m.Architecture["input_modalities"], []string{"text", "image"}) {
			t.Errorf("input_modalities = %v", m.Architecture["input_modalities"])
		}
		if !anySliceStrEqual(m.Architecture["output_modalities"], []string{"text", "audio"}) {
			t.Errorf("output_modalities = %v", m.Architecture["output_modalities"])
		}
		if m.Architecture["modality"] != "text+image->text+audio" {
			t.Errorf("modality = %v", m.Architecture["modality"])
		}
		if m.Capabilities == nil || m.Capabilities["vision"] != true {
			t.Errorf("vision = %v", m.Capabilities)
		}
		if m.Capabilities["audio_speech"] != true {
			t.Errorf("audio_speech = %v", m.Capabilities["audio_speech"])
		}
		if m.Capabilities["function_calling"] != true {
			t.Errorf("function_calling = %v", m.Capabilities["function_calling"])
		}
		if !stringSliceEqual(m.SupportedParameters, []string{"tools", "tool_choice"}) {
			t.Errorf("supported_parameters = %v", m.SupportedParameters)
		}
		if m.ContextLength != 100000 {
			t.Errorf("context_length = %d", m.ContextLength)
		}
	})

	t.Run("in_only", func(t *testing.T) {
		m := getModel(t, newServer(config.ModelConfig{
			Capabilities: config.ModelCapConfig{In: []string{"text", "image"}},
		}))
		if m.Architecture == nil {
			t.Fatal("architecture is nil")
		}
		if _, ok := m.Architecture["output_modalities"]; ok {
			t.Error("should not have output_modalities")
		}
		if _, ok := m.Architecture["modality"]; ok {
			t.Error("should not have modality")
		}
		if m.Capabilities == nil || m.Capabilities["vision"] != true {
			t.Error("expected vision: true")
		}
		if m.SupportedParameters != nil {
			t.Error("should not have supported_parameters")
		}
		if m.ContextLength != 0 {
			t.Error("should not have context_length")
		}
	})

	t.Run("out_only", func(t *testing.T) {
		m := getModel(t, newServer(config.ModelConfig{
			Capabilities: config.ModelCapConfig{Out: []string{"audio"}},
		}))
		if m.Architecture == nil {
			t.Fatal("architecture is nil")
		}
		if _, ok := m.Architecture["input_modalities"]; ok {
			t.Error("should not have input_modalities")
		}
		if len(m.Capabilities) > 0 {
			t.Errorf("expected no capabilities, got %v", m.Capabilities)
		}
	})

	t.Run("tools", func(t *testing.T) {
		m := getModel(t, newServer(config.ModelConfig{
			Capabilities: config.ModelCapConfig{Tools: true},
		}))
		if m.Capabilities == nil || m.Capabilities["function_calling"] != true {
			t.Error("expected function_calling: true")
		}
		if !stringSliceEqual(m.SupportedParameters, []string{"tools", "tool_choice"}) {
			t.Errorf("supported_parameters = %v", m.SupportedParameters)
		}
		if m.Architecture != nil {
			t.Error("should not have architecture")
		}
	})

	t.Run("reranker", func(t *testing.T) {
		m := getModel(t, newServer(config.ModelConfig{
			Capabilities: config.ModelCapConfig{Reranker: true},
		}))
		if m.Capabilities == nil || m.Capabilities["reranker"] != true {
			t.Error("expected reranker: true")
		}
		if m.Architecture != nil {
			t.Error("should not have architecture")
		}
	})

	t.Run("context", func(t *testing.T) {
		m := getModel(t, newServer(config.ModelConfig{
			Capabilities: config.ModelCapConfig{Context: 32768},
		}))
		if m.ContextLength != 32768 {
			t.Errorf("context_length = %d", m.ContextLength)
		}
		if m.Architecture != nil {
			t.Error("should not have architecture")
		}
	})

	t.Run("audio_transcriptions", func(t *testing.T) {
		m := getModel(t, newServer(config.ModelConfig{
			Capabilities: config.ModelCapConfig{In: []string{"audio"}, Out: []string{"text"}},
		}))
		if m.Capabilities == nil || m.Capabilities["audio_transcriptions"] != true {
			t.Error("expected audio_transcriptions: true")
		}
	})

	t.Run("image_generation", func(t *testing.T) {
		m := getModel(t, newServer(config.ModelConfig{
			Capabilities: config.ModelCapConfig{In: []string{"text"}, Out: []string{"image"}},
		}))
		if m.Capabilities == nil || m.Capabilities["image_generation"] != true {
			t.Error("expected image_generation: true")
		}
	})

	t.Run("image_to_image", func(t *testing.T) {
		m := getModel(t, newServer(config.ModelConfig{
			Capabilities: config.ModelCapConfig{In: []string{"image"}, Out: []string{"image"}},
		}))
		if m.Capabilities == nil || m.Capabilities["image_to_image"] != true {
			t.Error("expected image_to_image: true")
		}
	})

	t.Run("empty_skip", func(t *testing.T) {
		m := getModel(t, newServer(config.ModelConfig{}))
		if m.Architecture != nil {
			t.Error("should not have architecture")
		}
		if m.Capabilities != nil {
			t.Error("should not have capabilities")
		}
		if m.SupportedParameters != nil {
			t.Error("should not have supported_parameters")
		}
		if m.ContextLength != 0 {
			t.Error("should not have context_length")
		}
	})

	t.Run("metadata_precedence", func(t *testing.T) {
		m := getModel(t, newServer(config.ModelConfig{
			Capabilities: config.ModelCapConfig{In: []string{"text"}},
			Metadata: map[string]any{
				"architecture":   "should-be-dropped",
				"custom_field":   "should-remain",
				"capabilities":   "also-dropped",
				"other_metadata": "also-remain",
			},
		}))
		if m.Architecture == nil || m.Architecture["input_modalities"] == nil {
			t.Fatal("architecture should be rendered, not from metadata")
		}
		if m.Meta == nil || m.Meta["llamaswap"] == nil {
			t.Fatal("meta.llamaswap should exist")
		}
		meta := m.Meta["llamaswap"].(map[string]any)
		if _, ok := meta["architecture"]; ok {
			t.Error("architecture should be filtered from metadata")
		}
		if _, ok := meta["custom_field"]; !ok {
			t.Error("custom_field should remain in metadata")
		}
	})

	t.Run("metadata_passthrough_no_caps", func(t *testing.T) {
		m := getModel(t, newServer(config.ModelConfig{
			Metadata: map[string]any{
				"architecture":   "preserved",
				"context_length": 4096,
				"capabilities":   "preserved",
				"custom_field":   "preserved",
			},
		}))
		if m.Architecture != nil {
			t.Error("should not have architecture when caps is empty")
		}
		if m.Meta == nil || m.Meta["llamaswap"] == nil {
			t.Fatal("meta.llamaswap should exist")
		}
		meta := m.Meta["llamaswap"].(map[string]any)
		if _, ok := meta["architecture"]; !ok {
			t.Error("architecture should be preserved in metadata when caps is empty")
		}
		if _, ok := meta["context_length"]; !ok {
			t.Error("context_length should be preserved in metadata when caps is empty")
		}
	})
}

// TestServer_ModelStatus_Capabilities verifies the /api/events modelStatus
// payload carries capability booleans and context_length for configured models.
// The Models list reads both from this SSE payload.
func TestServer_ModelStatus_Capabilities(t *testing.T) {
	newServer := func(mc config.ModelConfig) *Server {
		s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
		s.cfg = config.Config{Models: map[string]config.ModelConfig{"m": mc}}
		return s
	}

	t.Run("renders capabilities and context", func(t *testing.T) {
		s := newServer(config.ModelConfig{
			Capabilities: config.ModelCapConfig{
				In:      []string{"text", "image"},
				Tools:   true,
				Context: 128000,
			},
		})
		status := s.modelStatus()
		if len(status) != 1 {
			t.Fatalf("expected 1 model, got %d", len(status))
		}
		m := status[0]
		if m.Id != "m" {
			t.Errorf("id = %q, want m", m.Id)
		}
		if m.Capabilities == nil || m.Capabilities["vision"] != true {
			t.Errorf("vision = %v", m.Capabilities)
		}
		if m.Capabilities["function_calling"] != true {
			t.Errorf("function_calling = %v", m.Capabilities["function_calling"])
		}
		if m.ContextLength != 128000 {
			t.Errorf("context_length = %d, want 128000", m.ContextLength)
		}
	})

	t.Run("omits capabilities when empty", func(t *testing.T) {
		s := newServer(config.ModelConfig{})
		m := s.modelStatus()[0]
		if m.Capabilities != nil {
			t.Errorf("expected no capabilities, got %v", m.Capabilities)
		}
		if m.ContextLength != 0 {
			t.Errorf("expected no context_length, got %d", m.ContextLength)
		}
	})
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func anySliceStrEqual(v any, want []string) bool {
	arr, ok := v.([]any)
	if !ok {
		return false
	}
	if len(arr) != len(want) {
		return false
	}
	for i := range arr {
		if s, ok := arr[i].(string); !ok || s != want[i] {
			return false
		}
	}
	return true
}

func TestServer_HandleListModels_AnthropicEnvelope(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	s.cfg = config.Config{
		Models: map[string]config.ModelConfig{
			"qwen": {
				Name:        "Qwen 35B",
				Description: "local qwen",
				Metadata: map[string]any{
					"anthropic_display_name": "Local Qwen",
					"anthropic_default":      true,
					"anthropic_efforts":      []any{"low", "high"},
				},
				Capabilities: config.ModelCapConfig{In: []string{"text"}},
			},
		},
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("anthropic-version", "2023-06-01")
	s.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}

	var resp struct {
		Data []struct {
			ID              string   `json:"id"`
			Type            string   `json:"type"`
			DisplayName     string   `json:"display_name"`
			CreatedAt       string   `json:"created_at"`
			Description     string   `json:"description"`
			Default         *bool    `json:"default"`
			Efforts         []string `json:"efforts"`
			InputModalities []string `json:"input_modalities"`
		} `json:"data"`
		HasMore bool   `json:"has_more"`
		FirstID string `json:"first_id"`
		LastID  string `json:"last_id"`
		Object  string `json:"object"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Object == "list" {
		t.Error("anthropic-version request must NOT get the OpenAI envelope")
	}
	if resp.HasMore {
		t.Error("has_more should be false")
	}
	if resp.FirstID != "qwen" || resp.LastID != "qwen" {
		t.Errorf("first_id/last_id = %q/%q, want qwen/qwen", resp.FirstID, resp.LastID)
	}
	if len(resp.Data) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(resp.Data))
	}
	e := resp.Data[0]
	if e.Type != "model" {
		t.Errorf("type = %q, want model", e.Type)
	}
	if e.DisplayName != "Local Qwen" {
		t.Errorf("display_name = %q, want overridden %q", e.DisplayName, "Local Qwen")
	}
	if e.Default == nil || !*e.Default {
		t.Errorf("default = %v, want true", e.Default)
	}
	if len(e.Efforts) != 2 || e.Efforts[0] != "low" || e.Efforts[1] != "high" {
		t.Errorf("efforts = %v, want [low high]", e.Efforts)
	}
	if len(e.InputModalities) != 1 || e.InputModalities[0] != "text" {
		t.Errorf("input_modalities = %v, want [text]", e.InputModalities)
	}
	if e.Description != "local qwen" {
		t.Errorf("description = %q", e.Description)
	}
	if e.CreatedAt == "" {
		t.Error("created_at must be a non-empty RFC3339 string")
	}

	// Same request WITHOUT the header still gets the OpenAI envelope.
	w2 := httptest.NewRecorder()
	s.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	var openai struct {
		Object string `json:"object"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &openai); err != nil {
		t.Fatalf("decode openai: %v", err)
	}
	if openai.Object != "list" {
		t.Errorf("object = %q, want OpenAI envelope %q", openai.Object, "list")
	}
}

// TestServer_HandleListModels_UserAgentNegotiation covers the 2026-07-22 live
// bug: Claude Code's gateway discovery call sends NO anthropic-version header,
// so negotiation must also trigger on its User-Agent ("claude-code/..."),
// while OpenAI consumers (curl, llama.cpp clients) keep the OpenAI envelope.
// TestServer_HandleListModels_AnthropicClaudeAliases covers the 2026-07-22
// picker gap: Claude Code's /model picker only surfaces discovered IDs starting
// with "claude", so the Anthropic envelope must also emit one row per
// claude-prefixed ALIAS (inheriting the parent model's metadata) while
// non-claude shell aliases (cq35) stay out. The raw model row stays, and the
// OpenAI listing path is untouched.
func TestServer_HandleListModels_AnthropicClaudeAliases(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	s.cfg = config.Config{
		Models: map[string]config.ModelConfig{
			"Qwen3.6-35B": {
				Name:        "Qwen 35B",
				Description: "local qwen",
				Aliases:     []string{"cq35", "claude-cq35"},
				Metadata: map[string]any{
					"anthropic_display_name": "Local Qwen",
					"anthropic_default":      true,
					"anthropic_efforts":      []any{"low", "high"},
				},
				Capabilities: config.ModelCapConfig{In: []string{"text"}},
			},
		},
	}

	type entry struct {
		ID              string   `json:"id"`
		DisplayName     string   `json:"display_name"`
		CreatedAt       string   `json:"created_at"`
		Description     string   `json:"description"`
		Default         *bool    `json:"default"`
		Efforts         []string `json:"efforts"`
		InputModalities []string `json:"input_modalities"`
	}

	// Anthropic path (claude-code UA, no anthropic-version header).
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("User-Agent", "claude-code/2.1.217")
	s.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp struct {
		Data []entry `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byID := map[string]entry{}
	for _, e := range resp.Data {
		if _, dup := byID[e.ID]; dup {
			t.Errorf("duplicate entry id %q", e.ID)
		}
		byID[e.ID] = e
	}

	// Raw model row stays in the envelope.
	if _, ok := byID["Qwen3.6-35B"]; !ok {
		t.Errorf("raw model row missing: %v", byID)
	}
	// claude-prefixed alias row exists, inheriting the parent's extras.
	alias, ok := byID["claude-cq35"]
	if !ok {
		t.Fatalf("claude-cq35 alias row missing: %v", byID)
	}
	if alias.DisplayName != "Local Qwen" {
		t.Errorf("alias display_name = %q, want parent override %q", alias.DisplayName, "Local Qwen")
	}
	if alias.Default == nil || !*alias.Default {
		t.Errorf("alias default = %v, want true", alias.Default)
	}
	if len(alias.Efforts) != 2 || alias.Efforts[0] != "low" || alias.Efforts[1] != "high" {
		t.Errorf("alias efforts = %v, want [low high]", alias.Efforts)
	}
	if len(alias.InputModalities) != 1 || alias.InputModalities[0] != "text" {
		t.Errorf("alias input_modalities = %v, want [text]", alias.InputModalities)
	}
	if alias.Description != "local qwen" {
		t.Errorf("alias description = %q", alias.Description)
	}
	if alias.CreatedAt == "" {
		t.Error("alias created_at must be non-empty")
	}
	// Non-claude alias stays OUT of the picker listing.
	if _, ok := byID["cq35"]; ok {
		t.Error("non-claude alias cq35 must not appear in the Anthropic envelope")
	}

	// OpenAI path (no UA/header) unchanged: same single raw model row, no
	// alias rows (includeAliasesInList is off).
	w2 := httptest.NewRecorder()
	s.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	var openai struct {
		Object string        `json:"object"`
		Data   []modelRecord `json:"data"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &openai); err != nil {
		t.Fatalf("decode openai: %v", err)
	}
	if openai.Object != "list" {
		t.Errorf("object = %q, want OpenAI envelope %q", openai.Object, "list")
	}
	if len(openai.Data) != 1 || openai.Data[0].ID != "Qwen3.6-35B" {
		t.Errorf("openai data = %+v, want exactly the raw model row", openai.Data)
	}
}

func TestServer_HandleListModels_UserAgentNegotiation(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	s.cfg = config.Config{
		Models: map[string]config.ModelConfig{
			"qwen": {Name: "Qwen 35B"},
		},
	}

	get := func(ua string) map[string]any {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		if ua != "" {
			req.Header.Set("User-Agent", ua)
		}
		s.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("UA %q: status = %d", ua, w.Code)
		}
		var resp map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("UA %q: decode: %v", ua, err)
		}
		return resp
	}

	// Claude Code UA, no anthropic-version header -> Anthropic envelope.
	resp := get("claude-code/2.1.217")
	if resp["object"] == "list" {
		t.Error("claude-code UA without anthropic-version got the OpenAI envelope")
	}
	if _, ok := resp["has_more"]; !ok {
		t.Errorf("claude-code UA: expected Anthropic envelope fields, got %v", resp)
	}

	// curl UA, no headers -> OpenAI envelope.
	resp = get("curl/8.7.1")
	if resp["object"] != "list" {
		t.Errorf("curl UA: object = %v, want OpenAI envelope %q", resp["object"], "list")
	}
}

// TestServer_HandleUpstream_RootLoadGuard pins the eager-reload guard added to
// handleUpstream: a GET/HEAD to the model root path (/upstream/<model>/) must NOT
// trigger a model load. It returns 503 unless the model is already StateReady, so
// an external health-poller can no longer accidentally reload an idle model. POST
// (a real inference request) always passes through and may queue a load.
func TestServer_HandleUpstream_RootLoadGuard(t *testing.T) {
	// newGuardServer builds a server whose local router Handles "m1", returns
	// "upstream-body" on pass-through, and reports the supplied running-state map.
	newGuardServer := func(running map[string]process.ProcessState) *Server {
		local := newStubRouter([]string{"m1"}, "upstream-body")
		local.running = running
		s := newTestServer(local, newStubRouter(nil, ""))
		s.cfg = config.Config{Models: map[string]config.ModelConfig{"m1": {}}}
		return s
	}

	t.Run("GET root 503 when model not ready", func(t *testing.T) {
		// running is nil => m1 is not resident/ready.
		s := newGuardServer(nil)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/upstream/m1/", nil))
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503 (load must not be triggered by GET)", w.Code)
		}
	})

	t.Run("HEAD root 503 when model not ready", func(t *testing.T) {
		// A starting (not-yet-ready) model must also be guarded.
		s := newGuardServer(map[string]process.ProcessState{"m1": process.StateStarting})
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodHead, "/upstream/m1/", nil))
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503 (load must not be triggered by HEAD)", w.Code)
		}
	})

	t.Run("GET root passes through when model is ready", func(t *testing.T) {
		s := newGuardServer(map[string]process.ProcessState{"m1": process.StateReady})
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/upstream/m1/", nil))
		if w.Code != http.StatusOK || w.Body.String() != "upstream-body" {
			t.Errorf("status=%d body=%q, want 200/upstream-body (ready model must pass through)", w.Code, w.Body.String())
		}
	})

	t.Run("POST root always passes through regardless of readiness", func(t *testing.T) {
		// running is nil => m1 not ready, yet POST must still pass through so the
		// real inference path can queue a load.
		s := newGuardServer(nil)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/upstream/m1/", nil))
		if w.Code != http.StatusOK || w.Body.String() != "upstream-body" {
			t.Errorf("status=%d body=%q, want 200/upstream-body (POST must pass through)", w.Code, w.Body.String())
		}
	})

	t.Run("GET sub-path is not guarded (only the root load-trigger path is)", func(t *testing.T) {
		// A GET to a real sub-path (e.g. /v1/models) is not the load-trigger root;
		// it passes through even when not ready, matching pre-guard behavior.
		s := newGuardServer(nil)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/upstream/m1/v1/models", nil))
		if w.Code != http.StatusOK || w.Body.String() != "upstream-body" {
			t.Errorf("status=%d body=%q, want 200/upstream-body (sub-path must pass through)", w.Code, w.Body.String())
		}
	})
}
