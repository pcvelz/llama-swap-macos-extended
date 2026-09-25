package router

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
)

// These tests pin the prefill-stall verdict (llama-cm incident
// 2026-09-25-prefill-frozen-slot-never-reclaimed-phantom-holder-parks-free-slot).
//
// The witnessed shape: llama-server launched a task on one slot, its
// n_prompt_tokens_processed stopped about two thirds of the way through the
// prompt and never moved again, and NO existing guard reclaimed it - because
// llama-server sends an SSE comment ping (":\n\n") every 30s from the moment a
// task launches, and the proxy counted those as body bytes. The upstream stub
// below reproduces that: a /slots endpoint whose counter the test controls.

// slotsStub is a fake llama-server /slots whose answer the test sets.
type slotsStub struct {
	mu    sync.Mutex
	body  string
	fail  bool
	polls atomic.Int32
	srv   *httptest.Server
}

func newSlotsStub(t *testing.T, body string) *slotsStub {
	t.Helper()
	s := &slotsStub{body: body}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.polls.Add(1)
		s.mu.Lock()
		body, fail := s.body, s.fail
		s.mu.Unlock()
		if fail {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *slotsStub) set(body string) {
	s.mu.Lock()
	s.body = body
	s.mu.Unlock()
}

func (s *slotsStub) setFail(fail bool) {
	s.mu.Lock()
	s.fail = fail
	s.mu.Unlock()
}

// slotJSON renders one llama-server slot in the shape current builds return.
func slotJSON(id, task int, processing bool, nPrompt, processed, decoded int) string {
	return `{"id":` + strconv.Itoa(id) + `,"id_task":` + strconv.Itoa(task) +
		`,"is_processing":` + strconv.FormatBool(processing) +
		`,"n_prompt_tokens":` + strconv.Itoa(nPrompt) +
		`,"n_prompt_tokens_processed":` + strconv.Itoa(processed) +
		`,"next_token":[{"n_decoded":` + strconv.Itoa(decoded) + `}]}`
}

const (
	stuckTask   = 42
	stuckPrompt = 44000
	stuckAt     = 30000
)

// frozenSlots is the incident's /slots reading: slot 0 idle, slot 1 stuck
// mid-prefill.
func frozenSlots(processed int) string {
	return "[" + slotJSON(0, 7, false, 0, 0, 0) + "," + slotJSON(1, stuckTask, true, stuckPrompt, processed, 0) + "]"
}

// prefillHarness is one granted, pre-first-token request plus its watcher.
type prefillHarness struct {
	pw        *pingWriter
	w         *syncRecorder
	cancelled chan struct{}
	restarts  atomic.Int32
	watch     *prefillWatch
	remove    func()
}

func newPrefillHarness(t *testing.T, stub *slotsStub, budget, restartAfter time.Duration) *prefillHarness {
	t.Helper()
	origQuiet, origInterval := pingQuietDelay, pingInterval
	pingQuietDelay, pingInterval = 10*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { pingQuietDelay, pingInterval = origQuiet, origInterval })

	logger := logmon.NewWriter(io.Discard)
	h := &prefillHarness{w: newSyncRecorder(), cancelled: make(chan struct{})}

	// Slot-stall and peer-stall off: this suite is about the prefill verdict
	// alone.
	guard := newPeerStallGuard(logger, "prefill-model", 0, 0)
	var once atomic.Bool
	guard.setCancel(func() {
		if once.CompareAndSwap(false, true) {
			close(h.cancelled)
		}
	})
	h.pw = newPingWriter(logger, "prefill-model", h.w, true, guard)
	t.Cleanup(func() { h.pw.stop(); h.pw.waitLoop() })
	h.pw.slotGranted()

	h.watch = newPrefillWatch(logger, "prefill-model", stub.srv.URL, budget, restartAfter,
		func() { h.restarts.Add(1) })
	h.watch.pollInterval = 20 * time.Millisecond
	h.watch.pollTimeout = 200 * time.Millisecond
	h.remove = h.watch.add(h.pw, time.Now())
	t.Cleanup(h.remove)
	return h
}

// upstreamPing is what llama-server writes during a prefill: an SSE comment,
// every sse_ping_interval (30s) once the task has launched.
func (h *prefillHarness) upstreamPing(t *testing.T) {
	t.Helper()
	if _, err := h.pw.Write([]byte(":\n\n")); err != nil {
		t.Fatalf("upstream ping write: %v", err)
	}
}

