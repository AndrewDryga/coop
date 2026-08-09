package cli

import (
	"context"
	"time"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/ui"
)

// orphanSweepTimeout bounds the sweep. It is housekeeping on the way into real work — if the
// runtime is slow or wedged, giving up costs an orphan that the next run (or `coop doctor`) still
// sees, while blocking would cost the command the user actually asked for.
const orphanSweepTimeout = 10 * time.Second

// sweepOrphanBoxes removes the boxes of THIS repo whose supervising host process is provably dead —
// a coop killed by SIGKILL (or a reboot mid-run) never fires `--rm`, and nothing else watches the
// box, so an orphan keeps a provider session burning tokens with no host-side observer. Cleanup is
// pull-only, so it rides the entry points that already reap: loop start, `fork start`, `fleet up`,
// and `coop build`'s recycle.
//
// Failure is silent by design. This is not the command the user ran: a broken runtime is about to
// be reported loudly by that command's own box work, and a runtime with no label inspection at all
// (Apple's container) must not print a line on every single start. `coop doctor` is where the state
// of the sweep is reported on demand.
func (a *app) sweepOrphanBoxes(repo string) {
	if repo == "" || a.sweptRepos[repo] {
		return // one sweep per repo per process: `fleet up` starts N forks through the same app
	}
	if a.sweptRepos == nil {
		a.sweptRepos = map[string]bool{}
	}
	a.sweptRepos[repo] = true
	if err := a.ensureRuntime(); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), orphanSweepTimeout)
	defer cancel()
	if n, _ := box.ReapOrphanBoxes(ctx, a.rt, repo); n > 0 {
		ui.Detail("removed %s whose coop process is gone", ui.Count(n, "orphaned box", "orphaned boxes"))
	}
}
