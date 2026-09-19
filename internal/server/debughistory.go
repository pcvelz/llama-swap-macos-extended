package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/membrake"
)

// Debug history (fork): a bounded, in-memory record of what the box did over
// the last retainMinutes, served by GET /api/debug/history. It exists so that
// "why did this session flip from DECODE back to PREFILL at 80k" is one
// request instead of a join of the llama-swap log, the child log and the
// state trace.
//
// Two streams, both pruned to the retain window:
//   - samples: one per intervalMs, taken on the 1 Hz session-state loop
//     (sessionsHub.tick): memory (wired / file-backed / swap, read in-process
//     by the memory brake's no-fork sampler), resident model, parked count,
//     brake hold, and every session's phase, slot and counters.
//   - events: request completions from the access-log middleware (method,
//     path, status, size, duration, tier, cut=), session phase transitions,
//     and memory-brake trips. GET requests that succeed are status polls and
//     are not recorded.
//
// Off unless debugHistory.enabled; costs one small struct per second then.

const debugHistoryMaxEvents = 5000

type dhMem struct {
	WiredGB      float64 `json:"wiredGB"`
	FileBackedGB float64 `json:"fileBackedGB"`
	SwapGB       float64 `json:"swapGB"`
}

type dhSession struct {
	Session     string   `json:"session"`
	Phase       string   `json:"phase"`
	ParkReason  *string  `json:"parkReason,omitempty"`
	Slot        *int     `json:"slot"`
	Tier        string   `json:"tier"`
	Priority    int      `json:"priority"`
	Used        int      `json:"used"`
	Cached      int      `json:"cached"`
	Processed   int      `json:"processed"`
	Decoded     int      `json:"decoded"`
	PromptTotal int      `json:"promptTotal"`
	Progress    *float64 `json:"progress"`
	TPS         *float64 `json:"tps"`
	RespTokens  int64    `json:"respTokens"`
}

type dhSample struct {
	T            time.Time   `json:"t"`
	Mem          *dhMem      `json:"mem"`
	Resident     string      `json:"resident"`
	Waiting      int         `json:"waiting"`
	BrakeHolding bool        `json:"brakeHolding"`
	Sessions     []dhSession `json:"sessions"`
}

type dhEvent struct {
	T       time.Time      `json:"t"`
	Kind    string         `json:"kind"`
	Session string         `json:"session,omitempty"`
	Detail  map[string]any `json:"detail"`
}

type debugHistory struct {
	interval time.Duration
	retain   time.Duration
	sampler  membrake.Sampler

	mu        sync.Mutex
	samples   []dhSample
	events    []dhEvent
	lastAt    time.Time
	lastPhase map[string]string
	lastBrake time.Time
	reading   membrake.Reading
}

// activeDebugHistory is read by the access-log middleware, which has no
// Server in scope. nil when the history is off.
var activeDebugHistory atomic.Pointer[debugHistory]

func newDebugHistory(cfg config.DebugHistoryConfig, sampler membrake.Sampler) *debugHistory {
	iv := time.Duration(cfg.IntervalMs) * time.Millisecond
	if iv < time.Second {
		iv = time.Second
	}
	return &debugHistory{
		interval:  iv,
		retain:    time.Duration(cfg.RetainMinutes) * time.Minute,
		sampler:   sampler,
		lastPhase: map[string]string{},
	}
}

func gbOf(b uint64) float64 { return round(float64(b)/(1<<30), 2) }

// pruneLocked drops samples and events older than the retain window.
func (h *debugHistory) pruneLocked(now time.Time) {
	cut := now.Add(-h.retain)
	i := 0
	for i < len(h.samples) && h.samples[i].T.Before(cut) {
		i++
	}
	h.samples = h.samples[i:]
	j := 0
	for j < len(h.events) && h.events[j].T.Before(cut) {
		j++
	}
	if over := len(h.events) - j - debugHistoryMaxEvents; over > 0 {
		j += over
	}
	h.events = h.events[j:]
}

func (h *debugHistory) addEventLocked(e dhEvent) {
	h.events = append(h.events, e)
}

