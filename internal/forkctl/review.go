package forkctl

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/sessionsvc"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

// gitFetchInto fetches a fork's branch into review/<name> in the parent repo. The
// fetch is forced (+) because landing rebases the fork branch, so a re-fetch after a
// rebase is not a fast-forward of the prior review ref.
func gitFetchInto(repo, ws, name string) error {
	return gitRun(repo, "fetch", "--quiet", ws, "+"+name+":review/"+name)
}

// forkReviewCandidate is a disposable, rebased view of a fork. base remains the parent commit the
// clone captured; name is the candidate branch. The caller owns cleanup whenever dir is non-empty.
//
// sessionsvc.reviewScratch is its deliberate near-twin — same scaffold, opposite anchor: this one
// PREVIEWS against the parent's current HEAD, that one rebuilds a CAPTURED intent and verifies it.
// Assessed and kept separate; read .agent/kb/fork-review-scratch-two-copies.md before merging them.
type forkReviewCandidate struct {
	dir      string
	base     string
	name     string
	conflict bool
}

type forkReviewGateOutcome uint8

const (
	forkReviewGateUnchecked forkReviewGateOutcome = iota
	forkReviewGateNone
	forkReviewGateGreen
	forkReviewGateRed
	forkReviewGateConflict
)

func (o forkReviewGateOutcome) exitCode() int {
	if o == forkReviewGateRed || o == forkReviewGateConflict {
		return 1
	}
	return 0
}

func (c forkReviewCandidate) cleanup() { _ = os.RemoveAll(c.dir) }

func (c forkReviewCandidate) detachBase() error {
	return gitRun(c.dir, "checkout", "--quiet", "--detach", c.base)
}

// prepareForkReviewCandidate clones the parent's committed HEAD, fetches the fork's named branch,
// and rebases that branch in the scratch clone. Neither source repo is modified: local clone/fetch
// reads objects only, and every checkout/rebase occurs under c.dir. Preview rebases stay unsigned;
// signing changes commit identity, not the tree the gate checks, and must not invoke pinentry here.
func prepareForkReviewCandidate(repo, ws, name string) (c forkReviewCandidate, err error) {
	c.dir, err = os.MkdirTemp("", "coop-fork-review-")
	if err != nil {
		return c, err
	}
	keep := false
	defer func() {
		if !keep {
			c.cleanup()
			c = forkReviewCandidate{}
		}
	}()
	if err = forkspace.GitClone(repo, c.dir); err != nil {
		return c, fmt.Errorf("clone parent into review scratch: %w", err)
	}
	forkspace.PropagateGitIdentity(repo, c.dir)
	c.base = gitOut(c.dir, "rev-parse", "HEAD")
	if c.base == "" {
		return c, errors.New("review scratch has no parent HEAD")
	}
	// Detach before the forced fetch so a fork named after the parent's checked-out branch cannot
	// collide with Git's refusal to update the current branch.
	if err = c.detachBase(); err != nil {
		return c, fmt.Errorf("detach review scratch base: %w", err)
	}
	c.name = name
	if err = gitRun(c.dir, "fetch", "--quiet", ws, "+"+name+":refs/heads/"+name); err != nil {
		return c, fmt.Errorf("fetch fork into review scratch: %w", err)
	}
	if err = gitRun(c.dir, "rebase", c.base, name); err != nil {
		if abortErr := gitRun(c.dir, "rebase", "--abort"); abortErr != nil {
			return c, fmt.Errorf("rebase review scratch failed and abort failed: %v; %w", err, abortErr)
		}
		c.conflict = true
	}
	keep = true
	return c, nil
}

