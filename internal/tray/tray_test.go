package tray

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"image/png"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/menubar"
)

// TestBarMetric_ConfigContract pins the tray's copies of the metric keys and
// env-var names to their single sources of truth (internal/config for the
// keys, internal/menubar for the env contract). The duplication exists so the
// tray binary doesn't import the launcher/config packages; this test is the
// compile-time guard that keeps the copies in lockstep.
func TestBarMetric_ConfigContract(t *testing.T) {
	trayKeys := []string{string(MetricGPU), string(MetricVRAM), string(MetricCPU), string(MetricRAM)}
	require.Equal(t, config.MenuBarMetrics, trayKeys)

	trayDefaults := make([]string, len(DefaultBars))
	for i, b := range DefaultBars {
		trayDefaults[i] = string(b)
	}
	require.Equal(t, config.DefaultMenuBarBars, trayDefaults)

	require.Equal(t, menubar.EnvBaseURL, EnvBaseURL)
	require.Equal(t, menubar.EnvBars, EnvBars)
}

func TestBarMetric_ParseBars(t *testing.T) {
	require.Equal(t, DefaultBars, ParseBars(""))
	require.Equal(t, DefaultBars, ParseBars("bogus,nope"))
	require.Equal(t, []BarMetric{MetricCPU, MetricRAM}, ParseBars("cpu,ram"))
	require.Equal(t, []BarMetric{MetricGPU}, ParseBars("GPU"))
	require.Equal(t, []BarMetric{MetricGPU, MetricVRAM}, ParseBars(" gpu , vram "))
	// extras beyond two are ignored
	require.Equal(t, []BarMetric{MetricGPU, MetricCPU}, ParseBars("gpu,cpu,ram"))
}

func TestPerfResponse_BarValues(t *testing.T) {
	raw := `{
		"gpu_stats": [{"gpu_util_pct": 100, "mem_util_pct": 62.5}],
		"sys_stats": [{"cpu_util_per_core": [50, 100], "mem_total_mb": 1000, "mem_used_mb": 250}]
	}`
	var p perfResponse
	require.NoError(t, json.Unmarshal([]byte(raw), &p))

	vals := p.barValues([]BarMetric{MetricGPU, MetricVRAM})
	require.InDelta(t, 1.0, vals[0], 0.001)
	require.InDelta(t, 0.625, vals[1], 0.001)

	vals = p.barValues([]BarMetric{MetricCPU, MetricRAM})
	require.InDelta(t, 0.75, vals[0], 0.001)
	require.InDelta(t, 0.25, vals[1], 0.001)
}

func TestPerfResponse_BarValuesEmptyStats(t *testing.T) {
	var p perfResponse
	vals := p.barValues([]BarMetric{MetricGPU, MetricRAM})
	require.Equal(t, []float64{0, 0}, vals)
}

func TestState_DecodeModelStatusEvent(t *testing.T) {
	models := `[{"id":"cq35","name":"Qwen 35","state":"ready","aliases":["cq35a"]},
	            {"id":"cq27","name":"Qwen 27","state":"stopped"}]`
	env, err := json.Marshal(eventEnvelope{Type: "modelStatus", Data: models})
	require.NoError(t, err)

	var s State
	require.True(t, s.decodeEvent(env))
	require.Len(t, s.Models, 2)
	require.Equal(t, "cq35", s.ActiveModelID)
	require.Equal(t, "cq35a", s.ActiveDisplayName())
}

func TestState_DecodeInflightEvent(t *testing.T) {
	env, err := json.Marshal(eventEnvelope{Type: "inflight", Data: `{"total": 3}`})
	require.NoError(t, err)

	var s State
	require.False(t, s.SeenInflight)
	require.True(t, s.decodeEvent(env))
	require.Equal(t, 3, s.Waiting)
	require.True(t, s.SeenInflight)
}

