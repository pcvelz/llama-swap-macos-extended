package membrake

import (
	"bufio"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/process"
)

// Incident 2026-10-20 (48 GB Mac): after the brake killed cq27 an EXTERNAL
// writer kept filling the page cache with no model loaded (file-backed 7.8 ->
// 33.1 GB). A purge dropped it to 7.6 GB, file-backed stayed under
// drainBelowGB for drainSettle, the gate released into a run-up that was still
// active, and the reloaded model was killed again 96 s later. A purge that
// never exits was then skipped silently by the pgrep single-flight.

var sysFileBacked = regexp.MustCompile(`filebacked_gb=([0-9.]+)`)

// loadSysGB reads a testdata observer log of ` sys ` ticks and returns the
// tick times and file-backed GB.
func loadSysGB(t *testing.T, path string) ([]time.Time, []float64) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var ts []time.Time
	var gb []float64
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") || !strings.Contains(line, " sys ") {
			continue
		}
		at, err := time.ParseInLocation("2006-01-02 15:04:05", line[:19], time.Local)
		if err != nil {
			t.Fatalf("bad tick time in %q: %v", line, err)
		}
		m := sysFileBacked.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("no filebacked_gb in %q", line)
		}
		v, _ := strconv.ParseFloat(m[1], 64)
		ts = append(ts, at)
		gb = append(gb, v)
	}
	if len(ts) < 2 {
		t.Fatalf("%s: need at least two ticks, got %d", path, len(ts))
	}
	return ts, gb
}

