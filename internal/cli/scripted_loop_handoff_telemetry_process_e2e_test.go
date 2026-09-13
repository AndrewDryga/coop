//go:build providere2e

package cli

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/loop"
)

func TestProviderScriptedReviewHandoffTelemetry(t *testing.T) {
	suite := newDirectProcessSuite(t)
	for _, stage := range []string{"between", "signoff", "verify"} {
		for _, terminal := range []string{"background-drained-review", "background-timeout-review", "pass"} {
			t.Run(stage+"/"+terminal, func(t *testing.T) {
				resetLoopProcessRepo(t, suite)
				t.Cleanup(func() { logLoopProcessFailure(t, suite) })
				taskID := "review-handoff-telemetry"
				seedLoopProcessTask(t, suite.layout.Repo, taskID)
				work := loopRecoveryTarget("claude", "work-model", "personal")
				reviewer := loopRecoveryTarget("claude", "review-model", "work")
				var between, verify []string
				if stage == "between" {
					between = []string{reviewer}
				}
				if stage == "verify" {
					verify = []string{reviewer}
				}
				writeLoopReviewConfig(t, suite.layout.Repo, between, []string{reviewer}, verify, 3)
				loopProcessGit(t, suite, "add", ".agent/loop.yaml")
				loopProcessGit(t, suite, "commit", "-q", "-m", "fixture: configure review telemetry")
				attempts := []loopProcessAttempt{{Target: work, Stage: "work", Result: "complete"}}
				if stage == "verify" {
					attempts = append(attempts, loopProcessAttempt{Target: reviewer, Stage: "signoff", Result: "pass"})
				}
				for _, result := range []string{"background-drained-review", "background-timeout-review", terminal} {
					attempts = append(attempts, loopProcessAttempt{Target: reviewer, Stage: stage, Result: result})
				}
				if stage == "between" {
					// Ordinary between-audit failures warn and still allow final review.
					attempts = append(attempts, loopProcessAttempt{Target: reviewer, Stage: "signoff", Result: "pass"})
				}
				suite.reset(t, loopRecoveryScenario(taskID, attempts))
				result := runLoopReview(t, suite, work, 30*time.Second)
				if result.Err != nil {
					t.Fatalf("review process did not finish: %v", result.Err)
				}
				output := result.Stdout + result.Stderr
				if terminal == "pass" {
					if result.ExitCode != 0 || !strings.Contains(output, "All tasks passed final review") {
						t.Fatalf("recovered review did not pass: exit %d", result.ExitCode)
					}
				} else {
					if !strings.Contains(output, "review provider ended with live background work 3 times") {
						t.Fatal("terminal handoff did not retain its failure diagnostic")
					}
					if stage == "signoff" && result.ExitCode != 1 {
						t.Fatalf("failed final review exit = %d, want 1", result.ExitCode)
					}
				}
				trace := readProcessTrace(t, suite.layout.Trace)
				assertLoopReviewContracts(t, suite, trace, taskID, attempts)
				assertLoopTraceProcessesGone(t, trace)
				records := readLoopStageRecords(t, suite)
				cost, summary := loop.WorkspaceCost(suite.layout.Repo)
				wantCost := float64(len(attempts)) * 0.25
				wantSummary := fmt.Sprintf("$%.2f · %d in / %d out", wantCost, len(attempts)*101, len(attempts)*11)
				t.Logf("provider attempts=%d stage rows=%d cost=$%.2f expected=$%.2f exit=%d final-pass=%t", len(attempts), len(records), cost, wantCost, result.ExitCode, strings.Contains(output, "All tasks passed final review"))
				if cost != wantCost || !strings.HasPrefix(summary, wantSummary+" · ") {
					t.Errorf("workspace usage = %q ($%.2f), want prefix %q", summary, cost, wantSummary)
				}
				if len(records) != len(attempts) {
					t.Fatalf("stage rows = %d, want one per %d provider attempts", len(records), len(attempts))
				}
				var retry int
				for i, attempt := range attempts {
					outcome, exit := "success", 0
					switch attempt.Result {
					case "background-drained-review":
						outcome, exit = "background_drained", 190
					case "background-timeout-review":
						outcome, exit = "background_timeout", 191
					}
					wantRetry := 0
					if attempt.Stage == stage {
						wantRetry = retry
						retry++
					}
					record := records[i]
					model, account := "review-model", "work"
					if attempt.Stage == "work" {
						model, account = "work-model", "personal"
					}
					if record.Stage != attempt.Stage || record.Outcome != outcome || record.Exit != exit || record.Retries != wantRetry ||
						record.Provider != "claude" || record.Model != model || record.Account != account ||
						record.Reopened != 0 || record.CostUSD != 0.25 || record.InTok != 101 || record.OutTok != 11 {
						t.Fatalf("stage row %d = %#v; expected %s/%s exit %d retry %d with exact fixture usage", i, record, attempt.Stage, outcome, exit, wantRetry)
					}
					if i > 0 && (record.HeadBefore != records[0].HeadAfter || record.HeadAfter != records[0].HeadAfter || len(record.Finished) != 0) {
						t.Fatalf("review row %d changed completed work or claimed completion", i)
					}
				}
				if !pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateDone, taskID)) || loopProcessGit(t, suite, "status", "--porcelain") != "" {
					t.Fatal("review recovery mutated the completed task or worktree")
				}
				for _, state := range []string{stateTodo, stateInProgress, stateBlocked} {
					if pathExists(filepath.Join(suite.layout.Repo, tasksRoot, state, taskID)) {
						t.Fatalf("review left a duplicate task in %s", state)
					}
				}
				if head := loopProcessGit(t, suite, "rev-parse", "HEAD"); head != records[0].HeadAfter {
					t.Fatalf("review moved HEAD from completed work %s to %s", records[0].HeadAfter, head)
				}
			})
		}
	}
}
