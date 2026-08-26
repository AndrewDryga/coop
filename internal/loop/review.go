package loop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/ladder"
	"github.com/AndrewDryga/coop/internal/loopcfg"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

// reviewLadder parses a review stage's raw .agent/loop.yaml agent: rungs into targets, PRESERVING
// provider, model, effort, and account (and every fallback rung) — dropping only preset rungs, since
// a once-per-stage review takes targets, not a rotation of presets. It replaces the old stepModel,
// which kept only (model, effort) off the first rung and discarded the provider — so a claude-led
// run's `codex:…` signoff resolved to `claude --model <a-codex-model>`, an invalid combination, and
// the cross-vendor reviewer the config promised was never actually run.
func reviewLadder(rungs []string) ([]agents.Target, error) {
	rs, err := loopcfg.Rungs(rungs)
	if err != nil {
		return nil, err
	}
	var targets []agents.Target
	for _, r := range rs {
		if r.Target != nil {
			targets = append(targets, *r.Target)
		}
	}
	return targets, nil
}

// reviewRotation builds a review stage's own rotation from its ladder, so the stage runs on the
// configured provider/model/effort/account and rotates its OWN fallback rungs on a rate limit —
// exactly like the work loop. An empty (or preset-only) ladder falls back to def: between → signoff
// → the work rotation, so an unconfigured stage still reviews on the work target.
func (c *Control) reviewRotation(rungs []string, workAgent string, def *ladder.Rotation) (*ladder.Rotation, error) {
	targets, err := reviewLadder(rungs)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return def, nil
	}
	return c.host.buildRotation(workAgent, targets)
}

// iterationCmdBuilder builds one attempt's argv for a stage, given the rotation's active agent.
type iterationCmdBuilder func(agent, prompt string) (cmd []string, streaming, agentCommand bool)

var (
	errReviewInterrupted      = errors.New("review interrupted")
	errReviewVerdict          = errors.New("review verdict invalid")
	errReviewVerdictMalformed = errors.New("review verdict malformed")
)

type completionWindowMode uint8

const (
	completionWindowStrict completionWindowMode = iota
	completionWindowReview
	completionWindowWork
)

type reviewRunResult struct {
	output   string
	usage    *iterResult
	outcome  string
	exit     int
	retries  int
	target   agents.Target
	reopened []string
	// concurrent holds non-subject tasks a parallel host controller completed while a review
	// window was open. They must enter later signoff bookkeeping rather than be absorbed.
	concurrent []string
}

func interruptedReviewResult(last reviewRunResult, retries int) reviewRunResult {
	last.outcome = "interrupted"
	last.exit = LoopInterruptedExitCode
	last.retries = retries
	return last
}

func iterationAuthenticationError(target agents.Target) error {
	if account := target.Account(); account != "" {
		return fmt.Errorf("%s authentication failed for account %q — run `%s`", target.Provider, account, loginCommand(target))
	}
	return fmt.Errorf("%s authentication failed — run `%s`", target.Provider, loginCommand(target))
}

// loginCommand renders the `coop login` invocation that restores one target's credential.
func loginCommand(t agents.Target) string {
	if account := t.Account(); account != "" {
		return "coop login " + t.Provider + "@" + account
	}
	return "coop login " + t.Provider
}

// rotationAuthenticationError reports a run that has no usable credential left. Once a rotation has
// burned through several accounts, naming only the last one tried would send the human to restore
// one login and hit the same wall on the next rung — so list every account that failed.
func rotationAuthenticationError(r *ladder.Rotation, target agents.Target) error {
	failed := r.AuthFailedTargets()
	if len(failed) < 2 {
		return iterationAuthenticationError(target)
	}
	names := make([]string, 0, len(failed))
	for _, t := range failed {
		names = append(names, t.String())
	}
	return fmt.Errorf("authentication failed for every target (%s) — restore one with `%s`",
		strings.Join(names, ", "), loginCommand(failed[0]))
}

func reviewRepoReadOnly(writes loopcfg.ReviewWrites) bool { return !writes.RepositoryWritable() }

