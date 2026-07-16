package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/AndrewDryga/coop/internal/ui"
)

// The Coop-Task trailer binds a commit to the task it completes. The agent writes it (loopWorkPrompt
// instructs it); the HOST controller reads it to verify attempts, resume informed after a crash, and
// reconcile the parent queue after a fork merge — the LLM still moves folders, the controller only
// supplies evidence and repairs drift. Before this, nothing linked a commit to a task
// (git log --grep <id> was 0 repo-wide), so "one task = one commit" was unobservable and a crash
// between commit and folder-move was ambiguous.
const coopTaskTrailer = "Coop-Task"

// commitsForTask returns the short shas whose sole Coop-Task trailer equals id. rangeExpr limits
// the search (e.g. "base..HEAD"); empty scans all of HEAD's reachable history. Git joins duplicate
// values with the explicit unit separator, so a duplicate trailer fails closed instead of looking
// like a valid first line followed by unrelated output.
func commitsForTask(repo, rangeExpr, id string) []string {
	const trailerSep = "\x1f"
	args := []string{"log", "--format=%h%x09%(trailers:key=" + coopTaskTrailer + ",valueonly,separator=%x1f)"}
	if rangeExpr != "" {
		args = append(args, rangeExpr)
	}
	var shas []string
	for _, line := range strings.Split(gitOut(repo, args...), "\n") {
		sha, val, ok := strings.Cut(line, "\t")
		values := strings.Split(val, trailerSep)
		if ok && len(values) == 1 && strings.TrimSpace(values[0]) == id {
			shas = append(shas, sha)
		}
	}
	return shas
}

// gateGuardGlobs name the files that DEFINE what "green" means — the candidate's own verifier: the
// Makefile/gate, the loop + project config, the hooks, CI. A task that edits these could weaken the
// gate to pass itself (cross-vendor review is no defense when every reviewer trusts the same mutable
// oracle). A trailing "/" matches a directory prefix; else an exact base name.
var gateGuardGlobs = []string{
	"Makefile", "makefile", "GNUmakefile",
	".agent/project.yaml", ".agent/loop.yaml",
	".agent/skills/sweep/", "queue-guard.sh",
	".claude/hooks/", ".claude/settings.json", ".claude/settings.local.json",
	".github/workflows/",
}

// isGateGuardPath reports whether a repo-relative path is gate-defining (in gateGuardGlobs).
func isGateGuardPath(f string) bool {
	for _, g := range gateGuardGlobs {
		if strings.HasSuffix(g, "/") {
			if strings.HasPrefix(f, g) {
				return true
			}
		} else if f == g || strings.HasSuffix(f, "/"+g) {
			return true
		}
	}
	return false
}

// protectedGateFiles filters an arbitrary file list down to the deterministic, deduplicated set
// that defines the gate. It is shared by iteration detection and commit-bound review context, so
// the warning and both reviewers use the same trust boundary.
func protectedGateFiles(files []string) []string {
	seen := map[string]bool{}
	for _, f := range files {
		if f = strings.TrimSpace(f); f != "" && isGateGuardPath(f) {
			seen[f] = true
		}
	}
	hits := make([]string, 0, len(seen))
	for f := range seen {
		hits = append(hits, f)
	}
	slices.Sort(hits)
	return hits
}

// protectedGateChanges returns the gate-defining files a commit range (base..head) touched — the
// boring first step of the verifier trust boundary: detect (host-side, deterministic) when a task
// edited its own checker, so the review can be told to scrutinize it rather than trust it blind.
// Empty when the range is empty or touched none.
func protectedGateChanges(repo, base, head string) []string {
	if base == "" || head == "" || base == head {
		return nil
	}
	return protectedGateFiles(strings.Split(gitOut(repo, "diff", "--no-renames", "--name-only", "-z", base+".."+head), "\x00"))
}

// queueSnapshot maps task id → state across the hosts for UI and audit bookkeeping.
func queueSnapshot(hosts []string) map[string]string {
	m := map[string]string{}
	for _, h := range hosts {
		for _, t := range readTaskTree(h) {
			m[t.ID] = t.State
		}
	}
	return m
}

func aggregateDuplicateTaskIDs(hosts []string) []string {
	return taskIDDuplicates(hosts, false)
}

func nonArchivedDuplicateTaskIDs(hosts []string) []string {
	return taskIDDuplicates(hosts, true)
}

