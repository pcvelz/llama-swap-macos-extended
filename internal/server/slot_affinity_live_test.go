package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Incident llama-cm 2026-09-16-two-live-sessions-pinned-same-slot-cache-thrash:
// two cq35h Claude Code sessions (5cee4df5, 14058186) both carried
// slot_affinity=0 while slot 1 sat idle, and every turn re-prefilled 5k-44k
// tokens because each switch on slot 0 threw away the other conversation's
// KV prefix. assign() counted every session seen in the last 15 min and broke
// the 1:1 tie (live 5cee4df5 on 0, 7b235539 quiet since 4 min on 1) to the
// lowest slot id; nothing ever moved a colliding session afterwards.
//
// Invariant: two sessions live at the same time never share a slot while a
// slot of the same child has no live session.

// A slot whose only session went quiet is free for a newcomer; the slot a
// live session is using is not - even though both sessions sit inside the
// old 15-minute window (the exact live shape of the incident).
func TestSlotAffinityStore_AssignAvoidsTheLiveSessionsSlot(t *testing.T) {
	s := newAffinityStore(affinityConfig())
	now := time.Unix(10_000, 0)
	s.now = func() time.Time { return now }

	slot, ok := s.assign("m", "live-A", 2)
	require.True(t, ok)
	require.Equal(t, 0, slot)
	slot, ok = s.assign("m", "quiet-B", 2)
	require.True(t, ok)
	require.Equal(t, 1, slot)

	// 4 minutes on: A is still in its tool loop, B has not spoken since.
	now = now.Add(4 * time.Minute)
	_, ok = s.lookup("m", "live-A")
	require.True(t, ok)

	slot, ok = s.assign("m", "new-C", 2)
	require.True(t, ok)
	assert.Equal(t, 1, slot, "a newcomer must take the slot no live session is using, not tie onto the live session's slot")
}

// Two sessions already pinned to one slot (e.g. assigned before this rule
// existed, or both live when every slot was taken) while the other slot has
// no live session: the NEWER session moves - one re-prefill once - and the
// older one keeps its slot and its cache.
func TestSlotAffinityMiddleware_RepairsTwoLiveSessionsOnOneSlot(t *testing.T) {
	cfg := affinityConfig()
	mc := cfg.Models["m"]
	mc.ConcurrencyLimit = 2
	cfg.Models["m"] = mc
	s := newAffinityStore(cfg)
	now := time.Unix(20_000, 0)
	s.now = func() time.Time { return now }

	s.learn("m", affSessionA, 0) // older session
	now = now.Add(time.Minute)
	s.learn("m", affSessionB, 0) // newer session, same slot
	now = now.Add(10 * time.Second)

	// The older session's turn: it keeps slot 0.
	body, _, _, called := runAffinityMiddleware(t, s, cfg, affinityRequest("m", affSessionA, ""))
	require.True(t, called)
	assert.Equal(t, int64(0), gjson.GetBytes(body, "id_slot").Int(), "the older session keeps its slot")

	// The newer session's turn, seconds later: slot 0 has a live session
	// that was there first, slot 1 has none -> move to slot 1.
	now = now.Add(3 * time.Second)
	body, _, _, called = runAffinityMiddleware(t, s, cfg, affinityRequest("m", affSessionB, ""))
	require.True(t, called)
	assert.Equal(t, int64(1), gjson.GetBytes(body, "id_slot").Int(), "the newer of two live sessions sharing a slot must move to the free slot")

	// And it stays there.
	now = now.Add(3 * time.Second)
	body, _, _, _ = runAffinityMiddleware(t, s, cfg, affinityRequest("m", affSessionB, ""))
	assert.Equal(t, int64(1), gjson.GetBytes(body, "id_slot").Int())
	body, _, _, _ = runAffinityMiddleware(t, s, cfg, affinityRequest("m", affSessionA, ""))
	assert.Equal(t, int64(0), gjson.GetBytes(body, "id_slot").Int())
}

// When every slot has a live session there is nowhere better to go: no move,
// no ping-pong.
func TestSlotAffinityMiddleware_NoRepairWhenEverySlotIsLive(t *testing.T) {
	cfg := affinityConfig()
	mc := cfg.Models["m"]
	mc.ConcurrencyLimit = 2
	cfg.Models["m"] = mc
	s := newAffinityStore(cfg)
	now := time.Unix(30_000, 0)
	s.now = func() time.Time { return now }

	s.learn("m", affSessionA, 0)
	now = now.Add(time.Second)
	s.learn("m", affSessionB, 0)
	now = now.Add(time.Second)
	s.learn("m", "third-live-C", 1)
	now = now.Add(time.Second)

	body, _, _, _ := runAffinityMiddleware(t, s, cfg, affinityRequest("m", affSessionB, ""))
	assert.Equal(t, int64(0), gjson.GetBytes(body, "id_slot").Int(), "slot 1 is live too: B has nowhere better to go")
}

// A request that is still streaming keeps its session live however long ago
// it started: a 5-minute decode is not a quiet session.
func TestSlotAffinityMiddleware_InFlightRequestKeepsItsSessionLive(t *testing.T) {
	cfg := affinityConfig()
	mc := cfg.Models["m"]
	mc.ConcurrencyLimit = 2
	cfg.Models["m"] = mc
	s := newAffinityStore(cfg)
	now := time.Unix(40_000, 0)
	s.now = func() time.Time { return now }

	s.learn("m", "quiet-Q", 1)
	now = now.Add(time.Second)

	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Downstream stands in for a long streaming turn: it holds the
		// request open until the test releases it.
		downstream := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			close(started)
			<-release
		})
		CreateSlotAffinityMiddleware(s, cfg)(downstream).ServeHTTP(httptest.NewRecorder(), affinityRequest("m", affSessionA, ""))
	}()
	<-started

	// A has been decoding for 6 minutes, Q has been quiet as long.
	now = now.Add(6 * time.Minute)
	slot, ok := s.assign("m", "new-C", 2)
	require.True(t, ok)
	close(release)
	<-done

	aSlot, _ := s.lookup("m", affSessionA)
	assert.NotEqual(t, aSlot, slot, "a session with a request in flight is live: a newcomer must not be pinned onto its slot")
}
