package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/forkctl"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/loop"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

// rebaseAttempt is what the stand-in agent does in a fork's rebase pass.
type rebaseAttempt int

const (
	rebaseNothing    rebaseAttempt = iota // exits 0 without rebasing
	rebaseThenCrash                       // finishes the rebase, then its box dies
	rebaseAndResolve                      // finishes the rebase and exits 0
	rebaseDropsAll                        // ends on the parent commit itself, as when it already has the work
	rebaseKeepsMerge                      // finishes the rebase, then merges a side branch on top
)

// sharedFilePasses stands in for a fork's loop passes over one shared file. A work pass checks off
// its task and puts the fork's name on the file's one line, so two forks conflict; a task the review
// sent back gets a repair commit with no trailer, as rework does. A rebase pass does what the next
// attempt says, resolving the conflict by keeping every line, then runs afterRebase when set. A
// candidate review fails with the next findings, if any, and passes otherwise. Every rebase and
// review spec is recorded.
//
// With reviewWantsRebaseChecks the review acts as the real reviewer did on a rebased round: the
// tasks' handoffs predate the conflict resolution, so without the rebase pass's own checks it fails
// the round with "gate: stale". With reportChecks a finished rebase reports its checks as the loop
// does from the agent's reply, and so does a check-only pass; with checkCommits that pass commits a
// fix it was told not to make.
type sharedFilePasses struct {
	t                       *testing.T
	attempts                []rebaseAttempt
	findings                [][]tasks.ForkCandidateFinding
	afterRebase             func(ws string)
	reportChecks            bool
	checkCommits            bool
	checkDirties            bool                // the check pass leaves a tracked file changed, as a gate that writes one does
	checkStopsRebase        bool                // the check pass leaves a rebase in progress
	checkInterrupted        bool                // the check pass is stopped by Ctrl-C, as the loop engine ends one
	work                    func(ws, id string) // replaces a work pass's commit when set
	ctrlCIn                 string              // "rebase", "check", "review" or "work": a first Ctrl-C arrives during that pass, which still finishes
	works                   int                 // work passes run
	lastQueue               string              // the projected queue the last work pass finished its task in
	reviewWantsRebaseChecks bool
	rebases                 []loop.RebaseSpec
	checks                  []loop.RebaseSpec
	reviews                 []loop.CandidateReviewSpec
}

const rebaseCheckReport = "REBASE CHECK — git diff --check — exit 0"

func (p *sharedFilePasses) run(spec loop.RunSpec) (int, error) {
	t := p.t
	kind := "work"
	switch {
	case spec.CandidateReview != nil:
		kind = "review"
	case spec.Rebase != nil && spec.Rebase.CheckOnly:
		kind = "check"
	case spec.Rebase != nil:
		kind = "rebase"
	}
	if kind == p.ctrlCIn {
		// What the engine does on a first Ctrl-C: it marks the shared stop and lets the pass finish.
		if spec.StopRequested == nil {
			t.Errorf("the fork loop gave its %s pass no stop to share", kind)
		} else {
			spec.StopRequested.Store(true)
		}
	}
	if kind == "work" {
		p.works++
	}
	switch {
	case spec.CandidateReview != nil:
		review := *spec.CandidateReview
		p.reviews = append(p.reviews, review)
		if p.reviewWantsRebaseChecks && review.RebasedOnto != "" && review.RebaseVerification == "" && !review.AfterRework {
			return 1, &loop.CandidateReviewFindings{Tasks: []tasks.ForkCandidateFinding{{
				TaskID: review.TaskIDs[0], Gate: "stale",
				Finding: "The worker's verification predates the conflict resolution. Run and record `git diff --check " + review.RebasedOnto + " " + review.Head + "`.",
			}}}
		}
		if len(p.findings) > 0 {
			findings := p.findings[0]
			p.findings = p.findings[1:]
			return 1, &loop.CandidateReviewFindings{Tasks: findings}
		}
		return 0, nil
	case spec.Rebase != nil && spec.Rebase.CheckOnly:
		p.checks = append(p.checks, *spec.Rebase)
		if p.checkInterrupted {
			// What the engine returns for a pass stopped mid-attempt: exit 130, nothing reported.
			return loop.LoopInterruptedExitCode, ui.Reported(errors.New("rebase pass stopped"))
		}
		if p.checkCommits {
			if err := os.WriteFile(filepath.Join(spec.Repo, "fix.txt"), []byte("fixed\n"), 0o644); err != nil {
				t.Error(err)
				return 1, nil
			}
			git(t, spec.Repo, "add", "fix.txt")
			git(t, spec.Repo, "commit", "-qm", "Fix what the check found")
		}
		if p.checkStopsRebase {
			// Starts a rebase that stops at once, so the pass ends with git mid-rebase.
			_ = gitCommand(spec.Repo, "rebase", "-q", "-x", "false", "HEAD~1").Run()
		}
		if p.checkDirties {
			if err := os.WriteFile(filepath.Join(spec.Repo, "shared.txt"), []byte("gate output\n"), 0o644); err != nil {
				t.Error(err)
				return 1, nil
			}
		}
		if p.reportChecks && spec.Rebase.Report != nil {
			spec.Rebase.Report(rebaseCheckReport)
		}
		return 0, nil
	case spec.Rebase != nil:
		p.rebases = append(p.rebases, *spec.Rebase)
		if len(p.attempts) == 0 {
			t.Error("an unexpected rebase pass")
			return 1, nil
		}
		attempt := p.attempts[0]
		p.attempts = p.attempts[1:]
		switch attempt {
		case rebaseNothing:
			return 0, nil
		case rebaseDropsAll:
			git(t, spec.Repo, "reset", "-q", "--hard", spec.Rebase.Onto)
			return 0, nil
		}
		if !rebaseKeepingEveryLine(t, spec.Repo, spec.Rebase.Onto) {
			return 1, nil
		}
		if p.afterRebase != nil {
			p.afterRebase(spec.Repo)
		}
		if attempt == rebaseKeepsMerge {
			hideBehindAMerge(t, spec.Repo)
		}
		if attempt == rebaseThenCrash {
			return 1, nil
		}
		if p.reportChecks && spec.Rebase.Report != nil {
			spec.Rebase.Report(rebaseCheckReport)
		}
		return 0, nil
	}
	queue := filepath.Join(spec.Repo, spec.Queues[0])
	entries, err := os.ReadDir(filepath.Join(queue, tasks.StateInProgress))
	if err != nil || len(entries) != 1 {
		t.Errorf("projected work = %v, %v; want one task in progress", entries, err)
		return 1, nil
	}
	id := entries[0].Name()
	dir := filepath.Join(queue, tasks.StateInProgress, id)
	log, _ := os.ReadFile(filepath.Join(dir, "log.md"))
	body, err := os.ReadFile(filepath.Join(dir, "task.md"))
	if err == nil {
		err = os.WriteFile(filepath.Join(dir, "task.md"), []byte(strings.ReplaceAll(string(body), "- [ ] ", "- [x] ")), 0o644)
	}
	if strings.Contains(string(log), "candidate review: fail") {
		if err == nil {
			err = os.WriteFile(filepath.Join(spec.Repo, "repair.txt"), []byte("repaired\n"), 0o644)
		}
		if err != nil {
			t.Error(err)
			return 1, nil
		}
		git(t, spec.Repo, "add", "repair.txt")
		git(t, spec.Repo, "commit", "-qm", "Repair the review finding")
	} else if p.work != nil {
		if err != nil {
			t.Error(err)
			return 1, nil
		}
		p.work(spec.Repo, id)
	} else {
		if err == nil {
			err = os.WriteFile(filepath.Join(spec.Repo, "shared.txt"), []byte(spec.ForkName+"\n"), 0o644)
		}
		if err != nil {
			t.Error(err)
			return 1, nil
		}
		git(t, spec.Repo, "commit", "-qam", "Do "+id+"\n\nCoop-Task: "+id)
	}
	if err := os.Rename(dir, filepath.Join(queue, tasks.StateDone, id)); err != nil {
		t.Error(err)
		return 1, nil
	}
	p.lastQueue = queue
	if p.ctrlCIn == "work" {
		// What the engine returns when a first Ctrl-C let the task finish: its final review is skipped.
		return loop.LoopInterruptedExitCode, nil
	}
	return 0, nil
}

