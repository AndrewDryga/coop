package loop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/ladder"
	"github.com/AndrewDryga/coop/internal/ui"
)

// candidateReviewHeading names the review about to run. A round whose review failed keeps its number
// for the review of its rework, so that one says it is the same round again.
func candidateReviewHeading(review CandidateReviewSpec) string {
	switch {
	case review.AfterRework:
		return fmt.Sprintf("Review round %d again, after the rework of its findings.", review.Round)
	case review.PreviousRound > 0:
		return fmt.Sprintf("Review round %d supersedes round %d.", review.Round, review.PreviousRound)
	}
	return fmt.Sprintf("Review round %d covers the complete fork candidate.", review.Round)
}

// rebasePass is what one fork rebase pass needs from the run that hosts it.
type rebasePass struct {
	repo, img, forkName, runid, continueCmd string
	rot                                     *ladder.Rotation
	hosts                                   []string
	sink                                    io.Writer
	peers                                   []agents.Target
	iterCmd                                 func(agent, prompt string) ([]string, bool, bool)
	wake                                    <-chan struct{}
	stopped                                 func() bool
}

// runRebasePass runs the fork's rebase agent until one attempt succeeds, with the work iteration's
// rotation and retries: a usage limit rotates to the next target or waits for its reset, a dead
// sign-in moves to another account, and an ordinary failure retries a few times. It holds no task,
// so an attempt that fails leaves nothing to undo: the next one starts by aborting any rebase the
// last left. The caller checks the rebase itself. A check-only pass runs the same way.
func (c *Control) runRebasePass(ctx context.Context, rebase RebaseSpec, p rebasePass) (int, error) {
	ui.Note("")
	what := "Rebase"
	switch {
	case rebase.CheckOnly:
		what = "Rebase check"
		ui.Note("Checking %s, rebased onto %s (%s), before its review.", rebase.Branch, rebase.ParentBranch, rebase.Onto[:12])
	case rebase.Changed:
		ui.Note("Rebasing %s onto %s (%s), where its reviewed change would land changed.", rebase.Branch, rebase.ParentBranch, rebase.Onto[:12])
	default:
		ui.Note("Rebasing %s onto %s (%s), which its reviewed commits conflict with.", rebase.Branch, rebase.ParentBranch, rebase.Onto[:12])
	}
	fails, waits, retries := 0, 0, 0
	for {
		agent := c.applyTarget(p.rot)
		target := p.rot.Active()
		start, headBefore := time.Now(), forkspace.GitOut(p.repo, "rev-parse", "HEAD")
		cmd, streaming, agentCommand := p.iterCmd(agent, forkRebasePrompt(rebase))
		code, _, res, classification, windows, runErr := c.runIteration(ctx, p.repo, p.img, agent, p.forkName, cmd, streaming, agentCommand, p.hosts, completionWindowReview, nil, nil, false, p.sink, p.peers, "rebase", "", nil)
		if stopsTheRun(runErr) {
			return 1, errors.Join(runErr, windows.Close())
		}
		if _, err := windows.FinishReview(); err != nil {
			return 1, fmt.Errorf("the rebase pass changed task completion ownership: %w", err)
		}
		c.recordStage(p.repo, p.runid, "rebase", classification.outcome, target, start, code, retries, 0, headBefore, p.hosts, nil, nil, res)
		if ctx.Err() != nil {
			ui.Failure(what+" interrupted", "Nothing was reviewed or merged.", [2]string{"Continue:", p.continueCmd})
			return LoopInterruptedExitCode, ui.Reported(ctx.Err())
		}
		action, wait, resetAt := decideIteration(classification, time.Now(), &fails, &waits, &retries)
		if action == actContinue {
			if rebase.Report != nil {
				rebase.Report(rebaseChecks(c.finalMessage))
			}
			return 0, nil
		}
		if p.stopped() {
			ui.Failure(what+" interrupted", "Nothing was reviewed or merged.", [2]string{"Continue:", p.continueCmd})
			return LoopInterruptedExitCode, ui.Reported(errors.New("rebase pass stopped"))
		}
		switch action {
		case actWait:
			if p.rot.Rotates() {
				c.rotateOnLimit(p.rot, resetAt, &waits, p.wake)
			} else {
				c.waitOutLimit(wait, resetAt, p.wake)
			}
		case actRetryNow:
			ladder.SleepOrWake(wait, p.wake)
		case actRetry:
			ui.Alert(what+" attempt failed",
				wrappedLoopText(attemptCause(attemptSubject(agent, nil), code, runErr), 6)+"\n"+
					fmt.Sprintf("Retrying in 10 seconds · attempt %d of %d.", fails+1, maxLoopFailures))
			ladder.SleepOrWake(10*time.Second, p.wake)
		case actAuthStop:
			if p.rot.Rotates() && p.rot.OnAuthFailure() {
				ui.Alert(authHeadline(target),
					wrappedLoopText(fmt.Sprintf("Continuing with %s.", cleanDiagnosticLine(agents.DisplayTarget(p.rot.Active().String()))), 6),
					[2]string{"Sign in again:", loginCommand(target)})
				continue
			}
			ui.Failure("No configured agent could sign in", "Authentication failed for every available account.",
				[2]string{"Sign in again:", loginCommand(target)})
			return 1, ui.Reported(rotationAuthenticationError(p.rot, target))
		case actLimitStop:
			ui.Failure(fmt.Sprintf("Stopped after %d usage-limit waits", maxLimitWaits),
				"Every configured agent was still rate limited. Nothing was reviewed or merged.",
				[2]string{"Continue:", p.continueCmd})
			return 1, ui.Reported(fmt.Errorf("still rate limited after %d waits", maxLimitWaits))
		default: // actStop, actOutputStop
			ui.Failure(what+" pass failed",
				wrappedLoopText("Last attempt: "+attemptCause(attemptSubject(agent, nil), code, runErr), 6)+"\nNothing was reviewed or merged.",
				[2]string{"Continue:", p.continueCmd})
			return 1, ui.Reported(errors.Join(runErr, errors.New(strings.ToLower(what)+" pass failed")))
		}
	}
}
