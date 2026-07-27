package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func sessionWorkspaceGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := sessionWorkspaceGitResult(dir, args...)
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(out)
}

func sessionWorkspaceGitResult(dir string, args ...string) (string, error) {
	env := append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+filepath.Join(os.TempDir(), "coop-session-workspace-no-global"),
		"GIT_CONFIG_SYSTEM="+filepath.Join(os.TempDir(), "coop-session-workspace-no-system"))
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func sessionWorkspaceWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sessionWorkspaceHasPath(changes []sessionWorkspaceChange, path, status string) bool {
	for _, change := range changes {
		if change.Path == path && (status == "" || change.Status == status) {
			return true
		}
	}
	return false
}

func TestSessionWorkspaceCreateCapturesExactParentHead(t *testing.T) {
	repo, git := gitRepo(t)
	sessionWorkspaceWrite(t, filepath.Join(repo, "base.txt"), "base\n")
	git("add", "base.txt")
	git("commit", "-qm", "base")
	base := gitOut(repo, "rev-parse", "HEAD")
	parentRefsBefore := sessionWorkspaceGit(t, repo, "for-each-ref", "--format=%(refname)=%(objectname)")

	created, err := createSessionWorkspace(repo, "remote-1")
	if err != nil {
		t.Fatal(err)
	}
	if created.Repo != repo || created.Name != "remote-1" || created.Path != forkWorkspace(repo, "remote-1") {
		t.Fatalf("created workspace identity = %+v", created)
	}
	if created.BaseCommit != base || created.ForkHead != base {
		t.Fatalf("created workspace commits = %+v, want base %s", created, base)
	}
	if created.Branch != "remote-1" || sessionWorkspaceGit(t, created.Path, "branch", "--show-current") != "remote-1" {
		t.Fatalf("created workspace branch = %q", created.Branch)
	}
	if got := sessionWorkspaceGit(t, repo, "rev-parse", "HEAD"); got != base {
		t.Fatalf("parent HEAD changed to %s, want %s", got, base)
	}
	if got := sessionWorkspaceGit(t, repo, "for-each-ref", "--format=%(refname)=%(objectname)"); got != parentRefsBefore {
		t.Fatalf("parent refs changed during workspace creation:\nbefore %s\nafter %s", parentRefsBefore, got)
	}
}

