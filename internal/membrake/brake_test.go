package membrake

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/process"
)

// Everything here uses an injected sampler, child source and killer: no real
// memory pressure is ever produced and no real process is ever signalled.

type fakeSampler struct{ r Reading }

func (f *fakeSampler) Sample(r *Reading) error { *r = f.r; return nil }

type recorder struct {
	mu  sync.Mutex
	seq []string
}

func (r *recorder) add(s string) { r.mu.Lock(); r.seq = append(r.seq, s); r.mu.Unlock() }
func (r *recorder) Warnf(format string, args ...any) {
	r.add("log:" + fmt.Sprintf(format, args...))
}

type fakeKids struct {
	kids    []process.LiveChild
	loading bool
}

func (f *fakeKids) snap(dst []process.LiveChild) ([]process.LiveChild, bool) {
	return append(dst, f.kids...), f.loading
}

func testCfg(t *testing.T) config.MemoryBrakeConfig {
	c := config.DefaultMemoryBrakeConfig()
	c.MarkerPath = filepath.Join(t.TempDir(), "sub", "MARKER")
	return c
}

type rig struct {
	b    *Brake
	s    *fakeSampler
	k    *fakeKids
	rec  *recorder
	hold *Hold
	t0   time.Time
}

func newRig(t *testing.T, cfg config.MemoryBrakeConfig) *rig {
	r := &rig{s: &fakeSampler{}, rec: &recorder{}, hold: &Hold{},
		k:  &fakeKids{kids: []process.LiveChild{{ID: "cq27", Pgid: 4242}, {ID: "cq35", Pgid: 4343}}},
		t0: time.Date(2026, 9, 18, 14, 0, 0, 0, time.Local)}
	kill := func(pgid int) error { r.rec.add(fmt.Sprintf("kill:%d", pgid)); return nil }
	r.b = New(cfg, r.s, r.k.snap, kill, r.rec, r.hold)
	return r
}

// armSecs is the default arm delay (10 min) plus one sample.
const armSecs = 601

// quiet takes flat file-backed samples from 0 through armSecs, arming the brake.
func (r *rig) quiet(t *testing.T, gb float64) int {
	t.Helper()
	sec := 0
	for ; sec <= armSecs; sec++ {
		if r.step(sec, gb) {
			t.Fatalf("tripped while quiet at %ds", sec)
		}
	}
	return sec
}

// step sets file-backed (GB) and takes one sample at t0+sec.
func (r *rig) step(sec int, gb float64) bool {
	r.s.r = Reading{Wired: 30 * gib, FileBacked: uint64(gb * gib)}
	return r.b.Step(r.t0.Add(time.Duration(sec) * time.Second))
}

func (r *rig) kills() int {
	n := 0
	for _, s := range r.rec.seq {
		if strings.HasPrefix(s, "kill:") {
			n++
		}
	}
	return n
}

func TestBrakeTripsKillsEveryGroupThenLogsAndMarks(t *testing.T) {
	cfg := testCfg(t)
	r := newRig(t, cfg)
	sec := r.quiet(t, 4)
	// +6 GB burst: first breaching sample must NOT trip (confirm = 2)...
	if r.step(sec, 10) {
		t.Fatal("tripped on the first breaching sample; confirmSamples=2")
	}
	sec++
	// ...the second consecutive one does.
	if !r.step(sec, 10.2) {
		t.Fatal("did not trip on the second consecutive breach")
	}
	seq := r.rec.seq
	if len(seq) < 3 || seq[0] != "kill:4242" && seq[0] != "kill:4343" || !strings.HasPrefix(seq[1], "kill:") {
		t.Fatalf("both kills must come first, got %q", seq)
	}
	if !strings.HasPrefix(seq[2], "log:!!!!! MEMORY BRAKE TRIPPED") || !strings.Contains(seq[2], "RE-PREFILL") {
		t.Fatalf("loud log line must follow the kills, got %q", seq[2])
	}
	data, err := os.ReadFile(cfg.MarkerPath)
	if err != nil {
		t.Fatalf("marker not written: %v", err)
	}
	for _, want := range []string{"MEMORY-BRAKE", "cq27(pgid 4242)", "cq35(pgid 4343)", "growth_gb=6.20",
		"window_min_gb=4.00", "filebacked_gb=10.20", "window_span_s=300", "threshold_gb=3.50", "window_min=5"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("marker missing %q: %s", want, data)
		}
	}
	if d := r.hold.Remaining(r.t0.Add(time.Duration(sec) * time.Second)); d != 5*time.Minute {
		t.Fatalf("hold = %v, want 5m", d)
	}
	if ev := r.hold.Last(); ev == nil || len(ev.Killed) != 2 {
		t.Fatalf("last event not recorded: %+v", ev)
	}

	// A trip starts a fresh arm delay; the next trip appends, never truncates.
	end := sec + armSecs
	for sec++; sec <= end; sec++ {
		if r.step(sec, 10.2) {
			t.Fatalf("re-tripped inside the arm delay at %ds", sec)
		}
	}
	r.step(sec, 17)
	r.step(sec+1, 17)
	data, _ = os.ReadFile(cfg.MarkerPath)
	if n := strings.Count(string(data), "MEMORY-BRAKE"); n != 2 {
		t.Fatalf("marker lines = %d, want 2 (append)", n)
	}
}

