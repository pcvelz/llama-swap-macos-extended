package router

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/tidwall/gjson"
)

// Incident llama-cm 2026-09-25-prefill-frozen-slot-never-reclaimed-phantom-
// holder-parks-free-slot, fault 2. Witnessed on :8001 (cq35, --parallel 2):
// slot 0 idle, slot 1 processing d02bb2dc's 44,401-token prompt; llama-swap
// showed a71c7fa7 ALSO in PREFILL on slot 1 with slot 1's numbers, and
// 22e288c8 PARKED(cap). Reproduced a second time after an evict with no
// freeze at all: two holders on slot 0, slot 1 idle, a third PARKED(cap).
//
// Mechanism: the slot-affinity middleware pins every full turn to its lane's
// id_slot at ARRIVAL, before the scheduler grants it, with the documented
// policy "busy slot: always inject the learned slot; llama.cpp queues the
// request". Three live sessions on two slots share lanes, so a granted
// request is sent to a slot another granted request is using; llama-server
// queues it behind that one while the other slot idles. The scheduler counts
// it as a holder, the cap is full, and the next request parks on "cap".
//
// Invariant: a granted request is BOUND to an upstream slot no other granted
// request holds; the id_slot on the wire is that binding, not the lane pin.

// slotUpstream is a fake two-slot llama-server that honours id_slot the way
// the id_slot-patched build does: a request for a busy slot waits in the
// server's own queue until that slot frees, whatever the other slot does.
type slotUpstream struct {
	mu       sync.Mutex
	cond     *sync.Cond
	busy     []string // tag holding each slot, "" = idle
	servedOn map[string]int
	queued   []string // "<tag> waited for slot N while slot M was idle"
	waitedOn int      // requests that ever waited for a busy slot upstream
	arrived  map[string]bool
	holds    map[string]chan struct{} // tag -> released when closed
	frozen   chan struct{}
}

func newSlotUpstream(n int) *slotUpstream {
	u := &slotUpstream{busy: make([]string, n), servedOn: map[string]int{}, arrived: map[string]bool{},
		holds: map[string]chan struct{}{}, frozen: make(chan struct{})}
	u.cond = sync.NewCond(&u.mu)
	return u
}

func (u *slotUpstream) idleSlotLocked() int {
	for i, t := range u.busy {
		if t == "" {
			return i
		}
	}
	return -1
}

// busySlots reports the upstream's own view, as /slots would.
func (u *slotUpstream) busySlots() []bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]bool, len(u.busy))
	for i, t := range u.busy {
		out[i] = t != ""
	}
	return out
}

// serve is a fakeProcess.serveFunc. Body tags: "tag" names the request,
// "frozen":true makes it hold its slot until u.frozen closes and keep
// processing after the client is gone (the witnessed id_task 4237 at
// 30390/44401 that a zero-output cut never freed).
func (u *slotUpstream) serve(w http.ResponseWriter, r *http.Request, _ int) bool {
	body, _ := io.ReadAll(r.Body)
	tag := gjson.GetBytes(body, "tag").String()
	frozen := gjson.GetBytes(body, "frozen").Bool()
	u.mu.Lock()
	u.arrived[tag] = true
	if gjson.GetBytes(body, "lost").Bool() {
		// The upstream accepted the connection but never runs the task:
		// no slot ever shows it (a phantom holder by construction).
		u.mu.Unlock()
		<-r.Context().Done()
		return true
	}
	slot := -1
	if v := gjson.GetBytes(body, "id_slot"); v.Exists() {
		slot = int(v.Int())
	}
	if slot < 0 {
		slot = u.idleSlotLocked()
	}
	if u.busy[slot] != "" {
		u.waitedOn++
	}
	for u.busy[slot] != "" {
		if free := u.idleSlotLocked(); free >= 0 {
			u.queued = append(u.queued, fmt.Sprintf("%s waited for slot %d (held by %s) while slot %d was idle", tag, slot, u.busy[slot], free))
		}
		if r.Context().Err() != nil {
			u.mu.Unlock()
			return true
		}
		stop := context.AfterFunc(r.Context(), func() { u.mu.Lock(); u.cond.Broadcast(); u.mu.Unlock() })
		u.cond.Wait()
		stop()
	}
	u.busy[slot] = tag
	u.servedOn[tag] = slot
	hold := u.holds[tag]
	u.mu.Unlock()

	if frozen {
		select {
		case <-u.frozen:
		case <-r.Context().Done():
			// The proxy returns on a cut, the upstream slot does NOT free:
			// exactly what the zero-output reclaim at 11:26:33 left behind.
			go func() {
				<-u.frozen
				u.mu.Lock()
				u.busy[slot] = ""
				u.cond.Broadcast()
				u.mu.Unlock()
			}()
			return true
		}
	}
	if hold != nil {
		select {
		case <-hold:
		case <-r.Context().Done():
		}
	}
	u.mu.Lock()
	u.busy[slot] = ""
	u.cond.Broadcast()
	u.mu.Unlock()
	if r.Context().Err() == nil {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok:" + tag))
	}
	return true
}

