package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/swaputil"
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

// TestInflightTracker_SetSlotID pins the live slot stamp a renderer joins to
// /upstream/<model>/slots: stamped mid-flight it is visible on the LIVE entry
// (not just the completed activity row), re-stamping the same number emits
// nothing, and a different number (child reassignment) wins.
func TestInflightTracker_SetSlotID(t *testing.T) {
	tracker := newInflightTracker()
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	id := tracker.Add(req, cancel)

	entry := func() swaputil.InflightRequestEntry {
		entries := tracker.Current().Requests
		if len(entries) != 1 {
			t.Fatalf("Requests = %+v, want 1 entry", entries)
		}
		return entries[0]
	}

	tracker.SetSlotID(id, 3)
	if got := entry().Metadata["slot_id"]; got != "3" {
		t.Fatalf(`entry.Metadata["slot_id"] = %q, want "3"`, got)
	}

	// Same number again: idempotent, still exactly one tracked request.
	tracker.SetSlotID(id, 3)
	if got := entry().Metadata["slot_id"]; got != "3" {
		t.Fatalf(`entry.Metadata["slot_id"] = %q after re-stamp, want "3"`, got)
	}

	// Child reassigned the request: the newer number wins.
	tracker.SetSlotID(id, 1)
	if got := entry().Metadata["slot_id"]; got != "1" {
		t.Fatalf(`entry.Metadata["slot_id"] = %q after reassignment, want "1"`, got)
	}

	// No-op once the request is gone.
	tracker.Remove(id)
	tracker.SetSlotID(id, 0)
	if entries := tracker.Current().Requests; len(entries) != 0 {
		t.Errorf("Requests = %+v, want none after Remove", entries)
	}
}

// TestInflightTracker_Add_CarriesAffinitySlotAsSlotID pins the admission-time
// stamp: a request whose context already carries slot_affinity (the affinity
// middleware injected it) shows slot_id on its FIRST event, so a renderer does
// not wait for the response's id_slot to join the row to a slot.
func TestInflightTracker_Add_CarriesAffinitySlotAsSlotID(t *testing.T) {
	tracker := newInflightTracker()
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	data := swaputil.ReqContextData{
		Model:    "cq27",
		ModelID:  "Qwen3.8-27B",
		Metadata: map[string]string{"slot_affinity": "1"},
	}
	req = req.WithContext(swaputil.SetContext(req.Context(), data))
	id := tracker.Add(req, cancel)

	entries := tracker.Current().Requests
	if len(entries) != 1 {
		t.Fatalf("Requests = %+v, want 1 entry", entries)
	}
	if got := entries[0].Metadata["slot_id"]; got != "1" {
		t.Fatalf(`entry.Metadata["slot_id"] = %q, want the affinity slot "1" from the first event`, got)
	}
	_ = id
}