func reviewReadOnlyPaths(mode completionWindowMode, repoReadOnly bool, hosts []string) []string {
	if mode != completionWindowReview || repoReadOnly {
		return nil
	}
	return hosts
}

// runReview runs one review stage (signoff or between) on its OWN rotation — the configured
// provider, model, effort, and account — and fails CLOSED. A rate limit rotates the stage's ladder
// (or waits) and retries; a launch error or a nonzero, non-limit exit is retried within a small
// budget, and if the stage still can't run it returns an error so the caller can't mistake "nothing
// reopened" for "reviewed and accepted". A user interrupt is returned distinctly from a review
// failure. The result preserves the terminal attempt and retry count so every caller records the
// same truthful stage telemetry before deciding whether to continue.
// subjects are the exact task ids under review: their completion evidence stays strict inside the
// stage's completion window, while a non-subject task a parallel host session completes during the
// window is reported as concurrent activity instead of killing the run.
// Local counters keep review trouble out of the work loop's stop accounting.
func (c *Control) runReview(ctx context.Context, repo, img string, rev *ladder.Rotation, forkName, prompt, activity string, iterCmd iterationCmdBuilder, hosts, subjects []string, writes loopcfg.ReviewWrites, sink io.Writer, peers []agents.Target, wake <-chan struct{}, observeHandoff reviewAttemptObserver) (reviewRunResult, error) {
	var fails, waits, outputRetries, totalRetries, handoffs, timeouts int
	var concurrent []string
	last := reviewRunResult{target: rev.Active()}
	for {
		if reviewStopRequested(ctx, wake) {
			return interruptedReviewResult(last, totalRetries), errReviewInterrupted
		}
		agent := c.applyTarget(rev)
		target := rev.Active()
		cmd, streaming, agentCommand := iterCmd(agent, prompt) // build after rotation so argv matches this provider
		start, headBefore := time.Now(), gitOut(repo, "rev-parse", "HEAD")
		code, out, usage, classification, windows, runErr := c.runIteration(ctx, repo, img, agent, forkName, cmd, streaming, agentCommand, hosts, completionWindowReview, subjects, reviewRepoReadOnly(writes), sink, peers, activity, "")
		last = reviewRunResult{output: out, usage: usage, outcome: classification.outcome, exit: code, retries: totalRetries, target: target, concurrent: concurrent}
		if errors.Is(runErr, tasks.ErrCompletionWindowSetup) {
			return last, runErr
		}
		observed, completionErr := windows.FinishReview()
		if len(observed) > 0 {
			ui.Info("concurrent host completion during review: %s — a parallel host session's change, not this review's", strings.Join(observed, ", "))
			concurrent = slices.Compact(slices.Sorted(slices.Values(append(concurrent, observed...))))
			last.concurrent = concurrent
		}
		if completionErr != nil {
			return last, fmt.Errorf("%w: review stage changed task completion ownership: %v", tasks.ErrCompletionWindowAudit, completionErr)
		}
		if ctx != nil && ctx.Err() != nil {
			return interruptedReviewResult(last, totalRetries), errReviewInterrupted
		}
		// The entrypoint only reports a handoff after the provider ended while work it started was
		// still live. Its review receipt is therefore not an observed verdict: discard it and rerun
		// the review with a fresh provider that can inspect the settled result.
		if isBackgroundHandoff(classification.outcome) {
			if observeHandoff != nil {
				observeHandoff(last, start, headBefore)
			}
			handoffs++
			if handoffs >= 3 {
				return last, fmt.Errorf("review provider ended with live background work %d times — stopped; rerun the review after its gate, consult, and delegate work finish in the foreground", handoffs)
			}
			totalRetries++
			ui.Warn("review provider ended with live background work; discarding its receipt and starting a fresh observed attempt (%d/3)", handoffs)
			continue
		}
		// A timed-out review attempt was killed for proven silence, so any receipt it printed
		// is not an observed verdict: discard it, rotate without cooling, and retry under the
		// dedicated timeout cap. Three consecutive timeouts stop the stage — a review that
		// can't run is never an accept.
		if isProviderTimeout(classification.outcome) {
			last.output = ""
			timeouts++
			if timeouts >= maxProviderTimeouts {
				return last, fmt.Errorf("review provider attempt timed out %d times in a row (%s)%s — stopping (a review that can't run is never an accept)", timeouts, classification.outcome, classification.timeoutDetail())
			}
			if observeHandoff != nil {
				observeHandoff(last, start, headBefore)
			}
			totalRetries++
			rev.AdvanceOnTimeout(time.Now())
			ui.Warn("review provider attempt timed out (%s)%s — discarding its partial output and retrying (%d/%d)", classification.outcome, classification.timeoutDetail(), timeouts, maxProviderTimeouts)
			continue
		}
		handoffs, timeouts = 0, 0
		if receipt, ok := reviewReopenReceipt(out); ok && len(receipt.reopened) > 0 && classification.outcome != "success" {
			verdictErr := fmt.Errorf("failed review stage declared reopen for %s; verdict was not applied", strings.Join(receipt.reopened, ", "))
			if classification.outcome == "authentication" {
				return last, fmt.Errorf("%w; %v", iterationAuthenticationError(target), verdictErr)
			}
			return last, verdictErr
		}
		switch action, wait, resetAt := decideIteration(classification, time.Now(), &fails, &waits, &outputRetries); action {
		case actContinue:
			return last, nil
		case actWait:
			totalRetries++
			if rev.Rotates() {
				c.rotateOnLimit(rev, resetAt, &waits, wake)
			} else {
				sleepForLimit(wait, resetAt, wake)
			}
		case actRetryNow:
			totalRetries++
			if !ladder.SleepOrWake(wait, wake) {
				return interruptedReviewResult(last, totalRetries), errReviewInterrupted
			}
		case actRetry:
			totalRetries++
			if !ladder.SleepOrWake(10*time.Second, wake) {
				return interruptedReviewResult(last, totalRetries), errReviewInterrupted
			}
		case actStop:
			return last, fmt.Errorf("review stage failed %d times — stopping (a review that can't run is never an accept)", fails)
		case actAuthStop:
			// Same rotation as the work stage: without it a between-task audit would hard-stop the
			// run on the very credential the work stage just routed around.
			if rev.Rotates() && rev.OnAuthFailure() {
				totalRetries++
				ui.Warn("review target %q authentication failed — switching to %q (restore it with `%s`)",
					target, rev.Active(), loginCommand(target))
				break
			}
			return last, rotationAuthenticationError(rev, target)
		case actOutputStop:
			return last, fmt.Errorf("review stage reached the model output limit %d times — stopping", outputRetries)
		}
	}
}

