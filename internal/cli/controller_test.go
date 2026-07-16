package cli

import (
	"context"
	"errors"
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
	windows, err := beginCompletionWindows([]string{root})
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

func TestReviewCompletionWindowRejectsInPlaceArchiveMutation(t *testing.T) {
	root := t.TempDir()
	archived := taskForLease(t, root, stateDone, "mutated-review-archive")
	if err := completeTrustedTask(root, archived); err != nil {
		t.Fatal(err)
	}
	archived, _ = currentTask(root, archived.ID)
	windows, err := beginReviewCompletionWindows([]string{root})
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
		false, []string{root}, completionWindowStrict, true, nil, io.Discard, nil, "setup failure",
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
	windows, err := beginReviewCompletionWindows([]string{root})
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
	if resumeLine("x", nil) != "" {
		t.Error("no commits should yield no resume line")
	}
	// A landed commit → a line that names the sha and BOTH cases (finish-the-move vs reopened-rework),
	// so it never falsely asserts the task is done.
	l := resumeLine("my-task", []string{"abc123"})
	for _, want := range []string{"my-task", "abc123", "log.md", "REOPENED", "Coop-Recovery", "finish the move"} {
		if !strings.Contains(l, want) {
			t.Errorf("resume line missing %q:\n%s", want, l)
		}
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

// TestCommitsForTaskAndUntrailered drives the real git trailer parser. Fresh work binds only in its
// commit range; unchanged HEAD, malformed, duplicate, different-id, and substring values fail closed.
func TestCommitsForTaskAndUntrailered(t *testing.T) {
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
	if m := untrailered(repo, base, head, []string{"task-42"}); len(m) != 0 {
		t.Errorf("task-42 is trailered in range, should not be flagged: %v", m)
	}
	if m := untrailered(repo, base, head, []string{"task-42", "task-99"}); len(m) != 1 || m[0] != "task-99" {
		t.Errorf("untrailered = %v, want [task-99]", m)
	}
	// No-HEAD-change work must fail closed even if an old exact trailer is reachable. Crash-left
	// completion recovery restores the task and requires a new range-bound amend/recommit.
	if m := untrailered(repo, head, head, []string{"task-42"}); !slices.Equal(m, []string{"task-42"}) {
		t.Errorf("unchanged HEAD used historical task binding: %v", m)
	}
	if m := untrailered(repo, head, head, []string{"task-4", "task"}); len(m) != 2 || m[0] != "task-4" || m[1] != "task" {
		t.Errorf("different ids and substrings must remain untrailered, got %v", m)
	}
	if m := untrailered(repo, "", head, []string{"task-42"}); !slices.Equal(m, []string{"task-42"}) {
		t.Errorf("unknown iteration base must fail closed, got %v", m)
	}
	if m := untrailered(repo, head, "", []string{"task-42"}); !slices.Equal(m, []string{"task-42"}) {
		t.Errorf("unknown iteration head must fail closed, got %v", m)
	}

	// Once HEAD changes, an older valid trailer cannot bless fresh unbound work.
	git("commit", "-q", "--allow-empty", "-m", "fresh rework without a trailer")
	unboundHead := gitOut(repo, "rev-parse", "HEAD")
	if m := untrailered(repo, head, unboundHead, []string{"task-42"}); !slices.Equal(m, []string{"task-42"}) {
		t.Errorf("historical-only binding after fresh work = %v, want [task-42]", m)
	}

	// A trailer-like line outside Git's final contiguous trailer block is not a trailer.
	git("commit", "-q", "--allow-empty", "-m", "malformed\n\nCoop-Task: task-42\n\nCo-authored-by: T <t@t>")
	malformedHead := gitOut(repo, "rev-parse", "HEAD")
	if m := untrailered(repo, unboundHead, malformedHead, []string{"task-42"}); !slices.Equal(m, []string{"task-42"}) {
		t.Errorf("malformed trailer binding = %v, want [task-42]", m)
	}

	// Multiple Coop-Task values are ambiguous even when both values happen to match.
	git("commit", "-q", "--allow-empty", "-m", "duplicate\n\nCoop-Task: task-42\nCoop-Task: task-42")
	duplicateHead := gitOut(repo, "rev-parse", "HEAD")
	if c := commitsForTask(repo, malformedHead+".."+duplicateHead, "task-42"); len(c) != 0 {
		t.Errorf("duplicate trailers must not bind, got commits %v", c)
	}
	if m := untrailered(repo, malformedHead, duplicateHead, []string{"task-42"}); !slices.Equal(m, []string{"task-42"}) {
		t.Errorf("duplicate trailer binding = %v, want [task-42]", m)
	}

	git("commit", "-q", "--allow-empty", "-m", "valid again\n\nCoop-Task: task-42")
	validHead := gitOut(repo, "rev-parse", "HEAD")
	if m := untrailered(repo, duplicateHead, validHead, []string{"task-42"}); len(m) != 0 {
		t.Errorf("single exact trailer should bind fresh work: %v", m)
	}
	// Two individually valid commits for one task are still ambiguous: one task must bind to one
	// commit in the iteration range, not merely find at least one matching trailer somewhere in it.
	git("commit", "-q", "--allow-empty", "-m", "second valid binding\n\nCoop-Task: task-42")
	twoBindingsHead := gitOut(repo, "rev-parse", "HEAD")
	if m := untrailered(repo, duplicateHead, twoBindingsHead, []string{"task-42"}); !slices.Equal(m, []string{"task-42"}) {
		t.Errorf("multiple matching commits must fail closed, got %v", m)
	}
	// landedTasks sees the trailer across all history.
	if !landedTasks(repo)["task-42"] {
		t.Error("landedTasks should include task-42")
	}
	git("commit", "-q", "--allow-empty", "-m", "ambiguous landed\n\nCoop-Task: duplicate-landed\nCoop-Task: duplicate-landed")
	if landedTasks(repo)["duplicate-landed"] {
		t.Error("landedTasks accepted a commit with duplicate Coop-Task trailers")
	}
}

func TestRestoreUnbindableCompletions(t *testing.T) {
	root := t.TempDir()
	id := "2026-01-01-unbound"
	doneDir := filepath.Join(root, stateDone, id)
	writeTaskFile(t, filepath.Join(doneDir, "task.md"), "# Unbound\n")
	writeTaskFile(t, filepath.Join(doneDir, "log.md"), "# Log\n")

	item := readTaskTree(root)[0]
	if err := restoreQueuedCompletion(queuedTask{Root: root, Item: item}); err != nil {
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
	for _, want := range []string{"completion rejected", "expected exactly one commit", "git commit --amend --no-edit --trailer", "rewrite or squash", id} {
		if !strings.Contains(string(log), want) {
			t.Errorf("rejection log missing %q:\n%s", want, log)
		}
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
	for _, want := range []string{"completion rejected", "restored to in_progress", "git commit --amend --no-edit --trailer", "rewrite/squash", id} {
		if !strings.Contains(rejectErr.Error(), want) {
			t.Errorf("controller error missing %q: %v", want, rejectErr)
		}
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
	git("commit", "-q", "--allow-empty", "-m", "todo1 work\n\nCoop-Task: todo1")
	git("commit", "-q", "--allow-empty", "-m", "wip1 work\n\nCoop-Task: wip1")
	git("commit", "-q", "--allow-empty", "-m", "blk1 work\n\nCoop-Task: blk1")
	git("commit", "-q", "--allow-empty", "-m", "ambiguous work\n\nCoop-Task: same-id")

	a := &app{cfg: &config.Config{TasksFiles: []string{tasksRoot, q2Rel}}}
	a.reconcileQueueAfterMerge(repo, "fork1")

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
