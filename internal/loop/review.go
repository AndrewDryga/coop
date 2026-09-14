package loop

import (
	"context"
	"crypto/rand"
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

type reviewCmdBuilder func(agent, prompt, sessionID string, resume bool) (cmd []string, streaming, agentCommand bool)

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
	output    string
	usage     *iterResult
	outcome   string
	exit      int
	retries   int
	target    agents.Target
	reopened  []string
	sessionID string
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
	provider := cleanDiagnosticLine(agents.DisplayTarget(target.Provider))
	if account := target.Account(); account != "" {
		message := fmt.Sprintf("%s authentication failed for account %q — run `%s`",
			provider, account, loginCommand(target))
		return errors.New(wrappedLoopText(message, 6))
	}
	return errors.New(wrappedLoopText(fmt.Sprintf("%s authentication failed — run `%s`",
		provider, loginCommand(target)), 6))
}

// loginCommand renders the `coop login` invocation that restores one target's credential.
func loginCommand(t agents.Target) string {
	target := t.Provider
	if account := t.Account(); account != "" {
		target += "@" + account
	}
	return agents.LoginCommand(target)
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
		names = append(names, cleanDiagnosticLine(agents.DisplayTarget(t.String())))
	}
	message := fmt.Sprintf("authentication failed for every target (%s) — restore one with `%s`",
		strings.Join(names, ", "), loginCommand(failed[0]))
	return errors.New(wrappedLoopText(message, 6))
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
func (c *Control) runReview(ctx context.Context, repo, img string, rev *ladder.Rotation, forkName, prompt, activity string, reviewCmd reviewCmdBuilder, hosts, subjects []string, pendingReview *tasks.PendingReviewPlan, writes loopcfg.ReviewWrites, sink io.Writer, peers []agents.Target, wake <-chan struct{}, observeHandoff reviewAttemptObserver) (reviewRunResult, error) {
	var fails, waits, outputRetries, totalRetries, handoffs, timeouts int
	var concurrent []string
	last := reviewRunResult{target: rev.Active()}
	for {
		if reviewStopRequested(ctx, wake) {
			return interruptedReviewResult(last, totalRetries), errReviewInterrupted
		}
		agent := c.applyTarget(rev)
		target := rev.Active()
		sessionID := newReviewSessionID()
		cmd, streaming, agentCommand := reviewCmd(agent, prompt, sessionID, false) // build after rotation so argv matches this provider
		if len(cmd) == 0 {
			return last, fmt.Errorf("%s cannot start an exact review session", agents.DisplayTarget(target.String()))
		}
		c.net.setStage(fmt.Sprintf("Review attempt %d", totalRetries+1))
		start, headBefore := time.Now(), gitOut(repo, "rev-parse", "HEAD")
		repoReadOnly := reviewRepoReadOnly(writes)
		recovery := iterationRecovery{reuseServices: totalRetries > 0 && repoReadOnly}
		code, out, usage, classification, windows, runErr := c.runIterationWithMode(ctx, repo, img, agent, forkName, cmd, streaming, agentCommand, hosts, completionWindowReview, subjects, pendingReview, repoReadOnly, sink, peers, activity, "", nil, recovery)
		if usage != nil && usage.SessionID != "" {
			sessionID = usage.SessionID
		} else if adapter, ok := agents.Get(agent); !ok || !adapter.PresetSessionID() {
			sessionID = ""
		}
		last = reviewRunResult{output: out, usage: usage, outcome: classification.outcome, exit: code, retries: totalRetries, target: target, concurrent: concurrent, sessionID: sessionID}
		if errors.Is(runErr, tasks.ErrCompletionWindowSetup) {
			return last, runErr
		}
		observed, completionErr := windows.FinishReview()
		if len(observed) > 0 {
			ui.Note("Another session completed %s during this review. Reviewing it before finishing.", ui.Count(len(observed), "task"))
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
			handoffs++
			if handoffs >= maxBackgroundHandoffs {
				return last, fmt.Errorf("review provider ended with live background work %d times during recovery — stopped; rerun the review after its gate, consult, and delegate work finish in the foreground", handoffs)
			}
			if observeHandoff != nil {
				observeHandoff(last, start, headBefore)
			}
			totalRetries++
			ui.Alert("The reviewer exited while its background work was still running",
				fmt.Sprintf("Its result was discarded.\nBackground handoffs · %d of %d. Starting a fresh attempt.", handoffs, maxBackgroundHandoffs))
			continue
		}
		// A timed-out review attempt was killed for proven silence, so any receipt it printed
		// is not an observed verdict: discard it, rotate without cooling, and retry under the
		// dedicated timeout cap. Three timeout outcomes during one recovery episode stop the
		// stage even when live-background handoffs occur between them — a review that can't run
		// is never an accept.
		if isProviderTimeout(classification.outcome) {
			last.output = ""
			timeouts++
			if timeouts >= maxProviderTimeouts {
				return last, fmt.Errorf("review provider attempt timed out %d times during recovery (%s)%s — stopping (a review that can't run is never an accept)", timeouts, classification.outcome, classification.timeoutDetail())
			}
			if observeHandoff != nil {
				observeHandoff(last, start, headBefore)
			}
			totalRetries++
			rev.AdvanceOnTimeout(time.Now())
			ui.Alert("Stopped an unresponsive review attempt",
				fmt.Sprintf("%s.\nProvider timeouts · %d of %d. Its partial result was discarded. Starting a fresh attempt.", capitalize(silenceDetail(classification)), timeouts, maxProviderTimeouts))
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
			ui.Note("The model reached its response limit.")
			if wait > 0 {
				ui.Note("  Continuing in %s.", humanWait(wait))
			} else {
				ui.Note("  Continuing now.")
			}
			if !ladder.SleepOrWake(wait, wake) {
				return interruptedReviewResult(last, totalRetries), errReviewInterrupted
			}
		case actRetry:
			totalRetries++
			ui.Alert("Review attempt failed",
				fmt.Sprintf("Retrying in 10 seconds · attempt %d of %d.", fails+1, maxLoopFailures))
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
				ui.Alert(authHeadline(target),
					wrappedLoopText(fmt.Sprintf("Continuing the review with %s.", cleanDiagnosticLine(agents.DisplayTarget(rev.Active().String()))), 6),
					[2]string{"Sign in again:", loginCommand(target)})
				break
			}
			return last, rotationAuthenticationError(rev, target)
		case actOutputStop:
			return last, fmt.Errorf("review stage reached the model output limit %d times — stopping", outputRetries)
		}
	}
}

