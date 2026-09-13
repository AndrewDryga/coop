package tasks

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
)

type candidateRoundFixture struct {
	repo        string
	workspace   string
	root        string
	identity    forkspace.Identity
	assignments []ForkAssignment
}

func newCandidateRoundFixture(t *testing.T, ids ...string) candidateRoundFixture {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "project")
	workspace, identity := testAssignmentFork(t, repo, "rounds")
	git(t, workspace, "init", "-q", "-b", "main")
	writeTaskFile(t, filepath.Join(workspace, "base.txt"), "base\n")
	git(t, workspace, "add", "base.txt")
	git(t, workspace, "commit", "-qm", "base")
	root := filepath.Join(t.TempDir(), "tasks")
	for _, id := range ids {
		taskWithCompletedChecklist(t, root, StateTodo, id)
	}
	fixture := candidateRoundFixture{repo: repo, workspace: workspace, root: root, identity: identity}
	for range ids {
		assignment, err := AssignForkTask([]string{root}, ForkAssignmentRequest{
			AuthorityRepo: repo, Fork: identity, WorkspaceRoot: workspace,
			BaselineHead: gitOut(workspace, "rev-parse", "HEAD"), LeaseOwner: testLeaseOwner(),
		})
		if err != nil || assignment.Outcome != ForkAssignmentSelected {
			t.Fatalf("assign candidate task: %+v, %v", assignment, err)
		}
		if err := assignment.Lease.Release(); err != nil {
			t.Fatal(err)
		}
		projected, ok := mustCurrentTask(t, assignment.Owner.Projection, assignment.Task.Item.ID)
		if !ok {
			t.Fatalf("projection %s is missing", assignment.Task.Item.ID)
		}
		if err := MoveTaskDir(assignment.Owner.Projection, projected, StateDone); err != nil {
			t.Fatal(err)
		}
		if _, err := AcceptForkProjection(repo, root, assignment.Task.Item.ID, assignment.Owner); err != nil {
			t.Fatal(err)
		}
		fixture.assignments = append(fixture.assignments, assignment)
	}
	return fixture
}

func (f candidateRoundFixture) headTree() (string, string) {
	return gitOut(f.workspace, "rev-parse", "HEAD"), gitOut(f.workspace, "rev-parse", "HEAD^{tree}")
}

func authorizeCandidateReviewForTest(t *testing.T, repo string, identity forkspace.Identity, candidateID string) {
	t.Helper()
	unlock, err := forkspace.LockState(repo, identity.Name)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	state, exists, err := readForkCandidateState(repo, identity)
	if err != nil || !exists || state.manifest == nil || state.manifest.Phase != forkCandidateReviewing ||
		state.manifest.Pending.ID != candidateID {
		t.Fatalf("candidate review state = %+v, exists=%v, err=%v", state, exists, err)
	}
	manifest := *state.manifest
	manifest.Phase, manifest.UpdatedAt = forkCandidatePublishing, time.Now().UTC()
	if err := writeForkCandidateManifest(repo, manifest); err != nil {
		t.Fatal(err)
	}
}

