package server

import (
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/stretchr/testify/assert"
)

func loadResidentAliasConfig(yaml string) (config.Config, error) {
	return config.LoadConfigFromReader(strings.NewReader(yaml))
}

func residentAliasTestConfig(t *testing.T) config.Config {
	cfg, err := loadResidentAliasConfig(`
models:
  modelA:
    cmd: echo a ${PORT}
  modelB:
    cmd: echo b ${PORT}
residentAliases:
  - "claude-haiku-*"
  - "default"
`)
	assert.NoError(t, err)
	return cfg
}

func TestResolveResidentAlias_ResolvesToReadyModel(t *testing.T) {
	cfg := residentAliasTestConfig(t)
	running := map[string]process.ProcessState{"modelA": process.StateReady}

	for _, requested := range []string{"claude-haiku-4-5-20251001", "default"} {
		resolved, ok := resolveResidentAlias(cfg, running, requested)
		assert.True(t, ok, requested)
		assert.Equal(t, "modelA", resolved, requested)
	}
}

func TestResolveResidentAlias_NothingResident404Path(t *testing.T) {
	cfg := residentAliasTestConfig(t)

	// No processes at all, and a process on its way out: both must refuse -
	// resolving would otherwise trigger a load.
	for _, running := range []map[string]process.ProcessState{
		{},
		{"modelA": process.StateStopping},
	} {
		_, ok := resolveResidentAlias(cfg, running, "default")
		assert.False(t, ok)
	}
}

// A model that is LOADING is the resident-to-be: a request resolved onto it
// waits in the router for ready, exactly like the parent session's own turn
// does during the same reload. Refusing here instead gave a Claude Code
// subagent a 0 ms 404 on every turn of a child reload while its parent
// survived (llama-cm incident 2026-09-08-subagent-turns-invisible-share-
// parent-slot-lane, dogfood audit row 3). Nothing new is loaded: the process
// is already starting.
func TestResolveResidentAlias_LoadingModelIsWaitedForNotRefused(t *testing.T) {
	cfg := residentAliasTestConfig(t)

	resolved, ok := resolveResidentAlias(cfg, map[string]process.ProcessState{"modelA": process.StateStarting}, "default")
	assert.True(t, ok)
	assert.Equal(t, "modelA", resolved)

	// A ready model still wins over a loading one.
	resolved, ok = resolveResidentAlias(cfg, map[string]process.ProcessState{
		"modelA": process.StateStarting,
		"modelB": process.StateReady,
	}, "default")
	assert.True(t, ok)
	assert.Equal(t, "modelB", resolved)
}

func TestResolveResidentAlias_NonMatchingIdRefused(t *testing.T) {
	cfg := residentAliasTestConfig(t)
	running := map[string]process.ProcessState{"modelA": process.StateReady}

	_, ok := resolveResidentAlias(cfg, running, "claude-sonnet-5")
	assert.False(t, ok)
}

func TestResolveResidentAlias_DeterministicTieBreak(t *testing.T) {
	cfg := residentAliasTestConfig(t)
	running := map[string]process.ProcessState{
		"modelB": process.StateReady,
		"modelA": process.StateReady,
	}

	resolved, ok := resolveResidentAlias(cfg, running, "default")
	assert.True(t, ok)
	assert.Equal(t, "modelA", resolved)
}

func TestResidentAliasConfigValidation(t *testing.T) {
	// Bad glob pattern is rejected at load.
	_, err := loadResidentAliasConfig(`
models:
  modelA:
    cmd: echo a ${PORT}
residentAliases:
  - "claude-[haiku"
`)
	assert.Error(t, err)

	// Collision with a static alias is rejected: the flat map would shadow
	// the resident alias forever.
	_, err = loadResidentAliasConfig(`
models:
  modelA:
    cmd: echo a ${PORT}
    aliases:
      - "default"
residentAliases:
  - "default"
`)
	assert.Error(t, err)
}
