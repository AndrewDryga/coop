package networkstate

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/AndrewDryga/coop/internal/egress"
)

var ErrApprovalChanged = errors.New("the request changed while you were reviewing it — run 'coop approve' again")

// ApprovalReview is the plain before/after diff a host operator reviews. Digest
// binds that exact view — the project identity, the stored approval it started
// from and the approval it would write. Approve takes the digest back and
// refuses a decision made against a view that has since changed.
type ApprovalReview struct {
	Before *Approval `json:"before"`
	After  *Approval `json:"after"`
	Digest string    `json:"digest"`
}

// ReviewApproval publishes nothing. Its arguments must be the same snapshot the
// operator sees; never re-read repository YAML between review and Approve.
// services carries each `service:` grant's reviewed Compose definition plus its
// startup dependencies. Only the rules grant network access.
func (s *Store) ReviewApproval(project string, mode egress.Mode, requests []egress.Rule, bundles []egress.Bundle, services map[string]string) (ApprovalReview, error) {
	review, _, err := s.reviewApproval(project, mode, requests, bundles, services)
	return review, err
}

func (s *Store) reviewApproval(project string, mode egress.Mode, requests []egress.Rule, bundles []egress.Bundle, services map[string]string) (ApprovalReview, []egress.Bundle, error) {
	if err := s.intactAuthority(); err != nil {
		return ApprovalReview{}, nil, err
	}
	resolved, err := canonicalPath(project)
	if err != nil {
		return ApprovalReview{}, nil, err
	}
	identity, err := os.Stat(resolved)
	if err != nil || !identity.IsDir() {
		return ApprovalReview{}, nil, errors.New("this project directory does not exist")
	}
	id, err := s.projectID(resolved)
	if err != nil {
		return ApprovalReview{}, nil, err
	}
	if _, err := egress.ParseMode(string(mode)); err != nil {
		return ApprovalReview{}, nil, err
	}
	rules, err := egress.NormalizeRules(requests)
	if err != nil {
		return ApprovalReview{}, nil, err
	}
	if mode != egress.Filtered && len(rules) != 0 {
		return ApprovalReview{}, nil, errors.New("remove box.egress_rules from .agent/project.yaml before approving open or none — rules only apply in filtered mode")
	}
	selected, err := egress.SelectedBundles(bundles)
	if err != nil {
		return ApprovalReview{}, nil, err
	}
	features, err := featureApprovals(rules, selected)
	if err != nil {
		return ApprovalReview{}, nil, err
	}
	device, inode, ok := directoryIdentity(identity)
	if !ok {
		return ApprovalReview{}, nil, errors.New("this project directory could not be read")
	}
	after := &Approval{Version: 1, ProjectID: id, Posture: mode, Envelope: rules, Device: device, Inode: inode, Features: features}
	if after.Services, err = approvedServices(rules, services); err != nil {
		return ApprovalReview{}, nil, err
	}
	if data, err := json.Marshal(after); err != nil || len(data) > maxPrivateRecordBytes {
		return ApprovalReview{}, nil, errors.New("network approval exceeds byte limit")
	}
	before, err := s.approval(id)
	if err != nil {
		return ApprovalReview{}, nil, err
	}
	digest, err := s.approvalReviewDigest(resolved, identity, before, after)
	if err != nil {
		return ApprovalReview{}, nil, err
	}
	return ApprovalReview{Before: copyApproval(before), After: copyApproval(after), Digest: digest}, selected, nil
}

// The digest is keyed: a public value cannot be replayed as owner consent, and
// a project rebound to another directory produces a different view.
func (s *Store) approvalReviewDigest(resolved string, identity os.FileInfo, before, after *Approval) (string, error) {
	stat, ok := identity.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errors.New("network approval project identity is unavailable")
	}
	view := struct {
		Project       string    `json:"project"`
		Device, Inode uint64    `json:"-"`
		Before        *Approval `json:"before"`
		After         *Approval `json:"after"`
	}{Project: resolved, Device: uint64(stat.Dev), Inode: stat.Ino, Before: before, After: after}
	data, err := json.Marshal(view)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte("network-approval-review-v1\x00"))
	_, _ = fmt.Fprintf(mac, "%d\x00%d\x00", view.Device, view.Inode)
	_, _ = mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func copyApproval(value *Approval) *Approval {
	if value == nil {
		return nil
	}
	data, _ := json.Marshal(value)
	var copy Approval
	_ = json.Unmarshal(data, &copy)
	return &copy
}

// Approve is a host operation. It never rereads repository configuration: the
// caller passes back the exact rules it displayed plus the review digest, and
// a changed pending request or stored approval fails with ErrApprovalChanged.
func (s *Store) Approve(ctx context.Context, project string, mode egress.Mode, requests []egress.Rule, bundles []egress.Bundle, services map[string]string, digest string) error {
	if !lowerHex(digest, 64) {
		return errors.New("network approval requires the digest of a current review")
	}
	review, selected, err := s.reviewApproval(project, mode, requests, bundles, services)
	if err != nil {
		return err
	}
	if !hmac.Equal([]byte(review.Digest), []byte(digest)) {
		return ErrApprovalChanged
	}
	return s.lockRecord(ctx, "approval", review.After.ProjectID, func() error {
		current, _, err := s.reviewApproval(project, mode, requests, bundles, services)
		if err != nil {
			return err
		}
		if !hmac.Equal([]byte(current.Digest), []byte(digest)) {
			return ErrApprovalChanged
		}
		if err := s.checkBundles(selected); err != nil {
			return fmt.Errorf("publish reviewed bundles: %w", err)
		}
		data, err := json.Marshal(current.After)
		if err != nil {
			return err
		}
		if err := s.publish(approvalRecord(current.After.ProjectID), data, true); err != nil {
			return fmt.Errorf("publish reviewed approval: %w", err)
		}
		// An explicit fresh approval is the ONE thing that lifts a withdrawal —
		// including an approval of an unchanged or empty request, which is how a
		// project with nothing left in its YAML gets moving again.
		if err := s.clearWithdrawal(current.After.ProjectID); err != nil {
			return fmt.Errorf("clear network withdrawal: %w", err)
		}
		return s.intactAuthority()
	})
}

// approvedServices keeps the reviewed definitions of direct services and their
// dependencies whenever at least one service rule exists. The extra definitions
// pin startup only; the rules remain the complete network grant set.
func approvedServices(rules []egress.Rule, digests map[string]string) (map[string]string, error) {
	hasServiceRule := false
	for _, rule := range rules {
		name := rule.To.Service
		if name == "" {
			continue
		}
		hasServiceRule = true
		digest := digests[name]
		if !lowerHex(digest, 64) {
			return nil, errors.New("approving the Compose service " + name + " requires the digest of its reviewed definition")
		}
	}
	if !hasServiceRule {
		return nil, nil
	}
	out := make(map[string]string, len(digests))
	for name, digest := range digests {
		if !lowerHex(digest, 64) {
			return nil, errors.New("approving the Compose service " + name + " requires the digest of its reviewed definition")
		}
		out[name] = digest
	}
	return out, nil
}
