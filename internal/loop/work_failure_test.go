package loop

import (
	"errors"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/ladder"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
	"github.com/AndrewDryga/coop/internal/ui"
)

func TestCustomWorkerFailuresKeepLoopExitContract(t *testing.T) {
	// os/signal owns a permanent receiver. Start it outside virtual time so only
	// this run's bounded retry waits live in the synctest bubble.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)
	signal.Stop(signals)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("TERM", "dumb")
	for _, tc := range []struct {
		name, diagnostic, outcome string
		exit, attempts            int
	}{
		{"make failure is not usage", "", "process_failure", 2, maxLoopFailures},
		{"worker failure is not blocked", "", "process_failure", 3, maxLoopFailures},
		{"authentication failure is not usage", "Not logged in · Please run /login", "authentication", 2, 1},
		{"output limit is not blocked", "finish_reason: length", "output_limit", 3, maxOutputRetries + 1},
		{"rate limit is not usage", "rate limit exceeded", "rate_limit", 2, maxLimitWaits + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			repo, git := gitrepo.New(t)
			writeTaskFile(t, filepath.Join(repo, ".gitignore"), ".agent/\n")
			git("add", ".gitignore")
			git("commit", "-m", "base")
			const id = "worker-failure"
			writeTaskFile(t, filepath.Join(repo, tasksRoot, stateTodo, id, "task.md"), "# Worker failure\n- [ ] Implement and verify the result\n")
			writeTaskFile(t, filepath.Join(repo, ".agent", "loop.yaml"), "mcp: false\nwork:\n  command: [make, loop-iter]\n")
			c := New(&config.Config{RepoOverride: repo, ConfigDir: t.TempDir()}, runtime.Runtime{Name: "true"}, "test", Host{})
			attempts := 0
			var code int
			var runErr error
			synctest.Test(t, func(t *testing.T) {
				c.boxRun = func(spec box.RunSpec) (int, error) {
					attempts++
					if attempts > tc.attempts || spec.AgentCommand || strings.Join(spec.Cmd, " ") != "make loop-iter" {
						t.Fatalf("unexpected attempt %d: agent command=%v argv=%q", attempts, spec.AgentCommand, spec.Cmd)
					}
					writeTaskFile(t, filepath.Join(repo, "result.txt"), "incomplete work\n")
					if tc.diagnostic != "" {
						if _, err := io.WriteString(spec.Stderr, tc.diagnostic+"\n"); err != nil {
							t.Fatal(err)
						}
					}
					return tc.exit, nil
				}
				code, runErr = c.Run(RunSpec{
					Repo: repo, Image: "fixture", Agent: "claude", Queues: []string{tasksRoot}, Sink: io.Discard,
					Rotation: ladder.NewRotation([]agents.Target{{Provider: "claude"}}),
				})
			})
			if code != 1 || runErr == nil || attempts != tc.attempts {
				t.Errorf("failed worker: exit=%d error=%v attempts=%d; want loop failure 1 after %d attempts", code, runErr, attempts, tc.attempts)
			}
			current, ok, err := tasks.CurrentTask(filepath.Join(repo, tasksRoot), id)
			if err != nil || !ok || current.State != stateInProgress {
				t.Fatalf("failure lost actionable task: %+v, %v, %v", current, ok, err)
			}
			if body, err := os.ReadFile(filepath.Join(repo, "result.txt")); err != nil || string(body) != "incomplete work\n" {
				t.Fatalf("failure lost candidate work: %q, %v", body, err)
			}
			records := readStageRecords(repo, c.runID)
			if len(records) != tc.attempts {
				t.Fatalf("stage records=%d, want %d", len(records), tc.attempts)
			}
			for _, record := range records {
				if record.Stage != "work" || record.Outcome != tc.outcome || record.Exit != tc.exit || record.QueueDoing != 1 {
					t.Errorf("raw attempt evidence changed: %+v", record)
				}
			}
		})
	}
}

