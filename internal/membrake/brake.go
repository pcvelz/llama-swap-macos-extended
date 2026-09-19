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
// Arming: a load reads the GGUF into the file cache (+12 to +28 GB with
// nothing wrong), so the brake is DISARMED while any child is starting and
// with no child running (nothing to kill). It arms once every child has been
// ready for armAfterMinutes - by default 0, i.e. at ready. After a load
// file-backed FALLS, and a falling signal cannot trip a growth-above-minimum
// rule, so a warm-up protects against nothing; the 10-minute warm-up it
// replaces is what hid Event 8 (2026-09-19 14:54: the collapse came five
// minutes after a reload). Every load or reload restarts the delay, and so
// does a trip.
//
// # After a kill: evict, then gate admission on the drain
//
// The killed server's mmap'd weights stay behind as file cache: 32-37 GB on
// every kill of 2026-09-19, draining on their own at ~0.2 GB/min. Reloading
// into that is what collapsed at 14:54. So after a kill - an action, not a
// second trigger - the brake:
//
//  1. waits for the killed process groups to exit (at most killExitWait);
//  2. evicts every model file it has seen a child load from the file cache
//     (msync MS_INVALIDATE, no root needed), logging file-backed before and
//     after, and re-evicts every reEvictEvery while the gate stays shut;
//  3. keeps admission HELD (Hold.Wait parks the next load, it does not fail
//     it) until file-backed has stayed below drainBelowGB for drainSettle.
//
// # Calibration (replay_test.go, the real observer ticks of 2026-09-18/19)
//
// The replay reproduces the owner backtest (/tmp/backtest_fb.py over
// llama-server-rss.log): at 3.5 GB / 5 min it fires ~14:39 in the 14:47
// run-up and ~15:56 in the 16:02 run-up of 09-18, and stays silent through
// the creep episodes 17:16-17:22, 18:31-18:36, 18:55-19:07, 19:55-20:09 and
// every post-load window; on 09-19 it fires inside the 14:54:33 tick.
// Caveat: three fatal events and two days of ticks.
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
// of the 300 samples a 5-minute window holds at 1 s. The eviction runs on the
// same goroutine but only after a kill, when no model is loaded.
package membrake

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/process"
)

const gib = 1 << 30

const (
	// killExitWait bounds how long the eviction waits for the killed process
	// groups to exit: pages still mapped by a dying server are not evictable.
	killExitWait = 30 * time.Second
	// reEvictEvery repeats the eviction while the gate is shut: pages of a
	// server being torn down keep landing in the file cache for a minute or
	// more (14:43:30 kill: 29.8 GB at +20 s, 37.0 GB at +80 s).
	reEvictEvery = 15 * time.Second
	// drainSettle is how long file-backed must stay below drainBelowGB before
	// admission reopens, so a late page-in cannot slip a reload through.
	drainSettle = 60 * time.Second
	// holdLogEvery is how often a still-shut gate says so in the log.
	holdLogEvery = 5 * time.Minute
)

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

// Hold is the admission gate after a brake kill: local-model loads are parked
// until the brake releases it (file-backed drained below drainBelowGB), so
// that swap-grace, the kv-admission parker or a waiting client do not reload
// the model straight back into the file cache the killed server left behind.
type Hold struct {
	holding    atomic.Bool
	fileBacked atomic.Int64 // bytes, last sample (for the status surface)
	drainBelow atomic.Int64 // bytes, the gate's release level
	mu         sync.Mutex
	last       *Event
	released   chan struct{} // closed by Release; nil when not holding
}

// DefaultHold is the process-wide hold the router consults before every load.
var DefaultHold = &Hold{}

// Set shuts the gate (if it is not already) until Release, releasing at
// drainBelow bytes of file-backed memory, and records ev.
func (h *Hold) Set(ev *Event, drainBelow int64) {
	h.mu.Lock()
	h.last = ev
	h.drainBelow.Store(drainBelow)
	if !h.holding.Load() {
		h.released = make(chan struct{})
		h.holding.Store(true)
	}
	h.mu.Unlock()
}

