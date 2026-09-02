package scheduler

import (
	"io"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// Forced arrival-order replays.
//
// Two contracts stated in fifo.go are asserted here. Each case quotes the one
// it asserts in its `contract` field, and the failure message repeats it, so a
// later reader does not have to re-derive which promise a failure breaks:
//
//	fifo.go:189 (OnRequest step 2b, the rank barrier)
//	  "never grant an arrival while a strictly higher-rank request is still
//	   queued. This applies to every remaining branch below (fast-path serve,
//	   joining/starting a swap)"
//
//	fifo.go:708 (the enqueue doc comment)
//	  "tier rank DESC first ..., then the existing per-model priority (also
//	   DESC), then arrival (FIFO) order for ties on both keys"
//
// Both promises are about the ORDER requests arrived in, which no live load
// test can stage reliably - so these cases drive the run-loop methods directly,
// in a fixed order, with no goroutines, timers or sleeps anywhere. Every case
// is a deterministic replay: the same arrival sequence always produces the same
// queue and the same grants.
//
// Each case's wantQueue/wantServed/wantStarts are derived from the quoted
// contract ALONE, by walking the arrival sequence through OnRequest, enqueue
// and blockedByRankBarrier by hand. They are deliberately NOT derived from what
// the scheduler currently does, and must not be "fixed" by copying observed
// values back into the table - that would make the test agree with the code by
// construction and assert nothing.

// queuedModels returns the scheduler's queue as model IDs, head first.
func queuedModels(s *FIFO) []string {
	out := make([]string, len(s.queued))
	for i, q := range s.queued {
		out[i] = q.Model
	}
	return out
}

func equalOrder(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestFIFO_ArrivalOrderReplay(t *testing.T) {
	cases := []struct {
		name string
		// contract is the fifo.go site + quoted promise this case asserts,
		// repeated in every failure message.
		contract string
		// replay drives one fixed arrival sequence and returns the resulting
		// scheduler + recorded effects.
		replay func() (*FIFO, *fakeEffects)
		// wantQueue is the queue, head first, by model ID.
		wantQueue []string
		// wantServed is the number of received serve grants per model.
		wantServed map[string]int
		// wantStarts is the number of StartSwap calls per model.
		wantStarts map[string]int
	}{
		{
			// Control: nothing re-enters the queue, so the queue is exactly
			// arrival order. Proves the replay harness itself preserves order.
			name:     "collisions_queue_in_arrival_order",
			contract: "fifo.go:708 enqueue: \"arrival (FIFO) order for ties on both keys\"",
			replay: func() (*FIFO, *fakeEffects) {
				eff := newFakeEffects()
				for _, m := range []string{"z", "a", "b", "c"} {
					eff.states[m] = process.StateStopped
				}
				// z's swap evicts everything, so a/b/c all collide with it.
				planner := &stubPlanner{evict: map[string][]string{"z": {"a", "b", "c"}}}
				s := newFIFO(planner, eff)

				s.OnRequest(tierReq("z", swaputil.DefaultTier)) // starts the swap
				s.OnRequest(tierReq("a", swaputil.DefaultTier))
				s.OnRequest(tierReq("b", swaputil.DefaultTier))
				s.OnRequest(tierReq("c", swaputil.DefaultTier))
				return s, eff
			},
			wantQueue:  []string{"a", "b", "c"},
			wantServed: map[string]int{"a": 0, "b": 0, "c": 0},
			wantStarts: map[string]int{"z": 1},
		},
		{
			// A swap waiter that is bounced by the concurrency cap re-enters
			// through enqueue, which can only APPEND within its (rank,
			// priority) group - so it lands behind requests that arrived
			// after it.
			//
			// Arrivals, in order:
			//   1. A  model m, default tier -> starts m's swap
			//   2. B  model m, default tier -> joins m's swap as a waiter
			//   3. C  model n, default tier -> collides with m's swap, queues
			// Then m becomes ready. m's concurrency limit is 1: A is granted,
			// B is over the cap and goes back to the queue. B arrived before
			// C, so the queue must read [B(m), C(n)].
			// Finally A finishes, freeing m's only slot.
			// CONTRACT SITE: fifo.go:708 (enqueue) is the promise; fifo.go:352
			// (OnSwapDone) is the path that breaks it - a waiter over the
			// freshly-ready model's cap goes back through the ordinary
			// s.enqueue(), which has no record of when that waiter arrived.
			name: "requeued_swap_waiter_keeps_its_arrival_slot",
			contract: "fifo.go:708 enqueue: \"arrival (FIFO) order for ties on both keys\"" +
				" (broken via the fifo.go:352 OnSwapDone cap re-queue)",
			replay: func() (*FIFO, *fakeEffects) {
				eff := newFakeEffects()
				eff.states["m"] = process.StateStopped
				eff.states["n"] = process.StateStopped
				// n must evict m; m evicts nothing.
				planner := &stubPlanner{evict: map[string][]string{"n": {"m"}}}
				models := map[string]config.ModelConfig{
					"m": {ConcurrencyLimit: 1},
					"n": {ConcurrencyLimit: 1},
				}
				s := NewFIFO("test", logmon.NewWriter(io.Discard), planner, config.FifoConfig{}, models, eff)

				s.OnRequest(tierReq("m", swaputil.DefaultTier)) // (1) A: StartSwap(m)
				s.OnRequest(tierReq("m", swaputil.DefaultTier)) // (2) B: joins the swap
				s.OnRequest(tierReq("n", swaputil.DefaultTier)) // (3) C: collides -> queued

				eff.states["m"] = process.StateReady
				s.OnSwapDone(SwapDone{ModelID: "m"}) // A granted, B bounced by the cap

				s.OnServeDone(ServeDoneEvent{ModelID: "m"}) // A finishes, slot free
				return s, eff
			},
			// B (m) arrived first, so it takes the freed slot and C keeps
			// waiting - no swap away from m happens.
			wantQueue:  []string{"n"},
			wantServed: map[string]int{"m": 2, "n": 0},
			wantStarts: map[string]int{"m": 1, "n": 0},
		},
		{
			// The rank barrier is checked at step (2b) of OnRequest, AFTER
			// step (2) joins an in-flight swap for the same model - so an
			// arrival can be granted by joining a swap while a strictly
			// higher-rank request sits in the queue.
			//
			// Arrivals, in order:
			//   1. A  model m, default tier (rank 0)  -> starts m's swap
			//   2. D  model n, default tier (rank 0)  -> collides, queues
			//   3. S  model m, silent tier (rank -5)  -> must NOT be granted
			//         while D (rank 0) is queued; it belongs in the queue,
			//         behind D.
			// Then m becomes ready and A finishes. With S correctly deferred,
			// m is idle and D's swap can start.
			// CONTRACT SITE: fifo.go:189 (the rank barrier) claims to cover
			// "joining/starting a swap", but it is step (2b) and the join is
			// step (2) at fifo.go:180 - one step earlier.
			name: "swap_join_respects_the_rank_barrier",
			contract: "fifo.go:189 rank barrier: \"never grant an arrival while a strictly" +
				" higher-rank request is still queued ... (fast-path serve," +
				" joining/starting a swap)\"",
			replay: func() (*FIFO, *fakeEffects) {
				eff := newFakeEffects()
				eff.states["m"] = process.StateStopped
				eff.states["n"] = process.StateStopped
				planner := &stubPlanner{evict: map[string][]string{"n": {"m"}}}
				s := newFIFO(planner, eff)

				s.OnRequest(tierReq("m", swaputil.DefaultTier)) // (1) A: StartSwap(m)
				s.OnRequest(tierReq("n", swaputil.DefaultTier)) // (2) D: collides -> queued
				s.OnRequest(tierReq("m", tierSilent))           // (3) S: rank -5

				eff.states["m"] = process.StateReady
				s.OnSwapDone(SwapDone{ModelID: "m"})

				s.OnServeDone(ServeDoneEvent{ModelID: "m"}) // A finishes
				return s, eff
			},
			// Only A was ever granted on m; S waits behind D, and D - the
			// highest-rank queued request - gets its swap the moment m drains.
			wantQueue:  []string{"m"},
			wantServed: map[string]int{"m": 1, "n": 0},
			wantStarts: map[string]int{"m": 1, "n": 1},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, eff := tc.replay()

			if got := queuedModels(s); !equalOrder(got, tc.wantQueue) {
				t.Errorf("queue=%v want %v\n  contract: %s", got, tc.wantQueue, tc.contract)
			}
			for model, want := range tc.wantServed {
				if got := eff.served(model); got != want {
					t.Errorf("served(%s)=%d want %d\n  contract: %s", model, got, want, tc.contract)
				}
			}
			for model, want := range tc.wantStarts {
				if got := eff.startsFor(model); got != want {
					t.Errorf("StartSwap(%s)=%d want %d\n  contract: %s", model, got, want, tc.contract)
				}
			}
		})
	}
}
