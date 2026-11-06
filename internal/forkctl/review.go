package forkctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/gitdiff"
	"github.com/AndrewDryga/coop/internal/hostsurface"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/sessionsvc"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

// gitFetchInto fetches a fork's branch into review/<name> in the parent repo. The
// fetch is forced (+) because landing rebases the fork branch, so a re-fetch after a
// rebase is not a fast-forward of the prior review ref.
func gitFetchInto(repo, ws, name string) error {
	return forkspace.GitRun(repo, "fetch", "--quiet", ws, "+"+name+":review/"+name)
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
	tip      string // the fork's own tip, before the rebase gave its commits ids that exist only here
	conflict bool
}

type forkReviewGateOutcome uint8

const (
	forkReviewGateUnchecked forkReviewGateOutcome = iota
	forkReviewGateNone
	forkReviewGateGreen
	forkReviewGateRed
	forkReviewGateConflict
	forkReviewGateChanged // the checks passed, and left the temporary copy different from the fork's commits
)

func (o forkReviewGateOutcome) exitCode() int {
	if o == forkReviewGateRed || o == forkReviewGateConflict || o == forkReviewGateChanged {
		return 1
	}
	return 0
}

// forkReviewGate is what the dossier says of the project's checks: how they ended, whether the
// project sets any, and the files they changed when that is how they ended.
type forkReviewGate struct {
	outcome    forkReviewGateOutcome
	configured bool
	changed    []string
}

func (c forkReviewCandidate) cleanup() { _ = os.RemoveAll(c.dir) }

func (c forkReviewCandidate) detachBase() error {
	return forkspace.GitDetach(context.Background(), c.dir, c.base)
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
	if err = forkspace.PropagateGitIdentity(repo, c.dir); err != nil {
		return c, fmt.Errorf("prepare review scratch Git identity: %w", err)
	}
	c.base = forkspace.GitOut(c.dir, "rev-parse", "HEAD")
	if c.base == "" {
		return c, errors.New("review scratch has no parent HEAD")
	}
	// Detach before the forced fetch so a fork named after the parent's checked-out branch cannot
	// collide with Git's refusal to update the current branch.
	if err = c.detachBase(); err != nil {
		return c, fmt.Errorf("detach review scratch base: %w", err)
	}
	c.name = name
	if err = forkspace.GitRun(c.dir, "fetch", "--quiet", ws, "+"+name+":refs/heads/"+name); err != nil {
		return c, fmt.Errorf("fetch fork into review scratch: %w", err)
	}
	if c.tip = forkspace.GitOut(c.dir, "rev-parse", "--verify", "refs/heads/"+name); c.tip == "" {
		return c, errors.New("review scratch has no fork tip")
	}
	if err = forkspace.GitRun(c.dir, "rebase", c.base, name); err != nil {
		if abortErr := forkspace.GitRun(c.dir, "rebase", "--abort"); abortErr != nil {
			return c, fmt.Errorf("rebase review scratch failed and abort failed: %v; %w", err, abortErr)
		}
		c.conflict = true
	}
	keep = true
	return c, nil
}

