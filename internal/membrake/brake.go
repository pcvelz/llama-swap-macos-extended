// Package membrake is the memory emergency brake: an in-process watchdog that
// SIGKILLs every local llama-server child the moment the machine's memory
// starts the run-up that ended in the 2026-09-18 kernel panics ("watchdog
// timeout: no checkins from watchdogd", llama-cm incident
// 2026-09-18-kernel-panic-watchdog-timeout-second-llama-server-suspected).
//
// # Signal and rule (user ruling 2026-09-18 20:35: ONE trigger)
//
// The signal is FILE-BACKED memory only (vm_statistics64 external_page_count).
// The trigger: file-backed(now) - min(file-backed over the last windowMinutes)
// >= growthGB, on confirmSamples consecutive samples. The window ROLLS and the
// growth is measured against the window's MINIMUM, not its first sample, so
// one rule covers both fatal shapes: the slow climb (14:47: 6.2 GB at 14:38 ->
// 10.2 GB at 14:40) crosses it minutes early, the fast burst (16:02: +4.6 GB
// in one tick) crosses it the instant it happens.
//
// Swap, free, pressure and wired are NOT in the condition. Free and pressure
// never separated fatal from normal; swap to 10.7 GB was survivable the same
// evening and braking on it would kill a healthy session every 30 minutes.
//
// Arming: a load reads the GGUF into the file cache before wiring it (+12 to
// +28 GB with nothing wrong), and file-backed stays high for minutes after.
// So the brake is DISARMED while any child is starting and with no child
// running (nothing to kill), and arms only once every child has been ready
// for armAfterMinutes. Every load or reload restarts that delay, and so does a
// trip (the killed model has to load again anyway).
//
// # Calibration (replay_test.go, the real observer ticks of 2026-09-18)
//
// The replay reproduces the owner backtest (/tmp/backtest_fb.py over
// llama-server-rss.log): at 3.5 GB / 5 min / 10 min arm it fires ~14:39 in the
// 14:47 run-up and ~15:56 in the 16:02 run-up, and stays silent through the
// creep episodes 17:16-17:22, 18:31-18:36, 18:55-19:07, 19:55-20:09 and every
// post-load window. Caveat: two fatal events and one day of ticks.
//
// # Hot path
//
// Run locks its goroutine to one OS thread and does no fork/exec and no heap
// allocation per sample (sampler_darwin.go reads host_statistics64 and
// sysctl vm.swapusage via cgo into preallocated buffers; the child snapshot
// appends into a preallocated slice). Forks stalled in kernel VM faults during
// the crashes, so anything that execs a tool would be dead exactly when needed.
// The window minimum is a monotonic deque in a fixed ring allocated up front
// (window/interval + slack entries): amortised O(1) per sample, never a scan
// of the 300 samples a 5-minute window holds at 1 s.
package membrake

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/process"
)

const gib = 1 << 30

// Reading is one memory sample, in bytes. Only FileBacked is the signal;
// Wired and SwapUsed are sampled for the marker's context.
type Reading struct {
	Wired      uint64
	FileBacked uint64
	SwapUsed   uint64
}

func (r Reading) metric() int64 { return int64(r.FileBacked) }

// Sampler fills r in place. Implementations must not allocate.
type Sampler interface {
	Sample(r *Reading) error
}

// ChildSource appends the live local children to dst and reports whether any
// is still starting (loading). process.SnapshotLive in production.
type ChildSource func(dst []process.LiveChild) ([]process.LiveChild, bool)

// Killer SIGKILLs one process group. killGroup in production.
type Killer func(pgid int) error

// Logger is the subset of logmon.Monitor the brake uses.
type Logger interface {
	Warnf(format string, args ...any)
}

