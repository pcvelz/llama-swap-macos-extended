package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// slotsEndpoint: false declares that a model's child has no llama.cpp style
// GET /slots. Everything the server derives from that endpoint is switched off
// for the model and fails open. Each test pairs a model that has the endpoint
// with one that declares it away, driven by the same traffic, so a zero on the
// second means something.

func slotsKey(v bool) *bool { return &v }

// countingChild is a child that counts every request it receives and answers
// /slots the way a llama.cpp child does, or with a 404 the way an engine
// without the endpoint does.
func countingChild(t *testing.T, status int) (url string, hits *atomic.Int32) {
	t.Helper()
	hits = new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/slots" {
			t.Errorf("child got %s %s, want only GET /slots", r.Method, r.URL.Path)
		}
		if status != http.StatusOK {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"slots":[{"id":0,"n_ctx":8192}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, hits
}

// The 1 Hz session poller reads /slots of every ready child. A child declared
// slotsEndpoint: false is not read at all, however often the tick runs, while
// the other child is read once per tick.
func TestSessions_Poller_SlotsEndpointFalse_NeverReadsTheChild(t *testing.T) {
	withURL, withHits := countingChild(t, http.StatusOK)
	withoutURL, withoutHits := countingChild(t, http.StatusNotFound)

	local := newStubRouter([]string{"alpha", "beta"}, "")
	local.running = map[string]process.ProcessState{
		"alpha": process.StateReady,
		"beta":  process.StateReady,
	}
	cfg := config.Config{Models: map[string]config.ModelConfig{
		"alpha": {Proxy: withURL, ConcurrencyLimit: 1},
		"beta":  {Proxy: withoutURL, ConcurrencyLimit: 1, SlotsEndpoint: slotsKey(false)},
	}}
	s := newTestServerWithConfig(cfg, local, newStubRouter(nil, ""))
	hub := newSessionsHub(s)
	s.sessions = hub

	for i := 0; i < 3; i++ {
		hub.tick(time.Now(), true)
	}
	assert.Equal(t, int32(3), withHits.Load(), "a child with /slots is read once per tick")
	assert.Zero(t, withoutHits.Load(), "slotsEndpoint: false must never be read: a 404 per second is the noise this key removes")

	// The snapshot is still built, and carries no reading for the model.
	snap := hub.snapshot()
	require.NotNil(t, snap.Resident)
	assert.Equal(t, "llama-swap.sessions/v1", snap.Schema)
	_, polled := hub.lastPoll["beta"]
	assert.False(t, polled, "no /slots reading may be folded or kept over for the model")
}

// GET /api/slots lists a model with no slots and no error instead of fetching.
func TestServer_HandleAPISlots_SlotsEndpointFalse_NoFetch(t *testing.T) {
	withURL, withHits := countingChild(t, http.StatusOK)
	withoutURL, withoutHits := countingChild(t, http.StatusNotFound)

	local := newStubRouter(nil, "")
	local.running = map[string]process.ProcessState{
		"alpha": process.StateReady,
		"beta":  process.StateReady,
	}
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{
		"alpha": {Proxy: withURL, ConcurrencyLimit: 2},
		"beta":  {Proxy: withoutURL, ConcurrencyLimit: 2, SlotsEndpoint: slotsKey(false)},
	}}

	w := httptest.NewRecorder()
	s.handleAPISlots(w, httptest.NewRequest(http.MethodGet, "/api/slots", nil))
	require.Equal(t, http.StatusOK, w.Code)

	var body struct {
		Models []apiModelSlots `json:"models"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	byID := map[string]apiModelSlots{}
	for _, m := range body.Models {
		byID[m.Model] = m
	}
	require.Len(t, byID, 2)

	assert.Equal(t, int32(1), withHits.Load())
	assert.Len(t, byID["alpha"].Slots, 1)

	assert.Zero(t, withoutHits.Load(), "no /slots fetch for slotsEndpoint: false")
	assert.Empty(t, byID["beta"].Slots)
	assert.Empty(t, byID["beta"].Error, "an absent endpoint is not an error")
	assert.Equal(t, "ready", byID["beta"].State)
}

// Slot affinity pins a session to a llama.cpp slot id and injects it as
// id_slot. A child with no /slots reports no ids and takes none, so a model that
// carries both keys is passed through untouched; a model without
// slotsEndpoint: false is still pinned.
func TestSlotAffinity_SlotsEndpointFalse_PassesRequestsThrough(t *testing.T) {
	cfg := config.Config{Models: map[string]config.ModelConfig{
		"alpha": {SlotAffinity: true, ConcurrencyLimit: 2},
		"beta":  {SlotAffinity: true, ConcurrencyLimit: 2, SlotsEndpoint: slotsKey(false)},
	}}
	store := newAffinityStore(cfg)

	assert.True(t, store.enabled("alpha"))
	assert.False(t, store.enabled("beta"))

	// Even a lane learned before the key was set (or by a bypass) is not used.
	store.byModel["beta"] = map[string]slotAffinityEntry{affSessionA: {slot: 1}}
	store.learn("beta", affSessionB, 1)
	_, learned := store.lookup("beta", affSessionB)
	assert.False(t, learned, "nothing is learned for a model without a /slots endpoint")

	for model, wantInjected := range map[string]bool{"alpha": true, "beta": false} {
		r := affinityRequest(model, affSessionA, `"max_tokens":8`)
		body, data, _, called := runAffinityMiddleware(t, store, cfg, r)
		require.True(t, called, model)
		assert.Equal(t, wantInjected, gjson.GetBytes(body, "id_slot").Exists(), "%s: id_slot injected", model)
		_, stamped := data.Metadata[slotAffinityMetadataKey]
		assert.Equal(t, wantInjected, stamped, "%s: slot_affinity stamped on the request", model)
	}
}
