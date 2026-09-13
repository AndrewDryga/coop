package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/ui"
)

type fakeApprovalReview struct {
	before, after *networkstate.Approval
	committed     int
	commitErr     error
}

func (f *fakeApprovalReview) Project() string                { return "/private/tmp/project" }
func (f *fakeApprovalReview) Before() *networkstate.Approval { return f.before }
func (f *fakeApprovalReview) After() *networkstate.Approval  { return f.after }
func (f *fakeApprovalReview) Commit(context.Context) error   { f.committed++; return f.commitErr }

func approvalRule(domain string) egress.Rule {
	return egress.Rule{To: egress.Destination{Domain: domain}, Protocol: "tls", Ports: []int{443}}
}

// The review is one stable diff under two uppercase sections: the access mode,
// then every rule with what it is — already approved, new, or no longer
// requested. Its exact bytes are the approved transcript.
func TestApprovalReviewIsOneStableDiff(t *testing.T) {
	review := &fakeApprovalReview{
		before: &networkstate.Approval{Posture: egress.Filtered, Envelope: []egress.Rule{approvalRule("github.com"), approvalRule("registry.npmjs.org")}},
		after:  &networkstate.Approval{Posture: egress.Filtered, Envelope: []egress.Rule{approvalRule("github.com"), approvalRule("api.example.com")}}}
	var out bytes.Buffer
	if err := confirmNetApproval(context.Background(), review, &out, func() bool { return true }); err != nil {
		t.Fatalf("confirmNetApproval: %v", err)
	}
	assertApprovedOutput(t, "21m-net-approve-review", out.String())
	if review.committed != 1 {
		t.Errorf("committed %d times, want 1", review.committed)
	}
	if !strings.HasSuffix(netApprovePrompt, "for new runs?") || netApproveApproved != "Approved for new runs" {
		t.Errorf("the question and the answer no longer say what an approval applies to: %q / %q", netApprovePrompt, netApproveApproved)
	}
	for _, unwanted := range []string{"Remember", "remembered", "(unchanged)", "websites", "Egress", "/private/tmp/project"} {
		if strings.Contains(out.String(), unwanted) {
			t.Errorf("approval review still says %q:\n%s", unwanted, out.String())
		}
	}
}

// A mode change is the same review with a before/after pair under NETWORK
// ACCESS, in the shared mode words.
func TestApprovalReviewShowsTheModeChange(t *testing.T) {
	review := &fakeApprovalReview{
		before: &networkstate.Approval{Posture: egress.None},
		after:  &networkstate.Approval{Posture: egress.Filtered, Envelope: []egress.Rule{approvalRule("docs.example.com")}}}
	var out bytes.Buffer
	if err := confirmNetApproval(context.Background(), review, &out, func() bool { return true }); err != nil {
		t.Fatalf("confirmNetApproval: %v", err)
	}
	assertApprovedOutput(t, "21n-net-approve-mode-change", out.String())
}

// Additions expand access, so they are yellow; removals are dim; nothing new is
// ever green. The markers carry the same meaning uncolored.
func TestApprovalReviewColorsAdditionsYellowNeverGreen(t *testing.T) {
	review := &fakeApprovalReview{
		before: &networkstate.Approval{Posture: egress.Filtered, Envelope: []egress.Rule{approvalRule("old.example")}},
		after:  &networkstate.Approval{Posture: egress.Filtered, Envelope: []egress.Rule{approvalRule("new.example")}}}
	var b bytes.Buffer
	writeNetAccessChange(&b, ui.Colored(), review.before, review.after.Posture, review.after.Envelope)
	text := b.String()
	if !strings.Contains(text, "\x1b[33m  + new.example:443 · TLS  new request\x1b[0m") {
		t.Errorf("the addition is not the yellow row:\n%q", text)
	}
	if !strings.Contains(text, "\x1b[2m  - old.example:443 · TLS  no longer requested\x1b[0m") {
		t.Errorf("the removal is not a dim row:\n%q", text)
	}
	if strings.Contains(text, "\x1b[32m") {
		t.Errorf("a review colored something green:\n%q", text)
	}
}