func (c *Control) ForkReview(args []string) (int, error) {
	name, stat, tool, open, gate := "", false, false, false, false
	for _, x := range args {
		switch x {
		case "--stat":
			stat = true
		case "--tool":
			tool = true
		case "--open":
			open = true
		case "--gate":
			gate = true
		default:
			if strings.HasPrefix(x, "-") {
				return 2, fmt.Errorf("coop fork review: unknown flag %q", x)
			}
			name = x
		}
	}
	if name == "" {
		return 2, errors.New("usage: coop fork review <name> [--stat | --tool | --open] [--gate]")
	}
	if !forkspace.ValidExistingName(name) {
		return 2, fmt.Errorf("invalid fork name %q", name)
	}
	if gate && open {
		return 2, errors.New("coop fork review: --gate cannot be combined with --open; use --stat or --tool so the review scratch can be removed reliably")
	}
	repo, err := box.ResolveRepo(c.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	ws := forkspace.Workspace(repo, name)
	if !pathExists(ws) {
		return -1, fmt.Errorf("no such fork: %s", name)
	}
	reviewRepo, ref := repo, "review/"+name
	outcome := forkReviewGateUnchecked
	if gate {
		candidate, err := prepareForkReviewCandidate(repo, ws, name)
		if err != nil {
			return -1, err
		}
		defer candidate.cleanup()
		reviewRepo, ref = candidate.dir, candidate.name
		pinnedCandidateHead := ""
		if candidate.conflict {
			outcome = forkReviewGateConflict
		} else {
			candidateHead, candidateTree, identityErr := sessionsvc.ReviewGitIdentity(candidate.dir, candidate.name)
			if identityErr != nil {
				return -1, fmt.Errorf("pin review scratch: %w", identityErr)
			}
			pinnedCandidateHead = candidateHead
			img, gateErr := c.MergeGate(repo)
			if gateErr != nil {
				return -1, gateErr
			}
			if img == "" {
				outcome = forkReviewGateNone
			} else {
				green, gateErr := c.ReviewGatePasses(repo, candidate.dir, img)
				if gateErr != nil {
					return -1, fmt.Errorf("run review gate: %w", gateErr)
				}
				if green {
					outcome = forkReviewGateGreen
				} else {
					outcome = forkReviewGateRed
				}
			}
			if !sessionsvc.ReviewCandidateUnchanged(candidate.dir, candidate.name, candidateHead, candidateTree) {
				outcome = forkReviewGateRed
				if err := gitRun(candidate.dir, "reset", "--hard", candidateHead); err != nil {
					return -1, fmt.Errorf("restore review scratch after gate mutation: %w", err)
				}
				if err := gitRun(candidate.dir, "clean", "-fd"); err != nil {
					return -1, fmt.Errorf("clean review scratch after gate mutation: %w", err)
				}
			}
		}
		// The candidate branch stays at its rebased tip while HEAD returns to the captured parent base,
		// preserving the existing HEAD...ref dossier/diff contract inside the scratch clone.
		if err := candidate.detachBase(); err != nil {
			return -1, fmt.Errorf("detach review scratch after gate: %w", err)
		}
		if outcome != forkReviewGateConflict {
			if err := gitRun(candidate.dir, "branch", "-f", candidate.name, pinnedCandidateHead); err != nil {
				return -1, fmt.Errorf("restore pinned review branch after gate: %w", err)
			}
		}
	} else if err := gitFetchInto(repo, ws, name); err != nil {
		return -1, fmt.Errorf("%s: git fetch: %w", name, err)
	}
	c.forkBrief(reviewRepo, ws, name, ref, outcome)
	if _, s := c.host.forkCost(ws); s != "" {
		ui.Info("cost: %s", s)
	}
	finish := func(code int, err error) (int, error) {
		if err != nil || code != 0 {
			return code, err
		}
		return outcome.exitCode(), nil
	}

	switch {
	case open: // open the fork in your IDE; review via its SCM panel
		return c.openInEditor(ws)
	case tool: // hand the diff to your GLOBAL git difftool (diff.tool), forced via -c so a
		// repo-poisoned diff.tool / difftool.<tool>.cmd can't run on `coop fork review --tool`.
		if t := gitGlobalOut("diff.tool"); t != "" {
			cargs := []string{"-c", "diff.tool=" + t}
			// Pin the tool's command from global too (empty neutralizes any repo override and lets
			// git use the built-in for a known tool), so the repo can't redirect even a named tool.
			cargs = append(cargs, "-c", "difftool."+t+".cmd="+gitGlobalOut("difftool."+t+".cmd"))
			_ = gitInteractive(reviewRepo, append(cargs, "difftool", "HEAD..."+ref)...)
		} else {
			ui.Note("no global git diff.tool set — showing the diff (--tool ignores repo config, for safety)")
			_ = gitInteractive(reviewRepo, "diff", "--no-ext-diff", "HEAD..."+ref) // internal diff (see default case)
		}
		return finish(0, nil)
	case stat:
		return finish(0, nil) // the brief already lists the files
	case c.cfg.ReviewCmd != "": // a user-defined review command
		return finish(c.runReviewCmd(reviewRepo, ws, name, ref))
	default:
		// --no-ext-diff: a broken diff.external / GIT_EXTERNAL_DIFF in the host environment would
		// otherwise make the diff "external diff died" (the -c diff.external= hardening blanks the
		// config but can't override the env var). Force git's internal diff so review always renders.
		_ = gitInteractive(reviewRepo, "diff", "--no-ext-diff", "HEAD..."+ref)
		return finish(0, nil)
	}
}

// resolveEditor picks the command used to open a fork for review, in order:
// $COOP_EDITOR, then your GLOBAL git core.editor, then a detected GUI editor, then
// $VISUAL/$EDITOR. Returns "" if nothing is configured or found. The editor is read from
// GLOBAL config only — never the agent-writable repo, which could otherwise point core.editor
// at a planted binary that runs on `coop fork review --open`.
func resolveEditor(cfgEditor string) string {
	if cfgEditor != "" {
		return cfgEditor
	}
	if e := gitGlobalOut("core.editor"); e != "" {
		return e // honor your global `git config core.editor`, e.g. "zed --wait"
	}
	return detectEditor()
}

// openInEditor opens the fork directory in an editor so you can review via its SCM
// panel. See resolveEditor for how the editor is chosen.
func (c *Control) openInEditor(ws string) (int, error) {
	editor := resolveEditor(c.cfg.Editor)
	if editor == "" {
		return 1, errors.New("no editor found — set $COOP_EDITOR, git config core.editor, or $VISUAL/$EDITOR (or install code/cursor/zed/idea)")
	}
	parts := append(strings.Fields(editor), ws)
	ui.Note("opening %s in %s", ws, parts[0])
	cmd := exec.Command(parts[0], parts[1:]...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	if err := cmd.Run(); err != nil {
		return 1, fmt.Errorf("couldn't launch your editor %q: %w — check $COOP_EDITOR / git core.editor", parts[0], err)
	}
	return 0, nil
}

// detectEditor finds a GUI editor on PATH (for opening a fork as a folder), falling
// back to $VISUAL/$EDITOR.
func detectEditor() string {
	for _, e := range []string{"cursor", "code", "zed", "idea", "subl"} {
		if _, err := exec.LookPath(e); err == nil {
			return e
		}
	}
	if v := os.Getenv("VISUAL"); v != "" {
		return v
	}
	return os.Getenv("EDITOR")
}

// runReviewCmd runs COOP_REVIEW_CMD via sh -c from the parent repo, with the fork's
// path/name/ref in the environment so the command can use them.
func (c *Control) runReviewCmd(repo, ws, name, ref string) (int, error) {
	cmd := exec.Command("sh", "-c", c.cfg.ReviewCmd)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"COOP_FORK_PATH="+ws, "COOP_FORK_NAME="+name, "COOP_REVIEW_REF="+ref)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	if err := cmd.Run(); err != nil {
		return 1, fmt.Errorf("COOP_REVIEW_CMD failed: %w", err)
	}
	return 0, nil
}

// forkBrief prints the review dossier before the diff — commits, the agent's claim,
// policy findings, risk-ordered files, and the gate status — so a reviewer gets a map
// of the risk before reading the patch. Everything except the task log is computed by
// the parent from git facts; the log is the fork's own voice and is labeled as such,
// so a fork can't steer its review via its narrative.
func (c *Control) forkBrief(repo, ws, name, ref string, gateOutcome forkReviewGateOutcome) {
	ins, del := parseShortstat(gitOut(repo, "diff", "--shortstat", "HEAD..."+ref))
	files := gitOut(repo, "diff", "--name-status", "HEAD..."+ref)
	nfiles := 0
	if files != "" {
		nfiles = len(strings.Split(files, "\n"))
	}
	ahead := gitOut(repo, "rev-list", "--count", "HEAD.."+ref)
	ui.Note("%s ← %s  ·  %s commit(s), +%d -%d across %d file(s)", ref, name, ahead, ins, del, nfiles)
	if log := gitOut(repo, "log", "--oneline", "--no-decorate", "HEAD.."+ref); log != "" {
		fmt.Println(ui.Bold("commits:"))
		fmt.Println(indent(log))
	}
	if why := tasks.LatestTaskLog(ws, 12); strings.TrimSpace(why) != "" {
		fmt.Println(ui.Bold("why (agent's claim — latest task log):"))
		fmt.Println(indent(why))
	} else {
		fmt.Printf("%s no completed task yet\n", ui.Bold("why:"))
	}
	if files != "" { // the sections below are diff-derived; an empty diff has nothing to map
		// The SAME scan `coop fork merge` enforces — printed here so findings surface at
		// review, not as a failed merge. Advisory: review's exit code stays 0.
		if warns := PolicyScan(repo, ref); len(warns) == 0 {
			fmt.Printf("%s %s nothing flagged\n", ui.Bold("policy:"), ui.Green("✓"))
		} else {
			fmt.Printf("%s %s %s — these block 'coop fork merge' without --force\n",
				ui.Bold("policy:"), ui.Yellow("⚠"), ui.Count(len(warns), "finding"))
			for _, w := range warns {
				fmt.Println(indent(w))
			}
		}
		fmt.Println(ui.Bold("files:"))
		for _, sec := range classifyChanged(files, gitOut(repo, "diff", "--numstat", "HEAD..."+ref)) {
			fmt.Println(indent(sec.title + ":"))
			for _, f := range sec.files {
				fmt.Println(indent(indent(f.render())))
			}
		}
		if gateOutcome == forkReviewGateUnchecked {
			if len(c.gateFor(repo)) == 0 {
				fmt.Printf("%s none configured (COOP_GATE or .agent/project.yaml gate:)\n", ui.Bold("gate:"))
			} else {
				fmt.Printf("%s runs at merge — rolled back on failure\n", ui.Bold("gate:"))
			}
		}
	}
	switch gateOutcome {
	case forkReviewGateNone:
		fmt.Printf("%s none configured — rebase clean\n", ui.Bold("gate:"))
	case forkReviewGateGreen:
		fmt.Printf("%s %s green on isolated rebased scratch\n", ui.Bold("gate:"), ui.Green("✓"))
	case forkReviewGateRed:
		fmt.Printf("%s %s red on isolated rebased scratch\n", ui.Bold("gate:"), ui.Red("✗"))
	case forkReviewGateConflict:
		fmt.Printf("%s %s conflict while rebasing onto current parent — gate not run\n", ui.Bold("gate:"), ui.Yellow("⚠"))
	}
	fmt.Println(ui.Bold("diff:"))
}
