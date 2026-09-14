package tasks

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func pendingReviewTestCompletion(t *testing.T) (repo, root, base string, assignment TaskAssignment, done Item, plan PendingReviewPlan) {
	t.Helper()
	repo = initRepo(t)
	root = filepath.Join(repo, TasksRoot)
	taskWithCompletedChecklist(t, root, StateTodo, "review-me")
	var err error
	assignment, err = AssignLoopTaskOnly([]string{root}, testLeaseOwner(), "")
	if err != nil || assignment.Lease == nil {
		t.Fatalf("assign = %+v, %v", assignment, err)
	}
	if _, err := EnsureTaskInstance(root, assignment.Task.Item); err != nil {
		t.Fatal(err)
	}
	assignment.Lease.Quiesce()
	base = gitOut(repo, "rev-parse", "HEAD")
	writeTaskFile(t, filepath.Join(repo, "change.txt"), "review me\n")
	git(t, repo, "add", "change.txt")
	git(t, repo, "commit", "-qm", "implement review subject\n\nCoop-Task: review-me")
	if err := MoveTaskDir(root, assignment.Task.Item, StateDone); err != nil {
		t.Fatal(err)
	}
	done, _ = mustCurrentTask(t, root, "review-me")
	if err := FinalizeQueuedCompletion(QueuedTask{Root: root, Item: done}); err != nil {
		t.Fatal(err)
	}
	plan, err = NewPendingReviewPlan(repo, []string{root}, base, "abc123", "coop loop",
		PendingReviewStage{Targets: []string{"codex:test@personal"}, Prompt: "review", Writes: "tasks"}, 5,
		true, PendingReviewStage{Targets: []string{"claude:test@personal"}, Prompt: "verify", Writes: "tasks"}, false)
	if err != nil {
		t.Fatal(err)
	}
	return repo, root, base, assignment, done, plan
}

func TestPendingReviewEnrollmentSurvivesRestart(t *testing.T) {
	repo, root, _, assignment, done, plan := pendingReviewTestCompletion(t)
	if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}

	cohort, err := LoadPendingReviews(repo, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if !pendingReviewPlanEqual(cohort.Plan, plan) || len(cohort.Subjects) != 1 {
		t.Fatalf("cohort = %+v, want one subject under original plan", cohort)
	}
	got := cohort.Subjects[0]
	if got.Task.Ref.ID != "review-me" || got.Phase != PendingReviewSignoff || got.Prepared || got.Fingerprint.Receipt == "" {
		t.Fatalf("pending subject = %+v", got)
	}
}

func TestPendingReviewPreparedRecordPromotesOnlyWithMatchingReceipt(t *testing.T) {
	repo, root, _, assignment, done, plan := pendingReviewTestCompletion(t)
	if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
		t.Fatal(err)
	}
	record, ok, err := readPendingReviewRecord(root, done.ID)
	if err != nil || !ok {
		t.Fatalf("record = %+v, %v, %v", record, ok, err)
	}
	record.Prepared = true // simulate death after receipt publication but before activation rewrite
	if err := writePendingReviewRecord(root, record); err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}

	cohort, err := LoadPendingReviews(repo, []string{root})
	if err != nil || len(cohort.Subjects) != 1 || cohort.Subjects[0].Prepared {
		t.Fatalf("prepared recovery = %+v, %v", cohort, err)
	}
}

func TestPendingReviewPreparedRecordRollsBackWithoutAcceptedReceipt(t *testing.T) {
	repo, root, _, assignment, done, plan := pendingReviewTestCompletion(t)
	if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
		t.Fatal(err)
	}
	record, ok, err := readPendingReviewRecord(root, done.ID)
	if err != nil || !ok {
		t.Fatalf("record = %+v, %v, %v", record, ok, err)
	}
	record.Prepared = true
	if err := writePendingReviewRecord(root, record); err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.ClearCompleted(); err != nil {
		t.Fatal(err)
	}
	if err := RestoreUnrecordedCompletion(QueuedTask{Root: root, Item: done}); err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	cohort, err := LoadPendingReviews(repo, []string{root})
	if err != nil || len(cohort.Subjects) != 0 {
		t.Fatalf("unaccepted preparation = %+v, %v", cohort, err)
	}
	if _, ok, err := readPendingReviewRecord(root, done.ID); err != nil || ok {
		t.Fatalf("prepared record after rollback = ok %v, err %v", ok, err)
	}
}

