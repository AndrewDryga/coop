// Package loop is coop's unattended work engine — what `coop loop` and a fork's worker actually
// run. One Run drains a repo's task queues: a work iteration per task, an optional between-task
// audit, and signoff rounds at the end, rotating targets on a rate limit, watching each provider
// for silence, decoding its stream JSON into the live bar, and recording every stage's telemetry.
//
// internal/cli keeps the COMMAND — argv to preset, target, peers, queues, rotation, and image
// (loop_cmd.go, fork_cmd.go) — and this package owns the RUN. That is the seam internal/forkctl
// draws one level down: cli owns LAUNCH, the extracted package owns what happens after. The
// engine's terminal output is a multi-hour incremental render (the sticky live bar with the
// agent's own output scrolling above it), so this package paints directly through internal/ui
// rather than returning data for a caller to print.
package loop

import (
	"io"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/ladder"
	"github.com/AndrewDryga/coop/internal/preset"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// The package's API is New, Host, Control, RunSpec, Run, and WorkspaceCost. Six more names are
// exported for ONE reason — StageRecord, PeerRecord, ReadPeerRecords, IterationCommand,
// LoopWorkPrompt, LoopInterruptedExitCode: internal/cli's scripted process e2e suite asserts the
// loop's observable contract (the argv it builds, the prompt it sends, the telemetry rows it
// writes, the exit status it returns), and those tests are welded to the shared process harness
// that also serves the fork/consult/delegate/preset e2e families, so they cannot move here. No
// production code outside this package reads them. See .agent/kb/provider-scripted-e2e.md.

// Host is what the loop needs from the CLI that owns the process and cannot own itself. All three
// are shared with commands outside the loop, which is exactly why they stay in internal/cli.
//
// Every field is optional and a zero Host is usable — a test that drives a pure part of the engine
// gets a harmless no-op default, the same contract tasks.Host, acpctl.Host, sessionsvc.Host and
// forkctl.Host document. Host carries BEHAVIOR only: the running coop's version is immutable data
// that -ldflags pins to internal/cli, so New takes it as a value instead.
type Host struct {
	// SweepOrphanBoxes reaps this repo's boxes whose supervising coop is dead. internal/cli owns
	// the once-per-process cache that `fork start`, `fleet up` and `coop build` share, so the loop
	// asks rather than sweeping the same repo again.
	SweepOrphanBoxes func(repo string)

	// SignUnpushed re-signs base..HEAD with the host key, returning how many commits it signed. It
	// needs cli's ref-update test seam, and is also `coop sign` and the interactive box-exit path.
	SignUnpushed func(repo, base string) (int, error)

	// BuildRotation expands a target ladder against the signed-in accounts. Ladder expansion is
	// credential policy shared with `coop acp` and `coop fork`, not loop material; the loop asks
	// only for a review stage's own rotation.
	BuildRotation func(agent string, rungs []agents.Target) (*ladder.Rotation, error)
}

func (h Host) sweepOrphanBoxes(repo string) {
	if h.SweepOrphanBoxes == nil {
		return
	}
	h.SweepOrphanBoxes(repo)
}

func (h Host) signUnpushed(repo, base string) (int, error) {
	if h.SignUnpushed == nil {
		return 0, nil
	}
	return h.SignUnpushed(repo, base)
}

func (h Host) buildRotation(agent string, rungs []agents.Target) (*ladder.Rotation, error) {
	if h.BuildRotation == nil {
		return nil, nil
	}
	return h.BuildRotation(agent, rungs)
}

// Control is the loop engine bound to one command's config, container runtime, and host. Every
// caller builds one per invocation and runs exactly one loop on it, so the per-run state below has
// the same lifetime it had as a field on internal/cli's app struct — with the difference that
// nothing outside this package can see it now.
type Control struct {
	cfg     *config.Config
	rt      runtime.Runtime
	version string // the running coop's version, stamped into every telemetry stage record
	host    Host

	// The run in flight, bound by Run. preset and forkOwner are RunSpec values the iteration
	// needs deep in the call graph; runID/streamSeq/streamOff are the engine's own stream-trace
	// bookkeeping.
	preset    *preset.Preset
	forkOwner string
	runID     string // COOP_RUN_ID, so a consult peer's usage lands in this run's cost digest
	streamSeq int    // streaming box attempt sequence within runID
	streamOff bool   // an open failure disables best-effort tracing for the rest of the run
}

// New binds the engine to the caller's config, its runtime as detected so far, the running coop's
// version (pinned to internal/cli by -ldflags, so it has to be passed in), and the host callbacks.
func New(cfg *config.Config, rt runtime.Runtime, version string, host Host) *Control {
	return &Control{cfg: cfg, rt: rt, version: version, host: host}
}

// RunSpec is one loop run's inputs: what to work, who works it, and the caps. It replaces an
// eleven-parameter call that two callers had to keep in the same order, and it is where a fork's
// container owner arrives — previously a field the fork launcher set on the shared app struct and
// restored with a defer.
type RunSpec struct {
	Repo  string // the checkout the loop works (a fork loop passes its own worktree)
	Image string // the box image, already resolved for Repo
	Agent string // the lead provider for the first iteration; a rotation may swap it

	ForkName  string // the fork's name, "" for a local loop
	ForkOwner string // repo-scoped runtime owner label for a fork's boxes, "" for a local loop

	Rotation *ladder.Rotation // the work stage's target ladder, already expanded
	Queues   []string         // repo-relative task queue dirs to drain
	Preset   *preset.Preset   // the run's loaded preset, "" roles when nil
	Peers    []agents.Target  // consult peers each iteration may call
	Sink     io.Writer        // extra copy of the agent's output (a fork's log), nil for none

	DebugOnFail bool // open a shell in the box after a failed iteration
	Preflight   bool // run the pre-flight probe before the first work iteration
	MaxTasks    int  // stop after this many settled tasks, 0 for the whole queue
}
