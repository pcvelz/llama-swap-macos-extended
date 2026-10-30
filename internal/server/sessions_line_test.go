package server

import "testing"

// The row text is rendered once, here. These tables are the formatting spec
// the macOS dropdown tests used to carry (ported from the Swift SessionRow
// tests when the rendering moved server-side).

func lnInt(n int) *int       { return &n }
func lnStr(s string) *string { return &s }
func lnF(f float64) *float64 { return &f }
func lnRate(tps float64) *sessionRate {
	k := "decode"
	return &sessionRate{Kind: &k, TokensPerSecond: &tps, WindowSeconds: 30}
}

func lnRow(mut func(*sessionEntry)) sessionEntry {
	r := sessionEntry{
		SessionShort: "aaaaaaaa", Model: "cq35", Alias: "cq35",
		Tier: "default", Priority: 0, Phase: "IDLE",
		Context: sessionContext{Window: 262144},
	}
	if mut != nil {
		mut(&r)
	}
	return r
}

func TestRowLine_Table(t *testing.T) {
	prefillRate := lnRate(50.7)
	cases := []struct {
		name string
		row  sessionEntry
		want string
	}{
		{"decode with rate", lnRow(func(r *sessionEntry) {
			r.Phase = "DECODE"
			r.Context.Used = 50000
			r.Rate = *lnRate(45.2)
		}), "[aaaaaaaa] cq35 · DECODE · 50.0k/262.1k · 45.2 t/s · default P0"},
		{"prefill progress and rate", lnRow(func(r *sessionEntry) {
			r.SessionShort = "69699f8b"
			r.Alias = "cq27"
			r.Phase = "PREFILL"
			r.Context.Used = 92160
			r.Progress = lnF(0.9)
			r.Rate = *prefillRate
		}), "[69699f8b] cq27 · PREFILL · 92.2k/262.1k · 90.0% · 50.7 t/s · default P0"},
		{"prefill without progress", lnRow(func(r *sessionEntry) {
			r.Phase = "PREFILL"
			r.Context.Used = 20000
		}), "[aaaaaaaa] cq35 · PREFILL · 20.0k/262.1k · default P0"},
		{"hot no rate", lnRow(func(r *sessionEntry) {
			r.SessionShort = "0f0e0d0c"
			r.Phase = "HOT"
			r.Context.Used = 31800
		}), "[0f0e0d0c] cq35 · HOT · 31.8k/262.1k · default P0"},
		{"parent bracket", lnRow(func(r *sessionEntry) {
			r.ParentSessionShort = "parent01"
			r.SessionShort = "child001"
			r.Phase = "PARKED"
			r.ParkReason = lnStr("kv")
		}), "[parent01 > child001] cq35 · PARKED (kv pool) · 0/262.1k · default P0"},
		{"parent with empty child", lnRow(func(r *sessionEntry) {
			r.ParentSessionShort = "parent03"
			r.SessionShort = ""
			r.Phase = "PARKED"
			r.ParkReason = lnStr("rank")
		}), "[parent03 > -] cq35 · PARKED (behind higher rank) · 0/262.1k · default P0"},
		{"empty short", lnRow(func(r *sessionEntry) { r.SessionShort = "" }),
			"[-] cq35 · IDLE · 0/262.1k · default P0"},
		{"priority tier", lnRow(func(r *sessionEntry) {
			r.ParentSessionShort = "p1234567"
			r.SessionShort = "child002"
			r.Alias = "cq27"
			r.Phase = "LOADING"
			r.Tier = "priority"
			r.Priority = 10
		}), "[p1234567 > child002] cq27 · LOADING · 0/262.1k · priority P10"},
		{"background negative rank", lnRow(func(r *sessionEntry) {
			r.Tier = "background"
			r.Priority = -10
		}), "[aaaaaaaa] cq35 · IDLE · 0/262.1k · background P-10"},
		{"unknown tier verbatim", lnRow(func(r *sessionEntry) {
			r.Tier = "batch"
			r.Priority = -5
		}), "[aaaaaaaa] cq35 · IDLE · 0/262.1k · batch P-5"},
		{"empty tier is a dash", lnRow(func(r *sessionEntry) { r.Tier = "" }),
			"[aaaaaaaa] cq35 · IDLE · 0/262.1k · - P0"},
		{"alias empty falls back to model", lnRow(func(r *sessionEntry) {
			r.Alias = ""
			r.Model = "Qwen3.6-35B"
		}), "[aaaaaaaa] Qwen3.6-35B · IDLE · 0/262.1k · default P0"},
		{"millions", lnRow(func(r *sessionEntry) {
			r.Context.Used = 1_234_567
			r.Context.Window = 2_000_000
		}), "[aaaaaaaa] cq35 · IDLE · 1.2M/2.0M · default P0"},

		{"park reason cap", lnRow(func(r *sessionEntry) {
			r.Phase = "PARKED"
			r.ParkReason = lnStr("cap")
		}), "[aaaaaaaa] cq35 · PARKED (slots full) · 0/262.1k · default P0"},
		{"park reason busy", lnRow(func(r *sessionEntry) {
			r.Phase = "PARKED"
			r.ParkReason = lnStr("busy")
		}), "[aaaaaaaa] cq35 · PARKED (resident busy) · 0/262.1k · default P0"},
		{"park reason cooldown", lnRow(func(r *sessionEntry) {
			r.Phase = "PARKED"
			r.ParkReason = lnStr("cooldown")
		}), "[aaaaaaaa] cq35 · PARKED (cooldown) · 0/262.1k · default P0"},
		{"park reason loading", lnRow(func(r *sessionEntry) {
			r.Phase = "PARKED"
			r.ParkReason = lnStr("loading")
		}), "[aaaaaaaa] cq35 · PARKED (loading) · 0/262.1k · default P0"},
		{"park reason swap-collision", lnRow(func(r *sessionEntry) {
			r.Phase = "PARKED"
			r.ParkReason = lnStr("swap-collision")
		}), "[aaaaaaaa] cq35 · PARKED (another swap in flight) · 0/262.1k · default P0"},
		{"park reason memory-brake", lnRow(func(r *sessionEntry) {
			r.Phase = "PARKED"
			r.ParkReason = lnStr("memory-brake")
		}), "[aaaaaaaa] cq35 · PARKED (memory brake) · 0/262.1k · default P0"},
		{"unknown park reason verbatim", lnRow(func(r *sessionEntry) {
			r.Phase = "PARKED"
			r.ParkReason = lnStr("some-future-reason")
		}), "[aaaaaaaa] cq35 · PARKED (some-future-reason) · 0/262.1k · default P0"},
		{"empty park reason has no suffix", lnRow(func(r *sessionEntry) {
			r.Phase = "PARKED"
			r.ParkReason = lnStr("")
		}), "[aaaaaaaa] cq35 · PARKED · 0/262.1k · default P0"},
		{"unknown phase verbatim", lnRow(func(r *sessionEntry) { r.Phase = "WARMING" }),
			"[aaaaaaaa] cq35 · WARMING · 0/262.1k · default P0"},

		{"penalized with hold", lnRow(func(r *sessionEntry) {
			r.SessionShort = "934159af"
			r.Alias = "ALIAS"
			r.Phase = "PENALIZED"
			r.Context.Used = 228_100
			r.Penalty = &sessionPenalty{Reason: "loop", Strike: 2, Strikes: 3, RemainingSeconds: lnInt(760), UniformRun: 86, TypicalTokens: 48}
		}), "[934159af] ALIAS · PENALIZED (loop 2/3) · 228.1k/262.1k · 12:40 · default P0"},
		{"penalized final strike held", lnRow(func(r *sessionEntry) {
			r.SessionShort = "934159af"
			r.Alias = "ALIAS"
			r.Phase = "PENALIZED"
			r.Context.Used = 228_100
			r.Penalty = &sessionPenalty{Reason: "loop", Strike: 3, Strikes: 3}
		}), "[934159af] ALIAS · PENALIZED (loop 3/3) · 228.1k/262.1k · held · default P0"},
		{"penalized parked by penalty keeps the penalized shape", lnRow(func(r *sessionEntry) {
			r.SessionShort = "934159af"
			r.Alias = "ALIAS"
			r.Phase = "PARKED"
			r.ParkReason = lnStr("penalized")
			r.Context.Used = 228_100
			r.Penalty = &sessionPenalty{Reason: "loop", Strike: 2, Strikes: 3, RemainingSeconds: lnInt(760)}
		}), "[934159af] ALIAS · PENALIZED (loop 2/3) · 228.1k/262.1k · 12:40 · default P0"},
		{"penalized countdown clamps at zero", lnRow(func(r *sessionEntry) {
			r.Phase = "PENALIZED"
			r.Penalty = &sessionPenalty{Reason: "loop", Strike: 1, Strikes: 3, RemainingSeconds: lnInt(-3)}
		}), "[aaaaaaaa] cq35 · PENALIZED (loop 1/3) · 0/262.1k · 0:00 · default P0"},
		{"penalized parent bracket", lnRow(func(r *sessionEntry) {
			r.ParentSessionShort = "parent02"
			r.SessionShort = "child003"
			r.Phase = "PENALIZED"
			r.Penalty = &sessionPenalty{Reason: "loop", Strike: 3, Strikes: 3}
		}), "[parent02 > child003] cq35 · PENALIZED (loop 3/3) · 0/262.1k · held · default P0"},
		{"penalized phase without penalty box renders verbatim", lnRow(func(r *sessionEntry) { r.Phase = "PENALIZED" }),
			"[aaaaaaaa] cq35 · PENALIZED · 0/262.1k · default P0"},

		{"retained decode renders the readout", lnRow(func(r *sessionEntry) {
			r.SessionShort = "zzzzzzzz"
			r.Alias = "a"
			r.Phase = "IDLE"
			r.Display = &sessionDisplay{Phase: "DECODE", ContextUsed: lnInt(100_800), Rate: lnRate(21.4)}
		}), "[zzzzzzzz] a · DECODE · 100.8k/262.1k · 21.4 t/s · default P0"},
		{"retained decode does not leak the raw prefill progress", lnRow(func(r *sessionEntry) {
			r.SessionShort = "zzzzzzzz"
			r.Alias = "a"
			r.Phase = "PREFILL"
			r.Progress = lnF(0.418)
			r.Rate = *lnRate(611.2)
			r.Display = &sessionDisplay{Phase: "DECODE", ContextUsed: lnInt(100_800), Rate: lnRate(21.4)}
		}), "[zzzzzzzz] a · DECODE · 100.8k/262.1k · 21.4 t/s · default P0"},
		{"retained decode covers a zero first prefill", lnRow(func(r *sessionEntry) {
			r.SessionShort = "zzzzzzzz"
			r.Alias = "a"
			r.Phase = "PREFILL"
			r.Context.Used = 0
			r.Display = &sessionDisplay{Phase: "DECODE", ContextUsed: lnInt(41_500), Rate: lnRate(58.0)}
		}), "[zzzzzzzz] a · DECODE · 41.5k/262.1k · 58.0 t/s · default P0"},
		{"retained decode does not leak the raw park reason", lnRow(func(r *sessionEntry) {
			r.SessionShort = "zzzzzzzz"
			r.Alias = "a"
			r.Phase = "PARKED"
			r.ParkReason = lnStr("lane")
			r.Display = &sessionDisplay{Phase: "DECODE", ContextUsed: lnInt(100_800), Rate: lnRate(21.4)}
		}), "[zzzzzzzz] a · DECODE · 100.8k/262.1k · 21.4 t/s · default P0"},
		{"retained decode without display rate shows no rate", lnRow(func(r *sessionEntry) {
			r.SessionShort = "zzzzzzzz"
			r.Alias = "a"
			r.Phase = "PREFILL"
			r.Rate = *lnRate(611.2)
			r.Display = &sessionDisplay{Phase: "DECODE", ContextUsed: lnInt(100_800)}
		}), "[zzzzzzzz] a · DECODE · 100.8k/262.1k · default P0"},
		{"raw prefill renders its own rate", lnRow(func(r *sessionEntry) {
			r.SessionShort = "zzzzzzzz"
			r.Alias = "a"
			r.Phase = "PREFILL"
			r.Context.Used = 20_000
			r.Progress = lnF(0.2222)
			r.Rate = *lnRate(10000)
		}), "[zzzzzzzz] a · PREFILL · 20.0k/262.1k · 22.2% · 10000.0 t/s · default P0"},
		{"display rate wins on a prefill row", lnRow(func(r *sessionEntry) {
			r.SessionShort = "zzzzzzzz"
			r.Alias = "a"
			r.Phase = "PREFILL"
			r.Context.Used = 20_000
			r.Progress = lnF(0.2222)
			r.Rate = *lnRate(0)
			r.Display = &sessionDisplay{Rate: lnRate(611.2)}
		}), "[zzzzzzzz] a · PREFILL · 20.0k/262.1k · 22.2% · 611.2 t/s · default P0"},
		{"display rate with no number shows no rate", lnRow(func(r *sessionEntry) {
			r.SessionShort = "zzzzzzzz"
			r.Alias = "a"
			r.Phase = "PREFILL"
			r.Context.Used = 20_000
			r.Progress = lnF(0.2222)
			k := "prefill"
			r.Rate = sessionRate{Kind: &k, TokensPerSecond: lnF(0)}
			r.Display = &sessionDisplay{Rate: &sessionRate{Kind: &k}}
		}), "[zzzzzzzz] a · PREFILL · 20.0k/262.1k · 22.2% · default P0"},
		{"raw rate with no number shows no rate", lnRow(func(r *sessionEntry) {
			k := "prefill"
			r.Phase = "PREFILL"
			r.Rate = sessionRate{Kind: &k}
		}), "[aaaaaaaa] cq35 · PREFILL · 0/262.1k · default P0"},
		{"retained zero used is rendered, not skipped", lnRow(func(r *sessionEntry) {
			r.Phase = "IDLE"
			r.Context.Used = 5000
			r.Display = &sessionDisplay{Phase: "DECODE", ContextUsed: lnInt(0)}
		}), "[aaaaaaaa] cq35 · DECODE · 0/262.1k · default P0"},
		{"display without phase falls back to the raw phase", lnRow(func(r *sessionEntry) {
			r.Phase = "IDLE"
			r.Display = &sessionDisplay{Rate: lnRate(3.0)}
		}), "[aaaaaaaa] cq35 · IDLE · 0/262.1k · 3.0 t/s · default P0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rowLine(c.row); got != c.want {
				t.Errorf("rowLine\n got: %q\nwant: %q", got, c.want)
			}
		})
	}
}

func TestCompactTokens(t *testing.T) {
	for n, want := range map[int]string{
		0: "0", 999: "999", 999_999: "1000.0k", 1_000: "1.0k", 98_912: "98.9k",
		999_500: "999.5k", 1_000_000: "1.0M", 1_234_567: "1.2M", 9_999_999: "10.0M",
	} {
		if got := compactTokens(n); got != want {
			t.Errorf("compactTokens(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestCompactCountdown(t *testing.T) {
	for n, want := range map[int]string{252: "4:12", 9: "0:09", 0: "0:00", -3: "0:00", 760: "12:40"} {
		if got := compactCountdown(n); got != want {
			t.Errorf("compactCountdown(%d) = %q, want %q", n, got, want)
		}
	}
}
