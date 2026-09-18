package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// The state trace is the box's state machine as a log: one canonical line
// per CHANGE of the box array (resident, per-slot phase with owning session,
// queued requests with their park reason, the cooldown), consecutive
// identical arrays deduped, so a test can assert "after this turn, exactly
// these lines" and a human can read what the router did. The vocabulary is
// the set of states observed by hand on 2026-09-10 (llama-cm
// docs/research/2026-09-10-cooldown-dogfood-ledger.md); the lines below are
// that ledger's O2, O9, O10 and O12 verbatim.

func entry(id, model, session string, meta map[string]string, tokens int64) swaputil.InflightRequestEntry {
	m := map[string]string{"session_id": session}
	for k, v := range meta {
		m[k] = v
	}
	return swaputil.InflightRequestEntry{ID: id, Model: model, ReqPath: "/v1/messages", Method: "POST", Metadata: m, RespTokens: tokens}
}

func TestStateTrace_LinesFollowTheLedger(t *testing.T) {
	clk := time.Date(2026, 9, 10, 9, 5, 2, 0, time.UTC)
	tr := newStateTrace(func() time.Time { return clk }, 100)
	alias := func(id string) string {
		return map[string]string{"Qwen3.6-35B-A3B-APEX-I-Balanced-384K": "cq35h", "Qwen3.8-27B-UD-Q5_K_XL": "cq27"}[id]
	}

	// O2: resident serving on both slots, one same-model request parked at the cap.
	tr.Observe(traceSnapshot{
		Alias:    alias,
		Resident: "Qwen3.6-35B-A3B-APEX-I-Balanced-384K", ResidentState: process.StateReady,
		Requests: []swaputil.InflightRequestEntry{
			entry("1", "Qwen3.6-35B-A3B-APEX-I-Balanced-384K", "725558cb-17fa", map[string]string{"slot_affinity": "0", "slot_granted": "1"}, 32),
			entry("2", "Qwen3.6-35B-A3B-APEX-I-Balanced-384K", "fd998b45-7c70", map[string]string{"slot_affinity": "1", "slot_granted": "1"}, 0),
			entry("3", "Qwen3.6-35B-A3B-APEX-I-Balanced-384K", "fd998b45-7c70", map[string]string{"slot_affinity": "1", "park_reason": "cap"}, 0),
		},
	})
	// Same array again (a tick with nothing changed): no new line.
	tr.Observe(traceSnapshot{
		Alias:    alias,
		Resident: "Qwen3.6-35B-A3B-APEX-I-Balanced-384K", ResidentState: process.StateReady,
		Requests: []swaputil.InflightRequestEntry{
			entry("1", "Qwen3.6-35B-A3B-APEX-I-Balanced-384K", "725558cb-17fa", map[string]string{"slot_affinity": "0", "slot_granted": "1"}, 40),
			entry("2", "Qwen3.6-35B-A3B-APEX-I-Balanced-384K", "fd998b45-7c70", map[string]string{"slot_affinity": "1", "slot_granted": "1"}, 0),
			entry("3", "Qwen3.6-35B-A3B-APEX-I-Balanced-384K", "fd998b45-7c70", map[string]string{"slot_affinity": "1", "park_reason": "cap"}, 0),
		},
	})
	// O9: cross-model requests parked behind a busy resident.
	clk = clk.Add(6 * time.Minute)
	tr.Observe(traceSnapshot{
		Alias:    alias,
		Resident: "Qwen3.6-35B-A3B-APEX-I-Balanced-384K", ResidentState: process.StateReady,
		Requests: []swaputil.InflightRequestEntry{
			entry("16", "Qwen3.6-35B-A3B-APEX-I-Balanced-384K", "725558cb-17fa", map[string]string{"slot_affinity": "0", "slot_granted": "1"}, 32),
			entry("14", "Qwen3.8-27B-UD-Q5_K_XL", "6bc24690-22c3", map[string]string{"slot_affinity": "1", "park_reason": "busy"}, 0),
			entry("15", "Qwen3.8-27B-UD-Q5_K_XL", "6bc24690-22c3", map[string]string{"slot_affinity": "0", "park_reason": "busy"}, 0),
		},
	})
	// O10: the resident drained, cooldown with hot slots.
	clk = clk.Add(90 * time.Second)
	tr.Observe(traceSnapshot{
		Alias:    alias,
		Resident: "Qwen3.6-35B-A3B-APEX-I-Balanced-384K", ResidentState: process.StateReady,
		Requests: []swaputil.InflightRequestEntry{
			entry("14", "Qwen3.8-27B-UD-Q5_K_XL", "6bc24690-22c3", map[string]string{"slot_affinity": "1", "park_reason": "cooldown"}, 0),
			entry("15", "Qwen3.8-27B-UD-Q5_K_XL", "6bc24690-22c3", map[string]string{"slot_affinity": "0", "park_reason": "cooldown"}, 0),
		},
		Cooldown: &swaputil.Cooldown{EvicteeModel: "Qwen3.6-35B-A3B-APEX-I-Balanced-384K", NextModel: "Qwen3.8-27B-UD-Q5_K_XL", Waiting: 2, RemainingSeconds: 582,
			Slots: []swaputil.HotSlot{{Slot: 0, SessionID: "725558cb-17fa", IdleSeconds: 324}, {Slot: 1, SessionID: "fd998b45-7c70", IdleSeconds: 362}}},
	})
	// The countdown alone ticking must NOT produce a line.
	tr.Observe(traceSnapshot{
		Alias:    alias,
		Resident: "Qwen3.6-35B-A3B-APEX-I-Balanced-384K", ResidentState: process.StateReady,
		Requests: []swaputil.InflightRequestEntry{
			entry("14", "Qwen3.8-27B-UD-Q5_K_XL", "6bc24690-22c3", map[string]string{"slot_affinity": "1", "park_reason": "cooldown"}, 0),
			entry("15", "Qwen3.8-27B-UD-Q5_K_XL", "6bc24690-22c3", map[string]string{"slot_affinity": "0", "park_reason": "cooldown"}, 0),
		},
		Cooldown: &swaputil.Cooldown{EvicteeModel: "Qwen3.6-35B-A3B-APEX-I-Balanced-384K", NextModel: "Qwen3.8-27B-UD-Q5_K_XL", Waiting: 2, RemainingSeconds: 559,
			Slots: []swaputil.HotSlot{{Slot: 0, SessionID: "725558cb-17fa", IdleSeconds: 347}, {Slot: 1, SessionID: "fd998b45-7c70", IdleSeconds: 385}}},
	})
	// O12: the swap; cq27 loading, nothing granted, no cooldown.
	clk = clk.Add(10 * time.Minute)
	tr.Observe(traceSnapshot{
		Alias:    alias,
		Resident: "Qwen3.8-27B-UD-Q5_K_XL", ResidentState: process.StateStarting,
		Requests: []swaputil.InflightRequestEntry{
			entry("14", "Qwen3.8-27B-UD-Q5_K_XL", "6bc24690-22c3", map[string]string{"slot_affinity": "1", "park_reason": "loading"}, 0),
			entry("15", "Qwen3.8-27B-UD-Q5_K_XL", "6bc24690-22c3", map[string]string{"slot_affinity": "0", "park_reason": "loading"}, 0),
		},
	})

	want := []string{
		"09:05:02 resident=cq35h slots=[0:DECODE(725558cb) 1:PREFILL(fd998b45)] queue=[cq35h:cap(fd998b45)] cooldown=-",
		"09:11:02 resident=cq35h slots=[0:DECODE(725558cb)] queue=[cq27:busy(6bc24690) cq27:busy(6bc24690)] cooldown=-",
		"09:12:32 resident=cq35h slots=[0:HOT(725558cb) 1:HOT(fd998b45)] queue=[cq27:cooldown(6bc24690) cq27:cooldown(6bc24690)] cooldown=cq35h->cq27",
		"09:22:32 resident=cq27(starting) slots=[] queue=[cq27:loading(6bc24690) cq27:loading(6bc24690)] cooldown=-",
	}
	if got := tr.Lines(); !reflect.DeepEqual(got, want) {
		t.Fatalf("trace lines:\n got=%q\nwant=%q", got, want)
	}
}

