package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/scaffold"
	"github.com/AndrewDryga/coop/internal/ui"
)

// cmdApprove is the ONE verb that turns a repository's request into host
// authority. The mode and the rules it approves come from .agent/project.yaml
// and nowhere else — to change access, edit the file and review it again. It
// requires a terminal on purpose: an approval is a human's decision, and a pipe
// that answers "y" is not a human.
func (a *app) cmdApprove(args []string) (int, error) {
	caseExposure := false
	for _, arg := range args {
		if arg != "--case-exposure" {
			if strings.HasPrefix(arg, "-") {
				return 2, unknownOptionErr(arg, "coop approve", []string{"--case-exposure"})
			}
			return 2, ui.UnexpectedArgument(arg, "coop approve", "coop approve [--case-exposure]")
		}
		if caseExposure {
			return 2, ui.RepeatedOption(arg, "coop approve")
		}
		caseExposure = true
	}
	if !ui.IsTerminal(os.Stdin) || !ui.IsTerminal(os.Stderr) {
		return 1, a.approveWithoutTerminal(caseExposure)
	}
	repo, err := netProject(a.cfg.RepoOverride)
	if err != nil {
		return 1, err
	}
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	return a.approveProject(ctx, repo, caseExposure, func(prompt string) bool {
		return confirmTerminalPrompt(ctx, prompt)
	})
}

func (a *app) approveProject(ctx context.Context, repo string, caseExposure bool, confirm func(string) bool) (int, error) {
	review, err := box.ReviewProjectNetworkContext(ctx, a.cfg, repo)
	if err != nil {
		return 1, err
	}
	// Future images and options can select mount protection even on an eBPF
	// runtime. Accepting that exposure is an explicit decision, never a side
	// effect of reviewing network access.
	var caseReview *box.CaseExposureReview
	if caseExposure {
		caseReview, err = box.ReviewCaseExposure(ctx, a.cfg, repo)
		if err != nil {
			return 1, errors.Join(err, review.Close())
		}
	}
	if review.Unchanged() && (caseReview == nil || len(caseReview.Pending) == 0) {
		// No security question has an answer that could change anything, so
		// none is asked and nothing is written — not even a fresher timestamp.
		// A box stuck on service rules nobody has asked for yet is the exception,
		// and that line alone would read as if nothing is wrong.
		services, err := box.ProjectServicesRefusal(ctx, a.cfg, repo)
		if err != nil {
			return 1, err
		}
		if services == nil {
			ui.Note("%s", netApproveUnchanged)
			return 0, nil
		}
		return a.offerServiceRules(ctx, repo, services, confirm)
	}
	prompt, done := netApprovePrompt, netApproveApproved
	if review.Unchanged() {
		prompt, done = "Remember this file access for future runs and service starts using mount protection?", "File access approved for future runs and service starts using mount protection"
	}
	err = confirmProjectApproval(ctx, review, caseReview, os.Stderr, func() bool {
		return confirm(prompt)
	})
	code, err := reportNetChange(errors.Join(err, review.Close()), done)
	if err != nil {
		return code, err
	}
	// What was approved may still leave the box stuck, and this is the moment to settle that too.
	services, err := box.ProjectServicesRefusal(ctx, a.cfg, repo)
	if err != nil || services == nil {
		return code, err
	}
	ui.Note("")
	return a.offerServiceRules(ctx, repo, services, confirm)
}

// approveWithoutTerminal is `coop approve` with nobody there to answer. It asks nothing and writes
// nothing, and it fails either way, since nothing got settled. When only service rules hold the box up
// it says what they are: reading them needs no decision, and the person can act on them later.
func (a *app) approveWithoutTerminal(caseExposure bool) error {
	refused := netTerminalOnly("coop approve")
	if caseExposure {
		return refused
	}
	repo, err := netProject(a.cfg.RepoOverride)
	if err != nil {
		return refused
	}
	ctx := context.Background()
	access, err := box.ProjectNetworkAccess(ctx, a.cfg, repo)
	if err != nil || access.Pending != nil {
		return refused
	}
	services, err := box.ProjectServicesRefusal(ctx, a.cfg, repo)
	if err != nil || services == nil {
		return refused
	}
	return services
}

