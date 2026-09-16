package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestConfig_MaxParallelLargePrefill(t *testing.T) {
	content := `
models:
  unset:
    cmd: path/to/cmd --port ${PORT}
  one:
    cmd: path/to/cmd --port ${PORT}
    concurrencyLimit: 2
    maxParallelLargePrefill: 1
  two:
    cmd: path/to/cmd --port ${PORT}
    concurrencyLimit: 2
    maxParallelLargePrefill: 2
  nocap:
    cmd: path/to/cmd --port ${PORT}
    maxParallelLargePrefill: 0
`
	config, err := LoadConfigFromReader(strings.NewReader(content))
	assert.NoError(t, err)
	assert.Equal(t, MODEL_CONFIG_DEFAULT_MAX_PARALLEL_LARGE_PREFILL, config.Models["unset"].MaxParallelLargePrefill)
	assert.Equal(t, 1, config.Models["unset"].MaxParallelLargePrefill)
	assert.Equal(t, 1, config.Models["one"].MaxParallelLargePrefill)
	assert.Equal(t, 2, config.Models["two"].MaxParallelLargePrefill)
	assert.Equal(t, 0, config.Models["nocap"].MaxParallelLargePrefill)
}

func TestConfig_MaxParallelLargePrefillNegativeRejected(t *testing.T) {
	content := `
models:
  bad:
    cmd: path/to/cmd --port ${PORT}
    maxParallelLargePrefill: -1
`
	_, err := LoadConfigFromReader(strings.NewReader(content))
	assert.ErrorContains(t, err, "maxParallelLargePrefill")
}
