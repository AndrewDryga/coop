package loop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/ladder"
	"github.com/AndrewDryga/coop/internal/loopcfg"
	"github.com/AndrewDryga/coop/internal/taskmcp"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

// watchInterrupt gives SIGINT its two-stage stop. A termination signal is always hard, including
// when it arrives first, because TERM/HUP callers cannot be expected to signal twice.
func watchInterrupt(sig <-chan os.Signal, onSoft, onHard func()) {
	first, ok := <-sig
	if !ok {
		return
	}
	if first != os.Interrupt {
		onHard()
		return
	}
	onSoft()
	if _, ok := <-sig; !ok {
		return
	}
	onHard()
}

// loopInterruptInfo prints a stop notice. On the plain line-oriented path it starts on a fresh
// line, because an interactive terminal may echo Ctrl-C as literal ^C at the current cursor
// without advancing it — without the leading newline, coop's notice is glued to that echo (or to
// a partial agent line). While the loop's live bar is up, the region positions lines itself (and
// wipes the echo on its next repaint), and a raw newline would desync the region's cursor
// bookkeeping — so there the notice goes through ui alone.
func loopInterruptInfo(lines ...string) {
	if !ui.LiveActive() {
		fmt.Fprintln(os.Stderr)
	}
	for _, line := range lines {
		ui.Note("%s", line)
	}
}

type loopTaskLimit struct {
	max          int
	settled      int
	currentID    string
	currentTitle string
	lastID       string
	lastTitle    string
	lastState    string
}

func (l *loopTaskLimit) enabled() bool { return l.max > 0 }

func (l *loopTaskLimit) scope() string {
	if !l.enabled() {
		return ""
	}
	return l.currentID
}

func (l *loopTaskLimit) assign(id, title string) {
	if l.enabled() && l.currentID == "" {
		l.currentID, l.currentTitle = id, title
	}
}

// observe counts the selected task only after its post-iteration audit has left it done or blocked.
// A reopened task stays selected; reaching the limit retains the last task for the closing banner.
func (l *loopTaskLimit) observe(snapshot map[string]string) (bool, error) {
	if l.scope() == "" {
		return false, nil
	}
	state, ok := snapshot[l.currentID]
	if !ok {
		return false, fmt.Errorf("task-limited run lost task %s from the queue — inspect `coop tasks` before retrying", l.currentID)
	}
	if state != tasks.StateDone && state != tasks.StateBlocked {
		return false, nil
	}
	l.settled++
	l.lastID, l.lastTitle, l.lastState = l.currentID, l.currentTitle, state
	if l.settled >= l.max {
		return true, nil
	}
	l.currentID, l.currentTitle = "", ""
	return false, nil
}