func (u *slotUpstream) snapshot() (servedOn map[string]int, queued []string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	servedOn = map[string]int{}
	for k, v := range u.servedOn {
		servedOn[k] = v
	}
	return servedOn, append([]string(nil), u.queued...)
}

func (u *slotUpstream) servedSlot(tag string) (int, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	s, ok := u.servedOn[tag]
	return s, ok
}

func (u *slotUpstream) waitServed(t *testing.T, tag string) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s, ok := u.servedSlot(tag); ok {
			return s
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, q := u.snapshot()
	t.Fatalf("%s never got an upstream slot within 2s; upstream queue: %v", tag, q)
	return -1
}

// pinnedRequest is a full turn as the affinity middleware hands it on: the
// lane's id_slot already in the body.
func pinnedRequest(ctx context.Context, model, tag string, laneSlot int, frozen bool) *http.Request {
	body := fmt.Sprintf(`{"model":%q,"tag":%q,"id_slot":%d,"frozen":%v}`, model, tag, laneSlot, frozen)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	return r
}

func newSlotBindingRouter(t *testing.T) (*baseRouter, *slotUpstream) {
	t.Helper()
	conf := config.Config{
		HealthCheckTimeout: 5,
		Models:             map[string]config.ModelConfig{"m": {ConcurrencyLimit: 2, SlotAffinity: true}},
	}
	up := newSlotUpstream(2)
	p := newFakeProcess("m")
	p.autoReady = true
	p.serveFunc = up.serve
	b := newTestBaseWithConfig(t, conf, map[string]process.Process{"m": p}, &stubPlanner{})
	t.Cleanup(func() {
		select {
		case <-up.frozen:
		default:
			close(up.frozen)
		}
	})
	return b, up
}

func serveAsync(b *baseRouter, r *http.Request) (*httptest.ResponseRecorder, chan struct{}) {
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		b.ServeHTTP(w, r)
		close(done)
	}()
	return w, done
}

func waitDone(t *testing.T, ch <-chan struct{}, what string, up *slotUpstream) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		_, q := up.snapshot()
		t.Fatalf("%s did not complete within 2s while an upstream slot was idle; upstream queue: %v", what, q)
	}
}

// The witnessed sequence, replayed: A (d02bb2dc) holds slot 1 frozen in
// prefill; B (a71c7fa7) and then C (22e288c8) arrive pinned to lane slot 1.
// Slot 0 is idle the whole time. B must run on slot 0 and finish, and C must
// then be admitted and run instead of parking on "cap" behind a phantom.
func TestSlotBinding_WitnessedSequenceNoPhantomHolder(t *testing.T) {
	b, up := newSlotBindingRouter(t)
	ctx := context.Background()

	_, aDone := serveAsync(b, pinnedRequest(ctx, "m", "A", 1, true))
	if s := up.waitServed(t, "A"); s != 1 {
		t.Fatalf("setup: A should hold slot 1, got %d", s)
	}

	wB, bDone := serveAsync(b, pinnedRequest(ctx, "m", "B", 1, false))
	waitDone(t, bDone, "B (a71c7fa7, lane pinned to the frozen slot 1)", up)
	served, queued := up.snapshot()
	if served["B"] != 0 || len(queued) > 0 {
		t.Fatalf("B was attributed to slot %d (upstream queue %v): a granted request must be bound to the idle slot 0, never to slot 1 held by A", served["B"], queued)
	}
	if !strings.Contains(wB.Body.String(), "ok:B") {
		t.Fatalf("B not served: %q", wB.Body.String())
	}

	_, cDone := serveAsync(b, pinnedRequest(ctx, "m", "C", 1, false))
	waitDone(t, cDone, "C (22e288c8)", up)
	if served, _ := up.snapshot(); served["C"] != 0 {
		t.Fatalf("C ran on slot %d, want the free slot 0", served["C"])
	}
	select {
	case <-aDone:
		t.Fatalf("A is frozen and must still be in flight")
	default:
	}
}

func lostRequest(ctx context.Context, model, tag string, laneSlot int) *http.Request {
	body := fmt.Sprintf(`{"model":%q,"tag":%q,"id_slot":%d,"lost":true}`, model, tag, laneSlot)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	return r
}

