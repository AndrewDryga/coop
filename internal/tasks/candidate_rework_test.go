package tasks

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func commitCandidateTaskForRework(t *testing.T, f candidateRoundFixture, id string) {
	t.Helper()
	name := "candidate-" + id + ".txt"
	writeTaskFile(t, filepath.Join(f.workspace, name), "implementation for "+id+"\n")
	git(t, f.workspace, "add", name)
	git(t, f.workspace, "commit", "-qm", "implement "+id+"\n\n"+CoopTaskTrailer+": "+id)
}

func beginCandidateReworkReview(t *testing.T, f candidateRoundFixture) ForkCandidateReview {
	t.Helper()
	head, tree := f.headTree()
	review, exists, err := BeginForkCandidateReview(f.repo, f.identity, head, tree)
	if err != nil || !exists || !review.Required {
		t.Fatalf("begin candidate review = %+v, exists=%v, err=%v", review, exists, err)
	}
	return review
}

func prepareCandidateReworkProjectionState(t *testing.T, f candidateRoundFixture) {
	t.Helper()
	for i := range f.assignments {
		assignment := &f.assignments[i]
		projected, ok := mustCurrentTask(t, assignment.Owner.Projection, assignment.Task.Item.ID)
		if !ok || projected.State != StateDone {
			t.Fatalf("projection before state setup = %+v, exists=%v", projected, ok)
		}
		if err := WriteTaskState(projected.Dir, projected.ID, TaskStateFields{
			Status: "done", DoneSoFar: "fixture implementation complete", NextAction: "none", Traps: "none",
		}); err != nil {
			t.Fatal(err)
		}
		owner, owned, err := ReadTaskOwnerRecord(assignment.Task.Root, assignment.Task.Item.ID)
		if err != nil || !owned || owner.Fork == nil {
			t.Fatalf("read owner during projection setup = %+v, owned=%v, err=%v", owner, owned, err)
		}
		if _, err := AcceptForkProjection(f.repo, assignment.Task.Root, assignment.Task.Item.ID, *owner.Fork); err != nil {
			t.Fatalf("accept fixture state for %s: %v", projected.ID, err)
		}
		owner, owned, err = ReadTaskOwnerRecord(assignment.Task.Root, assignment.Task.Item.ID)
		if err != nil || !owned || owner.Fork == nil {
			t.Fatalf("read refreshed owner during projection setup = %+v, owned=%v, err=%v", owner, owned, err)
		}
		assignment.Owner = *owner.Fork
	}
}