// Run works spec's task queues unattended until nothing actionable remains (todo/ and
// in_progress/ both empty), then (unless a custom work.command is set) runs a signoff pass over the
// results; if the review reopens anything, the loop drains and reviews again until a review reopens
// nothing (accepted) or the round cap (config.MaxReviewRounds) is hit, which blocks the stuck task
// for a human. A model rate/usage limit is not a failure: the loop waits for the
// reset — parsed from the agent's own output when possible — and retries, so a long run
// survives the limit. A task left in in_progress/ by an interrupted iteration is continued (the
// work prompt points the next agent at its uncommitted partial work), not stranded; a
// run that completes no task for maxStalls iterations stops rather than spinning.
// spec.ForkName is non-empty only for a detached fork loop — it labels each iteration's box so
// `coop fork stop` can tear the container down by label (see box.RunSpec.ForkName); the local
// `coop loop` leaves it "".
// spec.Peers opts every iteration into the second-opinion directive: the box mounts the authed
// peers' credentials and the coop-consult wrapper, so an unattended lead can ask registered peers
// on hard calls — the orchestrator pattern running headless. Off by default: it widens the
// credential scope, so mounting peers into every loop box stays a deliberate choice.
func (c *Control) Run(spec RunSpec) (int, error) {
	c.preset, c.forkOwner, c.forkGeneration = spec.Preset, spec.ForkOwner, spec.ForkGeneration
	c.activityRepo, c.activityKind, c.activityTask, c.forkWorker = spec.ActivityRepo, spec.ActivityKind, spec.ActivityTask, spec.ForkWorker
	c.proposalOutbox = spec.ProposalOutbox
	repo, img, agent, forkName := spec.Repo, spec.Image, spec.Agent, spec.ForkName
	if c.activityRepo == "" {
		c.activityRepo = repo
	}
	if c.activityKind == "" {
		if forkName == "" {
			c.activityKind = forkspace.ExecutionLocalLoop
		} else {
			c.activityKind = forkspace.ExecutionForkLoop
		}
	}
	rot, queues, sink, peers := spec.Rotation, spec.Queues, spec.Sink, spec.Peers
	debugOnFail, preflight, maxTasks := spec.DebugOnFail, spec.Preflight, spec.MaxTasks
	hosts := make([]string, len(queues)) // the queues' absolute host paths
	scopes := make(map[string]string, len(queues))
	for i, q := range queues {
		hosts[i] = filepath.Join(repo, q)
		scopes[hosts[i]] = queueScope(q) // the subproject each task's id is shown beside
	}
	// The one command that continues this exact run, quoted wherever a report tells the reader
	// how to carry on. Supplied by the launch, so a fork loop names its own form.
	continueCmd := spec.Continue
	if continueCmd == "" {
		continueCmd = "coop loop"
	}
	// Stable per-run task numbering: a retry keeps the ordinal a reader already saw, and the
	// signoff's iteration counter never renumbers the work.
	ordinals := newTaskOrdinals()
	var completedLines []taskLine
	if c.proposalOutbox != "" {
		proposalRoot := c.proposalOutboxPath(repo)
		rel, relErr := filepath.Rel(repo, proposalRoot)
		info, statErr := os.Lstat(proposalRoot)
		if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) ||
			statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return 1, errors.Join(relErr, statErr, errors.New("fork task proposal outbox is outside the checkout or not a real directory"))
		}
	}
	queueExists := false
	for _, host := range hosts {
		if _, statErr := os.Lstat(host); errors.Is(statErr, os.ErrNotExist) {
			continue
		} else if statErr != nil {
			return 1, statErr
		}
		if _, readErr := tasks.ReadTaskTree(host); readErr != nil {
			return 1, readErr
		}
		queueExists = true
	}
	if !queueExists {
		return -1, fmt.Errorf("no task queue found (%s) — run 'coop init' or pass --tasks", strings.Join(queues, ", "))
	}
	// One loop per checkout, claimed before ANY queue state is touched — the reconcilers just
	// below already mutate it. Per-worktree, so parallel forks stay independent (see lockLoopCheckout).
	releaseCheckout, err := lockLoopCheckout(c.cfg, repo)
	if err != nil {
		return 1, err
	}
	defer releaseCheckout()
	// .agent/loop.yaml is the committed loop config (prompts, per-step models, settings). A bad file
	// fails the run here, before any box work. Absent → an empty config (all built-in defaults).
	// The snapshot pins this ONE read for the whole run — announced here so every log names the
	// exact config the run derives from, and checked for drift before each later box launch: a
	// mid-run edit warns "restart to apply" instead of silently never applying (or, worse,
	// hot-reloading half of a coherent ladders+prompts+caps+writes derivation).
	lc, cfgSnap, err := loopcfg.LoadSnapshot(repo)
	if err != nil {
		return 1, err
	}
	currentSignoff, currentVerify := lc.Signoff, lc.Verify
	currentSignoff.Agent = slices.Clone(currentSignoff.Agent)
	currentVerify.Agent = slices.Clone(currentVerify.Agent)
	currentMCPFile := c.cfg.MCPFile
	currentMCPDisabled := lc.MCPDisabled() || currentMCPFile == ""
	ui.Note("%s", loopConfigLine(cfgSnap.Configured()))
	// loop.yaml `mcp: false` runs EVERY stage's box without the shared MCP config — the schemas
	// ride at the front of each model request, so a drain that doesn't need those tools shouldn't
	// pay for them each iteration. Sitting here (not cmdLoop) it covers fork loops too. Blanking
	// MCPFile is the one switch the box snapshot boundary keys off; the loop owns this process, so
	// nothing else reads the config after it. Caveat: a verify: pass whose e2e
	// depends on MCP tooling needs mcp left on — repo-local e2e via bash is unaffected.
	var recoveredReviewCompletions []string
	if spec.CandidateReview == nil {
		if err := tasks.ReconcileInterruptedCompletions(hosts); err != nil {
			return 1, fmt.Errorf("recover interrupted completion: %w", err)
		}
		recoveredReviewCompletions, err = tasks.ReconcileCompletionWindowsWithActivity(hosts)
		if err != nil {
			return 1, fmt.Errorf("recover interrupted completion window: %w", err)
		}
	}
	pendingReview, err := tasks.LoadPendingReviews(repo, hosts)
	if err != nil {
		return 1, fmt.Errorf("recover pending final review: %w", err)
	}
	duplicates, err := tasks.NonArchivedDuplicateTaskIDs(hosts)
	if err != nil {
		return 1, err
	}
	if len(duplicates) > 0 {
		return 1, fmt.Errorf("aggregated loop cannot safely distinguish non-archived task id(s) present in multiple queues: %s — rename the duplicates or select one queue with --tasks", strings.Join(duplicates, ", "))
	}
	resumingCohort := len(pendingReview.Subjects) > 0
	if resumingCohort {
		applyStoredReviewPlan(lc, pendingReview.Plan)
	}
	activeMCPDisabled := currentMCPDisabled
	if resumingCohort {
		activeMCPDisabled = pendingReview.Plan.MCPDisabled
	}
	if activeMCPDisabled {
		c.cfg.MCPFile = ""
	} else {
		c.cfg.MCPFile = currentMCPFile
	}
	currentCustom := slices.Clone(lc.Work.Command)
	var custom []string
	if !resumingCohort {
		custom = slices.Clone(currentCustom)
	}
	if spec.CandidateReview != nil {
		if len(spec.ReviewTasks) > 0 || resumingCohort {
			return 1, errors.New("fork candidate review cannot share generic pending-review state")
		}
		if len(currentCustom) > 0 {
			return 1, errors.New("fork candidate review requires the built-in loop reviewer; remove work.command")
		}
		ids := spec.CandidateReview.TaskIDs
		if spec.CandidateReview.CandidateID == "" || spec.CandidateReview.Head == "" ||
			spec.CandidateReview.Tree == "" || spec.CandidateReview.Round == 0 || len(ids) == 0 ||
			!slices.IsSorted(ids) || slices.Contains(ids, "") {
			return 1, errors.New("invalid fork candidate review specification")
		}
		for i := 1; i < len(ids); i++ {
			if ids[i] == ids[i-1] {
				return 1, errors.New("fork candidate review has duplicate task subjects")
			}
		}
	}
	if len(spec.ReviewTasks) > 0 && len(currentCustom) > 0 {
		return 1, errors.New("--review-task requires the built-in loop review; remove work.command or omit the import")
	}
	limit := loopTaskLimit{max: maxTasks}
	// A task-limited run with no actionable work is a pure host-side no-op: it does not need an
	// image and must not launch a configured preflight agent. Its built-in preflight may first
	// unblock answered decisions, since that is host-only and can make work actionable.
	preflightBuiltinRan := false
	var builtinResult preflightResult
	if limit.enabled() && preflight && len(custom) == 0 && !resumingCohort {
		builtinResult, err = builtinPreflight(hosts)
		if err != nil {
			return 1, err
		}
		preflightBuiltinRan = true
	}
	if limit.enabled() {
		cf, _, err := tasks.QueueProgress(hosts)
		if err != nil {
			return 1, err
		}
		if cf.Todo+cf.Doing == 0 && len(spec.ReviewTasks) == 0 && len(pendingReview.Subjects) == 0 && spec.CandidateReview == nil {
			if preflightBuiltinRan {
				printPreflight(hosts, builtinResult)
			}
			c.closeWith(func() { printNoActionableTasks(cf) })
			return loopExitCode(cf), nil
		}
	}
	if !box.ImageExists(c.rt, img) {
		// Same rule as resolveImage: a dead daemon looks exactly like a missing image, and an
		// overnight drain that dies on "run 'coop build'" hides the real cause until morning.
		if err := c.rt.EnsureDaemon(); err != nil {
			return -1, err
		}
		return -1, fmt.Errorf("image %q not built — run 'coop build'", img)
	}
	// Staleness is computed before setup narration but printed only after the stop handler exists.
	// A missing image remains the hard failure above, never a prettier stale-image warning.
	nudges := box.StalenessNudges(c.cfg, repo, img)
	// Every built-in provider streams JSON that coop decodes into the same live lines — on a
	// TTY and on redirected runs alike, since the stream also feeds the provider watchdog. Only
	// a custom work.command keeps plain text output.
	// signoff.prompt APPENDS to the built-in senior review (it never replaces it).
	health := newLoopHealth() // per-task risk signals (reopens and gate edits) accumulated across the run
	audits := newAuditEvidenceStore()
	// The signoff pass (end-of-loop) and between-tasks audits both run only under the signoff-aware
	// agent form, not a custom work.command. Ordinary between review is opt-in; a completed task that
	// changed a protected gate path gets the narrow built-in audit even when it is off.
	currentBetweenEnabled := len(currentCustom) == 0 && lc.Between.Enabled
	betweenEnabled := currentBetweenEnabled && !resumingCohort
	// Per-stage signoff/between rotations from .agent/loop.yaml — each runs on its OWN configured
	// provider/model/effort/account and rotates its own fallback ladder on a limit (NOT a model name
	// pasted onto the work provider). An unset stage falls back: between → signoff → the work loop.
	signoffRot, err := c.reviewRotation(lc.Signoff.Agent, agent, rot)
	if err != nil {
		return 2, fmt.Errorf("signoff agent: %w", err)
	}
	betweenRot, err := c.reviewRotation(lc.Between.Agent, agent, signoffRot)
	if err != nil {
		return 2, fmt.Errorf("between agent: %w", err)
	}
	verifyEnabled := len(custom) == 0 && lc.Verify.Enabled
	verifyRot, err := c.reviewRotation(lc.Verify.Agent, agent, signoffRot) // unset → the signoff model
	if err != nil {
		return 2, fmt.Errorf("verify agent: %w", err)
	}
	currentSignoffRot, currentBetweenRot, currentVerifyRot := signoffRot, betweenRot, verifyRot
	if resumingCohort {
		currentSignoffRot, err = c.reviewRotation(currentSignoff.Agent, agent, rot)
		if err != nil {
			return 2, fmt.Errorf("current signoff agent: %w", err)
		}
		currentBetweenRot, err = c.reviewRotation(lc.Between.Agent, agent, currentSignoffRot)
		if err != nil {
			return 2, fmt.Errorf("current between agent: %w", err)
		}
		currentVerifyRot, err = c.reviewRotation(currentVerify.Agent, agent, currentSignoffRot)
		if err != nil {
			return 2, fmt.Errorf("current verify agent: %w", err)
		}
	}
	currentVerifyEnabled := len(currentCustom) == 0 && currentVerify.Enabled
	// Restricted networking is admitted ONCE, here, for the whole run: the
	// operator's --egress choice, the project's approved requests, and the core
	// endpoints of every rung this run may rotate onto are frozen into one policy,
	// and every box below launches under it (see runBox). Re-admitting per
	// iteration would let an approval or a config edit landing at 3am change what
	// an unattended run may reach — and a rotation would have to re-qualify
	// mid-drain. An open or offline run gets a nil capture and proceeds as today.
	c.net = newNetworkLog()
	defer c.net.summary()
	capture, err := box.AdmitNetwork(c.cfg, c.rt,
		networkAdmissionSpec(c.cfg, repo, img, agent, c.preset, peers, rot, signoffRot, betweenRot, verifyRot,
			currentSignoffRot, currentBetweenRot, currentVerifyRot), spec.Network)
	if err != nil {
		return 1, err
	}
	defer capture.Close()
	c.capture = capture
	// A per-run id keys this run's telemetry file (.agent/runs/<runid>.jsonl) — one JSON-Lines
	// record per stage, so the harness's own behavior (which target ran, reopen/retry counts) is
	// measurable. Best-effort throughout; a telemetry hiccup never touches the work.
	ridb := make([]byte, 8)
	_, _ = rand.Read(ridb)
	runid := hex.EncodeToString(ridb)
	c.runID = runid // boxes get it as COOP_RUN_ID so a consult peer can log its usage for the cost digest
	if len(peers) > 0 || c.preset != nil {
		peerPath, peerErr := preparePeerRecordFile(repo, runid)
		if peerErr != nil {
			ui.Warn("telemetry: could not prepare peer usage for this run: %v", peerErr)
		} else {
			defer removeEmptyPeerRecordFile(peerPath)
		}
	}
	c.streamSeq, c.streamOff = 0, false
	// iterCmd builds one iteration's command: a raw work.command override if set,
	// otherwise the chosen agent's headless form carrying the work/signoff prompt. It runs
	// exactly once per box launch — work, pre-flight, and every review attempt — so it is also
	// the stage-launch boundary where loop.yaml drift is announced (once per new digest); the
	// run itself stays on its startup snapshot.
	iterCmd := func(iterAgent, prompt string) ([]string, bool, bool) {
		if cause, drifted := cfgSnap.Drift(); drifted {
			ui.Alert("Loop settings changed", cause)
		}
		var cmd []string
		if len(custom) == 0 {
			cmd = c.agentLoopCmd(iterAgent, prompt)
		}
		command, streaming := IterationCommand(iterAgent, cmd, custom)
		return command, streaming, len(custom) == 0
	}
	// Soft interrupt for any foreground loop that owns a terminal — a plain `coop loop` OR a
	// foreground `coop fork <name> --loop`: the first Ctrl-C finishes the current iteration then
	// stops before the next; a second stops now (tears the box down). TERM and HUP are always hard.
	// A redirected loop — a CI pipe, or a DETACHED fork worker stopped by `coop fork stop`
	// (SIGTERM) — needs its own watcher now: every built-in attempt runs the box in its own
	// cancelable process group for the provider watchdog, so a delivered signal no longer takes
	// the box down with coop. One SIGINT/SIGTERM cancels the box context — the run tears down
	// cleanly instead of exiting and orphaning it.
	var softStop atomic.Bool
	wake := make(chan struct{}) // closed on the first stop signal so any in-progress wait returns at once
	var wakeOnce sync.Once
	requestStop := func() {
		softStop.Store(true)
		wakeOnce.Do(func() { close(wake) })
	}
	interactive := ui.IsTerminal(os.Stdin)
	var iterCtx context.Context
	{
		ctx, cancel := context.WithCancel(context.Background())
		iterCtx = ctx
		defer cancel()
		sig := make(chan os.Signal, 2)
		if interactive {
			signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
			go watchInterrupt(sig,
				func() {
					requestStop()
					loopInterruptInfo("Finishing this attempt and its review, then stopping.", "Press Ctrl-C again to stop immediately.")
				},
				func() {
					loopInterruptInfo("Stopping immediately.")
					requestStop()
					cancel()
				})
		} else {
			signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
			go func() {
				if _, ok := <-sig; !ok {
					return
				}
				loopInterruptInfo("Stop requested — tearing down this attempt's box, then exiting.")
				requestStop()
				cancel()
			}()
		}
		defer func() { signal.Stop(sig); close(sig) }()
	}
	if interactive && !limit.enabled() {
		ui.Note("")
		ui.Note("Ctrl-C finishes this attempt and its review, then stops. Press again to stop immediately.")
	}
	// A previous run of THIS checkout may have been killed with its box still up. The host returns
	// only verified cleanup counts so the loop can narrate them as one preparation section.
	preparation := c.host.sweepOrphanBoxes(repo)
	stopAwake, awake := armKeepAwake(c.cfg)
	defer stopAwake()
	printLoopPreparation(preparation, nudges, awake)
	if spec.CandidateReview != nil {
		review := *spec.CandidateReview
		ui.Note("")
		if review.PreviousRound > 0 {
			ui.Note("Review round %d supersedes round %d.", review.Round, review.PreviousRound)
		} else {
			ui.Note("Review round %d covers the complete fork candidate.", review.Round)
		}
		ui.Note("Reviewer · %s · %s", cleanDiagnosticLine(agents.DisplayTarget(signoffRot.Active().String())), ui.Count(len(review.TaskIDs), "task"))
		prompt := forkCandidateReviewPrompt(repo, queues, review, lc.Signoff.Prompt)
		observe := func(run reviewRunResult, start time.Time, headBefore string) {
			c.recordStage(repo, runid, "candidate-review", run.outcome, run.target, start, run.exit, run.retries, len(run.reopened), headBefore, hosts, nil, nil, run.usage)
		}
		run, reviewErr := c.runCandidateReviewVerdict(iterCtx, repo, img, signoffRot, forkName, prompt,
			reviewActivity("candidate review", review.TaskIDs), iterCmd, hosts, review.TaskIDs, sink, peers, wake, observe)
		if errors.Is(reviewErr, errReviewInterrupted) {
			ui.Failure("Candidate review interrupted", "No review authority was recorded.", [2]string{"Continue:", continueCmd})
			return LoopInterruptedExitCode, ui.Reported(reviewErr)
		}
		if reviewErr != nil {
			ui.Failure("Candidate review did not pass", reviewErr.Error(), [2]string{"Continue:", continueCmd})
			return 1, ui.Reported(reviewErr)
		}
		if len(run.reopened) != 0 {
			return 1, errors.New("candidate review returned unresolved findings without an error")
		}
		ui.OK("Candidate review round %d passed", review.Round)
		return 0, nil
	}

	// Pre-flight: one best-effort housekeeping pass before working the queue. The built-in job —
	// return every blocked task whose decision.md now has a filled-in Resolution to todo — is
	// mechanical, so the HOST does it directly: no box, no model, no tokens, and the same bar as
	// `coop tasks unblock` (decisionResolved), so preflight and the CLI never disagree. It works
	// no task and deletes nothing: done tasks are pruned only by a human (`coop tasks rm
	// --all-done`), never by an agent. Opt-in (preflight.enabled / --preflight); skipped under a
	// custom work.command (not the agent's headless form).
	preflightRan := false
	runPreflight := func() error {
		if preflightRan || !preflight || len(custom) > 0 {
			return nil
		}
		preflightRan = true
		if !preflightBuiltinRan {
			builtinResult, err = builtinPreflight(hosts)
			if err != nil {
				return err
			}
			preflightBuiltinRan = true
		}
		printPreflight(hosts, builtinResult)
		// An agent runs only for a CUSTOM cleanup (loop.yaml preflight.prompt) — extra instructions
		// that need judgment. Best-effort like the signoff pass — a failure never blocks work.
		if s := strings.TrimSpace(lc.Preflight.Prompt); s != "" {
			// The pre-flight box runs the rung the first iteration will take, chosen BEFORE its argv
			// is built: applyTarget points cfg at that rung's account/model/effort and returns its
			// provider, so building the command first would mount one provider's credential while
			// carrying another's command line.
			agent = c.applyTarget(rot)
			pfStart, pfHead := time.Now(), gitOut(repo, "rev-parse", "HEAD")
			pfCmd, streaming, agentCommand := iterCmd(agent, loopPreflightPrompt(repo, queues, s))
			pfCode, _, _, pfClassification, windows, runErr := c.runIteration(iterCtx, repo, img, agent, forkName, pfCmd, streaming, agentCommand, hosts, completionWindowReview, nil, nil, false, sink, peers, "preflight", "", nil)
			if errors.Is(runErr, tasks.ErrCompletionWindowSetup) {
				return runErr
			}
			if _, err := windows.FinishReview(); err != nil {
				return fmt.Errorf("pre-flight changed task completion ownership: %w", err)
			}
			c.recordStage(repo, runid, "preflight", pfClassification.outcome, rot.Active(), pfStart, pfCode, 0, 0, pfHead, hosts, nil, nil, nil)
			prev := rot.Active()
			if wait, until, limited := rememberPreflightLimit(rot, pfClassification, time.Now()); limited {
				if wait > 0 {
					ui.Note("all %d targets are rate limited after pre-flight — waiting for the soonest reset", rot.Len())
					sleepForLimit(wait, until, wake)
					rot.ClearExpired(time.Now())
				} else {
					printLoopCaution(fmt.Sprintf("%s reached its usage limit during pre-flight, continuing with %s",
						cleanDiagnosticLine(agents.DisplayTarget(prev.String())),
						cleanDiagnosticLine(agents.DisplayTarget(rot.Active().String()))))
				}
			}
		}
		return nil
	}
	if !resumingCohort {
		if err := runPreflight(); err != nil {
			return 1, err
		}
	}
	c0, _, err := tasks.QueueProgress(hosts)
	if err != nil {
		return 1, err
	}
	// An in_progress task whose commit is already in history means a previous run died between the
	// commit and the folder move. Say so before working it: the resume recipe only stays safe while
	// that commit is HEAD, and left unnoticed these sat in the queue for days.
	committed, err := tasks.AlreadyCommittedInProgress(repo, hosts)
	if err != nil {
		return 1, err
	}
	for _, t := range committed {
		ui.Alert("This in-progress task already has a commit",
			alreadyCommittedCause(taskTitle(hosts, t.ID), t.Commit),
			[2]string{"Task:", "coop tasks path " + t.ID})
	}
	fails, waits, retries, handoffs, timeouts, stalls := 0, 0, 0, 0, 0, 0
	completionRepairs := map[string]int{}
	completedThisRun := map[string]bool{}
	settledBaseline := c0.Done + c0.Blocked // "settled" = tasks out of the actionable set (done OR blocked)
	// A commit between iterations is progress too (see below), and every completion is validated
	// against a commit range — so an unreadable HEAD isn't a head value the loop can carry, it's a
	// repo the loop cannot bookkeep against. Stop before the first box starts.
	prevHead, headErr := gitOutErr(repo, "rev-parse", "HEAD")
	if headErr != nil {
		ui.Failure("Could not start the loop",
			"This repository has no readable Git commit yet.\nCommit the initial project files, then start the loop again.")
		return 1, ui.Reported(fmt.Errorf("read HEAD of %s: %w", repo, headErr))
	}
	loopStartHead := prevHead // for the end-of-run signing sweep (catches any straggler cycle)
	var reviewPlan tasks.PendingReviewPlan
	if len(pendingReview.Subjects) > 0 {
		reviewPlan = pendingReview.Plan
		loopStartHead = reviewPlan.BaseHead
	} else {
		if len(spec.ReviewTasks) > 0 {
			loopStartHead, err = tasks.PendingReviewImportBase(repo, hosts, spec.ReviewTasks)
			if err != nil {
				return 1, fmt.Errorf("anchor archived task review context: %w", err)
			}
		}
		reviewPlan, err = pendingReviewPlanForRun(repo, hosts, loopStartHead, cfgSnap.Digest(), continueCmd, lc, signoffRot, verifyRot, currentMCPDisabled)
		if err != nil {
			return 1, fmt.Errorf("prepare durable final review: %w", err)
		}
	}
	if len(spec.ReviewTasks) > 0 {
		if len(pendingReview.Subjects) > 0 {
			additional := pendingReviewAdditionalImports(pendingReview, spec.ReviewTasks)
			if len(additional) > 0 {
				return 1, fmt.Errorf("pending final review already has fixed change context; resume it without importing %s, then import that archived work in a later loop", strings.Join(additional, ", "))
			}
		}
		if err := tasks.EnrollExistingPendingReviews(repo, hosts, spec.ReviewTasks, reviewPlan); err != nil {
			return 1, fmt.Errorf("import archived task for final review: %w", err)
		}
		pendingReview, err = tasks.LoadPendingReviews(repo, hosts)
		if err != nil {
			return 1, fmt.Errorf("reload imported final review: %w", err)
		}
		reviewPlan = pendingReview.Plan
		ui.Note("Imported %s into pending final review.", ui.Count(len(slices.Compact(slices.Sorted(slices.Values(spec.ReviewTasks)))), "archived task"))
	}
	if len(recoveredReviewCompletions) > 0 {
		if err := tasks.EnrollExistingPendingReviews(repo, hosts, recoveredReviewCompletions, reviewPlan); err != nil {
			return 1, fmt.Errorf("preserve recovered final-review subjects: %w", err)
		}
		pendingReview, err = tasks.LoadPendingReviews(repo, hosts)
		if err != nil {
			return 1, fmt.Errorf("reload recovered final review: %w", err)
		}
		reviewPlan = pendingReview.Plan
	}
	for _, id := range pendingReviewIDs(pendingReview) {
		completedThisRun[id] = true
	}
	resumingPendingReview := len(pendingReview.Subjects) > 0
	pendingReopened := pendingReviewIDs(pendingReview, tasks.PendingReviewReopened)
	announcePendingFinalReview(
		pendingReviewIDs(pendingReview, tasks.PendingReviewSignoff, tasks.PendingReviewVerify),
		reviewPlan.Continue,
	)
	// The signoff reviews only what THIS RUN completed: anchoring to the pre-run done set keeps
	// 99_done/'s history (pruned only by a human) out of every round's subject list.
	doneBaseline, err := doneTaskDirs(hosts)
	if err != nil {
		return 1, err
	}
	reviewBaseline := reviewBaselineAfterVerdict(doneBaseline, nil, nil, recoveredReviewCompletions)
	for _, id := range pendingReviewIDs(pendingReview, tasks.PendingReviewSignoff) {
		delete(reviewBaseline, id)
	}
	if len(recoveredReviewCompletions) > 0 {
		ui.Note("Another session completed %s during an interrupted review. Reviewing it before finishing.", ui.Count(len(recoveredReviewCompletions), "task"))
	}
	// Loop-until-accepted: drain the work queue, run the signoff pass, and if it reopened
	// anything, drain and sign off AGAIN — repeating until a signoff reopens nothing (accepted) or
	// the round cap is hit (block the stuck task for a human). The cap scales with the batch —
	// clamp(tasks worked/2, 3, signoff.rounds) — so a big overnight batch can't ping-pong one
	// stuck task forever while a tiny batch still gets a few tries (computed per round from the run's
	// completed count; the hard ceiling bounds it). A custom work.command has no signoff pass.
	// Final verify may jump back here when a parallel host completion needs its own signoff.
	signoffRound, maxReviewRounds := pendingSignoffStartRound(pendingReview), 0
	reviewCapped := false
	verificationFailed := false
