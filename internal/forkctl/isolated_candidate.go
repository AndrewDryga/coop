package forkctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/hostsurface"
)

type isolatedCandidate struct {
	dir        string
	identity   forkspace.Identity
	parent     isolatedParent
	sourceHead string
	sourceTree string
	upstream   string
	head       string
	tree       string
}

func isolatedForkIdentity(repo, name string) (forkspace.Identity, bool, error) {
	identity, exists, err := forkspace.ReadGeneration(repo, name)
	if err != nil || !exists {
		return identity, false, err
	}
	isIsolated, err := forkspace.IsolatedGeneration(repo, identity)
	return identity, isIsolated, err
}

func (candidate isolatedCandidate) cleanup() {
	if candidate.dir != "" {
		_ = os.RemoveAll(candidate.dir)
	}
}

// Custom tools need published files without changing the base comparison's
// HEAD...ref contract. Both copies are retained because GUI tools can return early.
func (candidate isolatedCandidate) committedPreview(repo string) (_ string, err error) {
	preview, err := os.MkdirTemp(forkspace.StateDir(repo), ".isolated-preview-")
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(preview)
		}
	}()
	ctx := context.Background()
	if err = forkspace.GitClonePinnedContext(ctx, candidate.dir, preview, candidate.head); err != nil {
		return "", err
	}
	if err = forkspace.CheckoutIndependent(ctx, preview, candidate.head, ""); err != nil {
		return "", err
	}
	if err = forkspace.MaterializeIndependentTree(ctx, repo, preview, candidate.parent.Head, candidate.head, candidate.dir); err != nil {
		return "", err
	}
	return preview, nil
}

// Lifecycle admission excludes registered model writers. Native outside writers
// remain outside the boundary, so exact source identity is checked again afterward.
func prepareIsolatedCandidate(repo, name string, identity forkspace.Identity, sign bool) (_ isolatedCandidate, err error) {
	return captureIsolatedCandidate(repo, name, identity, sign, false)
}

