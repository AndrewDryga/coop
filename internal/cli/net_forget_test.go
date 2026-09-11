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

type fakeForgetReview struct {
	approval  *networkstate.Approval
	gone      bool
	committed int
	commitErr error
}

func (f *fakeForgetReview) Project() string                  { return "/private/tmp/project" }
func (f *fakeForgetReview) Approval() *networkstate.Approval { return f.approval }
func (f *fakeForgetReview) Gone() bool                       { return f.gone }
func (f *fakeForgetReview) Commit(context.Context) error     { f.committed++; return f.commitErr }

func TestForgetProjectFlagParsing(t *testing.T) {
	if project, err := parseNetForgetArgs([]string{"--project", "/tmp/gone"}); err != nil || project != "/tmp/gone" {
		t.Errorf("parseNetForgetArgs = (%q, %v)", project, err)
	}
	if project, err := parseNetForgetArgs([]string{"--project=/tmp/gone"}); err != nil || project != "/tmp/gone" {
		t.Errorf("inline --project = (%q, %v)", project, err)
	}
	if project, err := parseNetForgetArgs(nil); err != nil || project != "" {
		t.Errorf("no flags = (%q, %v)", project, err)
	}
	for _, args := range [][]string{{"--project"}, {"--project="}, {"--yes"}, {"--project", "/a", "--project", "/b"}} {
		if _, err := parseNetForgetArgs(args); err == nil {
			t.Errorf("parseNetForgetArgs(%q) was accepted", args)
		}
	}
	// A bare path is the natural typo for a flag-only verb; say which flag it is.
	_, err := parseNetForgetArgs([]string{"/tmp/gone"})
	if err == nil || !strings.Contains(err.Error(), "--project <path>") {
		t.Errorf("a bare path = %v, want the flag spelled out", err)
	}
}

// The preview names the blast radius before the question: the approval that
// goes, and the evidence that does not. Its exact bytes are pinned by
// TestApprovedNetForget; this is the contract behind them.
func TestForgetPreviewShowsWhatGoesAndWhatStays(t *testing.T) {
	review := &fakeForgetReview{approval: &networkstate.Approval{Posture: egress.Filtered,
		Envelope: []egress.Rule{approvalRule("old.example"), approvalRule("other.example")}}}
	var out bytes.Buffer
	if err := confirmNetForget(context.Background(), review, &out, func() bool { return true }); err != nil {
		t.Fatalf("confirmNetForget: %v", err)
	}
	text := out.String()
	for _, want := range append([]string{netForgetTitle, "  Approved: Filtered",
		"    old.example:443 · TLS", "    other.example:443 · TLS", netForgetRestore}, netForgetConsequences...) {
		if !strings.Contains(text, want) {
			t.Errorf("forget preview is missing %q:\n%s", want, text)
		}
	}
	// Withdrawing can be undone by approving again, so the preview must not
	// borrow the rm gate's "this can't be undone".
	if strings.Contains(text, "can't be undone") {
		t.Errorf("the forget preview claims an unrecoverable delete:\n%s", text)
	}
	if review.committed != 1 {
		t.Errorf("committed %d times, want 1", review.committed)
	}
}

func TestForgetPreviewSaysWhenTheProjectItselfIsGone(t *testing.T) {
	review := &fakeForgetReview{gone: true, approval: &networkstate.Approval{Posture: egress.None}}
	var out bytes.Buffer
	if err := confirmNetForget(context.Background(), review, &out, func() bool { return true }); err != nil {
		t.Fatalf("confirmNetForget: %v", err)
	}
	for _, want := range []string{netForgetGone, "  Approved: Offline"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("gone-project preview is missing %q:\n%s", want, out.String())
		}
	}
}

// The preview must be shown BEFORE the question, and a "no" must remove
// nothing: a human who declines has decided, not deferred.
func TestForgetCancelledRemovesNothing(t *testing.T) {
	review := &fakeForgetReview{approval: &networkstate.Approval{Posture: egress.Filtered}}
	var out bytes.Buffer
	err := confirmNetForget(context.Background(), review, &out, func() bool {
		if out.Len() == 0 {
			t.Error("the confirmation was asked before the approval was shown")
		}
		return false
	})
	// Declining is an ANSWER: the caller prints what was kept and exits 0, so
	// the sentinel carries no message of its own.
	if !errors.Is(err, errNetDeclined) {
		t.Fatalf("cancelled forget = %v", err)
	}
	if review.committed != 0 {
		t.Error("a declined forget was committed")
	}
}

func TestForgetRefusesAnEmptyReview(t *testing.T) {
	if err := confirmNetForget(context.Background(), &fakeForgetReview{}, &bytes.Buffer{}, func() bool { return true }); err == nil {
		t.Fatal("a review with nothing to forget was confirmed")
	}
}

func TestForgetSurfacesACommitFailure(t *testing.T) {
	review := &fakeForgetReview{approval: &networkstate.Approval{Posture: egress.Filtered},
		commitErr: errors.New("the approval for /private/tmp/project was already gone — nothing was removed")}
	err := confirmNetForget(context.Background(), review, &bytes.Buffer{}, func() bool { return true })
	if err == nil || !strings.Contains(err.Error(), "nothing was removed") {
		t.Fatalf("commit failure = %v", err)
	}
}