func pendingReviewTestPlan(t *testing.T, repo, root, base string) PendingReviewPlan {
	t.Helper()
	plan, err := NewPendingReviewPlan(repo, []string{root}, base, "abc123", "coop loop",
		PendingReviewStage{Targets: []string{"codex:test@personal"}, Prompt: "review", Writes: "tasks"}, 5,
		false, PendingReviewStage{}, false)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestPendingAwareReviewWindowEnrollsConcurrentCompletionBeforeRetirement(t *testing.T) {
	repo := initRepo(t)
	root := filepath.Join(repo, TasksRoot)
	subject := taskWithCompletedChecklist(t, root, StateDone, "review-subject")
	if err := CompleteTrustedTask(root, subject); err != nil {
		t.Fatal(err)
	}
	foreign := taskWithCompletedChecklist(t, root, StateTodo, "foreign-completion")
	base := gitOut(repo, "rev-parse", "HEAD")
	plan := pendingReviewTestPlan(t, repo, root, base)
	windows, err := BeginReviewCompletionWindowsWithPending([]string{root}, []string{subject.ID}, plan)
	if err != nil {
		t.Fatal(err)
	}
	writeTaskFile(t, filepath.Join(repo, "foreign.txt"), "completed elsewhere\n")
	git(t, repo, "add", "foreign.txt")
	git(t, repo, "commit", "-qm", "finish foreign task\n\nCoop-Task: "+foreign.ID)
	if err := CompleteTrustedTask(root, foreign); err != nil {
		t.Fatal(err)
	}
	if record, ok, err := readPendingReviewRecord(root, foreign.ID); err != nil || !ok || record.Prepared {
		t.Fatalf("pending record before window retirement = %+v, %v, %v", record, ok, err)
	}
	concurrent, err := windows.FinishReview()
	if err != nil || len(concurrent) != 1 || concurrent[0] != foreign.ID {
		t.Fatalf("finish review = %v, %v", concurrent, err)
	}
	cohort, err := LoadPendingReviews(repo, []string{root})
	if err != nil || len(cohort.Subjects) != 1 || cohort.Subjects[0].Task.Ref.ID != foreign.ID {
		t.Fatalf("durable concurrent cohort = %+v, %v", cohort, err)
	}
}

func TestPendingAwareReviewWindowRecoveryEnrollsBeforeJournalRetirement(t *testing.T) {
	repo := initRepo(t)
	root := filepath.Join(repo, TasksRoot)
	subject := taskWithCompletedChecklist(t, root, StateDone, "review-subject")
	if err := CompleteTrustedTask(root, subject); err != nil {
		t.Fatal(err)
	}
	foreign := taskWithCompletedChecklist(t, root, StateTodo, "legacy-concurrent")
	assignment, err := AssignLoopTaskOnly([]string{root}, testLeaseOwner(), foreign.ID)
	if err != nil || assignment.Lease == nil {
		t.Fatalf("assign = %+v, %v", assignment, err)
	}
	assignment.Lease.Quiesce()
	base := gitOut(repo, "rev-parse", "HEAD")
	plan := pendingReviewTestPlan(t, repo, root, base)
	windows, err := BeginReviewCompletionWindowsWithPending([]string{root}, []string{subject.ID}, plan)
	if err != nil {
		t.Fatal(err)
	}
	writeTaskFile(t, filepath.Join(repo, "legacy.txt"), "completed by an older host path\n")
	git(t, repo, "add", "legacy.txt")
	git(t, repo, "commit", "-qm", "finish legacy concurrent task\n\nCoop-Task: "+foreign.ID)
	if err := MoveTaskDir(root, assignment.Task.Item, StateDone); err != nil {
		t.Fatal(err)
	}
	done, _ := mustCurrentTask(t, root, foreign.ID)
	if err := FinalizeQueuedCompletion(QueuedTask{Root: root, Item: done}); err != nil {
		t.Fatal(err)
	}
	// Model a receipt produced by a pre-feature host path: accepted, but not yet enrolled.
	if err := writeLeaseCompletionReceipt(assignment.Lease.authority, done.Dir, ""); err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	if err := windows.Abandon(); err != nil {
		t.Fatal(err)
	}

	concurrent, err := ReconcileCompletionWindowsWithActivity([]string{root})
	if err != nil || len(concurrent) != 1 || concurrent[0] != foreign.ID {
		t.Fatalf("recover completion window = %v, %v", concurrent, err)
	}
	cohort, err := LoadPendingReviews(repo, []string{root})
	if err != nil || len(cohort.Subjects) != 1 || cohort.Subjects[0].Task.Ref.ID != foreign.ID {
		t.Fatalf("recovered concurrent cohort = %+v, %v", cohort, err)
	}
}

func TestPendingReviewExplicitImportIsExactAndIdempotent(t *testing.T) {
	repo := initRepo(t)
	root := filepath.Join(repo, TasksRoot)
	item := taskWithCompletedChecklist(t, root, StateTodo, "legacy-import")
	base := gitOut(repo, "rev-parse", "HEAD")
	writeTaskFile(t, filepath.Join(repo, "import.txt"), "legacy completion\n")
	git(t, repo, "add", "import.txt")
	git(t, repo, "commit", "-qm", "finish legacy import\n\nCoop-Task: "+item.ID)
	if err := CompleteTrustedTask(root, item); err != nil {
		t.Fatal(err)
	}
	plan := pendingReviewTestPlan(t, repo, root, base)
	for attempt := 0; attempt < 2; attempt++ {
		if err := EnrollExistingPendingReviews(repo, []string{root}, []string{item.ID}, plan); err != nil {
			t.Fatalf("import attempt %d: %v", attempt+1, err)
		}
	}
	cohort, err := LoadPendingReviews(repo, []string{root})
	if err != nil || len(cohort.Subjects) != 1 || cohort.Subjects[0].Task.Ref.ID != item.ID {
		t.Fatalf("imported cohort = %+v, %v", cohort, err)
	}

	unaccepted := taskWithCompletedChecklist(t, root, StateDone, "unaccepted-import")
	if err := EnrollExistingPendingReviews(repo, []string{root}, []string{unaccepted.ID}, plan); err == nil || !strings.Contains(err.Error(), "no host-accepted completion receipt") {
		t.Fatalf("unaccepted import error = %v", err)
	}
}

func TestPendingReviewRejectsArchiveAndHistoryMutation(t *testing.T) {
	t.Run("archive", func(t *testing.T) {
		repo, root, _, assignment, done, plan := pendingReviewTestCompletion(t)
		if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
			t.Fatal(err)
		}
		if err := assignment.Lease.Release(); err != nil {
			t.Fatal(err)
		}
		writeTaskFile(t, filepath.Join(done.Dir, "log.md"), "tampered\n")
		if _, err := LoadPendingReviews(repo, []string{root}); err == nil || !strings.Contains(err.Error(), "archive changed") {
			t.Fatalf("archive mutation error = %v", err)
		}
	})

	t.Run("history", func(t *testing.T) {
		repo, root, base, assignment, done, plan := pendingReviewTestCompletion(t)
		if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
			t.Fatal(err)
		}
		if err := assignment.Lease.Release(); err != nil {
			t.Fatal(err)
		}
		git(t, repo, "reset", "--hard", base)
		writeTaskFile(t, filepath.Join(repo, "change.txt"), "different\n")
		git(t, repo, "add", "change.txt")
		git(t, repo, "commit", "-qm", "different review subject\n\nCoop-Task: review-me")
		if _, err := LoadPendingReviews(repo, []string{root}); err == nil || !strings.Contains(err.Error(), "Git history changed") {
			t.Fatalf("history mutation error = %v", err)
		}
	})
}