// record folds one session-state snapshot into the history. Phase
// transitions and brake trips become events on every call; a sample is kept
// only once per interval.
func (h *debugHistory) record(now time.Time, b sessionsBody) {
	h.mu.Lock()
	defer h.mu.Unlock()

	seen := map[string]bool{}
	for _, s := range b.Sessions {
		key := s.SessionShort
		seen[key] = true
		if prev, ok := h.lastPhase[key]; !ok || prev != s.Phase {
			d := map[string]any{"from": prev, "to": s.Phase, "used": s.Context.Used, "promptTotal": s.Context.PromptTotal}
			if !ok {
				d["from"] = ""
			}
			if s.ParkReason != nil {
				d["parkReason"] = *s.ParkReason
			}
			h.addEventLocked(dhEvent{T: now, Kind: "phase", Session: key, Detail: d})
			h.lastPhase[key] = s.Phase
		}
	}
	for key, prev := range h.lastPhase {
		if !seen[key] {
			h.addEventLocked(dhEvent{T: now, Kind: "phase", Session: key, Detail: map[string]any{"from": prev, "to": "GONE"}})
			delete(h.lastPhase, key)
		}
	}
	if ev := b.MemoryBrake.Last; ev != nil && ev.At.After(h.lastBrake) {
		h.lastBrake = ev.At
		h.addEventLocked(dhEvent{T: ev.At, Kind: "brake", Detail: map[string]any{
			"growthGB": ev.GrowthGB, "fileBackedGB": ev.FileGB, "windowMinGB": ev.WindowMinGB,
			"wiredGB": ev.WiredGB, "swapGB": ev.SwapGB, "killed": ev.Killed,
		}})
	}

	if !h.lastAt.IsZero() && now.Sub(h.lastAt) < h.interval {
		h.pruneLocked(now)
		return
	}
	h.lastAt = now

	smp := dhSample{T: now, Waiting: b.Queue.Waiting, BrakeHolding: b.MemoryBrake.Holding, Sessions: make([]dhSession, 0, len(b.Sessions))}
	if h.sampler != nil && h.sampler.Sample(&h.reading) == nil {
		smp.Mem = &dhMem{WiredGB: gbOf(h.reading.Wired), FileBackedGB: gbOf(h.reading.FileBacked), SwapGB: gbOf(h.reading.SwapUsed)}
	}
	if r := b.Resident; r != nil {
		name := r.Alias
		if name == "" {
			name = r.Model
		}
		smp.Resident = name + ":" + r.State
	}
	for _, s := range b.Sessions {
		smp.Sessions = append(smp.Sessions, dhSession{
			Session: s.SessionShort, Phase: s.Phase, ParkReason: s.ParkReason, Slot: s.Slot,
			Tier: s.Tier, Priority: s.Priority,
			Used: s.Context.Used, Cached: s.Context.Cached, Processed: s.Context.Processed,
			Decoded: s.Context.Decoded, PromptTotal: s.Context.PromptTotal,
			Progress: s.Progress, TPS: s.Rate.TokensPerSecond, RespTokens: s.RespTokens,
		})
	}
	h.samples = append(h.samples, smp)
	h.pruneLocked(now)
}

// requestDone records one finished request from the access-log middleware.
// Successful GETs are status polls and are skipped.
func (h *debugHistory) requestDone(now time.Time, method, path string, status int, size int64, dur time.Duration, tier, cut string) {
	if method == http.MethodGet && status < 400 {
		return
	}
	d := map[string]any{"method": method, "path": path, "status": status, "bytes": size, "durationMs": dur.Milliseconds(), "tier": tier}
	if cut != "" {
		d["cut"] = cut
	}
	h.mu.Lock()
	h.addEventLocked(dhEvent{T: now, Kind: "request", Detail: d})
	h.pruneLocked(now)
	h.mu.Unlock()
}

type debugHistoryBody struct {
	Enabled       bool       `json:"enabled"`
	IntervalMs    int64      `json:"intervalMs"`
	RetainMinutes int        `json:"retainMinutes"`
	From          *time.Time `json:"from"`
	To            *time.Time `json:"to"`
	Samples       []dhSample `json:"samples"`
	Events        []dhEvent  `json:"events"`
}

// view returns the history since `since` (zero = everything retained),
// optionally narrowed to one session (matched on its 8-char short id or any
// prefix of it); request and brake events are box-wide and always kept.
func (h *debugHistory) view(since time.Time, session string) debugHistoryBody {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := debugHistoryBody{
		Enabled: true, IntervalMs: h.interval.Milliseconds(), RetainMinutes: int(h.retain / time.Minute),
		Samples: []dhSample{}, Events: []dhEvent{},
	}
	// A full session id or any prefix of the 8-char short id both match.
	match := func(short string) bool {
		if session == "" {
			return true
		}
		return strings.HasPrefix(short, session) || (short != "" && strings.HasPrefix(session, short))
	}
	for _, s := range h.samples {
		if s.T.Before(since) {
			continue
		}
		cp := s
		if session != "" {
			cp.Sessions = nil
			for _, e := range s.Sessions {
				if match(e.Session) {
					cp.Sessions = append(cp.Sessions, e)
				}
			}
		}
		out.Samples = append(out.Samples, cp)
	}
	for _, e := range h.events {
		if e.T.Before(since) {
			continue
		}
		if e.Kind == "phase" && !match(e.Session) {
			continue
		}
		out.Events = append(out.Events, e)
	}
	if n := len(out.Samples); n > 0 {
		f, t := out.Samples[0].T, out.Samples[n-1].T
		out.From, out.To = &f, &t
	}
	return out
}

// handleAPIDebugHistory serves GET /api/debug/history?minutes=N&session=ID.
func (s *Server) handleAPIDebugHistory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.debugHistory == nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"enabled": false, "hint": "set debugHistory.enabled: true in the llama-swap config"})
		return
	}
	var since time.Time
	if m, err := strconv.ParseFloat(r.URL.Query().Get("minutes"), 64); err == nil && m > 0 {
		since = time.Now().Add(-time.Duration(m * float64(time.Minute)))
	}
	json.NewEncoder(w).Encode(s.debugHistory.view(since, r.URL.Query().Get("session")))
}