// rebaseKeepingEveryLine rebases beta's branch in ws onto onto, where it conflicts in shared.txt, and
// resolves the conflict by keeping every line, as an agent or a person would.
func rebaseKeepingEveryLine(t *testing.T, ws, onto string) bool {
	t.Helper()
	if out, err := gitCommand(ws, "rebase", onto).CombinedOutput(); err == nil {
		t.Errorf("the rebase onto the parent did not conflict:\n%s", out)
	}
	lines := strings.Fields(forkspace.GitOut(ws, "show", onto+":shared.txt"))
	if err := os.WriteFile(filepath.Join(ws, "shared.txt"), []byte(strings.Join(append(lines, "beta"), "\n")+"\n"), 0o644); err != nil {
		t.Error(err)
		return false
	}
	git(t, ws, "add", "shared.txt")
	continueRebase := gitCommand(ws, "rebase", "--continue")
	continueRebase.Env = append(continueRebase.Env, "GIT_EDITOR=true")
	if out, err := continueRebase.CombinedOutput(); err != nil {
		t.Errorf("git rebase --continue: %v\n%s", err, out)
		return false
	}
	return true
}

// hideBehindAMerge commits e.txt on a side branch of the fork's branch and merges it with a
// resolution that drops it: the merge's diff shows nothing, a flattening rebase replays e.txt.
func hideBehindAMerge(t *testing.T, ws string) {
	t.Helper()
	branch := forkspace.GitOut(ws, "symbolic-ref", "--short", "HEAD")
	git(t, ws, "checkout", "-q", "-b", "side")
	if err := os.WriteFile(filepath.Join(ws, "e.txt"), []byte("hidden\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "e.txt")
	git(t, ws, "commit", "-qm", "side: add e.txt")
	git(t, ws, "checkout", "-q", branch)
	git(t, ws, "merge", "-q", "--no-ff", "--no-commit", "side")
	git(t, ws, "rm", "-q", "-f", "e.txt")
	git(t, ws, "commit", "-qm", "Merge side, dropping e.txt")
	git(t, ws, "branch", "-q", "-D", "side")
}

func gitCommand(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	return cmd
}

func runForkLoopStoppingAfterReview(t *testing.T, a *app, repo, ws string, identity forkspace.Identity) (int, string, error) {
	t.Helper()
	var code int
	var runErr error
	var stderr string
	stdout := captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			code, runErr = a.runForkLoop(repo, ws, identity, "claude", nil, "", "", "", nil, networkFlags{}, true, false)
		})
	})
	return code, stdout + stderr, runErr
}

func mergeForkForTest(t *testing.T, a *app, name string) (int, string, error) {
	t.Helper()
	var code int
	var err error
	out := captureStderr(t, func() { code, err = a.forkctl().ForkMerge([]string{name, "--yes"}) })
	return code, out, err
}

// commitOnMain puts content in the project's file and commits it on main.
func commitOnMain(t *testing.T, repo, file, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", file)
	git(t, repo, "commit", "-qm", "main edits "+file)
	return forkspace.GitOut(repo, "rev-parse", "HEAD")
}

// conflictingForks is two reviewed forks that changed the same line, after the first merged and the
// second's merge conflicted.
type conflictingForks struct {
	a             *app
	passes        *sharedFilePasses
	repo, ws      string
	beta          forkspace.Identity
	reviewed      tasks.ForkCandidate
	first, second string
	parent        string // main when beta's merge conflicted
	mergeErr      error
}

func setUpConflictingForks(t *testing.T) conflictingForks {
	t.Helper()
	repo := initRepo(t)
	ignoreTheQueue(t, repo)
	commitOnMain(t, repo, "shared.txt", "base\n")
	f := conflictingForks{repo: repo, first: "2026-11-19-fork-conflict-a", second: "2026-11-19-fork-conflict-b"}
	seedForkDecisionTask(t, repo, f.first)
	seedForkDecisionTask(t, repo, f.second)
	f.a = forkNetworkPreflightApp(t, repo)
	f.passes = &sharedFilePasses{t: t}
	f.a.forkLoopRun = f.passes.run
	wsA, alpha := setUpForkForDecision(t, repo, "alpha")
	f.ws, f.beta = setUpForkForDecision(t, repo, "beta")
	for _, fork := range []struct {
		ws       string
		identity forkspace.Identity
	}{{wsA, alpha}, {f.ws, f.beta}} {
		if code, out, err := runForkLoopStoppingAfterReview(t, f.a, repo, fork.ws, fork.identity); code != 0 || err != nil {
			t.Fatalf("fork %s loop = (%d, %v)\n%s", fork.identity.Name, code, err, out)
		}
	}
	if code, out, err := mergeForkForTest(t, f.a, "alpha"); code != 0 || err != nil {
		t.Fatalf("merge alpha = (%d, %v)\n%s", code, err, out)
	}
	var ok bool
	var err error
	if f.reviewed, ok, err = tasks.ReadForkCandidate(repo, f.beta); err != nil || !ok {
		t.Fatalf("beta's reviewed candidate = %v, %v", ok, err)
	}
	f.parent = forkspace.GitOut(repo, "rev-parse", "HEAD")
	code, out, err := mergeForkForTest(t, f.a, "beta")
	if code == 0 || err == nil {
		t.Fatalf("merge of the conflicting beta = (%d, %v)\n%s", code, err, out)
	}
	f.mergeErr = err
	return f
}

func (f conflictingForks) loop(t *testing.T) (int, string, error) {
	t.Helper()
	return runForkLoopStoppingAfterReview(t, f.a, f.repo, f.ws, f.beta)
}

