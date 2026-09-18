package cli

import (
	"context"
	"time"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/loop"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/ui"
)

// orphanSweepTimeout bounds the sweep. It is housekeeping on the way into real work — if the
// runtime is slow or wedged, giving up costs an orphan that the next run (or `coop doctor`) still
// sees, while blocking would cost the command the user actually asked for.
const orphanSweepTimeout = 10 * time.Second

// sweepOrphanBoxes removes the boxes of THIS repo whose supervising host process is provably dead —
// a coop killed by SIGKILL (or a reboot mid-run) never fires `--rm`, and nothing else watches the
// box, so an orphan keeps a provider session burning tokens with no host-side observer. Cleanup is
// pull-only, so it rides the entry points that already reap: loop start, fork start, and `coop
// build`'s recycle.
//
// Failure is silent by design. This is not the command the user ran: a broken runtime is about to
// be reported loudly by that command's own box work, and a runtime with no label inspection at all
// (Apple's container) must not print a line on every single start. `coop doctor` is where the state
// of the sweep is reported on demand.
func (a *app) sweepOrphanBoxes(repo string) {
	result := a.collectOrphanBoxes(repo)
	if result.RemovedBoxes > 0 {
		ui.Note("Removed %s whose Coop processes had stopped", ui.Count(result.RemovedBoxes, "box", "boxes"))
	}
	if result.RemovedNetworks > 0 {
		ui.Note("Removed %s", ui.Count(result.RemovedNetworks, "unused Coop network"))
	}
	noteSettledFilteredRuns(result.RecoveredFilteredRuns)
}

// noteSettledFilteredRuns is the line a launch prints for what settleInterruptedFilteredRuns settled.
func noteSettledFilteredRuns(settled int) {
	if settled > 0 {
		ui.Detail("recovered %s whose coop process is gone (coop net runs)", ui.Count(settled, "interrupted filtered run"))
	}
}

func (a *app) collectOrphanBoxes(repo string) loop.Preparation {
	var result loop.Preparation
	if repo == "" || a.sweptRepos[repo] {
		return result // one sweep per repo per process
	}
	if a.sweptRepos == nil {
		a.sweptRepos = map[string]bool{}
	}
	a.sweptRepos[repo] = true
	if err := a.ensureRuntime(); err != nil {
		return result
	}
	ctx, cancel := context.WithTimeout(context.Background(), orphanSweepTimeout)
	defer cancel()
	result.RemovedBoxes, _ = box.ReapOrphanBoxes(ctx, a.rt, repo)
	// Networks are not scoped to a repo — a coop project's leftover network from ANY workspace
	// eats one of Docker's ~31 subnets — so one pass per process covers them all.
	if a.sweptNetworks {
		return result
	}
	a.sweptNetworks = true
	result.RemovedNetworks, _ = box.ReapOrphanNetworks(ctx, a.rt)
	result.RecoveredFilteredRuns = a.settleInterruptedFilteredRuns()
	return result
}

// runBox is box.Run for a host launch that may be filtered. A filtered one first settles what earlier
// killed runs left behind — before its own gateway exists, so it can never touch it — and notes what
// it settled unless the launch runs quiet. The loop settles once at its start, through the sweep.
func (a *app) runBox(spec box.RunSpec) (int, error) {
	if spec.CapturedEgress != nil {
		if settled := a.settleInterruptedFilteredRuns(); !spec.Quiet {
			noteSettledFilteredRuns(settled)
		}
	}
	return box.Run(a.cfg, a.rt, spec)
}

// recoverNetworkRuns is box.RecoverNetworkRuns; a variable so tests can see which launches settle.
var recoverNetworkRuns = box.RecoverNetworkRuns

// settleInterruptedFilteredRuns finishes the cleanup of filtered runs whose coop died before its own
// teardown did — a SIGKILL, a crash, a supervisor's escalation. Their gateway containers and volumes
// carry coop.network.* labels, so the box sweep never sees them; recovery removes exactly what each
// run recorded, and only once its supervisor is provably gone. Loop, fork and build reach it through
// the sweep; a direct, editor or session launch through runBox and a fork's gate through forkctl's
// SettleFilteredRuns, and only when filtered: finding the pending runs reads every retained run
// record (0.1s at five hundred), which a filtered launch's gateway dwarfs and an open or offline
// launch must not pay. Once per process; it returns how many runs it settled.
func (a *app) settleInterruptedFilteredRuns() int {
	if a.settledNetworkRuns {
		return 0
	}
	a.settledNetworkRuns = true
	return settleNetworkRuns(a.rt)
}

// settleNetworkRuns is one bounded recovery pass over every pending run, counting the runs it
// settled whole. A failure is silent: the run stays pending and the next filtered launch asks again.
func settleNetworkRuns(rt runtime.Runtime) int {
	ctx, cancel := context.WithTimeout(context.Background(), orphanSweepTimeout)
	defer cancel()
	results, err := recoverNetworkRuns(ctx, rt, "")
	if err != nil {
		return 0
	}
	settled := 0
	for _, result := range results {
		if result.Skipped == "" && len(result.Pending) == 0 && len(result.Failures) == 0 {
			settled++
		}
	}
	return settled
}
