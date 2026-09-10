package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkstate"
)

type fakeApprovalReview struct {
	before, after *networkstate.Approval
	mode          egress.Mode
	committed     int
	commitErr     error
}

func (f *fakeApprovalReview) Project() string                { return "/private/tmp/project" }
func (f *fakeApprovalReview) Mode() egress.Mode              { return f.mode }
func (f *fakeApprovalReview) Before() *networkstate.Approval { return f.before }
func (f *fakeApprovalReview) After() *networkstate.Approval  { return f.after }
func (f *fakeApprovalReview) Commit(context.Context) error   { f.committed++; return f.commitErr }

func approvalRule(domain string) egress.Rule {
	return egress.Rule{To: egress.Destination{Domain: domain}, Protocol: "tls", Ports: []int{443}}
}

func TestApproveModeFlagParsing(t *testing.T) {
	mode, err := parseNetApproveArgs([]string{"--mode", "open"})
	if err != nil || mode == nil || *mode != egress.Open {
		t.Fatalf("parseNetApproveArgs = (%v, %v)", mode, err)
	}
	if mode, err := parseNetApproveArgs([]string{"--mode=none"}); err != nil || *mode != egress.None {
		t.Errorf("inline --mode = (%v, %v)", mode, err)
	}
	if mode, err := parseNetApproveArgs(nil); err != nil || mode != nil {
		t.Errorf("no flags = (%v, %v)", mode, err)
	}
	for _, args := range [][]string{{"--mode"}, {"--mode", "sideways"}, {"--yes"}, {"--mode", "open", "--mode", "none"}} {
		if _, err := parseNetApproveArgs(args); err == nil {
			t.Errorf("parseNetApproveArgs(%q) was accepted", args)
		}
	}
}

func TestApprovalPreviewShowsTheDiffAndItsLimits(t *testing.T) {
	review := &fakeApprovalReview{mode: egress.Filtered,
		before: &networkstate.Approval{Posture: egress.Filtered, Envelope: []egress.Rule{approvalRule("old.example")}},
		after:  &networkstate.Approval{Posture: egress.Filtered, Envelope: []egress.Rule{approvalRule("new.example")}}}
	var out bytes.Buffer
	if err := confirmNetApproval(context.Background(), review, &out, func() bool { return true }); err != nil {
		t.Fatalf("confirmNetApproval: %v", err)
	}
	text := out.String()
	for _, want := range []string{"/private/tmp/project", "filtered (unchanged)", "+ new.example tls/443",
		"- old.example tls/443"} {
		if !strings.Contains(text, want) {
			t.Errorf("approval preview is missing %q:\n%s", want, text)
		}
	}
	// The review is the diff; the one limit that used to be four lines of
	// disclaimer under it now rides on the question and the answer.
	if !strings.Contains(netApprovePrompt, "new runs") || !strings.Contains(netApproveRemembered, "boxes already running") {
		t.Errorf("the approval question and answer no longer say what an approval applies to: %q / %q",
			netApprovePrompt, netApproveRemembered)
	}
	for _, unwanted := range []string{"OUTSIDE the repository", "Nothing is tested", "captured automatically"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("approval preview still carries the disclaimer %q:\n%s", unwanted, text)
		}
	}
	if review.committed != 1 {
		t.Errorf("committed %d times, want 1", review.committed)
	}
}

// The preview must be shown BEFORE the question, and a "no" must change
// nothing: a human who declines has decided, not deferred.
func TestApprovalCancelledLeavesNothingBehind(t *testing.T) {
	review := &fakeApprovalReview{mode: egress.Filtered, after: &networkstate.Approval{Posture: egress.Filtered}}
	var out bytes.Buffer
	err := confirmNetApproval(context.Background(), review, &out, func() bool {
		if out.Len() == 0 {
			t.Error("the confirmation was asked before the review was shown")
		}
		return false
	})
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("cancelled approval = %v", err)
	}
	if review.committed != 0 {
		t.Error("a declined approval was committed")
	}
}

func TestApprovalNamesWhatEachPostureMeans(t *testing.T) {
	for posture, want := range map[egress.Mode]string{
		egress.Open: "open is no filtering at all — every destination is reachable",
		egress.None: "none is offline — no provider or MCP connections either",
	} {
		review := &fakeApprovalReview{mode: posture, after: &networkstate.Approval{Posture: posture}}
		var out bytes.Buffer
		if err := confirmNetApproval(context.Background(), review, &out, func() bool { return true }); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), want) {
			t.Errorf("posture %q preview is missing %q:\n%s", posture, want, out.String())
		}
	}
}

func TestApprovalRefusesAnEmptyReview(t *testing.T) {
	err := confirmNetApproval(context.Background(), &fakeApprovalReview{}, &bytes.Buffer{}, func() bool { return true })
	if err == nil {
		t.Fatal("a review with nothing to approve was confirmed")
	}
}

func TestApprovalSurfacesACommitFailure(t *testing.T) {
	review := &fakeApprovalReview{mode: egress.Filtered, after: &networkstate.Approval{Posture: egress.Filtered},
		commitErr: errors.New("network approval changed after review")}
	err := confirmNetApproval(context.Background(), review, &bytes.Buffer{}, func() bool { return true })
	if err == nil || !strings.Contains(err.Error(), "changed after review") {
		t.Fatalf("commit failure = %v", err)
	}
}