func TestPendingReviewRecordRejectsUnknownAndOversizeData(t *testing.T) {
	repo, root, _, assignment, done, plan := pendingReviewTestCompletion(t)
	if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	name, err := pendingReviewRecordName(root, done.ID)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := OpenLeaseAuthorityRoot()
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	registryPath, err := leaseAuthorityRoot()
	if err != nil {
		t.Fatal(err)
	}
	hardlink := filepath.Join(registryPath, "pending-review-hardlink")
	if err := os.Link(filepath.Join(registryPath, name), hardlink); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readPendingReviewRecord(root, done.ID); err == nil || !strings.Contains(err.Error(), "single-link") {
		t.Fatalf("hard-linked record error = %v", err)
	}
	if err := os.Remove(hardlink); err != nil {
		t.Fatal(err)
	}
	record, _, err := readPendingReviewRecord(root, done.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(record)
	raw = append(raw[:len(raw)-1], []byte(`,"unknown":true}`)...)
	if err := AtomicWriteTaskFile(registry, name, append(raw, '\n')); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readPendingReviewRecord(root, done.ID); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field error = %v", err)
	}
	if err := AtomicWriteTaskFile(registry, name, []byte(strings.Repeat("x", pendingReviewFileLimit+1))); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readPendingReviewRecord(root, done.ID); err == nil || !strings.Contains(err.Error(), "bounded") {
		t.Fatalf("oversize error = %v", err)
	}
}

func TestPendingReviewRecordRejectsMalformedRollbackState(t *testing.T) {
	repo, _, _, assignment, done, plan := pendingReviewTestCompletion(t)
	if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
		t.Fatal(err)
	}
	record, ok, err := readPendingReviewRecord(assignment.Task.Root, done.ID)
	if err != nil || !ok {
		t.Fatalf("record = %+v, %v, %v", record, ok, err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*PendingReviewPrevious)
	}{
		{name: "generation", mutate: func(previous *PendingReviewPrevious) { previous.Task.Generation.Inode++ }},
		{name: "receipt", mutate: func(previous *PendingReviewPrevious) { previous.Fingerprint.Receipt = "not-a-nonce" }},
		{name: "raw head", mutate: func(previous *PendingReviewPrevious) {
			previous.Binding.Raw[len(previous.Binding.Raw)-1] = strings.Repeat("a", 40)
		}},
		{name: "semantic subject", mutate: func(previous *PendingReviewPrevious) { previous.Binding.Subject.TaskID = "another-task" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := record
			candidate.Prepared = true
			candidate.Previous = &PendingReviewPrevious{
				Task: record.Task, Fingerprint: record.Fingerprint, Binding: record.Binding,
				Phase: record.Phase, Round: record.Round, RoundStarted: record.RoundStarted,
			}
			candidate.Previous.Binding.Raw = slices.Clone(record.Binding.Raw)
			candidate.Previous.Binding.History = slices.Clone(record.Binding.History)
			tc.mutate(candidate.Previous)
			if err := validatePendingReviewRecord(candidate); err == nil {
				t.Fatal("malformed rollback state was accepted")
			}
		})
	}
}

func TestPreparedRecompletionRollbackRestoresPriorDebt(t *testing.T) {
	repo, root, _, assignment, done, plan := pendingReviewTestCompletion(t)
	if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	if err := MarkPendingReviewReopened([]string{root}, []string{done.ID}); err != nil {
		t.Fatal(err)
	}
	prior, _, _ := readPendingReviewRecord(root, done.ID)
	prepared := prior
	prepared.Phase = PendingReviewSignoff
	prepared.Prepared = true
	prepared.Previous = &PendingReviewPrevious{
		Task: prior.Task, Fingerprint: prior.Fingerprint, Binding: prior.Binding,
		Phase: prior.Phase, Round: prior.Round, RoundStarted: prior.RoundStarted,
	}
	if err := writePendingReviewRecord(root, prepared); err != nil {
		t.Fatal(err)
	}
	if err := rollbackPreparedPendingReview(root, prepared); err != nil {
		t.Fatal(err)
	}
	restored, ok, err := readPendingReviewRecord(root, done.ID)
	if err != nil || !ok || !pendingReviewRecordMatchesExpected(restored, prior) {
		t.Fatalf("restored prior debt = %+v, %v, %v", restored, ok, err)
	}
}

func TestTaskDeletionClearsPendingReviewRecord(t *testing.T) {
	repo, root, _, assignment, done, plan := pendingReviewTestCompletion(t)
	if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	removed, err := removeTaskFolderAndRecords(root, done)
	if err != nil || !removed {
		t.Fatalf("remove = %v, %v", removed, err)
	}
	if _, ok, err := readPendingReviewRecord(root, done.ID); err != nil || ok {
		t.Fatalf("pending record after delete = ok %v, err %v", ok, err)
	}
}