func (u *slotUpstream) holdTag(tag string) chan struct{} {
	ch := make(chan struct{})
	u.mu.Lock()
	u.holds[tag] = ch
	u.mu.Unlock()
	return ch
}

func (u *slotUpstream) hasArrived(tag string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.arrived[tag]
}

// observeFor feeds the upstream's own busy map to the router, as the 1 Hz
// /slots poller does, until stop closes.
func observeFor(b *baseRouter, up *slotUpstream, stop <-chan struct{}) {
	go func() {
		t := time.NewTicker(5 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				b.ObserveSlots("m", up.busySlots())
			}
		}
	}()
}

// Invariant: two granted requests never map to one upstream slot. Twenty
// turns, every one pinned to lane slot 0, pass a cap of 2: none may ever wait
// in the upstream's queue behind another granted request.
func TestSlotBinding_TwoHoldersNeverShareOneUpstreamSlot(t *testing.T) {
	b, up := newSlotBindingRouter(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		// Each turn holds its slot for a moment, so turns overlap.
		release := up.holdTag(fmt.Sprintf("r%d", i))
		go func() { time.Sleep(time.Duration(10+i) * time.Millisecond); close(release) }()
		go func(i int) {
			defer wg.Done()
			b.ServeHTTP(httptest.NewRecorder(), pinnedRequest(context.Background(), "m", fmt.Sprintf("r%d", i), 0, false))
		}(i)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	waitDone(t, done, "20 turns pinned to slot 0", up)
	up.mu.Lock()
	waited := up.waitedOn
	up.mu.Unlock()
	if waited != 0 {
		t.Fatalf("%d granted requests waited upstream for a slot another granted request held", waited)
	}
}

// A holder CUT by llama-swap (zero-output reclaim, preemption, client gone)
// whose upstream slot keeps processing still occupies that slot: the cap
// must not admit a request into a slot that is not free. Witnessed: the
// 11:26:33 reclaim cut a request while the upstream kept id_task 4237 busy.
func TestSlotBinding_CutHolderWhoseSlotStaysBusyStillOccupiesIt(t *testing.T) {
	b, up := newSlotBindingRouter(t)
	stopObs := make(chan struct{})
	defer close(stopObs)
	observeFor(b, up, stopObs)

	aCtx, cutA := context.WithCancel(context.Background())
	_, aDone := serveAsync(b, pinnedRequest(aCtx, "m", "A", 1, true))
	up.waitServed(t, "A")
	releaseB := up.holdTag("B")
	_, bDone := serveAsync(b, pinnedRequest(context.Background(), "m", "B", 0, false))
	up.waitServed(t, "B")

	cutA()
	waitDone(t, aDone, "the cut request A", up)

	_, cDone := serveAsync(b, pinnedRequest(context.Background(), "m", "C", 1, false))
	time.Sleep(200 * time.Millisecond)
	if up.hasArrived("C") {
		s, q := up.snapshot()
		t.Fatalf("C was sent upstream while slot 1 is still busy with the cut A and slot 0 with B (served %v, queue %v): a cut holder whose slot stays busy must keep its place", s, q)
	}

	close(releaseB)
	waitDone(t, bDone, "B", up)
	waitDone(t, cDone, "C after B freed slot 0", up)
	if s, _ := up.servedSlot("C"); s != 0 {
		t.Fatalf("C ran on slot %d, want 0 (slot 1 is still the frozen A's)", s)
	}
}

// A granted request bound to a slot the upstream keeps showing idle is a
// phantom: after slotPhantomAfter its cap place and its slot are released and
// the parked request gets them.
func TestSlotBinding_PhantomHolderIsReleasedAndParkedRequestRuns(t *testing.T) {
	b, up := newSlotBindingRouter(t)
	b.slotPhantomAfter = 100 * time.Millisecond
	stopObs := make(chan struct{})
	defer close(stopObs)
	observeFor(b, up, stopObs)

	pCtx, cancelP := context.WithCancel(context.Background())
	defer cancelP()
	_, _ = serveAsync(b, lostRequest(pCtx, "m", "P", 0))
	releaseQ := up.holdTag("Q")
	defer close(releaseQ)
	_, _ = serveAsync(b, pinnedRequest(context.Background(), "m", "Q", 1, false))
	up.waitServed(t, "Q")

	_, rDone := serveAsync(b, pinnedRequest(context.Background(), "m", "R", 1, false))
	waitDone(t, rDone, "R, parked behind the phantom P", up)
	if s, _ := up.servedSlot("R"); s != 0 {
		t.Fatalf("R ran on slot %d, want the phantom's idle slot 0", s)
	}
}
