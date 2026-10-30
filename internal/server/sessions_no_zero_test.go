package server

import (
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// User rule (2026-10-30): a zero is improbable, so a menu never shows one.
// A session that has had real context never shows "0/262.1k" again, in any
// phase and however long after its last decode; no row ever shows "0.0 t/s".
// The raw fields stay what the child reports - machine readers keep them.
// Measured live before the fix: 4989 polls of 0 context after real context
// and 9986 polls of 0.0 t/s in 25 minutes of one cq180 Claude Code dispatch.

// Past the decode retention, a PARKED row of a session that had real context
// shows that last context, not the parked request's 0.
func TestSessions_ContextNeverFallsBackToZero(t *testing.T) {
	b := newSessionsBuilder(sessionsTestConfig()) // 30 s window
	t0 := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	running := map[string]process.ProcessState{sessModel: process.StateReady}
	decodeWithRate(t, b, t0) // context.used 41530 at t0+1s

	got := b.build(sessionsInput{Now: t0.Add(45 * time.Second), Running: running,
		Requests: []swaputil.InflightRequestEntry{
			retentionReq("r2", t0.Add(44*time.Second), map[string]string{"park_reason": "kv", "waiting_for_session": "s2"})}})
	row := rowWireMap(t, got, "s1")
	if row["phase"] != "PARKED" || rawUsed(t, row) != float64(0) {
		t.Fatalf("setup: phase=%v raw used=%v, want a raw PARKED row at 0", row["phase"], rawUsed(t, row))
	}
	disp, ok := row["display"].(map[string]any)
	if !ok {
		t.Fatalf("display = %v, want {contextUsed: 41530} (the last real context)", row["display"])
	}
	if disp["phase"] != "PARKED" {
		t.Fatalf("display.phase = %v, want PARKED (past the window the word is the raw one)", disp["phase"])
	}
	if disp["contextUsed"] != float64(41530) {
		t.Fatalf("display.contextUsed = %v, want 41530", disp["contextUsed"])
	}
	assertSessionsInvariants(t, got)
}

// A PREFILL whose counters did not move between two polls measures 0 t/s.
// The row shows the session's last measured PREFILL rate instead, and no
// rate at all when it never measured one - never "0.0 t/s".
func TestSessions_RateNeverShowsZero(t *testing.T) {
	b := newSessionsBuilder(sessionsTestConfig())
	t0 := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	running := map[string]process.ProcessState{sessModel: process.StateReady}
	prefill := func(processed int) map[string][]childSlot {
		return map[string][]childSlot{sessModel: {{ID: 0, NCtx: 262144, IsProcessing: true,
			NPromptTokens: 90000, NPromptTokensProcessed: processed}}}
	}
	req := func(id string, at time.Time) []swaputil.InflightRequestEntry {
		return []swaputil.InflightRequestEntry{retentionReq(id, at, nil)}
	}
	zeroRate := func(when string, got sessionsBody) map[string]any {
		t.Helper()
		r := got.Sessions[0]
		if r.Phase != "PREFILL" || r.Rate.TokensPerSecond == nil || *r.Rate.TokensPerSecond != 0 {
			t.Fatalf("%s setup: phase=%s rate=%+v, want a raw PREFILL measuring 0 t/s", when, r.Phase, r.Rate)
		}
		disp, ok := rowWireMap(t, got, "s1")["display"].(map[string]any)
		if !ok {
			t.Fatalf("%s: display = nil, want a display.rate replacing the raw 0", when)
		}
		drate, ok := disp["rate"].(map[string]any)
		if !ok {
			t.Fatalf("%s: display.rate = %v, want present", when, disp["rate"])
		}
		return drate
	}

	// First turn: the prefill stalls before any rate was measured.
	b.build(sessionsInput{Now: t0, Running: running, Requests: req("r1", t0), Slots: prefill(1000)})
	got := b.build(sessionsInput{Now: t0.Add(time.Second), Running: running, Requests: req("r1", t0), Slots: prefill(1000)})
	if drate := zeroRate("never measured", got); drate["tokensPerSecond"] != nil {
		t.Fatalf("never measured: display.rate.tokensPerSecond = %v, want null (no rate shown)", drate["tokensPerSecond"])
	}

	// It moves: 2000 tokens in a second is a real rate.
	got = b.build(sessionsInput{Now: t0.Add(2 * time.Second), Running: running, Requests: req("r1", t0), Slots: prefill(3000)})
	if tps := got.Sessions[0].Rate.TokensPerSecond; tps == nil || *tps <= 0 {
		t.Fatalf("setup: rate = %+v, want a measured prefill rate", got.Sessions[0].Rate)
	}
	measured := *got.Sessions[0].Rate.TokensPerSecond
	rawWord(t, got, "s1")

	// A later sample with no progress over the window reads 0 raw; the row
	// shows the last measured prefill rate.
	got = b.build(sessionsInput{Now: t0.Add(40 * time.Second), Running: running, Requests: req("r1", t0), Slots: prefill(3000)})
	got = b.build(sessionsInput{Now: t0.Add(41 * time.Second), Running: running, Requests: req("r1", t0), Slots: prefill(3000)})
	if drate := zeroRate("after a measured rate", got); drate["tokensPerSecond"] != measured || drate["kind"] != "prefill" {
		t.Fatalf("after a measured rate: display.rate = %v, want prefill %v", drate, measured)
	}
	assertSessionsInvariants(t, got)
}
