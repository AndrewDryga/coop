package tasks

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/AndrewDryga/coop/internal/forkspace"
)

func TestPauseForkAssignmentsPreservesResumableWork(t *testing.T) {
	for _, phase := range []ForkAssignmentPhase{ForkAssignmentPreparing, ForkAssignmentWorking} {
		t.Run(string(phase), func(t *testing.T) {
			repo, root, assignment := proposalAssignment(t, "pause-worker")
			id := assignment.Task.Item.ID
			projected, _ := mustCurrentTask(t, assignment.Owner.Projection, id)
			proof := filepath.Join(projected.Dir, "artifacts", "retained.txt")
			writeTaskFile(t, proof, "unfinished work\n")
			if _, err := UpdateForkTaskAssignment(root, id, assignment.Owner, func(owner *ForkTaskOwner) error { owner.Phase = phase; return nil }); err != nil {
				t.Fatal(err)
			}
			if err := PauseForkAssignments(repo, assignment.Owner.Fork); err != nil {
				t.Fatal(err)
			}
			paused, owned, err := ReadTaskOwnerRecord(root, id)
			if err != nil || !owned || paused.Fork == nil || paused.Fork.Phase != ForkAssignmentPaused || paused.Fork.AssignmentID != assignment.Owner.AssignmentID || paused.Fork.Projection != assignment.Owner.Projection {
				t.Fatalf("paused=%+v err=%v", paused, err)
			}
			assertPausePreservesOwner(t, repo, root, id, assignment.Owner.Fork)
			resumed, err := AssignForkTask([]string{root}, ForkAssignmentRequest{AuthorityRepo: repo, Fork: assignment.Owner.Fork, WorkspaceRoot: forkspace.Workspace(repo, assignment.Owner.Fork.Name), BaselineHead: assignment.Owner.BaselineHead, LeaseOwner: testLeaseOwner()})
			if err != nil || resumed.Outcome != ForkAssignmentSelected || resumed.Lease == nil || resumed.Owner.AssignmentID != assignment.Owner.AssignmentID || resumed.Owner.Projection != assignment.Owner.Projection {
				t.Fatalf("resume=%+v err=%v", resumed, err)
			}
			defer resumed.Lease.Release()
			if body, err := os.ReadFile(proof); err != nil || string(body) != "unfinished work\n" {
				t.Fatalf("lost work: %q %v", body, err)
			}
		})
	}
}

func assertPausePreservesOwner(t *testing.T, repo, root, id string, identity forkspace.Identity) {
	t.Helper()
	before, owned, err := ReadTaskOwnerRecord(root, id)
	if err != nil || !owned {
		t.Fatalf("owner before pause: %+v %v", before, err)
	}
	if err := PauseForkAssignments(repo, identity); err != nil {
		t.Fatal(err)
	}
	after, owned, err := ReadTaskOwnerRecord(root, id)
	if err != nil || !owned || !reflect.DeepEqual(before, after) {
		t.Fatalf("pause changed settled owner: before=%+v after=%+v err=%v", before, after, err)
	}
}

func TestPauseForkAssignmentsRetainsCandidatePhases(t *testing.T) {
	f := newCandidateRoundFixture(t, "alpha")
	assertPausePreservesOwner(t, f.repo, f.root, "alpha", f.identity)
	head, tree := f.headTree()
	review, exists, err := BeginForkCandidateReview(f.repo, f.identity, head, tree)
	if err != nil || !exists {
		t.Fatalf("review=%+v %v", review, err)
	}
	if err := AuthorizeForkCandidateReview(f.repo, f.identity, review.Candidate.ID); err != nil {
		t.Fatal(err)
	}
	candidate, published, err := PublishForkCandidate(f.repo, f.identity, head, tree)
	if err != nil || !published {
		t.Fatalf("publish=%+v %v", candidate, err)
	}
	assertPausePreservesOwner(t, f.repo, f.root, "alpha", f.identity)
	after, exists, err := ReadForkCandidate(f.repo, f.identity)
	if err != nil || !exists || !reflect.DeepEqual(candidate, after) {
		t.Fatalf("pause changed candidate: %+v %v", after, err)
	}
}
