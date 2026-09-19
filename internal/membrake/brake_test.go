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
	// alive is the set of process groups the fake still reports as running;
	// kill removes nothing from it, the test decides when a group exits.
	alive map[int]bool
	// onEvict, when set, runs for every evicted file (e.g. to lower the
	// sampler's file-backed value).
	onEvict func(path string)
}

func newRig(t *testing.T, cfg config.MemoryBrakeConfig) *rig {
	r := &rig{s: &fakeSampler{}, rec: &recorder{}, hold: &Hold{}, alive: map[int]bool{},
		k: &fakeKids{kids: []process.LiveChild{
			{ID: "cq27", Pgid: 4242, Files: []string{"/models/cq27.gguf"}},
			{ID: "cq35", Pgid: 4343, Files: []string{"/models/cq35.gguf", "/models/mmproj-cq35.gguf"}},
		}},
		t0: time.Date(2026, 9, 18, 14, 0, 0, 0, time.Local)}
	kill := func(pgid int) error { r.rec.add(fmt.Sprintf("kill:%d", pgid)); return nil }
	r.b = New(cfg, r.s, r.k.snap, kill, r.rec, r.hold)
	r.b.alive = func(pgid int) bool { return r.alive[pgid] }
	r.b.evict = func(path string) error {
		r.rec.add("evict:" + path)
		if r.onEvict != nil {
			r.onEvict(path)
		}
		return nil
	}
	return r
}

func (r *rig) count(prefix string) int {
	r.rec.mu.Lock()
	defer r.rec.mu.Unlock()
	n := 0
	for _, s := range r.rec.seq {
		if strings.HasPrefix(s, prefix) {
			n++
		}
	}
	return n
}

// armSecs is ten minutes (two full windows) plus one sample: long enough to arm
// a brake configured with armAfterMinutes: 10.
const armSecs = 601

// quiet takes flat file-backed samples from 0 through armSecs.
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

