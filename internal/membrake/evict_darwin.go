//go:build darwin

package membrake

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
	"unsafe"

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

// residentBytes is how much of path sits in the file cache now: map it and
// count the pages mincore reports resident. A mapping of our own touches no
// page, so the reading does not change what it measures. A file that no longer
// exists has nothing cached.
func residentBytes(path string) (int64, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if st.Size() == 0 {
		return 0, nil
	}
	mem, err := unix.Mmap(int(f.Fd()), 0, int(st.Size()), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		return 0, err
	}
	defer unix.Munmap(mem)
	page := os.Getpagesize()
	vec := make([]byte, (len(mem)+page-1)/page)
	// x/sys/unix has no Mincore on darwin.
	if _, _, e := syscall.Syscall(syscall.SYS_MINCORE, uintptr(unsafe.Pointer(&mem[0])), uintptr(len(mem)), uintptr(unsafe.Pointer(&vec[0]))); e != 0 {
		return 0, e
	}
	var n int64
	for _, v := range vec {
		if v&1 != 0 {
			n++
		}
	}
	return n * int64(page), nil
}
