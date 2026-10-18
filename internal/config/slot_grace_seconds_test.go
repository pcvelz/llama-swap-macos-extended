package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// slotGraceSeconds and swapStarvationSeconds were removed 2026-10-18 (llama-cm
// docs/intent/cooldown.md): the fields still decode so an old yaml loads, but
// a positive value would promise a duration or a timeout exit the scheduler no
// longer has, so it is a load error that names why.

func TestConfig_SlotGraceSecondsZeroAndUnsetAccepted(t *testing.T) {
	content := `
models:
  unset:
    cmd: path/to/cmd --port ${PORT}
  zero:
    cmd: path/to/cmd --port ${PORT}
    slotGraceSeconds: 0
`
	config, err := LoadConfigFromReader(strings.NewReader(content))
	assert.NoError(t, err)
	assert.Equal(t, 0, config.Models["unset"].SlotGraceSeconds)
	assert.Equal(t, 0, config.Models["zero"].SlotGraceSeconds)
}

func TestConfig_SlotGraceSecondsPositiveRejected(t *testing.T) {
	content := `
models:
  old:
    cmd: path/to/cmd --port ${PORT}
    slotGraceSeconds: 600
`
	_, err := LoadConfigFromReader(strings.NewReader(content))
	assert.ErrorContains(t, err, "slotGraceSeconds")
	assert.ErrorContains(t, err, "removed 2026-10-18")
	assert.ErrorContains(t, err, "docs/intent/cooldown.md")
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

func TestConfig_SwapStarvationSecondsOffValuesAcceptedPositiveRejected(t *testing.T) {
	load := func(v string) error {
		_, err := LoadConfigFromReader(strings.NewReader("swapStarvationSeconds: " + v + `
models:
  m:
    cmd: path/to/cmd --port ${PORT}
`))
		return err
	}
	// The old "valve off" settings every deployed yaml carries.
	assert.NoError(t, load("0"))
	assert.NoError(t, load("-1"))

	err := load("30")
	assert.ErrorContains(t, err, "swapStarvationSeconds")
	assert.ErrorContains(t, err, "removed 2026-10-18")
	assert.ErrorContains(t, err, "docs/intent/cooldown.md")
}
