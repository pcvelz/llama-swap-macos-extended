package config

import (
	"strings"
	"testing"
	"time"
)

// TestPeerStallConfig pins the config contract for the dead-peer slot reclaim:
// on by default, overridable, and disable-able two ways. The disable paths are
// the part that matters - this feature can kill a live request, so getting back
// to pre-feature behaviour must not require a rebuild.
func TestPeerStallConfig(t *testing.T) {
	const models = "\nmodels:\n  m:\n    cmd: path/to/cmd\n    proxy: http://localhost:8080\n"

	for _, tc := range []struct {
		name    string
		yaml    string
		want    PeerStallConfig
		timeout time.Duration
	}{
		{
			name:    "absent block gets the defaults",
			yaml:    models,
			want:    PeerStallConfig{Enabled: true, TimeoutSeconds: 120},
			timeout: 120 * time.Second,
		},
		{
			name:    "partial block keeps the unspecified default",
			yaml:    "peerStall:\n  timeoutSeconds: 300\n" + models,
			want:    PeerStallConfig{Enabled: true, TimeoutSeconds: 300},
			timeout: 300 * time.Second,
		},
		{
			name:    "enabled false disables",
			yaml:    "peerStall:\n  enabled: false\n" + models,
			want:    PeerStallConfig{Enabled: false, TimeoutSeconds: 120},
			timeout: 0,
		},
		{
			name:    "zero timeout disables",
			yaml:    "peerStall:\n  timeoutSeconds: 0\n" + models,
			want:    PeerStallConfig{Enabled: true, TimeoutSeconds: 0},
			timeout: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadConfigFromReader(strings.NewReader(tc.yaml))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if cfg.PeerStall != tc.want {
				t.Errorf("PeerStall = %+v, want %+v", cfg.PeerStall, tc.want)
			}
			if got := cfg.PeerStall.StallTimeout(); got != tc.timeout {
				t.Errorf("StallTimeout() = %v, want %v", got, tc.timeout)
			}
		})
	}
}

// TestSlotStallConfig pins the same contract for the no-forward-progress knob.
// It is a SEPARATE config entry from peerStall, not a shared one, because the
// two answer different questions (is the client reading vs is the request
// advancing) and an operator may want one armed without the other.
func TestSlotStallConfig(t *testing.T) {
	const models = "\nmodels:\n  m:\n    cmd: path/to/cmd\n    proxy: http://localhost:8080\n"

	for _, tc := range []struct {
		name    string
		yaml    string
		want    SlotStallConfig
		timeout time.Duration
	}{
		{
			name:    "absent block gets the defaults",
			yaml:    models,
			want:    SlotStallConfig{Enabled: true, TimeoutSeconds: 180},
			timeout: 180 * time.Second,
		},
		{
			name:    "partial block keeps the unspecified default",
			yaml:    "slotStall:\n  timeoutSeconds: 600\n" + models,
			want:    SlotStallConfig{Enabled: true, TimeoutSeconds: 600},
			timeout: 600 * time.Second,
		},
		{
			name:    "enabled false disables",
			yaml:    "slotStall:\n  enabled: false\n" + models,
			want:    SlotStallConfig{Enabled: false, TimeoutSeconds: 180},
			timeout: 0,
		},
		{
			name:    "zero timeout disables",
			yaml:    "slotStall:\n  timeoutSeconds: 0\n" + models,
			want:    SlotStallConfig{Enabled: true, TimeoutSeconds: 0},
			timeout: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadConfigFromReader(strings.NewReader(tc.yaml))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if cfg.SlotStall != tc.want {
				t.Errorf("SlotStall = %+v, want %+v", cfg.SlotStall, tc.want)
			}
			if got := cfg.SlotStall.StallTimeout(); got != tc.timeout {
				t.Errorf("StallTimeout() = %v, want %v", got, tc.timeout)
			}
		})
	}
}

// TestStallKnobsAreIndependent: arming one verdict must not arm or disarm the
// other. This is the guard against a future "simplification" that folds them
// into one knob.
func TestStallKnobsAreIndependent(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader(
		"peerStall:\n  enabled: false\nslotStall:\n  timeoutSeconds: 42\n" +
			"\nmodels:\n  m:\n    cmd: path/to/cmd\n    proxy: http://localhost:8080\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.PeerStall.StallTimeout(); got != 0 {
		t.Errorf("PeerStall.StallTimeout() = %v, want 0 (disabled)", got)
	}
	if got := cfg.SlotStall.StallTimeout(); got != 42*time.Second {
		t.Errorf("SlotStall.StallTimeout() = %v, want 42s", got)
	}
}