const reviewVerdictCorrection = "\n\nREVIEW RECEIPT FORMAT CORRECTION: The previous review process succeeded, but Coop could not validate its structured verdict. Re-run the complete review over the same named subjects and return exactly one evidence line per subject followed by exactly one terminal `REVIEW COMPLETE` receipt, with nothing after that receipt."

type reviewAttemptObserver func(reviewRunResult, time.Time, string)

type reviewSubjectSnapshot struct {
	root        string
	dir         string
	id          string
	fingerprint tasks.CompletionFingerprint
}

func snapshotReviewSubjects(hosts, subjects []string) ([]reviewSubjectSnapshot, error) {
	snapshots := make([]reviewSubjectSnapshot, 0, len(subjects))
	for _, id := range subjects {
		subject, err := reviewSubject(hosts, id)
		if err != nil {
			return nil, err
		}
		fingerprint, err := tasks.CompletionFingerprintFor(subject.Root, subject.Item)
		if err != nil {
			return nil, fmt.Errorf("review subject %s fingerprint: %w", id, err)
		}
		snapshots = append(snapshots, reviewSubjectSnapshot{
			root: subject.Root, dir: subject.Item.Dir, id: id, fingerprint: fingerprint,
		})
	}
	return snapshots, nil
}

func validateReviewSubjects(hosts []string, snapshots []reviewSubjectSnapshot) error {
	for _, snapshot := range snapshots {
		subject, err := reviewSubject(hosts, snapshot.id)
		if err != nil {
			return err
		}
		if subject.Root != snapshot.root || subject.Item.Dir != snapshot.dir {
			return fmt.Errorf("review subject %s changed task queue", snapshot.id)
		}
		fingerprint, err := tasks.CompletionFingerprintFor(subject.Root, subject.Item)
		if err != nil {
			return fmt.Errorf("review subject %s fingerprint: %w", snapshot.id, err)
		}
		if fingerprint != snapshot.fingerprint {
			return fmt.Errorf("review subject %s changed completion generation", snapshot.id)
		}
	}
	return nil
}

