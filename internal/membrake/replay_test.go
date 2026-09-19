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

const (
	day18 = "2026-09-18"
	day19 = "2026-09-19"
)

func loadTicks(t *testing.T, day string) []tick {
	f, err := os.Open("testdata/observer-filebacked-" + day + ".log")
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
		ts, err := time.ParseInLocation("2006-01-02 15:04:05", day+" "+fs[0], time.Local)
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
	return replayDay(t, day18, cfg)
}

func replayDay(t *testing.T, day string, cfg config.MemoryBrakeConfig) replayResult {
	ticks := loadTicks(t, day)
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

	// The first fire in each run-up is the brake; later fires in the same
	// run-up are replay artefacts (the observer data keeps the "killed" child
	// alive, and with the brake armed at ready it re-fires on the same climb).
	got := sortedKeys(res.fires)
	first := map[string]string{}
	for _, k := range got {
		t.Logf("fired in the segment ending %s, %ds into the tick (linear-growth worst case)", k, res.fires[k])
		if !inRunUp(k) {
			t.Errorf("false fire in the segment ending %s", k)
			continue
		}
		if run := k[:2]; first[run] == "" {
			first[run] = k
		}
	}
	if first["14"] != "14:39:22" || first["15"] != "15:56:05" {
		t.Fatalf("first fires %v, want 14:39:22 (14:47 run-up) and 15:56:05 (16:02 run-up); all fires %v", first, got)
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

// Event 8 (2026-09-19): the reload at 14:48:52 went into 36 GB of leftover
// file cache and collapsed at 14:54:33, five minutes after ready - inside the
// old 10-minute warm-up, so the brake never saw it. Armed at ready, it fires
// inside that last tick, and nowhere after a load (file-backed falls there).
func TestReplay_Event8FiresOnTheReloadCollapse(t *testing.T) {
	cfg := config.DefaultMemoryBrakeConfig()
	cfg.MarkerPath = ""
	res := replayDay(t, day19, cfg)
	got := sortedKeys(res.fires)
	for _, k := range got {
		t.Logf("fired in the segment ending %s, %ds into the tick", k, res.fires[k])
	}
	if _, ok := res.fires["14:54:33"]; !ok {
		t.Fatalf("brake fired in segments ending %v, want one ending 14:54:33 (the 14:54 collapse)", got)
	}
	for _, k := range got {
		// The observer ticks miss the three live kills (each run-up fell
		// between two ticks), so the collapse is the only fire the replay
		// can see.
		if k != "14:54:33" {
			t.Errorf("unexpected fire in the segment ending %s", k)
		}
	}
}

// Arming at ready costs no false-fire margin: on both days the largest armed
// growth outside the fatal events is the same with the old 10-minute warm-up
// and with none, because file-backed only falls after a load.
func TestReplay_ArmAtReadyAddsNoFalseFireExposure(t *testing.T) {
	largestOther := func(day string, arm int) (float64, string) {
		cfg := config.DefaultMemoryBrakeConfig()
		cfg.MarkerPath = ""
		cfg.ArmAfterMinutes = arm
		cfg.GrowthGB = 1e6 // observe the growth, never trip
		mx, at := 0.0, ""
		for k, g := range replayDay(t, day, cfg).peak {
			if (day == day18 && inRunUp(k)) || (day == day19 && k == "14:54:33") {
				continue
			}
			if g > mx {
				mx, at = g, k
			}
		}
		return mx, at
	}
	for _, day := range []string{day18, day19} {
		old, oldAt := largestOther(day, 10)
		now, nowAt := largestOther(day, 0)
		t.Logf("%s: largest armed non-event growth %.2f GB at %s (arm 10 min) vs %.2f GB at %s (arm at ready); threshold 3.50", day, old, oldAt, now, nowAt)
		if now > old+0.01 || now >= 3.5 {
			t.Errorf("%s: arming at ready raised the largest non-event growth from %.2f to %.2f GB", day, old, now)
		}
	}
}

// Fix B backtest (Event 8): the drain gate started at each real brake kill of
// 2026-09-19 (marker times), fed the observer ticks that followed. The log
// cannot show what an eviction would have freed, so the replay is PASSIVE
// (eviction frees nothing): the gate opens only if the cache drains on its
// own. Expected: shut at every reload the fixed 5-minute hold admitted
// (14:11:05, 14:32:15 and the fatal 14:48:52).
func TestReplay_Event8DrainGateBlocksEveryReload(t *testing.T) {
	ticks := loadTicks(t, day19)
	at := func(hms string) time.Time {
		ts, err := time.ParseInLocation("2006-01-02 15:04:05", day19+" "+hms, time.Local)
		if err != nil {
			t.Fatal(err)
		}
		return ts
	}
	for _, k := range []struct{ kill, reload string }{
		{"14:06:00", "14:11:05"}, {"14:27:05", "14:32:15"}, {"14:43:30", "14:48:52"},
	} {
		cfg := config.DefaultMemoryBrakeConfig()
		cfg.MarkerPath = ""
		hold := &Hold{}
		b := New(cfg, nil, (&fakeKids{}).snap, func(int) error { return nil }, nil, hold)
		b.alive = func(int) bool { return false }
		b.evict = func(string) error { return nil }
		kill, reload := at(k.kill), at(k.reload)
		b.trip(kill, Reading{}, 0, 0, 0)

		// minFB/fbAtReload: the observer's own ticks strictly between the
		// kill and the reload tick (the interpolated seconds right after the
		// kill still lean on the pre-kill tick).
		openedAt, minFB, fbAtReload := time.Time{}, 1e9, 0.0
		for i := 1; i < len(ticks); i++ {
			a, z := ticks[i-1], ticks[i]
			if !z.t.After(kill) || !a.t.Before(reload) {
				continue
			}
			if z.t.Before(reload) {
				minFB = min(minFB, z.file)
				fbAtReload = z.file
			}
			secs := int(z.t.Sub(a.t) / time.Second)
			for s := 1; s <= secs; s++ {
				now := a.t.Add(time.Duration(s) * time.Second)
				if !now.After(kill) || !now.Before(reload) {
					continue
				}
				fb := a.file + (z.file-a.file)*float64(s)/float64(secs)
				b.observe(now, Reading{FileBacked: uint64(fb * gib)})
				if openedAt.IsZero() && !hold.Holding() {
					openedAt = now
				}
			}
		}
		t.Logf("kill %s -> reload %s (fixed 5 min hold admitted it): observer file-backed min %.2f GB, last tick before the reload %.2f GB; drain gate (< %.0f GB) at the reload: holding=%v",
			k.kill, k.reload, minFB, fbAtReload, cfg.DrainBelowGB, hold.Holding())
		if !openedAt.IsZero() || !hold.Holding() {
			t.Errorf("drain gate opened at %s, before the %s reload, with file-backed never below %.2f GB", openedAt.Format("15:04:05"), k.reload, minFB)
		}
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
