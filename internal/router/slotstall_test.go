package router

import (
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/logmon"
)

// These tests exercise the no-forward-progress verdict at the pingWriter level
// rather than over a socket. That is deliberate and is the opposite call from
// peerstall_test.go: the peer verdict is about a kernel property (does a write
// actually block), which only a real connection can show, whereas THIS verdict
// is pure timing over a counter the writer owns. A socket would add
// nondeterminism and prove nothing extra.

// newSlotStallWriter builds a pingWriter with a slot-stall budget, a granted
// slot, and a cancel that reports through the returned channel.
func newSlotStallWriter(t *testing.T, slotBudget time.Duration) (*pingWriter, *syncRecorder, <-chan struct{}) {
	t.Helper()

	// Ping fast so the loop re-evaluates the budget promptly; the budget itself
	// is what the tests measure.
	origQuiet, origInterval := pingQuietDelay, pingInterval
	pingQuietDelay, pingInterval = 10*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { pingQuietDelay, pingInterval = origQuiet, origInterval })

	logger := logmon.NewWriter(io.Discard)
	w := newSyncRecorder()

	// peerTimeout 0: a recorder cannot carry a write deadline anyway, and this
	// keeps the two verdicts from being confused for one another.
	guard := newPeerStallGuard(logger, "slot-model", 0, slotBudget)

	cancelled := make(chan struct{})
	var once atomic.Bool
	guard.setCancel(func() {
		if once.CompareAndSwap(false, true) {
			close(cancelled)
		}
	})

	pw := newPingWriter(logger, "slot-model", w, true, guard)
	t.Cleanup(func() { pw.stop(); pw.waitLoop() })

	// The budget only runs against a request that HOLDS a slot.
	pw.slotGranted()
	return pw, w, cancelled
}