func captureIsolatedCandidate(repo, name string, identity forkspace.Identity, sign, review bool) (_ isolatedCandidate, err error) {
	ws := forkspace.Workspace(repo, name)
	if err := forkspace.ValidateGenerationWorkspace(repo, identity); err != nil {
		return isolatedCandidate{}, err
	}
	if err := forkspace.ValidateIndependentGit(ws); err != nil {
		return isolatedCandidate{}, err
	}
	var parent isolatedParent
	if review {
		parent, err = captureIsolatedReviewBase(repo)
	} else {
		parent, err = captureIsolatedParent(repo)
	}
	if err != nil {
		return isolatedCandidate{}, err
	}
	sourceHead, err := observed(ws, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return isolatedCandidate{}, err
	}
	sourceTree, err := observed(ws, "rev-parse", "--verify", sourceHead+"^{tree}")
	if err != nil {
		return isolatedCandidate{}, err
	}
	upstream, err := isolatedSourceBoundary(repo, identity, parent.Head)
	if err != nil {
		return isolatedCandidate{}, err
	}
	if _, err := observed(ws, "merge-base", "--is-ancestor", upstream, sourceHead); err != nil {
		return isolatedCandidate{}, fmt.Errorf("isolated fork no longer descends from its published source boundary: %w", err)
	}
	if err := forkspace.EnsureStateDir(repo); err != nil {
		return isolatedCandidate{}, err
	}
	dir, err := os.MkdirTemp(forkspace.StateDir(repo), ".isolated-candidate-")
	if err != nil {
		return isolatedCandidate{}, err
	}
	candidate := isolatedCandidate{dir: dir, identity: identity, parent: parent,
		sourceHead: sourceHead, sourceTree: sourceTree, upstream: upstream}
	defer func() {
		if err != nil {
			candidate.cleanup()
		}
	}()
	ctx := context.Background()
	if err = forkspace.GitClonePinnedContext(ctx, repo, dir, parent.Head); err != nil {
		return isolatedCandidate{}, err
	}
	if err = forkspace.PropagateGitIdentity(repo, dir); err != nil {
		return isolatedCandidate{}, err
	}
	if err = forkspace.GitFetchPinnedContext(ctx, ws, dir, sourceHead); err != nil {
		return isolatedCandidate{}, err
	}
	if err = validateIsolatedHistory(dir, repo, upstream, sourceHead); err != nil {
		return isolatedCandidate{}, err
	}
	if err = forkspace.CheckoutIndependent(ctx, dir, sourceHead, "candidate"); err != nil {
		return isolatedCandidate{}, err
	}
	args := []string{}
	if sign {
		args = append(args, forkspace.TrustedSignArgs()...)
	}
	args = append(args, "rebase", "--onto", parent.Head, upstream, "candidate")
	command, commandErr := forkspace.GitCommandWithEnv(ctx, dir, forkspace.IndependentGitEnv(), args...)
	if commandErr != nil {
		return isolatedCandidate{}, commandErr
	}
	if output, rebaseErr := command.CombinedOutput(); rebaseErr != nil {
		return isolatedCandidate{}, fmt.Errorf("isolated private rebase failed; source and parent remain unchanged: %w\n"+
			"Retained source: %s\nPublished source boundary: %s\n"+
			"Review and rework only unpublished source commits after that boundary; preserve its ancestry. "+
			"Check trusted signing settings if signing failed. Retry `coop fork review %s` before merging.\n"+
			"Diagnostics below are from a discarded private clone; do not apply its recovery instructions to the retained source:\n%s",
			rebaseErr, ws, upstream, name, output)
	}
	candidate.head, err = observed(dir, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return isolatedCandidate{}, err
	}
	candidate.tree, err = observed(dir, "rev-parse", "--verify", "HEAD^{tree}")
	if err != nil {
		return isolatedCandidate{}, err
	}
	if err = validateIsolatedHistory(dir, repo, parent.Head, candidate.head); err != nil {
		return isolatedCandidate{}, err
	}
	if err = forkspace.MaterializeIndependentTree(ctx, repo, dir, parent.Head, candidate.head, ws); err != nil {
		return isolatedCandidate{}, err
	}
	if err = candidate.validateSource(repo); err != nil {
		return isolatedCandidate{}, err
	}
	return candidate, nil
}

