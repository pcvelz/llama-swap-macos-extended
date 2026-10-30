package server

import (
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// Row stability across one tool-loop turn (2026-10-30, live 10 Hz tap of
// session fea05c04 on fork 87bcb74): the retained DECODE word and readout
// held, but the rest of the visible row still changed at every turn. Each
// test below is one of those flips. Incident: llama-cm
// llama/incidents/2026-09-16-menubar-row-flickers-decode-flat-while-decoding.md
// § "2026-10-30 follow-up 2".

// A retained DECODE row is the ACTIVE row on screen, so it ranks as DECODE:
// it must not drop below parked rows while its raw phase reads HOT between
// turns and then jump back to the top on the next PREFILL.
func TestSessions_RetainedDecodeSortsAsDecode(t *testing.T) {
	b := newSessionsBuilder(sessionsTestConfig())
	t0 := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	running := map[string]process.ProcessState{sessModel: process.StateReady}
	decodeWithRate(t, b, t0) // s1 last DECODE at t0+1s

	parked := func(id, sess string) swaputil.InflightRequestEntry {
		return swaputil.InflightRequestEntry{ID: id, Model: sessModel, Timestamp: t0.Add(-time.Hour),
			Metadata: map[string]string{"session_id": sess, "park_reason": "busy"}}
	}
	got := b.build(sessionsInput{Now: t0.Add(4 * time.Second), Running: running,
		Requests: []swaputil.InflightRequestEntry{parked("p2", "s2"), parked("p3", "s3")},
		Hot:      map[string][]swaputil.HotSlot{sessModel: {{Slot: 0, SessionID: "s1", IdleSeconds: 3}}},
		Slots:    map[string][]childSlot{sessModel: {{ID: 0, NCtx: 262144}}}})
	retainedDisplay(t, got, "s1", 41530, 58.0)
	if got.Sessions[0].SessionID != "s1" {
		order := []string{}
		for _, r := range got.Sessions {
			order = append(order, r.SessionID+"/"+r.Phase)
		}
		t.Fatalf("order = %v, want the retained DECODE row s1 first (it is the active row on screen)", order)
	}
	if got.Sessions[0].Phase != "HOT" {
		t.Fatalf("raw phase = %q, want HOT untouched", got.Sessions[0].Phase)
	}
	assertSessionsInvariants(t, got)
}

// The first raw DECODE sample of a new turn has no measured rate yet (two
// samples are needed). The row must keep showing the last measured decode
// rate instead of blanking its t/s for that second; the raw rate stays nil.
func TestSessions_DecodeStartKeepsLastMeasuredRate(t *testing.T) {
	b := newSessionsBuilder(sessionsTestConfig())
	t0 := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	running := map[string]process.ProcessState{sessModel: process.StateReady}
	decodeWithRate(t, b, t0) // 58.0 t/s at t0+1s

	slots := func(processed, decoded int) map[string][]childSlot {
		return map[string][]childSlot{sessModel: {{ID: 0, NCtx: 262144, IsProcessing: true,
			NPromptTokens: 90000, NPromptTokensCache: 38000, NPromptTokensProcessed: processed, NDecoded: decoded}}}
	}
	// Next turn: PREFILL, then the first DECODE sample.
	b.build(sessionsInput{Now: t0.Add(4 * time.Second), Running: running,
		Requests: []swaputil.InflightRequestEntry{retentionReq("r2", t0.Add(3*time.Second), nil)}, Slots: slots(3100, 0)})
	got := b.build(sessionsInput{Now: t0.Add(6 * time.Second), Running: running,
		Requests: []swaputil.InflightRequestEntry{retentionReq("r2", t0.Add(3*time.Second), nil)}, Slots: slots(3300, 40)})
	row := got.Sessions[0]
	if row.Phase != "DECODE" || row.Rate.TokensPerSecond != nil {
		t.Fatalf("setup: phase=%s rate=%+v, want raw DECODE with an unmeasured rate", row.Phase, row.Rate)
	}
	retainedDisplay(t, got, "s1", float64(row.Context.Used), 58.0)

	// One second later the new turn has its own measured rate: no override.
	got = b.build(sessionsInput{Now: t0.Add(7 * time.Second), Running: running,
		Requests: []swaputil.InflightRequestEntry{retentionReq("r2", t0.Add(3*time.Second), nil)}, Slots: slots(3300, 70)})
	if got.Sessions[0].Rate.TokensPerSecond == nil {
		t.Fatalf("setup: second DECODE sample rate = %+v, want measured", got.Sessions[0].Rate)
	}
	noDisplay(t, got, "s1")
	assertSessionsInvariants(t, got)
}

// A dispatched child's row names its parent ("[parent > child]"). HOT and
// IDLE rows are rebuilt from the session's previous row, so they must carry
// the same parent - otherwise the label changes at every turn boundary.
func TestSessions_ParentSurvivesHotAndIdle(t *testing.T) {
	b := newSessionsBuilder(sessionsTestConfig())
	t0 := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	running := map[string]process.ProcessState{sessModel: process.StateReady}
	const parent = "6eba94e7-0000-0000-0000-000000000000"
	req := retentionReq("r1", t0.Add(-5*time.Second), map[string]string{"parent_session_id": parent})
	b.build(sessionsInput{Now: t0, Running: running, Requests: []swaputil.InflightRequestEntry{req},
		Slots: map[string][]childSlot{sessModel: {{ID: 0, NCtx: 262144, IsProcessing: true,
			NPromptTokens: 90000, NPromptTokensCache: 38000, NPromptTokensProcessed: 2960, NDecoded: 10}}}})

	got := b.build(sessionsInput{Now: t0.Add(2 * time.Second), Running: running,
		Hot:   map[string][]swaputil.HotSlot{sessModel: {{Slot: 0, SessionID: "s1", IdleSeconds: 1}}},
		Slots: map[string][]childSlot{sessModel: {{ID: 0, NCtx: 262144}}}})
	if r := got.Sessions[0]; r.Phase != "HOT" || r.ParentSessionShort != "6eba94e7" || r.ParentSessionId != parent {
		t.Fatalf("HOT row: phase=%s parent=%q/%q, want HOT with parent 6eba94e7", r.Phase, r.ParentSessionShort, r.ParentSessionId)
	}

	got = b.build(sessionsInput{Now: t0.Add(4 * time.Second), Running: running})
	if len(got.Sessions) != 1 {
		t.Fatalf("rows = %+v, want the vanished session kept as one IDLE row", got.Sessions)
	}
	if r := got.Sessions[0]; r.Phase != "IDLE" || r.ParentSessionShort != "6eba94e7" {
		t.Fatalf("IDLE row: phase=%s parent=%q, want IDLE with parent 6eba94e7", r.Phase, r.ParentSessionShort)
	}
	assertSessionsInvariants(t, got)
}
