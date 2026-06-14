//go:build linux

package ui

import (
	"syscall"
	"unsafe"
)

// isTerminalFd reports whether fd is a tty by attempting the terminal-attributes
// ioctl, which fails with ENOTTY on anything that is not a real terminal.
func isTerminalFd(fd uintptr) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, fd, syscall.TCGETS, uintptr(unsafe.Pointer(&t)), 0, 0, 0)
	return errno == 0
}
