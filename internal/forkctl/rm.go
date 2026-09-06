package forkctl

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

// RecoverOrphanedGenerationLocked removes only the exact host generation left by a crash after
// workspace destruction. The caller holds LockState(repo,name). Any surviving worker, sandbox,
// session reservation, task authority, land journal, or review branch makes the state ambiguous
// and therefore non-recoverable without its owning workflow.
func RecoverOrphanedGenerationLocked(repo, name string, force bool) (bool, error) {
	if pathExists(forkspace.Workspace(repo, name)) {
		return false, nil
	}
	identity, ok, err := forkspace.ReadGeneration(repo, name)
	if err != nil || !ok {
		return false, err
	}
	if err := CheckWorkerStateFormat(repo, name); err != nil {
		return false, err
	}
	if forkspace.NeedsStop(repo, name) {
		return false, errors.New("orphaned fork generation still has worker cleanup state")
	}
	if gitOut(repo, "show-ref", "--hash", "refs/heads/review/"+name) != "" {
		return false, errors.New("missing fork workspace still has a review branch; recover or inspect that Git work before removing its generation")
	}
	if _, pending, err := readLandIntent(repo, identity); err != nil {
		return false, err
	} else if pending {
		return false, errors.New("missing fork workspace has an interrupted land journal")
	}
	if err := forkspace.RequireNoForkExecutionsLocked(repo, identity); err != nil {
		return false, err
	}
	if err := forkspace.RequireNoWorkspaceReservationLocked(repo, identity); err != nil {
		return false, err
	}
	if active, err := tasks.ForkTaskState(repo, identity); err != nil {
		return false, err
	} else if active {
		// The workspace is gone but the fork still owns canonical tasks (an assignment, a reviewed
		// candidate). Without --force that is a stop; with it, the journaled discard returns the
		// tasks to the queue exactly as it does for a present workspace.
		if !force {
			return false, errors.New("missing fork workspace still owns canonical task authority — use --force to return its tasks to the queue")
		}
		if err := tasks.DiscardForkTaskStateLocked(repo, identity); err != nil {
			return false, fmt.Errorf("return the missing fork's canonical assignments: %w", err)
		}
	}
	if err := forkspace.RemoveGenerationIfMatchesLocked(repo, identity); err != nil {
		return false, err
	}
	return true, nil
}

// DestroyFork stops the fork's sibling services, then removes the fork itself. Teardown is driven
// by the fork's own compose file, so it must run BEFORE the workspace goes: DownServices otherwise
// finds no file and silently no-ops, which is how removed forks left containers running for days
// (measured: a fork's keycloak + postgres still up five days after `fork rm`, holding disk the whole
// time). Volumes go with them — a fork is disposable by definition.
//
// Best effort: a service that refuses to stop must not block the removal the operator asked
// for, but it must not vanish silently either.
func DestroyFork(rt runtime.Runtime, repo, name string, exposedRoots ...string) error {
	stopForkServices(rt, repo, name, exposedRoots...)
	return forkspace.Destroy(repo, name)
}

func stopForkServices(rt runtime.Runtime, repo, name string, exposedRoots ...string) {
	if rt.Name != "" {
		ws := forkspace.Workspace(repo, name)
		if err := box.DownServices(rt, ws, repo, true, io.Discard, io.Discard, exposedRoots...); err != nil {
			ui.Info("fork %s: sibling services did not stop cleanly (%v) — check 'coop ps'", name, err)
		}
	}
}

// oneForkName returns the single fork name from the parsed positionals, rejecting a second one. The
// rm/merge/stop/logs families used to let a later positional silently overwrite the first — acting on
// only the last and printing success — a data-loss footgun (`fork rm a b` looks like it removed both).
// Zero positionals returns "" so callers can apply their own "name required" usage error.
func oneForkName(verb string, pos []string) (string, error) {
	if len(pos) > 1 {
		return "", fmt.Errorf("coop fork %s takes one name (got %s)", verb, strings.Join(pos, ", "))
	}
	if len(pos) == 0 {
		return "", nil
	}
	return pos[0], nil
}

// ForkRmSafe is the guard for `rm`: never silently drop an agent's work.
func ForkRmSafe(unmerged, dirty, force bool) error {
	if force {
		return nil
	}
	if dirty {
		return errors.New("fork has uncommitted changes — use --force to discard")
	}
	if unmerged {
		return errors.New("fork has unmerged commits — merge it first, or use --force")
	}
	return nil
}

func ForkDestroyDescription(name string, dirty, unmerged bool, summary tasks.ForkTaskStateSummary) string {
	parts := []string{"delete fork " + name}
	if dirty {
		parts = append(parts, "discard uncommitted files")
	}
	if unmerged {
		parts = append(parts, "discard unmerged commits")
	}
	if summary.Assignments > 0 {
		parts = append(parts, fmt.Sprintf("return %d canonical task assignment(s) to the project queue", summary.Assignments))
	}
	if summary.Candidate {
		parts = append(parts, "discard its reviewed merge candidate")
	}
	if pending := summary.PreparedProposals + summary.PendingProposals; pending > 0 {
		parts = append(parts, fmt.Sprintf("discard %d not-yet-imported task proposal(s)", pending))
	}
	if summary.ImportedReceipts > 0 {
		parts = append(parts, fmt.Sprintf("retain %d imported canonical task(s) and retire their fork receipts", summary.ImportedReceipts))
	}
	return strings.Join(parts, "; ")
}

