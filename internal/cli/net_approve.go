package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/ui"
)

// cmdNetApprove is the ONE verb that turns a repository's request into host
// authority. The mode and the rules it approves come from .agent/project.yaml
// and nowhere else — to change access, edit the file and review it again. It
// requires a terminal on purpose: an approval is a human's decision, and a pipe
// that answers "y" is not a human.
func (a *app) cmdNetApprove(args []string) (int, error) {
	if err := rejectArgs("net approve", args); err != nil {
		return 2, err
	}
	if !ui.IsTerminal(os.Stdin) || !ui.IsTerminal(os.Stderr) {
		return 1, errors.New("coop net approve needs a terminal to ask you — an unattended run cannot approve its own network access")
	}
	repo, err := netProject(a.cfg.RepoOverride)
	if err != nil {
		return 1, err
	}
	review, err := box.ReviewProjectNetwork(a.cfg, repo)
	if err != nil {
		return 1, err
	}
	if review.Unchanged() {
		// No security question has an answer that could change anything, so
		// none is asked and nothing is written — not even a fresher timestamp.
		ui.Note("%s", netApproveUnchanged)
		return 0, nil
	}
	err = confirmNetApproval(context.Background(), review, os.Stderr, func() bool {
		return ui.Confirm(netApprovePrompt, false)
	})
	if err = errors.Join(err, review.Close()); err != nil {
		return 1, err
	}
	ui.OK("%s", netApproveApproved)
	return 0, nil
}

// The question and the answer carry the one limit that matters — an approval
// applies to NEW runs — so the review above them can be the change and nothing else.
const (
	netApprovePrompt    = "Approve these changes for new runs?"
	netApproveApproved  = "Approved for new runs"
	netApproveUnchanged = "No approval needed — this project's network access has not changed."
)

// netApprovalReview is what the confirmation needs from a review: the exact
// before and after, and one commit that consumes it.
type netApprovalReview interface {
	Project() string
	Before() *networkstate.Approval
	After() *networkstate.Approval
	Commit(context.Context) error
}

func confirmNetApproval(ctx context.Context, review netApprovalReview, out io.Writer, confirm func() bool) error {
	after := review.After()
	if after == nil {
		return errors.New("there is nothing to approve for this project")
	}
	var b strings.Builder
	p := ui.For(os.Stderr)
	fmt.Fprintf(&b, "%s\n  %s\n\n", p.Bold(p.Cyan("Network access changes for "+filepath.Base(review.Project()))), p.Dim(review.Project()))
	writeNetAccessChange(&b, p, review.Before(), after.Posture, after.Envelope)
	b.WriteString("\n")
	if _, err := io.WriteString(out, b.String()); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !confirm() {
		return errors.New("cancelled — nothing was approved")
	}
	return review.Commit(ctx)
}

// writeNetAccessChange is the one review a person reads before approving, and
// the same one bare `coop net` shows while it is pending: the access mode in
// plain words — with the warning an unrestricted request earns — then the rule
// diff against what is already approved.
func writeNetAccessChange(w io.Writer, p ui.Palette, before *networkstate.Approval, mode egress.Mode, requested []egress.Rule) {
	var approved []egress.Rule
	var approvedMode egress.Mode
	if before != nil {
		approved, approvedMode = before.Envelope, before.Posture
	}
	fmt.Fprintln(w, netModeChange(approvedMode, mode))
	switch mode {
	case egress.Filtered:
		fmt.Fprintln(w, "Only approved websites and services can be reached.")
	case egress.Open:
		fmt.Fprintln(w, p.Red(netOpenWarning))
	case egress.None:
		fmt.Fprintln(w, "An agent cannot reach its provider.")
	}
	add, remove := box.NetworkRuleDiff(approved, requested)
	if len(approved) != 0 || len(add) != 0 || len(remove) != 0 {
		fmt.Fprintln(w)
		writeNetRuleDiff(w, p, approved, add, remove)
	}
}

// netOpenWarning is the one red line an unrestricted request earns: it is an
// escalation, and a reader about to type y should see it without color too.
const netOpenWarning = "⚠ Nothing will be blocked — an agent can reach any destination"

// netModeChange says what the access mode becomes, against what it was: the
// raw enums never reach the screen, and "offline" is the human word for none.
func netModeChange(before, after egress.Mode) string {
	switch {
	case before == "":
		return "Internet access will be " + netModeWord(after) + "."
	case before == after:
		return "Internet access remains " + netModeWord(after) + "."
	default:
		return "Internet access changes from " + netModeWord(before) + " to " + netModeWord(after) + "."
	}
}

func netModeWord(mode egress.Mode) string {
	switch mode {
	case egress.Open:
		return "unrestricted"
	case egress.None:
		return "offline — no external network access"
	default:
		return "filtered"
	}
}