func TestForkCandidateRoundsRequireFreshReviewAndRetainHistory(t *testing.T) {
	f := newCandidateRoundFixture(t, "alpha", "beta")
	head, tree := f.headTree()
	review, exists, err := BeginForkCandidateReview(f.repo, f.identity, head, tree)
	if err != nil || !exists || !review.Required || review.Round != 1 || review.PreviousRound != 0 {
		t.Fatalf("round one review = %+v, exists=%v err=%v", review, exists, err)
	}
	if _, published, err := PublishForkCandidate(f.repo, f.identity, head, tree); err == nil || published || !strings.Contains(err.Error(), "fresh final review") {
		t.Fatalf("unreviewed publish = published %v, err %v", published, err)
	}
	if err := AuthorizeForkCandidateReview(f.repo, f.identity, review.Candidate.ID); err != nil {
		t.Fatal(err)
	}
	candidate, published, err := PublishForkCandidate(f.repo, f.identity, head, tree)
	if err != nil || !published || candidate.ID != review.Candidate.ID {
		t.Fatalf("round one publication = %+v, published=%v err=%v", candidate, published, err)
	}
	refs := []forkCandidateRef{{Round: 1, ID: candidate.ID}}
	for round := uint64(2); round <= 3; round++ {
		path := filepath.Join(f.workspace, "fix-"+string(rune('0'+round))+".txt")
		writeTaskFile(t, path, "fixed\n")
		git(t, f.workspace, "add", filepath.Base(path))
		git(t, f.workspace, "commit", "-qm", "fix reviewed candidate")
		head, tree = f.headTree()
		transition, err := SupersedeStaleForkCandidate(f.repo, f.identity, head)
		if err != nil || !transition.Started || transition.PreviousRound != round-1 || transition.NextRound != round {
			t.Fatalf("round %d supersession = %+v, %v", round, transition, err)
		}
		if _, active, err := ReadForkCandidate(f.repo, f.identity); err != nil || active {
			t.Fatalf("superseding round %d stayed landable: %v, %v", round, active, err)
		}
		review, exists, err = BeginForkCandidateReview(f.repo, f.identity, head, tree)
		if err != nil || !exists || !review.Required || review.Round != round || review.PreviousRound != round-1 {
			t.Fatalf("round %d review = %+v, exists=%v err=%v", round, review, exists, err)
		}
		if err := AuthorizeForkCandidateReview(f.repo, f.identity, review.Candidate.ID); err != nil {
			t.Fatal(err)
		}
		candidate, published, err = PublishForkCandidate(f.repo, f.identity, head, tree)
		if err != nil || !published || candidate.ID != review.Candidate.ID {
			t.Fatalf("round %d publication = %+v, published=%v err=%v", round, candidate, published, err)
		}
		refs = append(refs, forkCandidateRef{Round: round, ID: candidate.ID})
	}
	for _, ref := range refs {
		if _, err := readForkCandidateRound(f.repo, f.identity, ref); err != nil {
			t.Fatalf("retained round %d: %v", ref.Round, err)
		}
	}
	for _, assignment := range candidate.Assignments {
		if err := FinalizeForkCandidateTask(f.repo, candidate, assignment); err != nil {
			t.Fatalf("finalize %s: %v", assignment.Index.Task.Ref.ID, err)
		}
	}
	if err := MarkForkCandidateLandedLocked(f.repo, candidate); err != nil {
		t.Fatal(err)
	}
	wrongReplay := candidate
	wrongReplay.Head = strings.Repeat("f", 40)
	if err := MarkForkCandidateLandedLocked(f.repo, wrongReplay); err == nil || !strings.Contains(err.Error(), "changed before finalization") {
		t.Fatalf("terminal replay accepted another candidate: %v", err)
	}
	summary, err := ReadForkTaskStateSummary(f.repo, f.identity)
	if err != nil || summary.Active() || summary.Candidate || summary.CandidatePhase != forkCandidateLanded || summary.CandidateRound != 3 {
		t.Fatalf("landed candidate summary = %+v, %v", summary, err)
	}
	unlock, err := forkspace.LockState(f.repo, f.identity.Name)
	if err != nil {
		t.Fatal(err)
	}
	err = forkspace.RemoveGenerationIfMatchesLocked(f.repo, f.identity)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		if _, err := readForkCandidateRound(f.repo, f.identity, ref); err != nil {
			t.Fatalf("round %d lost with generation cleanup: %v", ref.Round, err)
		}
	}
}

