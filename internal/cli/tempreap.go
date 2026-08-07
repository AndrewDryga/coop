package cli

import (
	"context"
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

// startTempReap sweeps orphaned box temp entries once a day, in the background.
//
// Coop's per-run cleanup is a deferred call, which does not run when the
// process is killed — the normal way a box ends under supervision, a restart,
// or a machine sleeping. Everything those runs leave behind accumulates in the
// temp directory until it fills the volume.
//
// Nothing waits on this and nothing is reported. It is best effort by design:
// a sweep that fails is a sweep that happens tomorrow, and a user running a
// command should never see housekeeping in their output or their latency.
func startTempReap(cfg *config.Config) {
	path := tempReapPath(cfg)
	if !tempReapDue(path, time.Now()) {
		return
	}
	// Mark the attempt before doing it, so parallel coops do not all sweep.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
		_ = os.WriteFile(path, nil, 0o644)
	}
	go func() {
		rt, err := runtime.Detect(cfg.RuntimeName)
		if err != nil {
			// No container runtime means nothing can be asked what is mounted,
			// and the reaper refuses to guess. Tomorrow is fine.
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), tempReapTimeout)
		defer cancel()
		_, _ = box.ReapOrphanTempEntries(ctx, rt, os.TempDir(), time.Now())
	}()
}