// Event is one brake trip, as surfaced to /api/events and the marker file.
type Event struct {
	At          time.Time `json:"at"`
	GrowthGB    float64   `json:"growthGB"`
	FileGB      float64   `json:"fileBackedGB"`
	WindowMinGB float64   `json:"windowMinGB"`
	WindowSpanS int       `json:"windowSpanSeconds"`
	WiredGB     float64   `json:"wiredGB"`
	SwapGB      float64   `json:"swapUsedGB"`
	Killed      []string  `json:"killed"`
}

// Hold parks local-model loads for a while after a brake event, so that
// swap-grace, the kv-admission parker or a waiting client do not reload the
// model straight back into the condition that tripped the brake.
type Hold struct {
	until atomic.Int64 // unix nanos; 0 = no hold
	mu    sync.Mutex
	last  *Event
}

// DefaultHold is the process-wide hold the router consults before every load.
var DefaultHold = &Hold{}

// Set starts (or extends) the hold until t and records ev.
func (h *Hold) Set(t time.Time, ev *Event) {
	h.until.Store(t.UnixNano())
	h.mu.Lock()
	h.last = ev
	h.mu.Unlock()
}

// Remaining is how long the hold still runs at now (0 when not holding).
func (h *Hold) Remaining(now time.Time) time.Duration {
	u := h.until.Load()
	if u == 0 {
		return 0
	}
	if d := time.Unix(0, u).Sub(now); d > 0 {
		return d
	}
	return 0
}

// Last returns the most recent brake event, or nil.
func (h *Hold) Last() *Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.last
}

