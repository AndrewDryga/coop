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
			return nil, errors.New("net approve accepts --mode once")
		}
		if !inline {
			i++
			if i == len(args) {
				return nil, errors.New("net approve --mode needs open, filtered or none")
			}
			value = args[i]
		}
		parsed, err := egress.ParseMode(value)
		if err != nil {
			return nil, fmt.Errorf("net approve --mode: %w", err)
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
		return 1, errors.New("net approve needs an interactive terminal — an unattended run cannot approve its own network access")
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
		return ui.Confirm("Remember this exact network approval for new runs?", false)
	})
	if err = errors.Join(err, review.Close()); err != nil {
		return 1, err
	}
	ui.OK("network approval saved for new runs — boxes already running are unchanged")
	return 0, nil
}

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
		return errors.New("this network approval review produced nothing to approve")
	}
	var b strings.Builder
	p := ui.For(os.Stderr)
	fmt.Fprintf(&b, "%s\n", p.Bold(p.Cyan("network approval")))
	field := func(label, value string) { netField(&b, p, netLabelWidth, label, value) }
	field("Project", review.Project())
	before, current := review.Before(), "nothing remembered yet"
	if before != nil {
		current = string(before.Posture)
	}
	field("Egress", current+"  →  "+string(after.Posture))
	add, remove := approvalRuleDiff(before, after)
	switch {
	case len(add) == 0 && len(remove) == 0 && len(after.Envelope) == 0:
		field("Rules", "none — this project asks for no destinations of its own")
	case len(add) == 0 && len(remove) == 0:
		field("Rules", ui.Count(len(after.Envelope), "rule")+", unchanged")
	default:
		field("Rules", ui.Count(len(after.Envelope), "rule")+" after this change")
	}
	for _, rule := range add {
		netRow(&b, p.Green("+ "+box.NetworkRuleText(rule)))
	}
	for _, rule := range remove {
		netRow(&b, p.Red("- "+box.NetworkRuleText(rule)))
	}
	if before != nil && len(before.Features) != 0 && len(after.Features) == 0 {
		field("Features", "the previously approved optional provider features are removed")
	}
	fmt.Fprintln(&b, "\nThis remembers the request shown above, OUTSIDE the repository, for NEW runs only.")
	fmt.Fprintln(&b, "Running boxes are unchanged. Nothing is tested or connected to.")
	switch after.Posture {
	case egress.Open:
		fmt.Fprintln(&b, "Open means unrestricted networking: the rule list is not a filter.")
	case egress.None:
		fmt.Fprintln(&b, "None means offline, including provider and MCP connections.")
	default:
		fmt.Fprintln(&b, "A selected agent's core endpoints are captured automatically at each launch;")
		fmt.Fprintln(&b, "a filtered launch still needs 'coop net setup' on this host.")
	}
	if _, err := io.WriteString(out, b.String()); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !confirm() {
		return errors.New("network approval cancelled — nothing changed")
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
