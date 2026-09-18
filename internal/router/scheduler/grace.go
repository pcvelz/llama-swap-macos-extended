package scheduler

import (
	"sort"
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
//
// When nothing cross-model is queued, the resident may still be idle inside
// its own grace - its slots and KV cache are hot and cooling exactly as they
// are with a waiter (2026-09-18: this used to render as nothing at all). In
// that case cooldownSnapshotIdle publishes the same EvicteeModel/
// RemainingSeconds with an empty NextModel and Waiting 0, so the menu can
// show "Cooldown: <resident> (m:ss)" with no "then/waiting" suffix and no
// swap to finish. This is a published-snapshot addition only: it changes
// nothing about scheduling, eviction or TTL.
func (s *FIFO) cooldownSnapshot() *swaputil.Cooldown {
	now := s.now()
	if out := s.cooldownSnapshotWaiting(now); out != nil {
		return out
	}
	return s.cooldownSnapshotIdle(now)
}

// cooldownSnapshotWaiting is the original queued-request walk: the first
// queued request whose eviction set contains a model inside its grace names
// the cooldown, nil when nothing queued is waiting on a cooling resident.
func (s *FIFO) cooldownSnapshotWaiting(now time.Time) *swaputil.Cooldown {
	if len(s.queued) == 0 {
		return nil
	}
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

// cooldownSnapshotIdle reports a no-waiter cooldown: a model that is idle
// inside its own grace with nothing cross-model queued behind it. This is
// the resident's own state, not a wait - EvicteeModel/RemainingSeconds only,
// NextModel/Waiting left at their zero values so the menu omits the
// "then <next> · N waiting" suffix and a click has nothing to finish
// (FinishCooldown is a documented no-op when nothing is queued, see OnTick).
//
// A model an in-flight swap is already evicting is excluded even if it is
// still nominally inside its own grace: that grace was just spent by
// deferredByGrace/FinishCooldown to let this exact swap start, and by the
// time this snapshot runs there is no waiter left in s.queued to say so
// (it moved into the active swap's waiters) - reporting a fresh cooldown for
// a model already on its way out would be a stale, self-contradicting read.
//
// Sorted by model ID for a deterministic pick on the (practically
// vanishingly rare) case that more than one model is idle inside its own
// grace at once - cooldownSnapshot is a pure observer and must return the
// same answer for the same state.
func (s *FIFO) cooldownSnapshotIdle(now time.Time) *swaputil.Cooldown {
	ids := make([]string, 0, len(s.idleSince))
	for id := range s.idleSince {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		g := s.grace[id]
		if g <= 0 {
			continue
		}
		if s.inFlight[id] > 0 {
			continue
		}
		if s.beingEvicted(id) {
			continue
		}
		remaining := g - now.Sub(s.idleSince[id])
		if remaining <= 0 {
			continue
		}
		return &swaputil.Cooldown{
			EvicteeModel:     id,
			RemainingSeconds: int(remaining.Round(time.Second) / time.Second),
		}
	}
	return nil
}

// beingEvicted reports whether model is in the evict set of any currently
// active swap - i.e. some in-flight swap is already replacing it, so it is
// no longer a candidate for a fresh no-waiter cooldown (see
// cooldownSnapshotIdle).
func (s *FIFO) beingEvicted(model string) bool {
	for _, sw := range s.active {
		if containsString(sw.evict, model) {
			return true
		}
	}
	return false
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
