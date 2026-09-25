package router

// prefillWatch reclaims a slot whose UPSTREAM PREFILL has stopped moving.
//
// THE GAP IT CLOSES (llama-cm incident 2026-09-25-prefill-frozen-slot-never-
// reclaimed-phantom-holder-parks-free-slot). A llama-server slot held a task
// whose n_prompt_tokens_processed stopped two thirds of the way through its
// prompt and never moved again, with the main loop still alive. No proxy guard
// reclaimed it, and the reason is on the wire: from the moment llama-server
// launches a task it sends an SSE comment (":\n\n") every sse_ping_interval
// (default 30s) while it waits for the first token. To the proxy those are body
// bytes, so zero-output-budget (needs zero bytes) went inert and slot-stalled
// (measures the gap since the last byte) never saw a gap over 30s. Meanwhile
// the request QUEUED BEHIND the stuck slot, whose task never launched and so
// never got a ping, was cut by zero-output-budget every 4.5 minutes - cutting
// the victim and never the owner.
//
// THE SIGNAL is the upstream's own counter, per slot: (slot id, id_task,
// n_prompt_tokens_processed) from the child's /slots. A slot is prefill-flat
// when it is processing, has decoded nothing, has not finished its prompt, and
// the SAME task reports the SAME processed count across successful samples
// spanning the whole budget. The counter is monotonic within a task, so equal
// endpoints mean it did not move in between. Never a rate: a prefill that
// advances one token per sample is slow, and slow is the product working
// (llama-cm docs/intent/llama-swap-backend.md, "What a Claude Code session is
// promised"). A flat counter is zero progress, which is the one failure the
// ruling names.
//
// THE /slots TRAP (peerstall.go header): /slots is answered from the child's
// task queue, so a loaded child may not answer in time. Here a failed or slow
// poll is NO EVIDENCE - it neither advances nor resets anything - and a verdict
// needs two successful samples a full budget apart. A child too busy to answer
// is a child that is working, and this guard stays quiet about it.
//
// WHO IS CUT. Which proxy request owns which upstream slot is attribution,
// which this file deliberately does not do (and on the witnessed day the
// attribution was itself wrong). Instead the candidates are this model's
// granted requests that (a) have produced no token yet (only SSE comments -
// see pingWriter.wroteContent) and (b) were granted before the flat span began:
// a request granted later cannot own a task that was already stuck. And the
// cut WAITS while any other slot of the model is advancing a prefill, because
// then a candidate might be that slot's healthy owner; the ambiguity resolves
// on its own once that prefill reaches its first token.
//
// WHAT RECLAIM DOES: the existing path. pingWriter.prefillStallCut writes the
// same Anthropic SSE error frame the other pinger verdicts write and calls
// peerStallGuard.fire with verdict "prefill-stalled" (log line, access-log
// cut=, request-context cancel). Cancelling closes the upstream connection,
// and llama-server polls its client connection every HTTP_POLLING_SECONDS (1s)
// while it waits for results - prefill included - and answers a closed one by
// posting a cancel task that releases the slot.
//
// THE BACKSTOP. If the same task is still flat restartAfter after the cut (or
// there was nothing to cut, e.g. the owner is not on the Anthropic stream
// path), the child is restarted. It is the only remaining lever:
// /slots/<id>?action=erase refuses a slot that is processing, and llama-server
// has no per-task cancel endpoint. Fires once per stuck task.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
)

// upstreamSlot is the part of one llama-server /slots entry this guard reads.
type upstreamSlot struct {
	ID         int
	IDTask     int
	Processing bool
	NPrompt    int
	Processed  int
	Decoded    int
}

// inPrefill: processing, nothing decoded, prompt not finished.
func (s upstreamSlot) inPrefill() bool {
	return s.Processing && s.Decoded == 0 && (s.NPrompt == 0 || s.Processed < s.NPrompt)
}