func readMarker(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// The recorded 14:10-14:22 data, 1 s steps, linear between the 30 s ticks. The
// purge effect is already inside the data, so the fake purge is a no-op. The
// real release came at ~14:19:19 and the gate must still be shut at the end of
// the data, which sits inside the same run-up (the external writer was still
// active: file-backed 13.4 -> 16.6 GB at 14:21).
func TestReleaseRefusedWhileRunUpInTrailingWindow_Replay20261020(t *testing.T) {
	ticks, gbs := loadSysGB(t, "testdata/observer-sys-2026-10-20.log")
	cfg := testCfg(t)
	cfg.PurgeAfterMinutes = 5
	r := newRig(t, cfg)
	r.t0 = ticks[0]
	r.b.purge = func() error { return nil }

	total := int(ticks[len(ticks)-1].Sub(ticks[0]) / time.Second)
	var tripAt, releaseAt = -1, -1
	seg := 0
	for sec := 0; sec <= total; sec++ {
		at := ticks[0].Add(time.Duration(sec) * time.Second)
		for seg < len(ticks)-2 && !at.Before(ticks[seg+1]) {
			seg++
		}
		span := ticks[seg+1].Sub(ticks[seg]).Seconds()
		f := at.Sub(ticks[seg]).Seconds() / span
		gb := gbs[seg] + (gbs[seg+1]-gbs[seg])*f
		tripped := r.step(sec, gb)
		r.b.purgeWG.Wait()
		if tripped && tripAt < 0 {
			tripAt = sec
			r.k.kids = nil // the kill took the model; the reload is held by the gate
		}
		if tripAt >= 0 && releaseAt < 0 && !r.hold.Holding() {
			releaseAt = sec
		}
	}
	if tripAt < 0 {
		t.Fatal("setup: the 14:11 run-up did not trip the brake")
	}
	tripTime := ticks[0].Add(time.Duration(tripAt) * time.Second)
	if tripTime.Before(ticks[0].Add(50*time.Second)) || tripTime.After(ticks[0].Add(100*time.Second)) {
		t.Fatalf("setup: tripped at %s, expected ~14:11:15", tripTime.Format("15:04:05"))
	}
	if releaseAt >= 0 {
		t.Fatalf("gate released at %s (%.0fs after the kill) while the run-up was still active inside the trailing window; the recorded reload was killed again 96 s later",
			ticks[0].Add(time.Duration(releaseAt)*time.Second).Format("15:04:05"), float64(releaseAt-tripAt))
	}
}

// A purge that drops file-backed below the drain level must not open the gate
// while a trip-sized rise is still inside the trailing windowMinutes.
func TestPurgeDropDoesNotOpenGateDuringRunUp(t *testing.T) {
	cfg := testCfg(t)
	cfg.PurgeAfterMinutes = 5
	r := newRig(t, cfg)
	r.b.purge = func() error { return nil }
	sec := r.quiet(t, 4)
	r.step(sec, 10)
	sec++
	if !r.step(sec, 10.2) {
		t.Fatal("setup: the burst did not trip")
	}
	T := sec
	r.k.kids = nil
	at := func(off int, gb float64) {
		r.step(T+off, gb)
		r.b.purgeWG.Wait()
	}

	at(1, 5) // eviction sample
	for off := 2; off <= 171; off++ {
		at(off, 5+15*float64(off-1)/170) // 5 -> 20 GB over ~3 min, external writer
	}
	for off := 172; off <= 300; off++ { // purge becomes due at off 300
		at(off, 20)
	}
	if !r.hold.Holding() {
		t.Fatal("setup: gate opened during the run-up")
	}
	for off := 301; off <= 390; off++ { // purge dropped it to 5 GB; flat for 90 s
		at(off, 5)
		if !r.hold.Holding() {
			t.Fatalf("gate released %ds after the purge drop although a %.1f GB rise ended only %ds ago (growthGB %.1f, windowMinutes %d)",
				off-300, 15.0, off-300, cfg.GrowthGB, cfg.WindowMinutes)
		}
	}
	// 20 GB was last sampled at off 300; keep flat until a full window has passed.
	for off := 391; off <= 590; off++ {
		at(off, 5)
		if !r.hold.Holding() {
			t.Fatalf("gate released at off %d, before windowMinutes (%d) elapsed since the last trip-sized rise (off 300)", off, cfg.WindowMinutes)
		}
	}
	released := false
	for off := 591; off <= 720; off++ {
		at(off, 5)
		if !r.hold.Holding() {
			released = true
			break
		}
	}
	if !released {
		t.Fatal("gate still shut 420 s after the last rise with file-backed flat at 5 GB: it must release once a full window is quiet")
	}
}

// After a release, a re-trip within 30 min doubles the quiet window required
// for that hold (5 -> 10 min, cap 60 min).
func TestRepeatedTripBacksOffQuietWindow(t *testing.T) {
	cfg := testCfg(t)
	r := newRig(t, cfg)
	orig := r.k.kids
	sec := r.quiet(t, 4)

	// cycle: burst-trip at sec, then a post-eviction run-up 5 -> 11 GB over a
	// minute that ends in a drop back to 5 GB. Returns the trip second.
	trip := func() int {
		r.step(sec, 10)
		sec++
		if !r.step(sec, 10.2) {
			t.Fatalf("setup: the burst at %d did not trip", sec)
		}
		T := sec
		r.k.kids = nil
		return T
	}
	runUp := func(T int) {
		r.step(T+1, 5)
		for off := 2; off <= 61; off++ {
			r.step(T+off, 5+6*float64(off-1)/60)
		}
	}

	// Hold 1: backoff not yet in play; it only has to release in a normal time.
	T1 := trip()
	runUp(T1)
	rel1 := -1
	for off := 62; off <= 61+300+int(drainSettle/time.Second)+60; off++ {
		r.step(T1+off, 5)
		if !r.hold.Holding() {
			rel1 = off
			break
		}
	}
	if rel1 < 0 {
		t.Fatal("setup: hold 1 did not release after a quiet 5 min window")
	}
	sec = T1 + rel1

	// Reload and run again, then re-trip well inside 30 min of the release.
	r.k.kids, r.k.loading = orig, true
	for i := 0; i < 30; i++ {
		sec++
		r.step(sec, 4)
	}
	r.k.loading = false
	for i := 0; i < 120; i++ {
		sec++
		if r.step(sec, 4) {
			t.Fatalf("setup: tripped while quiet at %d", sec)
		}
	}
	sec++
	T2 := trip()
	runUp(T2)

	fiveMinQuiet := 61 + 300 + int(drainSettle/time.Second) + 30
	for off := 62; off <= fiveMinQuiet; off++ {
		r.step(T2+off, 5)
		if !r.hold.Holding() {
			t.Fatalf("second hold released at off %d (<= 5 min quiet + settle): a re-trip within 30 min must double the quiet window to 10 min", off)
		}
	}
	released := false
	for off := fiveMinQuiet + 1; off <= 61+600+int(drainSettle/time.Second)+60; off++ {
		r.step(T2+off, 5)
		if !r.hold.Holding() {
			released = true
			break
		}
	}
	if !released {
		t.Fatal("second hold never released after the doubled 10 min quiet window")
	}
}

// A purge that never exits (uninterruptible sleep) must be reported, not
// silently skipped when the next attempt falls due. Seam-free: with the fake
// clock the first attempt's goroutine stays alive, exactly as the real hung
// purge process does.
func TestStuckPurgeIsReportedNotSilentlySkipped(t *testing.T) {
	cfg := testCfg(t)
	cfg.PurgeAfterMinutes = 5
	r := newRig(t, cfg)
	forever := make(chan struct{})
	t.Cleanup(func() { close(forever) })
	var mu sync.Mutex
	calls := 0
	r.b.purge = func() error {
		mu.Lock()
		calls++
		mu.Unlock()
		<-forever // cannot be killed, never returns
		return nil
	}
	n := func() int { mu.Lock(); defer mu.Unlock(); return calls }

	sec := r.quiet(t, 4)
	r.step(sec, 10)
	sec++
	if !r.step(sec, 10.2) {
		t.Fatal("setup: the burst did not trip")
	}
	T := sec
	r.k.kids = nil
	for off := 1; off <= 301; off++ {
		r.step(T+off, 36)
	}
	deadline := time.Now().Add(2 * time.Second)
	for n() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n() != 1 {
		t.Fatalf("setup: first purge attempt count = %d, want 1", n())
	}
	for off := 302; off <= 605; off++ { // second attempt falls due at off 600
		r.step(T+off, 36)
	}
	if n() != 1 {
		t.Fatalf("a second purge was exec'd while the first is still alive: %d calls", n())
	}
	marker := readMarker(t, cfg.MarkerPath)
	m := regexp.MustCompile(`MEMORY-BRAKE-PURGE-STUCK[^\n]*running_s=(\d+)`).FindStringSubmatch(marker)
	if m == nil {
		t.Fatalf("no MEMORY-BRAKE-PURGE-STUCK line with running_s=<seconds> in the marker file:\n%s", marker)
	}
	if age, _ := strconv.Atoi(m[1]); age < 295 || age > 320 {
		t.Fatalf("running_s=%d, want ~300 (first attempt at off 300, next due at off 600)", age)
	}
	if r.count("log:MEMORY BRAKE PURGE STUCK") == 0 {
		t.Fatalf("no 'MEMORY BRAKE PURGE STUCK' warning logged, got %q", r.rec.seq)
	}
}

// The marker file (which survives a crash, unlike the main log) records the
// purge start and its result.
func TestPurgeStartAndResultWrittenToMarker(t *testing.T) {
	cfg := testCfg(t)
	cfg.PurgeAfterMinutes = 5
	r := newRig(t, cfg)
	startVisibleBeforeRun := false
	r.b.purge = func() error {
		startVisibleBeforeRun = strings.Contains(readMarker(t, cfg.MarkerPath), "MEMORY-BRAKE-PURGE-START")
		r.s.r.FileBacked = uint64(3 * gib)
		return nil
	}
	sec := r.quiet(t, 4)
	r.step(sec, 10)
	sec++
	if !r.step(sec, 10.2) {
		t.Fatal("setup: the burst did not trip")
	}
	T := sec
	r.k.kids = []process.LiveChild(nil)
	for off := 1; off <= 300; off++ {
		r.step(T+off, 36)
	}
	r.b.purgeWG.Wait()
	marker := readMarker(t, cfg.MarkerPath)
	start := strings.Index(marker, "MEMORY-BRAKE-PURGE-START filebacked_gb=36.00")
	result := strings.Index(marker, "MEMORY-BRAKE-PURGE-RESULT filebacked_before_gb=36.00 filebacked_after_gb=3.00")
	if start < 0 || result < 0 {
		t.Fatalf("marker lacks PURGE-START (%d) and/or PURGE-RESULT (%d) lines:\n%s", start, result, marker)
	}
	if start > result {
		t.Fatal("PURGE-RESULT precedes PURGE-START")
	}
	if !startVisibleBeforeRun {
		t.Fatal("PURGE-START was not on disk before the purge ran")
	}
}
