package forkctl

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

// The box writes the reviewed commit into the fork's shallow file, so git treats it as a root: the
// land rebase would replay it as all-new (a deletion it made would not land) and git's ancestry
// answers would skip the tree check. The land refuses a shallow fork, and so does the rebase check.
func TestLandRefusesAShallowFork(t *testing.T) {
	deletes := func(t *testing.T, ws, id string) {
		git(t, ws, "rm", "-q", "README.md")
		git(t, ws, "commit", "-qm", "remove the readme\n\nCoop-Task: "+id)
	}
	repo, ws, _, identity, c := prepareForkTaskCandidateHistory(t, "task-shallow", false, deletes, nil)
	candidate, ok, err := tasks.ReadForkCandidate(repo, identity)
	if err != nil || !ok {
		t.Fatalf("candidate = %v, %v", ok, err)
	}
	parent := forkspace.GitOut(repo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(ws, ".git", "shallow"), []byte(candidate.Head+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	landed, err := mergeOneForTest(t, c, repo, "", identity.Name, false)
	if landed || err == nil || !strings.Contains(err.Error(), "repository is shallow") || !strings.Contains(err.Error(), "fetch --unshallow") || strings.Contains(err.Error(), "rm /") || strings.Contains(err.Error(), "never shallow") {
		t.Fatalf("merge of a shallow fork = (%v, %v)", landed, err)
	}
	if forkspace.GitOut(repo, "rev-parse", "HEAD") != parent {
		t.Fatal("a shallow fork's merge moved the parent")
	}
	if got := CheckRebase(ws, identity.Name, parent); got != RebaseShallow {
		t.Fatalf("rebase check of a shallow fork = %v, want shallow", got)
	}
	if pending, err := ForkHasPendingLand(repo, identity); err != nil || pending {
		t.Fatalf("land journal after the refusal = %v, %v", pending, err)
	}
	// The command refuses before its header, whose commit count a shallow history makes meaningless.
	if c.cfg == nil {
		c.cfg = &config.Config{}
	}
	c.cfg.RepoOverride = repo
	var code int
	notes := captureStderr(t, func() { code, err = c.ForkMerge([]string{identity.Name, "--yes"}) })
	if code == 0 || err == nil || !strings.Contains(err.Error(), "repository is shallow") || strings.Contains(notes, "Merge fork") {
		t.Fatalf("coop fork merge of a shallow fork = (%d, %v), printed:\n%s", code, err, notes)
	}
	// merge --all agrees: it lands nothing and gives the same refusal.
	notes = captureStderr(t, func() { code, err = c.ForkMerge([]string{"--all", "--yes"}) })
	if code == 0 || !strings.Contains(notes+fmt.Sprint(err), "repository is shallow") || forkspace.GitOut(repo, "rev-parse", "HEAD") != parent {
		t.Fatalf("coop fork merge --all of a shallow fork = (%d, %v), printed:\n%s", code, err, notes)
	}
}

// Reviewed commits A (removes the readme's line), B (adds it back) and C (adds g.txt): the reviewed
// change is g.txt alone. The branch meanwhile gains a commit identical to A. A plain rebase skips A
// as already applied, replays B, and would put the line back on the branch though no reviewer saw
// that change. The land refuses, and records a rebase so the fork's loop has it reviewed against the
// branch.
func TestLandRefusesAChangeTheRebaseWouldAlter(t *testing.T) {
	history := func(t *testing.T, ws, id string) {
		for _, step := range []struct{ content, message string }{
			{"", "remove the line"},
			{"hi\n", "add the line back"},
		} {
			if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte(step.content), 0o644); err != nil {
				t.Fatal(err)
			}
			git(t, ws, "commit", "-qam", step.message)
		}
		if err := os.WriteFile(filepath.Join(ws, "g.txt"), []byte("g\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, ws, "add", "g.txt")
		git(t, ws, "commit", "-qm", "add g\n\nCoop-Task: "+id)
	}
	repo, _, _, identity, c := prepareForkTaskCandidateHistory(t, "task-cherry", false, history, nil)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "commit", "-qam", "main removes the line as well") // the same patch as A, another commit
	parent := forkspace.GitOut(repo, "rev-parse", "HEAD")
	landed, err := mergeOneForTest(t, c, repo, "", identity.Name, false)
	if landed || err == nil || !strings.Contains(err.Error(), "would land changed") || !strings.Contains(err.Error(), "coop fork "+identity.Name+" --loop") {
		t.Fatalf("merge whose rebase alters the reviewed change = (%v, %v)", landed, err)
	}
	if data, _ := os.ReadFile(filepath.Join(repo, "README.md")); len(data) != 0 || forkspace.GitOut(repo, "rev-parse", "HEAD") != parent {
		t.Fatalf("the parent changed: README %q", data)
	}
	if rebase, ok, err := tasks.ReadForkCandidateRebase(repo, identity); err != nil || !ok || rebase.Onto != parent || rebase.Cause != tasks.RebaseCauseChanged {
		t.Fatalf("rebase request = %+v, %v, %v; want one onto %s for a changed land", rebase, ok, err, parent)
	}
	if pending, err := ForkHasPendingLand(repo, identity); err != nil || pending {
		t.Fatalf("land journal after the refusal = %v, %v", pending, err)
	}
	// There is no textual conflict, so ls does not say there is one.
	s := c.gatherForkStatus(repo, identity.Name, tasks.ReadProjectSnapshot(repo, nil))
	if s.stateCell() != "changed" || s.driftLine() != "Changed: its reviewed change would land changed on main; rebase and review: coop fork "+identity.Name+" --loop" {
		t.Fatalf("fork after a changed land: state %q, %q", s.stateCell(), s.driftLine())
	}
	// And so the listing itself reads: changed, never "conflicts".
	if c.cfg == nil {
		c.cfg = &config.Config{}
	}
	c.cfg.RepoOverride = repo
	listing := captureStdout(t, func() {
		if code, err := c.ForkLs(nil); code != 0 || err != nil {
			t.Fatalf("fork ls = (%d, %v)", code, err)
		}
	})
	if !strings.Contains(listing, "changed after review") || !strings.Contains(listing, "would land changed on main") ||
		strings.Contains(strings.ToLower(listing), "conflict") {
		t.Fatalf("fork ls after a changed land:\n%s", listing)
	}
}

// Reviewed commits A (removes the readme's line, bound to the task) and C (adds g.txt), on a branch
// that meanwhile committed A's patch itself. A rebase skips A, so its task would land with no commit
// of its own, and a rebase pass can only skip it again or keep it empty. The land stops at once and
// says the two ways on; the loop runs no agent for it, a rebase onto that parent is never admitted,
// and ls says the same as merge.
func TestLandStopsWhenTheBranchAlreadyHasATaskCommit(t *testing.T) {
	history := func(t *testing.T, ws, id string) {
		if err := os.WriteFile(filepath.Join(ws, "README.md"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, ws, "commit", "-qam", "remove the line\n\nCoop-Task: "+id)
		if err := os.WriteFile(filepath.Join(ws, "g.txt"), []byte("g\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, ws, "add", "g.txt")
		git(t, ws, "commit", "-qm", "add g")
	}
	repo, ws, _, identity, c := prepareForkTaskCandidateHistory(t, "task-skipped", false, history, nil)
	name := identity.Name
	candidate, ok, err := tasks.ReadForkCandidate(repo, identity)
	if err != nil || !ok {
		t.Fatalf("candidate = %v, %v", ok, err)
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "commit", "-qam", "main removes the line itself") // A's patch, another commit
	parent := forkspace.GitOut(repo, "rev-parse", "HEAD")
	for attempt := range 2 { // the same plain answer every time, never a rebase to try
		landed, err := mergeOneForTest(t, c, repo, "", name, false)
		if landed || err == nil {
			t.Fatalf("merge %d whose rebase skips a task's commit = (%v, %v)", attempt, landed, err)
		}
		for _, want := range []string{"reviewed commit is already in main's history", "Coop-Task: ", "is on 0 commit(s)", "coop fork rm " + name + " --force", "Nothing was merged."} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("merge %d refusal does not say %q: %v", attempt, want, err)
			}
		}
		if strings.Contains(err.Error(), "--loop") {
			t.Errorf("merge %d sends a skipped commit to the loop: %v", attempt, err)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, "g.txt")); !os.IsNotExist(err) || forkspace.GitOut(repo, "rev-parse", "HEAD") != parent {
		t.Fatalf("the parent changed: g.txt %v", err)
	}
	if forkspace.GitOut(ws, "rev-parse", "HEAD") != candidate.Head {
		t.Fatal("the refused land left the fork off its reviewed commit")
	}
	rebase, ok, err := tasks.ReadForkCandidateRebase(repo, identity)
	if err != nil || !ok || rebase.Cause != tasks.RebaseCauseSkipped || rebase.Onto != parent {
		t.Fatalf("request after the refusal = %+v, %v, %v; want a skipped commit's", rebase, ok, err)
	}
	if pending, err := ForkHasPendingLand(repo, identity); err != nil || pending {
		t.Fatalf("land journal after the refusal = %v, %v", pending, err)
	}
	if got := SkippedCommits(ws, parent, candidate.Head); len(got) != 1 {
		t.Fatalf("skipped commits = %q; want A alone", got)
	}
	if err := RefuseDivergedFork(repo, identity); err == nil || !strings.Contains(err.Error(), "reviewed commit is already in main's history") {
		t.Fatalf("loop start after a skipped commit = %v; want the refusal before the loop starts", err)
	}
	loopErr := SkippedCommitError(ws, name, candidate, rebase, nil, " No agent ran.", false)
	if !strings.Contains(loopErr.Error(), "Its commit ") || !strings.Contains(loopErr.Error(), "No agent ran.") || strings.Contains(loopErr.Error(), "reset --hard") {
		t.Fatalf("loop refusal = %v", loopErr)
	}
	s := c.gatherForkStatus(repo, name, tasks.ReadProjectSnapshot(repo, nil))
	if s.stateCell() != "changed" || !strings.Contains(s.driftLine(), "main's history already has one of its task commits") {
		t.Fatalf("fork after a skipped commit: state %q, %q", s.stateCell(), s.driftLine())
	}
	// A hand rebase onto that parent changes nothing: merge says the same, with the way back.
	git(t, ws, "rebase", "-q", parent)
	landed, err := mergeOneForTest(t, c, repo, "", name, false)
	if landed || err == nil || !strings.Contains(err.Error(), "reviewed commit is already in main's history") || !strings.Contains(err.Error(), "reset --hard "+candidate.Head) {
		t.Fatalf("merge after a hand rebase = (%v, %v)", landed, err)
	}
	if _, err := tasks.SupersedeStaleForkCandidate(repo, identity, forkspace.GitOut(ws, "rev-parse", "HEAD")); err == nil {
		t.Fatal("a rebase onto a skipped commit's parent was admitted to review")
	}
	git(t, ws, "reset", "-q", "--hard", candidate.Head)

	// A revert on main is not enough: git rebase still skips a commit whose change is anywhere in
	// main's history since the fork started, and the refusal says so.
	git(t, repo, "revert", "--no-edit", "HEAD")
	landed, err = mergeOneForTest(t, c, repo, "", name, false)
	if landed || err == nil || !strings.Contains(err.Error(), "reviewed commit is already in main's history") ||
		!strings.Contains(err.Error(), "reverting the change on main does not either") || !strings.Contains(err.Error(), "from its history") {
		t.Fatalf("merge after main reverted its commit = (%v, %v); want the same refusal, saying a revert is not enough", landed, err)
	}

	// Removing main's commit from its history is. A land that then gets past the checks closes the
	// request, so a red merge gate leaves an ordinary reviewed fork, not a stale skipped refusal.
	git(t, repo, "reset", "-q", "--hard", "HEAD~2")
	c.gateOK = func(_, _, _ string) bool { return false }
	if landed, err := mergeOneForTest(t, c, repo, "img", name, false); landed || err == nil || !strings.Contains(err.Error(), "gate failed") {
		t.Fatalf("merge with a red gate = (%v, %v)", landed, err)
	}
	if rebase, ok, err := tasks.ReadForkCandidateRebase(repo, identity); err != nil || ok {
		t.Fatalf("request after a land past the checks = %+v, %v, %v; want it closed", rebase, ok, err)
	}
	if s := c.gatherForkStatus(repo, name, tasks.ReadProjectSnapshot(repo, nil)); s.stateCell() != "ready" {
		t.Fatalf("fork after the red gate: state %q, %q; want ready again", s.stateCell(), s.driftLine())
	}
	c.gateOK = func(_, _, _ string) bool { return true }
	if landed, err := mergeOneForTest(t, c, repo, "img", name, false); !landed || err != nil {
		t.Fatalf("merge once main no longer has the commit = (%v, %v)", landed, err)
	}
	if _, err := os.Stat(filepath.Join(repo, "g.txt")); err != nil {
		t.Fatalf("the reviewed work did not land: %v", err)
	}
}

// The skipped-commit refusal's ways out fit about 100 columns: its long second way is a sentence on
// its own row, so no label pads the commands. The go-back command is its own row with no padding,
// the one line a long workspace path may widen.
func TestSkippedCommitRefusalRowsFitTheTerminal(t *testing.T) {
	const sha = "0af777e0f091122d34f5ad653ec7cc3a478a2961"
	for _, ws := range []string{
		"/Users/someone/Projects/app-forks/o",
		"/Users/a-rather-long-user-name/Projects/client-work/some-quite-long-repository-name-forks/o",
	} {
		for _, back := range []string{"", sha} {
			var usage *ui.UsageError
			if !errors.As(skippedCommitError(ws, "o", "main", "Its commit 7d8f86cc56ee is already there.", " Nothing was merged.", back), &usage) {
				t.Fatal("the refusal is not a usage block")
			}
			command := GoBackCommand(ws, "reset", "--hard", sha)
			out := usage.Render(ui.Palette{})
			for _, line := range strings.Split(out, "\n") {
				if strings.HasPrefix(line, "      ") { // the cause, one sentence the terminal wraps
					continue
				}
				if back != "" && line == "  "+command {
					continue
				}
				if n := len([]rune(line)); n > 100 {
					t.Errorf("ws %q back %q: a %d-column line:\n%s", ws, back, n, out)
				}
			}
			if back != "" && !strings.Contains(out, "\n  "+command+"\n") {
				t.Errorf("the go-back command is not its own unpadded row:\n%s", out)
			}
			if !strings.Contains(out, "coop fork rm o --force") || !strings.Contains(out, "coop fork merge o") {
				t.Errorf("a way out is missing:\n%s", out)
			}
		}
	}
	// A realistic path keeps even the go-back command within about 100 columns.
	if n := len("  " + GoBackCommand("/Users/someone/Projects/app-forks/o", "reset", "--hard", sha)); n > 100 {
		t.Errorf("go-back command for a realistic path is %d columns", n)
	}
}

// Reviewed A (removes the line, bound to the task), A' (puts it back, no trailer) and C (adds g.txt),
// on a branch that made A's change and then A”s. The rebase skips both, and the line edits still
// match the reviewed change (g.txt alone), but the task would land with no commit of its own. The
// land refuses it as the other skipped-commit case, rather than marking the task done without one.
func TestLandRefusesASkippedTaskCommitEvenWhenTheEditsMatch(t *testing.T) {
	setReadme := func(t *testing.T, dir, content, message string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, dir, "commit", "-qam", message)
	}
	original := ""
	history := func(t *testing.T, ws, id string) {
		data, err := os.ReadFile(filepath.Join(ws, "README.md"))
		if err != nil {
			t.Fatal(err)
		}
		original = string(data)
		setReadme(t, ws, "", "remove the line\n\nCoop-Task: "+id)
		setReadme(t, ws, original, "put the line back")
		if err := os.WriteFile(filepath.Join(ws, "g.txt"), []byte("g\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, ws, "add", "g.txt")
		git(t, ws, "commit", "-qm", "add g")
	}
	repo, _, _, identity, c := prepareForkTaskCandidateHistory(t, "task-skipped-match", false, history, nil)
	setReadme(t, repo, "", "main removes the line")
	setReadme(t, repo, original, "main puts it back")
	parent := forkspace.GitOut(repo, "rev-parse", "HEAD")
	landed, err := mergeOneForTest(t, c, repo, "", identity.Name, false)
	if landed || err == nil || !strings.Contains(err.Error(), "reviewed commit is already in main's history") || !strings.Contains(err.Error(), "is on 0 commit(s)") {
		t.Fatalf("merge whose rebase skips the task's commit with matching edits = (%v, %v)", landed, err)
	}
	if forkspace.GitOut(repo, "rev-parse", "HEAD") != parent {
		t.Fatal("the parent moved")
	}
	if rebase, ok, err := tasks.ReadForkCandidateRebase(repo, identity); err != nil || !ok || rebase.Cause != tasks.RebaseCauseSkipped {
		t.Fatalf("request = %+v, %v, %v; want a skipped commit's", rebase, ok, err)
	}
}

// A binding problem from another cause (here a task commit doubled) is not a skipped commit just
// because some other commit, one without a trailer, matches a parent commit's patch: the skipped
// commits that count are the ones whose own task lost a commit.
func TestSkippedTaskCommitsAreTiedToTheirBinding(t *testing.T) {
	ws := t.TempDir()
	git(t, ws, "init", "-q", "-b", "main")
	write := func(file, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(ws, file), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, ws, "add", file)
	}
	write("a.txt", "a\n")
	git(t, ws, "commit", "-qm", "base")
	git(t, ws, "checkout", "-q", "-b", "fork")
	write("plain.txt", "plain\n")
	git(t, ws, "commit", "-qm", "untrailered change the parent also makes")
	write("task.txt", "task\n")
	git(t, ws, "commit", "-qm", "task work\n\nCoop-Task: task-a")
	before := forkspace.GitOut(ws, "rev-parse", "HEAD")
	git(t, ws, "checkout", "-q", "main")
	write("plain.txt", "plain\n")
	git(t, ws, "commit", "-qm", "the parent makes the untrailered change")
	onto := forkspace.GitOut(ws, "rev-parse", "HEAD")
	git(t, ws, "checkout", "-q", "fork")
	git(t, ws, "rebase", "-q", onto) // skips the untrailered commit
	git(t, ws, "commit", "-q", "--allow-empty", "-m", "doubled\n\nCoop-Task: task-a")
	after := forkspace.GitOut(ws, "rev-parse", "HEAD")

	skipped := SkippedCommits(ws, onto, before)
	if len(skipped) != 1 {
		t.Fatalf("git's patch matching = %q; want the untrailered commit", skipped)
	}
	if bound := SkippedTaskCommits(ws, onto, before); len(bound) != 0 {
		t.Fatalf("skipped task commits = %q; want none: the skipped commit binds no task", bound)
	}
	if dropped, err := tasks.RebaseDroppedTaskCommits(ws, onto, before, after, skipped); err != nil || len(dropped) != 0 {
		t.Fatalf("dropped task commits = %q, %v; want none: task-a gained a commit", dropped, err)
	}
}

// A shallow fork is refused by merge and the loop; ls says so too, rather than "conflicts" or "ready".
func TestForkLsShowsAShallowFork(t *testing.T) {
	repo, ws, _, identity, c := prepareForkTaskCandidate(t, "task-shallow-ls")
	candidate, ok, err := tasks.ReadForkCandidate(repo, identity)
	if err != nil || !ok {
		t.Fatalf("candidate = %v, %v", ok, err)
	}
	if s := c.gatherForkStatus(repo, identity.Name, tasks.ReadProjectSnapshot(repo, nil)); s.stateCell() != "ready" {
		t.Fatalf("fork before the shallow file: state %q", s.stateCell())
	}
	if err := os.WriteFile(filepath.Join(ws, ".git", "shallow"), []byte(candidate.Head+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := c.gatherForkStatus(repo, identity.Name, tasks.ReadProjectSnapshot(repo, nil))
	if s.stateCell() != "changed" || s.driftLine() != "Changed: its repository is shallow; coop fork merge "+identity.Name+" says how to go on" {
		t.Fatalf("shallow fork: state %q, %q", s.stateCell(), s.driftLine())
	}
}

// The count that decides whether the bindings are read fails closed: a count git cannot give is an
// error the land refuses on, never "nothing dropped", which would skip the binding check.
func TestRebaseDroppedCommitsFailsClosed(t *testing.T) {
	_, ws, _, _, _ := prepareForkTaskCandidate(t, "task-count")
	head := forkspace.GitOut(ws, "rev-parse", "HEAD")
	base := forkspace.GitOut(ws, "rev-parse", "HEAD~1")
	if dropped, err := rebaseDroppedCommits(ws, base, head, base, head); err != nil || dropped {
		t.Fatalf("same commits = %v, %v; want none dropped", dropped, err)
	}
	if dropped, err := rebaseDroppedCommits(ws, head, head, base, head); err != nil || !dropped {
		t.Fatalf("a rebase that left no commit = %v, %v; want dropped", dropped, err)
	}
	missing := strings.Repeat("e", 40)
	for _, args := range [][4]string{{base, missing, base, head}, {base, head, base, missing}, {missing, missing, missing, missing}} {
		if dropped, err := rebaseDroppedCommits(ws, args[0], args[1], args[2], args[3]); err == nil {
			t.Errorf("count over a missing commit %v = %v with no error; want a refusal", args, dropped)
		}
	}
}

// Two merge commits are named in the plural.
func TestMergeCommitsCauseAgreesInNumber(t *testing.T) {
	one, two := strings.Repeat("a", 40), strings.Repeat("b", 40)
	if got := mergeCommitsCause([]string{one}); !strings.HasPrefix(got, "Commit "+one[:12]+" merges two lines") {
		t.Errorf("one merge = %q", got)
	}
	if got := mergeCommitsCause([]string{one, two}); !strings.HasPrefix(got, "Commits "+one[:12]+", "+two[:12]+" merge two lines") {
		t.Errorf("two merges = %q", got)
	}
}

// An honest land onto a moved branch passes the check: the branch's own commit touches another file.
func TestLandAcceptsTheReviewedChangeOnAMovedBranch(t *testing.T) {
	repo, _, _, identity, c := prepareForkTaskCandidate(t, "task-moved")
	if err := os.WriteFile(filepath.Join(repo, "other.txt"), []byte("other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "other.txt")
	git(t, repo, "commit", "-qm", "the branch moves on")
	if landed, err := mergeOneForTest(t, c, repo, "", identity.Name, false); !landed || err != nil {
		t.Fatalf("honest land onto a moved branch = (%v, %v)", landed, err)
	}
	for _, file := range []string{"feature.txt", "other.txt"} {
		if _, err := os.Stat(filepath.Join(repo, file)); errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s did not land", file)
		}
	}
}

// The isolated port of TestLandRefusesAChangeTheRebaseWouldAlter. The private rebase's join commits
// keep it from replaying published source, but not from skipping A, whose patch the branch has since
// the fork was created, and then replaying B, which would put the line back on the branch. Nothing of
// the branch may enter an isolated box, so the merge refuses with a person's two ways out and records
// it only for coop fork ls, never as a rebase for the loop.
func TestIsolatedLandRefusesAChangeTheRebaseWouldAlter(t *testing.T) {
	history := func(t *testing.T, ws, id string) {
		for _, step := range []struct{ content, message string }{
			{"", "remove the line"},
			{"hi\n", "add the line back"},
		} {
			if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte(step.content), 0o644); err != nil {
				t.Fatal(err)
			}
			git(t, ws, "commit", "-qam", step.message)
		}
		if err := os.WriteFile(filepath.Join(ws, "g.txt"), []byte("g\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, ws, "add", "g.txt")
		git(t, ws, "commit", "-qm", "add g\n\nCoop-Task: "+id)
	}
	repo, _, _, identity, c := prepareForkTaskCandidateHistory(t, "task-cherry-isolated", true, history, nil)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "commit", "-qam", "main removes the line as well") // the same patch as A, another commit
	assertIsolatedChangedLandRefused(t, c, repo, identity, "would not make exactly the reviewed change")
}

// A reviewed task commit whose patch the branch already has drops out of the private rebase, and its
// task keeps no commit: refused, as for an ordinary fork, before anything merges.
func TestIsolatedLandRefusesASkippedTaskCommit(t *testing.T) {
	removes := func(t *testing.T, ws, id string) {
		if err := os.WriteFile(filepath.Join(ws, "README.md"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, ws, "commit", "-qam", "remove the line\n\nCoop-Task: "+id)
	}
	repo, _, _, identity, c := prepareForkTaskCandidateHistory(t, "task-skipped-isolated", true, removes, nil)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "commit", "-qam", "main removes the line as well")
	assertIsolatedChangedLandRefused(t, c, repo, identity, "belongs to a task")
}

func assertIsolatedChangedLandRefused(t *testing.T, c *Control, repo string, identity forkspace.Identity, cause string) {
	t.Helper()
	parent := forkspace.GitOut(repo, "rev-parse", "HEAD")
	// The person is not asked to merge what the check then refuses.
	unlock, err := forkspace.LockState(repo, identity.Name)
	if err != nil {
		t.Fatal(err)
	}
	asked := false
	outcome, err := c.mergeIsolatedLocked(repo, "", identity.Name, identity, false, false, func(isolatedCandidate) bool { asked = true; return true })
	unlock()
	if asked || outcome.landed || err == nil || !strings.Contains(err.Error(), cause) {
		t.Fatalf("isolated merge with a confirmation = (%v, %v), asked %v; want the refusal before any question", outcome.landed, err, asked)
	}
	landed, err := mergeOneForTest(t, c, repo, "", identity.Name, false)
	if landed || err == nil || !strings.Contains(err.Error(), "would land changed on main") || !strings.Contains(err.Error(), cause) ||
		!strings.Contains(err.Error(), "coop fork rm "+identity.Name+" --force") || strings.Contains(err.Error(), "--loop") {
		t.Fatalf("isolated merge whose rebase alters the reviewed change = (%v, %v)", landed, err)
	}
	if data, _ := os.ReadFile(filepath.Join(repo, "README.md")); len(data) != 0 || forkspace.GitOut(repo, "rev-parse", "HEAD") != parent {
		t.Fatalf("the parent changed: README %q", data)
	}
	if rebase, ok, err := tasks.ReadForkCandidateRebase(repo, identity); err != nil || !ok || rebase.Cause != tasks.RebaseCauseIsolatedChanged || rebase.Onto != parent {
		t.Fatalf("record after an isolated changed land = %+v, %v, %v; want the display-only isolated-changed cause", rebase, ok, err)
	}
	if _, pending, err := readIsolatedIntent(repo, identity); err != nil || pending {
		t.Fatalf("land journal after the refusal = %v, %v", pending, err)
	}
	s := c.gatherForkStatus(repo, identity.Name, tasks.ReadProjectSnapshot(repo, nil))
	if s.stateCell() != "changed" || !strings.Contains(s.driftLine(), "would land changed on main") ||
		!strings.Contains(s.driftLine(), "coop fork rm "+identity.Name+" --force") || strings.Contains(s.driftLine(), "--loop") {
		t.Fatalf("ls after an isolated changed land: state %q, %q; want changed with the merge's two ways out", s.stateCell(), s.driftLine())
	}
	// Like an isolated conflict, the record is no rebase request: a loop start on the reviewed commit
	// gets the merge's refusal with no agent, and the fence admits no HEAD built on the parent.
	if err := RefuseDivergedFork(repo, identity); err == nil || !strings.Contains(err.Error(), "would land changed on main") || !strings.Contains(err.Error(), "No agent ran") {
		t.Fatalf("loop start after an isolated changed land = %v; want the refusal before any agent", err)
	}
}

// coop fork ls agrees with merge about an isolated fork's shallow file: one at its boundary, as a
// fork of a shallow clone has, is ready and merges; one that cuts its reviewed commits is shallow in
// both. Before, ls called every shallow isolated fork changed.
func TestLsAgreesWithMergeAboutAnIsolatedForksShallowFile(t *testing.T) {
	work := func(t *testing.T, ws, id string) {
		if err := os.WriteFile(filepath.Join(ws, "g.txt"), []byte("g\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, ws, "add", "g.txt")
		git(t, ws, "commit", "-qm", "add g\n\nCoop-Task: "+id)
	}
	repo, ws, _, identity, c := prepareForkTaskCandidateHistory(t, "task-isolated-shallow-ls", true, work, nil)
	base, err := forkspace.IsolatedCreationBase(repo, identity)
	if err != nil {
		t.Fatal(err)
	}
	shallow := filepath.Join(ws, ".git", "shallow")
	writeTaskFile(t, shallow, forkspace.GitOut(ws, "rev-parse", "HEAD")+"\n")
	if s := c.gatherForkStatus(repo, identity.Name, tasks.ReadProjectSnapshot(repo, nil)); s.Drift != driftShallow {
		t.Fatalf("ls with a cut into the reviewed commits = %q, %q; want shallow", s.stateCell(), s.driftLine())
	}
	if landed, err := mergeOneForTest(t, c, repo, "", identity.Name, false); landed || err == nil || !strings.Contains(err.Error(), "repository is shallow") {
		t.Fatalf("merge with a cut into the reviewed commits = (%v, %v); want the shallow refusal", landed, err)
	}
	writeTaskFile(t, shallow, base+"\n")
	if s := c.gatherForkStatus(repo, identity.Name, tasks.ReadProjectSnapshot(repo, nil)); s.stateCell() != "ready" || s.Drift != driftNone {
		t.Fatalf("ls with a shallow file at the boundary = %q, %q; want ready, as merge allows it", s.stateCell(), s.driftLine())
	}
	// With the parent's HEAD on a commit the creation base is not in, the boundary cannot be read, so
	// ls cannot place the shallow file and must not call ready a fork merge refuses. Merge reads the
	// boundary first, so ls names that.
	branch := forkspace.GitOut(repo, "symbolic-ref", "HEAD")
	lone := forkspace.GitOut(repo, "commit-tree", "HEAD^{tree}", "-m", "unrelated")
	git(t, repo, "update-ref", "--no-deref", "HEAD", lone)
	if s := c.gatherForkStatus(repo, identity.Name, tasks.ReadProjectSnapshot(repo, nil)); s.stateCell() == "ready" || s.Drift != driftBoundary {
		t.Fatalf("ls with a shallow file and an unreadable boundary = %q, %q; want not ready", s.stateCell(), s.driftLine())
	}
	if landed, err := mergeOneForTest(t, c, repo, "", identity.Name, false); landed || err == nil {
		t.Fatalf("merge with an unreadable boundary = (%v, %v); want a refusal", landed, err)
	}
	git(t, repo, "symbolic-ref", "HEAD", branch)
	if landed, err := mergeOneForTest(t, c, repo, "", identity.Name, false); !landed || err != nil {
		t.Fatalf("merge with a shallow file at the boundary = (%v, %v)", landed, err)
	}
}

// coop fork ls agrees with merge about an isolated fork whose merge boundary cannot be read, with no
// shallow file involved: merge refuses it, so ls shows it as not ready and names the merge as the
// command that says why; once the boundary reads again, ls says ready and the merge lands.
func TestLsAgreesWithMergeAboutAnIsolatedForksUnreadableBoundary(t *testing.T) {
	work := func(t *testing.T, ws, id string) {
		if err := os.WriteFile(filepath.Join(ws, "g.txt"), []byte("g\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, ws, "add", "g.txt")
		git(t, ws, "commit", "-qm", "add g\n\nCoop-Task: "+id)
	}
	repo, ws, _, identity, c := prepareForkTaskCandidateHistory(t, "task-isolated-boundary-ls", true, work, nil)
	if _, err := os.Stat(filepath.Join(ws, ".git", "shallow")); !os.IsNotExist(err) {
		t.Fatalf("shallow file = %v; want none", err)
	}
	if s := c.gatherForkStatus(repo, identity.Name, tasks.ReadProjectSnapshot(repo, nil)); s.stateCell() != "ready" || s.Drift != driftNone {
		t.Fatalf("ls with a readable boundary = %q, %q; want ready", s.stateCell(), s.driftLine())
	}
	// With the parent's branch on a commit the creation base is not in, the boundary cannot be read.
	branch := forkspace.GitOut(repo, "symbolic-ref", "HEAD")
	before := forkspace.GitOut(repo, "rev-parse", "HEAD")
	lone := forkspace.GitOut(repo, "commit-tree", "HEAD^{tree}", "-m", "unrelated")
	git(t, repo, "update-ref", branch, lone)
	s := c.gatherForkStatus(repo, identity.Name, tasks.ReadProjectSnapshot(repo, nil))
	if s.stateCell() == "ready" || s.Drift != driftBoundary || !strings.Contains(s.driftLine(), "coop fork merge "+identity.Name) {
		t.Fatalf("ls with an unreadable boundary = %q, %q; want not ready, naming the merge", s.stateCell(), s.driftLine())
	}
	landed, err := mergeOneForTest(t, c, repo, "", identity.Name, false)
	if landed || err == nil || !strings.Contains(err.Error(), "no longer in the captured parent") {
		t.Fatalf("merge with an unreadable boundary = (%v, %v); want the boundary refusal", landed, err)
	}
	git(t, repo, "update-ref", branch, before)
	if s := c.gatherForkStatus(repo, identity.Name, tasks.ReadProjectSnapshot(repo, nil)); s.stateCell() != "ready" || s.Drift != driftNone {
		t.Fatalf("ls once the boundary reads again = %q, %q; want ready", s.stateCell(), s.driftLine())
	}
	if landed, err := mergeOneForTest(t, c, repo, "", identity.Name, false); !landed || err != nil {
		t.Fatalf("merge once the boundary reads again = (%v, %v)", landed, err)
	}
}

// coop fork ls shows an isolated fork whose merge would land a changed change as not mergeable only
// while the branch is where that merge met it; the record admits nothing new through the ancestry
// fence, and once the branch changes so the commits apply as reviewed, ls says ready and the merge
// lands them and drops the record.
func TestIsolatedChangedLandShowsInLsUntilTheBranchChanges(t *testing.T) {
	history := func(t *testing.T, ws, id string) {
		if err := os.WriteFile(filepath.Join(ws, "README.md"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, ws, "commit", "-qam", "remove the line")
		if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("hi\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, ws, "commit", "-qam", "add the line back")
		if err := os.WriteFile(filepath.Join(ws, "g.txt"), []byte("g\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, ws, "add", "g.txt")
		git(t, ws, "commit", "-qm", "add g\n\nCoop-Task: "+id)
	}
	repo, ws, _, identity, c := prepareForkTaskCandidateHistory(t, "task-isolated-changed-ls", true, history, nil)
	before := forkspace.GitOut(repo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "commit", "-qam", "main removes the line as well")
	if landed, err := mergeOneForTest(t, c, repo, "", identity.Name, false); landed || err == nil || !strings.Contains(err.Error(), "would land changed on main") {
		t.Fatalf("isolated changed land = (%v, %v)", landed, err)
	}
	if s := c.gatherForkStatus(repo, identity.Name, tasks.ReadProjectSnapshot(repo, nil)); s.stateCell() != "changed" {
		t.Fatalf("ls after the refusal = %q, %q; want changed", s.stateCell(), s.driftLine())
	}
	// The fence admits nothing by the record (tasks' TestForkCandidateRebaseRequestAdmitsOnlyARebaseOntoItsParent):
	// a branch rewritten off the reviewed commit is refused up front, as without it.
	reviewed := forkspace.GitOut(ws, "rev-parse", "HEAD")
	git(t, ws, "reset", "-q", "--hard", "HEAD~1")
	isolatedTestCommit(t, ws, "other.txt", "other\n")
	if err := RefuseDivergedFork(repo, identity); err == nil || !strings.Contains(err.Error(), "changed after its final review") {
		t.Fatalf("loop start on a rewritten isolated fork = %v; want the up-front refusal", err)
	}
	git(t, ws, "reset", "-q", "--hard", reviewed)
	// The person changes the branch so the reviewed commits apply as reviewed.
	git(t, repo, "reset", "-q", "--hard", before)
	isolatedTestCommit(t, repo, "later.txt", "later\n")
	if s := c.gatherForkStatus(repo, identity.Name, tasks.ReadProjectSnapshot(repo, nil)); s.stateCell() != "ready" || s.Drift != driftNone {
		t.Fatalf("ls once the branch changed = %q, %q; want ready", s.stateCell(), s.driftLine())
	}
	// The loop agrees with ls: still no agent, but it says the branch changed and names the merge.
	if err := RefuseDivergedFork(repo, identity); err == nil || !strings.Contains(err.Error(), "main has changed since") ||
		!strings.Contains(err.Error(), "coop fork merge "+identity.Name) || !strings.Contains(err.Error(), "No agent ran") ||
		strings.Contains(err.Error(), "Change main until") {
		t.Fatalf("loop start once the branch changed = %v; want the changed-since answer naming the merge", err)
	}
	if landed, err := mergeOneForTest(t, c, repo, "", identity.Name, false); !landed || err != nil {
		t.Fatalf("merge once the branch no longer changes the fork = (%v, %v)", landed, err)
	}
	if _, ok, err := tasks.ReadForkCandidateRebase(repo, identity); err != nil || ok {
		t.Fatalf("record after the land = %v, %v; want none", ok, err)
	}
}

// When the branch's tree is still the one the fork's commits were reviewed on, the merge must land
// exactly the reviewed tree, as an ordinary fork's land requires when its branch has not moved.
// Line edits alone cannot tell "x" added at the top of a file from "x" added at its bottom.
func TestIsolatedLandOnAnUnmovedBranchNeedsTheReviewedTree(t *testing.T) {
	repo := initRepo(t)
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, "list.txt"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a\nb\n")
	git(t, repo, "add", "list.txt")
	git(t, repo, "commit", "-qm", "list")
	base := forkspace.GitOut(repo, "rev-parse", "HEAD")
	write("x\na\nb\n")
	git(t, repo, "commit", "-qam", "reviewed: x on top")
	source := forkspace.GitOut(repo, "rev-parse", "HEAD")
	git(t, repo, "reset", "-q", "--hard", base)
	write("a\nb\nx\n")
	git(t, repo, "commit", "-qam", "landed: x at the bottom")
	landed := forkspace.GitOut(repo, "rev-parse", "HEAD")
	tree := func(commit string) string { return forkspace.GitOut(repo, "rev-parse", commit+"^{tree}") }
	candidate := isolatedCandidate{dir: repo, upstream: base, sourceHead: source, sourceTree: tree(source),
		parent: isolatedParent{Head: base, Tree: tree(base)}, head: landed, tree: tree(landed)}
	if a, err := lineEdits(repo, base, source); err != nil {
		t.Fatal(err)
	} else if b, _ := lineEdits(repo, base, landed); a != b {
		t.Fatalf("the fixture's line edits differ, so it does not test the tree check:\n%s\n%s", a, b)
	}
	if err := isolatedLandedAsReviewed("iso", "main", candidate); err == nil || !strings.Contains(err.Error(), "instead of the reviewed") {
		t.Fatalf("a land onto an unmoved branch that changes the reviewed tree = %v", err)
	}
	candidate.head, candidate.tree = source, tree(source)
	if err := isolatedLandedAsReviewed("iso", "main", candidate); err != nil {
		t.Fatalf("the exact reviewed tree on an unmoved branch = %v", err)
	}
}