func (c *Control) ForkReview(args []string) (int, error) {
	const usage = "coop fork review <name> [<options>]"
	stat, tool, open, gate := false, false, false, false
	var pos []string
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
				return 2, ui.UnknownOptionAmong(x, "coop fork review", []string{"--stat", "--tool", "--open", "--gate"})
			}
			pos = append(pos, x)
		}
	}
	name, err := oneForkName("review", usage, pos)
	if err != nil {
		return 2, err
	}
	if name == "" {
		return 2, ui.MissingArgument("fork name", "coop fork review", usage)
	}
	if !forkspace.ValidExistingName(name) {
		return 2, InvalidForkName(name, "coop fork review")
	}
	if gate && open {
		conflict := ui.ConflictingOptions("--gate", "--open", "coop fork review")
		conflict.Cause = "Use --gate with --stat or --tool, so Coop can remove the review's temporary copy reliably."
		return 2, conflict
	}
	repo, err := box.ResolveRepo(c.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	ws := forkspace.Workspace(repo, name)
	if !pathExists(ws) {
		return 2, NoSuchFork(name)
	}
	if identity, isolated, err := isolatedForkIdentity(repo, name); err != nil {
		return 1, err
	} else if isolated {
		return c.forkReviewIsolated(repo, ws, name, identity, stat, tool, open, gate)
	}
	gateCommand, err := c.gateFor(repo)
	if err != nil {
		return -1, err
	}
	gateConfigured := len(gateCommand) > 0
	reviewRepo, ref := repo, "review/"+name
	commits := "HEAD.." + ref // the fork's own commits, which a person can look up afterwards
	outcome := forkReviewGateUnchecked
	var changed []string
	if gate {
		candidate, err := prepareForkReviewCandidate(repo, ws, name)
		if err != nil {
			return -1, err
		}
		defer candidate.cleanup()
		reviewRepo, ref = candidate.dir, candidate.name
		// The copy's commits are the fork's rebased onto the parent, with new ids that exist nowhere
		// once the copy is gone, so the dossier lists the fork's own, as the review without --gate does.
		commits = candidate.base + ".." + candidate.tip
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
				// A red gate is red whatever else it wrote. Checks that passed and still changed the copy
				// did not check the fork's commits, and which files they changed is the one clue to why.
				if outcome == forkReviewGateGreen {
					outcome = forkReviewGateChanged
					changed = reviewGateChanges(candidate.dir)
				} else {
					outcome = forkReviewGateRed
				}
				if err := forkspace.GitRun(candidate.dir, "reset", "--hard", candidateHead); err != nil {
					return -1, fmt.Errorf("restore review scratch after gate mutation: %w", err)
				}
				if err := forkspace.GitRun(candidate.dir, "clean", "-fd"); err != nil {
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
			if err := forkspace.GitRun(candidate.dir, "branch", "-f", candidate.name, pinnedCandidateHead); err != nil {
				return -1, fmt.Errorf("restore pinned review branch after gate: %w", err)
			}
		}
	} else if err := gitFetchInto(repo, ws, name); err != nil {
		return -1, fmt.Errorf("%s: git fetch: %w", name, err)
	}
	uncommitted, err := forkspace.GitOutErr(ws, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return -1, fmt.Errorf("inspect fork worktree: %w", err)
	}
	// Into names the parent's branch: in the gated copy HEAD is detached, so it could only say "HEAD".
	if err := c.forkBriefAt(reviewRepo, reviewRepo, gitBranch(repo), commits, name, ref,
		forkReviewGate{outcome: outcome, configured: gateConfigured, changed: changed}); err != nil {
		return -1, err
	}
	if uncommitted != "" {
		fmt.Println(ui.AlertBlock(ui.For(os.Stdout), "Uncommitted fork changes",
			"Uncommitted changes are omitted from this review's counts and committed diff. Open the fork, commit intended work, then review again.",
			[2]string{"Open fork:", "coop fork open " + name}))
	}
	if _, s := c.host.forkCost(ws); s != "" {
		ui.Note("  Reported cost: %s", s)
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
			if err := gitInteractive(reviewRepo, append(cargs, "difftool", "HEAD..."+ref)...); err != nil {
				return 1, err
			}
		} else {
			ui.Note("No global Git diff tool is configured. Showing the diff here.")
			if err := gitInteractive(reviewRepo, "diff", "--no-ext-diff", "HEAD..."+ref); err != nil {
				return 1, err
			} // internal diff (see default case)
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
		if err := gitInteractive(reviewRepo, "diff", "--no-ext-diff", "HEAD..."+ref); err != nil {
			return 1, err
		}
		return finish(0, nil)
	}
}

// reviewGateChanges names the files a gate's checks left changed in the review copy: tracked files
// that differ from the commit, and files they added that no ignore rule hides. It reads before the
// copy is reset. An unreadable status names nothing, because the review fails for the change itself.
func reviewGateChanges(dir string) []string {
	out, err := forkspace.GitOutput(dir, "status", "--porcelain", "-z", "--untracked-files=all")
	if err != nil {
		return nil
	}
	var names []string
	entries := strings.Split(out, "\x00")
	for i := 0; i < len(entries); i++ {
		entry := entries[i]
		if len(entry) < 4 {
			continue
		}
		names = append(names, entry[3:])
		if strings.ContainsAny(entry[:2], "RC") {
			i++ // a rename or copy lists the path it came from as an entry of its own
		}
	}
	return names
}