// Record stores ev as the last brake event without shutting the gate
// (drainBelowGB: 0).
func (h *Hold) Record(ev *Event) {
	h.mu.Lock()
	h.last = ev
	h.mu.Unlock()
}

// Release opens the gate and wakes every parked load.
func (h *Hold) Release() {
	h.mu.Lock()
	if h.holding.Load() {
		close(h.released)
		h.released = nil
		h.holding.Store(false)
	}
	h.mu.Unlock()
}

// Holding reports whether loads are currently parked.
func (h *Hold) Holding() bool { return h.holding.Load() }

// Last returns the most recent brake event, or nil.
func (h *Hold) Last() *Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.last
}

// Wait parks the caller until the gate opens or ctx ends. It returns nil
// when loading may proceed, ctx.Err() otherwise. log may be nil.
func (h *Hold) Wait(ctx context.Context, model string, log Logger) error {
	h.mu.Lock()
	ch := h.released
	h.mu.Unlock()
	if ch == nil {
		return nil
	}
	if log != nil {
		log.Warnf("MEMORY BRAKE HOLD: load of <%s> parked until file-backed memory drains below %.2f GB (now %.2f GB) after a memory brake kill (drainBelowGB)",
			model, float64(h.drainBelow.Load())/gib, float64(h.fileBacked.Load())/gib)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-ch:
		return nil
	}
}

// Status is the /api/events and session-contract view of the brake.
type Status struct {
	Enabled bool `json:"enabled"`
	Holding bool `json:"holding"`
	// RemainingSeconds is always 0 since the fixed hold became a drain gate
	// (2026-09-19); kept so clients that decode it keep working.
	RemainingSeconds int     `json:"remainingSeconds"`
	FileBackedGB     float64 `json:"fileBackedGB,omitempty"`
	DrainBelowGB     float64 `json:"drainBelowGB,omitempty"`
	Last             *Event  `json:"last,omitempty"`
}

// running is set by Start once the brake goroutine is live.
var running atomic.Bool