func TestForkCandidateReworkReopensOnlyFindingAndRequiresFreshReview(t *testing.T) {
	f := newCandidateRoundFixture(t, "alpha", "beta")
	prepareCandidateReworkProjectionState(t, f)
	commitCandidateTaskForRework(t, f, "alpha")
	commitCandidateTaskForRework(t, f, "beta")
	review := beginCandidateReworkReview(t, f)
	finding := ForkCandidateFinding{TaskID: "alpha", Gate: "unit tests", Finding: "alpha misses its error path"}
	if err := ReworkForkCandidate(f.repo, f.identity, review.Candidate.ID, []ForkCandidateFinding{finding}); err != nil {
		t.Fatalf("rework alpha: %v", err)
	}

	alpha := f.assignments[0]
	beta := f.assignments[1]
	alphaProjection, alphaExists := mustCurrentTask(t, alpha.Owner.Projection, "alpha")
	betaProjection, betaExists := mustCurrentTask(t, beta.Owner.Projection, "beta")
	if !alphaExists || alphaProjection.State != StateInProgress {
		t.Fatalf("finding task projection = %+v, exists=%v; want in progress", alphaProjection, alphaExists)
	}
	if !betaExists || betaProjection.State != StateDone {
		t.Fatalf("unmentioned task projection = %+v, exists=%v; want done", betaProjection, betaExists)
	}
	alphaCanonical, alphaExists := mustCurrentTask(t, alpha.Task.Root, "alpha")
	betaCanonical, betaExists := mustCurrentTask(t, beta.Task.Root, "beta")
	if !alphaExists || alphaCanonical.State != StateInProgress || !betaExists || betaCanonical.State != StateInProgress {
		t.Fatalf("canonical states after rework = alpha:%v beta:%v; want both retained in progress", alphaCanonical.State, betaCanonical.State)
	}
	for _, expected := range []struct {
		root, id string
		owner    ForkTaskOwner
		phase    ForkAssignmentPhase
	}{{alpha.Task.Root, "alpha", alpha.Owner, ForkAssignmentPaused}, {beta.Task.Root, "beta", beta.Owner, ForkAssignmentReviewing}} {
		record, owned, err := ReadTaskOwnerRecord(expected.root, expected.id)
		if err != nil || !owned || record.Fork == nil || record.Fork.Fork != f.identity ||
			record.Fork.AssignmentID != expected.owner.AssignmentID || record.Fork.CandidateID != "" || record.Fork.Phase != expected.phase {
			t.Fatalf("canonical owner %s after rework = %+v, owned=%v, err=%v", expected.id, record.Fork, owned, err)
		}
	}
	audit, exists, err := ReadAuditReopenRecord(alpha.Owner.Projection, "alpha")
	if err != nil || !exists || audit.Finding != finding.Finding || audit.BaselineHead != review.Candidate.Head {
		t.Fatalf("alpha audit authority = %+v, exists=%v, err=%v", audit, exists, err)
	}
	if _, exists, err := ReadAuditReopenRecord(beta.Owner.Projection, "beta"); err != nil || exists {
		t.Fatalf("unmentioned beta got audit authority: exists=%v, err=%v", exists, err)
	}
	if _, published, err := PublishForkCandidate(f.repo, f.identity, review.Candidate.Head, review.Candidate.Tree); err == nil || published {
		t.Fatalf("publish before repair = published %v, err %v", published, err)
	}
	taskWithCompletedChecklist(t, f.root, StateTodo, "later")
	next, err := AssignForkTask([]string{f.root}, ForkAssignmentRequest{
		AuthorityRepo: f.repo, Fork: f.identity, WorkspaceRoot: f.workspace,
		BaselineHead: review.Candidate.Head, LeaseOwner: testLeaseOwner(),
	})
	if err != nil || next.Outcome != ForkAssignmentSelected || next.Task.Item.ID != "alpha" {
		t.Fatalf("rework scheduler = %+v, %v", next, err)
	}
	if err := next.Lease.Release(); err != nil {
		t.Fatal(err)
	}

	writeTaskFile(t, filepath.Join(alpha.Owner.Projection, StateInProgress, "alpha", "artifacts", "repair.txt"), "verified repair\n")
	writeTaskFile(t, filepath.Join(f.workspace, "repair-alpha.txt"), "verified repair\n")
	git(t, f.workspace, "add", "repair-alpha.txt")
	git(t, f.workspace, "commit", "-qm", "repair reviewed finding")
	alphaProjection, _ = mustCurrentTask(t, alpha.Owner.Projection, "alpha")
	if err := MoveTaskDir(alpha.Owner.Projection, alphaProjection, StateDone); err != nil {
		t.Fatal(err)
	}
	ownerRecord, owned, err := ReadTaskOwnerRecord(alpha.Task.Root, "alpha")
	if err != nil || !owned || ownerRecord.Fork == nil {
		t.Fatalf("repaired alpha owner = %+v, owned=%v, err=%v", ownerRecord, owned, err)
	}
	if _, err := AcceptForkProjection(f.repo, alpha.Task.Root, "alpha", *ownerRecord.Fork); err != nil {
		t.Fatalf("accept repaired alpha projection: %v", err)
	}
	newHead, newTree := f.headTree()
	next, err = AssignForkTask([]string{f.root}, ForkAssignmentRequest{
		AuthorityRepo: f.repo, Fork: f.identity, WorkspaceRoot: f.workspace,
		BaselineHead: newHead, LeaseOwner: testLeaseOwner(),
	})
	if err != nil || next.Outcome != ForkAssignmentExecutorDrained || next.Lease != nil {
		t.Fatalf("reworked candidate admitted outside task: %+v, %v", next, err)
	}
	newReview, exists, err := BeginForkCandidateReview(f.repo, f.identity, newHead, newTree)
	if err != nil || !exists || !newReview.Required || newReview.Candidate.ID == review.Candidate.ID {
		t.Fatalf("post-repair review = %+v, exists=%v, err=%v", newReview, exists, err)
	}
	if err := AuthorizeForkCandidateReview(f.repo, f.identity, newReview.Candidate.ID); err != nil {
		t.Fatal(err)
	}
	candidate, published, err := PublishForkCandidate(f.repo, f.identity, newHead, newTree)
	if err != nil || !published || candidate.ID != newReview.Candidate.ID {
		t.Fatalf("publish after repair and review = %+v, published=%v, err=%v", candidate, published, err)
	}
}

