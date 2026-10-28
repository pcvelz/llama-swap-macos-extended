package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Full chain: a turn under a resident alias reaches the child with the
// resolved model's useModelName in its model field, so an engine that checks
// the name (Splash answers 404 model_not_found to a name it does not serve)
// accepts it. The id_slot the affinity middleware injected earlier survives
// the rewrite, and Content-Length follows the rewritten body.
func TestServer_ResidentAliasTurn_ChildGetsUseModelName(t *testing.T) {
	cfg, err := loadResidentAliasConfig(`
models:
  modelA:
    cmd: echo a ${PORT}
    useModelName: served-name
    slotAffinity: true
    concurrencyLimit: 2
residentAliases:
  - "claude-*"
`)
	require.NoError(t, err)

	var seenBody []byte
	var seenLength int64
	local := newStubRouter([]string{"modelA"}, "")
	local.running = map[string]process.ProcessState{"modelA": process.StateReady}
	s := affinityTestServer(t, cfg, local)
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		seenBody, _ = io.ReadAll(r.Body)
		seenLength = r.ContentLength
		w.WriteHeader(http.StatusOK)
	}

	body := `{"model":"claude-haiku-4-5-20251001","messages":[{"role":"user","content":"` +
		strings.Repeat("x", 20000) + `"}]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Claude-Code-Session-Id", affSessionA)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "served-name", gjson.GetBytes(seenBody, "model").String())
	assert.True(t, gjson.GetBytes(seenBody, "id_slot").Exists(), "the rewrite must keep the injected id_slot")
	assert.Equal(t, int64(len(seenBody)), seenLength, "Content-Length must match the rewritten body")
}
