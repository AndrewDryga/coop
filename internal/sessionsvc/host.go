package sessionsvc

import (
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// Host is what the sessions service needs from the CLI that owns the terminal and the merge
// policy, and cannot own itself: the fork/git contracts it shares live in leaf packages
// (internal/forkspace, internal/ladder) and are plain imports.
//
// Every field is optional and a zero Host is usable — a test that drives the service directly
// injects the gate it wants. The seams are deliberately functions, not
// an interface: each is one call, and there is exactly one implementation (internal/cli).
type Host struct {
	// ReviewGateFactory builds the gate a review runs when the caller injected none. It is a
	// factory, not a gate, because the runtime is detected lazily: the service opens (and takes
	// the state-root lock) before any runtime-specific startup work happens.
	ReviewGateFactory func(*config.Config, runtime.Runtime) ReviewGate

	// Warnf reports a non-fatal condition to the operator — a failed janitorial cleanup that
	// something else will retry. Rendering belongs to the CLI, so this package never imports
	// internal/ui.
	Warnf func(format string, args ...any)
}

func (h Host) warnf(format string, args ...any) {
	if h.Warnf != nil {
		h.Warnf(format, args...)
	}
}
