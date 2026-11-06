package tasks

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/forkspace"
)

// publishedCandidateFixture is a fork named rounds whose finished tasks wait in a published
// candidate, the state the board lists under Needs you as ready to merge.
func publishedCandidateFixture(t *testing.T, ids ...string) (candidateRoundFixture, ForkCandidate) {
	t.Helper()
	f := newCandidateRoundFixture(t, ids...)
	head, tree := f.headTree()
	candidate, published, err := reviewAndPublishForkCandidate(t, f.repo, f.identity, head, tree)
	if err != nil || !published {
		t.Fatalf("publish = %v, %v", published, err)
	}
	return f, candidate
}

// fakeForkCommands replaces the coop fork child process for one test, recording what ran.
func fakeForkCommands(t *testing.T, run func(args ...string) error) *[][]string {
	t.Helper()
	var ran [][]string
	saved := runForkCommand
	t.Cleanup(func() { runForkCommand = saved })
	runForkCommand = func(args ...string) error {
		ran = append(ran, args)
		return run(args...)
	}
	return &ran
}

// The board's Needs you section sends people to coop tasks decisions for a fork ready to merge, so
// the listing names its tasks, the fork and the commands that move it on.
func TestDecisionsListAForkReadyToMerge(t *testing.T) {
	f, _ := publishedCandidateFixture(t, "alpha", "beta")
	var code int
	out := captureStdout(t, func() { _ = captureStderr(t, func() { code, _ = tasksFolderDecisions(f.repo, f.root, nil) }) })
	want := "Forks ready to merge · 1\n\n  alpha\n  beta\n    Fork: rounds\n" +
		"    Review and merge: coop fork review rounds, then coop fork merge rounds\n"
	if code != 0 || out != want {
		t.Errorf("decisions = %d:\n%s\nwant:\n%s", code, out, want)
	}
}

// Finished fork work that no review has published yet is not ready to merge.
func TestDecisionsListNoForkWithoutACandidate(t *testing.T) {
	f := newCandidateRoundFixture(t, "alpha")
	var code int
	out := captureStdout(t, func() { _ = captureStderr(t, func() { code, _ = tasksFolderDecisions(f.repo, f.root, nil) }) })
	if code != 0 || out != "" {
		t.Errorf("decisions without a candidate = %d:\n%s\nwant nothing listed", code, out)
	}
}

// In -i a fork's page runs its review and its merge as the commands a person types, and drops the
// fork once its candidate has landed.
func TestDecisionBrowserReviewsAndMergesAFork(t *testing.T) {
	f, candidate := publishedCandidateFixture(t, "alpha")
	ran := fakeForkCommands(t, func(args ...string) error {
		if args[0] != "merge" {
			return nil
		}
		unlock, err := forkspace.LockState(f.repo, f.identity.Name)
		if err != nil {
			return err
		}
		defer unlock()
		return MarkForkCandidateLandedLocked(f.repo, candidate)
	})
	refs := mergeRefs(readyMerges(f.repo, []string{f.root}))
	var out bytes.Buffer
	var code int
	var err error
	summary := captureStderr(t, func() {
		code, err = runDecisionBrowser(f.repo, refs, strings.NewReader(":r\n:m\n"), &out)
	})
	if code != 0 || err != nil {
		t.Fatalf("browser = (%d, %v)", code, err)
	}
	if want := [][]string{{"review", "rounds"}, {"merge", "rounds"}}; !reflect.DeepEqual(*ran, want) {
		t.Errorf("ran %v, want %v", *ran, want)
	}
	for _, want := range []string{"fork rounds", "Merge fork rounds?\n  alpha\n", ":r review · :m merge · :n next"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the fork's page lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Count(out.String(), "Merge fork rounds?") != 2 || !strings.Contains(summary, "Merged 1 fork") {
		t.Errorf("review should return to the fork's page and the merge end the session:\n%s\n%s", out.String(), summary)
	}
}

// A merge that did not land, because it was declined or refused, leaves the fork on its page; an
// answer that is neither key says which keys work.
func TestDecisionBrowserKeepsAForkWhoseMergeDidNotLand(t *testing.T) {
	f, _ := publishedCandidateFixture(t, "alpha")
	ran := fakeForkCommands(t, func(...string) error { return nil })
	refs := mergeRefs(readyMerges(f.repo, []string{f.root}))
	var out bytes.Buffer
	summary := captureStderr(t, func() {
		if code, err := runDecisionBrowser(f.repo, refs, strings.NewReader("yes\n:m\n:q\n"), &out); code != 0 || err != nil {
			t.Errorf("browser = (%d, %v)", code, err)
		}
	})
	if want := [][]string{{"merge", "rounds"}}; !reflect.DeepEqual(*ran, want) {
		t.Errorf("ran %v, want %v", *ran, want)
	}
	if !strings.Contains(out.String(), "Type :r to review the fork, :m to merge it") {
		t.Errorf("an unknown answer should name the keys:\n%s", out.String())
	}
	if strings.Count(out.String(), "Merge fork rounds?") != 3 || !strings.Contains(summary, "No forks merged.") {
		t.Errorf("the fork should stay on its page until :q:\n%s\n%s", out.String(), summary)
	}
	if !forkReadyToMerge(f.repo, "rounds") {
		t.Error("the candidate should still be waiting")
	}
}