// Two forks change the same line. Once the first merges, the second's reviewed commits conflict with
// the branch. Its merge refuses and names the fork's loop; the loop's agent rebases the work onto the
// branch and resolves the conflict; the result goes through a full candidate review as a new round;
// then it merges, exactly as reviewed, and completes its task. Until that review, nothing of the
// rebased work can land: not after a pass that did not rebase, not after one that died after it.
func TestForkMergeConflictIsRebasedAndReviewedAgain(t *testing.T) {
	f := setUpConflictingForks(t)
	repo, a, passes, beta, parent := f.repo, f.a, f.passes, f.beta, f.parent
	for _, want := range []string{"conflict with main", "shared.txt", "nothing merged", "reviewed again", "coop fork beta --loop", "coop fork merge beta"} {
		if !strings.Contains(f.mergeErr.Error(), want) {
			t.Errorf("the conflict refusal does not say %q: %v", want, f.mergeErr)
		}
	}
	if got := forkspace.GitOut(repo, "rev-parse", "HEAD"); got != parent {
		t.Fatalf("the refused merge moved main to %s", got)
	}
	if onto, ok, err := tasks.ReadForkCandidateRebase(repo, beta); err != nil || !ok || onto.Onto != parent || onto.Branch != "main" {
		t.Fatalf("rebase request = %+v, %v, %v; want main's %s", onto, ok, err, parent)
	}

	// A pass whose agent did not rebase sends nothing to review.
	passes.attempts = []rebaseAttempt{rebaseNothing}
	code, out, err := f.loop(t)
	if code == 0 || err == nil || !strings.Contains(err.Error(), "not rebased yet") || len(passes.reviews) != 2 {
		t.Fatalf("loop after a pass that did not rebase = (%d, %v) with %d reviews\n%s", code, err, len(passes.reviews), out)
	}
	if len(passes.rebases) != 1 || passes.rebases[0].Onto != parent || passes.rebases[0].Branch != "beta" || passes.rebases[0].ParentBranch != "main" {
		t.Fatalf("rebase pass = %+v; want beta onto main's %s", passes.rebases, parent)
	}

	// The agent rebases, and its box dies before the loop goes on. The rebased branch is not reviewed,
	// so it does not merge.
	passes.attempts = []rebaseAttempt{rebaseThenCrash}
	if code, out, err := f.loop(t); code == 0 || len(passes.reviews) != 2 {
		t.Fatalf("loop whose rebase pass died = (%d, %v) with %d reviews\n%s", code, err, len(passes.reviews), out)
	}
	if code, out, err := mergeForkForTest(t, a, "beta"); code == 0 || err == nil || !strings.Contains(err.Error(), "rebased but not reviewed again yet") {
		t.Fatalf("merge of the unreviewed rebase = (%d, %v)\n%s", code, err, out)
	}
	if got := forkspace.GitOut(repo, "rev-parse", "HEAD"); got != parent {
		t.Fatalf("the unreviewed rebase moved main to %s", got)
	}
	// Host re-signing rewrites the fork's commits, same trees, new IDs.
	resign := gitCommand(f.ws, "commit", "--amend", "--no-edit", "--quiet")
	resign.Env = append(resign.Env, "GIT_COMMITTER_DATE=2026-11-19T12:00:00Z")
	if out, err := resign.CombinedOutput(); err != nil {
		t.Fatalf("rewrite the rebased commit: %v\n%s", err, out)
	}
	rebased := forkspace.GitOut(f.ws, "rev-parse", "HEAD")

	// The next loop takes up the rebased branch without rebasing again and reviews it in full.
	code, out, err = f.loop(t)
	if code != 0 || err != nil || len(passes.rebases) != 2 || len(passes.reviews) != 3 {
		t.Fatalf("loop on the rebased fork = (%d, %v) after %d rebase passes and %d reviews\n%s", code, err, len(passes.rebases), len(passes.reviews), out)
	}
	review := passes.reviews[2]
	if review.Head != rebased || review.RebasedOnto != parent || review.Round != 2 || review.PreviousRound != 1 {
		t.Fatalf("review of the rebased fork = %+v; want round 2 of %s, rebased onto %s", review, rebased, parent)
	}
	if candidate, ok, err := tasks.ReadForkCandidate(repo, beta); err != nil || !ok || candidate.Head != rebased || candidate.ID == f.reviewed.ID {
		t.Fatalf("published candidate = %+v, %v, %v; want a new one at %s", candidate, ok, err, rebased)
	}
	if _, ok, err := tasks.ReadForkCandidateRebase(repo, beta); err != nil || ok {
		t.Fatalf("rebase request after the new round = %v, %v; want it closed", ok, err)
	}

	if code, out, err := mergeForkForTest(t, a, "beta"); code != 0 || err != nil {
		t.Fatalf("merge of the reviewed rebase = (%d, %v)\n%s", code, err, out)
	}
	if data, err := os.ReadFile(filepath.Join(repo, "shared.txt")); err != nil || string(data) != "alpha\nbeta\n" {
		t.Fatalf("main's shared.txt = %q, %v; want both forks' lines", data, err)
	}
	if got := forkspace.GitOut(repo, "rev-parse", "HEAD^{tree}"); got != review.Tree {
		t.Fatalf("main's tree %s is not the reviewed %s", got, review.Tree)
	}
	for _, id := range []string{f.first, f.second} {
		if bound := forkspace.GitOut(repo, "log", "--format=%H", "--grep", "Coop-Task: "+id); len(strings.Fields(bound)) != 1 {
			t.Errorf("main has commits %q for %s; want exactly one", bound, id)
		}
		if _, err := os.Stat(filepath.Join(repo, ".agent", "tasks", tasks.StateDone, id, "task.md")); err != nil {
			t.Errorf("task %s is not done after its merge: %v", id, err)
		}
	}
}

// main moves on after the conflict: the rebase pass rebases onto where main is when it starts, not
// the commit the merge met, so the review covers the work from the base it lands on. After that round
// is published, main conflicts with it again, and the new merge's request replaces the old one.
func TestForkRebaseFollowsTheBranchAndASecondConflictReplacesTheRequest(t *testing.T) {
	f := setUpConflictingForks(t)
	moved := commitOnMain(t, f.repo, "other.txt", "main moved on\n")
	f.passes.attempts = []rebaseAttempt{rebaseAndResolve}
	if code, out, err := f.loop(t); code != 0 || err != nil {
		t.Fatalf("loop after main moved = (%d, %v)\n%s", code, err, out)
	}
	if len(f.passes.rebases) != 1 || f.passes.rebases[0].Onto != moved || f.passes.rebases[0].ParentBranch != "main" {
		t.Fatalf("rebase pass = %+v; want it onto main's newest %s, not the merge's %s", f.passes.rebases, moved, f.parent)
	}
	review := f.passes.reviews[len(f.passes.reviews)-1]
	if review.RebasedOnto != moved {
		t.Fatalf("review of the rebased fork = %+v; want it reviewed from %s", review, moved)
	}
	if _, err := forkspace.GitOutErr(f.ws, "merge-base", "--is-ancestor", moved, review.Head); err != nil {
		t.Fatalf("the rebased round %s does not build on main's %s", review.Head, moved)
	}

	again := commitOnMain(t, f.repo, "shared.txt", "alpha\ngamma\n")
	code, out, err := mergeForkForTest(t, f.a, "beta")
	if code == 0 || err == nil || !strings.Contains(err.Error(), "conflict with main") {
		t.Fatalf("second conflicting merge = (%d, %v)\n%s", code, err, out)
	}
	rebased, ok, err := tasks.ReadForkCandidate(f.repo, f.beta)
	if err != nil || !ok || rebased.Head != review.Head {
		t.Fatalf("candidate after the second conflict = %+v, %v, %v", rebased, ok, err)
	}
	if request, ok, err := tasks.ReadForkCandidateRebase(f.repo, f.beta); err != nil || !ok || request.Onto != again {
		t.Fatalf("rebase request after the second conflict = %+v, %v, %v; want main's %s", request, ok, err, again)
	}
	f.passes.attempts = []rebaseAttempt{rebaseAndResolve}
	if code, out, err := f.loop(t); code != 0 || err != nil || f.passes.rebases[1].Onto != again {
		t.Fatalf("second rebase = (%d, %v) with %+v\n%s", code, err, f.passes.rebases, out)
	}
	if code, out, err := mergeForkForTest(t, f.a, "beta"); code != 0 || err != nil {
		t.Fatalf("merge after the second rebase = (%d, %v)\n%s", code, err, out)
	}
	if data, err := os.ReadFile(filepath.Join(f.repo, "shared.txt")); err != nil || string(data) != "alpha\ngamma\nbeta\n" {
		t.Fatalf("main's shared.txt = %q, %v", data, err)
	}
}