// ForkUnmerged reports whether the fork's branch tip is NOT yet an ancestor of the
// parent repo's HEAD (unknown-to-parent counts as unmerged, which is the safe side).
func ForkUnmerged(repo, ws string) bool {
	sha := gitOut(ws, "rev-parse", "HEAD")
	if sha == "" {
		return false
	}
	return gitRun(repo, "merge-base", "--is-ancestor", sha, "HEAD") != nil
}

func (c *Control) ForkRm(args []string) (int, error) {
	force := false
	var pos []string
	for _, x := range args {
		switch x {
		case "--force", "-f":
			force = true
		case "--yes", "-y": // accepted so `--yes` skips the confirm below (read via hasYes)
		default:
			if strings.HasPrefix(x, "-") {
				return 2, fmt.Errorf("coop fork rm: unknown flag %q", x)
			}
			pos = append(pos, x)
		}
	}
	name, err := oneForkName("rm", pos)
	if err != nil {
		return 2, err
	}
	if name == "" {
		return 2, errors.New("usage: coop fork rm <name> [--force] [--yes]")
	}
	if !forkspace.ValidExistingName(name) {
		return 2, fmt.Errorf("invalid fork name %q", name)
	}
	repo, err := box.ResolveRepo(c.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	if err := CheckWorkerStateFormat(repo, name); err != nil {
		return 1, err
	}
	ws := forkspace.Workspace(repo, name)
	if !pathExists(ws) {
		orphanedIdentity, orphaned, generationErr := forkspace.ReadGeneration(repo, name)
		if generationErr != nil {
			return 1, fmt.Errorf("read missing fork %q generation state: %w", name, generationErr)
		}
		if !orphaned {
			return -1, fmt.Errorf("no such fork: %s", name)
		}
		if err := ui.DestroyGate("remove orphaned fork state "+name, hasYes(args)); err != nil {
			return 2, err
		}
		unlock, err := forkspace.LockState(repo, name)
		if err != nil {
			return -1, err
		}
		currentIdentity, stillOrphaned, currentErr := forkspace.ReadGeneration(repo, name)
		if currentErr != nil || !stillOrphaned || currentIdentity != orphanedIdentity {
			unlock()
			return 1, errors.Join(currentErr, fmt.Errorf("orphaned fork %q changed while awaiting confirmation", name))
		}
		recovered, recoverErr := RecoverOrphanedGenerationLocked(repo, name, force)
		unlock()
		if recoverErr != nil {
			return 1, fmt.Errorf("recover missing fork %q: %w", name, recoverErr)
		}
		if !recovered {
			return -1, fmt.Errorf("no such fork: %s", name)
		}
		ui.OK("removed orphaned fork state %s", name)
		return 0, nil
	}
	handle, originalWS, err := forkspace.Pin(ws)
	if err != nil {
		return -1, fmt.Errorf("open fork %s before removal: %w", name, err)
	}
	defer handle.Close()
	// A running loop has the worktree bind-mounted RW; deleting it would orphan the worker +
	// container and strand the pidfile. Refuse (like merge/prune do) — or with --force, stop the
	// loop first so its container is reaped before the worktree goes.
	needsStop := forkspace.NeedsStop(repo, name)
	if needsStop && !force {
		return 1, fmt.Errorf("fork %q is running or awaiting cleanup — stop it first: coop fork stop %s (or use --force)", name, name)
	}
	initialUnmerged, initialDirty := ForkUnmerged(repo, ws), gitDirty(ws)
	if err := ForkRmSafe(initialUnmerged, initialDirty, force); err != nil {
		return 1, err
	}
	initialIdentity, hasGeneration, err := forkspace.ReadGeneration(repo, name)
	if err != nil {
		return 1, err
	}
	var initialTaskState tasks.ForkTaskStateSummary
	if hasGeneration {
		initialTaskState, err = tasks.ReadForkTaskStateSummary(repo, initialIdentity)
		if err != nil {
			return 1, err
		}
		if initialTaskState.Active() && !force {
			return 1, fmt.Errorf("fork %q still owns canonical task assignments, a reviewed candidate, proposals, or cleanup state — merge it, or use --force to resolve them before deletion", name)
		}
	}
	// Confirm the (unrecoverable) delete — default-No at a TTY, refuse piped without --yes. Distinct
	// from --force above, which overrides the unmerged/dirty guard, not this prompt.
	if err := ui.DestroyGate(ForkDestroyDescription(name, initialDirty, initialUnmerged, initialTaskState), hasYes(args)); err != nil {
		return 2, err
	}
	if needsStop {
		if code, err := c.ForkStop([]string{name}); err != nil {
			return code, err
		}
	}
	unlock, err := forkspace.LockState(repo, name)
	if err != nil {
		return -1, fmt.Errorf("lock fork %s state: %w", name, err)
	}
	defer unlock()
	// State may change while the confirmation prompt is open. Re-check under the same lock used by
	// detached startup so a newly-starting worker cannot lose its workspace underneath it.
	if !pathExists(ws) {
		return 1, fmt.Errorf("fork %q changed while awaiting confirmation — it no longer exists", name)
	}
	if !forkspace.SamePinned(ws, originalWS) {
		return 1, fmt.Errorf("fork %q was replaced while awaiting confirmation", name)
	}
	if forkspace.NeedsStop(repo, name) {
		return 1, fmt.Errorf("fork %q started while awaiting confirmation — stop it first: coop fork stop %s", name, name)
	}
	currentUnmerged, currentDirty := ForkUnmerged(repo, ws), gitDirty(ws)
	if currentUnmerged != initialUnmerged || currentDirty != initialDirty {
		return 1, fmt.Errorf("fork %q Git work changed while awaiting confirmation — retry to review the new deletion impact", name)
	}
	if err := ForkRmSafe(currentUnmerged, currentDirty, force); err != nil {
		return 1, fmt.Errorf("fork %q changed while awaiting confirmation: %w", name, err)
	}
	identity, hasGenerationNow, err := forkspace.ReadGeneration(repo, name)
	if err != nil {
		return 1, err
	}
	if hasGenerationNow {
		if !hasGeneration || identity != initialIdentity {
			return 1, fmt.Errorf("fork %q generation changed while awaiting confirmation", name)
		}
		if _, pendingLand, err := readLandIntent(repo, identity); err != nil {
			return 1, err
		} else if pendingLand {
			return 1, fmt.Errorf("fork %q has an interrupted land journal — rerun 'coop fork merge %s' before removal", name, name)
		}
		if err := forkspace.RequireNoForkExecutionsLocked(repo, identity); err != nil {
			return 1, fmt.Errorf("fork %q has sandbox activity: %w", name, err)
		}
		if err := forkspace.RequireNoWorkspaceReservationLocked(repo, identity); err != nil {
			return 1, err
		}
		currentTaskState, err := tasks.ReadForkTaskStateSummary(repo, identity)
		if err != nil {
			return 1, err
		}
		if currentTaskState.Fingerprint != initialTaskState.Fingerprint {
			return 1, fmt.Errorf("fork %q task authority changed while awaiting confirmation — retry to review the new deletion impact", name)
		}
		if currentTaskState.Active() && !force {
			return 1, fmt.Errorf("fork %q acquired canonical task work while awaiting confirmation", name)
		}
		if currentTaskState.Active() {
			if err := tasks.DiscardForkTaskStateLocked(repo, identity); err != nil {
				return 1, fmt.Errorf("return fork %s canonical assignments before removal: %w", name, err)
			}
		}
	}
	if err := DestroyFork(c.rt, repo, name, box.ConfigExposureRoots(c.cfg)...); err != nil {
		return -1, err
	}
	if hasGenerationNow {
		if err := forkspace.RemoveGenerationIfMatchesLocked(repo, identity); err != nil {
			return -1, fmt.Errorf("remove fork %s generation: %w", name, err)
		}
	}
	ui.OK("removed fork %s", name)
	return 0, nil
}

// forkOpen prints a fork's path (for `cd "$(coop fork open <name>)"`).
// ForkPath prints a fork's filesystem path (for `cd "$(coop fork path <name>)"` and the
// like). It's the plumbing companion to `coop fork open`, which opens it in your editor.
func (c *Control) ForkPath(args []string) (int, error) {
	if len(args) == 0 || args[0] == "" {
		return 2, errors.New("usage: coop fork path <name>")
	}
	name := args[0]
	if !forkspace.ValidExistingName(name) {
		return 2, fmt.Errorf("invalid fork name %q", name)
	}
	repo, err := box.ResolveRepo(c.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	ws := forkspace.Workspace(repo, name)
	if !pathExists(ws) {
		return -1, fmt.Errorf("no such fork: %s", name)
	}
	fmt.Println(ws)
	return 0, nil
}

// ForkOpenEditor opens a fork in your editor (see resolveEditor for how it's chosen) so
// you can work in or eyeball it on the host. Opening is a host-side action, so it
// doesn't need the box image built.
func (c *Control) ForkOpenEditor(args []string) (int, error) {
	if len(args) == 0 || args[0] == "" {
		return 2, errors.New("usage: coop fork open <name>")
	}
	name := args[0]
	if !forkspace.ValidExistingName(name) {
		return 2, fmt.Errorf("invalid fork name %q", name)
	}
	repo, err := box.ResolveRepo(c.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	ws := forkspace.Workspace(repo, name)
	if !pathExists(ws) {
		return -1, fmt.Errorf("no such fork: %s", name)
	}
	return c.openInEditor(ws)
}
