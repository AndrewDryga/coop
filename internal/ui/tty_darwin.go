//go:build darwin

package ui

import (
	"syscall"
	"unsafe"
)

// isTerminalFd reports whether fd is a tty by attempting the terminal-attributes
// ioctl, which fails with ENOTTY on anything that is not a real terminal.
func isTerminalFd(fd uintptr) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, fd, syscall.TIOCGETA, uintptr(unsafe.Pointer(&t)), 0, 0, 0)
	return errno == 0
}

// termWidthFd returns the terminal column count for fd via TIOCGWINSZ, or 0 if it can't be
// read (so the caller falls back to a default width).
func termWidthFd(fd uintptr) int {
	var ws struct{ Row, Col, Xpixel, Ypixel uint16 }
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, fd, syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws)), 0, 0, 0)
	if errno != 0 {
		return 0
	}
	return int(ws.Col)
}
