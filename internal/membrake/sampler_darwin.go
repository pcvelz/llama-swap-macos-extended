//go:build darwin && cgo

package membrake

/*
#include <mach/mach.h>
#include <mach/mach_host.h>
#include <sys/sysctl.h>
#include <stdint.h>

static mach_port_t mb_host;

static void mb_init(void) {
	// One send right for the life of the process: calling mach_host_self()
	// per sample would leak a port reference each time.
	mb_host = mach_host_self();
}

// out[0] = wired bytes, out[1] = file-backed bytes, out[2] = swap used bytes.
// File-backed is external_page_count: the very field vm_stat prints as
// "File-backed pages" (sampler_darwin_test.go checks the two agree).
static int mb_sample(uint64_t *out) {
	vm_statistics64_data_t vs;
	mach_msg_type_number_t cnt = HOST_VM_INFO64_COUNT;
	if (host_statistics64(mb_host, HOST_VM_INFO64, (host_info64_t)&vs, &cnt) != KERN_SUCCESS) {
		return -1;
	}
	uint64_t pg = (uint64_t)vm_kernel_page_size;
	out[0] = (uint64_t)vs.wire_count * pg;
	out[1] = (uint64_t)vs.external_page_count * pg;
	struct xsw_usage xs;
	size_t len = sizeof(xs);
	if (sysctlbyname("vm.swapusage", &xs, &len, NULL, 0) != 0) {
		return -2;
	}
	out[2] = (uint64_t)xs.xsu_used;
	return 0;
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// nativeSampler reads the kernel's VM counters in-process: no fork, no exec,
// and no allocation (buf is part of the heap-allocated sampler, so passing its
// address to C does not make anything escape per call).
type nativeSampler struct {
	buf [3]C.uint64_t
}

// NewNativeSampler returns the macOS sampler after one successful probe read.
func NewNativeSampler() (Sampler, error) {
	C.mb_init()
	s := &nativeSampler{}
	var r Reading
	if err := s.Sample(&r); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *nativeSampler) Sample(r *Reading) error {
	if rc := C.mb_sample((*C.uint64_t)(unsafe.Pointer(&s.buf[0]))); rc != 0 {
		return fmt.Errorf("membrake: vm sample failed (%d)", int(rc))
	}
	r.Wired = uint64(s.buf[0])
	r.FileBacked = uint64(s.buf[1])
	r.SwapUsed = uint64(s.buf[2])
	return nil
}
