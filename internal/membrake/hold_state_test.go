package membrake

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A restart used to clear the in-memory hold and send the next load straight
// into a file cache the brake had just judged unsafe (witnessed: restart
// 18:39:54, load, brake kill 3s after ready with file-backed +17 GB in 2s).
// holdStatePath survives that: a trip writes it, a new Brake over an
// existing file starts already holding, and release deletes it.

func TestBrake_TripWritesHoldStateFile(t *testing.T) {
	cfg := testCfg(t)
	r := newRig(t, cfg)
	sec := r.quiet(t, 4)
	r.step(sec, 10)
	sec++
	if !r.step(sec, 10.2) {
		t.Fatal("setup: the burst did not trip")
	}
	data, err := os.ReadFile(cfg.HoldStatePath)
	if err != nil {
		t.Fatalf("hold state file not written: %v", err)
	}
	killedAt := strings.TrimSpace(string(data))
	if killedAt == "" {
		t.Fatalf("hold state file empty: %q", data)
	}
	wantAt := r.t0.Add(time.Duration(sec) * time.Second).Unix()
	if killedAt != strconv.FormatInt(wantAt, 10) {
		t.Fatalf("hold state file = %q, want killed-at %d", data, wantAt)
	}
}

func TestBrake_RestartOverHoldStateFileStartsHoldingAndDrains(t *testing.T) {
	cfg := testCfg(t)
	if err := os.MkdirAll(filepath.Dir(cfg.HoldStatePath), 0o755); err != nil {
		t.Fatal(err)
	}
	killedAt := time.Date(2026, 9, 27, 18, 39, 54, 0, time.Local).Unix()
	if err := os.WriteFile(cfg.HoldStatePath, []byte(strconv.FormatInt(killedAt, 10)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := newRig(t, cfg)
	if !r.hold.Holding() {
		t.Fatal("a brake constructed over an existing hold-state file must start Holding")
	}

	// Stays shut while file-backed is at/above the drain level, re-evicting.
	base := time.Date(2026, 9, 27, 18, 40, 0, 0, time.Local)
	r.s.r = Reading{FileBacked: uint64(36 * gib)}
	for i := 0; i < int(reEvictEvery/time.Second)+5; i++ {
		r.b.Step(base.Add(time.Duration(i) * time.Second))
		if !r.hold.Holding() {
			t.Fatalf("gate opened at %ds while file-backed stayed at the drain level", i)
		}
	}
	if r.count("evict:") == 0 {
		t.Fatal("a restored hold must keep re-evicting known model files")
	}

	// Drains: released after drainBelowGB is held for drainSettle.
	r.s.r = Reading{FileBacked: uint64(8 * gib)}
	sec := int(reEvictEvery/time.Second) + 5
	for end := sec + int(drainSettle/time.Second) + 2; sec < end; sec++ {
		r.b.Step(base.Add(time.Duration(sec) * time.Second))
	}
	if r.hold.Holding() {
		t.Fatal("gate still shut after file-backed drained and stayed below the level for drainSettle")
	}
	if _, err := os.Stat(cfg.HoldStatePath); !os.IsNotExist(err) {
		t.Fatalf("hold state file not removed on release: err=%v", err)
	}
}

func TestBrake_NoHoldStateFileNotHolding(t *testing.T) {
	r := newRig(t, testCfg(t))
	if r.hold.Holding() {
		t.Fatal("no hold-state file: must not start holding")
	}
}

func TestBrake_DrainBelowZeroIgnoresAndDeletesHoldStateFile(t *testing.T) {
	cfg := testCfg(t)
	if err := os.MkdirAll(filepath.Dir(cfg.HoldStatePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.HoldStatePath, []byte("123\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.DrainBelowGB = 0
	r := newRig(t, cfg)
	if r.hold.Holding() {
		t.Fatal("drainBelowGB: 0 must ignore an existing hold-state file")
	}
	if _, err := os.Stat(cfg.HoldStatePath); !os.IsNotExist(err) {
		t.Fatal("drainBelowGB: 0 must delete a stale hold-state file")
	}
}
