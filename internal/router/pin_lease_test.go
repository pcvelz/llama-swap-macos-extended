package router

import (
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/process"
)

// These tests cover the lease-capable pin: a ttl > 0 pin must self-expire
// server-side (IsPinned goes false without any Unpin call), a permanent pin
// (ttl == 0, the pre-lease behavior) must never expire, and re-pinning an
// already-leased model must refresh its deadline rather than layering a
// second one.

func TestBaseRouter_Pin_PermanentNeverExpires(t *testing.T) {
	b := newTestBase(t, map[string]process.Process{}, &stubPlanner{})

	b.Pin("m")
	if !b.IsPinned("m") {
		t.Fatalf("IsPinned(m) = false, want true right after Pin")
	}

	deadline, pinned := b.PinExpiry("m")
	if !pinned {
		t.Fatalf("PinExpiry(m) pinned=false, want true")
	}
	if !deadline.IsZero() {
		t.Fatalf("PinExpiry(m) deadline=%v, want zero (permanent pin)", deadline)
	}
}

func TestBaseRouter_PinWithTTL_ExpiresWithoutUnpin(t *testing.T) {
	b := newTestBase(t, map[string]process.Process{}, &stubPlanner{})

	ttl := 30 * time.Millisecond
	deadline := b.PinWithTTL("m", ttl)
	if deadline.IsZero() {
		t.Fatalf("PinWithTTL returned zero deadline for ttl=%v, want non-zero", ttl)
	}
	if !b.IsPinned("m") {
		t.Fatalf("IsPinned(m) = false immediately after PinWithTTL, want true")
	}

	// Wait past the deadline. IsPinned must go false server-side with no
	// Unpin call - this is the crash-safety property: a client that dies
	// without unpinning must never wedge the box.
	time.Sleep(ttl + 50*time.Millisecond)

	if b.IsPinned("m") {
		t.Fatalf("IsPinned(m) = true after ttl elapsed, want false (lease must expire without Unpin)")
	}

	if _, pinned := b.PinExpiry("m"); pinned {
		t.Fatalf("PinExpiry(m) pinned=true after ttl elapsed, want false")
	}
}

func TestBaseRouter_PinWithTTL_RepinRefreshesDeadline(t *testing.T) {
	b := newTestBase(t, map[string]process.Process{}, &stubPlanner{})

	first := b.PinWithTTL("m", 20*time.Millisecond)
	if first.IsZero() {
		t.Fatalf("first PinWithTTL returned zero deadline")
	}

	// Refresh with a longer ttl before the first lease would expire.
	time.Sleep(10 * time.Millisecond)
	second := b.PinWithTTL("m", time.Hour)
	if !second.After(first) {
		t.Fatalf("refreshed deadline %v not after original %v", second, first)
	}

	// Past the ORIGINAL deadline, the model must still be pinned because the
	// refresh replaced it rather than leaving both in effect.
	time.Sleep(20 * time.Millisecond)
	if !b.IsPinned("m") {
		t.Fatalf("IsPinned(m) = false after refresh extended the deadline, want true")
	}
}

func TestBaseRouter_Unpin_RemovesLease(t *testing.T) {
	b := newTestBase(t, map[string]process.Process{}, &stubPlanner{})

	b.PinWithTTL("m", time.Hour)
	if !b.IsPinned("m") {
		t.Fatalf("IsPinned(m) = false right after PinWithTTL, want true")
	}

	b.Unpin("m")
	if b.IsPinned("m") {
		t.Fatalf("IsPinned(m) = true after explicit Unpin, want false")
	}
}
