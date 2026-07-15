// Copyright (c) Roman Atachiants and contributors. All rights reserved.
// Licensed under the MIT license. See LICENSE file in the project root for details.

package event

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Event represents an event contract
type Event interface {
	Type() uint32
}

// registry holds an immutable sorted array of event mappings
type registry struct {
	keys []uint32 // Event types (sorted)
	grps []any    // Corresponding subscribers
}

// ------------------------------------- Dispatcher -------------------------------------

// Dispatcher represents an event dispatcher.
type Dispatcher struct {
	subs     atomic.Pointer[registry] // Atomic pointer to immutable array
	done     chan struct{}            // Cancellation
	maxQueue int                      // Maximum queue size per consumer
	mu       sync.Mutex               // Only for writes (subscribe/unsubscribe)
}

// NewDispatcher creates a new dispatcher of events.
func NewDispatcher() *Dispatcher {
	return NewDispatcherConfig(50000)
}

// NewDispatcherConfig creates a new dispatcher with configurable max queue size
func NewDispatcherConfig(maxQueue int) *Dispatcher {
	d := &Dispatcher{
		done:     make(chan struct{}),
		maxQueue: maxQueue,
	}

	d.subs.Store(&registry{
		keys: make([]uint32, 0, 16),
		grps: make([]any, 0, 16),
	})
	return d
}

// Close closes the dispatcher
func (d *Dispatcher) Close() error {
	close(d.done)
	return nil
}

// isClosed returns whether the dispatcher is closed or not
func (d *Dispatcher) isClosed() bool {
	select {
	case <-d.done:
		return true
	default:
		return false
	}
}

// findGroup performs a lock-free binary search for the event type
func (d *Dispatcher) findGroup(eventType uint32) any {
	reg := d.subs.Load()
	keys := reg.keys

	// Inlined binary search for better cache locality
	left, right := 0, len(keys)
	for left < right {
		mid := left + (right-left)/2
		if keys[mid] < eventType {
			left = mid + 1
		} else {
			right = mid
		}
	}

	if left < len(keys) && keys[left] == eventType {
		return reg.grps[left]
	}
	return nil
}

// Subscribe subscribes to an event, the type of the event will be automatically
// inferred from the provided type. Must be constant for this to work.
func Subscribe[T Event](broker *Dispatcher, handler func(T)) context.CancelFunc {
	var event T
	return SubscribeTo(broker, event.Type(), handler)
}

// SubscribeTo subscribes to an event with the specified event type.
func SubscribeTo[T Event](broker *Dispatcher, eventType uint32, handler func(T)) context.CancelFunc {
	if broker.isClosed() {
		panic(errClosed)
	}

	broker.mu.Lock()
	defer broker.mu.Unlock()

	// Check if group already exists
	if existing := broker.findGroup(eventType); existing != nil {
		grp := groupOf[T](eventType, existing)
		sub := grp.Add(handler)
		return func() {
			grp.Del(sub)
		}
	}

	// Create new group
	grp := &group[T]{cond: sync.NewCond(new(sync.Mutex)), maxQueue: broker.maxQueue}
	sub := grp.Add(handler)

	// Copy-on-write: insert new entry in sorted position
	old := broker.subs.Load()
	idx := sort.Search(len(old.keys), func(i int) bool {
		return old.keys[i] >= eventType
	})

	// Create new arrays with space for one more element
	newKeys := make([]uint32, len(old.keys)+1)
	newGrps := make([]any, len(old.grps)+1)

	// Copy elements before insertion point
	copy(newKeys[:idx], old.keys[:idx])
	copy(newGrps[:idx], old.grps[:idx])

	// Insert new element
	newKeys[idx] = eventType
	newGrps[idx] = grp

	// Copy elements after insertion point
	copy(newKeys[idx+1:], old.keys[idx:])
	copy(newGrps[idx+1:], old.grps[idx:])

	// Atomically store the new registry (mutex ensures no concurrent writers)
	newReg := &registry{keys: newKeys, grps: newGrps}
	broker.subs.Store(newReg)

	return func() {
		grp.Del(sub)
	}
}

// Publish writes an event into the dispatcher
func Publish[T Event](broker *Dispatcher, ev T) {
	eventType := ev.Type()
	if sub := broker.findGroup(eventType); sub != nil {
		group := groupOf[T](eventType, sub)
		group.Broadcast(ev)
	}
}

// Count counts the number of subscribers, this is for testing only.
func (d *Dispatcher) count(eventType uint32) int {
	if group := d.findGroup(eventType); group != nil {
		return group.(interface{ Count() int }).Count()
	}
	return 0
}

// groupOf casts the subscriber group to the specified generic type
func groupOf[T Event](eventType uint32, subs any) *group[T] {
	if group, ok := subs.(*group[T]); ok {
		return group
	}

	panic(errConflict[T](eventType, subs))
}

// ------------------------------------- Subscriber -------------------------------------

// consumer represents a consumer with a message queue
type consumer[T Event] struct {
	queue []T  // Current work queue
	stop  bool // Stop signal
	// stuck marks a consumer that stayed at a FULL queue through an entire
	// bounded flow-control wait: its handler is presumed blocked (e.g. an SSE
	// subscriber writing to a dead TCP peer). Broadcast stops waiting on stuck
	// consumers and drop-oldests into their queue instead, so one dead peer
	// can never halt event publishing process-wide (2026-07-15 production
	// wedge: swap + TTL eviction frozen behind a full /api/events consumer).
	// Cleared by Listen the moment the consumer drains again.
	stuck bool
}

