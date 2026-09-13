//go:build !linux && !darwin

package ui

import (
	"errors"
	"os"
)

// isTerminalFd is conservatively false on platforms without a container runtime
// coop supports; the tool targets Linux and macOS hosts.
func isTerminalFd(uintptr) bool { return false }

// termWidthFd is unknown off the supported platforms; callers fall back to a default.
func termWidthFd(uintptr) int { return 0 }

func disableTerminalEcho(*os.File) (func() error, error) {
	return nil, errors.New("terminal echo control is unsupported on this platform")
}
