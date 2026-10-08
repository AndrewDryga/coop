package forkctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/ui"
)

func (c *Control) forkMergeIsolatedCommand(repo, name string, identity forkspace.Identity, force, yes, skipEmpty bool) (int, error) {
	result, err := func() (mergeOutcome, error) {
		unlock, err := lockForkForMerge(repo, name)
		if err != nil {
			return mergeOutcome{}, err
		}
		defer unlock()
		current, isolated, err := isolatedForkIdentity(repo, name)
		if err != nil || !isolated || current != identity {
			return mergeOutcome{}, errors.Join(errors.New("isolated fork generation changed before merge"), err)
		}
		_, pending, err := readIsolatedIntent(repo, identity)
		if err != nil {
			return mergeOutcome{}, err
		}
		img := ""
		if !pending {
			if !yes && !ui.IsTerminal(os.Stdin) {
				return mergeOutcome{}, errors.New("coop fork merge: refusing new isolated publication in a non-interactive shell — pass --yes to confirm")
			}
			img, err = c.MergeGate(repo)
			if err != nil {
				return mergeOutcome{}, err
			}
		}
		return c.mergeIsolatedLocked(repo, img, name, identity, force, skipEmpty, func(candidate isolatedCandidate) bool {
			count, _ := observed(candidate.dir, "rev-list", "--count", candidate.parent.Head+".."+candidate.head)
			n, _ := strconv.Atoi(count)
			ui.Note("Publish isolated fork %s into %s", name, strings.TrimPrefix(candidate.parent.Target, "refs/heads/"))
			ui.Note("  %s · exact reviewed candidate %s", ui.Count(n, "commit"), candidate.head)
			if !approve("Publish these commits?", yes) {
				ui.Note("Cancelled. Parent source, index and refs are unchanged.")
				return false
			}
			return true
		})
	}()
	defer result.approval.close()
	if err != nil {
		if result.landed {
			ui.Note("Isolated Git publication landed; remaining finalization is journaled.")
		}
		return 1, err
	}
	if result.skipped {
		if skipEmpty {
			ui.Note("Skipped %s — no new commits or active task bookkeeping.", name)
		}
		return 0, nil
	}
	if !result.landed {
		return 1, errors.New("isolated publication did not land")
	}
	ui.OK("Published isolated fork %s", name)
	if err := result.approval.validateRemoval(repo, name); err != nil {
		ui.Note("Kept fork %s — %v", name, err)
		return 0, nil
	}
	if forkHasServices(repo, name) {
		ui.Note("Deletion also removes this fork's service containers, volumes and stored data.")
	}
	if ui.DestroyGate("Delete the merged fork", yes) != nil {
		ui.Note("Kept fork %s; future publication starts after its exact published source boundary.", name)
		return 0, nil
	}
	if err := destroyLandedFork(c.rt, repo, name, result.approval, box.ConfigExposureRoots(c.cfg)...); err != nil {
		return 1, err
	}
	ui.OK("Deleted fork %s", name)
	return 0, nil
}

func (c *Control) forkMergeIsolatedBatch(repo string, names []string, force, yes bool) (int, error) {
	if err := ui.DestroyGate("Merge and delete forks that land", yes); err != nil {
		return 2, ForkCancelled(err)
	}
	for _, name := range names {
		if forkspace.NeedsStop(repo, name) {
			ui.Note("Kept fork %s — stop its active worker or finish cleanup first.", name)
			continue
		}
		identity, isolated, err := isolatedForkIdentity(repo, name)
		if err != nil {
			return 1, err
		}
		if isolated {
			if code, err := c.forkMergeIsolatedCommand(repo, name, identity, force, true, true); code != 0 || err != nil {
				return code, err
			}
		} else {
			args := []string{name, "--yes"}
			if force {
				args = append(args, "--force")
			}
			if code, err := c.ForkMerge(args); code != 0 || err != nil {
				return code, err
			}
		}
	}
	return 0, nil
}

func (c *Control) forkReviewIsolated(repo, ws, name string, identity forkspace.Identity, stat, tool, open, gate bool) (int, error) {
	unlock, err := lockForkForMerge(repo, name)
	if err != nil {
		return 1, err
	}
	defer unlock()
	if pending, err := ForkHasPendingLand(repo, identity); err != nil {
		return 1, err
	} else if pending {
		return 1, fmt.Errorf("%s has a pending isolated publication; reconcile it with coop fork merge %s", name, name)
	}
	candidate, err := captureIsolatedCandidate(repo, name, identity, false, true)
	if err != nil {
		return 1, err
	}
	defer func() { candidate.cleanup() }()
	configured, err := c.gateFor(repo)
	if err != nil {
		return 1, err
	}
	outcome := forkReviewGateUnchecked
	if gate {
		img, err := c.MergeGate(repo)
		if err != nil {
			return 1, err
		}
		if img == "" {
			outcome = forkReviewGateNone
		} else if err := c.gateIsolatedCandidate(repo, img, candidate, true); err != nil {
			return 1, err
		} else {
			outcome = forkReviewGateGreen
		}
	}
	ref := "refs/heads/" + name
	if err := forkspace.GitRefCommand(context.Background(), candidate.dir, "update-ref", ref, candidate.head).Run(); err != nil {
		return 1, err
	}
	if err := forkspace.CheckoutIndependent(context.Background(), candidate.dir, candidate.parent.Head, ""); err != nil {
		return 1, err
	}
	ui.Note("Isolated review uses private committed custody; parent refs and model workspace remain unchanged.")
	into := strings.TrimPrefix(candidate.parent.Target, "refs/heads/")
	if into == "" {
		into = "detached parent " + candidate.parent.Head
	}
	if err := c.forkBriefAt(candidate.dir, repo, into, name, ref, outcome, len(configured) > 0); err != nil {
		return 1, err
	}
	uncommitted, err := observed(ws, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return 1, err
	}
	if uncommitted != "" {
		ui.Note("Uncommitted source changes are not in this review; commit intended work in the fork first.")
	}
	switch {
	case open:
		if err := forkspace.CheckoutIndependent(context.Background(), candidate.dir, candidate.head, ""); err != nil {
			return 1, err
		}
		if err := forkspace.HydrateLFS(context.Background(), candidate.dir, candidate.head); err != nil {
			return 1, err
		}
		preview := candidate.dir
		candidate.dir = "" // GUI tools may return before their window closes.
		ui.Note("Host editor opens retained committed preview %s, not the model workspace.", preview)
		ui.Note("Preview edits never become merge input; remove this private preview when finished.")
		return c.openInEditor(preview)
	case stat:
		return 0, nil
	case tool:
		if t := gitGlobalOut("diff.tool"); t != "" {
			return 0, gitInteractive(candidate.dir, "-c", "diff.tool="+t, "-c", "difftool."+t+".cmd="+gitGlobalOut("difftool."+t+".cmd"), "difftool", "HEAD..."+ref)
		}
	case c.cfg.ReviewCmd != "":
		preview, err := candidate.committedPreview(repo)
		if err != nil {
			return 1, err
		}
		comparison := candidate.dir
		candidate.dir = ""
		ui.Note("Configured host review tool uses retained committed preview %s.", preview)
		ui.Note("Comparison runs in %s; neither copy becomes merge input. Remove both when finished.", comparison)
		return c.runReviewCmd(comparison, preview, name, ref)
	}
	return 0, gitInteractive(candidate.dir, "diff", "--no-ext-diff", "HEAD..."+ref)
}
