// Package tray implements the backend client, state model and icon rendering
// for the cross-platform system-tray helper (cmd/llama-swap-tray). Everything
// in this package is headless and unit-testable; only cmd/llama-swap-tray
// touches the actual system tray.
package tray

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// Environment variable names shared with internal/menubar (duplicated here so
// the tray package stays importable without pulling in the launcher).
const (
	EnvBaseURL = "LLAMA_SWAP_MENU_BASE_URL"
	EnvBars    = "LLAMA_SWAP_MENU_BARS"
)

// BarMetric mirrors the `menu_bar.bars` config keys.
type BarMetric string

const (
	MetricGPU  BarMetric = "gpu"
	MetricVRAM BarMetric = "vram"
	MetricCPU  BarMetric = "cpu"
	MetricRAM  BarMetric = "ram"
)

// Label returns the short human-readable name used in tooltips and menus.
func (m BarMetric) Label() string {
	switch m {
	case MetricGPU:
		return "GPU"
	case MetricVRAM:
		return "VRAM"
	case MetricCPU:
		return "CPU"
	case MetricRAM:
		return "RAM"
	default:
		return string(m)
	}
}

// DefaultBars preserves the original menu-bar behaviour: GPU util + VRAM.
var DefaultBars = []BarMetric{MetricGPU, MetricVRAM}

// ParseBars parses a comma-separated metric list (e.g. "gpu,vram"), falling
// back to DefaultBars when the value is missing, empty, or entirely invalid.
// At most two bars are rendered; extras are ignored.
func ParseBars(raw string) []BarMetric {
	var out []BarMetric
	for _, part := range strings.Split(raw, ",") {
		switch m := BarMetric(strings.ToLower(strings.TrimSpace(part))); m {
		case MetricGPU, MetricVRAM, MetricCPU, MetricRAM:
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return append([]BarMetric{}, DefaultBars...)
	}
	if len(out) > 2 {
		out = out[:2]
	}
	return out
}

// DefaultBaseURL matches llama-swap's default listen port.
const DefaultBaseURL = "http://127.0.0.1:8080"

// BaseURLFromEnv resolves the llama-swap base URL. The parent llama-swap
// process passes its listen address via LLAMA_SWAP_MENU_BASE_URL when it
// launches this helper. A value without an http(s) scheme would build broken
// request URLs silently, so anything malformed falls back to the default.
func BaseURLFromEnv() string {
	v := strings.TrimSpace(os.Getenv(EnvBaseURL))
	if strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") {
		return strings.TrimRight(v, "/")
	}
	return DefaultBaseURL
}

// BarsFromEnv resolves the configured bar metrics from LLAMA_SWAP_MENU_BARS.
func BarsFromEnv() []BarMetric {
	return ParseBars(os.Getenv(EnvBars))
}

// ModelRow is one entry of the modelStatus event / model list.
type ModelRow struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	State   string   `json:"state"`
	Aliases []string `json:"aliases"`
}

// DisplayName returns the first alias when available, otherwise the model
// name or ID, so the menu never shows an empty label.
func (m ModelRow) DisplayName() string {
	if len(m.Aliases) > 0 && m.Aliases[0] != "" {
		return m.Aliases[0]
	}
	if m.Name != "" {
		return m.Name
	}
	return m.ID
}

// waitingHold keeps a waiting count at its recent peak instead of flapping
// 0↔1 on short requests. Mirrors the config's swapGraceSeconds (600) — slot
// stability doctrine: llama-cm docs/intent/llama-swap-backend.md § Slot stability.
const waitingHold = 600 * time.Second

// State is the tray's view of the backend, updated by Client.
type State struct {
	BackendOnline bool
	Completed     int
	Waiting       int
	// WaitingByTier holds the per-tier breakdown of Waiting
	// (docs/intent/llama-swap-tiers.md). Only populated when the backend's
	// inflight event carried more than one tier; nil otherwise, so a
	// single-listener backend renders exactly as before tiers existed.
	WaitingByTier map[string]int
	Models        []ModelRow
	ActiveModelID string
	// BarValues holds 0..1 readings for the configured bars, in order.
	BarValues []float64

	// SeenInflight is set once the first inflight event has been decoded, so
	// one-shot consumers (cmd/llama-swap-status) can tell "queue is genuinely
	// idle" apart from "no inflight event received yet". The long-running
	// tray never needs it (its next event arrives within seconds), but a
	// one-shot snapshot must not print before the queue state is known.
	SeenInflight bool

	// heldSince tracks when each waiting count ("" = total) last peaked, for
	// the waitingHold anti-flap display. Now is overridable in tests.
	heldSince map[string]time.Time
	Now       func() time.Time
}

func (s *State) clock() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// holdWaiting returns the display value for a raw waiting count: rises apply
// immediately (and refresh the peak timestamp); drops only apply once the
// count has not re-peaked for waitingHold. No flapping — slot stability.
func (s *State) holdWaiting(key string, held, raw int, now time.Time) int {
	if s.heldSince == nil {
		s.heldSince = make(map[string]time.Time)
	}
	if raw >= held || now.Sub(s.heldSince[key]) > waitingHold {
		s.heldSince[key] = now
		return raw
	}
	return held
}

// ActiveDisplayName returns the display name of the active model, or "".
func (s State) ActiveDisplayName() string {
	for _, m := range s.Models {
		if m.ID == s.ActiveModelID {
			return m.DisplayName()
		}
	}
	return s.ActiveModelID
}

// BarStrings renders one "LABEL NN%" string per configured bar, shared by
// the tooltip and the menu's load row so the two readouts never diverge.
func (s State) BarStrings(bars []BarMetric) []string {
	out := make([]string, 0, len(bars))
	for i, b := range bars {
		if i < len(s.BarValues) {
			out = append(out, fmt.Sprintf("%s %d%%", b.Label(), int(s.BarValues[i]*100+0.5)))
		}
	}
	return out
}

