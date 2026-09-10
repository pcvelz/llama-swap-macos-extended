package scheduler

import "github.com/mostlygeek/llama-swap/internal/swaputil"

// CapacityReporter is implemented by schedulers that can publish a lock-free
// snapshot of how their serving slots are occupied.
//
// It is deliberately NOT part of the Scheduler interface. Scheduler methods all
// run on the router's single run-loop goroutine, so a scheduler needs no
// internal locking; a capacity reader is the opposite case, called from HTTP
// handlers on arbitrary goroutines. Keeping it separate lets callers type-assert
// for it and leaves the many bare-fake Scheduler implementations in tests
// untouched.
type CapacityReporter interface {
	// Capacity returns the current per-model slot occupancy. Safe to call from
	// any goroutine.
	Capacity() []swaputil.ModelCapacity
}

// capacitySnapshot builds the value published by FIFO.publishCapacity. It reads
// scheduler state directly, so it must only be called on the run loop.
func (s *FIFO) capacitySnapshot() []swaputil.ModelCapacity {
	queued := make(map[string]int, len(s.queued))
	for _, q := range s.queued {
		queued[q.Model]++
	}

	// Union of every model that currently has occupancy or waiters. A model
	// sitting at zero on both counts is omitted rather than reported as an
	// idle row: absent means "nothing of ours is here", which is what a
	// consumer needs to distinguish from "here and empty".
	models := make(map[string]struct{}, len(s.inFlight)+len(queued))
	for m := range s.inFlight {
		models[m] = struct{}{}
	}
	for m := range queued {
		models[m] = struct{}{}
	}

	out := make([]swaputil.ModelCapacity, 0, len(models))
	for m := range models {
		out = append(out, swaputil.ModelCapacity{
			Model:   m,
			Granted: s.inFlight[m],
			Limit:   s.limit(m),
			Queued:  queued[m],
		})
	}
	return out
}

// publishCapacity stores a fresh snapshot for lock-free readers. Called from
// the run loop at every point that changes occupancy or the queue.
func (s *FIFO) publishCapacity() {
	snap := s.capacitySnapshot()
	s.capacity.Store(&snap)
}

// Capacity implements CapacityReporter.
func (s *FIFO) Capacity() []swaputil.ModelCapacity {
	if snap := s.capacity.Load(); snap != nil {
		return *snap
	}
	return nil
}