func taskIDDuplicates(hosts []string, requireLive bool) []string {
	counts := map[string]int{}
	live := map[string]bool{}
	for _, host := range hosts {
		for _, task := range readTaskTree(host) {
			counts[task.ID]++
			if task.State != stateDone {
				live[task.ID] = true
			}
		}
	}
	var duplicates []string
	for id, count := range counts {
		if count > 1 && (!requireLive || live[id]) {
			duplicates = append(duplicates, id)
		}
	}
	slices.Sort(duplicates)
	return duplicates
}

func completedAssignedTask(root, id string) (queuedTask, bool) {
	task, ok := currentTask(root, id)
	if !ok || task.State != stateDone {
		return queuedTask{}, false
	}
	return queuedTask{Root: root, Item: task}, true
}

// finalizeFinishedTasks handles the loop's in-box completion path: the worker moved the folder, so
// the host normalizes state.md and removes tmp before any reviewer consumes a done task. It sweeps
// archived done tasks, not active/crash-left lease candidates. A task whose metadata cannot be
// finalized moves back to in_progress before the error returns, so cleanup remains retryable.
func finalizeFinishedTasks(hosts []string) error {
	for _, host := range hosts {
		for _, t := range readTaskTree(host) {
			if t.State != stateDone || crashCompletionCandidate(host, t) {
				continue
			}
			if err := finalizeQueuedCompletion(queuedTask{Root: host, Item: t}); err != nil {
				return err
			}
		}
	}
	return nil
}

func finalizeQueuedCompletion(task queuedTask) error {
	if err := finalizeCompletedTask(task.Item.ID, task.Item.Dir); err != nil {
		if restoreErr := moveTaskDir(task.Root, task.Item, stateInProgress); restoreErr != nil {
			return errors.Join(err, fmt.Errorf("restore task %s after finalization failure: %w", task.Item.ID, restoreErr))
		}
		restored := filepath.Join(task.Root, stateInProgress, task.Item.ID)
		recoveryErr := normalizeTaskState(
			task.Item.ID,
			restored,
			"in progress — finalization failed",
			"fix the task metadata or cleanup obstruction, then re-run `coop loop`",
			"completion finalization failed",
			"the task must finalize safely before completion is accepted",
		)
		if recoveryErr != nil {
			recoveryErr = fmt.Errorf("refresh restored task %s: %w", task.Item.ID, recoveryErr)
		}
		return errors.Join(err, recoveryErr)
	}
	return nil
}

// reconcileInterruptedCompletions closes the crash window after a worker moved its task to done
// but before the controller validated the current iteration's commit binding. The lease does not
// retain that iteration base, so every candidate is restored for the normal range-bound resume
// path; an older matching commit must never validate new unbound work.
func reconcileInterruptedCompletions(hosts []string) error {
	var restoreErrs []error
	for _, host := range hosts {
		for _, task := range readTaskTree(host) {
			if task.State != stateDone || !crashCompletionCandidate(host, task) {
				continue
			}
			lock, current, acquired, err := lockCrashCompletion(host, task)
			if err != nil {
				restoreErrs = append(restoreErrs, fmt.Errorf("lock interrupted task %s: %w", task.ID, err))
				continue
			}
			if !acquired {
				continue
			}
			restoreErr := restoreQueuedCompletion(queuedTask{Root: host, Item: current})
			unlockErr := lock.release()
			if err := errors.Join(restoreErr, unlockErr); err != nil {
				restoreErrs = append(restoreErrs, err)
			}
		}
	}
	return errors.Join(restoreErrs...)
}

type crashCompletionLock struct {
	files []*os.File
}

func (l crashCompletionLock) release() error {
	var errs []error
	for i := len(l.files) - 1; i >= 0; i-- {
		errs = append(errs, unlockLeaseFile(l.files[i]))
	}
	return errors.Join(errs...)
}

func lockCrashCompletion(root string, task taskItem) (crashCompletionLock, taskItem, bool, error) {
	authority, err := openLeaseAuthority(root, task.ID, true)
	if err != nil {
		return crashCompletionLock{}, taskItem{}, false, err
	}
	locks := crashCompletionLock{files: []*os.File{authority}}
	if err := syscall.Flock(int(authority.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = authority.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return crashCompletionLock{}, taskItem{}, false, nil
		}
		return crashCompletionLock{}, taskItem{}, false, err
	}
	local, err := openLeaseLock(task.Dir, false)
	if err == nil {
		if err := syscall.Flock(int(local.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = local.Close()
			_ = locks.release()
			if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
				return crashCompletionLock{}, taskItem{}, false, nil
			}
			return crashCompletionLock{}, taskItem{}, false, err
		}
		locks.files = append(locks.files, local)
	} else if !errors.Is(err, os.ErrNotExist) {
		_ = locks.release()
		return crashCompletionLock{}, taskItem{}, false, err
	}
	current, ok := currentTask(root, task.ID)
	if !ok || current.State != stateDone || current.Dir != task.Dir {
		return crashCompletionLock{}, taskItem{}, false, locks.release()
	}
	return locks, current, true, nil
}

