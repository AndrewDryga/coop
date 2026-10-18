package forkspace

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
	"github.com/AndrewDryga/coop/internal/testutil/wait"
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
				// Prove each planted driver is live before trusting its absence.
				control := []string{"status", "--porcelain"}
				if kind == "textconv" {
					control = []string{"diff", "--textconv", "--no-ext-diff", "--", "tracked.txt"}
				}
				raw := exec.Command("git", append(append([]string{"-C", repo}, GitHardening...), control...)...)
				if out, err := raw.CombinedOutput(); err != nil {
					t.Fatalf("raw driver control: %v\n%s", err, out)
				}
				if !invoked() {
					t.Fatalf("positive control did not execute %s driver", kind)
				}
				if err := os.Remove(marker); err != nil {
					t.Fatal(err)
				}

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

// hasRef reports whether repo holds ref, for a test that expects either answer.
func hasRef(repo, ref string) bool {
	return exec.Command("git", "-C", repo, "show-ref", "--verify", "--quiet", ref).Run() == nil
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
	// Host porcelain commit -a implicitly runs add's recursive submodule scan.
	// Exercise the same real index/object/ref writes with the supported plumbing.
	if out, err := viewRun(t, repo, "update-index", "tracked.txt"); err != nil {
		t.Fatalf("stage under view: %v: %s", err, out)
	}
	tree, err := viewRun(t, repo, "write-tree")
	if err != nil {
		t.Fatal(err)
	}
	commit, err := viewRun(t, repo, "-c", "user.email=a@b", "-c", "user.name=n", "commit-tree", strings.TrimSpace(tree), "-p", "HEAD", "-m", "under the view")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := viewRun(t, repo, "update-ref", "HEAD", strings.TrimSpace(commit)); err != nil {
		t.Fatalf("update real branch: %v: %s", err, out)
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

// A box can delete the .git of a folder Coop runs git for, and the walk to the nearest .git then
// climbs to the repository that holds the folder: host git would read and write a home folder
// kept in Git. Views, integrity checks and observations refuse that before laying out anything.
// The home folder itself, as a COOP_REPO may name it, keeps its repository, and a repository
// below the home folder is still found from a folder inside it.
func TestGitViewNeverClimbsToTheHomeRepository(t *testing.T) {
	ctx := context.Background()
	home, run := viewTestRepo(t)
	t.Setenv("HOME", home)
	workspace := filepath.Join(home, "project")
	run("init", "-q", "project")
	if out, err := viewRun(t, workspace, "status", "--porcelain"); err != nil {
		t.Fatalf("status while the workspace has its own .git: %v\n%s", err, out)
	}
	if err := os.RemoveAll(filepath.Join(workspace, ".git")); err != nil {
		t.Fatal(err)
	}
	views := t.TempDir()
	t.Setenv(GitViewRootEnv, views)
	CloseGitViews()
	refused := func(what, dir string, err error) {
		t.Helper()
		// Never errNoRepository, on which GitCommand runs plain git, which climbs there by itself.
		want := dir + " has no .git of its own, and the nearest Git repository above it is " + home + ", at your home folder"
		if err == nil || errors.Is(err, errNoRepository) || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "run git init in it") {
			t.Fatalf("%s in %s = %v; want a refusal naming the repository at the home folder", what, dir, err)
		}
	}
	_, err := GitCommand(ctx, workspace, "status")
	refused("GitCommand", workspace, err)
	refused("RunGitFsck", workspace, RunGitFsck(ctx, workspace, os.Environ(), io.Discard, io.Discard))
	_, err = ObserveGit(ctx, workspace, "rev-parse", "HEAD")
	refused("ObserveGit", workspace, err)
	if entries, err := os.ReadDir(views); err != nil || len(entries) != 0 {
		t.Fatalf("a refused command laid out a view: %v, %v", entries, err)
	}
	link := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(home, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", link)
	_, err = GitCommand(ctx, workspace, "status")
	refused("GitCommand with a linked HOME", workspace, err)
	if out, err := viewRun(t, home, "status", "--porcelain"); err != nil {
		t.Fatalf("status in the home folder itself: %v\n%s", err, out)
	}

	// A repository above the home folder, met from a folder beside it, is refused just the same.
	me, beside := filepath.Join(home, "users", "me"), filepath.Join(home, "opt", "tool")
	for _, dir := range []string{me, beside} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", me)
	_, err = GitCommand(ctx, beside, "status")
	refused("GitCommand beside the home folder", beside, err)
	run("init", "-q", "users/me/below")
	inside := filepath.Join(me, "below", "sub")
	if err := os.Mkdir(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	if gitDir, _, err := gitDirsOf(inside); err != nil || gitDir != filepath.Join(me, "below", ".git") {
		t.Fatalf("gitDirsOf in a folder of a repository below the home folder = %q, %v; want that repository", gitDir, err)
	}
}

// GitRefCommand lets git search for the repository itself, and git climbs past a folder's .git
// when it is gone or is one git cannot use: a fork land's update-ref, or a branch switch's
// symbolic-ref, which needs no commit there, would then move a branch or HEAD of a home folder
// kept in Git. Each is refused, a deleted .git in the very words a view uses, while plain git from
// the same folder, the control, does write there. The home folder's own .git, git init in a folder
// without one, and a climb that ends below the home folder still work.
func TestGitRefCommandNeverClimbsToTheHomeRepository(t *testing.T) {
	ctx := context.Background()
	home, run := viewTestRepo(t)
	t.Setenv("HOME", home)
	workspace := filepath.Join(home, "project")
	gitDir := filepath.Join(workspace, ".git")
	head := strings.TrimSpace(string(readOut(t, home, "rev-parse", "HEAD")))
	hasBranch := func(name string) bool { return hasRef(home, "refs/heads/"+name) }
	homeHead := func() string { return strings.TrimSpace(string(readOut(t, home, "symbolic-ref", "HEAD"))) }
	unusable := gitDir + " is not a Git repository git can use"
	for _, damage := range []struct {
		name, want string
		apply      func() error
	}{
		{"deleted", "the nearest Git repository above it is " + home + ", at your home folder", func() error {
			return os.RemoveAll(gitDir)
		}},
		{"emptied", unusable, func() error {
			if err := os.RemoveAll(gitDir); err != nil {
				return err
			}
			return os.Mkdir(gitDir, 0o700)
		}},
		{"objects-deleted", unusable, func() error { return os.RemoveAll(filepath.Join(gitDir, "objects")) }},
		{"head-emptied", unusable, func() error { return os.WriteFile(filepath.Join(gitDir, "HEAD"), nil, 0o600) }},
		// A HEAD and an objects folder in the project pass for a bare repository; git also wants refs.
		{"bare-lookalike", workspace + " is not a Git repository git can use", func() error {
			if err := os.RemoveAll(gitDir); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(workspace, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
				return err
			}
			return os.Mkdir(filepath.Join(workspace, "objects"), 0o700)
		}},
	} {
		if err := os.RemoveAll(workspace); err != nil {
			t.Fatal(err)
		}
		run("init", "-q", "project")
		if err := damage.apply(); err != nil {
			t.Fatal(err)
		}
		branch := "escaped-" + damage.name
		for _, args := range [][]string{
			{"update-ref", "refs/heads/" + branch, head},
			{"symbolic-ref", "HEAD", "refs/heads/" + branch},
		} {
			err := GitRefCommand(ctx, workspace, args...).Run()
			if err == nil || !strings.Contains(err.Error(), damage.want) || !strings.Contains(err.Error(), "run git init in it") {
				t.Fatalf("%s .git: GitRefCommand %v = %v; want a refusal saying %q", damage.name, args, err, damage.want)
			}
			if _, _, viewErr := gitDirsOf(workspace); damage.name == "deleted" && (viewErr == nil || err.Error() != viewErr.Error()) {
				t.Fatalf("deleted .git: GitRefCommand %v = %v; want the view's words, %v", args, err, viewErr)
			}
		}
		if hasBranch(branch) || homeHead() != "refs/heads/main" {
			t.Fatalf("%s .git: a refused GitRefCommand moved a branch or HEAD of the home repository", damage.name)
		}
		// The control: plain git from the same folder climbs to the home repository and writes there.
		run("-C", "project", "update-ref", "refs/heads/"+branch, head)
		if !hasBranch(branch) {
			t.Fatalf("%s .git: plain git never reached the home repository, so the refusal proves nothing", damage.name)
		}
	}

	if out, err := GitRefCommand(ctx, home, "update-ref", "refs/heads/own", head).CombinedOutput(); err != nil || !hasBranch("own") {
		t.Fatalf("update-ref in the home folder's own repository: %v\n%s", err, out)
	}
	if err := os.RemoveAll(workspace); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(workspace, "sub")
	if err := os.MkdirAll(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := GitRefCommand(ctx, workspace, "init", "--quiet", "--template=", "--object-format=sha1", "--initial-branch=main").CombinedOutput(); err != nil || !fileExists(filepath.Join(gitDir, "HEAD")) {
		t.Fatalf("git init in a folder inside the home repository: %v\n%s", err, out)
	}
	if out, err := GitRefCommand(ctx, inside, "symbolic-ref", "HEAD", "refs/heads/topic").CombinedOutput(); err != nil {
		t.Fatalf("symbolic-ref from a folder of a repository below the home folder: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(readOut(t, workspace, "symbolic-ref", "HEAD"))); got != "refs/heads/topic" {
		t.Fatalf("HEAD of the repository below the home folder = %q; want refs/heads/topic", got)
	}
	if got := homeHead(); got != "refs/heads/main" {
		t.Fatalf("HEAD of the home repository = %q; the climb below it moved the home repository", got)
	}
}

// A test binary that names no view root must fail. The per-process temporary root that used to
// stand in was left behind by every test binary that never removed it.
func TestGitViewRootRefusesATestBinaryWithoutItsOwnRoot(t *testing.T) {
	repo, _ := viewTestRepo(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(GitViewRootEnv, "")
	CloseGitViews()
	if cmd, err := GitCommand(context.Background(), repo, "status"); err == nil || !strings.Contains(err.Error(), GitViewRootEnv) {
		t.Fatalf("GitCommand = %v, %v; want a refusal naming %s", cmd, err, GitViewRootEnv)
	}
	if entries, err := os.ReadDir(home); err != nil || len(entries) != 0 {
		t.Fatalf("the refused command wrote under HOME: %v, %v", entries, err)
	}
}

// With no repository above the folder, GitCommand runs plain git, and git must not search for one
// when it starts: a box that makes a .git with a filter driver after coop's check would otherwise
// have host git read its config and run the driver. Here the .git appears between building the
// commands and running them.
func TestGitCommandOutsideARepositoryNeverFindsALaterGit(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "driver-ran")
	read, err := GitCommand(context.Background(), dir, "config", "--get", "coop.planted")
	if err != nil {
		t.Fatal(err)
	}
	hash, err := GitCommand(context.Background(), dir, "hash-object", "--path=file.txt", "file.txt")
	if err != nil {
		t.Fatal(err)
	}
	plant := exec.Command("git", "init", "-q", dir)
	plant.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	if out, err := plant.CombinedOutput(); err != nil {
		t.Fatalf("plant a repository: %v\n%s", err, out)
	}
	config := "[coop]\n\tplanted = yes\n[filter \"evil\"]\n\tclean = touch " + marker + "\n"
	f, err := os.OpenFile(filepath.Join(dir, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(config); err != nil {
		t.Fatal(err)
	}
	f.Close()
	for name, body := range map[string]string{".gitattributes": "* filter=evil\n", "file.txt": "x\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := read.CombinedOutput(); err == nil || strings.Contains(string(out), "yes") {
		t.Fatalf("git config after a planted .git = %v, %q; want git to find no repository", err, out)
	}
	_, _ = hash.CombinedOutput()
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("host git ran the planted filter driver: %v", err)
	}
}

// Git opens a repository's refs, index and objects with blocking opens, so a FIFO a box puts at
// .git/packed-refs holds a host git command forever. A command whose caller set no deadline is
// stopped at the limit, and GitStalled names the repository it stalled in.
func TestGitCommandsStopAtTheStallLimitAndSaySo(t *testing.T) {
	defer func(saved time.Duration) { gitStallLimit = saved }(gitStallLimit)
	gitStallLimit = time.Second
	repo, _ := gitrepo.NewCommitted(t)
	if err := os.Remove(filepath.Join(repo, ".git", "packed-refs")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(repo, ".git", "packed-refs"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := GitRefCommand(context.Background(), repo, "update-ref", "refs/heads/stalled", "HEAD")
	done := make(chan error, 1)
	go func() { done <- cmd.Run() }()
	select {
	case err := <-done:
		err = GitStalled(cmd, err)
		if err == nil || !strings.Contains(err.Error(), "git did not finish in "+repo) || !strings.Contains(err.Error(), "FIFO") {
			t.Fatalf("stalled git = %v, want the plain refusal naming %s", err, repo)
		}
	case <-time.After(wait.Deadline):
		t.Fatal("git waited on the FIFO past the stall limit")
	}
}

// TestGitOutErr: the erroring read tells a FAILED git call apart from one that legitimately printed
// nothing, and carries git's own stderr so a caller can hand a human the cause. GitOut, which every
// display-only site still uses, keeps conflating the two.
func TestGitOutErr(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, run := gitrepo.New(t)
	run("commit", "-q", "--allow-empty", "-m", "base")
	if head, err := GitOutErr(repo, "rev-parse", "HEAD"); err != nil || head == "" {
		t.Fatalf("GitOutErr(rev-parse HEAD) = (%q, %v), want a sha and no error", head, err)
	}
	// Empty output from a command that SUCCEEDED (a clean tree) is not a failure.
	if out, err := GitOutErr(repo, "status", "--porcelain"); err != nil || out != "" {
		t.Fatalf("GitOutErr(status --porcelain) on a clean tree = (%q, %v), want empty and no error", out, err)
	}
	out, err := GitOutErr(repo, "rev-parse", "no-such-ref")
	if err == nil || out != "" {
		t.Fatalf("GitOutErr(rev-parse no-such-ref) = (%q, %v), want empty and an error", out, err)
	}
	if !strings.Contains(err.Error(), "rev-parse no-such-ref") {
		t.Errorf("GitOutErr error %q does not name the command that failed", err)
	}
	if GitOut(repo, "rev-parse", "no-such-ref") != "" {
		t.Error("GitOut must keep swallowing a failure as empty — display sites depend on it")
	}
	// git's stderr is the whole reason the error is actionable; a stub git proves it survives
	// without depending on the locale of the real git's messages.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nprintf 'fatal: bad revision\\n' >&2\nexit 128\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if _, err := GitOutErr(repo, "log", "bogus..HEAD"); err == nil || !strings.Contains(err.Error(), "fatal: bad revision") {
		t.Errorf("GitOutErr = %v, want git's own stderr carried into the error", err)
	}
}