func TestForkCandidateReworkRefusalsDoNotMutateCandidateOrProjection(t *testing.T) {
	for _, tc := range []struct {
		name        string
		mutate      func(t *testing.T, f candidateRoundFixture, review ForkCandidateReview)
		candidateID string
		taskID      string
		want        string
	}{
		{
			name:        "stale candidate ID",
			candidateID: strings.Repeat("f", 32),
			taskID:      "alpha",
			want:        "no longer current",
		},
		{
			name:   "changed HEAD",
			taskID: "alpha",
			mutate: func(t *testing.T, f candidateRoundFixture, _ ForkCandidateReview) {
				git(t, f.workspace, "commit", "--allow-empty", "-qm", "advance HEAD without changing tree")
			},
			want: "changed during candidate review",
		},
		{
			name:   "changed projection",
			taskID: "alpha",
			mutate: func(t *testing.T, f candidateRoundFixture, _ ForkCandidateReview) {
				writeTaskFile(t, filepath.Join(f.assignments[0].Owner.Projection, StateDone, "alpha", "state.md"), "projection changed after review\n")
			},
			want: "projection changed after review",
		},
		{
			name:   "outside subject",
			taskID: "not-in-candidate",
			want:   "not a candidate review subject",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCandidateRoundFixture(t, "alpha")
			prepareCandidateReworkProjectionState(t, f)
			commitCandidateTaskForRework(t, f, "alpha")
			review := beginCandidateReworkReview(t, f)
			if tc.mutate != nil {
				tc.mutate(t, f, review)
			}
			beforeStatus, exists, err := ReadForkCandidateRoundStatus(f.repo, f.identity)
			if err != nil || !exists {
				t.Fatalf("read candidate before refusal = %+v, exists=%v, err=%v", beforeStatus, exists, err)
			}
			beforeCandidate, exists, err := forkCandidateFingerprintValue(f.repo, f.identity)
			if err != nil || !exists {
				t.Fatalf("read candidate fingerprint before refusal: exists=%v, err=%v", exists, err)
			}
			beforeOwner, owned, err := ReadTaskOwnerRecord(f.assignments[0].Task.Root, "alpha")
			if err != nil || !owned || beforeOwner.Fork == nil {
				t.Fatalf("read owner before refusal: %+v, owned=%v, err=%v", beforeOwner, owned, err)
			}
			beforeProjection, err := os.ReadFile(filepath.Join(f.assignments[0].Owner.Projection, StateDone, "alpha", "state.md"))
			if err != nil {
				t.Fatal(err)
			}
			findingID := tc.taskID
			if findingID == "" {
				findingID = "alpha"
			}
			candidateID := tc.candidateID
			if candidateID == "" {
				candidateID = review.Candidate.ID
			}
			err = ReworkForkCandidate(f.repo, f.identity, candidateID, []ForkCandidateFinding{{
				TaskID: findingID, Gate: "unit tests", Finding: "recorded review finding",
			}})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("rework refusal = %v; want error containing %q", err, tc.want)
			}
			afterStatus, exists, err := ReadForkCandidateRoundStatus(f.repo, f.identity)
			if err != nil || !exists || afterStatus != beforeStatus {
				t.Fatalf("refusal changed candidate status: before=%+v after=%+v exists=%v err=%v", beforeStatus, afterStatus, exists, err)
			}
			afterCandidate, exists, err := forkCandidateFingerprintValue(f.repo, f.identity)
			if err != nil || !exists || !reflect.DeepEqual(afterCandidate, beforeCandidate) {
				t.Fatalf("refusal changed candidate record: before=%+v after=%+v exists=%v err=%v", beforeCandidate, afterCandidate, exists, err)
			}
			afterOwner, owned, err := ReadTaskOwnerRecord(f.assignments[0].Task.Root, "alpha")
			if err != nil || !owned || afterOwner.Fork == nil || !reflect.DeepEqual(afterOwner.Fork, beforeOwner.Fork) {
				t.Fatalf("refusal changed canonical owner: before=%+v after=%+v owned=%v err=%v", beforeOwner.Fork, afterOwner.Fork, owned, err)
			}
			afterProjection, err := os.ReadFile(filepath.Join(f.assignments[0].Owner.Projection, StateDone, "alpha", "state.md"))
			if err != nil || !reflect.DeepEqual(afterProjection, beforeProjection) {
				t.Fatalf("refusal changed projection bytes: before=%q after=%q err=%v", beforeProjection, afterProjection, err)
			}
			canonical, ok := mustCurrentTask(t, f.assignments[0].Task.Root, "alpha")
			if !ok || canonical.State != StateInProgress {
				t.Fatalf("refusal changed canonical task: %+v, exists=%v", canonical, ok)
			}
		})
	}
}

