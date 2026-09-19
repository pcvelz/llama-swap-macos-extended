//go:build darwin

package membrake

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// residentPages maps path and counts its pages that are in memory (mincore).
func residentPages(t *testing.T, path string) (resident, total int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	mem, err := unix.Mmap(int(f.Fd()), 0, int(st.Size()), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Munmap(mem)
	vec := make([]byte, (len(mem)+os.Getpagesize()-1)/os.Getpagesize())
	// x/sys/unix has no Mincore on darwin.
	if _, _, e := syscall.Syscall(syscall.SYS_MINCORE, uintptr(unsafe.Pointer(&mem[0])), uintptr(len(mem)), uintptr(unsafe.Pointer(&vec[0]))); e != 0 {
		t.Fatal(e)
	}
	for _, v := range vec {
		if v&1 != 0 {
			resident++
		}
	}
	return resident, len(vec)
}

// The real eviction, no root: a freshly written and read 64 MB file is
// resident in the file cache; after evictFile it is (almost) not.
func TestEvictFile_DropsTheFileFromTheCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "weights.gguf")
	buf := make([]byte, 64<<20)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(path); err != nil {
		t.Fatal(err)
	}
	before, total := residentPages(t, path)
	if before < total/2 {
		t.Skipf("setup: only %d/%d pages resident after a read; cannot observe an eviction", before, total)
	}
	if err := evictFile(path); err != nil {
		t.Fatalf("evictFile: %v", err)
	}
	after, _ := residentPages(t, path)
	t.Logf("resident pages %d -> %d of %d", before, after, total)
	if after > total/10 {
		t.Fatalf("evictFile left %d/%d pages resident (was %d)", after, total, before)
	}
}

func TestEvictFile_MissingFileIsAnError(t *testing.T) {
	if evictFile(filepath.Join(t.TempDir(), "nope.gguf")) == nil {
		t.Fatal("a missing model file must be reported, not silently skipped")
	}
}