// TestStateTrace_NoWaiterCooldownStillReadsDash pins the trace's own
// vocabulary against the 2026-09-18 no-waiter cooldown addition
// (cooldownSnapshotIdle): the trace line is about queue TRANSITIONS
// ("cooldown=cq35h->cq27" means a swap is waited on), not the resident's own
// idle-grace state, which the menu now shows on its own but the trace never
// did before this and must not start now. A cooldown with an empty
// NextModel (nothing queued) renders "cooldown=-", same as no cooldown at
// all - even though the hot slots it protects are real and still listed.
func TestStateTrace_NoWaiterCooldownStillReadsDash(t *testing.T) {
	clk := time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)
	tr := newStateTrace(func() time.Time { return clk }, 100)
	alias := func(id string) string { return map[string]string{"cq35": "cq35"}[id] }

	tr.Observe(traceSnapshot{
		Alias:    alias,
		Resident: "cq35", ResidentState: process.StateReady,
		// No live requests: the resident is idle inside its own grace, which
		// is exactly what a no-waiter cooldown means. The hot slot below is
		// what is kept warm for a session that paused, not a running request.
		Requests: nil,
		Cooldown: &swaputil.Cooldown{EvicteeModel: "cq35", NextModel: "", Waiting: 0, RemainingSeconds: 240,
			Slots: []swaputil.HotSlot{{Slot: 0, SessionID: "725558cb-17fa", IdleSeconds: 5}}},
	})

	want := []string{
		"09:00:00 resident=cq35 slots=[0:HOT(725558cb)] queue=[] cooldown=-",
	}
	if got := tr.Lines(); !reflect.DeepEqual(got, want) {
		t.Fatalf("trace lines:\n got=%q\nwant=%q", got, want)
	}
}

func TestStateTrace_RingBufferKeepsTheLastN(t *testing.T) {
	clk := time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)
	tr := newStateTrace(func() time.Time { return clk }, 2)
	for i, res := range []string{"a", "b", "c"} {
		clk = clk.Add(time.Second)
		tr.Observe(traceSnapshot{Alias: func(s string) string { return s }, Resident: res, ResidentState: process.StateReady, Requests: nil})
		_ = i
	}
	got := tr.Lines()
	if len(got) != 2 || got[0][9:19] != "resident=b" || got[1][9:19] != "resident=c" {
		t.Fatalf("ring buffer lines=%q want the last two (b, c)", got)
	}
}

// GET /api/state-trace serves the lines, oldest first.
func TestServer_HandleAPIStateTrace(t *testing.T) {
	local := newStubRouter([]string{"m1"}, "")
	local.running = map[string]process.ProcessState{"m1": process.StateReady}
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"m1": {}}}
	s.trace = newStateTrace(time.Now, 10)
	s.observeState()

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/state-trace", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	var body struct {
		Lines []string `json:"lines"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Lines) != 1 || body.Lines[0][9:] != "resident=m1 slots=[] queue=[] cooldown=-" {
		t.Fatalf("lines=%q want one line for the idle resident", body.Lines)
	}
}
