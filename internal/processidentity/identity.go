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

// ProvesGone reports whether the recorded process no longer runs: its pid is gone, or now names a
// different process. It is the one rule for deleting a record or reaping what that process owned.
// Unknown and Foreign never count: the process may still run, and acting on it could take a live
// run's state.
func (s State) ProvesGone() bool { return s == Gone || s == Mismatch }

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

// PIDNamespace names the PID namespace this process sees pids in; ok is false when it cannot be
// read. A pid and its start token mean something only in the namespace they were read in: from any
// other, the same number is gone or another process. macOS has no PID namespaces, so there the
// answer is "" and true.
func PIDNamespace() (namespace string, ok bool) { return platformPIDNamespace() }

// InitialPIDNamespace is PIDNamespace's answer in a Linux host's own PID namespace: the kernel gives
// the initial namespace a fixed inode (PROC_PID_INIT_INO), the same on every boot. A container's
// namespace has another.
const InitialPIDNamespace = "pid:[4026531836]"

// Inspect separates liveness from authority to signal, asking this kernel. Unknown is fail-closed:
// nothing proves the process alive or dead. A reused pid is Mismatch, not Match, and another
// operating system's token is Foreign before any pid here is asked.
func Inspect(pid int, token string) State {
	return Kernel{Signal: syscall.Kill, StartToken: StartToken}.Inspect(pid, token)
}

// Kernel is what Inspect asks the kernel: kill(2), only ever with signal 0, and a pid's start token.
// A package that keeps its own seams for the two, as forkspace does for its tests, inspects through
// them by the same rules.
type Kernel struct {
	Signal     func(pid int, sig syscall.Signal) error
	StartToken func(pid int) string
}

// Inspect answers as the package's Inspect does, asking k. A pid of 1 or below names no process a
// record can hold (kill(2) aims 0 at the caller's own group and -1 at every process), and without a
// token of a known shape a pid cannot say which kernel it was read on, so a missing pid here proves
// nothing: both are Unknown before any pid is asked, never Gone, which would let a caller delete
// what that process may still own.
func (k Kernel) Inspect(pid int, token string) State {
	if pid <= 1 || !Stable(token) {
		return Unknown
	}
	if !Native(token) {
		return Foreign
	}
	if err := k.Signal(pid, 0); errors.Is(err, syscall.ESRCH) {
		return Gone
	} else if err != nil {
		return Unknown
	}
	current := k.StartToken(pid)
	if current == "" {
		// The process may have exited between kill(0) and the identity read. Recheck so that an
		// ordinary exit is not misreported as an unverifiable live pid, while pid reuse still fails
		// closed.
		if err := k.Signal(pid, 0); errors.Is(err, syscall.ESRCH) {
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
