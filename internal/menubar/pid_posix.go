//go:build !windows

package menubar

import "syscall"

// pidAlive probes liveness with signal 0, delivering nothing.
func pidAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
