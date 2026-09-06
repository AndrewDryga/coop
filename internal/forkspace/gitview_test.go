package forkspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

func viewTestRepo(t *testing.T) (string, func(...string)) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "empty-global"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "empty-system"))
	t.Cleanup(CloseGitViews)
	repo, run := gitrepo.New(t)
	if real, err := filepath.EvalSymlinks(repo); err == nil {
		repo = real // the view keys on git's absolute paths; macOS temp dirs are symlinked
	}
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "tracked.txt")
	run("commit", "-qm", "base")
	return repo, run
}

func viewRun(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd, err := GitCommand(context.Background(), dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// The inverted audit probe: a clean filter and a textconv driver, defined in the local config,
// in an included file, and in config.worktree, each assigned by an in-tree .gitattributes. Under
// the view, status/diff/checkout/rebase succeed and the driver never runs; the same hardened
// command on the real git dir (the positive control) still executes it.
func TestGitViewNeverExecutesRepositoryDrivers(t *testing.T) {
	for _, kind := range []string{"clean", "textconv"} {
		for _, where := range []string{"local", "included", "worktree-config"} {
			t.Run(kind+"-"+where, func(t *testing.T) {
				repo, run := viewTestRepo(t)
				// Branch setup first, with raw git, BEFORE any driver exists: raw checkout/commit would
				// run the driver themselves and the probe could not tell whose invocation it saw.
				run("checkout", "-q", "-b", "topic")
				if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("topic!\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				run("commit", "-qam", "topic change")
				run("checkout", "-q", "main")
				if err := os.WriteFile(filepath.Join(repo, "other.txt"), []byte("main\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				run("add", "other.txt")
				run("commit", "-qm", "main moves")
				run("checkout", "-q", "topic")
				marker := filepath.Join(t.TempDir(), "invoked")
				script := filepath.Join(repo, "driver.sh")
				body := "#!/bin/sh\nprintf invoked > '" + marker + "'\ncat"
				key, attr := "filter.audit.clean", "filter=audit"
				if kind == "textconv" {
					body += " \"$1\""
					key, attr = "diff.audit.textconv", "diff=audit"
				}
				if err := os.WriteFile(script, []byte(body+"\n"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("tracked.txt "+attr+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				switch where {
				case "local":
					run("config", key, script)
				case "included":
					include := filepath.Join(repo, "driver.config")
					if out, err := exec.Command("git", "config", "--file", include, key, script).CombinedOutput(); err != nil {
						t.Fatalf("fixture config: %v: %s", err, out)
					}
					run("config", "include.path", include)
				case "worktree-config":
					run("config", "extensions.worktreeConfig", "true")
					run("config", "--worktree", key, script)
				}
				// Same size as the committed content ("topic!"), so status must compare filtered content.
				if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("after!\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				invoked := func() bool { data, err := os.ReadFile(marker); return err == nil && string(data) == "invoked" }

				for _, command := range [][]string{
					{"status", "--porcelain"},
					{"diff", "--no-ext-diff"},
					{"checkout", "--", "tracked.txt"},
				} {
					if out, err := viewRun(t, repo, command...); err != nil {
						t.Fatalf("git %s under the view: %v\n%s", strings.Join(command, " "), err, out)
					}
					if invoked() {
						t.Fatalf("git %s under the view executed the repository's %s driver", strings.Join(command, " "), kind)
					}
				}
				// A rebase checks files out and diffs them; it must be inert too.
				if out, err := viewRun(t, repo, "rebase", "main"); err != nil {
					t.Fatalf("rebase under the view: %v\n%s", err, out)
				}
				if invoked() {
					t.Fatalf("rebase under the view executed the repository's %s driver", kind)
				}
				if got := strings.TrimSpace(string(readOut(t, repo, "rev-parse", "main"))); !strings.Contains(string(readOut(t, repo, "rev-list", "topic")), got) {
					t.Fatal("rebase under the view did not move the real branch onto main")
				}

				// Positive control: the same hardened command on the real git dir still runs the driver.
				if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("again!\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				raw := exec.Command("git", append(append([]string{"-C", repo}, GitHardening...), "status", "--porcelain")...)
				if out, err := raw.CombinedOutput(); err != nil {
					t.Fatalf("raw status: %v\n%s", err, out)
				}
				if kind == "clean" && !invoked() {
					t.Fatal("positive control: the raw hardened status did not execute the clean driver — the probe proves nothing")
				}
			})
		}
	}
}

func readOut(t *testing.T, repo string, args ...string) []byte {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return out
}

// The view's mechanics: the real HEAD keeps its symref through a rebase, status refreshes the
// REAL index (a raw status afterwards agrees), a linked worktree resolves objects and refs from
// the common dir and HEAD from its own git dir, and ref-store writes go through the real git dir.
func TestGitViewKeepsTheRealRepositoryAuthoritative(t *testing.T) {
	repo, run := viewTestRepo(t)
	headBefore, _ := os.ReadFile(filepath.Join(repo, ".git", "HEAD"))

	// status under the view refreshes the real index: raw git then sees a clean tree, and the view
	// directory holds no index of its own.
	if out, err := viewRun(t, repo, "status", "--porcelain"); err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("clean status under the view = %q, %v", out, err)
	}
	if raw := readOut(t, repo, "status", "--porcelain"); len(raw) != 0 {
		t.Fatalf("raw status after a view status = %q; want clean", raw)
	}
	view := gitViews[filepath.Join(repo, ".git")]
	if view == nil {
		t.Fatal("no view cached for the repository")
	}
	if _, err := os.Lstat(filepath.Join(view.dir, "index")); !os.IsNotExist(err) {
		t.Fatalf("the view grew its own index: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(view.dir, "hooks")); !os.IsNotExist(err) {
		t.Fatal("the view carries a hooks directory")
	}
	if _, err := os.Lstat(filepath.Join(view.dir, "info", "attributes")); !os.IsNotExist(err) {
		t.Fatal("the view carries info/attributes")
	}

	// A commit under the view lands on the real branch; HEAD stays the symref it was.
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := viewRun(t, repo, "-c", "user.email=a@b", "-c", "user.name=n", "commit", "-qam", "under the view"); err != nil {
		t.Fatalf("commit under the view: %v\n%s", err, out)
	}
	if subject := strings.TrimSpace(string(readOut(t, repo, "log", "-1", "--format=%s"))); subject != "under the view" {
		t.Fatalf("real branch after a view commit = %q", subject)
	}
	if headAfter, _ := os.ReadFile(filepath.Join(repo, ".git", "HEAD")); string(headAfter) != string(headBefore) {
		t.Fatalf("real HEAD changed: %q → %q", headBefore, headAfter)
	}

	// A packed branch deleted through GitRefCommand is really gone (the packed-refs rewrite must
	// hit the real file, never the view's symlink).
	run("branch", "doomed")
	run("pack-refs", "--all")
	cmd := GitRefCommand(context.Background(), repo, "branch", "-q", "-D", "doomed")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("branch -D through GitRefCommand: %v\n%s", err, out)
	}
	if out, _ := exec.Command("git", "-C", repo, "show-ref", "--verify", "--quiet", "refs/heads/doomed").CombinedOutput(); len(out) == 0 && exec.Command("git", "-C", repo, "show-ref", "--verify", "--quiet", "refs/heads/doomed").Run() == nil {
		t.Fatal("packed branch survived deletion through the real git dir")
	}
	if _, err := os.Lstat(filepath.Join(view.dir, "packed-refs")); err == nil {
		if info, _ := os.Lstat(filepath.Join(view.dir, "packed-refs")); info.Mode()&os.ModeSymlink == 0 {
			t.Fatal("the view's packed-refs was replaced by a regular file: a rewrite landed in the view")
		}
	}

	// A linked worktree: objects/refs from the common dir, HEAD from its own git dir.
	linked := filepath.Join(t.TempDir(), "linked")
	run("worktree", "add", "-q", linked, "-b", "linked")
	if real, err := filepath.EvalSymlinks(linked); err == nil {
		linked = real
	}
	if out, err := viewRun(t, linked, "status", "--porcelain"); err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("status in a linked worktree under the view = %q, %v", out, err)
	}
	linkedView := gitViews[filepath.Join(repo, ".git", "worktrees", "linked")]
	if linkedView == nil {
		t.Fatal("no view for the linked worktree")
	}
	if target, err := os.Readlink(filepath.Join(linkedView.dir, "objects")); err != nil || target != filepath.Join(repo, ".git", "objects") {
		t.Fatalf("linked view objects → %q, %v; want the common object store", target, err)
	}
	if head, _ := os.ReadFile(filepath.Join(linkedView.dir, "HEAD")); strings.TrimSpace(string(head)) != "ref: refs/heads/linked" {
		t.Fatalf("linked view HEAD = %q; want the linked worktree's own HEAD", head)
	}

	// A reftable repository is refused rather than half-served.
	run("config", "extensions.refStorage", "reftable")
	if _, err := GitCommand(context.Background(), repo, "status"); err == nil || !strings.Contains(err.Error(), "reftable") {
		t.Fatalf("reftable repository under the view = %v; want a refusal", err)
	}
}

// The re-sign path adds a worktree --no-checkout on the real git dir and populates it under the
// view (GitDetach): the files arrive, HEAD is detached at the commit, and the worktree's own git
// dir is the one viewed.
func TestGitViewPopulatesANoCheckoutWorktree(t *testing.T) {
	repo, run := viewTestRepo(t)
	head := strings.TrimSpace(string(readOut(t, repo, "rev-parse", "HEAD")))
	root := t.TempDir()
	worktree := filepath.Join(root, "worktree")
	if out, err := GitRefCommand(context.Background(), repo, "worktree", "add", "--no-checkout", "--detach", "--quiet", worktree, head).CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v\n%s", err, out)
	}
	if err := GitDetach(context.Background(), worktree, head); err != nil {
		t.Fatalf("populate the worktree under the view: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(worktree, "tracked.txt")); err != nil || string(data) != "before\n" {
		t.Fatalf("worktree file = %q, %v; want the checked-out content", data, err)
	}
	if out, err := viewRun(t, worktree, "status", "--porcelain"); err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("status in the populated worktree = %q, %v; want clean", out, err)
	}
	if got := strings.TrimSpace(string(readOut(t, worktree, "rev-parse", "HEAD"))); got != head {
		t.Fatalf("worktree HEAD = %s, want %s", got, head)
	}
	run("worktree", "remove", "--force", worktree)
}