// parsePrefillSlots decodes /slots in both the {"slots":[...]} and bare-array
// forms, with next_token as an object or a one-element array.
func parsePrefillSlots(body []byte) ([]upstreamSlot, error) {
	type rawSlot struct {
		ID           int             `json:"id"`
		IDTask       int             `json:"id_task"`
		IsProcessing bool            `json:"is_processing"`
		NPrompt      int             `json:"n_prompt_tokens"`
		Processed    int             `json:"n_prompt_tokens_processed"`
		NextToken    json.RawMessage `json:"next_token"`
	}
	var raws []rawSlot
	var wrap struct {
		Slots []rawSlot `json:"slots"`
	}
	if err := json.Unmarshal(body, &wrap); err == nil && wrap.Slots != nil {
		raws = wrap.Slots
	} else if err := json.Unmarshal(body, &raws); err != nil {
		return nil, fmt.Errorf("unparseable /slots: %w", err)
	}
	type next struct {
		NDecoded int `json:"n_decoded"`
	}
	out := make([]upstreamSlot, len(raws))
	for i, r := range raws {
		decoded := 0
		var one next
		var arr []next
		if err := json.Unmarshal(r.NextToken, &arr); err == nil && len(arr) > 0 {
			decoded = arr[0].NDecoded
		} else if err := json.Unmarshal(r.NextToken, &one); err == nil {
			decoded = one.NDecoded
		}
		out[i] = upstreamSlot{ID: r.ID, IDTask: r.IDTask, Processing: r.IsProcessing,
			NPrompt: r.NPrompt, Processed: r.Processed, Decoded: decoded}
	}
	return out, nil
}

// prefillHolder is one granted request on the watched model. pw is nil for a
// request off the Anthropic stream path: it keeps the watcher running (so a
// stuck task it owns still reaches the restart backstop) but cannot be cut.
type prefillHolder struct {
	pw      *pingWriter
	granted time.Time
}

// slotTrack is the last successful reading of one slot in prefill.
type slotTrack struct {
	idTask    int
	processed int
	since     time.Time // first successful sample showing this (task, processed)
	lastSeen  time.Time // most recent successful sample showing it
	cutAt     time.Time // when the verdict fired for this task; zero = not yet
	restarted bool
}

type prefillWatch struct {
	logger       *logmon.Monitor
	model        string
	slotsURL     string
	budget       time.Duration
	restartAfter time.Duration // zero disables the restart backstop
	restart      func()

	// Package-test knobs; production values are set by newPrefillWatch.
	pollInterval time.Duration
	pollTimeout  time.Duration

	mu      sync.Mutex
	holders map[*prefillHolder]struct{}
	running bool
	stopCh  chan struct{}
	tracks  map[int]*slotTrack // guarded by the loop goroutine only
}

// Production cadence. 15s keeps the poll cost negligible against any budget
// worth having; 5s is far above an idle child's millisecond answer and the
// point of the timeout is only to never pile polls up behind a busy one.
// Package vars, not consts, so router-level tests can shorten them.
var (
	prefillPollInterval = 15 * time.Second
	prefillPollTimeout  = 5 * time.Second
)

func newPrefillWatch(logger *logmon.Monitor, model, baseURL string, budget, restartAfter time.Duration, restart func()) *prefillWatch {
	return &prefillWatch{
		logger: logger, model: model, slotsURL: baseURL + "/slots",
		budget: budget, restartAfter: restartAfter, restart: restart,
		pollInterval: prefillPollInterval, pollTimeout: prefillPollTimeout,
		holders: map[*prefillHolder]struct{}{},
	}
}

// add registers a granted request and returns its removal. The poll loop runs
// only while at least one request holds a slot on this model: an idle model
// is never polled.
func (w *prefillWatch) add(pw *pingWriter, granted time.Time) func() {
	if w == nil {
		return func() {}
	}
	h := &prefillHolder{pw: pw, granted: granted}
	w.mu.Lock()
	w.holders[h] = struct{}{}
	if !w.running {
		w.running = true
		w.stopCh = make(chan struct{})
		go w.loop(w.stopCh)
	}
	w.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			w.mu.Lock()
			delete(w.holders, h)
			if len(w.holders) == 0 && w.running {
				w.running = false
				close(w.stopCh)
			}
			w.mu.Unlock()
		})
	}
}

func (w *prefillWatch) loop(stop chan struct{}) {
	tracks := map[int]*slotTrack{}
	t := time.NewTicker(w.pollInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		slots, err := w.poll()
		if err != nil {
			// No evidence. Deliberately not logged per tick: a busy child
			// failing to answer is the normal loaded case.
			continue
		}
		w.observe(tracks, slots, time.Now())
	}
}

func (w *prefillWatch) poll() ([]upstreamSlot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), w.pollTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.slotsURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return parsePrefillSlots(raw)
}

