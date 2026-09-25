package router

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/router/scheduler"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Slot binding (llama-cm incident 2026-09-25-prefill-frozen-slot-never-
// reclaimed-phantom-holder-parks-free-slot, fault 2).
//
// The slot-affinity middleware (internal/server/slot_affinity.go) pins a
// full turn to its session lane's id_slot at ARRIVAL, before the scheduler
// has granted anything. With more live sessions than slots two lanes share a
// slot, so a GRANTED request used to be sent to a slot another granted
// request was processing on. The id_slot-patched llama-server queues it
// there while the other slot idles; the scheduler counted it as a holder,
// the cap read full and the next request parked on "cap" beside a free slot.
// Witnessed twice on 2026-09-25: once behind a prefill frozen on slot 1, once
// with a healthy box right after an evict (two holders on slot 0, slot 1
// idle).
//
// The binding is decided HERE, at serve time, after the grant, and it is the
// request's own: each granted request of a slot-affinity model gets a slot no
// other granted request holds - the lane's slot when it is free (the KV cache
// is there), otherwise a free one - and the id_slot on the wire is rewritten
// to it. The cap (concurrencyLimit = the child's slot count) guarantees a
// free slot exists for every grant, so two holders never map to one slot.
//
// The cap follows the upstream's reality in the two directions it can drift,
// fed by the 1 Hz /slots poller (ObserveSlots):
//   - a request CUT by llama-swap (preemption, zero-output reclaim, client
//     gone) whose slot the upstream keeps processing still occupies that
//     slot: its cap place is only given back once the slot reads idle
//     (orphan), so the cap never admits a request into a slot that is busy.
//   - a bound request whose slot has read idle for slotPhantomAfter is a
//     phantom: its cap place and its slot are released so a parked request
//     gets them (safety net - with binding in place nothing is expected to
//     hit it).

// slotPhantomAfterDefault: a bound slot that reads idle this long while its
// request is still being served is a phantom. llama-server marks a slot
// processing as soon as the task is launched, so a real request shows busy
// within seconds; 60s leaves room for a slow /slots answer under load.
const slotPhantomAfterDefault = 60 * time.Second

// slotObserveStale: an orphan (cut request, slot still busy) keeps its cap
// place only while the poller is alive. No fresh reading for this long and
// the place is given back rather than stranded.
const slotObserveStale = 30 * time.Second

type slotHolder struct {
	slot     int
	boundAt  time.Time
	lastBusy time.Time
	released bool
	release  func()
}

type modelSlots struct {
	holders      []*slotHolder
	orphans      []chan struct{}
	busy         []bool
	lastObserved time.Time
}

type slotTable struct {
	mu     sync.Mutex
	models map[string]*modelSlots
	now    func() time.Time
}

func newSlotTable() *slotTable {
	return &slotTable{models: map[string]*modelSlots{}, now: time.Now}
}

func (t *slotTable) modelLocked(model string, n int) *modelSlots {
	ms := t.models[model]
	if ms == nil {
		ms = &modelSlots{}
		t.models[model] = ms
	}
	for len(ms.holders) < n {
		ms.holders = append(ms.holders, nil)
		ms.orphans = append(ms.orphans, nil)
	}
	return ms
}

// bind gives the request a slot of its own: preferred when free, else the
// lowest free slot. nil when no slot is free (cannot happen while the cap
// equals the slot count; the request then goes out unchanged).
func (t *slotTable) bind(model string, n, preferred int, release func()) *slotHolder {
	t.mu.Lock()
	defer t.mu.Unlock()
	ms := t.modelLocked(model, n)
	free := func(i int) bool { return ms.holders[i] == nil && ms.orphans[i] == nil }
	slot := -1
	if preferred >= 0 && preferred < n && free(preferred) {
		slot = preferred
	} else {
		for i := 0; i < n; i++ {
			if free(i) {
				slot = i
				break
			}
		}
	}
	if slot < 0 {
		return nil
	}
	now := t.now()
	h := &slotHolder{slot: slot, boundAt: now, lastBusy: now, release: release}
	ms.holders[slot] = h
	return h
}

// unbind frees h's slot. For a CUT request whose slot the last fresh reading
// showed busy, the slot becomes an orphan and the returned channel closes
// when the upstream reports it idle; the caller keeps the cap place until
// then. nil = nothing to wait for.
func (t *slotTable) unbind(model string, h *slotHolder, cut bool) <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	ms := t.models[model]
	if ms == nil || h.released {
		return nil
	}
	if ms.holders[h.slot] == h {
		ms.holders[h.slot] = nil
	}
	if !cut || h.slot >= len(ms.busy) || !ms.busy[h.slot] || t.now().Sub(ms.lastObserved) > slotObserveStale {
		return nil
	}
	ch := make(chan struct{})
	ms.orphans[h.slot] = ch
	return ch
}

