package loop

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// TestNewlyFinished: the before/after done-set diff names exactly what an iteration completed,
// sorted; taskIDsOf strips the dirs for the banner.
func TestNewlyFinished(t *testing.T) {
	before := map[string]string{"a": "/q/99_done/a"}
	now := map[string]string{"a": "/q/99_done/a", "c": "/q/99_done/c", "b": "/q/99_done/b"}
	got := newlyFinished(before, now)
	want := []string{"b — /q/99_done/b", "c — /q/99_done/c"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("newlyFinished = %v, want %v", got, want)
	}
	if ids := taskIDsOf(got); ids[0] != "b" || ids[1] != "c" {
		t.Errorf("taskIDsOf = %v, want [b c]", ids)
	}
	if extra := newlyFinished(now, now); len(extra) != 0 {
		t.Errorf("no change should mean no finished tasks, got %v", extra)
	}
	prior := map[string]string{"a": "/q/99_done/a", "c": "/q/99_done/c"}
	baseline := reviewBaselineAfterVerdict(prior, []string{"b — /q/99_done/b"}, []string{"b"}, []string{"c"})
	if _, present := baseline["b"]; present {
		t.Fatal("a concurrently re-completed reopen entered the next review baseline")
	}
	if _, present := baseline["c"]; present {
		t.Fatal("a concurrent host completion was absorbed into the next review baseline")
	}
	if got := taskIDsOf(newlyFinished(baseline, now)); !slices.Equal(got, []string{"b", "c"}) {
		t.Fatalf("re-completed reopen subjects = %v, want [b c]", got)
	}
}

func TestCompletedReviewSubjectsUseHostState(t *testing.T) {
	root := t.TempDir()
	taskForLease(t, root, stateDone, "accepted")
	taskForLease(t, root, stateBlocked, "later-blocked")
	taskForLease(t, root, stateDone, "trailer-only")
	completed := map[string]bool{"accepted": true, "later-blocked": true}
	got, err := completedReviewSubjects([]string{root}, completed)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"accepted"}) {
		t.Fatalf("completed review subjects = %v, want host-accepted archived task only", got)
	}
}

func TestBlockReopenedTasksLeavesUnrelatedActionableWork(t *testing.T) {
	q := filepath.Join(t.TempDir(), ".agent", "tasks")
	writeTaskFile(t, filepath.Join(q, stateInProgress, "review-reopen", "task.md"), "# Reopen\n")
	writeTaskFile(t, filepath.Join(q, stateInProgress, "review-reopen", "decision.md"), "# Old decision\n\n**Resolution:** already resolved\n")
	writeTaskFile(t, filepath.Join(q, stateInProgress, "unrelated", "task.md"), "# Unrelated\n")

	if err := blockReopenedTasks([]string{q}, []string{"review-reopen"}, 3); err != nil {
		t.Fatal(err)
	}

	if !pathExists(filepath.Join(q, stateBlocked, "review-reopen")) {
		t.Fatal("exact review reopen was not blocked")
	}
	decision, err := os.ReadFile(filepath.Join(q, stateBlocked, "review-reopen", "decision.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(decision), "review keeps reopening") || strings.Contains(string(decision), "Old decision") {
		t.Fatalf("review cap retained stale decision:\n%s", decision)
	}
	// A resumed review parks a task while unrelated work still waits: the decision counts it
	// instead of claiming the queue drained.
	if !strings.Contains(string(decision), "with 1 other task still in the queue") || strings.Contains(string(decision), "drained") {
		t.Fatalf("review cap decision misstates the queue:\n%s", decision)
	}
	if !pathExists(filepath.Join(q, stateInProgress, "unrelated")) {
		t.Fatal("unrelated actionable task was moved by the signoff cap")
	}
}

// With signoff.rounds: 1 the review limit counts in the singular. Before, decision.md said "after 1
// rounds" and "reopened this task 1 times", and the closing banner "after 1 rounds".
func TestReviewLimitCountsOneRoundInTheSingular(t *testing.T) {
	q := filepath.Join(t.TempDir(), ".agent", "tasks")
	writeTaskFile(t, filepath.Join(q, stateInProgress, "review-reopen", "task.md"), "# Add hello.txt\n")
	var err error
	out := captureStderr(t, func() { err = parkCappedReview([]string{q}, []string{"review-reopen"}, 1) })
	if err != nil {
		t.Fatal(err)
	}
	decision, err := os.ReadFile(filepath.Join(q, stateBlocked, "review-reopen", "decision.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`keeps reopening "Add hello.txt" after 1 round` + "\n", "reopened this task 1 time without"} {
		if !strings.Contains(string(decision), want) {
			t.Errorf("decision.md lacks %q:\n%s", want, decision)
		}
	}
	if !strings.Contains(out, "Could not resolve this task after 1 round.") {
		t.Errorf("the review limit banner lacks the singular round:\n%s", out)
	}
	if text := string(decision) + out; strings.Contains(text, "1 rounds") || strings.Contains(text, "1 times") {
		t.Errorf("a count of one kept its plural:\n%s", text)
	}
}

// A block that fails parked nothing, so nothing may report the review limit or point at decisions;
// the caller fails the run with the error instead of exit 3, which promises only decisions remain.
func TestParkCappedReviewReportsNothingWhenTheBlockFails(t *testing.T) {
	root := t.TempDir()
	var err error
	out := captureStderr(t, func() { err = parkCappedReview([]string{root}, []string{"2026-01-01-gone"}, 3) })
	if err == nil || !strings.Contains(err.Error(), "capped review task 2026-01-01-gone") {
		t.Fatalf("failed block = %v, want the task's error", err)
	}
	if out != "" {
		t.Errorf("a failed block still reported:\n%s", out)
	}
}

func TestBlockReopenedTasksUsesCompletionAuthority(t *testing.T) {
	q := filepath.Join(t.TempDir(), ".agent", "tasks")
	task := taskForLease(t, q, stateDone, "review-reopen")
	writeTaskFile(t, filepath.Join(task.Dir, "task.md"), "# Task\n- [x] required checks passed\n")
	if err := completeTrustedTask("", q, task); err != nil {
		t.Fatal(err)
	}
	if err := blockReopenedTasks([]string{q}, []string{task.ID}, 3); err != nil {
		t.Fatal(err)
	}
	if decision, err := os.ReadFile(filepath.Join(q, stateBlocked, task.ID, "decision.md")); err != nil {
		t.Fatal("completed review reopen was not authoritatively blocked")
	} else if !strings.Contains(string(decision), "No other task was left in the queue.") {
		t.Fatalf("review cap decision on an empty queue:\n%s", decision)
	}

	leased := taskForLease(t, q, stateInProgress, "leased-reopen")
	authority, err := openLeaseAuthority(q, leased.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	defer authority.Close()
	if err := syscall.Flock(int(authority.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(authority.Fd()), syscall.LOCK_UN)
	if err := blockReopenedTasks([]string{q}, []string{leased.ID}, 3); err == nil ||
		!strings.Contains(err.Error(), "leased by another controller") {
		t.Fatalf("block leased task = %v, want authority error", err)
	}
	if !pathExists(filepath.Join(q, stateInProgress, leased.ID)) {
		t.Fatal("failed authoritative block moved the leased task")
	}
}
