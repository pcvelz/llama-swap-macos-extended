package server

// The sessions API carries a NEW field on every parked row (user-approved
// menu layout, 2026-10-18): `waitingFor` - {"session": "<short or full
// session id>", "until": "<RFC3339 or empty>"} - the WHO and HOW LONG behind
// a park reason. It travels the same path park_reason does: the scheduler
// stamps it on the in-flight entry's metadata (next to `park_reason`, see
// fifo.go markParked) and the sessions builder copies it onto the row.

import (
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// A parked row carries waitingFor straight from its in-flight metadata: the
// session it waits behind and the RFC3339 moment the hold ends.
func TestSessions_ParkedRowCarriesWaitingFor(t *testing.T) {
	b := newSessionsBuilder(sessionsTestConfig())
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	until := now.Add(4*time.Minute + 51*time.Second).Format(time.RFC3339)

	reqs := []swaputil.InflightRequestEntry{
		// The in-flight large turn the parked request waits behind.
		{ID: "r-1", Model: sessModel, Timestamp: now.Add(-60 * time.Second),
			Metadata: map[string]string{"session_id": "8f10d234-aaaa-bbbb-cccc-dddddddddddd", "slot_granted": "1", "slot_id": "0", "tier": "default"}},
		// Parked on the slot cooldown, with its waitingFor stamped by the scheduler.
		{ID: "r-2", Model: sessModel, Timestamp: now.Add(-4 * time.Second),
			Metadata: map[string]string{"session_id": "04dcada1-1111-2222-3333-444455556666", "park_reason": "kv-warm",
				"waiting_for_session": "8f10d234-aaaa-bbbb-cccc-dddddddddddd", "waiting_for_until": until, "tier": "default"}},
	}
	running := map[string]process.ProcessState{sessModel: process.StateReady}
	got := b.build(sessionsInput{Now: now, Running: running, Requests: reqs, MemoryBrake: enabledBrake()})

	var parked *sessionEntry
	for i := range got.Sessions {
		if got.Sessions[i].SessionShort == "04dcada1" && got.Sessions[i].Phase == "PARKED" {
			parked = &got.Sessions[i]
		}
	}
	if parked == nil {
		t.Fatalf("no PARKED row for 04dcada1 in %+v", got.Sessions)
	}
	if parked.ParkReason == nil || *parked.ParkReason != "kv-warm" {
		t.Fatalf("parkReason = %v, want kv-warm", parked.ParkReason)
	}
	wf := parked.WaitingFor
	if wf == nil {
		t.Fatalf("waitingFor absent on a PARKED row that carries waiting_for metadata")
	}
	if wf.Session != "8f10d234-aaaa-bbbb-cccc-dddddddddddd" {
		t.Errorf("waitingFor.session = %q, want the in-flight session id", wf.Session)
	}
	if wf.Until != until {
		t.Errorf("waitingFor.until = %q, want %q", wf.Until, until)
	}
}

// A parked row with no waiting_for metadata (older scheduler, or a hold that
// names no session and no clock - rank, cap) still serialises the field as an
// empty object, never omits it: the renderer must be able to tell "no data"
// from "field predates".
func TestSessions_ParkedRowWaitingForEmptyWhenUnstamped(t *testing.T) {
	b := newSessionsBuilder(sessionsTestConfig())
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

	reqs := []swaputil.InflightRequestEntry{
		{ID: "r-3", Model: sessModel, Timestamp: now.Add(-4 * time.Second),
			Metadata: map[string]string{"session_id": "5e6f7a8b-9999-8888-7777-666655554444", "park_reason": "rank", "tier": "background"}},
	}
	running := map[string]process.ProcessState{sessModel: process.StateReady}
	got := b.build(sessionsInput{Now: now, Running: running, Requests: reqs, MemoryBrake: enabledBrake()})

	var parked *sessionEntry
	for i := range got.Sessions {
		if got.Sessions[i].SessionShort == "5e6f7a8b" && got.Sessions[i].Phase == "PARKED" {
			parked = &got.Sessions[i]
		}
	}
	if parked == nil {
		t.Fatalf("no PARKED row for 5e6f7a8b in %+v", got.Sessions)
	}
	wf := parked.WaitingFor
	if wf == nil {
		t.Fatal("waitingFor must be present on every PARKED row, even when the scheduler stamped nothing")
	}
	if wf.Session != "" || wf.Until != "" {
		t.Errorf("waitingFor = (%q, %q), want both empty", wf.Session, wf.Until)
	}
}

// A NON-parked row never carries waitingFor: the field is about what a
// parked request waits for, and an answering/cooling row waits for nothing.
func TestSessions_NonParkedRowHasNoWaitingFor(t *testing.T) {
	b := newSessionsBuilder(sessionsTestConfig())
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

	reqs := []swaputil.InflightRequestEntry{
		{ID: "r-4", Model: sessModel, Timestamp: now.Add(-60 * time.Second), RespTokens: 96,
			Metadata: map[string]string{"session_id": "8f10d234-aaaa-bbbb-cccc-dddddddddddd", "slot_granted": "1", "slot_id": "0", "tier": "default"}},
	}
	running := map[string]process.ProcessState{sessModel: process.StateReady}
	got := b.build(sessionsInput{Now: now, Running: running, Requests: reqs, MemoryBrake: enabledBrake()})

	for _, s := range got.Sessions {
		if s.SessionShort == "8f10d234" && s.WaitingFor != nil {
			t.Errorf("waitingFor set on phase %s: only PARKED rows carry it", s.Phase)
		}
	}
}