func wasCancelled(ch <-chan struct{}, within time.Duration) bool {
	if within <= 0 {
		select {
		case <-ch:
			return true
		default:
			return false
		}
	}
	select {
	case <-ch:
		return true
	case <-time.After(within):
		return false
	}
}

// TestPrefillStall_FrozenPrefillIsReclaimed is the incident: the upstream prefill
// counter is flat, the only bytes the proxy sees are llama-server's own comment
// pings, and the request must be cut through the existing reclaim path.
func TestPrefillStall_FrozenPrefillIsReclaimed(t *testing.T) {
	const budget = 300 * time.Millisecond
	stub := newSlotsStub(t, frozenSlots(stuckAt))
	h := newPrefillHarness(t, stub, budget, 0)

	// Upstream keeps pinging for the whole budget - the thing that hid the
	// wedge from every byte-counting guard.
	deadline := time.Now().Add(budget + 600*time.Millisecond)
	for time.Now().Before(deadline) && !wasCancelled(h.cancelled, 0) {
		h.upstreamPing(t)
		time.Sleep(30 * time.Millisecond)
	}

	if !wasCancelled(h.cancelled, time.Second) {
		t.Fatalf("a prefill flat at %d/%d for > %s was never reclaimed", stuckAt, stuckPrompt, budget)
	}
	body := h.w.bodyString()
	if !strings.Contains(body, "event: error") || !strings.Contains(body, "prefill") {
		t.Fatalf("client was not told why: want an SSE error naming the prefill stall, got %q", body)
	}
	if !h.pw.stall.stalled() {
		t.Fatal("reclaim did not go through the shared fire() latch")
	}
}

// TestPrefillStall_SlowButAdvancingPrefillSurvives is the ruling's other half:
// a prefill that advances even one token per sample is slow, not stuck, and
// runs untouched for many budgets.
func TestPrefillStall_SlowButAdvancingPrefillSurvives(t *testing.T) {
	const budget = 200 * time.Millisecond
	stub := newSlotsStub(t, frozenSlots(1000))
	h := newPrefillHarness(t, stub, budget, 0)

	processed := 1000
	end := time.Now().Add(10 * budget)
	for time.Now().Before(end) {
		processed++
		stub.set(frozenSlots(processed))
		h.upstreamPing(t)
		time.Sleep(15 * time.Millisecond)
	}
	if wasCancelled(h.cancelled, 0) {
		t.Fatal("an advancing prefill was reclaimed - slow is not stuck")
	}
}

// TestPrefillStall_FailedPollsAreNoEvidence pins the /slots trap in
// peerstall.go's header: a loaded child may not answer /slots in time, and a
// poll that failed says nothing about progress. It must never be read as flat.
func TestPrefillStall_FailedPollsAreNoEvidence(t *testing.T) {
	const budget = 200 * time.Millisecond
	stub := newSlotsStub(t, frozenSlots(stuckAt))
	stub.setFail(true)
	h := newPrefillHarness(t, stub, budget, 0)

	time.Sleep(5 * budget)
	if wasCancelled(h.cancelled, 0) {
		t.Fatal("reclaimed on failed polls alone - no successful sample ever showed a flat counter")
	}
	if stub.polls.Load() == 0 {
		t.Fatal("watcher never polled /slots")
	}
}

