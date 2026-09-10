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

// The review is one stable diff: the mode in plain words, then every rule with
// what it is — already approved, new, or no longer requested — sorted by the
// rule text so the same request always reads the same.
func TestApprovalReviewIsOneStableDiff(t *testing.T) {
	review := &fakeApprovalReview{
		before: &networkstate.Approval{Posture: egress.Filtered, Envelope: []egress.Rule{approvalRule("github.com"), approvalRule("registry.npmjs.org")}},
		after:  &networkstate.Approval{Posture: egress.Filtered, Envelope: []egress.Rule{approvalRule("github.com"), approvalRule("api.example.com")}}}
	var out bytes.Buffer
	if err := confirmNetApproval(context.Background(), review, &out, func() bool { return true }); err != nil {
		t.Fatalf("confirmNetApproval: %v", err)
	}
	want := "Network access changes for project\n  /private/tmp/project\n\n" +
		"Internet access remains filtered.\n" +
		"Only approved websites and services can be reached.\n\n" +
		"  Access:\n" +
		"    + api.example.com:443 · TLS      new request\n" +
		"      github.com:443 · TLS           already approved\n" +
		"    - registry.npmjs.org:443 · TLS   no longer requested\n\n"
	if out.String() != want {
		t.Errorf("approval review:\n%s\nwant:\n%s", out.String(), want)
	}
	if review.committed != 1 {
		t.Errorf("committed %d times, want 1", review.committed)
	}
	if !strings.HasSuffix(netApprovePrompt, "for new runs?") || netApproveApproved != "Approved for new runs" {
		t.Errorf("the question and the answer no longer say what an approval applies to: %q / %q", netApprovePrompt, netApproveApproved)
	}
	for _, unwanted := range []string{"Remember", "remembered", "(unchanged)", "Rules none", "destinations of its own", "Egress"} {
		if strings.Contains(out.String(), unwanted) {
			t.Errorf("approval review still says %q:\n%s", unwanted, out.String())
		}
	}
}

// Additions expand access, so they are yellow; removals and the baseline are
// dim; nothing new is ever green. The markers carry the same meaning uncolored.
func TestApprovalReviewColorsAdditionsYellowNeverGreen(t *testing.T) {
	review := &fakeApprovalReview{
		before: &networkstate.Approval{Posture: egress.Filtered, Envelope: []egress.Rule{approvalRule("old.example")}},
		after:  &networkstate.Approval{Posture: egress.Filtered, Envelope: []egress.Rule{approvalRule("new.example")}}}
	var b bytes.Buffer
	writeNetAccessChange(&b, ui.Colored(), review.before, review.after.Posture, review.after.Envelope)
	text := b.String()
	if !strings.Contains(text, "\x1b[33m    + new.example:443 · TLS   new request\x1b[0m") {
		t.Errorf("the addition is not the yellow row:\n%q", text)
	}
	if !strings.Contains(text, "\x1b[2m    - old.example:443 · TLS   no longer requested\x1b[0m") {
		t.Errorf("the removal is not a dim row:\n%q", text)
	}
	if strings.Contains(text, "\x1b[32m") {
		t.Errorf("a review colored something green:\n%q", text)
	}
}

// A mode change is part of the same review, in human words: open gets its red
// warning, none reads as offline, and no raw enum reaches the screen.
func TestApprovalReviewExplainsTheModeInHumanWords(t *testing.T) {
	filtered := &networkstate.Approval{Posture: egress.Filtered, Envelope: []egress.Rule{approvalRule("github.com")}}
	for name, tc := range map[string]struct {
		before *networkstate.Approval
		after  *networkstate.Approval
		want   []string
		absent []string
	}{
		"first approval": {after: &networkstate.Approval{Posture: egress.Filtered, Envelope: []egress.Rule{approvalRule("github.com")}},
			want: []string{"Internet access will be filtered.\nOnly approved websites and services can be reached.\n", "+ github.com:443 · TLS   new request"}},
		"escalation to open": {before: filtered, after: &networkstate.Approval{Posture: egress.Open},
			want:   []string{"Internet access changes from filtered to unrestricted.\n", "⚠ Nothing will be blocked — an agent can reach any destination\n", "- github.com:443 · TLS   no longer requested"},
			absent: []string{"Only approved"}},
		"offline": {before: filtered, after: &networkstate.Approval{Posture: egress.None},
			want: []string{"Internet access changes from filtered to offline — no external network access.\nAn agent cannot reach its provider.\n"}, absent: []string{"none"}},
		"open unchanged": {before: &networkstate.Approval{Posture: egress.Open}, after: &networkstate.Approval{Posture: egress.Open},
			want: []string{"Internet access remains unrestricted.\n⚠ Nothing will be blocked"}, absent: []string{"Access:"}},
	} {
		t.Run(name, func(t *testing.T) {
			var b bytes.Buffer
			writeNetAccessChange(&b, ui.Palette{}, tc.before, tc.after.Posture, tc.after.Envelope)
			for _, want := range tc.want {
				if !strings.Contains(b.String(), want) {
					t.Errorf("review is missing %q:\n%s", want, b.String())
				}
			}
			for _, absent := range append(tc.absent, "open is", "none is", "filtered (", "(unchanged)") {
				if strings.Contains(b.String(), absent) {
					t.Errorf("review says %q:\n%s", absent, b.String())
				}
			}
		})
	}
	// The warning is red on a terminal: an escalation must not blend in.
	var b bytes.Buffer
	writeNetAccessChange(&b, ui.Colored(), filtered, egress.Open, nil)
	if !strings.Contains(b.String(), "\x1b[31m⚠ Nothing will be blocked — an agent can reach any destination\x1b[0m") {
		t.Errorf("the escalation warning is not red:\n%q", b.String())
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
		if code, err := a.cmdNetApprove(args); code != 2 || err == nil {
			t.Errorf("cmdNetApprove(%q) = (%d, %v), want a usage error", args, code, err)
		}
	}
}