func TestPendingReviewStageTransitionsAndClear(t *testing.T) {
	repo, root, _, assignment, done, plan := pendingReviewTestCompletion(t)
	if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	if err := BeginPendingReviewRound([]string{root}, []string{done.ID}, 2); err != nil {
		t.Fatal(err)
	}
	if err := MarkPendingReviewVerify([]string{root}, []string{done.ID}); err != nil {
		t.Fatal(err)
	}
	record, _, _ := readPendingReviewRecord(root, done.ID)
	if record.Phase != PendingReviewVerify || record.Round != 2 {
		t.Fatalf("transition = phase %q round %d", record.Phase, record.Round)
	}
	if err := ClearPendingReviews([]string{root}, []PendingReviewRecord{record}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := readPendingReviewRecord(root, done.ID); err != nil || ok {
		t.Fatalf("record after clear = ok %v, err %v", ok, err)
	}
	if _, ok, err := readPendingReviewReviewed(root, done.ID); err != nil || !ok {
		t.Fatalf("reviewed receipt after clear = ok %v, err %v", ok, err)
	}
	if err := EnrollExistingPendingReviews(repo, []string{root}, []string{done.ID}, plan); err == nil || !strings.Contains(err.Error(), "already passed final review") {
		t.Fatalf("repeat import error = %v", err)
	}
	if marked, err := pendingReviewWorkspaceMarked(repo); err != nil || marked {
		t.Fatalf("workspace marker after clear = %v, %v", marked, err)
	}
}

func TestPendingReviewActivationFailureRestoresPriorReopen(t *testing.T) {
	repo, root, _, assignment, done, plan := pendingReviewTestCompletion(t)
	if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	prior, _, err := readPendingReviewRecord(root, done.ID)
	if err != nil {
		t.Fatal(err)
	}
	prior.Phase = PendingReviewReopened
	prior.Round = 2
	if err := writePendingReviewRecord(root, prior); err != nil {
		t.Fatal(err)
	}
	prepared := prior
	prepared.Prepared = true
	prepared.Phase = PendingReviewSignoff
	prepared.Previous = &PendingReviewPrevious{
		Task: prior.Task, Fingerprint: prior.Fingerprint, Binding: prior.Binding,
		Phase: prior.Phase, Round: prior.Round, RoundStarted: prior.RoundStarted,
	}
	if err := writePendingReviewRecord(root, prepared); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("activation write failed")
	err = activatePreparedPendingReview(root, prepared, func(string, PendingReviewRecord) error { return wantErr })
	if !errors.Is(err, wantErr) {
		t.Fatalf("activation failure = %v", err)
	}
	restored, ok, err := readPendingReviewRecord(root, done.ID)
	if err != nil || !ok || restored.Prepared || restored.Previous != nil || restored.Phase != PendingReviewReopened || restored.Round != 2 || restored.Fingerprint != prior.Fingerprint {
		t.Fatalf("restored prior review = %+v, %v, %v", restored, ok, err)
	}
}

func TestPendingReviewImportBasePrecedesImportedCommit(t *testing.T) {
	repo, root, base, assignment, done, plan := pendingReviewTestCompletion(t)
	if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	got, err := PendingReviewImportBase(repo, []string{root}, []string{done.ID})
	if err != nil || got != base {
		t.Fatalf("import base = %q, %v; want %q", got, err, base)
	}
}

func TestPendingReviewClearIsGenerationFencedAndCrashIdempotent(t *testing.T) {
	t.Run("changed after verdict", func(t *testing.T) {
		repo, root, _, assignment, done, plan := pendingReviewTestCompletion(t)
		if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
			t.Fatal(err)
		}
		if err := assignment.Lease.Release(); err != nil {
			t.Fatal(err)
		}
		want, _, _ := readPendingReviewRecord(root, done.ID)
		if err := MarkPendingReviewVerify([]string{root}, []string{done.ID}); err != nil {
			t.Fatal(err)
		}
		if err := ClearPendingReviews([]string{root}, []PendingReviewRecord{want}); err == nil || !strings.Contains(err.Error(), "changed generation or phase") {
			t.Fatalf("stale clear error = %v", err)
		}
		if _, ok, err := readPendingReviewRecord(root, done.ID); err != nil || !ok {
			t.Fatalf("stale clear removed debt = %v, %v", ok, err)
		}
	})

	t.Run("reviewed receipt before active removal", func(t *testing.T) {
		repo, root, _, assignment, done, plan := pendingReviewTestCompletion(t)
		if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
			t.Fatal(err)
		}
		if err := assignment.Lease.Release(); err != nil {
			t.Fatal(err)
		}
		record, _, _ := readPendingReviewRecord(root, done.ID)
		if err := writePendingReviewReviewed(root, record); err != nil {
			t.Fatal(err)
		}
		cohort, err := LoadPendingReviews(repo, []string{root})
		if err != nil || len(cohort.Subjects) != 0 {
			t.Fatalf("clear recovery = %+v, %v", cohort, err)
		}
		if _, ok, err := readPendingReviewRecord(root, done.ID); err != nil || ok {
			t.Fatalf("active record after clear recovery = %v, %v", ok, err)
		}
	})

	t.Run("archive changed after verdict", func(t *testing.T) {
		repo, root, _, assignment, done, plan := pendingReviewTestCompletion(t)
		if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
			t.Fatal(err)
		}
		if err := assignment.Lease.Release(); err != nil {
			t.Fatal(err)
		}
		want, _, _ := readPendingReviewRecord(root, done.ID)
		writeTaskFile(t, filepath.Join(done.Dir, "log.md"), "changed after verdict\n")
		if err := ClearPendingReviews([]string{root}, []PendingReviewRecord{want}); err == nil || !strings.Contains(err.Error(), "archive changed after the review verdict") {
			t.Fatalf("archive race clear error = %v", err)
		}
		if _, ok, err := readPendingReviewRecord(root, done.ID); err != nil || !ok {
			t.Fatalf("archive race removed debt = %v, %v", ok, err)
		}
	})
}

