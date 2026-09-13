package networkstate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	services, err := approvalServiceDigests(requests)
	if err != nil {
		return err
	}
	review, err := s.ReviewApproval(project, mode, requests, bundles, services)
	if err != nil {
		return err
	}
	return s.Approve(context.Background(), project, mode, requests, bundles, services, review.Digest)
}

// approvalServiceDigests stands in for the box's Compose read: one stable digest
// per requested service name.
func approvalServiceDigests(requests []egress.Rule) (map[string]string, error) {
	var out map[string]string
	for _, request := range requests {
		if request.To.Service == "" {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		sum := sha256.Sum256([]byte("definition of " + request.To.Service))
		out[request.To.Service] = hex.EncodeToString(sum[:])
	}
	return out, nil
}

func TestApprovedServicesKeepsReviewedDependenciesWithoutGrantingThem(t *testing.T) {
	db := egress.Rule{To: egress.Destination{Service: "db"}, Protocol: "tcp", Ports: []int{5432}}
	digests := map[string]string{"db": strings.Repeat("a", 64), "cache": strings.Repeat("b", 64)}
	got, err := approvedServices([]egress.Rule{db}, digests)
	if err != nil || !reflect.DeepEqual(got, digests) {
		t.Fatalf("approved services = %v, %v; want direct service and reviewed dependency", got, err)
	}
}

func TestApprovalReviewPublishesOnlyReviewedRules(t *testing.T) {
	s, project := openStore(t), t.TempDir()
	requests := []egress.Rule{rule("reviewed.example.com")}
	before := inventory(t, s)
	review, err := s.ReviewApproval(project, egress.Filtered, requests, nil, nil)
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
	if err := s.Approve(context.Background(), project, egress.Filtered, requests, nil, nil, review.Digest); err != nil {
		t.Fatal(err)
	}
	approval, err := s.Approval(project)
	if err != nil || approval.Posture != egress.Filtered || !reflect.DeepEqual(approval.Envelope, []egress.Rule{rule("reviewed.example.com")}) {
		t.Fatal("display mutation became a grant", approval, err)
	}
	// The same digest describes a view that no longer exists.
	if err := s.Approve(context.Background(), project, egress.Filtered, requests, nil, nil, review.Digest); !errors.Is(err, ErrApprovalChanged) {
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
	review, err := s.ReviewApproval(project, egress.Filtered, requests, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	changes := map[string]func() error{
		"other rules": func() error {
			return s.Approve(context.Background(), project, egress.Filtered, []egress.Rule{rule("other.example.com")}, nil, nil, review.Digest)
		},
		"other mode": func() error {
			return s.Approve(context.Background(), project, egress.Open, nil, nil, nil, review.Digest)
		},
		"other digest": func() error {
			return s.Approve(context.Background(), project, egress.Filtered, requests, nil, nil, strings.Repeat("a", 64))
		},
		"no digest": func() error { return s.Approve(context.Background(), project, egress.Filtered, requests, nil, nil, "") },
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
	if err := s.Approve(context.Background(), project, egress.Filtered, requests, nil, nil, review.Digest); !errors.Is(err, ErrApprovalChanged) {
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
			review, err := s.ReviewApproval(argument, egress.Filtered, requests, nil, nil)
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
			if err := s.Approve(ctx, argument, egress.Filtered, requests, nil, nil, review.Digest); err == nil {
				t.Fatal("changed identity or cancellation published a grant")
			}
			if _, err := os.Stat(filepath.Join(s.Path(), "approval-"+projectID+".json")); !os.IsNotExist(err) {
				t.Fatal("refused approval left authority", err)
			}
		})
	}
}

// An approval is bound to a DIRECTORY, not to the name of one. Moving the
// approved project aside and putting another checkout at the same path must not
// hand that checkout its grants.
func TestApprovalRefusesAReplacedProjectDirectory(t *testing.T) {
	s := openStore(t)
	base := t.TempDir()
	project := filepath.Join(base, "repo")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := approve(s, project, egress.Filtered, []egress.Rule{rule("example.com")}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Admit(project, Admission{Requests: []egress.Rule{rule("example.com")}}); err != nil {
		t.Fatal("the approved directory was refused", err)
	}
	if err := os.Rename(project, filepath.Join(base, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := s.Admit(project, Admission{Requests: []egress.Rule{rule("example.com")}})
	if err == nil || !strings.Contains(err.Error(), "was replaced since it was approved") {
		t.Fatalf("a replacement at the approved path inherited its grants: %v", err)
	}
	preview, err := s.admissionPreview(project, Admission{Requests: []egress.Rule{rule("example.com")}})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Pending == nil || !strings.Contains(preview.Pending.Error(), "was replaced since it was approved") {
		t.Fatalf("coop net did not report the replacement: %+v", preview)
	}
	// Reviewing it again rebinds the approval to the directory that is there now.
	if err := approve(s, project, egress.Filtered, []egress.Rule{rule("example.com")}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Admit(project, Admission{Requests: []egress.Rule{rule("example.com")}}); err != nil {
		t.Fatal("re-approval did not rebind the directory", err)
	}
}

// A project that REQUESTS a sidecar must be able to run it once a human approves
// that request. The envelope check compares rules, and a `service:` rule carries
// no address — so comparing addresses refused the very grant that was approved.
func TestApprovedServiceRequestPassesItsOwnEnvelope(t *testing.T) {
	s, project := openStore(t), t.TempDir()
	request := egress.Rule{To: egress.Destination{Service: "db"}, Protocol: "tcp", Ports: []int{5432, 5433}}
	if err := approve(s, project, egress.Filtered, []egress.Rule{request}, nil); err != nil {
		t.Fatal(err)
	}
	policy, err := s.Admit(project, Admission{Requests: []egress.Rule{request}})
	if err != nil {
		t.Fatalf("the approved service request was refused by its own envelope: %v", err)
	}
	if len(policy.Grants) != 1 || policy.Grants[0].Rule.To.Service != "db" {
		t.Fatalf("the capture did not carry the approved sidecar: %+v", policy.Grants)
	}
	// Narrower is inside the envelope; anything else still needs review.
	narrower := egress.Rule{To: egress.Destination{Service: "db"}, Protocol: "tcp", Ports: []int{5432}}
	if _, err := s.Admit(project, Admission{Requests: []egress.Rule{narrower}}); err != nil {
		t.Fatalf("a narrower service request was refused: %v", err)
	}
	other := egress.Rule{To: egress.Destination{Service: "cache"}, Protocol: "tcp", Ports: []int{5432}}
	if _, err := s.Admit(project, Admission{Requests: []egress.Rule{other}}); err == nil {
		t.Fatal("an unapproved sidecar passed the envelope")
	}
	wider := egress.Rule{To: egress.Destination{Service: "db"}, Protocol: "tcp", Ports: []int{5432, 6000}}
	if _, err := s.Admit(project, Admission{Requests: []egress.Rule{wider}}); err == nil {
		t.Fatal("a wider port set passed the envelope")
	}
}
