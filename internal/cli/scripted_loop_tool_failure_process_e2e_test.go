//go:build providere2e

package cli

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

func TestProviderScriptedClaudeToolFailure(t *testing.T) {
	suite := newDirectProcessSuite(t)
	for _, tc := range []struct {
		name, term string
		width      int
	}{
		{"redirected", "", 0},
		{"static terminal", "dumb", 38},
		{"narrow human terminal", "xterm-256color", 38},
		{"human terminal", "xterm-256color", 80},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetLoopProcessRepo(t, suite)
			t.Cleanup(func() { logLoopProcessFailure(t, suite) })
			id := "tool-failure"
			seedLoopProcessTask(t, suite.layout.Repo, id)
			target := loopRecoveryTarget("claude", "work-model", "personal")
			attempts := []loopProcessAttempt{{Target: target, Stage: "work", Result: "complete-tool-failure"}}
			suite.reset(t, loopRecoveryScenario(id, attempts))
			command := loopRecoveryCommand(suite, target)
			command.Env = replaceProcessEnv(command.Env, "NO_COLOR", "1")
			command.Env = replaceProcessEnv(command.Env, "COOP_SPINNER", "0")
			command.Env = replaceProcessEnv(command.Env, "COOP_STREAM_TRACE", "1")
			if tc.term != "" {
				command.Env = replaceProcessEnv(command.Env, "TERM", tc.term)
				sh, err := exec.LookPath("sh")
				if err != nil {
					t.Fatal(err)
				}
				command.Args = append([]string{"-c", fmt.Sprintf(`stty cols %d rows 40 && exec "$@"`, tc.width), "tool-failure-terminal", command.Path}, command.Args...)
				command.Path = sh
				command = terminalLoopCommand(t, command)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			result := procharness.Run(ctx, command)
			cancel()
			if result.Err != nil || result.ExitCode != 0 {
				t.Fatalf("tool error prevented completed work: %+v", result)
			}
			output := result.Stdout + result.Stderr
			var failure string
			for _, line := range strings.Split(output, "\n") {
				if i := strings.Index(line, "✗ Bash"); i >= 0 {
					failure = strings.TrimSpace(line[i:])
				}
			}
			t.Logf("failure: %s", failure)
			if !strings.Contains(failure, "cannot open") || strings.Contains(failure, "Exit code 1") {
				t.Fatalf("available tool cause was hidden: %q", failure)
			}
			if tc.term == "xterm-256color" {
				if strings.Contains(failure, "command-tail") || !strings.Contains(failure, "…") || !strings.Contains(output, "\x1b[K") || len([]rune(failure)) > tc.width-1 {
					t.Fatalf("human terminal lost its compact failure or live UI: %q", failure)
				}
			} else if !strings.Contains(failure, "command-tail") || strings.Contains(output, "\x1b[K") {
				t.Fatalf("static output was capped or repainted: %q", failure)
			}
			trace := readProcessTrace(t, suite.layout.Trace)
			assertLoopAttemptContracts(t, suite, trace, id, attempts)
			assertLoopTraceProcessesGone(t, trace)
			records := readLoopStageRecords(t, suite)
			if len(records) != 1 || records[0].Outcome != "success" || !pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateDone, id)) {
				t.Fatalf("tool error changed provider outcome or completion: %#v", records)
			}
			traces, err := filepath.Glob(filepath.Join(suite.layout.Repo, ".agent", "runs", "*.streams", "01-claude.jsonl"))
			if err != nil || len(traces) != 1 {
				t.Fatalf("raw stream traces = %v, %v", traces, err)
			}
			raw := readProcessFile(t, traces[0])
			if !strings.Contains(raw, `Exit code 1\ntail: cannot open`) || !strings.Contains(raw, "command-tail") || !strings.Contains(raw, "No such file or directory") {
				t.Fatal("raw native evidence was shortened or rewritten")
			}
		})
	}
}
