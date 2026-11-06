package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/loop"
	"github.com/AndrewDryga/coop/internal/tasks"
)

// completingForkPass stands in for a fork's loop passes: a work pass whose agent checks off its one
// task, commits the work and moves the task to done, and a candidate review that passes. It records
// which task each work pass got and counts the candidate reviews.
func completingForkPass(t *testing.T, ran *[]string, reviews *int) func(loop.RunSpec) (int, error) {
	return func(spec loop.RunSpec) (int, error) {
		if spec.CandidateReview != nil {
			*reviews++
			return 0, nil
		}
		if len(spec.Queues) != 1 {
			t.Errorf("fork pass = queues %v; want one assigned task", spec.Queues)
			return 1, nil
		}
		queue := filepath.Join(spec.Repo, spec.Queues[0])
		entries, err := os.ReadDir(filepath.Join(queue, tasks.StateInProgress))
		if err != nil || len(entries) != 1 {
			t.Errorf("projected work = %v, %v; want one task in progress", entries, err)
			return 1, nil
		}
		id := entries[0].Name()
		*ran = append(*ran, id)
		dir := filepath.Join(queue, tasks.StateInProgress, id)
		body, err := os.ReadFile(filepath.Join(dir, "task.md"))
		if err == nil {
			err = os.WriteFile(filepath.Join(dir, "task.md"), []byte(strings.ReplaceAll(string(body), "- [ ] ", "- [x] ")), 0o644)
		}
		if err == nil {
			err = os.WriteFile(filepath.Join(spec.Repo, id+".txt"), []byte("done\n"), 0o644)
		}
		if err != nil {
			t.Error(err)
			return 1, nil
		}
		git(t, spec.Repo, "add", id+".txt")
		git(t, spec.Repo, "commit", "-qm", "Do "+id+"\n\nCoop-Task: "+id)
		if err := os.Rename(dir, filepath.Join(queue, tasks.StateDone, id)); err != nil {
			t.Error(err)
			return 1, nil
		}
		return 0, nil
	}
}

// A fork loop publishes its reviewed work only once it can take no other task, so with a queue of
// two the first task cannot be merged while the second runs. --stop-after-review stops the loop
// before it takes the second: the first task's work is published, coop fork ls says the fork is
// ready to merge, and the second task stays in the project's queue for the next run.
func TestForkLoopStopsAfterReviewReadyToMerge(t *testing.T) {
	first, second := "2026-11-06-fork-stop-a", "2026-11-06-fork-stop-b"
	for _, tc := range []struct {
		name            string
		stopAfterReview bool
		want            []string
	}{
		{name: "stop after review", stopAfterReview: true, want: []string{first}},
		{name: "whole queue", want: []string{first, second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := initRepo(t)
			seedForkDecisionTask(t, repo, first)
			seedForkDecisionTask(t, repo, second)
			commitForkNetworkPreflightFixture(t, repo)
			a := forkNetworkPreflightApp(t, repo)
			var ran []string
			reviews := 0
			a.forkLoopRun = completingForkPass(t, &ran, &reviews)
			ws, identity := setUpForkForDecision(t, repo, "stopper")

			var code int
			var err error
			var stderr string
			stdout := captureStdout(t, func() {
				stderr = captureStderr(t, func() {
					code, err = a.runForkLoop(repo, ws, identity, "claude", nil, "", "", "", nil, networkFlags{}, tc.stopAfterReview, false)
				})
			})
			output := stdout + stderr
			if code != 0 || err != nil || !slices.Equal(ran, tc.want) || reviews != 1 {
				t.Fatalf("fork loop = (%d, %v) after working %v with %d candidate reviews; want (0, nil) after %v and one review\n%s", code, err, ran, reviews, tc.want, output)
			}
			if !strings.Contains(output, "Fork stopper is ready to merge") {
				t.Errorf("fork loop output does not say the fork is ready to merge:\n%s", output)
			}
			candidate, exists, err := tasks.ReadForkCandidate(repo, identity)
			if err != nil || !exists || len(candidate.Assignments) != len(tc.want) {
				t.Fatalf("published candidate = %+v, exists=%v, err=%v; want one covering %v", candidate, exists, err, tc.want)
			}
			if tc.stopAfterReview {
				assertForkNetworkTaskUnassigned(t, repo, second)
			}
			listing := captureStdout(t, func() {
				if code, err := a.forkctl().ForkLs(nil); code != 0 || err != nil {
					t.Fatalf("fork ls = (%d, %v)", code, err)
				}
			})
			if !strings.Contains(listing, "stopper · ready to merge") {
				t.Errorf("fork ls does not show the fork ready to merge:\n%s", listing)
			}
		})
	}
}

// The flag is a loop option: a fork with no loop has no review to stop after.
func TestForkStopAfterReviewNeedsALoop(t *testing.T) {
	if _, err := parseForkCreate([]string{"perf", "codex", "--stop-after-review"}); err == nil || !strings.Contains(err.Error(), `"--stop-after-review" needs "--loop"`) {
		t.Errorf("--stop-after-review without --loop = %v, want the needs --loop refusal", err)
	}
	for _, args := range [][]string{
		{"perf", "codex", "--loop", "--stop-after-review"},
		{"perf", "codex", "-d", "--stop-after-review"},
		{"perf", "codex", "--loop", "--_detached", "--stop-after-review"},
	} {
		if fa, err := parseForkCreate(args); err != nil || !fa.stopAfterReview {
			t.Errorf("parseForkCreate(%v) = (stopAfterReview %v, %v), want the loop to stop after review", args, fa.stopAfterReview, err)
		}
	}
}