func TestForkCandidateReworkCannotAddUnownedWorkToFrozenCohort(t *testing.T) {
	f := newCandidateRoundFixture(t, "alpha")
	prepareCandidateReworkProjectionState(t, f)
	commitCandidateTaskForRework(t, f, "alpha")
	review := beginCandidateReworkReview(t, f)
	added := taskWithCompletedChecklist(t, f.root, StateTodo, "later")
	statusBefore, exists, err := ReadForkCandidateRoundStatus(f.repo, f.identity)
	if err != nil || !exists {
		t.Fatalf("candidate before outside finding = %+v, exists=%v, err=%v", statusBefore, exists, err)
	}
	err = ReworkForkCandidate(f.repo, f.identity, review.Candidate.ID, []ForkCandidateFinding{{
		TaskID: added.ID, Gate: "unit tests", Finding: "new queue item must not join this review",
	}})
	if err == nil || !strings.Contains(err.Error(), "not a candidate review subject") {
		t.Fatalf("outside frozen cohort finding = %v", err)
	}
	statusAfter, exists, err := ReadForkCandidateRoundStatus(f.repo, f.identity)
	if err != nil || !exists || statusAfter != statusBefore {
		t.Fatalf("outside finding changed candidate status: before=%+v after=%+v exists=%v err=%v", statusBefore, statusAfter, exists, err)
	}
	current, ok := mustCurrentTask(t, f.root, added.ID)
	if !ok || current.State != StateTodo {
		t.Fatalf("new unowned work changed state: %+v, exists=%v", current, ok)
	}
	if _, owned := taskOwned(t, f.root, added.ID); owned {
		t.Fatal("unowned work acquired candidate authority")
	}
	assignment, err := AssignForkTask([]string{f.root}, ForkAssignmentRequest{
		AuthorityRepo: f.repo, Fork: f.identity, WorkspaceRoot: f.workspace,
		BaselineHead: review.Candidate.Head, LeaseOwner: testLeaseOwner(),
	})
	if err != nil || assignment.Outcome != ForkAssignmentExecutorDrained || assignment.Lease != nil {
		t.Fatalf("frozen candidate admitted new work: %+v, err=%v", assignment, err)
	}
}

