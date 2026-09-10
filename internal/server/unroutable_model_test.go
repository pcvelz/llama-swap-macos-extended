package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/stretchr/testify/assert"
)

// A model id no router handles must come back naming the id that was asked
// for, the ids that exist, and what is resident. A gateway in front of the
// proxy can only relay what the body says, so an anonymous 404 reaches the
// end user as an untraceable transport failure.
func TestUnroutableModelErrorNamesModels(t *testing.T) {
	local := newStubRouter([]string{"modelA", "modelB"}, "")
	local.running = map[string]process.ProcessState{
		"modelA": process.StateReady,
		"modelB": process.StateStopping,
	}
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{
		"modelA": {},
		"modelB": {},
	}}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"modelC"}`))
	r.Header.Set("Content-Type", "application/json")
	s.ServeHTTP(w, r)

	assert.Equal(t, http.StatusNotFound, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "modelC", "requested model id must be named")
	assert.Contains(t, body, "modelA", "served model ids must be listed")
	assert.Contains(t, body, "modelB", "served model ids must be listed")
	assert.Contains(t, body, "resident", "resident model must be reported")
}

// Only ready processes count as resident; a stopping one must not be reported
// as something the caller could have used.
func TestUnroutableModelReportsNoResident(t *testing.T) {
	local := newStubRouter([]string{"modelA"}, "")
	local.running = map[string]process.ProcessState{"modelA": process.StateStopping}
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"modelA": {}}}

	w := httptest.NewRecorder()
	s.sendUnroutableModel(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), "modelC")

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "resident: none")
}