// The mode is always the shared description, and an unrestricted request earns
// its red warning whether or not the mode itself changed.
func TestApprovalReviewExplainsTheModeInHumanWords(t *testing.T) {
	filtered := &networkstate.Approval{Posture: egress.Filtered, Envelope: []egress.Rule{approvalRule("github.com")}}
	for name, tc := range map[string]struct {
		before *networkstate.Approval
		after  *networkstate.Approval
		want   []string
		absent []string
	}{
		"first approval": {after: &networkstate.Approval{Posture: egress.Filtered, Envelope: []egress.Rule{approvalRule("github.com")}},
			want: []string{"NETWORK ACCESS\n  Filtered — only approved network traffic is allowed.\n", "+ github.com:443 · TLS  new request"}},
		"escalation to open": {before: filtered, after: &networkstate.Approval{Posture: egress.Open},
			want: []string{"  - Filtered — only approved network traffic is allowed.\n  + Unrestricted — nothing is blocked.\n",
				netOpenWarning + "\n", "- github.com:443 · TLS  no longer requested"}},
		"offline": {before: filtered, after: &networkstate.Approval{Posture: egress.None},
			want: []string{"  - Filtered — only approved network traffic is allowed.\n  + Offline — internet access is blocked.\n"}},
		"open unchanged": {before: &networkstate.Approval{Posture: egress.Open}, after: &networkstate.Approval{Posture: egress.Open},
			want:   []string{"NETWORK ACCESS\n  Unrestricted — nothing is blocked.\n", netOpenWarning},
			absent: []string{"NETWORK RULES"}},
	} {
		t.Run(name, func(t *testing.T) {
			var b bytes.Buffer
			writeNetAccessChange(&b, ui.Palette{}, tc.before, tc.after.Posture, tc.after.Envelope)
			for _, want := range tc.want {
				if !strings.Contains(b.String(), want) {
					t.Errorf("review is missing %q:\n%s", want, b.String())
				}
			}
			for _, absent := range append(tc.absent, "open is", "none is", "filtered (", "(unchanged)", "websites") {
				if strings.Contains(b.String(), absent) {
					t.Errorf("review says %q:\n%s", absent, b.String())
				}
			}
		})
	}
}

// The review must be shown BEFORE the question, and a "no" must change
// nothing: a human who declines has decided, not deferred.
func TestApprovalCancelledLeavesNothingBehind(t *testing.T) {
	review := &fakeApprovalReview{after: &networkstate.Approval{Posture: egress.Filtered, Envelope: []egress.Rule{approvalRule("new.example")}}}
	var out bytes.Buffer
	err := confirmNetApproval(context.Background(), review, &out, func() bool {
		if out.Len() == 0 {
			t.Error("the confirmation was asked before the review was shown")
		}
		return false
	})
	if err == nil || !strings.Contains(err.Error(), "cancelled — nothing was approved") {
		t.Fatalf("cancelled approval = %v", err)
	}
	if review.committed != 0 {
		t.Error("a declined approval was committed")
	}
}

func TestApprovalRefusesAnEmptyReview(t *testing.T) {
	err := confirmNetApproval(context.Background(), &fakeApprovalReview{}, &bytes.Buffer{}, func() bool { return true })
	if err == nil {
		t.Fatal("a review with nothing to approve was confirmed")
	}
}

func TestApprovalSurfacesACommitFailure(t *testing.T) {
	review := &fakeApprovalReview{after: &networkstate.Approval{Posture: egress.Filtered},
		commitErr: errors.New("network approval changed after review")}
	err := confirmNetApproval(context.Background(), review, &bytes.Buffer{}, func() bool { return true })
	if err == nil || !strings.Contains(err.Error(), "changed after review") {
		t.Fatalf("commit failure = %v", err)
	}
}

// There is no --mode: the mode comes from .agent/project.yaml and nowhere else.
func TestApproveTakesNoFlags(t *testing.T) {
	a := &app{}
	for _, args := range [][]string{{"--mode", "open"}, {"--mode=none"}, {"--yes"}, {"extra"}} {
		if code, err := a.cmdApprove(args); code != 2 || err == nil {
			t.Errorf("cmdApprove(%q) = (%d, %v), want a usage error", args, code, err)
		}
	}
	if code, err := a.dispatch([]string{"approve", "extra"}); code != 2 || err == nil {
		t.Errorf("dispatch coop approve extra = (%d, %v), want a usage error", code, err)
	}
}

func TestRetiredNetApprovePointsAtApprove(t *testing.T) {
	code, err := (&app{}).cmdNet([]string{"approve"})
	if code != 2 || err == nil || !strings.Contains(err.Error(), `"coop net approve" has moved`) || !strings.Contains(err.Error(), "coop approve") {
		t.Fatalf("coop net approve = (%d, %v)", code, err)
	}

	code, err = helpForPath([]string{"net", "approve"}, nil, true)
	if code != 2 || err == nil || !strings.Contains(err.Error(), `"coop net approve" has moved`) || !strings.Contains(err.Error(), "coop approve") {
		t.Fatalf("coop help net approve = (%d, %v)", code, err)
	}
}