// A rebase that leaves none of the fork's commits (the branch already has the work) has nothing to
// review. The loop says so and how to go on, every time, without another pass; it never reads as an
// unfinished rebase to try again.
//
// Two cases read alike in git and are told apart: the branch already has the work (only a person can
// close its tasks), or the rebase dropped it (go back and rebase again). The loop, merge and ls say
// the same thing.
func TestForkRebaseThatLeavesNoCommitsSaysSo(t *testing.T) {
	for _, tc := range []struct {
		name, mainHas, headline, way, line string
	}{
		{"dropped", "", "rebase dropped its reviewed work", "Then rebase again: coop fork beta --loop", "Rebase dropped its reviewed work"},
		{"branch has it", "alpha\nbeta\n", "rebase left no commits", "coop fork rm beta --force", "already has this work"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setUpConflictingForks(t)
			if tc.mainHas != "" {
				commitOnMain(t, f.repo, "shared.txt", tc.mainHas)
			}
			f.passes.attempts = []rebaseAttempt{rebaseDropsAll}
			for range 2 {
				code, out, err := f.loop(t)
				if code == 0 || err == nil || !strings.Contains(err.Error(), tc.headline) || !strings.Contains(err.Error(), tc.way) ||
					strings.Contains(err.Error(), "not rebased yet") {
					t.Fatalf("loop after an empty rebase = (%d, %v)\n%s", code, err, out)
				}
				if tc.mainHas == "" && !strings.Contains(err.Error(), "Coop-Task: "+f.second+" is on 0 commit(s)") {
					t.Errorf("the dropped work's binding is not named: %v", err)
				}
			}
			if len(f.passes.rebases) != 1 || len(f.passes.reviews) != 2 {
				t.Fatalf("an empty rebase ran %d rebase passes and %d reviews; want one pass and no new review", len(f.passes.rebases), len(f.passes.reviews))
			}
			if code, out, err := mergeForkForTest(t, f.a, "beta"); code == 0 || err == nil || !strings.Contains(err.Error(), tc.headline) {
				t.Errorf("merge after an empty rebase = (%d, %v)\n%s", code, err, out)
			}
			listing := captureStdout(t, func() {
				if code, err := f.a.forkctl().ForkLs(nil); code != 0 || err != nil {
					t.Fatalf("fork ls = (%d, %v)", code, err)
				}
			})
			if !strings.Contains(listing, tc.line) {
				t.Errorf("fork ls after an empty rebase does not say %q:\n%s", tc.line, listing)
			}
		})
	}
}

// The host checks a rebase's commits before any review sees them: a rebase that moved a commit's
// Coop-Task binding to another task is refused, whatever the agent was told.
func TestForkRebaseThatChangesTaskBindingsIsRefused(t *testing.T) {
	f := setUpConflictingForks(t)
	f.passes.attempts = []rebaseAttempt{rebaseAndResolve}
	f.passes.afterRebase = func(ws string) {
		git(t, ws, "commit", "-q", "--amend", "-m", "Do it\n\nCoop-Task: "+f.first)
	}
	code, out, err := f.loop(t)
	if code == 0 || err == nil || !strings.Contains(err.Error(), "changed which commits belong to its tasks") ||
		!strings.Contains(err.Error(), "Coop-Task: "+f.first) || !strings.Contains(err.Error(), "Coop-Task: "+f.second) {
		t.Fatalf("loop after a rebase that rebound a task = (%d, %v)\n%s", code, err, out)
	}
	if len(f.passes.reviews) != 2 {
		t.Fatalf("a rebase with changed bindings reached review (%d reviews)", len(f.passes.reviews))
	}
	if candidate, ok, err := tasks.ReadForkCandidate(f.repo, f.beta); err != nil || !ok || candidate.ID != f.reviewed.ID {
		t.Fatalf("candidate after the refusal = %+v, %v, %v; want the reviewed round still active", candidate, ok, err)
	}
}

// The rebased tree is new content, so its review needs checks run on it, which the tasks' own
// handoffs, older than the conflict resolution, cannot give. The rebase pass's reported checks reach
// the first review of exactly that tree, and the round passes without a stale-evidence rework.
func TestForkRebasePassChecksReachItsFirstReview(t *testing.T) {
	f := setUpConflictingForks(t)
	f.passes.attempts = []rebaseAttempt{rebaseAndResolve}
	f.passes.reportChecks, f.passes.reviewWantsRebaseChecks = true, true
	if code, out, err := f.loop(t); code != 0 || err != nil {
		t.Fatalf("loop = (%d, %v)\n%s", code, err, out)
	}
	if len(f.passes.reviews) != 3 {
		t.Fatalf("reviews = %+v; want one review of the rebased round", f.passes.reviews)
	}
	if review := f.passes.reviews[2]; review.RebaseVerification != rebaseCheckReport || review.AfterRework {
		t.Fatalf("review of the rebased round = %+v; want it given the rebase pass's checks", review)
	}
	if rebase, ok, err := tasks.ReadForkCandidateRebase(f.repo, f.beta); err != nil || ok {
		t.Fatalf("rebase request after the round passed = %+v, %v, %v", rebase, ok, err)
	}
}

// A rebased round can fail its review like any other. Its task goes back to rework, the repair is
// reviewed again, and that review is still told the work was rebased and from where.
func TestForkRebasedRoundThatFailsReviewIsReworkedAndReviewedAgain(t *testing.T) {
	f := setUpConflictingForks(t)
	f.passes.attempts = []rebaseAttempt{rebaseAndResolve}
	f.passes.findings = [][]tasks.ForkCandidateFinding{{{TaskID: f.second, Gate: "review", Finding: "the resolution needs a test"}}}
	for range 3 {
		if code, out, err := f.loop(t); code == 0 && err == nil {
			break
		} else if len(f.passes.reviews) > 4 {
			t.Fatalf("loop = (%d, %v)\n%s", code, err, out)
		}
	}
	if len(f.passes.reviews) != 4 {
		t.Fatalf("reviews = %+v; want the failed rebased round and its repair reviewed", f.passes.reviews)
	}
	failed, repaired := f.passes.reviews[2], f.passes.reviews[3]
	if failed.RebasedOnto != f.parent || repaired.RebasedOnto != f.parent || repaired.Head == failed.Head {
		t.Fatalf("rebased reviews = %+v then %+v; want both told the rebase onto %s", failed, repaired, f.parent)
	}
	if failed.AfterRework || !repaired.AfterRework || repaired.Round != failed.Round {
		t.Fatalf("rebased reviews = %+v then %+v; want the second named as the same round after its rework", failed, repaired)
	}
	if code, out, err := mergeForkForTest(t, f.a, "beta"); code != 0 || err != nil {
		t.Fatalf("merge of the repaired rebase = (%d, %v)\n%s", code, err, out)
	}
	if _, err := os.Stat(filepath.Join(f.repo, "repair.txt")); err != nil {
		t.Fatalf("the reviewed repair did not land: %v", err)
	}
}

// mergeCommitRefusal checks the one refusal of a fork whose commits include a merge commit: it says
// so and names the git rebase onto base that straightens them.
func mergeCommitRefusal(t *testing.T, step string, err error, ws, base string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "commits include a merge commit") ||
		!strings.Contains(err.Error(), filepath.Base(ws)+" rebase "+base+" && coop fork") ||
		strings.Contains(err.Error(), "not rebased yet") || strings.Contains(err.Error(), "no longer descends") {
		t.Errorf("%s = %v; want the merge-commit refusal naming the rebase onto %s", step, err, base)
	}
}

