//go:build windows

package router

import "syscall"

// setRcvBufFD sets SO_RCVBUF on a raw socket. See rcvBufDialer.
func setRcvBufFD(fd uintptr, size int) error {
	return syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, size)
}
