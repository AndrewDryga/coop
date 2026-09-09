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

func TestApprovalReviewPublishesOnlyCapturedRules(t *testing.T) {
	s, project := openStore(t), t.TempDir()
	requests := []egress.Rule{rule("reviewed.example.com")}
	before := inventory(t, s)
	review, err := s.ReviewApproval(project, egress.Filtered, requests, nil)
	if err != nil || review.Before() != nil {
		t.Fatal(review, err)
	}
	if !reflect.DeepEqual(before, inventory(t, s)) {
		t.Fatal("preview published authority")
	}
	requests[0].To.Domain = "mutated.example.com"
	requests[0].Ports[0] = 8443
	display := review.After()
	display.Posture = egress.Open
	display.Envelope[0].To.Domain = "changed-display.example.com"
	display.Envelope[0].Ports[0] = 9443
	if err := review.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	approval, err := s.Approval(project)
	if err != nil || approval.Posture != egress.Filtered || !reflect.DeepEqual(approval.Envelope, []egress.Rule{rule("reviewed.example.com")}) {
		t.Fatal("caller or display mutation became a grant", approval, err)
	}
	if err := review.Commit(context.Background()); err == nil {
		t.Fatal("review committed twice")
	}
	mode := egress.Filtered
	captured, err := s.Admit(project, Admission{InvocationMode: &mode, Requests: approval.Envelope})
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.ReviewApproval(project, egress.None, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	next.Before().Envelope[0].To.Domain = "changed-before.example.com"
	if err := next.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	retained, err := s.LoadSnapshot(project, captured.Fingerprint)
	if err != nil || !retained.Domain("reviewed.example.com", 443).Allowed {
		t.Fatal("new approval rewrote an existing run capture", err)
	}
}

func TestApprovalReviewConcurrentOwnersCannotPublishStaleDiff(t *testing.T) {
	s, project := openStore(t), t.TempDir()
	other, err := OpenExisting(s.Path(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	first, err := s.ReviewApproval(project, egress.Filtered, []egress.Rule{rule("first.example.com")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := other.ReviewApproval(project, egress.Filtered, []egress.Rule{rule("second.example.com")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	start, results := make(chan struct{}), make(chan error, 2)
	for _, review := range []*ApprovalReview{first, second} {
		go func() { <-start; results <- review.Commit(context.Background()) }()
	}
	close(start)
	success, conflict := 0, 0
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrApprovalChanged):
			conflict++
		default:
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("success=%d conflict=%d", success, conflict)
	}
	approval, err := s.Approval(project)
	if err != nil || len(approval.Envelope) != 1 {
		t.Fatal("concurrent reviews merged grants", approval, err)
	}
}

func TestApprovalReviewRefusesLostIdentityAndCancellation(t *testing.T) {
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
			review, err := s.ReviewApproval(argument, egress.Filtered, []egress.Rule{rule("reviewed.example.com")}, nil)
			if err != nil {
				t.Fatal(err)
			}
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
			if err := review.Commit(ctx); err == nil {
				t.Fatal("changed identity or cancellation published a grant")
			}
			if _, err := os.Stat(filepath.Join(s.Path(), "approval-"+review.after.ProjectID+".json")); !os.IsNotExist(err) {
				t.Fatal("refused review left approval authority", err)
			}
			if err := review.Commit(context.Background()); err == nil {
				t.Fatal("failed review was reusable")
			}
		})
	}
}