// offerServiceRules settles a box stuck on missing service rules in one question. Coop knows the names
// and ports, so typing them decides nothing: the decision is this review, and yes both writes the rules
// and approves them. Only the terminal path reaches this, like every other approval. A service with no
// stated port never gets a guessed one, so its rule stays the person's.
func (a *app) offerServiceRules(ctx context.Context, repo string, services *box.ServicesNeedRules, confirm func(string) bool) (int, error) {
	offer := services.Offer()
	if len(offer) == 0 {
		ui.Warning(services.Headline(), "", services.TerminalRemedy())
		return 0, nil
	}
	ui.Warning(services.Headline(), "", serviceRulesOfferText(offer, services.NoPort()))
	ui.Note("")
	confirmed := confirm(serviceRulesPrompt)
	if err := ctx.Err(); err != nil {
		return 1, err
	}
	if !confirmed {
		ui.Note("Nothing changed. This project's box stays blocked until its services have approved rules.")
		ui.Note("%s", services.TurnOff)
		return 0, nil
	}
	rules := make([]scaffold.ServiceRule, 0, len(offer))
	for _, draft := range offer {
		rules = append(rules, scaffold.ServiceRule{Service: draft.Name, Ports: draft.Ports})
	}
	added, err := scaffold.AddApprovedServiceRules(repo, rules)
	if err != nil {
		return 1, fmt.Errorf("could not add the rules to %s, so nothing was approved: %w", project.File, err)
	}
	if len(added.Unplaced) != 0 {
		return 1, fmt.Errorf("%s has a layout Coop could not update safely, so nothing changed\n\n  %s", project.File, services.PasteRemedy())
	}
	review, err := box.ReviewProjectNetworkContext(ctx, a.cfg, repo)
	if err != nil {
		return 1, fmt.Errorf("the rules are in %s, but they were not approved: %w\n\n  Review them: coop approve", project.File, err)
	}
	if !review.AddsOnly(services) {
		// The answer covers what the question showed, so a yes never carries a later edit along.
		err := review.Discard(context.WithoutCancel(ctx))
		return 1, errors.Join(errors.New("this project's files changed after that question, so nothing was approved\n\n  Review them: coop approve"), err, review.Close())
	}
	return reportNetChange(errors.Join(review.Commit(ctx), review.Close()), serviceRulesApproved)
}

// serviceRulesOfferText names what a yes adds, and what it leaves for the person.
func serviceRulesOfferText(offer, noPort []box.ServiceDraft) string {
	width := 0
	for _, draft := range offer {
		width = max(width, len(draft.Name))
	}
	lines := []string{"Coop can add rules for these services to " + project.File + " and approve them:"}
	for _, draft := range offer {
		ports := make([]string, 0, len(draft.Ports))
		for _, port := range draft.Ports {
			ports = append(ports, strconv.Itoa(port))
		}
		lines = append(lines, fmt.Sprintf("    %-*s  TCP %s", width, draft.Name, strings.Join(ports, ", ")))
	}
	if len(noPort) != 0 {
		lines = append(lines, "", "  Coop cannot tell these services' ports. To use one, add its rule with the port it listens on, then run coop approve:")
		for _, draft := range noPort {
			lines = append(lines, draft.RuleText())
		}
	}
	return strings.Join(lines, "\n")
}

// reportNetChange finishes a confirmed approval change. One that was saved but could not reach
// running editor sessions is still a success — new runs use it, and only those sessions wait for a
// reconnect — so it gets its ✓ and a caution, never a ✗.
func reportNetChange(err error, done string) (int, error) {
	untold := errors.Is(err, networkstate.ErrSessionsNotTold)
	if err != nil && !untold {
		return 1, err
	}
	ui.OK("%s", done)
	if untold {
		ui.Warn("%s", netSessionsNotTold)
	}
	return 0, nil
}

func confirmTerminalPrompt(ctx context.Context, prompt string) bool {
	if ctx.Err() != nil {
		return false
	}
	answered := make(chan bool, 1)
	go func() { answered <- ui.Confirm(prompt, false) }()
	select {
	case answer := <-answered:
		return answer
	case <-ctx.Done():
		return false
	}
}

// The question and the answer name everything a yes changes — new runs, and
// running editor sessions (coop acp), which move to new boxes under it — so the
// review above them can be the change and nothing else.
const (
	netApproveIntro     = "Review the requested project access."
	netApprovePrompt    = "Approve this access for new runs and running editor sessions?"
	netApproveApproved  = "Approved for new runs and running editor sessions"
	netApproveUnchanged = "No approval needed: this project's requested access has not changed."
	netSessionsNotTold  = "Running editor sessions keep their current network rules until you reconnect them."

	serviceRulesPrompt   = "Add these rules and approve them for new runs and running editor sessions?"
	serviceRulesApproved = "Rules added and approved for new runs and running editor sessions"
)

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
	Pending() *networkstate.PendingApproval
	Commit(context.Context) error
	Discard(context.Context) error
}

