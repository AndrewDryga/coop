package loop

import (
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
	if got := completedReviewSubjects([]string{root}, completed); !slices.Equal(got, []string{"accepted"}) {
		t.Fatalf("completed review subjects = %v, want host-accepted archived task only", got)
	}
}

func TestBlockReopenedTasksLeavesUnrelatedActionableWork(t *testing.T) {
	q := filepath.Join(t.TempDir(), ".agent", "tasks")
	writeTaskFile(t, filepath.Join(q, stateInProgress, "review-reopen", "task.md"), "# Reopen\n")
	writeTaskFile(t, filepath.Join(q, stateInProgress, "unrelated", "task.md"), "# Unrelated\n")

	if err := blockReopenedTasks([]string{q}, []string{"review-reopen"}, 3); err != nil {
		t.Fatal(err)
	}

	if !pathExists(filepath.Join(q, stateBlocked, "review-reopen")) {
		t.Fatal("exact review reopen was not blocked")
	}
	if !pathExists(filepath.Join(q, stateInProgress, "unrelated")) {
		t.Fatal("unrelated actionable task was moved by the signoff cap")
	}
}

func TestBlockReopenedTasksUsesCompletionAuthority(t *testing.T) {
	q := filepath.Join(t.TempDir(), ".agent", "tasks")
	task := taskForLease(t, q, stateDone, "review-reopen")
	if err := completeTrustedTask(q, task); err != nil {
		t.Fatal(err)
	}
	if err := blockReopenedTasks([]string{q}, []string{task.ID}, 3); err != nil {
		t.Fatal(err)
	}
	if !pathExists(filepath.Join(q, stateBlocked, task.ID, "decision.md")) {
		t.Fatal("completed review reopen was not authoritatively blocked")
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
