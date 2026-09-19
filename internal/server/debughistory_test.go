package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/membrake"
)

type fakeDHSampler struct{ r membrake.Reading }

func (f *fakeDHSampler) Sample(r *membrake.Reading) error { *r = f.r; return nil }

func dhBody(phase string, used int) sessionsBody {
	slot := 0
	p := 0.5
	return sessionsBody{
		Resident: &sessionsResident{Model: sessModel, Alias: "cq27", State: "ready", Window: 262144, Slots: 2},
		Sessions: []sessionEntry{{
			SessionID: "69699f8b-0b1e-4c3a-9f1e-2d7c5a1b9e00", SessionShort: "69699f8b",
			Tier: "default", Phase: phase, Slot: &slot, Progress: &p,
			Context: sessionContext{Used: used, Cached: used / 2, Processed: used - used/2, PromptTotal: 101824},
		}},
	}
}

func newTestHistory(retainMin int) (*debugHistory, *fakeDHSampler) {
	s := &fakeDHSampler{r: membrake.Reading{Wired: 31 << 30, FileBacked: 3 << 30, SwapUsed: 2 << 30}}
	return newDebugHistory(config.DebugHistoryConfig{Enabled: true, IntervalMs: 1000, RetainMinutes: retainMin}, s), s
}

// The PREFILL -> HOT -> PREFILL cycle a 300 s first-byte cut produces must
// come back as three phase events, with one sample per interval.
func TestDebugHistory_PhaseTransitionsAndSampleCadence(t *testing.T) {
	h, _ := newTestHistory(15)
	t0 := time.Unix(1_800_000_000, 0)
	h.record(t0, dhBody("PREFILL", 80000))
	h.record(t0.Add(300*time.Millisecond), dhBody("PREFILL", 80100)) // same interval: no new sample
	h.record(t0.Add(1*time.Second), dhBody("HOT", 86026))
	h.record(t0.Add(34*time.Second), dhBody("PREFILL", 86026))

	v := h.view(time.Time{}, "")
	if len(v.Samples) != 3 {
		t.Fatalf("samples=%d, want 3 (the sub-interval call must not add one)", len(v.Samples))
	}
	var phases []string
	for _, e := range v.Events {
		if e.Kind == "phase" {
			phases = append(phases, e.Detail["from"].(string)+">"+e.Detail["to"].(string))
		}
	}
	want := []string{">PREFILL", "PREFILL>HOT", "HOT>PREFILL"}
	if len(phases) != len(want) {
		t.Fatalf("phase events %v, want %v", phases, want)
	}
	for i := range want {
		if phases[i] != want[i] {
			t.Fatalf("phase events %v, want %v", phases, want)
		}
	}
	if m := v.Samples[0].Mem; m == nil || m.WiredGB != 31 || m.FileBackedGB != 3 || m.SwapGB != 2 {
		t.Fatalf("mem = %+v", v.Samples[0].Mem)
	}
	if v.Samples[0].Resident != "cq27:ready" {
		t.Fatalf("resident = %q", v.Samples[0].Resident)
	}
}

func TestDebugHistory_RetainWindowPrunes(t *testing.T) {
	h, _ := newTestHistory(1)
	t0 := time.Unix(1_800_000_000, 0)
	for i := 0; i < 180; i++ {
		h.record(t0.Add(time.Duration(i)*time.Second), dhBody("PREFILL", 1000+i))
	}
	v := h.view(time.Time{}, "")
	if len(v.Samples) > 61 || len(v.Samples) < 59 {
		t.Fatalf("samples=%d, want ~60 for a 1-minute retain", len(v.Samples))
	}
	if first := v.Samples[0].T; first.Before(t0.Add(179*time.Second - time.Minute)) {
		t.Fatalf("oldest sample %v is outside the retain window", first)
	}
}

