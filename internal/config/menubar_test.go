package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func loadMinimal(t *testing.T, extra string) (Config, error) {
	t.Helper()
	yaml := `
models:
  model1:
    cmd: server --port ${PORT}
` + extra
	return LoadConfigFromReader(strings.NewReader(yaml))
}

func TestMenuBarConfig_DefaultWhenAbsent(t *testing.T) {
	cfg, err := loadMinimal(t, "")
	require.NoError(t, err)
	require.True(t, cfg.MenuBar.Enabled)
	require.Equal(t, []string{"gpu", "vram"}, cfg.MenuBar.Bars)
}

func TestMenuBarConfig_LegacyBoolForm(t *testing.T) {
	cfg, err := loadMinimal(t, "menu_bar: false\n")
	require.NoError(t, err)
	require.False(t, cfg.MenuBar.Enabled)
	require.Equal(t, []string{"gpu", "vram"}, cfg.MenuBar.Bars)

	cfg, err = loadMinimal(t, "menu_bar: true\n")
	require.NoError(t, err)
	require.True(t, cfg.MenuBar.Enabled)
}

func TestMenuBarConfig_MappingForm(t *testing.T) {
	cfg, err := loadMinimal(t, "menu_bar:\n  bars: [cpu, ram]\n")
	require.NoError(t, err)
	require.True(t, cfg.MenuBar.Enabled, "enabled must default to true in mapping form")
	require.Equal(t, []string{"cpu", "ram"}, cfg.MenuBar.Bars)

	cfg, err = loadMinimal(t, "menu_bar:\n  enabled: false\n  bars: [gpu]\n")
	require.NoError(t, err)
	require.False(t, cfg.MenuBar.Enabled)
	require.Equal(t, []string{"gpu"}, cfg.MenuBar.Bars)
}

func TestMenuBarConfig_MappingFormDefaultsBars(t *testing.T) {
	cfg, err := loadMinimal(t, "menu_bar:\n  enabled: true\n")
	require.NoError(t, err)
	require.Equal(t, []string{"gpu", "vram"}, cfg.MenuBar.Bars)
}

func TestMenuBarConfig_RejectsUnknownMetric(t *testing.T) {
	_, err := loadMinimal(t, "menu_bar:\n  bars: [gpu, bogus]\n")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown bar metric")
}

func TestMenuBarConfig_RejectsTooManyBars(t *testing.T) {
	_, err := loadMinimal(t, "menu_bar:\n  bars: [gpu, vram, cpu]\n")
	require.Error(t, err)
	require.Contains(t, err.Error(), "1 or 2 entries")
}

func TestMenuBarConfig_RejectsDuplicateBars(t *testing.T) {
	_, err := loadMinimal(t, "menu_bar:\n  bars: [gpu, gpu]\n")
	require.Error(t, err)
	require.Contains(t, err.Error(), "duplicate bar metric")
}

func TestMenuBarConfig_NormalizesCase(t *testing.T) {
	cfg, err := loadMinimal(t, "menu_bar:\n  bars: [GPU, ' vram ']\n")
	require.NoError(t, err)
	require.Equal(t, []string{"gpu", "vram"}, cfg.MenuBar.Bars)
}