// The loop's own recovery limits stop the run with exit 1, whatever the last attempt's status was:
// a background handoff exits 190 or 191 inside the box, a watchdog kill has no status of its own,
// and a stalled attempt exits 0. Each status stays in its attempt's stage record.
func TestRecoveryLimitsKeepLoopExitContract(t *testing.T) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)
	signal.Stop(signals)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("TERM", "dumb")
	for _, tc := range []struct {
		name, outcome, headline string
		exit, attempts          int
	}{
		{"drained background work", "background_drained", "Stopped after 3 live-background handoffs", box.DescendantsDrainedExit, maxBackgroundHandoffs},
		{"timed-out background work", "background_timeout", "Stopped after 3 live-background handoffs", box.DescendantsTimedOutExit, maxBackgroundHandoffs},
		{"provider timeouts", outcomeStartTimeout, "Stopped after 3 provider timeouts", -1, maxProviderTimeouts},
		{"no progress", "success", "The loop stopped making progress", 0, maxStalls},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			t.Setenv(tasks.TestLeaseAuthorityRootEnv, t.TempDir())
			repo, git := gitrepo.New(t)
			writeTaskFile(t, filepath.Join(repo, ".gitignore"), ".agent/\n")
			git("add", ".gitignore")
			git("commit", "-m", "base")
			const id = "recovery-limit"
			root := filepath.Join(repo, tasksRoot)
			state := stateTodo
			if tc.outcome == "success" {
				// Only rework of a task a review reopened can exit 0 and leave it in progress: any
				// other task gets a terminal correction instead.
				git("commit", "--allow-empty", "-m", "Implement the task\n\nCoop-Task: "+id)
				state = stateInProgress
			}
			writeTaskFile(t, filepath.Join(root, state, id, "task.md"), "# Recovery limit\n- [ ] Implement and verify the result\n")
			if tc.outcome == "success" {
				record, err := tasks.CaptureAuditReopen(repo, id)
				if err != nil {
					t.Fatal(err)
				}
				if err := tasks.WriteAuditReopenRecord(root, record); err != nil {
					t.Fatal(err)
				}
			}
			// The watchdog supervises only a built-in agent's stream; the other limits need no agent.
			if tc.outcome != outcomeStartTimeout {
				writeTaskFile(t, filepath.Join(repo, ".agent", "loop.yaml"), "mcp: false\nwork:\n  command: [make, loop-iter]\n")
			}
			// One second of start silence instead of ten minutes: virtual time is free, but the task
			// lease writes a heartbeat every ten seconds of it.
			cfg := &config.Config{RepoOverride: repo, ConfigDir: t.TempDir(), ProviderTimeouts: "start=1s"}
			c := New(cfg, runtime.Runtime{Name: "true"}, "test", Host{})
			attempts := 0
			var code int
			var runErr error
			// Captured outside the bubble: the capture's pipe reader would stop virtual time.
			output := captureStderr(t, func() {
				synctest.Test(t, func(t *testing.T) {
					c.boxRun = func(spec box.RunSpec) (int, error) {
						if attempts++; attempts > tc.attempts {
							t.Fatalf("unexpected attempt %d", attempts)
						}
						if tc.outcome != outcomeStartTimeout {
							return tc.exit, nil
						}
						// A silent provider: its start deadline passes in virtual time, the watchdog
						// cancels the box, and the box returns as runtime.RunInterruptible does.
						if spec.OnRuntimeLaunch == nil {
							t.Fatal("the agent's attempt runs without a watchdog")
						}
						spec.OnRuntimeLaunch()
						<-spec.Ctx.Done()
						return -1, spec.Ctx.Err()
					}
					code, runErr = c.Run(RunSpec{
						Repo: repo, Image: "fixture", Agent: "claude", Queues: []string{tasksRoot}, Sink: io.Discard,
						Rotation: ladder.NewRotation([]agents.Target{{Provider: "claude"}}),
					})
				})
			})
			if code != 1 || !errors.Is(runErr, ui.ErrReported) || attempts != tc.attempts || !strings.Contains(output, tc.headline) {
				t.Fatalf("run = (%d, %v) after %d attempts, want exit 1 and %q after %d\n%s", code, runErr, attempts, tc.headline, tc.attempts, output)
			}
			if current, ok, err := tasks.CurrentTask(root, id); err != nil || !ok || current.State != stateInProgress {
				t.Fatalf("the stop lost the actionable task: %+v, %v, %v", current, ok, err)
			}
			records := readStageRecords(repo, c.runID)
			if len(records) != tc.attempts {
				t.Fatalf("stage records = %d, want %d", len(records), tc.attempts)
			}
			for _, record := range records {
				if record.Stage != "work" || record.Outcome != tc.outcome || record.Exit != tc.exit {
					t.Errorf("the attempt's own status is lost: %+v", record)
				}
			}
		})
	}
}