func TestDebugHistory_RequestEvents(t *testing.T) {
	h, _ := newTestHistory(15)
	now := time.Unix(1_800_000_000, 0)
	h.requestDone(now, "GET", "/api/sessions", 200, 900, time.Millisecond, "default", "")
	h.requestDone(now, "POST", "/v1/messages", 502, 0, 6*time.Minute, "default", "")
	h.requestDone(now, "POST", "/v1/messages", 200, 8114, 16*time.Second, "default", "zero-output-budget")
	v := h.view(time.Time{}, "")
	if len(v.Events) != 2 {
		t.Fatalf("events=%d, want 2 (a successful GET is a poll, not an event)", len(v.Events))
	}
	if v.Events[0].Detail["status"] != 502 || v.Events[0].Detail["durationMs"] != int64(360000) {
		t.Fatalf("502 event detail = %v", v.Events[0].Detail)
	}
	if v.Events[1].Detail["cut"] != "zero-output-budget" {
		t.Fatalf("cut not recorded: %v", v.Events[1].Detail)
	}
}

func TestDebugHistory_ViewFiltersSessionAndMinutes(t *testing.T) {
	h, _ := newTestHistory(15)
	t0 := time.Now().Add(-10 * time.Minute)
	h.record(t0, dhBody("PREFILL", 1000))
	h.record(time.Now(), dhBody("DECODE", 2000))
	recent := h.view(time.Now().Add(-time.Minute), "")
	if len(recent.Samples) != 1 {
		t.Fatalf("minutes filter: samples=%d, want 1", len(recent.Samples))
	}
	other := h.view(time.Time{}, "deadbeef")
	for _, s := range other.Samples {
		if len(s.Sessions) != 0 {
			t.Fatalf("session filter leaked %+v", s.Sessions)
		}
	}
	for _, id := range []string{"6969", "69699f8b-0b1e-4c3a-9f1e-2d7c5a1b9e00"} {
		if got := h.view(time.Time{}, id); len(got.Samples[0].Sessions) != 1 {
			t.Fatalf("session %q did not match", id)
		}
	}
}

func TestDebugHistory_BrakeEventRecordedOnce(t *testing.T) {
	h, _ := newTestHistory(15)
	t0 := time.Unix(1_800_000_000, 0)
	b := dhBody("DECODE", 5000)
	b.MemoryBrake.Last = &membrake.Event{At: t0, GrowthGB: 3.6, FileGB: 7.9}
	h.record(t0, b)
	h.record(t0.Add(time.Second), b)
	n := 0
	for _, e := range h.view(time.Time{}, "").Events {
		if e.Kind == "brake" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("brake events=%d, want 1", n)
	}
}

func TestDebugHistory_HandlerDisabledAndEnabled(t *testing.T) {
	s := &Server{}
	rr := httptest.NewRecorder()
	s.handleAPIDebugHistory(rr, httptest.NewRequest(http.MethodGet, "/api/debug/history", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("disabled: code=%d, want 404", rr.Code)
	}

	s.debugHistory, _ = newTestHistory(15)
	s.debugHistory.record(time.Now(), dhBody("PREFILL", 1000))
	rr = httptest.NewRecorder()
	s.handleAPIDebugHistory(rr, httptest.NewRequest(http.MethodGet, "/api/debug/history?minutes=5&session=69699f8b", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("enabled: code=%d", rr.Code)
	}
	var body debugHistoryBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Enabled || len(body.Samples) != 1 || body.IntervalMs != 1000 || body.RetainMinutes != 15 {
		t.Fatalf("body = %+v", body)
	}
}

func TestDebugHistory_ConfigDefaultOffAndIntervalFloor(t *testing.T) {
	if config.DefaultDebugHistoryConfig().Enabled {
		t.Fatal("debug history must default to off")
	}
	h := newDebugHistory(config.DebugHistoryConfig{Enabled: true, IntervalMs: 200, RetainMinutes: 1}, nil)
	if h.interval != time.Second {
		t.Fatalf("interval=%v, want the 1 s floor of the session loop", h.interval)
	}
}