reviewAgain:
	for ; ; signoffRound++ {
		for {
			// Cross-run debt is settled before unrelated queue work. Reopened subjects are the only
			// work eligible here; an already-completed cohort goes straight to its stored reviewer.
			if resumingPendingReview && len(pendingReopened) == 0 {
				break
			}
			// A first Ctrl-C (soft stop) that arrived between iterations — or that woke a wait
			// below — stops here, before the next task is claimed; a second (hard) Ctrl-C that
			// canceled iterCtx during a between-tasks audit stops here too, before respawning a box.
			if softStop.Load() || iterCtx.Err() != nil {
				break
			}
			snapshot, snapshotErr := tasks.QueueSnapshot(hosts)
			if snapshotErr != nil {
				return 1, snapshotErr
			}
			if !resumingPendingReview {
				reached, limitErr := limit.observe(snapshot)
				if limitErr != nil {
					return 1, limitErr
				}
				if reached {
					break
				}
			}
			// Point cfg at this iteration's target before leasing: the provider/target in metadata
			// identifies the owning controller, while flock remains the actual authority.
			agent = c.applyTarget(rot)
			target := rot.Active()
			// Select and host-claim one authoritative task before the box starts. The returned task
			// drives both the banner and prompt, so the model cannot guess a different "next" task.
			onlyID := limit.scope()
			if onlyID == "" && resumingPendingReview && len(pendingReopened) > 0 {
				onlyID = pendingReopened[0]
			}
			assignment, assignErr := tasks.AssignLoopTaskOnly(hosts, tasks.TaskLeaseOwner{
				RunID: c.runID, PID: os.Getpid(), Provider: agent, Target: target.String(),
			}, onlyID)
			if assignErr != nil {
				return 1, assignErr
			}
			if assignment.Outcome == tasks.AssignmentUnavailable {
				// Foreign-held work is not a drained queue. Do not sign off a batch another live
				// controller still owns; its kernel lock will make the task adoptable on death.
				printBusyQueue(assignment.Busy)
				return 0, nil
			}
			if assignment.Outcome == tasks.AssignmentDrained {
				if limit.scope() != "" {
					continue // the selected task settled between scans; observe and count its final state
				}
				break
			}
			assigned, lease := assignment.Task, assignment.Lease
			if !resumingPendingReview {
				limit.assign(assigned.Item.ID, assigned.Item.Title)
			}
			// The active profile is shown on the model line (streamjson) — don't repeat it on the header.
			active := cleanDiagnosticLine(assigned.Item.Title)
			current := taskLine{id: assigned.Item.ID, title: assigned.Item.Title, scope: scopes[assigned.Root]}
			runner := target.String()
			if len(custom) > 0 {
				runner = strings.Join(custom, " ")
			}
			attempt := ordinals.attempt(assigned.Item.ID)
			printTaskHeader(ordinals.of(assigned.Item.ID), attempt, current, runner, len(custom) > 0, assignment.Counts)
			// Informed resume: a lease carrying host audit-reopen authority gets the audit-rework
			// preamble (verify the finding; zero-commit re-close or a real tree change — never a
			// Coop-Recovery receipt); otherwise a landed Coop-Task commit (a crash after commit before
			// the folder-move) gets the crash/reopen disambiguation line. Empty prefix → prompt unchanged.
			iterHead := gitOut(repo, "rev-parse", "HEAD")
			if authorityErr := tasks.ValidateLeasedAuditReopen(repo, iterHead, assigned.Item.ID, lease.Reopen); authorityErr != nil {
				baseline := lease.Reopen.BaselineHead
				parkErr := tasks.ParkStaleAuditReopen(assigned, baseline)
				releaseErr := lease.Release()
				if parkErr != nil {
					return 1, errors.Join(
						authorityErr,
						fmt.Errorf("could not park stale audit task %s: %w", assigned.Item.ID, parkErr),
						releaseErr,
					)
				}
				return 1, errors.Join(
					fmt.Errorf("%w; %s", authorityErr, tasks.StaleAuditReopenRecovery(assigned.Item.ID, baseline)),
					releaseErr,
				)
			}
			work := LoopWorkPrompt(repo, assigned.Root, assigned.Item.ID, agent, peers, c.preset, lease.Reopen != nil)
			iterWork := work
			if pre := tasks.ResumePrefixFor(repo, assigned.Item.ID, assigned.Item.State, lease.Reopen); pre != "" {
				iterWork = pre + "\n\n" + work
			}
			// The box's task tools: every queue this loop works, the leased task, and — in a fork —
			// the proposal outbox the host imports at merge. Built here, where the lease lives; the
			// box only carries the server.
			// A successful binding precheck is not completion: a later checklist
			// refusal or failed move must not advance the next attempt's base.
			var completionAttempted atomic.Bool
			taskTools, toolsErr := taskmcp.New(taskmcp.Authority{
				QueueRoots: hosts, Assigned: assigned.Item.ID, ProposalOutbox: c.proposalOutboxPath(repo),
				Owner: tasks.TaskLeaseOwner{RunID: c.runID, PID: os.Getpid(), Provider: agent, Target: target.String()},
				ValidateAssignedCompletion: func() error {
					completionAttempted.Store(true)
					return checkAssignedCompletion(repo, iterHead, assigned.Item.ID, lease.Reopen, snapshot)
				},
			})
			if toolsErr != nil {
				return 1, errors.Join(toolsErr, lease.Release())
			}
			iterStart := time.Now()
			c.net.setStage(fmt.Sprintf("Task attempt %d", attempt))
			cmd, streaming, agentCommand := iterCmd(agent, iterWork)
			code, _, res, classification, windows, runErr := c.runIteration(iterCtx, repo, img, agent, forkName, cmd, streaming, agentCommand, hosts, completionWindowWork, []string{assigned.Item.ID}, nil, false, sink, peers, active, assigned.Item.ID, taskTools)
			if errors.Is(runErr, tasks.ErrCompletionWindowSetup) {
				return 1, errors.Join(runErr, lease.Release())
			}
			// Stop metadata writes but keep the flock while validating and finalizing this exact task.
			lease.Quiesce()
			// Completion integrity is a hard boundary. Fresh work must bind inside this iteration's
			// commit range. Crash recovery restores work for a new range-bound attempt, never trusting
			// provider-writable metadata or reachable history. The flock stays held through validation,
			// recovery notes, and accepted-state cleanup so no second controller sees a half-transition.
			completedTasks, unowned, completionScanErr := windows.AuditDoneCandidates(assigned)
			if completionScanErr != nil {
				return 1, errors.Join(
					fmt.Errorf("scan task completions: %w", completionScanErr),
					lease.Release(), windows.Abandon(),
				)
			}
			var finished []string
			var assignedCompletion *tasks.QueuedTask
			for i := range completedTasks {
				if completedTasks[i].Root == assigned.Root && completedTasks[i].Item.ID == assigned.Item.ID {
					assignedCompletion = &completedTasks[i]
					finished = []string{completedTasks[i].Item.ID}
					break
				}
			}
			// coop-entry returns this only after a successful provider left live agent-owned
			// descendants and it drained or forcibly terminated them. Any completion is premature:
			// restore it before the normal binding/finalization path and launch a fresh provider that
			// can inspect the outcome. A small dedicated cap prevents a quiet respawn loop.
			if isBackgroundHandoff(classification.outcome) {
				if assignedCompletion != nil {
					if restoreErr := tasks.RestoreBackgroundHandoffCompletion(*assignedCompletion); restoreErr != nil {
						return 1, errors.Join(restoreErr, lease.Release(), windows.Abandon())
					}
				}
				if releaseErr := errors.Join(lease.Release(), windows.Close()); releaseErr != nil {
					return 1, fmt.Errorf("release task lease %s after background handoff: %w", assigned.Item.ID, releaseErr)
				}
				handoffs++
				c.recordStage(repo, runid, "work", classification.outcome, rot.Active(), iterStart, code, retries, 0, iterHead, hosts, nil, nil, res)
				if handoffs >= maxBackgroundHandoffs {
					ui.Failure(fmt.Sprintf("Stopped after %d live-background handoffs", handoffs),
						fmt.Sprintf("%s was returned to in progress.\nRun its checks and any peer work in the foreground before retrying.", active))
					return code, ui.Reported(fmt.Errorf("provider ended with live background work %d times during recovery for task %s", handoffs, assigned.Item.ID))
				}
				ui.Alert("The agent exited while its background work was still running",
					fmt.Sprintf("The task was returned to in progress.\nBackground handoffs · %d of %d. Starting a fresh attempt.", handoffs, maxBackgroundHandoffs))
				continue
			}
			// The watchdog killed this attempt for proven silence. Any completion it produced is
			// premature: restore it, keep held audit authority truthful (rebase over a valid
			// complete rewrite, park fail-closed otherwise), release the lease, and retry under
			// the dedicated timeout policy — rotate to the next usable rung without cooling,
			// capped at three timeout outcomes during one recovery episode, no ordinary counter
			// consumed. A live-background handoff does not erase this independent timeout budget.
			if isProviderTimeout(classification.outcome) {
				if assignedCompletion != nil {
					if restoreErr := tasks.RestoreProviderTimeoutCompletion(*assignedCompletion, lease.Reopen != nil); restoreErr != nil {
						return 1, errors.Join(restoreErr, lease.Release(), windows.Abandon())
					}
				}
				if authorityErr := lease.RebaseTimedOutAuditReopen(repo, iterHead, gitOut(repo, "rev-parse", "HEAD")); authorityErr != nil {
					baseline := lease.Reopen.BaselineHead
					parkErr := tasks.ParkStaleAuditReopen(assigned, baseline)
					releaseErr := errors.Join(lease.Release(), windows.Abandon())
					if parkErr != nil {
						return 1, errors.Join(authorityErr, fmt.Errorf("could not park stale audit task %s: %w", assigned.Item.ID, parkErr), releaseErr)
					}
					return 1, errors.Join(
						fmt.Errorf("task %s audit authority no longer matches the tree its timed-out attempt left: %w; %s", assigned.Item.ID, authorityErr, tasks.StaleAuditReopenRecovery(assigned.Item.ID, baseline)),
						releaseErr,
					)
				}
				departed, departureErr := windows.Departures()
				if len(departed) > 0 {
					departureErr = errors.Join(departureErr, fmt.Errorf(
						"work stage reopened unowned archived task(s) %s",
						strings.Join(departed, ", "),
					))
				}
				var unownedErr error
				if len(unowned) > 0 {
					unownedErr = tasks.UnownedCompletionError(unowned, nil)
				}
				if auditErr := errors.Join(unownedErr, departureErr); auditErr != nil {
					return 1, errors.Join(auditErr, lease.Release(), windows.Abandon())
				}
				if releaseErr := errors.Join(lease.Release(), windows.Close()); releaseErr != nil {
					return 1, fmt.Errorf("release task lease %s after provider timeout: %w", assigned.Item.ID, releaseErr)
				}
				timeouts++
				c.recordStage(repo, runid, "work", classification.outcome, rot.Active(), iterStart, code, retries, 0, iterHead, hosts, nil, nil, res)
				if timeouts >= maxProviderTimeouts {
					ui.Failure(fmt.Sprintf("Stopped after %d provider timeouts", timeouts),
						fmt.Sprintf("%s is still in progress.\nThe last attempt recorded %s.", active, silenceDetail(classification)),
						[2]string{"Continue:", continueCmd})
					return code, ui.Reported(fmt.Errorf("provider attempt timed out %d times during recovery for task %s (%s)", timeouts, assigned.Item.ID, classification.outcome))
				}
				prev := rot.Active()
				rot.AdvanceOnTimeout(time.Now())
				next := rot.Active()
				resume := "Starting a fresh attempt"
				if next.String() != prev.String() {
					resume = "Starting a fresh attempt with " + cleanDiagnosticLine(agents.DisplayTarget(next.String()))
				}
				ui.Alert("Stopped an unresponsive task attempt",
					wrappedLoopText(fmt.Sprintf("%s. Provider timeouts · %d of %d. %s.", capitalize(silenceDetail(classification)), timeouts, maxProviderTimeouts, resume), 6))
				continue
			}
			handoffs, timeouts = 0, 0
			headAfter := gitOut(repo, "rev-parse", "HEAD")
			// Ref authority: from here through consumeAuditReopen/windows.Close(), this worktree's
			// HEAD is exclusive to this controller. Everything below assumes HEAD == headAfter; an
			// interactive coop run, a host signing rewrite, a fork land, or a human commit could move
			// it during the several filesystem operations between this line and consumeAuditReopen,
			// so the window closes that gap instead of trusting the value across it. The first action
			// inside the lock re-reads HEAD and compares — see tasks.EnterRefAuthorityWindow.
			refRelease, liveHead, refErr := tasks.EnterRefAuthorityWindow(c.cfg, repo, headAfter, nil)
			if refErr != nil {
				reason := refErr.Error()
				if errors.Is(refErr, tasks.ErrRefAuthorityMoved) {
					reason = fmt.Sprintf("HEAD moved from the validated %s to %s before task authority could be consumed — another process changed this checkout during completion", headAfter, liveHead)
				}
				var restoreErr error
				if assignedCompletion != nil {
					restoreErr = tasks.RestoreRefAuthorityFailure(*assignedCompletion, reason)
				}
				releaseErr := errors.Join(lease.Release(), windows.Abandon())
				return 1, errors.Join(tasks.RefAuthorityFailureError(assigned.Item.ID, reason, restoreErr), releaseErr)
			}
			// departures runs before the binding check so its ids are already known: the touched set
			// below needs them, and this restore/reject sequence stays in the exact order it ran in
			// before (departure churn still wins over a binding rejection).
			departed, departureErr := windows.Departures()
			var restoreErr error
			if departureErr != nil {
				if assignedCompletion != nil {
					restoreErr = tasks.RestoreCompromisedCompletion(*assignedCompletion, lease.Reopen != nil)
				}
				releaseErr := errors.Join(lease.Release(), windows.Abandon())
				refRelease()
				return 1, errors.Join(departureErr, restoreErr, releaseErr)
			}
			if len(departed) > 0 {
				if assignedCompletion != nil {
					restoreErr = tasks.RestoreCompromisedCompletion(*assignedCompletion, lease.Reopen != nil)
				}
				var windowErr error
				if restoreErr != nil {
					windowErr = windows.Abandon()
				} else {
					windowErr = windows.Close()
				}
				releaseErr := errors.Join(lease.Release(), windowErr)
				refRelease()
				departureErr = fmt.Errorf("work stage reopened unowned archived task(s) %s", strings.Join(departed, ", "))
				return 1, errors.Join(departureErr, restoreErr, releaseErr)
			}
			// The touched set is host-side knowledge the box cannot influence — everything this
			// iteration's authority consumption could affect: the finished set, the leased task id,
			// the audit-reopen record's task, every id whose queue state this completion window
			// observed change (auditDoneCandidates' full candidate list, plus any departure), and
			// every id already archived when the window's baseline was captured — before the box ever
			// ran, so an already-closed task stays protected even when its folder never moves; an
			// archived task's history is meant to be closed, and a forged extra commit corrupts that
			// closed record without needing to touch its folder at all. A foreign Coop-Task trailer in
			// range for anything outside this set is tolerated rather than rejecting this completion —
			// see unbindableTasks, tasks.CompletionWindowSet.baselineDoneIDs, and
			// .agent/kb/loop-range-rejects-outside-commits.md. All of it is built and used inside the
			// ref authority window already entered above, so nothing can move HEAD or a queue folder
			// out from under the comparison.
			touched := map[string]bool{assigned.Item.ID: true}
			for _, id := range finished {
				touched[id] = true
			}
			if lease.Reopen != nil {
				touched[lease.Reopen.TaskID] = true
			}
			for _, t := range completedTasks {
				touched[t.Item.ID] = true
			}
			for _, id := range departed {
				touched[id] = true
			}
			for id := range windows.BaselineDoneIDs() {
				touched[id] = true
			}
			var missing, tolerated []string
			completionCandidate := assignedCompletion
			// A tool refusal leaves the task in progress. If the worker exits successfully instead
			// of repairing it in-session, give the same bounded recovery as a rejected folder move.
			if completionCandidate == nil && completionAttempted.Load() && classification.outcome == "success" && lease.Reopen == nil {
				current, ok, scanErr := tasks.CurrentTask(assigned.Root, assigned.Item.ID)
				if scanErr != nil {
					refRelease()
					return 1, errors.Join(scanErr, lease.Release(), windows.Abandon())
				}
				if ok && current.State == tasks.StateInProgress {
					checkErr := checkAssignedCompletion(repo, iterHead, assigned.Item.ID, nil, snapshot)
					if !errors.Is(checkErr, errCompletionBinding) {
						// A repaired commit without a successful final tool call is not completion.
						// Do not advance the iteration base and lose its gate/signoff attribution.
						releaseErr := errors.Join(lease.Release(), windows.Close())
						refRelease()
						stopErr := errors.Join(fmt.Errorf("task %s exited before confirming completion; its work is preserved in progress — inspect its task notes before retrying", assigned.Item.ID), checkErr, releaseErr)
						if checkErr == nil && releaseErr == nil {
							message := fmt.Sprintf("%s has a commit, but its completion was not confirmed.\nIts work is preserved in progress; Coop cannot safely advance to another task.", active)
							if checklistErr := tasks.RequireCompletedChecklist(current); checklistErr != nil {
								message += "\n" + checklistErr.Error()
								stopErr = errors.Join(stopErr, checklistErr)
							}
							ui.Failure("Task completion was not confirmed",
								message,
								[2]string{"Inspect:", "coop tasks path " + assigned.Item.ID},
								[2]string{"Then continue:", continueCmd})
							return 1, ui.Reported(stopErr)
						}
						return 1, stopErr
					}
					candidate := tasks.QueuedTask{Root: assigned.Root, Item: current}
					completionCandidate = &candidate
					missing = []string{assigned.Item.ID}
				}
			}
			if assignedCompletion != nil {
				missing, tolerated = tasks.CompletionUnbindableTasks(repo, iterHead, headAfter, finished, lease.Reopen, touched)
			}
			if reportErr := tasks.ReportToleratedForeignBindings(repo, hosts, iterHead, headAfter, assigned.Item.ID, tolerated); reportErr != nil {
				if assignedCompletion != nil {
					restoreErr = errors.Join(restoreErr, tasks.RestoreQueuedCompletion(*assignedCompletion, lease.Reopen != nil))
				}
				releaseErr := errors.Join(lease.Release(), windows.Abandon())
				refRelease()
				return 1, errors.Join(reportErr, restoreErr, releaseErr)
			}
			if len(missing) > 0 {
				restoreErr = errors.Join(restoreErr, tasks.RestoreQueuedCompletion(*completionCandidate, lease.Reopen != nil))
				canRepair := restoreErr == nil && len(unowned) == 0 && lease.Reopen == nil && classification.outcome == "success" && iterCtx.Err() == nil &&
					tasks.UncommittedCompletionCanRetry(repo, iterHead, headAfter, assigned.Item.ID)
				parked := canRepair && completionRepairs[assigned.Item.ID] > 0
				if parked {
					restoreErr = tasks.ParkUncommittedCompletion(assigned)
				}
				var windowErr error
				if restoreErr != nil {
					windowErr = windows.Abandon()
				} else {
					windowErr = windows.Close()
				}
				releaseErr := errors.Join(lease.Release(), windowErr)
				refRelease()
				if canRepair && restoreErr == nil && releaseErr == nil {
					completionRepairs[assigned.Item.ID]++
					outcome := "completion_rejected"
					if parked {
						outcome = "completion_blocked"
						ui.Alert("“"+cleanDiagnosticLine(active)+"” still cannot be completed",
							"The repair attempt did not produce an accepted completion.\nThe task is blocked for your decision.\nAnswer it: coop tasks decisions -i")
						ui.Note("")
						ui.Note("Continuing the task queue.")
					} else {
						ui.Alert("Completion was not accepted for “"+cleanDiagnosticLine(active)+"”",
							"No task-bound commit was found, starting one repair attempt.")
					}
					c.recordStage(repo, runid, "work", outcome, rot.Active(), iterStart, code, retries, 0, iterHead, hosts, nil, nil, res)
					continue
				}
				var unownedErr error
				if len(unowned) > 0 {
					unownedErr = tasks.UnownedCompletionError(unowned, nil)
				}
				bindErr := tasks.UnbindableCompletionError(missing, restoreErr)
				if lease.Reopen != nil {
					// With audit authority, missing is exactly the assigned reopened task and the
					// failure was the semantic replay validation, not trailer counting.
					bindErr = tasks.AuditCompletionError(missing[0], restoreErr)
				}
				if lease.Reopen == nil && restoreErr == nil && unownedErr == nil && releaseErr == nil {
					ui.Failure("Task completion rejected",
						fmt.Sprintf("The new commit range does not verify one unique task binding.\n%s is still in progress; its work and recovery notes are preserved.\nCoop cannot safely advance to another task.", active),
						[2]string{"Inspect:", "coop tasks path " + assigned.Item.ID},
						[2]string{"Then continue:", continueCmd})
					return 1, ui.Reported(bindErr)
				}
				return 1, errors.Join(bindErr, unownedErr, releaseErr)
			}
			if len(unowned) > 0 {
				if assignedCompletion != nil {
					restoreErr = errors.Join(restoreErr, tasks.RestoreCompromisedCompletion(*assignedCompletion, lease.Reopen != nil))
				}
				var windowErr error
				if restoreErr != nil {
					windowErr = windows.Abandon()
				} else {
					windowErr = windows.Close()
				}
				releaseErr := errors.Join(lease.Release(), windowErr)
				refRelease()
				return 1, errors.Join(tasks.UnownedCompletionError(unowned, restoreErr), releaseErr)
			}
			if lease.Reopen != nil && iterHead != headAfter {
				var rebindErr error
				for _, subject := range pendingReview.Subjects {
					if subject.Task.Ref.ID == assigned.Item.ID {
						continue
					}
					root := pendingReviewSubjectRoot(pendingReview.Plan, subject)
					if root == "" {
						rebindErr = fmt.Errorf("pending final-review task %s has no recorded queue", subject.Task.Ref.ID)
						break
					}
					if err := tasks.RebindPendingReviewAfterAuditRewrite(repo, root, subject.Task.Ref.ID, iterHead, headAfter, assigned.Item.ID, *lease.Reopen); err != nil {
						rebindErr = fmt.Errorf("rebind pending final-review task %s after audit rewrite: %w", subject.Task.Ref.ID, err)
						break
					}
				}
				if rebindErr != nil {
					releaseErr := errors.Join(lease.Release(), windows.Close())
					refRelease()
					return 1, errors.Join(rebindErr, releaseErr)
				}
			}
			if err := lease.PreserveBlockedAuditReopen(repo, iterHead, headAfter); err != nil {
				releaseErr := errors.Join(lease.Release(), windows.Close())
				refRelease()
				return 1, errors.Join(fmt.Errorf("preserve task %s blocked audit reopen authority: %w", assigned.Item.ID, err), releaseErr)
			}
			// Finalize only the completion whose lease this controller owns. Concurrent controllers
			// close their own crash boundaries and unowned moves have already failed closed above.
			if assignedCompletion != nil {
				if cleanupErr := tasks.FinalizeQueuedCompletion(*assignedCompletion); cleanupErr != nil {
					releaseErr := errors.Join(lease.Release(), windows.Abandon())
					refRelease()
					return 1, errors.Join(fmt.Errorf("%w — completion was not accepted; fix the obstruction and re-run `coop loop`", cleanupErr), releaseErr)
				}
				var receiptErr error
				if len(custom) == 0 {
					receiptErr = lease.MarkCompletedForReview(repo, assignedCompletion.Item, reviewPlan)
				} else {
					receiptErr = lease.MarkCompleted(assignedCompletion.Item.Dir)
				}
				if receiptErr != nil {
					restoreErr := tasks.RestoreUnrecordedCompletion(*assignedCompletion)
					clearErr := lease.ClearCompleted()
					releaseErr := errors.Join(lease.Release(), windows.Abandon())
					refRelease()
					return 1, errors.Join(fmt.Errorf("record task completion %s: %w", assigned.Item.ID, receiptErr), restoreErr, clearErr, releaseErr)
				}
				if consumeErr := lease.ConsumeAuditReopen(); consumeErr != nil {
					releaseErr := errors.Join(lease.Release(), windows.Close())
					refRelease()
					return 1, errors.Join(fmt.Errorf("consume task %s audit reopen authority: %w", assigned.Item.ID, consumeErr), releaseErr)
				}
			}
			refRelease()
			if releaseErr := errors.Join(lease.Release(), windows.Close()); releaseErr != nil {
				return 1, fmt.Errorf("release task lease %s: %w", assigned.Item.ID, releaseErr)
			}
			if assignedCompletion != nil {
				completedThisRun[assignedCompletion.Item.ID] = true
				pendingReopened = slices.DeleteFunc(pendingReopened, func(id string) bool {
					return id == assignedCompletion.Item.ID
				})
				completedLine := taskReportLine(assignedCompletion.Item, scopes[assignedCompletion.Root])
				completedLines = rememberCompletion(completedLines, completedLine)
				printTaskCompleted(completedLine)
			}
			gateHits := tasks.ProtectedGateChanges(repo, iterHead, headAfter)
			health.noteIteration(finished, gateHits)
			// A second Ctrl-C canceled iterCtx and tore the box down mid-iteration — stop only after
			// completion validation and finalization closed the crash boundary above. Record the actual
			// attempt as interrupted rather than silently dropping it from telemetry.
			if iterCtx.Err() != nil {
				c.recordStage(repo, runid, "work", "interrupted", rot.Active(), iterStart, code, retries, 0, iterHead, hosts, finished, gateHits, res)
				break
			}
			action, wait, resetAt := decideIteration(classification, time.Now(), &fails, &waits, &retries)
			// Host signing rewrites commit SHAs. Do it before recording successful work so telemetry and
			// every reviewer name the final commits rather than the unsigned pre-rebase heads.
			if action == actContinue && forkspace.WantsSigning() {
				if signed, serr := c.host.signUnpushed(repo, iterHead); serr != nil {
					printSigningFailure(0, serr)
				} else if signed > 0 {
					signedHead := gitOut(repo, "rev-parse", "HEAD")
					if assignedCompletion != nil && len(custom) == 0 {
						if rebindErr := tasks.RebindPendingReviewAfterSigning(repo, assignedCompletion.Root, assignedCompletion.Item.ID, headAfter, signedHead); rebindErr != nil {
							return 1, fmt.Errorf("rebind task %s final-review evidence after signing: %w", assignedCompletion.Item.ID, rebindErr)
						}
					}
					ui.Note("Signed %s with your host key.", ui.Count(signed, "commit"))
				}
				headAfter = gitOut(repo, "rev-parse", "HEAD")
			}
			c.recordStage(repo, runid, "work", classification.outcome, rot.Active(), iterStart, code, retries, 0, iterHead, hosts, finished, gateHits, res)
			// Review a just-completed task now when a successful iteration has ordinary between
			// review configured OR its complete run-bound diff touched the gate. Protected completion
			// is checked even when the worker exited nonzero, so a retry cannot hand a changed checker
			// to the next task before the mandatory audit runs.
			if len(custom) == 0 {
				if assignedCompletion != nil {
					finishedDirs := []string{assignedCompletion.Item.ID + " — " + assignedCompletion.Item.Dir}
					finishedIDs := taskIDsOf(finishedDirs)
					stepChanges := loopChanges(repo, loopStartHead, headAfter).forTasks(finishedIDs)
					auditGateFiles := tasks.ProtectedGateFiles(append(stepChanges.gateFiles(), gateHits...))
					setPrompt, auditAvailable := betweenAuditSetPrompt(betweenEnabled, lc.Between.Prompt, auditGateFiles)
					protectedAudit := len(auditGateFiles) > 0
					runAudit := shouldRunBetweenAudit(action == actContinue, auditAvailable, protectedAudit)
					if runAudit {
						betweenCohort, loadErr := tasks.LoadPendingReviews(repo, hosts)
						if loadErr != nil {
							return 1, fmt.Errorf("capture between-review subjects: %w", loadErr)
						}
						betweenRecords, selectErr := pendingReviewRecordsForIDs(betweenCohort, finishedIDs)
						if selectErr != nil {
							return 1, selectErr
						}
						pendingReview = betweenCohort
						if protectedAudit {
							printProtectedReview(betweenRot.Active().String(), auditGateFiles)
						} else {
							reviewHeader("Reviewing task: "+assignedCompletion.Item.Title, betweenRot.Active().String())
						}
						prompt := loopBetweenPrompt(repo, queues, substituteLoopVars(setPrompt, stepChanges, health), finishedDirs, auditGateFiles) + stepChanges.reviewBlock(health)
						// An ordinary configured audit preserves its historical warn-and-continue behavior.
						// A protected audit is mandatory: failure or a missing/mismatched receipt stops
						// before another task can trust the changed gate.
						stage := "between audit"
						if protectedAudit {
							stage = "protected audit"
						}
						// A first Ctrl-C is a soft stop: the completed task still earns its audit. Only
						// the second cancels iterCtx; its Done channel also wakes a review backoff promptly.
						hardStop := iterCtx.Done()
						observe := func(run reviewRunResult, start time.Time, headBefore string) {
							c.recordStage(repo, runid, "between", run.outcome, run.target, start, run.exit, run.retries, len(run.reopened), headBefore, hosts, nil, auditGateFiles, run.usage)
						}
						btRun, rerr := c.runReviewVerdict(iterCtx, repo, img, betweenRot, forkName, prompt, reviewActivity(stage, finishedIDs), iterCmd, hosts, finishedIDs, &reviewPlan, lc.Between.Writes, sink, peers, hardStop, observe)
						reviewBaseline = reviewBaselineAfterVerdict(reviewBaseline, nil, nil, btRun.concurrent)
						reopenedIDs := btRun.reopened
						if len(reopenedIDs) > 0 {
							reopenedRecords, selectErr := pendingReviewRecordsForIDs(tasks.PendingReviewCohort{Subjects: betweenRecords}, reopenedIDs)
							if selectErr != nil {
								return 1, selectErr
							}
							if err := tasks.MarkExpectedPendingReviewsReopened(hosts, reopenedRecords); err != nil {
								return 1, fmt.Errorf("record between-review reopens: %w", err)
							}
						}
						if len(btRun.concurrent) > 0 {
							pendingReview, err = tasks.LoadPendingReviews(repo, hosts)
							if err != nil {
								return 1, fmt.Errorf("reload concurrent between-review subjects: %w", err)
							}
						}
						if errors.Is(rerr, errReviewInterrupted) {
							break
						}
						if errors.Is(rerr, tasks.ErrCompletionWindowSetup) || errors.Is(rerr, tasks.ErrCompletionWindowAudit) || errors.Is(rerr, errReviewVerdict) {
							return 1, rerr
						}
						if rerr != nil && !protectedAudit {
							ui.Alert("The task review could not run",
								fmt.Sprintf("%v\n%s was left unreviewed.", rerr, assignedCompletion.Item.Title))
						}
						interrupted := iterCtx.Err() != nil
						if verdictErr := protectedAuditVerdict(protectedAudit, interrupted, rerr, btRun.output, reopenedIDs, finishedIDs); verdictErr != nil {
							ui.Failure("Could not verify changes to project checks",
								fmt.Sprintf("%v\nThe review failed before another task could start.\n%s still needs review.", verdictErr, assignedCompletion.Item.Title),
								[2]string{"Continue:", continueCmd})
							return 1, ui.Reported(fmt.Errorf("protected-change audit for %s: %w", strings.Join(finishedIDs, ", "), verdictErr))
						}
						if rerr == nil && !interrupted {
							audits.capture(finishedIDs, reopenedIDs, protectedAudit, btRun.output)
							audits.drop(reopenedIDs)
						}
					}
				}
			}
			// A first Ctrl-C lets completion binding, host signing, and the mandatory between/protected
			// audit finish, then skips retries and the final signoff. The exit remains interrupted (130),
			// because an intentionally incomplete batch is not queue verification.
			if softStop.Load() {
				break
			}
			// --debug-on-fail: on a non-rate-limit failure, open an interactive box shell
			// (same repo/image) to inspect, then retry — instead of the auto-retry/stop.
			if (action == actRetry || action == actStop) && debugOnFail && ui.IsTerminal(os.Stdin) {
				ui.Note("Opening a box shell to investigate this failed attempt.")
				ui.Note("  Exit the shell to retry. Press Ctrl-C to stop.")
				c.debugShell(repo, img, agent, spec.ForkName)
				fails = 0 // the developer intervened; don't count this toward the stop cap
				continue
			}
			switch action {
			case actContinue:
				// A clean iteration that neither finishes/blocks a task NOR commits means the agent keeps
				// continuing an in_progress task it can't complete — advanceStall bails after maxStalls
				// rather than loop forever (a commit or a block still counts as progress).
				var stop error
				prevHead, settledBaseline, stalls, stop = c.advanceStall(repo, hosts, prevHead, settledBaseline, stalls, active)
				if stop != nil {
					ui.Failure("The loop stopped making progress",
						fmt.Sprintf("No task finished, became blocked or produced a commit in %s.\nCurrent task: %s.", ui.Count(maxStalls, "attempt"), active),
						[2]string{"Task:", "coop tasks path " + assigned.Item.ID})
					return code, ui.Reported(stop)
				}
			case actWait:
				// A rate/usage limit is expected on long runs. With more than one target in
				// the ladder, switch to the next provider/model/account rung and retry immediately;
				// otherwise wait for the reset. Either way the same iteration is retried, not burned.
				if rot.Rotates() {
					// Advancing the rotation is the point — the loop head re-derives the agent
					// from rot (applyTarget), so the returned name would go unread here.
					c.rotateOnLimit(rot, resetAt, &waits, wake)
				} else {
					sleepForLimit(wait, resetAt, wake)
				}
			case actRetryNow:
				ui.Note("The model reached its response limit.")
				if wait > 0 {
					ui.Note("  Continuing in %s.", humanWait(wait))
					ladder.SleepOrWake(wait, wake)
				} else {
					ui.Note("  Continuing now.")
				}
			case actRetry:
				ui.Alert("Task attempt failed",
					fmt.Sprintf("Retrying in 10 seconds · attempt %d of %d.", fails+1, maxLoopFailures))
				ladder.SleepOrWake(10*time.Second, wake)
			case actStop:
				if waits > maxLimitWaits {
					ui.Failure(fmt.Sprintf("Stopped after %d usage-limit waits", maxLimitWaits),
						"Every configured agent was still rate limited.",
						[2]string{"Continue:", continueCmd})
					return code, ui.Reported(fmt.Errorf("still rate limited after %d waits", maxLimitWaits))
				}
				ui.Failure(fmt.Sprintf("Stopped after %d failed attempts", fails),
					fmt.Sprintf("%s did not finish.", active),
					[2]string{"Task:", "coop tasks path " + assigned.Item.ID})
				return code, ui.Reported(fmt.Errorf("iteration failed %d times since the last success", fails))
			case actAuthStop:
				// A dead credential is no reason to abandon the queue while another account can still
				// work: mark this rung unusable for the run and switch, exactly as a rate limit does.
				// The mark is sticky, so this rotates at most once per rung and can't spin. Only when
				// EVERY rung has failed authentication is there nothing left to try.
				if rot.Rotates() && rot.OnAuthFailure() {
					ui.Alert(authHeadline(target),
						wrappedLoopText(fmt.Sprintf("Continuing with %s.", cleanDiagnosticLine(agents.DisplayTarget(rot.Active().String()))), 6),
						[2]string{"Sign in again:", loginCommand(target)})
					break
				}
				ui.Failure("No configured agent could sign in",
					"Authentication failed for every available account.",
					[2]string{"Sign in again:", loginCommand(target)})
				return code, ui.Reported(rotationAuthenticationError(rot, target))
			case actOutputStop:
				ui.Failure(fmt.Sprintf("Stopped after %d response-limit retries", retries),
					fmt.Sprintf("%s did not finish within the model's response limit.", active),
					[2]string{"Task:", "coop tasks path " + assigned.Item.ID})
				return code, ui.Reported(fmt.Errorf("iteration reached the model output limit %d times", retries))
			}
		}
		// A requested stop (soft: the current iteration finished; hard: it was torn down) skips the
		// signoff pass and the drain summary — the queue isn't done, the user asked to stop.
		if softStop.Load() || iterCtx.Err() != nil {
			cf, _, err := tasks.QueueProgress(hosts)
			if err != nil {
				return 1, err
			}
			pending, _ := completedReviewSubjects(hosts, completedThisRun)
			notePendingFinalReview(pending)
			c.closeWith(func() { printInterrupted(cf, continueCmd, false) })
			return LoopInterruptedExitCode, nil
		}
		if limit.enabled() {
			cf, _, err := tasks.QueueProgress(hosts)
			if err != nil {
				return 1, err
			}
			resumeDebt := len(pendingReview.Subjects) > 0
			if limit.settled == 0 && !resumeDebt {
				c.closeWith(func() { printNoActionableTasks(cf) })
				return loopExitCode(cf), nil
			}
			if limit.settled > 0 {
				pending, _ := completedReviewSubjects(hosts, completedThisRun)
				notePendingFinalReview(pending)
				c.closeWith(func() { printTaskLimitReached(limit, continueCmd) })
				return 0, nil
			}
		}
		// A custom work.command isn't the signoff-aware agent form, so it gets no signoff pass —
		// today's behavior: drain the queue, then report.
		if len(custom) > 0 {
			break
		}
		// Scale the cap to tasks this controller actually accepted, not successful attempts that
		// happened to leave the same task in progress.
		maxReviewRounds = signoffRoundCap(len(completedThisRun), signoffRounds(lc))
		// The round's subjects: what entered done/ since the last accepted round (for round 1, since
		// the run started) — a folder diff, so it also catches a completion with no commit. Nothing
		// new means nothing to review: skip the pass instead of burning a box on 99_done/'s history.
		doneNow, err := doneTaskDirs(hosts)
		if err != nil {
			return 1, err
		}
		subjects := newlyFinished(reviewBaseline, doneNow)
		if len(subjects) == 0 {
			break // nothing newly completed: a review with no subject is not a verdict
		}
		subjectIDs := taskIDsOf(subjects)
		// A host completion that landed between loop bookkeeping steps still carries its receipt.
		// Enroll it before launching the reviewer; the folder diff names subjects, never authority.
		if err := tasks.EnrollExistingPendingReviews(repo, hosts, subjectIDs, reviewPlan); err != nil {
			return 1, fmt.Errorf("preserve final-review subjects: %w", err)
		}
		if err := tasks.BeginPendingReviewRound(hosts, subjectIDs, signoffRound); err != nil {
			return 1, fmt.Errorf("record final-review round %d: %w", signoffRound, err)
		}
		roundCohort, err := tasks.LoadPendingReviews(repo, hosts)
		if err != nil {
			return 1, fmt.Errorf("capture final-review round %d subjects: %w", signoffRound, err)
		}
		roundRecords, err := pendingReviewRecordsForIDs(roundCohort, subjectIDs)
		if err != nil {
			return 1, err
		}
		printFinalReview(signoffRound, maxReviewRounds, signoffRot.Active().String(), len(subjects))
		// The signoff runs on signoff.agent's OWN target — a stronger, usually different-vendor model
		// reviews the work loop's output — and fails CLOSED: if it can't run after retries, stop loudly
		// rather than let "nothing reopened" read as an accepting signoff.
		// Hand the signoff the run's change context (per task, bound by the Coop-Task trailer) + health,
		// so a prompt like "e2e the affected features" resolves against a concrete list. Rebuilt each
		// round because the range (loopStartHead..HEAD) grows as reopened work lands.
		soHead := gitOut(repo, "rev-parse", "HEAD")
		cs := loopChanges(repo, loopStartHead, soHead)
		signoff := loopSignoffPrompt(repo, queues, substituteLoopVars(lc.Signoff.Prompt, cs, health), subjects) + audits.signoffBlock(subjectIDs) + cs.reviewBlock(health)
		observe := func(run reviewRunResult, start time.Time, headBefore string) {
			c.recordStage(repo, runid, "signoff", run.outcome, run.target, start, run.exit, run.retries, len(run.reopened), headBefore, hosts, nil, nil, run.usage)
		}
		soRun, serr := c.runReviewVerdict(iterCtx, repo, img, signoffRot, forkName, signoff, reviewActivity("signoff", subjectIDs), iterCmd, hosts, subjectIDs, &reviewPlan, lc.Signoff.Writes, sink, peers, wake, observe)
		// Preserve the exact tasks the host reopened before any early return.
		reopenedIDs := soRun.reopened
		if len(soRun.concurrent) > 0 {
			if err := tasks.EnrollExistingPendingReviews(repo, hosts, soRun.concurrent, reviewPlan); err != nil {
				return 1, fmt.Errorf("preserve concurrent final-review subjects: %w", err)
			}
		}
		if len(reopenedIDs) > 0 {
			reopenedRecords, selectErr := pendingReviewRecordsForIDs(tasks.PendingReviewCohort{Subjects: roundRecords}, reopenedIDs)
			if selectErr != nil {
				return 1, selectErr
			}
			if err := tasks.MarkExpectedPendingReviewsReopened(hosts, reopenedRecords); err != nil {
				return 1, fmt.Errorf("record final-review reopens: %w", err)
			}
			if resumingPendingReview {
				pendingReopened = slices.Compact(slices.Sorted(slices.Values(append(pendingReopened, reopenedIDs...))))
			}
		}
		if errors.Is(serr, errReviewInterrupted) {
			cf, _, err := tasks.QueueProgress(hosts)
			if err != nil {
				return 1, err
			}
			notePendingFinalReview(subjectIDs)
			c.closeWith(func() { printInterrupted(cf, continueCmd, true) })
			return LoopInterruptedExitCode, nil
		}
		if serr != nil {
			ui.Failure("Could not complete the final review", serr.Error(), [2]string{"Continue:", continueCmd})
			return 1, ui.Reported(serr)
		}
		health.noteReopen(reopenedIDs)
		// Guard against a lost verdict (the 2026-07-10 incident): a signoff that DECIDES reopens as
		// prose but never moves the folders — its subagents interrupted, or it batched them past the
		// end — would leave the queue empty and read as "accepted". The review must end with a
		// structured receipt; if its ids disagree with the folders that actually moved (or the receipt
		// is missing entirely), the round is treated as interrupted and
		// re-run within the cap, or — at the cap — the loop exits loudly rather than claim a false done.
		receipt, ok := reviewReopenReceipt(soRun.output)
		if reopenVerdictLost(receipt, ok, reopenedIDs, subjectIDs) {
			if signoffRound >= maxReviewRounds {
				return 3, fmt.Errorf("signoff verdict inconsistent after %d rounds: review reported %s but task delta was %s — verdicts may have been lost, a human should look", maxReviewRounds, receiptClaim(receipt, ok), receiptIDs(reopenedIDs))
			}
			ui.Alert("The review result could not be read",
				fmt.Sprintf("It reported %s, but the task queue moved %s.\nRepeating the full review once with the required response format.", receiptClaim(receipt, ok), receiptIDs(reopenedIDs)))
			continue
		}
		acceptedIDs := withoutReviewIDs(subjectIDs, reopenedIDs)
		acceptedRecords, selectErr := pendingReviewRecordsForIDs(tasks.PendingReviewCohort{Subjects: roundRecords}, acceptedIDs)
		if selectErr != nil {
			return 1, selectErr
		}
		if verifyEnabled {
			if err := tasks.MarkExpectedPendingReviewsVerify(hosts, acceptedRecords); err != nil {
				return 1, fmt.Errorf("record final verification subjects: %w", err)
			}
		} else {
			if err := tasks.ClearPendingReviews(hosts, acceptedRecords); err != nil {
				return 1, fmt.Errorf("clear accepted final-review subjects: %w", err)
			}
		}
		if len(soRun.concurrent) > 0 {
			pendingReview, err = tasks.LoadPendingReviews(repo, hosts)
			if err != nil {
				return 1, fmt.Errorf("reload concurrent final-review subjects: %w", err)
			}
		}
		// A stop that landed during a successful signoff is honored only after its host-applied
		// receipt and durable phase transition are complete.
		if softStop.Load() || iterCtx.Err() != nil {
			cf, _, err := tasks.QueueProgress(hosts)
			if err != nil {
				return 1, err
			}
			pendingStopIDs := slices.Clone(reopenedIDs)
			if verifyEnabled {
				pendingStopIDs = append(pendingStopIDs, acceptedIDs...)
			}
			notePendingFinalReview(pendingStopIDs)
			c.closeWith(func() { printInterrupted(cf, continueCmd, false) })
			return LoopInterruptedExitCode, nil
		}
		audits.drop(reopenedIDs)
		// This round's verdict is consistent — advance the baseline past its accepted subjects
		// WITHOUT rescanning done/. A completion landing during or just after the review window stays
		// outside the baseline and enters the next round's subject diff. The lost-verdict path above
		// deliberately keeps the old baseline so the whole untrusted subject set is reviewed again.
		reviewBaseline = reviewBaselineAfterVerdict(reviewBaseline, subjects, reopenedIDs, soRun.concurrent)
		switch signoffRoundOutcome(signoffRound, maxReviewRounds, len(reopenedIDs) > 0) {
		case signoffContinue:
			printReopened("Final review", reopenedIDs, titlesOf(hosts, reopenedIDs))
			continue
		case signoffAccepted:
			doneNow, err := doneTaskDirs(hosts)
			if err != nil {
				return 1, err
			}
			if pending := taskIDsOf(newlyFinished(reviewBaseline, doneNow)); len(pending) > 0 {
				ui.Note("Another session completed %s during this review. Reviewing it before finishing.", ui.Count(len(pending), "task"))
				signoffRound = 0
				continue
			}
		case signoffCapReached:
			// The work loop couldn't get these tasks to a state the signoff accepts within the cap —
			// park them for a human rather than spin or claim a false "done" (exit 3 via loopExitCode).
			stuck := titlesOf(hosts, reopenedIDs)
			if err := blockReopenedTasks(hosts, reopenedIDs, maxReviewRounds); err != nil {
				return 3, err
			}
			// Only after the host transition succeeded may the report say the work is blocked.
			printReviewLimit(stuck, maxReviewRounds)
			ui.Note("%s coop tasks decisions -i", decisionsLabel(len(reopenedIDs)))
			doneNow, err := doneTaskDirs(hosts)
			if err != nil {
				return 1, err
			}
			if pending := taskIDsOf(newlyFinished(reviewBaseline, doneNow)); len(pending) > 0 {
				ui.Note("Another session completed %s during this review. Reviewing it before finishing.", ui.Count(len(pending), "task"))
				signoffRound = 0
				continue
			}
			reviewCapped = true
		}
		// signoffAccepted (nothing reopened or pending) or signoffCapReached (just blocked) → done.
		break
	}
	// Verify: an optional FINAL pass over the whole run's changes — its prompt (verify.prompt) says
	// what, typically "e2e-test the affected features". It runs after the signoff accepted the batch,
	// on its own model, with the run's change context injected; best-effort, and it may reopen a task
	// whose e2e it can't get to pass (surfaced in the closing digest + exit). Skipped on a custom
	// work.command or a requested stop. Ordinary process failures preserve completed work but make
	// the final verdict unverified and nonzero; completion ownership setup/audit failures stop here.
	if verifyEnabled && !reviewCapped && !softStop.Load() && iterCtx.Err() == nil {
		cs, changeErr := finalVerificationChanges(repo, loopStartHead)
		if changeErr != nil {
			verificationFailed = true
			ui.Alert("The final checks could not run",
				fmt.Sprintf("The run's changes could not be verified: %v\nThe affected work remains unverified.", changeErr))
		} else if cs.empty() {
			// Nothing changed: a check with nothing to check is omitted, not reported as skipped.
			pendingVerify, pendingErr := tasks.LoadPendingReviews(repo, hosts)
			if pendingErr != nil {
				return 1, fmt.Errorf("validate no-op final verification subjects: %w", pendingErr)
			}
			verifyIDs := pendingReviewIDs(pendingVerify, tasks.PendingReviewVerify)
			expected, selectErr := pendingReviewRecordsForIDs(pendingVerify, verifyIDs)
			if selectErr != nil {
				return 1, selectErr
			}
			if err := tasks.ClearPendingReviews(hosts, expected); err != nil {
				return 1, fmt.Errorf("clear no-op final-verification subjects: %w", err)
			}
		} else {
			vPrompt := substituteLoopVars(lc.Verify.Prompt, cs, health) + cs.reviewBlock(health) +
				"\n\n" + auditEvidencePrompt + "\n\n" + reviewContextFooter(repo, queues)
			pendingVerify, pendingErr := tasks.LoadPendingReviews(repo, hosts)
			if pendingErr != nil {
				return 1, fmt.Errorf("capture final-verification subjects: %w", pendingErr)
			}
			verifyIDs := pendingReviewIDs(pendingVerify, tasks.PendingReviewVerify)
			verifyRecords, selectErr := pendingReviewRecordsForIDs(pendingVerify, verifyIDs)
			if selectErr != nil {
				return 1, selectErr
			}
			printVerification(verifyRot.Active().String(), len(verifyIDs))
			if len(cs.subsystems) > 0 {
				ui.Note("Affected areas: %s", strings.Join(cs.subsystems, ", "))
			}
			verifyActivity := reviewActivity("verify", verifyIDs)
			if len(verifyIDs) == 0 {
				verifyActivity = "verify: unbound changes"
			}
			observe := func(run reviewRunResult, start time.Time, headBefore string) {
				c.recordStage(repo, runid, "verify", run.outcome, run.target, start, run.exit, run.retries, len(run.reopened), headBefore, hosts, nil, nil, run.usage)
			}
			vRun, verr := c.runReviewVerdict(iterCtx, repo, img, verifyRot, forkName, vPrompt, verifyActivity, iterCmd, hosts, verifyIDs, &reviewPlan, lc.Verify.Writes, sink, peers, wake, observe)
			reopenedIDs := vRun.reopened
			if len(vRun.concurrent) > 0 {
				if err := tasks.EnrollExistingPendingReviews(repo, hosts, vRun.concurrent, reviewPlan); err != nil {
					return 1, fmt.Errorf("preserve concurrent verification subjects: %w", err)
				}
			}
			if len(reopenedIDs) > 0 {
				reopenedRecords, selectErr := pendingReviewRecordsForIDs(tasks.PendingReviewCohort{Subjects: verifyRecords}, reopenedIDs)
				if selectErr != nil {
					return 1, selectErr
				}
				if err := tasks.MarkExpectedPendingReviewsReopened(hosts, reopenedRecords); err != nil {
					return 1, fmt.Errorf("record verification reopens: %w", err)
				}
				if resumingPendingReview {
					pendingReopened = slices.Compact(slices.Sorted(slices.Values(append(pendingReopened, reopenedIDs...))))
				}
			}
			health.noteReopen(reopenedIDs)
			if errors.Is(verr, errReviewInterrupted) {
				cf, _, err := tasks.QueueProgress(hosts)
				if err != nil {
					return 1, err
				}
				notePendingFinalReview(verifyIDs)
				c.closeWith(func() { printInterrupted(cf, continueCmd, true) })
				return LoopInterruptedExitCode, nil
			}
			if errors.Is(verr, tasks.ErrCompletionWindowSetup) || errors.Is(verr, tasks.ErrCompletionWindowAudit) {
				return 1, verr
			}
			if verr != nil {
				verificationFailed = true
			}
			if errors.Is(verr, errReviewVerdictMalformed) {
				ui.Alert("The final checks could not be read",
					"The review result stayed malformed after one corrected attempt.\nThe affected work remains unverified.")
			} else if verr != nil {
				ui.Alert("The final checks could not run",
					fmt.Sprintf("%v\nThe affected work remains unverified.", verr))
			}
			if verr == nil {
				expected, selectErr := pendingReviewRecordsForIDs(tasks.PendingReviewCohort{Subjects: verifyRecords}, withoutReviewIDs(verifyIDs, reopenedIDs))
				if selectErr != nil {
					return 1, selectErr
				}
				if err := tasks.ClearPendingReviews(hosts, expected); err != nil {
					return 1, fmt.Errorf("clear accepted final-verification subjects: %w", err)
				}
				if len(vRun.concurrent) > 0 {
					pendingReview, err = tasks.LoadPendingReviews(repo, hosts)
					if err != nil {
						return 1, fmt.Errorf("reload concurrent verification subjects: %w", err)
					}
				}
				audits.drop(reopenedIDs)
				reviewBaseline = reviewBaselineAfterVerdict(reviewBaseline, nil, reopenedIDs, vRun.concurrent)
				if len(reopenedIDs) > 0 {
					switch signoffRoundOutcome(signoffRound, maxReviewRounds, true) {
					case signoffContinue:
						printReopened("Verification", reopenedIDs, titlesOf(hosts, reopenedIDs))
						signoffRound++
						goto reviewAgain
					case signoffCapReached:
						stuck := titlesOf(hosts, reopenedIDs)
						if err := blockReopenedTasks(hosts, reopenedIDs, maxReviewRounds); err != nil {
							return 3, err
						}
						printReviewLimit(stuck, maxReviewRounds)
						ui.Note("%s coop tasks decisions -i", decisionsLabel(len(reopenedIDs)))
						reviewCapped = true
					case signoffAccepted:
						return 1, errors.New("internal review decision accepted a non-empty verification reopen")
					}
				}
			} else {
				reviewBaseline = reviewBaselineAfterVerdict(reviewBaseline, nil, nil, vRun.concurrent)
			}
			doneNow, err := doneTaskDirs(hosts)
			if err != nil {
				return 1, err
			}
			if pending := taskIDsOf(newlyFinished(reviewBaseline, doneNow)); len(pending) > 0 {
				ui.Note("Another session completed %s during this review. Reviewing it before finishing.", ui.Count(len(pending), "task"))
				signoffRound = 1
				goto reviewAgain
			}
			if verr == nil {
				// Reaching here means this verification returned an accepted verdict, reopened
				// nothing and observed no completion that still needs its own signoff. Only that
				// complete pass can clear an earlier failed attempt from a prior review round.
				verificationFailed = false
			}
		}
	}
	if resumingPendingReview && !reviewCapped && !verificationFailed && !softStop.Load() && iterCtx.Err() == nil {
		remaining, pendingErr := tasks.LoadPendingReviews(repo, hosts)
		if pendingErr != nil {
			return 1, fmt.Errorf("validate resumed final review: %w", pendingErr)
		}
		if len(remaining.Subjects) == 0 {
			// The older cohort is fully receipt-accepted. Restore this invocation's settings before
			// touching unrelated queue work, and give new completions their own run-anchored cohort.
			resumingPendingReview = false
			pendingReview = tasks.PendingReviewCohort{}
			pendingReopened = nil
			lc.Signoff, lc.Verify = currentSignoff, currentVerify
			lc.Signoff.Agent = slices.Clone(currentSignoff.Agent)
			lc.Verify.Agent = slices.Clone(currentVerify.Agent)
			signoffRot, betweenRot, verifyRot = currentSignoffRot, currentBetweenRot, currentVerifyRot
			betweenEnabled, verifyEnabled = currentBetweenEnabled, currentVerifyEnabled
			custom = slices.Clone(currentCustom)
			if currentMCPDisabled {
				c.cfg.MCPFile = ""
			} else {
				c.cfg.MCPFile = currentMCPFile
			}
			if err := runPreflight(); err != nil {
				return 1, err
			}
			c0, _, err = tasks.QueueProgress(hosts)
			if err != nil {
				return 1, err
			}
			settledBaseline = c0.Done + c0.Blocked
			completedThisRun = map[string]bool{}
			prevHead, headErr = gitOutErr(repo, "rev-parse", "HEAD")
			if headErr != nil {
				return 1, fmt.Errorf("read HEAD after resumed final review: %w", headErr)
			}
			loopStartHead = prevHead
			reviewPlan, err = pendingReviewPlanForRun(repo, hosts, loopStartHead, cfgSnap.Digest(), continueCmd, lc, signoffRot, verifyRot, currentMCPDisabled)
			if err != nil {
				return 1, fmt.Errorf("prepare final review after recovery: %w", err)
			}
			reviewBaseline, err = doneTaskDirs(hosts)
			if err != nil {
				return 1, err
			}
			signoffRound, maxReviewRounds = 1, 0
			if c0.Todo+c0.Doing > 0 {
				ui.Note("The earlier final review is settled. Continuing with the current queue.")
				goto reviewAgain
			}
		}
	}
	if softStop.Load() || iterCtx.Err() != nil {
		cf, _, err := tasks.QueueProgress(hosts)
		if err != nil {
			return 1, err
		}
		pending, _ := completedReviewSubjects(hosts, completedThisRun)
		notePendingFinalReview(pending)
		c.closeWith(func() { printStoppedBeforeFinalVerdict(cf) })
		return LoopInterruptedExitCode, nil
	}
	// End-of-run signing sweep: normally a no-op (per-cycle signing already covered each iteration),
	// but it catches any straggler — a commit from a previously interrupted run, or a preflight
	// commit — so the whole run's range is signed before you push. Best-effort.
	if forkspace.WantsSigning() && len(custom) == 0 {
		unsignedHead := gitOut(repo, "rev-parse", "HEAD")
		pendingBeforeSigning, pendingErr := tasks.LoadPendingReviews(repo, hosts)
		if pendingErr != nil {
			return 1, fmt.Errorf("validate pending final-review evidence before signing: %w", pendingErr)
		}
		if signed, serr := c.host.signUnpushed(repo, loopStartHead); serr != nil {
			printSigningFailure(0, serr)
		} else if signed > 0 {
			signedHead := gitOut(repo, "rev-parse", "HEAD")
			for _, subject := range pendingBeforeSigning.Subjects {
				root := pendingReviewSubjectRoot(pendingBeforeSigning.Plan, subject)
				if root == "" {
					return 1, fmt.Errorf("pending final-review task %s has no recorded queue", subject.Task.Ref.ID)
				}
				if err := tasks.RebindPendingReviewAfterSigning(repo, root, subject.Task.Ref.ID, unsignedHead, signedHead); err != nil {
					return 1, fmt.Errorf("rebind pending final-review evidence after signing: %w", err)
				}
			}
			ui.Note("Signed %s with your host key.", ui.Count(signed, "commit"))
		}
	}
	cf, _, err := tasks.QueueProgress(hosts)
	if err != nil {
		return 1, err
	}
	// A human-facing digest above the verdict banner: what shipped (per task + areas), what's blocked,
	// and any task the run flagged — so you see what to review/e2e at a glance.
	if len(custom) == 0 {
		cost := costFromRecords(readStageRecords(repo, runid), ReadPeerRecords(repo, runid))
		printRunSummary(completedLines, cost, health)
		// Done folders accumulate until a human prunes them (agents never delete) — and a big
		// 99_done/ taxes every future run: each iteration's box lists it, and it's the haystack a
		// crash-resume scan walks. Past a threshold, say so once, at close.
		if nudge := pruneNudge(cf.Done); nudge != "" {
			fmt.Fprintln(os.Stderr, nudge)
		}
	}
	actionable, blocked, err := queueTaskLines(hosts, scopes)
	if err != nil {
		return 1, err
	}
	c.closeWith(func() {
		if verificationFailed && cf.Todo+cf.Doing+cf.Blocked == 0 {
			printFailedVerificationVerdict(cf)
			return
		}
		printFinalVerdict(cf, actionable, blocked, continueCmd)
	})
	return loopExitCodeAfterVerification(cf, verificationFailed), nil
}