func TestForkCandidateReworkReplayAfterInterruptedTransitionPreservesAuthority(t *testing.T) {
	f := newCandidateRoundFixture(t, "alpha")
	prepareCandidateReworkProjectionState(t, f)
	commitCandidateTaskForRework(t, f, "alpha")
	review := beginCandidateReworkReview(t, f)
	assignment := f.assignments[0]
	finding := ForkCandidateFinding{TaskID: "alpha", Gate: "unit tests", Finding: "alpha misses an error case"}
	audit, pending, err := PrepareReviewReopen(f.workspace, assignment.Owner.Projection, "alpha", finding.Finding)
	if err != nil {
		t.Fatalf("prepare task-bound audit authority: %v", err)
	}
	state, exists, err := readForkCandidateState(f.repo, f.identity)
	if err != nil || !exists || state.manifest == nil || state.manifest.Phase != forkCandidateReviewing ||
		state.manifest.Pending.ID != review.Candidate.ID {
		t.Fatalf("candidate intent before interruption = %+v, exists=%v, err=%v", state.manifest, exists, err)
	}
	manifest := *state.manifest
	manifest.Phase = forkCandidateReworking
	manifest.Rework = &forkCandidateRework{
		CandidateID: review.Candidate.ID,
		Head:        review.Candidate.Head,
		Tasks: []forkCandidateTaskRework{{
			AssignmentID: assignment.Owner.AssignmentID, Finding: finding, Reopen: &audit, Pending: pending,
		}},
	}
	if err := writeForkCandidateManifest(f.repo, manifest); err != nil {
		t.Fatal(err)
	}

	projected, ok := mustCurrentTask(t, assignment.Owner.Projection, "alpha")
	if !ok || projected.State != StateDone {
		t.Fatalf("interrupted projection before move = %+v, exists=%v", projected, ok)
	}
	move := candidateReworkMove(assignment.Owner.Projection, projected, manifest.Rework.Tasks[0])
	move.SourceStates = []string{StateDone, StateInProgress, StateBlocked}
	if err := MoveTrustedTasksFromDoneWith([]TrustedTaskMove{move}); err != nil {
		t.Fatalf("simulate interruption after projection move: %v", err)
	}
	if _, owned, err := ReadTaskOwnerRecord(assignment.Task.Root, "alpha"); err != nil || !owned {
		t.Fatalf("owner changed before replay: owned=%v, err=%v", owned, err)
	}
	if err := ReworkForkCandidate(f.repo, f.identity, review.Candidate.ID, nil); err != nil {
		t.Fatalf("replay interrupted candidate transition: %v", err)
	}
	replayed, exists, err := ReadAuditReopenRecord(assignment.Owner.Projection, "alpha")
	if err != nil || !exists || !AuditReopenRecordsEqual(replayed, audit) {
		t.Fatalf("replay did not preserve exact audit authority: %+v, exists=%v, err=%v", replayed, exists, err)
	}
	state, exists, err = readForkCandidateState(f.repo, f.identity)
	if err != nil || !exists || state.manifest == nil || state.manifest.Phase != forkCandidateReworking ||
		len(state.manifest.Rework.Tasks) != 1 || !state.manifest.Rework.Tasks[0].Applied {
		t.Fatalf("replayed candidate transition = %+v, exists=%v, err=%v", state.manifest, exists, err)
	}
	owner, owned, err := ReadTaskOwnerRecord(assignment.Task.Root, "alpha")
	if err != nil || !owned || owner.Fork == nil || owner.Fork.Fork != f.identity ||
		owner.Fork.AssignmentID != assignment.Owner.AssignmentID || owner.Fork.Phase != ForkAssignmentPaused || owner.Fork.CandidateID != "" {
		t.Fatalf("owner after replay = %+v, owned=%v, err=%v", owner.Fork, owned, err)
	}
}