func TestSessionWorkspaceCreateRefusesExistingAndInvalidPartialWorkspace(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	partial := forkWorkspace(repo, "partial")
	if err := os.MkdirAll(partial, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(partial, "must-remain")
	sessionWorkspaceWrite(t, marker, "foreign\n")
	if _, err := createSessionWorkspace(repo, "partial"); err == nil {
		t.Fatal("existing partial workspace was accepted")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("existing partial workspace was changed: %v", err)
	}
	if _, err := createSessionWorkspace(repo, "bad..name"); err == nil {
		t.Fatal("invalid workspace name was accepted")
	}
	if pathExists(forkWorkspace(repo, "bad..name")) {
		t.Fatal("invalid workspace name left a partial workspace")
	}
}

func TestSessionWorkspaceInspectTypedChangesAndOddFilenames(t *testing.T) {
	repo, git := gitRepo(t)
	for name, body := range map[string]string{
		"committed.txt": "before committed\n",
		"staged.txt":    "before staged\n",
		"unstaged.txt":  "before unstaged\n",
	} {
		sessionWorkspaceWrite(t, filepath.Join(repo, name), body)
	}
	git("add", ".")
	git("commit", "-qm", "base")
	base := gitOut(repo, "rev-parse", "HEAD")
	created, err := createSessionWorkspace(repo, "changes")
	if err != nil {
		t.Fatal(err)
	}

	sessionWorkspaceWrite(t, filepath.Join(created.Path, "committed.txt"), "committed change\n")
	sessionWorkspaceGit(t, created.Path, "add", "committed.txt")
	sessionWorkspaceGit(t, created.Path, "commit", "-qm", "committed change")
	forkHead := gitOut(created.Path, "rev-parse", "HEAD")
	sessionWorkspaceWrite(t, filepath.Join(created.Path, "staged.txt"), "staged change\n")
	sessionWorkspaceGit(t, created.Path, "add", "staged.txt")
	sessionWorkspaceWrite(t, filepath.Join(created.Path, "unstaged.txt"), "unstaged change\n")
	oddName := "untracked file with spaces.txt"
	sessionWorkspaceWrite(t, filepath.Join(created.Path, oddName), "untracked contents must not enter patch\n")

	changes, err := inspectSessionChanges(repo, created.Path, base, sessionWorkspacePatchLimit)
	if err != nil {
		t.Fatal(err)
	}
	if changes.BaseCommit != base || changes.ForkHead != forkHead || changes.ParentHead != base {
		t.Fatalf("inspection identities = %+v", changes)
	}
	if !sessionWorkspaceHasPath(changes.Committed, "committed.txt", "M") {
		t.Fatalf("committed changes = %+v", changes.Committed)
	}
	if !sessionWorkspaceHasPath(changes.Staged, "staged.txt", "M") {
		t.Fatalf("staged changes = %+v", changes.Staged)
	}
	if !sessionWorkspaceHasPath(changes.Unstaged, "unstaged.txt", "M") {
		t.Fatalf("unstaged changes = %+v", changes.Unstaged)
	}
	if !sessionWorkspaceHasPath(changes.Untracked, oddName, "??") {
		t.Fatalf("untracked changes = %+v", changes.Untracked)
	}
	if len(changes.Conflicts) != 0 {
		t.Fatalf("unexpected conflicts = %+v", changes.Conflicts)
	}
	if changes.ParentDivergence.Ahead != 1 || changes.ParentDivergence.Behind != 0 || changes.ParentDivergence.Diverged {
		t.Fatalf("parent divergence = %+v", changes.ParentDivergence)
	}
	if changes.Truncated || !strings.Contains(changes.Patch, "committed change") ||
		!strings.Contains(changes.Patch, "staged change") || !strings.Contains(changes.Patch, "unstaged change") {
		t.Fatalf("tracked patch = truncated=%v patch=%q", changes.Truncated, changes.Patch)
	}
	if strings.Contains(changes.Patch, oddName) || strings.Contains(changes.Patch, "untracked contents") {
		t.Fatal("untracked contents were embedded in tracked patch")
	}

	sessionWorkspaceWrite(t, filepath.Join(repo, "parent-only.txt"), "parent advanced\n")
	git("add", "parent-only.txt")
	git("commit", "-qm", "parent advance")
	changes, err = inspectSessionChanges(repo, created.Path, base, sessionWorkspacePatchLimit)
	if err != nil {
		t.Fatalf("inspect after parent advance: %v", err)
	}
	if changes.ParentDivergence.Ahead != 1 || changes.ParentDivergence.Behind != 1 || !changes.ParentDivergence.Diverged {
		t.Fatalf("divergence after parent advance = %+v", changes.ParentDivergence)
	}
}

func TestSessionWorkspaceInspectTruncatesPatchAndSurfacesGitFailure(t *testing.T) {
	repo, git := gitRepo(t)
	sessionWorkspaceWrite(t, filepath.Join(repo, "large.txt"), "base\n")
	git("add", "large.txt")
	git("commit", "-qm", "base")
	base := gitOut(repo, "rev-parse", "HEAD")
	created, err := createSessionWorkspace(repo, "patch")
	if err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceWrite(t, filepath.Join(created.Path, "large.txt"), strings.Repeat("large tracked change\n", 20))
	changes, err := inspectSessionChanges(repo, created.Path, base, 16)
	if err != nil {
		t.Fatal(err)
	}
	if !changes.Truncated || len(changes.Patch) > 16 {
		t.Fatalf("truncated patch = %d bytes, truncated=%v", len(changes.Patch), changes.Truncated)
	}
	if _, err := inspectSessionChanges(repo, filepath.Join(t.TempDir(), "not-a-git-workspace"), base, 16); err == nil {
		t.Fatal("Git failure was reported as a clean result")
	}
}

func TestSessionWorkspaceDiscardClean(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	created, err := createSessionWorkspace(repo, "discard-clean")
	if err != nil {
		t.Fatal(err)
	}
	parentHead := gitOut(repo, "rev-parse", "HEAD")
	plan, err := planSessionWorkspaceDiscard(repo, created.Path, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Dirty || plan.Unmerged || plan.Running || plan.AcceptedDirty || plan.AcceptedUnmerged {
		t.Fatalf("clean discard plan = %+v", plan)
	}
	if err := discardSessionWorkspace(plan); err != nil {
		t.Fatal(err)
	}
	if pathExists(created.Path) {
		t.Fatal("clean discard left the workspace")
	}
	if got := sessionWorkspaceGit(t, repo, "rev-parse", "HEAD"); got != parentHead {
		t.Fatalf("clean discard changed parent HEAD to %s, want %s", got, parentHead)
	}
}

func TestSessionWorkspaceDiscardRefusesStaleHeadStatusReplacementAndRunning(t *testing.T) {
	repo, git := gitRepo(t)
	sessionWorkspaceWrite(t, filepath.Join(repo, "file.txt"), "base\n")
	git("add", "file.txt")
	git("commit", "-qm", "base")
	created, err := createSessionWorkspace(repo, "discard-stale")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planSessionWorkspaceDiscard(repo, created.Path, false, false)
	if err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceGit(t, created.Path, "commit", "--allow-empty", "-qm", "head moved")
	if err := discardSessionWorkspace(plan); err == nil {
		t.Fatal("stale HEAD discard unexpectedly succeeded")
	}
	if !pathExists(created.Path) {
		t.Fatal("stale HEAD discard removed the workspace")
	}

	plan, err = planSessionWorkspaceDiscard(repo, created.Path, false, false)
	if err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceWrite(t, filepath.Join(created.Path, "new status.txt"), "status changed\n")
	if err := discardSessionWorkspace(plan); err == nil {
		t.Fatal("stale status discard unexpectedly succeeded")
	}
	if !pathExists(created.Path) {
		t.Fatal("stale status discard removed the workspace")
	}
	os.Remove(filepath.Join(created.Path, "new status.txt"))

	plan, err = planSessionWorkspaceDiscard(repo, created.Path, false, false)
	if err != nil {
		t.Fatal(err)
	}
	replaced := created.Path + ".old"
	if err := os.Rename(created.Path, replaced); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(created.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := discardSessionWorkspace(plan); err == nil {
		t.Fatal("replaced workspace discard unexpectedly succeeded")
	}
	if !pathExists(created.Path) || !pathExists(replaced) {
		t.Fatal("replaced workspace discard removed a path")
	}
	if err := os.Remove(created.Path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replaced, created.Path); err != nil {
		t.Fatal(err)
	}

	plan, err = planSessionWorkspaceDiscard(repo, created.Path, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(forkPid(repo, created.Name), []byte(forkReapPending), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := discardSessionWorkspace(plan); err == nil {
		t.Fatal("running workspace discard unexpectedly succeeded")
	}
	if !pathExists(created.Path) {
		t.Fatal("running workspace discard removed the workspace")
	}
	if err := os.Remove(forkPid(repo, created.Name)); err != nil {
		t.Fatal(err)
	}
}

func TestSessionWorkspaceDiscardRequiresExactDirtyAndUnmergedAcknowledgement(t *testing.T) {
	repo, git := gitRepo(t)
	sessionWorkspaceWrite(t, filepath.Join(repo, "conflict.txt"), "base\n")
	git("add", "conflict.txt")
	git("commit", "-qm", "base")
	parentBranch := gitOut(repo, "branch", "--show-current")
	created, err := createSessionWorkspace(repo, "discard-dirty")
	if err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceWrite(t, filepath.Join(created.Path, "conflict.txt"), "fork\n")
	dirtyPlan, err := planSessionWorkspaceDiscard(repo, created.Path, false, false)
	if err != nil || !dirtyPlan.Dirty || dirtyPlan.Unmerged {
		t.Fatalf("dirty discard plan = %+v, err=%v", dirtyPlan, err)
	}
	if err := discardSessionWorkspace(dirtyPlan); err == nil {
		t.Fatal("dirty discard without acknowledgement unexpectedly succeeded")
	}
	ackPlan, err := planSessionWorkspaceDiscard(repo, created.Path, true, false)
	if err != nil || !ackPlan.AcceptedDirty {
		t.Fatalf("dirty acknowledgement plan = %+v, err=%v", ackPlan, err)
	}
	if err := discardSessionWorkspace(ackPlan); err != nil {
		t.Fatal(err)
	}

	created, err = createSessionWorkspace(repo, "discard-unmerged")
	if err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceWrite(t, filepath.Join(created.Path, "conflict.txt"), "fork\n")
	sessionWorkspaceGit(t, created.Path, "add", "conflict.txt")
	sessionWorkspaceGit(t, created.Path, "commit", "-qm", "fork change")
	sessionWorkspaceWrite(t, filepath.Join(repo, "conflict.txt"), "parent\n")
	git("add", "conflict.txt")
	git("commit", "-qm", "parent change")
	sessionWorkspaceGit(t, created.Path, "fetch", "origin")
	if _, err := sessionWorkspaceGitResult(created.Path, "merge", "--no-edit", "origin/"+parentBranch); err == nil {
		t.Fatal("expected merge conflict did not occur")
	}
	plan, err := planSessionWorkspaceDiscard(repo, created.Path, true, false)
	if err != nil || !plan.Dirty || !plan.Unmerged || plan.AcceptedUnmerged {
		t.Fatalf("unmerged discard plan = %+v, err=%v", plan, err)
	}
	if err := discardSessionWorkspace(plan); err == nil {
		t.Fatal("unmerged discard without acknowledgement unexpectedly succeeded")
	}
	plan, err = planSessionWorkspaceDiscard(repo, created.Path, true, true)
	if err != nil || !plan.AcceptedDirty || !plan.AcceptedUnmerged {
		t.Fatalf("unmerged acknowledgement plan = %+v, err=%v", plan, err)
	}
	if err := discardSessionWorkspace(plan); err != nil {
		t.Fatal(err)
	}
	if pathExists(created.Path) {
		t.Fatal("acknowledged unmerged discard left the workspace")
	}

	created, err = createSessionWorkspace(repo, "discard-clean-commit")
	if err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceWrite(t, filepath.Join(created.Path, "committed-only.txt"), "valuable commit\n")
	sessionWorkspaceGit(t, created.Path, "add", "committed-only.txt")
	sessionWorkspaceGit(t, created.Path, "commit", "-qm", "valuable clean commit")
	plan, err = planSessionWorkspaceDiscard(repo, created.Path, false, false)
	if err != nil || plan.Dirty || !plan.Unmerged {
		t.Fatalf("clean committed discard plan = %+v, err=%v", plan, err)
	}
	if err := discardSessionWorkspace(plan); err == nil {
		t.Fatal("clean unlanded commit was discarded without acknowledgement")
	}
	plan, err = planSessionWorkspaceDiscard(repo, created.Path, false, true)
	if err != nil || !plan.AcceptedUnmerged {
		t.Fatalf("clean committed acknowledgement plan = %+v, err=%v", plan, err)
	}
	if err := discardSessionWorkspace(plan); err != nil {
		t.Fatal(err)
	}
}

func TestSessionWorkspaceDiscardPlanRejectsMissingWorkspace(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	_, err := planSessionWorkspaceDiscard(repo, filepath.Join(forkHome(repo), "missing"), false, false)
	if err == nil || !strings.Contains(err.Error(), "pin session workspace") {
		t.Fatalf("missing workspace plan = %v", err)
	}
}
