package server

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/stretchr/testify/assert"
)

// A resident alias request that lands while NOTHING is resident or starting
// used to be refused with a 0 ms 404. Witnessed 2026-09-08 19:13:46, one
// second after a llama-swap restart: the parent session's own cq35h turn
// triggered the load and waited; its subagent's claude-haiku-* turn got
// `no router for requested model ... resident: none` and Claude Code killed
// the subagent (model_not_found is fatal to it, no retry). llama-cm incident
// 2026-09-08-subagent-turns-invisible-share-parent-slot-lane, follow-up
// "alias grace".
//
// Contract: the alias handler waits up to a grace window for a model to
// become resident or start loading before it refuses. A hiccup of a few
// seconds is absorbed; a genuinely idle box still gets its 404 once the
// window closes; a client that gives up stops the wait immediately. Nothing
// is ever loaded by the wait itself.

func TestAwaitResidentAlias_ResolvesOnceAModelStartsWithinGrace(t *testing.T) {
	cfg := residentAliasTestConfig(t)
	var polls int32
	running := func() map[string]process.ProcessState {
		// Nothing for the first three polls, then a process starting.
		if atomic.AddInt32(&polls, 1) <= 3 {
			return map[string]process.ProcessState{}
		}
		return map[string]process.ProcessState{"modelA": process.StateStarting}
	}

	start := time.Now()
	resolved, ok := awaitResidentAlias(context.Background(), cfg, running, "claude-haiku-4-5-20251001",
		2*time.Second, 10*time.Millisecond)
	assert.True(t, ok, "a model that appears inside the grace window must be resolved, not refused")
	assert.Equal(t, "modelA", resolved)
	assert.Less(t, time.Since(start), time.Second, "must return as soon as the model appears, not at the end of the window")
}

func TestAwaitResidentAlias_RefusesAfterGraceWithNothingResident(t *testing.T) {
	cfg := residentAliasTestConfig(t)
	running := func() map[string]process.ProcessState { return map[string]process.ProcessState{} }

	start := time.Now()
	_, ok := awaitResidentAlias(context.Background(), cfg, running, "default", 60*time.Millisecond, 10*time.Millisecond)
	assert.False(t, ok, "an idle box must still 404 once the window closes - the wait never loads anything")
	assert.GreaterOrEqual(t, time.Since(start), 60*time.Millisecond, "the whole window must be given before refusing")
}

func TestAwaitResidentAlias_ClientCancelStopsTheWait(t *testing.T) {
	cfg := residentAliasTestConfig(t)
	running := func() map[string]process.ProcessState { return map[string]process.ProcessState{} }
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()

	start := time.Now()
	_, ok := awaitResidentAlias(ctx, cfg, running, "default", 10*time.Second, 10*time.Millisecond)
	assert.False(t, ok)
	assert.Less(t, time.Since(start), time.Second, "a departed client must not keep the handler parked for the full window")
}

func TestAwaitResidentAlias_ZeroGraceIsTheOldImmediateAnswer(t *testing.T) {
	cfg := residentAliasTestConfig(t)
	var polls int32
	running := func() map[string]process.ProcessState {
		atomic.AddInt32(&polls, 1)
		return map[string]process.ProcessState{}
	}

	_, ok := awaitResidentAlias(context.Background(), cfg, running, "default", 0, 10*time.Millisecond)
	assert.False(t, ok)
	assert.Equal(t, int32(1), atomic.LoadInt32(&polls), "grace 0 must look exactly once")
}

func TestAwaitResidentAlias_NonMatchingIdDoesNotWait(t *testing.T) {
	cfg := residentAliasTestConfig(t)
	running := func() map[string]process.ProcessState { return map[string]process.ProcessState{} }

	start := time.Now()
	_, ok := awaitResidentAlias(context.Background(), cfg, running, "no-such-model", 2*time.Second, 10*time.Millisecond)
	assert.False(t, ok)
	assert.Less(t, time.Since(start), 200*time.Millisecond, "only alias ids get the grace; an unknown id is refused at once")
}
