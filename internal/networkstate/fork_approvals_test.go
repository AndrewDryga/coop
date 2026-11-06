package networkstate

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/egress"
)

func forkApprovalDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSnapshotForkApprovalsSharingALockStripe(t *testing.T) {
	s := openStore(t)
	parent, home := forkApprovalDir(t), forkApprovalDir(t)
	parentID, err := s.projectID(parent)
	if err != nil {
		t.Fatal(err)
	}
	stripe := func(id string) byte {
		value, _ := hex.DecodeString(id[:2])
		return value[0] % 64
	}
	var fork string
	for n := range 10000 {
		candidate := filepath.Join(home, fmt.Sprintf("fork-%d", n))
		if stripe(s.projectKey(candidate)) == stripe(parentID) {
			fork = candidate
			break
		}
	}
	if fork == "" {
		t.Fatal("no colliding fixture name found")
	}
	if err := os.Mkdir(fork, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := approve(s, parent, egress.Filtered, []egress.Rule{rule("approved.example")}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SnapshotForkApprovals(t.Context(), parent, fork, "coop-fork-filtered"); err != nil {
		t.Fatalf("parent and fork sharing one approval lock: %v", err)
	}
}

func TestSnapshotForkApprovalsRetiresPreviousImageGrants(t *testing.T) {
	s := openStore(t)
	parent, fork := forkApprovalDir(t), forkApprovalDir(t)
	inputs, image := strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	for _, tag := range []string{"coop-fork-filtered:first", "coop-fork-filtered:second", "coop-unrelated-filtered:first"} {
		if err := s.ApproveProjectBuild(tag, inputs, image); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SnapshotForkApprovals(t.Context(), parent, fork, "coop-fork-filtered"); err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"coop-fork-filtered:first", "coop-fork-filtered:second"} {
		if got := s.ApprovedProjectBuild(tag, inputs); got != "" {
			t.Errorf("new checkout retained previous fork image grant %s: %s", tag, got)
		}
	}
	if got := s.ApprovedProjectBuild("coop-unrelated-filtered:first", inputs); got != image {
		t.Fatalf("another project's image grant changed: %s", got)
	}
}

func TestSnapshotForkApprovalCopiesFrozenRulesAndDoesNotInheritLaterChanges(t *testing.T) {
	s := openStore(t)
	parent, fork, unrelated := forkApprovalDir(t), forkApprovalDir(t), forkApprovalDir(t)
	first := []egress.Rule{rule("first.example.com")}
	unrelatedRules := []egress.Rule{rule("unrelated.example.com")}
	if err := approve(s, parent, egress.Filtered, first, nil); err != nil {
		t.Fatal(err)
	}
	if err := approve(s, unrelated, egress.Filtered, unrelatedRules, nil); err != nil {
		t.Fatal(err)
	}
	parentBefore, err := s.Approval(parent)
	if err != nil {
		t.Fatal(err)
	}
	unrelatedBefore, err := s.Approval(unrelated)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.SnapshotForkApprovals(t.Context(), parent, fork, "coop-fork-filtered"); err != nil {
		t.Fatal(err)
	}
	child, err := s.Approval(fork)
	if err != nil || child == nil {
		t.Fatalf("fork approval = %+v, %v", child, err)
	}
	if !reflect.DeepEqual(child.Envelope, first) || child.Posture != parentBefore.Posture {
		t.Fatalf("fork approval = %+v; want the rules and posture approved for the parent", child)
	}
	if child.ProjectID == parentBefore.ProjectID || child.ProjectAnchor == parentBefore.ProjectAnchor {
		t.Fatalf("fork and parent share project identity: parent=%+v fork=%+v", parentBefore, child)
	}
	if _, err := s.Admit(fork, Admission{Requests: first}); err != nil {
		t.Fatalf("fork could not use the rules approved at creation: %v", err)
	}
	changed := []egress.Rule{rule("later.example.com")}
	if _, err := s.Admit(fork, Admission{Requests: changed}); !refusedWith(err, "more than what was approved") {
		t.Fatalf("changed fork rules were not refused by the copied approval: %v", err)
	}
	if got, err := s.Approval(parent); err != nil || !reflect.DeepEqual(got, parentBefore) {
		t.Fatalf("snapshot changed the parent approval: got %+v, err %v; want %+v", got, err, parentBefore)
	}
	if got, err := s.Approval(unrelated); err != nil || !reflect.DeepEqual(got, unrelatedBefore) {
		t.Fatalf("snapshot changed an unrelated approval: got %+v, err %v; want %+v", got, err, unrelatedBefore)
	}

	later := []egress.Rule{first[0], changed[0]}
	if err := approve(s, parent, egress.Filtered, later, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Admit(parent, Admission{Requests: later}); err != nil {
		t.Fatalf("later parent approval was not usable by the parent: %v", err)
	}
	if _, err := s.Admit(fork, Admission{Requests: later}); !refusedWith(err, "more than what was approved") {
		t.Fatalf("later parent approval expanded the old fork snapshot: %v", err)
	}
	if got, err := s.Approval(fork); err != nil || !reflect.DeepEqual(got, child) {
		t.Fatalf("later parent approval changed the fork grant: got %+v, err %v; want %+v", got, err, child)
	}
}

func TestSnapshotForkApprovalPreservesMissingAndWithdrawnAuthority(t *testing.T) {
	t.Run("missing approval stays absent", func(t *testing.T) {
		s := openStore(t)
		parent, fork := forkApprovalDir(t), forkApprovalDir(t)
		request := []egress.Rule{rule("never-approved.example.com")}
		if err := s.SnapshotForkApprovals(t.Context(), parent, fork, "coop-fork-filtered"); err != nil {
			t.Fatal(err)
		}
		if got, err := s.Approval(fork); err != nil || got != nil {
			t.Fatalf("missing parent approval became a fork grant: %+v, %v", got, err)
		}
		if _, err := os.Lstat(filepath.Join(fork, ProjectApprovalMarker)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing parent approval left a fork marker: %v", err)
		}
		if _, err := s.Admit(fork, Admission{Requests: request}); !refusedWith(err, "egress rules were never approved") {
			t.Fatalf("missing parent approval did not stay absent: %v", err)
		}
	})

	t.Run("withdrawal stays refused", func(t *testing.T) {
		s := openStore(t)
		parent, fork := forkApprovalDir(t), forkApprovalDir(t)
		if err := approve(s, parent, egress.Filtered, []egress.Rule{rule("withdrawn.example.com")}, nil); err != nil {
			t.Fatal(err)
		}
		withdraw(t, s, parent)
		if err := s.SnapshotForkApprovals(t.Context(), parent, fork, "coop-fork-filtered"); err != nil {
			t.Fatal(err)
		}
		for name, project := range map[string]string{"parent": parent, "fork": fork} {
			withdrawn, err := s.Withdrawn(project)
			if err != nil || !withdrawn {
				t.Errorf("%s withdrawal = %v, %v; want a refusal barrier", name, withdrawn, err)
			}
			if _, err := s.Admit(project, Admission{}); !refusedWith(err, "withdrawn") {
				t.Errorf("%s launch crossed the withdrawal barrier: %v", name, err)
			}
		}
		if got, err := s.Approval(fork); err != nil || got != nil {
			t.Fatalf("withdrawn parent grant remained usable in the fork: %+v, %v", got, err)
		}
	})
}

func TestSnapshotForkApprovalPublishesInheritedWithdrawalBeforeRetiringApproval(t *testing.T) {
	s := openStore(t)
	root := forkApprovalDir(t)
	parent := filepath.Join(root, "parent")
	fork := filepath.Join(root, "fork")
	retired := filepath.Join(root, "retired-fork")
	for _, dir := range []string{parent, fork} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	parentRules := []egress.Rule{rule("parent.example.com")}
	forkRules := []egress.Rule{rule("fork.example.com")}
	if err := approve(s, parent, egress.Filtered, parentRules, nil); err != nil {
		t.Fatal(err)
	}
	if err := approve(s, fork, egress.Filtered, forkRules, nil); err != nil {
		t.Fatal(err)
	}
	withdraw(t, s, parent)
	forkID, err := s.projectID(fork)
	if err != nil {
		t.Fatal(err)
	}
	approvalName := approvalRecord(forkID)
	approvalBefore, err := s.read(approvalName)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(fork, retired); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(fork, 0o700); err != nil {
		t.Fatal(err)
	}

	failure := errors.New("injected inherited withdrawal publish failure")
	observed := false
	s.afterPublish = func(name string) error {
		if name != withdrawalRecord(forkID) {
			return nil
		}
		observed = true
		got, err := s.read(approvalName)
		if err != nil || !bytes.Equal(got, approvalBefore) {
			t.Errorf("child approval during inherited withdrawal publish = %q, %v; want unchanged record %q", got, err, approvalBefore)
		}
		return failure
	}
	t.Cleanup(func() { s.afterPublish = nil })

	err = s.SnapshotForkApprovals(t.Context(), parent, fork, "coop-fork-filtered")
	if !errors.Is(err, failure) || !observed {
		t.Fatalf("snapshot error = %v, callback observed = %v; want injected withdrawal publish failure", err, observed)
	}
	if withdrawn, err := s.Withdrawn(fork); err != nil || !withdrawn {
		t.Fatalf("child withdrawal after failure = %v, %v; want durable refusal barrier", withdrawn, err)
	}
	if got, err := s.read(approvalName); err != nil || !bytes.Equal(got, approvalBefore) {
		t.Fatalf("child approval after failure = %q, %v; want unchanged record %q", got, err, approvalBefore)
	}
	if withdrawn, err := s.Withdrawn(parent); err != nil || !withdrawn {
		t.Fatalf("parent withdrawal after snapshot failure = %v, %v; want it to remain", withdrawn, err)
	}
	if _, err := s.Admit(parent, Admission{}); !refusedWith(err, "withdrawn") {
		t.Fatalf("parent admission crossed its withdrawal after snapshot failure: %v", err)
	}
}

func TestSnapshotForkApprovalPreservesDestinationWithdrawalWithApprovedParent(t *testing.T) {
	s := openStore(t)
	root := forkApprovalDir(t)
	parent := filepath.Join(root, "parent")
	fork := filepath.Join(root, "fork")
	retired := filepath.Join(root, "retired-fork")
	for _, dir := range []string{parent, fork} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	parentRules := []egress.Rule{rule("parent.example.com")}
	forkRules := []egress.Rule{rule("fork.example.com")}
	if err := approve(s, parent, egress.Filtered, parentRules, nil); err != nil {
		t.Fatal(err)
	}
	if err := approve(s, fork, egress.Filtered, forkRules, nil); err != nil {
		t.Fatal(err)
	}
	withdraw(t, s, fork)
	if err := os.Rename(fork, retired); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(fork, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := s.SnapshotForkApprovals(t.Context(), parent, fork, "coop-fork-filtered"); err != nil {
		t.Fatal(err)
	}
	if withdrawn, err := s.Withdrawn(fork); err != nil || !withdrawn {
		t.Fatalf("child withdrawal after snapshot = %v, %v; want it to remain", withdrawn, err)
	}
	if _, err := s.Admit(fork, Admission{Requests: forkRules}); !refusedWith(err, "withdrawn") {
		t.Fatalf("recreated child admission crossed its withdrawal barrier: %v", err)
	}
	if _, err := s.Admit(parent, Admission{Requests: parentRules}); err != nil {
		t.Fatalf("parent approval was not usable after snapshot: %v", err)
	}
}

func TestSnapshotForkApprovalRefusesMarkersWithoutChangingParentAuthority(t *testing.T) {
	for _, kind := range []string{"copied parent marker", "replaced parent marker", "preexisting fork marker"} {
		t.Run(kind, func(t *testing.T) {
			s := openStore(t)
			parent, fork, other := forkApprovalDir(t), forkApprovalDir(t), forkApprovalDir(t)
			requests := []egress.Rule{rule("parent.example.com")}
			if err := approve(s, parent, egress.Filtered, requests, nil); err != nil {
				t.Fatal(err)
			}
			parentApproval, err := s.Approval(parent)
			if err != nil {
				t.Fatal(err)
			}
			parentMarker := filepath.Join(parent, ProjectApprovalMarker)
			parentMarkerBody, err := os.ReadFile(parentMarker)
			if err != nil {
				t.Fatal(err)
			}

			switch kind {
			case "copied parent marker":
				if err := os.WriteFile(filepath.Join(fork, ProjectApprovalMarker), parentMarkerBody, 0o600); err != nil {
					t.Fatal(err)
				}
			case "replaced parent marker":
				if err := approve(s, other, egress.Filtered, []egress.Rule{rule("other.example.com")}, nil); err != nil {
					t.Fatal(err)
				}
				foreign, err := os.ReadFile(filepath.Join(other, ProjectApprovalMarker))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(parentMarker, foreign, 0o600); err != nil {
					t.Fatal(err)
				}
			case "preexisting fork marker":
				if _, err := s.ReviewApproval(fork, egress.Filtered, []egress.Rule{rule("fork-review.example.com")}, nil, nil); err != nil {
					t.Fatal(err)
				}
			}

			err = s.SnapshotForkApprovals(t.Context(), parent, fork, "coop-fork-filtered")
			want := "new fork already has a network approval marker"
			if kind == "replaced parent marker" {
				want = "was replaced since it was approved"
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("snapshot with %s returned %v, want refusal containing %q", kind, err, want)
			}
			if kind == "replaced parent marker" {
				if err := os.WriteFile(parentMarker, parentMarkerBody, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := s.Approval(parent); err != nil || !reflect.DeepEqual(got, parentApproval) {
				t.Fatalf("refused snapshot changed parent authority: got %+v, err %v; want %+v", got, err, parentApproval)
			}
			if _, err := s.Admit(parent, Admission{Requests: requests}); err != nil {
				t.Fatalf("parent approval was not usable after the refused snapshot: %v", err)
			}
			if got, err := s.Approval(fork); err != nil || got != nil {
				t.Fatalf("refused snapshot left a fork grant: %+v, %v", got, err)
			}
		})
	}
}

func TestSnapshotForkApprovalRecreatedPathClearsStaleBuildReview(t *testing.T) {
	for _, copyParentReview := range []bool{false, true} {
		name := "parent has no build review"
		if copyParentReview {
			name = "copy only the current parent review"
		}
		t.Run(name, func(t *testing.T) {
			s := openStore(t)
			parent := filepath.Join(forkApprovalDir(t), "parent")
			fork := filepath.Join(forkApprovalDir(t), "fork")
			retired := filepath.Join(filepath.Dir(fork), "retired-fork")
			for _, dir := range []string{parent, fork} {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := approve(s, parent, egress.Filtered, []egress.Rule{rule("parent.example.com")}, nil); err != nil {
				t.Fatal(err)
			}
			if err := approve(s, fork, egress.Filtered, []egress.Rule{rule("stale-fork.example.com")}, nil); err != nil {
				t.Fatal(err)
			}
			staleReview := strings.Repeat("a", 64)
			if err := s.RecordProjectBuildReview(fork, staleReview, nil); err != nil {
				t.Fatal(err)
			}
			if copyParentReview {
				parentReview := strings.Repeat("b", 64)
				if err := s.RecordProjectBuildReview(parent, parentReview, nil); err != nil {
					t.Fatal(err)
				}
			}

			if err := os.Rename(fork, retired); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(fork, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := s.SnapshotForkApprovals(t.Context(), parent, fork, "coop-fork-filtered"); err != nil {
				t.Fatal(err)
			}
			if s.ProjectBuildReviewed(fork, staleReview) {
				t.Fatal("recreated fork path retained its old build review")
			}
			if copyParentReview {
				if !s.ProjectBuildReviewed(fork, strings.Repeat("b", 64)) {
					t.Fatal("the current parent build review was not copied")
				}
			} else if s.ProjectBuildReviewed(fork, strings.Repeat("b", 64)) {
				t.Fatal("a missing parent build review became a fork review")
			}
			parentApproval, err := s.Approval(parent)
			if err != nil {
				t.Fatal(err)
			}
			forkApproval, err := s.Approval(fork)
			if err != nil || forkApproval == nil || forkApproval.ProjectAnchor == parentApproval.ProjectAnchor {
				t.Fatalf("recreated fork identity = %+v, %v; parent = %+v", forkApproval, err, parentApproval)
			}
			if _, err := s.Admit(fork, Admission{Requests: []egress.Rule{rule("stale-fork.example.com")}}); !refusedWith(err, "more than what was approved") {
				t.Fatalf("recreated fork inherited its former network grant: %v", err)
			}
		})
	}
}

func TestSnapshotForkApprovalPublishFailureLeavesNoChildAuthority(t *testing.T) {
	s := openStore(t)
	parent, fork, unrelated := forkApprovalDir(t), forkApprovalDir(t), forkApprovalDir(t)
	requests := []egress.Rule{rule("parent.example.com")}
	if err := approve(s, parent, egress.Filtered, requests, nil); err != nil {
		t.Fatal(err)
	}
	if err := approve(s, unrelated, egress.Filtered, []egress.Rule{rule("unrelated.example.com")}, nil); err != nil {
		t.Fatal(err)
	}
	parentBefore, err := s.Approval(parent)
	if err != nil {
		t.Fatal(err)
	}
	unrelatedBefore, err := s.Approval(unrelated)
	if err != nil {
		t.Fatal(err)
	}
	forkID, err := s.projectID(fork)
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("injected destination approval publish failure")
	observed := false
	s.afterPublish = func(name string) error {
		if name != approvalRecord(forkID) {
			return nil
		}
		observed = true
		if got, err := s.Approval(fork); err != nil || got == nil {
			t.Errorf("failure injection did not observe the published child grant: %+v, %v", got, err)
		}
		return failure
	}
	err = s.SnapshotForkApprovals(t.Context(), parent, fork, "coop-fork-filtered")
	s.afterPublish = nil
	if !errors.Is(err, failure) || !observed {
		t.Fatalf("snapshot error = %v, callback observed = %v; want injected publish failure", err, observed)
	}
	if got, err := s.Approval(fork); err != nil || got != nil {
		t.Fatalf("failed snapshot left a usable child grant: %+v, %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(fork, ProjectApprovalMarker)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed snapshot left a child marker: %v", err)
	}
	if _, err := s.Admit(fork, Admission{Requests: requests}); !refusedWith(err, "egress rules were never approved") {
		t.Fatalf("failed snapshot left usable authority: %v", err)
	}
	if got, err := s.Approval(parent); err != nil || !reflect.DeepEqual(got, parentBefore) {
		t.Fatalf("failed snapshot changed parent authority: got %+v, err %v; want %+v", got, err, parentBefore)
	}
	if got, err := s.Approval(unrelated); err != nil || !reflect.DeepEqual(got, unrelatedBefore) {
		t.Fatalf("failed snapshot changed unrelated authority: got %+v, err %v; want %+v", got, err, unrelatedBefore)
	}
}

func TestRetireForkApprovalsLeavesTheParentAndWithdrawals(t *testing.T) {
	t.Run("grants", func(t *testing.T) {
		s := openStore(t)
		parent, fork := forkApprovalDir(t), forkApprovalDir(t)
		rules := []egress.Rule{rule("approved.example.com")}
		if err := approve(s, parent, egress.Filtered, rules, nil); err != nil {
			t.Fatal(err)
		}
		review := strings.Repeat("c", 64)
		if err := s.RecordProjectBuildReview(parent, review, nil); err != nil {
			t.Fatal(err)
		}
		if err := s.SnapshotForkApprovals(t.Context(), parent, fork, "coop-fork-filtered"); err != nil {
			t.Fatal(err)
		}
		inputs, image := strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
		for _, tag := range []string{"coop-fork-filtered:built", "coop-parent-filtered:built"} {
			if err := s.ApproveProjectBuild(tag, inputs, image); err != nil {
				t.Fatal(err)
			}
		}
		parentBefore, err := s.Approval(parent)
		if err != nil {
			t.Fatal(err)
		}
		child, err := s.Approval(fork)
		if err != nil || child == nil || !s.ProjectBuildReviewed(fork, review) {
			t.Fatalf("fork snapshot = %+v, %v; want the parent's network and build grants", child, err)
		}

		for range 2 { // a retry after a finished retirement finds nothing left and succeeds
			if err := s.RetireForkApprovals(t.Context(), fork, "coop-fork-filtered"); err != nil {
				t.Fatal(err)
			}
		}
		if got, err := s.Approval(fork); err != nil || got != nil {
			t.Fatalf("retired fork approval = %+v, %v; want none", got, err)
		}
		if _, err := s.Admit(fork, Admission{Requests: rules}); err == nil {
			t.Fatal("a retired fork was still admitted")
		}
		if _, err := os.Lstat(filepath.Join(fork, ProjectApprovalMarker)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("retired fork kept its approval marker: %v", err)
		}
		if _, err := s.root.Lstat(projectAnchorName(child.ProjectID, child.ProjectAnchor)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("retired fork kept its private anchor: %v", err)
		}
		if s.ProjectBuildReviewed(fork, review) || s.ApprovedProjectBuild("coop-fork-filtered:built", inputs) != "" {
			t.Error("retired fork kept its build review or image grant")
		}
		if got, err := s.Approval(parent); err != nil || !reflect.DeepEqual(got, parentBefore) {
			t.Fatalf("retirement changed the parent approval: %+v, %v", got, err)
		}
		if _, err := s.Admit(parent, Admission{Requests: rules}); err != nil {
			t.Fatalf("the parent lost its approval: %v", err)
		}
		if !s.ProjectBuildReviewed(parent, review) || s.ApprovedProjectBuild("coop-parent-filtered:built", inputs) != image {
			t.Error("retirement changed the parent's build review or image grant")
		}
	})

	t.Run("withdrawal stays", func(t *testing.T) {
		s := openStore(t)
		parent, fork := forkApprovalDir(t), forkApprovalDir(t)
		if err := approve(s, parent, egress.Filtered, []egress.Rule{rule("withdrawn.example.com")}, nil); err != nil {
			t.Fatal(err)
		}
		withdraw(t, s, parent)
		if err := s.SnapshotForkApprovals(t.Context(), parent, fork, "coop-fork-filtered"); err != nil {
			t.Fatal(err)
		}
		if err := s.RetireForkApprovals(t.Context(), fork, "coop-fork-filtered"); err != nil {
			t.Fatal(err)
		}
		for name, project := range map[string]string{"parent": parent, "fork": fork} {
			if withdrawn, err := s.Withdrawn(project); err != nil || !withdrawn {
				t.Errorf("%s withdrawal after retirement = %v, %v; want the refusal kept", name, withdrawn, err)
			}
		}
	})
}
