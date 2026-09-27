package membrake

import (
	"sync"
	"testing"
	"time"
)

// The purge valve is off unless configured (purgeAfterMinutes: 0, the
// default): a trip and a stuck drain must never run PurgeCommand.
func TestBrake_PurgeOffByDefault(t *testing.T) {
	cfg := testCfg(t)
	r := newRig(t, cfg)
	purged := 0
	r.b.purge = func() error { purged++; return nil }
	sec := r.quiet(t, 4)
	r.step(sec, 10)
	sec++
	if !r.step(sec, 10.2) {
		t.Fatal("setup: the burst did not trip")
	}
	r.k.kids = nil
	for end := sec + 600; sec < end; sec++ {
		r.step(sec+1, 36) // stuck above the drain level throughout
	}
	r.b.purgeWG.Wait()
	if purged != 0 {
		t.Fatalf("purge ran %d times with purgeAfterMinutes: 0", purged)
	}
}

// purgeAfterMinutes: 5 - no attempt before 5 min after the kill, exactly one
// at 5 min, the next only after another 5 min.
func TestBrake_PurgeRetriedOnItsOwnInterval(t *testing.T) {
	cfg := testCfg(t)
	cfg.PurgeAfterMinutes = 5
	r := newRig(t, cfg)
	var mu sync.Mutex
	purged := 0
	r.b.purge = func() error { mu.Lock(); purged++; mu.Unlock(); return nil }
	count := func() int { mu.Lock(); defer mu.Unlock(); return purged }

	sec := r.quiet(t, 4)
	r.step(sec, 10)
	sec++
	if !r.step(sec, 10.2) {
		t.Fatal("setup: the burst did not trip")
	}
	r.k.kids = nil
	killSec := sec

	fiveMin := int(5 * time.Minute / time.Second)
	for end := killSec + fiveMin - 1; sec < end; sec++ {
		r.step(sec+1, 36)
	}
	r.b.purgeWG.Wait()
	if n := count(); n != 0 {
		t.Fatalf("purge ran %d time(s) before 5 min elapsed", n)
	}

	// Cross the 5-minute mark: exactly one attempt.
	sec++
	r.step(sec, 36)
	r.b.purgeWG.Wait()
	if n := count(); n != 1 {
		t.Fatalf("purge ran %d time(s) at the 5 min mark, want 1", n)
	}

	// Stays at one for the next 4:59.
	for end := sec + fiveMin - 1; sec < end; sec++ {
		r.step(sec+1, 36)
	}
	r.b.purgeWG.Wait()
	if n := count(); n != 1 {
		t.Fatalf("purge ran %d time(s) before the second interval elapsed, want 1", n)
	}

	// The next attempt fires only after another 5 minutes.
	sec++
	r.step(sec, 36)
	r.b.purgeWG.Wait()
	if n := count(); n != 2 {
		t.Fatalf("purge ran %d time(s) at the second interval, want 2", n)
	}
}

// A purge that actually frees memory lets the ordinary drain rule release the
// gate once file-backed has stayed below the level for drainSettle. Purging
// never opens the gate itself.
func TestBrake_SuccessfulPurgeLetsDrainRuleRelease(t *testing.T) {
	cfg := testCfg(t)
	cfg.PurgeAfterMinutes = 5
	r := newRig(t, cfg)
	sec := r.quiet(t, 4)
	r.step(sec, 10)
	sec++
	if !r.step(sec, 10.2) {
		t.Fatal("setup: the burst did not trip")
	}
	r.k.kids = nil
	r.b.purge = func() error {
		r.s.r.FileBacked = uint64(3 * gib)
		return nil
	}

	// Up to (not including) the 5-minute mark: still holding, purge not yet
	// eligible. The last step of this run crosses the mark and starts the
	// purge goroutine; Wait() before any further step touches the fake
	// sampler's shared state, which the purge func also writes.
	fiveMin := int(5 * time.Minute / time.Second)
	for i := 0; i < fiveMin-1; i++ {
		sec++
		r.step(sec, 36)
		if !r.hold.Holding() {
			t.Fatalf("gate opened at %ds before the purge ran", i)
		}
	}
	sec++
	r.step(sec, 36) // crosses the 5-minute mark: starts the purge
	r.b.purgeWG.Wait()
	if r.s.r.FileBacked != uint64(3*gib) {
		t.Fatal("setup: purge did not run")
	}

	for end := sec + int(drainSettle/time.Second) + 2; sec < end; sec++ {
		r.step(sec+1, 3)
	}
	if r.hold.Holding() {
		t.Fatal("gate still shut after a successful purge and drainSettle below the level")
	}
	if r.count("log:MEMORY BRAKE PURGE:") == 0 {
		t.Fatal("a successful purge must be logged")
	}
}

// A failing purge logs PURGE FAILED and leaves the gate shut; it is retried
// only after another purgeAfterMinutes.
func TestBrake_FailedPurgeLogsAndGateStaysShut(t *testing.T) {
	cfg := testCfg(t)
	cfg.PurgeAfterMinutes = 5
	r := newRig(t, cfg)
	sec := r.quiet(t, 4)
	r.step(sec, 10)
	sec++
	if !r.step(sec, 10.2) {
		t.Fatal("setup: the burst did not trip")
	}
	r.k.kids = nil
	r.b.purge = func() error { return errPurgeRefused }

	fiveMin := int(5 * time.Minute / time.Second)
	for end := sec + fiveMin; sec < end; sec++ {
		r.step(sec+1, 36)
	}
	r.b.purgeWG.Wait()
	if !r.hold.Holding() {
		t.Fatal("a failed purge must not open the gate")
	}
	if r.count("log:MEMORY BRAKE PURGE FAILED") == 0 {
		t.Fatalf("a failed purge must be logged, got %q", r.rec.seq)
	}
}

// No purge is attempted while file-backed is already below the drain level:
// the ordinary drain rule handles it.
func TestBrake_NoPurgeWhileAlreadyBelowTheLevel(t *testing.T) {
	cfg := testCfg(t)
	cfg.PurgeAfterMinutes = 5
	r := newRig(t, cfg)
	sec := r.quiet(t, 4)
	r.step(sec, 10)
	sec++
	if !r.step(sec, 10.2) {
		t.Fatal("setup: the burst did not trip")
	}
	r.k.kids = nil
	purged := false
	r.b.purge = func() error { purged = true; return nil }

	fiveMin := int(5 * time.Minute / time.Second)
	for end := sec + fiveMin + int(drainSettle/time.Second) + 5; sec < end; sec++ {
		r.step(sec+1, 3) // already below the drain level throughout
	}
	r.b.purgeWG.Wait()
	if purged {
		t.Fatal("purge ran while file-backed was already below the drain level")
	}
	if r.hold.Holding() {
		t.Fatal("setup: gate did not release via the ordinary drain rule")
	}
}

var errPurgeRefused = &purgeError{"sudo: a password is required"}

type purgeError struct{ msg string }

func (e *purgeError) Error() string { return e.msg }