// CurrentStatus reads DefaultHold and whether the brake is running. The
// drain fields are filled only while holding: file-backed moves every second,
// and the session snapshot is pushed on change.
func CurrentStatus() Status {
	s := Status{
		Enabled: running.Load(),
		Holding: DefaultHold.Holding(),
		Last:    DefaultHold.Last(),
	}
	if s.Holding {
		s.FileBackedGB = round2(float64(DefaultHold.fileBacked.Load()) / gib)
		s.DrainBelowGB = round2(float64(DefaultHold.drainBelow.Load()) / gib)
	}
	return s
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

	// evict drops one file's pages from the file cache (evictFile); alive
	// reports whether a process group still exists (groupAlive). Tests
	// replace both.
	evict func(path string) error
	alive func(pgid int) bool

	window     time.Duration
	armAfter   time.Duration
	threshold  int64
	drainBelow int64

	// files is every model file any child was seen with: after a kill the
	// file cache can hold the weights of earlier models too.
	files map[string]struct{}
	post  postKill

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

// postKill is the evict-then-drain sequence after a trip.
type postKill struct {
	active     bool
	killedAt   int64
	pgids      []int
	evicted    bool
	lastEvict  int64
	belowSince int64 // 0 = not below the drain level
	lastLog    int64
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
		cfg:        cfg,
		sampler:    sampler,
		children:   children,
		kill:       kill,
		log:        log,
		hold:       hold,
		evict:      evictFile,
		alive:      groupAlive,
		window:     window,
		armAfter:   time.Duration(cfg.ArmAfterMinutes) * time.Minute,
		threshold:  int64(cfg.GrowthGB * gib),
		drainBelow: int64(cfg.DrainBelowGB * gib),
		files:      map[string]struct{}{},
		post:       postKill{pgids: make([]int, 0, 16)},
		kids:       make([]process.LiveChild, 0, 16),
		dqT:        make([]int64, capacity),
		dqV:        make([]int64, capacity),
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
	b.hold.fileBacked.Store(m)

	var loading bool
	b.kids, loading = b.children(b.kids[:0])
	for i := range b.kids {
		for _, f := range b.kids[i].Files {
			if _, ok := b.files[f]; !ok {
				b.files[f] = struct{}{}
			}
		}
	}
	if b.post.active {
		b.drain(now, r)
	}
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
// only then spend time on logging and the marker file. The eviction and the
// drain gate follow on later samples (drain).
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
	if b.drainBelow > 0 {
		b.hold.Set(ev, b.drainBelow)
	} else {
		b.hold.Record(ev)
	}
	b.post = postKill{active: true, killedAt: now.UnixNano(), pgids: b.post.pgids[:0]}
	for _, c := range b.kids {
		b.post.pgids = append(b.post.pgids, c.Pgid)
	}

	line := fmt.Sprintf("%s MEMORY-BRAKE killed=%s signal=filebacked growth_gb=%.2f window_min_gb=%.2f filebacked_gb=%.2f window_span_s=%d window_min=%d threshold_gb=%.2f arm_after_min=%d confirm=%d wired_gb=%.2f swap_used_gb=%.2f drain_below_gb=%.2f",
		now.Format("2006-01-02 15:04:05"), strings.Join(killed, ","), ev.GrowthGB, ev.WindowMinGB, ev.FileGB, ev.WindowSpanS,
		b.cfg.WindowMinutes, b.cfg.GrowthGB, b.cfg.ArmAfterMinutes, b.cfg.ConfirmSamples, ev.WiredGB, ev.SwapGB, b.cfg.DrainBelowGB)
	if b.log != nil {
		b.log.Warnf("!!!!! MEMORY BRAKE TRIPPED: SIGKILLed every local model (%s). File-backed memory grew %.2f GB (%.2f -> %.2f GB) within %ds of a %d min rolling window (limit %.2f GB) - the run-up that preceded the 2026-09-18 kernel panics. Any live session on these models lost its KV cache and will RE-PREFILL from zero on its next turn. Local-model loads are held until file-backed memory drains below %.2f GB. Marker: %s",
			strings.Join(killed, ", "), ev.GrowthGB, ev.WindowMinGB, ev.FileGB, ev.WindowSpanS, b.cfg.WindowMinutes, b.cfg.GrowthGB, b.cfg.DrainBelowGB, b.cfg.MarkerPath)
	}
	b.mark(line)
}

// drain runs on every sample after a trip: wait for the killed groups to
// exit, evict, then open the gate once file-backed has stayed below the drain
// level for drainSettle, re-evicting while it has not.
func (b *Brake) drain(now time.Time, r Reading) {
	p := &b.post
	ts := now.UnixNano()
	if !p.evicted {
		if ts-p.killedAt < int64(killExitWait) {
			for _, g := range p.pgids {
				if b.alive(g) {
					return
				}
			}
		}
		b.evictAll(now, r, true)
		p.evicted, p.lastEvict, p.lastLog = true, ts, ts
		return
	}
	if r.metric() < b.drainBelow || b.drainBelow == 0 {
		if p.belowSince == 0 {
			p.belowSince = ts
		}
		if ts-p.belowSince >= int64(drainSettle) || b.drainBelow == 0 {
			b.releaseAfterDrain(now, r)
		}
		return
	}
	p.belowSince = 0
	if ts-p.lastEvict >= int64(reEvictEvery) {
		b.evictAll(now, r, false)
		p.lastEvict = ts
	}
	if ts-p.lastLog >= int64(holdLogEvery) {
		p.lastLog = ts
		if b.log != nil {
			b.log.Warnf("MEMORY BRAKE HOLD: local-model loads still parked %s after the kill: file-backed %.2f GB, drain level %.2f GB",
				time.Duration(ts-p.killedAt).Round(time.Second), float64(r.FileBacked)/gib, b.cfg.DrainBelowGB)
		}
	}
}

func (b *Brake) releaseAfterDrain(now time.Time, r Reading) {
	held := time.Duration(now.UnixNano() - b.post.killedAt).Round(time.Second)
	b.post.active = false
	if !b.hold.Holding() {
		return
	}
	b.hold.Release()
	if b.log != nil {
		b.log.Warnf("MEMORY BRAKE HOLD RELEASED: file-backed %.2f GB has stayed below %.2f GB for %s; local-model loads admitted again (held %s after the kill)",
			float64(r.FileBacked)/gib, b.cfg.DrainBelowGB, drainSettle, held)
	}
	b.mark(fmt.Sprintf("%s MEMORY-BRAKE-RELEASE filebacked_gb=%.2f drain_below_gb=%.2f held_s=%d",
		now.Format("2006-01-02 15:04:05"), float64(r.FileBacked)/gib, b.cfg.DrainBelowGB, int(held/time.Second)))
}

// evictAll drops every known model file from the file cache and logs
// file-backed before and after. The first pass after a kill is always logged
// and marked; a re-evict is logged only when it freed something.
func (b *Brake) evictAll(now time.Time, r Reading, first bool) {
	paths := expandSplits(b.files)
	before := r.FileBacked
	var failed []string
	for _, p := range paths {
		if err := b.evict(p); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", p, err))
		}
	}
	after := before
	if b.sampler != nil && b.sampler.Sample(&b.cur) == nil {
		after = b.cur.FileBacked
		b.hold.fileBacked.Store(int64(after))
	}
	freed := (float64(before) - float64(after)) / gib
	if !first && freed < 0.5 {
		return
	}
	if b.log != nil {
		b.log.Warnf("MEMORY BRAKE EVICT: dropped %d model file(s) from the file cache: file-backed %.2f -> %.2f GB (drain level %.2f GB)%s",
			len(paths)-len(failed), float64(before)/gib, float64(after)/gib, b.cfg.DrainBelowGB, failedSuffix(failed))
	}
	if first {
		b.mark(fmt.Sprintf("%s MEMORY-BRAKE-EVICT files=%d filebacked_before_gb=%.2f filebacked_after_gb=%.2f drain_below_gb=%.2f failed=%d",
			now.Format("2006-01-02 15:04:05"), len(paths)-len(failed), float64(before)/gib, float64(after)/gib, b.cfg.DrainBelowGB, len(failed)))
	}
}

