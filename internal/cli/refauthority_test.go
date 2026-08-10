package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

// These four tests stay in cli (the other eight moved to internal/tasks/refauthority_test.go, per
// Risk 5 of this task's spec.md): each proves one of cli's OWN staying ref-touching mutators
// (`coop tasks done`, fork-merge's fastForwardParent, sign's signUnpushed, fork-merge's
// reconcileQueueAfterMerge — half of these four now call straight into internal/tasks) takes the
// SAME lock a running loop's validate→consume window holds, so none of them can ever land mid-window
// and none can make that window refuse. tasks.LockRefAuthority is the shared mechanism under test.

// TestCmdTasksDoneTakesRefAuthority proves the wrap in cmdTasks (tasks.CmdTasks) is real, not
// decorative: completeTrustedTask's audit-reopen branch shares the loop's validate-then-consume
// shape, so `coop tasks done` must take the same lock a running loop's window holds — never able to
// complete mid-window and never able to make that window refuse.
func TestCmdTasksDoneTakesRefAuthority(t *testing.T) {
	repo := t.TempDir()
	a := appFor(repo)
	root := filepath.Join(repo, tasksRoot)
	id := "2026-08-09-contended-done"
	writeTaskFile(t, filepath.Join(root, tasks.StateInProgress, id, "task.md"), "# "+id+"\n")

	resolved, err := filepath.Abs(repo)
	if err != nil {
		t.Fatal(err)
	}
	release, err := tasks.LockRefAuthority(a.cfg, resolved)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	code, err := a.cmdTasks([]string{"done", id})
	if err == nil || !strings.Contains(err.Error(), "ref authority") {
		t.Fatalf("cmdTasks(done) while ref authority is held = (%d, %v), want a ref-authority refusal", code, err)
	}
	current, ok := findTaskForTest(t, root, id)
	if !ok || current.State != tasks.StateInProgress {
		t.Fatalf("task state after the refused done = %+v, %v; want it untouched", current, ok)
	}
}

// TestFastForwardParentTakesRefAuthority proves fork-merge's landing step (the parent-checkout
// mutation, not the fork's own isolated rebase) shares the same lock, so a fork can never land in
// the middle of a loop's validate→consume window on the parent it's landing onto.
func TestFastForwardParentTakesRefAuthority(t *testing.T) {
	repo, run := gitrepo.New(t)
	run("commit", "-q", "--allow-empty", "-m", "base")
	run("branch", "candidate") // gitFetchInto(repo, repo, "candidate") must succeed before the lock is even reached
	a := appFor(repo)

	resolved, err := filepath.Abs(repo)
	if err != nil {
		t.Fatal(err)
	}
	release, err := tasks.LockRefAuthority(a.cfg, resolved)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if err := a.fastForwardParent(repo, repo, "candidate"); err == nil || !strings.Contains(err.Error(), "ref authority") {
		t.Fatalf("fastForwardParent while ref authority is held = %v, want a ref-authority refusal", err)
	}
}

// TestSignUnpushedTakesRefAuthority proves signUnpushed's ref-mutating tail (the update-ref compare-
// and-swap) shares the lock: a signing rewrite can never land inside another controller's
// validate→consume window, and vice versa, so coop's own signing sweep can never trip its own
// refusal.
func TestSignUnpushedTakesRefAuthority(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not available")
	}
	keyDir := t.TempDir()
	key := filepath.Join(keyDir, "sk")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-f", key, "-N", "", "-C", "coop-test").CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	globalCfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(globalCfg, []byte("[commit]\n\tgpgsign = true\n[gpg]\n\tformat = ssh\n[user]\n\tsigningkey = "+key+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalCfg)
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))

	repo := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "T")
	run("config", "commit.gpgsign", "false")
	run("commit", "-q", "--allow-empty", "-m", "base")
	base := gitOut(repo, "rev-parse", "HEAD")
	run("commit", "-q", "--allow-empty", "-m", "unpushed")

	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	resolved, err := filepath.Abs(repo)
	if err != nil {
		t.Fatal(err)
	}
	release, err := tasks.LockRefAuthority(a.cfg, resolved)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if _, err := a.signUnpushed(repo, base); err == nil || !strings.Contains(err.Error(), "ref authority") {
		t.Fatalf("signUnpushed while ref authority is held = %v, want a ref-authority refusal", err)
	}
}

// TestReconcileQueueAfterMergeTakesRefAuthority proves the fork-merge reconciliation path shares the
// lock: completeTrustedTask's audit-reopen branch has the same validate-then-consume shape as the
// work loop's own completion, so closing a landed task here must be exclusive with a running loop's
// window on the same parent checkout too.
func TestReconcileQueueAfterMergeTakesRefAuthority(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, run := gitrepo.New(t)
	writeTaskFile(t, filepath.Join(repo, tasksRoot, tasks.StateTodo, "landed", "task.md"), "# landed\n")
	if err := os.WriteFile(filepath.Join(repo, "code.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "seed queue")
	beforeLand := gitOut(repo, "rev-parse", "HEAD")
	run("commit", "-q", "--allow-empty", "-m", "landed work\n\n"+tasks.CoopTaskTrailer+": landed")

	cfg := &config.Config{ConfigDir: t.TempDir(), TasksFiles: []string{tasksRoot}}
	resolved, err := filepath.Abs(repo)
	if err != nil {
		t.Fatal(err)
	}
	release, err := tasks.LockRefAuthority(cfg, resolved)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if err := tasks.ReconcileQueueAfterMerge(cfg, repo, "fork1", beforeLand+"..HEAD"); err == nil || !strings.Contains(err.Error(), "ref authority") {
		t.Fatalf("reconcileQueueAfterMerge while ref authority is held = %v, want a ref-authority refusal", err)
	}
	current, ok := findTaskForTest(t, filepath.Join(repo, tasksRoot), "landed")
	if !ok || current.State != tasks.StateTodo {
		t.Fatalf("landed task state after the refused reconcile = %+v, %v; want it untouched", current, ok)
	}
}

// findTaskForTest resolves id in root's lifecycle tree — the cli-side equivalent of
// internal/tasks's own (unexported, test-only) currentTask, used here only to assert a refused
// mutator left a task's state untouched.
func findTaskForTest(t *testing.T, root, id string) (tasks.Item, bool) {
	t.Helper()
	for _, item := range tasks.ReadTaskTree(root) {
		if item.ID == id {
			return item, true
		}
	}
	return tasks.Item{}, false
}
