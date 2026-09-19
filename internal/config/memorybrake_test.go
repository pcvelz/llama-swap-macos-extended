package config

import (
	"strings"
	"testing"
)

// The memory brake is a kernel-panic guard: a config that never mentions it
// must still get it, with every calibrated default.
func TestMemoryBrakeDefaultsOnWhenBlockAbsent(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader("models: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MemoryBrake != DefaultMemoryBrakeConfig() {
		t.Fatalf("absent block: got %+v, want defaults %+v", cfg.MemoryBrake, DefaultMemoryBrakeConfig())
	}
	if !cfg.MemoryBrake.Enabled {
		t.Fatal("memory brake must default to enabled")
	}
}

func TestMemoryBrakePartialBlockKeepsOtherDefaults(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader("memoryBrake:\n  drainBelowGB: 8\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := DefaultMemoryBrakeConfig()
	want.DrainBelowGB = 8
	if cfg.MemoryBrake != want {
		t.Fatalf("got %+v, want %+v", cfg.MemoryBrake, want)
	}
}

// A yaml written for the fixed hold (holdMinutes) still loads; the value is
// recorded as ignored (the brake warns about it at startup) and the drain gate
// keeps its default.
func TestMemoryBrakeLegacyHoldMinutesAcceptedAndIgnored(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader("memoryBrake:\n  holdMinutes: 5\n"))
	if err != nil {
		t.Fatalf("legacy holdMinutes must not fail the load: %v", err)
	}
	if cfg.MemoryBrake.LegacyHoldMinutes != 5 || cfg.MemoryBrake.DrainBelowGB != 10 {
		t.Fatalf("legacy holdMinutes not recorded, or it changed the drain gate: %+v", cfg.MemoryBrake)
	}
}

func TestMemoryBrakeCanBeDisabled(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader("memoryBrake:\n  enabled: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MemoryBrake.Enabled {
		t.Fatal("enabled: false must disable the brake")
	}
}

// The 2026-09-18 20:35 ruling: file-backed growth >= 3.5 GB within a rolling
// 5-minute window. Armed at ready since 2026-09-19 (Event 8: the 14:54
// collapse came five minutes after ready, inside the old 10-minute warm-up).
func TestMemoryBrakeFileBackedDefaults(t *testing.T) {
	d := DefaultMemoryBrakeConfig()
	if d.GrowthGB != 3.5 || d.WindowMinutes != 5 || d.ArmAfterMinutes != 0 ||
		d.ConfirmSamples != 2 || d.DrainBelowGB != 10 || d.SampleIntervalMs != 1000 || !d.Enabled {
		t.Fatalf("defaults drifted from the ruling: %+v", d)
	}
}

// A yaml written for the superseded 30 s rule still loads; windowSeconds is
// recorded as ignored (the brake warns about it at startup) and changes nothing.
func TestMemoryBrakeLegacyWindowSecondsAcceptedAndIgnored(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader("memoryBrake:\n  windowSeconds: 30\n"))
	if err != nil {
		t.Fatalf("legacy windowSeconds must not fail the load: %v", err)
	}
	if cfg.MemoryBrake.LegacyWindowSeconds != 30 {
		t.Fatalf("legacy key not recorded for the startup warning: %+v", cfg.MemoryBrake)
	}
	if cfg.MemoryBrake.WindowMinutes != 5 {
		t.Fatalf("legacy windowSeconds must not change the window: %+v", cfg.MemoryBrake)
	}
}

func TestMemoryBrakeRejectsNonsense(t *testing.T) {
	for _, y := range []string{
		"memoryBrake:\n  growthGB: 0\n",
		"memoryBrake:\n  windowMinutes: 0\n",
		"memoryBrake:\n  armAfterMinutes: -1\n",
		"memoryBrake:\n  confirmSamples: 0\n",
		"memoryBrake:\n  drainBelowGB: -1\n",
		"memoryBrake:\n  sampleIntervalMs: 0\n",
	} {
		if _, err := LoadConfigFromReader(strings.NewReader(y)); err == nil {
			t.Errorf("expected error for %q", y)
		}
	}
}
