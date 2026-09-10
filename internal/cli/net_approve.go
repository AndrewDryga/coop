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

func parseNetApproveArgs(args []string) (*egress.Mode, error) {
	var mode *egress.Mode
	for i := 0; i < len(args); i++ {
		name, value, inline := strings.Cut(args[i], "=")
		if name != "--mode" {
			return nil, unknownErr("net approve flag", args[i], []string{"--mode"})
		}
		if mode != nil {
			return nil, errors.New("coop net approve takes --mode once")
		}
		if !inline {
			i++
			if i == len(args) {
				return nil, errors.New("coop net approve --mode needs open, filtered or none")
			}
			value = args[i]
		}
		parsed, err := egress.ParseMode(value)
		if err != nil {
			return nil, fmt.Errorf("coop net approve --mode: %w", err)
		}
		mode = &parsed
	}
	return mode, nil
}

// cmdNetApprove is the ONE verb that turns a repository's request into host
// authority. It requires a terminal on purpose: an approval is a human's
// decision, and a pipe that answers "y" is not a human.
func (a *app) cmdNetApprove(args []string) (int, error) {
	mode, err := parseNetApproveArgs(args)
	if err != nil {
		return 2, err
	}
	if !ui.IsTerminal(os.Stdin) || !ui.IsTerminal(os.Stderr) {
		return 1, errors.New("coop net approve needs a terminal to ask you — an unattended run cannot approve its own network access")
	}
	repo, err := netProject(a.cfg.RepoOverride)
	if err != nil {
		return 1, err
	}
	review, err := box.ReviewProjectNetwork(a.cfg, repo, mode)
	if err != nil {
		return 1, err
	}
	err = confirmNetApproval(context.Background(), review, os.Stderr, func() bool {
		return ui.Confirm(netApprovePrompt, false)
	})
	if err = errors.Join(err, review.Close()); err != nil {
		return 1, err
	}
	ui.OK("%s", netApproveRemembered)
	return 0, nil
}

// The question and the answer carry the one limit that matters — an approval
// applies to NEW runs — so the review above it can be the diff and nothing else.
const (
	netApprovePrompt     = "Remember this for new runs?"
	netApproveRemembered = "remembered — applies to new runs; boxes already running keep their current rules"
)

// netApprovalReview is what the confirmation needs from a review: the exact
// before and after, and one commit that consumes it.
type netApprovalReview interface {
	Project() string
	Mode() egress.Mode
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
	block := newNetBlock(&b, p, "Network approval for "+review.Project())
	before := review.Before()
	switch {
	case before == nil:
		block.field("Egress", string(after.Posture)+" (nothing was remembered before)")
	case before.Posture == after.Posture:
		block.field("Egress", string(after.Posture)+" (unchanged)")
	default:
		block.field("Egress", string(before.Posture)+"  →  "+string(after.Posture))
	}
	add, remove := approvalRuleDiff(before, after)
	switch {
	case len(add) == 0 && len(remove) == 0 && len(after.Envelope) == 0:
		block.field("Rules", "none — this project asks for no destinations of its own")
	case len(add) == 0 && len(remove) == 0:
		block.field("Rules", ui.Count(len(after.Envelope), "rule")+", unchanged")
	default:
		block.field("Rules", ui.Count(len(after.Envelope), "rule")+" after this change")
	}
	for _, rule := range add {
		block.row(p.Green("+ " + box.NetworkRuleText(rule)))
	}
	for _, rule := range remove {
		block.row(p.Red("- " + box.NetworkRuleText(rule)))
	}
	if before != nil && len(before.Features) != 0 && len(after.Features) == 0 {
		block.field("Dropped", "the optional provider features approved before")
	}
	// One line, only where the word itself would mislead: "open" and "none" are
	// not degrees of filtering, and a reader about to type y should know that.
	switch after.Posture {
	case egress.Open:
		block.field("Note", "open is no filtering at all — every destination is reachable")
	case egress.None:
		block.field("Note", "none is offline — no provider or MCP connections either")
	}
	block.flush(&b)
	if _, err := io.WriteString(out, b.String()); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !confirm() {
		return errors.New("cancelled — nothing was remembered")
	}
	return review.Commit(ctx)
}

// approvalRuleDiff is the plain before/after set difference a human reviews.
func approvalRuleDiff(before, after *networkstate.Approval) (add, remove []egress.Rule) {
	var previous []egress.Rule
	if before != nil {
		previous = before.Envelope
	}
	return box.NetworkRuleDiff(previous, after.Envelope)
}