func confirmProjectApproval(ctx context.Context, review netApprovalReview, cases *box.CaseExposureReview, out io.Writer, confirm func() bool) error {
	after := review.After()
	if after == nil && (cases == nil || len(cases.Pending) == 0) {
		return errors.New("there is nothing to approve for this project")
	}
	var b strings.Builder
	p := ui.For(os.Stderr)
	fmt.Fprintf(&b, "%s\n", netApproveIntro)
	if pending := review.Pending(); pending != nil && pending.Sentence() != "" {
		fmt.Fprintf(&b, "\nWhy approval is needed:\n  %s\n", pending.Sentence())
	}
	if after != nil {
		writeNetAccessChange(&b, p, review.Before(), after.Posture, after.Envelope)
	}
	if cases != nil && len(cases.Pending) > 0 {
		fmt.Fprintln(&b, "\nFILE ACCESS WITH MOUNT PROTECTION")
		fmt.Fprintln(&b, "  With mount protection, these hidden files can be read under another letter case:")
		for _, path := range cases.Paths {
			fmt.Fprintf(&b, "    %s\n", ui.SafeInline(path))
		}
		fmt.Fprintln(&b, "  Qualified eBPF runs still protect these files without this approval.")
		fmt.Fprintln(&b, "  Approval remembers these paths, including future contents, for agents and services using mount protection.")
		fmt.Fprintln(&b, "  New paths need approval. Already running boxes stay unchanged.")
	}
	if _, err := io.WriteString(out, b.String()); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	confirmed := confirm()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !confirmed {
		// A decline is not an interruption: leave no marker for the next review to misread.
		if err := review.Discard(context.WithoutCancel(ctx)); err != nil {
			return fmt.Errorf("cancelled — nothing was approved, but the pending marker could not be removed: %w", err)
		}
		return errors.New("cancelled — nothing was approved")
	}
	// Save file access before the network generation wakes editor supervisors.
	if cases != nil {
		if err := cases.Commit(ctx); err != nil {
			return fmt.Errorf("could not save file access approval: %w", err)
		}
	}
	if after != nil {
		err := review.Commit(ctx)
		if err != nil && !errors.Is(err, networkstate.ErrSessionsNotTold) && cases != nil && len(cases.Pending) > 0 {
			return fmt.Errorf("file access was saved, but network approval failed: %w", err)
		}
		return err
	}
	return nil
}

// writeNetAccessChange is the review a person reads before approving: the
// access mode under NETWORK ACCESS — as a before/after pair when it changes,
// one line when it does not — and then the rules, each row's marker saying
// whether it is retained, added or removed. A section with nothing to say is not
// printed.
func writeNetAccessChange(w io.Writer, p ui.Palette, before *networkstate.Approval, mode egress.Mode, requested []egress.Rule) {
	var approved []egress.Rule
	var approvedMode egress.Mode
	if before != nil {
		approved, approvedMode = before.Envelope, before.Posture
	}
	fmt.Fprintf(w, "\n%s\n", p.Bold(p.Cyan("NETWORK ACCESS")))
	writeNetModeChange(w, p, approvedMode, mode)
	add, remove := box.NetworkRuleDiff(approved, requested)
	if rows := netRuleDiffRows(approved, add, remove, netRuleLabels{}); len(rows) != 0 {
		fmt.Fprintf(w, "\n%s%s\n", p.Bold(p.Cyan("NETWORK RULES")), netRuleLegend(p, rows))
		writeNetRuleRows(w, p, rows, 2)
	}
}

// netOpenWarning is the one red line an unrestricted request earns: it is an
// escalation, and a reader about to type y should see it without color too.
const netOpenWarning = "⚠ An agent can reach any destination; nothing will be blocked"

// netModeDescription is the access mode in the words every network view uses,
// so approve, the posture view and a withdrawal never describe the same mode
// three ways. The raw enum never reaches the screen.
func netModeDescription(mode egress.Mode) string {
	switch mode {
	case egress.Open:
		return "Unrestricted: nothing is blocked."
	case egress.None:
		return "Offline: internet access is blocked."
	default:
		return "Filtered: only approved network traffic is allowed."
	}
}
