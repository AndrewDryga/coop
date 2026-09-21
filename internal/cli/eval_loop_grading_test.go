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
