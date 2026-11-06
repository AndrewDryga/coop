package loop

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/ladder"
	"github.com/AndrewDryga/coop/internal/loopcfg"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

func TestReviewRepeatedFindingAfterReclose(t *testing.T) {
	for _, tc := range []struct {
		name, second    string
		repair, sibling bool
		want            string
	}{
		{"same finding", "Missing desktop and mobile evidence", false, false, tasks.StateBlocked},
		{"normalized finding", "  MISSING desktop   and mobile evidence. ", false, false, tasks.StateBlocked},
		{"changed finding", "Missing keyboard navigation evidence", false, false, tasks.StateInProgress},
		{"real repair", "Missing desktop and mobile evidence", true, false, tasks.StateInProgress},
		{"interrupted repair", "Missing desktop and mobile evidence", true, false, tasks.StateInProgress},
		{"sibling commit", "Missing desktop and mobile evidence", false, true, tasks.StateBlocked},
		{"different long finding", strings.Repeat("Missing evidence ", 20) + "mobile", false, false, tasks.StateInProgress},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, git := gitrepo.New(t)
			writeTaskFile(t, filepath.Join(repo, ".gitignore"), ".agent/\n")
			git("add", ".gitignore")
			git("commit", "-qm", "base", "--allow-empty")
			base := forkspace.GitOut(repo, "rev-parse", "HEAD")
			root := filepath.Join(repo, tasks.TasksRoot)
			taskForLease(t, root, tasks.StateTodo, "subject")
			assign := func() tasks.TaskAssignment {
				a, err := tasks.AssignLoopTaskOnly([]string{root}, tasks.TaskLeaseOwner{RunID: "repeat-test", PID: os.Getpid()}, "subject")
				if err != nil || a.Lease == nil {
					t.Fatalf("assign: %v", err)
				}
				a.Lease.Quiesce()
				t.Cleanup(func() { _ = a.Lease.Release() })
				return a
			}
			a := assign()
			git("commit", "-qm", "implement\n\nCoop-Task: subject", "--allow-empty")
			plan, err := tasks.NewPendingReviewPlan(repo, []string{root}, strings.TrimSpace(base), "test", "coop loop", tasks.PendingReviewStage{Targets: []string{"codex:test"}}, 5, false, tasks.PendingReviewStage{}, false)
			if err != nil {
				t.Fatal(err)
			}
			closeTask := func(a tasks.TaskAssignment) {
				writeTaskFile(t, filepath.Join(a.Task.Item.Dir, "task.md"), "# subject\n\n## Subtasks\n\n- [x] done\n")
				if err := tasks.WriteTaskState(a.Task.Item.Dir, "subject", tasks.TaskStateFields{Status: "complete", DoneSoFar: "Finding is false: no markup changed; I ran nothing this pass.", NextAction: "none", Traps: "—"}); err != nil {
					t.Fatal(err)
				}
				if err := tasks.MoveTaskDir(root, a.Task.Item, tasks.StateDone); err != nil {
					t.Fatal(err)
				}
				done, _, err := tasks.CurrentTask(root, "subject")
				if err != nil {
					t.Fatal(err)
				}
				if err := tasks.FinalizeQueuedCompletion(tasks.QueuedTask{Root: root, Item: done}); err != nil {
					t.Fatal(err)
				}
				if err := a.Lease.MarkCompletedForReview(repo, done, plan, !tc.repair || tc.name == "interrupted repair"); err != nil {
					t.Fatal(err)
				}
				if err := a.Lease.ConsumeAuditReopen(); err != nil {
					t.Fatal(err)
				}
				if err := a.Lease.Release(); err != nil {
					t.Fatal(err)
				}
			}
			closeTask(a)
			first := "Missing desktop and mobile evidence"
			if tc.name == "different long finding" {
				first = strings.Repeat("Missing evidence ", 20) + "desktop"
			}
			c := New(&config.Config{RepoOverride: repo, ConfigDir: t.TempDir(), Homes: true}, runtime.Runtime{Name: "true"}, "test", Host{})
			c.net, c.runID = newNetworkLog(), "dispute-test"
			verdict := func(finding string) reviewRunResult {
				c.boxRun = func(spec box.RunSpec) (int, error) {
					_, _ = fmt.Fprintf(spec.Stdout, "AUDIT EVIDENCE — subject — gate: not run — findings: %s\nREVIEW COMPLETE — FAIL — reopened: subject\n", finding)
					return 0, nil
				}
				command := func(string, string, string, bool) ([]string, bool, bool) {
					return []string{"review-fixture"}, false, false
				}
				rot := ladder.NewRotation([]agents.Target{{Provider: "claude", Accounts: []string{"test"}}})
				run, err := c.runReviewVerdict(context.Background(), repo, "image", rot, "", "review", "signoff", command,
					[]string{root}, []string{"subject"}, "", &plan, loopcfg.ReviewWritesTasks, io.Discard, nil, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				return run
			}
			verdict(first)
			if _, err := tasks.LoadPendingReviews(repo, []string{root}); err != nil {
				t.Fatal(err)
			}
			a = assign()
			if tc.repair {
				writeTaskFile(t, filepath.Join(repo, "repair.txt"), "repair\n")
				git("add", "repair.txt")
				git("commit", "-qm", "repair")
			}
			closeTask(a)
			if tc.sibling {
				git("commit", "-qm", "another task\n\nCoop-Task: sibling", "--allow-empty")
			}
			run := verdict(tc.second)
			reopened, blocked := run.reopened, run.blocked
			if tc.want == tasks.StateBlocked && (len(blocked) != 1 || len(reopened) != 0) || tc.want == tasks.StateInProgress && (len(reopened) != 1 || len(blocked) != 0) {
				t.Fatalf("wrong work scheduling: reopened %v, blocked %v", reopened, blocked)
			}
			current, _, err := tasks.CurrentTask(root, "subject")
			if err != nil || current.State != tc.want {
				t.Fatalf("state = %s, %v; want %s", current.State, err, tc.want)
			}
			cohort, err := tasks.LoadPendingReviews(repo, []string{root})
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == tasks.StateBlocked {
				if len(cohort.Subjects) != 0 {
					t.Fatal("blocked dispute remained scheduled for review")
				}
				body, err := os.ReadFile(filepath.Join(current.Dir, "decision.md"))
				if err != nil {
					t.Fatal(err)
				}
				for _, want := range []string{"desktop", "no markup changed", "I ran nothing this pass"} {
					if !strings.Contains(string(body), want) {
						t.Errorf("decision missing %q: %s", want, body)
					}
				}
			}
		})
	}
}