func TestTrustedRecompletionReturnsReopenedTaskToPendingSignoff(t *testing.T) {
	repo, root, _, assignment, done, plan := pendingReviewTestCompletion(t)
	if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	if err := MarkPendingReviewReopened([]string{root}, []string{done.ID}); err != nil {
		t.Fatal(err)
	}
	if err := MoveTaskDir(root, done, StateTodo); err != nil {
		t.Fatal(err)
	}
	reopened, _ := mustCurrentTask(t, root, done.ID)
	if err := CompleteTrustedTask(root, reopened); err != nil {
		t.Fatal(err)
	}
	record, ok, err := readPendingReviewRecord(root, done.ID)
	if err != nil || !ok || record.Prepared || record.Phase != PendingReviewSignoff {
		t.Fatalf("recompleted record = %+v, %v, %v", record, ok, err)
	}
	if current, ok, err := CurrentTask(root, done.ID); err != nil || !ok || current.State != StateDone {
		t.Fatalf("recompleted task = %+v, %v, %v", current, ok, err)
	}
}

func TestPendingReviewRecordNameCannotEscapeRegistry(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tasks")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	name, err := pendingReviewRecordName(root, "../../escape")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(name, "/") || !strings.HasSuffix(name, pendingReviewFileSuffix) {
		t.Fatalf("record name = %q", name)
	}
}

func TestPendingReviewSigningRebindIsExplicitAndExact(t *testing.T) {
	repo, root, _, assignment, done, plan := pendingReviewTestCompletion(t)
	if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	oldHead := gitOut(repo, "rev-parse", "HEAD")
	cmd := exec.Command("git", "-C", repo, "commit", "--amend", "--no-edit", "--no-gpg-sign", "--quiet")
	cmd.Env = append(os.Environ(), "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_COMMITTER_DATE=2030-01-02T03:04:05Z")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("amend: %v\n%s", err, out)
	}
	newHead := gitOut(repo, "rev-parse", "HEAD")
	if oldHead == newHead {
		t.Fatal("test signing rewrite did not change the commit id")
	}
	if _, err := LoadPendingReviews(repo, []string{root}); err == nil || !strings.Contains(err.Error(), "outside an acknowledged host signing rewrite") {
		t.Fatalf("unacknowledged rewrite error = %v", err)
	}
	if err := RebindPendingReviewAfterSigning(repo, root, done.ID, oldHead, newHead); err != nil {
		t.Fatal(err)
	}
	cohort, err := LoadPendingReviews(repo, []string{root})
	if err != nil || len(cohort.Subjects) != 1 || cohort.Subjects[0].Binding.Head != newHead {
		t.Fatalf("rebound cohort = %+v, %v", cohort, err)
	}
}

func pendingReviewRewrittenLaterCohort(t *testing.T) (string, string, Item, Item, string, string, AuditReopenRecord) {
	t.Helper()
	repo := initRepo(t)
	root := filepath.Join(repo, TasksRoot)
	earlier := taskWithCompletedChecklist(t, root, StateTodo, "review-earlier")
	later := taskWithCompletedChecklist(t, root, StateTodo, "review-later")
	base := gitOut(repo, "rev-parse", "HEAD")

	writeTaskFile(t, filepath.Join(repo, "earlier.txt"), "earlier\n")
	git(t, repo, "add", "earlier.txt")
	git(t, repo, "commit", "-qm", "earlier implementation\n\nCoop-Task: "+earlier.ID)
	if err := CompleteTrustedTask(root, earlier); err != nil {
		t.Fatal(err)
	}
	writeTaskFile(t, filepath.Join(repo, "later.txt"), "later\n")
	git(t, repo, "add", "later.txt")
	git(t, repo, "commit", "-qm", "later implementation\n\nCoop-Task: "+later.ID)
	if err := CompleteTrustedTask(root, later); err != nil {
		t.Fatal(err)
	}
	plan := pendingReviewTestPlan(t, repo, root, base)
	if err := EnrollExistingPendingReviews(repo, []string{root}, []string{earlier.ID, later.ID}, plan); err != nil {
		t.Fatal(err)
	}

	oldHead := gitOut(repo, "rev-parse", "HEAD")
	authority, err := CaptureAuditReopen(repo, later.ID)
	if err != nil {
		t.Fatal(err)
	}
	writeTaskFile(t, filepath.Join(repo, "later.txt"), "later repaired\n")
	git(t, repo, "add", "later.txt")
	git(t, repo, "commit", "--amend", "--no-edit", "--no-gpg-sign", "--quiet")
	newHead := gitOut(repo, "rev-parse", "HEAD")
	return repo, root, earlier, later, oldHead, newHead, authority
}

func TestPendingReviewRebindAllowsAuthorizedRewriteOfLaterCohortTask(t *testing.T) {
	repo, root, earlier, later, oldHead, newHead, authority := pendingReviewRewrittenLaterCohort(t)
	if err := RebindPendingReviewAfterAuditRewrite(repo, root, earlier.ID, oldHead, newHead, later.ID, authority); err != nil {
		t.Fatal(err)
	}
	record, ok, err := readPendingReviewRecord(root, earlier.ID)
	if err != nil || !ok || record.Binding.Head != newHead || len(record.Binding.History) != 1 ||
		record.Binding.History[0].TaskID != later.ID || record.Binding.History[0] == authority.Subject {
		t.Fatalf("rebound earlier review = %+v, ok=%v, err=%v", record, ok, err)
	}
}