func TestForkCandidateReviewRejectsStaleSnapshotAndFreezesAssignments(t *testing.T) {
	f := newCandidateRoundFixture(t, "alpha")
	taskWithCompletedChecklist(t, f.root, StateTodo, "later")
	head, tree := f.headTree()
	review, exists, err := BeginForkCandidateReview(f.repo, f.identity, head, tree)
	if err != nil || !exists || !review.Required {
		t.Fatalf("begin review = %+v, exists=%v err=%v", review, exists, err)
	}
	next, err := AssignForkTask([]string{f.root}, ForkAssignmentRequest{
		AuthorityRepo: f.repo, Fork: f.identity, WorkspaceRoot: f.workspace,
		BaselineHead: head, LeaseOwner: testLeaseOwner(),
	})
	if err != nil || next.Outcome != ForkAssignmentExecutorDrained || next.Lease != nil {
		t.Fatalf("reviewing candidate admitted another assignment: %+v, %v", next, err)
	}
	git(t, f.workspace, "commit", "--allow-empty", "-qm", "same tree, different head")
	if err := AuthorizeForkCandidateReview(f.repo, f.identity, review.Candidate.ID); err == nil || !strings.Contains(err.Error(), "changed during") {
		t.Fatalf("same-tree HEAD mutation authorization = %v", err)
	}
	newHead, newTree := f.headTree()
	replacement, exists, err := BeginForkCandidateReview(f.repo, f.identity, newHead, newTree)
	if err != nil || !exists || !replacement.Required || replacement.Candidate.ID == review.Candidate.ID {
		t.Fatalf("replacement review = %+v, exists=%v err=%v", replacement, exists, err)
	}
	if err := AuthorizeForkCandidateReview(f.repo, f.identity, review.Candidate.ID); err == nil || !strings.Contains(err.Error(), "no longer current") {
		t.Fatalf("stale candidate ID authorization = %v", err)
	}
	projected, ok := mustCurrentTask(t, f.assignments[0].Owner.Projection, "alpha")
	if !ok {
		t.Fatal("projection disappeared")
	}
	writeTaskFile(t, filepath.Join(projected.Dir, "state.md"), "changed after review\n")
	if err := AuthorizeForkCandidateReview(f.repo, f.identity, replacement.Candidate.ID); err == nil || !strings.Contains(err.Error(), "projection changed") {
		t.Fatalf("changed projection authorization = %v", err)
	}
}