// observedWithin reports whether model had a fresh /slots reading within d.
func (t *slotTable) observedWithin(model string, d time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	ms := t.models[model]
	return ms != nil && t.now().Sub(ms.lastObserved) <= d
}

// observe folds one FRESH upstream reading in. busy == nil means the model
// is not running: every orphan is let go (its slots are gone).
func (t *slotTable) observe(model string, busy []bool, phantomAfter time.Duration) {
	var releases []func()
	t.mu.Lock()
	ms := t.modelLocked(model, len(busy))
	now := t.now()
	if busy == nil {
		for i, ch := range ms.orphans {
			if ch != nil {
				close(ch)
				ms.orphans[i] = nil
			}
		}
		ms.busy = nil
		t.mu.Unlock()
		return
	}
	ms.busy = append(ms.busy[:0], busy...)
	ms.lastObserved = now
	for i := range ms.holders {
		isBusy := i < len(busy) && busy[i]
		if ch := ms.orphans[i]; ch != nil && !isBusy {
			close(ch)
			ms.orphans[i] = nil
		}
		h := ms.holders[i]
		if h == nil {
			continue
		}
		if isBusy {
			h.lastBusy = now
			continue
		}
		if phantomAfter > 0 && now.Sub(h.lastBusy) >= phantomAfter {
			h.released = true
			ms.holders[i] = nil
			if h.release != nil {
				releases = append(releases, h.release)
			}
		}
	}
	t.mu.Unlock()
	for _, r := range releases {
		go r()
	}
}

// ObserveSlots feeds one fresh upstream /slots reading for modelID: busy[i]
// is slot i's is_processing. nil = the model is not running. Called by the
// server's 1 Hz sessions poller; only readings the child actually answered
// may be passed (a stale copy would start the phantom clock on a slot that
// is busy but too busy to answer).
func (b *baseRouter) ObserveSlots(modelID string, busy []bool) {
	if _, ok := b.processes[modelID]; !ok {
		return
	}
	b.slots.observe(modelID, busy, b.slotPhantomAfter)
}

func (b *baseRouter) sendServeDone(ev scheduler.ServeDoneEvent) {
	select {
	case b.serveDoneCh <- ev:
	case <-b.shutdownCtx.Done():
	}
}

// sendServeDoneWhenSlotIdle holds a cut request's cap place until the
// upstream reports its slot idle, or the poller goes quiet (never strand the
// place on a reading that no longer comes).
func (b *baseRouter) sendServeDoneWhenSlotIdle(ev scheduler.ServeDoneEvent, idle <-chan struct{}) {
	check := time.NewTicker(slotObserveStale / 3)
	defer check.Stop()
	for {
		select {
		case <-idle:
			b.sendServeDone(ev)
			return
		case <-b.shutdownCtx.Done():
			return
		case <-check.C:
			if !b.slots.observedWithin(ev.ModelID, slotObserveStale) {
				b.sendServeDone(ev)
				return
			}
		}
	}
}

// bindSlot binds r (a granted request for modelID) to a slot of its own and
// rewrites its id_slot to that slot. nil when modelID does not use slot
// binding or r is not a JSON body.
func (b *baseRouter) bindSlot(modelID string, r *http.Request, release func()) *slotHolder {
	mc, ok := b.config.Models[modelID]
	if !ok || !mc.SlotAffinity || mc.ConcurrencyLimit <= 1 || r.Method != http.MethodPost ||
		!strings.Contains(r.Header.Get("Content-Type"), "application/json") || r.Body == nil {
		return nil
	}
	body, err := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil || !gjson.ValidBytes(body) {
		return nil
	}
	preferred := -1
	if v := gjson.GetBytes(body, "id_slot"); v.Exists() {
		preferred = int(v.Int())
	}
	h := b.slots.bind(modelID, mc.ConcurrencyLimit, preferred, release)
	if h == nil {
		return nil
	}
	if h.slot != preferred {
		if nb, err := sjson.SetBytes(body, "id_slot", h.slot); err == nil {
			body = nb
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			r.Header.Del("Transfer-Encoding")
			r.Header.Set("Content-Length", strconv.Itoa(len(body)))
		}
		// The session lane follows its request, so its next turn prefers
		// the slot that now holds its KV cache.
		if rebind, ok := swaputil.SlotReboundFromContext(r.Context()); ok {
			rebind(h.slot)
		}
	}
	// Attribution follows the binding, never the lane pin: GET /api/sessions
	// reads this key to join the row to the child's /slots.
	if setter, ok := swaputil.InflightMetadataSetterFromContext(r.Context()); ok {
		setter("slot_affinity", strconv.Itoa(h.slot))
	}
	return h
}