func TestForkCandidateReworkBlocksNoChangeTaskWithoutAuditCommit(t *testing.T) {
	f := newCandidateRoundFixture(t, "alpha")
	prepareCandidateReworkProjectionState(t, f)
	review := beginCandidateReworkReview(t, f)
	finding := ForkCandidateFinding{TaskID: "alpha", Gate: "required checks", Finding: "no auditable implementation commit exists"}
	if err := ReworkForkCandidate(f.repo, f.identity, review.Candidate.ID, []ForkCandidateFinding{finding}); err != nil {
		t.Fatalf("block no-change review finding: %v", err)
	}
	blocked, ok := mustCurrentTask(t, f.assignments[0].Owner.Projection, "alpha")
	if !ok || blocked.State != StateBlocked {
		t.Fatalf("no-change projection = %+v, exists=%v; want blocked", blocked, ok)
	}
	canonical, ok := mustCurrentTask(t, f.assignments[0].Task.Root, "alpha")
	if !ok || canonical.State != StateBlocked {
		t.Fatalf("canonical no-change task = %+v, exists=%v; want blocked", canonical, ok)
	}
	decision, err := os.ReadFile(filepath.Join(canonical.Dir, "decision.md"))
	if err != nil || !strings.Contains(string(decision), "could not authorize automatic rework") || !strings.Contains(string(decision), finding.Finding) {
		t.Fatalf("no-change decision = %q, err=%v", decision, err)
	}
	if _, exists, err := ReadAuditReopenRecord(f.assignments[0].Owner.Projection, "alpha"); err != nil || exists {
		t.Fatalf("no-change task received fabricated audit authority: exists=%v, err=%v", exists, err)
	}
	owner, owned, err := ReadTaskOwnerRecord(f.assignments[0].Task.Root, "alpha")
	if err != nil || !owned || owner.Fork == nil || owner.Fork.Fork != f.identity || owner.Fork.Phase != ForkAssignmentBlocked {
		t.Fatalf("blocked no-change owner = %+v, owned=%v, err=%v", owner.Fork, owned, err)
	}
	if _, published, err := PublishForkCandidate(f.repo, f.identity, review.Candidate.Head, review.Candidate.Tree); err == nil || published {
		t.Fatalf("no-change candidate published: published=%v, err=%v", published, err)
	}
}

func TestForkCandidateReworkNormalizesFindingWhitespace(t *testing.T) {
	f := newCandidateRoundFixture(t, "alpha")
	prepareCandidateReworkProjectionState(t, f)
	commitCandidateTaskForRework(t, f, "alpha")
	review := beginCandidateReworkReview(t, f)
	if err := ReworkForkCandidate(f.repo, f.identity, review.Candidate.ID, []ForkCandidateFinding{{
		TaskID: "alpha", Gate: "worker\treport\x1b[31m", Finding: "missing\tcompletion evidence\x07 " + strings.Repeat("x", TaskBlockLimit),
	}}); err != nil {
		t.Fatal(err)
	}
	item, _ := mustCurrentTask(t, f.assignments[0].Owner.Projection, "alpha")
	log, err := os.ReadFile(filepath.Join(item.Dir, "log.md"))
	if err != nil || !strings.Contains(string(log), `missing completion evidence? xxx`) || !strings.Contains(string(log), `worker report?[31m`) {
		t.Fatalf("normalized finding = %.300q, %v", log, err)
	}
	state, exists, err := readForkCandidateState(f.repo, f.identity)
	if err != nil || !exists || state.manifest.Phase != forkCandidateReworking || len(state.manifest.Rework.Tasks[0].Finding.Finding) > TaskBlockLimit {
		t.Fatalf("oversized finding was not recorded within its limit: exists=%v, err=%v", exists, err)
	}
}

func TestForkCandidateReworkRepeatedFindingNeedsDecision(t *testing.T) {
	for _, repeated := range []bool{true, false} {
		t.Run(map[bool]string{true: "same finding", false: "new finding"}[repeated], func(t *testing.T) {
			f := newCandidateRoundFixture(t, "alpha")
			prepareCandidateReworkProjectionState(t, f)
			commitCandidateTaskForRework(t, f, "alpha")
			review := beginCandidateReworkReview(t, f)
			finding := ForkCandidateFinding{TaskID: "alpha", Gate: "worker report", Finding: "missing completion evidence"}
			if err := ReworkForkCandidate(f.repo, f.identity, review.Candidate.ID, []ForkCandidateFinding{finding}); err != nil {
				t.Fatal(err)
			}
			owner, _, err := ReadTaskOwnerRecord(f.root, "alpha")
			if err != nil {
				t.Fatal(err)
			}
			projected, _ := mustCurrentTask(t, owner.Fork.Projection, "alpha")
			if err := MoveTaskDir(owner.Fork.Projection, projected, StateDone); err != nil {
				t.Fatal(err)
			}
			if _, err := AcceptForkProjection(f.repo, f.root, "alpha", *owner.Fork); err != nil {
				t.Fatal(err)
			}
			review = beginCandidateReworkReview(t, f)
			finding.Finding = "Missing\tcompletion evidence."
			want := StateBlocked
			if !repeated {
				finding.Finding, want = "another error path needs repair", StateInProgress
			}
			if err := ReworkForkCandidate(f.repo, f.identity, review.Candidate.ID, []ForkCandidateFinding{finding}); err != nil {
				t.Fatal(err)
			}
			canonical, _ := mustCurrentTask(t, f.root, "alpha")
			if canonical.State != want {
				t.Fatalf("candidate finding left %s, want %s", canonical.State, want)
			}
		})
	}
}

