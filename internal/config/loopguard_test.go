package config

import (
	"reflect"
	"strings"
	"testing"
)

// The loop guard protects every other session on the box from one degenerate
// loop (2026-09-19), so a config that never mentions it must still get it.
func TestLoopGuardDefaultsOnWhenBlockAbsent(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader("models: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.LoopGuard, DefaultLoopGuardConfig()) {
		t.Fatalf("absent block: got %+v, want defaults %+v", cfg.LoopGuard, DefaultLoopGuardConfig())
	}
	if !cfg.LoopGuard.Enabled || cfg.LoopGuard.RunBar != 20 || cfg.LoopGuard.ToleranceTokens != 2 {
		t.Fatalf("loop guard defaults changed: %+v", cfg.LoopGuard)
	}
	// The strike ladder: flag, 15 minutes, then held until a human looks.
	if cfg.LoopGuard.Strikes != 3 || cfg.LoopGuard.ClearRequests != 10 {
		t.Fatalf("strike defaults changed: %+v", cfg.LoopGuard)
	}
	// NO INFINITE DEFAULT (2026-09-20): -1 stays legal config but is never
	// shipped - the guard held a healthy session for an hour against an empty
	// box and only a human could end it.
	if !reflect.DeepEqual(cfg.LoopGuard.PenaltySeconds, []int{0, 900, 3600}) {
		t.Fatalf("penaltySeconds=%v want [0 900 3600]", cfg.LoopGuard.PenaltySeconds)
	}
	for i, p := range cfg.LoopGuard.PenaltySeconds {
		if p < 0 {
			t.Errorf("penaltySeconds[%d]=%d: no default may require a human to clear it", i, p)
		}
	}
	// The phase-3 guards against that same incident.
	if cfg.LoopGuard.MaxLoopTokens != 256 {
		t.Fatalf("maxLoopTokens=%d want 256", cfg.LoopGuard.MaxLoopTokens)
	}
	if !cfg.LoopGuard.HoldOnlyWhenContended {
		t.Fatalf("holdOnlyWhenContended must default to true: a hold exists to protect somebody else")
	}
}

// -1 is still accepted, just not shipped.
func TestLoopGuardForeverPenaltyStillLoads(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader("loopGuard:\n  penaltySeconds: [0, 900, -1]\n"))
	if err != nil {
		t.Fatalf("-1 must remain legal configuration: %v", err)
	}
	if cfg.LoopGuard.PenaltySeconds[2] != -1 {
		t.Fatalf("penaltySeconds=%v", cfg.LoopGuard.PenaltySeconds)
	}
}

func TestLoopGuardPhase3Keys(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader("loopGuard:\n  maxLoopTokens: 64\n  holdOnlyWhenContended: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LoopGuard.MaxLoopTokens != 64 || cfg.LoopGuard.HoldOnlyWhenContended {
		t.Fatalf("got %+v", cfg.LoopGuard)
	}

	// A ceiling of 0 or less would make every response productive, which
	// `enabled: false` already expresses honestly.
	for _, yaml := range []string{
		"loopGuard:\n  maxLoopTokens: 0\n",
		"loopGuard:\n  maxLoopTokens: -1\n",
	} {
		if _, err := LoadConfigFromReader(strings.NewReader(yaml)); err == nil {
			t.Errorf("%q must not load", yaml)
		}
	}
}

// The ladder is read off the yaml with NO implicit padding: a list that does
// not match the strike count is an operator who thinks a different number of
// strikes exists, and that must fail loudly rather than be guessed at.
func TestLoopGuardPenaltySecondsMustMatchStrikes(t *testing.T) {
	for _, yaml := range []string{
		"loopGuard:\n  strikes: 3\n  penaltySeconds: [0, 900]\n",
		"loopGuard:\n  strikes: 2\n  penaltySeconds: [0, 900, -1, -1]\n",
		"loopGuard:\n  strikes: 4\n", // default 3-entry ladder, 4 strikes
		"loopGuard:\n  strikes: 2\n", // default 3-entry ladder, 2 strikes
	} {
		if _, err := LoadConfigFromReader(strings.NewReader(yaml)); err == nil {
			t.Errorf("%q must not load", yaml)
		}
	}

	cfg, err := LoadConfigFromReader(strings.NewReader("loopGuard:\n  strikes: 2\n  penaltySeconds: [0, -1]\n"))
	if err != nil {
		t.Fatalf("a matching ladder must load: %v", err)
	}
	if cfg.LoopGuard.Strikes != 2 || !reflect.DeepEqual(cfg.LoopGuard.PenaltySeconds, []int{0, -1}) {
		t.Fatalf("got %+v", cfg.LoopGuard)
	}
}

func TestLoopGuardRejectsNonsenseStrikeSettings(t *testing.T) {
	for _, yaml := range []string{
		"loopGuard:\n  strikes: 0\n  penaltySeconds: []\n",
		"loopGuard:\n  strikes: -1\n  penaltySeconds: []\n",
		"loopGuard:\n  clearRequests: 0\n",
		"loopGuard:\n  clearRequests: -3\n",
		// -1 is the "held until un-penalized" sentinel; below it is a typo.
		"loopGuard:\n  penaltySeconds: [0, 900, -2]\n",
	} {
		if _, err := LoadConfigFromReader(strings.NewReader(yaml)); err == nil {
			t.Errorf("%q must not load", yaml)
		}
	}
}

func TestLoopGuardPartialBlockKeepsOtherDefaults(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader("loopGuard:\n  runBar: 40\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := DefaultLoopGuardConfig()
	want.RunBar = 40
	if !reflect.DeepEqual(cfg.LoopGuard, want) {
		t.Fatalf("got %+v, want %+v", cfg.LoopGuard, want)
	}
}

func TestLoopGuardCanBeDisabled(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader("loopGuard:\n  enabled: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LoopGuard.Enabled {
		t.Fatal("enabled: false must disable the loop guard")
	}
}

// Both thresholds are unproven, so an operator WILL tune them; a typo that
// would make every response a loop must refuse to boot rather than quietly
// become policy.
func TestLoopGuardRejectsNonsenseThresholds(t *testing.T) {
	for _, yaml := range []string{
		"loopGuard:\n  runBar: 1\n",
		"loopGuard:\n  runBar: 0\n",
		"loopGuard:\n  runBar: -5\n",
		"loopGuard:\n  toleranceTokens: -1\n",
	} {
		if _, err := LoadConfigFromReader(strings.NewReader(yaml)); err == nil {
			t.Errorf("%q must not load", yaml)
		}
	}
}

// A zero tolerance is legitimate: "byte-identical responses only".
func TestLoopGuardZeroToleranceIsValid(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader("loopGuard:\n  toleranceTokens: 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LoopGuard.ToleranceTokens != 0 {
		t.Fatalf("toleranceTokens=%d want 0", cfg.LoopGuard.ToleranceTokens)
	}
}
