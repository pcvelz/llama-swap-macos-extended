package event

import (
	"sync/atomic"
	"testing"
	"time"
)

// Regression test for the 2026-07-15 production wedge (llama-cm incident
// 2026-07-07-llamaswap-drain-wedge-empty-200): group.Broadcast blocked forever
// in cond.Wait while ONE consumer's queue was full — a dead/slow /api/events
// SSE subscriber halted ALL event publishing process-wide, freezing the model
// swap and TTL-eviction machinery (witnessed via SIGQUIT goroutine dump:
// publishers parked in Broadcast backpressure for 43-49 minutes).
//
// Contract under test:
//  1. Publish must NEVER block indefinitely because one consumer stopped
//     draining. A bounded flow-control stall (~stuckWait, once) is acceptable.
//  2. Healthy consumers keep receiving every event (lossless for live
//     consumers — the existing TestBackpressure contract stays intact).
//  3. A consumer that resumes draining recovers (stuck marking clears).
func TestPublishNotWedgedByStuckConsumer(t *testing.T) {
	d := NewDispatcherConfig(4) // tiny per-consumer queue so the wedge arms fast
	defer d.Close()

	// Stuck consumer: blocks forever inside its handler (dead SSE peer analogue).
	block := make(chan struct{})
	unsubStuck := Subscribe(d, func(ev MyEvent1) { <-block })
	defer unsubStuck()

	// Healthy consumer: drains normally, counts everything it sees.
	var got atomic.Int64
	unsubOK := Subscribe(d, func(ev MyEvent1) { got.Add(1) })
	defer unsubOK()

	// Publish well past the stuck consumer's queue capacity. Before the fix
	// this goroutine parks forever in group.Broadcast's backpressure wait.
	const events = 50
	done := make(chan struct{})
	go func() {
		for i := 1; i <= events; i++ {
			Publish(d, MyEvent1{Number: i})
		}
		close(done)
	}()

	select {
	case <-done:
		// publishing survived a stuck consumer — the invariant this test exists for
	case <-time.After(10 * time.Second):
		t.Fatal("Publish wedged: a single stuck consumer blocked Broadcast for all (global backpressure deadlock)")
	}

	// The healthy consumer must still have received every event.
	deadline := time.Now().Add(5 * time.Second)
	for got.Load() < events && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got.Load() != events {
		t.Fatalf("healthy consumer starved: got %d/%d events", got.Load(), events)
	}

	// Recovery: unblock the stuck consumer; it must drain what it still holds
	// (bounded by its queue) and the dispatcher must keep working end-to-end.
	close(block)
	time.Sleep(200 * time.Millisecond)
	before := got.Load()
	Publish(d, MyEvent1{Number: events + 1})
	deadline = time.Now().Add(2 * time.Second)
	for got.Load() != before+1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got.Load() != before+1 {
		t.Fatalf("dispatcher did not recover after stuck consumer resumed (got %d, want %d)", got.Load(), before+1)
	}
}
