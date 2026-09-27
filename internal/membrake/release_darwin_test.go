//go:build darwin

package membrake

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/process"
)

// cachedModel writes a 32 MB stand-in GGUF and reads it back so its pages
// are in the file cache, as a killed server's weights are.
func cachedModel(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cq27.gguf")
	buf := make([]byte, 32<<20)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(path); err != nil {
		t.Fatal(err)
	}
	if res, total := residentPages(t, path); res < total/2 {
		t.Skipf("setup: only %d/%d pages resident after a read", res, total)
	}
	return path
}

// realRig is newRig around a real model file on disk.
func realRig(t *testing.T) *rig {
	return newRig(t, testCfg(t))
}

// tripThenExit drives the rig through a trip with one child on path and lets
// the killed group exit, file-backed sitting at fbGB throughout.
func tripThenExit(t *testing.T, r *rig, path string, fbGB float64) int {
	t.Helper()
	r.k.kids = []process.LiveChild{{ID: "cq27", Pgid: 4242, Files: []string{path}}}
	sec := r.quiet(t, 4)
	r.step(sec, 10)
	sec++
	if !r.step(sec, 10.2) {
		t.Fatal("setup: the burst did not trip")
	}
	r.k.kids = nil
	r.alive = map[int]bool{}
	sec++
	r.step(sec, fbGB) // killed group gone: the eviction runs on this sample
	return sec
}

// The invariant: the gate NEVER opens while file-backed is at or above the
// drain level, whatever else says the hazard is gone. On 2026-09-27 a release
// path that opened once the killed model's own file had left the cache let a
// reload through at 32 GB file-backed; the machine died in a watchdog panic
// four minutes later. Here the model file is really evicted and file-backed
// stays at 36 GB: the gate must stay shut, even with the old knob set.
func TestBrake_NeverReleasesAboveTheDrainLevel(t *testing.T) {
	path := cachedModel(t)
	r := realRig(t)
	r.b.evict = evictFile // the real eviction on a real file
	sec := tripThenExit(t, r, path, 36)
	if res, total := residentPages(t, path); res > total/50 {
		t.Fatalf("setup: eviction left %d/%d pages resident", res, total)
	}
	for end := sec + 5*int(drainSettle/time.Second); sec < end; sec++ {
		r.step(sec+1, 36)
		if !r.hold.Holding() {
			t.Fatalf("gate opened at file-backed 36 GB (drain level %.0f GB) %ds after the kill", r.b.cfg.DrainBelowGB, sec+1)
		}
	}
	data, _ := os.ReadFile(r.b.cfg.MarkerPath)
	if strings.Contains(string(data), "MEMORY-BRAKE-RELEASE") {
		t.Errorf("a release was marked above the drain level:\n%s", data)
	}
}

// The guard on the new release path: while a known model file is still in
// the cache (the eviction did nothing), the gate stays shut above the drain
// level exactly as before.
func TestBrake_StaysShutWhileAModelFileIsStillCached(t *testing.T) {
	path := cachedModel(t)
	r := realRig(t)
	r.b.evict = func(string) error { return nil } // eviction that frees nothing
	sec := tripThenExit(t, r, path, 36)
	for end := sec + 3*int(drainSettle/time.Second); sec < end; sec++ {
		r.step(sec+1, 36)
	}
	if !r.hold.Holding() {
		t.Fatal("gate opened while the killed model's weights were still in the file cache")
	}
}
