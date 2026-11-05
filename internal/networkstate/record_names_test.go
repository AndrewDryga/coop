package networkstate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/randid"
)

// legacyProjectRecords moves an approved project's records under the keyed id builds up to v11.3.0
// gave it: the approval, in the version they wrote, its private anchor, and the anchor's body,
// which is the marker's too.
func legacyProjectRecords(t *testing.T, s *Store, project string) string {
	t.Helper()
	canonical, err := canonicalPath(project)
	if err != nil {
		t.Fatal(err)
	}
	id, legacy := s.projectKey(canonical), s.keyedProjectID(canonical)
	approval, err := s.approval(id)
	if err != nil || approval == nil {
		t.Fatalf("approval = %v, %v", approval, err)
	}
	approval.Version, approval.ProjectID = 3, legacy
	data, err := json.Marshal(approval)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Path(), approvalRecord(legacy)), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(s.Path(), approvalRecord(id))); err != nil {
		t.Fatal(err)
	}
	anchor := filepath.Join(s.Path(), projectAnchorName(legacy, approval.ProjectAnchor))
	if err := os.Rename(filepath.Join(s.Path(), projectAnchorName(id, approval.ProjectAnchor)), anchor); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(anchor)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(anchor, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(anchor, projectAnchorBody(legacy, approval.ProjectAnchor), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(anchor, info.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
	return legacy
}

// A review digest is keyed with this store's key, so the plain hash of the very review it covers,
// the most anyone without the key can compute, never approves; the digest itself does.
func TestApprovalDigestWithoutTheStoreKeyNeverApproves(t *testing.T) {
	s, project := openStore(t), t.TempDir()
	requests := []egress.Rule{rule("docs.example.com")}
	review, err := s.ReviewApproval(project, egress.Filtered, requests, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := canonicalPath(project)
	if err != nil {
		t.Fatal(err)
	}
	view, err := json.Marshal(struct {
		Project string    `json:"project"`
		Before  *Approval `json:"before"`
		After   *Approval `json:"after"`
	}{Project: canonical, Before: review.Before, After: review.After})
	if err != nil {
		t.Fatal(err)
	}
	plain := sha256.Sum256(append([]byte("network-approval-review-v3\x00"), view...))
	if err := s.Approve(context.Background(), project, egress.Filtered, requests, nil, nil, hex.EncodeToString(plain[:])); !errors.Is(err, ErrApprovalChanged) {
		t.Fatalf("an unkeyed digest of the review = %v; want ErrApprovalChanged", err)
	}
	if err := s.Approve(context.Background(), project, egress.Filtered, requests, nil, nil, review.Digest); err != nil {
		t.Fatalf("the review's own digest = %v", err)
	}
}

// Record names do not depend on owner.key: with the key replaced, approvals are still found,
// while snapshots captured under the old key fail their check instead of being trusted.
func TestReplacedKeyKeepsApprovalsAndRefusesOldSnapshots(t *testing.T) {
	s, project := openStore(t), t.TempDir()
	requests := []egress.Rule{rule("docs.example.com")}
	if err := approve(s, project, egress.Filtered, requests, nil); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.Admit(project, Admission{Requests: requests})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Path(), "owner.key"), randid.Bytes(32), 0o600); err != nil {
		t.Fatal(err)
	}
	replaced, err := Open(s.Path(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer replaced.Close()
	if approval, err := replaced.Approval(project); err != nil || approval == nil {
		t.Fatalf("approval after the key was replaced = %v, %v", approval, err)
	}
	if _, err := replaced.LoadSnapshot(project, snapshot.Fingerprint); err == nil {
		t.Fatal("a snapshot captured under the old key was trusted under the new one")
	}
}