// Listen listens to the event queue and processes events
func (s *consumer[T]) Listen(c *sync.Cond, fn func(T)) {
	pending := make([]T, 0, 128)

	for {
		c.L.Lock()
		for len(s.queue) == 0 {
			switch {
			case s.stop:
				c.L.Unlock()
				return
			default:
				c.Wait()
			}
		}

		// Swap buffers and reset the current queue
		temp := s.queue
		s.queue = pending[:0]
		pending = temp
		s.stuck = false // it drained — eligible for lossless flow control again
		c.L.Unlock()

		// Outside of the critical section, process the work
		for _, event := range pending {
			fn(event)
		}

		// Notify potential publishers waiting due to backpressure
		c.Broadcast()
	}
}

// ------------------------------------- Subscriber Group -------------------------------------

// group represents a consumer group
type group[T Event] struct {
	cond     *sync.Cond
	subs     []*consumer[T]
	maxQueue int // Maximum queue size per consumer
}

// stuckWait bounds how long ONE Broadcast may stall in flow control waiting
// for a full consumer to drain. Long enough that a merely-slow consumer gets
// real backpressure (lossless delivery, the original design), short enough
// that a DEAD consumer (blocked handler, e.g. SSE write to a dead peer) can
// only ever stall publishing once — after the wait it is marked stuck and all
// later Broadcasts drop-oldest into its queue without waiting. WHY not 0: the
// pre-existing lossless contract for live consumers (TestBackpressure) must
// survive; WHY not unbounded: unbounded is the 2026-07-15 production wedge
// (swap/TTL machinery frozen 43+ minutes behind one full consumer).
const stuckWait = time.Second

// fullActive reports whether any NON-stuck consumer is at a full queue —
// only those are worth waiting on: a stuck one has no pending drain to wait for.
func (s *group[T]) fullActive() bool {
	for _, sub := range s.subs {
		if !sub.stuck && len(sub.queue) >= s.maxQueue {
			return true
		}
	}
	return false
}

// Broadcast sends an event to all consumers
func (s *group[T]) Broadcast(ev T) {
	s.cond.L.Lock()
	defer s.cond.L.Unlock()

	// Bounded backpressure: wait for full-but-draining consumers, but never
	// indefinitely. sync.Cond has no timed wait, so a one-shot timer issues
	// the wakeup that a stuck consumer would otherwise never send.
	if s.fullActive() {
		start := time.Now()
		timer := time.AfterFunc(stuckWait, func() {
			s.cond.L.Lock()
			s.cond.Broadcast()
			s.cond.L.Unlock()
		})
		for s.fullActive() && time.Since(start) < stuckWait {
			s.cond.Wait()
		}
		timer.Stop()

		// Anything still full after a whole stuckWait is presumed dead: mark
		// it so no future Broadcast waits on it (Listen clears the mark the
		// moment it drains).
		for _, sub := range s.subs {
			if len(sub.queue) >= s.maxQueue {
				sub.stuck = true
			}
		}
	}

	// Enqueue: lossless append for consumers with room; drop-oldest for a
	// full (stuck) consumer — its queue is bounded and its newest events win,
	// which is the right trade for state/metric streams read by a peer that
	// may never come back.
	for _, sub := range s.subs {
		if len(sub.queue) >= s.maxQueue {
			copy(sub.queue, sub.queue[1:])
			sub.queue[len(sub.queue)-1] = ev
		} else {
			sub.queue = append(sub.queue, ev)
		}
	}
	s.cond.Broadcast() // Wake consumers
}

// Add adds a subscriber to the list
func (s *group[T]) Add(handler func(T)) *consumer[T] {
	sub := &consumer[T]{
		queue: make([]T, 0, 64),
	}

	// Add the consumer to the list of active consumers
	s.cond.L.Lock()
	s.subs = append(s.subs, sub)
	s.cond.L.Unlock()

	// Start listening
	go sub.Listen(s.cond, handler)
	return sub
}

// Del removes a subscriber from the list
func (s *group[T]) Del(sub *consumer[T]) {
	s.cond.L.Lock()
	defer s.cond.L.Unlock()

	// Search and remove the subscriber
	sub.stop = true
	for i, v := range s.subs {
		if v == sub {
			copy(s.subs[i:], s.subs[i+1:])
			s.subs = s.subs[:len(s.subs)-1]
			break
		}
	}
}

// ------------------------------------- Debugging -------------------------------------

var errClosed = fmt.Errorf("event dispatcher is closed")

// Count returns the number of subscribers in this group
func (s *group[T]) Count() int {
	return len(s.subs)
}

// String returns string representation of the type
func (s *group[T]) String() string {
	typ := reflect.TypeOf(s).String()
	idx := strings.LastIndex(typ, "/")
	typ = typ[idx+1 : len(typ)-1]
	return typ
}

// errConflict returns a conflict message
func errConflict[T any](eventType uint32, existing any) string {
	var want T
	return fmt.Sprintf(
		"conflicting event type, want=<%T>, registered=<%s>, event=0x%v",
		want, existing, eventType,
	)
}
