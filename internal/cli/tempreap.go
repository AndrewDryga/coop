package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
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
	roots := []string{os.TempDir()}
	// Ordinary run credentials and policy snapshots now live outside the shared system temp
	// directory so a prior box with a broad /tmp bind cannot observe a later run. Sweep that
	// stable private root too; locating it is intentionally read-only so housekeeping never
	// creates run state.
	if root, exists, err := box.ExistingRunArtifactRoot(a.cfg); err == nil && exists {
		roots = append(roots, root)
	}
	go reapTemp(a.rt, roots, stamp)
}

// reapTemp is one pass over roots. It stamps the day only after a pass in which
// the runtime said what is mounted. A process that exits mid-pass — a launch that
// fails at once, a coop stopped — stamps nothing, and a pass that could not ask
// removed nothing, so either way the next launch tries again. A pass that failed
// any other way, such as a removal the filesystem refused, would fail again, so
// it waits a day. Coops launched while one sweeps may sweep too, which only
// repeats work: every pass asks what is mounted before it removes anything.
func reapTemp(rt runtime.Runtime, roots []string, stamp string) {
	ctx, cancel := context.WithTimeout(context.Background(), tempReapTimeout)
	defer cancel()
	for _, root := range roots {
		if _, err := box.ReapOrphanTempEntries(ctx, rt, root, time.Now()); errors.Is(err, box.ErrMountsUnknown) {
			return
		}
	}
	if err := os.MkdirAll(filepath.Dir(stamp), 0o755); err == nil {
		_ = os.WriteFile(stamp, nil, 0o644)
	}
}
