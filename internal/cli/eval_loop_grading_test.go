package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/eval"
)

// A shim line that does what a real loop does when its first box launches: create the start record
// coop eval named. A shim without it is a loop that never started.
const loopShimStarted = ": > \"$COOP_EVAL_LOOP_STARTED_FILE\"\n"

// A loop that could not start exits 1, the same code as a loop that ran and left work, so only the
// loop's start record can tell them apart. Both shims change the workspace, so the check for an
// attempt that changed nothing cannot decide for it.
func TestLoopTrialGradesOnlyALoopThatStarted(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	restore := loopExecutable
	t.Cleanup(func() { loopExecutable = restore })
	for _, tc := range []struct {
		name    string
		started bool
		want    eval.TrialStatus
	}{
		{"a loop that ran and left work is graded", true, eval.TrialFailed},
		{"a loop that could not start is a harness error", false, eval.TrialError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := "#!/bin/sh\nset -eu\nprintf 'package main\\nfunc main() { println(1) }\\n' > main.go\n"
			if tc.started {
				script += loopShimStarted
			}
			shim := filepath.Join(t.TempDir(), "coop-shim")
			if err := os.WriteFile(shim, []byte(script+"exit 1\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			loopExecutable = func() (string, error) { return shim, nil }
			suite := loopSuite(t)
			graded := false
			r := newTrialRunner(t, suite, func(spec box.RunSpec) (int, error) {
				graded = true
				return 1, nil // the verifier rejects the unfinished work
			})
			res := r.run(context.Background(), trialFor(suite))
			if graded != tc.started || res.Status != tc.want {
				t.Fatalf("graded=%v status=%s detail=%s; want graded=%v status=%s", graded, res.Status, res.Detail, tc.started, tc.want)
			}
			if !tc.started && !strings.Contains(res.Detail, "the loop exited 1 without launching a box") {
				t.Errorf("a loop that never started is not reported as such: %s", res.Detail)
			}
		})
	}
}

func TestLoopTrialGradesBlockedQueueWithoutCodeChanges(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	shim := filepath.Join(t.TempDir(), "coop-shim")
	const decision = "Choose the public flag spelling before implementation."
	script := "#!/bin/sh\nset -eu\n" + loopShimStarted +
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
		name    string
		isLoop  bool
		started bool
		code    int
	}{
		{"unchanged agent exit 3 is not a blocked loop", false, false, 3},
		{"unchanged loop failure", true, true, 1},
		{"custom loop command exits 3 without blocking", true, true, 3},
		{"loop startup refusal", true, false, 2},
		{"interrupted loop", true, true, 130},
	} {
		t.Run(tc.name, func(t *testing.T) {
			suite := trialSuite(t)
			if tc.isLoop {
				suite = loopSuite(t)
				script := "#!/bin/sh\n"
				if tc.started {
					script += loopShimStarted
				}
				shim := filepath.Join(t.TempDir(), "coop-shim")
				if err := os.WriteFile(shim, []byte(fmt.Sprintf("%sexit %d\n", script, tc.code)), 0o755); err != nil {
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
			if err := os.WriteFile(shim, []byte("#!/bin/sh\nset -eu\n"+loopShimStarted+tc.script+"exit 3\n"), 0o755); err != nil {
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