// TestPrefillStall_StreamingRequestIsNotACandidate: once real tokens flowed,
// the request is past prefill and belongs to the slot-stall verdict.
func TestPrefillStall_StreamingRequestIsNotACandidate(t *testing.T) {
	const budget = 200 * time.Millisecond
	stub := newSlotsStub(t, frozenSlots(stuckAt))
	h := newPrefillHarness(t, stub, budget, 0)

	if _, err := h.pw.Write([]byte("event: content_block_delta\ndata: {\"x\":1}\n\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(4 * budget)
	if wasCancelled(h.cancelled, 0) {
		t.Fatal("a request that already streamed tokens was cut as a prefill stall")
	}
}

// TestPrefillStall_NewTaskResetsTheClock: a slot that moves to a new task has
// made progress, even if the new counter happens to read the same.
func TestPrefillStall_NewTaskResetsTheClock(t *testing.T) {
	const budget = 250 * time.Millisecond
	stub := newSlotsStub(t, frozenSlots(500))
	h := newPrefillHarness(t, stub, budget, 0)

	task := stuckTask
	end := time.Now().Add(6 * budget)
	for time.Now().Before(end) {
		task++
		stub.set("[" + slotJSON(1, task, true, stuckPrompt, 500, 0) + "]")
		time.Sleep(budget / 3)
	}
	if wasCancelled(h.cancelled, 0) {
		t.Fatal("a slot cycling through new tasks was read as one flat prefill")
	}
}

// TestPrefillStall_OtherSlotStillPrefillingDefers: while another slot of the
// same model is advancing its own prefill, a pre-first-token holder may be ITS
// owner, so the cut waits rather than risk ending a healthy request.
func TestPrefillStall_OtherSlotStillPrefillingDefers(t *testing.T) {
	const budget = 200 * time.Millisecond
	stub := newSlotsStub(t, frozenSlots(stuckAt))
	h := newPrefillHarness(t, stub, budget, 0)

	other := 100
	end := time.Now().Add(5 * budget)
	for time.Now().Before(end) {
		other += 50
		stub.set("[" + slotJSON(0, 90, true, 90000, other, 0) + "," + slotJSON(1, stuckTask, true, stuckPrompt, stuckAt, 0) + "]")
		time.Sleep(15 * time.Millisecond)
	}
	if wasCancelled(h.cancelled, 0) {
		t.Fatal("cut while another slot was still advancing a prefill the holder may own")
	}

	// Once the other slot stops being an ambiguity, the frozen slot is cut.
	stub.set(frozenSlots(stuckAt))
	if !wasCancelled(h.cancelled, 2*time.Second) {
		t.Fatal("frozen slot never reclaimed after the ambiguity cleared")
	}
}

// TestPrefillStall_EscalatesToRestartWhenUpstreamKeepsTheSlot is the backstop:
// the cut closes the owner's connection, which llama-server normally answers
// with a task cancel within its 1s poll. If the same task is still flat after
// restartAfter, the child is restarted - the only other lever that frees a
// busy slot (/slots?action=erase refuses a processing slot).
func TestPrefillStall_EscalatesToRestartWhenUpstreamKeepsTheSlot(t *testing.T) {
	const budget, restartAfter = 150 * time.Millisecond, 150 * time.Millisecond
	stub := newSlotsStub(t, frozenSlots(stuckAt))
	h := newPrefillHarness(t, stub, budget, restartAfter)

	if !wasCancelled(h.cancelled, 2*time.Second) {
		t.Fatal("frozen prefill never cut")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && h.restarts.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if h.restarts.Load() == 0 {
		t.Fatal("slot stayed flat after the cut and the child was never restarted")
	}
	time.Sleep(4 * restartAfter)
	if n := h.restarts.Load(); n != 1 {
		t.Fatalf("restart must fire once per stuck task, fired %d times", n)
	}
}

// TestPrefillStall_NoRestartWhenTheCutFreesTheSlot: the normal outcome. The
// cut lands, llama-server cancels the task, the slot goes idle - no restart.
func TestPrefillStall_NoRestartWhenTheCutFreesTheSlot(t *testing.T) {
	const budget, restartAfter = 150 * time.Millisecond, 150 * time.Millisecond
	stub := newSlotsStub(t, frozenSlots(stuckAt))
	h := newPrefillHarness(t, stub, budget, restartAfter)

	if !wasCancelled(h.cancelled, 2*time.Second) {
		t.Fatal("frozen prefill never cut")
	}
	stub.set("[" + slotJSON(0, 7, false, 0, 0, 0) + "," + slotJSON(1, stuckTask, false, stuckPrompt, stuckAt, 0) + "]")
	time.Sleep(5 * restartAfter)
	if n := h.restarts.Load(); n != 0 {
		t.Fatalf("restarted a child whose slot the cut already freed (%d)", n)
	}
}

// TestPrefillStall_LateGrantIsNotTheOwner: a request granted AFTER the slot
// went flat cannot own the stuck task; it is queued behind it upstream and is
// spared (the owner's cut frees the slot for it).
func TestPrefillStall_LateGrantIsNotTheOwner(t *testing.T) {
	const budget = 200 * time.Millisecond
	stub := newSlotsStub(t, frozenSlots(stuckAt))
	h := newPrefillHarness(t, stub, budget, 0)

	// Keep the watcher running on an unrelated holder while the slot is
	// already flat, then swap the harness request in as a late grant.
	keep := h.watch.add(nil, time.Now())
	defer keep()
	h.remove()
	time.Sleep(budget / 2)
	late := h.watch.add(h.pw, time.Now())
	defer late()

	time.Sleep(4 * budget)
	if wasCancelled(h.cancelled, 0) {
		t.Fatal("cut a request granted after the slot was already flat")
	}
}

// TestBaseRouter_PrefillStall_ReclaimsThroughServeHTTP is the wiring proof: a
// real ServeHTTP on an Anthropic stream, an upstream that behaves like
// llama-server during a stuck prefill (200 + SSE comment pings, never a token,
// until its connection is closed), and a /slots stub showing the flat counter.
// The request must be cut with the SSE error, its upstream context cancelled
// (which is what makes llama-server cancel the task), and the verdict logged.
func TestBaseRouter_PrefillStall_ReclaimsThroughServeHTTP(t *testing.T) {
	origPoll, origTimeout := prefillPollInterval, prefillPollTimeout
	prefillPollInterval, prefillPollTimeout = 50*time.Millisecond, 500*time.Millisecond
	t.Cleanup(func() { prefillPollInterval, prefillPollTimeout = origPoll, origTimeout })
	shortenPingCadence(t, 30*time.Millisecond)

	stub := newSlotsStub(t, frozenSlots(stuckAt))

	const model = "pm"
	proc := newFakeProcess(model)
	proc.markReady()
	upstreamClosed := make(chan struct{})
	proc.serveFunc = func(w http.ResponseWriter, r *http.Request, callNum int) bool {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-r.Context().Done():
				close(upstreamClosed)
				return true
			case <-tick.C:
				_, _ = io.WriteString(w, ":\n\n")
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
		}
	}

	sendLoading := true
	mc := config.ModelConfig{SendLoadingState: &sendLoading, Proxy: stub.srv.URL}
	logs := &syncBuf{}
	conf := config.Config{HealthCheckTimeout: 5, Models: map[string]config.ModelConfig{model: mc},
		PrefillStall: config.PrefillStallConfig{Enabled: true, TimeoutSeconds: 1}}
	b, err := newBaseRouter("test", conf, map[string]process.Process{model: proc}, logmon.NewWriter(logs), &stubPlanner{})
	if err != nil {
		t.Fatal(err)
	}
	go b.run()
	t.Cleanup(func() { _ = b.Shutdown(time.Second) })

	rec := newSyncRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.ServeHTTP(rec, buildParkReq(model, "", replayTierDefault, "/v1/messages", `{"model":"pm","stream":true}`))
	}()

	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatalf("request never reclaimed.\nlogs:\n%s", logs.String())
	}
	select {
	case <-upstreamClosed:
	default:
		t.Fatal("the upstream request context was never cancelled - llama-server would keep the slot")
	}
	if body := rec.bodyString(); !strings.Contains(body, "event: error") || !strings.Contains(body, "prefill") {
		t.Fatalf("client body = %q, want the prefill SSE error", body)
	}
	if got := logs.String(); !strings.Contains(got, "prefill-stalled: reclaimed slot") {
		t.Fatalf("verdict line missing.\nlogs:\n%s", got)
	}
}

