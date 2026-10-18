package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestConfig_SlotGraceSeconds(t *testing.T) {
	content := `
models:
  unset:
    cmd: path/to/cmd --port ${PORT}
  zero:
    cmd: path/to/cmd --port ${PORT}
    slotGraceSeconds: 0
  thirty:
    cmd: path/to/cmd --port ${PORT}
    slotGraceSeconds: 30
  sixhundred:
    cmd: path/to/cmd --port ${PORT}
    slotGraceSeconds: 600
`
	config, err := LoadConfigFromReader(strings.NewReader(content))
	assert.NoError(t, err)
	assert.Equal(t, 0, config.Models["unset"].SlotGraceSeconds)
	assert.Equal(t, 0, config.Models["zero"].SlotGraceSeconds)
	assert.Equal(t, 30, config.Models["thirty"].SlotGraceSeconds)
	assert.Equal(t, 600, config.Models["sixhundred"].SlotGraceSeconds)
}

func TestConfig_SlotGraceSecondsNegativeRejected(t *testing.T) {
	content := `
models:
  bad:
    cmd: path/to/cmd --port ${PORT}
    slotGraceSeconds: -1
`
	_, err := LoadConfigFromReader(strings.NewReader(content))
	assert.ErrorContains(t, err, "slotGraceSeconds")
}
