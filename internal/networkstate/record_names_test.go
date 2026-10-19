package networkstate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	id, legacy := s.projectKey(canonical), s.legacyProjectKey(canonical)
	approval, err := s.approval(id)
	if err != nil || approval == nil {
		t.Fatalf("approval = %v, %v", approval, err)
	}
	approval.Version, approval.ProjectID = networkApprovalAnchoredVersion, legacy
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

// A withdrawal an older build filed under the keyed project id still holds after the rename: it
// moves to the project's id on first lookup, and only a fresh approval lifts it, under both names.
func TestRenamedRecordsKeepAWithdrawal(t *testing.T) {
	s, project := openStore(t), t.TempDir()
	canonical, err := canonicalPath(project)
	if err != nil {
		t.Fatal(err)
	}
	id, legacy := s.projectKey(canonical), s.legacyProjectKey(canonical)
	plant := func() {
		t.Helper()
		data, err := json.Marshal(Withdrawal{Version: withdrawalLegacyVersion, ProjectID: legacy})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(s.Path(), withdrawalRecord(legacy)), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	plant()
	if withdrawn, err := s.Withdrawn(project); err != nil || !withdrawn {
		t.Fatalf("withdrawn = %v, %v; an older build's withdrawal was lost in the rename", withdrawn, err)
	}
	if _, err := os.Stat(filepath.Join(s.Path(), withdrawalRecord(id))); err != nil {
		t.Fatalf("the withdrawal did not move to the project's id: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(s.Path(), withdrawalRecord(legacy))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the old withdrawal stayed: %v", err)
	}
	_, err = s.Admit(project, Admission{})
	if pending := (*PendingApproval)(nil); !errors.As(err, &pending) || !strings.Contains(pending.Reason, "withdrawn") {
		t.Fatalf("a withdrawn project's launch after the rename = %v; want the withdrawal's refusal", err)
	}

	// One never looked up is lifted too by the approval, or it would move back on the next lookup.
	if err := os.Remove(filepath.Join(s.Path(), withdrawalRecord(id))); err != nil {
		t.Fatal(err)
	}
	plant()
	if err := approve(s, project, egress.Filtered, nil, nil); err != nil {
		t.Fatal(err)
	}
	if withdrawn, err := s.Withdrawn(project); err != nil || withdrawn {
		t.Fatalf("withdrawn after approval = %v, %v", withdrawn, err)
	}
}

// The move reads both names again under the approval's lock: a launch that read an older build's
// withdrawal just before a fresh approval lifted it does not file it again.
func TestALiftedWithdrawalIsNotMovedBack(t *testing.T) {
	s, project := openStore(t), t.TempDir()
	canonical, err := canonicalPath(project)
	if err != nil {
		t.Fatal(err)
	}
	id, legacy := s.projectKey(canonical), s.legacyProjectKey(canonical)
	data, err := json.Marshal(Withdrawal{Version: withdrawalLegacyVersion, ProjectID: legacy})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Path(), withdrawalRecord(legacy)), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := approve(s, project, egress.Filtered, nil, nil); err != nil {
		t.Fatal(err)
	}
	if found, err := s.moveWithdrawal(legacy, id); err != nil || found {
		t.Fatalf("moving a lifted withdrawal = %v, %v", found, err)
	}
	if _, err := os.Lstat(filepath.Join(s.Path(), withdrawalRecord(id))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a lifted withdrawal was filed again: %v", err)
	}
}

// pendingReason is a preview's refusal for a failure message; formatting a nil *PendingApproval
// directly would call its Error method.
func pendingReason(pending *PendingApproval) string {
	if pending == nil {
		return "no refusal"
	}
	return pending.Reason
}

// assertLegacyApprovalRetired fails when anything an older build filed for the project's approval
// is left: the record or its private anchor.
func assertLegacyApprovalRetired(t *testing.T, s *Store, legacy string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(s.Path(), "project-"+legacy+"-*.anchor"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("the old anchor survived: %v, %v", matches, err)
	}
	if _, err := os.Lstat(filepath.Join(s.Path(), approvalRecord(legacy))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the old approval survived: %v", err)
	}
}

