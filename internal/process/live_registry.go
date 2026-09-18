package process

import "sync"

// LiveChild is one running upstream child as the memory brake
// (internal/membrake) sees it: the model ID and the process group to kill.
// Upstreams are started with Setpgid (runtime_unix.go), so the group ID is
// the child's own pid and -Pgid reaches every descendant.
type LiveChild struct {
	ID   string
	Pgid int
}

// The live registry holds every child between cmd.Start and cmd.Wait
// returning. It exists so the memory brake can SIGKILL process groups
// directly, without a round trip through each process's run goroutine (which
// may itself be stuck in the very memory stall the brake reacts to).
var (
	liveMu sync.Mutex
	live   = map[*ProcessCommand]int{}
)

func registerLive(p *ProcessCommand, pgid int) {
	liveMu.Lock()
	live[p] = pgid
	liveMu.Unlock()
}

func unregisterLive(p *ProcessCommand) {
	liveMu.Lock()
	delete(live, p)
	liveMu.Unlock()
}

// SnapshotLive appends every live child to dst (pass a preallocated slice to
// stay allocation-free) and reports whether any of them is still starting.
// A starting child is loading its model: the GGUF read floods the file cache
// on purpose, which the brake must not mistake for a pager run-up.
func SnapshotLive(dst []LiveChild) ([]LiveChild, bool) {
	loading := false
	liveMu.Lock()
	for p, pgid := range live {
		dst = append(dst, LiveChild{ID: p.id, Pgid: pgid})
		if p.State() == StateStarting {
			loading = true
		}
	}
	liveMu.Unlock()
	return dst, loading
}
