package scheduler

// The waiting rows of the sessions API say WHAT a parked request waits for,
// WHO it waits behind and HOW LONG (user-approved menu layout, 2026-10-18):
// every hold must carry a `waitingFor` alongside its park reason -
//   {"session": "<the session id being waited on>", "until": "<RFC3339 or empty>"}
// so the menu can render "after [c83f32d8] cools down · 4:51" instead of an
// internal reason name. The field travels the same path park_reason does
// (markParked -> in-flight metadata -> sessions API), so these tests pin it
// on the scheduler's parked request, next to wantParked.

import (
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/process"
)

// The model-cooldown test needs its own rig: two models, a ready resident
// "a" with a 60s grace and an evictee target "b", on the same harness as
// cooldown_unified_test.go's modelContinuousFixture.
func newWaitingForCooldownRig(t *testing.T) (*FIFO, *time.Time) {
	t.Helper()
	eff := newFakeEffects()
	eff.states["a"] = process.StateReady
	eff.states["b"] = process.StateStopped
	clk := time.Unix(1000, 0)
	grace := map[string]time.Duration{"a": 60 * time.Second}
	s := newFIFOGrace(&stubPlanner{evict: map[string][]string{"b": {"a"}}}, eff, grace, &clk)

	s.OnRequest(req("a"))
	s.OnServeDone(ServeDoneEvent{ModelID: "a"})
	clk = clk.Add(10 * time.Second) // a is 10s into its 60s grace
	return s, &clk
}

// wantWaitingFor asserts the parked request carries the given waitingFor:
// the session it waits behind ("" when there is none, e.g. a model cooldown)
// and the RFC3339 moment the hold ends ("" when it has no clock).
func (r *kvWarmRig) wantWaitingFor(session, until string) {
	r.t.Helper()
	if len(r.s.queued) != 1 {
		var got []string
		for _, q := range r.s.queued {
			got = append(got, q.Session+":"+q.parkReason)
		}
		r.t.Fatalf("queue = %v, want exactly one parked request", got)
	}
	q := r.s.queued[0]
	if q.waitingForSession != session {
		r.t.Errorf("waitingFor.session = %q, want %q (park reason %q)", q.waitingForSession, session, q.parkReason)
	}
	if q.waitingForUntil != until {
		r.t.Errorf("waitingFor.until = %q, want %q (park reason %q)", q.waitingForUntil, until, q.parkReason)
	}
}

// Slot cooldown (kv-warm): the waiter is held by a COOLING session's slot.
// The hold must name that session and when its cooldown ends - the "after
// [c83f32d8] cools down · 4:51" data, not just the reason.
func TestWaitingFor_KVWarmNamesTheCoolingSessionAndUntil(t *testing.T) {
	r := newKVWarmRig(t, kvwPool)
	r.req("A", kvwA)
	r.wantServed(1, "A alone")
	r.done("A", kvwA)

	// A is now idle and cooling for the full swap grace; B would displace it.
	r.req("B", kvwB)
	r.wantParked(ParkKVWarm)

	wantUntil := r.clock.Add(kvwGrace * time.Second).Format(time.RFC3339)
	r.wantWaitingFor("A", wantUntil)
}

// KV admission (kv): a large request parks while the pool is busy with an
// in-flight large turn - "after [8f10d234] (cq27: 1 at a time)". The hold
// must name the session whose turn it waits behind; there is no clock, so
// until stays empty.
func TestWaitingFor_KVNamesTheInFlightLargeSession(t *testing.T) {
	r := newKVWarmRig(t, kvwPool)
	r.req("A", kvwA) // in flight: the large turn the pool is busy with
	r.wantServed(1, "A alone")

	r.req("B", kvwB)
	r.wantParked(ParkKV)
	r.wantWaitingFor("A", "")
}

// Model cooldown (ParkCooldown): a cross-model swap waits for the resident's
// grace to run down. There is no session behind it - the waiter waits on a
// MODEL - so waitingFor.session stays empty and until is the resident's
// grace end.
func TestWaitingFor_CooldownNamesTheResidentGraceEnd(t *testing.T) {
	s, clk := newWaitingForCooldownRig(t)

	s.OnRequest(reqCh("b"))
	if len(s.queued) != 1 || s.queued[0].parkReason != ParkCooldown {
		var got []string
		for _, q := range s.queued {
			got = append(got, q.Model+":"+q.parkReason)
		}
		t.Fatalf("queue = %v, want b parked on cooldown", got)
	}
	wantUntil := (*clk).Add(50 * time.Second).Format(time.RFC3339)
	if s.queued[0].waitingForSession != "" {
		t.Errorf("waitingFor.session = %q, want empty: a model cooldown waits on no session", s.queued[0].waitingForSession)
	}
	if s.queued[0].waitingForUntil != wantUntil {
		t.Errorf("waitingFor.until = %q, want %q (resident's grace end)", s.queued[0].waitingForUntil, wantUntil)
	}
}

// A rank-barrier hold has no session and no clock: it simply waits for the
// higher-priority work ahead of it to run. The field must still be present
// (both parts empty), so a renderer can say "after higher-priority work"
// without inventing data.
func TestWaitingFor_RankHasNoSessionAndNoUntil(t *testing.T) {
	r := newKVWarmRig(t, kvwPool)
	r.req("A", kvwA) // in flight on the default tier
	r.wantServed(1, "A alone")

	// A higher-rank request parked ahead of B (on KV admission: A holds 750 of
	// the 1000-token pool, so its 500 cannot fit): B parks on the rank barrier.
	high := tierReq("m", tierPriority)
	high.EstimatedTokens = 500
	r.s.OnRequest(high)
	if len(r.s.queued) != 1 || r.s.queued[0].parkReason != ParkKV {
		var got []string
		for _, q := range r.s.queued {
			got = append(got, q.Session+":"+q.parkReason)
		}
		t.Fatalf("queue = %v, want the priority request parked on KV admission", got)
	}

	r.req("B", kvwB)
	if len(r.s.queued) != 2 || r.s.queued[1].parkReason != ParkRank {
		var got []string
		for _, q := range r.s.queued {
			got = append(got, q.Session+":"+q.parkReason)
		}
		t.Fatalf("queue = %v, want B parked on the rank barrier", got)
	}
	b := r.s.queued[1]
	if b.waitingForSession != "" || b.waitingForUntil != "" {
		t.Errorf("waitingFor = (%q, %q), want both empty: a rank hold waits on higher-priority work, not a session or a clock",
			b.waitingForSession, b.waitingForUntil)
	}
}
