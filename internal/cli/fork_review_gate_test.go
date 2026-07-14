package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
)

func reviewSourceSnapshot(dir string) string {
	return strings.Join([]string{
		gitOut(dir, "rev-parse", "HEAD"),
		gitOut(dir, "rev-parse", "--abbrev-ref", "HEAD"),
		gitOut(dir, "status", "--porcelain"),
		gitOut(dir, "show-ref"),
	}, "\n")
}

func setupReviewGateFork(t *testing.T, conflict bool) (string, string) {
	t.Helper()
	repo := initRepo(t)
	if conflict {
		if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("base\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, repo, "add", "-A")
		git(t, repo, "commit", "-qm", "shared base")
	}
	ws, err := setupFork(repo, "perf")
	if err != nil {
		t.Fatal(err)
	}
	forkPath, parentPath := filepath.Join(ws, "fork.txt"), filepath.Join(repo, "parent.txt")
	if conflict {
		forkPath, parentPath = filepath.Join(ws, "shared.txt"), filepath.Join(repo, "shared.txt")
	}
	if err := os.WriteFile(forkPath, []byte("fork\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "fork work")
	if err := os.WriteFile(parentPath, []byte("parent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "parent moves")
	return repo, ws
}

func reviewCommandOutput(t *testing.T, a *app, args ...string) (string, int, error) {
	t.Helper()
	var code int
	var err error
	out := captureStdout(t, func() { code, err = a.forkReview(args) })
	return out, code, err
}

func TestPrepareForkReviewCandidate(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))

	t.Run("rebases in scratch without changing either source", func(t *testing.T) {
		repo := initRepo(t)
		ws, err := setupFork(repo, "perf")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, "fork.txt"), []byte("fork\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, ws, "add", "-A")
		git(t, ws, "commit", "-qm", "fork work")
		if err := os.WriteFile(filepath.Join(repo, "parent.txt"), []byte("parent\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, repo, "add", "-A")
		git(t, repo, "commit", "-qm", "parent moves")

		parentBefore, forkBefore := reviewSourceSnapshot(repo), reviewSourceSnapshot(ws)
		candidate, err := prepareForkReviewCandidate(repo, ws, "perf")
		if err != nil {
			t.Fatal(err)
		}
		if candidate.conflict {
			t.Fatal("independent changes should rebase cleanly")
		}
		if gitBranch(candidate.dir) != "perf" {
			t.Fatalf("scratch branch = %q, want perf", gitBranch(candidate.dir))
		}
		for _, file := range []string{"fork.txt", "parent.txt"} {
			if !pathExists(filepath.Join(candidate.dir, file)) {
				t.Errorf("rebased candidate missing %s", file)
			}
		}
		if got := reviewSourceSnapshot(repo); got != parentBefore {
			t.Errorf("parent changed during scratch rebase:\nbefore:\n%s\nafter:\n%s", parentBefore, got)
		}
		if got := reviewSourceSnapshot(ws); got != forkBefore {
			t.Errorf("fork changed during scratch rebase:\nbefore:\n%s\nafter:\n%s", forkBefore, got)
		}
		dir := candidate.dir
		candidate.cleanup()
		if pathExists(dir) {
			t.Errorf("scratch clone still exists after cleanup: %s", dir)
		}
	})

	t.Run("reports conflict without changing either source", func(t *testing.T) {
		repo := initRepo(t)
		path := filepath.Join(repo, "conflict.txt")
		if err := os.WriteFile(path, []byte("base\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, repo, "add", "-A")
		git(t, repo, "commit", "-qm", "conflict base")
		ws, err := setupFork(repo, "perf")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, "conflict.txt"), []byte("fork\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, ws, "add", "-A")
		git(t, ws, "commit", "-qm", "fork conflicts")
		if err := os.WriteFile(path, []byte("parent\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, repo, "add", "-A")
		git(t, repo, "commit", "-qm", "parent conflicts")

		parentBefore, forkBefore := reviewSourceSnapshot(repo), reviewSourceSnapshot(ws)
		candidate, err := prepareForkReviewCandidate(repo, ws, "perf")
		if err != nil {
			t.Fatal(err)
		}
		if !candidate.conflict {
			t.Fatal("conflicting changes should report a rebase conflict")
		}
		if got := reviewSourceSnapshot(repo); got != parentBefore {
			t.Errorf("parent changed during conflict preview:\nbefore:\n%s\nafter:\n%s", parentBefore, got)
		}
		if got := reviewSourceSnapshot(ws); got != forkBefore {
			t.Errorf("fork changed during conflict preview:\nbefore:\n%s\nafter:\n%s", forkBefore, got)
		}
		candidate.cleanup()
	})

	t.Run("operational failure removes scratch", func(t *testing.T) {
		repo := initRepo(t)
		tmp := t.TempDir()
		t.Setenv("TMPDIR", tmp)
		if _, err := prepareForkReviewCandidate(repo, filepath.Join(repo, "missing"), "perf"); err == nil {
			t.Fatal("missing fork source should fail")
		}
		entries, err := os.ReadDir(tmp)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Errorf("failed preview leaked scratch entries: %v", entries)
		}
	})
}

func TestForkReviewGateOutcomes(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	t.Run("no configured gate reports a clean rebase", func(t *testing.T) {
		repo, ws := setupReviewGateFork(t, false)
		tmp := t.TempDir()
		t.Setenv("TMPDIR", tmp)
		parentBefore, forkBefore := reviewSourceSnapshot(repo), reviewSourceSnapshot(ws)
		a := &app{cfg: &config.Config{RepoOverride: repo}}
		out, code, err := reviewCommandOutput(t, a, "perf", "--gate", "--stat")
		if err != nil || code != 0 {
			t.Fatalf("forkReview = (%d, %v), want (0, nil)\n%s", code, err, out)
		}
		if !strings.Contains(out, "gate: none configured — rebase clean") {
			t.Errorf("missing no-gate outcome:\n%s", out)
		}
		assertReviewSourcesUnchanged(t, repo, ws, parentBefore, forkBefore)
		assertReviewScratchEmpty(t, tmp)
	})

	t.Run("conflict exits one without running a gate", func(t *testing.T) {
		repo, ws := setupReviewGateFork(t, true)
		tmp := t.TempDir()
		t.Setenv("TMPDIR", tmp)
		parentBefore, forkBefore := reviewSourceSnapshot(repo), reviewSourceSnapshot(ws)
		called := false
		a := &app{
			cfg:   &config.Config{RepoOverride: repo, BaseImage: "gate-image", Gate: []string{"make", "check"}},
			rt:    runtime.Runtime{Name: "definitely-not-a-runtime"},
			rtSet: true,
			gateOK: func(_, _, _ string) bool {
				called = true
				return true
			},
		}
		out, code, err := reviewCommandOutput(t, a, "perf", "--gate", "--stat")
		if err != nil || code != 1 {
			t.Fatalf("forkReview = (%d, %v), want (1, nil)\n%s", code, err, out)
		}
		if called {
			t.Error("gate ran despite the rebase conflict")
		}
		if !strings.Contains(out, "conflict while rebasing onto current parent — gate not run") {
			t.Errorf("missing conflict outcome:\n%s", out)
		}
		assertReviewSourcesUnchanged(t, repo, ws, parentBefore, forkBefore)
		assertReviewScratchEmpty(t, tmp)
	})

	t.Run("box startup failure is operational and still cleans scratch", func(t *testing.T) {
		repo, ws := setupReviewGateFork(t, false)
		tmp := t.TempDir()
		t.Setenv("TMPDIR", tmp)
		parentBefore, forkBefore := reviewSourceSnapshot(repo), reviewSourceSnapshot(ws)
		runtimePath := filepath.Join(t.TempDir(), "one-shot-runtime")
		if err := os.WriteFile(runtimePath, []byte("#!/bin/sh\nrm -- \"$0\"\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		a := &app{
			cfg:   &config.Config{RepoOverride: repo, BaseImage: "gate-image", Gate: []string{"true"}, Egress: "none"},
			rt:    runtime.Runtime{Name: runtimePath},
			rtSet: true,
		}
		_, code, err := reviewCommandOutput(t, a, "perf", "--gate", "--stat")
		if err == nil || code != -1 || !strings.Contains(err.Error(), "run review gate") {
			t.Fatalf("forkReview = (%d, %v), want operational gate error", code, err)
		}
		assertReviewSourcesUnchanged(t, repo, ws, parentBefore, forkBefore)
		assertReviewScratchEmpty(t, tmp)
	})

	for _, tc := range []struct {
		name     string
		green    bool
		wantCode int
		wantLine string
	}{
		{name: "green", green: true, wantCode: 0, wantLine: "green on rebased scratch (read-only)"},
		{name: "red", green: false, wantCode: 1, wantLine: "red on rebased scratch (read-only)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, ws := setupReviewGateFork(t, false)
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			parentBefore, forkBefore := reviewSourceSnapshot(repo), reviewSourceSnapshot(ws)
			var scratch string
			a := &app{
				cfg:   &config.Config{RepoOverride: repo, BaseImage: "gate-image", Gate: []string{"make", "check"}},
				rt:    runtime.Runtime{Name: "true"},
				rtSet: true,
				gateOK: func(gateRepo, treeDir, _ string) bool {
					if gateRepo != repo {
						t.Errorf("gate policy repo = %q, want parent %q", gateRepo, repo)
					}
					if treeDir == repo || treeDir == ws {
						t.Errorf("gate tree = source repo %q, want scratch", treeDir)
					}
					if got := gitBranch(treeDir); got != "perf" {
						t.Errorf("gate tree branch = %q, want perf", got)
					}
					for _, file := range []string{"fork.txt", "parent.txt"} {
						if !pathExists(filepath.Join(treeDir, file)) {
							t.Errorf("rebased gate tree missing %s", file)
						}
					}
					scratch = treeDir
					return tc.green
				},
			}
			out, code, err := reviewCommandOutput(t, a, "perf", "--gate", "--stat")
			if err != nil || code != tc.wantCode {
				t.Fatalf("forkReview = (%d, %v), want (%d, nil)\n%s", code, err, tc.wantCode, out)
			}
			if !strings.Contains(out, tc.wantLine) {
				t.Errorf("missing %s outcome:\n%s", tc.name, out)
			}
			if scratch == "" {
				t.Fatal("gate seam was not called")
			}
			if pathExists(scratch) {
				t.Errorf("review scratch still exists after %s outcome: %s", tc.name, scratch)
			}
			assertReviewSourcesUnchanged(t, repo, ws, parentBefore, forkBefore)
			assertReviewScratchEmpty(t, tmp)
		})
	}
}

func TestForkReviewWithoutGateKeepsExistingPath(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, ws := setupReviewGateFork(t, false)
	forkBefore := reviewSourceSnapshot(ws)
	parentHead := gitOut(repo, "rev-parse", "HEAD")
	badTmp := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(badTmp, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", badTmp) // a scratch clone would fail immediately
	a := &app{
		cfg:   &config.Config{RepoOverride: repo, Gate: []string{"make", "check"}},
		rt:    runtime.Runtime{Name: "definitely-not-a-runtime"},
		rtSet: true,
		gateOK: func(_, _, _ string) bool {
			t.Fatal("flag-off review must not run the gate")
			return false
		},
	}
	out, code, err := reviewCommandOutput(t, a, "perf", "--stat")
	if err != nil || code != 0 {
		t.Fatalf("forkReview = (%d, %v), want (0, nil)\n%s", code, err, out)
	}
	if !strings.Contains(out, "runs at merge — rolled back on failure") {
		t.Errorf("flag-off dossier changed:\n%s", out)
	}
	if got := gitOut(repo, "rev-parse", "HEAD"); got != parentHead {
		t.Errorf("parent HEAD moved: %s -> %s", parentHead, got)
	}
	if got := gitOut(repo, "rev-parse", "review/perf"); got != gitOut(ws, "rev-parse", "perf") {
		t.Errorf("review ref = %q, want fork tip", got)
	}
	if got := reviewSourceSnapshot(ws); got != forkBefore {
		t.Errorf("fork changed on flag-off review:\nbefore:\n%s\nafter:\n%s", forkBefore, got)
	}
}

func TestForkReviewGateRejectsOpen(t *testing.T) {
	a := &app{cfg: &config.Config{}}
	if code, err := a.forkReview([]string{"perf", "--gate", "--open"}); code != 2 || err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("forkReview(--gate --open) = (%d, %v), want usage rejection", code, err)
	}
}

func assertReviewSourcesUnchanged(t *testing.T, repo, ws, parentBefore, forkBefore string) {
	t.Helper()
	if got := reviewSourceSnapshot(repo); got != parentBefore {
		t.Errorf("parent changed during review gate:\nbefore:\n%s\nafter:\n%s", parentBefore, got)
	}
	if got := reviewSourceSnapshot(ws); got != forkBefore {
		t.Errorf("fork changed during review gate:\nbefore:\n%s\nafter:\n%s", forkBefore, got)
	}
	if got := gitOut(repo, "show-ref", "--verify", "refs/heads/review/perf"); got != "" {
		t.Errorf("review gate created parent review ref: %s", got)
	}
}

func assertReviewScratchEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("review scratch leaked entries: %v", entries)
	}
}
