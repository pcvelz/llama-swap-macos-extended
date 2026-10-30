package scheduler

import (
	"io"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
)

// User ruling (2026-10-30): the model cooldown starts only after the decode
// retention. Inside that delay nothing is cooling yet - Cooldown() is already
// nil there - so a request held behind the resident is still waiting for it
// to finish answering: park_reason "busy" with no countdown. Before this, the
// reason read "cooldown" with the full completion+retention+grace deadline,
// so every waiting row flipped "finishes answering" <-> "cools down 10:10" at
// each turn boundary of the resident's tool loop.
func TestFIFO_ParkReason_BusyDuringRetentionDelayThenCooldown(t *testing.T) {
	eff := newFakeEffects()
	eff.states["a"] = process.StateReady
	eff.states["b"] = process.StateStopped
	clk := time.Unix(1000, 0)
	models := map[string]config.ModelConfig{"a": {SwapGraceSeconds: 60}, "b": {}}
	s := NewFIFO("test", logmon.NewWriter(io.Discard),
		&stubPlanner{evict: map[string][]string{"b": {"a"}}},
		config.FifoConfig{DecodeRetentionSeconds: 10}, models, eff)
	s.now = func() time.Time { return clk }

	s.OnRequest(req("a"))
	s.OnServeDone(ServeDoneEvent{ModelID: "a"}) // completion at t=1000

	clk = clk.Add(5 * time.Second) // t=1005: inside the 10 s retention
	rec := newReasonRecorder()
	r := reqCh("b")
	r.Ctx = rec.ctx()
	s.OnRequest(r)
	if got := rec.reason(); got != ParkBusy {
		t.Fatalf("park_reason=%q want %q inside the retention delay (the cooldown has not started)", got, ParkBusy)
	}
	if got := rec.values["waiting_for_until"]; got != "" {
		t.Fatalf("waiting_for_until=%q want empty inside the retention delay (no countdown yet)", got)
	}

	clk = clk.Add(6 * time.Second) // t=1011: retention over, grace running
	s.OnTick()
	if got := rec.reason(); got != ParkCooldown {
		t.Fatalf("park_reason=%q want %q once the delayed grace has started", got, ParkCooldown)
	}
	want := time.Unix(1070, 0).Format(time.RFC3339) // completion + 10 + 60
	if got := rec.values["waiting_for_until"]; got != want {
		t.Fatalf("waiting_for_until=%q want %q (completion+retention+grace)", got, want)
	}
	if got := eff.startsFor("b"); got != 0 {
		t.Fatalf("StartSwap(b)=%d want 0: protection is unchanged", got)
	}
}
