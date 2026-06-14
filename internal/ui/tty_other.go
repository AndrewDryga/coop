//go:build !linux && !darwin

package ui

// isTerminalFd is conservatively false on platforms without a container runtime
// coop supports; the tool targets Linux and macOS hosts.
func isTerminalFd(uintptr) bool { return false }
