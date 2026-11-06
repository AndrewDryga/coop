package forkctl

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/runtime"
)

func reviewSourceSnapshot(dir string) string {
	return strings.Join([]string{
		forkspace.GitOut(dir, "rev-parse", "HEAD"),
		forkspace.GitOut(dir, "rev-parse", "--abbrev-ref", "HEAD"),
		forkspace.GitOut(dir, "status", "--porcelain"),
		forkspace.GitOut(dir, "show-ref"),
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
	ws, err := forkspace.Setup(repo, "perf")
	if err != nil {
		t.Fatal(err)
	}
	ForkPath, parentPath := filepath.Join(ws, "fork.txt"), filepath.Join(repo, "parent.txt")
	if conflict {
		ForkPath, parentPath = filepath.Join(ws, "shared.txt"), filepath.Join(repo, "shared.txt")
	}
	if err := os.WriteFile(ForkPath, []byte("fork\n"), 0o644); err != nil {
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

func reviewCommandOutput(t *testing.T, c *Control, args ...string) (string, int, error) {
	t.Helper()
	var code int
	var err error
	out := captureStdout(t, func() { code, err = c.ForkReview(args) })
	return out, code, err
}

func TestPrepareForkReviewCandidate(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
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
		c := &Control{cfg: &config.Config{RepoOverride: repo}}
		out, code, err := reviewCommandOutput(t, c, "perf", "--gate", "--stat")
		if err != nil || code != 0 {
			t.Fatalf("ForkReview = (%d, %v), want (0, nil)\n%s", code, err, out)
		}
		if !strings.Contains(out, "⚠ No project checks are configured") ||
			!strings.Contains(out, "rebased cleanly onto") {
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
		c := &Control{
			cfg: &config.Config{RepoOverride: repo, BaseImage: "gate-image", Gate: []string{"make", "check"}},
			rt:  runtime.Runtime{Name: "definitely-not-a-runtime"},
			gateOK: func(_, _, _ string) bool {
				called = true
				return true
			},
		}
		out, code, err := reviewCommandOutput(t, c, "perf", "--gate", "--stat")
		if err != nil || code != 1 {
			t.Fatalf("ForkReview = (%d, %v), want (1, nil)\n%s", code, err, out)
		}
		if called {
			t.Error("gate ran despite the rebase conflict")
		}
		if !strings.Contains(out, "✗ Fork perf conflicts with") || !strings.Contains(out, "Project checks did not run.") {
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
		c := &Control{
			// The gate box is real up to its runtime, so its run files need a Coop home of the test's own.
			cfg: &config.Config{RepoOverride: repo, BaseImage: "gate-image", Gate: []string{"true"}, Egress: "none", BoxHome: t.TempDir()},
			rt:  runtime.Runtime{Name: runtimePath},
		}
		_, code, err := reviewCommandOutput(t, c, "perf", "--gate", "--stat")
		if err == nil || code != -1 || !strings.Contains(err.Error(), "run review gate") {
			t.Fatalf("ForkReview = (%d, %v), want operational gate error", code, err)
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
		{name: "green", green: true, wantCode: 0, wantLine: "✓ Project checks passed"},
		{name: "red", green: false, wantCode: 1, wantLine: "✗ Project checks failed for fork perf"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, ws := setupReviewGateFork(t, false)
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			parentBefore, forkBefore := reviewSourceSnapshot(repo), reviewSourceSnapshot(ws)
			var scratch string
			c := &Control{
				cfg: &config.Config{RepoOverride: repo, BaseImage: "gate-image", Gate: []string{"make", "check"}},
				rt:  runtime.Runtime{Name: "true"},
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
			out, code, err := reviewCommandOutput(t, c, "perf", "--gate", "--stat")
			if err != nil || code != tc.wantCode {
				t.Fatalf("ForkReview = (%d, %v), want (%d, nil)\n%s", code, err, tc.wantCode, out)
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

// The gated review rebases a temporary copy of the fork, whose HEAD is detached and whose commits get
// new ids that exist nowhere once the copy is gone. The dossier still names the branch the fork is
// going into, as the review without --gate does, and lists the fork's own commits, which a person can
// look up in the fork.
func TestForkReviewGateNamesTheBranchAndListsTheForksOwnCommits(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	for _, tc := range []struct {
		name       string
		green      bool
		unattended bool // no gate configured
		wantLine   string
	}{
		{name: "green", green: true, wantLine: "✓ Project checks passed"},
		{name: "red", green: false, wantLine: "They ran on a temporary copy rebased onto main."},
		{name: "none", unattended: true, wantLine: "The fork rebased cleanly onto main, which is not the same as passing checks."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, ws := setupReviewGateFork(t, false)
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			forkTip := forkspace.GitOut(ws, "rev-parse", "HEAD")
			rebasedTip := ""
			cfg := &config.Config{RepoOverride: repo, BaseImage: "gate-image"}
			if !tc.unattended {
				cfg.Gate = []string{"make", "check"}
			}
			c := &Control{cfg: cfg, rt: runtime.Runtime{Name: "true"}, gateOK: func(_, treeDir, _ string) bool {
				rebasedTip = forkspace.GitOut(treeDir, "rev-parse", "perf")
				return tc.green
			}}
			out, _, err := reviewCommandOutput(t, c, "perf", "--gate", "--stat")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, "  Into: main\n") || strings.Contains(out, "HEAD") || !strings.Contains(out, tc.wantLine) {
				t.Errorf("the dossier does not name the branch:\n%s", out)
			}
			if !strings.Contains(out, "  1 commit · 1 file · +1 −0\n") || !strings.Contains(out, "Commits\n  "+forkTip[:7]+" fork work\n") {
				t.Errorf("the dossier does not list the fork's own commit %s:\n%s", forkTip[:7], out)
			}
			if !tc.unattended {
				if rebasedTip == "" || rebasedTip == forkTip {
					t.Fatalf("the gate saw no rebased copy: rebased %q, fork %q", rebasedTip, forkTip)
				}
				if strings.Contains(out, rebasedTip[:7]) {
					t.Errorf("the dossier lists the copy's commit %s, which exists nowhere afterwards:\n%s", rebasedTip[:7], out)
				}
			}
			// The id is one a person can look up in the fork.
			if kind := forkspace.GitOut(ws, "cat-file", "-t", forkTip[:7]); kind != "commit" {
				t.Errorf("git cat-file -t %s in the fork = %q", forkTip[:7], kind)
			}
		})
	}
}

// Checks that exit 0 and still change the temporary copy did not check the fork's commits. The review
// fails them and names what they changed, instead of the plain failure of checks that ran and passed
// when run by hand. Checks that failed stay failed, whatever else they wrote.
func TestForkReviewGateNamesTheFilesTheChecksChanged(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, dir string)
		green bool
		want  []string
		not   []string
	}{
		{name: "a tracked file", green: true,
			write: func(t *testing.T, dir string) {
				writeTaskFile(t, filepath.Join(dir, "fork.txt"), "changed by the checks\n")
			},
			want: []string{"✗ Project checks changed files in fork perf", "and changed fork.txt.", "commit what they write"},
			not:  []string{"Project checks failed", "Project checks passed"}},
		{name: "a new file", green: true,
			write: func(t *testing.T, dir string) {
				writeTaskFile(t, filepath.Join(dir, "generated.txt"), "made by the checks\n")
			},
			want: []string{"✗ Project checks changed files in fork perf", "and changed generated.txt."},
			not:  []string{"Project checks failed"}},
		{name: "many files", green: true,
			write: func(t *testing.T, dir string) {
				for _, name := range []string{"a.txt", "b.txt", "c.txt", "d.txt", "e.txt"} {
					writeTaskFile(t, filepath.Join(dir, name), "x\n")
				}
			},
			want: []string{"and changed a.txt, b.txt, c.txt, and 2 more files."}},
		{name: "a failed gate stays failed", green: false,
			write: func(t *testing.T, dir string) {
				writeTaskFile(t, filepath.Join(dir, "fork.txt"), "changed by the checks\n")
			},
			want: []string{"✗ Project checks failed for fork perf"},
			not:  []string{"changed files"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, ws := setupReviewGateFork(t, false)
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			parentBefore, forkBefore := reviewSourceSnapshot(repo), reviewSourceSnapshot(ws)
			c := &Control{
				cfg: &config.Config{RepoOverride: repo, BaseImage: "gate-image", Gate: []string{"make", "check"}},
				rt:  runtime.Runtime{Name: "true"},
				gateOK: func(_, treeDir, _ string) bool {
					tc.write(t, treeDir)
					return tc.green
				},
			}
			out, code, err := reviewCommandOutput(t, c, "perf", "--gate", "--stat")
			if err != nil || code != 1 {
				t.Fatalf("ForkReview = (%d, %v), want (1, nil)\n%s", code, err, out)
			}
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("the dossier lacks %q:\n%s", want, out)
				}
			}
			for _, not := range tc.not {
				if strings.Contains(out, not) {
					t.Errorf("the dossier says %q:\n%s", not, out)
				}
			}
			assertReviewSourcesUnchanged(t, repo, ws, parentBefore, forkBefore)
			assertReviewScratchEmpty(t, tmp)
		})
	}
}

// The line a merge or a review prints for the checks is one a person can paste: each argument quoted
// so a shell reads the same command back.
func TestShellLineReadsBackAsTheSameArguments(t *testing.T) {
	for _, args := range [][]string{
		{"make", "check"},
		{"sh", "-c", "test -f f.txt"},
		{"sh", "-c", "echo x >> a.txt"},
		{"sh", "-c", "uname -s | grep -qx Linux"},
		{"go", "test", "-run", "^(A|B)$", "./..."},
		{"echo", "it's", "", `a "quoted" word`, "$HOME", "`id`", "a;b"},
	} {
		line := shellLine(args)
		got, err := exec.Command("sh", "-c", `for arg in `+line+`; do printf '<%s>' "$arg"; done`).Output()
		want := ""
		for _, arg := range args {
			want += "<" + arg + ">"
		}
		if err != nil || string(got) != want {
			t.Errorf("shellLine(%q) = %s reads back as %q, %v; want %q", args, line, got, err, want)
		}
	}
	if got := shellLine([]string{"sh", "-c", "test -f f.txt"}); got != "sh -c 'test -f f.txt'" {
		t.Errorf("shellLine = %s, want the quoted command", got)
	}
	if got := shellLine([]string{"echo", "a\x1bb"}); strings.Contains(got, "\x1b") {
		t.Errorf("shellLine let a control character through: %q", got)
	}
}

func TestForkReviewWithoutGateKeepsExistingPath(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, ws := setupReviewGateFork(t, false)
	forkBefore := reviewSourceSnapshot(ws)
	parentHead := forkspace.GitOut(repo, "rev-parse", "HEAD")
	badTmp := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(badTmp, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", badTmp) // a scratch clone would fail immediately
	c := &Control{
		cfg: &config.Config{RepoOverride: repo, Gate: []string{"make", "check"}},
		rt:  runtime.Runtime{Name: "definitely-not-a-runtime"},
		gateOK: func(_, _, _ string) bool {
			t.Fatal("flag-off review must not run the gate")
			return false
		},
	}
	out, code, err := reviewCommandOutput(t, c, "perf", "--stat")
	if err != nil || code != 0 {
		t.Fatalf("ForkReview = (%d, %v), want (0, nil)\n%s", code, err, out)
	}
	if !strings.Contains(out, "Project checks will run when you merge.") {
		t.Errorf("flag-off dossier changed:\n%s", out)
	}
	if got := forkspace.GitOut(repo, "rev-parse", "HEAD"); got != parentHead {
		t.Errorf("parent HEAD moved: %s -> %s", parentHead, got)
	}
	if got := forkspace.GitOut(repo, "rev-parse", "review/perf"); got != forkspace.GitOut(ws, "rev-parse", "perf") {
		t.Errorf("review ref = %q, want fork tip", got)
	}
	if got := reviewSourceSnapshot(ws); got != forkBefore {
		t.Errorf("fork changed on flag-off review:\nbefore:\n%s\nafter:\n%s", forkBefore, got)
	}
}

func TestForkReviewWarnsWhenUncommittedWorkIsOmitted(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Run("untracked work with no commits", func(t *testing.T) {
		repo := initRepo(t)
		ws, err := forkspace.Setup(repo, "perf")
		if err != nil {
			t.Fatal(err)
		}
		c := &Control{cfg: &config.Config{RepoOverride: repo}}
		clean, code, err := reviewCommandOutput(t, c, "perf", "--stat")
		if err != nil || code != 0 || strings.Contains(clean, "Uncommitted fork changes") {
			t.Fatalf("clean review = (%d, %v):\n%s", code, err, clean)
		}
		path := filepath.Join(ws, "new.txt")
		if err := os.WriteFile(path, []byte("kept\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		out, code, err := reviewCommandOutput(t, c, "perf", "--stat")
		if err != nil || code != 0 || !strings.Contains(out, "0 commits · 0 files") ||
			!strings.Contains(out, "Uncommitted fork changes") ||
			!strings.Contains(out, "omitted from this review's counts and committed diff") ||
			!strings.Contains(out, "coop fork open perf") {
			t.Fatalf("untracked review = (%d, %v):\n%s", code, err, out)
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != "kept\n" {
			t.Fatalf("review changed untracked work: %q, %v", got, err)
		}
	})
	t.Run("modified tracked work", func(t *testing.T) {
		repo, ws := setupReviewGateFork(t, false)
		c := &Control{cfg: &config.Config{RepoOverride: repo}}
		if err := os.WriteFile(filepath.Join(ws, "fork.txt"), []byte("revised\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		out, code, err := reviewCommandOutput(t, c, "perf", "--stat")
		if err != nil || code != 0 || !strings.Contains(out, "Uncommitted fork changes") ||
			!strings.Contains(out, "omitted from this review's counts and committed diff") {
			t.Fatalf("modified review = (%d, %v):\n%s", code, err, out)
		}
	})
}

func TestForkReviewGateRejectsOpen(t *testing.T) {
	c := &Control{cfg: &config.Config{}}
	if code, err := c.ForkReview([]string{"perf", "--gate", "--open"}); code != 2 || err == nil || !strings.Contains(err.Error(), `Cannot combine "--gate" and "--open"`) {
		t.Fatalf("ForkReview(--gate --open) = (%d, %v), want usage rejection", code, err)
	}
}

func TestForkReviewRejectsInvalidProjectBeforeDossierOrGitMutation(t *testing.T) {
	repo := initRepo(t)
	ws, err := forkspace.Setup(repo, "perf")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, project.File), []byte("gatez: make check\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", project.File)
	git(t, repo, "commit", "-qm", "invalid project policy")
	parentBefore, forkBefore := reviewSourceSnapshot(repo), reviewSourceSnapshot(ws)
	c := &Control{cfg: &config.Config{RepoOverride: repo}}
	var code int
	var runErr error
	out := captureStdout(t, func() { code, runErr = c.ForkReview([]string{"perf", "--stat"}) })
	if code != -1 || runErr == nil || !strings.Contains(runErr.Error(), project.File) {
		t.Fatalf("ForkReview = (%d, %v), want policy error", code, runErr)
	}
	if out != "" {
		t.Fatalf("invalid project emitted a partial dossier: %q", out)
	}
	if got := reviewSourceSnapshot(repo); got != parentBefore {
		t.Fatalf("parent changed before policy refusal:\n%s", got)
	}
	if got := reviewSourceSnapshot(ws); got != forkBefore {
		t.Fatalf("fork changed before policy refusal:\n%s", got)
	}
	if got := forkspace.GitOut(repo, "show-ref", "--verify", "refs/heads/review/perf"); got != "" {
		t.Fatalf("invalid project created review ref: %s", got)
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
	if got := forkspace.GitOut(repo, "show-ref", "--verify", "refs/heads/review/perf"); got != "" {
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

func TestForkReviewReturnsViewerFailure(t *testing.T) {
	for _, withGate := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "gated"}[withGate], func(t *testing.T) {
			repo, ws := setupReviewGateFork(t, false)
			marker := filepath.Join(t.TempDir(), "viewer-called")
			t.Setenv("COOP_TEST_VIEWER_CALLED", marker)
			git(t, repo, "config", "--global", "diff.tool", "quality-fixture")
			git(t, repo, "config", "--global", "difftool.quality-fixture.cmd", `printf called > "$COOP_TEST_VIEWER_CALLED"; exit 17`)
			git(t, repo, "config", "--global", "difftool.prompt", "false")
			git(t, repo, "config", "--global", "difftool.trustExitCode", "true")
			// Ordinary review publishes this ref; pre-create it so the snapshot isolates viewer effects.
			if err := gitFetchInto(repo, ws, "perf"); err != nil {
				t.Fatal(err)
			}
			beforeParent, beforeFork := reviewSourceSnapshot(repo), reviewSourceSnapshot(ws)
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			gateCalled := false
			c := &Control{cfg: &config.Config{RepoOverride: repo, BaseImage: "gate-image", Gate: []string{"make", "check"}}, rt: runtime.Runtime{Name: "true"}, gateOK: func(string, string, string) bool { gateCalled = true; return true }}
			args := []string{"perf", "--tool"}
			if withGate {
				args = append(args, "--gate")
			}
			out, code, err := reviewCommandOutput(t, c, args...)
			if code == 0 || err == nil {
				t.Fatalf("viewer failure: %d %v\n%s", code, err, out)
			}
			if raw, err := os.ReadFile(marker); err != nil || string(raw) != "called" {
				t.Fatalf("viewer not invoked: %q %v", raw, err)
			}
			if gateCalled != withGate {
				t.Fatalf("gate called: %v", gateCalled)
			}
			if reviewSourceSnapshot(repo) != beforeParent || reviewSourceSnapshot(ws) != beforeFork {
				t.Fatal("viewer failure changed source repositories")
			}
			assertReviewScratchEmpty(t, tmp)
		})
	}
}