// TestState_DecodeInflightEvent_ToleratesUpstreamUnionFields decodes the
// merged /api/events "inflight" payload: our total/byTier fields plus
// upstream's operation/requests/request/id in the same JSON object (see
// internal/swaputil/events.go InFlightRequestsEvent). This was verified by
// inspection (encoding/json ignores unrecognized keys by default) rather than
// by a test; pinning it here means a future upstream merge that changes the
// union shape again fails a test instead of silently zeroing the waiting
// count.
func TestState_DecodeInflightEvent_ToleratesUpstreamUnionFields(t *testing.T) {
	data := `{"total":3,"byTier":{"default":2,"priority":1},"operation":"snapshot",` +
		`"requests":[{"id":"abc123","timestamp":"2026-08-14T00:00:00Z","model":"cq35",` +
		`"req_path":"/v1/chat/completions","method":"POST","req_headers":{},` +
		`"remote_ip":"127.0.0.1","resp_headers":{},"resp_bytes":0,"elapsed_ms":120}],` +
		`"id":"abc123"}`
	env, err := json.Marshal(eventEnvelope{Type: "inflight", Data: data})
	require.NoError(t, err)

	var s State
	require.True(t, s.decodeEvent(env))
	require.Equal(t, 3, s.Waiting)
	require.Equal(t, 2, s.WaitingByTier["default"])
	require.Equal(t, 1, s.WaitingByTier["priority"])
}

func TestState_WaitingHoldSuppressesFlap(t *testing.T) {
	now := time.Now()
	s := State{Now: func() time.Time { return now }}

	ev := func(data string) []byte {
		env, err := json.Marshal(eventEnvelope{Type: "inflight", Data: data})
		require.NoError(t, err)
		return env
	}

	require.True(t, s.decodeEvent(ev(`{"total":1,"byTier":{"default":1,"priority":0}}`)))
	require.Equal(t, 1, s.WaitingByTier["default"])

	// Short request finishes: raw drops to 0 but the display holds the peak.
	require.True(t, s.decodeEvent(ev(`{"total":0,"byTier":{"default":0,"priority":0}}`)))
	require.Equal(t, 1, s.Waiting)
	require.Equal(t, 1, s.WaitingByTier["default"])

	// After the hold expires without a re-peak, the drop applies.
	now = now.Add(waitingHold + time.Second)
	require.True(t, s.decodeEvent(ev(`{"total":0,"byTier":{"default":0,"priority":0}}`)))
	require.Equal(t, 0, s.Waiting)
	require.Equal(t, 0, s.WaitingByTier["default"])
}

func TestState_DecodeEventIgnoresUnknownAndGarbage(t *testing.T) {
	var s State
	require.False(t, s.decodeEvent([]byte(`{"type":"other","data":"{}"}`)))
	require.False(t, s.decodeEvent([]byte(`not json`)))
}

func TestState_Tooltip(t *testing.T) {
	s := State{
		BackendOnline: true,
		Models:        []ModelRow{{ID: "cq35", Name: "Qwen 35", State: "ready"}},
		ActiveModelID: "cq35",
		BarValues:     []float64{0.84, 0.62},
		Waiting:       2,
	}
	tip := s.Tooltip([]BarMetric{MetricGPU, MetricVRAM})
	require.Contains(t, tip, "Qwen 35")
	require.Contains(t, tip, "GPU 84%")
	require.Contains(t, tip, "VRAM 62%")
	require.Contains(t, tip, "2 waiting")

	offline := State{}
	require.Contains(t, offline.Tooltip(nil), "backend offline")
}

func TestRenderIcon_PNGIsValid(t *testing.T) {
	data := RenderIconPNG([]float64{0.5, 1.0})
	img, err := png.Decode(bytes.NewReader(data))
	require.NoError(t, err)
	require.Equal(t, iconSize, img.Bounds().Dx())
	require.Equal(t, iconSize, img.Bounds().Dy())
}

func TestRenderIcon_ICOStructure(t *testing.T) {
	data := RenderIconICO([]float64{0.5})
	require.Greater(t, len(data), 22)

	// ICONDIR header
	require.Equal(t, uint16(0), binary.LittleEndian.Uint16(data[0:2]), "reserved")
	require.Equal(t, uint16(1), binary.LittleEndian.Uint16(data[2:4]), "type icon")
	require.Equal(t, uint16(1), binary.LittleEndian.Uint16(data[4:6]), "count")

	// ICONDIRENTRY payload size + offset must frame a decodable PNG
	size := binary.LittleEndian.Uint32(data[14:18])
	offset := binary.LittleEndian.Uint32(data[18:22])
	require.Equal(t, uint32(22), offset)
	require.Equal(t, len(data), int(offset+size))

	_, err := png.Decode(bytes.NewReader(data[offset:]))
	require.NoError(t, err)
}

func TestRenderIcon_ClampsAndHandlesOddCounts(t *testing.T) {
	// no values, negative, >1, and >2 bars must all render without panic
	for _, vals := range [][]float64{nil, {-5}, {2, 2, 2}, {0.5}} {
		img := RenderIconImage(vals)
		require.Equal(t, iconSize, img.Bounds().Dx())
	}
}