// Wait parks the caller until the hold expires or ctx ends. It returns nil
// when loading may proceed, ctx.Err() otherwise. log may be nil.
func (h *Hold) Wait(ctx context.Context, model string, log Logger) error {
	logged := false
	for {
		d := h.Remaining(time.Now())
		if d <= 0 {
			return nil
		}
		if !logged && log != nil {
			log.Warnf("MEMORY BRAKE HOLD: load of <%s> parked for %s after a memory brake event (holdMinutes)", model, d.Round(time.Second))
			logged = true
		}
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Status is the /api/events view of the brake.
type Status struct {
	Enabled          bool   `json:"enabled"`
	Holding          bool   `json:"holding"`
	RemainingSeconds int    `json:"remainingSeconds"`
	Last             *Event `json:"last,omitempty"`
}

// running is set by Start once the brake goroutine is live.
var running atomic.Bool

// CurrentStatus reads DefaultHold and whether the brake is running.
func CurrentStatus() Status {
	d := DefaultHold.Remaining(time.Now())
	return Status{
		Enabled:          running.Load(),
		Holding:          d > 0,
		RemainingSeconds: int((d + time.Second - 1) / time.Second),
		Last:             DefaultHold.Last(),
	}
}

// Brake is the detector + actuator. Step is the whole per-sample algorithm and
// is what the tests drive; Run calls it on a ticker.
type Brake struct {
	cfg      config.MemoryBrakeConfig
	sampler  Sampler
	children ChildSource
	kill     Killer
	log      Logger
	hold     *Hold

	window    time.Duration
	armAfter  time.Duration
	threshold int64

	// preallocated hot-path state
	cur  Reading
	kids []process.LiveChild
	// Monotonic deque (values strictly increasing from head) in a fixed ring:
	// the head is always the window minimum.
	dqT        []int64
	dqV        []int64
	head, n    int
	winStart   int64 // unix nanos of the first sample since the last reset
	consec     int
	lastDisarm int64 // unix nanos of the last sample that was disarmed
	lastGrowth int64 // growth at the last sample, -1 when disarmed
	started    bool
}

// New builds a brake. sampler/children/kill/hold may be nil to use the
// production implementations.
func New(cfg config.MemoryBrakeConfig, sampler Sampler, children ChildSource, kill Killer, log Logger, hold *Hold) *Brake {
	if children == nil {
		children = process.SnapshotLive
	}
	if kill == nil {
		kill = killGroup
	}
	if hold == nil {
		hold = DefaultHold
	}
	interval := time.Duration(cfg.SampleIntervalMs) * time.Millisecond
	window := time.Duration(cfg.WindowMinutes) * time.Minute
	capacity := int(window/interval) + 8
	return &Brake{
		cfg:       cfg,
		sampler:   sampler,
		children:  children,
		kill:      kill,
		log:       log,
		hold:      hold,
		window:    window,
		armAfter:  time.Duration(cfg.ArmAfterMinutes) * time.Minute,
		threshold: int64(cfg.GrowthGB * gib),
		kids:      make([]process.LiveChild, 0, 16),
		dqT:       make([]int64, capacity),
		dqV:       make([]int64, capacity),
	}
}

func (b *Brake) resetWindow(now int64) {
	b.head, b.n, b.consec = 0, 0, 0
	b.winStart = now
	b.lastDisarm = now
}

// push adds (ts, v) to the sliding-minimum deque and drops entries older than
// the window. Amortised O(1), no allocation.
func (b *Brake) push(ts, v int64) {
	c := len(b.dqT)
	// Pop from the back every value >= v: it can never be the minimum again.
	for b.n > 0 && b.dqV[(b.head+b.n-1)%c] >= v {
		b.n--
	}
	if b.n == c { // only reachable if samples arrive far faster than configured
		b.head = (b.head + 1) % c
		b.n--
	}
	i := (b.head + b.n) % c
	b.dqT[i], b.dqV[i] = ts, v
	b.n++
	win := int64(b.window)
	for b.n > 1 && ts-b.dqT[b.head] > win {
		b.head = (b.head + 1) % c
		b.n--
	}
}

// Step takes one sample at now and returns true when the brake tripped.
func (b *Brake) Step(now time.Time) bool {
	if b.sampler == nil || b.sampler.Sample(&b.cur) != nil {
		return false
	}
	return b.observe(now, b.cur)
}

// observe is Step after the sample is taken (the replay test feeds it directly).
func (b *Brake) observe(now time.Time, r Reading) bool {
	ts := now.UnixNano()
	m := r.metric()

	var loading bool
	b.kids, loading = b.children(b.kids[:0])
	if !b.started || loading || len(b.kids) == 0 {
		b.started = true
		b.resetWindow(ts)
	}

	b.push(ts, m)

	armed := ts-b.lastDisarm >= int64(b.armAfter)
	b.lastGrowth = -1 // -1 = disarmed this sample (read by the replay test)
	if !armed {
		b.consec = 0
		return false
	}
	lo := b.dqV[b.head]
	growth := m - lo
	b.lastGrowth = growth
	if growth < b.threshold {
		b.consec = 0
		return false
	}
	b.consec++
	if b.consec < b.cfg.ConfirmSamples {
		return false
	}
	span := time.Duration(ts - b.winStart)
	if span > b.window {
		span = b.window
	}
	b.trip(now, r, growth, lo, span)
	b.resetWindow(ts)
	return true
}

// trip is the ONE action: kill every live child's process group first, and
// only then spend time on logging and the marker file.
func (b *Brake) trip(now time.Time, r Reading, growth, windowMin int64, span time.Duration) {
	for _, c := range b.kids {
		_ = b.kill(c.Pgid)
	}

	killed := make([]string, 0, len(b.kids))
	for _, c := range b.kids {
		killed = append(killed, fmt.Sprintf("%s(pgid %d)", c.ID, c.Pgid))
	}
	ev := &Event{
		At:          now,
		GrowthGB:    round2(float64(growth) / gib),
		FileGB:      round2(float64(r.FileBacked) / gib),
		WindowMinGB: round2(float64(windowMin) / gib),
		WindowSpanS: int(span / time.Second),
		WiredGB:     round2(float64(r.Wired) / gib),
		SwapGB:      round2(float64(r.SwapUsed) / gib),
		Killed:      killed,
	}
	hold := time.Duration(b.cfg.HoldMinutes) * time.Minute
	if hold > 0 {
		b.hold.Set(now.Add(hold), ev)
	} else {
		b.hold.Set(now, ev)
	}

	line := fmt.Sprintf("%s MEMORY-BRAKE killed=%s signal=filebacked growth_gb=%.2f window_min_gb=%.2f filebacked_gb=%.2f window_span_s=%d window_min=%d threshold_gb=%.2f arm_after_min=%d confirm=%d wired_gb=%.2f swap_used_gb=%.2f hold_min=%d",
		now.Format("2006-01-02 15:04:05"), strings.Join(killed, ","), ev.GrowthGB, ev.WindowMinGB, ev.FileGB, ev.WindowSpanS,
		b.cfg.WindowMinutes, b.cfg.GrowthGB, b.cfg.ArmAfterMinutes, b.cfg.ConfirmSamples, ev.WiredGB, ev.SwapGB, b.cfg.HoldMinutes)
	if b.log != nil {
		b.log.Warnf("!!!!! MEMORY BRAKE TRIPPED: SIGKILLed every local model (%s). File-backed memory grew %.2f GB (%.2f -> %.2f GB) within %ds of a %d min rolling window (limit %.2f GB) - the run-up that preceded the 2026-09-18 kernel panics. Any live session on these models lost its KV cache and will RE-PREFILL from zero on its next turn. Local-model loads are held for %d min. Marker: %s",
			strings.Join(killed, ", "), ev.GrowthGB, ev.WindowMinGB, ev.FileGB, ev.WindowSpanS, b.cfg.WindowMinutes, b.cfg.GrowthGB, b.cfg.HoldMinutes, b.cfg.MarkerPath)
	}
	if err := appendMarker(expandHome(b.cfg.MarkerPath), line); err != nil && b.log != nil {
		b.log.Warnf("memory brake: could not write marker %s: %v", b.cfg.MarkerPath, err)
	}
}

// Run samples every SampleIntervalMs until ctx ends. It pins itself to one OS
// thread so the sampler never waits for a Go scheduler handoff.
func (b *Brake) Run(ctx context.Context) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	t := time.NewTicker(time.Duration(b.cfg.SampleIntervalMs) * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			b.Step(now)
		}
	}
}

