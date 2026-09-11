package networkstate

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/AndrewDryga/coop/internal/egress"
)

// Forgetting removes ONE file. Everything else this host retained — the other
// project's approval, the recorded run, this host's setup record — is evidence
// of what already happened, and a revoked decision does not unmake any of it.
func TestForgetRemovesOneApprovalAndKeepsEveryOtherRecord(t *testing.T) {
	ctx := context.Background()
	smoke, _ := qualificationFixture(t)
	setup, err := smoke.Complete(smokeDomain)
	if err != nil {
		t.Fatal(err)
	}
	s := smoke.store
	forgotten, kept := t.TempDir(), t.TempDir()
	if err := approve(s, forgotten, egress.Filtered, []egress.Rule{rule("gone.example.com")}, nil); err != nil {
		t.Fatal(err)
	}
	if err := approve(s, kept, egress.Filtered, []egress.Rule{rule("stays.example.com")}, nil); err != nil {
		t.Fatal(err)
	}
	record, err := s.ApprovalAt(forgotten)
	if err != nil || record.Approval == nil || record.ID == "" {
		t.Fatalf("ApprovalAt(%s) = %+v, %v", forgotten, record, err)
	}
	id, err := s.projectID(forgotten)
	if err != nil || record.ID != id {
		t.Fatalf("the lexical id %q is not the id an approval is filed under (%q, %v)", record.ID, id, err)
	}
	before := inventory(t, s)
	removed, err := s.Forget(ctx, record)
	if err != nil || !removed {
		t.Fatalf("Forget = (%v, %v)", removed, err)
	}
	want := maps.Clone(before)
	if _, ok := want[approvalRecord(record.ID)]; !ok {
		t.Fatalf("the approval record %q was never on disk", approvalRecord(record.ID))
	}
	delete(want, approvalRecord(record.ID))
	got := inventory(t, s)
	// Removing the grant is only half of it: the withdrawal marker is what keeps
	// a project whose YAML is gone from falling back to the open default.
	barrier, ok := got[withdrawalRecord(record.ID)]
	if !ok {
		t.Fatalf("forget left no withdrawal marker:\n%v", got)
	}
	want[withdrawalRecord(record.ID)] = barrier
	if !reflect.DeepEqual(want, got) {
		t.Errorf("forget changed more than the one approval and its marker:\nwant %v\ngot  %v", want, got)
	}
	if approval, err := s.Approval(forgotten); err != nil || approval != nil {
		t.Errorf("the forgotten project still has an approval: %+v %v", approval, err)
	}
	if approval, err := s.Approval(kept); err != nil || approval == nil {
		t.Errorf("another project's approval was removed: %+v %v", approval, err)
	}
	if _, err := s.Execution(setup.Smoke.RunID); err != nil {
		t.Errorf("the recorded run did not survive a forget: %v", err)
	}
	records, err := s.Qualifications(ctx)
	if err != nil || len(records) != 1 || records[0].ID != setup.ID {
		t.Errorf("this host's setup record did not survive a forget: %+v %v", records, err)
	}
	// Forgetting twice is not an error, but it is not a second removal either.
	if removed, err := s.Forget(ctx, record); err != nil || removed {
		t.Errorf("a second forget reported (%v, %v), want no removal", removed, err)
	}
}

// The case nothing else can serve: the checkout is gone, so the id cannot be
// derived by stat-ing it, and the approval it left behind is unreachable
// through every other verb.
func TestForgetLocatesAnApprovalWhoseDirectoryIsGone(t *testing.T) {
	s := openStore(t)
	project := filepath.Join(t.TempDir(), "checkout")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := approve(s, project, egress.Filtered, []egress.Rule{rule("deleted.example.com")}, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(project); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Approval(project); err == nil {
		t.Fatal("a missing project directory was described by the launch-side reader")
	}
	record, err := s.ApprovalAt(project)
	if err != nil || record.Approval == nil || len(record.Approval.Envelope) != 1 {
		t.Fatalf("ApprovalAt on a deleted checkout = %+v, %v", record, err)
	}
	removed, err := s.Forget(context.Background(), record)
	if err != nil || !removed {
		t.Fatalf("Forget on a deleted checkout = (%v, %v)", removed, err)
	}
	if again, err := s.ApprovalAt(project); err != nil || again.Approval != nil {
		t.Fatalf("the record outlived its removal: %+v %v", again, err)
	}
}

// An approval made through a symlink was filed under the directory it pointed
// at. Once BOTH are gone that record cannot be located from the link's path —
// and the honest answer is to find nothing, never to remove a different one.
func TestForgetCannotLocateAnApprovalMadeThroughAGoneSymlink(t *testing.T) {
	s := openStore(t)
	parent := t.TempDir()
	target, link := filepath.Join(parent, "target"), filepath.Join(parent, "link")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := approve(s, link, egress.Filtered, []egress.Rule{rule("linked.example.com")}, nil); err != nil {
		t.Fatal(err)
	}
	through, err := s.ApprovalAt(link)
	if err != nil || through.Approval == nil {
		t.Fatalf("ApprovalAt through a live symlink = %+v, %v", through, err)
	}
	direct, err := s.ApprovalAt(target)
	if err != nil || direct.ID != through.ID {
		t.Fatalf("the link and its target are not one project: %q vs %q (%v)", through.ID, direct.ID, err)
	}
	for _, path := range []string{link, target} {
		if err := os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
	}
	lost, err := s.ApprovalAt(link)
	if err != nil || lost.Approval != nil {
		t.Fatalf("a gone symlink located a record: %+v %v", lost, err)
	}
	if lost.ID == direct.ID {
		t.Fatal("the lexical id of a gone symlink collided with its target's")
	}
	if removed, err := s.Forget(context.Background(), lost); err != nil || removed {
		t.Fatalf("forgetting an unlocatable record = (%v, %v), want no removal", removed, err)
	}
	// The record it could not find is still exactly where it was, and the path
	// it was actually filed under still removes it.
	surviving, err := s.ApprovalAt(target)
	if err != nil || surviving.Approval == nil {
		t.Fatalf("the target's approval was removed anyway: %+v %v", surviving, err)
	}
	if removed, err := s.Forget(context.Background(), surviving); err != nil || !removed {
		t.Fatalf("forgetting by the approved path = (%v, %v)", removed, err)
	}
}

func TestForgetNeedsTheRecordItWasReadFrom(t *testing.T) {
	s := openStore(t)
	for _, record := range []ApprovalRecord{{}, {ID: "not-hex"}, {ID: "AB"}} {
		if removed, err := s.Forget(context.Background(), record); err == nil || removed {
			t.Errorf("Forget(%+v) = (%v, %v), want a refusal", record, removed, err)
		}
	}
	if _, err := s.ApprovalAt("relative/path"); err == nil {
		t.Error("a relative project path was resolved to a record id")
	}
}
