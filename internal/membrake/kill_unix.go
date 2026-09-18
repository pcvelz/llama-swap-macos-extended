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
