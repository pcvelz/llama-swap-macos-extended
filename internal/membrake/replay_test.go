package membrake

import (
	"bufio"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/process"
)

// The calibration replay: the real observer `sys` ticks of 2026-09-18
// 13:44-20:15 (testdata/), interpolated LINEARLY to the 1 s sample interval,
// fed through the brake with its shipped defaults. Linear interpolation is the
// worst case for detection (a real burst front-loads its growth and fires
// sooner).
//
// Replay stand-ins for what production reads from the process registry:
//   - children: one child on a segment whose two ticks carry the same
//     non-zero llama-swap child pid;
//   - "a child is starting": the segment on which a NEW pid first appears
//     (the load). The arm delay counts from there, as in the owner backtest
//     (/tmp/backtest_fb.py: armed once that pid has been seen 10 minutes);
//   - a gap > 120 s between ticks is a reboot or observer outage: the brake
//     restarts, as llama-swap itself would.
//
// Expected (owner backtest at 3.5 GB): fire in the 14:47 run-up (segment
// ending 14:39:22) and the 16:02 run-up (segment ending 15:56:05), and on no
// other segment - in particular not in the creep episodes 17:16-17:22,
// 18:31-18:36, 18:55-19:07, 19:55-20:09 nor after any load.

type tick struct {
	t       time.Time
	file    float64
	servers int
	pid     int
}

func loadTicks(t *testing.T) []tick {
	f, err := os.Open("testdata/observer-filebacked-2026-09-18.log")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []tick
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fs := strings.Fields(line)
		ts, err := time.ParseInLocation("2006-01-02 15:04:05", "2026-09-18 "+fs[0], time.Local)
		if err != nil {
			t.Fatal(err)
		}
		fb, _ := strconv.ParseFloat(fs[1], 64)
		sv, _ := strconv.Atoi(fs[2])
		pid, _ := strconv.Atoi(fs[3])
		out = append(out, tick{t: ts, file: fb, servers: sv, pid: pid})
	}
	return out
}

type replayResult struct {
	fires map[string]int     // segment-end tick -> seconds into the segment when it fired
	peak  map[string]float64 // segment-end tick -> peak armed growth (GB) inside the segment
}

func replay(t *testing.T, cfg config.MemoryBrakeConfig) replayResult {
	ticks := loadTicks(t)
	res := replayResult{fires: map[string]int{}, peak: map[string]float64{}}
	kids := &fakeKids{}
	var b *Brake
	fresh := func() {
		b = New(cfg, nil, kids.snap, func(int) error { return nil }, nil, &Hold{})
	}
	fresh()
	for i := 1; i < len(ticks); i++ {
		a, z := ticks[i-1], ticks[i]
		label := z.t.Format("15:04:05")
		secs := int(z.t.Sub(a.t) / time.Second)
		if secs > 120 {
			fresh()
			continue
		}
		kids.kids = nil
		if a.servers >= 1 && z.servers >= 1 && z.pid != 0 {
			kids.kids = []process.LiveChild{{ID: "cq27", Pgid: 4242}}
		}
		kids.loading = z.pid != 0 && a.pid != z.pid
		for s := 1; s <= secs; s++ {
			f := float64(s) / float64(secs)
			r := Reading{FileBacked: uint64((a.file + (z.file-a.file)*f) * gib)}
			fired := b.observe(a.t.Add(time.Duration(s)*time.Second), r)
			if g := float64(b.lastGrowth) / gib; b.lastGrowth >= 0 && g > res.peak[label] {
				res.peak[label] = g
			}
			if fired {
				if _, ok := res.fires[label]; !ok {
					res.fires[label] = s
				}
			}
		}
	}
	return res
}

func sortedKeys(m map[string]int) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// inRunUp: the two fatal run-ups (from their first 3 GB-scale rise to the last
// tick before the panic). Growth there is the signal, not a margin.
func inRunUp(label string) bool {
	return (label >= "14:34:00" && label <= "14:42:01") || (label >= "15:53:00" && label <= "16:01:07")
}

func TestReplayFiresOnlyOnTheFatalRunUps(t *testing.T) {
	cfg := config.DefaultMemoryBrakeConfig()
	cfg.MarkerPath = ""
	res := replay(t, cfg)

	got := sortedKeys(res.fires)
	if len(got) != 2 || got[0] != "14:39:22" || got[1] != "15:56:05" {
		t.Fatalf("brake fired in segments ending %v, want exactly [14:39:22 15:56:05]", got)
	}
	for _, k := range got {
		t.Logf("fired in the segment ending %s, %ds into the tick (linear-growth worst case)", k, res.fires[k])
	}

	// Margins, against the pure observer data (no threshold in the way).
	probe := cfg
	probe.GrowthGB = 1e6
	all := replay(t, probe)
	maxNormal, maxNormalAt := 0.0, ""
	for k, g := range all.peak {
		if !inRunUp(k) && g > maxNormal {
			maxNormal, maxNormalAt = g, k
		}
	}
	fatal1, fatal2 := 0.0, 0.0
	for k, g := range all.peak {
		if k >= "14:34:00" && k <= "14:39:22" && g > fatal1 {
			fatal1 = g
		}
		if k >= "15:53:00" && k <= "15:56:05" && g > fatal2 {
			fatal2 = g
		}
	}
	creep := map[string][2]string{
		"17:16-17:22": {"17:16:00", "17:22:59"}, "18:31-18:36": {"18:31:00", "18:36:59"},
		"18:55-19:07": {"18:55:00", "19:07:59"}, "19:55-20:09": {"19:55:00", "20:09:59"},
	}
	for _, name := range []string{"17:16-17:22", "18:31-18:36", "18:55-19:07", "19:55-20:09"} {
		w, pk := creep[name], 0.0
		for k, g := range all.peak {
			if k >= w[0] && k <= w[1] && g > pk {
				pk = g
			}
		}
		t.Logf("creep %s: peak armed growth %.2f GB (margin -%.2f)", name, pk, cfg.GrowthGB-pk)
	}
	t.Logf("threshold %.2f GB / %d min, armed %d min after ready; 14:47 run-up peak by 14:39:22 %.2f GB (margin +%.2f); 16:02 run-up peak by 15:56:05 %.2f GB (margin +%.2f); largest armed non-run-up growth %.2f GB at %s (margin -%.2f)",
		cfg.GrowthGB, cfg.WindowMinutes, cfg.ArmAfterMinutes, fatal1, fatal1-cfg.GrowthGB, fatal2, fatal2-cfg.GrowthGB, maxNormal, maxNormalAt, cfg.GrowthGB-maxNormal)
	if maxNormal >= cfg.GrowthGB {
		t.Fatalf("armed non-fatal growth %.2f GB at %s reaches the threshold", maxNormal, maxNormalAt)
	}
}

// The owner backtest table, reproduced: 3.0 fires earlier in both run-ups,
// 4.0 still catches both on this replay. Logged, and the silence outside the
// run-ups asserted, for every candidate.
func TestReplayOwnerBacktestThresholds(t *testing.T) {
	for _, g := range []float64{3.0, 3.5, 4.0} {
		cfg := config.DefaultMemoryBrakeConfig()
		cfg.MarkerPath = ""
		cfg.GrowthGB = g
		res := replay(t, cfg)
		keys := sortedKeys(res.fires)
		t.Logf("%.1f GB: fires in segments ending %v", g, keys)
		for _, k := range keys {
			if !inRunUp(k) {
				t.Errorf("%.1f GB: false fire in segment ending %s", g, k)
			}
		}
	}
}