// After a conflict, a branch holding a merge commit is refused before any agent runs, with the
// reason and the git rebase that straightens it; a pass that leaves a merge gets the same answer,
// once, and the next loop runs no agent for it. "Not rebased yet, try again" repeated forever.
func TestForkRebaseRefusesMergeCommitsWithoutAnAgentRun(t *testing.T) {
	f := setUpConflictingForks(t)
	hideBehindAMerge(t, f.ws)
	code, out, err := f.loop(t)
	if code == 0 || len(f.passes.rebases) != 0 {
		t.Fatalf("loop over a merge commit = (%d, %v) after %d rebase passes; want a refusal and no agent\n%s", code, err, len(f.passes.rebases), out)
	}
	mergeCommitRefusal(t, "loop over a merge commit", err, f.ws, f.parent)
	if code, out, err := mergeForkForTest(t, f.a, "beta"); code == 0 {
		t.Fatalf("merge over a merge commit = (%d, %v)\n%s", code, err, out)
	} else {
		mergeCommitRefusal(t, "merge over a merge commit", err, f.ws, f.parent)
	}

	git(t, f.ws, "reset", "-q", "--hard", "HEAD^1") // back to the reviewed commit
	f.passes.attempts = []rebaseAttempt{rebaseKeepsMerge}
	code, out, err = f.loop(t)
	if code == 0 || len(f.passes.rebases) != 1 {
		t.Fatalf("loop whose pass kept a merge = (%d, %v) after %d passes\n%s", code, err, len(f.passes.rebases), out)
	}
	mergeCommitRefusal(t, "loop whose pass kept a merge", err, f.ws, f.parent)
	if code, _, err := f.loop(t); code == 0 || len(f.passes.rebases) != 1 {
		t.Fatalf("rerun after a pass kept a merge = (%d, %v) after %d passes; want no second agent run", code, err, len(f.passes.rebases))
	} else {
		mergeCommitRefusal(t, "rerun after a pass kept a merge", err, f.ws, f.parent)
	}
	if len(f.passes.reviews) != 2 {
		t.Fatalf("a merge commit reached review (%d reviews)", len(f.passes.reviews))
	}
}

// A merge commit on top of a reviewed fork: merge, the loop and ls all give the true reason and the
// rebase that straightens it. The loop said "fork HEAD no longer descends from its reviewed
// candidate", which was false, and merge pointed to that loop.
func TestForkMergeCommitOnAReviewedForkGetsOneTrueReason(t *testing.T) {
	repo := initRepo(t)
	ignoreTheQueue(t, repo)
	commitOnMain(t, repo, "shared.txt", "base\n")
	id := "2026-11-19-fork-merge-on-top"
	seedForkDecisionTask(t, repo, id)
	a := forkNetworkPreflightApp(t, repo)
	passes := &sharedFilePasses{t: t}
	a.forkLoopRun = passes.run
	ws, identity := setUpForkForDecision(t, repo, "solo")
	if code, out, err := runForkLoopStoppingAfterReview(t, a, repo, ws, identity); code != 0 || err != nil {
		t.Fatalf("fork loop = (%d, %v)\n%s", code, err, out)
	}
	reviewed := forkspace.GitOut(ws, "rev-parse", "HEAD")
	hideBehindAMerge(t, ws)
	if code, out, err := mergeForkForTest(t, a, "solo"); code == 0 {
		t.Fatalf("merge = (%d, %v)\n%s", code, err, out)
	} else {
		mergeCommitRefusal(t, "merge", err, ws, reviewed)
	}
	code, out, err := runForkLoopStoppingAfterReview(t, a, repo, ws, identity)
	if code == 0 {
		t.Fatalf("loop = (%d, %v)\n%s", code, err, out)
	}
	mergeCommitRefusal(t, "loop", err, ws, reviewed)
	listing := captureStdout(t, func() {
		if code, err := a.forkctl().ForkLs(nil); code != 0 || err != nil {
			t.Fatalf("fork ls = (%d, %v)", code, err)
		}
	})
	if !strings.Contains(listing, "commits include a merge commit") {
		t.Errorf("fork ls does not give the reason:\n%s", listing)
	}
	if _, err := os.Stat(filepath.Join(repo, "e.txt")); !os.IsNotExist(err) {
		t.Fatalf("content the review never saw reached the parent: %v", err)
	}
}

// A person who rebases the fork by hand, as the merge-commit refusal tells them to, runs no rebase
// pass, so no pass reported checks on the rebased tree, and the tasks' handoffs predate it: the first
// review failed as stale and a rework run followed. The loop now runs one check-only pass, which
// changes nothing, and its reported checks reach the first review of exactly that tree.
func TestForkHandRebaseGetsACheckPassBeforeItsFirstReview(t *testing.T) {
	f := setUpConflictingForks(t)
	if !rebaseKeepingEveryLine(t, f.ws, f.parent) {
		t.FailNow()
	}
	rebased := forkspace.GitOut(f.ws, "rev-parse", "HEAD")
	f.passes.reportChecks, f.passes.reviewWantsRebaseChecks = true, true
	if code, out, err := f.loop(t); code != 0 || err != nil {
		t.Fatalf("loop after a hand rebase = (%d, %v)\n%s", code, err, out)
	}
	if len(f.passes.rebases) != 0 || len(f.passes.checks) != 1 {
		t.Fatalf("passes = %d rebase, %d check; want only one check pass", len(f.passes.rebases), len(f.passes.checks))
	}
	if check := f.passes.checks[0]; check.Onto != f.parent || check.Branch != "beta" || check.ParentBranch != "main" {
		t.Fatalf("check pass = %+v; want beta's rebase onto main's %s", check, f.parent)
	}
	if len(f.passes.reviews) != 3 {
		t.Fatalf("reviews = %+v; want one passing review of the rebased round", f.passes.reviews)
	}
	if review := f.passes.reviews[2]; review.Head != rebased || review.RebasedOnto != f.parent || review.RebaseVerification != rebaseCheckReport || review.AfterRework {
		t.Fatalf("review of the hand rebase = %+v; want it given the check pass's report", review)
	}
	if code, out, err := mergeForkForTest(t, f.a, "beta"); code != 0 || err != nil {
		t.Fatalf("merge of the reviewed hand rebase = (%d, %v)\n%s", code, err, out)
	}
}

// A check pass that reports nothing runs once; the review then judges the handoffs alone, as before.
func TestForkRebaseCheckPassThatReportsNothingRunsOnce(t *testing.T) {
	f := setUpConflictingForks(t)
	if !rebaseKeepingEveryLine(t, f.ws, f.parent) {
		t.FailNow()
	}
	if code, out, err := f.loop(t); code != 0 || err != nil {
		t.Fatalf("loop = (%d, %v)\n%s", code, err, out)
	}
	if len(f.passes.checks) != 1 || len(f.passes.reviews) != 3 || f.passes.reviews[2].RebaseVerification != "" {
		t.Fatalf("%d check passes, reviews %+v; want one check pass and a review with no reported checks", len(f.passes.checks), f.passes.reviews)
	}
}

// The check pass may only run checks. One that commits is refused before any review, and keeps no
// report: its checks would be of a tree no review reads.
func TestForkRebaseCheckPassThatChangesTheForkIsRefused(t *testing.T) {
	f := setUpConflictingForks(t)
	if !rebaseKeepingEveryLine(t, f.ws, f.parent) {
		t.FailNow()
	}
	rebased := forkspace.GitOut(f.ws, "rev-parse", "HEAD")
	f.passes.reportChecks, f.passes.checkCommits = true, true
	code, out, err := f.loop(t)
	if code == 0 || err == nil || !strings.Contains(err.Error(), "changed during its rebase check") ||
		!strings.Contains(err.Error(), "checkout -f -B beta "+rebased) {
		t.Fatalf("loop whose check pass committed = (%d, %v)\n%s", code, err, out)
	}
	if len(f.passes.reviews) != 2 {
		t.Fatalf("a changed check pass reached review (%d reviews)", len(f.passes.reviews))
	}
	if rebase, ok, err := tasks.ReadForkCandidateRebase(f.repo, f.beta); err != nil || !ok || rebase.Verification != "" {
		t.Fatalf("request after the refused check = %+v, %v, %v; want it open with no report", rebase, ok, err)
	}
	if candidate, ok, err := tasks.ReadForkCandidate(f.repo, f.beta); err != nil || !ok || candidate.ID != f.reviewed.ID {
		t.Fatalf("candidate after the refusal = %+v, %v, %v; want the reviewed round still active", candidate, ok, err)
	}
}

