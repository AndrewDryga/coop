package tasks

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
)

func reviewAndPublishForkCandidate(t *testing.T, repo string, identity forkspace.Identity, head, tree string) (ForkCandidate, bool, error) {
	t.Helper()
	review, exists, err := BeginForkCandidateReview(repo, identity, head, tree)
	if err != nil || !exists {
		return ForkCandidate{}, false, err
	}
	if review.Required {
		unlock, lockErr := forkspace.LockState(repo, identity.Name)
		if lockErr != nil {
			return ForkCandidate{}, false, lockErr
		}
		state, stateExists, stateErr := readForkCandidateState(repo, identity)
		if stateErr == nil && (!stateExists || state.manifest == nil || state.manifest.Phase != forkCandidateReviewing || state.manifest.Pending.ID != review.Candidate.ID) {
			stateErr = errors.New("candidate review state changed in test")
		}
		if stateErr == nil {
			manifest := *state.manifest
			manifest.Phase, manifest.UpdatedAt = forkCandidatePublishing, time.Now().UTC()
			stateErr = writeForkCandidateManifest(repo, manifest)
		}
		unlock()
		if stateErr != nil {
			return ForkCandidate{}, false, stateErr
		}
	}
	return PublishForkCandidate(repo, identity, head, tree)
}

func testAssignmentFork(t *testing.T, repo, name string) (string, forkspace.Identity) {
	t.Helper()
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	workspace := forkspace.Workspace(repo, name)
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	unlock, err := forkspace.LockState(repo, name)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := forkspace.EnsureGenerationLocked(repo, name)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	return workspace, identity
}

func TestForkAssignmentProjectsOneCanonicalTaskAndDefersCompletion(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".agent", "tasks")
	first := taskWithCompletedChecklist(t, root, StateTodo, "first")
	taskWithCompletedChecklist(t, root, StateTodo, "second")
	authorityRepo := filepath.Join(t.TempDir(), "project")
	workspace, identity := testAssignmentFork(t, authorityRepo, "a")
	assignment, err := AssignForkTask([]string{root}, ForkAssignmentRequest{
		AuthorityRepo: authorityRepo, Fork: identity, WorkspaceRoot: workspace, BaselineHead: strings.Repeat("a", 40), LeaseOwner: testLeaseOwner(),
	})
	if err != nil || assignment.Outcome != ForkAssignmentSelected || assignment.Task.Item.ID != first.ID {
		t.Fatalf("assignment = %+v, err %v", assignment, err)
	}
	if err := MaterializeForkProjection(assignment); err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	if _, ok := mustCurrentTask(t, root, "second"); !ok {
		t.Fatal("materializing one assignment changed its canonical sibling")
	}
	projected := mustReadTaskTree(t, assignment.Owner.Projection)
	if len(projected) != 1 || projected[0].ID != "first" || projected[0].State != StateInProgress {
		t.Fatalf("projection = %+v, want only first in progress", projected)
	}
	if err := MoveTaskDir(assignment.Owner.Projection, projected[0], StateDone); err != nil {
		t.Fatal(err)
	}
	if _, err := AcceptForkProjection(authorityRepo, root, "first", assignment.Owner); err != nil {
		t.Fatal(err)
	}
	canonical, _ := mustCurrentTask(t, root, "first")
	if canonical.State != StateInProgress {
		t.Fatalf("candidate-ready canonical state = %s, want in progress", canonical.State)
	}
	record, ok, err := ReadTaskOwnerRecord(root, "first")
	if err != nil || !ok || record.Fork.Phase != ForkAssignmentReviewing || record.Fork.ProjectionDigest == "" {
		t.Fatalf("accepted owner = %#v, ok=%v err=%v", record, ok, err)
	}
	if err := CompleteTrustedTask(root, canonical); !errors.Is(err, ErrTaskSandboxOwned) {
		t.Fatalf("ordinary completion = %v, want sandbox-owned refusal", err)
	}
	canonical, _ = mustCurrentTask(t, root, "first")
	if canonical.State != StateInProgress {
		t.Fatal("refused ordinary completion mutated canonical state")
	}
}

