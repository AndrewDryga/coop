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

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/ladder"
	"github.com/AndrewDryga/coop/internal/loopcfg"
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
func loopInterruptInfo(msg string) {
	if !ui.LiveActive() {
		fmt.Fprintln(os.Stderr)
	}
	ui.Info("%s", msg)
}

type loopTaskLimit struct {
	max       int
	settled   int
	currentID string
	lastID    string
	lastState string
}

func (l *loopTaskLimit) enabled() bool { return l.max > 0 }

func (l *loopTaskLimit) scope() string {
	if !l.enabled() {
		return ""
	}
	return l.currentID
}

func (l *loopTaskLimit) assign(id string) {
	if l.enabled() && l.currentID == "" {
		l.currentID = id
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
	l.lastID, l.lastState = l.currentID, state
	if l.settled >= l.max {
		return true, nil
	}
	l.currentID = ""
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
	c.preset, c.forkOwner = spec.Preset, spec.ForkOwner
	repo, img, agent, forkName := spec.Repo, spec.Image, spec.Agent, spec.ForkName
	rot, queues, sink, peers := spec.Rotation, spec.Queues, spec.Sink, spec.Peers
	debugOnFail, preflight, maxTasks := spec.DebugOnFail, spec.Preflight, spec.MaxTasks
	hosts := make([]string, len(queues)) // the queues' absolute host paths
	for i, q := range queues {
		hosts[i] = filepath.Join(repo, q)
	}
	// A queue is a directory (.agent/tasks), so check for one with tasks.IsTaskDir — fileExists is
	// false for a directory and used to reject every folder queue, so the loop never ran.
	if !slices.ContainsFunc(hosts, tasks.IsTaskDir) {
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
	ui.Info("loop config: %s", cfgSnap.State())
	// loop.yaml `mcp: false` runs EVERY stage's box without the shared MCP config — the schemas
	// ride at the front of each model request, so a drain that doesn't need those tools shouldn't
	// pay for them each iteration. Sitting here (not cmdLoop) it covers fork loops too. Blanking
	// MCPFile is the one switch everything downstream keys off (Config.MCPActive); the loop owns
	// this process, so nothing else reads the config after it. Caveat: a verify: pass whose e2e
	// depends on MCP tooling needs mcp left on — repo-local e2e via bash is unaffected.
	if lc.MCPDisabled() {
		c.cfg.MCPFile = ""
	}
	if err := tasks.ReconcileInterruptedCompletions(hosts); err != nil {
		return 1, fmt.Errorf("recover interrupted completion: %w", err)
	}
	recoveredReviewCompletions, err := tasks.ReconcileCompletionWindowsWithActivity(hosts)
	if err != nil {
		return 1, fmt.Errorf("recover interrupted completion window: %w", err)
	}
	if duplicates := tasks.NonArchivedDuplicateTaskIDs(hosts); len(duplicates) > 0 {
		return 1, fmt.Errorf("aggregated loop cannot safely distinguish non-archived task id(s) present in multiple queues: %s — rename the duplicates or select one queue with --tasks", strings.Join(duplicates, ", "))
	}
	custom := lc.Work.Command
	limit := loopTaskLimit{max: maxTasks}
	// A task-limited run with no actionable work is a pure host-side no-op: it does not need an
	// image and must not launch a configured preflight agent. Its built-in preflight may first
	// unblock answered decisions, since that is host-only and can make work actionable.
	preflightBuiltinRan := false
	if limit.enabled() && preflight && len(custom) == 0 {
		ui.Info("pre-flight: resolving answered blockers")
		if ids := tasks.UnblockResolved(hosts); len(ids) > 0 {
			ui.Info("pre-flight: unblocked %s — resolution filled in", strings.Join(ids, ", "))
		}
		preflightBuiltinRan = true
	}
	if limit.enabled() {
		cf, _ := tasks.QueueProgress(hosts)
		if cf.Todo+cf.Doing == 0 {
			fmt.Fprintln(os.Stderr, loopTaskLimitBanner(cf, limit))
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
	// A previous run of THIS checkout may have been killed with its box still up (--rm never fires
	// on SIGKILL). Reap those before adding another box, so an overnight drain doesn't stack an
	// orphaned provider session per crash. Fork loops run this too, each for its own workspace.
	c.host.sweepOrphanBoxes(repo)
	// Iterations run Batch (box.Run stays quiet), so surface image staleness once here —
	// an overnight drain on a month-old box is exactly where a stale nudge earns its line.
	for _, nudge := range box.StalenessNudges(c.cfg, repo, img) {
		ui.Info("%s", nudge)
	}
	// Hold a sleep inhibitor for the whole run so an unattended overnight drain isn't stalled by
	// the machine idle-sleeping (caffeinate on macOS; see armKeepAwake). Released when loop returns.
	defer armKeepAwake(c.cfg)()
	// Every built-in provider streams JSON that coop decodes into the same live lines — on a
	// TTY and on redirected runs alike, since the stream also feeds the provider watchdog. Only
	// a custom work.command keeps plain text output.
	// signoff.prompt APPENDS to the built-in senior review (it never replaces it).
	health := newLoopHealth() // per-task risk signals (reopens and gate edits) accumulated across the run
	audits := newAuditEvidenceStore()
	// The signoff pass (end-of-loop) and between-tasks audits both run only under the signoff-aware
	// agent form, not a custom work.command. Ordinary between review is opt-in; a completed task that
	// changed a protected gate path gets the narrow built-in audit even when it is off.
	betweenEnabled := len(custom) == 0 && lc.Between.Enabled
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
	iterCmd := func(iterAgent, prompt string) ([]string, bool) {
		if warning, drifted := cfgSnap.Drift(); drifted {
			ui.Warn("%s", warning)
		}
		var cmd []string
		if len(custom) == 0 {
			cmd = c.agentLoopCmd(iterAgent, prompt)
		}
		return IterationCommand(iterAgent, cmd, custom)
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
					loopInterruptInfo("⏸ finishing this iteration, then stopping — Ctrl-C again to stop now")
				},
				func() {
					loopInterruptInfo("■ stopping now")
					requestStop()
					cancel()
				})
		} else {
			signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
			go func() {
				if _, ok := <-sig; !ok {
					return
				}
				loopInterruptInfo("■ stop requested — tearing down this iteration's box, then exiting")
				requestStop()
				cancel()
			}()
		}
		defer func() { signal.Stop(sig); close(sig) }()
	}

	// Pre-flight: one best-effort housekeeping pass before working the queue. The built-in job —
	// return every blocked task whose decision.md now has a filled-in Resolution to todo — is
	// mechanical, so the HOST does it directly: no box, no model, no tokens, and the same bar as
	// `coop tasks unblock` (decisionResolved), so preflight and the CLI never disagree. It works
	// no task and deletes nothing: done tasks are pruned only by a human (`coop tasks rm
	// --all-done`), never by an agent. Opt-in (preflight.enabled / --preflight); skipped under a
	// custom work.command (not the agent's headless form).
	if preflight && len(custom) == 0 {
		if !preflightBuiltinRan {
			ui.Info("pre-flight: resolving answered blockers")
			if ids := tasks.UnblockResolved(hosts); len(ids) > 0 {
				ui.Info("pre-flight: unblocked %s — resolution filled in", strings.Join(ids, ", "))
			}
		}
		// An agent runs only for a CUSTOM cleanup (loop.yaml preflight.prompt) — extra instructions
		// that need judgment. Best-effort like the signoff pass — a failure never blocks work.
		if s := strings.TrimSpace(lc.Preflight.Prompt); s != "" {
			pfStart, pfHead := time.Now(), gitOut(repo, "rev-parse", "HEAD")
			pfCmd, streaming := iterCmd(agent, loopPreflightPrompt(repo, queues, s))
			pfCode, _, _, pfClassification, windows, runErr := c.runIteration(iterCtx, repo, img, agent, forkName, pfCmd, streaming, hosts, completionWindowReview, nil, false, sink, peers, "preflight", "")
			if errors.Is(runErr, tasks.ErrCompletionWindowSetup) {
				return 1, runErr
			}
			if _, err := windows.FinishReview(); err != nil {
				return 1, fmt.Errorf("pre-flight changed task completion ownership: %w", err)
			}
			c.recordStage(repo, runid, "preflight", pfClassification.outcome, rot.Active(), pfStart, pfCode, 0, 0, pfHead, hosts, nil, nil, nil)
			prev := rot.Active()
			if wait, until, limited := rememberPreflightLimit(rot, pfClassification, time.Now()); limited {
				if wait > 0 {
					ui.Info("all %d targets are rate limited after pre-flight — waiting for the soonest reset", rot.Len())
					sleepForLimit(wait, until, wake)
					rot.ClearExpired(time.Now())
				} else {
					ui.Info("pre-flight target %q rate limited — starting work on %q", prev, rot.Active())
				}
			}
		}
	}
	label := strings.Join(queues, ", ")
	c0, _ := tasks.QueueProgress(hosts)
	stopHint := "Ctrl-C to stop"
	if limit.enabled() {
		stopHint = fmt.Sprintf("at most %s, then pause", ui.Count(limit.max, "task"))
	} else if interactive {
		stopHint = "Ctrl-C to stop after this task, again to stop now"
	}
	if len(custom) == 0 {
		ui.Info("starting unattended loop on %s with %s — %d/%d done (%s)", label, agent, c0.Done, c0.Total(), stopHint)
	} else {
		ui.Info("starting unattended loop on %s — %d/%d done (%s)", label, c0.Done, c0.Total(), stopHint)
	}
	if rot.Rotates() {
		ui.Info("rotating %d targets on rate limit: %s", rot.Len(), strings.Join(rot.Members(), ", "))
	}
	// An in_progress task whose commit is already in history means a previous run died between the
	// commit and the folder move. Say so before working it: the resume recipe only stays safe while
	// that commit is HEAD, and left unnoticed these sat in the queue for days.
	for _, t := range tasks.AlreadyCommittedInProgress(repo, hosts) {
		ui.Warn("task %s is in progress but its commit %s is already in history (%s on top) — it may be finished; verify it and `coop tasks done %s`, or leave it to be resumed",
			t.ID, t.Commit, ui.Count(t.Depth, "commit"), t.ID)
	}
	fails, waits, retries, handoffs, timeouts, completed, stalls := 0, 0, 0, 0, 0, 0, 0
	completedThisRun := map[string]bool{}
	settledBaseline := c0.Done + c0.Blocked // "settled" = tasks out of the actionable set (done OR blocked)
	// A commit between iterations is progress too (see below), and every completion is validated
	// against a commit range — so an unreadable HEAD isn't a head value the loop can carry, it's a
	// repo the loop cannot bookkeep against. Stop before the first box starts.
	prevHead, headErr := gitOutErr(repo, "rev-parse", "HEAD")
	if headErr != nil {
		return 1, fmt.Errorf("read HEAD of %s: %w — the loop tracks progress by commit and binds each completion to a commit range, so it needs a repo with a readable HEAD (at least one commit); fix the repo, then re-run `coop loop`", repo, headErr)
	}
	loopStartHead := prevHead // for the end-of-run signing sweep (catches any straggler cycle)
	// The signoff reviews only what THIS RUN completed: anchoring to the pre-run done set keeps
	// 99_done/'s history (pruned only by a human) out of every round's subject list.
	reviewBaseline := reviewBaselineAfterVerdict(doneTaskDirs(hosts), nil, nil, recoveredReviewCompletions)
	if len(recoveredReviewCompletions) > 0 {
		ui.Info("recovered concurrent host completion during an interrupted review: %s — carrying it into signoff", strings.Join(recoveredReviewCompletions, ", "))
	}
	// Loop-until-accepted: drain the work queue, run the signoff pass, and if it reopened
	// anything, drain and sign off AGAIN — repeating until a signoff reopens nothing (accepted) or
	// the round cap is hit (block the stuck task for a human). The cap scales with the batch —
	// clamp(tasks worked/2, 3, signoff.rounds) — so a big overnight batch can't ping-pong one
	// stuck task forever while a tiny batch still gets a few tries (computed per round from the run's
	// completed count; the hard ceiling bounds it). A custom work.command has no signoff pass.
	// Final verify may jump back here when a parallel host completion needs its own signoff.
reviewAgain:
	for signoffRound := 1; ; signoffRound++ {
		for n := 1; ; {
			// A first Ctrl-C (soft stop) that arrived between iterations — or that woke a wait
			// below — stops here, before the next task is claimed; a second (hard) Ctrl-C that
			// canceled iterCtx during a between-tasks audit stops here too, before respawning a box.
			if softStop.Load() || iterCtx.Err() != nil {
				break
			}
			reached, limitErr := limit.observe(tasks.QueueSnapshot(hosts))
			if limitErr != nil {
				return 1, limitErr
			}
			if reached {
				break
			}
			// Point cfg at this iteration's target before leasing: the provider/target in metadata
			// identifies the owning controller, while flock remains the actual authority.
			agent = c.applyTarget(rot)
			target := rot.Active()
			// Select and host-claim one authoritative task before the box starts. The returned task
			// drives both the banner and prompt, so the model cannot guess a different "next" task.
			assignment, assignErr := tasks.AssignLoopTaskOnly(hosts, tasks.TaskLeaseOwner{
				RunID: c.runID, PID: os.Getpid(), Provider: agent, Target: target.String(),
			}, limit.scope())
			if assignErr != nil {
				return 1, assignErr
			}
			if assignment.Outcome == tasks.AssignmentUnavailable {
				// Foreign-held work is not a drained queue. Do not sign off a batch another live
				// controller still owns; its kernel lock will make the task adoptable on death.
				ui.Info("no task lease available — %s; stopping without signoff", assignment.Busy)
				return 0, nil
			}
			if assignment.Outcome == tasks.AssignmentDrained {
				if limit.scope() != "" {
					continue // the selected task settled between scans; observe and count its final state
				}
				break
			}
			counts, assigned, lease := assignment.Counts, assignment.Task, assignment.Lease
			limit.assign(assigned.Item.ID)
			if lease.Legacy {
				ui.Info("adopting unleased in-progress task %s", assigned.Item.ID)
			}
			// The active profile is shown on the model line (streamjson) — don't repeat it on the banner.
			active := assigned.Item.Title
			owner := " · owned by " + agent
			banner := progressBanner(n, counts, active)
			if ui.IsTerminal(os.Stderr) {
				banner = progressBannerWidth(n, counts, active, ui.TermWidth(os.Stderr)-1-len([]rune("coop: "+owner)))
			}
			ui.Info("%s%s", banner, owner)
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
			iterStart := time.Now()
			cmd, streaming := iterCmd(agent, iterWork)
			code, _, res, classification, windows, runErr := c.runIteration(iterCtx, repo, img, agent, forkName, cmd, streaming, hosts, completionWindowWork, []string{assigned.Item.ID}, false, sink, peers, active, assigned.Item.ID)
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
				if handoffs >= 3 {
					return code, fmt.Errorf("provider ended with live background work 3 times for task %s — stopped; inspect the task's restored state and run its gate, consult, and delegate work in the foreground", assigned.Item.ID)
				}
				ui.Warn("provider ended with live background work; restored %s and starting a fresh observed attempt (%d/3)", assigned.Item.ID, handoffs)
				continue
			}
			// The watchdog killed this attempt for proven silence. Any completion it produced is
			// premature: restore it, keep held audit authority truthful (rebase over a valid
			// complete rewrite, park fail-closed otherwise), release the lease, and retry under
			// the dedicated timeout policy — rotate to the next usable rung without cooling,
			// capped at three consecutive timeouts, no ordinary counter consumed.
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
					return code, fmt.Errorf("provider attempt timed out %d times in a row on task %s (%s)%s — stopped; the task remains actionable, inspect the provider and re-run `coop loop`", timeouts, assigned.Item.ID, classification.outcome, classification.timeoutDetail())
				}
				prev := rot.Active()
				rot.AdvanceOnTimeout(time.Now())
				if next := rot.Active(); next.String() != prev.String() {
					ui.Warn("provider attempt for %s timed out (%s)%s — switching to %q for a fresh attempt (%d/%d)", assigned.Item.ID, classification.outcome, classification.timeoutDetail(), next, timeouts, maxProviderTimeouts)
				} else {
					ui.Warn("provider attempt for %s timed out (%s)%s — starting a fresh attempt (%d/%d)", assigned.Item.ID, classification.outcome, classification.timeoutDetail(), timeouts, maxProviderTimeouts)
				}
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
			if assignedCompletion != nil {
				missing, tolerated = tasks.CompletionUnbindableTasks(repo, iterHead, headAfter, finished, lease.Reopen, touched)
			}
			tasks.ReportToleratedForeignBindings(repo, hosts, iterHead, headAfter, assigned.Item.ID, tolerated)
			if len(missing) > 0 {
				restoreErr = errors.Join(restoreErr, tasks.RestoreQueuedCompletion(*assignedCompletion, lease.Reopen != nil))
				var windowErr error
				if restoreErr != nil {
					windowErr = windows.Abandon()
				} else {
					windowErr = windows.Close()
				}
				releaseErr := errors.Join(lease.Release(), windowErr)
				refRelease()
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
				if receiptErr := lease.MarkCompleted(assignedCompletion.Item.Dir); receiptErr != nil {
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
			}
			gateHits := tasks.ProtectedGateChanges(repo, iterHead, headAfter)
			if len(gateHits) > 0 {
				ui.Warn("this iteration edited gate-defining file(s) %s — the review must confirm the gate wasn't weakened to pass", strings.Join(gateHits, ", "))
			}
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
					ui.Warn("could not sign this cycle's commits: %v — left unsigned", serr)
				} else if signed > 0 {
					ui.Info("signed %s with your host key", ui.Count(signed, "commit"))
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
						if protectedAudit && !betweenEnabled {
							ui.Info("protected-change audit — reviewing %s", strings.Join(finishedIDs, ", "))
						} else {
							ui.Info("between-tasks audit — reviewing %s", strings.Join(finishedIDs, ", "))
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
						btRun, rerr := c.runReviewVerdict(iterCtx, repo, img, betweenRot, forkName, prompt, reviewActivity(stage, finishedIDs), iterCmd, hosts, finishedIDs, lc.Between.Writes, sink, peers, hardStop, observe)
						reviewBaseline = reviewBaselineAfterVerdict(reviewBaseline, nil, nil, btRun.concurrent)
						reopenedIDs := btRun.reopened
						if errors.Is(rerr, errReviewInterrupted) {
							break
						}
						if errors.Is(rerr, tasks.ErrCompletionWindowSetup) || errors.Is(rerr, tasks.ErrCompletionWindowAudit) || errors.Is(rerr, errReviewVerdict) {
							return 1, rerr
						}
						if rerr != nil {
							ui.Warn("between audit could not run for %s: %v — left unaudited", strings.Join(finishedIDs, ", "), rerr)
						}
						interrupted := iterCtx.Err() != nil
						if verdictErr := protectedAuditVerdict(protectedAudit, interrupted, rerr, btRun.output, reopenedIDs, finishedIDs); verdictErr != nil {
							return 1, fmt.Errorf("protected-change audit for %s: %w — stopped before another task could trust the changed gate; inspect the task and re-run `coop loop`", strings.Join(finishedIDs, ", "), verdictErr)
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
				ui.Info("iteration failed — opening a debug shell in the box (exit it to retry; Ctrl-C to stop)")
				c.debugShell(repo, img, agent)
				fails = 0 // the developer intervened; don't count this toward the stop cap
				continue
			}
			switch action {
			case actContinue:
				completed++
				n++
				// A clean iteration that neither finishes/blocks a task NOR commits means the agent keeps
				// continuing an in_progress task it can't complete — advanceStall bails after maxStalls
				// rather than loop forever (a commit or a block still counts as progress).
				var stop error
				prevHead, settledBaseline, stalls, stop = c.advanceStall(repo, hosts, prevHead, settledBaseline, stalls, active)
				if stop != nil {
					return code, stop
				}
			case actWait:
				// A rate/usage limit is expected on long runs. With more than one profile in
				// the pool, switch to another subscription and retry immediately; otherwise wait
				// for the reset. Either way the same iteration is retried, not burned.
				if rot.Rotates() {
					// Advancing the rotation is the point — the loop head re-derives the agent
					// from rot (applyTarget), so the returned name would go unread here.
					c.rotateOnLimit(rot, resetAt, &waits, wake)
				} else {
					sleepForLimit(wait, resetAt, wake)
				}
			case actRetryNow:
				if wait > 0 {
					ui.Info("iteration reached model output limit (%d/%d) — resuming in %s", retries, maxOutputRetries, wait)
					ladder.SleepOrWake(wait, wake)
				} else {
					ui.Info("iteration reached model output limit — resuming immediately")
				}
			case actRetry:
				ui.Info("iteration failed (%d/%d) — retrying in 10s", fails, maxLoopFailures)
				ladder.SleepOrWake(10*time.Second, wake)
			case actStop:
				if waits > maxLimitWaits {
					return code, fmt.Errorf("still rate limited after %d waits — stopping", maxLimitWaits)
				}
				return code, fmt.Errorf("iteration failed %d times since the last success — stopping", fails)
			case actAuthStop:
				// A dead credential is no reason to abandon the queue while another account can still
				// work: mark this rung unusable for the run and switch, exactly as a rate limit does.
				// The mark is sticky, so this rotates at most once per rung and can't spin. Only when
				// EVERY rung has failed authentication is there nothing left to try.
				if rot.Rotates() && rot.OnAuthFailure() {
					ui.Warn("target %q authentication failed — switching to %q (restore it with `%s`)",
						target, rot.Active(), loginCommand(target))
					break
				}
				return code, rotationAuthenticationError(rot, target)
			case actOutputStop:
				return code, fmt.Errorf("iteration reached the model output limit %d times — stopping", retries)
			}
		}
		// A requested stop (soft: the current iteration finished; hard: it was torn down) skips the
		// signoff pass and the drain summary — the queue isn't done, the user asked to stop.
		if softStop.Load() || iterCtx.Err() != nil {
			cf, _ := tasks.QueueProgress(hosts)
			fmt.Fprintln(os.Stderr, loopInterruptedBanner(cf))
			return LoopInterruptedExitCode, nil
		}
		if limit.enabled() {
			cf, _ := tasks.QueueProgress(hosts)
			fmt.Fprintln(os.Stderr, loopTaskLimitBanner(cf, limit))
			if limit.settled == 0 {
				return loopExitCode(cf), nil
			}
			return 0, nil
		}
		// A custom work.command isn't the signoff-aware agent form, so it gets no signoff pass —
		// today's behavior: drain the queue, then report.
		if len(custom) > 0 {
			break
		}
		// Scale the cap to this run's batch (completed tasks), clamped to [3, signoff.rounds].
		maxSignoffRounds := signoffRoundCap(completed, signoffRounds(lc))
		// The round's subjects: what entered done/ since the last accepted round (for round 1, since
		// the run started) — a folder diff, so it also catches a completion with no commit. Nothing
		// new means nothing to review: skip the pass instead of burning a box on 99_done/'s history.
		subjects := newlyFinished(reviewBaseline, doneTaskDirs(hosts))
		if len(subjects) == 0 {
			ui.Info("signoff — nothing newly completed to review, skipping")
			break
		}
		ui.Info("queue empty — running signoff (round %d/%d)", signoffRound, maxSignoffRounds)
		// The signoff runs on signoff.agent's OWN target — a stronger, usually different-vendor model
		// reviews the work loop's output — and fails CLOSED: if it can't run after retries, stop loudly
		// rather than let "nothing reopened" read as an accepting signoff.
		// Hand the signoff the run's change context (per task, bound by the Coop-Task trailer) + health,
		// so a prompt like "e2e the affected features" resolves against a concrete list. Rebuilt each
		// round because the range (loopStartHead..HEAD) grows as reopened work lands.
		soHead := gitOut(repo, "rev-parse", "HEAD")
		cs := loopChanges(repo, loopStartHead, soHead)
		subjectIDs := taskIDsOf(subjects)
		signoff := loopSignoffPrompt(repo, queues, substituteLoopVars(lc.Signoff.Prompt, cs, health), subjects) + audits.signoffBlock(subjectIDs) + cs.reviewBlock(health)
		observe := func(run reviewRunResult, start time.Time, headBefore string) {
			c.recordStage(repo, runid, "signoff", run.outcome, run.target, start, run.exit, run.retries, len(run.reopened), headBefore, hosts, nil, nil, run.usage)
		}
		soRun, serr := c.runReviewVerdict(iterCtx, repo, img, signoffRot, forkName, signoff, reviewActivity("signoff", subjectIDs), iterCmd, hosts, subjectIDs, lc.Signoff.Writes, sink, peers, wake, observe)
		// Preserve the exact tasks the host reopened before any early return.
		reopenedIDs := soRun.reopened
		if errors.Is(serr, errReviewInterrupted) {
			cf, _ := tasks.QueueProgress(hosts)
			fmt.Fprintln(os.Stderr, loopInterruptedBanner(cf))
			return LoopInterruptedExitCode, nil
		}
		if serr != nil {
			return 1, serr
		}
		// A stop that landed during the signoff pass is honored before the next round is decided.
		if softStop.Load() || iterCtx.Err() != nil {
			cf, _ := tasks.QueueProgress(hosts)
			fmt.Fprintln(os.Stderr, loopInterruptedBanner(cf))
			return LoopInterruptedExitCode, nil
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
			if signoffRound >= maxSignoffRounds {
				return 3, fmt.Errorf("signoff verdict inconsistent after %d rounds: review reported %s but task delta was %s — verdicts may have been lost, a human should look", maxSignoffRounds, receiptClaim(receipt, ok), receiptIDs(reopenedIDs))
			}
			ui.Warn("signoff review inconsistent (reported %s, task delta %s) — re-running the round", receiptClaim(receipt, ok), receiptIDs(reopenedIDs))
			continue
		}
		audits.drop(reopenedIDs)
		// This round's verdict is consistent — advance the baseline past its accepted subjects
		// WITHOUT rescanning done/. A completion landing during or just after the review window stays
		// outside the baseline and enters the next round's subject diff. The lost-verdict path above
		// deliberately keeps the old baseline so the whole untrusted subject set is reviewed again.
		reviewBaseline = reviewBaselineAfterVerdict(reviewBaseline, subjects, reopenedIDs, soRun.concurrent)
		switch signoffRoundOutcome(signoffRound, maxSignoffRounds, len(reopenedIDs) > 0) {
		case signoffContinue:
			ui.Info("signoff reopened %s — draining again", ui.Count(len(reopenedIDs), "task"))
			continue
		case signoffAccepted:
			if pending := taskIDsOf(newlyFinished(reviewBaseline, doneTaskDirs(hosts))); len(pending) > 0 {
				ui.Info("signoff passed, but a parallel session completed %s during the round — running another signoff round to review it", ui.Count(len(pending), "task"))
				signoffRound = 0
				continue
			}
		case signoffCapReached:
			// The work loop couldn't get these tasks to a state the signoff accepts within the cap —
			// park them for a human rather than spin or claim a false "done" (exit 3 via loopExitCode).
			ui.Info("signoff still reopening after %d rounds — blocking %s for a human", maxSignoffRounds, ui.Count(len(reopenedIDs), "task"))
			if err := blockReopenedTasks(hosts, reopenedIDs, maxSignoffRounds); err != nil {
				return 3, err
			}
			if pending := taskIDsOf(newlyFinished(reviewBaseline, doneTaskDirs(hosts))); len(pending) > 0 {
				ui.Info("blocked the repeatedly reopened work; a parallel session also completed %s — running a fresh signoff round for it", ui.Count(len(pending), "task"))
				signoffRound = 0
				continue
			}
		}
		// signoffAccepted (nothing reopened or pending) or signoffCapReached (just blocked) → done.
		break
	}
	// Verify: an optional FINAL pass over the whole run's changes — its prompt (verify.prompt) says
	// what, typically "e2e-test the affected features". It runs after the signoff accepted the batch,
	// on its own model, with the run's change context injected; best-effort, and it may reopen a task
	// whose e2e it can't get to pass (surfaced in the closing digest + exit). Skipped on a custom
	// work.command or a requested stop. Ordinary process failures remain best-effort; completion
	// ownership setup/audit failures are hard boundaries and stop the loop.
	if verifyEnabled && !softStop.Load() && iterCtx.Err() == nil {
		cs := loopChanges(repo, loopStartHead, gitOut(repo, "rev-parse", "HEAD"))
		if cs.empty() {
			ui.Info("verify pass — nothing changed this run, skipping")
		} else {
			ui.Info("verify pass — e2e the affected features (%s)", strings.Join(cs.subsystems, ", "))
			vPrompt := substituteLoopVars(lc.Verify.Prompt, cs, health) + cs.reviewBlock(health) +
				"\n\n" + auditEvidencePrompt + "\n\n" + reviewContextFooter(repo, queues)
			verifyIDs := completedReviewSubjects(hosts, completedThisRun)
			verifyActivity := reviewActivity("verify", verifyIDs)
			if len(verifyIDs) == 0 {
				verifyActivity = "verify: unbound changes"
			}
			observe := func(run reviewRunResult, start time.Time, headBefore string) {
				c.recordStage(repo, runid, "verify", run.outcome, run.target, start, run.exit, run.retries, len(run.reopened), headBefore, hosts, nil, nil, run.usage)
			}
			vRun, verr := c.runReviewVerdict(iterCtx, repo, img, verifyRot, forkName, vPrompt, verifyActivity, iterCmd, hosts, verifyIDs, lc.Verify.Writes, sink, peers, wake, observe)
			reopenedIDs := vRun.reopened
			health.noteReopen(reopenedIDs)
			if errors.Is(verr, errReviewInterrupted) {
				cf, _ := tasks.QueueProgress(hosts)
				fmt.Fprintln(os.Stderr, loopInterruptedBanner(cf))
				return LoopInterruptedExitCode, nil
			}
			if errors.Is(verr, tasks.ErrCompletionWindowSetup) || errors.Is(verr, tasks.ErrCompletionWindowAudit) {
				return 1, verr
			}
			if errors.Is(verr, errReviewVerdictMalformed) {
				ui.Warn("verify verdict remained malformed after one receipt-format correction — no proposal was applied; the affected features remain unverified")
			} else if verr != nil {
				ui.Warn("verify pass could not run: %v — the affected features went un-e2e'd", verr)
			}
			reviewBaseline = reviewBaselineAfterVerdict(reviewBaseline, nil, nil, vRun.concurrent)
			if pending := taskIDsOf(newlyFinished(reviewBaseline, doneTaskDirs(hosts))); len(pending) > 0 {
				ui.Info("verify observed concurrent host completion of %s — returning to signoff before exit", strings.Join(pending, ", "))
				goto reviewAgain
			}
		}
	}
	// End-of-run signing sweep: normally a no-op (per-cycle signing already covered each iteration),
	// but it catches any straggler — a commit from a previously interrupted run, or a preflight
	// commit — so the whole run's range is signed before you push. Best-effort.
	if forkspace.WantsSigning() && len(custom) == 0 {
		if signed, serr := c.host.signUnpushed(repo, loopStartHead); serr != nil {
			ui.Warn("end-of-run signing sweep failed: %v — some commits may be unsigned (run `coop sign`)", serr)
		} else if signed > 0 {
			ui.Info("signed %s with your host key", ui.Count(signed, "commit"))
		}
	}
	cf, _ := tasks.QueueProgress(hosts)
	// A human-facing digest above the verdict banner: what shipped (per task + areas), what's blocked,
	// and any task the run flagged — so you see what to review/e2e at a glance.
	if len(custom) == 0 {
		cost := costFromRecords(readStageRecords(repo, runid), ReadPeerRecords(repo, runid))
		if digest := loopChanges(repo, loopStartHead, gitOut(repo, "rev-parse", "HEAD")).humanDigest(health, tasks.BlockedTaskIDs(hosts), cost); digest != "" {
			fmt.Fprintln(os.Stderr, digest)
		}
		// Done folders accumulate until a human prunes them (agents never delete) — and a big
		// 99_done/ taxes every future run: each iteration's box lists it, and it's the haystack a
		// crash-resume scan walks. Past a threshold, say so once, at close.
		if nudge := pruneNudge(cf.Done); nudge != "" {
			fmt.Fprintln(os.Stderr, nudge)
		}
	}
	fmt.Fprintln(os.Stderr, loopClosingBanner(cf, completed))
	return loopExitCode(cf), nil
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
	return fmt.Sprintf("  %s accumulated in 99_done/ — after you review and push, prune with 'coop tasks rm --all-done'",
		ui.Count(done, "done task folder"))
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
	after, _ := tasks.QueueProgress(hosts)
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