func (candidate isolatedCandidate) validateSource(repo string) error {
	if err := forkspace.ValidateGenerationWorkspace(repo, candidate.identity); err != nil {
		return err
	}
	ws := forkspace.Workspace(repo, candidate.identity.Name)
	if err := forkspace.ValidateIndependentGit(ws); err != nil {
		return err
	}
	head, err := observed(ws, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || head != candidate.sourceHead {
		return errors.Join(errors.New("isolated source no longer matches the captured review; preserve any publication journal/custody"), err)
	}
	return nil
}

// Scan every reachable incoming commit, including add-then-remove attacks. A
// terminal diff cannot grant permission to import unsafe intermediate history.
func validateIsolatedHistory(repo, authority, upstream, head string) error {
	shadowed := box.NewShadowDecider(authority)
	commits, err := observed(repo, "rev-list", "--reverse", upstream+".."+head)
	if err != nil {
		return err
	}
	for _, commit := range strings.Fields(commits) {
		after, err := isolatedTree(repo, commit)
		if err != nil {
			return err
		}
		parents, err := observed(repo, "rev-list", "--parents", "-n", "1", commit)
		if err != nil {
			return err
		}
		fields := strings.Fields(parents)
		if len(fields) < 2 {
			return errors.New("isolated incoming history has no qualified parent")
		}
		before, err := isolatedTree(repo, fields[1])
		if err != nil {
			return err
		}
		old := map[string]isolatedTreeEntry{}
		for _, entry := range before {
			old[entry.path] = entry
		}
		for _, entry := range after {
			previous, present := old[entry.path]
			delete(old, entry.path)
			if present && previous == entry {
				continue
			}
			if entry.mode == "160000" || previous.mode == "160000" || filepath.Base(isolatedPathKey(entry.path)) == ".gitmodules" {
				return fmt.Errorf("isolated publication refuses changed nested Git sources at %q", entry.path)
			}
			if reason, automatic := hostsurface.Classify("M", isolatedHostSurfacePath(entry.path)); automatic {
				return fmt.Errorf("isolated publication refuses %q: %s; --force cannot bypass this boundary", entry.path, reason)
			}
			if shadowed(isolatedPathKey(entry.path)) {
				return fmt.Errorf("isolated publication refuses secret-like file %q; --force cannot bypass this boundary", entry.path)
			}
			sizeText, err := observed(repo, "cat-file", "-s", entry.object)
			if err != nil {
				return err
			}
			size, err := strconv.ParseInt(sizeText, 10, 64)
			if err != nil || size < 0 || size > 5<<20 {
				return errors.Join(fmt.Errorf("isolated publication refuses oversized or uninspectable blob at %q", entry.path), err)
			}
			content, err := observed(repo, "cat-file", "blob", entry.object)
			if err != nil {
				return err
			}
			if filepath.Base(isolatedPathKey(entry.path)) == "package.json" {
				if entry.mode == "120000" || !json.Valid([]byte(content)) {
					return fmt.Errorf("isolated publication cannot inspect package lifecycle scripts at %q", entry.path)
				}
				oldContent := ""
				if present && previous.mode != "120000" {
					oldSize, err := observed(repo, "cat-file", "-s", previous.object)
					if err != nil {
						return err
					}
					n, err := strconv.ParseInt(oldSize, 10, 64)
					if err != nil || n < 0 || n > 5<<20 {
						return errors.Join(errors.New("previous package lifecycle scripts are uninspectable"), err)
					}
					oldContent, err = observed(repo, "cat-file", "blob", previous.object)
					if err != nil {
						return err
					}
				}
				if script := changedLifecycleScript(oldContent, content); script != "" {
					return fmt.Errorf("isolated publication refuses new or changed automatic %s script at %q; --force cannot bypass this boundary", script, entry.path)
				}
			}
			if strings.IndexByte(content, 0) < 0 && len(box.ScanSecrets(content)) != 0 {
				return fmt.Errorf("isolated publication refuses possible credentials at %q; remove real credentials before review", entry.path)
			}
		}
		for _, entry := range old {
			if entry.mode == "160000" || filepath.Base(isolatedPathKey(entry.path)) == ".gitmodules" {
				return fmt.Errorf("isolated publication refuses changed nested Git sources at %q", entry.path)
			}
		}
	}
	return nil
}

func (c *Control) gateIsolatedCandidate(repo, img string, candidate isolatedCandidate, review bool) error {
	if img == "" {
		return nil
	}
	dir, err := os.MkdirTemp("", "coop-isolated-gate-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	ctx := context.Background()
	if err := forkspace.GitClonePinnedContext(ctx, candidate.dir, dir, candidate.head); err != nil {
		return err
	}
	if err := forkspace.CheckoutIndependent(ctx, dir, candidate.head, "candidate"); err != nil {
		return err
	}
	if err := forkspace.MaterializeIndependentTree(ctx, repo, dir, candidate.parent.Head, candidate.head, candidate.dir); err != nil {
		return err
	}
	base := forkspace.GitRefCommand(ctx, dir, "update-ref", "refs/coop/session-parent", candidate.parent.Head)
	base.Env = forkspace.IndependentGitEnv()
	if err := base.Run(); err != nil {
		return err
	}
	var green bool
	if c.gateOK != nil {
		green = c.gateOK(repo, dir, img)
	} else {
		green, err = c.runGateBoundary(repo, dir, img, review, repo)
	}
	if err != nil || !green {
		return errors.Join(errors.New("isolated candidate gate failed; parent and model source unchanged"), err)
	}
	state, err := captureIsolatedParent(dir)
	if err != nil || state.Head != candidate.head || state.Tree != candidate.tree {
		return errors.Join(errors.New("isolated gate changed the checked candidate; approval refused"), err)
	}
	return candidate.validateSource(repo)
}