func TestForkCandidateReworkReplaysBlockingAcceptance(t *testing.T) {
	f := newCandidateRoundFixture(t, "alpha")
	prepareCandidateReworkProjectionState(t, f)
	review := beginCandidateReworkReview(t, f)
	if err := ReworkForkCandidate(f.repo, f.identity, review.Candidate.ID, []ForkCandidateFinding{{
		TaskID: "alpha", Gate: "worker report", Finding: "missing completion evidence",
	}}); err != nil {
		t.Fatal(err)
	}
	// Reconstruct the crash boundary after the canonical folder move but before the
	// blocking owner and the candidate's Applied bit were finalized.
	owner, _, err := ReadTaskOwnerRecord(f.root, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateForkTaskAssignment(f.root, "alpha", *owner.Fork, func(owner *ForkTaskOwner) error {
		owner.Phase = ForkAssignmentBlocking
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	state, _, err := readForkCandidateState(f.repo, f.identity)
	if err != nil {
		t.Fatal(err)
	}
	state.manifest.Rework.Tasks[0].Applied = false
	if err := writeForkCandidateManifest(f.repo, *state.manifest); err != nil {
		t.Fatal(err)
	}
	if err := ReworkForkCandidate(f.repo, f.identity, review.Candidate.ID, nil); err != nil {
		t.Fatalf("replay blocking acceptance: %v", err)
	}
	owner, _, err = ReadTaskOwnerRecord(f.root, "alpha")
	if err != nil || owner.Fork.Phase != ForkAssignmentBlocked {
		t.Fatalf("replayed owner = %+v, %v", owner, err)
	}
}

func TestForkCandidateReworkFailureKeepsBlockedProjection(t *testing.T) {
	f := newCandidateRoundFixture(t, "alpha")
	prepareCandidateReworkProjectionState(t, f)
	commitCandidateTaskForRework(t, f, "alpha")
	review := beginCandidateReworkReview(t, f)
	if err := ReworkForkCandidate(f.repo, f.identity, review.Candidate.ID, []ForkCandidateFinding{{
		TaskID: "alpha", Gate: "worker report", Finding: "missing completion evidence",
	}}); err != nil {
		t.Fatal(err)
	}
	owner, _, err := ReadTaskOwnerRecord(f.root, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	projected, _ := mustCurrentTask(t, owner.Fork.Projection, "alpha")
	if err := MoveTaskDir(owner.Fork.Projection, projected, StateBlocked); err != nil {
		t.Fatal(err)
	}
	if err := RestoreFailedForkProjection(f.repo, f.root, "alpha", *owner.Fork); err != nil {
		t.Fatal(err)
	}
	if _, err := AcceptForkProjection(f.repo, f.root, "alpha", *owner.Fork); err != nil {
		t.Fatal(err)
	}
	canonical, _ := mustCurrentTask(t, f.root, "alpha")
	projected, _ = mustCurrentTask(t, owner.Fork.Projection, "alpha")
	if canonical.State != StateBlocked || projected.State != StateBlocked {
		t.Fatalf("failure cleanup unblocked task: canonical=%s projection=%s", canonical.State, projected.State)
	}
}