// A person who merges main into the fork instead of rebasing gets the merge-commit refusal from the
// loop, which does not claim a rebase ran.
func TestForkHandMergeIsRefusedWithoutClaimingARebase(t *testing.T) {
	f := setUpConflictingForks(t)
	if out, err := gitCommand(f.ws, "merge", "--no-ff", f.parent).CombinedOutput(); err == nil {
		t.Fatalf("merging main into beta did not conflict:\n%s", out)
	}
	if err := os.WriteFile(filepath.Join(f.ws, "shared.txt"), []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, f.ws, "add", "shared.txt")
	git(t, f.ws, "commit", "-qm", "Merge main")
	code, out, err := f.loop(t)
	if code == 0 || len(f.passes.rebases)+len(f.passes.checks) != 0 {
		t.Fatalf("loop over a hand merge = (%d, %v) after %d passes\n%s", code, err, len(f.passes.rebases)+len(f.passes.checks), out)
	}
	mergeCommitRefusal(t, "loop over a hand merge", err, f.ws, f.parent)
	if strings.Contains(err.Error(), "rebase kept") || strings.Contains(err.Error(), "pass kept") {
		t.Errorf("the refusal claims a rebase ran: %v", err)
	}
}

// After a merge found that the branch already has one of the fork's task commits, the loop runs no
// agent: no rebase can keep that commit. It says so and names the ways on, every time.
func TestForkLoopStopsAtOnceOnASkippedTaskCommit(t *testing.T) {
	f := setUpConflictingForks(t)
	unlock, err := forkspace.LockState(f.repo, f.beta.Name)
	if err != nil {
		t.Fatal(err)
	}
	err = tasks.RequestForkCandidateRebaseLocked(f.repo, f.reviewed, f.parent, "main", tasks.RebaseCauseSkipped)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		code, out, err := f.loop(t)
		if code == 0 || err == nil || !strings.Contains(err.Error(), "reviewed commit is already in main's history") ||
			!strings.Contains(err.Error(), "No agent ran") || !strings.Contains(err.Error(), "coop fork rm beta --force") {
			t.Fatalf("loop after a skipped commit = (%d, %v)\n%s", code, err, out)
		}
	}
	if len(f.passes.rebases)+len(f.passes.checks) != 0 || len(f.passes.reviews) != 2 {
		t.Fatalf("a skipped commit ran %d passes and %d reviews; want none", len(f.passes.rebases)+len(f.passes.checks), len(f.passes.reviews))
	}
}

// An isolated fork's conflict, recorded by its merge, gives its loop nothing to do: no agent may
// rebase it. The loop's own pass refuses it as the skipped-commit case does, with exit 1, so a
// `coop fork x --loop && coop fork merge x` stops there, and names the ways out. This checks the
// loop's in-pass branch, which keys on the record; the start-up refusal on a real isolated fork is
// forkctl's TestIsolatedConflictNamesTheWaysOutAndShowsInLs (the isolated fixture lives there).
func TestForkLoopOnAConflictedIsolatedForkNamesTheWaysOut(t *testing.T) {
	f := setUpConflictingForks(t)
	unlock, err := forkspace.LockState(f.repo, f.beta.Name)
	if err != nil {
		t.Fatal(err)
	}
	err = tasks.RequestForkCandidateRebaseLocked(f.repo, f.reviewed, f.parent, "main", tasks.RebaseCauseIsolatedConflict)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	reviews := len(f.passes.reviews)
	code, out, err := f.loop(t)
	if code != 1 || err == nil {
		t.Fatalf("loop on a conflicted isolated fork = (%d, %v)\n%s", code, err, out)
	}
	for _, want := range []string{"reviewed commits conflict with main", "no agent can rebase it", "No agent ran", "coop fork merge beta", "coop fork rm beta --force", " --isolated"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("loop refusal lacks %q: %v", want, err)
		}
	}
	if strings.Contains(out, "next steps") || strings.Contains(out, "ready to merge") {
		t.Errorf("loop output still lists the generic next steps:\n%s", out)
	}
	if len(f.passes.rebases)+len(f.passes.checks) != 0 || len(f.passes.reviews) != reviews {
		t.Fatalf("the loop ran %d passes and %d reviews; want none", len(f.passes.rebases)+len(f.passes.checks), len(f.passes.reviews)-reviews)
	}
}

// Ctrl-C during the check-only pass after a hand rebase stops the loop with the engine's exit 130,
// before any review and with no checks kept. The rerun the stop names checks the same tree again,
// then reviews it once, and the reviewed result merges.
func TestForkRebaseCheckPassStoppedByCtrlCRunsAgainOnTheRerun(t *testing.T) {
	f := setUpConflictingForks(t)
	if !rebaseKeepingEveryLine(t, f.ws, f.parent) {
		t.FailNow()
	}
	rebased := forkspace.GitOut(f.ws, "rev-parse", "HEAD")
	f.passes.checkInterrupted = true
	if code, out, err := f.loop(t); code != loop.LoopInterruptedExitCode || err == nil {
		t.Fatalf("loop whose check pass was stopped = (%d, %v)\n%s", code, err, out)
	}
	if len(f.passes.checks) != 1 || len(f.passes.reviews) != 2 {
		t.Fatalf("after the stop: %d check passes, %d reviews; want one check pass and no new review", len(f.passes.checks), len(f.passes.reviews))
	}
	if rebase, ok, err := tasks.ReadForkCandidateRebase(f.repo, f.beta); err != nil || !ok || rebase.Verification != "" || rebase.VerifiedTree != "" {
		t.Fatalf("request after the stop = %+v, %v, %v; want it open with no checks kept", rebase, ok, err)
	}
	if code, _, err := mergeForkForTest(t, f.a, "beta"); code == 0 || err == nil || !strings.Contains(err.Error(), "rebased but not reviewed again yet") {
		t.Fatalf("merge after the stop = (%d, %v); want the not-reviewed refusal", code, err)
	}
	f.passes.checkInterrupted, f.passes.reportChecks, f.passes.reviewWantsRebaseChecks = false, true, true
	if code, out, err := f.loop(t); code != 0 || err != nil {
		t.Fatalf("rerun after the stop = (%d, %v)\n%s", code, err, out)
	}
	if len(f.passes.checks) != 2 || len(f.passes.rebases) != 0 || len(f.passes.reviews) != 3 {
		t.Fatalf("rerun: %d check passes, %d rebase passes, %d reviews; want a second check pass and one review", len(f.passes.checks), len(f.passes.rebases), len(f.passes.reviews))
	}
	if review := f.passes.reviews[2]; review.Head != rebased || review.RebaseVerification != rebaseCheckReport {
		t.Fatalf("review after the rerun = %+v; want the rebased head with the rerun's checks", review)
	}
	if code, out, err := mergeForkForTest(t, f.a, "beta"); code != 0 || err != nil {
		t.Fatalf("merge after the rerun = (%d, %v)\n%s", code, err, out)
	}
}

