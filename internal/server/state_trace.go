package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// The state trace: the box's state machine as a log. One canonical line per
// CHANGE of the box array - the resident, each granted slot with its phase
// and owning session, each queued request with its park reason, the
// cooldown - with consecutive identical arrays deduped. It exists so that
// "what did the router do" is a list of lines a human reads on
// GET /api/state-trace and a test asserts exactly ("after this turn, these
// lines"; "after the swap, zero lines containing cooldown"), instead of a
// menu screenshot nobody can verify. The vocabulary is what was observed by
// hand on the live box (llama-cm docs/research/2026-09-10-cooldown-dogfood-ledger.md):
//
//	09:05:02 resident=cq35h slots=[0:DECODE(725558cb) 1:PREFILL(fd998b45)] queue=[cq35h:cap(fd998b45)] cooldown=-
//	09:12:32 resident=cq35h slots=[0:HOT(725558cb) 1:HOT(fd998b45)] queue=[cq27:cooldown(6bc24690) ...] cooldown=cq35h->cq27
//	09:22:32 resident=cq27(starting) slots=[] queue=[cq27:loading(6bc24690) ...] cooldown=-
//
// Deliberately NOT in a line: the cooldown countdown and the hot slots' idle
// seconds (they change every second and would turn the log into a clock),
// and byte counts (the keepalive pinger makes them climb on parked requests
// too). PREFILL stands for "granted, no output yet": a slot starved by the
// peer slot's prefill looks the same here until the child's per-slot
// counters are joined (ledger O4).

const stateTraceCapacity = 500

// traceSnapshot is one reading of the box, assembled by Server.observeState
// from the router, the inflight tracker and the cooldown, or built directly
// by a test.
type traceSnapshot struct {
	Alias         func(modelID string) string
	Resident      string
	ResidentState process.ProcessState
	Requests      []swaputil.InflightRequestEntry
	Cooldown      *swaputil.Cooldown
}

type stateTrace struct {
	mu    sync.Mutex
	now   func() time.Time
	cap   int
	last  string // the last line without its timestamp
	lines []string
}

func newStateTrace(now func() time.Time, capacity int) *stateTrace {
	return &stateTrace{now: now, cap: capacity}
}

// Observe appends a line when the box array changed since the last line.
func (t *stateTrace) Observe(snap traceSnapshot) {
	body := traceLine(snap)
	t.mu.Lock()
	defer t.mu.Unlock()
	if body == t.last {
		return
	}
	t.last = body
	t.lines = append(t.lines, t.now().Format("15:04:05")+" "+body)
	if len(t.lines) > t.cap {
		t.lines = t.lines[len(t.lines)-t.cap:]
	}
}

// Lines returns the trace oldest first; never nil, so an empty trace
// serialises as [] rather than null.
func (t *stateTrace) Lines() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string{}, t.lines...)
}

func short(session string) string {
	if len(session) > 8 {
		return session[:8]
	}
	return session
}

// traceLine renders the box array; see the file header for the vocabulary.
func traceLine(snap traceSnapshot) string {
	alias := snap.Alias
	if alias == nil {
		alias = func(s string) string { return s }
	}

	resident := "-"
	if snap.Resident != "" {
		resident = alias(snap.Resident)
		if snap.ResidentState != process.StateReady {
			resident += "(" + string(snap.ResidentState) + ")"
		}
	}

	type slotLine struct {
		idx  int
		text string
	}
	var slots []slotLine
	var queue []string
	for _, r := range snap.Requests {
		m := r.Metadata
		session := short(m["session_id"])
		if m["slot_granted"] == "1" && m["kv_parked"] != "1" {
			phase := "PREFILL"
			if r.RespTokens > 0 {
				phase = "DECODE"
			}
			idx, label := 99, "?"
			for _, k := range []string{"slot_id", "slot_affinity"} {
				if v, ok := m[k]; ok {
					if n, err := strconv.Atoi(v); err == nil {
						idx, label = n, v
						break
					}
				}
			}
			slots = append(slots, slotLine{idx, fmt.Sprintf("%s:%s(%s)", label, phase, session)})
			continue
		}
		reason := m["park_reason"]
		if reason == "" && m["kv_parked"] == "1" {
			reason = "kv"
		}
		if reason == "" {
			reason = "?"
		}
		queue = append(queue, fmt.Sprintf("%s:%s(%s)", alias(r.Model), reason, session))
	}
	cooldown := "-"
	if cd := snap.Cooldown; cd != nil {
		cooldown = alias(cd.EvicteeModel) + "->" + alias(cd.NextModel)
		for _, hs := range cd.Slots {
			if hs.SessionID != "" {
				slots = append(slots, slotLine{hs.Slot, fmt.Sprintf("%d:HOT(%s)", hs.Slot, short(hs.SessionID))})
			}
		}
	}
	sort.SliceStable(slots, func(i, j int) bool { return slots[i].idx < slots[j].idx })
	texts := make([]string, len(slots))
	for i, s := range slots {
		texts[i] = s.text
	}
	return fmt.Sprintf("resident=%s slots=[%s] queue=[%s] cooldown=%s",
		resident, strings.Join(texts, " "), strings.Join(queue, " "), cooldown)
}

// observeState takes one reading of the live box and feeds the trace. Called
// on every inflight event and every process-state change (the two things
// that move the array), never on the countdown tick.
func (s *Server) observeState() {
	if s.trace == nil {
		return
	}
	snap := traceSnapshot{Alias: func(id string) string { return swaputil.ShortestAlias(s.cfg, id) }}
	// The resident: a ready model first, else one that is starting. The
	// exclusive group makes this at most one in practice.
	for id, st := range s.local.RunningModels() {
		if st == process.StateReady || snap.Resident == "" {
			snap.Resident, snap.ResidentState = id, st
		}
	}
	cur := s.inflight.Current()
	reqs := append([]swaputil.InflightRequestEntry(nil), cur.Requests...)
	sort.SliceStable(reqs, func(i, j int) bool {
		a, _ := strconv.Atoi(reqs[i].ID)
		b, _ := strconv.Atoi(reqs[j].ID)
		return a < b
	})
	snap.Requests = reqs
	snap.Cooldown = s.currentCooldown()
	s.trace.Observe(snap)
}

// wireStateTrace subscribes the trace to the two events that move the box
// array. Returns the unsubscribe funcs (kept for symmetry with the other
// event.On sites; the server lives as long as the process).
func (s *Server) wireStateTrace() {
	s.trace = newStateTrace(time.Now, stateTraceCapacity)
	event.On(func(swaputil.InFlightRequestsEvent) { s.observeState() })
	event.On(func(swaputil.ProcessStateChangeEvent) { s.observeState() })
}

// handleAPIStateTrace serves the trace, oldest first: {"lines":[...]}.
func (s *Server) handleAPIStateTrace(w http.ResponseWriter, r *http.Request) {
	lines := []string{}
	if s.trace != nil {
		lines = s.trace.Lines()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"lines": lines})
}
