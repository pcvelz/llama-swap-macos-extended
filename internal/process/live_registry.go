package process

import (
	"strings"
	"sync"
)

// LiveChild is one running upstream child as the memory brake
// (internal/membrake) sees it: the model ID, the process group to kill, and
// the model files named on its command line (the brake evicts their pages
// from the file cache after a kill). Upstreams are started with Setpgid
// (runtime_unix.go), so the group ID is the child's own pid and -Pgid reaches
// every descendant.
type LiveChild struct {
	ID    string
	Pgid  int
	Files []string
}

type liveEntry struct {
	pgid  int
	files []string
}

// modelFileFlags are the llama-server flags whose value is a model file the
// server mmaps: the weights, the multimodal projector and the draft model.
var modelFileFlags = map[string]bool{
	"-m": true, "--model": true,
	"-mm": true, "--mmproj": true,
	"-md": true, "--model-draft": true,
}

// ModelFiles returns the model file paths named in a child's argv, in order,
// for both "--model path" and "--model=path". Anything else is ignored: a
// model fetched by -hf has no path on the command line.
func ModelFiles(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if k, v, ok := strings.Cut(a, "="); ok && modelFileFlags[k] {
			if v != "" {
				out = append(out, v)
			}
			continue
		}
		if modelFileFlags[a] && i+1 < len(args) {
			out = append(out, args[i+1])
			i++
		}
	}
	return out
}

// The live registry holds every child between cmd.Start and cmd.Wait
// returning. It exists so the memory brake can SIGKILL process groups
// directly, without a round trip through each process's run goroutine (which
// may itself be stuck in the very memory stall the brake reacts to).
var (
	liveMu sync.Mutex
	live   = map[*ProcessCommand]liveEntry{}
)

func registerLive(p *ProcessCommand, pgid int, args []string) {
	e := liveEntry{pgid: pgid, files: ModelFiles(args)}
	liveMu.Lock()
	live[p] = e
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
	for p, e := range live {
		dst = append(dst, LiveChild{ID: p.id, Pgid: e.pgid, Files: e.files})
		if p.State() == StateStarting {
			loading = true
		}
	}
	liveMu.Unlock()
	return dst, loading
}