// observe folds one successful sample into tracks and acts on any verdict.
func (w *prefillWatch) observe(tracks map[int]*slotTrack, slots []upstreamSlot, now time.Time) {
	seen := map[int]bool{}
	for _, s := range slots {
		if !s.inPrefill() {
			continue
		}
		seen[s.ID] = true
		tr := tracks[s.ID]
		if tr == nil || tr.idTask != s.IDTask || tr.processed != s.Processed {
			tracks[s.ID] = &slotTrack{idTask: s.IDTask, processed: s.Processed, since: now, lastSeen: now}
			continue
		}
		tr.lastSeen = now
	}
	for id := range tracks {
		if !seen[id] {
			// Finished, decoding, idle or gone: progress, or nothing to watch.
			delete(tracks, id)
		}
	}

	for id, tr := range tracks {
		flat := tr.lastSeen.Sub(tr.since)
		if flat < w.budget {
			continue
		}
		if tr.cutAt.IsZero() {
			if w.otherSlotAdvancing(tracks, id, now) {
				continue
			}
			tr.cutAt = now
			cut := w.cutHolders(tr.since, flat)
			w.logger.Warnf("prefill-stalled: model=%s slot=%d task=%d processed=%d flat=%s cut=%d request(s)",
				w.model, id, tr.idTask, tr.processed, flat.Round(time.Second), cut)
			continue
		}
		if w.restartAfter > 0 && !tr.restarted && now.Sub(tr.cutAt) >= w.restartAfter {
			tr.restarted = true
			w.logger.Warnf("prefill-stalled: model=%s slot=%d task=%d still flat %s after the cut - restarting the child to free the slot",
				w.model, id, tr.idTask, now.Sub(tr.cutAt).Round(time.Second))
			if w.restart != nil {
				w.restart()
			}
		}
	}
}

// otherSlotAdvancing: some other slot is in prefill and its counter moved
// within the budget. While that holds, a pre-first-token holder could be its
// healthy owner, so no one is cut yet.
func (w *prefillWatch) otherSlotAdvancing(tracks map[int]*slotTrack, id int, now time.Time) bool {
	for other, tr := range tracks {
		if other != id && now.Sub(tr.since) < w.budget {
			return true
		}
	}
	return false
}

// prefillWatchFor returns model's watcher, creating it on first use, or nil
// (a no-op watcher) when the guard is disabled or the model has no resolvable
// proxy URL.
func (b *baseRouter) prefillWatchFor(model string) *prefillWatch {
	budget := b.config.PrefillStall.StallTimeout()
	if budget <= 0 {
		return nil
	}
	b.prefillMu.Lock()
	defer b.prefillMu.Unlock()
	if w, ok := b.prefillWatches[model]; ok {
		return w
	}
	base := childBaseURL(b.config, model)
	if base == "" {
		return nil
	}
	if b.prefillWatches == nil {
		b.prefillWatches = map[string]*prefillWatch{}
	}
	w := newPrefillWatch(b.logger, model, base, budget, b.config.PrefillStall.RestartAfter(),
		// The backstop goes through the scheduler's own unload path, off the
		// watcher goroutine: Unload blocks until the child is stopped, and the
		// next request for the model loads it fresh.
		func() { go b.Unload(0, model) })
	b.prefillWatches[model] = w
	return w
}

// childBaseURL resolves model's proxy URL with its macros, the same way
// internal/server's childProxyURL does (the router cannot import server).
func childBaseURL(cfg config.Config, model string) string {
	mc, ok := cfg.Models[model]
	if !ok || mc.Proxy == "" {
		return ""
	}
	macros := config.MacroList{{Name: "MODEL_ID", Value: model}}
	macros = append(macros, cfg.Macros...)
	for _, entry := range mc.Macros {
		found := false
		for i, existing := range macros {
			if existing.Name == entry.Name {
				macros[i] = entry
				found = true
				break
			}
		}
		if !found {
			macros = append(macros, entry)
		}
	}
	resolved, _ := config.SubstituteMacros(mc.Proxy, macros).(string)
	if resolved == "" || strings.Contains(resolved, "${") {
		return ""
	}
	return strings.TrimSuffix(resolved, "/")
}

// cutHolders ends every candidate: pre-first-token, granted no later than the
// start of the flat span. Returns how many were cut.
func (w *prefillWatch) cutHolders(flatSince time.Time, flat time.Duration) int {
	w.mu.Lock()
	var cands []*pingWriter
	for h := range w.holders {
		if h.pw != nil && !h.granted.After(flatSince) && h.pw.preFirstToken() {
			cands = append(cands, h.pw)
		}
	}
	w.mu.Unlock()
	n := 0
	for _, pw := range cands {
		if pw.prefillStallCut(flat) {
			n++
		}
	}
	return n
}