// TestPrefillStall_UpstreamCommentPingIsNotAToken pins the counter underneath
// every verdict above: llama-server's ":\n\n" keepalive is not a token.
func TestPrefillStall_UpstreamCommentPingIsNotAToken(t *testing.T) {
	logger := logmon.NewWriter(io.Discard)
	pw := newPingWriter(logger, "m", newSyncRecorder(), true, nil)
	defer func() { pw.stop(); pw.waitLoop() }()

	_, _ = pw.Write([]byte(":\n\n"))
	_, _ = pw.Write([]byte(": ping\n\n"))
	if !pw.preFirstToken() {
		t.Fatal("an SSE comment ping was counted as a token")
	}
	_, _ = pw.Write([]byte("event: message_start\ndata: {}\n\n"))
	if pw.preFirstToken() {
		t.Fatal("a real SSE event was not counted")
	}
}

// TestPrefillStall_ParseSlots covers both /slots shapes and next_token as an
// object or a one-element array.
func TestPrefillStall_ParseSlots(t *testing.T) {
	for name, body := range map[string]string{
		"array":   frozenSlots(stuckAt),
		"wrapped": `{"slots":` + frozenSlots(stuckAt) + `}`,
		"object":  `[{"id":1,"id_task":42,"is_processing":true,"n_prompt_tokens":44000,"n_prompt_tokens_processed":30000,"next_token":{"n_decoded":0}}]`,
	} {
		slots, err := parsePrefillSlots([]byte(body))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var got *upstreamSlot
		for i := range slots {
			if slots[i].ID == 1 {
				got = &slots[i]
			}
		}
		if got == nil || got.IDTask != stuckTask || !got.Processing || got.Processed != stuckAt || got.NPrompt != stuckPrompt {
			t.Fatalf("%s: parsed %+v", name, slots)
		}
	}
	if _, err := parsePrefillSlots([]byte("not json")); err == nil {
		t.Fatal("garbage parsed as slots")
	}
}
