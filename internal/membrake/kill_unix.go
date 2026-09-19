//go:build !windows

package membrake

import "syscall"

// killGroup SIGKILLs the whole process group (children are started with
// Setpgid, so pgid == the child's pid and -pgid reaches every descendant).
// A pgid <= 1 is refused: kill(-1) or kill(0) would hit far more than a model.
func killGroup(pgid int) error {
	if pgid <= 1 {
		return syscall.EINVAL
	}
	return syscall.Kill(-pgid, syscall.SIGKILL)
}

// groupAlive reports whether any process of the group still exists (a zombie
// not yet reaped by llama-swap counts as alive: its mappings may still be
// torn down).
func groupAlive(pgid int) bool {
	if pgid <= 1 {
		return false
	}
	return syscall.Kill(-pgid, 0) != syscall.ESRCH
}