// A first Ctrl-C during a rebase or check pass that still succeeds stops the loop right after that
// pass, as its notice says ("then stopping before its review"): the finished rebase and its checks are
// kept, no review starts, and the rerun goes straight to the one review, with no pass run twice.
func TestForkRebaseAndCheckPassesStopAfterAFirstCtrlCAndKeepTheirResult(t *testing.T) {
	for _, kind := range []string{"rebase", "check"} {
		t.Run(kind, func(t *testing.T) {
			f := setUpConflictingForks(t)
			if kind == "check" {
				if !rebaseKeepingEveryLine(t, f.ws, f.parent) {
					t.FailNow()
				}
			} else {
				f.passes.attempts = []rebaseAttempt{rebaseAndResolve}
			}
			f.passes.reportChecks, f.passes.reviewWantsRebaseChecks, f.passes.ctrlCIn = true, true, kind
			code, out, err := f.loop(t)
			if code != loop.LoopInterruptedExitCode || err == nil || !strings.Contains(out, "Fork beta stopped") || !strings.Contains(out, "coop fork beta") {
				t.Fatalf("loop stopped during its %s pass = (%d, %v)\n%s", kind, code, err, out)
			}
			if passes := len(f.passes.rebases) + len(f.passes.checks); passes != 1 || len(f.passes.reviews) != 2 {
				t.Fatalf("after the stop: %d rebase or check passes, %d new reviews; want the one pass and no review", passes, len(f.passes.reviews)-2)
			}
			rebased := forkspace.GitOut(f.ws, "rev-parse", "HEAD")
			if rebase, ok, err := tasks.ReadForkCandidateRebase(f.repo, f.beta); err != nil || !ok || rebase.Verification != rebaseCheckReport ||
				rebase.VerifiedTree != forkspace.GitOut(f.ws, "rev-parse", "HEAD^{tree}") || forkctl.CheckRebase(f.ws, "beta", rebase.Onto) != forkctl.RebaseFinished {
				t.Fatalf("request after the stop = %+v, %v, %v; want the finished rebase kept with its checks", rebase, ok, err)
			}
			f.passes.ctrlCIn = ""
			if code, out, err := f.loop(t); code != 0 || err != nil {
				t.Fatalf("rerun after the stop = (%d, %v)\n%s", code, err, out)
			}
			if passes := len(f.passes.rebases) + len(f.passes.checks); passes != 1 || len(f.passes.reviews) != 3 {
				t.Fatalf("rerun: %d rebase or check passes, %d new reviews; want no pass again and one review", passes, len(f.passes.reviews)-2)
			}
			if review := f.passes.reviews[2]; review.Head != rebased || review.RebaseVerification != rebaseCheckReport {
				t.Fatalf("review after the rerun = %+v; want the rebased head with the kept checks", review)
			}
			if code, out, err := mergeForkForTest(t, f.a, "beta"); code != 0 || err != nil {
				t.Fatalf("merge after the rerun = (%d, %v)\n%s", code, err, out)
			}
		})
	}
}

// A first Ctrl-C during a candidate review that finds something stops after that review, as its
// notice says ("Finishing this review, then stopping"): the findings are recorded, and the rework
// waits for the rerun instead of starting at once.
func TestForkCandidateReviewStopsAfterAFirstCtrlCBeforeItsRework(t *testing.T) {
	f := setUpConflictingForks(t)
	f.passes.attempts = []rebaseAttempt{rebaseAndResolve}
	f.passes.reportChecks, f.passes.ctrlCIn = true, "review"
	f.passes.findings = [][]tasks.ForkCandidateFinding{{{TaskID: f.second, Gate: "review", Finding: "the resolution needs a test"}}}
	works := f.passes.works
	code, out, err := f.loop(t)
	if code != loop.LoopInterruptedExitCode || err == nil || !strings.Contains(out, "Fork beta stopped") {
		t.Fatalf("loop stopped during its review = (%d, %v)\n%s", code, err, out)
	}
	if len(f.passes.reviews) != 3 || f.passes.works != works {
		t.Fatalf("after the stop: %d new reviews, %d rework passes; want the one review and no rework", len(f.passes.reviews)-2, f.passes.works-works)
	}
	f.passes.ctrlCIn = ""
	if code, out, err := f.loop(t); code != 0 || err != nil {
		t.Fatalf("rerun after the stop = (%d, %v)\n%s", code, err, out)
	}
	if f.passes.works != works+1 || len(f.passes.reviews) != 4 {
		t.Fatalf("rerun: %d rework passes, %d new reviews; want the rework and its review", f.passes.works-works, len(f.passes.reviews)-3)
	}
}

// N10 of the 2026-11-20 live pass: a first Ctrl-C during a fork's task pass lets the task finish
// and its between-task audit run, but skips its final review, so the engine exits 130. The fork loop
// then treats the pass like any unaccepted completion: its copy of the task goes back to in progress
// (RestoreFailedForkProjection), never accepted without that review, and the commit stays. The rerun
// takes the task again; its agent folds the review into the bound commit as the bound-rework refusal
// tells it (Coop-Recovery), and the task is reviewed and merges with one binding. That costs one work
// run, the same as after a crash between completion and acceptance, and skips no review.
func TestForkTaskPassStoppedByAFirstCtrlCIsReviewedOnTheRerun(t *testing.T) {
	repo := initRepo(t)
	ignoreTheQueue(t, repo)
	commitOnMain(t, repo, "shared.txt", "base\n")
	const id = "2026-11-20-stopped-task"
	seedForkDecisionTask(t, repo, id)
	a := forkNetworkPreflightApp(t, repo)
	passes := &sharedFilePasses{t: t, ctrlCIn: "work"}
	a.forkLoopRun = passes.run
	ws, identity := setUpForkForDecision(t, repo, "alpha")
	code, out, err := runForkLoopStoppingAfterReview(t, a, repo, ws, identity)
	if code != loop.LoopInterruptedExitCode || passes.works != 1 || len(passes.reviews) != 0 {
		t.Fatalf("loop stopped during its task pass = (%d, %v), %d work passes, %d reviews\n%s", code, err, passes.works, len(passes.reviews), out)
	}
	if _, err := os.Stat(filepath.Join(passes.lastQueue, tasks.StateInProgress, id)); err != nil {
		t.Fatalf("the stopped pass's task was not put back in progress for its review: %v", err)
	}
	if bound := tasks.CommitsForTask(ws, "", id); len(bound) != 1 {
		t.Fatalf("the stopped pass's commit = %v; want it kept", bound)
	}
	passes.ctrlCIn = ""
	passes.work = func(ws, id string) {
		git(t, ws, "commit", "-q", "--amend", "--only", "--no-edit", "--trailer", "Coop-Recovery: 2026-11-20T12:00:00Z")
	}
	if code, out, err := runForkLoopStoppingAfterReview(t, a, repo, ws, identity); code != 0 || err != nil {
		t.Fatalf("rerun after the stop = (%d, %v)\n%s", code, err, out)
	}
	if passes.works != 2 || len(passes.reviews) != 1 {
		t.Fatalf("rerun: %d work passes, %d reviews; want one more work pass and the review", passes.works, len(passes.reviews))
	}
	if code, out, err := mergeForkForTest(t, a, "alpha"); code != 0 || err != nil {
		t.Fatalf("merge after the rerun = (%d, %v)\n%s", code, err, out)
	}
	if bound := tasks.CommitsForTask(repo, "", id); len(bound) != 1 {
		t.Fatalf("merged bindings = %v; want one", bound)
	}
}