func TestTwoForksCannotAssignOneCanonicalTask(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tasks")
	taskForLease(t, root, StateTodo, "only")
	authorityRepo := filepath.Join(t.TempDir(), "project")
	workspaceA, identityA := testAssignmentFork(t, authorityRepo, "a")
	workspaceB, identityB := testAssignmentFork(t, authorityRepo, "b")
	requests := []ForkAssignmentRequest{
		{AuthorityRepo: authorityRepo, Fork: identityA, WorkspaceRoot: workspaceA, BaselineHead: strings.Repeat("a", 40), LeaseOwner: testLeaseOwner()},
		{AuthorityRepo: authorityRepo, Fork: identityB, WorkspaceRoot: workspaceB, BaselineHead: strings.Repeat("b", 40), LeaseOwner: testLeaseOwner()},
	}
	start := make(chan struct{})
	results := make([]ForkAssignment, len(requests))
	errs := make([]error, len(requests))
	var wg sync.WaitGroup
	for i := range requests {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = AssignForkTask([]string{root}, requests[i])
		}(i)
	}
	close(start)
	wg.Wait()
	winners := 0
	for i, result := range results {
		if errs[i] != nil {
			t.Fatalf("fork %d assignment: %v", i, errs[i])
		}
		if result.Outcome == ForkAssignmentSelected {
			winners++
			_ = result.Lease.Release()
		}
	}
	if winners != 1 {
		t.Fatalf("assignment winners = %d, results %+v", winners, results)
	}
	record, ok, err := ReadTaskOwnerRecord(root, "only")
	if err != nil || !ok || record.Kind != TaskOwnerFork {
		t.Fatalf("durable winner = %#v, ok=%v err=%v", record, ok, err)
	}
}

