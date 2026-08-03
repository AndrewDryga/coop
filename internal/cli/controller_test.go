package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
)

func TestFinishedTasksAndReconcileDecision(t *testing.T) {
	// reconcileMerged: a landed todo/in_progress task moves; a landed blocked task is flagged (no
	// move); an unlanded task is ignored entirely.
	states := map[string]string{"todo1": stateTodo, "wip1": stateInProgress, "blk1": stateBlocked, "safe": stateTodo}
	landed := map[string]bool{"todo1": true, "wip1": true, "blk1": true} // "safe" did NOT land
	acts := reconcileMerged(states, landed)
	got := map[string]bool{}
	for _, a := range acts {
		got[a.ID] = a.Move
	}
	if len(acts) != 3 || !got["todo1"] || !got["wip1"] || got["blk1"] {
		t.Errorf("reconcileMerged = %+v; want todo1/wip1 move, blk1 flagged, safe absent", acts)
	}
	if _, present := got["safe"]; present {
		t.Error("an unlanded task must not be reconciled")
	}
}

func TestAggregateDuplicateTaskIDs(t *testing.T) {
	first, second := filepath.Join(t.TempDir(), "first"), filepath.Join(t.TempDir(), "second")
	writeTaskFile(t, filepath.Join(first, stateTodo, "actionable", "task.md"), "# actionable\n")
	writeTaskFile(t, filepath.Join(second, stateDone, "actionable", "task.md"), "# actionable archive\n")
	for _, root := range []string{first, second} {
		writeTaskFile(t, filepath.Join(root, stateBlocked, "blocked", "task.md"), "# blocked\n")
		writeTaskFile(t, filepath.Join(root, stateBlocked, "blocked", "decision.md"), "# decision\n")
		writeTaskFile(t, filepath.Join(root, stateDone, "archived", "task.md"), "# archived\n")
	}
	hosts := []string{first, second}
	if got, want := aggregateDuplicateTaskIDs(hosts), []string{"actionable", "archived", "blocked"}; !slices.Equal(got, want) {
		t.Fatalf("aggregate duplicate ids = %v, want %v", got, want)
	}
	if got, want := nonArchivedDuplicateTaskIDs(hosts), []string{"actionable", "blocked"}; !slices.Equal(got, want) {
		t.Fatalf("non-archived duplicate ids = %v, want %v", got, want)
	}
}

func TestCompletionWindowAndRestoreRespectForeignLease(t *testing.T) {
	root := t.TempDir()
	old := taskForLease(t, root, stateDone, "old")
	assigned := taskForLease(t, root, stateInProgress, "assigned")
	rogue := taskForLease(t, root, stateInProgress, "rogue")
	spoofed := taskForLease(t, root, stateInProgress, "spoofed")
	foreign := taskForLease(t, root, stateInProgress, "foreign")
	finalized := taskForLease(t, root, stateInProgress, "finalized")
	foreignLease, _, err := tryTaskLease(root, foreign, testLeaseOwner())
	if err != nil || foreignLease == nil {
		t.Fatalf("foreign lease = %v, %v", foreignLease, err)
	}
	t.Cleanup(func() { _ = foreignLease.release() })
	finalizedLease, _, err := tryTaskLease(root, finalized, testLeaseOwner())
	if err != nil || finalizedLease == nil {
		t.Fatalf("finalized lease = %v, %v", finalizedLease, err)
	}
	windows, err := beginCompletionWindows([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = windows.close() })
	for _, task := range []taskItem{assigned, rogue, spoofed, foreign, finalized} {
		if err := moveTaskDir(root, task, stateDone); err != nil {
			t.Fatal(err)
		}
	}
	if err := normalizeCompletedTaskState(spoofed.ID, filepath.Join(root, stateDone, spoofed.ID)); err != nil {
		t.Fatal(err)
	}
	finalizedDir := filepath.Join(root, stateDone, finalized.ID)
	if err := finalizeQueuedCompletion(queuedTask{Root: root, Item: taskItem{ID: finalized.ID, Dir: finalizedDir, State: stateDone}}); err != nil {
		t.Fatal(err)
	}
	if err := finalizedLease.markCompleted(finalizedDir); err != nil {
		t.Fatal(err)
	}
	if err := finalizedLease.release(); err != nil {
		t.Fatal(err)
	}
	completed, err := windows.candidates()
	if err != nil {
		t.Fatal(err)
	}
	gotIDs := make([]string, len(completed))
	for i := range completed {
		gotIDs[i] = completed[i].Item.ID
	}
	if want := []string{"assigned", "finalized", "foreign", "rogue", "spoofed"}; !slices.Equal(gotIDs, want) {
		t.Fatalf("window completions = %v, want %v", gotIDs, want)
	}
	rejected, err := rejectUnownedCompletions(completed, queuedTask{Root: root, Item: assigned})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rejected, []string{"rogue", "spoofed"}) {
		t.Fatalf("rejected unowned completions = %v, want rogue and spoofed", rejected)
	}
	if !pathExists(filepath.Join(root, stateDone, assigned.ID)) {
		t.Fatal("unowned scan touched this controller's assigned completion")
	}
	if !pathExists(filepath.Join(root, stateInProgress, rogue.ID)) || pathExists(filepath.Join(root, stateDone, rogue.ID)) {
		t.Error("unowned completion was not restored")
	}
	if !pathExists(filepath.Join(root, stateInProgress, spoofed.ID)) || pathExists(filepath.Join(root, stateDone, spoofed.ID)) {
		t.Error("provider-writable finalized state bypassed unowned completion rejection")
	}
	if !pathExists(filepath.Join(root, stateDone, foreign.ID)) {
		t.Fatal("restore stole a completion from its foreign lease owner")
	}
	if !pathExists(filepath.Join(root, stateDone, finalized.ID)) {
		t.Fatal("restore stole an already-finalized completion from another controller")
	}
	if !pathExists(filepath.Join(root, stateDone, old.ID)) {
		t.Fatal("restore touched a task that was already done before the iteration")
	}
}

func TestCompletionWindowDoesNotRedetectDuplicateArchives(t *testing.T) {
	first, second := filepath.Join(t.TempDir(), "first"), filepath.Join(t.TempDir(), "second")
	for _, root := range []string{first, second} {
		writeTaskFile(t, filepath.Join(root, stateDone, "same-id", "task.md"), "# archive\n")
	}
	hosts := []string{first, second}
	windows, err := beginCompletionWindows(hosts)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.close()
	if completed, err := windows.candidates(); err != nil || len(completed) != 0 {
		t.Fatalf("duplicate archives redetected as new = %#v, %v", completed, err)
	}
}

