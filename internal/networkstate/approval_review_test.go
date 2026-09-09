package networkstate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/egress"
)

// approve is the ordinary host sequence: review the diff, then commit exactly
// the view that was reviewed.
func approve(s *Store, project string, mode egress.Mode, requests []egress.Rule, bundles []egress.Bundle) error {
	review, err := s.ReviewApproval(project, mode, requests, bundles)
	if err != nil {
		return err
	}
	return s.Approve(context.Background(), project, mode, requests, bundles, review.Digest)
}

func TestApprovalReviewPublishesOnlyReviewedRules(t *testing.T) {
	s, project := openStore(t), t.TempDir()
	requests := []egress.Rule{rule("reviewed.example.com")}
	before := inventory(t, s)
	review, err := s.ReviewApproval(project, egress.Filtered, requests, nil)
	if err != nil || review.Before != nil || review.After == nil || !lowerHex(review.Digest, 64) {
		t.Fatal(review, err)
	}
	if !reflect.DeepEqual(before, inventory(t, s)) {
		t.Fatal("review published authority")
	}
	// Neither the displayed copy nor the caller's slice is the stored decision.
	review.After.Posture = egress.Open
	review.After.Envelope[0].To.Domain = "changed-display.example.com"
	review.After.Envelope[0].Ports[0] = 9443
	if err := s.Approve(context.Background(), project, egress.Filtered, requests, nil, review.Digest); err != nil {
		t.Fatal(err)
	}
	approval, err := s.Approval(project)
	if err != nil || approval.Posture != egress.Filtered || !reflect.DeepEqual(approval.Envelope, []egress.Rule{rule("reviewed.example.com")}) {
		t.Fatal("display mutation became a grant", approval, err)
	}
	// The same digest describes a view that no longer exists.
	if err := s.Approve(context.Background(), project, egress.Filtered, requests, nil, review.Digest); !errors.Is(err, ErrApprovalChanged) {
		t.Fatal("stale review published again", err)
	}
	mode := egress.Filtered
	captured, err := s.Admit(project, Admission{InvocationMode: &mode, Requests: approval.Envelope})
	if err != nil {
		t.Fatal(err)
	}
	if err := approve(s, project, egress.None, nil, nil); err != nil {
		t.Fatal(err)
	}
	retained, err := s.LoadSnapshot(project, captured.Fingerprint)
	if err != nil || !retained.Domain("reviewed.example.com", 443).Allowed {
		t.Fatal("new approval rewrote an existing run capture", err)
	}
}

func TestApprovalDigestBindsTheExactReviewedRequest(t *testing.T) {
	s, project := openStore(t), t.TempDir()
	requests := []egress.Rule{rule("reviewed.example.com")}
	review, err := s.ReviewApproval(project, egress.Filtered, requests, nil)
	if err != nil {
		t.Fatal(err)
	}
	changes := map[string]func() error{
		"other rules": func() error {
			return s.Approve(context.Background(), project, egress.Filtered, []egress.Rule{rule("other.example.com")}, nil, review.Digest)
		},
		"other mode": func() error { return s.Approve(context.Background(), project, egress.Open, nil, nil, review.Digest) },
		"other digest": func() error {
			return s.Approve(context.Background(), project, egress.Filtered, requests, nil, strings.Repeat("a", 64))
		},
		"no digest": func() error { return s.Approve(context.Background(), project, egress.Filtered, requests, nil, "") },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			if err := change(); err == nil {
				t.Fatal("a decision made against another view was published")
			}
			if got, err := s.Approval(project); err != nil || got != nil {
				t.Fatal("refused approval left authority", got, err)
			}
		})
	}
	// A concurrent owner's commit invalidates a review taken before it.
	other, err := OpenExisting(s.Path(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := approve(other, project, egress.Filtered, []egress.Rule{rule("second.example.com")}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Approve(context.Background(), project, egress.Filtered, requests, nil, review.Digest); !errors.Is(err, ErrApprovalChanged) {
		t.Fatal("stale review overwrote a concurrent decision", err)
	}
	approval, err := s.Approval(project)
	if err != nil || len(approval.Envelope) != 1 || approval.Envelope[0].To.Domain != "second.example.com" {
		t.Fatal("concurrent reviews merged grants", approval, err)
	}
}

func TestApprovalRefusesLostIdentityAndCancellation(t *testing.T) {
	for _, change := range []string{"key", "root", "project", "alias", "cancel"} {
		t.Run(change, func(t *testing.T) {
			s, project := openStore(t), t.TempDir()
			argument := project
			if change == "alias" {
				argument = filepath.Join(t.TempDir(), "project-alias")
				if err := os.Symlink(project, argument); err != nil {
					t.Fatal(err)
				}
			}
			requests := []egress.Rule{rule("reviewed.example.com")}
			review, err := s.ReviewApproval(argument, egress.Filtered, requests, nil)
			if err != nil {
				t.Fatal(err)
			}
			projectID := review.After.ProjectID
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch change {
			case "key":
				err = os.WriteFile(filepath.Join(s.Path(), "owner.key"), []byte(strings.Repeat("x", 32)), 0o600)
			case "root":
				err = os.Chmod(s.Path(), 0o755)
			case "project":
				err = os.Rename(project, project+"-original")
				if err == nil {
					err = os.Mkdir(project, 0o700)
				}
			case "alias":
				err = os.Remove(argument)
				if err == nil {
					err = os.Symlink(t.TempDir(), argument)
				}
			case "cancel":
				cancel()
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Approve(ctx, argument, egress.Filtered, requests, nil, review.Digest); err == nil {
				t.Fatal("changed identity or cancellation published a grant")
			}
			if _, err := os.Stat(filepath.Join(s.Path(), "approval-"+projectID+".json")); !os.IsNotExist(err) {
				t.Fatal("refused approval left authority", err)
			}
		})
	}
}