// An approval an older build filed under the keyed project id reads as pending, saying the
// records were renamed. The review starts from it, so only what changed since shows as new, and
// approving re-enrolls the project through its marker and retires the old records.
func TestRenamedRecordsAskToApproveAgain(t *testing.T) {
	s, project := openStore(t), t.TempDir()
	requests := []egress.Rule{rule("docs.example.com")}
	if err := approve(s, project, egress.Filtered, requests, nil); err != nil {
		t.Fatal(err)
	}
	legacy := legacyProjectRecords(t, s, project)
	_, err := s.CheckRequests(project, requests, nil)
	var pending *PendingApproval
	if !errors.As(err, &pending) || !strings.Contains(pending.Reason, "renamed its network records") {
		t.Fatalf("an approval under the old id = %v; want a pending review that names the rename", err)
	}
	review, err := s.ReviewApproval(project, egress.Filtered, requests, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if review.Before == nil || review.Before.Posture != egress.Filtered || len(review.Before.Envelope) != 1 {
		t.Fatalf("the review starts from %+v; want the older build's approval", review.Before)
	}
	if err := s.Approve(context.Background(), project, egress.Filtered, requests, nil, nil, review.Digest); err != nil {
		t.Fatalf("approving again after the rename: %v", err)
	}
	if approval, err := s.CheckRequests(project, requests, nil); err != nil || approval == nil {
		t.Fatalf("after approving again = %v, %v", approval, err)
	}
	assertLegacyApprovalRetired(t, s, legacy)
}

// The box can delete the checkout's marker and the project's settings, never the older build's
// approval record: that record keeps the project refused, so its remembered posture cannot fall
// through to the open default. Approving again starts a fresh pair and retires the old records.
func TestRenamedRecordsRefuseWithoutTheMarker(t *testing.T) {
	s, project := openStore(t), t.TempDir()
	requests := []egress.Rule{rule("docs.example.com")}
	if err := approve(s, project, egress.Filtered, requests, nil); err != nil {
		t.Fatal(err)
	}
	legacy := legacyProjectRecords(t, s, project)
	if err := os.Remove(filepath.Join(project, ProjectApprovalMarker)); err != nil {
		t.Fatal(err)
	}
	if pending := pendingAfter(t, s, project, Admission{}); pending == nil || !strings.Contains(pending.Reason, "renamed its network records") {
		t.Fatalf("a launch with the marker and settings gone previews %q; want the rename's refusal", pendingReason(pending))
	}
	snapshot, err := s.Admit(project, Admission{})
	if pending := (*PendingApproval)(nil); !errors.As(err, &pending) || !strings.Contains(pending.Reason, "renamed its network records") {
		t.Fatalf("a launch with the marker and settings gone = %+v, %v; want the rename's refusal", snapshot, err)
	}
	if err := approve(s, project, egress.Filtered, requests, nil); err != nil {
		t.Fatalf("approving again after the marker was deleted: %v", err)
	}
	if _, err := s.Admit(project, Admission{Requests: requests}); err != nil {
		t.Fatalf("after approving again: %v", err)
	}
	assertLegacyApprovalRetired(t, s, legacy)
}

// `coop net forget` takes back an older build's approval like any other: the withdrawal is filed
// under the project's id, and the old record, its anchor and the checkout's marker go.
func TestForgetTakesBackAnOlderBuildsApproval(t *testing.T) {
	s, project := openStore(t), t.TempDir()
	if err := approve(s, project, egress.Filtered, []egress.Rule{rule("docs.example.com")}, nil); err != nil {
		t.Fatal(err)
	}
	legacy := legacyProjectRecords(t, s, project)
	withdraw(t, s, project)
	assertLegacyApprovalRetired(t, s, legacy)
	if _, err := os.Lstat(filepath.Join(project, ProjectApprovalMarker)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the checkout's marker survived the forget: %v", err)
	}
	id, err := s.projectID(project)
	if err != nil {
		t.Fatal(err)
	}
	if found, err := s.readWithdrawal(id, withdrawalVersion); err != nil || !found {
		t.Fatalf("withdrawal under the project's id = %v, %v", found, err)
	}
	if pending := pendingAfter(t, s, project, Admission{}); pending == nil || !strings.Contains(pending.Reason, "withdrawn") {
		t.Fatalf("a launch after forgetting an older build's approval previews %q; want the withdrawal's refusal", pendingReason(pending))
	}
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

// Record names no longer depend on owner.key: with the key replaced, approvals are still found,
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

// A review of an older build's approval retires the old pair and leaves a fresh pending one. If the
// human declines, `coop net forget` still takes the old approval back: nothing of the old pair is
// left to retire. The project stays refused; the launch names the pending marker, which only an
// approval completes.
func TestForgetAfterADeclinedReviewOfAnOlderBuildsApproval(t *testing.T) {
	s, project := openStore(t), t.TempDir()
	requests := []egress.Rule{rule("docs.example.com")}
	if err := approve(s, project, egress.Filtered, requests, nil); err != nil {
		t.Fatal(err)
	}
	legacy := legacyProjectRecords(t, s, project)
	if _, err := s.ReviewApproval(project, egress.Filtered, requests, nil, nil); err != nil {
		t.Fatal(err)
	}
	withdraw(t, s, project)
	assertLegacyApprovalRetired(t, s, legacy)
	if pending := pendingAfter(t, s, project, Admission{}); pending == nil {
		t.Fatal("a launch after forgetting a declined review was not refused")
	}
	if _, err := s.Admit(project, Admission{}); err == nil {
		t.Fatal("a launch after forgetting a declined review was admitted")
	}
}

// A review starts from an older build's approval, so the mode it proposes keeps the project's
// remembered posture; once approved again, the current approval is the start.
func TestPriorApprovalIsTheOlderBuildsUntilApprovedAgain(t *testing.T) {
	s, project := openStore(t), t.TempDir()
	if err := approve(s, project, egress.None, nil, nil); err != nil {
		t.Fatal(err)
	}
	legacyProjectRecords(t, s, project)
	if prior, err := s.PriorApproval(project); err != nil || prior == nil || prior.Posture != egress.None || prior.Version != networkApprovalAnchoredVersion {
		t.Fatalf("prior approval of a project an older build approved = %+v, %v", prior, err)
	}
	if err := approve(s, project, egress.None, nil, nil); err != nil {
		t.Fatal(err)
	}
	if prior, err := s.PriorApproval(project); err != nil || prior == nil || prior.Version != networkApprovalVersion {
		t.Fatalf("prior approval after approving again = %+v, %v", prior, err)
	}
}
