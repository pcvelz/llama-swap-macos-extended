package server

import (
	"net/http"
	"testing"
)

// TestIsModelDispatchedRequest_ExcludesControlPlane guards the boundary between
// requests that enter ADMISSION and requests that do not.
//
// WHY IT MATTERS BEYOND ROUTING: this predicate decides what is tracked as
// in-flight, which decides what appears in the queue view and what contributes
// to its counts. A control-plane path leaking through would show the tray a
// queue entry for a /health poll and inflate the waiting count with requests
// that never wanted a slot. The menu-bar helper polls /api/metrics/stats every
// 2 seconds, so a leak here is not hypothetical - it would be a permanent fake
// queue entry.
//
// The function is an ALLOWLIST of explicit routes, which is the right shape:
// a new control-plane endpoint is excluded by default and has to be added
// deliberately to become dispatched. These cases pin that property.
func TestIsModelDispatchedRequest_ExcludesControlPlane(t *testing.T) {
	// Every one of these is mounted on apiChain and never enters admission.
	control := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/health"},
		{http.MethodGet, "/running"},
		{http.MethodGet, "/api/events"},
		{http.MethodGet, "/api/capacity"},
		{http.MethodGet, "/api/swap-grace"},
		{http.MethodGet, "/api/slots"},
		{http.MethodPost, "/api/swap-grace/finish/m1"},
		{http.MethodGet, "/api/metrics/stats"},
		{http.MethodGet, "/api/metrics/activity"},
		{http.MethodGet, "/api/performance"},
		{http.MethodGet, "/api/version"},
		{http.MethodGet, "/api/hardware"},
		{http.MethodGet, "/api/profiles"},
		{http.MethodGet, "/metrics"},
		{http.MethodGet, "/ui"},
		{http.MethodPost, "/api/models/unload"},
	}
	for _, c := range control {
		if isModelDispatchedRequest(c.method, c.path) {
			t.Errorf("%s %s is control-plane and must NOT be model-dispatched - it would appear as a queue entry and inflate the waiting count",
				c.method, c.path)
		}
	}
}

// TestIsModelDispatchedRequest_AdmitsRealWork is the other half. Excluding
// everything would also pass the test above, so the inference paths must be
// pinned as INCLUDED or the predicate could be gutted without notice.
func TestIsModelDispatchedRequest_AdmitsRealWork(t *testing.T) {
	dispatched := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/v1/chat/completions"},
		{http.MethodPost, "/v1/completions"},
		{http.MethodPost, "/v1/embeddings"},
	}
	for _, c := range dispatched {
		if !isModelDispatchedRequest(c.method, c.path) {
			t.Errorf("%s %s is real inference work and MUST be model-dispatched", c.method, c.path)
		}
	}
}

// TestIsModelDispatchedRequest_MethodMatters: the same path under a method the
// route lists do not name is not dispatched. A method-blind check would admit
// a GET probe of an inference path as though it were a turn.
func TestIsModelDispatchedRequest_MethodMatters(t *testing.T) {
	if isModelDispatchedRequest(http.MethodDelete, "/v1/chat/completions") {
		t.Error("DELETE /v1/chat/completions is not a dispatch route; the predicate must be method-aware")
	}
}
