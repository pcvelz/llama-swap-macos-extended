//go:build windows

package router

import "syscall"

// setSockBufFD sets a SOL_SOCKET buffer option (syscall.SO_RCVBUF or
// syscall.SO_SNDBUF) on a raw socket. See sockBufControl.
func setSockBufFD(fd uintptr, opt, size int) error {
	return syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, opt, size)
}