// runReviewVerdict owns the complete review under its configured writes policy and the host-side
// verdict transaction. A successful process with malformed structured output gets one fresh full
// review over cloned inputs; every other failure keeps runReview/applyReviewVerdict's existing
// fail-closed behavior.
func (c *Control) runReviewVerdict(ctx context.Context, repo, img string, rev *ladder.Rotation, forkName, prompt, activity string, iterCmd iterationCmdBuilder, hosts, subjects []string, writes loopcfg.ReviewWrites, sink io.Writer, peers []agents.Target, wake <-chan struct{}, observe reviewAttemptObserver) (reviewRunResult, error) {
	hosts = slices.Clone(hosts)
	subjects = slices.Clone(subjects)
	subjectSnapshots, err := snapshotReviewSubjects(hosts, subjects)
	if err != nil {
		return reviewRunResult{target: rev.Active()}, fmt.Errorf("%w: snapshot review subjects: %v", tasks.ErrCompletionWindowSetup, err)
	}
	var concurrent []string
	var last reviewRunResult
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			if err := validateReviewSubjects(hosts, subjectSnapshots); err != nil {
				return last, fmt.Errorf("%w: review subjects changed before the corrected attempt: %v", tasks.ErrCompletionWindowAudit, err)
			}
		}
		attemptPrompt := prompt
		if attempt > 0 {
			attemptPrompt += reviewVerdictCorrection
		}
		start, headBefore := time.Now(), gitOut(repo, "rev-parse", "HEAD")
		run, err := c.runReview(ctx, repo, img, rev, forkName, attemptPrompt, activity, iterCmd, hosts, subjects, writes, sink, peers, wake, observe)
		run.output = normalizeReviewVerdictOutput(run.output)
		concurrent = slices.Compact(slices.Sorted(slices.Values(append(concurrent, run.concurrent...))))
		run.concurrent = slices.Clone(concurrent)
		if err == nil {
			if snapshotErr := validateReviewSubjects(hosts, subjectSnapshots); snapshotErr != nil {
				err = fmt.Errorf("%w: review subjects changed before verdict application: %v", tasks.ErrCompletionWindowAudit, snapshotErr)
			} else {
				run.reopened, err = applyReviewVerdictInRepo(repo, hosts, subjects, run.output)
			}
		}
		if observe != nil {
			observe(run, start, headBefore)
		}
		last = run
		if err == nil || !errors.Is(err, errReviewVerdictMalformed) || attempt > 0 {
			return run, err
		}
		if reviewStopRequested(ctx, wake) {
			return interruptedReviewResult(last, last.retries), errReviewInterrupted
		}
		// Carry the parse failure into the warning. The retry usually rescues this, and when it does
		// the error is discarded here and the run reports nothing — so a fault that costs a whole
		// extra review stays invisible for as long as it keeps being rescued. That is exactly how
		// this one hid: seen repeatedly, diagnosable never. The error carries a bounded output tail.
		ui.Warn("%s process succeeded but its structured verdict was malformed (%v) — re-running the full review once with a receipt-format correction", activity, err)
	}
	return last, nil
}

func reviewStopRequested(ctx context.Context, wake <-chan struct{}) bool {
	if ctx != nil && ctx.Err() != nil {
		return true
	}
	select {
	case <-wake:
		return true
	default:
		return false
	}
}
