package tasks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
)

func pendingReviewTestCompletion(t *testing.T) (repo, root, base string, assignment TaskAssignment, done Item, plan PendingReviewPlan) {
	t.Helper()
	repo = initRepo(t)
	root = filepath.Join(repo, TasksRoot)
	taskWithCompletedChecklist(t, root, StateTodo, "review-me")
	var err error
	assignment, err = AssignLoopTaskOnly([]string{root}, testLeaseOwner(), "")
	if lease := assignment.Lease; lease != nil {
		t.Cleanup(func() {
			if err := lease.Release(); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil || assignment.Lease == nil {
		t.Fatalf("assign = %+v, %v", assignment, err)
	}
	if _, err := EnsureTaskInstance(root, assignment.Task.Item); err != nil {
		t.Fatal(err)
	}
	assignment.Lease.Quiesce()
	base = forkspace.GitOut(repo, "rev-parse", "HEAD")
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

func TestPendingReviewBranchReadErrorsAreNotBranchLoss(t *testing.T) {
	repo, root, base, assignment, _, plan := pendingReviewTestCompletion(t)
	t.Cleanup(func() { _ = assignment.Lease.Release() })
	configPath := filepath.Join(repo, ".git", "config")
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	git(t, repo, "config", "extensions.refStorage", "reftable")
	if _, err := NewPendingReviewPlan(repo, []string{root}, base, "abc123", "coop loop", plan.Signoff, 1, false, PendingReviewStage{}, false); err == nil || !strings.Contains(err.Error(), "reftable") {
		t.Fatalf("capture replaced the actual Git failure with branch loss: %v", err)
	}
	if err := pendingReviewPlanMatches(repo, []string{root}, plan); err == nil || !strings.Contains(err.Error(), "reftable") {
		t.Fatalf("resume replaced the actual Git failure with branch drift: %v", err)
	}
	// Git 2.55 refuses discovery of this invalid repository even for config cleanup.
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "checkout", "--detach", "-q", "HEAD")
	if _, err := NewPendingReviewPlan(repo, []string{root}, base, "abc123", "coop loop", plan.Signoff, 1, false, PendingReviewStage{}, false); err == nil || !strings.Contains(err.Error(), "no checked-out branch") {
		t.Fatalf("detached HEAD refusal changed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("invalid HEAD\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPendingReviewPlan(repo, []string{root}, base, "abc123", "coop loop", plan.Signoff, 1, false, PendingReviewStage{}, false); err == nil || !strings.Contains(err.Error(), "git symbolic-ref") {
		t.Fatalf("capture discarded the Git command failure: %v", err)
	}
}

// The lease registry is host-wide: another queue's review records, however many, must not fail
// this queue's load (tests once left 2,236 of them in a real registry, on course for the bound),
// while this queue's own records still count toward it.
func TestLoadPendingReviewsBoundsOnlyTheSelectedQueues(t *testing.T) {
	registry := t.TempDir()
	t.Setenv(TestLeaseAuthorityRootEnv, registry)
	repo, root, _, assignment, done, plan := pendingReviewTestCompletion(t)
	if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	foreign := strings.Repeat("f", 64) + "-"
	for i := range pendingReviewScanLimit + 1 {
		writeTaskFile(t, filepath.Join(registry, fmt.Sprintf("%s%064x%s", foreign, i, pendingReviewFileSuffix)), "{}")
	}
	cohort, err := LoadPendingReviews(repo, []string{root})
	if err != nil {
		t.Fatalf("another queue's records failed the load: %v", err)
	}
	if len(cohort.Subjects) != 1 || cohort.Subjects[0].Task.Ref.ID != "review-me" {
		t.Fatalf("cohort = %+v, want this queue's one subject", cohort)
	}

	canonical, err := canonicalTaskRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	own, err := pendingReviewRecordPrefix(canonical)
	if err != nil {
		t.Fatal(err)
	}
	for i := range pendingReviewScanLimit + 1 {
		writeTaskFile(t, filepath.Join(registry, fmt.Sprintf("%s%064x%s", own, i, pendingReviewFileSuffix)), "{}")
	}
	if _, err := LoadPendingReviews(repo, []string{root}); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %v, want this queue's own records still bounded", err)
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
	if err := CompleteTrustedTask("", root, subject); err != nil {
		t.Fatal(err)
	}
	foreign := taskWithCompletedChecklist(t, root, StateTodo, "foreign-completion")
	base := forkspace.GitOut(repo, "rev-parse", "HEAD")
	plan := pendingReviewTestPlan(t, repo, root, base)
	windows, err := BeginReviewCompletionWindowsWithPending([]string{root}, []string{subject.ID}, plan)
	if err != nil {
		t.Fatal(err)
	}
	writeTaskFile(t, filepath.Join(repo, "foreign.txt"), "completed elsewhere\n")
	git(t, repo, "add", "foreign.txt")
	git(t, repo, "commit", "-qm", "finish foreign task\n\nCoop-Task: "+foreign.ID)
	if err := CompleteTrustedTask("", root, foreign); err != nil {
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
	if err := CompleteTrustedTask("", root, subject); err != nil {
		t.Fatal(err)
	}
	foreign := taskWithCompletedChecklist(t, root, StateTodo, "legacy-concurrent")
	assignment, err := AssignLoopTaskOnly([]string{root}, testLeaseOwner(), foreign.ID)
	if err != nil || assignment.Lease == nil {
		t.Fatalf("assign = %+v, %v", assignment, err)
	}
	assignment.Lease.Quiesce()
	base := forkspace.GitOut(repo, "rev-parse", "HEAD")
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

// earlyHostTask is a task another session committed before the loop started: its commit predates
// the run base the returned plan records.
func earlyHostTask(t *testing.T, commits int) (repo, root string, host Item, plan PendingReviewPlan) {
	t.Helper()
	repo = initRepo(t)
	root = filepath.Join(repo, TasksRoot)
	host = taskWithCompletedChecklist(t, root, StateTodo, "early-host")
	for i := range commits {
		writeTaskFile(t, filepath.Join(repo, "early.txt"), fmt.Sprintf("committed before the run %d\n", i))
		git(t, repo, "add", "early.txt")
		git(t, repo, "commit", "-qm", "early host work\n\nCoop-Task: "+host.ID)
	}
	writeTaskFile(t, filepath.Join(repo, "start.txt"), "the run starts here\n")
	git(t, repo, "add", "start.txt")
	git(t, repo, "commit", "-qm", "run start")
	return repo, root, host, pendingReviewTestPlan(t, repo, root, forkspace.GitOut(repo, "rev-parse", "HEAD"))
}

// Another session committed its task before the loop started and completes it while a review runs,
// between reviews, or before the controller dies with the review window open. The completion is
// host-receipted, so the review takes the task's one commit in the branch, anchored just before it
// the way --review-task anchors an import, instead of refusing the completion.
func TestPendingReviewAnchorsAHostCompletionFromBeforeTheRun(t *testing.T) {
	requireAnchored := func(t *testing.T, repo, root string, host Item) {
		t.Helper()
		anchor := forkspace.GitOut(repo, "rev-parse", "HEAD~2")
		record, ok, err := readPendingReviewRecord(root, host.ID)
		if err != nil || !ok || record.Base != anchor {
			t.Fatalf("host completion record = %+v, ok %v, %v; want it anchored at %s", record, ok, err, anchor)
		}
		cohort, err := LoadPendingReviews(repo, []string{root})
		if err != nil || len(cohort.Subjects) != 1 || cohort.Subjects[0].Task.Ref.ID != host.ID {
			t.Fatalf("cohort = %+v, %v", cohort, err)
		}
		if err := BeginPendingReviewRound([]string{root}, []string{host.ID}, 1); err != nil {
			t.Fatal(err)
		}
		round, _, _ := readPendingReviewRecord(root, host.ID)
		if err := ClearPendingReviews([]string{root}, []PendingReviewRecord{round}); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := readPendingReviewRecord(root, host.ID); err != nil || ok {
			t.Fatalf("reviewed host completion still pending = %v, %v", ok, err)
		}
	}

	reviewing := func(t *testing.T, root string) []string {
		t.Helper()
		subject := taskWithCompletedChecklist(t, root, StateDone, "review-subject")
		if err := CompleteTrustedTask("", root, subject); err != nil {
			t.Fatal(err)
		}
		return []string{subject.ID}
	}

	t.Run("during a review", func(t *testing.T) {
		repo, root, host, plan := earlyHostTask(t, 1)
		windows, err := BeginReviewCompletionWindowsWithPending([]string{root}, reviewing(t, root), plan)
		if err != nil {
			t.Fatal(err)
		}
		if err := CompleteTrustedTask("", root, host); err != nil {
			t.Fatalf("host completion during the review = %v", err)
		}
		if concurrent, err := windows.FinishReview(); err != nil || len(concurrent) != 1 || concurrent[0] != host.ID {
			t.Fatalf("finish review = %v, %v", concurrent, err)
		}
		requireAnchored(t, repo, root, host)
	})

	t.Run("between reviews", func(t *testing.T) {
		repo, root, host, plan := earlyHostTask(t, 1)
		if err := CompleteTrustedTask("", root, host); err != nil {
			t.Fatal(err)
		}
		if err := EnrollExistingPendingReviews(repo, []string{root}, []string{host.ID}, plan); err != nil {
			t.Fatalf("enroll the host completion = %v", err)
		}
		requireAnchored(t, repo, root, host)
	})

	t.Run("window replay after the controller died", func(t *testing.T) {
		repo, root, host, plan := earlyHostTask(t, 1)
		subjects := reviewing(t, root)
		assignment, err := AssignLoopTaskOnly([]string{root}, testLeaseOwner(), host.ID)
		if err != nil || assignment.Lease == nil {
			t.Fatalf("assign = %+v, %v", assignment, err)
		}
		assignment.Lease.Quiesce()
		windows, err := BeginReviewCompletionWindowsWithPending([]string{root}, subjects, plan)
		if err != nil {
			t.Fatal(err)
		}
		if err := MoveTaskDir(root, assignment.Task.Item, StateDone); err != nil {
			t.Fatal(err)
		}
		done, _ := mustCurrentTask(t, root, host.ID)
		if err := FinalizeQueuedCompletion(QueuedTask{Root: root, Item: done}); err != nil {
			t.Fatal(err)
		}
		// A receipt from a host path that publishes no review debt, as an older build's did.
		if err := writeLeaseCompletionReceipt(assignment.Lease.authority, done.Dir, ""); err != nil {
			t.Fatal(err)
		}
		if err := assignment.Lease.Release(); err != nil {
			t.Fatal(err)
		}
		if err := windows.Abandon(); err != nil {
			t.Fatal(err)
		}
		if replayed, err := ReconcileCompletionWindowsWithActivity([]string{root}); err != nil || len(replayed) != 1 || replayed[0] != host.ID {
			t.Fatalf("window replay = %v, %v", replayed, err)
		}
		requireAnchored(t, repo, root, host)
	})
}

// Zero or several commits naming the task anywhere in the branch leave nothing to anchor: the
// completion is refused and names the count.
func TestPendingReviewRefusesAHostCompletionWithoutOneCommit(t *testing.T) {
	for _, commits := range []int{0, 2} {
		t.Run(fmt.Sprintf("%d commits", commits), func(t *testing.T) {
			repo, root, host, plan := earlyHostTask(t, commits)
			if err := CompleteTrustedTask("", root, host); err != nil {
				t.Fatal(err)
			}
			err := EnrollExistingPendingReviews(repo, []string{root}, []string{host.ID}, plan)
			var missing *PendingReviewCommitsError
			if !errors.As(err, &missing) || missing.Found != commits || missing.Base != "" ||
				!strings.Contains(err.Error(), fmt.Sprintf("in this branch; found %d", commits)) {
				t.Fatalf("enroll = %v, want a refusal that found %d commits in the branch", err, commits)
			}
			if _, ok, err := readPendingReviewRecord(root, host.ID); err != nil || ok {
				t.Fatalf("refused completion left review debt = %v, %v", ok, err)
			}
		})
	}
}

// A box can plant a commit naming a task it does not hold; the completion check tolerates that with a
// warning. When the human later completes the task, the review must not bind the planted commit in
// the run's range while the human's own commit from before the run goes unreviewed: the trailer must
// name exactly one commit in the whole branch.
func TestPendingReviewRefusesAHostCompletionBesideAPlantedCommit(t *testing.T) {
	repo, root, host, plan := earlyHostTask(t, 1)
	writeTaskFile(t, filepath.Join(repo, "planted.txt"), "a box's commit naming the task\n")
	git(t, repo, "add", "planted.txt")
	git(t, repo, "commit", "-qm", "unrelated work\n\nCoop-Task: "+host.ID)
	if err := CompleteTrustedTask("", root, host); err != nil {
		t.Fatal(err)
	}
	err := EnrollExistingPendingReviews(repo, []string{root}, []string{host.ID}, plan)
	var duplicate *PendingReviewCommitsError
	if !errors.As(err, &duplicate) || duplicate.Found != 2 || duplicate.Base != "" ||
		duplicate.Remedy() != "Drop the extra commit or squash them" {
		t.Fatalf("enroll beside a planted commit = %v, want a refusal that found 2 in the branch", err)
	}
	// Only the person can tell the planted commit from their own, so the refusal names both.
	planted, own := forkspace.GitOut(repo, "rev-parse", "--short", "HEAD"), forkspace.GitOut(repo, "rev-parse", "--short", "HEAD~2")
	if len(duplicate.Commits) != 2 || !strings.Contains(duplicate.Cause(), planted+" by t <t@t>") ||
		!strings.Contains(duplicate.Cause(), own+" by t <t@t>") {
		t.Fatalf("refusal = %q, want both commits named with their author", duplicate.Cause())
	}
	if _, ok, err := readPendingReviewRecord(root, host.ID); err != nil || ok {
		t.Fatalf("refused completion left review debt = %v, %v", ok, err)
	}
}

func TestPendingReviewExplicitImportIsExactAndIdempotent(t *testing.T) {
	repo := initRepo(t)
	root := filepath.Join(repo, TasksRoot)
	item := taskWithCompletedChecklist(t, root, StateTodo, "legacy-import")
	base := forkspace.GitOut(repo, "rev-parse", "HEAD")
	writeTaskFile(t, filepath.Join(repo, "import.txt"), "legacy completion\n")
	git(t, repo, "add", "import.txt")
	git(t, repo, "commit", "-qm", "finish legacy import\n\nCoop-Task: "+item.ID)
	if err := CompleteTrustedTask("", root, item); err != nil {
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

// A pending final review that straddles a reboot recorded a device the volume no longer has. It
// used to fail with "no matching completion receipt"; it must find its completion — whether its tree
// was recorded in this build's format or the device-hashing one before it — and still notice a real
// change to the archive.
func TestPendingReviewSurvivesADeviceRenumber(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		repo, root, _, assignment, done, plan := pendingReviewTestCompletion(t)
		if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
			t.Fatal(err)
		}
		if err := assignment.Lease.Release(); err != nil {
			t.Fatal(err)
		}
		record, ok, err := readPendingReviewRecord(root, done.ID)
		if err != nil || !ok {
			t.Fatalf("pending review record = %v, %v", ok, err)
		}
		record.Task.Generation.Device++
		record.Fingerprint.Device = record.Task.Generation.Device
		if legacy {
			live, err := CompletionFingerprintFor(root, done)
			if err != nil {
				t.Fatal(err)
			}
			record.Fingerprint.TreeFormat = 0
			record.Fingerprint.Tree = legacyCompletionTreeDigest(live.walk, record.Fingerprint.Device)
		}
		if err := writePendingReviewRecord(root, record); err != nil {
			t.Fatal(err)
		}
		if cohort, err := LoadPendingReviews(repo, []string{root}); err != nil || len(cohort.Subjects) != 1 {
			t.Fatalf("pending review after a reboot (legacy tree %v) = %+v, %v", legacy, cohort, err)
		}
		writeTaskFile(t, filepath.Join(done.Dir, "log.md"), "tampered after the reboot\n")
		if _, err := LoadPendingReviews(repo, []string{root}); err == nil || !strings.Contains(err.Error(), "archive changed") {
			t.Fatalf("archive mutation after a reboot (legacy tree %v) = %v", legacy, err)
		}
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

	t.Run("dropped commit", func(t *testing.T) {
		repo, root, base, assignment, done, plan := pendingReviewTestCompletion(t)
		if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
			t.Fatal(err)
		}
		if err := assignment.Lease.Release(); err != nil {
			t.Fatal(err)
		}
		git(t, repo, "reset", "--hard", base)
		_, err := LoadPendingReviews(repo, []string{root})
		var missing *PendingReviewCommitsError
		if !errors.As(err, &missing) || missing.Found != 0 || missing.Base != base ||
			!strings.Contains(err.Error(), "after "+base[:12]+", where this run started; found 0") {
			t.Fatalf("dropped commit error = %v", err)
		}
		if _, ok, err := readPendingReviewRecord(root, done.ID); err != nil || !ok {
			t.Fatalf("dropped commit discarded the debt = %v, %v", ok, err)
		}
	})

	// A signoff verdict covers the content it reviewed. Different content under the same trailer,
	// from a later attempt's amend or a rewrite while the loop was stopped, is never cleared by
	// verification: loading sends the task back to signoff.
	t.Run("history", func(t *testing.T) {
		repo, root, base, assignment, done, plan := pendingReviewTestCompletion(t)
		if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
			t.Fatal(err)
		}
		if err := assignment.Lease.Release(); err != nil {
			t.Fatal(err)
		}
		if err := BeginPendingReviewRound([]string{root}, []string{done.ID}, 1); err != nil {
			t.Fatal(err)
		}
		if err := MarkPendingReviewVerify([]string{root}, []string{done.ID}); err != nil {
			t.Fatal(err)
		}
		accepted, _, _ := readPendingReviewRecord(root, done.ID)
		git(t, repo, "reset", "--hard", base)
		writeTaskFile(t, filepath.Join(repo, "change.txt"), "never reviewed\n")
		git(t, repo, "add", "change.txt")
		git(t, repo, "commit", "-qm", "different review subject\n\nCoop-Task: review-me")
		cohort, err := LoadPendingReviews(repo, []string{root})
		if err != nil || len(cohort.Subjects) != 1 || cohort.Subjects[0].Phase != PendingReviewSignoff ||
			cohort.Subjects[0].RoundStarted || cohort.Subjects[0].Reviewed != nil {
			t.Fatalf("changed accepted commit = %+v, %v; want it back in signoff", cohort, err)
		}
		if err := ClearPendingReviews([]string{root}, []PendingReviewRecord{accepted}); err == nil {
			t.Fatal("the verdict taken before the change cleared the task")
		}
		if record, ok, err := readPendingReviewRecord(root, done.ID); err != nil || !ok || record.Phase != PendingReviewSignoff {
			t.Fatalf("changed accepted commit's debt = %+v, ok %v, %v", record, ok, err)
		}
	})
}

// resignSince rewrites every commit after base the way a host signing pass does: same trees, authors
// and messages, a new committer date, so every commit gets a new ID.
func resignSince(t *testing.T, repo, base, date string) {
	t.Helper()
	cmd := exec.Command("git", "-C", repo, "rebase", "-f", "--quiet", "--no-gpg-sign", base)
	cmd.Env = append(os.Environ(), "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_COMMITTER_DATE="+date)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("re-sign since %s: %v\n%s", base, err, out)
	}
}

// A signing pass, Coop's or anyone's, replaces every commit ID after the first unsigned commit. The
// review still finds the task's commit by its trailer, and the verdict of a round that started
// before the pass still covers it: signing keeps what the commit does.
func TestPendingReviewFindsSignedCommitsByTrailer(t *testing.T) {
	repo, root, base, assignment, done, plan := pendingReviewTestCompletion(t)
	writeTaskFile(t, filepath.Join(repo, "later.txt"), "later unbound work\n")
	git(t, repo, "add", "later.txt")
	git(t, repo, "commit", "-qm", "later unbound work")
	if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	if err := BeginPendingReviewRound([]string{root}, []string{done.ID}, 1); err != nil {
		t.Fatal(err)
	}
	before := CommitsForTask(repo, base+"..HEAD", done.ID)
	resignSince(t, repo, base, "2031-02-03T04:05:06Z")
	after := CommitsForTask(repo, base+"..HEAD", done.ID)
	if len(before) != 1 || len(after) != 1 || before[0] == after[0] {
		t.Fatalf("signing did not replace the task commit: before %v, after %v", before, after)
	}
	cohort, err := LoadPendingReviews(repo, []string{root})
	if err != nil || len(cohort.Subjects) != 1 || cohort.Subjects[0].Task.Ref.ID != done.ID {
		t.Fatalf("cohort after signing = %+v, %v", cohort, err)
	}
	if err := ClearPendingReviews([]string{root}, cohort.Subjects); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := readPendingReviewRecord(root, done.ID); err != nil || ok {
		t.Fatalf("record after clear = ok %v, err %v", ok, err)
	}
}

// Commit messages are box-written, so the review counts a task's trailers only in base..HEAD as git
// reads it, even after the run replaced the base commit itself (the recovery recipe amends it), and a
// second one in the range keeps the debt and starts no reviewer. A trailer the base already had makes
// a new enrollment refuse instead (TestPendingReviewRefusesAHostCompletionBesideAPlantedCommit).
func TestPendingReviewSearchesOnlyTheRecordedRange(t *testing.T) {
	t.Run("the run's start commit can be replaced", func(t *testing.T) {
		repo := initRepo(t)
		root := filepath.Join(repo, TasksRoot)
		item := taskWithCompletedChecklist(t, root, StateTodo, "range-subject")
		writeTaskFile(t, filepath.Join(repo, "old.txt"), "older work\n")
		git(t, repo, "add", "old.txt")
		git(t, repo, "commit", "-qm", "older work")
		older := forkspace.GitOut(repo, "rev-parse", "HEAD")
		writeTaskFile(t, filepath.Join(repo, "start.txt"), "the run starts here\n")
		git(t, repo, "add", "start.txt")
		git(t, repo, "commit", "-qm", "run start")
		base := forkspace.GitOut(repo, "rev-parse", "HEAD")
		writeTaskFile(t, filepath.Join(repo, "range.txt"), "the task's work\n")
		git(t, repo, "add", "range.txt")
		git(t, repo, "commit", "-qm", "the task's work\n\nCoop-Task: "+item.ID)
		if err := CompleteTrustedTask("", root, item); err != nil {
			t.Fatal(err)
		}
		plan := pendingReviewTestPlan(t, repo, root, base)
		if err := EnrollExistingPendingReviews(repo, []string{root}, []string{item.ID}, plan); err != nil {
			t.Fatal(err)
		}
		if cohort, err := LoadPendingReviews(repo, []string{root}); err != nil || len(cohort.Subjects) != 1 {
			t.Fatalf("cohort = %+v, %v", cohort, err)
		}

		resignSince(t, repo, older, "2032-03-04T05:06:07Z")
		if forkspace.GitOut(repo, "rev-parse", "HEAD~1") == base {
			t.Fatal("the rewrite kept the recorded start commit")
		}
		if cohort, err := LoadPendingReviews(repo, []string{root}); err != nil || len(cohort.Subjects) != 1 {
			t.Fatalf("cohort after the start commit was replaced = %+v, %v", cohort, err)
		}
	})

	t.Run("a second commit in the range", func(t *testing.T) {
		repo, root, _, assignment, done, plan := pendingReviewTestCompletion(t)
		if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
			t.Fatal(err)
		}
		if err := assignment.Lease.Release(); err != nil {
			t.Fatal(err)
		}
		writeTaskFile(t, filepath.Join(repo, "forged.txt"), "a second commit naming the task\n")
		git(t, repo, "add", "forged.txt")
		git(t, repo, "commit", "-qm", "second binding\n\nCoop-Task: "+done.ID)
		if _, err := LoadPendingReviews(repo, []string{root}); err == nil || !strings.Contains(err.Error(), "found 2") {
			t.Fatalf("duplicate binding error = %v", err)
		}
		if _, ok, err := readPendingReviewRecord(root, done.ID); err != nil || !ok {
			t.Fatalf("duplicate binding discarded the debt = %v, %v", ok, err)
		}
	})
}

// Version 1 records also pinned commit IDs. A build that drops them still resumes the review those
// records describe, and its next write stores version 2.
func TestPendingReviewReadsVersion1Records(t *testing.T) {
	repo, root, _, assignment, done, plan := pendingReviewTestCompletion(t)
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
	record.Prepared = true
	record.Previous = &PendingReviewPrevious{Task: record.Task, Fingerprint: record.Fingerprint, Phase: PendingReviewReopened, Round: 1}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]any
	if err := json.Unmarshal(data, &legacy); err != nil {
		t.Fatal(err)
	}
	binding := map[string]any{"head": strings.Repeat("a", 40), "raw": []string{strings.Repeat("a", 40)}, "subject": map[string]any{}, "history": []any{}}
	legacy["version"], legacy["binding"] = pendingReviewLegacyVersion, binding
	legacy["previous"].(map[string]any)["binding"] = binding
	name, err := pendingReviewRecordName(root, done.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := OpenLeaseAuthorityRoot()
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	if err := AtomicWriteTaskFile(registry, name, append(data, '\n')); err != nil {
		t.Fatal(err)
	}

	got, ok, err := readPendingReviewRecord(root, done.ID)
	if err != nil || !ok || got.Version != pendingReviewVersion || got.Previous == nil ||
		got.Previous.Phase != PendingReviewReopened || !pendingReviewPlanEqual(got.Plan, plan) {
		t.Fatalf("version 1 record = %+v, ok=%v, err=%v", got, ok, err)
	}
	cohort, err := LoadPendingReviews(repo, []string{root})
	if err != nil || len(cohort.Subjects) != 1 || cohort.Subjects[0].Prepared {
		t.Fatalf("version 1 cohort = %+v, %v", cohort, err)
	}
	stored, err := ReadTaskMetadataFile(registry, name)
	if err != nil || !strings.Contains(string(stored), `"version":2`) || strings.Contains(string(stored), `"binding"`) {
		t.Fatalf("promoted record kept the version 1 shape: %s, %v", stored, err)
	}
}

// Version 1 pinned a task's commit wherever it was, so a host completion committed before its run
// base was reviewable. After the upgrade its record has no Base; loading anchors it once, as a new
// enrollment would, instead of refusing every start with "found 0" for a commit that exists.
func TestPendingReviewAnchorsAVersion1RecordFromBeforeTheRun(t *testing.T) {
	repo, root, host, plan := earlyHostTask(t, 1)
	anchor := forkspace.GitOut(repo, "rev-parse", "HEAD~2")
	if err := CompleteTrustedTask("", root, host); err != nil {
		t.Fatal(err)
	}
	if err := EnrollExistingPendingReviews(repo, []string{root}, []string{host.ID}, plan); err != nil {
		t.Fatal(err)
	}
	record, _, err := readPendingReviewRecord(root, host.ID)
	if err != nil {
		t.Fatal(err)
	}
	record.Base = ""
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]any
	if err := json.Unmarshal(data, &legacy); err != nil {
		t.Fatal(err)
	}
	legacy["version"] = pendingReviewLegacyVersion
	legacy["binding"] = map[string]any{"head": strings.Repeat("a", 40), "raw": []string{strings.Repeat("a", 40)}, "subject": map[string]any{}, "history": []any{}}
	if data, err = json.Marshal(legacy); err != nil {
		t.Fatal(err)
	}
	name, err := pendingReviewRecordName(root, host.ID)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := OpenLeaseAuthorityRoot()
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	if err := AtomicWriteTaskFile(registry, name, append(data, '\n')); err != nil {
		t.Fatal(err)
	}

	cohort, err := LoadPendingReviews(repo, []string{root})
	if err != nil || len(cohort.Subjects) != 1 || cohort.Subjects[0].Base != anchor {
		t.Fatalf("version 1 record from before the run = %+v, %v; want it anchored at %s", cohort, err, anchor)
	}
	if stored, _, err := readPendingReviewRecord(root, host.ID); err != nil || stored.Base != anchor {
		t.Fatalf("stored record after the load = %+v, %v; want the anchor saved", stored, err)
	}
	if again, err := LoadPendingReviews(repo, []string{root}); err != nil || len(again.Subjects) != 1 {
		t.Fatalf("second load = %+v, %v", again, err)
	}
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
		{name: "phase", mutate: func(previous *PendingReviewPrevious) { previous.Phase = "accepted" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := record
			candidate.Prepared = true
			candidate.Previous = &PendingReviewPrevious{
				Task: record.Task, Fingerprint: record.Fingerprint,
				Phase: record.Phase, Round: record.Round, RoundStarted: record.RoundStarted,
			}
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
		Task: prior.Task, Fingerprint: prior.Fingerprint,
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
	// An explicit import of the reviewed task asks for another review; nothing remembers the first.
	if err := EnrollExistingPendingReviews(repo, []string{root}, []string{done.ID}, plan); err != nil {
		t.Fatalf("repeat import = %v", err)
	}
	if cohort, err := LoadPendingReviews(repo, []string{root}); err != nil || len(cohort.Subjects) != 1 {
		t.Fatalf("repeat import cohort = %+v, %v", cohort, err)
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
		Task: prior.Task, Fingerprint: prior.Fingerprint,
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
	if err != nil || !ok || restored.Prepared || restored.Previous != nil || restored.Phase != PendingReviewReopened || restored.Round != 2 || !prior.Fingerprint.Matches(restored.Fingerprint) {
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

// A field's bytes must never pass for a boundary. Git stores an author line with a NUL in it, so two
// different histories, one hiding a change in an author's email, must fingerprint differently (the
// re-review's proof of concept, fed through the same author parser). Length prefixes do that, so
// such an author line, or an escape a plain `git commit` stores, still reads like any other.
func TestPendingReviewDigestKeepsFieldsApart(t *testing.T) {
	change := func(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
	at := func(unix int64) string { return time.Unix(unix, 0).In(time.FixedZone("", 0)).Format(time.RFC3339) }
	window := func(commits ...[3]string) pendingReviewHistory {
		var history pendingReviewHistory
		for _, commit := range commits {
			name, email, date, err := auditAuthorIdentity(commit[1])
			if err != nil {
				t.Fatal(err)
			}
			history.commits = append(history.commits, rawAuditCommit{authorName: name, authorEmail: email, authorDate: date, commitMessage: commit[2]})
			history.changes = append(history.changes, commit[0])
		}
		return history
	}
	subject := [3]string{change("subject diff"), "Worker <w@x> 1760000000 +0000", "Add thing\n\nCoop-Task: X\n"}
	evil, repair, tidy := change("EVIL diff"), change("repair-1 diff"), change("repair-2 diff")
	reviewed := window(subject,
		[3]string{repair, "A <a@x\x00" + at(1760000100) + "\x00" + "3\x00fix" + evil + "> 1760000200 +0000", "fix typo"},
		[3]string{tidy, "B <b@x> 1760000300 +0000", "tidy"})
	swapped := window(subject,
		[3]string{repair, "A <a@x> 1760000100 +0000", "fix"},
		[3]string{evil, at(1760000200) + " <8\x00fix typo" + tidy + "\x00B\x00b@x> 1760000300 +0000", "tidy"})
	if reviewed.digest(3) == swapped.digest(3) {
		t.Fatal("two different histories have the same fingerprint")
	}

	repo := initRepo(t)
	base := forkspace.GitOut(repo, "rev-parse", "HEAD")
	raw := "tree " + forkspace.GitOut(repo, "rev-parse", "HEAD^{tree}") + "\nparent " + base +
		"\nauthor A <a@x\x00junk> 1760000000 +0000\ncommitter A <a@x> 1760000000 +0000\n\nsubject\n"
	store := exec.Command("git", "-C", repo, "hash-object", "-t", "commit", "-w", "--literally", "--stdin")
	store.Stdin = strings.NewReader(raw)
	out, err := store.Output()
	if err != nil {
		t.Fatalf("store a commit with a NUL in its author: %v", err)
	}
	git(t, repo, "reset", "-q", "--hard", strings.TrimSpace(string(out)))
	writeTaskFile(t, filepath.Join(repo, "box.txt"), "box work\n")
	git(t, repo, "add", "box.txt")
	escaped := exec.Command("git", "-C", repo, "commit", "-qm", "box work")
	escaped.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Bo\x1b[31mx", "GIT_AUTHOR_EMAIL=b@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := escaped.CombinedOutput(); err != nil {
		t.Fatalf("commit with an escape in its author: %v\n%s", err, out)
	}
	if window, err := readPendingReviewHistory(repo, base); err != nil || len(window.commits) != 2 {
		t.Fatalf("window with unusual author lines = %d commits, %v; want both read", len(window.commits), err)
	}
}

// A box can plant any number of commits with one task's trailer; a refusal lists the first five and
// says how many more.
func TestDescribeTaskCommitsListsFiveAndCountsTheRest(t *testing.T) {
	repo := initRepo(t)
	var shas []string
	for i := range 7 {
		git(t, repo, "commit", "-q", "--allow-empty", "-m", fmt.Sprintf("planted %d", i))
		shas = append(shas, forkspace.GitOut(repo, "rev-parse", "HEAD"))
	}
	got := describeTaskCommits(repo, shas)
	if len(got) != 6 || got[5] != "and 2 more" {
		t.Fatalf("describeTaskCommits = %q; want five commits and \"and 2 more\"", got)
	}
}

// A box sets its own author name, so the commits a refusal lists show anything a terminal would act
// on or a reader could not see as '?'.
func TestDescribeTaskCommitsEscapesAuthors(t *testing.T) {
	repo := initRepo(t)
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "own work")
	escaped := exec.Command("git", "-C", repo, "commit", "-q", "--allow-empty", "-m", "box work")
	escaped.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Bo\x1b[31mx\u202Ey\u2028z", "GIT_AUTHOR_EMAIL=b@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := escaped.CombinedOutput(); err != nil {
		t.Fatalf("commit with an escape in its author: %v\n%s", err, out)
	}
	got := describeTaskCommits(repo, []string{forkspace.GitOut(repo, "rev-parse", "HEAD~1"), forkspace.GitOut(repo, "rev-parse", "HEAD")})
	if len(got) != 2 || !strings.HasSuffix(got[0], " by t <t@t>") || !strings.HasSuffix(got[1], " by Bo?[31mx?y?z <b@x>") {
		t.Fatalf("described commits = %q, want the escape, the override and the line separator shown as ?", got)
	}
}

// markHidden shows each character a terminal would act on or a reader could not see, and invalid
// UTF-8, as '?', and leaves an ordinary author line as it is.
func TestMarkHidden(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"ordinary author line", "1a2b3c4 by Zoë Ngô 日本 한국 🙂 👍🏽 <z@x>", "1a2b3c4 by Zoë Ngô 日本 한국 🙂 👍🏽 <z@x>"},
		{"escape, new line and tab", "a\x1b[31mb\nc\td", "a?[31mb?c?d"},
		{"C1 controls and DEL", "a\u009Bb\u0085c\x7f", "a?b?c?"},
		{"direction marks, overrides and isolates", "a\u061Cb\u200Fc\u202Ed\u2066e", "a?b?c?d?e"},
		{"zero-width space and joiner", "w\u200Be\u200Db", "w?e?b"},
		{"byte-order mark and soft hyphen", "\uFEFFa\u00ADb", "?a?b"},
		{"tag characters", "a\U000E0041b\U000E0000", "a?b?"},
		{"line and paragraph separators", "a\u2028b\u2029c", "a?b?c"},
		{"Hangul filler and grapheme joiner", "a\u1160b\u034Fc", "a?b?c"},
		{"variation selector at the start and after an ASCII letter", "\uFE0Ea\uFE0Fb", "?a?b"},
		{"emoji and ideograph variation selectors", "❤\uFE0F 葛\U000E0100", "❤\uFE0F 葛\U000E0100"},
		{"invalid UTF-8 and the replacement character", "a\xffb\uFFFD", "a?b?"},
	} {
		if got := markHidden(tc.in); got != tc.want {
			t.Errorf("%s: markHidden(%+q) = %+q, want %+q", tc.name, tc.in, got, tc.want)
		}
	}
}

// Every change in a window is a diff against its parent. Dropping the commit the window starts from,
// with `git rebase --onto base~1 base`, keeps every diff and changes HEAD's tree, so it must send an
// accepted task back to signoff too.
func TestPendingReviewVerdictCoversTheTreeTheRunStartsFrom(t *testing.T) {
	repo := initRepo(t)
	root := filepath.Join(repo, TasksRoot)
	writeTaskFile(t, filepath.Join(repo, "security.txt"), "fixed\n")
	git(t, repo, "add", "security.txt")
	git(t, repo, "commit", "-qm", "security fix")
	base := forkspace.GitOut(repo, "rev-parse", "HEAD")
	item := taskWithCompletedChecklist(t, root, StateTodo, "start-subject")
	writeTaskFile(t, filepath.Join(repo, "change.txt"), "the task's work\n")
	git(t, repo, "add", "change.txt")
	git(t, repo, "commit", "-qm", "the task's work\n\nCoop-Task: "+item.ID)
	if err := CompleteTrustedTask("", root, item); err != nil {
		t.Fatal(err)
	}
	plan, err := NewPendingReviewPlan(repo, []string{root}, base, "abc123", "coop loop",
		PendingReviewStage{Targets: []string{"codex:test@personal"}, Prompt: "review", Writes: "tasks"}, 5,
		true, PendingReviewStage{Targets: []string{"claude:test@personal"}, Prompt: "verify", Writes: "tasks"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := EnrollExistingPendingReviews(repo, []string{root}, []string{item.ID}, plan); err != nil {
		t.Fatal(err)
	}
	if err := BeginPendingReviewRound([]string{root}, []string{item.ID}, 1); err != nil {
		t.Fatal(err)
	}
	if err := MarkPendingReviewVerify([]string{root}, []string{item.ID}); err != nil {
		t.Fatal(err)
	}
	accepted, _, _ := readPendingReviewRecord(root, item.ID)
	tree := forkspace.GitOut(repo, "rev-parse", "HEAD^{tree}")
	git(t, repo, "rebase", "-q", "--onto", base+"~1", base)
	if forkspace.GitOut(repo, "rev-parse", "HEAD^{tree}") == tree {
		t.Fatal("dropping the run's start commit kept HEAD's tree")
	}
	cohort, err := LoadPendingReviews(repo, []string{root})
	if err != nil || len(cohort.Subjects) != 1 || cohort.Subjects[0].Phase != PendingReviewSignoff {
		t.Fatalf("cohort after the run's start commit was dropped = %+v, %v; want the task back in signoff", cohort, err)
	}
	if err := ClearPendingReviews([]string{root}, []PendingReviewRecord{accepted}); err == nil {
		t.Fatal("the verdict taken before the drop cleared the task")
	}
}

// A review of the run sees every commit after its base, not only the subject's own. An attempt that
// exits before any range check can rewrite an untrailered commit made earlier in the run, such as a
// host commit, keep its message, and replay the subject's commit unchanged on top; verification must
// not clear that.
func TestPendingReviewVerdictCoversTheRunBeforeTheTasksCommit(t *testing.T) {
	repo, root, base, assignment, done, plan := pendingReviewTestCompletion(t)
	if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	hostCommitBelow := func(content string) {
		t.Helper()
		subjectCommit := forkspace.GitOut(repo, "rev-parse", "HEAD")
		git(t, repo, "reset", "-q", "--hard", base)
		writeTaskFile(t, filepath.Join(repo, "host.txt"), content)
		git(t, repo, "add", "host.txt")
		git(t, repo, "commit", "-qm", "host work")
		git(t, repo, "cherry-pick", subjectCommit)
	}
	hostCommitBelow("host work\n")
	if err := BeginPendingReviewRound([]string{root}, []string{done.ID}, 1); err != nil {
		t.Fatal(err)
	}
	if err := MarkPendingReviewVerify([]string{root}, []string{done.ID}); err != nil {
		t.Fatal(err)
	}
	accepted, _, _ := readPendingReviewRecord(root, done.ID)
	hostCommitBelow("never reviewed\n")
	cohort, err := LoadPendingReviews(repo, []string{root})
	if err != nil || len(cohort.Subjects) != 1 || cohort.Subjects[0].Phase != PendingReviewSignoff {
		t.Fatalf("cohort after the earlier run commit changed = %+v, %v; want the task back in signoff", cohort, err)
	}
	if err := ClearPendingReviews([]string{root}, []PendingReviewRecord{accepted}); err == nil {
		t.Fatal("the verdict taken before the change cleared the task")
	}
}

// Audit rework adds its repair as a commit with no trailer, after the reopened task's own commit, so
// a verdict covers the commits around the task's own as well. A sibling's failed attempt that amends
// that repair, and exits before any range check, leaves the task's own commit untouched;
// verification still must not clear a repair no reviewer saw. A commit added after the round is not
// covered and changes nothing.
func TestPendingReviewVerdictCoversTheCommitsAfterTheTasksOwn(t *testing.T) {
	setup := func(t *testing.T) (repo, root string, done Item, accepted PendingReviewRecord) {
		t.Helper()
		repo, root, _, assignment, done, plan := pendingReviewTestCompletion(t)
		if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
			t.Fatal(err)
		}
		if err := assignment.Lease.Release(); err != nil {
			t.Fatal(err)
		}
		// Round 1 reopens the task; its rework appends an untrailered repair and completes again.
		if err := MarkPendingReviewReopened([]string{root}, []string{done.ID}); err != nil {
			t.Fatal(err)
		}
		if err := MoveTaskDir(root, done, StateTodo); err != nil {
			t.Fatal(err)
		}
		writeTaskFile(t, filepath.Join(repo, "change.txt"), "review me, repaired\n")
		git(t, repo, "add", "change.txt")
		git(t, repo, "commit", "-qm", "repair the review subject")
		reopened, _ := mustCurrentTask(t, root, done.ID)
		if err := CompleteTrustedTask("", root, reopened); err != nil {
			t.Fatal(err)
		}
		// Round 2 accepts the task and its repair.
		if err := BeginPendingReviewRound([]string{root}, []string{done.ID}, 2); err != nil {
			t.Fatal(err)
		}
		if err := MarkPendingReviewVerify([]string{root}, []string{done.ID}); err != nil {
			t.Fatal(err)
		}
		accepted, _, err := readPendingReviewRecord(root, done.ID)
		if err != nil || accepted.Reviewed == nil {
			t.Fatalf("accepted record = %+v, %v", accepted, err)
		}
		done, _ = mustCurrentTask(t, root, done.ID)
		return repo, root, done, accepted
	}
	requireBackInSignoff := func(t *testing.T, repo, root string, done Item, accepted PendingReviewRecord) {
		t.Helper()
		cohort, err := LoadPendingReviews(repo, []string{root})
		if err != nil || len(cohort.Subjects) != 1 || cohort.Subjects[0].Phase != PendingReviewSignoff || cohort.Subjects[0].Reviewed != nil {
			t.Fatalf("cohort after the reviewed repair changed = %+v, %v; want the task back in signoff", cohort, err)
		}
		if err := ClearPendingReviews([]string{root}, []PendingReviewRecord{accepted}); err == nil {
			t.Fatal("the verdict taken before the change cleared the task")
		}
		if record, ok, err := readPendingReviewRecord(root, done.ID); err != nil || !ok || record.Phase != PendingReviewSignoff {
			t.Fatalf("debt after the reviewed repair changed = %+v, ok %v, %v", record, ok, err)
		}
	}

	t.Run("a sibling amends the repair", func(t *testing.T) {
		repo, root, done, accepted := setup(t)
		writeTaskFile(t, filepath.Join(repo, "change.txt"), "never reviewed\n")
		git(t, repo, "add", "change.txt")
		git(t, repo, "commit", "-q", "--amend", "--no-edit", "--no-gpg-sign")
		requireBackInSignoff(t, repo, root, done, accepted)
	})

	t.Run("the repair disappears", func(t *testing.T) {
		repo, root, done, accepted := setup(t)
		git(t, repo, "reset", "-q", "--hard", "HEAD~1")
		requireBackInSignoff(t, repo, root, done, accepted)
	})

	t.Run("a later commit is not covered", func(t *testing.T) {
		repo, root, done, _ := setup(t)
		writeTaskFile(t, filepath.Join(repo, "later.txt"), "another task's work\n")
		git(t, repo, "add", "later.txt")
		git(t, repo, "commit", "-qm", "later work")
		resignSince(t, repo, forkspace.GitOut(repo, "rev-parse", "HEAD~3"), "2036-07-08T09:10:11Z")
		cohort, err := LoadPendingReviews(repo, []string{root})
		if err != nil || len(cohort.Subjects) != 1 || cohort.Subjects[0].Phase != PendingReviewVerify {
			t.Fatalf("cohort after a later commit and signing = %+v, %v; want the task still accepted", cohort, err)
		}
		if err := ClearPendingReviews([]string{root}, cohort.Subjects); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := readPendingReviewRecord(root, done.ID); err != nil || ok {
			t.Fatalf("accepted task after its verification = ok %v, %v", ok, err)
		}
	})
}

func TestPendingReviewClearChecksTheVerdictStillApplies(t *testing.T) {
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

	t.Run("commit dropped after verdict", func(t *testing.T) {
		repo, root, base, assignment, done, plan := pendingReviewTestCompletion(t)
		if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
			t.Fatal(err)
		}
		if err := assignment.Lease.Release(); err != nil {
			t.Fatal(err)
		}
		want, _, _ := readPendingReviewRecord(root, done.ID)
		git(t, repo, "reset", "--hard", base)
		if err := ClearPendingReviews([]string{root}, []PendingReviewRecord{want}); err == nil || !strings.Contains(err.Error(), "after the review verdict") {
			t.Fatalf("dropped commit clear error = %v", err)
		}
		if _, ok, err := readPendingReviewRecord(root, done.ID); err != nil || !ok {
			t.Fatalf("dropped commit removed debt = %v, %v", ok, err)
		}
	})

	t.Run("content changed during the review", func(t *testing.T) {
		repo, root, base, assignment, done, plan := pendingReviewTestCompletion(t)
		if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
			t.Fatal(err)
		}
		if err := assignment.Lease.Release(); err != nil {
			t.Fatal(err)
		}
		if err := BeginPendingReviewRound([]string{root}, []string{done.ID}, 1); err != nil {
			t.Fatal(err)
		}
		want, _, _ := readPendingReviewRecord(root, done.ID)
		git(t, repo, "reset", "--hard", base)
		writeTaskFile(t, filepath.Join(repo, "change.txt"), "changed while the reviewer read the old one\n")
		git(t, repo, "add", "change.txt")
		git(t, repo, "commit", "-qm", "implement review subject\n\nCoop-Task: review-me")
		if err := ClearPendingReviews([]string{root}, []PendingReviewRecord{want}); err != nil {
			t.Fatalf("clear = %v, want the changed task sent back to signoff", err)
		}
		if record, ok, err := readPendingReviewRecord(root, done.ID); err != nil || !ok ||
			record.Phase != PendingReviewSignoff || record.RoundStarted || record.Reviewed != nil {
			t.Fatalf("changed task after the clear = %+v, ok %v, %v", record, ok, err)
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
	if err := CompleteTrustedTask("", root, reopened); err != nil {
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

func TestPendingReviewRejectsBranchDrift(t *testing.T) {
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
}

// The 2026-09-14 Emisar run: rework of the later task in a two-task cohort replaced its commit, and
// the earlier task's review debt, which had recorded that commit, was refused. Debt names tasks, not
// commits, so a sibling's rework cannot invalidate it.
func TestPendingReviewKeepsEarlierDebtAcrossLaterTaskRework(t *testing.T) {
	repo := initRepo(t)
	root := filepath.Join(repo, TasksRoot)
	earlier := taskWithCompletedChecklist(t, root, StateTodo, "review-earlier")
	later := taskWithCompletedChecklist(t, root, StateTodo, "review-later")
	base := forkspace.GitOut(repo, "rev-parse", "HEAD")
	writeTaskFile(t, filepath.Join(repo, "earlier.txt"), "earlier\n")
	git(t, repo, "add", "earlier.txt")
	git(t, repo, "commit", "-qm", "earlier implementation\n\nCoop-Task: "+earlier.ID)
	if err := CompleteTrustedTask("", root, earlier); err != nil {
		t.Fatal(err)
	}
	writeTaskFile(t, filepath.Join(repo, "later.txt"), "later\n")
	git(t, repo, "add", "later.txt")
	git(t, repo, "commit", "-qm", "later implementation\n\nCoop-Task: "+later.ID)
	if err := CompleteTrustedTask("", root, later); err != nil {
		t.Fatal(err)
	}
	plan := pendingReviewTestPlan(t, repo, root, base)
	if err := EnrollExistingPendingReviews(repo, []string{root}, []string{earlier.ID, later.ID}, plan); err != nil {
		t.Fatal(err)
	}
	writeTaskFile(t, filepath.Join(repo, "later.txt"), "later repaired\n")
	git(t, repo, "add", "later.txt")
	git(t, repo, "commit", "--amend", "--no-edit", "--no-gpg-sign", "--quiet")
	if cohort, err := LoadPendingReviews(repo, []string{root}); err != nil || len(cohort.Subjects) != 2 {
		t.Fatalf("cohort after the later task's rework = %+v, %v", cohort, err)
	}
}

// reopenForRework turns a pending subject into reopened work the way a review verdict does: the
// reopened phase, host audit authority captured at HEAD, and the folder back in the queue.
func reopenForRework(t *testing.T, repo, root string, task Item) AuditReopenRecord {
	t.Helper()
	if err := MarkPendingReviewReopened([]string{root}, []string{task.ID}); err != nil {
		t.Fatal(err)
	}
	reopen, err := CaptureAuditReopen(repo, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteAuditReopenRecord(root, reopen); err != nil {
		t.Fatal(err)
	}
	if err := MoveTaskDir(root, task, StateTodo); err != nil {
		t.Fatal(err)
	}
	return reopen
}

// A signing pass replaces the commit IDs a reopened task's audit authority records. The pass
// carries that authority to the signed copies with its generation; a rewrite that changes what a
// recorded commit does is not carried.
func TestCarryReopenedReviewsAfterSigning(t *testing.T) {
	repo, root, base, assignment, done, plan := pendingReviewTestCompletion(t)
	writeTaskFile(t, filepath.Join(repo, "descendant.txt"), "descendant\n")
	git(t, repo, "add", "descendant.txt")
	git(t, repo, "commit", "-qm", "descendant implementation\n\nCoop-Task: descendant")
	if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	reopen := reopenForRework(t, repo, root, done)
	oldHead := forkspace.GitOut(repo, "rev-parse", "HEAD")
	resignSince(t, repo, base, "2034-05-06T07:08:09Z")
	if AuditReopenCurrentValid(repo, "HEAD", done.ID, reopen) {
		t.Fatal("the signing rewrite left the recorded commits in place")
	}
	if err := CarryReopenedReviewsAfterSigning(repo, oldHead); err != nil {
		t.Fatal(err)
	}
	head := forkspace.GitOut(repo, "rev-parse", "HEAD")
	got, ok, err := ReadAuditReopenRecord(root, done.ID)
	if err != nil || !ok || got.Generation != reopen.Generation || got.BaselineHead != head ||
		!AuditReopenCurrentValid(repo, head, done.ID, got) {
		t.Fatalf("carried authority = %+v, ok=%v, err=%v", got, ok, err)
	}
	if cohort, err := LoadPendingReviews(repo, []string{root}); err != nil || len(cohort.Subjects) != 1 ||
		cohort.Subjects[0].Phase != PendingReviewReopened {
		t.Fatalf("reopened cohort after signing = %+v, %v", cohort, err)
	}

	writeTaskFile(t, filepath.Join(repo, "descendant.txt"), "descendant changed\n")
	git(t, repo, "add", "descendant.txt")
	git(t, repo, "commit", "--amend", "--no-edit", "--no-gpg-sign", "--quiet")
	if err := CarryReopenedReviewsAfterSigning(repo, head); err != nil {
		t.Fatal(err)
	}
	if unchanged, _, err := ReadAuditReopenRecord(root, done.ID); err != nil || !AuditReopenRecordsEqual(unchanged, got) {
		t.Fatalf("a content change moved the authority = %+v, %v", unchanged, err)
	}
}

// Accepted rework of one reopened task replaces commits another reopened task's authority records:
// the earlier task's rework replays the later task's commit, and the later task's rework changes an
// entry in the earlier task's recorded history. Each sibling keeps its generation. An authority that
// was already stale before the rewrite is never carried.
func TestCarryReopenedReviewsAcrossSiblingRework(t *testing.T) {
	setup := func(t *testing.T) (repo, root string, earlier, later AuditReopenRecord) {
		t.Helper()
		repo = initRepo(t)
		root = filepath.Join(repo, TasksRoot)
		for _, id := range []string{"task-a", "task-b"} {
			taskWithCompletedChecklist(t, root, StateTodo, id)
			authority, err := OpenLeaseAuthority(root, id, true)
			if err != nil {
				t.Fatal(err)
			}
			if err := authority.Close(); err != nil {
				t.Fatal(err)
			}
		}
		for _, id := range []string{"task-a", "task-b"} {
			writeTaskFile(t, filepath.Join(repo, id+".txt"), id+"\n")
			git(t, repo, "add", id+".txt")
			git(t, repo, "commit", "-qm", id+" implementation\n\nCoop-Task: "+id)
		}
		var err error
		if earlier, err = CaptureAuditReopen(repo, "task-a"); err != nil {
			t.Fatal(err)
		}
		if later, err = CaptureAuditReopen(repo, "task-b"); err != nil {
			t.Fatal(err)
		}
		for _, record := range []AuditReopenRecord{earlier, later} {
			if err := WriteAuditReopenRecord(root, record); err != nil {
				t.Fatal(err)
			}
		}
		return repo, root, earlier, later
	}
	ids := []string{"task-a", "task-b"}
	carried := func(t *testing.T, repo, root, id string, want AuditReopenRecord) AuditReopenRecord {
		t.Helper()
		head := forkspace.GitOut(repo, "rev-parse", "HEAD")
		got, ok, err := ReadAuditReopenRecord(root, id)
		if err != nil || !ok || got.Generation != want.Generation || got.BaselineHead != head ||
			!AuditReopenCurrentValid(repo, head, id, got) {
			t.Fatalf("carried %s authority = %+v, ok=%v, err=%v", id, got, ok, err)
		}
		return got
	}

	t.Run("the earlier task's rework replays the later one", func(t *testing.T) {
		repo, root, earlier, later := setup(t)
		oldHead := forkspace.GitOut(repo, "rev-parse", "HEAD")
		git(t, repo, "reset", "--hard", "HEAD~2")
		writeTaskFile(t, filepath.Join(repo, "task-a.txt"), "task-a repaired\n")
		git(t, repo, "add", "task-a.txt")
		git(t, repo, "commit", "-qm", "task-a implementation\n\nCoop-Task: task-a")
		git(t, repo, "cherry-pick", oldHead)
		if err := CarryReopenedReviews(repo, []string{root}, ids, oldHead, &earlier); err != nil {
			t.Fatal(err)
		}
		carried(t, repo, root, "task-b", later)
	})

	t.Run("the later task's rework changes the earlier one's history", func(t *testing.T) {
		repo, root, earlier, later := setup(t)
		oldHead := forkspace.GitOut(repo, "rev-parse", "HEAD")
		writeTaskFile(t, filepath.Join(repo, "task-b.txt"), "task-b repaired\n")
		git(t, repo, "add", "task-b.txt")
		git(t, repo, "commit", "--amend", "--no-edit", "--no-gpg-sign", "--quiet")
		if err := CarryReopenedReviews(repo, []string{root}, ids, oldHead, &later); err != nil {
			t.Fatal(err)
		}
		got := carried(t, repo, root, "task-a", earlier)
		if len(got.History) != 1 || got.History[0].TaskID != "task-b" || got.History[0] == earlier.History[0] {
			t.Fatalf("earlier task history = %+v, want the repaired task-b commit", got.History)
		}
	})

	t.Run("an already stale authority stays as it was", func(t *testing.T) {
		repo, root, earlier, _ := setup(t)
		writeTaskFile(t, filepath.Join(repo, "task-b.txt"), "task-b changed outside any rework\n")
		git(t, repo, "add", "task-b.txt")
		git(t, repo, "commit", "--amend", "--no-edit", "--no-gpg-sign", "--quiet")
		stale := forkspace.GitOut(repo, "rev-parse", "HEAD")
		resignSince(t, repo, "HEAD~1", "2035-06-07T08:09:10Z")
		if err := CarryReopenedReviews(repo, []string{root}, ids, stale, nil); err != nil {
			t.Fatal(err)
		}
		if got, _, err := ReadAuditReopenRecord(root, "task-a"); err != nil || !AuditReopenRecordsEqual(got, earlier) {
			t.Fatalf("stale authority was carried = %+v, %v", got, err)
		}
	})
}

// A controller that stops after a reopened task's rework rewrote its commit, but before completing
// it, leaves the authority at the reviewed history. The next load accepts that rework the way
// completion would and keeps the generation, so the task resumes instead of being parked.
func TestPendingReviewResumesReworkAStoppedControllerLeft(t *testing.T) {
	repo, root, _, assignment, done, plan := pendingReviewTestCompletion(t)
	writeTaskFile(t, filepath.Join(repo, "descendant.txt"), "descendant\n")
	git(t, repo, "add", "descendant.txt")
	git(t, repo, "commit", "-qm", "descendant implementation\n\nCoop-Task: descendant")
	descendant := forkspace.GitOut(repo, "rev-parse", "HEAD")
	if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	reopen := reopenForRework(t, repo, root, done)
	reopened, _ := mustCurrentTask(t, root, done.ID)
	if err := MoveTaskDir(root, reopened, StateInProgress); err != nil {
		t.Fatal(err)
	}

	git(t, repo, "reset", "--hard", reopen.BaselineHead+"~2")
	writeTaskFile(t, filepath.Join(repo, "change.txt"), "review repaired\n")
	git(t, repo, "add", "change.txt")
	git(t, repo, "commit", "-qm", "implement review subject\n\nCoop-Task: "+done.ID)
	git(t, repo, "cherry-pick", descendant)
	head := forkspace.GitOut(repo, "rev-parse", "HEAD")

	cohort, err := LoadPendingReviews(repo, []string{root})
	if err != nil || len(cohort.Subjects) != 1 || cohort.Subjects[0].Phase != PendingReviewReopened {
		t.Fatalf("cohort after interrupted rework = %+v, %v", cohort, err)
	}
	got, ok, err := ReadAuditReopenRecord(root, done.ID)
	if err != nil || !ok || got.Generation != reopen.Generation || got.BaselineHead != head ||
		!AuditReopenCurrentValid(repo, head, done.ID, got) {
		t.Fatalf("resumed audit authority = %+v, ok=%v, err=%v", got, ok, err)
	}
}

// The same stop with two reopened tasks: the earlier task's rework replayed the later task's commit,
// which the later task's authority records by ID. Loading carries that authority across the rework
// too, so the later task is not parked as stale, and both keep their generations.
func TestPendingReviewResumesInterruptedReworkForItsSiblings(t *testing.T) {
	repo := initRepo(t)
	root := filepath.Join(repo, TasksRoot)
	base := forkspace.GitOut(repo, "rev-parse", "HEAD")
	ids := []string{"task-a", "task-b"}
	for _, id := range ids {
		task := taskWithCompletedChecklist(t, root, StateTodo, id)
		writeTaskFile(t, filepath.Join(repo, id+".txt"), id+"\n")
		git(t, repo, "add", id+".txt")
		git(t, repo, "commit", "-qm", id+" implementation\n\nCoop-Task: "+id)
		if err := CompleteTrustedTask("", root, task); err != nil {
			t.Fatal(err)
		}
	}
	if err := EnrollExistingPendingReviews(repo, []string{root}, ids, pendingReviewTestPlan(t, repo, root, base)); err != nil {
		t.Fatal(err)
	}
	authority := map[string]AuditReopenRecord{}
	for _, id := range ids {
		done, _ := mustCurrentTask(t, root, id)
		authority[id] = reopenForRework(t, repo, root, done)
	}
	reworking, _ := mustCurrentTask(t, root, "task-a")
	if err := MoveTaskDir(root, reworking, StateInProgress); err != nil {
		t.Fatal(err)
	}
	reviewed := forkspace.GitOut(repo, "rev-parse", "HEAD")
	git(t, repo, "reset", "--hard", base)
	writeTaskFile(t, filepath.Join(repo, "task-a.txt"), "task-a repaired\n")
	git(t, repo, "add", "task-a.txt")
	git(t, repo, "commit", "-qm", "task-a implementation\n\nCoop-Task: task-a")
	git(t, repo, "cherry-pick", reviewed)
	head := forkspace.GitOut(repo, "rev-parse", "HEAD")

	if cohort, err := LoadPendingReviews(repo, []string{root}); err != nil || len(cohort.Subjects) != 2 {
		t.Fatalf("cohort after the interrupted rework = %+v, %v", cohort, err)
	}
	for _, id := range ids {
		got, ok, err := ReadAuditReopenRecord(root, id)
		if err != nil || !ok || got.Generation != authority[id].Generation || got.BaselineHead != head ||
			!AuditReopenCurrentValid(repo, head, id, got) {
			t.Fatalf("%s authority after the interrupted rework = %+v, ok=%v, err=%v", id, got, ok, err)
		}
	}
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
	if err := CompleteTrustedTask("", root, foreign); err != nil {
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

func TestPendingReviewIgnoresAmbientHooks(t *testing.T) {
	for _, source := range []string{"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM"} {
		t.Run(source, func(t *testing.T) {
			dir := t.TempDir()
			hooks := filepath.Join(dir, "hooks")
			if err := os.Mkdir(hooks, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(hooks, "prepare-commit-msg"),
				[]byte("#!/bin/sh\nprintf '\nCoop-Task: ambient-hook\n' >> \"$1\"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			cfg := filepath.Join(dir, "gitconfig")
			if err := os.WriteFile(cfg, []byte("[core]\n hooksPath = "+filepath.ToSlash(hooks)+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
			t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
			t.Setenv("GIT_CONFIG_NOSYSTEM", "0")
			t.Setenv(source, cfg)
			repo, _, _, assignment, done, plan := pendingReviewTestCompletion(t)
			message := forkspace.GitOut(repo, "log", "-1", "--format=%B")
			if strings.TrimSpace(message) != "implement review subject\n\nCoop-Task: review-me" {
				t.Fatalf("fixture commit message changed: %q", message)
			}
			if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func MarkPendingReviewReopened(hosts, ids []string) error {
	return updatePendingReviewRecords(hosts, ids, func(record *PendingReviewRecord) {
		record.Phase = PendingReviewReopened
		record.RoundStarted = false
		record.Reviewed = nil
	})
}

func MarkPendingReviewVerify(hosts, ids []string) error {
	return updatePendingReviewRecords(hosts, ids, func(record *PendingReviewRecord) {
		record.Phase = PendingReviewVerify
		record.RoundStarted = false
	})
}
