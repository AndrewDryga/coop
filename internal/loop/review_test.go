package loop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/ladder"
	"github.com/AndrewDryga/coop/internal/loopcfg"
	"github.com/AndrewDryga/coop/internal/preset"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
	"github.com/AndrewDryga/coop/internal/ui"
)

func TestReviewRepoReadOnly(t *testing.T) {
	for _, tc := range []struct {
		name     string
		writes   loopcfg.ReviewWrites
		readOnly bool
	}{
		{name: "default", readOnly: true},
		{name: "explicit tasks", writes: loopcfg.ReviewWritesTasks, readOnly: true},
		{name: "explicit repository", writes: loopcfg.ReviewWritesRepo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if readOnly := reviewRepoReadOnly(tc.writes); readOnly != tc.readOnly {
				t.Errorf("reviewRepoReadOnly(%q) = %v, want %v", tc.writes, readOnly, tc.readOnly)
			}
		})
	}
}

func TestReadOnlyReviewFallbackReusesPreparedServices(t *testing.T) {
	repo := t.TempDir()
	task := taskForLease(t, repo, stateDone, "task-a")
	c := New(&config.Config{RepoOverride: repo, ConfigDir: t.TempDir(), Homes: true}, runtime.Runtime{Name: "true"}, "test", Host{})
	c.net = newNetworkLog()
	c.runID = "service-reuse-test"
	var launches []box.RunSpec
	c.boxRun = func(spec box.RunSpec) (int, error) {
		launches = append(launches, spec)
		if len(launches) == 1 {
			_, _ = fmt.Fprintln(spec.Stderr, "rate limit exceeded")
			return 1, nil
		}
		_, _ = fmt.Fprintln(spec.Stdout, "review complete")
		return 0, nil
	}
	command := func(_ string, _ string, _ string, _ bool) ([]string, bool, bool) {
		return []string{"review-fixture"}, false, false
	}
	rot := ladder.NewRotation([]agents.Target{
		{Provider: "claude", Accounts: []string{"first"}},
		{Provider: "claude", Accounts: []string{"second"}},
	})
	if _, err := c.runReview(context.Background(), repo, "image", rot, "", "review", "signoff", command,
		[]string{repo}, []string{task.ID}, nil, loopcfg.ReviewWritesTasks, io.Discard, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(launches) != 2 || launches[0].ReuseServices || launches[0].Quiet || !launches[0].LoopPresentation ||
		!launches[1].ReuseServices || !launches[1].Quiet || launches[1].LoopPresentation {
		t.Fatalf("review fallback launches = %+v, want narrated setup once then prepared-service reuse", launches)
	}
}

// A failed review attempt says why, as a task attempt does, and the stage's stop carries the last
// cause to the report that ends the review. A launch refused before the reviewer starts stops at once.
func TestReviewAttemptsNameTheirCause(t *testing.T) {
	repo := t.TempDir()
	task := taskForLease(t, repo, stateDone, "task-a")
	command := func(string, string, string, bool) ([]string, bool, bool) {
		return []string{"review-fixture"}, false, false
	}
	review := func(t *testing.T, boxRun func(box.RunSpec) (int, error)) (string, error) {
		c := New(&config.Config{RepoOverride: repo, ConfigDir: t.TempDir()}, runtime.Runtime{Name: "true"}, "test", Host{})
		c.net = newNetworkLog()
		c.boxRun = boxRun
		var err error
		// Captured outside the bubble: the capture's pipe reader would stop virtual time.
		output := captureStderr(t, func() {
			synctest.Test(t, func(t *testing.T) {
				_, err = c.runReview(context.Background(), repo, "image", ladder.NewRotation([]agents.Target{{Provider: "claude"}}),
					"", "review", "signoff", command, []string{repo}, []string{task.ID}, nil, loopcfg.ReviewWritesTasks, io.Discard, nil, nil, nil)
			})
		})
		return output, err
	}

	t.Run("failed attempts", func(t *testing.T) {
		launches := 0
		output, err := review(t, func(box.RunSpec) (int, error) { launches++; return 1, nil })
		retry := "\n⚠ Review attempt failed\n\n      Claude Code exited with status 1\n      Retrying in 10 seconds · attempt 2 of 5.\n"
		if launches != maxLoopFailures || !strings.Contains(output, retry) {
			t.Errorf("launches = %d, want %d, each failure naming its cause; want\n%s\ngot\n%s", launches, maxLoopFailures, retry, output)
		}
		if err == nil || !strings.HasSuffix(err.Error(), "\nLast attempt: Claude Code exited with status 1") {
			t.Errorf("review stop = %v, want it to name the last cause", err)
		}
	})
	t.Run("refused launch", func(t *testing.T) {
		refusal := &box.LaunchRefused{Err: errors.New("writer mount cannot be resolved")}
		launches := 0
		output, err := review(t, func(box.RunSpec) (int, error) { launches++; return -1, refusal })
		if launches != 1 || !errors.Is(err, refusal) || strings.Contains(output, "Review attempt failed") {
			t.Errorf("refused review = %v after %d launches, want the refusal at the first\n%s", err, launches, output)
		}
	})
}

func TestWorkCredentialFallbackReusesServicesOnlyFromCleanUnchangedTree(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	repo, git := gitrepo.New(t)
	if err := os.WriteFile(filepath.Join(repo, "source.go"), []byte("package source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "source.go")
	git("commit", "-qm", "base")
	head := forkspace.GitOut(repo, "rev-parse", "HEAD")

	if !cleanCredentialRetryCanReuseServices(repo, head, head, "rate_limit") ||
		!cleanCredentialRetryCanReuseServices(repo, head, head, "authentication") {
		t.Fatal("clean credential fallback did not reuse prepared services")
	}
	if cleanCredentialRetryCanReuseServices(repo, head, head, "process_failure") ||
		cleanCredentialRetryCanReuseServices(repo, head, head+"changed", "rate_limit") {
		t.Fatal("non-credential or changed-HEAD retry reused prepared services")
	}
	if err := os.WriteFile(filepath.Join(repo, "source.go"), []byte("package changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cleanCredentialRetryCanReuseServices(repo, head, head, "rate_limit") {
		t.Fatal("dirty credential fallback reused prepared services")
	}
}

func TestReviewVerdictUsesOneExactSessionCorrection(t *testing.T) {
	for _, returned := range []string{"absent", "same", "different"} {
		t.Run(returned, func(t *testing.T) {
			t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
			t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
			repo, git := gitrepo.New(t)
			if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".agent/\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, "source.go"), []byte("package source\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			git("add", ".gitignore", "source.go")
			git("commit", "-qm", "base")
			task := taskForLease(t, repo, stateDone, "task-a")

			c := New(&config.Config{RepoOverride: repo, ConfigDir: t.TempDir(), Homes: true}, runtime.Runtime{Name: "true"}, "test", Host{})
			c.net = newNetworkLog()
			c.runID = "correction-test"
			type commandCall struct {
				prompt, id string
				resume     bool
			}
			var commands []commandCall
			launches := 0
			c.boxRun = func(spec box.RunSpec) (int, error) {
				launches++
				if spec.OnRuntimeLaunch != nil {
					spec.OnRuntimeLaunch()
				}
				verdict := "AUDIT EVIDENCE — task-a — gate: focused tests passed — findings: none\nREVIEW COMPLETE — PASS — reopened: none"
				if !spec.FormatCorrection {
					verdict += "\ntrailing prose"
				} else if spec.Preset != nil || len(spec.Peers) != 0 || spec.TaskTools != nil {
					t.Fatal("format correction repeated preset peers or task tooling")
				}
				_, _ = fmt.Fprintf(spec.Stdout, "{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":%q}]}}\n", verdict)
				sessionID := commands[0].id
				if spec.FormatCorrection {
					switch returned {
					case "absent":
						sessionID = ""
					case "different":
						sessionID = "00000000-0000-4000-8000-000000000001"
					}
				}
				_, _ = fmt.Fprintf(spec.Stdout, "{\"type\":\"result\",\"subtype\":\"success\",\"num_turns\":1,\"duration_ms\":1,\"session_id\":%q}\n", sessionID)
				return 0, nil
			}
			command := func(_ string, prompt, id string, resume bool) ([]string, bool, bool) {
				commands = append(commands, commandCall{prompt: prompt, id: id, resume: resume})
				return []string{"claude", "-p", prompt}, true, true
			}
			rot := ladder.NewRotation([]agents.Target{{Provider: "claude", Accounts: []string{"test"}}})
			observed := 0
			run, err := c.runReviewVerdict(context.Background(), repo, "image", rot, "", "review", "signoff", command,
				[]string{repo}, []string{task.ID}, "", nil, loopcfg.ReviewWritesTasks, io.Discard, nil, nil,
				func(reviewRunResult, time.Time, string) { observed++ })
			if returned == "different" {
				if err == nil || !strings.Contains(err.Error(), "different reviewer session") || len(run.reopened) != 0 {
					t.Fatalf("foreign correction = %+v, %v; want refusal without task changes", run, err)
				}
			} else if err != nil {
				t.Fatalf("corrected review: %v\n%s", err, run.output)
			}
			if launches != 2 || len(commands) != 2 || commands[0].resume || !commands[1].resume || commands[0].id == "" || commands[1].id != commands[0].id {
				t.Fatalf("review/correction launches = %d commands = %+v", launches, commands)
			}
			if !strings.Contains(commands[1].prompt, "Validation error:") || !strings.Contains(commands[1].prompt, "AUDIT EVIDENCE — task-a") {
				t.Fatalf("correction prompt lacks precise repair data:\n%s", commands[1].prompt)
			}
			if observed != 1 || run.retries != 1 {
				t.Fatalf("recovery episode observer/retries = %d/%d, want 1/1", observed, run.retries)
			}
			if current, ok, err := tasks.CurrentTask(repo, task.ID); err != nil || !ok || current.State != stateDone {
				t.Fatalf("passing correction changed completed task: %+v %v %v", current, ok, err)
			}
		})
	}
}

// A writes: repo reviewer may fix source, so its verdict stands over the commits it added on top of
// the HEAD it reviewed, and the loop names them as the reviewer's. Before, any commit failed the
// completion audit, so such a review could never pass. The commits still pass the loop's binding
// and range checks, any other change to the checkout fails the review as before, and a report-only
// review (writes: tasks) still refuses every commit.
func TestRepositoryWritableReviewKeepsOnlyItsOwnCommits(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	commit := func(message string) func(string, func(...string)) {
		return func(repo string, git func(...string)) {
			writeTaskFile(t, filepath.Join(repo, "notes.txt"), "reviewed\n")
			git("add", "notes.txt")
			git("commit", "-qm", message)
		}
	}
	for _, tc := range []struct {
		name   string
		writes loopcfg.ReviewWrites
		change func(repo string, git func(...string))
		refuse string // "" accepts the review; else part of the refusal
	}{
		{"its own commit", loopcfg.ReviewWritesRepo, commit("QA review edit"), ""},
		{"report-only review", loopcfg.ReviewWritesTasks, commit("QA review edit"), "review source changed after the reviewer returned"},
		{"uncommitted change", loopcfg.ReviewWritesRepo, func(repo string, _ func(...string)) {
			writeTaskFile(t, filepath.Join(repo, "source.go"), "package changed\n")
		}, "review source changed after the reviewer returned"},
		{"rewritten history", loopcfg.ReviewWritesRepo, func(_ string, git func(...string)) {
			git("commit", "--amend", "-qm", "base, rewritten")
		}, "HEAD no longer contains commit"},
		{"task binding", loopcfg.ReviewWritesRepo, commit("QA review edit\n\nCoop-Task: task-a"), "binds task task-a, and a review binds no task"},
		{"merge", loopcfg.ReviewWritesRepo, func(repo string, git func(...string)) {
			git("checkout", "-qb", "side")
			commit("side work")(repo, git)
			git("checkout", "-q", "main")
			git("merge", "-q", "--no-ff", "-m", "merge side", "side")
		}, "is a merge, and a review may only add commits on top"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, git := gitrepo.New(t)
			writeTaskFile(t, filepath.Join(repo, ".gitignore"), ".agent/\n")
			writeTaskFile(t, filepath.Join(repo, "source.go"), "package source\n")
			git("add", ".gitignore", "source.go")
			git("commit", "-qm", "base")
			task := taskForLease(t, repo, stateDone, "task-a")
			head := forkspace.GitOut(repo, "rev-parse", "HEAD")

			c := New(&config.Config{RepoOverride: repo, ConfigDir: t.TempDir(), Homes: true}, runtime.Runtime{Name: "true"}, "test", Host{})
			c.net = newNetworkLog()
			c.runID = "writes-test"
			c.boxRun = func(spec box.RunSpec) (int, error) {
				if spec.RepoReadOnly == tc.writes.RepositoryWritable() {
					t.Fatalf("review box read-only = %t for writes %q", spec.RepoReadOnly, tc.writes)
				}
				tc.change(repo, git)
				verdict := "AUDIT EVIDENCE — task-a — gate: focused tests passed — findings: none\nREVIEW COMPLETE — PASS — reopened: none"
				_, _ = fmt.Fprintf(spec.Stdout, "{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":%q}]}}\n", verdict)
				_, _ = io.WriteString(spec.Stdout, "{\"type\":\"result\",\"subtype\":\"success\",\"num_turns\":1,\"duration_ms\":1}\n")
				return 0, nil
			}
			command := func(_ string, prompt, _ string, _ bool) ([]string, bool, bool) {
				return []string{"claude", "-p", prompt}, true, true
			}
			rot := ladder.NewRotation([]agents.Target{{Provider: "claude", Accounts: []string{"test"}}})
			var run reviewRunResult
			var err error
			output := captureStderr(t, func() {
				run, err = c.runReviewVerdict(context.Background(), repo, "image", rot, "", "review", "signoff", command,
					[]string{repo}, []string{task.ID}, head, nil, tc.writes, io.Discard, nil, nil, nil)
			})
			if tc.refuse != "" {
				if err == nil || !errors.Is(err, tasks.ErrCompletionWindowAudit) || !strings.Contains(err.Error(), tc.refuse) {
					t.Fatalf("review = %v, want the completion audit to refuse it with %q", err, tc.refuse)
				}
				return
			}
			after := forkspace.GitOut(repo, "rev-parse", "HEAD")
			if err != nil || len(run.reopened) != 0 || len(run.commits) != 1 || run.commits[0] != after {
				t.Fatalf("review = commits %q reopened %q err %v, want the reviewer's commit %s accepted", run.commits, run.reopened, err, after)
			}
			if !strings.Contains(output, "The reviewer added 1 commit: "+after[:7]+" QA review edit.") {
				t.Fatalf("the reviewer's commit was not named:\n%s", output)
			}
		})
	}
}