func (r *rig) kills() int { return r.count("kill:") }

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
	if !r.hold.Holding() {
		t.Fatal("a trip must shut the admission gate")
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
	if n := strings.Count(string(data), "MEMORY-BRAKE killed="); n != 2 {
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

// The default arms at ready (armAfterMinutes: 0); the knob still delays arming
// when set, and every load restarts the delay.
func TestBrakeDisarmedWhileLoadingAndForArmAfterMinutes(t *testing.T) {
	cfg := testCfg(t)
	cfg.ArmAfterMinutes = 10
	r := newRig(t, cfg)
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

// Event 8 (2026-09-19 14:49 -> 14:54): the reload after a brake kill loads
// into 36 GB of file cache, settles at 13 GB for five minutes, then file-backed
// jumps 13.5 -> 30.9 GB in one 38 s tick. The growth rule measures against the
// window MINIMUM and file-backed FALLS after a load, so there is nothing for a
// warm-up to protect: the default brake must be armed and trip here.
func TestBrake_ArmsAtReadyAndCatchesTheReloadCollapse(t *testing.T) {
	r := newRig(t, testCfg(t))
	r.k.loading = true
	sec := 0
	for ; sec < 30; sec++ { // the load: leftover cache, falling
		r.step(sec, 36-float64(sec)*0.2)
	}
	r.k.loading = false
	for end := sec + 300; sec < end; sec++ { // ready, 5 min flat at ~13 GB
		if r.step(sec, 13) {
			t.Fatalf("tripped on flat file-backed at %ds", sec)
		}
	}
	for i := 1; i <= 38; i++ { // 13.54 -> 30.94 GB within one observer tick
		if r.step(sec+i, 13.54+float64(i)*(30.94-13.54)/38) {
			if r.kills() != 2 {
				t.Fatalf("tripped without killing both children: %q", r.rec.seq)
			}
			return
		}
	}
	t.Fatal("the 14:54 collapse, 5 min after ready, did not trip the default brake")
}

// Event 8 (2026-09-19): after the 14:43:30 kill the dead server's weights
// stayed behind as 32-37 GB of file cache; the fixed 5-minute hold expired,
// the model reloaded at 14:49 into that state and collapsed at 14:54. A reload
// requested after a kill must wait - not fail - until file-backed has drained
// below drainBelowGB, however long that takes.
func TestBrake_ReloadAfterAKillWaitsUntilFileBackedDrains(t *testing.T) {
	r := newRig(t, testCfg(t))
	sec := r.quiet(t, 4)
	r.step(sec, 10)
	sec++
	if !r.step(sec, 10.2) {
		t.Fatal("setup: the burst did not trip")
	}
	r.k.kids = nil // killed
	killSec := sec

	released := make(chan error, 1)
	go func() { released <- r.hold.Wait(context.Background(), "cq27", nil) }()
	isReleased := func() bool {
		select {
		case err := <-released:
			if err != nil {
				t.Fatalf("the parked reload failed: %v", err)
			}
			return true
		case <-time.After(20 * time.Millisecond):
			return false
		}
	}

	// Ten minutes of leftover cache at 36 GB: well past the old 5-minute hold.
	for end := sec + 600; sec < end; sec++ {
		r.step(sec+1, 36)
		if sec%60 == 0 && isReleased() {
			t.Fatalf("reload admitted %ds after the kill with file-backed still at 36 GB", sec-killSec)
		}
	}
	if isReleased() {
		t.Fatal("reload admitted with file-backed still at 36 GB")
	}
	// Drained: admitted once file-backed stays below 10 GB.
	for end := sec + 120; sec < end; sec++ {
		r.step(sec+1, 8)
	}
	select {
	case err := <-released:
		if err != nil {
			t.Fatalf("the parked reload failed instead of waiting: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reload still parked after file-backed drained to 8 GB")
	}
}

func TestBrakeNoChildrenNoTrip(t *testing.T) {
	r := newRig(t, testCfg(t))
	r.k.kids = nil
	for sec := 0; sec <= 1200; sec++ {
		r.step(sec, float64(sec)/10)
	}
	if r.kills() != 0 || r.hold.Holding() {
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

func TestHold_WaitParksUntilReleased(t *testing.T) {
	h := &Hold{}
	if err := h.Wait(context.Background(), "cq27", nil); err != nil {
		t.Fatal("no hold: Wait must return at once")
	}
	h.Set(&Event{}, 10*gib)
	h.fileBacked.Store(36 * gib)
	rec := &recorder{}
	done := make(chan error, 1)
	go func() { done <- h.Wait(context.Background(), "cq27", rec) }()
	select {
	case <-done:
		t.Fatal("Wait returned while the gate is shut")
	case <-time.After(100 * time.Millisecond):
	}
	h.Release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Release did not wake the parked load")
	}
	if len(rec.seq) != 1 || !strings.Contains(rec.seq[0], "MEMORY BRAKE HOLD") || !strings.Contains(rec.seq[0], "below 10.00 GB (now 36.00 GB)") {
		t.Fatalf("park must be logged once with the drain level and the current value, got %q", rec.seq)
	}
	if h.Holding() {
		t.Fatal("gate must be open after Release")
	}

	h.Set(&Event{}, 10*gib)
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
	DefaultHold.Set(&Event{Killed: []string{"cq27(pgid 1)"}}, 10*gib)
	DefaultHold.fileBacked.Store(int64(36.5 * gib))
	s := CurrentStatus()
	if !s.Holding || s.FileBackedGB != 36.5 || s.DrainBelowGB != 10 || s.Last == nil {
		t.Fatalf("holding status wrong: %+v", s)
	}
}

// After a kill: nothing is evicted while a killed group is still alive (its
// pages are not evictable yet); once it exits every model file seen is
// evicted, file-backed before and after is logged and marked, and the gate
// opens only after file-backed has stayed below drainBelowGB for drainSettle.
func TestBrake_EvictsAfterExitThenReleasesOnDrain(t *testing.T) {
	cfg := testCfg(t)
	r := newRig(t, cfg)
	sec := r.quiet(t, 4)
	r.alive[4242], r.alive[4343] = true, true
	r.step(sec, 10)
	sec++
	if !r.step(sec, 10.2) {
		t.Fatal("setup: the burst did not trip")
	}
	r.k.kids = nil
	for end := sec + 10; sec < end; sec++ { // teardown: pages land in the cache
		r.step(sec+1, 36)
	}
	if n := r.count("evict:"); n != 0 {
		t.Fatalf("evicted %d file(s) while the killed groups were still alive", n)
	}
	r.alive = map[int]bool{}
	evicted := false
	r.onEvict = func(string) { evicted = true; r.s.r.FileBacked = 3 * gib }
	sec++
	r.step(sec, 36)
	if !evicted {
		t.Fatal("no eviction once the killed groups exited")
	}
	for _, f := range []string{"/models/cq27.gguf", "/models/cq35.gguf", "/models/mmproj-cq35.gguf"} {
		if r.count("evict:"+f) != 1 {
			t.Errorf("model file %s not evicted exactly once: %q", f, r.rec.seq)
		}
	}
	if r.count("log:MEMORY BRAKE EVICT: dropped 3 model file(s) from the file cache: file-backed 36.00 -> 3.00 GB") != 1 {
		t.Fatalf("eviction must log file-backed before and after, got %q", r.rec.seq)
	}
	// Below the drain level, but the gate stays shut until drainSettle.
	for end := sec + int(drainSettle/time.Second) - 1; sec < end; sec++ {
		r.step(sec+1, 3)
		if !r.hold.Holding() {
			t.Fatalf("gate opened %ds after file-backed drained; settle is %v", sec+1-(end-int(drainSettle/time.Second)+1), drainSettle)
		}
	}
	for i := 0; i < 3 && r.hold.Holding(); i++ {
		sec++
		r.step(sec, 3)
	}
	if r.hold.Holding() {
		t.Fatal("gate still shut after file-backed stayed below the drain level for the settle time")
	}
	data, _ := os.ReadFile(cfg.MarkerPath)
	for _, want := range []string{"MEMORY-BRAKE-EVICT files=3 filebacked_before_gb=36.00 filebacked_after_gb=3.00 drain_below_gb=10.00",
		"MEMORY-BRAKE-RELEASE filebacked_gb=3.00 drain_below_gb=10.00"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("marker missing %q:\n%s", want, data)
		}
	}
}

// drainBelowGB: 0 turns the gate off: the kill and the eviction still happen,
// loads are never parked.
func TestBrake_DrainBelowZeroNeverHolds(t *testing.T) {
	cfg := testCfg(t)
	cfg.DrainBelowGB = 0
	r := newRig(t, cfg)
	sec := r.quiet(t, 4)
	r.step(sec, 10)
	if !r.step(sec+1, 10.2) {
		t.Fatal("setup: the burst did not trip")
	}
	if r.hold.Holding() {
		t.Fatal("drainBelowGB 0 must not shut the gate")
	}
	r.k.kids = nil
	r.step(sec+2, 36)
	if r.count("evict:") == 0 {
		t.Fatal("the eviction must still run with the gate off")
	}
}

func TestKillGroupRefusesDangerousPgids(t *testing.T) {
	for _, p := range []int{-1, 0, 1} {
		if killGroup(p) == nil {
			t.Fatalf("killGroup(%d) must refuse", p)
		}
	}
}
