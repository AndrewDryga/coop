package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// tempReapInterval is how often orphaned temp entries are swept.
//
// Daily rather than per-run: the leak accumulates over days, and asking the
// container runtime what is mounted on every single command would put an
// inspect round trip on a hot path to solve a slow problem.
const tempReapInterval = 24 * time.Hour

// tempReapTimeout bounds the sweep. It is housekeeping — if the runtime is slow
// or wedged, giving up costs nothing and blocking costs the user's command.
const tempReapTimeout = 30 * time.Second

func tempReapPath(cfg *config.Config) string {
	return filepath.Join(cfg.BoxHome, "temp-reap")
}

func tempReapDue(path string, now time.Time) bool {
	fi, err := os.Stat(path)
	return err != nil || now.Sub(fi.ModTime()) >= tempReapInterval
}

// startTempReap sweeps orphaned box temp entries once a day, in the background
// of a box launch.
//
// Coop's per-run cleanup is a deferred call, which does not run when the
// process is killed — the normal way a box ends under supervision, a restart,
// or a machine sleeping. Everything those runs leave behind accumulates in the
// temp directory until it fills the volume.
//
// The sweep dies with its process, so it starts, once per process, only where a
// box keeps the process alive: runBox (direct, editor and fork launches), the
// readonly and bare launches, and the orphan-box sweep that loop and fork starts
// run first. A build's recycle runs that sweep too, near its end; if the process
// exits first, the next launch tries again. Doctor probes, eval trials and fork
// merge gates launch boxes without it.
//
// Nothing waits on this and nothing is reported: a user running a command should
// never see housekeeping in their output or their latency. Every runtime call and
// removal happens in the goroutine; the launch pays for a few filesystem calls.
func (a *app) startTempReap() {
	if !a.tempReapPending {
		return
	}
	a.tempReapPending = false // once per process
	stamp := tempReapPath(a.cfg)
	if !tempReapDue(stamp, time.Now()) {
		return
	}
	go reapTemp(a.rt, a.cfg, stamp)
}

// reapTemp is one pass over the system temp directory and Coop's private runfiles root. That root
// keeps ordinary run credentials and policy snapshots out of the shared temp directory, where a
// prior box with a broad /tmp bind could observe a later run, and holds each run's Docker client
// folder. Locating it is read-only, so housekeeping never creates run state.
//
// A run's folder is judged only by the daemon its box started on (box.ReapOrphanRunfiles): the
// runtime lists the boxes of one Docker daemon, and another daemon's boxes are simply missing from
// that list. So when there is a runfiles root to judge, the pass first pins the daemon the current
// Docker context selects, and lists only that daemon however the context changes meanwhile. A pin
// keeps a Docker client folder in that root while it lists, so a host without one is not pinned.
// Docker at an endpoint that is not a local socket (TCP, SSH) cannot be pinned, and a pass with no
// pinned daemon keeps every run folder while it still clears the rest. Apple's container cannot
// list mounts at all, so there the pass removes nothing and stamps nothing.
//
// It stamps the day only after a pass in which the runtime said what is mounted. A process that
// exits mid-pass — a launch that fails at once, a coop stopped — stamps nothing, and a pass that
// could not ask removed nothing, so either way the next launch tries again. A pass that failed any
// other way, such as a removal the filesystem refused, would fail again, so it waits a day. Coops
// launched while one sweeps may sweep too, which only repeats work: every pass asks what is mounted
// before it removes anything.
func reapTemp(rt runtime.Runtime, cfg *config.Config, stamp string) {
	ctx, cancel := context.WithTimeout(context.Background(), tempReapTimeout)
	defer cancel()
	if _, exists, err := config.ExistingRunfiles(cfg); err == nil && exists {
		if pinned, err := rt.FreezeCompose(ctx); err == nil {
			rt = pinned
		}
	}
	if _, err := box.ReapOrphanTempEntries(ctx, rt, os.TempDir(), time.Now()); errors.Is(err, box.ErrMountsUnknown) {
		return
	}
	if _, err := box.ReapOrphanRunfiles(ctx, rt, cfg, time.Now()); errors.Is(err, box.ErrMountsUnknown) {
		return
	}
	// Views of repositories that are gone; the sweep keeps one it cannot judge or that holds an
	// unfinished rebase or merge.
	_, _ = forkspace.SweepGitViews()
	if err := os.MkdirAll(filepath.Dir(stamp), 0o755); err == nil {
		_ = os.WriteFile(stamp, nil, 0o644)
	}
}
