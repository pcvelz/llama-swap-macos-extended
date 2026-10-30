package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The scheduler's cooldown delay reads the decode retention through
// FifoConfig, but the operator-facing knob stays sessions.decodeRetentionSeconds
// (it also drives the sessions API's display retention). The loader must
// therefore copy the sessions value into the scheduler config: the seeded
// default (30) reaches a box whose yaml names neither block, an explicit
// sessions.decodeRetentionSeconds wins, and an explicit 0 means no delay.
// A hand-built FifoConfig at its zero value means no delay, which is what
// every pre-existing scheduler test pins.
func TestConfig_DecodeRetentionReachesFifoConfig(t *testing.T) {
	base := `
models:
  m:
    cmd: path/to/cmd --port ${PORT}
`

	cfg, err := LoadConfigFromReader(strings.NewReader(base))
	assert.NoError(t, err)
	assert.Equal(t, 30, cfg.Sessions.DecodeRetentionSeconds,
		"the seeded default a real box runs")
	assert.Equal(t, 30, cfg.Routing.Scheduler.Settings.Fifo.DecodeRetentionSeconds,
		"the loader must hand the sessions default to the scheduler")

	custom := "sessions:\n  decodeRetentionSeconds: 12\n" + base
	cfg, err = LoadConfigFromReader(strings.NewReader(custom))
	assert.NoError(t, err)
	assert.Equal(t, 12, cfg.Sessions.DecodeRetentionSeconds)
	assert.Equal(t, 12, cfg.Routing.Scheduler.Settings.Fifo.DecodeRetentionSeconds,
		"an explicit sessions value wins over the seeded default")

	off := "sessions:\n  decodeRetentionSeconds: 0\n" + base
	cfg, err = LoadConfigFromReader(strings.NewReader(off))
	assert.NoError(t, err)
	assert.Equal(t, 0, cfg.Sessions.DecodeRetentionSeconds)
	assert.Equal(t, 0, cfg.Routing.Scheduler.Settings.Fifo.DecodeRetentionSeconds,
		"an explicit 0 turns the delay off at both levels")
}
