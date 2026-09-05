package forkctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// Existing land tests care about the committed result, not removal. Own and close
// the approval here; deletion tests below exercise the actual outcome directly.
func mergeOneForTest(t *testing.T, c *Control, repo, img, name string, force bool) (bool, error) {
	t.Helper()
	result, err := c.mergeOne(repo, img, name, force)
	t.Cleanup(result.approval.close)
	return result.landed, err
}

func prepareLandedFork(t *testing.T, generation bool) (string, string, *landedFork) {
	t.Helper()
	repo := initRepo(t)
	ws, err := forkspace.Setup(repo, "landed")
	if err != nil {
		t.Fatal(err)
	}
	if generation {
		changeForkGeneration(t, repo, false)
	}
	if err := os.WriteFile(filepath.Join(ws, "feature.txt"), []byte("landed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "feature.txt")
	git(t, ws, "commit", "-qm", "feature")
	c := &Control{cfg: &config.Config{}}
	result, err := c.mergeOne(repo, "", "landed", false)
	t.Cleanup(result.approval.close)
	if err != nil || !result.landed || result.approval == nil {
		t.Fatalf("land = %+v, %v", result, err)
	}
	return repo, ws, result.approval
}

func changeForkGeneration(t *testing.T, repo string, remove bool) {
	t.Helper()
	unlock, err := forkspace.LockState(repo, "landed")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if remove {
		identity, _, err := forkspace.ReadGeneration(repo, "landed")
		if err != nil {
			t.Fatal(err)
		}
		if err := forkspace.RemoveGenerationIfMatchesLocked(repo, identity); err != nil {
			t.Fatal(err)
		}
	} else if _, err := forkspace.EnsureGenerationLocked(repo, "landed"); err != nil {
		t.Fatal(err)
	}
}

func TestDestroyLandedForkRequiresExactApproval(t *testing.T) {
	for _, change := range []string{"generation added", "generation removed", "generation replaced", "directory replaced", "symlink replaced", "closed approval", "closed descriptor", "missing approval", "invalid HEAD", "invalid config", "invalid parent config", "parent no longer contains land"} {
		t.Run(change, func(t *testing.T) {
			repo, ws, approval := prepareLandedFork(t, change == "generation removed" || change == "generation replaced")
			switch change {
			case "generation added":
				changeForkGeneration(t, repo, false)
			case "generation removed", "generation replaced":
				changeForkGeneration(t, repo, true)
				if change == "generation replaced" {
					changeForkGeneration(t, repo, false)
				}
			case "directory replaced", "symlink replaced":
				retained := ws + "-retained"
				if err := os.Rename(ws, retained); err != nil {
					t.Fatal(err)
				}
				if change == "symlink replaced" {
					if err := os.Symlink(retained, ws); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Mkdir(ws, 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(ws, "feature.txt"), []byte("replacement\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			case "closed approval":
				approval.close()
			case "closed descriptor":
				if err := approval.pin.Close(); err != nil {
					t.Fatal(err)
				}
			case "missing approval":
				approval = nil
			case "invalid HEAD":
				if err := os.WriteFile(filepath.Join(ws, ".git", "HEAD"), []byte("invalid\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "invalid config", "invalid parent config":
				target := ws
				if change == "invalid parent config" {
					target = repo
				}
				if err := os.WriteFile(filepath.Join(target, ".git", "config"), []byte("[invalid\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "parent no longer contains land":
				git(t, repo, "update-ref", "HEAD", "HEAD~1")
			}
			if err := destroyLandedFork(runtime.Runtime{}, repo, "landed", approval); err == nil {
				t.Fatal("cleanup accepted changed or uninspectable authority")
			}
			if _, err := os.ReadFile(filepath.Join(ws, "feature.txt")); err != nil {
				t.Fatalf("cleanup lost retained workspace: %v", err)
			}
		})
	}
}

func TestDestroyLandedForkRechecksAfterServiceShutdown(t *testing.T) {
	repo := initRepo(t)
	ws, err := forkspace.Setup(repo, "landed")
	if err != nil {
		t.Fatal(err)
	}
	compose := filepath.Join(ws, filepath.FromSlash(project.DefaultCompose))
	if err := os.MkdirAll(filepath.Dir(compose), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(compose, []byte("services:\n  db:\n    image: postgres:18.4-alpine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", ".agent")
	git(t, ws, "commit", "-qm", "service")
	c := &Control{cfg: &config.Config{}}
	result, err := c.mergeOne(repo, "", "landed", false)
	t.Cleanup(result.approval.close)
	if err != nil || result.approval == nil {
		t.Fatalf("land: %v", err)
	}
	stubDir := t.TempDir()
	stub := "#!/bin/sh\nfor arg in \"$@\"; do\n  if [ \"$arg\" = down ]; then\n    printf 'late shutdown work\\n' > \"$COOP_TEST_LATE_FILE\"\n  fi\ndone\n"
	if err := os.WriteFile(filepath.Join(stubDir, "stubruntime"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	lateFile := filepath.Join(ws, "shutdown.txt")
	t.Setenv("COOP_TEST_LATE_FILE", lateFile)
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := destroyLandedFork(runtime.Runtime{Name: "stubruntime"}, repo, "landed", result.approval); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("cleanup after service write = %v", err)
	}
	if data, err := os.ReadFile(lateFile); err != nil || string(data) != "late shutdown work\n" {
		t.Fatalf("shutdown work lost: %q, %v", data, err)
	}
}

func TestDestroyLandedForkConsumesLegacyAndGeneratedApproval(t *testing.T) {
	for _, generation := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "generated"}[generation], func(t *testing.T) {
			repo, ws, approval := prepareLandedFork(t, generation)
			if approval.hasGeneration != generation || approval.head != gitOut(repo, "rev-parse", "HEAD") {
				t.Fatalf("approval = %+v", approval)
			}
			if err := destroyLandedFork(runtime.Runtime{}, repo, "landed", approval); err != nil {
				t.Fatal(err)
			}
			if pathExists(ws) {
				t.Fatal("approved workspace survived cleanup")
			}
			if _, exists, err := forkspace.ReadGeneration(repo, "landed"); err != nil || exists {
				t.Fatalf("generation after cleanup = %v, %v", exists, err)
			}
		})
	}
}

func TestMergeOneTaskApprovalTransfersOnlyAfterFinalization(t *testing.T) {
	for _, replay := range []bool{false, true} {
		t.Run(map[bool]string{false: "new candidate", true: "journal replay"}[replay], func(t *testing.T) {
			repo, ws, _, identity, c := prepareForkTaskCandidate(t, "task-land")
			if replay {
				c.afterLandFastForward = func() error { return errors.New("injected finalization failure") }
				result, err := c.mergeOne(repo, "", identity.Name, false)
				t.Cleanup(result.approval.close)
				if err == nil || !result.landed || result.approval != nil {
					t.Fatalf("failed finalization transferred approval: %+v, %v", result, err)
				}
				c.afterLandFastForward = nil
			}
			result, err := c.mergeOne(repo, "", identity.Name, false)
			t.Cleanup(result.approval.close)
			if err != nil || !result.landed || result.approval == nil {
				t.Fatalf("finished task land: %+v, %v", result, err)
			}
			if result.approval.identity != identity || result.approval.head != gitOut(ws, "rev-parse", "HEAD") {
				t.Fatalf("approval did not bind exact completed candidate: %+v", result.approval)
			}
			if err := destroyLandedFork(runtime.Runtime{}, repo, identity.Name, result.approval); err != nil {
				t.Fatal(err)
			}
			if pathExists(ws) {
				t.Fatal("completed candidate workspace survived approval")
			}
		})
	}
}

func TestForkMergeAllTerminalRefusal(t *testing.T) {
	if os.Getenv("COOP_TEST_MERGE_PROMPT") == "1" {
		repo := initRepo(t)
		ws := forkspace.Workspace(repo, "untouched")
		if err := os.MkdirAll(ws, 0o700); err != nil {
			t.Fatal(err)
		}
		c := &Control{cfg: &config.Config{}}
		code, err := c.forkMergeAll(repo, []string{"untouched"}, "", false, false)
		if code != 2 || err == nil || err.Error() != "cancelled" || !pathExists(ws) {
			t.Fatalf("terminal refusal = %d, %v; workspace exists=%v", code, err, pathExists(ws))
		}
		fmt.Println("terminal-refusal-kept-workspace")
		return
	}
	script, err := exec.LookPath("script")
	if err != nil {
		t.Skip("script(1) unavailable for terminal coverage")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"\n", "\x04"} {
		t.Run(map[string]string{"\n": "Enter", "\x04": "EOF"}[input], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			var args []string
			switch goruntime.GOOS {
			case "darwin", "freebsd":
				args = []string{"-q", "/dev/null", binary, "-test.run=^TestForkMergeAllTerminalRefusal$"}
			case "linux":
				quoted := "'" + strings.ReplaceAll(binary, "'", "'\"'\"'") + "'"
				args = []string{"-qefc", quoted + " -test.run=^TestForkMergeAllTerminalRefusal$", "/dev/null"}
			default:
				t.Skipf("terminal coverage unsupported on %s", goruntime.GOOS)
			}
			cmd := exec.CommandContext(ctx, script, args...)
			cmd.Env = append(os.Environ(), "COOP_TEST_MERGE_PROMPT=1")
			cmd.Stdin = strings.NewReader(input)
			cmd.WaitDelay = time.Second
			out, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(out), "[y/N]") || !strings.Contains(string(out), "terminal-refusal-kept-workspace") {
				t.Fatalf("terminal confirmation: %v\n%s", err, out)
			}
		})
	}
}

func TestDestroyLandedForkInspectsEveryPopulatedSubmodule(t *testing.T) {
	for _, mode := range []string{"clean", "whitespace child", "empty uninitialized", "top ignore", "nested ignore", "nested assume", "nested skip", "inactive nested assume", "nested commit", "inactive nested commit", "hidden untracked", "missing Git file", "foreign Git root", "symlinked child"} {
		t.Run(mode, func(t *testing.T) {
			leafSource, middleSource, repo := initRepo(t), initRepo(t), initRepo(t)
			leafName := "leaf"
			if mode == "whitespace child" {
				leafName = "leaf "
			}
			git(t, middleSource, "-c", "protocol.file.allow=always", "submodule", "add", "--quiet", "--name", "leaf", leafSource, leafName)
			git(t, middleSource, "commit", "-qam", "nested module")
			git(t, repo, "-c", "protocol.file.allow=always", "submodule", "add", "--quiet", middleSource, "middle")
			git(t, repo, "commit", "-qam", "module")
			ws, err := forkspace.Setup(repo, "landed")
			if err != nil {
				t.Fatal(err)
			}
			if mode != "empty uninitialized" {
				git(t, ws, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--recursive", "--quiet")
			}
			c := &Control{cfg: &config.Config{}}
			result, err := c.mergeOne(repo, "", "landed", false)
			t.Cleanup(result.approval.close)
			if err != nil || result.approval == nil {
				t.Fatalf("land: %v", err)
			}
			middle, leaf := filepath.Join(ws, "middle"), filepath.Join(ws, "middle", leafName)
			target, file := leaf, "README.md"
			switch mode {
			case "top ignore":
				git(t, ws, "config", "diff.ignoreSubmodules", "all")
				target = middle
			case "nested ignore":
				git(t, middle, "config", "submodule.leaf.ignore", "all")
			case "nested assume", "inactive nested assume":
				git(t, leaf, "update-index", "--assume-unchanged", "README.md")
				if mode == "inactive nested assume" {
					git(t, ws, "config", "submodule.middle.active", "false")
				}
			case "nested skip":
				git(t, leaf, "update-index", "--skip-worktree", "README.md")
			case "inactive nested commit":
				git(t, ws, "config", "submodule.middle.active", "false")
			case "hidden untracked":
				git(t, leaf, "config", "status.showUntrackedFiles", "no")
				file = "late-untracked.txt"
			case "missing Git file":
				if err := os.Rename(filepath.Join(leaf, ".git"), filepath.Join(t.TempDir(), "saved-git-file")); err != nil {
					t.Fatal(err)
				}
			case "foreign Git root":
				git(t, leaf, "config", "core.worktree", leafSource)
			case "symlinked child":
				retained := filepath.Join(t.TempDir(), "retained-leaf")
				if err := os.Rename(leaf, retained); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(retained, leaf); err != nil {
					t.Fatal(err)
				}
			}
			safe := mode == "clean" || mode == "whitespace child" || mode == "empty uninitialized"
			if !safe {
				if err := os.WriteFile(filepath.Join(target, file), []byte("late nested work\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if mode == "nested commit" || mode == "inactive nested commit" {
					git(t, leaf, "add", file)
					git(t, leaf, "commit", "-qm", "late nested commit")
				}
			}
			err = destroyLandedFork(runtime.Runtime{}, repo, "landed", result.approval)
			if safe {
				if err != nil || pathExists(ws) {
					t.Fatalf("clean submodule cleanup: %v, workspace remains=%v", err, pathExists(ws))
				}
				return
			}
			if err == nil {
				t.Fatal("cleanup accepted hidden or uninspectable child work")
			}
			if data, err := os.ReadFile(filepath.Join(target, file)); err != nil || string(data) != "late nested work\n" {
				t.Fatalf("nested work lost: %q, %v", data, err)
			}
		})
	}
}

func TestDestroyLandedForkPreservesPostLandWork(t *testing.T) {
	for _, change := range []string{"tracked", "untracked", "commit", "unreadable index", "assume-unchanged", "skip-worktree"} {
		t.Run(change, func(t *testing.T) {
			repo := initRepo(t)
			ws, err := forkspace.Setup(repo, "landed")
			if err != nil {
				t.Fatal(err)
			}
			c := &Control{cfg: &config.Config{}}
			result, err := c.mergeOne(repo, "", "landed", false)
			t.Cleanup(result.approval.close)
			if err != nil || !result.landed || result.approval == nil {
				t.Fatalf("land = %v, %v", result, err)
			}
			file := "README.md"
			if change == "untracked" {
				file = "late-work.txt"
			}
			if change == "assume-unchanged" || change == "skip-worktree" {
				git(t, ws, "update-index", "--"+change, file)
			}
			if err := os.WriteFile(filepath.Join(ws, file), []byte("new unapproved work\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if change == "commit" {
				git(t, ws, "add", "README.md")
				git(t, ws, "commit", "-qm", "late work")
			}
			if change == "unreadable index" {
				if err := os.WriteFile(filepath.Join(ws, ".git", "index"), []byte("invalid index"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := destroyLandedFork(runtime.Runtime{}, repo, "landed", result.approval); err == nil {
				t.Error("post-land cleanup accepted unapproved or unreadable work")
			}
			if data, err := os.ReadFile(filepath.Join(ws, file)); err != nil || string(data) != "new unapproved work\n" {
				t.Fatalf("post-land work lost: %q, %v", data, err)
			}
		})
	}
}