func newReviewSessionID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return ""
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[:4], raw[4:6], raw[6:8], raw[8:10], raw[10:])
}

func reviewVerdictCorrection(verdictErr error, output string, subjects []string) string {
	return "FORMAT CORRECTION ONLY. Your review is complete; keep its findings. Do not inspect files, invoke tools or services, rerun tests, or repeat analysis. Return only a corrected terminal envelope.\n" +
		"Validation error: " + truncate(cleanDiagnosticLine(verdictErr.Error()), 600) + "\n" +
		"Rejected terminal output:\n" + reviewEnvelopeTail(output) + "\n\n" + auditEvidencePrompt(subjects)
}

func reviewEnvelopeTail(output string) string {
	const maxLines, maxRunes = 16, 512
	lines := strings.Split(output, "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	for i := range lines {
		lines[i] = truncate(cleanDiagnosticLine(lines[i]), maxRunes)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

type reviewAttemptObserver func(reviewRunResult, time.Time, string)

type reviewSubjectSnapshot struct {
	root        string
	dir         string
	id          string
	fingerprint tasks.CompletionFingerprint
}

type reviewSourceSnapshot struct {
	head, trackedState string
}

func snapshotReviewSource(repo string) (reviewSourceSnapshot, error) {
	head, err := gitOutErr(repo, "rev-parse", "HEAD")
	if err != nil || head == "" {
		return reviewSourceSnapshot{}, errors.New("could not resolve review source HEAD")
	}
	state, err := gitOutErr(repo, "status", "--porcelain=v1", "--untracked-files=no")
	if err != nil {
		return reviewSourceSnapshot{}, fmt.Errorf("could not inspect review source: %w", err)
	}
	return reviewSourceSnapshot{head: head, trackedState: state}, nil
}

func validateReviewSource(repo string, snapshot reviewSourceSnapshot) error {
	return validateReviewSourceAfterConcurrent(repo, snapshot, false)
}

func validateReviewSourceAfterConcurrent(repo string, snapshot reviewSourceSnapshot, concurrent bool) error {
	current, err := snapshotReviewSource(repo)
	if err != nil {
		return err
	}
	// A completion observed by the host-owned review window may advance HEAD. The loop enrolls
	// that task into the next review round; uncommitted tracked changes are never accepted.
	if current.trackedState != snapshot.trackedState || (!concurrent && current.head != snapshot.head) {
		return errors.New("review source changed after the reviewer returned")
	}
	return nil
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

func addIterResults(first, second *iterResult) *iterResult {
	if first == nil {
		return second
	}
	if second == nil {
		return first
	}
	return &iterResult{
		CostUSD: first.CostUSD + second.CostUSD, Turns: first.Turns + second.Turns,
		DurationMS: first.DurationMS + second.DurationMS, InTok: first.InTok + second.InTok,
		OutTok: first.OutTok + second.OutTok, SessionID: second.SessionID,
		FreshInTok:         sumReportedInt(first.FreshInTok, second.FreshInTok),
		CacheWriteTok:      sumReportedInt(first.CacheWriteTok, second.CacheWriteTok),
		CacheReadTok:       sumReportedInt(first.CacheReadTok, second.CacheReadTok),
		ReportedOutTok:     sumReportedInt(first.ReportedOutTok, second.ReportedOutTok),
		ReportedDurationMS: sumReportedInt(first.ReportedDurationMS, second.ReportedDurationMS),
		ReportedCostUSD:    sumReportedFloat(first.ReportedCostUSD, second.ReportedCostUSD),
		BlindWaitSeconds:   max(first.BlindWaitSeconds, second.BlindWaitSeconds),
	}
}

func sumReportedInt(first, second *int) *int {
	if first == nil || second == nil {
		return nil
	}
	return intPtr(*first + *second)
}

func sumReportedFloat(first, second *float64) *float64 {
	if first == nil || second == nil {
		return nil
	}
	total := *first + *second
	return &total
}

// runReviewCorrection performs one exact-session continuation. It deliberately has no ladder,
// retry, or fresh-prompt path: a correction either returns one valid envelope or fails closed.
func (c *Control) runReviewCorrection(ctx context.Context, repo, img string, rev *ladder.Rotation, forkName, prompt, activity string, reviewCmd reviewCmdBuilder, previous reviewRunResult, hosts, subjects []string, pendingReview *tasks.PendingReviewPlan, writes loopcfg.ReviewWrites, sink io.Writer, peers []agents.Target) (reviewRunResult, error) {
	if previous.sessionID == "" || rev.Active().String() != previous.target.String() {
		return previous, errors.New("review format correction cannot resume the exact reviewer session")
	}
	agent := c.applyTarget(rev)
	cmd, streaming, agentCommand := reviewCmd(agent, prompt, previous.sessionID, true)
	if len(cmd) == 0 {
		return previous, errors.New("review format correction is unsupported for this reviewer session")
	}
	c.net.setStage("Review format correction")
	code, output, usage, classification, windows, runErr := c.runIterationWithMode(ctx, repo, img, agent, forkName, cmd, streaming, agentCommand, hosts, completionWindowReview, subjects, pendingReview, reviewRepoReadOnly(writes), sink, peers, activity+": format correction", "", nil, iterationRecovery{formatCorrection: true})
	result := reviewRunResult{
		output: normalizeReviewVerdictOutput(output), usage: addIterResults(previous.usage, usage),
		outcome: classification.outcome, exit: code, retries: previous.retries + 1,
		target: previous.target, sessionID: previous.sessionID, concurrent: slices.Clone(previous.concurrent),
	}
	if errors.Is(runErr, tasks.ErrCompletionWindowSetup) {
		return result, runErr
	}
	observed, completionErr := windows.FinishReview()
	result.concurrent = slices.Compact(slices.Sorted(slices.Values(append(result.concurrent, observed...))))
	if completionErr != nil {
		return result, fmt.Errorf("%w: review correction changed task completion ownership: %v", tasks.ErrCompletionWindowAudit, completionErr)
	}
	if ctx != nil && ctx.Err() != nil {
		return interruptedReviewResult(result, result.retries), errReviewInterrupted
	}
	if runErr != nil || code != 0 || classification.outcome != "success" {
		if runErr == nil {
			runErr = errors.New("provider did not complete successfully")
		}
		return result, fmt.Errorf("review format correction failed (%s, exit %d): %w", classification.outcome, code, runErr)
	}
	return result, nil
}

// runReviewVerdict owns the complete review under its configured writes policy and the host-side
// verdict transaction. A successful process with malformed structured output gets one format-only
// continuation in that exact native session; every other failure remains fail closed.
func (c *Control) runReviewVerdict(ctx context.Context, repo, img string, rev *ladder.Rotation, forkName, prompt, activity string, reviewCmd reviewCmdBuilder, hosts, subjects []string, pendingReview *tasks.PendingReviewPlan, writes loopcfg.ReviewWrites, sink io.Writer, peers []agents.Target, wake <-chan struct{}, observe reviewAttemptObserver) (reviewRunResult, error) {
	hosts = slices.Clone(hosts)
	subjects = slices.Clone(subjects)
	subjectSnapshots, err := snapshotReviewSubjects(hosts, subjects)
	if err != nil {
		return reviewRunResult{target: rev.Active()}, fmt.Errorf("%w: snapshot review subjects: %v", tasks.ErrCompletionWindowSetup, err)
	}
	sourceSnapshot, err := snapshotReviewSource(repo)
	if err != nil {
		return reviewRunResult{target: rev.Active()}, fmt.Errorf("%w: snapshot review source: %v", tasks.ErrCompletionWindowSetup, err)
	}
	start, headBefore := time.Now(), gitOut(repo, "rev-parse", "HEAD")
	run, runErr := c.runReview(ctx, repo, img, rev, forkName, prompt, activity, reviewCmd, hosts, subjects, pendingReview, writes, sink, peers, wake, observe)
	run.output = normalizeReviewVerdictOutput(run.output)
	if runErr == nil {
		if snapshotErr := validateReviewSourceAfterConcurrent(repo, sourceSnapshot, len(run.concurrent) > 0); snapshotErr != nil {
			runErr = fmt.Errorf("%w: %v", tasks.ErrCompletionWindowAudit, snapshotErr)
		} else if snapshotErr := validateReviewSubjects(hosts, subjectSnapshots); snapshotErr != nil {
			runErr = fmt.Errorf("%w: review subjects changed before verdict application: %v", tasks.ErrCompletionWindowAudit, snapshotErr)
		} else {
			run.reopened, runErr = applyReviewVerdictInRepo(repo, hosts, subjects, run.output)
		}
	}
	if runErr == nil || !errors.Is(runErr, errReviewVerdictMalformed) {
		if observe != nil {
			observe(run, start, headBefore)
		}
		return run, runErr
	}
	if reviewStopRequested(ctx, wake) {
		return interruptedReviewResult(run, run.retries), errReviewInterrupted
	}
	if snapshotErr := validateReviewSubjects(hosts, subjectSnapshots); snapshotErr != nil {
		return run, fmt.Errorf("%w: review subjects changed before format correction: %v", tasks.ErrCompletionWindowAudit, snapshotErr)
	}
	if snapshotErr := validateReviewSourceAfterConcurrent(repo, sourceSnapshot, len(run.concurrent) > 0); snapshotErr != nil {
		return run, fmt.Errorf("%w: %v", tasks.ErrCompletionWindowAudit, snapshotErr)
	}
	ui.Alert("The review result could not be read", fmt.Sprintf("%v\nRequesting one format-only correction in the same reviewer session.", runErr))
	correction := reviewVerdictCorrection(runErr, run.output, subjects)
	run, runErr = c.runReviewCorrection(ctx, repo, img, rev, forkName, correction, activity, reviewCmd, run, hosts, subjects, pendingReview, writes, sink, peers)
	if runErr == nil {
		if snapshotErr := validateReviewSourceAfterConcurrent(repo, sourceSnapshot, len(run.concurrent) > 0); snapshotErr != nil {
			runErr = fmt.Errorf("%w: %v", tasks.ErrCompletionWindowAudit, snapshotErr)
		} else if snapshotErr := validateReviewSubjects(hosts, subjectSnapshots); snapshotErr != nil {
			runErr = fmt.Errorf("%w: review subjects changed before corrected verdict application: %v", tasks.ErrCompletionWindowAudit, snapshotErr)
		} else {
			run.reopened, runErr = applyReviewVerdictInRepo(repo, hosts, subjects, run.output)
		}
	}
	if observe != nil {
		observe(run, start, headBefore)
	}
	return run, runErr
}

// runCandidateReviewVerdict runs the same provider/error/receipt boundary as final review without
// importing generic pending-review authority. Candidate review never moves task folders: findings
// leave the frozen manifest in reviewing, while only an exact all-subject pass returns success.
func (c *Control) runCandidateReviewVerdict(ctx context.Context, repo, img string, rev *ladder.Rotation, forkName, prompt, activity string, reviewCmd reviewCmdBuilder, hosts, subjects []string, sink io.Writer, peers []agents.Target, wake <-chan struct{}, observe reviewAttemptObserver) (reviewRunResult, error) {
	hosts = slices.Clone(hosts)
	subjects = slices.Clone(subjects)
	subjectSnapshots, err := snapshotReviewSubjects(hosts, subjects)
	if err != nil {
		return reviewRunResult{target: rev.Active()}, fmt.Errorf("%w: snapshot candidate review subjects: %v", tasks.ErrCompletionWindowSetup, err)
	}
	sourceSnapshot, err := snapshotReviewSource(repo)
	if err != nil {
		return reviewRunResult{target: rev.Active()}, fmt.Errorf("%w: snapshot candidate review source: %v", tasks.ErrCompletionWindowSetup, err)
	}
	start, headBefore := time.Now(), gitOut(repo, "rev-parse", "HEAD")
	run, runErr := c.runReview(ctx, repo, img, rev, forkName, prompt, activity, reviewCmd, hosts, subjects, nil, loopcfg.ReviewWritesTasks, sink, peers, wake, observe)
	run.output = normalizeReviewVerdictOutput(run.output)
	apply := func() error {
		if len(run.concurrent) > 0 {
			return fmt.Errorf("%w: candidate review observed concurrent task completion", tasks.ErrCompletionWindowAudit)
		}
		if snapshotErr := validateReviewSource(repo, sourceSnapshot); snapshotErr != nil {
			return fmt.Errorf("%w: %v", tasks.ErrCompletionWindowAudit, snapshotErr)
		}
		if snapshotErr := validateReviewSubjects(hosts, subjectSnapshots); snapshotErr != nil {
			return fmt.Errorf("%w: candidate review subjects changed before verdict acceptance: %v", tasks.ErrCompletionWindowAudit, snapshotErr)
		}
		var err error
		run.reopened, err = candidateReviewVerdict(subjects, run.output)
		if err == nil && len(run.reopened) > 0 {
			err = fmt.Errorf("candidate review found unresolved issues in %s", strings.Join(run.reopened, ", "))
		}
		return err
	}
	if runErr == nil {
		runErr = apply()
	}
	if runErr == nil || !errors.Is(runErr, errReviewVerdictMalformed) {
		if observe != nil {
			observe(run, start, headBefore)
		}
		return run, runErr
	}
	if reviewStopRequested(ctx, wake) {
		return interruptedReviewResult(run, run.retries), errReviewInterrupted
	}
	if snapshotErr := validateReviewSubjects(hosts, subjectSnapshots); snapshotErr != nil {
		return run, fmt.Errorf("%w: candidate review subjects changed before format correction: %v", tasks.ErrCompletionWindowAudit, snapshotErr)
	}
	if snapshotErr := validateReviewSource(repo, sourceSnapshot); snapshotErr != nil {
		return run, fmt.Errorf("%w: %v", tasks.ErrCompletionWindowAudit, snapshotErr)
	}
	ui.Alert("The candidate review result could not be read", fmt.Sprintf("%v\nRequesting one format-only correction in the same reviewer session.", runErr))
	correction := reviewVerdictCorrection(runErr, run.output, subjects)
	run, runErr = c.runReviewCorrection(ctx, repo, img, rev, forkName, correction, activity, reviewCmd, run, hosts, subjects, nil, loopcfg.ReviewWritesTasks, sink, peers)
	if runErr == nil {
		runErr = apply()
	}
	if observe != nil {
		observe(run, start, headBefore)
	}
	return run, runErr
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
