package membrake

import (
	"testing"

	"github.com/mostlygeek/llama-swap/internal/process"
)

// A Splash child keeps its weights wired (MLX), so its file-backed growth is
// its own SSD prefix-cache restores: one restore of a full 256K context is
// ~8 GiB at int8 KV. With splashGrowthGB set, a box whose EVERY live child is
// Splash uses that threshold; any llama-server child keeps growthGB, which is
// what the 09-18 replays are calibrated on.

// splashRig is newRig with one cq27 child that the engine probe reports as
// Splash (pgid 4242) and, optionally, a llama-server cq35 child (4343).
func splashRig(t *testing.T, splashGB float64, withLlamaServer bool) *rig {
	t.Helper()
	cfg := testCfg(t)
	cfg.SplashGrowthGB = splashGB
	r := newRig(t, cfg)
	r.k.kids = []process.LiveChild{{ID: "cq27", Pgid: 4242, Files: []string{"/models/cq27.gguf"}}}
	if withLlamaServer {
		r.k.kids = append(r.k.kids, process.LiveChild{ID: "cq35", Pgid: 4343, Files: []string{"/models/cq35.gguf"}})
	}
	r.b.isSplash = func(pgid int) bool { return pgid == 4242 }
	return r
}

// ramp steps file-backed linearly from `from` by `by` GB over n seconds and
// reports whether the brake tripped.
func (r *rig) ramp(sec int, from, by float64, n int) bool {
	for i := 1; i <= n; i++ {
		if r.step(sec+i, from+by*float64(i)/float64(n)) {
			return true
		}
	}
	return false
}

// The 2026-10-24 06:01 shape: +3.72 GB within the window at 5.5 -> 9.25 GB,
// one 133K-token cache restore. Under a Splash-only box it must not kill.
func TestBrake_SplashCacheRestoreBelowSplashThresholdDoesNotTrip(t *testing.T) {
	r := splashRig(t, 9, false)
	sec := r.quiet(t, 5.5)
	if r.ramp(sec, 5.5, 3.75, 120) {
		t.Fatal("tripped on a 3.75 GB Splash cache restore with splashGrowthGB=9")
	}
}

// A Time Machine-sized fill (+12 GB in a minute) still trips a Splash-only box.
func TestBrake_SplashThresholdStillCatchesALargeFill(t *testing.T) {
	r := splashRig(t, 9, false)
	sec := r.quiet(t, 5.5)
	if !r.ramp(sec, 5.5, 12, 60) {
		t.Fatal("a 12 GB fill within a minute must trip even under splashGrowthGB=9")
	}
	if r.kills() != 1 {
		t.Fatalf("kills = %d, want 1 (the Splash child)", r.kills())
	}
}

// Any llama-server child on the box keeps the base growthGB (3.5).
func TestBrake_MixedChildrenKeepTheBaseThreshold(t *testing.T) {
	r := splashRig(t, 9, true)
	sec := r.quiet(t, 5.5)
	if !r.ramp(sec, 5.5, 3.75, 120) {
		t.Fatal("with a llama-server child live, 3.75 GB growth must trip at growthGB=3.5")
	}
}

// A child the probe does not report as Splash keeps the base threshold.
func TestBrake_LlamaServerOnlyKeepsTheBaseThreshold(t *testing.T) {
	r := splashRig(t, 9, false)
	r.b.isSplash = func(int) bool { return false }
	sec := r.quiet(t, 5.5)
	if !r.ramp(sec, 5.5, 3.75, 120) {
		t.Fatal("a llama-server-only box must trip at growthGB=3.5")
	}
}

// A child probed before its engine process exists (Splash's shim answers
// /health before `splash serve-native` is up) must not be pinned to the base
// threshold for its whole life: a "not Splash" answer is re-probed later.
func TestBrake_NotSplashAnswerIsReprobed(t *testing.T) {
	r := splashRig(t, 9, false)
	engineUp := false
	r.b.isSplash = func(int) bool { return engineUp }
	sec := r.quiet(t, 5.5) // first probes: engine not up yet
	engineUp = true
	for i := 0; i <= splashReprobeSecs; i++ { // flat while the re-probe interval passes
		r.step(sec+i, 5.5)
	}
	sec += splashReprobeSecs + 1
	if r.ramp(sec, 5.5, 3.75, 120) {
		t.Fatal("a child first probed before its engine started kept the 3.5 GB threshold")
	}
}

// splashGrowthGB 0 (the default) changes nothing, and never runs the probe.
func TestBrake_SplashThresholdOffByDefault(t *testing.T) {
	r := splashRig(t, 0, false)
	probed := false
	r.b.isSplash = func(int) bool { probed = true; return true }
	sec := r.quiet(t, 5.5)
	if !r.ramp(sec, 5.5, 3.75, 120) {
		t.Fatal("splashGrowthGB=0 must keep growthGB=3.5")
	}
	if probed {
		t.Fatal("the engine probe ran although splashGrowthGB is 0")
	}
}
