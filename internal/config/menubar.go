package config

import (
	"fmt"
	"strings"
)

// MenuBarMetrics lists the metric keys the menu-bar / system-tray helper can
// render as bars. Values are normalized 0..1 by the helper:
//   - gpu:  GPU utilization percentage
//   - vram: GPU memory (VRAM) utilization percentage
//   - cpu:  average CPU utilization across cores
//   - ram:  system memory used / total
var MenuBarMetrics = []string{"gpu", "vram", "cpu", "ram"}

// DefaultMenuBarBars preserves the original hardcoded behaviour: top bar GPU
// utilization, bottom bar GPU memory.
var DefaultMenuBarBars = []string{"gpu", "vram"}

// MenuBarConfig configures the menu-bar (macOS) / system-tray (Windows,
// Linux) helper. It accepts two YAML shapes for backwards compatibility:
//
//	menu_bar: true          # legacy bool form
//
//	menu_bar:               # mapping form
//	  enabled: true
//	  bars: [gpu, vram]     # one or two of: gpu, vram, cpu, ram
type MenuBarConfig struct {
	Enabled bool     `yaml:"enabled"`
	Bars    []string `yaml:"bars"`
}

// DefaultMenuBarConfig is the fork default: helper on, GPU util + VRAM bars.
func DefaultMenuBarConfig() MenuBarConfig {
	return MenuBarConfig{Enabled: true, Bars: append([]string{}, DefaultMenuBarBars...)}
}

func (m *MenuBarConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	// Legacy bool form: `menu_bar: false`
	var b bool
	if err := unmarshal(&b); err == nil {
		*m = DefaultMenuBarConfig()
		m.Enabled = b
		return nil
	}

	// Mapping form. Enabled defaults to true (fork default) so
	// `menu_bar: {bars: [cpu, ram]}` keeps the helper on.
	type rawMenuBarConfig MenuBarConfig
	defaults := rawMenuBarConfig(DefaultMenuBarConfig())
	defaults.Bars = nil
	if err := unmarshal(&defaults); err != nil {
		return err
	}
	*m = MenuBarConfig(defaults)
	if len(m.Bars) == 0 {
		m.Bars = append([]string{}, DefaultMenuBarBars...)
	}
	return nil
}

// Validate checks the bar metric keys and count (1 or 2 distinct bars).
func (m *MenuBarConfig) Validate() error {
	if len(m.Bars) < 1 || len(m.Bars) > 2 {
		return fmt.Errorf("bars must contain 1 or 2 entries, got %d", len(m.Bars))
	}
	seen := make(map[string]bool, len(m.Bars))
	for i, bar := range m.Bars {
		normalized := strings.ToLower(strings.TrimSpace(bar))
		valid := false
		for _, known := range MenuBarMetrics {
			if normalized == known {
				valid = true
				break
			}
		}
		if !valid {
			return fmt.Errorf("unknown bar metric %q, valid metrics: %s", bar, strings.Join(MenuBarMetrics, ", "))
		}
		if seen[normalized] {
			return fmt.Errorf("duplicate bar metric %q", normalized)
		}
		seen[normalized] = true
		m.Bars[i] = normalized
	}
	return nil
}
