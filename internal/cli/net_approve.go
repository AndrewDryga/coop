package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/ui"
)

// cmdApprove is the ONE verb that turns a repository's request into host
// authority. The mode and the rules it approves come from .agent/project.yaml
// and nowhere else — to change access, edit the file and review it again. It
// requires a terminal on purpose: an approval is a human's decision, and a pipe
// that answers "y" is not a human.
func (a *app) cmdApprove(args []string) (int, error) {
	if err := rejectArgs("approve", args); err != nil {
		return 2, err
	}
	if !ui.IsTerminal(os.Stdin) || !ui.IsTerminal(os.Stderr) {
		return 1, netTerminalOnly("coop approve")
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
	netApproveIntro     = "Review the requested project access changes."
	netApprovePrompt    = "Approve these changes for new runs?"
	netApproveApproved  = "Approved for new runs"
	netApproveUnchanged = "No approval needed — this project's requested access has not changed."
)

func approveMovedError() error {
	return &ui.UsageError{Headline: `"coop net approve" has moved`, Rows: [][2]string{{"Use:", "coop approve"}}}
}

// netTerminalOnly refuses a decision a pipe cannot make. An approval and a
// withdrawal are both a human's: an unattended run answering "y" is not one.
func netTerminalOnly(command string) error {
	return &ui.UsageError{
		Headline: fmt.Sprintf("%q needs your confirmation in a terminal", command),
		Rows:     [][2]string{{"Help:", ui.HelpCommand(command)}},
	}
}

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
	fmt.Fprintf(&b, "%s\n", netApproveIntro)
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

// writeNetAccessChange is the review a person reads before approving: the
// access mode under NETWORK ACCESS — as a before/after pair when it changes,
// one line when it does not — and then the rules, each row saying whether it is
// retained, added or removed. A section with nothing to say is not printed.
func writeNetAccessChange(w io.Writer, p ui.Palette, before *networkstate.Approval, mode egress.Mode, requested []egress.Rule) {
	var approved []egress.Rule
	var approvedMode egress.Mode
	if before != nil {
		approved, approvedMode = before.Envelope, before.Posture
	}
	fmt.Fprintf(w, "\n%s\n", p.Bold(p.Cyan("NETWORK ACCESS")))
	writeNetModeChange(w, p, approvedMode, mode)
	add, remove := box.NetworkRuleDiff(approved, requested)
	if rows := netRuleDiffRows(approved, add, remove, netApprovalLabels); len(rows) != 0 {
		fmt.Fprintf(w, "\n%s\n", p.Bold(p.Cyan("NETWORK RULES")))
		writeNetRuleRows(w, p, rows, 2)
	}
}

// netOpenWarning is the one red line an unrestricted request earns: it is an
// escalation, and a reader about to type y should see it without color too.
const netOpenWarning = "⚠ Nothing will be blocked — an agent can reach any destination"

// netModeDescription is the access mode in the words every network view uses,
// so approve, the posture view and a withdrawal never describe the same mode
// three ways. The raw enum never reaches the screen.
func netModeDescription(mode egress.Mode) string {
	switch mode {
	case egress.Open:
		return "Unrestricted — nothing is blocked."
	case egress.None:
		return "Offline — internet access is blocked."
	default:
		return "Filtered — only approved network traffic is allowed."
	}
}

// netModeWord is the same mode as a noun phrase, for a sentence that names what
// was approved rather than describing it.
func netModeWord(mode egress.Mode) string {
	switch mode {
	case egress.Open:
		return "Unrestricted internet"
	case egress.None:
		return "Offline"
	default:
		return "Filtered"
	}
}