// A candidate review whose box's services did not start reviewed nothing. Its attempt is retried
// like any failed review attempt; when every attempt fails that way, it says the review could not
// start, never that it did not pass.
func TestCandidateReviewRetriesABoxWhoseServicesDidNotStart(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	repo, git := gitrepo.New(t)
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".agent/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".gitignore")
	git("commit", "-qm", "base")
	task := taskForLease(t, repo, stateDone, "task-a")
	unhealthy := &box.ServicesNotStarted{Err: errors.New("a filtered box needs this project's approved sidecars running, and starting them failed: compose up exited with status 1: container coop-fs-app-service is unhealthy")}
	command := func(string, string, string, bool) ([]string, bool, bool) {
		return []string{"claude", "-p", "review"}, true, true
	}
	review := func(t *testing.T, failures int) (reviewRunResult, int, string, error) {
		c := New(&config.Config{RepoOverride: repo, ConfigDir: t.TempDir(), Homes: true}, runtime.Runtime{Name: "true"}, "test", Host{})
		c.net = newNetworkLog()
		c.runID = "candidate-start-test"
		launches := 0
		c.boxRun = func(spec box.RunSpec) (int, error) {
			launches++
			if launches <= failures {
				return -1, unhealthy
			}
			spec.OnRuntimeLaunch()
			verdict := "AUDIT EVIDENCE — task-a — gate: focused tests passed — findings: none\nREVIEW COMPLETE — PASS — reopened: none"
			_, _ = fmt.Fprintf(spec.Stdout, "{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":%q}]}}\n", verdict)
			_, _ = fmt.Fprintf(spec.Stdout, "{\"type\":\"result\",\"subtype\":\"success\",\"num_turns\":1,\"duration_ms\":1,\"session_id\":\"\"}\n")
			return 0, nil
		}
		var run reviewRunResult
		var err error
		// Captured outside the bubble: the capture's pipe reader would stop virtual time.
		output := captureStderr(t, func() {
			synctest.Test(t, func(t *testing.T) {
				run, err = c.runCandidateReviewVerdict(context.Background(), repo, "image", ladder.NewRotation([]agents.Target{{Provider: "claude", Accounts: []string{"test"}}}),
					"fork", "review", "candidate review", command, []string{repo}, []string{task.ID}, io.Discard, nil, nil, nil)
			})
		})
		return run, launches, output, err
	}

	t.Run("one start failure, then a pass", func(t *testing.T) {
		run, launches, output, err := review(t, 1)
		if err != nil || len(run.reopened) != 0 || launches != 2 {
			t.Fatalf("candidate review = %+v, %v after %d launches, want a pass on the retry\n%s", run, err, launches, output)
		}
		if !strings.Contains(output, "Review attempt failed") || !strings.Contains(output, "is unhealthy") || !strings.Contains(output, "attempt 2 of 5") {
			t.Errorf("the start failure was not retried as a failed attempt naming its cause:\n%s", output)
		}
	})
	t.Run("every start fails", func(t *testing.T) {
		_, launches, _, err := review(t, maxLoopFailures)
		if launches != maxLoopFailures || !errors.Is(err, unhealthy) || candidateReviewFailure(err) != "Candidate review could not start" {
			t.Fatalf("candidate review = %v after %d launches, headline %q; want it to say it could not start", err, launches, candidateReviewFailure(err))
		}
		if !strings.Contains(err.Error(), "Last attempt: ") || !strings.Contains(err.Error(), "is unhealthy") {
			t.Errorf("the stop does not say why the review could not start: %v", err)
		}
	})
	if got := candidateReviewFailure(errors.New("candidate review found unresolved issues in task-a")); got != "Candidate review did not pass" {
		t.Errorf("a review with findings reads %q, want that it did not pass", got)
	}
}