func TestBrakeSingleSpikeDoesNotTrip(t *testing.T) {
	r := newRig(t, testCfg(t))
	sec := r.quiet(t, 4)
	r.step(sec, 11) // one-sample glitch
	r.step(sec+1, 4)
	r.step(sec+2, 4)
	if r.kills() != 0 {
		t.Fatal("a single breaching sample must not kill")
	}
}

// The slow shape (14:47): 4 GB spread over 4 minutes is inside one 5-minute
// window, so it fires even though no 30 s step is large.
func TestBrakeSlowClimbInsideWindowTrips(t *testing.T) {
	r := newRig(t, testCfg(t))
	sec := r.quiet(t, 4)
	for i := 1; i <= 240; i++ {
		if r.step(sec+i, 4+float64(i)*4/240) {
			if g := 4 * float64(i) / 240; g < 3.5 {
				t.Fatalf("tripped at growth %.2f GB", g)
			}
			return
		}
	}
	t.Fatal("4 GB within 4 min must trip a 3.5 GB / 5 min brake")
}

// Growth is measured against the window's MINIMUM, not its first sample.
func TestBrakeGrowthIsAgainstWindowMinimum(t *testing.T) {
	r := newRig(t, testCfg(t))
	sec := r.quiet(t, 6)
	for i := 1; i <= 60; i++ { // dip to 2 GB
		r.step(sec+i, 2)
	}
	sec += 60
	// 5.6 GB is below the window start (6) but 3.6 above the window min (2).
	if r.step(sec+1, 5.6) || !r.step(sec+2, 5.6) {
		t.Fatal("growth from the window minimum must trip")
	}
}

// A steady creep below growthGB per window never fires, however long.
func TestBrakeSlowCreepDoesNotTrip(t *testing.T) {
	r := newRig(t, testCfg(t))
	sec := r.quiet(t, 2)
	// 3 GB per 5 min, sustained 30 min.
	for i := 1; i <= 1800; i++ {
		r.step(sec+i, 2+float64(i)*3/300)
	}
	if r.kills() != 0 {
		t.Fatal("growth below growthGB per window must not kill")
	}
}

// Old samples leave the window: a rise that took longer than windowMinutes
// does not fire even though its total exceeds growthGB.
func TestBrakeOldMinimumExpires(t *testing.T) {
	r := newRig(t, testCfg(t))
	sec := r.quiet(t, 2)
	for i := 1; i <= 310; i++ { // 2 -> 5 GB over 5 min 10 s
		r.step(sec+i, 2+float64(i)*3/310)
	}
	sec += 310
	for i := 1; i <= 400; i++ { // then +0.6 GB (total 3.6 from 2, but 2 has aged out)
		r.step(sec+i, 5+float64(i)*0.6/400)
	}
	if r.kills() != 0 {
		t.Fatal("a minimum older than the window must not count")
	}
}

// Only file-backed pages count: wired and swap growth are not the signal.
func TestBrakeIgnoresWiredAndSwap(t *testing.T) {
	r := newRig(t, testCfg(t))
	sec := r.quiet(t, 4)
	for i := 1; i <= 60; i++ {
		r.s.r = Reading{Wired: uint64(30+i) * gib, FileBacked: 4 * gib, SwapUsed: uint64(i) * gib}
		if r.b.Step(r.t0.Add(time.Duration(sec+i) * time.Second)) {
			t.Fatal("wired/swap growth must not trip")
		}
	}
}

