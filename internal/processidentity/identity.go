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
)

// StartToken returns a stable, opaque token for pid, or an empty string when the kernel identity
// cannot be read. Destructive callers must treat an empty token as unauthorised, not as a match.
func StartToken(pid int) string { return platformStartToken(pid) }

// Stable reports whether token has a supported, versioned kernel identity shape.
func Stable(token string) bool {
	return strings.HasPrefix(token, "linux-proc-v1:") ||
		strings.HasPrefix(token, "darwin-kinfo-v1:")
}

// Inspect separates liveness from authority to signal. Unknown is fail-closed: the process appears
// live, but its identity cannot be proved. A reused pid is Mismatch, not Match.
func Inspect(pid int, token string) State {
	if pid <= 1 || !Stable(token) {
		return Unknown
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
