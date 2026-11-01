package server

import (
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// Slot ownership (2026-10-31, cq180 - a Splash child whose /slots reports ONE
// row). GET /api/sessions showed two sessions bound to the same slot carrying
// the same numbers:
//
//	19193195  phase HOT      slot 0  display {phase DECODE, contextUsed 89891, rate 34.9}
//	9cb63b15  phase PREFILL  slot 0  display {phase DECODE, contextUsed 89891, rate 34.9}
//
// while the child's /slots was a single processing row:
//
//	[{"id":0,"is_processing":true,"n_prompt_tokens":90173,"n_prompt_tokens_processed":89600,...}]
//
// One child slot's counters were attributed to two session rows in one
// snapshot, and both stored them as their own decode readout, so the menu
// showed the same "87.5k/262.1k · 36.6 t/s" on both rows.
//
// INVARIANT: within one build() snapshot, a child slot's counters belong to
// at most ONE session row. When the one reported slot is processing an
// in-flight request bound to session B, a HOT row of a different session A on
// that same slot id must not take the slot's counters (context.used, decoded,
// rate), and A's display readout must stay A's own last values - never B's.

func TestSessions_SlotCountersOwnedByOneRow(t *testing.T) {
	b := newSessionsBuilder(sessionsTestConfig()) // 30 s retention window
	t0 := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	running := map[string]process.ProcessState{sessModel: process.StateReady}

	// Session A decodes on slot 0 with its OWN counters: two polls one second
	// apart give it a measured decode rate, and its readout snapshot is
	// used = 38000 + 2960 + 570 = 41530 at 58.0 t/s (same shape as
	// decodeWithRate). retentionReq stamps slot_id "0", so A is bound there.
	dec := func(n int) map[string][]childSlot {
		return map[string][]childSlot{sessModel: {{ID: 0, NCtx: 262144, IsProcessing: true,
			NPromptTokens: 90000, NPromptTokensCache: 38000, NPromptTokensProcessed: 2960, NDecoded: n}}}
	}
	for i, n := range []int{512, 570} {
		at := t0.Add(time.Duration(i) * time.Second)
		got := b.build(sessionsInput{Now: at, Running: running,
			Requests: []swaputil.InflightRequestEntry{retentionReq("rA", t0.Add(-5*time.Second), nil)},
			Slots:    dec(n)})
		if len(got.Sessions) != 1 || got.Sessions[0].Phase != "DECODE" {
			t.Fatalf("setup poll %d: %+v, want one DECODE row for s1", i+1, got.Sessions)
		}
	}

	// The live snapshot: A's turn ended - A holds NO request (its HOT row
	// still claims slot 0), while session B's request was admitted and bound
	// to slot 0, and the one reported slot (id 0) is processing B's prompt
	// with DIFFERENT numbers (the witnessed 90173 / 89600). B is the only
	// admitted request bound to slot 0, so the attribution is unambiguous:
	// B reads the slot's counters. A's HOT row must not copy them - the
	// witnessed body showed A as HOT carrying B's numbers, same bug either
	// way.
	at := t0.Add(4 * time.Second)
	got := b.build(sessionsInput{Now: at, Running: running,
		Hot: map[string][]swaputil.HotSlot{sessModel: {{Slot: 0, SessionID: "s1", IdleSeconds: 2}}},
		Requests: []swaputil.InflightRequestEntry{
			{ID: "rB", Model: sessModel, Timestamp: at.Add(-time.Second),
				Metadata: map[string]string{"session_id": "s2", "slot_granted": "1", "slot_id": "0"}},
		},
		Slots: map[string][]childSlot{sessModel: {{ID: 0, NCtx: 262144, IsProcessing: true,
			NPromptTokens: 90173, NPromptTokensProcessed: 89600}}}})

	if len(got.Sessions) != 2 {
		t.Fatalf("rows = %+v, want exactly two: HOT s1 and PREFILL s2", got.Sessions)
	}
	a := rowFor(t, got, "s1")
	c := rowFor(t, got, "s2")
	if a.Phase != "HOT" || a.Slot == nil || *a.Slot != 0 {
		t.Fatalf("s1 raw row: phase=%s slot=%v, want HOT on slot 0", a.Phase, a.Slot)
	}
	if c.Phase != "PREFILL" || c.Slot == nil || *c.Slot != 0 {
		t.Fatalf("s2 raw row: phase=%s slot=%v, want PREFILL on slot 0", c.Phase, c.Slot)
	}

	// B owns the slot's counters: its PREFILL row takes them...
	if c.Context.Used != 89600 || c.Context.PromptTotal != 90173 {
		t.Fatalf("s2 context = %+v, want the processing slot's own counters (used 89600, promptTotal 90173)", c.Context)
	}
	// ...and A must NOT. B is the only admitted request bound to slot 0, so
	// its counters are B's alone: A's HOT row must not copy them into its
	// raw context, and its retained display readout must stay A's own last
	// decode values - never B's. This is the witnessed failure: both rows
	// carried the same contextUsed and the same rate.
	if a.Display == nil || a.Display.Phase != "DECODE" {
		t.Fatalf("s1 display = %+v, want the retained DECODE word", a.Display)
	}
	deref := func(v *int) any {
		if v == nil {
			return nil
		}
		return *v
	}
	t.Logf("s1 raw context = %+v, display = {phase %s, contextUsed %v, rate %+v}", a.Context, a.Display.Phase, deref(a.Display.ContextUsed), a.Display.Rate)
	t.Logf("s2 raw context = %+v, display = %+v", c.Context, c.Display)
	if a.Display.ContextUsed == nil || *a.Display.ContextUsed != 41530 {
		t.Fatalf("s1 display.contextUsed = %v, want A's own last decode readout 41530, never B's counters (raw s1 context = %+v, slot now at used %d)", a.Display.ContextUsed, a.Context, c.Context.Used)
	}
	if a.Display.Rate == nil || a.Display.Rate.TokensPerSecond == nil || *a.Display.Rate.TokensPerSecond != 58.0 {
		t.Fatalf("s1 display.rate = %+v, want A's own last measured rate 58.0 t/s", a.Display.Rate)
	}
	if a.Context.Used == 90173 || a.Context.PromptTotal == 90173 {
		t.Fatalf("s1 context = %+v, took B's slot counters (90173) - one slot's counters belong to one row", a.Context)
	}

	// B's row does not get A's readout: inside the window a PREFILL row may
	// keep displaying DECODE, but only with B's OWN numbers - B has never
	// decoded, so it has no readout to retain and its display must not carry
	// A's 41530 / 58.0.
	if c.Display != nil {
		if c.Display.ContextUsed != nil && *c.Display.ContextUsed == 41530 {
			t.Fatalf("s2 display = %+v, got A's readout 41530 - a slot's counters and readout belong to one row", c.Display)
		}
		if c.Display.Rate != nil && c.Display.Rate.TokensPerSecond != nil && *c.Display.Rate.TokensPerSecond == 58.0 {
			t.Fatalf("s2 display.rate = %+v, got A's rate 58.0 t/s", c.Display.Rate)
		}
	}

	// Wire shape: the same numbers must not appear as both rows' display
	// readout, which is what the menu rendered twice on 2026-10-31.
	aW := rowWireMap(t, got, "s1")
	cW := rowWireMap(t, got, "s2")
	dA, okA := aW["display"].(map[string]any)
	if !okA {
		t.Fatalf("s1 wire display = %v, want the retained readout", aW["display"])
	}
	if dC, okC := cW["display"].(map[string]any); okC && dC["contextUsed"] == dA["contextUsed"] {
		t.Fatalf("both rows carry display.contextUsed %v: one slot's counters attributed to two session rows", dA["contextUsed"])
	}

	assertSessionsInvariants(t, got)
}

// The display duplication (same 2026-10-31 cq180 body): Splash runs
// queue_size 2, so BOTH sessions can have a request in flight at once, and
// the fork binds both requests to slot 0. The shim reports ONE /slots row
// (id 0, keyed by list position) carrying whichever request the child is
// actually processing. Both requestRow() calls then findSlot(0) and read the
// same counters, so both rows go raw DECODE with identical context.used and
// decoded, and stampDecodeReadout stores those identical numbers as BOTH
// sessions' readout - later both retained rows show the same 89.9k / 34.9.
//
// Same invariant, display half: within one snapshot at most ONE row takes
// slot 0's counters, and the other session's raw row and its display
// readout never carry them.
func TestSessions_OneSlotCountersNotSharedByTwoDecodeRows(t *testing.T) {
	b := newSessionsBuilder(sessionsTestConfig()) // 30 s retention window
	t0 := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	running := map[string]process.ProcessState{sessModel: process.StateReady}

	// Both requests in flight, both bound to slot 0 (queue_size 2 on a
	// one-row shim). The single reported slot carries A's numbers.
	req := func(id, sess string, at time.Time) swaputil.InflightRequestEntry {
		return swaputil.InflightRequestEntry{ID: id, Model: sessModel, Timestamp: at,
			Metadata: map[string]string{"session_id": sess, "slot_granted": "1", "slot_id": "0"}}
	}
	// A decodes ALONE first, so the attribution is unambiguous and A stores
	// its own readout: used = 88000 + 1600 + n, so poll 2 reads 90170, and
	// the rate over the one second is (570-512)/1s = 58.0 t/s.
	slotA := func(n int) map[string][]childSlot {
		return map[string][]childSlot{sessModel: {{ID: 0, NCtx: 262144, IsProcessing: true,
			NPromptTokens: 90173, NPromptTokensCache: 88000, NPromptTokensProcessed: 1600, NDecoded: n}}}
	}
	for i, n := range []int{512, 570} {
		at := t0.Add(time.Duration(i) * time.Second)
		got := b.build(sessionsInput{Now: at, Running: running,
			Requests: []swaputil.InflightRequestEntry{
				req("rA", "s1", t0.Add(-5*time.Second)),
			},
			Slots: slotA(n)})
		if len(got.Sessions) != 1 || got.Sessions[0].Phase != "DECODE" {
			t.Fatalf("setup poll %d: %+v, want one DECODE row for s1", i+1, got.Sessions)
		}
	}

	// The witnessed duplication: BOTH rows read raw DECODE off the same slot
	// row, so both snapshotted the slot's counters as their own readout.
	// Between turns (both HOT inside the window) each retained display
	// showed those numbers: the menu rendered "87.5k/262.1k · 36.6 t/s" on
	// both rows. Under the fix the setup polls attribute NOTHING: two
	// admitted requests bound to the one reported slot are ambiguous, so
	// neither row takes the slot's counters and B never stores a readout of
	// its own. Between turns (both HOT inside the window) only A - which
	// decoded alone in the earlier polls - has a readout to retain; the
	// invariant below is that the two rows never end up with the same
	// counters or the same display readout.
	at := t0.Add(4 * time.Second)
	got := b.build(sessionsInput{Now: at, Running: running,
		Hot: map[string][]swaputil.HotSlot{sessModel: {
			{Slot: 0, SessionID: "s1", IdleSeconds: 2},
			{Slot: 0, SessionID: "s2", IdleSeconds: 2},
		}},
		Slots: map[string][]childSlot{sessModel: {{ID: 0, NCtx: 262144}}}})

	a := rowFor(t, got, "s1")
	c := rowFor(t, got, "s2")
	if a.Display == nil {
		t.Fatalf("s1 display = nil, want A's retained readout inside the window (s2 display = %+v)", c.Display)
	}
	derefU := func(d *sessionDisplay) any {
		if d == nil || d.ContextUsed == nil {
			return nil
		}
		return *d.ContextUsed
	}
	derefR := func(d *sessionDisplay) any {
		if d == nil || d.Rate == nil || d.Rate.TokensPerSecond == nil {
			return nil
		}
		return *d.Rate.TokensPerSecond
	}
	t.Logf("s1 display = {phase %s, contextUsed %v, rate %v}", a.Display.Phase, derefU(a.Display), derefR(a.Display))
	t.Logf("s2 display = %+v", c.Display)

	// 1. At most one row takes slot 0's counters: both rows must not carry
	// the same context.used in one snapshot.
	if a.Context.Used != 0 && a.Context.Used == c.Context.Used {
		t.Fatalf("both rows carry the slot's counters (used %d): one slot's counters attributed to two session rows", a.Context.Used)
	}
	// 2. The other session's raw row and its display readout never carry
	// them: s2 never decoded anything of its own, so its retained readout
	// must not be A's numbers (the witnessed 89891 / 34.9 on both rows).
	if derefU(c.Display) == derefU(a.Display) && derefU(a.Display) != nil {
		t.Fatalf("both rows display contextUsed %v: A's slot counters stored as B's decode readout", derefU(a.Display))
	}
	if derefR(c.Display) != nil && derefR(c.Display) == derefR(a.Display) {
		t.Fatalf("both rows display rate %v: A's rate stored as B's decode readout", derefR(a.Display))
	}

	assertSessionsInvariants(t, got)
}
