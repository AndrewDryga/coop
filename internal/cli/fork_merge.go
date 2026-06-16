package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/ui"
)

// mergeGate resolves the box image when COOP_GATE is set (so a merge can be
// revalidated in the box), or returns "" when no gate is configured.
func (a *app) mergeGate(repo string) (string, error) {
	if len(a.cfg.Gate) == 0 {
		return "", nil
	}
	img := box.ImageForRepo(repo, a.cfg.BaseImage, a.cfg.ImageOverride)
	if !box.ImageExists(a.rt, img) {
		return "", fmt.Errorf("COOP_GATE is set but image %q isn't built — run 'coop build'", img)
	}
	return img, nil
}

// runGate runs COOP_GATE in the box against repo, reporting whether it passed.
func (a *app) runGate(repo, img string) bool {
	ui.Info("revalidating: %s", strings.Join(a.cfg.Gate, " "))
	code, _ := box.Run(a.cfg, a.rt, box.RunSpec{
		Image: img, Repo: repo, Cmd: a.cfg.Gate, Batch: true,
		Homes: a.cfg.Homes, Network: a.cfg.Network, Cache: a.cfg.Cache,
	})
	return code == 0
}

// mergeOne fetches a fork's branch, merges it into the parent's HEAD, and — when a
// gate is configured — revalidates the merged result, rolling back on failure.
// "green" thus means green against the tree as it stands now, not the stale base the
// fork was cut from. Reports whether the merge landed.
func (a *app) mergeOne(repo, img, name string) (bool, error) {
	ws := forkWorkspace(repo, name)
	if !pathExists(ws) {
		return false, fmt.Errorf("no such fork: %s", name)
	}
	if err := gitFetchInto(repo, ws, name); err != nil {
		return false, fmt.Errorf("%s: git fetch: %w", name, err)
	}
	ref := "review/" + name
	pre := gitOut(repo, "rev-parse", "HEAD")
	if err := gitRun(repo, "merge", "--no-edit", ref); err != nil {
		_ = gitRun(repo, "merge", "--abort")
		return false, fmt.Errorf("%s: merge conflicts — resolve in your tree, then `git merge %s`", name, ref)
	}
	if img != "" { // COOP_GATE configured
		if !a.runGate(repo, img) {
			_ = gitRun(repo, "reset", "--hard", pre)
			return false, fmt.Errorf("%s: gate failed after merge — rolled back", name)
		}
	}
	return true, nil
}

func (a *app) forkMerge(args []string) (int, error) {
	all, name := false, ""
	for _, x := range args {
		switch x {
		case "--all":
			all = true
		default:
			if strings.HasPrefix(x, "-") {
				return 2, fmt.Errorf("coop fork merge: unknown flag %q", x)
			}
			name = x
		}
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	if gitDirty(repo) {
		return 1, errors.New("your working tree has uncommitted changes — commit or stash before merging")
	}
	img, err := a.mergeGate(repo)
	if err != nil {
		return -1, err
	}
	if all {
		return a.forkMergeAll(repo, img)
	}
	if name == "" {
		return 2, errors.New("usage: coop fork merge <name> [--all]")
	}
	ws := forkWorkspace(repo, name)
	if !pathExists(ws) {
		return -1, fmt.Errorf("no such fork: %s", name)
	}
	if err := gitFetchInto(repo, ws, name); err != nil {
		return -1, fmt.Errorf("git fetch: %w", err)
	}
	ref := "review/" + name
	ahead := gitOut(repo, "rev-list", "--count", "HEAD.."+ref)
	ins, del := parseShortstat(gitOut(repo, "diff", "--shortstat", "HEAD..."+ref))
	ui.Info("merge %s into %s — %s commit(s), +%d -%d", ref, gitBranch(repo), ahead, ins, del)
	if !confirm("merge?", true) {
		return 0, nil
	}
	landed, err := a.mergeOne(repo, img, name)
	if err != nil {
		ui.Error("%v", err)
		return 1, nil
	}
	if !landed {
		return 1, nil
	}
	ui.Info("%s", ui.Green("✓ merged "+name))
	if confirm("remove the fork?", true) {
		if err := destroyFork(repo, name); err != nil {
			return -1, err
		}
		ui.Info("removed fork %s", name)
	}
	return 0, nil
}

// forkMergeAll lands every fork as a revalidating merge queue: each is merged onto
// the result of the previous one and re-gated, so a later fork can't ride in green
// against a base that an earlier merge already changed. It stops at the first
// conflict or gate failure, leaving the remaining forks untouched.
func (a *app) forkMergeAll(repo, img string) (int, error) {
	names := forkNames(repo)
	if len(names) == 0 {
		ui.Info("no forks to merge")
		return 0, nil
	}
	var landed []string
	for _, n := range names {
		ws := forkWorkspace(repo, n)
		if err := gitFetchInto(repo, ws, n); err != nil {
			continue
		}
		if gitOut(repo, "rev-list", "--count", "HEAD..review/"+n) == "0" {
			continue // nothing to land
		}
		ok, err := a.mergeOne(repo, img, n)
		if err != nil {
			ui.Error("%v", err)
			ui.Info("merge queue stopped at %s — %d landed, the rest left untouched", n, len(landed))
			return 1, nil
		}
		if ok {
			ui.Info("%s", ui.Green("✓ merged "+n))
			_ = destroyFork(repo, n)
			landed = append(landed, n)
		}
	}
	ui.Info("merge queue: %d landed", len(landed))
	return 0, nil
}
