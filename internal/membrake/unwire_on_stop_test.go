package membrake

import (
	"testing"

	"github.com/mostlygeek/llama-swap/internal/process"
)

// The 2026-10-19 16:06:39 false trip, replayed from the /api/debug/history
// per-second samples: cq35 resident, ready and idle; the cooldown ended and
// the swap stopped it. As the weights UN-WIRED on the way out they were
// reclassified as file cache with no new data read - file-backed 1.84 ->
// 25.72 GB in one second while wired fell 33.07 -> 4.87 GB. The brake was
// disarmed while a child is STARTING but not while one is STOPPING, so the
// stop read as a +24 GB run-up and it SIGKILLed the model it was about to
// unload anyway.

// TestUnwireOnStopDoesNotTrip is the exact 16:06:37-16:06:40 sequence plus the
// swap that followed it: armed, one ready child at 1.8 GB for five minutes;
// the child goes stopping while file-backed jumps to 25.7 GB in one tick; no
// child; a new child starts (file-backed 25 GB); the new child becomes ready
// and file-backed drifts DOWN from 25 to 20 GB over five minutes. The brake
// must not trip anywhere: an un-wire is not a run-up, and a re-arm after the
// transition must start from fresh samples, not from the pre-stop minimum.
func TestUnwireOnStopDoesNotTrip(t *testing.T) {
	r := newRig(t, testCfg(t))
	stopping := []process.LiveChild{{ID: "cq35", Pgid: 4343}}

	// Armed and ready: five minutes flat at the resident's idle level.
	for sec := 0; sec < 300; sec++ {
		if r.step(sec, 1.8) {
			t.Fatalf("tripped on flat file-backed before the stop at %ds", sec)
		}
	}
	// The swap stops cq35: in one tick its weights un-wire and reclassify as
	// file cache (+24 GB, no new data read). The child is still live - it is
	// stopping - so the brake must see that and stay disarmed.
	r.k.kids = append(r.k.kids[:0], stopping...)
	r.k.transitioning = true
	if r.step(300, 25.7) {
		t.Fatal("tripped on the stop's un-wire: a departing model is not a run-up")
	}
	for sec := 301; sec < 360; sec++ {
		if r.step(sec, 25.7) {
			t.Fatalf("tripped while the child was still stopping at %ds", sec-300)
		}
	}
	// The child exits; nothing is running for two minutes.
	r.k.kids = nil
	r.k.transitioning = false
	for sec := 360; sec < 480; sec++ {
		if r.step(sec, 25.7) {
			t.Fatalf("tripped with no child at %ds", sec)
		}
	}
	// The swap loads the next model: file-backed sits at ~25 GB while it starts.
	r.k.loading = true
	for sec := 480; sec < 540; sec++ {
		if r.step(sec, 25) {
			t.Fatalf("tripped while a child is starting at %ds", sec)
		}
	}
	// Ready: file-backed drifts DOWN from 25 to 20 GB over five minutes. A
	// falling signal must not trip - and neither may the re-arm itself, whose
	// window minimum must be the post-transition samples, not the pre-stop 1.8.
	r.k.loading = false
	r.k.kids = append(r.k.kids[:0], stopping...)
	for sec := 540; sec < 840; sec++ {
		v := 25 - float64(sec-540)*5/300
		if r.step(sec, v) {
			t.Fatalf("tripped %ds after the new child was ready (file-backed %.1f GB): the re-arm carried pre-stop samples", sec-540, v)
		}
	}
	if r.kills() != 0 || r.hold.Holding() {
		t.Fatal("the un-wire-on-stop sequence must not kill or hold")
	}
}

// TestUnwireOnStopThenRealRiseStillTrips is the same sequence, but once the new
// child is ready file-backed RISES 4 GB above its post-ready minimum within the
// window: a genuine run-up after the transition. The brake must keep its job -
// the disarm on the stop may not blind it to what follows.
func TestUnwireOnStopThenRealRiseStillTrips(t *testing.T) {
	r := newRig(t, testCfg(t))
	stopping := []process.LiveChild{{ID: "cq35", Pgid: 4343}}

	for sec := 0; sec < 300; sec++ {
		if r.step(sec, 1.8) {
			t.Fatalf("tripped on flat file-backed before the stop at %ds", sec)
		}
	}
	r.k.kids = append(r.k.kids[:0], stopping...)
	r.k.transitioning = true
	if r.step(300, 25.7) {
		t.Fatal("tripped on the stop's un-wire: a departing model is not a run-up")
	}
	for sec := 301; sec < 360; sec++ {
		if r.step(sec, 25.7) {
			t.Fatalf("tripped while the child was still stopping at %ds", sec-300)
		}
	}
	r.k.kids = nil
	r.k.transitioning = false
	for sec := 360; sec < 480; sec++ {
		if r.step(sec, 25.7) {
			t.Fatalf("tripped with no child at %ds", sec)
		}
	}
	r.k.loading = true
	for sec := 480; sec < 540; sec++ {
		if r.step(sec, 25) {
			t.Fatalf("tripped while a child is starting at %ds", sec)
		}
	}
	// Ready: settles at 20 GB for two minutes (the post-ready minimum)...
	r.k.loading = false
	r.k.kids = append(r.k.kids[:0], stopping...)
	for sec := 540; sec < 660; sec++ {
		if r.step(sec, 20) {
			t.Fatalf("tripped on flat file-backed after ready at %ds", sec-540)
		}
	}
	// ...then a real +4 GB run-up within the window: it MUST trip.
	for i := 1; i <= 60; i++ {
		if r.step(660+i, 20+float64(i)*4/60) {
			return
		}
	}
	t.Fatal("a +4 GB rise above the post-ready minimum within the window must trip")
}