func TestForkCandidateTransitionsAllFenceOnLandIntentPresence(t *testing.T) {
	f := newCandidateRoundFixture(t, "alpha")
	head, tree := f.headTree()
	review, exists, err := BeginForkCandidateReview(f.repo, f.identity, head, tree)
	if err != nil || !exists || !review.Required {
		t.Fatalf("begin review = %+v, %v, %v", review, exists, err)
	}
	landPath := forkspace.LandIntentPath(f.repo, f.identity)
	writeLandIntent := func() {
		if err := os.WriteFile(landPath, []byte("any pending phase\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	removeLandIntent := func() {
		if err := os.Remove(landPath); err != nil {
			t.Fatal(err)
		}
	}
	writeLandIntent()
	if _, _, err := BeginForkCandidateReview(f.repo, f.identity, head, tree); err == nil || !strings.Contains(err.Error(), "unfinished merge") {
		t.Fatalf("begin crossed land intent: %v", err)
	}
	if err := AuthorizeForkCandidateReview(f.repo, f.identity, review.Candidate.ID); err == nil || !strings.Contains(err.Error(), "unfinished merge") {
		t.Fatalf("authorize crossed land intent: %v", err)
	}
	removeLandIntent()
	if err := AuthorizeForkCandidateReview(f.repo, f.identity, review.Candidate.ID); err != nil {
		t.Fatal(err)
	}
	writeLandIntent()
	if _, _, err := PublishForkCandidate(f.repo, f.identity, head, tree); err == nil || !strings.Contains(err.Error(), "unfinished merge") {
		t.Fatalf("publish crossed land intent: %v", err)
	}
	removeLandIntent()
	if _, published, err := PublishForkCandidate(f.repo, f.identity, head, tree); err != nil || !published {
		t.Fatalf("publish after land intent removal = %v, %v", published, err)
	}
	git(t, f.workspace, "commit", "--allow-empty", "-qm", "descendant")
	head, _ = f.headTree()
	writeLandIntent()
	if _, err := SupersedeStaleForkCandidate(f.repo, f.identity, head); err == nil || !strings.Contains(err.Error(), "unfinished merge") {
		t.Fatalf("supersession crossed land intent: %v", err)
	}
}

func TestForkCandidateReviewReplacementMustDescendFromReviewedRound(t *testing.T) {
	f := newCandidateRoundFixture(t, "alpha")
	head, tree := f.headTree()
	candidate, published, err := reviewAndPublishForkCandidate(t, f.repo, f.identity, head, tree)
	if err != nil || !published {
		t.Fatalf("publish round one = %+v, %v, %v", candidate, published, err)
	}
	writeTaskFile(t, filepath.Join(f.workspace, "descendant.txt"), "descendant\n")
	git(t, f.workspace, "add", "descendant.txt")
	git(t, f.workspace, "commit", "-qm", "descendant")
	descendantHead, descendantTree := f.headTree()
	if transition, err := SupersedeStaleForkCandidate(f.repo, f.identity, descendantHead); err != nil || !transition.Started {
		t.Fatalf("start supersession = %+v, %v", transition, err)
	}
	review, exists, err := BeginForkCandidateReview(f.repo, f.identity, descendantHead, descendantTree)
	if err != nil || !exists || !review.Required {
		t.Fatalf("begin descendant review = %+v, %v, %v", review, exists, err)
	}
	git(t, f.workspace, "checkout", "--orphan", "unrelated-review")
	git(t, f.workspace, "commit", "--allow-empty", "-qm", "unrelated replacement")
	unrelatedHead, unrelatedTree := f.headTree()
	if _, _, err := BeginForkCandidateReview(f.repo, f.identity, unrelatedHead, unrelatedTree); err == nil ||
		!strings.Contains(err.Error(), "no longer descends") {
		t.Fatalf("non-descendant review replacement = %v", err)
	}
	status, exists, err := ReadForkCandidateRoundStatus(f.repo, f.identity)
	if err != nil || !exists || status.Phase != forkCandidateReviewing || status.PendingID != review.Candidate.ID {
		t.Fatalf("rejected replacement changed frozen review = %+v, %v, %v", status, exists, err)
	}
}

func TestForkCandidatePartialPublicationAndSupersessionReplay(t *testing.T) {
	f := newCandidateRoundFixture(t, "alpha", "beta")
	head, tree := f.headTree()
	review, _, err := BeginForkCandidateReview(f.repo, f.identity, head, tree)
	if err != nil {
		t.Fatal(err)
	}
	authorizeCandidateReviewForTest(t, f.repo, f.identity, review.Candidate.ID)
	state, _, err := readForkCandidateState(f.repo, f.identity)
	if err != nil {
		t.Fatal(err)
	}
	manifest := *state.manifest
	record := forkCandidateRoundRecord{
		Version: forkCandidateRoundVersion, Round: 1, Candidate: *manifest.Pending,
		CreatedAt: manifest.Pending.CreatedAt,
	}
	if err := writeForkCandidateRound(f.repo, record); err != nil {
		t.Fatal(err)
	}
	first := review.Candidate.Assignments[0]
	ownerRecord, owned, err := ReadTaskOwnerRecord(first.Index.CanonicalRoot, first.Index.Task.Ref.ID)
	if err != nil || !owned || ownerRecord.Fork == nil {
		t.Fatalf("first owner = %+v, %v, %v", ownerRecord, owned, err)
	}
	expected := *ownerRecord.Fork
	if _, err := UpdateForkTaskAssignment(first.Index.CanonicalRoot, first.Index.Task.Ref.ID, expected, func(owner *ForkTaskOwner) error {
		owner.Phase, owner.CandidateID = ForkAssignmentReady, review.Candidate.ID
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	candidate, published, err := PublishForkCandidate(f.repo, f.identity, head, tree)
	if err != nil || !published {
		t.Fatalf("publication replay = %+v, %v, %v", candidate, published, err)
	}
	writeTaskFile(t, filepath.Join(f.workspace, "fix.txt"), "fix\n")
	git(t, f.workspace, "add", "fix.txt")
	git(t, f.workspace, "commit", "-qm", "fix")
	head, _ = f.headTree()
	state, _, err = readForkCandidateState(f.repo, f.identity)
	if err != nil {
		t.Fatal(err)
	}
	manifest = *state.manifest
	manifest.Phase, manifest.UpdatedAt = forkCandidateSuperseding, time.Now().UTC()
	if err := writeForkCandidateManifest(f.repo, manifest); err != nil {
		t.Fatal(err)
	}
	if err := returnForkCandidateOwnersToReview(ForkCandidate{
		Version: candidate.Version, ID: candidate.ID, Fork: candidate.Fork, Head: candidate.Head,
		Tree: candidate.Tree, Assignments: candidate.Assignments[:1], CreatedAt: candidate.CreatedAt,
	}); err != nil {
		t.Fatal(err)
	}
	transition, err := SupersedeStaleForkCandidate(f.repo, f.identity, head)
	if err != nil || transition.Started {
		t.Fatalf("supersession replay = %+v, %v", transition, err)
	}
	for _, assignment := range candidate.Assignments {
		record, owned, err := ReadTaskOwnerRecord(assignment.Index.CanonicalRoot, assignment.Index.Task.Ref.ID)
		if err != nil || !owned || record.Fork == nil || record.Fork.Phase != ForkAssignmentReviewing || record.Fork.CandidateID != "" {
			t.Fatalf("replayed owner %s = %+v, %v, %v", assignment.Index.Task.Ref.ID, record.Fork, owned, err)
		}
	}
}

func TestForkCandidateCurrentRoundRequiresItsExactPredecessor(t *testing.T) {
	f := newCandidateRoundFixture(t, "alpha")
	head, tree := f.headTree()
	if _, published, err := reviewAndPublishForkCandidate(t, f.repo, f.identity, head, tree); err != nil || !published {
		t.Fatalf("publish round one = %v, %v", published, err)
	}
	git(t, f.workspace, "commit", "--allow-empty", "-qm", "descendant")
	head, tree = f.headTree()
	if transition, err := SupersedeStaleForkCandidate(f.repo, f.identity, head); err != nil || !transition.Started {
		t.Fatalf("supersede round one = %+v, %v", transition, err)
	}
	review, exists, err := BeginForkCandidateReview(f.repo, f.identity, head, tree)
	if err != nil || !exists || !review.Required || review.Round != 2 {
		t.Fatalf("begin round two = %+v, %v, %v", review, exists, err)
	}
	if err := AuthorizeForkCandidateReview(f.repo, f.identity, review.Candidate.ID); err != nil {
		t.Fatal(err)
	}
	if _, published, err := PublishForkCandidate(f.repo, f.identity, head, tree); err != nil || !published {
		t.Fatalf("publish round two = %v, %v", published, err)
	}
	ref := forkCandidateRef{Round: 2, ID: review.Candidate.ID}
	record, err := readForkCandidateRound(f.repo, f.identity, ref)
	if err != nil {
		t.Fatal(err)
	}
	record.Supersedes = strings.Repeat("f", 32)
	body, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(forkCandidateHistoryDir(f.repo, f.identity), forkCandidateRoundName(ref))
	if err := os.WriteFile(path, append(body, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadForkCandidate(f.repo, f.identity); err == nil || !strings.Contains(err.Error(), "previous fork candidate round") {
		t.Fatalf("rewired round history = %v", err)
	}
}

func TestDiscardForkCandidateStateRetainsOnlyReviewedRounds(t *testing.T) {
	t.Run("unreviewed attempt is dropped", func(t *testing.T) {
		f := newCandidateRoundFixture(t, "alpha")
		head, tree := f.headTree()
		review, exists, err := BeginForkCandidateReview(f.repo, f.identity, head, tree)
		if err != nil || !exists || !review.Required {
			t.Fatalf("begin review = %+v, %v, %v", review, exists, err)
		}
		unlock, err := forkspace.LockState(f.repo, f.identity.Name)
		if err != nil {
			t.Fatal(err)
		}
		err = DiscardForkCandidateStateLocked(f.repo, f.identity)
		unlock()
		if err != nil {
			t.Fatal(err)
		}
		status, exists, err := ReadForkCandidateRoundStatus(f.repo, f.identity)
		if err != nil || !exists || status.Phase != forkCandidateDiscarded || status.CurrentRound != 0 ||
			status.PendingRound != 0 || status.TerminalID != review.Candidate.ID {
			t.Fatalf("discarded unreviewed state = %+v, %v, %v", status, exists, err)
		}
		if _, err := os.Lstat(forkCandidateHistoryDir(f.repo, f.identity)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unreviewed attempt entered immutable history: %v", err)
		}
	})

	t.Run("authorized attempt is retained", func(t *testing.T) {
		f := newCandidateRoundFixture(t, "alpha")
		head, tree := f.headTree()
		review, exists, err := BeginForkCandidateReview(f.repo, f.identity, head, tree)
		if err != nil || !exists || !review.Required {
			t.Fatalf("begin review = %+v, %v, %v", review, exists, err)
		}
		authorizeCandidateReviewForTest(t, f.repo, f.identity, review.Candidate.ID)
		unlock, err := forkspace.LockState(f.repo, f.identity.Name)
		if err != nil {
			t.Fatal(err)
		}
		err = DiscardForkCandidateStateLocked(f.repo, f.identity)
		unlock()
		if err != nil {
			t.Fatal(err)
		}
		status, exists, err := ReadForkCandidateRoundStatus(f.repo, f.identity)
		if err != nil || !exists || status.Phase != forkCandidateDiscarded || status.CurrentRound != 1 ||
			status.CurrentID != review.Candidate.ID || status.TerminalID != review.Candidate.ID {
			t.Fatalf("discarded reviewed state = %+v, %v, %v", status, exists, err)
		}
		if record, err := readForkCandidateRound(f.repo, f.identity, forkCandidateRef{Round: 1, ID: review.Candidate.ID}); err != nil ||
			!reflect.DeepEqual(record.Candidate, review.Candidate) {
			t.Fatalf("retained reviewed round = %+v, %v", record, err)
		}
	})
}

func TestForkDiscardReplayRecognizesDroppedUnreviewedAttempt(t *testing.T) {
	f := newCandidateRoundFixture(t, "alpha")
	head, tree := f.headTree()
	review, exists, err := BeginForkCandidateReview(f.repo, f.identity, head, tree)
	if err != nil || !exists || !review.Required {
		t.Fatalf("begin review = %+v, %v, %v", review, exists, err)
	}
	indexes, problems := IndexedForkAssignments(f.repo, f.identity)
	if len(problems) != 0 || len(indexes) != 1 {
		t.Fatalf("candidate indexes = %+v, %v", indexes, problems)
	}
	if err := writeForkDiscard(f.repo, forkDiscardIntent{
		Version: forkDiscardVersion, Fork: f.identity, Assignments: indexes,
		CandidateID: review.Candidate.ID, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	for _, index := range indexes {
		if err := discardForkAssignment(f.repo, f.identity, index); err != nil {
			t.Fatal(err)
		}
	}
	if err := DiscardForkCandidateStateLocked(f.repo, f.identity); err != nil {
		t.Fatal(err)
	}
	if err := DiscardForkTaskStateLocked(f.repo, f.identity); err != nil {
		t.Fatalf("replay after terminal candidate write: %v", err)
	}
	if _, pending, err := readForkDiscard(f.repo, f.identity); err != nil || pending {
		t.Fatalf("discard journal after replay = pending %v, err %v", pending, err)
	}
}

func TestForkCandidateLegacyAdoptionAndStrictStateRefusals(t *testing.T) {
	f := newCandidateRoundFixture(t, "alpha")
	if err := validateForkCandidateManifest(forkCandidateManifest{
		Version: forkCandidateManifestVersion, Fork: f.identity, Phase: forkCandidateLanded, UpdatedAt: time.Now().UTC(),
	}, f.identity); err == nil {
		t.Fatal("landed candidate manifest without reviewed history was accepted")
	}
	head, tree := f.headTree()
	candidate, published, err := reviewAndPublishForkCandidate(t, f.repo, f.identity, head, tree)
	if err != nil || !published {
		t.Fatal(err)
	}
	body, err := json.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(ForkCandidatePath(f.repo, f.identity)); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(forkCandidateHistoryDir(f.repo, f.identity)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ForkCandidatePath(f.repo, f.identity), append(body, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(forkspace.LandIntentPath(f.repo, f.identity), []byte("malformed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := BeginForkCandidateReview(f.repo, f.identity, head, tree); err == nil || !strings.Contains(err.Error(), "unfinished merge") {
		t.Fatalf("legacy adoption crossed land intent: %v", err)
	}
	if err := os.Remove(forkspace.LandIntentPath(f.repo, f.identity)); err != nil {
		t.Fatal(err)
	}
	review, exists, err := BeginForkCandidateReview(f.repo, f.identity, head, tree)
	if err != nil || !exists || review.Required || review.Candidate.ID != candidate.ID || review.Round != 1 {
		t.Fatalf("legacy adoption = %+v, exists=%v err=%v", review, exists, err)
	}
	if _, err := readForkCandidateRound(f.repo, f.identity, forkCandidateRef{Round: 1, ID: candidate.ID}); err != nil {
		t.Fatalf("adopted history: %v", err)
	}
	if err := os.WriteFile(ForkCandidatePath(f.repo, f.identity), []byte("{\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadForkCandidate(f.repo, f.identity); err == nil {
		t.Fatal("malformed candidate state was accepted")
	}
	oversized, err := os.OpenFile(ForkCandidatePath(f.repo, f.identity), os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := oversized.Truncate(forkCandidateLimit + 1); err != nil {
		oversized.Close()
		t.Fatal(err)
	}
	if err := oversized.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadForkCandidate(f.repo, f.identity); err == nil || !strings.Contains(err.Error(), "bounded single-link") {
		t.Fatalf("oversized candidate state = %v", err)
	}
	unknown := []byte(`{"version":99}` + "\n")
	if err := os.WriteFile(ForkCandidatePath(f.repo, f.identity), unknown, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadForkCandidate(f.repo, f.identity); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("unknown candidate state = %v", err)
	}
	outside := filepath.Join(t.TempDir(), "candidate")
	if err := os.WriteFile(outside, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(ForkCandidatePath(f.repo, f.identity)); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, ForkCandidatePath(f.repo, f.identity)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadForkCandidate(f.repo, f.identity); err == nil || !strings.Contains(err.Error(), "single-link regular") {
		t.Fatalf("hardlinked candidate state = %v", err)
	}
	if err := os.Remove(ForkCandidatePath(f.repo, f.identity)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, ForkCandidatePath(f.repo, f.identity)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadForkCandidate(f.repo, f.identity); err == nil || !strings.Contains(err.Error(), "single-link regular") {
		t.Fatalf("symlink candidate state = %v", err)
	}
	if err := os.Remove(ForkCandidatePath(f.repo, f.identity)); err != nil {
		t.Fatal(err)
	}
}

func TestForkCandidateRecoversLegacyLandAfterCandidatePointerRemoval(t *testing.T) {
	f := newCandidateRoundFixture(t, "alpha")
	head, tree := f.headTree()
	candidate, published, err := reviewAndPublishForkCandidate(t, f.repo, f.identity, head, tree)
	if err != nil || !published {
		t.Fatalf("publish fixture = %v, %v", published, err)
	}
	if err := os.Remove(ForkCandidatePath(f.repo, f.identity)); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(forkCandidateHistoryDir(f.repo, f.identity)); err != nil {
		t.Fatal(err)
	}
	if err := MarkForkCandidateLandedLocked(f.repo, candidate); err != nil {
		t.Fatal(err)
	}
	status, exists, err := ReadForkCandidateRoundStatus(f.repo, f.identity)
	if err != nil || !exists || status.Phase != forkCandidateLanded || status.CurrentRound != 1 ||
		status.CurrentID != candidate.ID || status.TerminalID != candidate.ID {
		t.Fatalf("recovered terminal candidate = %+v, exists=%v err=%v", status, exists, err)
	}
}
