package scheduler

import (
	"time"

	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// CooldownReporter is implemented by schedulers that can publish a lock-free
// snapshot of the current swap-grace cooldown, and let an operator end it.
// Kept separate from Scheduler for the same reason as CapacityReporter (see
// its doc comment in capacity.go): Scheduler methods all run on the router's
// single run-loop goroutine, while a CooldownReporter is called from HTTP/SSE
// handlers on arbitrary goroutines.
type CooldownReporter interface {
	// Cooldown returns the current cooldown: the resident model that is idle
	// inside its swap-grace while at least one queued request for another
	// model waits for it. Nil when nothing is held. Safe to call from any
	// goroutine.
	Cooldown() *swaputil.Cooldown

	// FinishCooldown manually ends the current cooldown, letting the queued
	// swap proceed at the next scheduling pass (normally the next OnTick,
	// within ~1s) instead of waiting out the resident's remaining grace. A
	// no-op if nothing is held. Safe to call from any goroutine.
	FinishCooldown()
}

// cooldownSnapshot builds the value published by FIFO.publishGrace. It reads
// scheduler state directly, so it must only be called on the run loop — the
// same constraint capacitySnapshot documents.
//
// It deliberately duplicates the idle-streak comparison from withinGrace
// rather than calling it: withinGrace (via deferredByGrace) also drains the
// one-shot manual finish and records the starvation-valve reference, both of
// which are decisions a snapshot read must never make as a side effect of
// merely being observed.
//
// ONE cooldown, on the resident: the queue is walked in order, the first
// queued request whose eviction set contains a model inside its grace names
// the resident (EvicteeModel) and what loads next (NextModel); every further
// queued request held by that same resident only adds to Waiting. Two models
// queued behind one cooling resident are one cooldown, not two
// (2026-09-10). Slots is left for the server to fill in: which session each
// slot is kept warm for is slot-affinity state, not scheduler state.
func (s *FIFO) cooldownSnapshot() *swaputil.Cooldown {
	if len(s.queued) == 0 {
		return nil
	}
	now := s.now()
	var out *swaputil.Cooldown
	for _, q := range s.queued {
		running := s.runningSet(q.Model)
		evict := s.planner.EvictionFor(q.Model, running)
		for _, ev := range evict {
			g := s.grace[ev]
			if g <= 0 {
				continue
			}
			since, ok := s.idleSince[ev]
			if !ok {
				continue
			}
			if s.inFlight[ev] > 0 {
				// Busy, not cooling: the in-flight check holds this request,
				// and the cooldown only starts when the resident drains.
				continue
			}
			remaining := g - now.Sub(since)
			if remaining <= 0 {
				continue
			}
			if out == nil {
				// Starvation valve: report the earlier of the resident's own
				// grace elapsing and the valve threshold opening, mirroring
				// the two ways withinGrace can let the hold end. starvationOff
				// means the valve never opens, so the resident's own grace is
				// the only deadline that matters.
				if !s.starvationOff && !s.cooldownWaitSince.IsZero() {
					threshold := g
					if s.starvation > 0 {
						threshold = s.starvation
					}
					if valveRemaining := threshold - now.Sub(s.cooldownWaitSince); valveRemaining < remaining {
						remaining = valveRemaining
					}
				}
				if remaining < 0 {
					remaining = 0
				}
				out = &swaputil.Cooldown{
					EvicteeModel:     ev,
					NextModel:        q.Model,
					RemainingSeconds: int(remaining.Round(time.Second) / time.Second),
				}
			}
			if out.EvicteeModel == ev {
				out.Waiting++
			}
			break
		}
	}
	return out
}

// publishGrace stores a fresh cooldown snapshot for lock-free readers. Called
// from the run loop at every point that changes the queue or idle
// bookkeeping — the same call sites as publishCapacity.
func (s *FIFO) publishGrace() {
	s.cooldown.Store(s.cooldownSnapshot())
}

// Cooldown implements CooldownReporter.
func (s *FIFO) Cooldown() *swaputil.Cooldown {
	if snap := s.cooldown.Load(); snap != nil {
		c := *snap
		return &c
	}
	return nil
}