func TestBrakeDisarmedWhileLoadingAndForArmAfterMinutes(t *testing.T) {
	r := newRig(t, testCfg(t))
	r.k.loading = true
	for sec := 0; sec <= 60; sec++ {
		r.step(sec, 4+float64(sec)*0.5) // GGUF read floods the file cache
	}
	if r.kills() != 0 {
		t.Fatal("must not trip while a child is starting")
	}
	r.k.loading = false
	// Ready, but under 10 min: a 5 GB jump must not fire.
	for sec := 61; sec <= 600; sec++ {
		v := 34.0
		if sec > 300 {
			v = 2 + float64(sec-300)*0.05
		}
		if r.step(sec, v) {
			t.Fatalf("tripped %ds after ready; arm delay is 10 min", sec-60)
		}
	}
	// A reload restarts the arm delay.
	r.k.loading = true
	r.step(601, 30)
	r.k.loading = false
	for sec := 602; sec <= 1100; sec++ {
		if r.step(sec, 2+float64(sec%200)*0.05) {
			t.Fatalf("tripped %ds after a reload; arm delay restarts on every load", sec-601)
		}
	}
	// Past the arm delay the same shape does fire.
	for sec := 1101; sec <= 1300; sec++ {
		if r.step(sec, 2+float64(sec-1100)*0.05) {
			return
		}
	}
	t.Fatal("must fire once armed")
}

func TestBrakeNoChildrenNoTrip(t *testing.T) {
	r := newRig(t, testCfg(t))
	r.k.kids = nil
	for sec := 0; sec <= 1200; sec++ {
		r.step(sec, float64(sec)/10)
	}
	if r.kills() != 0 || r.hold.Remaining(r.t0.Add(time.Hour)) != 0 {
		t.Fatal("nothing to kill: no trip, no hold")
	}
}

func TestBrakeHotPathDoesNotAllocate(t *testing.T) {
	r := newRig(t, testCfg(t))
	sec := 0
	allocs := testing.AllocsPerRun(500, func() {
		sec++
		r.step(sec, 4+float64(sec%3)*0.1)
	})
	if allocs != 0 {
		t.Fatalf("Step allocated %.1f times per sample; the hot path must not allocate", allocs)
	}
}

func TestHoldBlocksAndExpires(t *testing.T) {
	h := &Hold{}
	if err := h.Wait(context.Background(), "cq27", nil); err != nil {
		t.Fatal("no hold: Wait must return at once")
	}
	h.Set(time.Now().Add(300*time.Millisecond), &Event{})
	start := time.Now()
	rec := &recorder{}
	if err := h.Wait(context.Background(), "cq27", rec); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el < 250*time.Millisecond {
		t.Fatalf("Wait returned after %v, hold was 300ms", el)
	}
	if len(rec.seq) != 1 || !strings.Contains(rec.seq[0], "MEMORY BRAKE HOLD") {
		t.Fatalf("park must be logged once, got %q", rec.seq)
	}
	if h.Remaining(time.Now()) != 0 {
		t.Fatal("hold must have expired")
	}

	h.Set(time.Now().Add(time.Hour), &Event{})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := h.Wait(ctx, "cq27", nil); err == nil {
		t.Fatal("a cancelled waiter must not be released into a load")
	}
}

func TestCurrentStatusReflectsHold(t *testing.T) {
	old := DefaultHold
	defer func() { DefaultHold = old }()
	DefaultHold = &Hold{}
	if s := CurrentStatus(); s.Holding || s.RemainingSeconds != 0 {
		t.Fatalf("idle status wrong: %+v", s)
	}
	DefaultHold.Set(time.Now().Add(90*time.Second), &Event{Killed: []string{"cq27(pgid 1)"}})
	s := CurrentStatus()
	if !s.Holding || s.RemainingSeconds < 89 || s.RemainingSeconds > 90 || s.Last == nil {
		t.Fatalf("holding status wrong: %+v", s)
	}
}

func TestKillGroupRefusesDangerousPgids(t *testing.T) {
	for _, p := range []int{-1, 0, 1} {
		if killGroup(p) == nil {
			t.Fatalf("killGroup(%d) must refuse", p)
		}
	}
}
