package forkctl

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/ui"
)

// DestroyFork stops the fork's sibling services, then removes the fork itself. Teardown is driven
// by the fork's own compose file, so it must run BEFORE the workspace goes: DownServices otherwise
// finds no file and silently no-ops, which is how removed forks left containers running for days
// (measured: a fork's keycloak + postgres still up five days after `fork rm`, holding disk the whole
// time). Volumes go with them — a fork is disposable by definition.
//
// Best effort: a service that refuses to stop must not block the removal the operator asked
// for, but it must not vanish silently either.
func DestroyFork(rt runtime.Runtime, repo, name string) error {
	if rt.Name != "" {
		ws := forkspace.Workspace(repo, name)
		if err := box.DownServices(rt, ws, repo, true, io.Discard, io.Discard); err != nil {
			ui.Info("fork %s: sibling services did not stop cleanly (%v) — check 'coop ps'", name, err)
		}
	}
	return forkspace.Destroy(repo, name)
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
	ws := forkspace.Workspace(repo, name)
	if !pathExists(ws) {
		return -1, fmt.Errorf("no such fork: %s", name)
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
	if err := ForkRmSafe(ForkUnmerged(repo, ws), gitDirty(ws), force); err != nil {
		return 1, err
	}
	// Confirm the (unrecoverable) delete — default-No at a TTY, refuse piped without --yes. Distinct
	// from --force above, which overrides the unmerged/dirty guard, not this prompt.
	if err := ui.DestroyGate("delete fork "+name, hasYes(args)); err != nil {
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
	if err := ForkRmSafe(ForkUnmerged(repo, ws), gitDirty(ws), force); err != nil {
		return 1, fmt.Errorf("fork %q changed while awaiting confirmation: %w", name, err)
	}
	if err := DestroyFork(c.rt, repo, name); err != nil {
		return -1, err
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
