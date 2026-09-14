package loop

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoopShellGuidance(t *testing.T) {
	for _, audit := range []bool{false, true} {
		prompt := LoopWorkPrompt("/repo", ".agent/tasks", "task", "claude", nil, nil, audit)
		for _, want := range []string{"Run the final gate in the foreground", "independent work to do", "wait on that job"} {
			if !strings.Contains(prompt, want) {
				t.Errorf("missing foreground-first guidance %q", want)
			}
		}
		for _, want := range []string{loopCheckScript, "absolute check cwd", "absolute task tmp", "supported completion tools", "not long blind sleeps", "exact verified owned process/resource identities", "never broad name-pattern kills", "verify the resource is stopped/absent", "report cleanup failure", "unique, create-only scratch database names", "never drop a pre-existing unknown database", "normal human interactive output keeps its progress UI"} {
			if !strings.Contains(prompt, want) {
				t.Errorf("missing shell guidance %q", want)
			}
		}
	}
}

// Execute exactly the emitted script, not a separately transcribed shell recipe.
func TestLoopCheckScript(t *testing.T) {
	t.Run("missing command", func(t *testing.T) {
		root := t.TempDir()
		cmd := exec.Command("sh", "-c", loopCheckScript, "sh", root, filepath.Join(root, "scratch"))
		out, err := cmd.CombinedOutput()
		if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 2 {
			t.Fatalf("missing check accepted: %v, %s", err, out)
		}
	})
	for _, tc := range []struct {
		name  string
		code  int
		setup string
	}{
		{name: "success"}, {name: "failed producer", code: 7},
		{name: "missing cwd", setup: "cwd"}, {name: "non-directory scratch", setup: "scratch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cwd := t.TempDir()
			scratch := filepath.Join(t.TempDir(), "new scratch", "nested")
			marker := filepath.Join(t.TempDir(), "producer-started")
			if tc.setup == "cwd" {
				cwd = filepath.Join(cwd, "missing")
			}
			if tc.setup == "scratch" {
				scratch = filepath.Join(t.TempDir(), "file")
				writeTaskFile(t, scratch, "not a directory\n")
			}
			producer := `touch "$1"; pwd; printf '<%s>\n' "$2"; i=0; while [ "$i" -lt 60 ]; do echo line; i=$((i+1)); done; exit "$3"`
			cmd := exec.Command("sh", "-c", loopCheckScript, "sh", cwd, scratch, "sh", "-c", producer, "sh", marker, "space and $literal ; text", fmt.Sprint(tc.code))
			out, err := cmd.CombinedOutput()
			code := 0
			if err != nil {
				exit, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatal(err)
				}
				code = exit.ExitCode()
			}
			if tc.setup != "" {
				if _, err := os.Stat(marker); !os.IsNotExist(err) || code == 0 {
					t.Fatalf("setup refusal ran producer or succeeded: code %d, marker %v, %s", code, err, out)
				}
				return
			}
			if code != tc.code {
				t.Fatalf("tail replaced producer status: got %d want %d: %s", code, tc.code, out)
			}
			logs, err := filepath.Glob(filepath.Join(scratch, "check.*"))
			if err != nil || len(logs) != 1 {
				t.Fatalf("missing unique log: %v, %v", logs, err)
			}
			log, err := os.ReadFile(logs[0])
			if err != nil || !strings.Contains(string(log), "<space and $literal ; text>\n") || !strings.Contains(string(log), cwd+"\n") || strings.Count(string(log), "line\n") != 60 {
				t.Fatalf("lost cwd/argv/full log: %q, %v", log, err)
			}
			if strings.Count(string(out), "line\n") != 40 || !strings.Contains(string(out), "Check log: "+logs[0]) {
				t.Fatalf("display not bounded or missing receipt: %s", out)
			}
			// A second check in the same scratch directory cannot replace this log.
			cmd = exec.Command("sh", "-c", loopCheckScript, "sh", cwd, scratch, "true")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("second check: %v, %s", err, out)
			}
			after, err := os.ReadFile(logs[0])
			if err != nil || string(after) != string(log) {
				t.Fatalf("second check overwrote evidence: %v", err)
			}
		})
	}
}

// A synthetic resource command validates the cleanup recipe, not real DB/process
// enforcement or model compliance. In particular, unknown ownership never deletes.
func TestLoopOwnedCleanupRecipe(t *testing.T) {
	for _, scenario := range []string{"success", "failure", "unknown owner"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			owned, foreign := filepath.Join(root, "scratch-owned"), filepath.Join(root, "scratch-owned-backup")
			writeTaskFile(t, owned, "owned data\n")
			writeTaskFile(t, foreign, "foreign data\n")
			// This stand-in represents a project's exact-ID lifecycle command.
			cleanup := `test "$1" = "$2" || exit 17; test "$3" != failure || exit 19; rm -- "$1" || exit; test ! -e "$1"`
			target := owned
			if scenario == "unknown owner" {
				target = foreign
			}
			cmd := exec.Command("sh", "-c", loopCheckScript, "sh", root, filepath.Join(root, "logs"), "sh", "-c", cleanup, "sh", target, owned, scenario)
			out, err := cmd.CombinedOutput()
			wantCode := map[string]int{"success": 0, "failure": 19, "unknown owner": 17}[scenario]
			if cmd.ProcessState == nil {
				t.Fatalf("cleanup did not start: %v, %s", err, out)
			}
			code := cmd.ProcessState.ExitCode()
			if code != wantCode {
				t.Fatalf("cleanup result %d, want %d: %v, %s", code, wantCode, err, out)
			}
			body, err := os.ReadFile(foreign)
			if err != nil || string(body) != "foreign data\n" {
				t.Fatalf("foreign resource changed: %q, %v", body, err)
			}
			_, err = os.Stat(owned)
			if (scenario == "success" && !os.IsNotExist(err)) || (scenario != "success" && err != nil) {
				t.Fatalf("wrong owned-resource postcondition: %v", err)
			}
		})
	}
}
