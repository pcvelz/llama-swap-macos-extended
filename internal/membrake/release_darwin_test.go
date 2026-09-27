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

// realRig is newRig measuring REAL file-cache residency, with the release
// threshold scaled to the 32 MB stand-in (16 MB) the way 0.5 GB is scaled to
// a 20-30 GB GGUF.
func realRig(t *testing.T) *rig {
	cfg := testCfg(t)
	cfg.ReleaseWhenModelsEvictedBelowGB = 16.0 / 1024
	r := newRig(t, cfg)
	r.b.resident = residentBytes
	return r
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

// 2026-09-27: after the 12:43 kill the cq27 weights left the cache, but the
// memory the server freed filled with UNRELATED file cache (29.9 -> 39.0 GB
// over 2h10m on an idle box; macOS does not drop clean cache without
// pressure). An absolute "file-backed < 10 GB" level measures the whole box,
// not the hazard - the killed model's own pages - so the gate never opened and
// every local load was held until a restart. Once the eviction has verifiably
// emptied the known model files from the cache, the hazard is gone and the
// gate must open after the settle time, whatever the unrelated cache does.
func TestBrake_ReleasesOnceModelFilesAreOutOfTheCache(t *testing.T) {
	path := cachedModel(t)
	r := realRig(t)
	r.b.evict = evictFile // the real eviction on a real file
	sec := tripThenExit(t, r, path, 36)
	if res, total := residentPages(t, path); res > total/50 {
		t.Fatalf("setup: eviction left %d/%d pages resident", res, total)
	}
	for end := sec + int(drainSettle/time.Second) + 3; sec < end && r.hold.Holding(); sec++ {
		r.step(sec+1, 36)
	}
	if r.hold.Holding() {
		t.Fatalf("gate still shut %v after the kill although the model file is out of the cache; only unrelated cache (36 GB) keeps file-backed above the drain level", drainSettle+3*time.Second)
	}
	data, _ := os.ReadFile(r.b.cfg.MarkerPath)
	if !strings.Contains(string(data), "MEMORY-BRAKE-RELEASE") {
		t.Errorf("release not marked:\n%s", data)
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