// A gate that writes to tracked files makes every check pass end dirty. The refusal says that is the
// likely cause and what to change, instead of only "changed".
func TestForkRebaseCheckPassThatDirtiesTrackedFilesSaysWhy(t *testing.T) {
	f := setUpConflictingForks(t)
	if !rebaseKeepingEveryLine(t, f.ws, f.parent) {
		t.FailNow()
	}
	f.passes.reportChecks, f.passes.checkDirties = true, true
	code, out, err := f.loop(t)
	if code == 0 || err == nil || !strings.Contains(err.Error(), "changed during its rebase check") ||
		!strings.Contains(err.Error(), "writes to files git tracks") || !strings.Contains(err.Error(), "ignored paths") {
		t.Fatalf("loop whose check left tracked files changed = (%d, %v)\n%s", code, err, out)
	}
	if len(f.passes.reviews) != 2 {
		t.Fatalf("a dirty check pass reached review (%d reviews)", len(f.passes.reviews))
	}
}

// A check pass that leaves git mid-rebase gets advice that works there: abort the rebase first,
// since git will not switch branches during one. Following it puts back the rebased commit.
func TestForkRebaseCheckPassLeftMidRebaseNamesTheAbort(t *testing.T) {
	f := setUpConflictingForks(t)
	if !rebaseKeepingEveryLine(t, f.ws, f.parent) {
		t.FailNow()
	}
	rebased := forkspace.GitOut(f.ws, "rev-parse", "HEAD")
	f.passes.checkStopsRebase = true
	code, out, err := f.loop(t)
	wantBack := "git -C " + shellWord(f.ws) + " rebase --abort && git -C " + shellWord(f.ws) + " checkout -f -B beta " + rebased
	if code == 0 || err == nil || !strings.Contains(err.Error(), wantBack) {
		t.Fatalf("loop whose check pass stopped mid-rebase = (%d, %v); want %q\n%s", code, err, wantBack, out)
	}
	git(t, f.ws, "rebase", "--abort")
	git(t, f.ws, "checkout", "-q", "-f", "-B", "beta", rebased)
	f.passes.checkStopsRebase, f.passes.reportChecks = false, true
	if code, out, err := f.loop(t); code != 0 || err != nil {
		t.Fatalf("loop after following the advice = (%d, %v)\n%s", code, err, out)
	}
}

// After a real conflict, the rebase (here the agent's) skips a task's commit because main already
// has its change. The loop spends that one agent run, then refuses with the plain skipped-commit
// answer and records it, so a second loop runs no agent, starts no round, and ls and merge say the
// same as the loop.
func TestForkRebaseThatSkipsATaskCommitAfterAConflictStopsForGood(t *testing.T) {
	repo := initRepo(t)
	ignoreTheQueue(t, repo)
	commitOnMain(t, repo, "shared.txt", "base\n")
	commitOnMain(t, repo, "notes.txt", "one\nZ\ntwo\n")
	first, second := "2026-11-19-skip-a", "2026-11-19-skip-b"
	seedForkDecisionTask(t, repo, first)
	seedForkDecisionTask(t, repo, second)
	a := forkNetworkPreflightApp(t, repo)
	passes := &sharedFilePasses{t: t, reportChecks: true}
	passes.work = func(ws, id string) {
		file, content := "shared.txt", "beta\n"
		if id == first {
			file, content = "notes.txt", "one\ntwo\n" // A: removes the line Z
		}
		if err := os.WriteFile(filepath.Join(ws, file), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, ws, "commit", "-qam", "Do "+id+"\n\nCoop-Task: "+id)
	}
	a.forkLoopRun = passes.run
	ws, beta := setUpForkForDecision(t, repo, "beta")
	if code, out, err := runForkLoopForDecision(t, a, repo, ws, beta); code != 0 || err != nil { // both tasks, one review
		t.Fatalf("fork loop = (%d, %v)\n%s", code, err, out)
	}
	reviewed, ok, err := tasks.ReadForkCandidate(repo, beta)
	if err != nil || !ok || len(reviewed.Assignments) != 2 {
		t.Fatalf("reviewed candidate = %+v, %v, %v; want both tasks", reviewed, ok, err)
	}
	commitOnMain(t, repo, "notes.txt", "one\ntwo\n") // A's patch
	parent := commitOnMain(t, repo, "shared.txt", "main\n")
	if code, out, err := mergeForkForTest(t, a, "beta"); code == 0 || err == nil || !strings.Contains(err.Error(), "conflict with main") {
		t.Fatalf("conflicting merge = (%d, %v)\n%s", code, err, out)
	}
	if request, ok, err := tasks.ReadForkCandidateRebase(repo, beta); err != nil || !ok || request.Cause != tasks.RebaseCauseConflict {
		t.Fatalf("request after the conflict = %+v, %v, %v", request, ok, err)
	}
	reviews := len(passes.reviews)
	refusal := func(step string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "reviewed commit is already in main's history") || !strings.Contains(err.Error(), "coop fork rm beta --force") {
			t.Fatalf("%s = %v; want the skipped-commit refusal", step, err)
		}
	}
	passes.attempts = []rebaseAttempt{rebaseAndResolve}
	code, out, err := runForkLoopStoppingAfterReview(t, a, repo, ws, beta)
	if code == 0 {
		t.Fatalf("loop whose rebase skipped A = (%d, %v)\n%s", code, err, out)
	}
	refusal("loop whose rebase skipped A", err)
	if !strings.Contains(err.Error(), "Its commit ") {
		t.Errorf("the refusal does not name the skipped commit: %v", err)
	}
	if len(passes.rebases) != 1 || len(passes.checks) != 0 || len(passes.reviews) != reviews {
		t.Fatalf("%d rebase, %d check passes, %d new reviews; want one agent run and no review", len(passes.rebases), len(passes.checks), len(passes.reviews)-reviews)
	}
	if request, ok, err := tasks.ReadForkCandidateRebase(repo, beta); err != nil || !ok || request.Cause != tasks.RebaseCauseSkipped || request.Onto != parent {
		t.Fatalf("request after the skip = %+v, %v, %v; want it marked skipped", request, ok, err)
	}
	code, _, err = runForkLoopStoppingAfterReview(t, a, repo, ws, beta)
	if code == 0 {
		t.Fatal("second loop passed")
	}
	refusal("second loop", err)
	if !strings.Contains(err.Error(), "No agent ran") || len(passes.rebases) != 1 || len(passes.checks) != 0 || len(passes.reviews) != reviews {
		t.Fatalf("second loop ran %d rebase, %d check passes, %d reviews: %v", len(passes.rebases), len(passes.checks), len(passes.reviews)-reviews, err)
	}
	if candidate, ok, err := tasks.ReadForkCandidate(repo, beta); err != nil || !ok || candidate.ID != reviewed.ID {
		t.Fatalf("candidate = %+v, %v, %v; want the reviewed round, no new one", candidate, ok, err)
	}
	if code, _, err := mergeForkForTest(t, a, "beta"); code == 0 {
		t.Fatal("merge after the skip passed")
	} else {
		refusal("merge after the skip", err)
	}
	listing := captureStdout(t, func() {
		if code, err := a.forkctl().ForkLs(nil); code != 0 || err != nil {
			t.Fatalf("fork ls = (%d, %v)", code, err)
		}
	})
	if !strings.Contains(listing, "main's history already has one of its task commits") || strings.Contains(listing, "review it") {
		t.Errorf("fork ls disagrees with the loop:\n%s", listing)
	}
	if got := forkspace.GitOut(repo, "rev-parse", "HEAD"); got != parent {
		t.Fatalf("main moved to %s", got)
	}
}