func TestPendingReviewRecoversAuthorizedRewriteOfLaterCohortTask(t *testing.T) {
	repo, root, _, later, _, newHead, authority := pendingReviewRewrittenLaterCohort(t)
	if err := WriteAuditReopenRecord(root, authority); err != nil {
		t.Fatal(err)
	}

	cohort, err := LoadPendingReviews(repo, []string{root})
	if err != nil || len(cohort.Subjects) != 2 {
		t.Fatalf("recovered cohort = %+v, %v", cohort, err)
	}
	for _, subject := range cohort.Subjects {
		if subject.Binding.Head != newHead {
			t.Fatalf("recovered %s at %s, want %s", subject.Task.Ref.ID, subject.Binding.Head, newHead)
		}
	}
	reopen, ok, err := ReadAuditReopenRecord(root, later.ID)
	if err != nil || !ok || reopen.BaselineHead != newHead || !AuditReopenCurrentValid(repo, newHead, later.ID, reopen) {
		t.Fatalf("recovered audit authority = %+v, ok=%v, err=%v", reopen, ok, err)
	}
}

func TestPendingReviewRebindsReopenedSiblingAuditAuthority(t *testing.T) {
	repo := initRepo(t)
	root := filepath.Join(repo, TasksRoot)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	authority, err := OpenLeaseAuthority(root, "task-b", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}

	writeTaskFile(t, filepath.Join(repo, "a.txt"), "A\n")
	git(t, repo, "add", "a.txt")
	git(t, repo, "commit", "-qm", "A implementation\n\nCoop-Task: task-a")
	actor := gitOut(repo, "rev-parse", "HEAD")
	writeTaskFile(t, filepath.Join(repo, "b.txt"), "B\n")
	git(t, repo, "add", "b.txt")
	git(t, repo, "commit", "-qm", "B implementation\n\nCoop-Task: task-b")
	sibling := gitOut(repo, "rev-parse", "HEAD")
	reopen, err := CaptureAuditReopen(repo, "task-b")
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteAuditReopenRecord(root, reopen); err != nil {
		t.Fatal(err)
	}
	gitConfig := filepath.Join(t.TempDir(), "gitconfig")
	writeTaskFile(t, gitConfig, "")
	amend := func(date string) {
		t.Helper()
		cmd := exec.Command("git", "-C", repo, "commit", "--amend", "--no-edit", "--no-gpg-sign", "--quiet")
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL="+gitConfig, "GIT_CONFIG_SYSTEM="+gitConfig,
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_COMMITTER_DATE="+date)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("amend sibling metadata: %v\n%s", err, out)
		}
	}

	git(t, repo, "reset", "--hard", actor+"^")
	writeTaskFile(t, filepath.Join(repo, "a.txt"), "A repaired\n")
	git(t, repo, "add", "a.txt")
	git(t, repo, "commit", "-qm", "A implementation\n\nCoop-Task: task-a")
	git(t, repo, "cherry-pick", sibling)
	rewrittenHead := gitOut(repo, "rev-parse", "HEAD")
	binding, err := capturePendingReviewBinding(repo, "task-b")
	if err != nil {
		t.Fatal(err)
	}
	if binding.Subject != reopen.Subject {
		t.Fatal("test rewrite changed the sibling task semantics")
	}
	if err := rebindAuditReopenToPendingBinding(repo, root, "task-b", binding); err != nil {
		t.Fatal(err)
	}
	got, ok, err := ReadAuditReopenRecord(root, "task-b")
	if err != nil || !ok || got.Generation != reopen.Generation || got.BaselineHead != rewrittenHead ||
		!AuditReopenCurrentValid(repo, rewrittenHead, "task-b", got) {
		t.Fatalf("rebound sibling authority = %+v, ok=%v, err=%v", got, ok, err)
	}

	amend("2032-03-04T05:06:07Z")
	signedHead := gitOut(repo, "rev-parse", "HEAD")
	signedBinding, err := capturePendingReviewBinding(repo, "task-b")
	if err != nil {
		t.Fatal(err)
	}
	if err := rebindAuditReopenToPendingBinding(repo, root, "task-b", signedBinding); err != nil {
		t.Fatal(err)
	}
	got, ok, err = ReadAuditReopenRecord(root, "task-b")
	if err != nil || !ok || got.Generation != reopen.Generation || got.BaselineHead != signedHead ||
		!AuditReopenCurrentValid(repo, signedHead, "task-b", got) {
		t.Fatalf("signed sibling authority = %+v, ok=%v, err=%v", got, ok, err)
	}

	amend("2033-04-05T06:07:08Z")
	bad, err := capturePendingReviewBinding(repo, "task-b")
	if err != nil {
		t.Fatal(err)
	}
	bad.Subject.ChangeTree = "changed"
	if err := rebindAuditReopenToPendingBinding(repo, root, "task-b", bad); err == nil {
		t.Fatal("changed sibling semantics acquired audit authority")
	}
	unchanged, ok, err := ReadAuditReopenRecord(root, "task-b")
	if err != nil || !ok || !AuditReopenRecordsEqual(unchanged, got) {
		t.Fatalf("denied rebind changed authority = %+v, ok=%v, err=%v", unchanged, ok, err)
	}
}

