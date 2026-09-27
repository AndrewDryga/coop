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

// These three tests prove the CLI's ref-touching mutators (`coop tasks done`, fork landing's
// fastForwardParent, and signUnpushed) take the same lock as a running loop's
// validate→consume window. tasks.LockRefAuthority is the shared mechanism under test.

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

	if err := a.forkctl().FastForwardParent(repo, repo, "candidate"); err == nil || !strings.Contains(err.Error(), "ref authority") {
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

// findTaskForTest resolves id in root's lifecycle tree — the cli-side equivalent of
// internal/tasks's own (unexported, test-only) currentTask, used here only to assert a refused
// mutator left a task's state untouched.
func findTaskForTest(t *testing.T, root, id string) (tasks.Item, bool) {
	t.Helper()
	items, err := tasks.ReadTaskTree(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.ID == id {
			return item, true
		}
	}
	return tasks.Item{}, false
}