// Start launches the brake when cfg enables it and a native sampler exists
// on this platform. It returns false (and logs why) otherwise.
func Start(ctx context.Context, cfg config.MemoryBrakeConfig, log Logger) bool {
	if !cfg.Enabled {
		if log != nil {
			log.Warnf("memory brake is DISABLED by config (memoryBrake.enabled: false)")
		}
		return false
	}
	s, err := NewNativeSampler()
	if err != nil {
		if log != nil {
			log.Warnf("memory brake unavailable: %v", err)
		}
		return false
	}
	if cfg.LegacyWindowSeconds != 0 && log != nil {
		log.Warnf("memoryBrake.windowSeconds (%d) is OBSOLETE and ignored: the brake now uses windowMinutes (%d) on file-backed growth only; remove the key from the yaml",
			cfg.LegacyWindowSeconds, cfg.WindowMinutes)
	}
	b := New(cfg, s, nil, nil, log, nil)
	running.Store(true)
	go func() {
		defer running.Store(false)
		b.Run(ctx)
	}()
	if log != nil {
		log.Warnf("memory brake ON: SIGKILL all local models if file-backed memory grows >= %.2f GB above its minimum within any rolling %d min window (%d confirming samples at %dms), armed once a model has been ready %d min; hold %d min; marker %s",
			cfg.GrowthGB, cfg.WindowMinutes, cfg.ConfirmSamples, cfg.SampleIntervalMs, cfg.ArmAfterMinutes, cfg.HoldMinutes, cfg.MarkerPath)
	}
	return true
}

func round2(f float64) float64 { return float64(int64(f*100+0.5)) / 100 }

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

func appendMarker(path, line string) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
