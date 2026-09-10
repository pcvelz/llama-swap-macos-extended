package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestInflightTracker_SetMetadata_EmptyValueDeletesKey pins the delete signal
// the router's grant path uses to end a KV park (base.go clears kv_parked by
// setting it empty). Renderers read these keys as flags, so a state that has
// ended must leave no key behind at all.
func TestInflightTracker_SetMetadata_EmptyValueDeletesKey(t *testing.T) {
	tracker := newInflightTracker()
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	id := tracker.Add(req, cancel)

	tracker.SetMetadata(id, "kv_parked", "1")
	entries := tracker.Current().Requests
	if len(entries) != 1 {
		t.Fatalf("Requests = %+v, want 1 entry", entries)
	}
	if got := entries[0].Metadata["kv_parked"]; got != "1" {
		t.Fatalf(`entry.Metadata["kv_parked"] = %q, want "1"`, got)
	}

	tracker.SetMetadata(id, "kv_parked", "")

	entries = tracker.Current().Requests
	if len(entries) != 1 {
		t.Fatalf("Requests = %+v, want 1 entry", entries)
	}
	if v, ok := entries[0].Metadata["kv_parked"]; ok {
		t.Errorf(`entry.Metadata["kv_parked"] = %q, want the key absent`, v)
	}
}

// TestInflightTracker_SetMetadata_DeleteIsSafeOnUntrackedRequest guards the
// two no-op cases the grant path can hit: clearing a key on a request that
// already finished, and clearing one that was never set.
func TestInflightTracker_SetMetadata_DeleteIsSafeOnUntrackedRequest(t *testing.T) {
	tracker := newInflightTracker()
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	id := tracker.Add(req, cancel)

	tracker.SetMetadata(id, "kv_parked", "")
	tracker.Remove(id)
	tracker.SetMetadata(id, "kv_parked", "")

	if entries := tracker.Current().Requests; len(entries) != 0 {
		t.Errorf("Requests = %+v, want none after Remove", entries)
	}
}