func unlockLeaseFile(file *os.File) error {
	return errors.Join(syscall.Flock(int(file.Fd()), syscall.LOCK_UN), file.Close())
}

func crashCompletionCandidate(root string, task taskItem) bool {
	if leaseAuthorityMetadataExists(root, task.ID) {
		return true
	}
	if info, err := os.Lstat(filepath.Join(task.Dir, "tmp")); err == nil &&
		(info.Mode()&os.ModeSymlink != 0 || !info.IsDir()) {
		return true
	}
	for _, name := range []string{"lease.lock", "lease.json"} {
		if info, err := os.Lstat(filepath.Join(task.Dir, "tmp", name)); err == nil && info.Mode().IsRegular() {
			return true
		}
	}
	return false
}

// blockedTaskIDs returns the ids currently parked in 50_blocked/ across the hosts — what needs a
// human decision, for the closing digest. Sorted.
func blockedTaskIDs(hosts []string) []string {
	var ids []string
	for id, st := range queueSnapshot(hosts) {
		if st == stateBlocked {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// reopenedBySignoff returns the ids the signoff bounced OUT of done/ (back to todo or in_progress)
// between two snapshots — what the review reopened this round, for the health digest. Sorted.
func reopenedBySignoff(before, after map[string]string) []string {
	var ids []string
	for id, st := range after {
		if before[id] == stateDone && st != stateDone {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// untrailered returns finished ids without exactly one Coop-Task binding in this iteration's range.
// A no-HEAD-change completion always fails closed; crash recovery restores it for a fresh range.
func untrailered(repo, base, head string, finished []string) []string {
	if base == "" || head == "" || base == head {
		return slices.Clone(finished)
	}
	search := base + ".." + head
	var missing []string
	for _, id := range finished {
		if len(commitsForTask(repo, search, id)) != 1 {
			missing = append(missing, id)
		}
	}
	return missing
}

func unbindableQueuedCompletion(repo, base, head string, completed queuedTask) bool {
	return len(untrailered(repo, base, head, []string{completed.Item.ID})) != 0
}

// restoreQueuedCompletions moves exact rejected completions back to in_progress and records why.
// Retaining the queue root is required because duplicate ids across configured queues are valid.
func restoreQueuedCompletions(tasks []queuedTask) error {
	var restoreErrs []error
	for _, task := range tasks {
		if task.Item.State != stateDone && task.Item.State != stateInProgress {
			restoreErrs = append(restoreErrs, fmt.Errorf("restore task %s: task is not in done or in_progress", task.Item.ID))
			continue
		}
		if err := restoreQueuedCompletion(task); err != nil {
			restoreErrs = append(restoreErrs, err)
		}
	}
	return errors.Join(restoreErrs...)
}

func restoreQueuedCompletion(task queuedTask) error {
	id := task.Item.ID
	if task.Item.State == stateDone {
		if err := moveTaskDir(task.Root, task.Item, stateInProgress); err != nil {
			return fmt.Errorf("restore task %s: %w", id, err)
		}
	}
	dir := filepath.Join(task.Root, stateInProgress, id)
	note := fmt.Sprintf("completion rejected: expected exactly one commit with one matching %s trailer in the iteration's range; if missing, add it with `git commit --amend --no-edit --trailer %q`; if the matching trailer already exists outside the new range, amend that commit with a unique `Coop-Recovery: <current UTC timestamp>` trailer while preserving exactly one %s trailer; if duplicated, rewrite or squash the range down to one binding; then re-run `coop loop`", coopTaskTrailer, coopTaskTrailer+": "+id, coopTaskTrailer)
	var errs []error
	if err := appendTaskLogStrict(dir, note); err != nil {
		errs = append(errs, fmt.Errorf("record rejection for task %s: %w", id, err))
	}
	if err := normalizeRejectedTaskState(id, dir); err != nil {
		errs = append(errs, fmt.Errorf("refresh rejected task %s: %w", id, err))
	}
	return errors.Join(errs...)
}

func normalizeRejectedTaskState(id, taskDir string) error {
	return normalizeTaskState(
		id,
		taskDir,
		"in progress — completion rejected",
		"repair the commit binding, then re-run `coop loop`",
		"completion was rejected as unbindable",
		"the task needs exactly one matching Coop-Task trailer",
	)
}

func unbindableCompletionError(ids []string, restoreErr error) error {
	commands := make([]string, 0, len(ids))
	for _, id := range ids {
		commands = append(commands, fmt.Sprintf("git commit --amend --no-edit --trailer %q", coopTaskTrailer+": "+id))
	}
	msg := fmt.Sprintf("completion rejected for task(s) %s: the new commit range needs exactly one commit with one parseable `%s: <id>` trailer per task; task(s) restored to in_progress — add a missing trailer (%s), add a unique `Coop-Recovery: <current UTC timestamp>` trailer when amending an already-bound crash-left commit, or rewrite/squash duplicate bindings down to one, then re-run `coop loop`", strings.Join(ids, ", "), coopTaskTrailer, strings.Join(commands, "; "))
	if restoreErr != nil {
		return fmt.Errorf("%s; recovery bookkeeping also failed: %w", msg, restoreErr)
	}
	return errors.New(msg)
}

// resumeLine is the informed-resume hint for an in_progress task that ALREADY has a commit carrying
// its Coop-Task trailer in history. Empty when there's none (a genuinely mid-work task — the
// blind-resume path stays byte-identical). It names the fact but doesn't assume the case, because a
// landed trailer means EITHER a crash after commit before the folder-move OR a review reopen for
// rework — so it tells the agent to disambiguate from the task's own log.md/state.md.
func resumeLine(id string, commits []string) string {
	if len(commits) == 0 {
		return ""
	}
	return "Task " + id + " has commit(s) " + strings.Join(commits, ", ") + " already in history carrying " +
		"its Coop-Task trailer. Read its log.md/state.md and determine which case applies: (a) a prior " +
		"attempt COMMITTED then was interrupted before moving the folder to 99_done/ — verify that work " +
		"against the acceptance criteria, amend the commit with a unique `Coop-Recovery: <current UTC timestamp>` " +
		"trailer while preserving exactly one Coop-Task trailer, and finish the move, but do NOT redo it; or (b) the review REOPENED it " +
		"for rework (its log.md will say what's wrong) — do that rework. Disambiguate before acting."
}

// resumePrefixFor builds the informed-resume preamble for the assigned task when its Coop-Task
// trailer is already in history. Empty when none, so a fresh claim keeps the ordinary prompt.
func (a *app) resumePrefixFor(repo, id string) string {
	return resumeLine(id, commitsForTask(repo, "", id))
}

type taskAssignmentOutcome uint8

const (
	assignmentDrained taskAssignmentOutcome = iota
	assignmentUnavailable
	assignmentReady
)

type taskAssignment struct {
	Counts  taskCounts
	Task    queuedTask
	Lease   *taskLease
	Outcome taskAssignmentOutcome
	Busy    taskLeaseSummary
}

const maxLeaseRescans = 3

// assignLoopTask scans in stable queue/id order and atomically leases exactly one task before the
// box starts. An available in-progress task remains preferred, but a foreign-held one is skipped so
// another controller can take independent todo work. The flock is obtained while a todo folder is
// still in todo, then rides its atomic rename to in_progress by inode.
func assignLoopTask(hosts []string, owner taskLeaseOwner) (taskAssignment, error) {
	return assignLoopTaskOnly(hosts, owner, "")
}

// assignLoopTaskOnly scopes assignment to the current task in a limited run. Counts still cover the
// whole queue for truthful banners, but another actionable task can never be claimed while the
// selected task is retrying or has been reopened by its between-task audit.
func assignLoopTaskOnly(hosts []string, owner taskLeaseOwner, onlyID string) (taskAssignment, error) {
	for attempt := 0; attempt < maxLeaseRescans; attempt++ {
		var counts taskCounts
		var inProgress, todo []queuedTask
		for _, root := range hosts {
			for _, item := range readTaskTree(root) {
				switch item.State {
				case stateTodo:
					counts.Todo++
					if onlyID == "" || item.ID == onlyID {
						todo = append(todo, queuedTask{Root: root, Item: item})
					}
				case stateInProgress:
					counts.Doing++
					if onlyID == "" || item.ID == onlyID {
						inProgress = append(inProgress, queuedTask{Root: root, Item: item})
					}
				case stateBlocked:
					counts.Blocked++
				case stateDone:
					counts.Done++
				}
			}
		}

		var busy taskLeaseSummary
		changed := false
		for _, candidate := range inProgress {
			lease, observed, err := tryTaskLease(candidate.Root, candidate.Item, owner)
			if errors.Is(err, errLeaseCandidateGone) {
				changed = true
				break
			}
			if err != nil {
				return taskAssignment{}, fmt.Errorf("lease task %s: %w", candidate.Item.ID, err)
			}
			if lease == nil {
				busy.add(observed)
				continue
			}
			return taskAssignment{
				Counts: counts, Task: candidate, Lease: lease, Outcome: assignmentReady, Busy: busy,
			}, nil
		}
		if changed {
			continue
		}

		for _, candidate := range todo {
			lease, observed, err := tryTaskLease(candidate.Root, candidate.Item, owner)
			if errors.Is(err, errLeaseCandidateGone) {
				changed = true
				break
			}
			if err != nil {
				return taskAssignment{}, fmt.Errorf("lease task %s: %w", candidate.Item.ID, err)
			}
			if lease == nil {
				busy.add(observed)
				continue
			}
			if err := moveTaskDir(candidate.Root, candidate.Item, stateInProgress); err != nil {
				_ = lease.release()
				if strings.Contains(err.Error(), "changed state under us") {
					changed = true
					break
				}
				return taskAssignment{}, fmt.Errorf("claim task %s: %w", candidate.Item.ID, err)
			}
			candidate.Item.State = stateInProgress
			candidate.Item.Dir = filepath.Join(candidate.Root, stateInProgress, candidate.Item.ID)
			counts.Todo--
			counts.Doing++
			return taskAssignment{
				Counts: counts, Task: candidate, Lease: lease, Outcome: assignmentReady, Busy: busy,
			}, nil
		}
		if changed {
			continue
		}
		if onlyID != "" && len(inProgress)+len(todo) == 0 {
			return taskAssignment{Counts: counts, Outcome: assignmentDrained}, nil
		}
		if counts.Todo+counts.Doing == 0 {
			return taskAssignment{Counts: counts, Outcome: assignmentDrained}, nil
		}
		return taskAssignment{Counts: counts, Outcome: assignmentUnavailable, Busy: busy}, nil
	}
	return taskAssignment{}, fmt.Errorf("task queue kept changing while leasing — retry the loop")
}

// reconcileAction is what post-merge reconciliation should do with one parent-queue task after a
// fork landed: move a trailer-landed todo/in_progress task to done, or FLAG (never auto-move) a
// blocked one.
type reconcileAction struct {
	ID   string
	Move bool // true → move to done/; false → flag for a human (blocked/ tasks)
}

// reconcileMerged decides, for each parent-queue task whose Coop-Task trailer now appears in
// parent history (landed by the merge), what to do: a todo/ or in_progress/ task is reconciled to
// done/ (redoing landed work is the worse failure — it already passed the fork's own review and the
// merge gate); a blocked/ task is only flagged, never moved, since a human parked it. Pure: it maps
// (task states, the set of landed ids) to actions.
func reconcileMerged(states map[string]string, landed map[string]bool) []reconcileAction {
	var acts []reconcileAction
	for id, st := range states {
		if !landed[id] {
			continue
		}
		switch st {
		case stateTodo, stateInProgress:
			acts = append(acts, reconcileAction{ID: id, Move: true})
		case stateBlocked:
			acts = append(acts, reconcileAction{ID: id, Move: false})
		}
	}
	slices.SortFunc(acts, func(a, b reconcileAction) int { return strings.Compare(a.ID, b.ID) })
	return acts
}

// landedTasks is the set of task ids whose Coop-Task trailer appears anywhere in repo's history.
func landedTasks(repo string) map[string]bool {
	set := map[string]bool{}
	const separator = "\x1f"
	format := "%(trailers:key=" + coopTaskTrailer + ",valueonly,separator=%x1f)"
	for _, line := range strings.Split(gitOut(repo, "log", "--format="+format), "\n") {
		values := strings.Split(line, separator)
		if len(values) == 1 {
			if value := strings.TrimSpace(values[0]); value != "" {
				set[value] = true
			}
		}
	}
	return set
}

// reconcileQueueAfterMerge moves any parent-queue task whose Coop-Task trailer now sits in parent
// history (landed by the just-merged fork) from todo/ or in_progress/ to done/, with a reconcile
// note; a blocked task with a landed trailer is flagged for a human, never moved. Best-effort — the
// merge already succeeded, so a reconcile hiccup must not fail it. Prevents the parent loop from
// redoing work a fork already landed.
func (a *app) reconcileQueueAfterMerge(repo, forkName string) {
	queues, err := taskQueues(a.cfg, repo, nil)
	if err != nil {
		return
	}
	landed := landedTasks(repo)
	hosts := make([]string, len(queues))
	for i, queue := range queues {
		hosts[i] = filepath.Join(repo, queue)
	}
	for _, id := range aggregateDuplicateTaskIDs(hosts) {
		delete(landed, id)
		ui.Warn("reconcile: task id %s exists in multiple queues; skipped automatic fork reconciliation", id)
	}
	for _, q := range queues {
		host := filepath.Join(repo, q)
		states := map[string]string{}
		items := map[string]taskItem{}
		for _, t := range readTaskTree(host) {
			states[t.ID] = t.State
			items[t.ID] = t
		}
		for _, act := range reconcileMerged(states, landed) {
			if !act.Move {
				ui.Warn("task %s is blocked but its work landed via fork %s — a human should reconcile it", act.ID, forkName)
				continue
			}
			if err := moveTaskDir(host, items[act.ID], stateDone); err != nil {
				ui.Warn("reconcile: could not move %s to done: %v", act.ID, err)
				continue
			}
			doneDir := filepath.Join(host, stateDone, act.ID)
			if err := finalizeCompletedTask(act.ID, doneDir); err != nil {
				ui.Warn("reconcile: %v — fix the obstruction, then retry: coop tasks done %s", err, act.ID)
				continue
			}
			appendTaskLog(doneDir, "reconciled: landed by fork "+forkName)
		}
	}
}

// unblockResolved is the loop's built-in preflight, run host-side (no box, no model): every
// blocked task whose decision.md now carries a filled-in Resolution — the same bar
// `coop tasks unblock` applies (decisionResolved) — moves back to 00_todo/ with a log note.
// A task with no decision.md, or one whose format decisionResolved can't read, stays parked:
// never act on a file we can't parse confidently. Best-effort; a move failure warns and skips.
// Returns the unblocked ids in readTaskTree order.
func unblockResolved(hosts []string) []string {
	var ids []string
	for _, host := range hosts {
		for _, t := range readTaskTree(host) {
			if t.State != stateBlocked || !decisionResolved(filepath.Join(t.Dir, "decision.md")) {
				continue
			}
			if err := moveTaskDir(host, t, stateTodo); err != nil {
				ui.Warn("pre-flight: could not unblock %s: %v", t.ID, err)
				continue
			}
			appendTaskLog(filepath.Join(host, stateTodo, t.ID), "preflight: resolution filled in — unblocked")
			ids = append(ids, t.ID)
		}
	}
	return ids
}

func appendTaskLogStrict(taskDir, note string) error {
	root, err := openTaskMetadataRoot(taskDir)
	if err != nil {
		return err
	}
	defer root.Close()
	before, statErr := root.Lstat("log.md")
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if statErr == nil {
		if err := validateTaskMetadataFile("log.md", before); err != nil {
			return err
		}
	}
	f, err := root.OpenFile("log.md", os.O_APPEND|os.O_CREATE|os.O_WRONLY|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return err
	}
	after, err := f.Stat()
	if err != nil || (statErr == nil && !os.SameFile(before, after)) {
		_ = f.Close()
		if err != nil {
			return err
		}
		return errors.New("task log changed while opening")
	}
	if err := validateTaskMetadataFile("log.md", after); err != nil {
		_ = f.Close()
		return err
	}
	line := "\n- " + note + "\n"
	if after.Size()+int64(len(line)) > taskMetadataFileLimit {
		_ = f.Close()
		return fmt.Errorf("task metadata file %q exceeds %d bytes", "log.md", taskMetadataFileLimit)
	}
	if _, err := f.WriteString(line); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// appendTaskLog appends a one-line note to a task folder's log.md, best-effort.
func appendTaskLog(taskDir, note string) {
	_ = appendTaskLogStrict(taskDir, note)
}
