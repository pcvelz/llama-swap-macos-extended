//go:build !windows

package process

import (
	"context"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"syscall"
)

func liveFor(p *ProcessCommand) (LiveChild, bool, bool) {
	kids, loading := SnapshotLive(nil)
	for _, c := range kids {
		if c.ID == p.id {
			return c, loading, true
		}
	}
	return LiveChild{}, loading, false
}

// The memory brake's kill path end to end on a TEST-OWNED child (`sleep`,
// never a model): a started child is in the live registry with pgid == its
// own process group, a SIGKILL to -pgid (what membrake.killGroup sends) ends
// it, the process machine sees an unexpected exit, and the registry drops it.
func TestLiveRegistryPgidAndGroupKill(t *testing.T) {
	p := newProcessCommand(t, config.ModelConfig{
		Cmd:           "sleep 60",
		Proxy:         "http://127.0.0.1:1",
		CheckEndpoint: "none",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.EnsureReady(ctx, 10*time.Second); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}
	c, _, ok := liveFor(p)
	if !ok {
		t.Fatal("a running child must be in the live registry")
	}
	if g, err := syscall.Getpgid(c.Pgid); err != nil || g != c.Pgid {
		t.Fatalf("registered pgid %d is not its own group leader (getpgid=%d, err=%v)", c.Pgid, g, err)
	}

	buf := make([]LiveChild, 0, 16)
	if a := testing.AllocsPerRun(100, func() { buf, _ = SnapshotLive(buf[:0]) }); a != 0 {
		t.Fatalf("SnapshotLive allocated %.1f per call on the brake's hot path", a)
	}

	if err := syscall.Kill(-c.Pgid, syscall.SIGKILL); err != nil {
		t.Fatalf("group kill: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, ok := liveFor(p); !ok && p.State() == StateStopped {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, _, still := liveFor(p)
	t.Fatalf("after SIGKILL: state=%s, still registered=%v", p.State(), still)
}