// changedFilesPhrase says what the checks changed as the end of a sentence about them, naming up to
// three files. With none to name, the change was to the copy's Git state rather than to a file.
func changedFilesPhrase(changed []string) string {
	if len(changed) == 0 {
		return "left it different from the fork's commits"
	}
	const shown = 3
	names := make([]string, 0, shown+1)
	for _, path := range changed[:min(len(changed), shown)] {
		names = append(names, ui.SafeInline(path))
	}
	if more := len(changed) - len(names); more > 0 {
		names = append(names, ui.Count(more, "more file"))
	}
	return "changed " + ui.List(names, "and")
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
	ui.Note("Opening fork %s in %s", filepath.Base(ws), filepath.Base(parts[0]))
	ui.Note("  %s", ws)
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

// forkBrief prints the review dossier before the diff — what changed and where it is going, the
// commits, the agent's own account of the work, the files, and what the project's checks have (or
// have not) said about it. Everything except the task notes is computed by the parent from git
// facts; the notes are the fork's own voice and are labeled as such, so a fork can't steer its
// review through its narrative. It shares stdout with the diff that follows, so a redirected
// review keeps the dossier attached to the patch it describes.
func (c *Control) forkBrief(repo, name, ref string, gateOutcome forkReviewGateOutcome, gateConfigured bool) error {
	return c.forkBriefAt(repo, repo, gitBranch(repo), "HEAD.."+ref, name, ref, forkReviewGate{outcome: gateOutcome, configured: gateConfigured})
}

// forkBriefAt prints the dossier of ref as the diff HEAD...ref in repo, into the branch it is going
// to, with the commits range listed as the fork's own commits (a range of repo's history).
func (c *Control) forkBriefAt(repo, authority, into, commits, name, ref string, gate forkReviewGate) error {
	p := ui.For(os.Stdout)
	say := func(format string, a ...any) { fmt.Printf(format+"\n", a...) }
	ins, del := forkspace.ParseShortstat(forkspace.GitOut(repo, "diff", "--shortstat", "HEAD..."+ref))
	changes, err := readForkChanges(repo, ref)
	if err != nil {
		return err
	}
	rawCounts, err := forkspace.GitOutput(repo, "diff", "--numstat", "-z", "HEAD..."+ref)
	if err != nil {
		return err
	}
	counts, err := gitdiff.Numstat(rawCounts)
	if err != nil {
		return err
	}
	ahead, _ := strconv.Atoi(forkspace.GitOut(repo, "rev-list", "--count", commits))
	say("Changes in fork %s", name)
	say("  Into: %s", into)
	say("  %s · %s · +%d −%d", ui.Count(ahead, "commit"), ui.Count(len(changes), "file"), ins, del)
	if log := forkspace.GitOut(repo, "log", "--oneline", "--no-decorate", commits); log != "" {
		say("")
		say("Commits")
		say("%s", indent(log))
	}
	why, err := tasks.LatestForkTaskLog(authority, name, 12)
	if err != nil {
		return err
	}
	// Absent notes are omitted: an empty section is not evidence, and the commits above already
	// say what landed.
	if strings.TrimSpace(why) != "" {
		say("")
		say("Agent's notes")
		say("%s", indent(why))
	}
	if len(changes) != 0 { // the sections below are diff-derived; an empty diff has nothing to map
		// The SAME scan `coop fork merge` enforces — surfaced here so findings appear at review,
		// not as a failed merge. Only a deny-policy hit actually blocks; a host-executed file is
		// advisory, and calling it blocked would send a reviewer to --force it never needed.
		warns, scanErr := PolicyScan(repo, ref)
		if scanErr != nil {
			warns = append(warns, "Coop could not check these changes: "+scanErr.Error())
		}
		surfaces := hostsurface.Findings(changes)
		if len(warns) > 0 || len(surfaces) > 0 {
			cause := append([]string{}, warns...)
			for _, f := range surfaces {
				cause = append(cause, ui.SafeInline(f.Path)+"\n  "+f.Reason)
			}
			row := [2]string{"Review the diff before running commands that use these files.", ""}
			if len(warns) > 0 {
				row = [2]string{"Merge is blocked by the project's merge policy:", "coop help fork merge"}
			}
			say("%s", ui.AlertBlock(p, "This change needs your review before merging", strings.Join(cause, "\n"), row))
		}
		say("")
		say("Files")
		for _, sec := range classifyChanged(changes, counts) {
			if sec.title != "" {
				say("%s", indent(sec.title+":"))
			}
			for _, f := range sec.files {
				say("%s", indent(indent(f.render())))
			}
		}
	}
	// The checks have exactly one truthful thing to say per state, and "not run yet" is one of
	// them: a rebase that applied cleanly is not a passing check.
	say("")
	switch {
	case gate.outcome == forkReviewGateUnchecked && !gate.configured:
		say("%s", ui.AlertBlock(p, "No project checks are configured",
			"Set gate: in "+project.File+", or COOP_GATE, to check a fork before it merges."))
	case gate.outcome == forkReviewGateUnchecked:
		say("Project checks will run when you merge.")
	case gate.outcome == forkReviewGateNone:
		say("%s", ui.AlertBlock(p, "No project checks are configured",
			"The fork rebased cleanly onto "+into+", which is not the same as passing checks."))
	case gate.outcome == forkReviewGateGreen:
		say("  %s Project checks passed", p.Green("✓"))
	case gate.outcome == forkReviewGateRed:
		say("%s", ui.FailureBlock(p, "Project checks failed for fork "+name,
			"They ran on a temporary copy rebased onto "+into+".",
			[2]string{"Open the fork:", "coop fork open " + name}))
	case gate.outcome == forkReviewGateChanged:
		say("%s", ui.FailureBlock(p, "Project checks changed files in fork "+name,
			"They ran on a temporary copy rebased onto "+into+" and "+changedFilesPhrase(gate.changed)+".\n"+
				"Make them only read files, or commit what they write, then review again.",
			[2]string{"Open the fork:", "coop fork open " + name}))
	case gate.outcome == forkReviewGateConflict:
		say("%s", ui.FailureBlock(p, "Fork "+name+" conflicts with "+into,
			"Project checks did not run.",
			[2]string{"Open the fork:", "coop fork open " + name}))
	}
	return nil
}
