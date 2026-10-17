package forkspace

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

// returnsWithin runs fn and fails the test unless it returns within wait.Deadline: a read that
// waits on a FIFO nobody writes never returns at all. Tests pass t.Context() to what they run, so
// a git child left waiting is killed when the test ends.
func returnsWithin(t *testing.T, what string, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(wait.Deadline):
		t.Fatalf("%s did not return within %v", what, wait.Deadline)
		return nil
	}
}

// plantMetadata replaces path with what a box can leave where git keeps a file: a FIFO nobody
// writes, a link to an endless device, or a sparse file one byte over limit.
func plantMetadata(t *testing.T, path, kind string, limit int64) {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	var err error
	switch kind {
	case "fifo":
		err = syscall.Mkfifo(path, 0o600)
	case "device":
		err = os.Symlink("/dev/zero", path)
	case "oversize":
		if err = os.WriteFile(path, nil, 0o600); err == nil {
			err = os.Truncate(path, limit+1)
		}
	default:
		t.Fatalf("unknown kind %q", kind)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// fifoHeadRepo returns a repository whose HEAD a box replaced with a FIFO nobody writes. Whatever
// opens it without O_NONBLOCK waits forever, and git's own search for a repository opens it so.
func fifoHeadRepo(t *testing.T) string {
	t.Helper()
	box, _ := viewTestRepo(t)
	plantMetadata(t, filepath.Join(box, ".git", "HEAD"), "fifo", 0)
	return box
}

// Every file a view reads by content is box-writable. Whatever the box leaves there, the host
// command fails fast with a plain error instead of waiting on a FIFO or reading without end. Coop
// usually runs inside the repository, in its project folder, so each case also runs from there.
func TestGitViewRefusesSpecialMetadataPromptly(t *testing.T) {
	for _, site := range []string{"commondir", "HEAD", "config", "worktree .git", "worktree backlink"} {
		for _, kind := range []string{"fifo", "device", "oversize"} {
			for _, from := range []string{"elsewhere", "inside"} {
				t.Run(site+"/"+kind+"/from "+from, func(t *testing.T) {
					repo, run := viewTestRepo(t)
					dir, path, limit := repo, filepath.Join(repo, ".git", site), int64(gitPointerLimit)
					switch site {
					case "config":
						limit = gitConfigLimit
					case "worktree .git", "worktree backlink":
						dir = filepath.Join(t.TempDir(), "linked")
						run("worktree", "add", "-q", dir, "-b", "linked")
						path = filepath.Join(dir, ".git")
						if site == "worktree backlink" {
							path = filepath.Join(repo, ".git", "worktrees", "linked", "gitdir")
						}
					}
					plantMetadata(t, path, kind, limit)
					if from == "inside" {
						t.Chdir(dir)
					}
					err := returnsWithin(t, "a view with "+kind+" "+site, func() error {
						_, err := GitCommand(t.Context(), dir, "status")
						return err
					})
					// A .git that is neither a folder nor a regular file is passed over, as git passes
					// it; all that matters there is that the walk never waits on it.
					if site == "worktree .git" && kind != "oversize" {
						return
					}
					if err == nil {
						t.Fatalf("a view read %s %s", kind, path)
					}
					if kind == "oversize" && !strings.Contains(err.Error(), "more than the") {
						t.Fatalf("oversize %s = %v; want it refused for its size", site, err)
					}
				})
			}
		}
	}
}

// Git searches for a repository from the folder it starts in upward, and opens any HEAD it finds
// without O_NONBLOCK, even to parse one named config file. A box's repository can hold coop's
// working folder, its view root (a project in the home folder holds ~/.local/state) or its temp
// folder; none of them stalls a view, a snapshot or an observation of another repository.
func TestGitViewNeverSearchesTheFoldersAroundIt(t *testing.T) {
	for _, place := range []string{"working folder", "view root", "temp folder"} {
		t.Run(place, func(t *testing.T) {
			repo, _ := viewTestRepo(t)
			box := fifoHeadRepo(t)
			switch place {
			case "working folder":
				t.Chdir(box)
			case "view root":
				t.Setenv(GitViewRootEnv, filepath.Join(box, "views"))
			case "temp folder":
				// A test binary keeps its views in the temp folder too.
				tmp := filepath.Join(box, "tmp")
				if err := os.Mkdir(tmp, 0o700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("TMPDIR", tmp)
			}
			CloseGitViews() // the next view picks its root again
			err := returnsWithin(t, "commands beside a box repository with a FIFO at HEAD", func() error {
				cmd, err := GitCommand(t.Context(), repo, "status", "--porcelain")
				if err != nil {
					return err
				}
				if err := cmd.Run(); err != nil {
					return err
				}
				if err := RunGitFsck(t.Context(), repo, os.Environ(), nil, nil); err != nil {
					return err
				}
				_, err = ObserveGit(t.Context(), repo, "status", "--porcelain")
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A `.git` file a box writes can name any repository's linked worktree. The view, like `coop
// approve`, follows it only when the git dir it names keeps a backlink to this worktree, as git's
// own worktrees do: absolute or relative, and spelled any way that names the same folder. The
// advice to run `git worktree repair` appears only where repair restores a lost link, never where
// it would hand another worktree's git dir to this folder.
func TestGitViewRequiresALinkedWorktreeBacklink(t *testing.T) {
	repo, run := viewTestRepo(t)
	victim := filepath.Join(t.TempDir(), "victim")
	run("worktree", "add", "-q", victim, "-b", "victim")
	victimDir := filepath.Join(repo, ".git", "worktrees", "victim")

	t.Run("pointer into another worktree", func(t *testing.T) {
		box := t.TempDir()
		if err := os.WriteFile(filepath.Join(box, ".git"), []byte("gitdir: "+victimDir+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := GitCommand(t.Context(), box, "status")
		if err == nil || !strings.Contains(err.Error(), "belongs to the worktree at") || strings.Contains(err.Error(), "repair") {
			t.Fatalf("a pointer into another worktree's git dir = %v; want it refused without advice to repair", err)
		}
		if out, err := viewRun(t, victim, "status", "--porcelain"); err != nil || strings.TrimSpace(out) != "" {
			t.Fatalf("the victim worktree's own view = %q, %v", out, err)
		}
	})

	// A box can also replace its own .git with a link to another worktree's .git file. Either kind
	// of link names that very file, so neither views nor `coop approve` may accept it; approve must
	// not add a pattern to the other repository's info/exclude.
	for _, link := range []string{"symbolic", "hard"} {
		t.Run(link+" link to another worktree's .git", func(t *testing.T) {
			box := t.TempDir()
			linkFile := os.Symlink
			if link == "hard" {
				linkFile = os.Link
			}
			if err := linkFile(filepath.Join(victim, ".git"), filepath.Join(box, ".git")); err != nil {
				t.Fatal(err)
			}
			if _, err := GitCommand(t.Context(), box, "status"); err == nil || !strings.Contains(err.Error(), "belongs to the worktree at") {
				t.Errorf("a view through a %s link to another worktree's .git = %v; want it refused", link, err)
			}
			exclude := filepath.Join(repo, ".git", "info", "exclude")
			before, err := os.ReadFile(exclude)
			if err != nil {
				t.Fatal(err)
			}
			if err := ExcludeIfRepository(box, "/.coop-linked-dot-git"); err == nil {
				t.Errorf("coop approve accepted a %s link to another worktree's .git", link)
			}
			if after, err := os.ReadFile(exclude); err != nil || string(after) != string(before) {
				t.Errorf("coop approve wrote another repository's exclude: %q, %v", after, err)
			}
			if out, err := viewRun(t, victim, "status", "--porcelain"); err != nil || strings.TrimSpace(out) != "" {
				t.Fatalf("the victim worktree's own view = %q, %v", out, err)
			}
		})
	}

	t.Run("forged git dir naming another repository", func(t *testing.T) {
		// The box owns this repository's .git, so it can forge a whole worktree folder in it, the
		// backlink included; its commondir must still be the folder it sits in.
		other, _ := viewTestRepo(t)
		box := t.TempDir()
		forged := filepath.Join(repo, ".git", "worktrees", "forged")
		if err := os.MkdirAll(forged, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(forged) })
		for name, data := range map[string]string{
			"commondir": filepath.Join(other, ".git"),
			"gitdir":    filepath.Join(box, ".git"),
			"HEAD":      "ref: refs/heads/main",
		} {
			if err := os.WriteFile(filepath.Join(forged, name), []byte(data+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(box, ".git"), []byte("gitdir: "+forged+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := GitCommand(t.Context(), box, "status"); err == nil || !strings.Contains(err.Error(), "not a worktree folder") {
			t.Fatalf("a forged git dir naming another repository = %v; want it refused", err)
		}
	})

	// What `git worktree repair` does fix, run in the worktree as the advice says.
	for _, broken := range []string{"missing", "empty", "moved"} {
		t.Run(broken+" backlink", func(t *testing.T) {
			name := "linked-" + broken
			linked := filepath.Join(t.TempDir(), name)
			run("worktree", "add", "-q", linked, "-b", name)
			backlink := filepath.Join(repo, ".git", "worktrees", name, "gitdir")
			var err error
			switch broken {
			case "missing":
				err = os.Remove(backlink)
			case "empty":
				err = os.WriteFile(backlink, nil, 0o600)
			case "moved":
				moved := filepath.Join(t.TempDir(), "moved")
				err = os.Rename(linked, moved)
				linked = moved
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := GitCommand(t.Context(), linked, "status"); err == nil || !strings.Contains(err.Error(), "run `git worktree repair`") {
				t.Fatalf("a %s backlink = %v; want it refused with advice to repair", broken, err)
			}
			repair := exec.Command("git", "worktree", "repair")
			repair.Dir = linked
			if out, err := repair.CombinedOutput(); err != nil {
				t.Fatalf("git worktree repair: %v\n%s", err, out)
			}
			if out, err := viewRun(t, linked, "status", "--porcelain"); err != nil || strings.TrimSpace(out) != "" {
				t.Fatalf("the repaired worktree's view = %q, %v", out, err)
			}
		})
	}

	// The backlink names the worktree through a linked folder, as /var names /private/var on
	// macOS, or in another case on a disk that ignores case. Views and `coop approve` compare the
	// files themselves, so both accept every spelling or neither does.
	for _, spelling := range []string{"through a linked folder", "in another case"} {
		t.Run("spelled "+spelling, func(t *testing.T) {
			parent := t.TempDir()
			linked := filepath.Join(parent, "spelled")
			run("worktree", "add", "-q", linked, "-b", "spelled-"+strings.ReplaceAll(spelling, " ", "-"))
			spelled := filepath.Join(parent, "SPELLED", ".git")
			if spelling == "through a linked folder" {
				alias := filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(parent, alias); err != nil {
					t.Fatal(err)
				}
				spelled = filepath.Join(alias, "spelled", ".git")
			} else if _, err := os.Stat(filepath.Dir(spelled)); err != nil {
				t.Skip("this disk tells names apart by case") // nothing else in the fresh folder is called SPELLED
			}
			pointer, err := os.ReadFile(filepath.Join(linked, ".git"))
			if err != nil {
				t.Fatal(err)
			}
			admin := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(pointer)), "gitdir:"))
			if err := os.WriteFile(filepath.Join(admin, "gitdir"), []byte(spelled+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if out, err := viewRun(t, linked, "status", "--porcelain"); err != nil || strings.TrimSpace(out) != "" {
				t.Fatalf("a backlink spelled %s, in a view = %q, %v", spelling, out, err)
			}
			if err := ExcludeIfRepository(linked, "/.coop-spelling"); err != nil {
				t.Fatalf("a backlink spelled %s, in coop approve: %v", spelling, err)
			}
		})
	}

	t.Run("relative paths", func(t *testing.T) {
		// Git 2.48's worktree.useRelativePaths writes both links relative; they name the same folders.
		relative := filepath.Join(t.TempDir(), "relative")
		run("worktree", "add", "-q", relative, "-b", "relative")
		if real, err := filepath.EvalSymlinks(relative); err == nil {
			relative = real
		}
		admin := filepath.Join(repo, ".git", "worktrees", "relative")
		toAdmin, err := filepath.Rel(relative, admin)
		if err != nil {
			t.Fatal(err)
		}
		toWorktree, err := filepath.Rel(admin, filepath.Join(relative, ".git"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(relative, ".git"), []byte("gitdir: "+toAdmin+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(admin, "gitdir"), []byte(toWorktree+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := viewRun(t, relative, "status", "--porcelain"); err != nil || strings.TrimSpace(out) != "" {
			t.Fatalf("a worktree linked by relative paths = %q, %v", out, err)
		}
	})
}

// A view refreshes under its own lock, never the process-wide one: while one repository's refresh
// is held up, a command in another repository still runs. The shim holds the config parse of the
// repository marked for it until the test lets go. Coop may also run inside a box's repository
// whose HEAD is a FIFO, which no refresh of another repository may touch.
func TestGitViewStalledRefreshNeverHoldsUpAnotherView(t *testing.T) {
	for _, from := range []string{"the test folder", "a box repository with a FIFO at HEAD"} {
		t.Run("from "+from, func(t *testing.T) {
			stalled, run := viewTestRepo(t)
			run("config", "cooptest.stall", "true")
			other, _ := viewTestRepo(t)
			if from != "the test folder" {
				t.Chdir(fifoHeadRepo(t))
			}
			realGit, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			signals, shim := t.TempDir(), t.TempDir()
			entered, release := filepath.Join(signals, "entered"), filepath.Join(signals, "release")
			script := `#!/bin/sh
if [ "$1" = config ] && [ "$2" = --file ] && grep -q 'stall = true' "$3"; then
	: > "$COOP_TEST_STALL_ENTERED"
	while [ ! -e "$COOP_TEST_STALL_RELEASE" ]; do sleep 0.01; done
fi
exec "$COOP_TEST_REAL_GIT" "$@"
`
			if err := os.WriteFile(filepath.Join(shim, "git"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("COOP_TEST_STALL_ENTERED", entered)
			t.Setenv("COOP_TEST_STALL_RELEASE", release)
			t.Setenv("COOP_TEST_REAL_GIT", realGit)
			t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))

			held, heldErr := make(chan struct{}), error(nil)
			go func() {
				defer close(held)
				_, heldErr = GitCommand(t.Context(), stalled, "status")
			}()
			t.Cleanup(func() { <-held }) // t.Context() ends first, so a failed test kills the shim
			wait.ForFile(t, entered)

			err = returnsWithin(t, "a command in another repository while one view is held", func() error {
				cmd, err := GitCommand(t.Context(), other, "status", "--porcelain")
				if err != nil {
					return err
				}
				return cmd.Run()
			})
			if err != nil {
				t.Fatalf("the other repository's command: %v", err)
			}
			select {
			case <-held:
				t.Fatalf("the held view returned before it was released: %v", heldErr)
			default:
			}
			if err := os.WriteFile(release, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := returnsWithin(t, "the released view", func() error { <-held; return heldErr }); err != nil {
				t.Fatalf("the released view: %v", err)
			}
		})
	}
}

// A view whose first refresh fails is not kept. Kept, it would count as used by this process, and
// the next command reaching its git dir by another path would clear the state an interrupted
// rebase left in it for the next coop to recover.
func TestGitViewIsNotKeptWhenItsFirstRefreshFails(t *testing.T) {
	repo, _ := viewTestRepo(t)
	config := filepath.Join(repo, ".git", "config")
	saved, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	cached := func() bool {
		gitViewsMu.Lock()
		defer gitViewsMu.Unlock()
		return gitViews[filepath.Join(repo, ".git")] != nil
	}
	plantMetadata(t, config, "fifo", 0)
	if _, err := GitCommand(t.Context(), repo, "status"); err == nil {
		t.Fatal("a view read a FIFO config")
	}
	if cached() {
		t.Fatal("a view whose first refresh failed was kept")
	}
	if err := os.Remove(config); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, saved, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := viewRun(t, repo, "status", "--porcelain"); err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("the view after the config came back = %q, %v", out, err)
	}
	if !cached() {
		t.Fatal("a view that refreshed was not kept")
	}
}

// Two commands reaching one git dir by two spellings at once each get their own work tree: the
// environment is built before the other command can move the view to its spelling.
func TestGitViewEnvironmentIsEachCommandsOwn(t *testing.T) {
	repo, _ := viewTestRepo(t)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(repo, alias); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, dir := range []string{repo, alias} {
		wg.Go(func() {
			for range 10 {
				cmd, err := GitCommand(t.Context(), dir, "status")
				if err == nil && !slices.Contains(cmd.Env, "GIT_WORK_TREE="+dir) {
					// Only the work tree: the rest of the environment can hold secrets.
					got := "none"
					if i := slices.IndexFunc(cmd.Env, func(entry string) bool { return strings.HasPrefix(entry, "GIT_WORK_TREE=") }); i >= 0 {
						got = cmd.Env[i]
					}
					err = fmt.Errorf("a command in %s got another work tree: %s", dir, got)
				}
				if err != nil {
					errs <- err
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// The observation snapshot copies the index as well; whatever a box leaves there fails fast. An
// index too large to copy is named with its size, and the refusal says how to write a smaller one.
func TestGitObservationRefusesSpecialIndexPromptly(t *testing.T) {
	for _, kind := range []string{"fifo", "device", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			repo, _ := viewTestRepo(t)
			index := filepath.Join(repo, ".git", "index")
			plantMetadata(t, index, kind, gitStoreFileLimit)
			err := returnsWithin(t, "an observation with "+kind+" index", func() error {
				_, err := ObserveGit(t.Context(), repo, "status", "--porcelain")
				return err
			})
			if err == nil {
				t.Fatalf("an observation read a %s index", kind)
			}
			if kind == "oversize" && (!strings.Contains(err.Error(), index+" is 1.0 GiB, more than the 1 GiB Coop reads") ||
				!strings.Contains(err.Error(), "git update-index --index-version 4")) {
				t.Fatalf("an oversize index = %v; want it named with its size and how to shrink it", err)
			}
		})
	}
}