// TestSlotStall_WedgedRequestReclaimsSlot is the ruling: a request that produced
// tokens and then went completely flat gives its slot back, whatever wedged it.
func TestSlotStall_WedgedRequestReclaimsSlot(t *testing.T) {
	const budget = 300 * time.Millisecond

	pw, _, cancelled := newSlotStallWriter(t, budget)

	// One real token, then nothing ever again. This is the shape of a wedged
	// upstream, and of a client stall the peer verdict did not catch.
	if _, err := pw.Write([]byte("event: x\ndata: hi\n\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case <-cancelled:
	case <-time.After(budget + 5*time.Second):
		t.Fatal("flat request was not reclaimed: request context never cancelled")
	}
}

// TestSlotStall_WedgedRequestEndsStreamReadably pins CHANGE A: the mid-stream
// no-forward-progress verdict must give the client the same readable SSE
// error the zero-progress budget gives it (see
// TestSlotStall_ZeroBudgetAlsoFreesTheSlot below), not a bare chunked
// termination. httputil.ReverseProxy's ErrorHandler
// (internal/process/process_command.go newProxyErrorHandler) returns WITHOUT
// WRITING once swaputil.ResponseStarted(w) is true - guaranteed here, since
// pingsStarted already committed the 200 - so if this verdict does not write
// its own frame before cancelling, nothing downstream ever does.
func TestSlotStall_WedgedRequestEndsStreamReadably(t *testing.T) {
	const budget = 300 * time.Millisecond

	pw, w, cancelled := newSlotStallWriter(t, budget)

	if _, err := pw.Write([]byte("event: x\ndata: hi\n\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case <-cancelled:
	case <-time.After(budget + 5*time.Second):
		t.Fatal("flat request was not reclaimed: request context never cancelled")
	}

	if body := w.bodyString(); !strings.Contains(body, `"type":"error"`) {
		t.Errorf("mid-stream stall did not end with a readable SSE error event; body=%q", body)
	}
}

// TestSlotStall_SlowButAdvancingSurvives10xBudget is the companion the ruling
// demands. A thermally throttled box decoding at 5-10 tok/s writes every 100-200
// ms; here we write far slower still (a fifth of the budget apart) for TEN
// budgets, and it must not be touched. SLOW IS NOT STUCK.
//
// If this ever goes red, the guard has become a rate threshold and must be
// reverted, not tuned.
func TestSlotStall_SlowButAdvancingSurvives10xBudget(t *testing.T) {
	const budget = 300 * time.Millisecond

	pw, _, cancelled := newSlotStallWriter(t, budget)

	deadline := time.After(10 * budget)
	tick := time.NewTicker(budget / 5)
	defer tick.Stop()

	for {
		select {
		case <-cancelled:
			t.Fatal("a slow but advancing request was reclaimed; the guard has become a rate threshold")
		case <-deadline:
			return
		case <-tick.C:
			if _, err := pw.Write([]byte("event: x\ndata: tok\n\n")); err != nil {
				t.Fatalf("write: %v", err)
			}
		}
	}
}

// TestSlotStall_PrefillNotReaped pins PREFILL IS PROGRESS. A request that holds
// a slot and has produced NO byte yet may be legitimately prefilling (at 40
// tok/s a large context takes minutes with the byte counter flat), so this
// guard must stay inert until the first byte. That window belongs to
// pingZeroBudget, which is separately user-gated - so it is raised out of the
// way here to prove the SLOT guard specifically does not fire.
func TestSlotStall_PrefillNotReaped(t *testing.T) {
	const budget = 300 * time.Millisecond

	origZero := pingZeroBudget
	pingZeroBudget = time.Hour
	t.Cleanup(func() { pingZeroBudget = origZero })

	_, _, cancelled := newSlotStallWriter(t, budget)

	select {
	case <-cancelled:
		t.Fatal("a prefilling request was reclaimed before its first byte; prefill is progress")
	case <-time.After(6 * budget):
	}
}

// TestSlotStall_ZeroBudgetAlsoFreesTheSlot covers the gap the ruling named:
// pingZeroBudget used to end the CLIENT stream and leave the original request
// in flight, still holding the slot the client's retry then had to queue behind.
// Ending the stream must also give the slot back.
func TestSlotStall_ZeroBudgetAlsoFreesTheSlot(t *testing.T) {
	origZero := pingZeroBudget
	pingZeroBudget = 200 * time.Millisecond
	t.Cleanup(func() { pingZeroBudget = origZero })

	// slotBudget is irrelevant here - no byte is ever written, so the
	// no-progress guard is inert by construction and the zero budget is what
	// must fire.
	_, w, cancelled := newSlotStallWriter(t, time.Hour)

	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("zero-budget stream end did not release the slot")
	}

	// The client-visible half must be unchanged: it still gets the SSE error it
	// can parse and retry.
	if body := w.bodyString(); !strings.Contains(body, "event: error") {
		t.Fatalf("client did not get the SSE error event; body=%q", body)
	}
}

// TestSlotStall_DisabledLeavesFlatRequestAlone pins the config contract: with
// the budget at zero the flat request keeps its slot, so the feature is
// revertible from yaml alone.
func TestSlotStall_DisabledLeavesFlatRequestAlone(t *testing.T) {
	origZero := pingZeroBudget
	pingZeroBudget = time.Hour
	t.Cleanup(func() { pingZeroBudget = origZero })

	pw, _, cancelled := newSlotStallWriter(t, 0)

	if _, err := pw.Write([]byte("event: x\ndata: hi\n\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case <-cancelled:
		t.Fatal("guard fired with a zero budget; disabled must mean untouched")
	case <-time.After(2 * time.Second):
	}
}

// TestSlotStall_OneVerdictPerRequest pins the shared latch: whichever verdict
// notices first wins, and the other must not add a second, contradictory line
// to the log or cancel again.
func TestSlotStall_OneVerdictPerRequest(t *testing.T) {
	logger := logmon.NewWriter(io.Discard)
	guard := newPeerStallGuard(logger, "m", time.Second, time.Second)

	var cancels atomic.Int32
	guard.setCancel(func() { cancels.Add(1) })

	guard.reclaim(time.Second, nil)
	guard.reclaimStalledSlot(time.Second, nil)
	guard.reclaim(time.Second, nil)

	if got := cancels.Load(); got != 1 {
		t.Fatalf("cancel called %d times, want 1", got)
	}
}