func TestPendingReviewSigningJournalRecoversCrashAfterRefUpdate(t *testing.T) {
	repo, root, base, assignment, done, plan := pendingReviewTestCompletion(t)
	if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	oldHead := gitOut(repo, "rev-parse", "HEAD")
	oldCommits := strings.Fields(gitOut(repo, "log", "--reverse", "--format=%H", base+".."+oldHead))
	cmd := exec.Command("git", "-C", repo, "commit", "--amend", "--no-edit", "--no-gpg-sign", "--quiet")
	cmd.Env = append(os.Environ(), "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_COMMITTER_DATE=2031-02-03T04:05:06Z")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("amend: %v\n%s", err, out)
	}
	newHead := gitOut(repo, "rev-parse", "HEAD")
	newCommits := strings.Fields(gitOut(repo, "log", "--reverse", "--format=%H", base+".."+newHead))
	git(t, repo, "reset", "--hard", oldHead)
	branch := gitOut(repo, "symbolic-ref", "--quiet", "HEAD")
	if err := RecordPendingReviewSigning(repo, branch, oldHead, newHead, oldCommits, newCommits); err != nil {
		t.Fatal(err)
	}
	// Model the host dying immediately after update-ref and before the loop can call its ordinary
	// in-process rebind helper.
	git(t, repo, "reset", "--hard", newHead)
	cohort, err := LoadPendingReviews(repo, []string{root})
	if err != nil || len(cohort.Subjects) != 1 || cohort.Subjects[0].Binding.Head != newHead {
		t.Fatalf("journal recovery = %+v, %v", cohort, err)
	}
}

func TestPendingReviewSigningJournalRecoversReopenedAuditAuthority(t *testing.T) {
	repo, root, _, assignment, done, plan := pendingReviewTestCompletion(t)
	writeTaskFile(t, filepath.Join(repo, "descendant.txt"), "descendant\n")
	git(t, repo, "add", "descendant.txt")
	git(t, repo, "commit", "-qm", "descendant implementation\n\nCoop-Task: descendant")
	if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	if err := MarkPendingReviewReopened([]string{root}, []string{done.ID}); err != nil {
		t.Fatal(err)
	}
	reopen, err := CaptureAuditReopen(repo, done.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteAuditReopenRecord(root, reopen); err != nil {
		t.Fatal(err)
	}
	if err := MoveTaskDir(root, done, StateTodo); err != nil {
		t.Fatal(err)
	}

	oldHead := gitOut(repo, "rev-parse", "HEAD")
	gitConfig := filepath.Join(t.TempDir(), "gitconfig")
	writeTaskFile(t, gitConfig, "")
	cmd := exec.Command("git", "-C", repo, "commit", "--amend", "--no-edit", "--no-gpg-sign", "--quiet")
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+gitConfig, "GIT_CONFIG_SYSTEM="+gitConfig,
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_COMMITTER_DATE=2034-05-06T07:08:09Z")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("amend: %v\n%s", err, out)
	}
	newHead := gitOut(repo, "rev-parse", "HEAD")
	git(t, repo, "reset", "--hard", oldHead)
	branch := gitOut(repo, "symbolic-ref", "--quiet", "HEAD")
	if err := RecordPendingReviewSigning(repo, branch, oldHead, newHead, []string{oldHead}, []string{newHead}); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "reset", "--hard", newHead)

	cohort, err := LoadPendingReviews(repo, []string{root})
	if err != nil || len(cohort.Subjects) != 1 || cohort.Subjects[0].Binding.Head != newHead {
		t.Fatalf("reopened journal recovery = %+v, %v", cohort, err)
	}
	got, ok, err := ReadAuditReopenRecord(root, done.ID)
	if err != nil || !ok || got.Generation != reopen.Generation || got.BaselineHead != newHead ||
		!AuditReopenCurrentValid(repo, newHead, done.ID, got) {
		t.Fatalf("recovered reopened authority = %+v, ok=%v, err=%v", got, ok, err)
	}
}

