package forkctl

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/tasks"
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
	if landed || err == nil || !strings.Contains(err.Error(), "repository is shallow") || !strings.Contains(err.Error(), "rm ") {
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
		for _, want := range []string{"reviewed commit is already on main", "Coop-Task: ", "is on 0 commit(s)", "coop fork rm " + name + " --force", "Nothing was merged."} {
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
	if err := RefuseDivergedFork(repo, identity); err == nil || !strings.Contains(err.Error(), "reviewed commit is already on main") {
		t.Fatalf("loop start after a skipped commit = %v; want the refusal before the loop starts", err)
	}
	loopErr := SkippedCommitError(ws, name, candidate, rebase, nil, " No agent ran.", false)
	if !strings.Contains(loopErr.Error(), "Its commit ") || !strings.Contains(loopErr.Error(), "No agent ran.") || strings.Contains(loopErr.Error(), "reset --hard") {
		t.Fatalf("loop refusal = %v", loopErr)
	}
	s := c.gatherForkStatus(repo, name, tasks.ReadProjectSnapshot(repo, nil))
	if s.stateCell() != "changed" || !strings.Contains(s.driftLine(), "main already has one of its task commits") {
		t.Fatalf("fork after a skipped commit: state %q, %q", s.stateCell(), s.driftLine())
	}
	// A hand rebase onto that parent changes nothing: merge says the same, with the way back.
	git(t, ws, "rebase", "-q", parent)
	landed, err := mergeOneForTest(t, c, repo, "", name, false)
	if landed || err == nil || !strings.Contains(err.Error(), "reviewed commit is already on main") || !strings.Contains(err.Error(), "reset --hard "+candidate.Head) {
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
	if landed || err == nil || !strings.Contains(err.Error(), "reviewed commit is already on main") ||
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
	if landed || err == nil || !strings.Contains(err.Error(), "reviewed commit is already on main") || !strings.Contains(err.Error(), "is on 0 commit(s)") {
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
	if s.stateCell() != "changed" || s.driftLine() != "Changed: its repository is shallow; coop fork merge "+identity.Name+" names the file to remove" {
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
