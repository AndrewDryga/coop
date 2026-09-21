package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/eval"
)

func TestLoopTrialGradesBlockedQueueWithoutCodeChanges(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	shim := filepath.Join(t.TempDir(), "coop-shim")
	const decision = "Choose the public flag spelling before implementation."
	script := "#!/bin/sh\nset -eu\n" +
		"mkdir -p .agent/tasks/50_blocked\n" +
		"mv .agent/tasks/00_todo/add-a-flag .agent/tasks/50_blocked/\n" +
		"printf '%s\\n' '" + decision + "' > .agent/tasks/50_blocked/add-a-flag/decision.md\n" +
		"exit 3\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	restore := loopExecutable
	loopExecutable = func() (string, error) { return shim, nil }
	t.Cleanup(func() { loopExecutable = restore })

	for _, tc := range []struct {
		name      string
		gradeCode int
		want      eval.TrialStatus
	}{
		{"verifier accepts the result", 0, eval.TrialPassed},
		{"verifier rejects the result", 1, eval.TrialFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			suite := loopSuite(t)
			graded := false
			r := newTrialRunner(t, suite, func(spec box.RunSpec) (int, error) {
				graded = true
				for path, want := range map[string]string{
					"main.go": "package main\n\nfunc main() {}\n",
					".agent/tasks/50_blocked/add-a-flag/task.md":     "# Add a flag\n\nAdd --verbose.\n",
					".agent/tasks/50_blocked/add-a-flag/decision.md": decision + "\n",
				} {
					body, err := os.ReadFile(filepath.Join(spec.Repo, path))
					if err != nil || string(body) != want {
						t.Errorf("grader snapshot %s = %q, %v; want %q", path, body, err, want)
					}
				}
				if _, err := os.Stat(filepath.Join(spec.Repo, ".agent/tasks/00_todo/add-a-flag")); !os.IsNotExist(err) {
					t.Errorf("grader received the original todo task: %v", err)
				}
				if !spec.GradeSnapshot || spec.Homes || spec.Cache || spec.Agent != "" {
					t.Error("blocked outcome did not use the independent credential-free grader")
				}
				return tc.gradeCode, nil
			})
			res := r.run(context.Background(), trialFor(suite))
			if !graded || res.Status != tc.want {
				t.Fatalf("graded=%v status=%s detail=%s; want %s", graded, res.Status, res.Detail, tc.want)
			}
		})
	}
}

func TestTrialRunnerKeepsIncompleteAttemptsUngraded(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	restore := loopExecutable
	t.Cleanup(func() { loopExecutable = restore })
	for _, tc := range []struct {
		name   string
		isLoop bool
		code   int
	}{
		{"unchanged agent exit 3 is not a blocked loop", false, 3},
		{"unchanged loop failure", true, 1},
		{"custom loop command exits 3 without blocking", true, 3},
		{"loop startup refusal", true, 2},
		{"interrupted loop", true, 130},
	} {
		t.Run(tc.name, func(t *testing.T) {
			suite := trialSuite(t)
			if tc.isLoop {
				suite = loopSuite(t)
				shim := filepath.Join(t.TempDir(), "coop-shim")
				if err := os.WriteFile(shim, []byte(fmt.Sprintf("#!/bin/sh\nexit %d\n", tc.code)), 0o755); err != nil {
					t.Fatal(err)
				}
				loopExecutable = func() (string, error) { return shim, nil }
			}
			graded := false
			r := newTrialRunner(t, suite, func(spec box.RunSpec) (int, error) {
				if spec.Agent != "" {
					return tc.code, nil
				}
				graded = true
				return 0, nil
			})
			res := r.run(context.Background(), trialFor(suite))
			if graded || res.Status != eval.TrialError {
				t.Fatalf("graded=%v status=%s detail=%s; want ungraded error", graded, res.Status, res.Detail)
			}
		})
	}
}

func TestLoopTrialExitThreeNeedsBlockedOutcome(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	restore := loopExecutable
	t.Cleanup(func() { loopExecutable = restore })
	const blockAnother = "mkdir -p .agent/tasks/50_blocked/another\n" +
		"printf '# Another task\\n' > .agent/tasks/50_blocked/another/task.md\n"
	for _, tc := range []struct {
		name   string
		script string
		graded bool
	}{
		{"blocked plus todo", blockAnother, false},
		{"blocked plus in progress", blockAnother + "mkdir -p .agent/tasks/10_in_progress\nmv .agent/tasks/00_todo/add-a-flag .agent/tasks/10_in_progress/\n", false},
		{"missing queue", "mv .agent/tasks .agent/runs\n", false},
		{"empty queue", "mv .agent/tasks .agent/runs\nmkdir .agent/tasks\n", false},
		{"done only", "mkdir -p .agent/tasks/99_done\nmv .agent/tasks/00_todo/add-a-flag .agent/tasks/99_done/\n", false},
		{"unsafe blocked metadata", "mkdir -p .agent/tasks/50_blocked\nmv .agent/tasks/00_todo/add-a-flag .agent/tasks/50_blocked/\nmkdir .agent/tasks/50_blocked/add-a-flag/decision.md\n", false},
		{"source edits remain gradeable", "printf 'package main\\nfunc main() { println(1) }\\n' > main.go\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shim := filepath.Join(t.TempDir(), "coop-shim")
			if err := os.WriteFile(shim, []byte("#!/bin/sh\nset -eu\n"+tc.script+"exit 3\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			loopExecutable = func() (string, error) { return shim, nil }
			suite := loopSuite(t)
			graded := false
			r := newTrialRunner(t, suite, func(spec box.RunSpec) (int, error) {
				graded = true
				return 1, nil // admission to grading is not a passing verdict
			})
			res := r.run(context.Background(), trialFor(suite))
			want := eval.TrialError
			if tc.graded {
				want = eval.TrialFailed
			}
			if graded != tc.graded || res.Status != want {
				t.Fatalf("graded=%v status=%s detail=%s; want graded=%v status=%s", graded, res.Status, res.Detail, tc.graded, want)
			}
		})
	}
}

func TestLoopQueueBlockedRejectsLinkedPaths(t *testing.T) {
	for _, linked := range []string{
		".agent", ".agent/tasks", ".agent/tasks/50_blocked", ".agent/tasks/50_blocked/choice",
	} {
		t.Run(linked, func(t *testing.T) {
			workspace := t.TempDir()
			taskDir := filepath.Join(workspace, ".agent/tasks/50_blocked/choice")
			if err := os.MkdirAll(taskDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(taskDir, "task.md"), []byte("# Choose an option\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if !loopQueueBlocked(workspace) {
				t.Fatal("fixture was not a blocked-only queue before linking")
			}
			path := filepath.Join(workspace, linked)
			external := filepath.Join(t.TempDir(), "outside-workspace")
			if err := os.Rename(path, external); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(external, path); err != nil {
				t.Fatal(err)
			}
			if loopQueueBlocked(workspace) {
				t.Fatal("external queue content was accepted through a candidate-controlled link")
			}
		})
	}
}
