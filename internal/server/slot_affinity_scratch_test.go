package server

import (
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Housekeeping calls (bodies below the inject threshold) used to pass
// through with no id_slot at all, leaving the child to pick a slot by
// least-recently-used - which, with one interactive session idle between
// two turns, is that session's slot: its whole context was replaced by a
// ~1k-token prompt (witnessed live 2026-09-08 14:56, a 123k-token session on
// slot 0 lost to a 1032-token curl). Contract: a small body is pinned to the
// scratch slot, the one no active lane owns, and never to a lane's slot.

func TestSlotAffinityStore_ScratchSlotAvoidsActiveLanes(t *testing.T) {
	cfg := config.Config{Models: map[string]config.ModelConfig{
		"m": {SlotAffinity: true, ConcurrencyLimit: 2},
	}}
	s := newAffinityStore(cfg)

	// No lanes at all: the highest id is the scratch slot by convention.
	slot, ok := s.scratchSlot("m", 2)
	require.True(t, ok)
	assert.Equal(t, 1, slot)

	// One session on slot 1: scratch moves to slot 0.
	s.learn("m", affSessionA, 1)
	slot, ok = s.scratchSlot("m", 2)
	require.True(t, ok)
	assert.Equal(t, 0, slot)

	// Session on slot 0 instead: scratch is slot 1.
	s.learn("m", affSessionA, 0)
	slot, ok = s.scratchSlot("m", 2)
	require.True(t, ok)
	assert.Equal(t, 1, slot)

	// Single-slot child or a model that did not opt in: nothing to pin.
	_, ok = s.scratchSlot("m", 1)
	assert.False(t, ok)
	_, ok = s.scratchSlot("plain", 2)
	assert.False(t, ok)
}

func TestSlotAffinityMiddleware_SmallBodyPinnedToScratchSlot(t *testing.T) {
	cfg := config.Config{Models: map[string]config.ModelConfig{
		"m": {SlotAffinity: true, ConcurrencyLimit: 2},
	}}
	s := newSlotAffinityStore(cfg)
	defer s.Close()
	s.learn("m", affSessionA, 0)

	// The session's own housekeeping call: small body, same session id. It
	// must not ride into slot 0 (the session's lane) and must not be left
	// unpinned; it goes to slot 1.
	r := affinityRequest("m", affSessionA, "")
	body, data, _, called := runAffinityMiddleware(t, s, cfg, r)
	require.True(t, called)
	assert.Equal(t, int64(1), gjson.GetBytes(body, "id_slot").Int(), "housekeeping call pinned to the scratch slot")
	assert.Equal(t, "1", data.Metadata[slotAffinityMetadataKey])

	// An anonymous small request (no session at all) gets the same scratch slot.
	r = affinityRequest("m", "", "")
	body, _, _, called = runAffinityMiddleware(t, s, cfg, r)
	require.True(t, called)
	assert.Equal(t, int64(1), gjson.GetBytes(body, "id_slot").Int())

	// The session's full turn still rides its own lane.
	pad := `"pad":"` + strings.Repeat("x", 17000) + `"`
	r = affinityRequest("m", affSessionA, pad)
	body, _, _, called = runAffinityMiddleware(t, s, cfg, r)
	require.True(t, called)
	assert.Equal(t, int64(0), gjson.GetBytes(body, "id_slot").Int())

	// A small request never teaches the store a lane.
	assert.Equal(t, 1, s.sessionCount("m"))
}