func failedSuffix(failed []string) string {
	if len(failed) == 0 {
		return ""
	}
	return "; could not evict: " + strings.Join(failed, "; ")
}

// splitGGUF matches the first shard of a split GGUF; llama-server loads the
// others from the same directory, so they are evicted too.
var splitGGUF = regexp.MustCompile(`^(.*)-(\d{5})-of-(\d{5})\.gguf$`)

// expandSplits returns the known files, sorted, with every shard of a split
// GGUF that exists on disk.
func expandSplits(files map[string]struct{}) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for f := range files {
		add(f)
		if m := splitGGUF.FindStringSubmatch(f); m != nil {
			shards, _ := filepath.Glob(m[1] + "-[0-9][0-9][0-9][0-9][0-9]-of-" + m[3] + ".gguf")
			for _, s := range shards {
				add(s)
			}
		}
	}
	sort.Strings(out)
	return out
}

func (b *Brake) mark(line string) {
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
	if cfg.LegacyHoldMinutes != 0 && log != nil {
		log.Warnf("memoryBrake.holdMinutes (%d) is OBSOLETE and ignored: after a kill, loads are now held until file-backed memory drains below drainBelowGB (%.2f); remove the key from the yaml",
			cfg.LegacyHoldMinutes, cfg.DrainBelowGB)
	}
	b := New(cfg, s, nil, nil, log, nil)
	running.Store(true)
	go func() {
		defer running.Store(false)
		b.Run(ctx)
	}()
	if log != nil {
		log.Warnf("memory brake ON: SIGKILL all local models if file-backed memory grows >= %.2f GB above its minimum within any rolling %d min window (%d confirming samples at %dms), armed %d min after a model is ready; after a kill, evict the model files from the file cache and hold loads until file-backed < %.2f GB; marker %s",
			cfg.GrowthGB, cfg.WindowMinutes, cfg.ConfirmSamples, cfg.SampleIntervalMs, cfg.ArmAfterMinutes, cfg.DrainBelowGB, cfg.MarkerPath)
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