func TestForkAssignmentReplaysPreparingOwnerBeforeExecution(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tasks")
	taskForLease(t, root, StateTodo, "preparing")
	repo := filepath.Join(t.TempDir(), "project")
	workspace, identity := testAssignmentFork(t, repo, "preparing-worker")
	request := ForkAssignmentRequest{
		AuthorityRepo: repo, Fork: identity, WorkspaceRoot: workspace,
		BaselineHead: strings.Repeat("2", 40), LeaseOwner: testLeaseOwner(),
	}
	assignment, err := AssignForkTask([]string{root}, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	current, _ := mustCurrentTask(t, root, "preparing")
	if err := MoveTaskDir(root, current, StateTodo); err != nil {
		t.Fatal(err)
	}
	record, _, err := ReadTaskOwnerRecord(root, "preparing")
	if err != nil || record.Fork == nil {
		t.Fatal(err)
	}
	if _, err := UpdateForkTaskAssignment(root, "preparing", *record.Fork, func(owner *ForkTaskOwner) error {
		owner.Phase = ForkAssignmentPreparing
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	resumed, err := AssignForkTask([]string{root}, request)
	if err != nil || resumed.Outcome != ForkAssignmentSelected {
		t.Fatalf("preparing replay = %+v, %v", resumed, err)
	}
	defer resumed.Lease.Release()
	current, _ = mustCurrentTask(t, root, "preparing")
	if current.State != StateInProgress {
		t.Fatalf("preparing replay canonical state = %s", current.State)
	}
}

func TestHumanLifecycleCannotClearForkAssignment(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tasks")
	taskForLease(t, root, StateTodo, "owned")
	authorityRepo := filepath.Join(t.TempDir(), "project")
	workspace, identity := testAssignmentFork(t, authorityRepo, "c")
	assignment, err := AssignForkTask([]string{root}, ForkAssignmentRequest{
		AuthorityRepo: authorityRepo, Fork: identity, WorkspaceRoot: workspace, BaselineHead: strings.Repeat("c", 40), LeaseOwner: testLeaseOwner(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = assignment.Lease.Release()
	if code, err := tasksFolderRelease(root, []string{"owned"}); code == 0 || !errors.Is(err, ErrTaskSandboxOwned) {
		t.Fatalf("release fork-owned task = code %d err %v", code, err)
	}
	if code, err := tasksFolderBlock(root, []string{"owned"}); code == 0 || !errors.Is(err, ErrTaskSandboxOwned) {
		t.Fatalf("block fork-owned task = code %d err %v", code, err)
	}
	current, _ := mustCurrentTask(t, root, "owned")
	if current.State != StateInProgress {
		t.Fatal("refused human lifecycle changed fork-owned task")
	}
}

func TestTaskOwnerV2StrictAndInstanceFenced(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tasks")
	item := taskForLease(t, root, StateTodo, "strict")
	instance, err := EnsureTaskInstance(root, item)
	if err != nil {
		t.Fatal(err)
	}
	now := testLeaseOwner().Now()
	record := TaskOwnerRecord{
		Version: taskOwnershipRecordVersion, TaskID: item.ID, Kind: TaskOwnerHuman, Task: &instance,
		Source: taskOwnerSourceInteractiveClaim, User: "ada", Host: "host", ClaimedAt: now,
	}
	if err := writeTaskOwnerRecord(root, record); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ReadTaskOwnerRecord(root, item.ID); err != nil || !ok {
		t.Fatalf("v2 read = ok %v err %v", ok, err)
	}
	if err := os.RemoveAll(item.Dir); err != nil {
		t.Fatal(err)
	}
	writeTaskFile(t, filepath.Join(item.Dir, "task.md"), "# replacement\n")
	if _, ok, err := ReadTaskOwnerRecord(root, item.ID); err == nil || ok {
		t.Fatalf("recreated task inherited owner: ok=%v err=%v", ok, err)
	}
}

func TestConcurrentDifferentAssignmentsShareOneQueueIdentity(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tasks")
	taskForLease(t, root, StateTodo, "alpha")
	taskForLease(t, root, StateTodo, "beta")
	repo := filepath.Join(t.TempDir(), "project")
	workspaceA, identityA := testAssignmentFork(t, repo, "a")
	workspaceB, identityB := testAssignmentFork(t, repo, "b")
	requests := []ForkAssignmentRequest{
		{AuthorityRepo: repo, Fork: identityA, WorkspaceRoot: workspaceA, BaselineHead: strings.Repeat("a", 40), LeaseOwner: testLeaseOwner()},
		{AuthorityRepo: repo, Fork: identityB, WorkspaceRoot: workspaceB, BaselineHead: strings.Repeat("b", 40), LeaseOwner: testLeaseOwner()},
	}
	start := make(chan struct{})
	results := make([]ForkAssignment, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range requests {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = AssignForkTask([]string{root}, requests[i])
		}(i)
	}
	close(start)
	wg.Wait()
	for i := range results {
		if errs[i] != nil || results[i].Outcome != ForkAssignmentSelected {
			t.Fatalf("assignment %d = %+v, err %v", i, results[i], errs[i])
		}
		if err := results[i].Lease.Release(); err != nil {
			t.Fatal(err)
		}
	}
	owners := make([]TaskOwnerRecord, 0, 2)
	for _, id := range []string{"alpha", "beta"} {
		record, ok, err := ReadTaskOwnerRecord(root, id)
		if err != nil || !ok {
			t.Fatalf("owner %s = %+v, %v", id, record, err)
		}
		owners = append(owners, record)
	}
	if owners[0].Task.Ref.QueueID != owners[1].Task.Ref.QueueID {
		t.Fatalf("concurrent queue identities diverged: %s != %s", owners[0].Task.Ref.QueueID, owners[1].Task.Ref.QueueID)
	}
}

func TestForkCandidateFinalizesExactCanonicalTask(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tasks")
	item := taskWithCompletedChecklist(t, root, StateTodo, "land-me")
	writeTaskFile(t, filepath.Join(item.Dir, "artifacts", "proof.txt"), "old\n")
	repo := filepath.Join(t.TempDir(), "project")
	workspace, identity := testAssignmentFork(t, repo, "candidate")
	assignment, err := AssignForkTask([]string{root}, ForkAssignmentRequest{
		AuthorityRepo: repo, Fork: identity, WorkspaceRoot: workspace,
		BaselineHead: strings.Repeat("a", 40), LeaseOwner: testLeaseOwner(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	projected, _ := mustCurrentTask(t, assignment.Owner.Projection, "land-me")
	if err := os.WriteFile(filepath.Join(projected.Dir, "artifacts", "proof.txt"), []byte("reviewed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := MoveTaskDir(assignment.Owner.Projection, projected, StateDone); err != nil {
		t.Fatal(err)
	}
	if _, err := AcceptForkProjection(repo, root, "land-me", assignment.Owner); err != nil {
		t.Fatal(err)
	}
	candidate, published, err := reviewAndPublishForkCandidate(t, repo, identity, strings.Repeat("b", 40), strings.Repeat("c", 40))
	if err != nil || !published {
		t.Fatalf("publish candidate = %+v, %v, %v", candidate, published, err)
	}
	record, ok, err := ReadTaskOwnerRecord(root, "land-me")
	if err != nil || !ok || record.Fork.Phase != ForkAssignmentReady || record.Fork.CandidateID != candidate.ID {
		t.Fatalf("ready owner = %+v, ok=%v err=%v", record, ok, err)
	}
	if err := FinalizeForkCandidateTask(repo, candidate, candidate.Assignments[0]); err != nil {
		t.Fatal(err)
	}
	landed, ok := mustCurrentTask(t, root, "land-me")
	if !ok || landed.State != StateDone {
		t.Fatalf("landed task = %+v, ok=%v", landed, ok)
	}
	if data, err := os.ReadFile(filepath.Join(landed.Dir, "artifacts", "proof.txt")); err != nil || string(data) != "reviewed\n" {
		t.Fatalf("landed artifact = %q, %v", data, err)
	}
	if _, owned, err := ReadTaskOwnerRecord(root, "land-me"); err != nil || owned {
		t.Fatalf("landed owner survives: owned=%v err=%v", owned, err)
	}
	if indexes, problems := IndexedForkAssignments(repo, identity); len(indexes) != 0 || len(problems) != 0 {
		t.Fatalf("landed indexes = %+v problems=%v", indexes, problems)
	}
	if err := MarkForkCandidateLandedLocked(repo, candidate); err != nil {
		t.Fatal(err)
	}
}

func TestPublishedForkCandidateFreezesGenerationBeforeAnotherAssignment(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tasks")
	taskWithCompletedChecklist(t, root, StateTodo, "a-first")
	taskWithCompletedChecklist(t, root, StateTodo, "z-next")
	repo := filepath.Join(t.TempDir(), "project")
	workspace, identity := testAssignmentFork(t, repo, "frozen")
	request := ForkAssignmentRequest{
		AuthorityRepo: repo, Fork: identity, WorkspaceRoot: workspace,
		BaselineHead: strings.Repeat("a", 40), LeaseOwner: testLeaseOwner(),
	}
	assignment, err := AssignForkTask([]string{root}, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	projected, _ := mustCurrentTask(t, assignment.Owner.Projection, assignment.Task.Item.ID)
	if err := MoveTaskDir(assignment.Owner.Projection, projected, StateDone); err != nil {
		t.Fatal(err)
	}
	if _, err := AcceptForkProjection(repo, root, assignment.Task.Item.ID, assignment.Owner); err != nil {
		t.Fatal(err)
	}
	if _, published, err := reviewAndPublishForkCandidate(t, repo, identity, strings.Repeat("b", 40), strings.Repeat("c", 40)); err != nil || !published {
		t.Fatalf("publish candidate = published %v, err=%v", published, err)
	}
	next, err := AssignForkTask([]string{root}, request)
	if err != nil {
		t.Fatal(err)
	}
	if next.Outcome != ForkAssignmentExecutorDrained || next.Lease != nil {
		t.Fatalf("ready generation selected more work: %+v", next)
	}
	remaining, ok := mustCurrentTask(t, root, "z-next")
	if !ok || remaining.State != StateTodo {
		t.Fatalf("next canonical task changed under ready candidate: %+v, ok=%v", remaining, ok)
	}
}

func TestForkProjectionRejectsAdditionalTask(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tasks")
	taskForLease(t, root, StateTodo, "only")
	repo := filepath.Join(t.TempDir(), "project")
	workspace, identity := testAssignmentFork(t, repo, "one")
	assignment, err := AssignForkTask([]string{root}, ForkAssignmentRequest{
		AuthorityRepo: repo, Fork: identity, WorkspaceRoot: workspace,
		BaselineHead: strings.Repeat("d", 40), LeaseOwner: testLeaseOwner(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer assignment.Lease.Release()
	writeTaskFile(t, filepath.Join(assignment.Owner.Projection, StateTodo, "escape", "task.md"), "# escape\n")
	if _, err := ValidateForkProjection(repo, root, "only", assignment.Owner); err == nil || !strings.Contains(err.Error(), "outside its one assigned task") {
		t.Fatalf("extra projected task validation = %v", err)
	}
}

func TestForkProjectionRefusesPreplantedWorkspaceSymlinkAncestor(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tasks")
	taskForLease(t, root, StateTodo, "only")
	repo := filepath.Join(t.TempDir(), "project")
	workspace, identity := testAssignmentFork(t, repo, "preplanted")
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, ".coop")); err != nil {
		t.Fatal(err)
	}
	_, err := AssignForkTask([]string{root}, ForkAssignmentRequest{
		AuthorityRepo: repo, Fork: identity, WorkspaceRoot: workspace,
		BaselineHead: strings.Repeat("d", 40), LeaseOwner: testLeaseOwner(),
	})
	if err == nil || !strings.Contains(err.Error(), "not a real directory") {
		t.Fatalf("preplanted projection ancestor = %v, want real-directory refusal", err)
	}
	if entries, readErr := os.ReadDir(outside); readErr != nil || len(entries) != 0 {
		t.Fatalf("outside symlink target was mutated: entries=%v err=%v", entries, readErr)
	}
}

func TestForkProjectionRefusesSwappedWorkspaceSymlinkAncestor(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tasks")
	taskForLease(t, root, StateTodo, "only")
	repo := filepath.Join(t.TempDir(), "project")
	workspace, identity := testAssignmentFork(t, repo, "swapped")
	assignment, err := AssignForkTask([]string{root}, ForkAssignmentRequest{
		AuthorityRepo: repo, Fork: identity, WorkspaceRoot: workspace,
		BaselineHead: strings.Repeat("e", 40), LeaseOwner: testLeaseOwner(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer assignment.Lease.Release()
	realControl := filepath.Join(workspace, ".coop-real")
	if err := os.Rename(filepath.Join(workspace, ".coop"), realControl); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, ".coop")); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateForkProjection(repo, root, "only", assignment.Owner); err == nil ||
		!strings.Contains(err.Error(), "not a real directory") {
		t.Fatalf("swapped projection ancestor = %v, want real-directory refusal", err)
	}
	if entries, readErr := os.ReadDir(outside); readErr != nil || len(entries) != 0 {
		t.Fatalf("outside symlink target was read or mutated: entries=%v err=%v", entries, readErr)
	}
}

func TestForkProjectionAcceptanceReplacesExistingNestedArtifactsIdempotently(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tasks")
	taskForLease(t, root, StateTodo, "repeat")
	repo := filepath.Join(t.TempDir(), "project")
	workspace, identity := testAssignmentFork(t, repo, "repeat-worker")
	assignment, err := AssignForkTask([]string{root}, ForkAssignmentRequest{
		AuthorityRepo: repo, Fork: identity, WorkspaceRoot: workspace,
		BaselineHead: strings.Repeat("a", 40), LeaseOwner: testLeaseOwner(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	projected, _ := mustCurrentTask(t, assignment.Owner.Projection, "repeat")
	artifact := filepath.Join(projected.Dir, "artifacts", "nested", "proof.txt")
	writeTaskFile(t, artifact, "first\n")
	if _, err := AcceptForkProjection(repo, root, "repeat", assignment.Owner); err != nil {
		t.Fatal(err)
	}
	record, ok, err := ReadTaskOwnerRecord(root, "repeat")
	if err != nil || !ok {
		t.Fatalf("paused owner = %+v, ok=%v err=%v", record, ok, err)
	}
	if err := os.WriteFile(artifact, []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := AcceptForkProjection(repo, root, "repeat", *record.Fork); err != nil {
		t.Fatalf("second acceptance over existing nested artifact: %v", err)
	}
	canonical, _ := mustCurrentTask(t, root, "repeat")
	data, err := os.ReadFile(filepath.Join(canonical.Dir, "artifacts", "nested", "proof.txt"))
	if err != nil || string(data) != "second\n" {
		t.Fatalf("canonical repeated artifact = %q, err=%v", data, err)
	}
}

func TestFailedForkRunCannotPublishProjectedDoneForReview(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tasks")
	taskWithCompletedChecklist(t, root, StateTodo, "failed-signoff")
	repo := filepath.Join(t.TempDir(), "project")
	workspace, identity := testAssignmentFork(t, repo, "failed-signoff-worker")
	assignment, err := AssignForkTask([]string{root}, ForkAssignmentRequest{
		AuthorityRepo: repo, Fork: identity, WorkspaceRoot: workspace,
		BaselineHead: strings.Repeat("f", 40), LeaseOwner: testLeaseOwner(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	projected, _ := mustCurrentTask(t, assignment.Owner.Projection, "failed-signoff")
	if err := MoveTaskDir(assignment.Owner.Projection, projected, StateDone); err != nil {
		t.Fatal(err)
	}
	if err := PrepareForkProjectionForRun(repo, root, "failed-signoff", assignment.Owner); err != nil {
		t.Fatal(err)
	}
	if _, err := AcceptForkProjection(repo, root, "failed-signoff", assignment.Owner); err != nil {
		t.Fatal(err)
	}
	record, ok, err := ReadTaskOwnerRecord(root, "failed-signoff")
	if err != nil || !ok || record.Fork == nil || record.Fork.Phase != ForkAssignmentPaused {
		t.Fatalf("failed run owner = %+v, owned=%v err=%v; want paused", record, ok, err)
	}
	canonical, _ := mustCurrentTask(t, root, "failed-signoff")
	if canonical.State != StateInProgress {
		t.Fatalf("failed run canonical state = %s, want in progress", canonical.State)
	}
}

func TestForkDiscardReplayNormalizesTaskAlreadyReturnedToTodo(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tasks")
	taskForLease(t, root, StateTodo, "discard-replay")
	repo := filepath.Join(t.TempDir(), "project")
	workspace, identity := testAssignmentFork(t, repo, "discard-replay-worker")
	assignment, err := AssignForkTask([]string{root}, ForkAssignmentRequest{
		AuthorityRepo: repo, Fork: identity, WorkspaceRoot: workspace,
		BaselineHead: strings.Repeat("1", 40), LeaseOwner: testLeaseOwner(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	canonical, _ := mustCurrentTask(t, root, "discard-replay")
	if err := MoveTaskDir(root, canonical, StateTodo); err != nil {
		t.Fatal(err)
	}
	canonical, _ = mustCurrentTask(t, root, "discard-replay")
	writeTaskFile(t, filepath.Join(canonical.Dir, "state.md"), "# stale\n")
	if err := DiscardForkTaskStateLocked(repo, identity); err != nil {
		t.Fatal(err)
	}
	bodyBytes, err := os.ReadFile(filepath.Join(canonical.Dir, "state.md"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(bodyBytes)
	if !strings.Contains(body, "**Status:** ready — sandbox assignment discarded") ||
		!strings.Contains(body, "**Next action:** claim or assign this canonical task again") {
		t.Fatalf("replayed discard state = %q", body)
	}
	if _, owned, err := ReadTaskOwnerRecord(root, "discard-replay"); err != nil || owned {
		t.Fatalf("replayed discard owner: owned=%v err=%v", owned, err)
	}
}

func TestUnblockedForkAssignmentRestoresBlockedProjectionForSameGeneration(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tasks")
	taskForLease(t, root, StateTodo, "decision")
	repo := filepath.Join(t.TempDir(), "project")
	workspace, identity := testAssignmentFork(t, repo, "decision-worker")
	request := ForkAssignmentRequest{
		AuthorityRepo: repo, Fork: identity, WorkspaceRoot: workspace,
		BaselineHead: strings.Repeat("b", 40), LeaseOwner: testLeaseOwner(),
	}
	assignment, err := AssignForkTask([]string{root}, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	projected, _ := mustCurrentTask(t, assignment.Owner.Projection, "decision")
	writeTaskFile(t, filepath.Join(projected.Dir, "decision.md"), "# Decision\n\n**Resolution:** accepted\n")
	if err := MoveTaskDir(assignment.Owner.Projection, projected, StateBlocked); err != nil {
		t.Fatal(err)
	}
	if _, err := AcceptForkProjection(repo, root, "decision", assignment.Owner); err != nil {
		t.Fatal(err)
	}
	canonical, _ := mustCurrentTask(t, root, "decision")
	if canonical.State != StateBlocked {
		t.Fatalf("canonical state = %s, want blocked", canonical.State)
	}
	if err := resolveAndUnblock(root, canonical, ""); err != nil {
		t.Fatal(err)
	}
	resumed, err := AssignForkTask([]string{root}, request)
	if err != nil || resumed.Outcome != ForkAssignmentSelected {
		t.Fatalf("resumed assignment = %+v, err=%v", resumed, err)
	}
	defer resumed.Lease.Release()
	if err := PrepareForkProjectionForRun(repo, root, "decision", resumed.Owner); err != nil {
		t.Fatal(err)
	}
	projected, _ = mustCurrentTask(t, resumed.Owner.Projection, "decision")
	if projected.State != StateInProgress {
		t.Fatalf("unblocked projection state = %s, want in progress", projected.State)
	}
}

func TestForkUnblockReplaysPausedOwnerWhileCanonicalStillBlocked(t *testing.T) {
	repo, root, assignment := proposalAssignment(t, "paused-blocked-unblock")
	projected, _ := mustCurrentTask(t, assignment.Owner.Projection, "assigned")
	writeTaskFile(t, filepath.Join(projected.Dir, "decision.md"), "# Decision\n\n**Resolution:** accepted\n")
	if err := MoveTaskDir(assignment.Owner.Projection, projected, StateBlocked); err != nil {
		t.Fatal(err)
	}
	if _, err := AcceptForkProjection(repo, root, "assigned", assignment.Owner); err != nil {
		t.Fatal(err)
	}
	record, _, err := ReadTaskOwnerRecord(root, "assigned")
	if err != nil || record.Fork == nil {
		t.Fatal(err)
	}
	if _, err := UpdateForkTaskAssignment(root, "assigned", *record.Fork, func(owner *ForkTaskOwner) error {
		owner.Phase = ForkAssignmentPaused
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	canonical, _ := mustCurrentTask(t, root, "assigned")
	if err := resolveAndUnblock(root, canonical, ""); err != nil {
		t.Fatal(err)
	}
	canonical, _ = mustCurrentTask(t, root, "assigned")
	if canonical.State != StateTodo {
		t.Fatalf("replayed paused unblock state = %s", canonical.State)
	}
}

func TestForkAssignmentReplaysBlockedAcceptanceAtBothCrashBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name          string
		phase         ForkAssignmentPhase
		moveCanonical bool
	}{
		{name: "after intent", phase: ForkAssignmentBlocking},
		{name: "after canonical move", phase: ForkAssignmentBlocking, moveCanonical: true},
		{name: "legacy move before owner write", phase: ForkAssignmentWorking, moveCanonical: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, root, assignment := proposalAssignment(t, "blocked-replay")
			projected, _ := mustCurrentTask(t, assignment.Owner.Projection, "assigned")
			writeTaskFile(t, filepath.Join(projected.Dir, "decision.md"), "# Decision\n\nBlocked pending an operator choice.\n")
			if err := MoveTaskDir(assignment.Owner.Projection, projected, StateBlocked); err != nil {
				t.Fatal(err)
			}
			result, err := ValidateForkProjection(repo, root, "assigned", assignment.Owner)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := UpdateForkTaskAssignment(root, "assigned", assignment.Owner, func(owner *ForkTaskOwner) error {
				owner.Phase = tc.phase
				owner.ProjectionDigest = result.Digest
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if tc.moveCanonical {
				canonical, _ := mustCurrentTask(t, root, "assigned")
				if err := MoveTaskDir(root, canonical, StateBlocked); err != nil {
					t.Fatal(err)
				}
			}
			resumed, err := AssignForkTask([]string{root}, ForkAssignmentRequest{
				AuthorityRepo: repo, Fork: assignment.Owner.Fork,
				WorkspaceRoot: forkspace.Workspace(repo, assignment.Owner.Fork.Name),
				BaselineHead:  assignment.Owner.BaselineHead, LeaseOwner: testLeaseOwner(),
			})
			if err != nil || resumed.Outcome != ForkAssignmentExecutorDrained {
				t.Fatalf("blocking replay = %+v, err=%v", resumed, err)
			}
			canonical, _ := mustCurrentTask(t, root, "assigned")
			record, owned, err := ReadTaskOwnerRecord(root, "assigned")
			if err != nil || !owned || canonical.State != StateBlocked || record.Fork == nil ||
				record.Fork.Phase != ForkAssignmentBlocked || record.Fork.ProjectionDigest != result.Digest {
				t.Fatalf("replayed block = task %+v owner %+v owned=%v err=%v", canonical, record, owned, err)
			}
		})
	}
}

func TestForkAssignmentBlockingReplayRejectsChangedProjectionBeforeCanonicalSync(t *testing.T) {
	repo, root, assignment := proposalAssignment(t, "blocked-changed")
	projected, _ := mustCurrentTask(t, assignment.Owner.Projection, "assigned")
	if err := MoveTaskDir(assignment.Owner.Projection, projected, StateBlocked); err != nil {
		t.Fatal(err)
	}
	result, err := ValidateForkProjection(repo, root, "assigned", assignment.Owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateForkTaskAssignment(root, "assigned", assignment.Owner, func(owner *ForkTaskOwner) error {
		owner.Phase = ForkAssignmentBlocking
		owner.ProjectionDigest = result.Digest
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	canonical, _ := mustCurrentTask(t, root, "assigned")
	canonicalTask, err := os.ReadFile(filepath.Join(canonical.Dir, "task.md"))
	if err != nil {
		t.Fatal(err)
	}
	projected, _ = mustCurrentTask(t, assignment.Owner.Projection, "assigned")
	writeTaskFile(t, filepath.Join(projected.Dir, "task.md"), "# changed after blocking intent\n")
	_, err = AssignForkTask([]string{root}, ForkAssignmentRequest{
		AuthorityRepo: repo, Fork: assignment.Owner.Fork,
		WorkspaceRoot: forkspace.Workspace(repo, assignment.Owner.Fork.Name),
		BaselineHead:  assignment.Owner.BaselineHead, LeaseOwner: testLeaseOwner(),
	})
	if err == nil || !strings.Contains(err.Error(), "changed after its durable transition intent") {
		t.Fatalf("changed blocking replay error = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(canonical.Dir, "task.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(canonicalTask) {
		t.Fatalf("changed blocking replay mutated canonical task: got %q want %q", got, canonicalTask)
	}
}

func TestForkUnblockRecoversTodoWithBlockedOwner(t *testing.T) {
	repo, root, assignment := proposalAssignment(t, "todo-blocked-unblock")
	projected, _ := mustCurrentTask(t, assignment.Owner.Projection, "assigned")
	writeTaskFile(t, filepath.Join(projected.Dir, "decision.md"), "# Decision\n\n**Resolution:** accepted\n")
	if err := MoveTaskDir(assignment.Owner.Projection, projected, StateBlocked); err != nil {
		t.Fatal(err)
	}
	if _, err := AcceptForkProjection(repo, root, "assigned", assignment.Owner); err != nil {
		t.Fatal(err)
	}
	canonical, _ := mustCurrentTask(t, root, "assigned")
	if err := MoveTaskDir(root, canonical, StateTodo); err != nil {
		t.Fatal(err)
	}
	code, err := tasksFolderUnblock(root, []string{"assigned"})
	if err != nil || code != 0 {
		t.Fatalf("todo+blocked-owner recovery = code %d err %v", code, err)
	}
	record, owned, err := ReadTaskOwnerRecord(root, "assigned")
	if err != nil || !owned || record.Fork == nil || record.Fork.Phase != ForkAssignmentPaused {
		t.Fatalf("recovered owner = %+v, owned=%v err=%v", record, owned, err)
	}
}

// A discard that crashed between removing the owner record and removing the assignment index
// leaves its intent behind. Until that intent is replayed the fork takes no new work (index
// recovery would otherwise drop the half-discarded assignment as an orphan), the task-state
// summary still reads (so `fork rm --force` can reach the replay), and the replay covers every
// assignment the fork holds now, not only the journaled ones.
func TestPendingForkDiscardFailsClosedAndReplayCoversCurrentAssignments(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tasks")
	taskForLease(t, root, StateTodo, "crash-held")
	taskForLease(t, root, StateTodo, "crash-next")
	repo := filepath.Join(t.TempDir(), "project")
	workspace, identity := testAssignmentFork(t, repo, "crash-worker")
	request := ForkAssignmentRequest{
		AuthorityRepo: repo, Fork: identity, WorkspaceRoot: workspace,
		BaselineHead: strings.Repeat("1", 40), LeaseOwner: testLeaseOwner(),
	}
	assignment, err := AssignForkTask([]string{root}, request)
	if err != nil || assignment.Task.Item.ID != "crash-held" {
		t.Fatalf("first assignment = %+v, err=%v; want crash-held", assignment.Owner, err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}

	// The crash window: intent journaled, task returned to todo, owner record removed, index left.
	indexes, problems := IndexedForkAssignments(repo, identity)
	if len(problems) > 0 || len(indexes) != 1 {
		t.Fatalf("indexes = %+v, problems = %v", indexes, problems)
	}
	if err := writeForkDiscard(repo, forkDiscardIntent{
		Version: forkDiscardVersion, Fork: identity, Assignments: indexes, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	held, _ := mustCurrentTask(t, root, "crash-held")
	if err := MoveTaskDir(root, held, StateTodo); err != nil {
		t.Fatal(err)
	}
	if err := removeTaskOwnerRecordFile(root, "crash-held"); err != nil {
		t.Fatal(err)
	}

	if _, err := AssignForkTask([]string{root}, request); err == nil || !strings.Contains(err.Error(), "interrupted discard") {
		t.Fatalf("assignment during a pending discard = %v; want a fail-closed refusal", err)
	}
	if next, _ := mustCurrentTask(t, root, "crash-next"); next.State != StateTodo {
		t.Fatalf("crash-next was assigned during a pending discard: %+v", next)
	}
	if _, owned, err := ReadTaskOwnerRecord(root, "crash-next"); err != nil || owned {
		t.Fatalf("crash-next owned during a pending discard: owned=%v err=%v", owned, err)
	}
	if after, _ := IndexedForkAssignments(repo, identity); len(after) != 1 {
		t.Fatalf("half-discarded index dropped as an orphan: %+v", after)
	}
	summary, err := ReadForkTaskStateSummary(repo, identity)
	if err != nil || !summary.DiscardPending || summary.Assignments != 1 {
		t.Fatalf("summary during a pending discard = %+v, err=%v; want it readable with the discard pending", summary, err)
	}

	if err := DiscardForkTaskStateLocked(repo, identity); err != nil {
		t.Fatalf("replay = %v", err)
	}
	if _, pending, err := readForkDiscard(repo, identity); err != nil || pending {
		t.Fatalf("intent survives the replay: pending=%v err=%v", pending, err)
	}
	if after, _ := IndexedForkAssignments(repo, identity); len(after) != 0 {
		t.Fatalf("indexes after the replay = %+v", after)
	}
	if held, _ := mustCurrentTask(t, root, "crash-held"); held.State != StateTodo {
		t.Fatalf("crash-held after the replay = %+v", held)
	}

	// An assignment the journaled intent does not name (acquired after an interrupted discard by
	// an older binary) is discarded by the replay too, so nothing stays fork-owned once the
	// generation goes.
	assignment, err = AssignForkTask([]string{root}, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	acquired := assignment.Task.Item.ID
	if err := writeForkDiscard(repo, forkDiscardIntent{
		Version: forkDiscardVersion, Fork: identity, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := DiscardForkTaskStateLocked(repo, identity); err != nil {
		t.Fatalf("replay with an unlisted assignment = %v", err)
	}
	if item, _ := mustCurrentTask(t, root, acquired); item.State != StateTodo {
		t.Fatalf("%s stranded after the replay: %+v", acquired, item)
	}
	if _, owned, err := ReadTaskOwnerRecord(root, acquired); err != nil || owned {
		t.Fatalf("%s still fork-owned after the replay: owned=%v err=%v", acquired, owned, err)
	}
	if after, _ := IndexedForkAssignments(repo, identity); len(after) != 0 {
		t.Fatalf("indexes after the union replay = %+v", after)
	}
}