// closeWith prints the run's final banner, flushing the networking summary first
// so the banner stays the LAST line however the run ended — drained, capped,
// or interrupted. Every exit that has a banner goes through here.
// proposalOutboxPath is the fork proposal outbox as an absolute path, or "" for a plain loop.
func (c *Control) proposalOutboxPath(repo string) string {
	if c.proposalOutbox == "" {
		return ""
	}
	return filepath.Join(repo, c.proposalOutbox)
}

func (c *Control) closeWith(report func()) {
	c.net.summary()
	report()
}

// rememberPreflightLimit carries a failed custom pre-flight's provider limit into the work
// rotation. A successful pre-flight may legitimately discuss limits, and output exhaustion is
// resumable rather than a provider limit, so neither changes target selection.
func rememberPreflightLimit(r *ladder.Rotation, classification iterationClassification, now time.Time) (wait time.Duration, until time.Time, limited bool) {
	if classification.outcome == "success" {
		return 0, time.Time{}, false
	}
	hint := classification.limit
	if !hint.Limited || hint.OutputLimited {
		return 0, time.Time{}, false
	}
	wait, until = r.OnLimit(hint.ResetAt, 1, now)
	return wait, until, true
}

// doneNudgeThreshold is how many done task folders accumulate before the loop's close suggests
// pruning. Agents never delete tasks, so without a nudge the pile only grows.
const doneNudgeThreshold = 10

