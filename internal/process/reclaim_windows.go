//go:build windows

package process

import (
	"errors"
	"fmt"
)

// errReclaimUnsupported is returned by findPortHolder on Windows so that
// reclaimOrphanedUpstream declines and the original start error surfaces.
var errReclaimUnsupported = errors.New("orphan reclaim is not supported on windows")

// findPortHolder never identifies a holder on Windows.
//
// The reclaim's safety rests on the holder being an orphan, which the Unix side
// reads as "parent pid is 1". Windows has no such re-parenting, so that guard
// cannot be evaluated, and killing a port holder without it is exactly the
// blanket kill this path refuses to be. It is also not needed here: every
// upstream is bound to a Job Object (see treecleanup_windows.go) and is reaped
// by the OS when llama-swap exits, even on a forced kill, so the leaked upstream
// this recovers from does not occur.
func findPortHolder(port int) (portHolder, error) {
	return portHolder{}, fmt.Errorf("port %d: %w", port, errReclaimUnsupported)
}

// killPID is never reached on Windows because findPortHolder never returns a
// holder. It refuses rather than acting so a future caller cannot use it to
// kill without the orphan guard.
func killPID(pid int) error {
	return fmt.Errorf("pid %d: %w", pid, errReclaimUnsupported)
}
