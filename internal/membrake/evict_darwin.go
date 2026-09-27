//go:build darwin

package membrake

import (
	"os"

	"golang.org/x/sys/unix"
)

// evictFile drops path's clean pages from the unified buffer cache: map it
// shared and msync(MS_INVALIDATE) the whole mapping - the same call
// `vmtouch -e` uses on macOS, and it needs no root (`purge` does). Measured
// 2026-09-19 on macOS 27.0: a cached 1 GB file dropped file-backed memory by
// 1.0 GB. Pages still mapped by a live process are not dropped, which is why
// the brake evicts only after the killed process groups have exited.
func evictFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() == 0 {
		return nil
	}
	mem, err := unix.Mmap(int(f.Fd()), 0, int(st.Size()), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		return err
	}
	defer unix.Munmap(mem)
	return unix.Msync(mem, unix.MS_INVALIDATE)
}