func TestPendingReviewRecoversCompletedAuditRewriteAfterControllerFailure(t *testing.T) {
	repo, root, _, assignment, done, plan := pendingReviewTestCompletion(t)
	writeTaskFile(t, filepath.Join(repo, "descendant.txt"), "descendant\n")
	git(t, repo, "add", "descendant.txt")
	git(t, repo, "commit", "-qm", "descendant implementation\n\nCoop-Task: descendant")
	descendant := gitOut(repo, "rev-parse", "HEAD")
	if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	if err := MarkPendingReviewReopened([]string{root}, []string{done.ID}); err != nil {
		t.Fatal(err)
	}
	reopen, err := CaptureAuditReopen(repo, done.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteAuditReopenRecord(root, reopen); err != nil {
		t.Fatal(err)
	}
	stale, ok, err := readPendingReviewRecord(root, done.ID)
	if err != nil || !ok {
		t.Fatalf("stale pending record = %+v, ok=%v, err=%v", stale, ok, err)
	}
	if err := MoveTaskDir(root, done, StateInProgress); err != nil {
		t.Fatal(err)
	}

	git(t, repo, "reset", "--hard", reopen.BaselineHead+"~2")
	writeTaskFile(t, filepath.Join(repo, "change.txt"), "review repaired\n")
	git(t, repo, "add", "change.txt")
	git(t, repo, "commit", "-qm", "implement review subject\n\nCoop-Task: "+done.ID)
	git(t, repo, "cherry-pick", descendant)
	head := gitOut(repo, "rev-parse", "HEAD")

	cohort, err := LoadPendingReviews(repo, []string{root})
	if err != nil || len(cohort.Subjects) != 1 || cohort.Subjects[0].Binding.Head != head ||
		cohort.Subjects[0].Binding.Subject == reopen.Subject {
		t.Fatalf("audit rewrite recovery = %+v, %v", cohort, err)
	}
	got, ok, err := ReadAuditReopenRecord(root, done.ID)
	if err != nil || !ok || got.Generation != reopen.Generation || got.BaselineHead != head ||
		!AuditReopenCurrentValid(repo, head, done.ID, got) {
		t.Fatalf("recovered audit authority = %+v, ok=%v, err=%v", got, ok, err)
	}
	if err := writePendingReviewRecord(root, stale); err != nil {
		t.Fatal(err)
	}
	cohort, err = LoadPendingReviews(repo, []string{root})
	if err != nil || len(cohort.Subjects) != 1 || cohort.Subjects[0].Binding.Head != head {
		t.Fatalf("audit-first crash recovery = %+v, %v", cohort, err)
	}
}

func TestPendingReviewRejectsBranchAndRawBindingDrift(t *testing.T) {
	t.Run("branch", func(t *testing.T) {
		repo, root, _, assignment, done, plan := pendingReviewTestCompletion(t)
		if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
			t.Fatal(err)
		}
		if err := assignment.Lease.Release(); err != nil {
			t.Fatal(err)
		}
		git(t, repo, "checkout", "-qb", "other")
		if _, err := LoadPendingReviews(repo, []string{root}); err == nil || !strings.Contains(err.Error(), "different branch") {
			t.Fatalf("branch drift error = %v", err)
		}
	})

	t.Run("raw", func(t *testing.T) {
		repo, root, base, assignment, done, plan := pendingReviewTestCompletion(t)
		if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
			t.Fatal(err)
		}
		if err := assignment.Lease.Release(); err != nil {
			t.Fatal(err)
		}
		record, _, err := readPendingReviewRecord(root, done.ID)
		if err != nil {
			t.Fatal(err)
		}
		record.Binding.Raw[0] = base
		record.Binding.Head = base
		if err := writePendingReviewRecord(root, record); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadPendingReviews(repo, []string{root}); err == nil || !strings.Contains(err.Error(), "Git history changed") {
			t.Fatalf("raw drift error = %v", err)
		}
	})
}

func TestPendingReviewRequiresItsExactOrderedQueueSelection(t *testing.T) {
	repo, root, base, assignment, done, _ := pendingReviewTestCompletion(t)
	otherRoot := filepath.Join(repo, ".agent", "other-tasks")
	if err := os.MkdirAll(otherRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	plan, err := NewPendingReviewPlan(repo, []string{root, otherRoot}, base, "abc123", "coop loop",
		PendingReviewStage{Targets: []string{"codex:test@personal"}, Writes: "tasks"}, 3,
		false, PendingReviewStage{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	if cohort, err := LoadPendingReviews(repo, []string{root, otherRoot}); err != nil || len(cohort.Subjects) != 1 {
		t.Fatalf("exact queue load = %+v, %v", cohort, err)
	}
	if _, err := LoadPendingReviews(repo, []string{root}); err == nil || !strings.Contains(err.Error(), "different workspace or queue selection") {
		t.Fatalf("omitted queue error = %v", err)
	}
	if _, err := LoadPendingReviews(repo, []string{otherRoot, root}); err == nil || !strings.Contains(err.Error(), "queue 1 changed identity") {
		t.Fatalf("reordered queue error = %v", err)
	}
}

func TestPendingReviewIgnoresMalformedRecordFromAnotherQueue(t *testing.T) {
	repo, root, _, assignment, done, plan := pendingReviewTestCompletion(t)
	if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	otherRoot := filepath.Join(t.TempDir(), "other-tasks")
	if err := os.MkdirAll(otherRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	name, err := pendingReviewRecordName(otherRoot, "broken")
	if err != nil {
		t.Fatal(err)
	}
	registry, err := OpenLeaseAuthorityRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := AtomicWriteTaskFile(registry, name, []byte("{not-json\n")); err != nil {
		registry.Close()
		t.Fatal(err)
	}
	registry.Close()
	cohort, err := LoadPendingReviews(repo, []string{root})
	if err != nil || len(cohort.Subjects) != 1 {
		t.Fatalf("cohort with unrelated malformed record = %+v, %v", cohort, err)
	}
}

func TestPendingReviewCrashWindowEnrollsConcurrentCompletionBeforeRetiring(t *testing.T) {
	repo, root, _, assignment, done, plan := pendingReviewTestCompletion(t)
	if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	foreign := taskWithCompletedChecklist(t, root, StateTodo, "review-concurrent")
	writeTaskFile(t, filepath.Join(repo, "concurrent.txt"), "concurrent\n")
	git(t, repo, "add", "concurrent.txt")
	git(t, repo, "commit", "-qm", "complete concurrent review task\n\nCoop-Task: review-concurrent")
	windows, err := BeginReviewCompletionWindowsWithPending([]string{root}, []string{done.ID}, plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := CompleteTrustedTask(root, foreign); err != nil {
		t.Fatal(err)
	}
	if err := unlockLeaseFile(windows.windows[0].live); err != nil {
		t.Fatal(err)
	}
	windows.windows[0].live = nil
	observed, err := ReconcileCompletionWindowsWithActivity([]string{root})
	if err != nil || len(observed) != 1 || observed[0] != foreign.ID {
		t.Fatalf("recovered review activity = %v, %v", observed, err)
	}
	cohort, err := LoadPendingReviews(repo, []string{root})
	if err != nil || len(cohort.Subjects) != 2 || !pendingReviewPlanEqual(cohort.Plan, plan) {
		t.Fatalf("recovered pending cohort = %+v, %v", cohort, err)
	}
}