// WaitingSummary renders the waiting count: a per-tier breakdown ("priority 2,
// default 1 waiting") when the backend carries more than one tier, otherwise
// the plain "N waiting" string, unchanged from before tiers existed. Returns
// "" when there is nothing waiting.
func (s State) WaitingSummary() string {
	if len(s.WaitingByTier) > 1 {
		names := make([]string, 0, len(s.WaitingByTier))
		for name := range s.WaitingByTier {
			names = append(names, name)
		}
		sort.Strings(names)
		parts := make([]string, 0, len(names))
		for _, name := range names {
			parts = append(parts, fmt.Sprintf("%s %d", name, s.WaitingByTier[name]))
		}
		return strings.Join(parts, ", ") + " waiting"
	}
	if s.Waiting > 0 {
		return fmt.Sprintf("%d waiting", s.Waiting)
	}
	return ""
}

// Tooltip renders a one-line summary, e.g.
// "llama-swap · cq35 · GPU 84% · VRAM 62% · 3 waiting".
func (s State) Tooltip(bars []BarMetric) string {
	parts := []string{"llama-swap"}
	if !s.BackendOnline {
		parts = append(parts, "backend offline")
	}
	if name := s.ActiveDisplayName(); name != "" {
		parts = append(parts, name)
	}
	parts = append(parts, s.BarStrings(bars)...)
	if summary := s.WaitingSummary(); summary != "" {
		parts = append(parts, summary)
	}
	return strings.Join(parts, " · ")
}

// perfResponse decodes the fields of GET /api/performance the tray needs.
// Timestamps feed the ?after= incremental poll (the endpoint otherwise
// returns up to an hour of ring-buffer samples on every request).
type perfResponse struct {
	GpuStats []struct {
		Timestamp  string  `json:"timestamp"`
		GpuUtilPct float64 `json:"gpu_util_pct"`
		MemUtilPct float64 `json:"mem_util_pct"`
	} `json:"gpu_stats"`
	SysStats []struct {
		Timestamp      string    `json:"timestamp"`
		CpuUtilPerCore []float64 `json:"cpu_util_per_core"`
		MemTotalMB     int       `json:"mem_total_mb"`
		MemUsedMB      int       `json:"mem_used_mb"`
	} `json:"sys_stats"`
}

// lastTimestamp returns the newest sample timestamp in the response, or "".
func (p perfResponse) lastTimestamp() string {
	last := ""
	if n := len(p.GpuStats); n > 0 {
		last = p.GpuStats[n-1].Timestamp
	}
	if n := len(p.SysStats); n > 0 && p.SysStats[n-1].Timestamp > last {
		last = p.SysStats[n-1].Timestamp
	}
	return last
}

// barValues extracts the latest normalized 0..1 reading per configured metric.
func (p perfResponse) barValues(bars []BarMetric) []float64 {
	out := make([]float64, len(bars))
	for i, b := range bars {
		switch b {
		case MetricGPU:
			if n := len(p.GpuStats); n > 0 {
				out[i] = p.GpuStats[n-1].GpuUtilPct / 100.0
			}
		case MetricVRAM:
			if n := len(p.GpuStats); n > 0 {
				out[i] = p.GpuStats[n-1].MemUtilPct / 100.0
			}
		case MetricCPU:
			if n := len(p.SysStats); n > 0 {
				cores := p.SysStats[n-1].CpuUtilPerCore
				if len(cores) > 0 {
					sum := 0.0
					for _, c := range cores {
						sum += c
					}
					out[i] = sum / float64(len(cores)) / 100.0
				}
			}
		case MetricRAM:
			if n := len(p.SysStats); n > 0 {
				s := p.SysStats[n-1]
				if s.MemTotalMB > 0 {
					out[i] = float64(s.MemUsedMB) / float64(s.MemTotalMB)
				}
			}
		}
	}
	return out
}

// eventEnvelope is one SSE payload from GET /api/events. Data is a nested
// JSON string, matching the web UI and macOS helper contract.
type eventEnvelope struct {
	Type string `json:"type"`
	Data string `json:"data"`
}

// decodeEvent applies one /api/events payload to the state. Returns true when
// the state changed in a way that affects the tray display.
func (s *State) decodeEvent(payload []byte) bool {
	var env eventEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return false
	}
	switch env.Type {
	case "modelStatus":
		var models []ModelRow
		if err := json.Unmarshal([]byte(env.Data), &models); err != nil {
			return false
		}
		s.Models = models
		s.ActiveModelID = ""
		for _, m := range models {
			if m.State == "ready" || m.State == "starting" {
				s.ActiveModelID = m.ID
				break
			}
		}
		return true
	case "inflight":
		var stats struct {
			Total  int            `json:"total"`
			ByTier map[string]int `json:"byTier"`
		}
		if err := json.Unmarshal([]byte(env.Data), &stats); err != nil {
			return false
		}
		now := s.clock()
		s.SeenInflight = true
		s.Waiting = s.holdWaiting("", s.Waiting, stats.Total, now)
		merged := make(map[string]int, len(stats.ByTier))
		for name := range s.WaitingByTier {
			merged[name] = 0
		}
		for name, v := range stats.ByTier {
			merged[name] = v
		}
		if len(merged) > 0 {
			held := make(map[string]int, len(merged))
			for name, raw := range merged {
				held[name] = s.holdWaiting(name, s.WaitingByTier[name], raw, now)
			}
			s.WaitingByTier = held
		}
		return true
	}
	return false
}