// pruneNudge is the one-line prune suggestion once done/ has accumulated past the threshold; ""
// below it. The command is named, never run — pruning destroys state, so it stays the human's call.
func pruneNudge(done int) string {
	if done < doneNudgeThreshold {
		return ""
	}
	return fmt.Sprintf("  %s are archived. To delete them: coop tasks rm --all-done",
		ui.Count(done, "completed task folder"))
}

// advanceStall updates the loop's stall bookkeeping after a clean iteration and reports whether to
// stop. Progress is a task SETTLING (done or blocked) OR a new commit — a genuinely stuck loop keeps
// continuing an in_progress task it can't finish AND commits nothing, so after maxStalls such
// iterations it returns a stop error rather than looping forever. It returns the updated
// (prevHead, settledBaseline, stalls); a new commit resets the stall count and rebaselines.
// An unreadable HEAD stops the loop instead of counting as "no new commit": a git failure would
// otherwise masquerade as a stalled iteration, spending the stall budget on a broken repo — and the
// next iteration would work a task it can't bind a commit range to anyway.
func (c *Control) advanceStall(repo string, hosts []string, prevHead string, settledBaseline, stalls int, active string) (string, int, int, error) {
	after, _, err := tasks.QueueProgress(hosts)
	if err != nil {
		return prevHead, settledBaseline, stalls, err
	}
	settled := after.Done + after.Blocked
	head, err := gitOutErr(repo, "rev-parse", "HEAD")
	if err != nil {
		return prevHead, settledBaseline, stalls, fmt.Errorf("read HEAD of %s after the iteration: %w — the loop cannot tell a committing iteration from a stalled one without it; fix the repo, then re-run `coop loop` (in-progress work is resumed, nothing is lost)", repo, err)
	}
	if head != prevHead {
		return head, settled, 0, nil
	}
	newBase, newStalls, stop := progressStall(settled, settledBaseline, stalls)
	if stop {
		return prevHead, settledBaseline, stalls, fmt.Errorf("no task finished, blocked, or committed in %d iterations — stopping (stuck on %q?)", maxStalls, active)
	}
	return prevHead, newBase, newStalls, nil
}

// builtinPreflight is the host-side pre-loop tidy: no box, no model, no tokens. It returns blocked
// tasks whose decision.md now carries a Resolution to todo, and releases claims whose owning
// process is gone — both make work actionable again, so both run before the loop decides whether
// there is anything to do.
type preflightResult struct {
	unblocked []string
	released  []string
}

func builtinPreflight(hosts []string) (preflightResult, error) {
	var result preflightResult
	ids, err := tasks.UnblockResolved(hosts)
	if err != nil {
		return result, err
	}
	result.unblocked = ids
	released, err := tasks.ReleaseGoneOwners(hosts)
	if err != nil {
		return result, err
	}
	result.released = released
	return result, nil
}

func printPreflight(hosts []string, result preflightResult) {
	ui.Section("Running pre-flight checks")
	ui.Note("  Resolving answered blockers")
	for _, id := range result.unblocked {
		ui.Note("  %s Unblocked “%s” - resolution filled in", ui.Green("✓"), cleanDiagnosticLine(taskTitle(hosts, id)))
	}
	for _, id := range result.released {
		ui.Note("  %s Released “%s” - the claiming process is gone", ui.Green("✓"), cleanDiagnosticLine(taskTitle(hosts, id)))
	}
}
