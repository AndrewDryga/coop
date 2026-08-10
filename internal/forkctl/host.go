// Package forkctl is the fork/fleet control plane: everything a fork needs AFTER it exists —
// supervision (claim, stop, reap, detach, logs), listing and status, the review dossier and gate,
// the fast-forward-only land, the declarative fleet, and the live board.
//
// Opening a fork stays in internal/cli beside the other launch paths (`coop fork <name>` resolves a
// preset, a one-off target, an image, and peers, then runs a box exactly as `coop run` and `coop
// acp` do). That is the seam: cli owns LAUNCH, forkctl owns LIFECYCLE. A fork's on-disk contract —
// where it lives, its branch, its lifecycle state file — is internal/forkspace's, one level below.
package forkctl

import (
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/ui"
)

// Host is what the fork/fleet commands need from the CLI that owns the process and cannot own
// itself: the lazily-detected container runtime (internal/cli caches one per process, so forkctl
// must ask rather than detect its own), the alternate-screen board driver `coop fleet watch` shares
// with `coop tasks watch`, and the loop's usage accounting.
//
// Every field is optional and a zero Host is usable — a test that drives a pure-local verb gets a
// harmless no-op default, the same contract tasks.Host and sessionsvc.Host document.
type Host struct {
	// EnsureRuntime detects (and process-wide caches) the container runtime, returning the
	// detected one — the fork verbs that tear a box down need it, the pure-local ones never ask.
	EnsureRuntime func() (runtime.Runtime, error)

	// RunWatchLoop drives the alternate-screen live board (enter/leave, signal handling, the poll
	// ticker, the settled-debounce auto-exit) that `coop fleet watch` shares with `coop tasks
	// watch` — see internal/cli/watch.go's doc comment for the full contract.
	RunWatchLoop func(screen *ui.AltScreen, tick func(spin int) (frame []string, settled bool), done func()) (int, error)

	// ForkCost reports a workspace's total loop spend and its human summary, from the loop's own
	// usage telemetry (internal/cli/telemetry.go). Both come from one read, because every caller
	// that wants one is a display site that already has the workspace.
	ForkCost func(ws string) (usd float64, summary string)
}

func (h Host) ensureRuntime() (runtime.Runtime, error) {
	if h.EnsureRuntime == nil {
		return runtime.Runtime{}, nil
	}
	return h.EnsureRuntime()
}

func (h Host) runWatchLoop(screen *ui.AltScreen, tick func(spin int) (frame []string, settled bool), done func()) (int, error) {
	if h.RunWatchLoop == nil {
		return 0, nil
	}
	return h.RunWatchLoop(screen, tick, done)
}

func (h Host) forkCost(ws string) (float64, string) {
	if h.ForkCost == nil {
		return 0, ""
	}
	return h.ForkCost(ws)
}

// Control is the fork/fleet control plane bound to one command's config and container runtime.
// internal/cli builds one per invocation and dispatches the whole verb family on it, so a step that
// detects the runtime (a stop's reap) and a later step that reads it (the teardown's DownServices)
// see the same runtime — the app-field lifetime this replaced.
type Control struct {
	cfg  *config.Config
	rt   runtime.Runtime
	host Host

	// gateOK is the merge-gate test seam: nil runs the real gate in the box.
	gateOK func(gateRepo, treeDir, img string) bool
}

// New binds a control plane to the caller's config and its runtime as detected SO FAR — the zero
// runtime.Runtime is the honest "not detected yet", exactly what the teardown paths test for.
func New(cfg *config.Config, rt runtime.Runtime, host Host) *Control {
	return &Control{cfg: cfg, rt: rt, host: host}
}

// ensureRuntime detects the runtime through the host (which owns the per-process cache) and
// remembers what it got, so the teardown steps that read c.rt directly see the same runtime. A
// runtime the caller already handed over is kept as-is — detection is lazy, and asking twice must
// not replace a working runtime with whatever a second detection (or a zero Host) returns.
func (c *Control) ensureRuntime() error {
	if c.rt.Name != "" {
		return nil
	}
	rt, err := c.host.ensureRuntime()
	if err != nil {
		return err
	}
	c.rt = rt
	return nil
}
