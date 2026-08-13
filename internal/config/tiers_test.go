package config

import (
	"strings"
	"testing"
)

func TestTiersSmoke_DuplicateListen(t *testing.T) {
	yaml := `
models:
  a:
    cmd: "echo hi"
    proxy: "http://127.0.0.1:9999"
tiers:
  priority:
    listen: "127.0.0.1:8002"
    rank: 10
  background:
    listen: "127.0.0.1:8002"
    rank: -10
`
	_, err := LoadConfigFromReader(strings.NewReader(yaml))
	if err == nil {
		t.Fatalf("expected duplicate listen error")
	}
}

func TestTiersSmoke_ValidTiers(t *testing.T) {
	yaml := `
models:
  a:
    cmd: "echo hi"
    proxy: "http://127.0.0.1:9999"
tiers:
  priority:
    listen: "127.0.0.1:8002"
    rank: 10
    preempts: true
  background:
    listen: "127.0.0.1:8003"
    rank: -10
    preemptible: true
`
	cfg, err := LoadConfigFromReader(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Tiers) != 2 {
		t.Fatalf("tiers=%v want 2", cfg.Tiers)
	}
	if cfg.Tiers["priority"].Rank != 10 || !cfg.Tiers["priority"].Preempts {
		t.Fatalf("priority tier=%+v", cfg.Tiers["priority"])
	}
}