func TestReviewVerdictRejectsSourceChangeBeforeCorrection(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	repo, git := gitrepo.New(t)
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".agent/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(repo, "source.go")
	if err := os.WriteFile(source, []byte("package source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".gitignore", "source.go")
	git("commit", "-qm", "base")
	task := taskForLease(t, repo, stateDone, "task-a")

	c := New(&config.Config{RepoOverride: repo, ConfigDir: t.TempDir(), Homes: true}, runtime.Runtime{Name: "true"}, "test", Host{})
	c.net = newNetworkLog()
	c.runID = "source-change-test"
	launches := 0
	c.boxRun = func(spec box.RunSpec) (int, error) {
		launches++
		if err := os.WriteFile(source, []byte("package changed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, _ = fmt.Fprintln(spec.Stdout, `{"type":"assistant","message":{"content":[{"type":"text","text":"invalid"}]}}`)
		_, _ = fmt.Fprintln(spec.Stdout, `{"type":"result","subtype":"success","num_turns":1,"duration_ms":1}`)
		return 0, nil
	}
	command := func(_ string, prompt, id string, resume bool) ([]string, bool, bool) {
		return []string{"claude", "-p", prompt}, true, true
	}
	rot := ladder.NewRotation([]agents.Target{{Provider: "claude", Accounts: []string{"test"}}})
	_, err := c.runReviewVerdict(context.Background(), repo, "image", rot, "", "review", "signoff", command,
		[]string{repo}, []string{task.ID}, "", nil, loopcfg.ReviewWritesTasks, io.Discard, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "review source changed") {
		t.Fatalf("source-changing review error = %v", err)
	}
	if launches != 1 {
		t.Fatalf("source-changing malformed review launched %d corrections, want 0", launches-1)
	}
}

// A final-review round records what it covers at one commit. A HEAD that moved after that, before the
// review's own snapshot, stops the review before any reviewer runs: its verdict would otherwise
// accept history nobody recorded.
func TestReviewVerdictRefusesAHeadOtherThanTheRounds(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	repo, git := gitrepo.New(t)
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".agent/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".gitignore")
	git("commit", "-qm", "base")
	roundHead := forkspace.GitOut(repo, "rev-parse", "HEAD")
	git("commit", "-q", "--allow-empty", "-m", "moved after the round started")
	task := taskForLease(t, repo, stateDone, "task-a")

	c := New(&config.Config{RepoOverride: repo, ConfigDir: t.TempDir(), Homes: true}, runtime.Runtime{Name: "true"}, "test", Host{})
	c.net = newNetworkLog()
	c.runID = "round-head-test"
	launches := 0
	c.boxRun = func(box.RunSpec) (int, error) { launches++; return 0, nil }
	command := func(_ string, prompt, _ string, _ bool) ([]string, bool, bool) {
		return []string{"claude", "-p", prompt}, true, true
	}
	rot := ladder.NewRotation([]agents.Target{{Provider: "claude", Accounts: []string{"test"}}})
	_, err := c.runReviewVerdict(context.Background(), repo, "image", rot, "", "review", "signoff", command,
		[]string{repo}, []string{task.ID}, roundHead, nil, loopcfg.ReviewWritesTasks, io.Discard, nil, nil, nil)
	if !errors.Is(err, tasks.ErrCompletionWindowSetup) || !strings.Contains(err.Error(), "HEAD moved from "+roundHead) {
		t.Fatalf("review on a moved HEAD = %v", err)
	}
	if launches != 0 {
		t.Fatalf("the refused review launched %d reviewers", launches)
	}
}

func TestReviewSourceSnapshotRejectsHeadAndTrackedChanges(t *testing.T) {
	repo, git := gitrepo.New(t)
	if err := os.WriteFile(filepath.Join(repo, "source.go"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "source.go")
	git("commit", "-qm", "base")
	snapshot, err := snapshotReviewSource(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateReviewSource(repo, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "source.go"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateReviewSource(repo, snapshot); err == nil {
		current, _ := snapshotReviewSource(repo)
		t.Fatalf("tracked source change preserved a stale review snapshot: before %#v after %#v", snapshot, current)
	}
	git("add", "source.go")
	git("commit", "-qm", "changed")
	if err := validateReviewSource(repo, snapshot); err == nil {
		t.Fatal("HEAD change preserved a stale review snapshot")
	}
	if _, err := validateReviewSourceAfterConcurrent(repo, snapshot, []string{"concurrent-task"}, "", loopcfg.ReviewWritesTasks); err != nil {
		t.Fatalf("host-observed concurrent commit rejected: %v", err)
	}
}

// A completion another session lands during a writes: repo review shares the review's range. Its
// commit belongs to that task, which the next round reviews; only the other commits are the
// reviewer's. Without the host's word for that completion, its trailer is a binding the review made.
func TestRepositoryWritableReviewSharesItsRangeWithAConcurrentCompletion(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	repo, git := gitrepo.New(t)
	commit := func(name, message string) {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", name)
		git("commit", "-qm", message)
	}
	commit("source.go", "base")
	snapshot, err := snapshotReviewSource(repo)
	if err != nil {
		t.Fatal(err)
	}
	commit("other.go", "Other work\n\nCoop-Task: other-task")
	commit("notes.txt", "QA review edit")
	reviewer := forkspace.GitOut(repo, "rev-parse", "HEAD")
	commits, err := validateReviewSourceAfterConcurrent(repo, snapshot, []string{"other-task"}, "", loopcfg.ReviewWritesRepo)
	if err != nil || len(commits) != 1 || commits[0] != reviewer {
		t.Fatalf("review commits = %q, %v; want only the reviewer's %s", commits, err, reviewer)
	}
	if _, err := validateReviewSourceAfterConcurrent(repo, snapshot, nil, "", loopcfg.ReviewWritesRepo); err == nil ||
		!strings.Contains(err.Error(), "binds task other-task, and a review binds no task") {
		t.Fatalf("an unobserved completion's trailer = %v, want it refused as the review's binding", err)
	}
	// Nor can the review bind that task a second time behind the completion's own commit.
	commit("more.txt", "QA review edit\n\nCoop-Task: other-task")
	if _, err := validateReviewSourceAfterConcurrent(repo, snapshot, []string{"other-task"}, "", loopcfg.ReviewWritesRepo); err == nil ||
		!strings.Contains(err.Error(), "does not have exactly one commit") {
		t.Fatalf("a second binding for the concurrent task = %v, want it refused", err)
	}
}

func TestReviewVerdictCorrectionDropsControlBytes(t *testing.T) {
	got := reviewVerdictCorrection(errors.New("invalid\x00receipt"), "answer\x00tail", []string{"task-a"})
	if strings.ContainsRune(got, '\x00') || !strings.Contains(got, "invalidreceipt") || !strings.Contains(got, "answertail") {
		t.Fatalf("unsafe correction prompt %q", got)
	}
}

// TestReviewLadder: a review stage's ladder keeps each rung's PROVIDER, model, effort, and the
// fallback rungs. Without the provider, a claude-led run's `codex:…` signoff would resolve to
// `claude --model <a-codex-model>` and the cross-vendor reviewer would never run.
func TestReviewLadder(t *testing.T) {
	ladder, err := reviewLadder([]string{"codex:gpt-5.6-sol/xhigh", "claude:claude-fable-5/xhigh"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ladder) != 2 {
		t.Fatalf("both rungs must survive (the fallback too), got %d", len(ladder))
	}
	// Rung 0 keeps its provider — NOT discarded onto the work provider.
	if ladder[0].Provider != "codex" || ladder[0].Model != "gpt-5.6-sol" || ladder[0].Effort != "xhigh" {
		t.Errorf("rung 0 = %+v, want codex / gpt-5.6-sol / xhigh", ladder[0])
	}
	// Rung 1 (the fallback) survives with its own provider.
	if ladder[1].Provider != "claude" || ladder[1].Model != "claude-fable-5" {
		t.Errorf("rung 1 = %+v, want claude / claude-fable-5", ladder[1])
	}
	// An empty ladder yields no rungs — the caller falls back to the work rotation.
	if got, _ := reviewLadder(nil); len(got) != 0 {
		t.Errorf("empty ladder → no rungs, got %v", got)
	}
}

func TestReviewRotationsShareOnlyInheritedCooldowns(t *testing.T) {
	c := &Control{host: Host{BuildRotation: func(_ string, rungs []agents.Target) (*ladder.Rotation, error) {
		return ladder.NewRotation(rungs), nil
	}}}
	now := time.Unix(1000, 0)
	for _, explicit := range []bool{false, true} {
		work := rts("work", "backup")
		lc := &loopcfg.Config{}
		if explicit {
			lc.Signoff.Agent = []string{"claude@review", "claude@spare"}
		}
		rotations, err := c.resolveReviewRotations(lc, "claude", work)
		if err != nil {
			t.Fatal(err)
		}
		active := rotations.between.Active().String()
		rotations.between.OnLimit(now.Add(time.Hour), 0, now)
		for name, rotation := range map[string]*ladder.Rotation{"signoff": rotations.signoff, "verify": rotations.verify} {
			if !rotation.LimitedUntil(active).Equal(now.Add(time.Hour)) || rotation.Active().String() == active {
				t.Fatalf("%s did not inherit the review cooldown and advancement", name)
			}
		}
		if explicit {
			if !work.LimitedUntil(active).IsZero() || work.Active().String() != "claude@work" {
				t.Fatal("explicit reviewer changed work rotation")
			}
		} else if !work.LimitedUntil(active).Equal(now.Add(time.Hour)) || work.Active().String() != "claude@backup" {
			t.Fatal("inherited reviewer lost work rotation state")
		}
	}
}

func TestReviewRotationsPreserveStageErrorOrder(t *testing.T) {
	for index, stage := range []string{"signoff", "between", "verify"} {
		t.Run(stage, func(t *testing.T) {
			refusal := errors.New("fixture credential refusal")
			calls := 0
			c := &Control{host: Host{BuildRotation: func(_ string, rungs []agents.Target) (*ladder.Rotation, error) {
				calls++
				if calls == index+1 {
					return nil, refusal
				}
				return ladder.NewRotation(rungs), nil
			}}}
			lc := &loopcfg.Config{
				Signoff: loopcfg.Signoff{Agent: []string{"claude@signoff"}},
				Between: loopcfg.Between{Agent: []string{"claude@between"}},
				Verify:  loopcfg.Verify{Agent: []string{"claude@verify"}},
			}
			_, err := c.resolveReviewRotations(lc, "claude", rts("work"))
			if !errors.Is(err, refusal) || !strings.HasPrefix(err.Error(), stage+" agent: ") || calls != index+1 {
				t.Fatalf("calls=%d error=%v, want first %s refusal", calls, err, stage)
			}
		})
	}
}

// A review stage that cannot run stops the run before any box and fails it as the work rotation
// does in cmdLoop: Run returns -1 with the error intact, and the error sets the exit code, 1 for each.
// None is rejected input: not unlistable accounts, not a missing login, and not a stage lead that
// cannot host the preset's native role, which .agent/loop.yaml and the preset decide, not argv.
func TestReviewStageRotationRefusals(t *testing.T) {
	native := &preset.Preset{Name: "t", Dir: t.TempDir(), Roles: []preset.Role{
		{Name: "thinker", Mode: preset.ModeNative, Targets: []agents.Target{{Provider: "claude"}}},
	}}
	for _, test := range []struct {
		name   string
		err    error          // the signoff rotation's refusal; nil builds the rotation
		preset *preset.Preset // the run's preset, whose roles every stage's lead must host
	}{
		{"accounts that could not be listed", ui.CommandFailed("Could not list every Codex account", "open /config/codex/credentials: permission denied"), nil},
		{"no account to run", errors.New("credential codex@work is not signed in — run: coop login codex@work"), nil},
		{"a lead that cannot host the preset's native role", nil, native},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			repo := t.TempDir()
			writeTaskFile(t, filepath.Join(repo, tasksRoot, stateTodo, "2026-01-01-x", "task.md"), "# x\n")
			writeTaskFile(t, filepath.Join(repo, ".agent", "loop.yaml"), "signoff:\n  agent: [codex:gpt-5.6-sol@work]\n")
			host := Host{BuildRotation: func(_ string, rungs []agents.Target) (*ladder.Rotation, error) {
				if test.err != nil {
					return nil, test.err
				}
				return ladder.NewRotation(rungs), nil
			}}
			c := New(&config.Config{RepoOverride: repo, ConfigDir: t.TempDir()}, runtime.Runtime{Name: "true"}, "test", host)
			c.boxRun = func(box.RunSpec) (int, error) {
				t.Error("a box launched")
				return 0, nil
			}
			code, err := c.Run(RunSpec{Repo: repo, Image: "img", Agent: "claude", Queues: []string{tasksRoot}, Sink: io.Discard,
				Preset: test.preset, Rotation: ladder.NewRotation([]agents.Target{target("claude", "work")})})
			refused := errors.Is(err, test.err)
			if test.err == nil {
				var leads *preset.LoadError
				refused = errors.As(err, &leads)
			}
			if code != -1 || !refused || !strings.HasPrefix(err.Error(), "signoff agent: ") {
				t.Fatalf("Run = (%d, %v), want (-1, the signoff stage's refusal)", code, err)
			}
		})
	}
}

func TestReviewSubjectSnapshotsRejectLifecycleGenerationChange(t *testing.T) {
	root := t.TempDir()
	task := taskForLease(t, root, stateDone, "task-a")
	snapshots, err := snapshotReviewSubjects([]string{root}, []string{task.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateReviewSubjects([]string{root}, snapshots); err != nil {
		t.Fatalf("unchanged review subject failed validation: %v", err)
	}

	oldDir := filepath.Join(t.TempDir(), task.ID)
	if err := os.Rename(task.Dir, oldDir); err != nil {
		t.Fatal(err)
	}
	taskForLease(t, root, stateDone, task.ID)
	if err := validateReviewSubjects([]string{root}, snapshots); err == nil ||
		!strings.Contains(err.Error(), "changed completion generation") {
		t.Fatalf("replacement review subject validation = %v, want generation change", err)
	}
}