func TestCompletionWindowDetectsNewArchiveAndReceiptClearedSamePath(t *testing.T) {
	root := t.TempDir()
	legacy := taskForLease(t, root, stateDone, "legacy")
	if err := completeTrustedTask(root, legacy); err != nil {
		t.Fatal(err)
	}
	windows, err := beginCompletionWindows([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer windows.close()

	rogue := taskForLease(t, root, stateInProgress, "rogue-in-window")
	if err := moveTaskDir(root, rogue, stateDone); err != nil {
		t.Fatal(err)
	}
	if err := moveTaskDir(root, legacy, stateInProgress); err != nil {
		t.Fatal(err)
	}
	if err := clearTaskCompletionReceipt(root, legacy.ID); err != nil {
		t.Fatal(err)
	}
	returned := taskItem{ID: legacy.ID, Dir: filepath.Join(root, stateInProgress, legacy.ID), State: stateInProgress}
	if err := moveTaskDir(root, returned, stateDone); err != nil {
		t.Fatal(err)
	}
	candidates, err := windows.candidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 {
		t.Fatalf("completion window candidates = %#v, want two", candidates)
	}
	got := []string{candidates[0].Item.ID, candidates[1].Item.ID}
	slices.Sort(got)
	if want := []string{legacy.ID, rogue.ID}; !slices.Equal(got, want) {
		t.Fatalf("completion window candidates = %v, want %v", got, want)
	}
}

func TestCompletionWindowDetectsReplacedArchiveAtSamePath(t *testing.T) {
	root := t.TempDir()
	archived := taskForLease(t, root, stateDone, "replaced-archive")
	windows, err := beginCompletionWindows([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer windows.close()

	displaced := filepath.Join(root, "displaced-archive")
	if err := os.Rename(archived.Dir, displaced); err != nil {
		t.Fatal(err)
	}
	writeTaskFile(t, filepath.Join(archived.Dir, "task.md"), "# replacement\n")
	candidates, err := windows.candidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].Item.ID != archived.ID {
		t.Fatalf("replacement candidates = %#v, want %s", candidates, archived.ID)
	}
}

func TestReviewCompletionWindowDetectsNewReceiptGeneration(t *testing.T) {
	root := t.TempDir()
	archived := taskForLease(t, root, stateDone, "recompleted-during-review")
	if err := completeTrustedTask(root, archived); err != nil {
		t.Fatal(err)
	}
	archived, _ = currentTask(root, archived.ID)
	before, ok := taskCompletionReceipt(root, archived)
	if !ok {
		t.Fatal("trusted completion did not record its first receipt")
	}
	windows, err := beginReviewCompletionWindows([]string{root}, []string{archived.ID})
	if err != nil {
		t.Fatal(err)
	}

	if err := moveTrustedTaskFromDone(root, archived, stateInProgress); err != nil {
		t.Fatal(err)
	}
	reopened, _ := currentTask(root, archived.ID)
	if err := completeTrustedTask(root, reopened); err != nil {
		t.Fatal(err)
	}
	recompleted, _ := currentTask(root, archived.ID)
	after, ok := taskCompletionReceipt(root, recompleted)
	if !ok || after.Nonce == before.Nonce {
		t.Fatalf("completion generation was not refreshed: before=%q after=%q", before.Nonce, after.Nonce)
	}
	if _, err := windows.finishReview(); err == nil || !strings.Contains(err.Error(), archived.ID) {
		t.Fatalf("review completion audit = %v, want changed generation failure", err)
	}
	if !pathExists(recompleted.Dir) {
		t.Fatal("review audit restored a completion carrying valid host evidence")
	}
}

func TestReviewCompletionWindowRejectsRawSameInodeOutAndBack(t *testing.T) {
	root := t.TempDir()
	archived := taskForLease(t, root, stateDone, "raw-review-recompletion")
	if err := completeTrustedTask(root, archived); err != nil {
		t.Fatal(err)
	}
	archived, _ = currentTask(root, archived.ID)
	windows, err := beginReviewCompletionWindows([]string{root}, []string{archived.ID})
	if err != nil {
		t.Fatal(err)
	}

	if err := moveTaskDir(root, archived, stateInProgress); err != nil {
		t.Fatal(err)
	}
	reopened, _ := currentTask(root, archived.ID)
	if err := moveTaskDir(root, reopened, stateDone); err != nil {
		t.Fatal(err)
	}
	if _, err := windows.finishReview(); err == nil || !strings.Contains(err.Error(), archived.ID) {
		t.Fatalf("raw out-and-back review audit = %v, want changed generation failure", err)
	}
	if !pathExists(filepath.Join(root, stateInProgress, archived.ID)) || pathExists(filepath.Join(root, stateDone, archived.ID)) {
		t.Fatal("raw same-inode review completion was not restored")
	}
}

func TestWorkCompletionWindowRejectsRawSameInodeOutAndBack(t *testing.T) {
	root := t.TempDir()
	archived := taskForLease(t, root, stateDone, "raw-work-recompletion")
	if err := completeTrustedTask(root, archived); err != nil {
		t.Fatal(err)
	}
	archived, _ = currentTask(root, archived.ID)
	windows, err := beginCompletionWindows([]string{root})
	if err != nil {
		t.Fatal(err)
	}

	if err := moveTaskDir(root, archived, stateInProgress); err != nil {
		t.Fatal(err)
	}
	reopened, _ := currentTask(root, archived.ID)
	if err := moveTaskDir(root, reopened, stateDone); err != nil {
		t.Fatal(err)
	}
	_, rejected, err := windows.auditDoneCandidates(queuedTask{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rejected, []string{archived.ID}) {
		t.Fatalf("work audit rejected %v, want %s", rejected, archived.ID)
	}
	if !pathExists(filepath.Join(root, stateInProgress, archived.ID)) || pathExists(filepath.Join(root, stateDone, archived.ID)) {
		t.Fatal("raw same-inode work completion was not restored")
	}
	if err := windows.close(); err != nil {
		t.Fatal(err)
	}
}

func TestWorkCompletionWindowWaitsToInvalidateStaleReceipt(t *testing.T) {
	root := t.TempDir()
	archived := taskForLease(t, root, stateDone, "contended-work-recompletion")
	if err := completeTrustedTask(root, archived); err != nil {
		t.Fatal(err)
	}
	archived, _ = currentTask(root, archived.ID)
	windows, err := beginCompletionWindows([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if err := moveTaskDir(root, archived, stateInProgress); err != nil {
		t.Fatal(err)
	}
	reopened, _ := currentTask(root, archived.ID)
	if err := moveTaskDir(root, reopened, stateDone); err != nil {
		t.Fatal(err)
	}
	authority, err := openLeaseAuthority(root, archived.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(authority.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	type auditResult struct {
		rejected []string
		err      error
	}
	done := make(chan auditResult, 1)
	go func() {
		_, rejected, err := windows.auditDoneCandidates(queuedTask{})
		done <- auditResult{rejected: rejected, err: err}
	}()
	select {
	case result := <-done:
		t.Fatalf("work audit returned while stale-receipt reader held its lock: %+v", result)
	case <-time.After(30 * time.Millisecond):
	}
	if err := unlockLeaseFile(authority); err != nil {
		t.Fatal(err)
	}
	result := <-done
	if result.err != nil || !slices.Equal(result.rejected, []string{archived.ID}) {
		t.Fatalf("contended work audit = rejected %v err %v", result.rejected, result.err)
	}
	if !pathExists(filepath.Join(root, stateInProgress, archived.ID)) {
		t.Fatal("contended stale receipt let a raw completion remain done")
	}
	if err := windows.close(); err != nil {
		t.Fatal(err)
	}
}

func TestWorkCompletionWindowWaitsForTransientLocalLeaseReader(t *testing.T) {
	root := t.TempDir()
	rogue := taskForLease(t, root, stateInProgress, "local-reader-completion")
	if _, err := taskLeaseDir(rogue.Dir); err != nil {
		t.Fatal(err)
	}
	local, err := openLeaseLock(rogue.Dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(local.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	windows, err := beginCompletionWindows([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if err := moveTaskDir(root, rogue, stateDone); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := windows.auditDoneCandidates(queuedTask{})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("work audit returned while the local reader held its lock: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if err := unlockLeaseFile(local); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !pathExists(filepath.Join(root, stateInProgress, rogue.ID)) {
		t.Fatal("transient local lock let an unowned completion remain done")
	}
	if err := windows.close(); err != nil {
		t.Fatal(err)
	}
}

func TestWorkCompletionWindowRejectsArchivedTaskDeparture(t *testing.T) {
	root := t.TempDir()
	archived := taskForLease(t, root, stateDone, "raw-work-reopen")
	windows, err := beginCompletionWindows([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if err := moveTaskDir(root, archived, stateInProgress); err != nil {
		t.Fatal(err)
	}
	if err := windows.rejectAndClose(queuedTask{}); err == nil || !strings.Contains(err.Error(), archived.ID) {
		t.Fatalf("work departure audit = %v, want ownership failure", err)
	}
	if !pathExists(filepath.Join(root, stateInProgress, archived.ID)) {
		t.Fatal("work departure audit lost the reopened task")
	}
}

func TestWorkCompletionWindowAcceptsHostReceiptedForeignArchivedDeparture(t *testing.T) {
	root := t.TempDir()
	assigned := taskForLease(t, root, stateInProgress, "work-subject")
	archived := taskForLease(t, root, stateDone, "host-reopened-archive")
	if err := completeTrustedTask(root, archived); err != nil {
		t.Fatal(err)
	}
	archived, _ = currentTask(root, archived.ID)
	windows, err := beginWorkCompletionWindows([]string{root}, assigned.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := moveTrustedTaskFromDone(root, archived, stateInProgress); err != nil {
		t.Fatal(err)
	}
	if departed, err := windows.departures(); err != nil || len(departed) != 0 {
		t.Fatalf("host-receipted foreign departure = %v, %v", departed, err)
	}
	if err := windows.rejectAndClose(queuedTask{Root: root, Item: assigned}); err != nil {
		t.Fatalf("host-receipted foreign departure rejected: %v", err)
	}
}

func TestWorkCompletionWindowRejectsSubjectAndWrongDepartureRecord(t *testing.T) {
	t.Run("subject", func(t *testing.T) {
		root := t.TempDir()
		assigned := taskForLease(t, root, stateDone, "work-subject-departure")
		if err := completeTrustedTask(root, assigned); err != nil {
			t.Fatal(err)
		}
		assigned, _ = currentTask(root, assigned.ID)
		windows, err := beginWorkCompletionWindows([]string{root}, assigned.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := moveTrustedTaskFromDone(root, assigned, stateInProgress); err != nil {
			t.Fatal(err)
		}
		if err := windows.rejectAndClose(queuedTask{}); err == nil || !strings.Contains(err.Error(), assigned.ID) {
			t.Fatalf("subject departure audit = %v, want rejection", err)
		}
	})
	t.Run("wrong nonce", func(t *testing.T) {
		root := t.TempDir()
		assigned := taskForLease(t, root, stateInProgress, "work-subject-wrong-nonce")
		archived := taskForLease(t, root, stateDone, "wrong-nonce-archive")
		if err := completeTrustedTask(root, archived); err != nil {
			t.Fatal(err)
		}
		archived, _ = currentTask(root, archived.ID)
		windows, err := beginWorkCompletionWindows([]string{root}, assigned.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := moveTaskDir(root, archived, stateInProgress); err != nil {
			t.Fatal(err)
		}
		if err := writeTrustedDoneDeparture(root, trustedDoneDeparture{
			Version: trustedDoneDepartureVersion, TaskID: archived.ID, Nonces: []string{strings.Repeat("0", 32)},
		}); err != nil {
			t.Fatal(err)
		}
		if err := windows.rejectAndClose(queuedTask{}); err == nil || !strings.Contains(err.Error(), archived.ID) {
			t.Fatalf("wrong departure record audit = %v, want rejection", err)
		}
	})
}

func TestWorkCompletionWindowReplayAcceptsHostReceiptedForeignDeparture(t *testing.T) {
	root := t.TempDir()
	assigned := taskForLease(t, root, stateInProgress, "replay-work-subject")
	archived := taskForLease(t, root, stateDone, "replay-host-reopened")
	if err := completeTrustedTask(root, archived); err != nil {
		t.Fatal(err)
	}
	archived, _ = currentTask(root, archived.ID)
	windows, err := beginWorkCompletionWindows([]string{root}, assigned.ID)
	if err != nil {
		t.Fatal(err)
	}
	windowID := windows.windows[0].id
	if err := moveTrustedTaskFromDone(root, archived, stateInProgress); err != nil {
		t.Fatal(err)
	}
	if err := unlockLeaseFile(windows.windows[0].live); err != nil {
		t.Fatal(err)
	}
	windows.windows[0].live = nil
	if err := reconcileCompletionWindows([]string{root}); err != nil {
		t.Fatalf("replay host departure: %v", err)
	}
	index, err := readCompletionWindowIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := index.Windows[windowID]; ok {
		t.Fatal("replay retained a settled work window")
	}
}

func TestReviewCompletionWindowRejectsInPlaceArchiveMutation(t *testing.T) {
	root := t.TempDir()
	archived := taskForLease(t, root, stateDone, "mutated-review-archive")
	if err := completeTrustedTask(root, archived); err != nil {
		t.Fatal(err)
	}
	archived, _ = currentTask(root, archived.ID)
	windows, err := beginReviewCompletionWindows([]string{root}, []string{archived.ID})
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(archived.Dir, "state.md")
	state, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	state[0] = '!'
	if err := os.WriteFile(statePath, state, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := windows.finishReview(); err == nil || !strings.Contains(err.Error(), archived.ID) {
		t.Fatalf("in-place archive mutation audit = %v, want changed generation failure", err)
	}
	if !pathExists(filepath.Join(root, stateInProgress, archived.ID)) {
		t.Fatal("in-place review mutation was not restored for inspection")
	}
}

func TestReviewCompletionWindowRejectsDirectTaskReopen(t *testing.T) {
	root := t.TempDir()
	archived := taskForLease(t, root, stateDone, "direct-review-reopen")
	if err := completeTrustedTask(root, archived); err != nil {
		t.Fatal(err)
	}
	archived, _ = currentTask(root, archived.ID)
	windows, err := beginReviewCompletionWindows([]string{root}, []string{archived.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := moveTaskDir(root, archived, stateInProgress); err != nil {
		t.Fatal(err)
	}
	reopened, err := windows.finishReview()
	if err == nil || !strings.Contains(err.Error(), "reviews must report verdicts for host application") {
		t.Fatalf("direct review reopen audit = %v, want host-application failure", err)
	}
	if len(reopened) != 0 {
		t.Fatalf("direct review reopen was accepted as host-applied: %v", reopened)
	}
	if !pathExists(filepath.Join(root, stateInProgress, archived.ID)) {
		t.Fatal("rejected direct review reopen lost the actionable task")
	}
}

func TestReviewCompletionWindowRejectsReplacedSubject(t *testing.T) {
	root := t.TempDir()
	subject := taskForLease(t, root, stateDone, "replaced-review-subject")
	if err := completeTrustedTask(root, subject); err != nil {
		t.Fatal(err)
	}
	subject, _ = currentTask(root, subject.ID)
	windows, err := beginReviewCompletionWindows([]string{root}, []string{subject.ID})
	if err != nil {
		t.Fatal(err)
	}
	displaced := filepath.Join(root, "displaced-review-subject")
	if err := os.Rename(subject.Dir, displaced); err != nil {
		t.Fatal(err)
	}
	writeTaskFile(t, filepath.Join(subject.Dir, "task.md"), "# replacement\n")
	concurrent, err := windows.finishReview()
	if err == nil || !strings.Contains(err.Error(), subject.ID) {
		t.Fatalf("replaced subject audit = %v, want subject failure", err)
	}
	if len(concurrent) != 0 {
		t.Fatalf("replaced subject was reported as concurrent host activity: %v", concurrent)
	}
}

func TestReviewCompletionWindowRejectsDeletedSubject(t *testing.T) {
	root := t.TempDir()
	subject := taskForLease(t, root, stateDone, "deleted-review-subject")
	windows, err := beginReviewCompletionWindows([]string{root}, []string{subject.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(subject.Dir); err != nil {
		t.Fatal(err)
	}
	concurrent, err := windows.finishReview()
	if err == nil || !strings.Contains(err.Error(), subject.ID) {
		t.Fatalf("deleted subject audit = %v, want subject failure", err)
	}
	if len(concurrent) != 0 {
		t.Fatalf("deleted subject was reported as concurrent host activity: %v", concurrent)
	}
}

// TestReviewCompletionWindowReportsConcurrentHostCompletion: a parallel host session finishing an
// UNRELATED task (`coop tasks done`) while a review of another exact subject runs is concurrent
// host activity — the audit reports it for the next review round instead of killing the run, and
// the completion keeps its host receipt.
func TestReviewCompletionWindowReportsConcurrentHostCompletion(t *testing.T) {
	root := t.TempDir()
	subject := taskForLease(t, root, stateDone, "review-subject")
	if err := completeTrustedTask(root, subject); err != nil {
		t.Fatal(err)
	}
	subject, _ = currentTask(root, subject.ID)
	foreign := taskForLease(t, root, stateTodo, "foreign-host-completion")
	windows, err := beginReviewCompletionWindows([]string{root}, []string{subject.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := completeTrustedTask(root, foreign); err != nil {
		t.Fatal(err)
	}
	concurrent, err := windows.finishReview()
	if err != nil {
		t.Fatalf("concurrent host completion killed the review audit: %v", err)
	}
	if !slices.Equal(concurrent, []string{foreign.ID}) {
		t.Fatalf("concurrent completions = %v, want [%s]", concurrent, foreign.ID)
	}
	completed, ok := currentTask(root, foreign.ID)
	if !ok || completed.State != stateDone {
		t.Fatalf("foreign host completion = %+v, want done", completed)
	}
	if !taskCompletionRecorded(root, completed) {
		t.Fatal("tolerated foreign completion lost its host receipt")
	}
	if kept, _ := currentTask(root, subject.ID); kept.State != stateDone {
		t.Fatal("review subject was disturbed by concurrent host activity")
	}
}

func TestReviewCompletionWindowReportsCompletionAfterInitialAudit(t *testing.T) {
	root := t.TempDir()
	subject := taskForLease(t, root, stateDone, "handoff-review-subject")
	foreign := taskForLease(t, root, stateTodo, "handoff-host-completion")
	windows, err := beginReviewCompletionWindows([]string{root}, []string{subject.ID})
	if err != nil {
		t.Fatal(err)
	}
	scans := 0
	windows.scan = func(root string, baseline map[string]completionFingerprint) ([]queuedTask, error) {
		scans++
		if scans == 2 {
			if err := completeTrustedTask(root, foreign); err != nil {
				return nil, err
			}
		}
		return changedDoneCompletions(root, baseline)
	}
	concurrent, err := windows.finishReview()
	if err != nil {
		t.Fatalf("post-close host completion killed review: %v", err)
	}
	if scans != 2 || !slices.Equal(concurrent, []string{foreign.ID}) {
		t.Fatalf("post-close audit = scans %d concurrent %v, want 2 and [%s]", scans, concurrent, foreign.ID)
	}
}

func TestReviewCompletionWindowRejectsSubjectIDThatBecomesAmbiguous(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	subject := taskForLease(t, first, stateDone, "ambiguous-review-subject")
	windows, err := beginReviewCompletionWindows([]string{first, second}, []string{subject.ID})
	if err != nil {
		t.Fatal(err)
	}
	taskForLease(t, second, stateTodo, subject.ID)
	concurrent, err := windows.finishReview()
	if err == nil || !strings.Contains(err.Error(), "became ambiguous") || !strings.Contains(err.Error(), subject.ID) {
		t.Fatalf("ambiguous review subject audit = %v, want task-specific failure", err)
	}
	if len(concurrent) != 0 {
		t.Fatalf("ambiguous review subject reported concurrent completions: %v", concurrent)
	}
}

// TestReviewCompletionWindowSubjectStaysStrictBesideConcurrentActivity: tolerating foreign
// completions must not weaken subject tamper detection — a mutated subject still fails closed in
// the same window, and the audit reports no concurrent activity on the failure path.
func TestReviewCompletionWindowSubjectStaysStrictBesideConcurrentActivity(t *testing.T) {
	root := t.TempDir()
	subject := taskForLease(t, root, stateDone, "strict-review-subject")
	if err := completeTrustedTask(root, subject); err != nil {
		t.Fatal(err)
	}
	subject, _ = currentTask(root, subject.ID)
	foreign := taskForLease(t, root, stateTodo, "foreign-beside-subject")
	windows, err := beginReviewCompletionWindows([]string{root}, []string{subject.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := completeTrustedTask(root, foreign); err != nil {
		t.Fatal(err)
	}
	writeTaskFile(t, filepath.Join(subject.Dir, "task.md"), "# tampered subject\n")
	concurrent, err := windows.finishReview()
	if err == nil || !strings.Contains(err.Error(), subject.ID) {
		t.Fatalf("tampered subject audit = %v, want subject failure", err)
	}
	if len(concurrent) != 0 {
		t.Fatalf("failed review audit still reported concurrent completions: %v", concurrent)
	}
	if done, _ := currentTask(root, foreign.ID); done.State != stateDone {
		t.Fatal("subject tampering revoked an unrelated host completion")
	}
}

// TestReviewCompletionWindowRejectsRawNonSubjectCompletion: subject scoping tolerates only
// completions a host authority finalized — a raw folder move into done (the review box completing
// a task itself) is still rejected and restored even though the task is not a review subject.
func TestReviewCompletionWindowRejectsRawNonSubjectCompletion(t *testing.T) {
	root := t.TempDir()
	subject := taskForLease(t, root, stateDone, "raw-review-subject")
	if err := completeTrustedTask(root, subject); err != nil {
		t.Fatal(err)
	}
	rogue := taskForLease(t, root, stateTodo, "raw-review-completion")
	windows, err := beginReviewCompletionWindows([]string{root}, []string{subject.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := moveTaskDir(root, rogue, stateDone); err != nil {
		t.Fatal(err)
	}
	concurrent, err := windows.finishReview()
	if err == nil || !strings.Contains(err.Error(), rogue.ID) {
		t.Fatalf("raw non-subject completion audit = %v, want ownership failure", err)
	}
	if len(concurrent) != 0 {
		t.Fatalf("unowned completion was reported as concurrent host activity: %v", concurrent)
	}
	if !pathExists(filepath.Join(root, stateInProgress, rogue.ID)) || pathExists(filepath.Join(root, stateDone, rogue.ID)) {
		t.Fatal("raw non-subject completion was not restored")
	}
}

// TestReviewCompletionWindowWithoutSubjectsStaysStrict: concurrent-activity tolerance exists only
// under an explicit subject contract — a review window opened with NO subjects (preflight, a
// subject-free verify) must fail closed on any completion, even a host-receipted one, instead of
// reading the empty set as blanket permission to absorb queue churn.
func TestReviewCompletionWindowWithoutSubjectsStaysStrict(t *testing.T) {
	root := t.TempDir()
	foreign := taskForLease(t, root, stateTodo, "foreign-without-subjects")
	windows, err := beginReviewCompletionWindows([]string{root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := completeTrustedTask(root, foreign); err != nil {
		t.Fatal(err)
	}
	concurrent, err := windows.finishReview()
	if err == nil || !strings.Contains(err.Error(), foreign.ID) {
		t.Fatalf("subject-free review audit = %v, want strict completion failure", err)
	}
	if len(concurrent) != 0 {
		t.Fatalf("subject-free review reported concurrent completions: %v", concurrent)
	}
}

func TestTrustedTaskReopenRollsBackMetadataAndReceipt(t *testing.T) {
	injected := errors.New("injected metadata failure")
	cases := []struct {
		name     string
		complete bool
		update   func(string) error
	}{
		{
			name:     "failure after log write",
			complete: true,
			update: func(dir string) error {
				if err := appendTaskLogStrict(dir, "partial review note"); err != nil {
					return err
				}
				return injected
			},
		},
		{
			name:     "failure after state write",
			complete: true,
			update: func(dir string) error {
				if err := normalizeTaskState(
					"atomic-review-reopen",
					dir,
					"reopened",
					"partial state",
					"partial done",
					"partial trap",
				); err != nil {
					return err
				}
				return injected
			},
		},
		{
			name: "new metadata files are removed",
			update: func(dir string) error {
				if err := appendTaskLogStrict(dir, "new review note"); err != nil {
					return err
				}
				if err := normalizeTaskState(
					"atomic-review-reopen",
					dir,
					"reopened",
					"new state",
					"new done",
					"new trap",
				); err != nil {
					return err
				}
				return injected
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			task := taskForLease(t, root, stateDone, "atomic-review-reopen")
			if tc.complete {
				writeTaskFile(t, filepath.Join(task.Dir, "log.md"), "# original log\n")
				writeTaskFile(t, filepath.Join(task.Dir, "state.md"),
					"# State\n\n**Status:** complete\n**Done so far:** original\n**Next action:** none\n**Traps:** original\n")
				if err := completeTrustedTask(root, task); err != nil {
					t.Fatal(err)
				}
				task, _ = currentTask(root, task.ID)
			}
			beforeFiles := map[string][]byte{}
			beforeExists := map[string]bool{}
			for _, name := range []string{"log.md", "state.md"} {
				body, err := os.ReadFile(filepath.Join(task.Dir, name))
				if err == nil {
					beforeFiles[name], beforeExists[name] = body, true
				} else if !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
			}
			beforeReceipt, beforeReceiptOK := taskCompletionReceipt(root, task)

			err := moveTrustedTaskFromDoneWith(root, task, stateInProgress, tc.update)
			if !errors.Is(err, injected) {
				t.Fatalf("trusted reopen error = %v, want injected failure", err)
			}
			current, ok := currentTask(root, task.ID)
			if !ok || current.State != stateDone || current.Dir != task.Dir {
				t.Fatalf("task after rollback = %+v, %v; want original done task", current, ok)
			}
			if pathExists(filepath.Join(root, stateInProgress, task.ID)) {
				t.Fatal("failed host reopen left an in-progress task")
			}
			for _, name := range []string{"log.md", "state.md"} {
				body, err := os.ReadFile(filepath.Join(current.Dir, name))
				if beforeExists[name] {
					if err != nil || string(body) != string(beforeFiles[name]) {
						t.Fatalf("%s after rollback = %q, %v; want %q", name, body, err, beforeFiles[name])
					}
				} else if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("%s was created by failed reopen: %v", name, err)
				}
			}
			afterReceipt, afterReceiptOK := taskCompletionReceipt(root, current)
			if afterReceiptOK != beforeReceiptOK || (afterReceiptOK && afterReceipt != beforeReceipt) {
				t.Fatalf("completion receipt after rollback = %+v/%v, want %+v/%v",
					afterReceipt, afterReceiptOK, beforeReceipt, beforeReceiptOK)
			}
			if err := reconcileCompletionWindows([]string{root}); err != nil {
				t.Fatalf("failed reopen left a recovery window: %v", err)
			}
		})
	}
}

func TestTrustedTaskReopenRollsBackEverySubject(t *testing.T) {
	root := t.TempDir()
	var tasks []taskItem
	beforeFiles := map[string]map[string][]byte{}
	beforeReceipts := map[string]leaseCompletionReceipt{}
	for _, id := range []string{"atomic-review-a", "atomic-review-b"} {
		task := taskForLease(t, root, stateDone, id)
		writeTaskFile(t, filepath.Join(task.Dir, "log.md"), "# original "+id+" log\n")
		writeTaskFile(t, filepath.Join(task.Dir, "state.md"),
			"# State\n\n**Status:** complete\n**Done so far:** original\n**Next action:** none\n**Traps:** original\n")
		if err := completeTrustedTask(root, task); err != nil {
			t.Fatal(err)
		}
		task, _ = currentTask(root, id)
		tasks = append(tasks, task)
		beforeFiles[id] = map[string][]byte{}
		for _, name := range []string{"log.md", "state.md"} {
			body, err := os.ReadFile(filepath.Join(task.Dir, name))
			if err != nil {
				t.Fatal(err)
			}
			beforeFiles[id][name] = body
		}
		receipt, ok := taskCompletionReceipt(root, task)
		if !ok {
			t.Fatalf("task %s has no completion receipt", id)
		}
		beforeReceipts[id] = receipt
	}
	injected := errors.New("second task metadata failed")
	moves := []trustedTaskMove{
		{
			root: root, task: tasks[0], newState: stateInProgress,
			afterMove: func(dir string) error {
				return appendTaskLogStrict(dir, "first task partial review")
			},
		},
		{
			root: root, task: tasks[1], newState: stateInProgress,
			afterMove: func(dir string) error {
				if err := appendTaskLogStrict(dir, "second task partial review"); err != nil {
					return err
				}
				return injected
			},
		},
	}
	if err := moveTrustedTasksFromDoneWith(moves); !errors.Is(err, injected) {
		t.Fatalf("multi-task reopen = %v, want injected failure", err)
	}
	for _, task := range tasks {
		current, ok := currentTask(root, task.ID)
		if !ok || current.State != stateDone {
			t.Fatalf("task %s after rollback = %+v/%v, want done", task.ID, current, ok)
		}
		if pathExists(filepath.Join(root, stateInProgress, task.ID)) {
			t.Fatalf("task %s remained in progress after transaction rollback", task.ID)
		}
		for _, name := range []string{"log.md", "state.md"} {
			body, err := os.ReadFile(filepath.Join(current.Dir, name))
			if err != nil || string(body) != string(beforeFiles[task.ID][name]) {
				t.Fatalf("%s/%s after rollback = %q, %v; want %q",
					task.ID, name, body, err, beforeFiles[task.ID][name])
			}
		}
		receipt, ok := taskCompletionReceipt(root, current)
		if !ok || receipt != beforeReceipts[task.ID] {
			t.Fatalf("task %s receipt after rollback = %+v/%v, want %+v",
				task.ID, receipt, ok, beforeReceipts[task.ID])
		}
	}
}

func TestTrustedTaskMoveRollsBackDeclaredMetadata(t *testing.T) {
	root := t.TempDir()
	task := taskForLease(t, root, stateInProgress, "decision-rollback")
	original := []byte("# Original decision\n")
	writeTaskFile(t, filepath.Join(task.Dir, "decision.md"), string(original))
	injected := errors.New("decision callback failed")
	err := moveTrustedTasksFromDoneWith([]trustedTaskMove{{
		root: root, task: taskItem{ID: task.ID}, newState: stateBlocked,
		sourceStates:  []string{stateInProgress},
		metadataNames: []string{"decision.md"},
		afterMove: func(dir string) error {
			if err := os.WriteFile(filepath.Join(dir, "decision.md"), []byte("# Partial replacement\n"), 0o644); err != nil {
				return err
			}
			return injected
		},
	}})
	if !errors.Is(err, injected) {
		t.Fatalf("trusted move = %v, want injected callback failure", err)
	}
	current, ok := currentTask(root, task.ID)
	if !ok || current.State != stateInProgress {
		t.Fatalf("task after rollback = %+v/%v, want in progress", current, ok)
	}
	body, err := os.ReadFile(filepath.Join(current.Dir, "decision.md"))
	if err != nil || string(body) != string(original) {
		t.Fatalf("decision after rollback = %q, %v; want %q", body, err, original)
	}
}

func TestStaleReceiptClearDoesNotEraseFreshGeneration(t *testing.T) {
	root := t.TempDir()
	archived := taskForLease(t, root, stateDone, "fresh-receipt")
	if err := completeTrustedTask(root, archived); err != nil {
		t.Fatal(err)
	}
	archived, _ = currentTask(root, archived.ID)
	old, ok := taskCompletionReceipt(root, archived)
	if !ok {
		t.Fatal("trusted completion did not record its first receipt")
	}
	if err := completeTrustedTask(root, archived); err != nil {
		t.Fatal(err)
	}
	archived, _ = currentTask(root, archived.ID)
	fresh, ok := taskCompletionReceipt(root, archived)
	if !ok || fresh.Nonce == old.Nonce {
		t.Fatalf("trusted recompletion did not publish a fresh nonce: old=%q fresh=%q", old.Nonce, fresh.Nonce)
	}
	cleared, err := clearTaskCompletionReceiptIfMatches(root, archived, old.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	if cleared {
		t.Fatal("stale generation unexpectedly cleared a fresh receipt")
	}
	got, ok := taskCompletionReceipt(root, archived)
	if !ok || got.Nonce != fresh.Nonce {
		t.Fatalf("fresh receipt changed after stale clear: got=%q want=%q", got.Nonce, fresh.Nonce)
	}
}

func TestCompletionWindowAuditsBusyReceiptBaseline(t *testing.T) {
	root := t.TempDir()
	archived := taskForLease(t, root, stateDone, "busy-baseline")
	if err := completeTrustedTask(root, archived); err != nil {
		t.Fatal(err)
	}
	archived, _ = currentTask(root, archived.ID)
	authority, err := openLeaseAuthority(root, archived.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(authority.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if err := clearLeaseCompletionReceipt(authority); err != nil {
		t.Fatal(err)
	}
	windows, err := beginCompletionWindows([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if err := unlockLeaseFile(authority); err != nil {
		t.Fatal(err)
	}
	if err := moveTaskDir(root, archived, stateInProgress); err != nil {
		t.Fatal(err)
	}
	reopened, _ := currentTask(root, archived.ID)
	if err := moveTaskDir(root, reopened, stateDone); err != nil {
		t.Fatal(err)
	}
	if err := windows.rejectAndClose(queuedTask{}); err == nil || !strings.Contains(err.Error(), archived.ID) {
		t.Fatalf("busy-baseline completion audit = %v, want ownership failure", err)
	}
	if !pathExists(filepath.Join(root, stateInProgress, archived.ID)) {
		t.Fatal("busy-baseline unowned completion was not restored")
	}
}

func TestTrustedReopenMoveFailureRestoresExactReceipt(t *testing.T) {
	root := t.TempDir()
	archived := taskForLease(t, root, stateDone, "failed-trusted-reopen")
	if err := completeTrustedTask(root, archived); err != nil {
		t.Fatal(err)
	}
	archived, _ = currentTask(root, archived.ID)
	before, ok := taskCompletionReceipt(root, archived)
	if !ok {
		t.Fatal("trusted completion did not record its receipt")
	}
	windows, err := beginCompletionWindows([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, stateInProgress), []byte("obstruction\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := moveTrustedTaskFromDone(root, archived, stateInProgress); err == nil {
		t.Fatal("trusted reopen unexpectedly moved through a non-directory destination")
	}
	after, ok := taskCompletionReceipt(root, archived)
	if !ok || after.Nonce != before.Nonce {
		t.Fatalf("failed trusted reopen changed receipt: before=%q after=%q", before.Nonce, after.Nonce)
	}
	if candidates, err := windows.candidates(); err != nil || len(candidates) != 0 {
		t.Fatalf("failed trusted reopen looked like a completion generation: %#v, %v", candidates, err)
	}
	if err := windows.close(); err != nil {
		t.Fatal(err)
	}
}

func TestTrustedReopenCrashWindowRestoresClearedReceipt(t *testing.T) {
	root := t.TempDir()
	archived := taskForLease(t, root, stateDone, "crashed-trusted-reopen")
	if err := completeTrustedTask(root, archived); err != nil {
		t.Fatal(err)
	}
	archived, _ = currentTask(root, archived.ID)
	windows, err := beginCompletionWindowsAllowing([]string{root}, archived.ID)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := openLeaseAuthority(root, archived.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(authority.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if err := clearLeaseCompletionReceipt(authority); err != nil {
		t.Fatal(err)
	}
	if err := unlockLeaseFile(authority); err != nil {
		t.Fatal(err)
	}
	// Simulate death after receipt invalidation but before the intended done-to-actionable rename.
	if err := unlockLeaseFile(windows.windows[0].live); err != nil {
		t.Fatal(err)
	}
	windows.windows[0].live = nil
	if err := reconcileCompletionWindows([]string{root}); err != nil {
		t.Fatal(err)
	}
	if !pathExists(filepath.Join(root, stateInProgress, archived.ID)) || pathExists(filepath.Join(root, stateDone, archived.ID)) {
		t.Fatal("crash-left trusted reopen was not recovered to actionable")
	}
}

func TestCompletionWindowScanFailureKeepsJournalForReplay(t *testing.T) {
	root := t.TempDir()
	rogue := taskForLease(t, root, stateInProgress, "scan-failure-replay")
	windows, err := beginCompletionWindows([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	windowID := windows.windows[0].id
	if err := moveTaskDir(root, rogue, stateDone); err != nil {
		t.Fatal(err)
	}
	windows.scan = func(string, map[string]completionFingerprint) ([]queuedTask, error) {
		return nil, errors.New("injected completion scan failure")
	}
	if err := windows.rejectAndClose(queuedTask{}); err == nil || !strings.Contains(err.Error(), "injected completion scan failure") {
		t.Fatalf("failed completion audit = %v", err)
	}
	index, err := readCompletionWindowIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := index.Windows[windowID]; !ok {
		t.Fatal("failed completion scan deleted its durable replay journal")
	}
	if err := reconcileCompletionWindows([]string{root}); err != nil {
		t.Fatal(err)
	}
	if !pathExists(filepath.Join(root, stateInProgress, rogue.ID)) || pathExists(filepath.Join(root, stateDone, rogue.ID)) {
		t.Fatal("startup replay did not restore the completion left by the failed scan")
	}
}

func TestCompletionWindowCloseRemovesLivenessLock(t *testing.T) {
	root := t.TempDir()
	windows, err := beginCompletionWindows([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	windowID := windows.windows[0].id
	if err := windows.close(); err != nil {
		t.Fatal(err)
	}
	file, err := openLeaseAuthority(root, completionWindowLockPrefix+windowID, false)
	if file != nil {
		_ = file.Close()
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("closed completion window lock = %v, want not exist", err)
	}
}

func TestCompletionWindowSetupFailureRemovesUnregisteredLivenessLock(t *testing.T) {
	root := t.TempDir()
	indexName, err := completionWindowIndexName(root)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := openLeaseAuthorityRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteTaskFile(registry, indexName, []byte("{not-json\n")); err != nil {
		_ = registry.Close()
		t.Fatal(err)
	}
	_ = registry.Close()
	before, err := os.ReadDir(os.Getenv(testLeaseAuthorityRootEnv))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := beginCompletionWindows([]string{root}); err == nil || !strings.Contains(err.Error(), "decode completion window index") {
		t.Fatalf("completion window setup = %v, want corrupt-index failure", err)
	}
	after, err := os.ReadDir(os.Getenv(testLeaseAuthorityRootEnv))
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("setup failure left %d authority files, want only the stable index lock added to %d", len(after), len(before))
	}
	indexKey, err := leaseAuthorityKey(root, completionWindowIndexID)
	if err != nil {
		t.Fatal(err)
	}
	if after[len(after)-1].Name() != indexKey+".lock" {
		found := false
		for _, entry := range after {
			found = found || entry.Name() == indexKey+".lock"
		}
		if !found {
			t.Fatal("setup failure did not retain the stable index authority lock")
		}
	}
}

func TestRunIterationStopsBeforeLaunchOnCompletionWindowSetupFailure(t *testing.T) {
	root := t.TempDir()
	indexName, err := completionWindowIndexName(root)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := openLeaseAuthorityRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteTaskFile(registry, indexName, []byte("{not-json\n")); err != nil {
		_ = registry.Close()
		t.Fatal(err)
	}
	_ = registry.Close()

	a := &app{}
	code, output, usage, classification, windows, err := a.runIteration(
		context.Background(), t.TempDir(), "must-not-launch", "codex", "", []string{"must-not-launch"},
		false, []string{root}, completionWindowStrict, nil, true, io.Discard, nil, "setup failure", "",
	)
	if code != 1 || !errors.Is(err, errCompletionWindowSetup) || windows != nil || output != "" || usage != nil {
		t.Fatalf("setup-failed iteration = code %d output %q usage %#v windows %#v err %v", code, output, usage, windows, err)
	}
	if classification.outcome != "process_failure" {
		t.Fatalf("setup-failed iteration outcome = %q, want process_failure", classification.outcome)
	}
}

func TestCompletionWindowReplayRejectsCrashLeftUnownedCompletion(t *testing.T) {
	root := t.TempDir()
	rogue := taskForLease(t, root, stateInProgress, "rogue-before-crash")
	windows, err := beginCompletionWindows([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if err := moveTaskDir(root, rogue, stateDone); err != nil {
		t.Fatal(err)
	}
	// Simulate controller death: the kernel releases the live lock, but the durable index remains.
	if err := unlockLeaseFile(windows.windows[0].live); err != nil {
		t.Fatal(err)
	}
	windows.windows[0].live = nil
	if err := reconcileCompletionWindows([]string{root}); err != nil {
		t.Fatalf("crash replay: %v", err)
	}
	if !pathExists(filepath.Join(root, stateInProgress, rogue.ID)) || pathExists(filepath.Join(root, stateDone, rogue.ID)) {
		t.Fatal("crash-left unowned completion was not restored")
	}
	if err := reconcileCompletionWindows([]string{root}); err != nil {
		t.Fatalf("replayed completion window was not removed: %v", err)
	}
}

func TestCompletionWindowReplayLeavesChangedBaselineArchivesDone(t *testing.T) {
	root := t.TempDir()
	trustedArchive := func(id string) taskItem {
		t.Helper()
		task := taskForLease(t, root, stateInProgress, id)
		if err := completeTrustedTask(root, task); err != nil {
			t.Fatal(err)
		}
		task, _ = currentTask(root, id)
		return task
	}
	first := trustedArchive("baseline-first")
	second := trustedArchive("baseline-second")
	rogue := taskForLease(t, root, stateInProgress, "new-unowned-completion")
	firstWindow, err := beginCompletionWindows([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	secondWindow, err := beginCompletionWindows([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	windowIDs := []string{firstWindow.windows[0].id, secondWindow.windows[0].id}
	for _, archived := range []taskItem{first, second} {
		if err := os.WriteFile(filepath.Join(archived.Dir, "late-audit.log"), []byte("changed\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := moveTaskDir(root, rogue, stateDone); err != nil {
		t.Fatal(err)
	}
	for _, windows := range []*completionWindowSet{firstWindow, secondWindow} {
		if err := unlockLeaseFile(windows.windows[0].live); err != nil {
			t.Fatal(err)
		}
		windows.windows[0].live = nil
	}

	err = reconcileCompletionWindows([]string{root})
	if err == nil || !strings.Contains(err.Error(), first.ID) || !strings.Contains(err.Error(), second.ID) ||
		!strings.Contains(err.Error(), "remain done") {
		t.Fatalf("baseline mutation replay error = %v", err)
	}
	for _, archived := range []taskItem{first, second} {
		current, ok := currentTask(root, archived.ID)
		if !ok || current.State != stateDone {
			t.Fatalf("baseline archive %s was reopened: %#v", archived.ID, current)
		}
		if taskCompletionRecorded(root, current) {
			t.Fatalf("baseline archive %s retained stale completion receipt", archived.ID)
		}
	}
	current, ok := currentTask(root, rogue.ID)
	if !ok || current.State != stateInProgress {
		t.Fatalf("new unowned completion was not restored: %#v", current)
	}
	index, err := readCompletionWindowIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, windowID := range windowIDs {
		if _, ok := index.Windows[windowID]; ok {
			t.Fatalf("deterministic baseline mutation retained stale recovery journal %s", windowID)
		}
	}
	if err := reconcileCompletionWindows([]string{root}); err != nil {
		t.Fatalf("second startup recovery = %v, want clean", err)
	}
}

func TestCompletionWindowReplayArrivalPrecedesLaterMutation(t *testing.T) {
	root := t.TempDir()
	rogue := taskForLease(t, root, stateInProgress, "staggered-unowned-completion")
	older, err := beginCompletionWindows([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if err := moveTaskDir(root, rogue, stateDone); err != nil {
		t.Fatal(err)
	}
	rogue, _ = currentTask(root, rogue.ID)
	newer, err := beginCompletionWindows([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rogue.Dir, "late-audit.log"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, windows := range []*completionWindowSet{older, newer} {
		if err := unlockLeaseFile(windows.windows[0].live); err != nil {
			t.Fatal(err)
		}
		windows.windows[0].live = nil
	}

	if err := reconcileCompletionWindows([]string{root}); err != nil {
		t.Fatalf("staggered recovery: %v", err)
	}
	current, ok := currentTask(root, rogue.ID)
	if !ok || current.State != stateInProgress {
		t.Fatalf("older window's unowned arrival was suppressed by later mutation: %#v", current)
	}
	if err := reconcileCompletionWindows([]string{root}); err != nil {
		t.Fatalf("second staggered recovery = %v", err)
	}
}

func TestCompletionWindowReplayOwnedArrivalDoesNotHideLaterMutation(t *testing.T) {
	root := t.TempDir()
	arrival := taskForLease(t, root, stateInProgress, "owned-arrival-before-mutation")
	older, err := beginCompletionWindows([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if err := completeTrustedTask(root, arrival); err != nil {
		t.Fatal(err)
	}
	arrival, _ = currentTask(root, arrival.ID)
	newer, err := beginCompletionWindows([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(arrival.Dir, "late-audit.log"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, windows := range []*completionWindowSet{older, newer} {
		if err := unlockLeaseFile(windows.windows[0].live); err != nil {
			t.Fatal(err)
		}
		windows.windows[0].live = nil
	}

	if err := reconcileCompletionWindows([]string{root}); err == nil ||
		!strings.Contains(err.Error(), arrival.ID) ||
		!strings.Contains(err.Error(), "remain done") {
		t.Fatalf("owned staggered recovery error = %v", err)
	}
	current, ok := currentTask(root, arrival.ID)
	if !ok || current.State != stateDone {
		t.Fatalf("owned arrival mutation was reopened: %#v", current)
	}
	if taskCompletionRecorded(root, current) {
		t.Fatal("owned arrival mutation retained stale completion authority")
	}
	if err := reconcileCompletionWindows([]string{root}); err != nil {
		t.Fatalf("second owned staggered recovery = %v", err)
	}
}

func TestCompletionWindowReplayPersistsRecoveredDepartureAcrossCrash(t *testing.T) {
	root := t.TempDir()
	rogue := taskForLease(t, root, stateInProgress, "crash-persisted-recovery-departure")
	older, err := beginCompletionWindows([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if err := moveTaskDir(root, rogue, stateDone); err != nil {
		t.Fatal(err)
	}
	rogue, _ = currentTask(root, rogue.ID)
	newer, err := beginCompletionWindows([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	for _, windows := range []*completionWindowSet{older, newer} {
		if err := unlockLeaseFile(windows.windows[0].live); err != nil {
			t.Fatal(err)
		}
		windows.windows[0].live = nil
	}

	// Simulate death after the recovery provenance and older-window retirement became durable,
	// and after the task was restored, but before the newer baseline window was retired.
	indexFile, index, err := lockCompletionWindowIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	newerID := newer.windows[0].id
	record := index.Windows[newerID]
	record.RecoveredDepartures = []string{rogue.ID}
	index.Windows[newerID] = record
	delete(index.Windows, older.windows[0].id)
	if err := errors.Join(writeCompletionWindowIndex(root, index), unlockLeaseFile(indexFile)); err != nil {
		t.Fatal(err)
	}
	if err := restoreUnownedCompletion(queuedTask{Root: root, Item: rogue}); err != nil {
		t.Fatal(err)
	}

	if err := reconcileCompletionWindows([]string{root}); err != nil {
		t.Fatalf("recovered-departure retry: %v", err)
	}
	index, err = readCompletionWindowIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := index.Windows[newerID]; ok {
		t.Fatal("recovered-departure retry retained the newer journal")
	}
	current, ok := currentTask(root, rogue.ID)
	if !ok || current.State != stateInProgress {
		t.Fatalf("recovered departure changed state on retry: %#v", current)
	}
}

func TestCompletionWindowReplayLeavesReplacedBaselineArchiveDone(t *testing.T) {
	root := t.TempDir()
	archived := taskForLease(t, root, stateInProgress, "replaced-baseline")
	if err := completeTrustedTask(root, archived); err != nil {
		t.Fatal(err)
	}
	archived, _ = currentTask(root, archived.ID)
	windows, err := beginCompletionWindows([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), archived.ID)
	if err := os.Rename(archived.Dir, backup); err != nil {
		t.Fatal(err)
	}
	replacement := taskForLease(t, root, stateDone, archived.ID)
	if taskCompletionRecorded(root, replacement) {
		t.Fatal("replacement unexpectedly inherited the inode-bound completion receipt")
	}
	if err := unlockLeaseFile(windows.windows[0].live); err != nil {
		t.Fatal(err)
	}
	windows.windows[0].live = nil

	if err := reconcileCompletionWindows([]string{root}); err == nil ||
		!strings.Contains(err.Error(), archived.ID) {
		t.Fatalf("replaced baseline recovery error = %v", err)
	}
	current, ok := currentTask(root, archived.ID)
	if !ok || current.State != stateDone {
		t.Fatalf("replaced baseline archive was reopened: %#v", current)
	}
}

func TestCompletionWindowReplayLeavesBusyBaselineWithoutReceiptDone(t *testing.T) {
	root := t.TempDir()
	archived := taskForLease(t, root, stateInProgress, "busy-baseline-replay")
	if err := completeTrustedTask(root, archived); err != nil {
		t.Fatal(err)
	}
	archived, _ = currentTask(root, archived.ID)
	authority, err := openLeaseAuthority(root, archived.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(authority.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if err := clearLeaseCompletionReceipt(authority); err != nil {
		t.Fatal(err)
	}
	windows, err := beginCompletionWindows([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if err := unlockLeaseFile(authority); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(archived.Dir, "late-audit.log"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := unlockLeaseFile(windows.windows[0].live); err != nil {
		t.Fatal(err)
	}
	windows.windows[0].live = nil

	if err := reconcileCompletionWindows([]string{root}); err == nil ||
		!strings.Contains(err.Error(), archived.ID) {
		t.Fatalf("busy baseline recovery error = %v", err)
	}
	current, ok := currentTask(root, archived.ID)
	if !ok || current.State != stateDone {
		t.Fatalf("busy receiptless baseline archive was reopened: %#v", current)
	}
}

func TestCompletionWindowReplayAcceptsReceiptCompletedFromBusyBaseline(t *testing.T) {
	root := t.TempDir()
	archived := taskForLease(t, root, stateDone, "busy-baseline-completed-receipt")
	authority, err := openLeaseAuthority(root, archived.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(authority.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if err := clearLeaseCompletionReceipt(authority); err != nil {
		t.Fatal(err)
	}
	windows, err := beginCompletionWindows([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeLeaseCompletionReceipt(authority, archived.Dir); err != nil {
		t.Fatal(err)
	}
	if err := unlockLeaseFile(authority); err != nil {
		t.Fatal(err)
	}
	if err := unlockLeaseFile(windows.windows[0].live); err != nil {
		t.Fatal(err)
	}
	windows.windows[0].live = nil

	if err := reconcileCompletionWindows([]string{root}); err != nil {
		t.Fatalf("completed busy-baseline receipt recovery: %v", err)
	}
	current, ok := currentTask(root, archived.ID)
	if !ok || current.State != stateDone || !taskCompletionRecorded(root, current) {
		t.Fatalf("completed busy-baseline receipt was not accepted: %#v", current)
	}
}

func TestCompletionWindowReplayResumesPersistedBaselineMutation(t *testing.T) {
	root := t.TempDir()
	archived := taskForLease(t, root, stateInProgress, "persisted-baseline-mutation")
	if err := completeTrustedTask(root, archived); err != nil {
		t.Fatal(err)
	}
	archived, _ = currentTask(root, archived.ID)
	windows, err := beginCompletionWindows([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	windowID := windows.windows[0].id
	if err := os.WriteFile(filepath.Join(archived.Dir, "late-audit.log"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := unlockLeaseFile(windows.windows[0].live); err != nil {
		t.Fatal(err)
	}
	windows.windows[0].live = nil

	indexFile, index, err := lockCompletionWindowIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	record := index.Windows[windowID]
	record.BaselineMutations = []string{archived.ID}
	index.Windows[windowID] = record
	if err := errors.Join(writeCompletionWindowIndex(root, index), unlockLeaseFile(indexFile)); err != nil {
		t.Fatal(err)
	}
	authority, err := openLeaseAuthority(root, archived.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(authority.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(clearLeaseCompletionReceipt(authority), unlockLeaseFile(authority)); err != nil {
		t.Fatal(err)
	}

	if err := reconcileCompletionWindows([]string{root}); err == nil ||
		!strings.Contains(err.Error(), archived.ID) {
		t.Fatalf("persisted baseline mutation recovery error = %v", err)
	}
	current, ok := currentTask(root, archived.ID)
	if !ok || current.State != stateDone {
		t.Fatalf("persisted baseline mutation was reopened: %#v", current)
	}
	index, err = readCompletionWindowIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := index.Windows[windowID]; ok {
		t.Fatal("persisted baseline mutation journal was not consumed")
	}
}

func TestCompletionWindowReplayFreshReceiptSupersedesPersistedMutation(t *testing.T) {
	root := t.TempDir()
	archived := taskForLease(t, root, stateInProgress, "recompleted-marked-baseline")
	if err := completeTrustedTask(root, archived); err != nil {
		t.Fatal(err)
	}
	archived, _ = currentTask(root, archived.ID)
	before, ok := taskCompletionReceipt(root, archived)
	if !ok {
		t.Fatal("initial trusted completion has no receipt")
	}
	windows, err := beginCompletionWindows([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	windowID := windows.windows[0].id
	if err := os.WriteFile(filepath.Join(archived.Dir, "late-audit.log"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := unlockLeaseFile(windows.windows[0].live); err != nil {
		t.Fatal(err)
	}
	windows.windows[0].live = nil

	indexFile, index, err := lockCompletionWindowIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	record := index.Windows[windowID]
	record.BaselineMutations = []string{archived.ID}
	index.Windows[windowID] = record
	if err := errors.Join(writeCompletionWindowIndex(root, index), unlockLeaseFile(indexFile)); err != nil {
		t.Fatal(err)
	}
	if err := moveTrustedTaskFromDone(root, archived, stateInProgress); err != nil {
		t.Fatal(err)
	}
	reopened, _ := currentTask(root, archived.ID)
	if err := completeTrustedTask(root, reopened); err != nil {
		t.Fatal(err)
	}

	if err := reconcileCompletionWindows([]string{root}); err != nil {
		t.Fatalf("fresh trusted re-completion recovery: %v", err)
	}
	current, ok := currentTask(root, archived.ID)
	if !ok || current.State != stateDone {
		t.Fatalf("fresh trusted re-completion was reopened: %#v", current)
	}
	after, ok := taskCompletionReceipt(root, current)
	if !ok || after.Nonce == before.Nonce {
		t.Fatalf("fresh trusted receipt = %q, want generation after %q", after.Nonce, before.Nonce)
	}
	index, err = readCompletionWindowIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := index.Windows[windowID]; ok {
		t.Fatal("fresh trusted re-completion retained its stale journal")
	}
}

func TestCompletionWindowReplayRetainsMarkedMutationWhileReceiptOwned(t *testing.T) {
	root := t.TempDir()
	archived := taskForLease(t, root, stateInProgress, "owned-marked-baseline-mutation")
	if err := completeTrustedTask(root, archived); err != nil {
		t.Fatal(err)
	}
	archived, _ = currentTask(root, archived.ID)
	windows, err := beginCompletionWindows([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	windowID := windows.windows[0].id
	if err := os.WriteFile(filepath.Join(archived.Dir, "late-audit.log"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := unlockLeaseFile(windows.windows[0].live); err != nil {
		t.Fatal(err)
	}
	windows.windows[0].live = nil

	indexFile, index, err := lockCompletionWindowIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	record := index.Windows[windowID]
	record.BaselineMutations = []string{archived.ID}
	index.Windows[windowID] = record
	if err := errors.Join(writeCompletionWindowIndex(root, index), unlockLeaseFile(indexFile)); err != nil {
		t.Fatal(err)
	}
	lease, _, err := tryTaskLease(root, archived, testLeaseOwner())
	if err != nil || lease == nil {
		t.Fatalf("live receipt owner = %v, err %v", lease, err)
	}
	if err := reconcileCompletionWindows([]string{root}); err == nil ||
		!strings.Contains(err.Error(), "authoritative owner") {
		_ = lease.release()
		t.Fatalf("owned marked mutation recovery = %v", err)
	}
	index, err = readCompletionWindowIndex(root)
	if err != nil {
		_ = lease.release()
		t.Fatal(err)
	}
	if _, ok := index.Windows[windowID]; !ok {
		_ = lease.release()
		t.Fatal("owned marked mutation consumed its recovery journal")
	}
	if err := lease.release(); err != nil {
		t.Fatal(err)
	}
	if err := reconcileCompletionWindows([]string{root}); err == nil ||
		!strings.Contains(err.Error(), archived.ID) {
		t.Fatalf("released marked mutation recovery = %v", err)
	}
	index, err = readCompletionWindowIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := index.Windows[windowID]; ok {
		t.Fatal("released marked mutation retained its recovery journal")
	}
}

func TestBaselineMutationLockRejectsOwnerAcquiredAfterClassification(t *testing.T) {
	root := t.TempDir()
	archived := taskForLease(t, root, stateInProgress, "post-classification-owner")
	if err := completeTrustedTask(root, archived); err != nil {
		t.Fatal(err)
	}
	archived, _ = currentTask(root, archived.ID)
	baseline, err := snapshotDoneCompletions(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(archived.Dir, "late-audit.log"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidates, err := changedDoneCompletions(root, baseline)
	if err != nil {
		t.Fatal(err)
	}
	replay, mutations, err := completionReplayCandidates(
		root,
		completionWindowRecord{Baseline: baseline},
		candidates,
	)
	if err != nil || len(replay) != 0 || len(mutations) != 1 || mutations[0].Item.ID != archived.ID {
		t.Fatalf("classified replay=%v mutations=%v err=%v", replay, mutations, err)
	}

	lease, _, err := tryTaskLease(root, archived, testLeaseOwner())
	if err != nil || lease == nil {
		t.Fatalf("post-classification owner = %v, err %v", lease, err)
	}
	mutationTasks := map[string]queuedTask{archived.ID: mutations[0]}
	if locked, err := lockCompletionCandidates(root, mutationTasks); err == nil || len(locked) != 0 ||
		!strings.Contains(err.Error(), "authoritative owner") {
		_ = releaseLockedCompletionCandidates(locked)
		_ = lease.release()
		t.Fatalf("post-classification mutation lock = %v, err %v", locked, err)
	}
	if err := lease.release(); err != nil {
		t.Fatal(err)
	}
	locked, err := lockCompletionCandidates(root, mutationTasks)
	if err != nil || len(locked) != 1 {
		t.Fatalf("released mutation lock = %v, err %v", locked, err)
	}
	if err := releaseLockedCompletionCandidates(locked); err != nil {
		t.Fatal(err)
	}
}

func TestCompletionWindowRecoveryDetectsSameIDMetadataHandoff(t *testing.T) {
	root := t.TempDir()
	archived := taskForLease(t, root, stateInProgress, "same-id-metadata-handoff")
	if err := completeTrustedTask(root, archived); err != nil {
		t.Fatal(err)
	}
	archived, _ = currentTask(root, archived.ID)
	candidates := map[string]queuedTask{
		archived.ID: {Root: root, Item: archived},
	}
	locked, err := lockCompletionCandidates(root, candidates)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(archived.Dir, "late-provider-write.log"), []byte("changed\n"), 0o600); err != nil {
		_ = releaseLockedCompletionCandidates(locked)
		t.Fatal(err)
	}
	if err := verifyLockedCompletionCandidates(locked); err == nil ||
		!strings.Contains(err.Error(), archived.ID) {
		_ = releaseLockedCompletionCandidates(locked)
		t.Fatalf("same-id metadata handoff verification = %v", err)
	}
	if err := releaseLockedCompletionCandidates(locked); err != nil {
		t.Fatal(err)
	}
}

func TestCompletionWindowReplayWaitsForTransientReceiptReader(t *testing.T) {
	root := t.TempDir()
	rogue := taskForLease(t, root, stateInProgress, "reader-contended-replay")
	windows, err := beginCompletionWindows([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if err := moveTaskDir(root, rogue, stateDone); err != nil {
		t.Fatal(err)
	}
	if err := unlockLeaseFile(windows.windows[0].live); err != nil {
		t.Fatal(err)
	}
	windows.windows[0].live = nil

	authority, err := openLeaseAuthority(root, rogue.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(authority.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- reconcileCompletionWindows([]string{root}) }()
	select {
	case err := <-done:
		t.Fatalf("replay returned while the receipt reader held its lock: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if err := unlockLeaseFile(authority); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !pathExists(filepath.Join(root, stateInProgress, rogue.ID)) || pathExists(filepath.Join(root, stateDone, rogue.ID)) {
		t.Fatal("replay skipped the completion after transient reader contention")
	}
}

func TestCompletionWindowReplayReportsArchivedTaskDepartureOnce(t *testing.T) {
	root := t.TempDir()
	archived := taskForLease(t, root, stateDone, "departed-before-replay")
	windows, err := beginCompletionWindows([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	windowID := windows.windows[0].id
	if err := moveTaskDir(root, archived, stateInProgress); err != nil {
		t.Fatal(err)
	}
	if err := unlockLeaseFile(windows.windows[0].live); err != nil {
		t.Fatal(err)
	}
	windows.windows[0].live = nil
	if err := reconcileCompletionWindows([]string{root}); err == nil || !strings.Contains(err.Error(), archived.ID) {
		t.Fatalf("departure replay = %v, want task-specific ownership failure", err)
	}
	index, err := readCompletionWindowIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := index.Windows[windowID]; ok {
		t.Fatal("recognized departure replay retained a stale journal")
	}
	if !pathExists(filepath.Join(root, stateInProgress, archived.ID)) {
		t.Fatal("departure replay lost the actionable task")
	}
	if err := reconcileCompletionWindows([]string{root}); err != nil {
		t.Fatalf("second departure replay = %v, want clean", err)
	}
}

func TestReviewCompletionWindowReplayRejectsDeletedArchive(t *testing.T) {
	root := t.TempDir()
	archived := taskForLease(t, root, stateDone, "deleted-during-review")
	windows, err := beginReviewCompletionWindows([]string{root}, []string{archived.ID})
	if err != nil {
		t.Fatal(err)
	}
	windowID := windows.windows[0].id
	if err := os.RemoveAll(archived.Dir); err != nil {
		t.Fatal(err)
	}
	if err := unlockLeaseFile(windows.windows[0].live); err != nil {
		t.Fatal(err)
	}
	windows.windows[0].live = nil
	if err := reconcileCompletionWindows([]string{root}); err == nil || !strings.Contains(err.Error(), archived.ID) {
		t.Fatalf("deleted review replay = %v, want missing-task failure", err)
	}
	index, err := readCompletionWindowIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := index.Windows[windowID]; !ok {
		t.Fatal("missing review task deleted its recovery journal")
	}
	if record := index.Windows[windowID]; !record.ReviewWindow || !slices.Equal(record.ReviewSubjects, []string{archived.ID}) {
		t.Fatalf("deleted review journal scope = %#v, want exact review subject", record)
	}
}

func TestReviewCompletionWindowReplayRejectsSubjectRecompletion(t *testing.T) {
	root := t.TempDir()
	subject := taskForLease(t, root, stateDone, "recompleted-review-subject")
	if err := completeTrustedTask(root, subject); err != nil {
		t.Fatal(err)
	}
	subject, _ = currentTask(root, subject.ID)
	windows, err := beginReviewCompletionWindows([]string{root}, []string{subject.ID})
	if err != nil {
		t.Fatal(err)
	}
	windowID := windows.windows[0].id
	if err := moveTrustedTaskFromDone(root, subject, stateInProgress); err != nil {
		t.Fatal(err)
	}
	reopened, _ := currentTask(root, subject.ID)
	if err := completeTrustedTask(root, reopened); err != nil {
		t.Fatal(err)
	}
	if err := unlockLeaseFile(windows.windows[0].live); err != nil {
		t.Fatal(err)
	}
	windows.windows[0].live = nil
	if err := reconcileCompletionWindows([]string{root}); err == nil || !strings.Contains(err.Error(), subject.ID) {
		t.Fatalf("subject recompletion replay = %v, want subject failure", err)
	}
	index, err := readCompletionWindowIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := index.Windows[windowID]; !ok {
		t.Fatal("subject recompletion replay discarded its recovery journal")
	}
}

func TestSubjectFreeReviewCompletionWindowReplayStaysStrict(t *testing.T) {
	root := t.TempDir()
	foreign := taskForLease(t, root, stateTodo, "subject-free-review-completion")
	windows, err := beginReviewCompletionWindows([]string{root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	windowID := windows.windows[0].id
	if err := completeTrustedTask(root, foreign); err != nil {
		t.Fatal(err)
	}
	if err := unlockLeaseFile(windows.windows[0].live); err != nil {
		t.Fatal(err)
	}
	windows.windows[0].live = nil
	if err := reconcileCompletionWindows([]string{root}); err == nil || !strings.Contains(err.Error(), foreign.ID) {
		t.Fatalf("subject-free review replay = %v, want strict completion failure", err)
	}
	index, err := readCompletionWindowIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	record, ok := index.Windows[windowID]
	if !ok || !record.ReviewWindow || len(record.ReviewSubjects) != 0 {
		t.Fatalf("subject-free review replay journal = %#v, present %v", record, ok)
	}
}

// TestReviewCompletionWindowReplayPreservesConcurrentHostCompletion: the journal record carries
// the review's subject scope, and replaying a crashed review window applies the same rules the
// live audit does — a host-receipted foreign completion stays done with its receipt while the
// journal entry is retired cleanly.
func TestReviewCompletionWindowReplayPreservesConcurrentHostCompletion(t *testing.T) {
	root := t.TempDir()
	subject := taskForLease(t, root, stateDone, "crashed-review-subject")
	if err := completeTrustedTask(root, subject); err != nil {
		t.Fatal(err)
	}
	subject, _ = currentTask(root, subject.ID)
	foreign := taskForLease(t, root, stateTodo, "crashed-review-foreign")
	windows, err := beginReviewCompletionWindows([]string{root}, []string{subject.ID})
	if err != nil {
		t.Fatal(err)
	}
	windowID := windows.windows[0].id
	if err := completeTrustedTask(root, foreign); err != nil {
		t.Fatal(err)
	}
	// Simulate controller death: the kernel releases the live lock, but the durable index remains.
	if err := unlockLeaseFile(windows.windows[0].live); err != nil {
		t.Fatal(err)
	}
	windows.windows[0].live = nil
	index, err := readCompletionWindowIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if record, ok := index.Windows[windowID]; !ok || !record.ReviewWindow || !slices.Equal(record.ReviewSubjects, []string{subject.ID}) {
		t.Fatalf("review window journal = %#v, want exact subject [%s]", record, subject.ID)
	}
	concurrent, err := reconcileCompletionWindowsWithActivity([]string{root})
	if err != nil {
		t.Fatalf("crash replay: %v", err)
	}
	if !slices.Equal(concurrent, []string{foreign.ID}) {
		t.Fatalf("crash replay concurrent completions = %v, want [%s]", concurrent, foreign.ID)
	}
	done, ok := currentTask(root, foreign.ID)
	if !ok || done.State != stateDone || !taskCompletionRecorded(root, done) {
		t.Fatal("crash replay revoked a host-receipted concurrent completion")
	}
	index, err = readCompletionWindowIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := index.Windows[windowID]; ok {
		t.Fatal("replayed review window retained a stale journal")
	}
}

func TestCompletionWindowReplayLeavesLiveWindowToItsController(t *testing.T) {
	root := t.TempDir()
	rogue := taskForLease(t, root, stateInProgress, "live-window-task")
	windows, err := beginCompletionWindows([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if err := moveTaskDir(root, rogue, stateDone); err != nil {
		t.Fatal(err)
	}
	if err := reconcileCompletionWindows([]string{root}); err != nil {
		t.Fatal(err)
	}
	if !pathExists(filepath.Join(root, stateDone, rogue.ID)) {
		t.Fatal("startup replay stole a completion from a live controller window")
	}
	if err := windows.rejectAndClose(queuedTask{}); err == nil || !strings.Contains(err.Error(), rogue.ID) {
		t.Fatalf("live controller ownership audit = %v, want rejection", err)
	}
	if !pathExists(filepath.Join(root, stateInProgress, rogue.ID)) {
		t.Fatal("live controller did not restore its unowned completion")
	}
}

func TestTrustedCompletionDoesNotStealActiveLease(t *testing.T) {
	root := t.TempDir()
	task := taskForLease(t, root, stateInProgress, "actively-leased")
	lease, _, err := tryTaskLease(root, task, testLeaseOwner())
	if err != nil || lease == nil {
		t.Fatalf("lease = %v, err %v", lease, err)
	}
	defer lease.release()
	if err := completeTrustedTask(root, task); err == nil || !strings.Contains(err.Error(), "leased by another controller") {
		t.Fatalf("trusted completion against active lease = %v", err)
	}
	if !pathExists(filepath.Join(root, stateInProgress, task.ID)) || pathExists(filepath.Join(root, stateDone, task.ID)) {
		t.Fatal("trusted completion moved an actively leased task")
	}
}

func TestLoopRejectsActionableDuplicateIDsAcrossQueues(t *testing.T) {
	for _, tc := range []struct {
		name      string
		crashDone bool
	}{
		{name: "already actionable"},
		{name: "made actionable by crash recovery", crashDone: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			queues := []string{"queue-a", "queue-b"}
			for _, queue := range queues {
				state := stateTodo
				if tc.crashDone {
					state = stateDone
				}
				dir := filepath.Join(repo, queue, state, "same-id")
				writeTaskFile(t, filepath.Join(dir, "task.md"), "# same id\n")
				if tc.crashDone {
					writeTaskFile(t, filepath.Join(dir, "log.md"), "# log\n")
					writeTaskFile(t, filepath.Join(dir, "state.md"), "# state\n")
					writeTaskFile(t, filepath.Join(dir, "tmp", leaseLockName), "")
					writeTaskFile(t, filepath.Join(dir, "tmp", leaseMetadataName), "{}\n")
				}
			}
			a := &app{cfg: &config.Config{}}
			code, err := a.loop(repo, "missing-image", "codex", "", nil, queues, nil, nil, false, false, 0)
			if code != 1 || err == nil || !strings.Contains(err.Error(), "same-id") || !strings.Contains(err.Error(), "multiple queues") {
				t.Fatalf("duplicate loop = code %d err %v", code, err)
			}
			if tc.crashDone {
				for _, queue := range queues {
					if !pathExists(filepath.Join(repo, queue, stateInProgress, "same-id")) {
						t.Fatalf("%s crash candidate was not restored before duplicate validation", queue)
					}
				}
			}
		})
	}
}

func TestFinalizeQueuedCompletion(t *testing.T) {
	root := t.TempDir()
	doneID := "2026-01-01-done"
	liveID := "2026-01-02-live"
	writeTaskFile(t, filepath.Join(root, stateDone, doneID, "task.md"), "# done\n")
	writeTaskFile(t, filepath.Join(root, stateDone, doneID, "state.md"), "# State — done\n\n**Status:** commit next\n**Done so far:** kept summary\n**Next action:** move to done\n**Traps:** kept trap\n")
	writeTaskFile(t, filepath.Join(root, stateDone, doneID, "tmp", "scratch"), "remove\n")
	writeTaskFile(t, filepath.Join(root, stateInProgress, liveID, "task.md"), "# live\n")
	writeTaskFile(t, filepath.Join(root, stateInProgress, liveID, "tmp", "scratch"), "retain\n")

	doneDir := filepath.Join(root, stateDone, doneID)
	if err := finalizeQueuedCompletion(queuedTask{Root: root, Item: taskItem{ID: doneID, Dir: doneDir, State: stateDone}}); err != nil {
		t.Fatal(err)
	}
	if pathExists(filepath.Join(doneDir, "tmp")) {
		t.Error("observed done task kept its tmp")
	}
	if !fileExists(filepath.Join(root, stateInProgress, liveID, "tmp", "scratch")) {
		t.Error("cleanup touched an unfinished task's tmp")
	}
	state := readFileString(filepath.Join(doneDir, "state.md"))
	if !strings.Contains(state, "**Status:** complete") || !strings.Contains(state, "**Next action:** none") {
		t.Errorf("done task was not finalized:\n%s", state)
	}
	if !strings.Contains(state, "**Done so far:** kept summary") || !strings.Contains(state, "**Traps:** kept trap") {
		t.Errorf("finalization discarded agent-authored fields:\n%s", state)
	}
}

func TestReconcileInterruptedCompletions(t *testing.T) {
	newRepo := func(t *testing.T) (string, func(...string)) {
		t.Helper()
		repo := t.TempDir()
		git := func(args ...string) {
			t.Helper()
			cmd := exec.Command("git", args...)
			cmd.Dir = repo
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
		git("init", "-q")
		git("config", "user.email", "t@t")
		git("config", "user.name", "T")
		git("commit", "-q", "--allow-empty", "-m", "base")
		return repo, git
	}
	seedDone := func(t *testing.T, repo, id string) string {
		t.Helper()
		dir := filepath.Join(repo, tasksRoot, stateDone, id)
		writeTaskFile(t, filepath.Join(dir, "task.md"), "# task\n")
		writeTaskFile(t, filepath.Join(dir, "log.md"), "# log\n")
		writeTaskFile(t, filepath.Join(dir, "state.md"), "# state\n\n**Status:** in progress\n**Next action:** finish\n")
		writeTaskFile(t, filepath.Join(dir, "tmp", "lease.lock"), "")
		writeTaskFile(t, filepath.Join(dir, "tmp", "lease.json"), "{}\n")
		return dir
	}

	t.Run("bound completion restores for range validation", func(t *testing.T) {
		repo, git := newRepo(t)
		id := "interrupted-bound"
		seedDone(t, repo, id)
		git("commit", "-q", "--allow-empty", "-m", "done\n\nCoop-Task: "+id)
		if err := reconcileInterruptedCompletions([]string{filepath.Join(repo, tasksRoot)}); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(repo, tasksRoot, stateInProgress, id)
		state := readFileString(filepath.Join(dir, "state.md"))
		if !strings.Contains(state, "**Status:** in progress") || !strings.Contains(state, "**Next action:** repair the commit binding") {
			t.Fatalf("bound interrupted completion state:\n%s", state)
		}
	})

	t.Run("audit completion restores with host-authority remedy", func(t *testing.T) {
		repo, _ := newRepo(t)
		id := "interrupted-audit"
		seedDone(t, repo, id)
		host := filepath.Join(repo, tasksRoot)
		if err := writeAuditReopenRecord(host, testAuditReopenRecord(id, "generation-interrupted")); err != nil {
			t.Fatal(err)
		}
		if err := reconcileInterruptedCompletions([]string{host}); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(host, stateInProgress, id)
		log := readFileString(filepath.Join(dir, "log.md"))
		state := readFileString(filepath.Join(dir, "state.md"))
		for _, want := range []string{"host-authorized review rework", "zero new commits", "tree actually changes"} {
			if !strings.Contains(log, want) {
				t.Errorf("reconciled audit log missing %q:\n%s", want, log)
			}
		}
		if strings.Contains(log, "Coop-Recovery: <current UTC timestamp>") ||
			!strings.Contains(state, "never add a Coop-Recovery trailer") {
			t.Errorf("reconciled audit completion received ordinary recovery guidance:\nlog:\n%s\nstate:\n%s", log, state)
		}
	})

	t.Run("unbound completion restores", func(t *testing.T) {
		repo, _ := newRepo(t)
		id := "interrupted-unbound"
		seedDone(t, repo, id)
		host := filepath.Join(repo, tasksRoot)
		authority, err := openLeaseAuthority(host, id, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := syscall.Flock(int(authority.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Fatal(err)
		}
		staleDir := filepath.Join(repo, "old-completion")
		if err := os.Mkdir(staleDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := writeLeaseCompletionReceipt(authority, staleDir); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(staleDir); err != nil {
			t.Fatal(err)
		}
		if err := unlockLeaseFile(authority); err != nil {
			t.Fatal(err)
		}
		if err := reconcileInterruptedCompletions([]string{host}); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(host, stateInProgress, id)
		if !fileExists(filepath.Join(dir, "task.md")) || pathExists(filepath.Join(repo, tasksRoot, stateDone, id)) {
			t.Fatal("unbound interrupted completion was not restored")
		}
		authority, err = openLeaseAuthority(host, id, false)
		if err != nil {
			t.Fatal(err)
		}
		info, err := authority.Stat()
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() != 0 {
			t.Fatal("startup restore retained a stale completion receipt")
		}
		_ = authority.Close()
	})

	t.Run("ordinary archive is untouched", func(t *testing.T) {
		repo, _ := newRepo(t)
		id := "ordinary-archive"
		dir := filepath.Join(repo, tasksRoot, stateDone, id)
		writeTaskFile(t, filepath.Join(dir, "task.md"), "# task\n")
		writeTaskFile(t, filepath.Join(dir, "tmp", "artifact"), "keep\n")
		if err := reconcileInterruptedCompletions([]string{filepath.Join(repo, tasksRoot)}); err != nil {
			t.Fatal(err)
		}
		if !fileExists(filepath.Join(dir, "tmp", "artifact")) {
			t.Fatal("startup reconciliation touched an archive without lease metadata")
		}
	})

	t.Run("duplicate ids restore the exact queue", func(t *testing.T) {
		first, _ := newRepo(t)
		second, _ := newRepo(t)
		id := "same-id"
		writeTaskFile(t, filepath.Join(first, tasksRoot, stateInProgress, id, "task.md"), "# active\n")
		seedDone(t, second, id)
		hosts := []string{filepath.Join(first, tasksRoot), filepath.Join(second, tasksRoot)}
		if err := reconcileInterruptedCompletions(hosts); err != nil {
			t.Fatal(err)
		}
		if pathExists(filepath.Join(second, tasksRoot, stateDone, id)) ||
			!fileExists(filepath.Join(second, tasksRoot, stateInProgress, id, "task.md")) {
			t.Fatal("startup reconciliation restored the same id from the wrong queue")
		}
	})

	t.Run("active completion lease is untouched", func(t *testing.T) {
		repo, _ := newRepo(t)
		id := "active-completion"
		dir := seedDone(t, repo, id)
		lock, err := openLeaseLock(dir, false)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Fatal(err)
		}
		host := filepath.Join(repo, tasksRoot)
		if err := reconcileInterruptedCompletions([]string{host}); err != nil {
			t.Fatal(err)
		}
		if !pathExists(filepath.Join(host, stateDone, id)) {
			t.Fatal("startup reconciliation moved a completion while its lease was held")
		}
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
			t.Fatal(err)
		}
		if err := reconcileInterruptedCompletions([]string{host}); err != nil {
			t.Fatal(err)
		}
		if pathExists(filepath.Join(host, stateDone, id)) || !pathExists(filepath.Join(host, stateInProgress, id)) {
			t.Fatal("released crash-left completion was not restored")
		}
	})

	t.Run("accepted completion cleans stale lease metadata", func(t *testing.T) {
		repo, _ := newRepo(t)
		host := filepath.Join(repo, tasksRoot)
		id := "accepted-before-release"
		item := taskForLease(t, host, stateInProgress, id)
		lease, _, err := tryTaskLease(host, item, testLeaseOwner())
		if err != nil || lease == nil {
			t.Fatalf("lease = %v, err %v", lease, err)
		}
		if err := moveTaskDir(host, item, stateDone); err != nil {
			t.Fatal(err)
		}
		doneDir := filepath.Join(host, stateDone, id)
		lease.quiesce()
		if err := finalizeQueuedCompletion(queuedTask{Root: host, Item: taskItem{ID: id, Dir: doneDir, State: stateDone}}); err != nil {
			t.Fatal(err)
		}
		if err := lease.markCompleted(doneDir); err != nil {
			t.Fatal(err)
		}
		// Simulate death after the receipt is durable but before release removes metadata.
		if err := errors.Join(unlockLeaseFile(lease.local), unlockLeaseFile(lease.authority)); err != nil {
			t.Fatal(err)
		}
		if !leaseAuthorityMetadataExists(host, id) {
			t.Fatal("test did not retain crash-left authority metadata")
		}
		if err := reconcileInterruptedCompletions([]string{host}); err != nil {
			t.Fatal(err)
		}
		if !pathExists(doneDir) || pathExists(filepath.Join(host, stateInProgress, id)) {
			t.Fatal("accepted completion was restored despite its host receipt")
		}
		if leaseAuthorityMetadataExists(host, id) {
			t.Fatal("accepted completion kept stale authority metadata")
		}
		authority, err := openLeaseAuthority(host, id, false)
		if err != nil {
			t.Fatal(err)
		}
		if !leaseCompletionReceiptMatches(authority, doneDir) {
			t.Fatal("accepted completion lost its host receipt")
		}
		_ = authority.Close()
	})

	t.Run("recovery lock covers rename and bookkeeping window", func(t *testing.T) {
		repo, _ := newRepo(t)
		id := "serialized-recovery"
		dir := seedDone(t, repo, id)
		host := filepath.Join(repo, tasksRoot)
		item := readTaskTree(host)[0]
		lock, current, acquired, err := lockCrashCompletion(host, item)
		if err != nil || !acquired {
			t.Fatalf("lock crash completion = acquired %v, err %v", acquired, err)
		}
		if err := moveTaskDir(host, current, stateInProgress); err != nil {
			_ = lock.release()
			t.Fatal(err)
		}
		moved := readTaskTree(host)[0]
		lease, observed, err := tryTaskLease(host, moved, testLeaseOwner())
		if err != nil || lease != nil || observed.State != leaseBusy {
			_ = lock.release()
			t.Fatalf("contender during recovery = lease %v observed %+v err %v", lease, observed, err)
		}
		if err := lock.release(); err != nil {
			t.Fatal(err)
		}
		if !fileExists(filepath.Join(dir, "tmp", leaseLockName)) && !fileExists(filepath.Join(moved.Dir, "tmp", leaseLockName)) {
			t.Fatal("recovery lock inode disappeared")
		}
	})
}

func TestFinalizeQueuedCompletionCleanupFailureRestoresActionableState(t *testing.T) {
	root := t.TempDir()
	id := "2026-01-01-cleanup-obstructed"
	doneDir := filepath.Join(root, stateDone, id)
	writeTaskFile(t, filepath.Join(doneDir, "task.md"), "# done\n")
	writeTaskFile(t, filepath.Join(doneDir, "state.md"), "# State\n\n**Status:** complete\n**Done so far:** implementation complete\n**Next action:** none\n**Traps:** cleanup must succeed\n")
	writeTaskFile(t, filepath.Join(doneDir, "tmp", "scratch"), "retain\n")
	oldCleaner := taskTmpCleaner
	taskTmpCleaner = func(string) error { return errors.New("loop cleanup failed") }
	t.Cleanup(func() { taskTmpCleaner = oldCleaner })

	item := queuedTask{Root: root, Item: taskItem{ID: id, Dir: doneDir, State: stateDone}}
	if err := finalizeQueuedCompletion(item); err == nil || !strings.Contains(err.Error(), "loop cleanup failed") {
		t.Fatalf("loop cleanup failure = %v, want propagated error", err)
	}
	restored := filepath.Join(root, stateInProgress, id)
	if !fileExists(filepath.Join(restored, "tmp", "scratch")) {
		t.Fatal("cleanup failure did not restore the task with diagnostic scratch")
	}
	state := readFileString(filepath.Join(restored, "state.md"))
	for _, want := range []string{"**Status:** in progress — finalization failed", "**Done so far:** implementation complete", "**Next action:** fix the task metadata or cleanup obstruction", "**Traps:** cleanup must succeed"} {
		if !strings.Contains(state, want) {
			t.Errorf("restored cleanup state missing %q:\n%s", want, state)
		}
	}
}

func TestFinalizeQueuedCompletionStateFailureIsRetryable(t *testing.T) {
	root := t.TempDir()
	id := "2026-01-01-state-obstructed"
	taskDir := filepath.Join(root, stateDone, id)
	writeTaskFile(t, filepath.Join(taskDir, "task.md"), "# done\n")
	writeTaskFile(t, filepath.Join(taskDir, "tmp", "scratch"), "retain\n")
	if err := os.MkdirAll(filepath.Join(taskDir, "state.md"), 0o755); err != nil {
		t.Fatal(err)
	}

	item := queuedTask{Root: root, Item: taskItem{ID: id, Dir: taskDir, State: stateDone}}
	if err := finalizeQueuedCompletion(item); err == nil || !strings.Contains(err.Error(), "state finalization failed") {
		t.Fatalf("loop finalization state failure = %v, want propagated error", err)
	}
	taskDir = filepath.Join(root, stateInProgress, id)
	if !fileExists(filepath.Join(taskDir, "tmp", "scratch")) {
		t.Fatal("loop state failure did not restore the actionable task with its tmp")
	}
	if err := os.RemoveAll(filepath.Join(taskDir, "state.md")); err != nil {
		t.Fatal(err)
	}
	doneDir := filepath.Join(root, stateDone, id)
	if err := os.Rename(taskDir, doneDir); err != nil {
		t.Fatal(err)
	}
	item.Item.Dir = doneDir
	if err := finalizeQueuedCompletion(item); err != nil {
		t.Fatalf("loop finalization retry: %v", err)
	}
	if pathExists(filepath.Join(doneDir, "tmp")) {
		t.Fatal("loop finalization retry left tmp")
	}
	state := readFileString(filepath.Join(doneDir, "state.md"))
	if !strings.Contains(state, "**Status:** complete") || !strings.Contains(state, "**Next action:** none") {
		t.Errorf("loop finalization retry did not create safe state:\n%s", state)
	}
}

func TestResumeLine(t *testing.T) {
	// No landed commit → empty (blind-resume path stays byte-identical).
	if resumeLine("x", nil, true) != "" {
		t.Error("no commits should yield no resume line")
	}
	// A landed commit → a line that names the sha and BOTH cases (finish-the-move vs reopened-rework),
	// so it never falsely asserts the task is done.
	l := resumeLine("my-task", []string{"abc123"}, true)
	for _, want := range []string{"my-task", "abc123", "log.md", "REOPENED", "Coop-Recovery", "finish the move", "exactly one reachable", "do not add a second"} {
		if !strings.Contains(l, want) {
			t.Errorf("resume line missing %q:\n%s", want, l)
		}
	}
	if strings.Contains(l, "STOP") {
		t.Errorf("a bound commit AT head is amendable and must not be blocked:\n%s", l)
	}
	// The amend recipe is safe only while the bound commit IS HEAD. Deeper, the same instruction
	// reparents every descendant — it rewrote a whole 286-commit branch once — so the line must
	// forbid it by name and route to a block instead.
	deep := resumeLine("my-task", []string{"abc123"}, false)
	for _, want := range []string{"STOP", "NOT HEAD", "reparent every", "rebase, cherry-pick, or plumbing", "50_blocked/", "decision.md"} {
		if !strings.Contains(deep, want) {
			t.Errorf("deep resume line missing %q:\n%s", want, deep)
		}
	}
	// This text is read by an agent INSIDE the box, where the loop prompt says coop is not
	// installed and state changes are folder moves. Prescribing a coop command there is unrunnable.
	for _, line := range []string{l, deep, taskBindingRecovery("my-task")} {
		if strings.Contains(line, "coop tasks") {
			t.Errorf("in-box guidance prescribes the coop CLI, which the box does not have:\n%s", line)
		}
	}
}

// boundTaskCommitIsHead is the switch between the two recipes, so it has to be exact: one commit
// later and the amend recipe becomes a branch rewrite.
func TestBoundTaskCommitIsHead(t *testing.T) {
	repo := t.TempDir()
	env := append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "g"),
		"GIT_CONFIG_SYSTEM="+filepath.Join(t.TempDir(), "s"))
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = repo, env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("config", "user.email", "t@t")
	git("config", "user.name", "T")
	git("commit", "-q", "--allow-empty", "-m", "base")
	git("commit", "-q", "--allow-empty", "-m", "impl\n\nCoop-Task: task-42")
	bound := commitsForTask(repo, "", "task-42")
	if len(bound) != 1 {
		t.Fatalf("bound commits = %v, want one", bound)
	}
	if !boundTaskCommitIsHead(repo, bound) {
		t.Error("the bound commit IS HEAD, so the amend recipe must stay available")
	}
	git("commit", "-q", "--allow-empty", "-m", "a later, unrelated commit")
	if boundTaskCommitIsHead(repo, bound) {
		t.Error("one commit later the bound commit is no longer HEAD — amending it would reparent that descendant")
	}
	if boundTaskCommitIsHead(repo, nil) || boundTaskCommitIsHead(repo, []string{bound[0], bound[0]}) {
		t.Error("only a single unambiguous binding may authorize the amend recipe")
	}
}

// The rejection guidance must never send an agent to rewrite a commit that is not HEAD: that
// reparents every descendant, and the reparented ones carry OTHER tasks' trailers, so the result
// trips the foreign-binding guard and can never pass. A 286-deep amend rewrote a whole branch
// before this was forbidden by name.
func TestTaskBindingRecoveryNeverPrescribesDeepRewrite(t *testing.T) {
	r := taskBindingRecovery("my-task")
	for _, forbidden := range []string{"replay its descendants", "reword that implementation commit"} {
		if strings.Contains(r, forbidden) {
			t.Errorf("binding recovery still prescribes %q:\n%s", forbidden, r)
		}
	}
	for _, want := range []string{"already reachable but is NOT HEAD", "do not rewrite it", "reparents every commit after it", "50_blocked/"} {
		if !strings.Contains(r, want) {
			t.Errorf("binding recovery missing %q:\n%s", want, r)
		}
	}
}

func TestAuditResumeLine(t *testing.T) {
	l := auditResumeLine("my-task")
	// Host-authorized rework: verify the finding, then either a zero-commit re-close or a real
	// tree change — with the recovery-only shapes forbidden by name.
	for _, want := range []string{
		"my-task", "host-authorized review rework", "NOT crash recovery", "log.md",
		"independently verify", "ZERO new commits", "99_done/", "verification-only",
		"tree actually changes", "exactly one reachable", "semantically unchanged",
		"Do NOT add a Coop-Recovery trailer", "message-only", "recovery-only replay",
	} {
		if !strings.Contains(l, want) {
			t.Errorf("audit resume line missing %q:\n%s", want, l)
		}
	}
	// It forbids the recovery receipt but must never carry the case-(a) recipe that produces one.
	for _, banned := range []string{"Coop-Recovery: <current UTC timestamp>", "determine which case applies", "amend the commit with"} {
		if strings.Contains(l, banned) {
			t.Errorf("audit resume line carries crash-recovery guidance %q:\n%s", banned, l)
		}
	}
	// The lease's host audit authority — not commit presence — selects the audit preamble.
	record := auditReopenRecord{TaskID: "my-task"}
	if got := (&app{}).resumePrefixFor(t.TempDir(), "my-task", stateInProgress, &record); got != l {
		t.Errorf("resumePrefixFor with audit authority = %q, want the audit resume line", got)
	}
	if got := (&app{}).resumePrefixFor(t.TempDir(), "my-task", stateInProgress, nil); got != "" {
		t.Errorf("resumePrefixFor without commits or authority = %q, want empty", got)
	}
}

func TestAssignLoopTaskSelectionAndClaim(t *testing.T) {
	q1 := filepath.Join(t.TempDir(), ".agent", "tasks")
	q2 := filepath.Join(t.TempDir(), ".agent", "tasks")
	writeTaskFile(t, filepath.Join(q1, stateTodo, "todo-first", "task.md"), "# Todo first\n")
	writeTaskFile(t, filepath.Join(q2, stateInProgress, "resume", "task.md"), "# Resume me\n")

	assignment, err := assignLoopTask([]string{q1, q2}, testLeaseOwner())
	if err != nil || assignment.Outcome != assignmentReady {
		t.Fatalf("assignLoopTask resume = %+v, err %v", assignment, err)
	}
	defer assignment.Lease.release()
	if !assignment.Lease.legacy {
		t.Error("a legacy in-progress task with no lock should be marked as an adoption")
	}
	c, got := assignment.Counts, assignment.Task
	if got.Item.ID != "resume" || got.Root != q2 || got.Item.State != stateInProgress {
		t.Fatalf("assignLoopTask chose %+v, want the later queue's in_progress task", got)
	}
	if c.Todo != 1 || c.Doing != 1 {
		t.Fatalf("resume counts = %+v, want Todo=1 Doing=1", c)
	}
	if !pathExists(filepath.Join(q1, stateTodo, "todo-first")) {
		t.Fatal("selecting an interrupted task must not claim a different todo")
	}
}

func TestAssignLoopTaskClaimsBeforeReturningAndCanBlock(t *testing.T) {
	q := filepath.Join(t.TempDir(), ".agent", "tasks")
	writeTaskFile(t, filepath.Join(q, stateTodo, "b-task", "task.md"), "# B\n")
	writeTaskFile(t, filepath.Join(q, stateTodo, "a-task", "task.md"), "# A\n")

	assignment, err := assignLoopTask([]string{q}, testLeaseOwner())
	if err != nil || assignment.Outcome != assignmentReady {
		t.Fatalf("assignLoopTask = %+v, err %v", assignment, err)
	}
	c, got := assignment.Counts, assignment.Task
	if got.Item.ID != "a-task" || got.Item.State != stateInProgress {
		t.Fatalf("assignment = %+v, want first sorted todo claimed in_progress", got)
	}
	if c.Todo != 1 || c.Doing != 1 {
		t.Fatalf("post-claim counts = %+v, want Todo=1 Doing=1", c)
	}
	if pathExists(filepath.Join(q, stateTodo, "a-task")) || !pathExists(got.Item.Dir) {
		t.Fatal("assignment returned before the host-side todo to in_progress move was observable")
	}
	if _, active := queueProgress([]string{q}); active != got.Item.Title {
		t.Fatalf("banner active title = %q, assigned title = %q", active, got.Item.Title)
	}

	writeTaskFile(t, filepath.Join(got.Item.Dir, "decision.md"), "# Decision\n")
	if err := moveTaskDir(q, got.Item, stateBlocked); err != nil {
		t.Fatalf("assigned task should remain movable to blocked: %v", err)
	}
	if !pathExists(filepath.Join(q, stateBlocked, "a-task")) {
		t.Fatal("assigned task did not bounce to blocked")
	}
	if err := assignment.Lease.release(); err != nil {
		t.Fatalf("release moved task lease: %v", err)
	}
}

func TestAssignLoopTaskEmptyIsNoOp(t *testing.T) {
	q := filepath.Join(t.TempDir(), ".agent", "tasks")
	writeTaskFile(t, filepath.Join(q, stateDone, "done", "task.md"), "# Done\n")
	assignment, err := assignLoopTask([]string{q}, testLeaseOwner())
	if err != nil || assignment.Outcome != assignmentDrained {
		t.Fatalf("empty actionable queue = %+v, err %v", assignment, err)
	}
	c := assignment.Counts
	if c.Done != 1 || c.Todo+c.Doing != 0 {
		t.Fatalf("empty actionable counts = %+v", c)
	}
}

func TestAssignLoopTaskOnlyNeverSwitchesTasks(t *testing.T) {
	root := t.TempDir()
	targetID := "2026-01-01-target"
	otherID := "2026-01-01-other"
	writeTaskFile(t, filepath.Join(root, stateTodo, targetID, "task.md"), "# Target\n")
	writeTaskFile(t, filepath.Join(root, stateInProgress, otherID, "task.md"), "# Other\n")

	assignment, err := assignLoopTaskOnly([]string{root}, testLeaseOwner(), targetID)
	if err != nil || assignment.Outcome != assignmentReady || assignment.Task.Item.ID != targetID {
		t.Fatalf("scoped assignment = (%+v, %v), want target task", assignment, err)
	}
	if err := assignment.Lease.release(); err != nil {
		t.Fatal(err)
	}
	if !pathExists(filepath.Join(root, stateInProgress, targetID)) {
		t.Fatal("scoped todo task was not claimed")
	}

	target := taskItem{ID: targetID, State: stateInProgress, Dir: filepath.Join(root, stateInProgress, targetID)}
	if err := moveTaskDir(root, target, stateDone); err != nil {
		t.Fatal(err)
	}
	settled, err := assignLoopTaskOnly([]string{root}, testLeaseOwner(), targetID)
	if err != nil || settled.Outcome != assignmentDrained {
		t.Fatalf("settled scoped assignment = (%+v, %v), want drained", settled, err)
	}
	if !pathExists(filepath.Join(root, stateInProgress, otherID)) {
		t.Fatal("task-limited assignment touched another in-progress task")
	}
}

// TestCommitsForTaskAndUnbindableTasks drives the real git trailer parser. Fresh work binds only
// when the iteration range and reachable history each contain exactly one binding; unchanged HEAD,
// malformed, duplicate, different-id, substring, and historical duplicate values fail closed.
func TestCommitsForTaskAndUnbindableTasks(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	env := append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "g"),
		"GIT_CONFIG_SYSTEM="+filepath.Join(t.TempDir(), "s"))
	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = repo, env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("config", "user.email", "t@t")
	git("config", "user.name", "T")
	git("commit", "-q", "--allow-empty", "-m", "base")
	base := gitOut(repo, "rev-parse", "HEAD")
	git("commit", "-q", "--allow-empty", "-m", "did the work\n\nCoop-Task: task-42")
	head := gitOut(repo, "rev-parse", "HEAD")

	if c := commitsForTask(repo, "", "task-42"); len(c) != 1 {
		t.Errorf("commitsForTask(task-42) = %v, want 1", c)
	}
	if c := commitsForTask(repo, "", "task-99"); len(c) != 0 {
		t.Errorf("commitsForTask(task-99) = %v, want none", c)
	}
	// A finished task WITH a trailer commit in range is bindable (not untrailered); one WITHOUT is.
	if m := unbindableTasks(repo, base, head, []string{"task-42"}); len(m) != 0 {
		t.Errorf("task-42 is trailered in range, should not be flagged: %v", m)
	}
	if m := unbindableTasks(repo, base, head, []string{"task-42", "task-99"}); len(m) != 1 || m[0] != "task-99" {
		t.Errorf("unbindable = %v, want [task-99]", m)
	}
	git("commit", "-q", "--allow-empty", "-m", "forge another task\n\nCoop-Task: archived-task")
	forgedHead := gitOut(repo, "rev-parse", "HEAD")
	if m := unbindableTasks(repo, base, forgedHead, []string{"task-42"}); !slices.Equal(m, []string{"task-42"}) {
		t.Errorf("foreign task binding in assigned range = %v, want [task-42]", m)
	}
	git("commit", "-q", "--allow-empty", "-m", "hide foreign task\n\nCoop-Task:\nCoop-Task: archived-task")
	hiddenForeignHead := gitOut(repo, "rev-parse", "HEAD")
	if m := unbindableTasks(repo, forgedHead, hiddenForeignHead, []string{"task-42"}); !slices.Equal(m, []string{"task-42"}) {
		t.Errorf("empty then foreign task binding = %v, want [task-42]", m)
	}
	git("reset", "--hard", "-q", head)
	// No-HEAD-change work must fail closed even if an old exact trailer is reachable: a zero-commit
	// close is valid ONLY under fresh host audit authority, so a resumed task cannot buy one by
	// pointing at history. Crash recovery restores it for a fresh range instead.
	if m := unbindableTasks(repo, head, head, []string{"task-42"}); !slices.Equal(m, []string{"task-42"}) {
		t.Errorf("unchanged HEAD used historical task binding: %v", m)
	}
	if m := unbindableTasks(repo, head, head, []string{"task-4", "task"}); len(m) != 2 || m[0] != "task-4" || m[1] != "task" {
		t.Errorf("different ids and substrings must remain unbindable, got %v", m)
	}
	if m := unbindableTasks(repo, "", head, []string{"task-42"}); !slices.Equal(m, []string{"task-42"}) {
		t.Errorf("unknown iteration base must fail closed, got %v", m)
	}
	if m := unbindableTasks(repo, head, "", []string{"task-42"}); !slices.Equal(m, []string{"task-42"}) {
		t.Errorf("unknown iteration head must fail closed, got %v", m)
	}

	// Once HEAD changes, an older valid trailer cannot bless fresh unbound work.
	git("commit", "-q", "--allow-empty", "-m", "fresh rework without a trailer")
	unboundHead := gitOut(repo, "rev-parse", "HEAD")
	if m := unbindableTasks(repo, head, unboundHead, []string{"task-42"}); !slices.Equal(m, []string{"task-42"}) {
		t.Errorf("historical-only binding after fresh work = %v, want [task-42]", m)
	}

	// A trailer-like line outside Git's final contiguous trailer block is not a trailer.
	git("commit", "-q", "--allow-empty", "-m", "malformed\n\nCoop-Task: task-42\n\nCo-authored-by: T <t@t>")
	malformedHead := gitOut(repo, "rev-parse", "HEAD")
	if m := unbindableTasks(repo, unboundHead, malformedHead, []string{"task-42"}); !slices.Equal(m, []string{"task-42"}) {
		t.Errorf("malformed trailer binding = %v, want [task-42]", m)
	}

	// Multiple Coop-Task values are ambiguous even when both values happen to match.
	git("commit", "-q", "--allow-empty", "-m", "duplicate\n\nCoop-Task: task-42\nCoop-Task: task-42")
	duplicateHead := gitOut(repo, "rev-parse", "HEAD")
	if c := commitsForTask(repo, malformedHead+".."+duplicateHead, "task-42"); len(c) != 0 {
		t.Errorf("duplicate trailers must not bind, got commits %v", c)
	}
	if m := unbindableTasks(repo, malformedHead, duplicateHead, []string{"task-42"}); !slices.Equal(m, []string{"task-42"}) {
		t.Errorf("duplicate trailer binding = %v, want [task-42]", m)
	}

	git("commit", "-q", "--allow-empty", "-m", "valid again\n\nCoop-Task: task-42")
	validHead := gitOut(repo, "rev-parse", "HEAD")
	if m := unbindableTasks(repo, duplicateHead, validHead, []string{"task-42"}); !slices.Equal(m, []string{"task-42"}) {
		t.Errorf("a second reachable binding outside the fresh range must fail closed: %v", m)
	}
	// Two individually valid commits for one task are still ambiguous: one task must bind to one
	// commit in the iteration range, not merely find at least one matching trailer somewhere in it.
	git("commit", "-q", "--allow-empty", "-m", "second valid binding\n\nCoop-Task: task-42")
	twoBindingsHead := gitOut(repo, "rev-parse", "HEAD")
	if m := unbindableTasks(repo, duplicateHead, twoBindingsHead, []string{"task-42"}); !slices.Equal(m, []string{"task-42"}) {
		t.Errorf("multiple matching commits must fail closed, got %v", m)
	}
	// landedTasks sees the trailer in the explicitly requested history.
	if !landedTasks(repo, "HEAD")["task-42"] {
		t.Error("landedTasks should include task-42")
	}
	git("commit", "-q", "--allow-empty", "-m", "ambiguous landed\n\nCoop-Task: duplicate-landed\nCoop-Task: duplicate-landed")
	if landedTasks(repo, "HEAD")["duplicate-landed"] {
		t.Error("landedTasks accepted a commit with duplicate Coop-Task trailers")
	}

	// Rewriting the existing binding makes the old commit unreachable and creates exactly one
	// range-local and reachable binding, which is the required reopened-task recovery shape.
	git("reset", "--hard", "-q", head)
	git("commit", "--amend", "-q", "--allow-empty", "-m", "reworked\n\nCoop-Task: task-42\nCoop-Recovery: fixture")
	rewrittenHead := gitOut(repo, "rev-parse", "HEAD")
	if m := unbindableTasks(repo, base, rewrittenHead, []string{"task-42"}); len(m) != 0 {
		t.Errorf("rewritten sole binding should be accepted: %v", m)
	}
	if commits := commitsForTask(repo, rewrittenHead, "task-42"); len(commits) != 1 || commits[0] != rewrittenHead[:7] {
		t.Errorf("rewritten reachable bindings = %v, want only %s", commits, rewrittenHead[:7])
	}
}

func TestUnbindableTasksIgnoresGraftAndShallowMetadata(t *testing.T) {
	hideParents := func(t *testing.T, repo, metadata, commit string) {
		t.Helper()
		path := filepath.Join(repo, ".git", "shallow")
		if metadata == "grafts" {
			path = filepath.Join(repo, ".git", "info", "grafts")
		}
		if err := os.WriteFile(path, []byte(commit+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for _, metadata := range []string{"grafts", "shallow"} {
		t.Run(metadata+" cannot hide older duplicate", func(t *testing.T) {
			repo, git := gitRepo(t)
			git("commit", "-q", "--allow-empty", "-m", "old binding\n\nCoop-Task: task-a")
			git("commit", "-q", "--allow-empty", "-m", "base")
			base := gitOut(repo, "rev-parse", "HEAD")
			git("commit", "-q", "--allow-empty", "-m", "new binding\n\nCoop-Task: task-a")
			head := gitOut(repo, "rev-parse", "HEAD")
			hideParents(t, repo, metadata, base)

			if got := commitsForTask(repo, head, "task-a"); len(got) != 1 {
				t.Fatalf("fixture did not hide the older binding from Git traversal: %v", got)
			}
			if got := unbindableTasks(repo, base, head, []string{"task-a"}); !slices.Equal(got, []string{"task-a"}) {
				t.Fatalf("hidden older duplicate = %v, want [task-a]", got)
			}
		})

		t.Run(metadata+" cannot remove in-range binding", func(t *testing.T) {
			repo, git := gitRepo(t)
			git("commit", "-q", "--allow-empty", "-m", "base")
			base := gitOut(repo, "rev-parse", "HEAD")
			git("commit", "-q", "--allow-empty", "-m", "binding\n\nCoop-Task: task-a")
			git("commit", "-q", "--allow-empty", "-m", "tail")
			head := gitOut(repo, "rev-parse", "HEAD")
			hideParents(t, repo, metadata, head)

			if got := commitsForTask(repo, base+".."+head, "task-a"); len(got) != 0 {
				t.Fatalf("fixture did not hide the in-range binding from Git traversal: %v", got)
			}
			if got := unbindableTasks(repo, base, head, []string{"task-a"}); len(got) != 0 {
				t.Fatalf("raw in-range binding was hidden: %v", got)
			}
		})

		t.Run(metadata+" cannot conceal foreign in-range binding", func(t *testing.T) {
			repo, git := gitRepo(t)
			git("commit", "-q", "--allow-empty", "-m", "base")
			base := gitOut(repo, "rev-parse", "HEAD")
			git("commit", "-q", "--allow-empty", "-m", "foreign\n\nCoop-Task: task-b")
			git("commit", "-q", "--allow-empty", "-m", "assigned\n\nCoop-Task: task-a")
			head := gitOut(repo, "rev-parse", "HEAD")
			hideParents(t, repo, metadata, head)

			changes := loopChanges(repo, base, head)
			if changes.invalidTaskBindings || !slices.Equal(changes.taskIDs(), []string{"task-a"}) {
				t.Fatalf("fixture did not hide the foreign binding from Git traversal: %#v", changes)
			}
			if got := unbindableTasks(repo, base, head, []string{"task-a"}); !slices.Equal(got, []string{"task-a"}) {
				t.Fatalf("hidden foreign binding = %v, want [task-a]", got)
			}
		})
	}
}

// Rewriting A makes the old B tip a sibling; replaying B puts its task binding in the proposed
// range even though B's exact introduced content and author intent are unchanged. Fresh completion
// rejects that foreign binding, while the host-captured audit generation accepts it exactly once.
func TestAuditReopenCompletionAcceptsSemanticDescendantReplay(t *testing.T) {
	repo := t.TempDir()
	env := append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "g"),
		"GIT_CONFIG_SYSTEM="+filepath.Join(t.TempDir(), "s"))
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = repo, env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("config", "user.email", "t@t")
	git("config", "user.name", "T")
	git("commit", "-q", "--allow-empty", "-m", "base")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("A\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "a.txt")
	git("commit", "-q", "-m", "A implementation\n\nCoop-Task: task-a")
	a := git("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "b.txt"), []byte("B\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "b.txt")
	git("commit", "-q", "-m", "B implementation\n\nCoop-Task: task-b")
	oldHead := git("rev-parse", "HEAD")
	reopen, err := captureAuditReopen(repo, "task-a")
	if err != nil {
		t.Fatal(err)
	}
	if got := completionUnbindableTasks(repo, oldHead, oldHead, []string{"task-a"}, &reopen); len(got) != 0 {
		t.Fatalf("verification-only audit re-close rejected: %v", got)
	}
	git("branch", "old-head")
	git("reset", "--hard", "-q", a+"^")
	git("cherry-pick", a)
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("A reworked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "a.txt")
	git("commit", "--amend", "-q", "-m", "A reworked\n\nCoop-Task: task-a\nCoop-Recovery: fixture")
	git("cherry-pick", git("rev-list", "--reverse", a+"..old-head"))
	newHead := git("rev-parse", "HEAD")
	if got := unbindableTasks(repo, oldHead, newHead, []string{"task-a"}); !slices.Equal(got, []string{"task-a"}) {
		t.Fatalf("clean-tree reproduction unexpectedly changed: got %v", got)
	}
	if got := completionUnbindableTasks(repo, oldHead, newHead, []string{"task-a"}, &reopen); len(got) != 0 {
		replayed, err := semanticHistoryCommits(repo, oldHead+".."+newHead)
		t.Fatalf("authorized semantic descendant replay rejected: %v; recorded=%#v replayed=%#v err=%v", got, reopen, replayed, err)
	}
	for _, id := range []string{"task-a", "task-b"} {
		if got := commitsForTask(repo, "HEAD", id); len(got) != 1 {
			t.Fatalf("reachable %s bindings = %v, want exactly one", id, got)
		}
	}
}

func TestCaptureAuditReopenRecordsCompleteOrderedHistory(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	write := func(name, body, message string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		git("add", name)
		git("commit", "-q", "-m", message)
		return gitOut(repo, "rev-parse", "HEAD")
	}
	write("a.txt", "A\n", "A implementation\n\nCoop-Task: task-a")
	write("manual.txt", "manual\n", "manual release note")
	write("b.txt", "B\n", "B implementation\n\nCoop-Task: task-b")
	head := write("release.txt", "release\n", "release v-next")

	record, err := captureAuditReopen(repo, "task-a")
	if err != nil {
		t.Fatal(err)
	}
	if record.Version != auditReopenVersion || record.BaselineHead != head ||
		record.History == nil || len(record.History) != 3 || len(record.Descendants) != 0 {
		t.Fatalf("complete audit record = %#v", record)
	}
	wantIDs := []string{"", "task-b", ""}
	for i, want := range wantIDs {
		if record.History[i].TaskID != want {
			t.Errorf("history[%d] task = %q, want %q", i, record.History[i].TaskID, want)
		}
	}
	if record.History[0].CommitMessage != "manual release note\n" ||
		record.History[2].CommitMessage != "release v-next\n" {
		t.Fatalf("unbound messages were not captured exactly: %#v", record.History)
	}
}

func TestAuditReopenHistoryEnumeratorFailsClosed(t *testing.T) {
	newRepo := func(t *testing.T) (string, func(...string), string) {
		t.Helper()
		repo, git := gitRepo(t)
		git("commit", "-q", "--allow-empty", "-m", "base")
		git("commit", "-q", "--allow-empty", "-m", "subject\n\nCoop-Task: task-a")
		return repo, git, gitOut(repo, "rev-parse", "HEAD")
	}

	t.Run("malformed task trailer", func(t *testing.T) {
		repo, git, _ := newRepo(t)
		git("commit", "-q", "--allow-empty", "-m", "bad\n\nCoop-Task:")
		if _, err := captureAuditReopen(repo, "task-a"); err == nil ||
			!strings.Contains(err.Error(), "invalid task binding") {
			t.Fatalf("malformed trailer error = %v", err)
		}
	})
	t.Run("multiple task trailers", func(t *testing.T) {
		repo, git, _ := newRepo(t)
		git("commit", "-q", "--allow-empty", "-m", "bad\n\nCoop-Task: task-b\nCoop-Task: task-c")
		if _, err := captureAuditReopen(repo, "task-a"); err == nil ||
			!strings.Contains(err.Error(), "invalid task binding") {
			t.Fatalf("multiple trailer error = %v", err)
		}
	})
	t.Run("prose plus trailer is unbound in both parsers", func(t *testing.T) {
		repo, git, _ := newRepo(t)
		git("commit", "-q", "--allow-empty", "-m", "bad\n\nnot a trailer\nCoop-Task: task-b")
		if got := commitsForTask(repo, "HEAD", "task-b"); len(got) != 0 {
			t.Fatalf("ordinary parser bound prose-plus-trailer commit: %v", got)
		}
		if _, err := captureAuditReopen(repo, "task-a"); err == nil ||
			!strings.Contains(err.Error(), "invalid task binding") {
			t.Fatalf("raw parser accepted prose-plus-trailer commit: %v", err)
		}
	})
	t.Run("repository trailer separators cannot hide bindings", func(t *testing.T) {
		repo, git, subject := newRepo(t)
		git("config", "trailer.separators", "=")
		git("config", "trailer.coop.key", "Coop-Task:")
		bindings, ok := rawTaskBindings(repo, subject)
		if !ok || !slices.Equal(bindings["task-a"], []string{subject}) {
			t.Fatalf("raw task bindings = %#v, ok=%v", bindings, ok)
		}
	})
	t.Run("duplicate task ids", func(t *testing.T) {
		repo, git, _ := newRepo(t)
		git("commit", "-q", "--allow-empty", "-m", "B1\n\nCoop-Task: task-b")
		git("commit", "-q", "--allow-empty", "-m", "B2\n\nCoop-Task: task-b")
		if _, err := captureAuditReopen(repo, "task-a"); err == nil ||
			!strings.Contains(err.Error(), "duplicate task binding") {
			t.Fatalf("duplicate task error = %v", err)
		}
	})
	for _, metadata := range []string{"grafts", "shallow"} {
		t.Run(metadata+" cannot hide an older duplicate binding", func(t *testing.T) {
			repo, git := gitRepo(t)
			git("commit", "-q", "--allow-empty", "-m", "old A\n\nCoop-Task: task-a")
			git("commit", "-q", "--allow-empty", "-m", "unrelated pre-subject work")
			subjectParent := gitOut(repo, "rev-parse", "HEAD")
			git("commit", "-q", "--allow-empty", "-m", "new A\n\nCoop-Task: task-a")
			git("commit", "-q", "--allow-empty", "-m", "tail")
			var path, body string
			if metadata == "grafts" {
				path = filepath.Join(repo, ".git", "info", "grafts")
				body = subjectParent + "\n"
			} else {
				path = filepath.Join(repo, ".git", "shallow")
				body = subjectParent + "\n"
			}
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := captureAuditReopen(repo, "task-a"); err == nil {
				t.Fatalf("%s-hidden duplicate task binding was accepted", metadata)
			}
		})
	}
	t.Run("merge", func(t *testing.T) {
		repo, git, _ := newRepo(t)
		branch := gitOut(repo, "branch", "--show-current")
		git("checkout", "-q", "-b", "side")
		git("commit", "-q", "--allow-empty", "-m", "side")
		git("checkout", "-q", branch)
		git("commit", "-q", "--allow-empty", "-m", "main")
		git("merge", "-q", "--no-ff", "side", "-m", "merge")
		if _, err := captureAuditReopen(repo, "task-a"); err == nil ||
			!strings.Contains(err.Error(), "merge commit") {
			t.Fatalf("merge history error = %v", err)
		}
	})
	t.Run("overflow", func(t *testing.T) {
		repo, git, subject := newRepo(t)
		for i := 0; i < 3; i++ {
			git("commit", "-q", "--allow-empty", "-m", "manual")
		}
		if _, err := semanticHistoryCommitsLimit(repo, subject+"..HEAD", 2); err == nil ||
			!strings.Contains(err.Error(), "exceeds 2") {
			t.Fatalf("history overflow error = %v", err)
		}
	})
	t.Run("batch diff boundaries handle empty commits and hex paths", func(t *testing.T) {
		repo, git, subject := newRepo(t)
		name := strings.Repeat("a", 40)
		var heads []string
		if err := os.WriteFile(filepath.Join(repo, name), []byte("hex path\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		git("add", name)
		git("commit", "-q", "-m", "hex-shaped path")
		heads = append(heads, gitOut(repo, "rev-parse", "HEAD"))
		if err := os.Chmod(filepath.Join(repo, name), 0o700); err != nil {
			t.Fatal(err)
		}
		git("add", name)
		git("commit", "-q", "-m", "mode change")
		heads = append(heads, gitOut(repo, "rev-parse", "HEAD"))
		if err := os.Remove(filepath.Join(repo, name)); err != nil {
			t.Fatal(err)
		}
		git("add", "-u")
		git("commit", "-q", "-m", "delete hex-shaped path")
		heads = append(heads, gitOut(repo, "rev-parse", "HEAD"))
		git("commit", "-q", "--allow-empty", "-m", "empty manual marker")
		heads = append(heads, gitOut(repo, "rev-parse", "HEAD"))
		history, err := semanticHistoryCommits(repo, subject+"..HEAD")
		if err != nil || len(history) != len(heads) {
			t.Fatalf("batched complete history = %#v, err=%v", history, err)
		}
		for i, commit := range history {
			want, err := semanticCommit(repo, heads[i], "")
			if err != nil {
				t.Fatal(err)
			}
			if commit.sha != heads[i] || commit.semantic != want {
				t.Errorf("history[%d] = %#v, want sha %s semantic %#v", i, commit, heads[i], want)
			}
		}
	})
	t.Run("representative bounded history is extracted in one batch", func(t *testing.T) {
		repo, git, subject := newRepo(t)
		const count = 128
		for i := 0; i < count; i++ {
			git("commit", "-q", "--allow-empty", "-m", "manual marker")
		}
		history, err := semanticHistoryCommitsLimit(repo, subject+"..HEAD", count)
		if err != nil || len(history) != count {
			t.Fatalf("representative batch length = %d, err=%v", len(history), err)
		}
	})
}

func TestAuditTaskTrailersUseFixedRawGrammar(t *testing.T) {
	tests := []struct {
		name    string
		message string
		values  []string
		invalid bool
	}{
		{name: "ordinary", message: "subject\n\nCoop-Task: task-a\n", values: []string{"task-a"}},
		{name: "case insensitive key", message: "subject\n\ncoop-task: task-a", values: []string{"task-a"}},
		{name: "other trailers", message: "subject\n\nOther: value\nCoop-Task: task-a", values: []string{"task-a"}},
		{name: "not final paragraph", message: "Coop-Task: decoy\n\nsubject"},
		{
			name: "prose in final paragraph", message: "subject\n\nnot a trailer\nCoop-Task: task-a",
			values: []string{"task-a"}, invalid: true,
		},
		{name: "empty", message: "subject\n\nCoop-Task:", invalid: true},
		{
			name: "duplicate", message: "subject\n\nCoop-Task: task-a\nCoop-Task: task-b",
			values: []string{"task-a", "task-b"},
		},
		{
			name: "folded", message: "subject\n\nCoop-Task: task-a\n continuation",
			values: []string{"task-a"}, invalid: true,
		},
		{
			name:    "oversized value",
			message: "subject\n\nCoop-Task: " + strings.Repeat("x", auditTaskTrailerValueLimit+1),
			invalid: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values, invalid := auditTaskTrailersFromMessage([]byte(tt.message))
			if !slices.Equal(values, tt.values) || invalid != tt.invalid {
				t.Fatalf("trailers = %v, invalid=%v; want %v, %v", values, invalid, tt.values, tt.invalid)
			}
		})
	}
}

func TestOrdinaryAuditBindingIdentityRejectsGraftedDecoy(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "root")
	main := gitOut(repo, "branch", "--show-current")
	git("checkout", "-q", "-b", "decoy")
	git("commit", "-q", "--allow-empty", "-m", "decoy\n\nCoop-Task=task-a")
	decoy := gitOut(repo, "rev-parse", "HEAD")
	git("checkout", "-q", main)
	git("commit", "-q", "--allow-empty", "-m", "subject\n\nCoop-Task: task-a")
	subject := gitOut(repo, "rev-parse", "HEAD")
	git("config", "trailer.separators", "=")
	if err := os.WriteFile(
		filepath.Join(repo, ".git", "info", "grafts"),
		[]byte(subject+" "+decoy+"\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	ordinary := commitsForTask(repo, subject, "task-a")
	raw, ok := rawTaskBindings(repo, subject)
	if !ok || len(raw["task-a"]) != 1 || raw["task-a"][0] != subject {
		t.Fatalf("decoy fixture ordinary=%v raw=%v ok=%v", ordinary, raw, ok)
	}
	// Git versions disagree on whether the configured "=" separator yields the grafted decoy or
	// no binding. Either way, the config-sensitive traversal must not identify the raw subject.
	for _, sha := range ordinary {
		if gitOut(repo, "rev-parse", "--verify", sha+"^{commit}") == subject {
			t.Fatalf("configured traversal unexpectedly identified raw subject: ordinary=%v", ordinary)
		}
	}
	if ordinaryBindingMatchesRaw(repo, subject, "task-a") {
		t.Fatal("grafted ordinary decoy matched the distinct raw audit subject")
	}
}

func TestAuditTreeParserRejectsNoncanonicalDirectoryMode(t *testing.T) {
	raw := append([]byte("040000 dir\x00"), make([]byte, 20)...)
	if _, _, err := auditTreeChildren(raw, 20); err == nil ||
		!strings.Contains(err.Error(), "noncanonical") {
		t.Fatalf("zero-padded directory mode error = %v", err)
	}
}

func TestAuditCommitParent(t *testing.T) {
	total := int64(auditLinearHistoryByteLimit)
	if err := addAuditLinearHistoryBytes(&total, []byte{0}); err == nil ||
		!strings.Contains(err.Error(), "linear audit history") {
		t.Fatalf("linear history byte bound error = %v", err)
	}

	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "root")
	root := gitOut(repo, "rev-parse", "HEAD")
	if got, err := auditCommitParent(repo, root); err != nil || got != "" {
		t.Fatalf("root parent = %q, %v; want empty", got, err)
	}

	git("commit", "-q", "--allow-empty", "-m", "child")
	child := gitOut(repo, "rev-parse", "HEAD")
	if got, err := auditCommitParent(repo, child); err != nil || got != root {
		t.Fatalf("child parent = %q, %v; want %s", got, err, root)
	}
	if _, err := rawReachableAuditCommitsLimit(repo, child, 10, 10, 1); err == nil ||
		!strings.Contains(err.Error(), "exceeds 1 bytes") {
		t.Fatalf("raw reachable byte bound error = %v", err)
	}
	if _, err := auditCommitParent(repo, strings.Repeat("f", 40)); err == nil {
		t.Fatal("missing commit parent was accepted")
	}

	writeRawCommit := func(body string) string {
		t.Helper()
		cmd := exec.Command("git", gitArgs(repo,
			[]string{"hash-object", "-t", "commit", "-w", "--stdin", "--literally"})...)
		cmd.Stdin = strings.NewReader(body)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	tree := gitOut(repo, "rev-parse", child+"^{tree}")
	identity := "Test User <test@example.com> 1 +0000"
	misplacedParent := writeRawCommit(
		"tree " + tree + "\n" +
			"author " + identity + "\n" +
			"parent " + root + "\n" +
			"committer " + identity + "\n\nmalformed parent placement\n",
	)
	if _, err := auditCommitParent(repo, misplacedParent); err == nil {
		t.Error("parent after author header was accepted")
	}
	danglingParent := writeRawCommit(
		"tree " + tree + "\n" +
			"parent " + strings.Repeat("f", 40) + "\n" +
			"author " + identity + "\n" +
			"committer " + identity + "\n\ndangling parent\n",
	)
	if _, err := auditCommitParent(repo, danglingParent); err == nil {
		t.Error("dangling parent was accepted")
	}
	duplicateParent := writeRawCommit(
		"tree " + tree + "\n" +
			"parent " + root + "\n" +
			"parent " + root + "\n" +
			"author " + identity + "\n" +
			"committer " + identity + "\n\nduplicate parent\n",
	)
	if _, err := auditCommitParent(repo, duplicateParent); err == nil {
		t.Error("duplicate raw parent was accepted")
	}
	oversizedCommit := writeRawCommit(
		"tree " + tree + "\n" +
			"author " + identity + "\n" +
			"committer " + identity + "\n\n" +
			strings.Repeat("x", 1<<20),
	)
	if _, err := rawAuditHistoryCount(repo, oversizedCommit, 1); err == nil {
		t.Error("oversized raw commit was accepted")
	}

	blobCmd := exec.Command("git", gitArgs(repo, []string{"hash-object", "-t", "blob", "-w", "--stdin"})...)
	blobCmd.Stdin = strings.NewReader(strings.Repeat("x", 1<<20))
	blobOut, err := blobCmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	blobParent := strings.TrimSpace(string(blobOut))
	limitedBlob := exec.Command("git", gitArgs(repo, []string{"cat-file", "blob", blobParent})...)
	if _, err := auditCommandOutput(limitedBlob, 1024); err == nil {
		t.Error("bounded command output accepted an oversized stream")
	}
	blobParentCommit := writeRawCommit(
		"tree " + tree + "\n" +
			"parent " + blobParent + "\n" +
			"author " + identity + "\n" +
			"committer " + identity + "\n\nblob parent\n",
	)
	badParent := make(chan error, 1)
	go func() {
		_, err := rawAuditHistoryCount(repo, blobParentCommit, 1)
		badParent <- err
	}()
	select {
	case err := <-badParent:
		if err == nil {
			t.Error("blob parent was accepted as a commit")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blob parent left cat-file blocked on unread output")
	}

	branch := gitOut(repo, "branch", "--show-current")
	git("checkout", "-q", "-b", "parent-side", root)
	git("commit", "-q", "--allow-empty", "-m", "side")
	git("checkout", "-q", branch)
	git("commit", "-q", "--allow-empty", "-m", "main")
	git("merge", "-q", "--no-ff", "parent-side", "-m", "merge")
	if _, err := auditCommitParent(repo, gitOut(repo, "rev-parse", "HEAD")); err == nil {
		t.Fatal("merge commit parent was accepted")
	}
}

func TestSemanticHistoryRejectsOversizedTreeBeforeDiff(t *testing.T) {
	repo, _ := gitRepo(t)
	writeObject := func(objectType, body string) string {
		t.Helper()
		cmd := exec.Command("git", gitArgs(repo,
			[]string{"hash-object", "-t", objectType, "-w", "--stdin", "--literally"})...)
		cmd.Stdin = strings.NewReader(body)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	tree := writeObject("tree", strings.Repeat("x", auditTreeObjectSizeLimit+1))
	identity := "Test User <test@example.com> 1 +0000"
	commit := writeObject(
		"commit",
		"tree "+tree+"\n"+
			"author "+identity+"\n"+
			"committer "+identity+"\n\noversized tree\n",
	)
	rawHistory, err := rawAuditHistoryCount(repo, commit, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := semanticHistoryCommitsExact(repo, rawHistory); err == nil ||
		!strings.Contains(err.Error(), "tree "+tree+" size") {
		t.Fatalf("oversized tree error = %v", err)
	}
}

func TestAuditTreeSnapshotSurvivesSourceRemoval(t *testing.T) {
	repo, git := gitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("A\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "a.txt")
	git("commit", "-q", "-m", "tree")
	tree := gitOut(repo, "rev-parse", "HEAD^{tree}")
	empty := auditObjectID("tree", nil, len(tree))
	snapshot, err := snapshotAuditTreeDAGs(repo, []string{empty, tree})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(snapshot) })
	if err := os.Remove(filepath.Join(repo, ".git", "objects", tree[:2], tree[2:])); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", gitArgs(snapshot, []string{"diff-tree", "--raw", "-r", empty, tree})...)
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		t.Fatalf("snapshot diff after source removal = %q, %v", out, err)
	}
}

func TestSemanticHistoryExactIgnoresGrafts(t *testing.T) {
	repo, git := gitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "a.txt")
	git("commit", "-q", "-m", "parent")
	parent := gitOut(repo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("commit", "-qam", "child")
	child := gitOut(repo, "rev-parse", "HEAD")
	rawHistory, err := rawAuditHistoryCount(repo, child, 1)
	if err != nil {
		t.Fatal(err)
	}
	want, err := semanticHistoryCommitsExact(repo, rawHistory)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("decoy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "a.txt")
	decoyTree := gitOut(repo, "write-tree")
	decoyParent := gitOut(repo, "commit-tree", decoyTree, "-p", parent, "-m", "decoy parent")
	if err := os.WriteFile(
		filepath.Join(repo, ".git", "info", "grafts"),
		[]byte(child+" "+decoyParent+"\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	got, err := semanticHistoryCommitsExact(repo, rawHistory)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("graft changed exact semantic history: got %#v want %#v", got, want)
	}
}

func TestSemanticHistoryExactSupportsSHA256Root(t *testing.T) {
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "--object-format=sha256")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	run("config", "commit.gpgsign", "false")
	emptyTree, err := auditEmptyTree(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(emptyTree) != 64 {
		t.Fatalf("SHA-256 empty tree length = %d, want 64", len(emptyTree))
	}
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("A\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "a.txt")
	run("commit", "-q", "-m", "A implementation\n\nCoop-Task: task-a")
	if err := os.WriteFile(filepath.Join(repo, "b.txt"), []byte("B\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "b.txt")
	run("commit", "-q", "-m", "B implementation\n\nCoop-Task: task-b")
	oldHead := gitOut(repo, "rev-parse", "HEAD")
	if len(oldHead) != 64 {
		t.Fatalf("SHA-256 HEAD length = %d, want 64", len(oldHead))
	}
	descendant := commitsForTask(repo, oldHead, "task-b")
	if len(descendant) != 1 {
		t.Fatalf("descendant bindings = %v, want exactly one", descendant)
	}
	record, err := captureAuditReopen(repo, "task-a")
	if err != nil {
		t.Fatal(err)
	}

	run("checkout", "-q", "--orphan", "rewritten-root")
	run("rm", "-q", "-rf", ".")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("A repaired\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "a.txt")
	run("commit", "-q", "-m", "A repaired\n\nCoop-Task: task-a")
	run("cherry-pick", descendant[0])
	head := gitOut(repo, "rev-parse", "HEAD")
	if !auditReopenCompletionValid(repo, oldHead, head, "task-a", record) {
		t.Fatal("SHA-256 root subject with exact semantic descendant replay was rejected")
	}
}

func TestAuditReopenCompletionAcceptsRootSubjectReplay(t *testing.T) {
	repo, git := gitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("A\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "a.txt")
	git("commit", "-q", "-m", "A implementation\n\nCoop-Task: task-a")
	if err := os.WriteFile(filepath.Join(repo, "b.txt"), []byte("B\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "b.txt")
	git("commit", "-q", "-m", "B implementation\n\nCoop-Task: task-b")
	oldHead := gitOut(repo, "rev-parse", "HEAD")
	descendant := commitsForTask(repo, oldHead, "task-b")
	if len(descendant) != 1 {
		t.Fatalf("descendant bindings = %v, want exactly one", descendant)
	}
	record, err := captureAuditReopen(repo, "task-a")
	if err != nil {
		t.Fatal(err)
	}

	git("checkout", "-q", "--orphan", "rewritten-root")
	git("rm", "-q", "-rf", ".")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("A repaired\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "a.txt")
	git("commit", "-q", "-m", "A repaired\n\nCoop-Task: task-a")
	git("cherry-pick", descendant[0])
	head := gitOut(repo, "rev-parse", "HEAD")
	if !auditReopenCompletionValid(repo, oldHead, head, "task-a", record) {
		t.Fatal("root subject with exact semantic descendant replay was rejected")
	}
}

func TestAuditReopenCompletionProtectsUnboundHistory(t *testing.T) {
	type fixture struct {
		repo                     string
		git                      func(...string)
		subjectParent, a, manual string
		bound, tail, base        string
		record                   auditReopenRecord
	}
	type graftFixture struct {
		repo                   string
		git                    func(...string)
		root, subjectParent, a string
		manual, bound, tail    string
		record                 auditReopenRecord
	}
	newFixture := func(t *testing.T) fixture {
		t.Helper()
		repo, git := gitRepo(t)
		git("commit", "-q", "--allow-empty", "-m", "base")
		commit := func(name, body, message string) string {
			t.Helper()
			if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			git("add", name)
			git("commit", "-q", "-m", message)
			return gitOut(repo, "rev-parse", "HEAD")
		}
		subjectParent := commit("before.txt", "before\n", "unrelated pre-subject work")
		a := commit("a.txt", "A\n", "A implementation\n\nCoop-Task: task-a")
		manual := commit("manual.txt", "manual\n", "manual release step")
		bound := commit("b.txt", "B\n", "B implementation\n\nCoop-Task: task-b")
		tail := commit("tail.txt", "tail\n", "unbound release tail")
		record, err := captureAuditReopen(repo, "task-a")
		if err != nil {
			t.Fatal(err)
		}
		return fixture{
			repo: repo, git: git, subjectParent: subjectParent, a: a, manual: manual,
			bound: bound, tail: tail, base: tail, record: record,
		}
	}
	newGraftFixture := func(t *testing.T) graftFixture {
		t.Helper()
		repo, git := gitRepo(t)
		git("commit", "-q", "--allow-empty", "-m", "base")
		root := gitOut(repo, "rev-parse", "HEAD")
		git("commit", "-q", "--allow-empty", "-m", "unrelated pre-subject work")
		subjectParent := gitOut(repo, "rev-parse", "HEAD")
		commit := func(name, body, message string) string {
			t.Helper()
			if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			git("add", name)
			git("commit", "-q", "-m", message)
			return gitOut(repo, "rev-parse", "HEAD")
		}
		a := commit("a.txt", "A\n", "A implementation\n\nCoop-Task: task-a")
		manual := commit("manual.txt", "manual\n", "manual release step")
		bound := commit("b.txt", "B\n", "B implementation\n\nCoop-Task: task-b")
		tail := commit("tail.txt", "tail\n", "unbound release tail")
		record, err := captureAuditReopen(repo, "task-a")
		if err != nil {
			t.Fatal(err)
		}
		return graftFixture{
			repo: repo, git: git, root: root, subjectParent: subjectParent, a: a,
			manual: manual, bound: bound, tail: tail, record: record,
		}
	}
	rewrite := func(t *testing.T, f fixture) {
		t.Helper()
		f.git("reset", "--hard", "-q", f.a+"^")
		if err := os.WriteFile(filepath.Join(f.repo, "a.txt"), []byte("A repaired\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		f.git("add", "a.txt")
		f.git("commit", "-q", "-m", "A repaired\n\nCoop-Task: task-a")
	}
	assertValid := func(t *testing.T, f fixture) {
		t.Helper()
		head := gitOut(f.repo, "rev-parse", "HEAD")
		if !auditReopenCompletionValid(f.repo, f.base, head, "task-a", f.record) {
			t.Fatal("exact complete-history replay was rejected")
		}
	}
	assertRejected := func(t *testing.T, f fixture) {
		t.Helper()
		head := gitOut(f.repo, "rev-parse", "HEAD")
		if auditReopenCompletionValid(f.repo, f.base, head, "task-a", f.record) {
			t.Fatal("unsafe unbound history replay was accepted")
		}
	}

	t.Run("exact replay with new committer metadata", func(t *testing.T) {
		f := newFixture(t)
		rewrite(t, f)
		f.git("cherry-pick", f.manual, f.bound, f.tail)
		assertValid(t, f)
	})
	t.Run("dropped subject parent", func(t *testing.T) {
		f := newFixture(t)
		f.git("reset", "--hard", "-q", f.subjectParent+"^")
		if err := os.WriteFile(filepath.Join(f.repo, "a.txt"), []byte("A repaired\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		f.git("add", "a.txt")
		f.git("commit", "-q", "-m", "A repaired\n\nCoop-Task: task-a")
		f.git("cherry-pick", f.manual, f.bound, f.tail)
		assertRejected(t, f)
	})
	t.Run("replace ref cannot forge subject parent", func(t *testing.T) {
		f := newFixture(t)
		f.git("reset", "--hard", "-q", f.subjectParent+"^")
		if err := os.WriteFile(filepath.Join(f.repo, "a.txt"), []byte("A repaired\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		f.git("add", "a.txt")
		f.git("commit", "-q", "-m", "A repaired\n\nCoop-Task: task-a")
		rewrittenSubject := gitOut(f.repo, "rev-parse", "HEAD")
		f.git("cherry-pick", f.manual, f.bound, f.tail)
		f.git("replace", "--graft", rewrittenSubject, f.subjectParent)
		assertRejected(t, f)
	})
	t.Run("graft cannot redirect reviewed subject", func(t *testing.T) {
		f := newGraftFixture(t)
		f.git("reset", "--hard", "-q", f.root)
		f.git("cherry-pick", f.a, f.manual, f.bound)
		redirectedParent := gitOut(f.repo, "rev-parse", "HEAD")
		if err := os.WriteFile(
			filepath.Join(f.repo, ".git", "info", "grafts"),
			[]byte(f.tail+" "+redirectedParent+"\n"),
			0o600,
		); err != nil {
			t.Fatal(err)
		}

		f.git("reset", "--hard", "-q", f.root)
		if err := os.WriteFile(filepath.Join(f.repo, "a.txt"), []byte("A repaired\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		f.git("add", "a.txt")
		f.git("commit", "-q", "-m", "A repaired\n\nCoop-Task: task-a")
		f.git("cherry-pick", f.manual, f.bound, f.tail)
		if auditReopenCompletionValid(
			f.repo, f.tail, gitOut(f.repo, "rev-parse", "HEAD"), "task-a", f.record,
		) {
			t.Fatal("graft-selected reviewed subject was accepted")
		}
	})
	t.Run("graft cannot redirect rewritten subject", func(t *testing.T) {
		f := newGraftFixture(t)
		f.git("reset", "--hard", "-q", f.root)
		if err := os.WriteFile(filepath.Join(f.repo, "a.txt"), []byte("A repaired\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		f.git("add", "a.txt")
		f.git("commit", "-q", "-m", "A repaired\n\nCoop-Task: task-a")
		actualSubject := gitOut(f.repo, "rev-parse", "HEAD")
		f.git("cherry-pick", f.manual)
		firstDescendant := gitOut(f.repo, "rev-parse", "HEAD")
		f.git("cherry-pick", f.bound, f.tail)
		head := gitOut(f.repo, "rev-parse", "HEAD")

		tree := gitOut(f.repo, "rev-parse", actualSubject+"^{tree}")
		decoySubject := gitOut(
			f.repo, "commit-tree", tree, "-p", f.subjectParent,
			"-m", "A repaired\n\nCoop-Task: task-a",
		)
		if err := os.WriteFile(
			filepath.Join(f.repo, ".git", "info", "grafts"),
			[]byte(firstDescendant+" "+decoySubject+"\n"),
			0o600,
		); err != nil {
			t.Fatal(err)
		}
		if auditReopenCompletionValid(f.repo, f.tail, head, "task-a", f.record) {
			t.Fatal("graft-selected rewritten subject was accepted")
		}
	})
	t.Run("dropped", func(t *testing.T) {
		f := newFixture(t)
		rewrite(t, f)
		f.git("cherry-pick", f.bound, f.tail)
		assertRejected(t, f)
	})
	t.Run("changed tree", func(t *testing.T) {
		f := newFixture(t)
		rewrite(t, f)
		f.git("cherry-pick", f.manual)
		if err := os.WriteFile(filepath.Join(f.repo, "manual.txt"), []byte("changed\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		f.git("add", "manual.txt")
		f.git("commit", "--amend", "-q", "--no-edit")
		f.git("cherry-pick", f.bound, f.tail)
		assertRejected(t, f)
	})
	t.Run("changed message", func(t *testing.T) {
		f := newFixture(t)
		rewrite(t, f)
		f.git("cherry-pick", f.manual)
		f.git("commit", "--amend", "-q", "-m", "changed manual message")
		f.git("cherry-pick", f.bound, f.tail)
		assertRejected(t, f)
	})
	t.Run("changed author", func(t *testing.T) {
		f := newFixture(t)
		rewrite(t, f)
		f.git("cherry-pick", f.manual)
		f.git("commit", "--amend", "-q", "--no-edit", "--author", "Other <other@example.com>")
		f.git("cherry-pick", f.bound, f.tail)
		assertRejected(t, f)
	})
	t.Run("reordered", func(t *testing.T) {
		f := newFixture(t)
		rewrite(t, f)
		f.git("cherry-pick", f.bound, f.manual, f.tail)
		assertRejected(t, f)
	})
	t.Run("invented", func(t *testing.T) {
		f := newFixture(t)
		rewrite(t, f)
		f.git("cherry-pick", f.manual, f.bound, f.tail)
		f.git("commit", "-q", "--allow-empty", "-m", "invented unbound descendant")
		assertRejected(t, f)
	})
	t.Run("later host suffix is snapshotted", func(t *testing.T) {
		f := newFixture(t)
		if err := os.WriteFile(filepath.Join(f.repo, "suffix.txt"), []byte("suffix\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		f.git("add", "suffix.txt")
		f.git("commit", "-q", "-m", "later host suffix")
		f.base = gitOut(f.repo, "rev-parse", "HEAD")
		if !auditReopenCurrentValid(f.repo, f.base, "task-a", f.record) {
			t.Fatal("later host suffix invalidated the recorded prefix")
		}
		suffix := f.base
		rewrite(t, f)
		f.git("cherry-pick", f.manual, f.bound, f.tail, suffix)
		assertValid(t, f)
	})
}

func TestAuditReopenCompletionRejectsChangedOrInventedHistory(t *testing.T) {
	type fixture struct {
		repo, oldHead, a, b string
		reopen              auditReopenRecord
		git                 func(...string) string
	}
	newFixture := func(t *testing.T) fixture {
		t.Helper()
		repo := t.TempDir()
		env := append(os.Environ(),
			"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "g"),
			"GIT_CONFIG_SYSTEM="+filepath.Join(t.TempDir(), "s"))
		git := func(args ...string) string {
			t.Helper()
			cmd := exec.Command("git", args...)
			cmd.Dir, cmd.Env = repo, env
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
			return strings.TrimSpace(string(out))
		}
		git("init", "-q")
		git("config", "user.email", "t@t")
		git("config", "user.name", "T")
		git("commit", "-q", "--allow-empty", "-m", "base")
		if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("A\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		git("add", "a.txt")
		git("commit", "-q", "-m", "A implementation\n\nCoop-Task: task-a")
		a := git("rev-parse", "HEAD")
		if err := os.WriteFile(filepath.Join(repo, "b.txt"), []byte("B\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		git("add", "b.txt")
		git("commit", "-q", "-m", "B implementation\n\nCoop-Task: task-b")
		b := git("rev-parse", "HEAD")
		reopen, err := captureAuditReopen(repo, "task-a")
		if err != nil {
			t.Fatal(err)
		}
		git("branch", "old-head")
		return fixture{repo: repo, oldHead: b, a: a, b: b, reopen: reopen, git: git}
	}
	rewriteA := func(t *testing.T, f fixture) {
		t.Helper()
		f.git("reset", "--hard", "-q", f.a+"^")
		if err := os.WriteFile(filepath.Join(f.repo, "a.txt"), []byte("A repaired\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		f.git("add", "a.txt")
		f.git("commit", "-q", "-m", "A repaired\n\nCoop-Task: task-a")
	}
	rejected := func(t *testing.T, f fixture) {
		t.Helper()
		head := f.git("rev-parse", "HEAD")
		if got := completionUnbindableTasks(f.repo, f.oldHead, head, []string{"task-a"}, &f.reopen); !slices.Equal(got, []string{"task-a"}) {
			t.Fatalf("unsafe audit recovery accepted: %v", got)
		}
	}

	t.Run("changed descendant", func(t *testing.T) {
		f := newFixture(t)
		rewriteA(t, f)
		f.git("cherry-pick", f.b)
		if err := os.WriteFile(filepath.Join(f.repo, "b.txt"), []byte("B changed\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		f.git("add", "b.txt")
		f.git("commit", "--amend", "-q", "--no-edit")
		rejected(t, f)
	})
	t.Run("new foreign binding", func(t *testing.T) {
		f := newFixture(t)
		rewriteA(t, f)
		f.git("cherry-pick", f.b)
		f.git("commit", "-q", "--allow-empty", "-m", "C\n\nCoop-Task: task-c")
		rejected(t, f)
	})
	t.Run("duplicate subject binding", func(t *testing.T) {
		f := newFixture(t)
		f.git("commit", "-q", "--allow-empty", "-m", "duplicate A\n\nCoop-Task: task-a")
		rejected(t, f)
	})
	t.Run("empty receipt commit", func(t *testing.T) {
		f := newFixture(t)
		f.git("commit", "-q", "--allow-empty", "-m", "receipt only")
		rejected(t, f)
	})
	t.Run("message-only subject rewrite", func(t *testing.T) {
		f := newFixture(t)
		f.git("reset", "--hard", "-q", f.a)
		f.git("commit", "--amend", "-q", "--no-edit", "--trailer", "Coop-Recovery: forged")
		f.git("cherry-pick", f.b)
		rejected(t, f)
	})
	t.Run("record cannot authorize another task", func(t *testing.T) {
		f := newFixture(t)
		if got := completionUnbindableTasks(f.repo, f.oldHead, f.oldHead, []string{"task-b"}, &f.reopen); !slices.Equal(got, []string{"task-b"}) {
			t.Fatalf("task A recovery authorized task B: %v", got)
		}
	})
}

type legacyAuditAdoptionFixture struct {
	repo, root, id, head, subjectHead string
	task                              taskItem
	record                            auditReopenRecord
	git                               func(...string)
}

func newLegacyAuditAdoptionFixture(t *testing.T) legacyAuditAdoptionFixture {
	t.Helper()
	repo, git := gitRepo(t)
	t.Setenv(testLeaseAuthorityRootEnv, t.TempDir())
	git("commit", "-q", "--allow-empty", "-m", "base")
	commit := func(name, body, message string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		git("add", name)
		git("commit", "-q", "-m", message)
		return gitOut(repo, "rev-parse", "HEAD")
	}
	id := "legacy-adoption"
	subjectHead := commit("a.txt", "A\n", "A implementation\n\nCoop-Task: "+id)
	commit("manual.txt", "manual\n", "manual release")
	descendantHead := commit("b.txt", "B\n", "B implementation\n\nCoop-Task: legacy-descendant")
	subject, err := semanticCommit(repo, subjectHead, id)
	if err != nil {
		t.Fatal(err)
	}
	descendant, err := semanticCommit(repo, descendantHead, "legacy-descendant")
	if err != nil {
		t.Fatal(err)
	}
	record := auditReopenRecord{
		Version: auditReopenLegacyVersion, Generation: "legacy-generation", TaskID: id,
		Subject: subject, Descendants: []auditReopenCommit{descendant},
	}
	root := filepath.Join(repo, tasksRoot)
	task := taskForLease(t, root, stateBlocked, id)
	writeTaskFile(t, filepath.Join(task.Dir, "decision.md"), "# Decision\n\n**Resolution:** <!-- unresolved -->\n")
	authority, err := openLeaseAuthority(root, id, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writeAuditReopenRecord(root, record); err != nil {
		t.Fatal(err)
	}
	return legacyAuditAdoptionFixture{
		repo: repo, root: root, id: id, head: descendantHead, subjectHead: subjectHead,
		task: task, record: record, git: git,
	}
}

func TestLegacyAuditReopenAdoption(t *testing.T) {
	t.Run("ordinary unblock names the required adoption retry", func(t *testing.T) {
		f := newLegacyAuditAdoptionFixture(t)
		code, err := tasksFolderUnblock(f.root, []string{f.id, "restored baseline"})
		want := "retry: coop tasks unblock " + f.id + ` --adopt-audit-head <full-sha> "<answer>"`
		if code != -1 || err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("legacy retry = code %d err %v, want %q", code, err, want)
		}
		if decisionResolved(filepath.Join(f.task.Dir, "decision.md")) {
			t.Fatal("rejected legacy unblock recorded its answer")
		}
	})
	t.Run("interactive answer returns the adoption command", func(t *testing.T) {
		f := newLegacyAuditAdoptionFixture(t)
		var out strings.Builder
		code, err := runDecisionBrowser(
			[]decisionRef{{root: f.root, id: f.id}},
			strings.NewReader("restored baseline\n"),
			&out,
		)
		want := "retry: coop tasks unblock " + f.id + ` --adopt-audit-head <full-sha> "<answer>"`
		if code != -1 || err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("interactive legacy retry = code %d err %v, want %q", code, err, want)
		}
		if decisionResolved(filepath.Join(f.task.Dir, "decision.md")) {
			t.Fatal("rejected interactive legacy answer was recorded")
		}
	})
	t.Run("exact HEAD captures complete history and retains generation", func(t *testing.T) {
		f := newLegacyAuditAdoptionFixture(t)
		code, err := tasksFolderUnblock(f.root, []string{
			f.id, "--adopt-audit-head", f.head, "restored audited baseline",
		})
		if code != 0 || err != nil {
			t.Fatalf("legacy adoption = code %d err %v", code, err)
		}
		current, ok := currentTask(f.root, f.id)
		if !ok || current.State != stateTodo {
			t.Fatalf("adopted task = %#v, ok=%v", current, ok)
		}
		record, ok, err := readAuditReopenRecord(f.root, f.id)
		if err != nil || !ok || !auditReopenRecordActive(record) {
			t.Fatalf("adopted authority = %#v, ok=%v err=%v", record, ok, err)
		}
		if record.Generation != f.record.Generation || record.BaselineHead != f.head ||
			len(record.History) != 2 || record.History[0].TaskID != "" ||
			record.History[1].TaskID != "legacy-descendant" || len(record.Descendants) != 0 {
			t.Fatalf("adopted complete history = %#v", record)
		}
	})
	t.Run("wrong current SHA fails closed", func(t *testing.T) {
		f := newLegacyAuditAdoptionFixture(t)
		code, err := tasksFolderUnblock(f.root, []string{
			f.id, "--adopt-audit-head", f.subjectHead, "must not be recorded",
		})
		if code != -1 || err == nil ||
			!strings.Contains(err.Error(), "was authorized for "+f.subjectHead) ||
			!strings.Contains(err.Error(), "restore "+f.subjectHead+" exactly") ||
			!strings.Contains(err.Error(), "same --adopt-audit-head value") {
			t.Fatalf("stale adoption = code %d err %v", code, err)
		}
		got, ok, readErr := readAuditReopenRecord(f.root, f.id)
		if readErr != nil || !ok || !sameAuditReopenRecord(got, f.record) ||
			!pathExists(f.task.Dir) || decisionResolved(filepath.Join(f.task.Dir, "decision.md")) {
			t.Fatalf("stale adoption mutated state: record=%#v ok=%v err=%v", got, ok, readErr)
		}
	})
	t.Run("unbound drift does not replace the supplied audited SHA", func(t *testing.T) {
		f := newLegacyAuditAdoptionFixture(t)
		auditedHead := f.head
		f.git("commit", "-q", "--allow-empty", "-m", "later unbound drift")
		currentHead := gitOut(f.repo, "rev-parse", "HEAD")
		code, err := tasksFolderUnblock(f.root, []string{
			f.id, "--adopt-audit-head", auditedHead, "must not be recorded",
		})
		if code != -1 || err == nil ||
			!strings.Contains(err.Error(), auditedHead) ||
			!strings.Contains(err.Error(), currentHead) ||
			!strings.Contains(err.Error(), "restore "+auditedHead+" exactly") {
			t.Fatalf("unbound drift adoption = code %d err %v", code, err)
		}
		if strings.Contains(err.Error(), "--adopt-audit-head "+currentHead) {
			t.Fatalf("drift error suggested adopting current HEAD: %v", err)
		}
		got, ok, readErr := readAuditReopenRecord(f.root, f.id)
		if readErr != nil || !ok || !sameAuditReopenRecord(got, f.record) ||
			!pathExists(f.task.Dir) || decisionResolved(filepath.Join(f.task.Dir, "decision.md")) {
			t.Fatalf("unbound drift mutated state: record=%#v ok=%v err=%v", got, ok, readErr)
		}
	})
	t.Run("changed legacy projection fails closed", func(t *testing.T) {
		f := newLegacyAuditAdoptionFixture(t)
		f.git("commit", "--amend", "-q", "-m", "changed B\n\nCoop-Task: legacy-descendant")
		head := gitOut(f.repo, "rev-parse", "HEAD")
		code, err := tasksFolderUnblock(f.root, []string{
			f.id, "--adopt-audit-head", head, "must not be recorded",
		})
		if code != -1 || err == nil || !strings.Contains(err.Error(), "legacy subject and task-bound descendant projection") {
			t.Fatalf("changed projection adoption = code %d err %v", code, err)
		}
		got, ok, readErr := readAuditReopenRecord(f.root, f.id)
		if readErr != nil || !ok || !sameAuditReopenRecord(got, f.record) || !pathExists(f.task.Dir) {
			t.Fatalf("changed projection mutated state: record=%#v ok=%v err=%v", got, ok, readErr)
		}
	})
}

type blockedAuditUpgradeFixture struct {
	repo, root, authorityRoot, id string
	subjectParent, descendant     string
	task                          taskItem
	record                        auditReopenRecord
	git                           func(...string)
}

func newBlockedAuditUpgradeFixture(t *testing.T) blockedAuditUpgradeFixture {
	t.Helper()
	repo, git := gitRepo(t)
	authorityRoot := t.TempDir()
	t.Setenv(testLeaseAuthorityRootEnv, authorityRoot)
	git("commit", "-q", "--allow-empty", "-m", "base")
	id := "blocked-upgrade"
	git("commit", "-q", "--allow-empty", "-m", "unrelated pre-subject work")
	subjectParent := gitOut(repo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("A\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "a.txt")
	git("commit", "-q", "-m", "A implementation\n\nCoop-Task: "+id)
	a := gitOut(repo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "b.txt"), []byte("B\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "b.txt")
	git("commit", "-q", "-m", "B implementation\n\nCoop-Task: blocked-upgrade-descendant")
	b := gitOut(repo, "rev-parse", "HEAD")
	record, err := captureAuditReopen(repo, id)
	if err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(repo, tasksRoot)
	task := taskForLease(t, root, stateBlocked, id)
	writeTaskFile(t, filepath.Join(task.Dir, "decision.md"), "# Decision\n\n**Resolution:** <!-- unresolved -->\n")
	authority, err := openLeaseAuthority(root, id, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writeAuditReopenRecord(root, record); err != nil {
		t.Fatal(err)
	}

	git("reset", "--hard", "-q", a+"^")
	git("cherry-pick", a)
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("A repaired\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "a.txt")
	git("commit", "--amend", "-q", "-m", "A repaired\n\nCoop-Task: "+id+"\nCoop-Recovery: fixture")
	git("cherry-pick", b)
	return blockedAuditUpgradeFixture{
		repo: repo, root: root, authorityRoot: authorityRoot, id: id,
		subjectParent: subjectParent, descendant: b, task: task, record: record, git: git,
	}
}

func sameAuditReopenRecord(a, b auditReopenRecord) bool {
	return auditReopenRecordsEqual(a, b)
}

func TestBlockedAuditUnblockUpgradesStaleAuthority(t *testing.T) {
	f := newBlockedAuditUpgradeFixture(t)
	if err := os.WriteFile(filepath.Join(f.repo, "later.txt"), []byte("later task\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.git("add", "later.txt")
	f.git("commit", "-q", "-m", "Later implementation\n\nCoop-Task: later-task")
	if err := resolveAndUnblock(f.root, f.task, "external acceptance passed"); err != nil {
		t.Fatal(err)
	}
	current, ok := currentTask(f.root, f.id)
	if !ok || current.State != stateTodo {
		t.Fatalf("upgraded unblock task = %#v, ok=%v", current, ok)
	}
	got, ok, err := readAuditReopenRecord(f.root, f.id)
	if err != nil || !ok {
		t.Fatalf("read upgraded authority: ok=%v err=%v", ok, err)
	}
	subjects := commitsForTask(f.repo, "HEAD", f.id)
	if len(subjects) != 1 {
		t.Fatalf("reachable subject bindings = %v", subjects)
	}
	wantSubject, err := semanticCommit(f.repo, subjects[0], f.id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Generation != f.record.Generation || got.Subject != wantSubject ||
		!slices.Equal(got.History, f.record.History) {
		t.Fatalf("upgraded authority = %#v, want generation %q subject %#v history %#v", got, f.record.Generation, wantSubject, f.record.History)
	}
}

func TestUpgradeBlockedAuditReopenAcceptsRootSubjectReplay(t *testing.T) {
	repo, git := gitRepo(t)
	id := "blocked-root-upgrade"
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("A\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "a.txt")
	git("commit", "-q", "-m", "A implementation\n\nCoop-Task: "+id)
	if err := os.WriteFile(filepath.Join(repo, "b.txt"), []byte("B\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "b.txt")
	git("commit", "-q", "-m", "B implementation\n\nCoop-Task: blocked-root-descendant")
	descendant := gitOut(repo, "rev-parse", "HEAD")
	record, err := captureAuditReopen(repo, id)
	if err != nil {
		t.Fatal(err)
	}

	git("checkout", "-q", "--orphan", "rewritten-root")
	git("rm", "-q", "-rf", ".")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("A repaired\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "a.txt")
	git("commit", "-q", "-m", "A repaired\n\nCoop-Task: "+id)
	git("cherry-pick", descendant)
	head := gitOut(repo, "rev-parse", "HEAD")

	replacement, err := upgradeBlockedAuditReopen(repo, head, id, record)
	if err != nil {
		t.Fatal(err)
	}
	if !auditReopenCurrentValid(repo, head, id, replacement) {
		t.Fatal("root subject replay produced invalid upgraded authority")
	}
}

func TestRawAuditRewriteHistoryUsesSubjectBoundary(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "reviewed parent")
	reviewedParent := gitOut(repo, "rev-parse", "HEAD")
	git("commit", "-q", "--allow-empty", "-m", "rewritten subject")
	subject := gitOut(repo, "rev-parse", "HEAD")
	git("commit", "-q", "--allow-empty", "-m", "replayed descendant")
	head := gitOut(repo, "rev-parse", "HEAD")

	history, err := rawAuditRewriteHistory(repo, head, reviewedParent, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[0].sha != subject || history[1].sha != head {
		t.Fatalf("rewrite history = %#v, want subject and descendant within exact limit", history)
	}
}

func TestUpgradeBlockedAuditReopenAcceptsMergeSubjectParent(t *testing.T) {
	repo, git := gitRepo(t)
	id := "blocked-merge-parent-upgrade"
	git("commit", "-q", "--allow-empty", "-m", "root")
	root := gitOut(repo, "rev-parse", "HEAD")
	branch := gitOut(repo, "branch", "--show-current")
	git("checkout", "-q", "-b", "parent-side", root)
	git("commit", "-q", "--allow-empty", "-m", "side")
	git("checkout", "-q", branch)
	git("commit", "-q", "--allow-empty", "-m", "main")
	git("merge", "-q", "--no-ff", "parent-side", "-m", "merge parent")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("A\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "a.txt")
	git("commit", "-q", "-m", "A implementation\n\nCoop-Task: "+id)
	subject := gitOut(repo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "b.txt"), []byte("B\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "b.txt")
	git("commit", "-q", "-m", "descendant")
	descendant := gitOut(repo, "rev-parse", "HEAD")
	record, err := captureAuditReopen(repo, id)
	if err != nil {
		t.Fatal(err)
	}

	git("reset", "--hard", "-q", subject+"^")
	git("cherry-pick", subject)
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("A repaired\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "a.txt")
	git("commit", "--amend", "-q", "-m", "A repaired\n\nCoop-Task: "+id)
	git("cherry-pick", descendant)
	head := gitOut(repo, "rev-parse", "HEAD")

	replacement, err := upgradeBlockedAuditReopen(repo, head, id, record)
	if err != nil {
		t.Fatal(err)
	}
	if !auditReopenCurrentValid(repo, head, id, replacement) {
		t.Fatal("merge-parent replay produced invalid upgraded authority")
	}
}

func TestBlockedAuditUnblockFailsClosed(t *testing.T) {
	assertBlocked := func(t *testing.T, f blockedAuditUpgradeFixture) {
		t.Helper()
		current, ok := currentTask(f.root, f.id)
		if !ok || current.State != stateBlocked {
			t.Fatalf("rejected unblock task = %#v, ok=%v", current, ok)
		}
	}

	t.Run("tampered subject", func(t *testing.T) {
		f := newBlockedAuditUpgradeFixture(t)
		tampered := f.record
		tampered.Subject.ChangeTree = strings.Repeat("0", 64)
		if err := writeAuditReopenRecord(f.root, tampered); err != nil {
			t.Fatal(err)
		}
		code, err := tasksFolderUnblock(f.root, []string{f.id, "must not be recorded"})
		if code != -1 || err == nil {
			t.Fatalf("tampered subject unblock = code %d, err %v", code, err)
		}
		for _, want := range []string{
			"unblock " + f.id + " failed during audit authority validation",
			"task remains blocked", "restore the unchanged host audit record and reviewed Git baseline",
			`coop tasks unblock ` + f.id + ` "<answer>"`,
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("unblock error missing %q: %v", want, err)
			}
		}
		assertBlocked(t, f)
		got, ok, err := readAuditReopenRecord(f.root, f.id)
		if err != nil || !ok || !sameAuditReopenRecord(got, tampered) {
			t.Fatalf("rejected subject changed authority: got=%#v ok=%v err=%v", got, ok, err)
		}
		if decisionResolved(filepath.Join(f.task.Dir, "decision.md")) {
			t.Fatal("rejected subject recorded the inline answer")
		}
	})

	t.Run("missing generation", func(t *testing.T) {
		f := newBlockedAuditUpgradeFixture(t)
		invalid := f.record
		invalid.Generation = ""
		body, err := json.Marshal(invalid)
		if err != nil {
			t.Fatal(err)
		}
		name, err := auditReopenRecordName(f.root, f.id)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(f.authorityRoot, name)
		body = append(body, '\n')
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := resolveAndUnblock(f.root, f.task, "must not be recorded"); err == nil {
			t.Fatal("missing generation was accepted")
		}
		assertBlocked(t, f)
		if got, err := os.ReadFile(path); err != nil || !slices.Equal(got, body) {
			t.Fatalf("rejected invalid generation changed authority: %q err=%v", got, err)
		}
	})

	t.Run("replaced generation", func(t *testing.T) {
		f := newBlockedAuditUpgradeFixture(t)
		replacement := f.record
		replacement.Generation = "replacement-generation"
		if err := writeAuditReopenRecord(f.root, replacement); err != nil {
			t.Fatal(err)
		}
		upgrade, err := lockBlockedAuditReopenUnblock(f.root, f.task, f.record)
		if upgrade != nil {
			_ = upgrade.finish(nil)
		}
		if err == nil {
			t.Fatal("replaced generation was accepted")
		}
		assertBlocked(t, f)
		got, ok, readErr := readAuditReopenRecord(f.root, f.id)
		if readErr != nil || !ok || !sameAuditReopenRecord(got, replacement) {
			t.Fatalf("rejected replacement changed authority: got=%#v ok=%v err=%v", got, ok, readErr)
		}
	})

	t.Run("changed descendant", func(t *testing.T) {
		f := newBlockedAuditUpgradeFixture(t)
		if err := os.WriteFile(filepath.Join(f.repo, "tamper.txt"), []byte("tampered\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		f.git("add", "tamper.txt")
		f.git("commit", "--amend", "-q", "--no-edit")
		if err := resolveAndUnblock(f.root, f.task, "must not be recorded"); err == nil {
			t.Fatal("changed descendant was accepted")
		}
		assertBlocked(t, f)
		got, ok, err := readAuditReopenRecord(f.root, f.id)
		if err != nil || !ok || !sameAuditReopenRecord(got, f.record) {
			t.Fatalf("rejected descendant changed authority: got=%#v ok=%v err=%v", got, ok, err)
		}
	})

	t.Run("dropped subject parent", func(t *testing.T) {
		f := newBlockedAuditUpgradeFixture(t)
		f.git("reset", "--hard", "-q", f.subjectParent+"^")
		if err := os.WriteFile(filepath.Join(f.repo, "a.txt"), []byte("A repaired\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		f.git("add", "a.txt")
		f.git("commit", "-q", "-m", "A repaired\n\nCoop-Task: "+f.id)
		f.git("cherry-pick", f.descendant)
		if err := resolveAndUnblock(f.root, f.task, "must not be recorded"); err == nil {
			t.Fatal("dropped subject parent was accepted")
		}
		assertBlocked(t, f)
		got, ok, err := readAuditReopenRecord(f.root, f.id)
		if err != nil || !ok || !sameAuditReopenRecord(got, f.record) {
			t.Fatalf("rejected parent rewrite changed authority: got=%#v ok=%v err=%v", got, ok, err)
		}
	})

	t.Run("graft cannot redirect rewritten subject", func(t *testing.T) {
		f := newBlockedAuditUpgradeFixture(t)
		f.git("reset", "--hard", "-q", f.subjectParent+"^")
		if err := os.WriteFile(filepath.Join(f.repo, "a.txt"), []byte("A repaired\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		f.git("add", "a.txt")
		f.git("commit", "-q", "-m", "A repaired\n\nCoop-Task: "+f.id)
		actualSubject := gitOut(f.repo, "rev-parse", "HEAD")
		f.git("cherry-pick", f.descendant)
		head := gitOut(f.repo, "rev-parse", "HEAD")

		tree := gitOut(f.repo, "rev-parse", actualSubject+"^{tree}")
		decoySubject := gitOut(
			f.repo, "commit-tree", tree, "-p", f.subjectParent,
			"-m", "A repaired\n\nCoop-Task: "+f.id,
		)
		if err := os.WriteFile(
			filepath.Join(f.repo, ".git", "info", "grafts"),
			[]byte(head+" "+decoySubject+"\n"),
			0o600,
		); err != nil {
			t.Fatal(err)
		}
		if err := resolveAndUnblock(f.root, f.task, "must not be recorded"); err == nil {
			t.Fatal("graft-selected blocked rewrite subject was accepted")
		}
		assertBlocked(t, f)
		got, ok, err := readAuditReopenRecord(f.root, f.id)
		if err != nil || !ok || !sameAuditReopenRecord(got, f.record) {
			t.Fatalf("rejected graft rewrite changed authority: got=%#v ok=%v err=%v", got, ok, err)
		}
	})

	t.Run("raw move does not rebase", func(t *testing.T) {
		f := newBlockedAuditUpgradeFixture(t)
		if err := moveTaskDir(f.root, f.task, stateTodo); err != nil {
			t.Fatal(err)
		}
		got, ok, err := readAuditReopenRecord(f.root, f.id)
		if err != nil || !ok || !sameAuditReopenRecord(got, f.record) {
			t.Fatalf("raw move changed authority: got=%#v ok=%v err=%v", got, ok, err)
		}
		head := gitOut(f.repo, "rev-parse", "HEAD")
		if auditReopenCompletionValid(f.repo, head, head, f.id, got) {
			t.Fatal("raw move silently upgraded stale authority")
		}

		base := head
		subjects := commitsForTask(f.repo, head, f.id)
		descendants := commitsForTask(f.repo, head, "blocked-upgrade-descendant")
		if len(subjects) != 1 || len(descendants) != 1 {
			t.Fatalf("fixture bindings = subject %v descendant %v", subjects, descendants)
		}
		f.git("reset", "--hard", "-q", subjects[0]+"^")
		f.git("cherry-pick", subjects[0])
		if err := os.WriteFile(filepath.Join(f.repo, "a.txt"), []byte("A repaired again\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		f.git("add", "a.txt")
		f.git("commit", "--amend", "-q", "--no-edit")
		f.git("cherry-pick", descendants[0])
		secondHead := gitOut(f.repo, "rev-parse", "HEAD")
		if auditReopenCompletionValid(f.repo, base, secondHead, f.id, got) {
			t.Fatal("raw-moved stale authority authorized a second rewrite")
		}
	})

	t.Run("decision write failure leaves authority stale", func(t *testing.T) {
		f := newBlockedAuditUpgradeFixture(t)
		decision := filepath.Join(f.task.Dir, "decision.md")
		if err := os.Remove(decision); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(decision, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := resolveAndUnblock(f.root, f.task, "must not be recorded"); err == nil {
			t.Fatal("decision write obstruction was accepted")
		}
		if !pathExists(f.task.Dir) {
			t.Fatal("decision write obstruction moved the blocked task")
		}
		got, ok, err := readAuditReopenRecord(f.root, f.id)
		if err != nil || !ok || !sameAuditReopenRecord(got, f.record) {
			t.Fatalf("decision write obstruction changed authority: got=%#v ok=%v err=%v", got, ok, err)
		}
	})

	t.Run("move failure leaves authority stale", func(t *testing.T) {
		f := newBlockedAuditUpgradeFixture(t)
		upgrade, err := prepareBlockedAuditReopenUnblock(f.root, f.task)
		if err != nil {
			t.Fatal(err)
		}
		obstruction := filepath.Join(f.root, stateTodo, f.id)
		if err := os.MkdirAll(obstruction, 0o755); err != nil {
			_ = upgrade.finish(nil)
			t.Fatal(err)
		}
		if err := moveBlockedAuditUnblock(f.root, f.task, upgrade); err == nil {
			t.Fatal("destination obstruction was accepted")
		}
		if !pathExists(f.task.Dir) {
			t.Fatal("destination obstruction moved the blocked task")
		}
		got, ok, err := readAuditReopenRecord(f.root, f.id)
		if err != nil || !ok || !sameAuditReopenRecord(got, f.record) {
			t.Fatalf("destination obstruction changed stale authority: got=%#v ok=%v err=%v", got, ok, err)
		}
	})

	t.Run("authority write failure restores blocked folder", func(t *testing.T) {
		f := newBlockedAuditUpgradeFixture(t)
		upgrade, err := prepareBlockedAuditReopenUnblock(f.root, f.task)
		if err != nil {
			t.Fatal(err)
		}
		upgrade.replacement.Generation = "invalid-replacement-generation"
		if err := moveBlockedAuditUnblock(f.root, f.task, upgrade); err == nil {
			t.Fatal("authority persistence failure was accepted")
		}
		if !pathExists(f.task.Dir) || pathExists(filepath.Join(f.root, stateTodo, f.id)) {
			t.Fatal("authority persistence failure did not restore the blocked folder")
		}
		got, ok, err := readAuditReopenRecord(f.root, f.id)
		if err != nil || !ok || !sameAuditReopenRecord(got, f.record) {
			t.Fatalf("authority persistence failure changed stale authority: got=%#v ok=%v err=%v", got, ok, err)
		}
	})

	t.Run("rollback failure reports unknown state", func(t *testing.T) {
		f := newBlockedAuditUpgradeFixture(t)
		upgrade, err := prepareBlockedAuditReopenUnblock(f.root, f.task)
		if err != nil {
			t.Fatal(err)
		}
		upgrade.replacement.Generation = "invalid-replacement-generation"
		if err := upgrade.markPending(); err != nil {
			_ = upgrade.finish(nil)
			t.Fatal(err)
		}
		if err := moveTaskDir(f.root, f.task, stateTodo); err != nil {
			_ = upgrade.finish(nil)
			t.Fatal(err)
		}
		if err := os.MkdirAll(f.task.Dir, 0o755); err != nil {
			_ = upgrade.finish(nil)
			t.Fatal(err)
		}
		persistErr := upgrade.persist()
		moved := f.task
		moved.State = stateTodo
		moved.Dir = filepath.Join(f.root, stateTodo, f.id)
		rollbackErr := moveTaskDir(f.root, moved, stateBlocked)
		if persistErr == nil || rollbackErr == nil {
			_ = upgrade.finish(nil)
			t.Fatalf("failure seam = persist %v rollback %v", persistErr, rollbackErr)
		}
		stageErr := &unblockStageError{
			stage: "audit authority persistence", state: "",
			err: upgrade.finish(errors.Join(persistErr, rollbackErr)),
		}
		reported := unblockRetryError(f.id, true, stageErr).Error()
		for _, want := range []string{"could not restore a known task state", "coop tasks path " + f.id} {
			if !strings.Contains(reported, want) {
				t.Errorf("unknown-state error missing %q: %s", want, reported)
			}
		}
		if strings.Contains(reported, "task remains blocked") {
			t.Errorf("unknown-state error claimed blocked: %s", reported)
		}
		pending, ok, readErr := readAuditReopenRecord(f.root, f.id)
		if readErr != nil || !ok || !pending.UnblockPending ||
			!pathExists(filepath.Join(f.root, stateTodo, f.id)) {
			t.Fatalf("rollback failure state = pending %#v ok=%v todo=%v err=%v",
				pending, ok, pathExists(filepath.Join(f.root, stateTodo, f.id)), readErr)
		}
	})

	t.Run("crash boundary after move cannot grant authority", func(t *testing.T) {
		f := newBlockedAuditUpgradeFixture(t)
		upgrade, err := prepareBlockedAuditReopenUnblock(f.root, f.task)
		if err != nil {
			t.Fatal(err)
		}
		if err := upgrade.markPending(); err != nil {
			_ = upgrade.finish(nil)
			t.Fatal(err)
		}
		if err := recordResolution(filepath.Join(f.task.Dir, "decision.md"), "external acceptance passed"); err != nil {
			_ = upgrade.finish(nil)
			t.Fatal(err)
		}
		if err := moveTaskDir(f.root, f.task, stateTodo); err != nil {
			_ = upgrade.finish(nil)
			t.Fatal(err)
		}
		if err := upgrade.finish(nil); err != nil {
			t.Fatal(err)
		}
		got, ok, err := readAuditReopenRecord(f.root, f.id)
		if err != nil || !ok || !got.UnblockPending || got.Generation != f.record.Generation {
			t.Fatalf("move-before-persist boundary lost pending authority: got=%#v ok=%v err=%v", got, ok, err)
		}
		head := gitOut(f.repo, "rev-parse", "HEAD")
		if auditReopenCompletionValid(f.repo, head, head, f.id, got) {
			t.Fatal("move-before-persist boundary granted verification-only authority")
		}
		if got.Version != auditReopenPendingVersion || got.Version == auditReopenVersion {
			t.Fatalf("pending record version = %d, want downgrade-safe %d", got.Version, auditReopenPendingVersion)
		}
		downgraded := got
		downgraded.Version = auditReopenVersion
		if validateAuditReopenRecord(downgraded, f.id) == nil {
			t.Fatal("v1 reader-compatible pending record was accepted")
		}
		if code, err := tasksFolderUnblock(f.root, []string{f.id}); code != 0 || err != nil {
			t.Fatalf("explicit recovery of post-move pending unblock = code %d err %v", code, err)
		}
		recovered, ok, err := readAuditReopenRecord(f.root, f.id)
		if err != nil || !ok || recovered.UnblockPending ||
			!auditReopenCompletionValid(f.repo, head, head, f.id, recovered) {
			t.Fatalf("lease recovery did not activate valid authority: got=%#v ok=%v err=%v", recovered, ok, err)
		}
	})

	t.Run("pending marker plus raw move cannot activate on lease", func(t *testing.T) {
		f := newBlockedAuditUpgradeFixture(t)
		upgrade, err := prepareBlockedAuditReopenUnblock(f.root, f.task)
		if err != nil {
			t.Fatal(err)
		}
		if err := upgrade.markPending(); err != nil {
			_ = upgrade.finish(nil)
			t.Fatal(err)
		}
		if err := upgrade.finish(nil); err != nil {
			t.Fatal(err)
		}
		if err := moveTaskDir(f.root, f.task, stateTodo); err != nil {
			t.Fatal(err)
		}
		todo, ok := currentTask(f.root, f.id)
		if !ok {
			t.Fatal("raw-moved pending task disappeared")
		}
		if err := moveTaskDir(f.root, todo, stateInProgress); err != nil {
			t.Fatal(err)
		}
		inProgress, ok := currentTask(f.root, f.id)
		if !ok {
			t.Fatal("claimed pending task disappeared")
		}
		lease, _, err := tryTaskLease(f.root, inProgress, testLeaseOwner())
		if lease != nil || err == nil || !strings.Contains(err.Error(), "non-authorizing pending audit unblock") {
			t.Fatalf("pending raw-move lease = lease %#v err %v", lease, err)
		}
		pending, ok, readErr := readAuditReopenRecord(f.root, f.id)
		if readErr != nil || !ok || !pending.UnblockPending || pending.Version != auditReopenPendingVersion {
			t.Fatalf("denied lease changed pending authority: got=%#v ok=%v err=%v", pending, ok, readErr)
		}
	})

	t.Run("todo pending inspection error is actionable", func(t *testing.T) {
		f := newBlockedAuditUpgradeFixture(t)
		upgrade, err := prepareBlockedAuditReopenUnblock(f.root, f.task)
		if err != nil {
			t.Fatal(err)
		}
		if err := upgrade.markPending(); err != nil {
			_ = upgrade.finish(nil)
			t.Fatal(err)
		}
		if err := moveTaskDir(f.root, f.task, stateTodo); err != nil {
			_ = upgrade.finish(nil)
			t.Fatal(err)
		}
		if err := upgrade.finish(nil); err != nil {
			t.Fatal(err)
		}
		name, err := auditReopenRecordName(f.root, f.id)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.authorityRoot, name), []byte("{malformed\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		code, err := tasksFolderUnblock(f.root, []string{f.id})
		if code != -1 || err == nil {
			t.Fatalf("todo authority inspection = code %d err %v", code, err)
		}
		for _, want := range []string{"inspect interrupted audit unblock", "task remains todo", "coop tasks unblock " + f.id} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("todo authority inspection error missing %q: %v", want, err)
			}
		}
		if !pathExists(filepath.Join(f.root, stateTodo, f.id)) {
			t.Fatal("todo authority inspection error moved the task")
		}
	})

	t.Run("crash boundary before move is explicitly retryable", func(t *testing.T) {
		f := newBlockedAuditUpgradeFixture(t)
		upgrade, err := prepareBlockedAuditReopenUnblock(f.root, f.task)
		if err != nil {
			t.Fatal(err)
		}
		if err := upgrade.markPending(); err != nil {
			_ = upgrade.finish(nil)
			t.Fatal(err)
		}
		if err := upgrade.finish(nil); err != nil {
			t.Fatal(err)
		}
		pending, ok, err := readAuditReopenRecord(f.root, f.id)
		if err != nil || !ok || !pending.UnblockPending || !pathExists(f.task.Dir) {
			t.Fatalf("pre-move boundary = record %#v ok=%v blocked=%v err=%v", pending, ok, pathExists(f.task.Dir), err)
		}

		if err := resolveAndUnblock(f.root, f.task, "external acceptance passed"); err != nil {
			t.Fatalf("explicit retry of pending unblock: %v", err)
		}
		recovered, ok, err := readAuditReopenRecord(f.root, f.id)
		head := gitOut(f.repo, "rev-parse", "HEAD")
		if err != nil || !ok || recovered.UnblockPending ||
			!auditReopenCompletionValid(f.repo, head, head, f.id, recovered) ||
			!pathExists(filepath.Join(f.root, stateTodo, f.id)) {
			t.Fatalf("explicit pending retry = record %#v ok=%v todo=%v err=%v",
				recovered, ok, pathExists(filepath.Join(f.root, stateTodo, f.id)), err)
		}
	})

	t.Run("recorded baseline is the only recovery candidate", func(t *testing.T) {
		f := newBlockedAuditUpgradeFixture(t)
		tampered := f.record
		tampered.BaselineHead = gitOut(f.repo, "rev-parse", "HEAD")
		if err := writeAuditReopenRecord(f.root, tampered); err != nil {
			t.Fatal(err)
		}
		if err := resolveAndUnblock(f.root, f.task, "must not be recorded"); err == nil ||
			!strings.Contains(err.Error(), "recorded baseline") {
			t.Fatalf("wrong exact-baseline error = %v", err)
		}
		if !pathExists(f.task.Dir) {
			t.Fatal("wrong exact baseline moved the blocked task")
		}
		got, ok, err := readAuditReopenRecord(f.root, f.id)
		if err != nil || !ok || !sameAuditReopenRecord(got, tampered) {
			t.Fatalf("wrong exact baseline changed authority: got=%#v ok=%v err=%v", got, ok, err)
		}
	})
}

func TestRestoreUnbindableCompletions(t *testing.T) {
	root := t.TempDir()
	id := "2026-01-01-unbound"
	doneDir := filepath.Join(root, stateDone, id)
	writeTaskFile(t, filepath.Join(doneDir, "task.md"), "# Unbound\n")
	writeTaskFile(t, filepath.Join(doneDir, "log.md"), "# Log\n")

	item := readTaskTree(root)[0]
	if err := restoreQueuedCompletion(queuedTask{Root: root, Item: item}, false); err != nil {
		t.Fatalf("restoreQueuedCompletion: %v", err)
	}
	inProgressDir := filepath.Join(root, stateInProgress, id)
	if !pathExists(inProgressDir) || pathExists(doneDir) {
		t.Fatalf("rejected completion was not restored: in_progress=%v done=%v", pathExists(inProgressDir), pathExists(doneDir))
	}
	log, err := os.ReadFile(filepath.Join(inProgressDir, "log.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"completion rejected", "expected exactly one commit", "git commit --amend --only --no-edit --trailer", "already reachable but is NOT HEAD", "do not rewrite it", "50_blocked/", "rewrite or squash", id} {
		if !strings.Contains(string(log), want) {
			t.Errorf("rejection log missing %q:\n%s", want, log)
		}
	}
	if strings.Contains(string(log), "git commit --amend --no-edit --trailer") {
		t.Errorf("rejection log retained the index-unsafe amend command:\n%s", log)
	}
	state, err := os.ReadFile(filepath.Join(inProgressDir, "state.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"**Status:** in progress", "completion rejected", "**Next action:** repair the commit binding"} {
		if !strings.Contains(string(state), want) {
			t.Errorf("rejection state missing %q:\n%s", want, state)
		}
	}

	rejectErr := unbindableCompletionError([]string{id}, nil)
	if rejectErr == nil {
		t.Fatal("unbindable completion must stop the controller")
	}
	for _, want := range []string{"completion rejected", "restored to in_progress", "git commit --amend --only --no-edit --trailer", "already reachable but is NOT HEAD", "do not rewrite it", "50_blocked/", "rewrite/squash", id} {
		if !strings.Contains(rejectErr.Error(), want) {
			t.Errorf("controller error missing %q: %v", want, rejectErr)
		}
	}
	if strings.Contains(rejectErr.Error(), "git commit --amend --no-edit --trailer") {
		t.Errorf("controller error retained the index-unsafe amend command: %v", rejectErr)
	}
}

func TestRestoreAuditRejectedCompletion(t *testing.T) {
	root := t.TempDir()
	id := "2026-01-01-audit-reopened"
	doneDir := filepath.Join(root, stateDone, id)
	writeTaskFile(t, filepath.Join(doneDir, "task.md"), "# Audit\n")
	writeTaskFile(t, filepath.Join(doneDir, "log.md"), "# Log\n")

	item := readTaskTree(root)[0]
	if err := restoreQueuedCompletion(queuedTask{Root: root, Item: item}, true); err != nil {
		t.Fatalf("restoreQueuedCompletion: %v", err)
	}
	inProgressDir := filepath.Join(root, stateInProgress, id)
	if !pathExists(inProgressDir) || pathExists(doneDir) {
		t.Fatalf("rejected audit completion was not restored: in_progress=%v done=%v", pathExists(inProgressDir), pathExists(doneDir))
	}
	log := readFileString(filepath.Join(inProgressDir, "log.md"))
	for _, want := range []string{"completion rejected", "host-authorized review rework", "zero new commits", "tree actually changes", "semantically", "rejected again", id} {
		if !strings.Contains(log, want) {
			t.Errorf("audit rejection log missing %q:\n%s", want, log)
		}
	}
	// The audit remedy forbids the recovery receipt but must never prescribe its recipe.
	for _, banned := range []string{"Coop-Recovery: <current UTC timestamp>", "git commit --amend"} {
		if strings.Contains(log, banned) {
			t.Errorf("audit rejection log prescribes the recovery recipe %q:\n%s", banned, log)
		}
	}
	state := readFileString(filepath.Join(inProgressDir, "state.md"))
	for _, want := range []string{"**Status:** in progress", "rejected by the host audit authority", "**Next action:** independently verify the audit finding, then re-close with zero commits or a real tree change"} {
		if !strings.Contains(state, want) {
			t.Errorf("audit rejection state missing %q:\n%s", want, state)
		}
	}

	rejectErr := auditCompletionError(id, nil)
	if rejectErr == nil {
		t.Fatal("audit-invalid completion must stop the controller")
	}
	for _, want := range []string{"completion rejected", "restored to in_progress", "host-authorized review rework", "zero-commit verification-only re-close", "semantically unchanged descendants", "then re-run `coop loop`", id} {
		if !strings.Contains(rejectErr.Error(), want) {
			t.Errorf("audit controller error missing %q: %v", want, rejectErr)
		}
	}
	if strings.Contains(rejectErr.Error(), "Coop-Recovery: <current UTC timestamp>") {
		t.Errorf("audit controller error prescribes the recovery trailer recipe: %v", rejectErr)
	}
}

func TestParkStaleAuditReopenPreservesPriorDecision(t *testing.T) {
	root := t.TempDir()
	id := "2026-01-01-stale-audit"
	dir := filepath.Join(root, stateInProgress, id)
	writeTaskFile(t, filepath.Join(dir, "task.md"), "# Stale audit\n")
	writeTaskFile(t, filepath.Join(dir, "log.md"), "# Log\n")
	writeTaskFile(t, filepath.Join(dir, "state.md"), "# State\n")
	writeTaskFile(t, filepath.Join(dir, "decision.md"), "# Prior decision\n\n**Resolution:** accepted earlier\n")

	item := readTaskTree(root)[0]
	baseline := strings.Repeat("b", 40)
	if err := parkStaleAuditReopen(queuedTask{Root: root, Item: item}, baseline); err != nil {
		t.Fatalf("parkStaleAuditReopen: %v", err)
	}
	blockedDir := filepath.Join(root, stateBlocked, id)
	if !pathExists(blockedDir) || pathExists(dir) {
		t.Fatalf("stale audit task was not parked: blocked=%v in_progress=%v", pathExists(blockedDir), pathExists(dir))
	}
	decision := readFileString(filepath.Join(blockedDir, "decision.md"))
	for _, want := range []string{
		"restore the host-audited Git baseline",
		"restore the exact pre-attempt baseline",
		baseline,
		"git rev-parse HEAD",
		"Blocking and unblocking alone cannot repair Git history",
		"> **Resolution:** accepted earlier",
	} {
		if !strings.Contains(decision, want) {
			t.Errorf("stale audit decision missing %q:\n%s", want, decision)
		}
	}
	if decisionResolved(filepath.Join(blockedDir, "decision.md")) {
		t.Errorf("stale audit decision retained a live prior resolution:\n%s", decision)
	}
	state := readFileString(filepath.Join(blockedDir, "state.md"))
	for _, want := range []string{"**Status:** blocked", "stopped before provider launch", baseline, "git rev-parse HEAD", "never add a Coop-Recovery receipt"} {
		if !strings.Contains(state, want) {
			t.Errorf("stale audit state missing %q:\n%s", want, state)
		}
	}
}

func TestRestoreCompromisedCompletionTrapFollowsLeaseAuthority(t *testing.T) {
	cases := []struct {
		name, want, banned string
		audit              bool
	}{
		{name: "ordinary", want: "the next completion needs a unique Coop-Recovery trailer"},
		{name: "audit", audit: true, want: "never a Coop-Recovery receipt", banned: "unique Coop-Recovery trailer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			id := "2026-01-01-compromised"
			doneDir := filepath.Join(root, stateDone, id)
			writeTaskFile(t, filepath.Join(doneDir, "task.md"), "# Compromised\n")
			writeTaskFile(t, filepath.Join(doneDir, "log.md"), "# Log\n")
			item := readTaskTree(root)[0]
			if err := restoreCompromisedCompletion(queuedTask{Root: root, Item: item}, tc.audit); err != nil {
				t.Fatalf("restoreCompromisedCompletion: %v", err)
			}
			state := readFileString(filepath.Join(root, stateInProgress, id, "state.md"))
			if !strings.Contains(state, tc.want) {
				t.Errorf("compromised state missing %q:\n%s", tc.want, state)
			}
			if tc.banned != "" && strings.Contains(state, tc.banned) {
				t.Errorf("audit compromised state prescribes %q:\n%s", tc.banned, state)
			}
		})
	}
}

func TestAppendTaskLogStrictRejectsSymlinkedLog(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside-log")
	want := "outside log sentinel\n"
	if err := os.WriteFile(outside, []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}
	taskDir := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(taskDir, "log.md")); err != nil {
		t.Fatal(err)
	}
	if err := appendTaskLogStrict(taskDir, "must stay contained"); err == nil || !strings.Contains(err.Error(), "single-link regular file") {
		t.Fatalf("symlinked log error = %v", err)
	}
	data, err := os.ReadFile(outside)
	if err != nil || string(data) != want {
		t.Fatalf("outside log changed to %q, %v", data, err)
	}
}

func TestIsGateGuardPath(t *testing.T) {
	guarded := []string{"Makefile", "sub/Makefile", ".agent/project.yaml", ".agent/loop.yaml",
		".agent/skills/sweep/SKILL.md", ".agent/skills/sweep/queue-guard.sh",
		".claude/skills/workflow-sweep/queue-guard.sh",
		".claude/settings.json", ".claude/hooks/commit-gate.sh", ".github/workflows/ci.yml"}
	for _, f := range guarded {
		if !isGateGuardPath(f) {
			t.Errorf("%q should be gate-defining", f)
		}
	}
	// Ordinary source and test files are NOT gate-defining — only the checker's own definition is.
	for _, f := range []string{"internal/cli/sign.go", "internal/cli/sign_test.go", "README.md", "docs/cli.md",
		".claude/skills/workflow-sweep/helper.sh", ".claude/skills/workflow-sweep/queue-guard.sh.bak"} {
		if isGateGuardPath(f) {
			t.Errorf("%q should NOT be gate-defining (only the gate's own definition is)", f)
		}
	}
}

func TestProtectedGateChanges(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	env := append(os.Environ(), "GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "g"), "GIT_CONFIG_SYSTEM="+filepath.Join(t.TempDir(), "s"))
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = repo, env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(p, s string) {
		full := filepath.Join(repo, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	git("config", "user.email", "t@t")
	git("config", "user.name", "T")
	write("code.go", "package x")
	write(".claude/skills/workflow-sweep/queue-guard.sh", "#!/bin/sh\n")
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	base := gitOut(repo, "rev-parse", "HEAD")
	// A commit that touches ordinary code → no protected change.
	write("code.go", "package x // edit")
	git("add", "-A")
	git("commit", "-q", "-m", "code edit")
	if hits := protectedGateChanges(repo, base, gitOut(repo, "rev-parse", "HEAD")); len(hits) != 0 {
		t.Errorf("an ordinary code change is not protected: %v", hits)
	}
	// A commit that weakens the Makefile → flagged.
	mid := gitOut(repo, "rev-parse", "HEAD")
	write("Makefile", "check:\n\ttrue\n")
	git("add", "-A")
	git("commit", "-q", "-m", "loosen the gate")
	if hits := protectedGateChanges(repo, mid, gitOut(repo, "rev-parse", "HEAD")); len(hits) != 1 || hits[0] != "Makefile" {
		t.Errorf("a Makefile change should be flagged, got %v", hits)
	}
	// Renaming a guard away must report the deleted protected path, not only its new name.
	renameBase := gitOut(repo, "rev-parse", "HEAD")
	git("mv", ".claude/skills/workflow-sweep/queue-guard.sh", ".claude/skills/workflow-sweep/disabled.sh")
	git("commit", "-q", "-m", "disable the adopted guard")
	if hits := protectedGateChanges(repo, renameBase, gitOut(repo, "rev-parse", "HEAD")); len(hits) != 1 || hits[0] != ".claude/skills/workflow-sweep/queue-guard.sh" {
		t.Errorf("renaming an adopted guard should flag its old path, got %v", hits)
	}
	// NUL-delimited names prevent Git from quoting paths before basename matching.
	unicodeGuard := "\u00e9/queue-guard.sh"
	unicodeBase := gitOut(repo, "rev-parse", "HEAD")
	write(unicodeGuard, "#!/bin/sh\n")
	git("add", "-A")
	git("commit", "-q", "-m", "add guard below unicode directory")
	if hits := protectedGateChanges(repo, unicodeBase, gitOut(repo, "rev-parse", "HEAD")); len(hits) != 1 || hits[0] != unicodeGuard {
		t.Errorf("a protected basename below a unicode directory should be flagged, got %v", hits)
	}
}

func TestProtectedGateFiles(t *testing.T) {
	got := protectedGateFiles([]string{
		"internal/cli/commands.go", ".claude/settings.json", "Makefile", "Makefile", " .agent/skills/sweep/SKILL.md ",
	})
	want := []string{".agent/skills/sweep/SKILL.md", ".claude/settings.json", "Makefile"}
	if !slices.Equal(got, want) {
		t.Errorf("protectedGateFiles = %v, want %v", got, want)
	}
}

// TestReconcileQueueAfterMerge: a queued task whose Coop-Task trailer just landed moves to done;
// a blocked task with a landed trailer is NOT moved (flagged for a human); an unlanded task stays.
func TestReconcileQueueAfterMerge(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	env := append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "g"),
		"GIT_CONFIG_SYSTEM="+filepath.Join(t.TempDir(), "s"))
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = repo, env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	q := filepath.Join(repo, tasksRoot)
	q2Rel := filepath.Join(".agent", "other-tasks")
	q2 := filepath.Join(repo, q2Rel)
	writeTaskFile(t, filepath.Join(q, stateTodo, "todo1", "task.md"), "# todo1\n")
	writeTaskFile(t, filepath.Join(q, stateTodo, "todo1", "tmp", "scratch"), "remove\n")
	writeTaskFile(t, filepath.Join(q, stateInProgress, "wip1", "task.md"), "# wip1\n")
	writeTaskFile(t, filepath.Join(q, stateInProgress, "wip1", "tmp", "scratch"), "remove\n")
	writeTaskFile(t, filepath.Join(q, stateBlocked, "blk1", "task.md"), "# blk1\n")
	writeTaskFile(t, filepath.Join(q, stateBlocked, "blk1", "decision.md"), "# blocked\n")
	writeTaskFile(t, filepath.Join(q, stateBlocked, "blk1", "tmp", "scratch"), "retain\n")
	writeTaskFile(t, filepath.Join(q, stateTodo, "safe", "task.md"), "# safe\n")
	writeTaskFile(t, filepath.Join(q, stateTodo, "same-id", "task.md"), "# same root\n")
	writeTaskFile(t, filepath.Join(q2, stateTodo, "same-id", "task.md"), "# same second queue\n")
	git("init", "-q")
	git("config", "user.email", "t@t")
	git("config", "user.name", "T")
	// A landed commit for todo1, wip1, and blk1 (as a merged fork would carry); "safe" did not land.
	if err := os.WriteFile(filepath.Join(repo, "code.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "seed queue")
	beforeLand := gitOut(repo, "rev-parse", "HEAD")
	git("commit", "-q", "--allow-empty", "-m", "todo1 work\n\nCoop-Task: todo1")
	git("commit", "-q", "--allow-empty", "-m", "wip1 work\n\nCoop-Task: wip1")
	git("commit", "-q", "--allow-empty", "-m", "blk1 work\n\nCoop-Task: blk1")
	git("commit", "-q", "--allow-empty", "-m", "ambiguous work\n\nCoop-Task: same-id")

	a := &app{cfg: &config.Config{TasksFiles: []string{tasksRoot, q2Rel}}}
	a.reconcileQueueAfterMerge(repo, "fork1", beforeLand+"..HEAD")

	if !pathExists(filepath.Join(q, stateDone, "todo1")) || pathExists(filepath.Join(q, stateTodo, "todo1")) {
		t.Error("a landed todo task should have moved to done")
	}
	if !pathExists(filepath.Join(q, stateDone, "wip1")) {
		t.Error("a landed in_progress task should have moved to done")
	}
	if !pathExists(filepath.Join(q, stateBlocked, "blk1")) || pathExists(filepath.Join(q, stateDone, "blk1")) {
		t.Error("a blocked task must be flagged, never auto-moved")
	}
	if !pathExists(filepath.Join(q, stateTodo, "safe")) {
		t.Error("an unlanded task must stay put")
	}
	if !pathExists(filepath.Join(q, stateTodo, "same-id")) || !pathExists(filepath.Join(q2, stateTodo, "same-id")) {
		t.Error("an ambiguous landed id must be skipped in every queue")
	}
	if pathExists(filepath.Join(q, stateDone, "todo1", "tmp")) || pathExists(filepath.Join(q, stateDone, "wip1", "tmp")) {
		t.Error("fork reconciliation must clean completed task tmp")
	}
	for _, id := range []string{"todo1", "wip1"} {
		doneDir := filepath.Join(q, stateDone, id)
		state := readFileString(filepath.Join(doneDir, "state.md"))
		if !strings.Contains(state, "**Status:** complete") || !strings.Contains(state, "**Next action:** none") {
			t.Errorf("fork reconciliation did not finalize %s state:\n%s", id, state)
		}
		if !taskCompletionRecorded(q, taskItem{ID: id, Dir: doneDir, State: stateDone}) {
			t.Errorf("fork reconciliation did not record completion evidence for %s", id)
		}
	}
	if !fileExists(filepath.Join(q, stateBlocked, "blk1", "tmp", "scratch")) {
		t.Error("fork reconciliation must retain blocked task tmp")
	}
	// The reconciled task got a note in its log.md.
	if data, _ := os.ReadFile(filepath.Join(q, stateDone, "todo1", "log.md")); !strings.Contains(string(data), "reconciled: landed by fork fork1") {
		t.Errorf("reconcile note missing from todo1 log.md: %q", data)
	}

	// Reusing an old task ID must not let an unrelated later fork merge complete the new task.
	git("commit", "-q", "--allow-empty", "-m", "historical work\n\nCoop-Task: reused")
	unrelatedBase := gitOut(repo, "rev-parse", "HEAD")
	writeTaskFile(t, filepath.Join(q, stateTodo, "reused", "task.md"), "# reused\n")
	git("commit", "-q", "--allow-empty", "-m", "unrelated fork work")
	a.reconcileQueueAfterMerge(repo, "unrelated", unrelatedBase+"..HEAD")
	if !pathExists(filepath.Join(q, stateTodo, "reused")) || pathExists(filepath.Join(q, stateDone, "reused")) {
		t.Error("an old historical trailer completed a reused task during an unrelated merge")
	}
}

// TestUnblockResolved: the host-side preflight returns a blocked task to todo only when its
// decision.md carries a filled-in Resolution by the SAME bar `coop tasks unblock` applies
// (decisionResolved) — the untouched stub, a missing decision.md, and a free-form file with no
// **Resolution:** marker all stay parked (parse-or-park: never act on a format we can't read).
func TestUnblockResolved(t *testing.T) {
	root := t.TempDir()
	mk := func(id, decision string) {
		dir := filepath.Join(root, stateBlocked, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "task.md"), []byte("# "+id+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if decision != "" {
			if err := os.WriteFile(filepath.Join(dir, "decision.md"), []byte(decision), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("answered", "# Decision\n\n**Resolution:** ship it as designed.\n")
	mk("stub", "# Decision\n\n**Resolution:** <!-- HUMAN: your answer here, then: coop tasks unblock stub -->\n")
	mk("no-decision", "")
	mk("freeform", "we talked and agreed to do X\n") // no **Resolution:** marker

	ids := unblockResolved([]string{root})
	if len(ids) != 1 || ids[0] != "answered" {
		t.Fatalf("unblockResolved = %v, want [answered]", ids)
	}
	// The answered task moved to todo and its log records why; the rest stayed parked.
	if !pathExists(filepath.Join(root, stateTodo, "answered")) {
		t.Error("answered task should have moved to todo")
	}
	if data, _ := os.ReadFile(filepath.Join(root, stateTodo, "answered", "log.md")); !strings.Contains(string(data), "unblocked") {
		t.Errorf("unblock note missing from log.md: %q", data)
	}
	for _, id := range []string{"stub", "no-decision", "freeform"} {
		if !pathExists(filepath.Join(root, stateBlocked, id)) {
			t.Errorf("%s should have stayed blocked", id)
		}
	}
}

func TestUnblockResolvedDoesNotUpgradeAuditAuthority(t *testing.T) {
	root := t.TempDir()
	authorityRoot := t.TempDir()
	t.Setenv(testLeaseAuthorityRootEnv, authorityRoot)
	task := taskForLease(t, root, stateBlocked, "audit-answer")
	writeTaskFile(t, filepath.Join(task.Dir, "decision.md"), "# Decision\n\n**Resolution:** provider supplied prose\n")
	record := testAuditReopenRecord(task.ID, "preflight-generation")
	if err := writeAuditReopenRecord(root, record); err != nil {
		t.Fatal(err)
	}

	if ids := unblockResolved([]string{root}); len(ids) != 0 {
		t.Fatalf("audit-authority preflight unblocked %v", ids)
	}
	if !pathExists(task.Dir) || pathExists(filepath.Join(root, stateTodo, task.ID)) {
		t.Fatal("provider-written resolution moved an audit-authority task")
	}
	got, ok, err := readAuditReopenRecord(root, task.ID)
	if err != nil || !ok || !sameAuditReopenRecord(got, record) {
		t.Fatalf("preflight changed audit authority: got=%#v ok=%v err=%v", got, ok, err)
	}

	name, err := auditReopenRecordName(root, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authorityRoot, name), []byte("{malformed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ids := unblockResolved([]string{root}); len(ids) != 0 {
		t.Fatalf("authority read error preflight unblocked %v", ids)
	}
	if !pathExists(task.Dir) || pathExists(filepath.Join(root, stateTodo, task.ID)) {
		t.Fatal("authority read error did not fail closed")
	}
}

// A task left in_progress with its commit already in history is the silent trap: the run died
// between the commit and the folder move, and nothing surfaced it. Both emisar landmines that
// preceded a 283-commit rewrite sat in exactly this state.
func TestAlreadyCommittedInProgress(t *testing.T) {
	repo := t.TempDir()
	env := append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "g"),
		"GIT_CONFIG_SYSTEM="+filepath.Join(t.TempDir(), "s"))
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = repo, env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("config", "user.email", "t@t")
	git("config", "user.name", "T")
	git("commit", "-q", "--allow-empty", "-m", "base")

	root := filepath.Join(repo, ".agent", "tasks")
	committed, fresh, finished := "2026-01-01-committed", "2026-01-01-fresh", "2026-01-01-finished"
	writeTaskFile(t, filepath.Join(root, stateInProgress, committed, "task.md"), "# Committed\n")
	writeTaskFile(t, filepath.Join(root, stateInProgress, fresh, "task.md"), "# Fresh\n")
	writeTaskFile(t, filepath.Join(root, stateDone, finished, "task.md"), "# Finished\n")
	hosts := []string{root}

	// Nothing bound yet: an ordinary start must stay silent.
	if got := alreadyCommittedInProgress(repo, hosts); len(got) != 0 {
		t.Fatalf("no bound commits yet, want no report, got %+v", got)
	}

	git("commit", "-q", "--allow-empty", "-m", "impl\n\nCoop-Task: "+committed)
	git("commit", "-q", "--allow-empty", "-m", "done work\n\nCoop-Task: "+finished)
	git("commit", "-q", "--allow-empty", "-m", "an unrelated later commit")

	got := alreadyCommittedInProgress(repo, hosts)
	if len(got) != 1 {
		t.Fatalf("want only the in_progress task with a commit, got %+v", got)
	}
	if got[0].ID != committed {
		t.Errorf("reported %q, want %q (a done task's binding is not a landmine)", got[0].ID, committed)
	}
	// Depth is what tells a human whether the resume recipe is still safe: at 0 the commit is HEAD
	// and amendable, deeper it is not.
	if got[0].Depth != 2 {
		t.Errorf("depth = %d, want 2 commits on top of the binding", got[0].Depth)
	}
	if got[0].Commit == "" {
		t.Error("report must name the commit so a human can verify it")
	}
}

// A task killed before it could commit OR checkpoint leaves its work only in the tree, with a
// state.md that still says whatever it last said. The resume preamble must point the next agent at
// that work — this is the case that cost a ~13h task a full redo.
func TestUncommittedResumeLinePointsAtTheStrandedWork(t *testing.T) {
	line := uncommittedResumeLine("2026-08-03-some-task", []string{"runner/internal/catalog/catalog.go", "packs/AGENTS.md"})
	for _, want := range []string{
		"NO commit in history",
		"runner/internal/catalog/catalog.go",
		"git diff",
		"do not trust it over the tree",
		"never `git add -A`",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("resume line missing %q:\n%s", want, line)
		}
	}
}

// A clean tree must keep the ordinary prompt byte-identical — no hint, no noise.
func TestUncommittedResumeLineIsSilentOnACleanTree(t *testing.T) {
	if got := uncommittedResumeLine("t", nil); got != "" {
		t.Errorf("clean tree produced a resume hint: %q", got)
	}
}

// The list is bounded: a wide-open tree must not paste hundreds of paths into every prompt.
func TestUncommittedResumeLineBoundsTheFileList(t *testing.T) {
	var many []string
	for i := 0; i < 40; i++ {
		many = append(many, fmt.Sprintf("file-%02d.go", i))
	}
	line := uncommittedResumeLine("t", many)
	if !strings.Contains(line, "(+28 more)") {
		t.Errorf("file list was not bounded to 12 with a remainder count:\n%s", line)
	}
	if strings.Contains(line, "file-30.go") {
		t.Error("resume line listed beyond the bound")
	}
}

// Rename entries are "old -> new"; the resume hint must name the file that exists now.
func TestInterruptedWorkFilesReportsRenameTargets(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "before.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "add before.txt")
	git(t, repo, "mv", "before.txt", "after.txt")

	files := interruptedWorkFiles(repo)
	if !slices.Contains(files, "after.txt") {
		t.Errorf("interruptedWorkFiles(renamed) = %v, want it to name after.txt", files)
	}
	if slices.Contains(files, "before.txt -> after.txt") {
		t.Errorf("rename arrow leaked into the file list: %v", files)
	}
}

// The stranded-work hint is for RESUMED tasks only. A fresh claim in a dirty checkout is somebody
// else's work in the tree; pointing a new task at it invites cross-task edits.
func TestResumePrefixOnlyFlagsStrandedWorkForAResumedTask(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "stranded.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	resumed := (&app{}).resumePrefixFor(repo, "t", stateInProgress, nil)
	if !strings.Contains(resumed, "stranded.go") {
		t.Errorf("a resumed task was not told about the uncommitted work:\n%s", resumed)
	}
	if fresh := (&app{}).resumePrefixFor(repo, "t", stateTodo, nil); fresh != "" {
		t.Errorf("a freshly claimed task was pointed at another task's dirty tree:\n%s", fresh)
	}
}
