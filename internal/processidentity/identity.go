// Package processidentity reads stable kernel process identities on supported Unix platforms.
// A start token distinguishes a live process from a later process that reused the same pid.
package processidentity

import (
	"errors"
	"strings"
	"syscall"
)

// State is the result of comparing a pid with a previously recorded start token.
type State uint8

const (
	Gone State = iota
	Match
	Mismatch
	Unknown
	// Foreign is a token another operating system wrote: a Linux token read on macOS, or the
	// reverse, as when a folder is shared with a Linux container. Its pid is not this kernel's, so
	// nothing here can prove that process alive or dead, and callers keep its record.
	Foreign
)

// StartToken returns a stable, opaque token for pid, or an empty string when the kernel identity
// cannot be read. Destructive callers must treat an empty token as unauthorised, not as a match.
func StartToken(pid int) string { return platformStartToken(pid) }

// Stable reports whether token has a supported, versioned kernel identity shape.
func Stable(token string) bool { return System(token) != "" }

// System names the operating system whose kernel wrote token, "Linux" or "macOS", or returns ""
// for a shape it does not know.
func System(token string) string {
	switch {
	case strings.HasPrefix(token, "linux-proc-v1:"):
		return "Linux"
	case strings.HasPrefix(token, "darwin-kinfo-v1:"):
		return "macOS"
	}
	return ""
}

// Native reports whether token has the shape this platform's StartToken writes. A stable token that
// is not native was written on another operating system, whose pids this kernel cannot check.
func Native(token string) bool { return strings.HasPrefix(token, platformTokenPrefix) }

// Inspect separates liveness from authority to signal. Unknown is fail-closed: the process appears
// live, but its identity cannot be proved. A reused pid is Mismatch, not Match, and another
// operating system's token is Foreign before any pid here is asked.
func Inspect(pid int, token string) State {
	if pid <= 1 || !Stable(token) {
		return Unknown
	}
	if !Native(token) {
		return Foreign
	}
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return Gone
	} else if err != nil {
		return Unknown
	}
	current := StartToken(pid)
	if current == "" {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return Gone
		}
		return Unknown
	}
	if current != token {
		return Mismatch
	}
	return Match
}

// Parent returns pid's parent pid, or 0 when it cannot be read.
func Parent(pid int) int { return platformParent(pid) }

// Command returns the kernel's short command name for pid (the executable's basename, truncated the
// way the kernel truncates it), or "" when it cannot be read.
func Command(pid int) string { return platformCommand(pid) }
