// Package tasks is coop's task-authority engine: the folder-mode task/backlog queue (a task's
// lifecycle state IS the directory it sits in), the durable claim + kernel-flock lease + ref
// authorities that decide who may act on a task and its checkout right now, and the trusted
// completion audit that a host applies to a box's finished work before trusting it. Command-line
// wiring, loop orchestration and fork landing remain in their owning packages.
//
// The full four-authority map — claim, lease, checkout (owned by internal/loop), ref — with their
// mechanisms, hold times, and workflow-specific lock nesting lives in
// .agent/kb/task-authority-model.md; read that first, this
// comment does not repeat it.
package tasks

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/hostsurface"
	"github.com/AndrewDryga/coop/internal/ui"
)

// The Coop-Task trailer binds a commit to the task it completes. The agent writes it
// (loop.LoopWorkPrompt instructs it); the HOST controller reads it to verify attempts, resume informed
// after a crash, and bind reviewed fork candidates to exact assigned tasks. A trailer alone never
// authorizes queue completion. Without it nothing links a commit to a task, so "one task = one commit"
// is unobservable and a crash between commit and folder-move is ambiguous.
const (
	CoopTaskTrailer             = "Coop-Task"
	auditReopenHistoryLimit     = 4096
	auditReachableHistoryLimit  = 100000
	auditReachableEdgeLimit     = 200000
	auditReachableByteLimit     = 128 << 20
	auditLinearHistoryByteLimit = 128 << 20
	auditReopenCommitSizeLimit  = 1 << 20
	auditTaskTrailerValueLimit  = 4096
	auditTreeObjectSizeLimit    = 4 << 20
	auditTreeObjectCountLimit   = 100000
	auditTreeEntryLimit         = 1000000
	auditTreeByteLimit          = 64 << 20
	auditHistoryOutputLimit     = 20 << 20
	auditDiffOutputLimit        = 64 << 20
	auditMetadataOutputLimit    = 2 << 20
)

// CommitInfo is a commit's short sha and subject line — the same shape internal/loop's own
// commitInfo mirrors rather than shares; see its parseLoopCommits for the one-site conversion between
// the two.
type CommitInfo struct{ SHA, Subject string }

type TaskTrailerCommit struct {
	Info          CommitInfo
	fullSHA       string
	authorName    string
	authorEmail   string
	authorDate    string
	commitMessage string
	Values        []string
	Malformed     bool
}

// TaskTrailerCommits uses one NUL-delimited Git stream, so a trailer value can never be confused
// with the next commit record without paying one process launch per commit. Git's trailer parser
// identifies the final trailer block; the explicit inner separator preserves empty and duplicate
// Coop-Task occurrences so callers can fail closed. It returns an error rather than an ok-bool so a
// caller that REPORTS the failure (rather than just failing closed) has a cause to hand the human.
func TaskTrailerCommits(repo, rangeExpr string, reverse bool) ([]TaskTrailerCommit, error) {
	args := []string{"log"}
	if reverse {
		args = append(args, "--reverse")
	}
	format := "%h%x00%s%x00%(trailers:key=" + CoopTaskTrailer + ",only,unfold,separator=%x1f)"
	args = append(args, "-z", "--format="+format)
	if rangeExpr != "" {
		args = append(args, rangeExpr)
	}
	cmd, err := forkspace.GitCommand(context.Background(), repo, args...)
	if err != nil {
		return nil, err
	}
	raw, err := auditCommandOutput(cmd, auditHistoryOutputLimit)
	if err != nil {
		return nil, fmt.Errorf("git log: %w", err) // git's own diagnostic went to stderr as it ran
	}
	fields := strings.Split(string(raw), "\x00")
	if len(fields) == 0 || fields[len(fields)-1] != "" || (len(fields)-1)%3 != 0 {
		return nil, errors.New("git log returned a truncated commit stream")
	}
	commits := make([]TaskTrailerCommit, 0, (len(fields)-1)/3)
	for i := 0; i < len(fields)-1; i += 3 {
		record := TaskTrailerCommit{Info: CommitInfo{SHA: fields[i], Subject: fields[i+1]}}
		if fields[i+2] != "" {
			for _, trailer := range strings.Split(fields[i+2], "\x1f") {
				key, value, ok := strings.Cut(trailer, ":")
				if !ok || !strings.EqualFold(strings.TrimSpace(key), CoopTaskTrailer) {
					record.Malformed = true
					continue
				}
				record.Values = append(record.Values, strings.TrimSpace(value))
			}
		}
		commits = append(commits, record)
	}
	return commits, nil
}

func auditCommandOutput(cmd *exec.Cmd, limit int64) ([]byte, error) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(stdout, limit+1))
	overflow := int64(len(raw)) > limit
	if overflow || readErr != nil {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}
	waitErr := cmd.Wait()
	if overflow {
		return nil, fmt.Errorf("command output exceeds %d bytes", limit)
	}
	if readErr != nil {
		return nil, readErr
	}
	if waitErr != nil {
		return nil, waitErr
	}
	return raw, nil
}

// CommitsForTask returns the short shas whose sole Coop-Task trailer equals id. rangeExpr limits
// the search (e.g. "base..HEAD"); empty scans all of HEAD's reachable history.
func CommitsForTask(repo, rangeExpr, id string) []string {
	shas, _ := TaskCommits(repo, rangeExpr, id)
	return shas
}

// TaskCommits is CommitsForTask for a caller that must tell a task with no commit from a history it
// could not read.
func TaskCommits(repo, rangeExpr, id string) ([]string, error) {
	var shas []string
	commits, err := TaskTrailerCommits(repo, rangeExpr, false)
	if err != nil {
		return nil, err
	}
	for _, commit := range commits {
		if !commit.Malformed && len(commit.Values) == 1 && commit.Values[0] == id {
			shas = append(shas, commit.Info.SHA)
		}
	}
	return shas, nil
}

func taskBindingCounts(repo, head string) (map[string]int, bool) {
	bindings, ok := rawTaskBindings(repo, head)
	if !ok {
		return nil, false
	}
	counts := map[string]int{}
	for id, shas := range bindings {
		counts[id] = len(shas)
	}
	return counts, true
}

// gateGuardGlobs name the files that DEFINE what "green" means — the candidate's own verifier: the
// Makefile/gate, the loop + project config, the hooks, CI. A task that edits these could weaken the
// gate to pass itself (cross-vendor review is no defense when every reviewer trusts the same mutable
// oracle). A trailing "/" matches a directory prefix; else an exact base name. They are written as
// hostsurface.Key spells them: lower case.
var gateGuardGlobs = []string{
	"makefile", "gnumakefile",
	".agent/project.yaml", ".agent/loop.yaml",
	".agent/skills/sweep/", "queue-guard.sh",
	".claude/hooks/", ".claude/settings.json", ".claude/settings.local.json",
	".github/workflows/",
}

// isGateGuardPath reports whether a repo-relative path is gate-defining (in gateGuardGlobs). It
// judges the path's hostsurface.Key, so every spelling counts: on a Mac, MAKEFILE is the Makefile
// the gate runs.
func isGateGuardPath(f string) bool {
	f = hostsurface.Key(f)
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

// ProtectedGateFiles filters an arbitrary file list down to the deterministic, deduplicated set
// that defines the gate. Project sources are exact paths from the trusted startup config; built-in
// guards remain immutable. Iteration detection and review context pass the same list.
func ProtectedGateFiles(files, projectSources []string) []string {
	declared := make(map[string]bool, len(projectSources))
	for _, path := range projectSources {
		declared[hostsurface.Key(path)] = true
	}
	seen := map[string]bool{}
	for _, f := range files {
		if f = strings.TrimSpace(f); f != "" && (isGateGuardPath(f) || declared[hostsurface.Key(f)]) {
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

// ProtectedGateChanges returns the gate-defining files a commit range (base..head) touched — the
// boring first step of the verifier trust boundary: detect (host-side, deterministic) when a task
// edited its own checker, so the review can be told to scrutinize it rather than trust it blind.
// Empty when the range is empty or touched none. An error means the files could not be listed: a
// box can break the objects or refs the read needs, so a caller treats the range as a possible
// gate change, never as one that touched none.
func ProtectedGateChanges(repo, base, head string, projectSources []string) ([]string, error) {
	if base == "" || head == "" {
		return nil, fmt.Errorf("list the files changed in %q..%q: the range has no commit at one end", base, head)
	}
	if base == head {
		return nil, nil
	}
	out, err := forkspace.GitOutput(repo, "diff", "--no-renames", "--name-only", "-z", base+".."+head)
	if err != nil {
		return nil, err
	}
	return ProtectedGateFiles(strings.Split(out, "\x00"), projectSources), nil
}

// QueueSnapshot maps task id → state across the hosts for UI and audit bookkeeping.
func QueueSnapshot(hosts []string) (map[string]string, error) {
	m := map[string]string{}
	for _, h := range hosts {
		items, err := ReadTaskTree(h)
		if err != nil {
			return nil, err
		}
		for _, t := range items {
			m[t.ID] = t.State
		}
	}
	return m, nil
}

func aggregateDuplicateTaskIDs(hosts []string) ([]string, error) {
	return taskIDDuplicates(hosts, false)
}

func NonArchivedDuplicateTaskIDs(hosts []string) ([]string, error) {
	return taskIDDuplicates(hosts, true)
}

func taskIDDuplicates(hosts []string, requireLive bool) ([]string, error) {
	counts := map[string]int{}
	live := map[string]bool{}
	for _, host := range hosts {
		items, err := ReadTaskTree(host)
		if err != nil {
			return nil, err
		}
		for _, task := range items {
			counts[task.ID]++
			if task.State != StateDone {
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
	return duplicates, nil
}

// auditHistory reads the repository and HEAD commit the audit checks a reopen record against, from
// the project folder the caller resolved — the same history the record was captured from. Read from
// a queue folder instead (a member's .agent/tasks, a --tasks queue), git searches upward for the
// repository, and a trusted git view refuses a search that reaches the home folder, as it does when
// the home folder itself is the project (COOP_REPO=~). No project means no history: the audit
// refuses rather than let `git -C ""` read whatever repository holds the working directory.
func auditHistory(project string) (repo, head string) {
	if project == "" {
		return "", ""
	}
	return forkspace.GitOut(project, "rev-parse", "--show-toplevel"), forkspace.GitOut(project, "rev-parse", "--verify", "HEAD^{commit}")
}

// CompleteTrustedTask is the host equivalent of the provider completion boundary. It holds the
// task authority lock across move, finalization, and receipt creation so a concurrent loop can
// observe either the old state or an accepted done inode, never a transient. project is the
// project folder whose history a reopened task's audit reads. It always requires a finished
// checklist; only a person's `coop tasks done --force` waives that, through completeTrustedTask.
func CompleteTrustedTask(project, root string, task Item) error {
	_, err := completeTrustedTask(project, root, task, completionOptions{})
	return err
}

// completionOptions are a person's choices at `coop tasks done`. The zero value is the strict
// completion every agent path uses.
type completionOptions struct {
	force bool       // close even though the checklist is empty or still open
	actor ClaimActor // who ran it, named in the log note a forced close leaves
	// refuse, when set, judges the task's owner record while the lease authority is held: a box's
	// tools refuse a task someone else holds (CompleteBoxTask).
	refuse func(TaskOwnerRecord) error
	// landed, when set, takes the task from the fork assignment that holds it, whose commit already
	// reached the project's branch (`coop tasks done --from-fork`), instead of refusing it.
	landed *landedForkRelease
}

// errFinalReviewRunning refuses a forced close while a loop's final review could reopen the task.
var errFinalReviewRunning = errors.New("a loop's final review is running")

func finalReviewRunning(root string) (bool, error) {
	index, err := ReadCompletionWindowIndex(root)
	if err != nil {
		return false, err
	}
	for id, window := range index.Windows {
		if !window.ReviewWindow {
			continue
		}
		// The window's lock is what says its owner is alive: a killed loop's record stays behind.
		live, err := lockLeaseAuthority(root, completionWindowLockPrefix+id, false, syscall.LOCK_SH|syscall.LOCK_NB)
		switch {
		case errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN):
			return true, nil
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return false, err
		default:
			if err := unlockLeaseFile(live); err != nil {
				return false, err
			}
		}
	}
	return false, nil
}

func completeTrustedTask(project, root string, task Item, opts completionOptions) (_ *IncompleteChecklistError, retErr error) {
	windows, err := BeginCompletionWindows([]string{root})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCompletionWindowSetup, err)
	}
	accepted := false
	var acceptedTask QueuedTask
	defer func() {
		if accepted {
			retErr = errors.Join(retErr, windows.rejectAndClose(acceptedTask))
		} else {
			retErr = errors.Join(retErr, windows.Abandon())
		}
	}()
	authority, err := lockLeaseAuthority(root, task.ID, true, syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("task %s is %w", task.ID, ErrTaskLeased)
		}
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, unlockLeaseFile(authority)) }()
	// A projected completion is only candidate-ready. Ordinary host completion must
	// refuse it before moving or normalizing the canonical folder; exact fork landing owns that
	// transition through its generation-bound journal, and a person's --from-fork takes it from the
	// exact assignment its caller checked.
	if opts.landed != nil {
		if err := opts.landed.requireOwner(root, task.ID); err != nil {
			return nil, err
		}
	} else if err := refuseForkTaskOwner(root, task.ID, "complete"); err != nil {
		return nil, err
	}
	if opts.refuse != nil {
		if record, owned, err := ReadTaskOwnerRecord(root, task.ID); err != nil {
			return nil, err
		} else if owned {
			if err := opts.refuse(record); err != nil {
				return nil, err
			}
		}
	}
	current, ok, err := CurrentTask(root, task.ID)
	if err != nil {
		return nil, err
	}
	if !ok || current.Dir != task.Dir || current.State != task.State {
		return nil, errLeaseCandidateGone
	}
	// --force waives this one check, for a person only. Every refusal above and below still applies,
	// and without --force an accepted task whose checklist reopened still refuses, receipt intact.
	var waived *IncompleteChecklistError
	if err := RequireCompletedChecklist(current); err != nil && (!opts.force || !errors.As(err, &waived)) {
		return nil, err
	}
	// A done folder that already holds its receipt was closed before; forcing it again finishes it
	// without recording a second forced close.
	alreadyAccepted := false
	if current.State == StateDone {
		_, alreadyAccepted = readLeaseCompletionReceipt(authority, current.Dir)
	}
	if !alreadyAccepted {
		if err := refuseExtraBindings(project, current.ID); err != nil {
			return nil, err
		}
	}
	// A loop's final review running now would enroll this close, and its reviewer reopens a task with
	// skipped items, silently undoing the person's decision. So the person waits for it to end.
	if waived != nil && !alreadyAccepted {
		running, err := finalReviewRunning(root)
		if err != nil {
			return nil, fmt.Errorf("inspect active final-review windows: %w", err)
		}
		if running {
			return nil, errFinalReviewRunning
		}
	}
	reopen, reopened, err := ReadAuditReopenRecord(root, task.ID)
	if err != nil {
		return nil, err
	}
	if reopened {
		repo, head := auditHistory(project)
		if reopen.UnblockPending {
			return nil, &auditCompletionStateError{message: fmt.Sprintf(
				"task %s has non-authorizing pending audit authority from an interrupted unblock; "+
					"run `coop tasks unblock %s` to activate it, then retry completion",
				task.ID, task.ID,
			)}
		}
		_, matchErr := auditReopenCurrentHistory(repo, head, task.ID, reopen)
		if !auditReopenRecordActive(reopen) || repo == "" || matchErr != nil {
			recovery := fmt.Sprintf(
				"run `coop tasks unblock %s \"restored or validated audited baseline\"`",
				task.ID,
			)
			if current.State != StateBlocked {
				recovery = fmt.Sprintf(
					"run `coop tasks block %s` without changing Git history, then %s",
					task.ID, recovery,
				)
			}
			return nil, &auditCompletionStateError{message: fmt.Sprintf(
				"task %s complete-history audit authority no longer matches current HEAD; "+
					"its exact recorded baseline is %s: %v — %s before completion",
				task.ID, reopen.BaselineHead, matchErr, recovery,
			)}
		}
	}
	if err := clearLeaseCompletionReceipt(authority); err != nil {
		return nil, err
	}
	if current.State != StateDone {
		if err := MoveTaskDir(root, current, StateDone); err != nil {
			return nil, err
		}
		current.State = StateDone
		current.Dir = filepath.Join(root, StateDone, current.ID)
	}
	finish := finalizeCompletedTask
	if waived != nil {
		finish = finishCompletedTask // accepted as its checklist stands
	}
	if err := finish(current.ID, current.Dir); err != nil {
		return nil, err
	}
	generation := ""
	if reopened {
		generation = reopen.Generation
	}
	lease := &TaskLease{root: root, id: current.ID, authority: authority}
	if reopened {
		lease.Reopen = &reopen
	}
	if err := lease.MarkCompleted(current.Dir); err != nil {
		return nil, err
	}
	if reopened {
		if err := removeAuditReopenRecordIfMatches(root, task.ID, generation); err != nil {
			return nil, err
		}
	}
	// done ends a human claim same as block/unblock/release — a task in 99_done/ is never a loop
	// candidate again, so a leftover record would only ever be silent dead weight, but clearing it
	// here (not just in tasksFolderMove) covers the interactive verb and a re-run of `done` on
	// an already-done task. Fork landing finalizes its exact candidate through its durable journal.
	released := false
	if opts.landed != nil {
		if released, err = opts.landed.release(root, task.ID); err != nil {
			return nil, err
		}
	} else if err := removeTaskOwnerRecord(root, task.ID); err != nil {
		return nil, err
	}
	acceptedTask = QueuedTask{Root: root, Item: current}
	accepted = true
	if released {
		if err := AppendTaskLogStrict(current.Dir, opts.landed.note()); err != nil {
			return nil, fmt.Errorf("task %s is done, but its log.md could not record where its work landed: %w", current.ID, err)
		}
	}
	if waived == nil || alreadyAccepted {
		return nil, nil
	}
	// Last, so the note never outlives a close that failed; a retry finds the receipt instead.
	if err := AppendTaskLogStrict(current.Dir, forcedCompletionNote(waived, opts.actor)); err != nil {
		return waived, fmt.Errorf("task %s is done, but its log.md could not record the forced close: %w", current.ID, err)
	}
	return waived, nil
}

// ExtraBindingsError refuses to close a task whose history already holds more than one commit with
// its Coop-Task trailer. A final review can reopen a task only while it, and every task committed
// after it, has exactly one, so a second binding accepted now loses a later review's verdict.
type ExtraBindingsError struct {
	TaskID  string
	Found   int
	Commits []string // "<short id> by <author>", the first few
}

func (e *ExtraBindingsError) facts(task string) string {
	return fmt.Sprintf("%s %s has %d commits carrying %q: %s", task,
		bindingLabel(e.TaskID), e.Found, CoopTaskTrailer+": "+e.TaskID, strings.Join(e.Commits, ", "))
}

func (e *ExtraBindingsError) Error() string {
	return e.facts("task") + "; a task binds one commit, so make the follow-up unbound by dropping its trailer, or file a new task for that work"
}

// Cause is the refusal as a sentence.
func (e *ExtraBindingsError) Cause() string { return e.facts("Task") + ". A task binds one commit." }

// Remedy keeps the follow-up's work and drops only the binding it should not carry.
func (e *ExtraBindingsError) Remedy() string {
	return "Make the follow-up unbound by dropping its trailer, or file a new task for that work"
}

// refuseExtraBindings is the binding count a review reopen requires, checked when a task is closed
// rather than when a review later fails on it. No repository, or no commit yet, has nothing to
// count; a history Coop cannot walk is left to the checks that need it, which fail closed.
func refuseExtraBindings(project, id string) error {
	repo, head := auditHistory(project)
	if repo == "" || head == "" {
		return nil
	}
	bindings, ok := rawTaskBindings(repo, head)
	if !ok || len(bindings[id]) < 2 {
		return nil
	}
	return extraBindings(repo, id, bindings[id])
}

func extraBindings(repo, id string, shas []string) *ExtraBindingsError {
	return &ExtraBindingsError{TaskID: id, Found: len(shas), Commits: describeTaskCommits(repo, shas)}
}

// ExtraBindings lists, sorted, the queued tasks with more than one commit reachable from HEAD
// carrying their Coop-Task trailer — the refusal each would get at completion. The loop names them
// before a final review spends a run on a verdict the host could not apply. A history Coop cannot
// walk lists nothing here; the checks that need it fail closed on their own.
func ExtraBindings(repo string, hosts []string) ([]*ExtraBindingsError, error) {
	snapshot, err := QueueSnapshot(hosts)
	if err != nil {
		return nil, err
	}
	head := forkspace.GitOut(repo, "rev-parse", "--verify", "HEAD^{commit}")
	if head == "" {
		return nil, nil
	}
	bindings, ok := rawTaskBindings(repo, head)
	if !ok {
		return nil, nil
	}
	var extra []*ExtraBindingsError
	for _, id := range slices.Sorted(maps.Keys(snapshot)) {
		if shas := bindings[id]; len(shas) > 1 {
			extra = append(extra, extraBindings(repo, id, shas))
		}
	}
	return extra, nil
}

// moveTrustedTaskFromDone invalidates completion evidence under the same task authority lock before
// `coop tasks block` reopens an archived task. On a failed move it restores the old receipt, so
// concurrent supervised windows never see a false unowned completion.
func moveTrustedTaskFromDone(root string, task Item, newState string) error {
	return moveTrustedTaskFromDoneWith(root, task, newState, nil)
}

type TrustedTaskMove struct {
	Root          string
	Task          Item
	NewState      string
	SourceStates  []string
	MetadataNames []string
	Reopen        *AuditReopenRecord
	PendingReview *PendingReviewRecord
	AfterMove     func(string) error
}

type trustedTaskMoveState struct {
	move                TrustedTaskMove
	current             Item
	authority           *os.File
	metadata            map[string]taskMetadataSnapshot
	previous            leaseCompletionReceipt
	previousOK          bool
	previousReopen      AuditReopenRecord
	previousReopenOK    bool
	previousDeparture   trustedDoneDeparture
	previousDepartureOK bool
	receiptTouched      bool
	reopenTouched       bool
	departureTouched    bool
	moved               bool
	metadataTouched     bool
}

// moveTrustedTaskFromDoneWith is the one-task form of the atomic host move used for review
// verdicts. The callback may update only log.md and state.md.
func moveTrustedTaskFromDoneWith(root string, task Item, newState string, afterMove func(string) error) error {
	return MoveTrustedTasksFromDoneWith([]TrustedTaskMove{{
		Root: root, Task: task, NewState: newState, AfterMove: afterMove,
	}})
}

// MoveTrustedTasksFromDoneWith holds every task authority lock before the first mutation. Review
// verdicts use this all-or-nothing boundary so a later lease or metadata failure cannot leave an
// earlier subject reopened. Every declared metadata file and completion receipt is restored if any
// callback or move fails.
func MoveTrustedTasksFromDoneWith(moves []TrustedTaskMove) (retErr error) {
	if len(moves) == 0 {
		return nil
	}
	moves = slices.Clone(moves)
	slices.SortFunc(moves, func(a, b TrustedTaskMove) int {
		if byRoot := strings.Compare(a.Root, b.Root); byRoot != 0 {
			return byRoot
		}
		return strings.Compare(a.Task.ID, b.Task.ID)
	})
	var roots, taskIDs []string
	for i, move := range moves {
		if i > 0 && move.Root == moves[i-1].Root && move.Task.ID == moves[i-1].Task.ID {
			return fmt.Errorf("duplicate trusted task move for %s", move.Task.ID)
		}
		if i == 0 || move.Root != moves[i-1].Root {
			roots = append(roots, move.Root)
		}
		taskIDs = append(taskIDs, move.Task.ID)
	}
	windows, err := beginCompletionWindowsAllowingTasks(roots, taskIDs)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCompletionWindowSetup, err)
	}
	windowOpen := true
	defer func() {
		if windowOpen {
			retErr = errors.Join(retErr, windows.Abandon())
		}
	}()
	closeWindow := func(audit bool) error {
		windowOpen = false
		if audit {
			return windows.rejectAndClose(QueuedTask{})
		}
		return windows.Close()
	}
	var states []trustedTaskMoveState
	unlockAll := func() error {
		var errs []error
		for i := len(states) - 1; i >= 0; i-- {
			if states[i].authority != nil {
				errs = append(errs, unlockLeaseFile(states[i].authority))
				states[i].authority = nil
			}
		}
		return errors.Join(errs...)
	}
	failBeforeMutation := func(cause error) error {
		return errors.Join(cause, unlockAll(), closeWindow(true))
	}
	for _, move := range moves {
		authority, err := lockLeaseAuthority(move.Root, move.Task.ID, true, syscall.LOCK_EX|syscall.LOCK_NB)
		if err != nil {
			if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
				err = fmt.Errorf("task %s is leased by another controller", move.Task.ID)
			}
			return failBeforeMutation(err)
		}
		states = append(states, trustedTaskMoveState{move: move, authority: authority})
		current, ok, err := CurrentTask(move.Root, move.Task.ID)
		if err != nil {
			return failBeforeMutation(err)
		}
		if !ok {
			return failBeforeMutation(errLeaseCandidateGone)
		}
		if len(move.SourceStates) == 0 {
			if current.Dir != move.Task.Dir || current.State != StateDone {
				return failBeforeMutation(errLeaseCandidateGone)
			}
		} else if !slices.Contains(move.SourceStates, current.State) {
			return failBeforeMutation(fmt.Errorf(
				"task %s is %s, want one of %s",
				move.Task.ID,
				StateLabel(current.State),
				strings.Join(move.SourceStates, ", "),
			))
		}
		states[len(states)-1].current = current
		if move.PendingReview != nil {
			record, ok, err := readPendingReviewRecord(move.Root, move.Task.ID)
			if err != nil || !ok || !pendingReviewRecordMatchesExpected(record, *move.PendingReview) {
				return failBeforeMutation(errors.Join(err, fmt.Errorf("task %s pending review changed before verdict", move.Task.ID)))
			}
		}
		if move.AfterMove != nil {
			names := move.MetadataNames
			if len(names) == 0 {
				names = []string{"log.md", "state.md"}
			}
			metadata, err := snapshotTaskMetadata(current.Dir, names...)
			if err != nil {
				return failBeforeMutation(err)
			}
			states[len(states)-1].metadata = metadata
		}
		states[len(states)-1].previous, states[len(states)-1].previousOK =
			readLeaseCompletionReceipt(authority, current.Dir)
		previousReopen, previousReopenOK, err := ReadAuditReopenRecord(move.Root, move.Task.ID)
		if err != nil {
			return failBeforeMutation(err)
		}
		states[len(states)-1].previousReopen = previousReopen
		states[len(states)-1].previousReopenOK = previousReopenOK
		previousDeparture, previousDepartureOK, err := readTrustedDoneDeparture(move.Root, move.Task.ID)
		if err != nil {
			return failBeforeMutation(err)
		}
		states[len(states)-1].previousDeparture = previousDeparture
		states[len(states)-1].previousDepartureOK = previousDepartureOK
	}
	rollback := func(cause error) error {
		rollbackErr := rollbackTrustedTaskMoves(states)
		unlockErr := unlockAll()
		if rollbackErr != nil {
			return errors.Join(cause, fmt.Errorf("roll back trusted task moves: %w", rollbackErr), unlockErr)
		}
		return errors.Join(cause, unlockErr, closeWindow(false))
	}
	for i := range states {
		if states[i].current.State == StateDone && states[i].move.NewState != StateDone && states[i].previousOK {
			states[i].departureTouched = true
			if err := appendTrustedDoneDeparture(states[i].move.Root, states[i].current.ID, states[i].previous.Nonce); err != nil {
				return rollback(err)
			}
		}
		states[i].receiptTouched = true
		if err := clearLeaseCompletionReceipt(states[i].authority); err != nil {
			return rollback(err)
		}
		if states[i].move.Reopen != nil {
			states[i].reopenTouched = true
			if err := WriteAuditReopenRecord(states[i].move.Root, *states[i].move.Reopen); err != nil {
				return rollback(err)
			}
		}
	}
	for i := range states {
		state := &states[i]
		if state.current.State != state.move.NewState {
			if err := MoveTaskDir(state.move.Root, state.current, state.move.NewState); err != nil {
				return rollback(err)
			}
			state.moved = true
		}
		if state.move.AfterMove != nil {
			state.metadataTouched = true
			dir := filepath.Join(state.move.Root, state.move.NewState, state.current.ID)
			if err := state.move.AfterMove(dir); err != nil {
				return rollback(err)
			}
		}
	}
	return errors.Join(unlockAll(), closeWindow(true))
}

func rollbackTrustedTaskMoves(states []trustedTaskMoveState) error {
	restored := make([]bool, len(states))
	var errs []error
	for i := len(states) - 1; i >= 0; i-- {
		state := &states[i]
		restored[i] = true
		dir := filepath.Join(state.move.Root, state.move.NewState, state.current.ID)
		if !state.moved {
			dir = state.current.Dir
		}
		var metadataErr error
		if state.metadataTouched {
			metadataErr = restoreTaskMetadata(dir, state.metadata)
		}
		var moveErr error
		if state.moved {
			moved := state.current
			moved.State = state.move.NewState
			moved.Dir = dir
			moveErr = MoveTaskDir(state.move.Root, moved, state.current.State)
		}
		restored[i] = metadataErr == nil && moveErr == nil
		if err := errors.Join(metadataErr, moveErr); err != nil {
			errs = append(errs, fmt.Errorf("restore task %s: %w", state.current.ID, err))
		}
	}
	for i := range states {
		state := &states[i]
		if state.departureTouched {
			var err error
			if state.previousDepartureOK {
				err = writeTrustedDoneDeparture(state.move.Root, state.previousDeparture)
			} else {
				err = removeTrustedDoneDeparture(state.move.Root, state.current.ID)
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("restore task %s trusted done departure: %w", state.current.ID, err))
				restored[i] = false
			}
		}
		if state.reopenTouched {
			var err error
			if state.previousReopenOK {
				err = WriteAuditReopenRecord(state.move.Root, state.previousReopen)
			} else {
				err = removeAuditReopenRecord(state.move.Root, state.current.ID)
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("restore task %s audit reopen authority: %w", state.current.ID, err))
				restored[i] = false
			}
		}
		if !state.receiptTouched || !state.previousOK || !restored[i] {
			continue
		}
		if err := writeLeaseCompletionReceiptValue(state.authority, state.previous); err != nil {
			errs = append(errs, fmt.Errorf("restore task %s completion receipt: %w", state.current.ID, err))
			continue
		}
		if state.move.PendingReview != nil {
			// Restoring bytes and renaming the archive changes its filesystem fingerprint.
			// Bind the restored debt to that archive while its original receipt is still locked.
			record := *state.move.PendingReview
			fingerprint, err := completionFingerprintWithReceipt(state.current, state.previous.Nonce, false)
			if err == nil {
				record.Fingerprint = fingerprint
				err = writePendingReviewRecord(state.move.Root, record)
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("restore task %s pending review: %w", state.current.ID, err))
			}
		}
	}
	return errors.Join(errs...)
}

type taskMetadataSnapshot struct {
	body   []byte
	exists bool
}

func snapshotTaskMetadata(taskDir string, names ...string) (map[string]taskMetadataSnapshot, error) {
	root, err := OpenTaskMetadataRoot(taskDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	snapshots := make(map[string]taskMetadataSnapshot, len(names))
	for _, name := range names {
		body, err := ReadTaskMetadataFile(root, name)
		if errors.Is(err, os.ErrNotExist) {
			snapshots[name] = taskMetadataSnapshot{}
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("snapshot task metadata %q: %w", name, err)
		}
		snapshots[name] = taskMetadataSnapshot{body: body, exists: true}
	}
	return snapshots, nil
}

func restoreTaskMetadata(taskDir string, snapshots map[string]taskMetadataSnapshot) error {
	root, err := OpenTaskMetadataRoot(taskDir)
	if err != nil {
		return err
	}
	defer root.Close()
	var errs []error
	names := make([]string, 0, len(snapshots))
	for name := range snapshots {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		snapshot := snapshots[name]
		if snapshot.exists {
			errs = append(errs, AtomicWriteTaskFile(root, name, snapshot.body))
			continue
		}
		if err := root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove rolled-back task metadata %q: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// rejectUnownedCompletions restores folders this iteration moved without owning their lease. A
// foreign-held folder belongs to another live controller and is ignored. After both locks are
// acquired, only a matching host-only completion receipt proves a released foreign controller
// finalized the folder; provider-writable task metadata is never ownership evidence. A review's
// own subjects are restored the same way, under a note that names the archive change instead.
func rejectUnownedCompletions(tasks []QueuedTask, assigned QueuedTask, subjects []string) ([]string, error) {
	var rejected []string
	var errs []error
	for _, task := range tasks {
		if task.Root == assigned.Root && task.Item.ID == assigned.Item.ID {
			continue
		}
		lock, current, acquired, err := lockCrashCompletion(task.Root, task.Item)
		if err != nil {
			errs = append(errs, fmt.Errorf("lock rejected task %s: %w", task.Item.ID, err))
			continue
		}
		if !acquired {
			continue
		}
		if lock.completed(current.Dir) {
			errs = append(errs, lock.release())
			continue
		}
		rejected = append(rejected, task.Item.ID)
		restore := restoreUnownedCompletion
		if slices.Contains(subjects, task.Item.ID) {
			restore = restoreChangedReviewSubject
		}
		errs = append(errs, restore(QueuedTask{Root: task.Root, Item: current}), lock.clearCompleted(), lock.release())
	}
	slices.Sort(rejected)
	return rejected, errors.Join(errs...)
}

func FinalizeQueuedCompletion(task QueuedTask) error {
	if err := finalizeCompletedTask(task.Item.ID, task.Item.Dir); err != nil {
		if restoreErr := MoveTaskDir(task.Root, task.Item, StateInProgress); restoreErr != nil {
			return errors.Join(err, fmt.Errorf("restore task %s after finalization failure: %w", task.Item.ID, restoreErr))
		}
		restored := filepath.Join(task.Root, StateInProgress, task.Item.ID)
		next := "fix the task metadata or cleanup obstruction, then re-run `coop loop`"
		if errors.Is(err, ErrIncompleteChecklist) {
			next = "finish the remaining checklist and required verification before completing; keep unavailable checks open"
		}
		recoveryErr := NormalizeTaskState(
			task.Item.ID,
			restored,
			"in progress — finalization failed",
			next,
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

// ReconcileInterruptedCompletions closes the crash window around accepted completion. A matching
// host receipt means finalization finished and only stale lease metadata needs removal. Without a
// receipt, the lease does not retain the iteration base, so the task is restored for a new
// range-bound attempt; an older matching commit must never validate new unbound work.
func ReconcileInterruptedCompletions(hosts []string) error {
	var restoreErrs []error
	for _, host := range hosts {
		items, err := ReadTaskTree(host)
		if err != nil {
			restoreErrs = append(restoreErrs, err)
			continue
		}
		for _, task := range items {
			if task.State != StateDone {
				restoreErrs = append(restoreErrs, clearTaskCompletionReceipt(host, task.ID))
				continue
			}
			if !crashCompletionCandidate(host, task) {
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
			if receipt, ok := lock.completionReceipt(current.Dir); ok {
				record, hasAuditAuthority, authorityErr := ReadAuditReopenRecord(host, current.ID)
				if authorityErr != nil {
					restoreErrs = append(restoreErrs, errors.Join(
						fmt.Errorf("inspect interrupted task %s audit authority: %w", current.ID, authorityErr),
						lock.release(),
					))
					continue
				}
				if receipt.AuditReopenGeneration != "" && hasAuditAuthority &&
					(!auditReopenRecordActive(record) ||
						record.Generation != receipt.AuditReopenGeneration) {
					restoreErr := RestoreQueuedCompletion(
						QueuedTask{Root: host, Item: current},
						hasAuditAuthority,
					)
					clearErr := lock.clearCompleted()
					unlockErr := lock.release()
					if err := errors.Join(restoreErr, clearErr, unlockErr); err != nil {
						restoreErrs = append(restoreErrs, err)
					}
					continue
				}
				cleanupErr := errors.Join(
					removeAuditReopenRecordIfMatches(host, current.ID, receipt.AuditReopenGeneration),
					removeLeaseAuthorityMetadata(host, current.ID),
					lock.release(),
				)
				if cleanupErr != nil {
					restoreErrs = append(restoreErrs, fmt.Errorf("clean accepted task %s lease metadata: %w", task.ID, cleanupErr))
				}
				continue
			}
			record, hasAuditAuthority, authorityErr := ReadAuditReopenRecord(host, current.ID)
			if authorityErr != nil {
				restoreErrs = append(restoreErrs, errors.Join(
					fmt.Errorf("inspect interrupted task %s audit authority: %w", current.ID, authorityErr),
					lock.release(),
				))
				continue
			}
			restoreErr := RestoreQueuedCompletion(
				QueuedTask{Root: host, Item: current},
				hasAuditAuthority && !record.UnblockPending,
			)
			clearErr := lock.clearCompleted()
			unlockErr := lock.release()
			if err := errors.Join(restoreErr, clearErr, unlockErr); err != nil {
				restoreErrs = append(restoreErrs, err)
			}
		}
	}
	return errors.Join(restoreErrs...)
}

type crashCompletionLock struct {
	authority *os.File
}

func (l crashCompletionLock) completed(taskDir string) bool {
	_, ok := l.completionReceipt(taskDir)
	return ok
}

func (l crashCompletionLock) completionReceipt(taskDir string) (leaseCompletionReceipt, bool) {
	if l.authority == nil {
		return leaseCompletionReceipt{}, false
	}
	return readLeaseCompletionReceipt(l.authority, taskDir)
}

func (l crashCompletionLock) clearCompleted() error {
	if l.authority == nil {
		return nil
	}
	return clearLeaseCompletionReceipt(l.authority)
}

func (l crashCompletionLock) release() error {
	if l.authority == nil {
		return nil
	}
	return unlockLeaseFile(l.authority)
}

func lockCrashCompletion(root string, task Item) (crashCompletionLock, Item, bool, error) {
	authority, err := lockLeaseAuthorityForAudit(root, task.ID, true, "task "+task.ID+" authority", func() bool {
		return leaseAuthorityMetadataExists(root, task.ID)
	})
	if errors.Is(err, errCompletionAuditLockOwned) {
		return crashCompletionLock{}, Item{}, false, nil
	}
	if err != nil {
		return crashCompletionLock{}, Item{}, false, err
	}
	lock := crashCompletionLock{authority: authority}
	current, ok, err := CurrentTask(root, task.ID)
	if err != nil {
		return crashCompletionLock{}, Item{}, false, errors.Join(err, lock.release())
	}
	if !ok || current.State != StateDone || current.Dir != task.Dir {
		return crashCompletionLock{}, Item{}, false, lock.release()
	}
	return lock, current, true, nil
}

var errCompletionAuditLockOwned = errors.New("completion audit lock is owned")

// lockExclusiveForCompletionAudit waits out short host-only receipt readers. A real controller is
// identified by authority metadata; any other unresolved lock fails the audit so its durable
// journal is retained instead of accepting uncertainty.
func lockExclusiveForCompletionAudit(file *os.File, label string, owned func() bool) error {
	const (
		pollInterval = 2 * time.Millisecond
		waitLimit    = time.Second
	)
	deadline := time.Now().Add(waitLimit)
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return err
		}
		if owned != nil && owned() {
			return errCompletionAuditLockOwned
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s remained busy without authoritative owner metadata", label)
		}
		time.Sleep(pollInterval)
	}
}

func unlockLeaseFile(file *os.File) error {
	return errors.Join(syscall.Flock(int(file.Fd()), syscall.LOCK_UN), file.Close())
}

func crashCompletionCandidate(root string, task Item) bool {
	return leaseAuthorityMetadataExists(root, task.ID) || auditReopenRecordExists(root, task.ID)
}

// AlreadyCommittedInProgress reports the in_progress tasks whose implementation commit is ALREADY
// reachable from HEAD, with that commit and how many commits sit on top of it. That state means the
// run died between the commit and the folder move — a hard Ctrl-C, a crash, or a background-timeout
// handoff un-completing finished work — and nothing else surfaces it: the loop just re-picks the
// task, and the resume recipe is only safe while the commit is still HEAD. Reporting it at startup
// turns a silent trap into something a human can confirm or close. Sorted; read-only.
func AlreadyCommittedInProgress(repo string, hosts []string) ([]struct {
	ID, Commit string
	Depth      int
}, error) {
	var out []struct {
		ID, Commit string
		Depth      int
	}
	snapshot, err := QueueSnapshot(hosts)
	if err != nil {
		return nil, err
	}
	for id, st := range snapshot {
		if st != StateInProgress {
			continue
		}
		commits := CommitsForTask(repo, "", id)
		if len(commits) == 0 {
			continue
		}
		depth := 0
		if n := forkspace.GitOut(repo, "rev-list", "--count", commits[0]+"..HEAD"); n != "" {
			depth, _ = strconv.Atoi(n)
		}
		out = append(out, struct {
			ID, Commit string
			Depth      int
		}{ID: id, Commit: commits[0], Depth: depth})
	}
	slices.SortFunc(out, func(a, b struct {
		ID, Commit string
		Depth      int
	}) int {
		return strings.Compare(a.ID, b.ID)
	})
	return out, nil
}

type semanticHistoryCommit struct {
	sha      string
	semantic AuditReopenCommit
}

func semanticHistoryCommitsExact(repo string, rawHistory []rawAuditCommit) ([]semanticHistoryCommit, error) {
	commits := make([]TaskTrailerCommit, len(rawHistory))
	for i := range rawHistory {
		raw := rawHistory[i]
		if raw.sha == "" || raw.authorDate == "" {
			return nil, errors.New("exact raw audit history metadata is incomplete")
		}
		commits[i] = TaskTrailerCommit{
			fullSHA: raw.sha, authorName: raw.authorName, authorEmail: raw.authorEmail,
			authorDate: raw.authorDate, commitMessage: raw.commitMessage,
			Values: slices.Clone(raw.taskValues), Malformed: raw.taskBindingInvalid,
		}
	}
	changeTrees, err := semanticRawHistoryChangeTrees(repo, rawHistory)
	if err != nil {
		return nil, err
	}
	return semanticHistoryCommitsFromChangeTrees(commits, changeTrees)
}

func semanticHistoryCommitsFromChangeTrees(
	commits []TaskTrailerCommit,
	changeTrees []string,
) ([]semanticHistoryCommit, error) {
	if len(changeTrees) != len(commits) {
		return nil, errors.New("complete audit history changes are incomplete")
	}
	taskIDs := make([]string, len(commits))
	seen := map[string]bool{}
	for i, commit := range commits {
		if commit.Malformed || len(commit.Values) > 1 || (len(commit.Values) == 1 && commit.Values[0] == "") {
			return nil, errors.New("history contains an invalid task binding")
		}
		if !validAuditReopenHead(commit.fullSHA) {
			return nil, errors.New("history contains an invalid commit id")
		}
		if len(commit.Values) == 1 {
			taskIDs[i] = commit.Values[0]
			if seen[taskIDs[i]] {
				return nil, fmt.Errorf("history contains duplicate task binding %s", taskIDs[i])
			}
			seen[taskIDs[i]] = true
		}
	}
	result := make([]semanticHistoryCommit, len(commits))
	for i, commit := range commits {
		result[i] = semanticHistoryCommit{
			sha: commit.fullSHA,
			semantic: AuditReopenCommit{
				TaskID:        taskIDs[i],
				ChangeTree:    changeTrees[i],
				AuthorName:    commit.authorName,
				AuthorEmail:   commit.authorEmail,
				AuthorDate:    commit.authorDate,
				CommitMessage: commit.commitMessage,
			},
		}
	}
	return result, nil
}

// semanticRawHistoryChangeTrees hashes diffs between explicit raw parent/child objects. No Git
// traversal metadata participates after the raw walk, so a concurrent graft or shallow-file edit
// cannot change a commit's semantic identity between validation and diff extraction.
func semanticRawHistoryChangeTrees(repo string, history []rawAuditCommit) ([]string, error) {
	if len(history) == 0 {
		return []string{}, nil
	}
	emptyTree := auditObjectID("tree", nil, len(history[0].tree))
	if emptyTree == "" {
		return nil, errors.New("derive repository empty tree")
	}
	type treePair struct{ parent, child string }
	pairs := make([]treePair, len(history))
	var input strings.Builder
	for i, commit := range history {
		if !validAuditReopenHead(commit.tree) {
			return nil, fmt.Errorf("audit history commit %s has an invalid raw tree", commit.sha)
		}
		parentTree := emptyTree
		if commit.parent != "" {
			if i > 0 && history[i-1].sha == commit.parent {
				parentTree = history[i-1].tree
			} else {
				var err error
				parentTree, err = auditCommitTree(repo, commit.parent)
				if err != nil {
					return nil, fmt.Errorf("resolve audit history commit %s parent tree", commit.sha)
				}
			}
		}
		pairs[i] = treePair{parent: parentTree, child: commit.tree}
		input.WriteString(parentTree)
		input.WriteByte(' ')
		input.WriteString(commit.tree)
		input.WriteByte('\n')
	}
	treeRoots := make([]string, 0, len(pairs)*2)
	for _, pair := range pairs {
		treeRoots = append(treeRoots, pair.parent, pair.child)
	}
	treeSnapshot, err := snapshotAuditTreeDAGs(repo, treeRoots)
	if err != nil {
		return nil, fmt.Errorf("validate exact raw audit history trees: %w", err)
	}
	defer os.RemoveAll(treeSnapshot)
	cmd, err := forkspace.GitCommand(context.Background(), treeSnapshot,
		"diff-tree", "--stdin", "--root", "--always", "--raw", "-z", "-r", "--no-renames")
	if err != nil {
		return nil, err
	}
	cmd.Stdin = strings.NewReader(input.String())
	raw, err := auditCommandOutput(cmd, auditDiffOutputLimit)
	if err != nil {
		return nil, fmt.Errorf("read exact raw audit history changes: %w", err)
	}
	trees := make([]string, 0, len(pairs))
	for i, pair := range pairs {
		header := []byte(pair.parent + " " + pair.child + "\n")
		if !bytes.HasPrefix(raw, header) {
			return nil, errors.New("exact raw audit history changes are out of order")
		}
		raw = raw[len(header):]
		var diff bytes.Buffer
		for len(raw) > 0 {
			if i+1 < len(pairs) {
				next := []byte(pairs[i+1].parent + " " + pairs[i+1].child + "\n")
				if bytes.HasPrefix(raw, next) {
					break
				}
			}
			if raw[0] != ':' {
				return nil, errors.New("parse exact raw audit history change")
			}
			metaEnd := bytes.IndexByte(raw, 0)
			if metaEnd < 0 {
				return nil, errors.New("parse exact raw audit history metadata")
			}
			pathEnd := bytes.IndexByte(raw[metaEnd+1:], 0)
			if pathEnd < 0 {
				return nil, errors.New("parse exact raw audit history path")
			}
			pathEnd += metaEnd + 1
			diff.Write(raw[:pathEnd+1])
			raw = raw[pathEnd+1:]
		}
		sum := sha256.Sum256(diff.Bytes())
		trees = append(trees, fmt.Sprintf("%x", sum))
	}
	if len(raw) != 0 {
		return nil, errors.New("exact raw audit history changes have trailing output")
	}
	return trees, nil
}

func snapshotAuditTreeDAGs(repo string, roots []string) (snapshot string, err error) {
	if len(roots) == 0 {
		return "", errors.New("audit tree snapshot needs a root")
	}
	objectIDBytes := len(roots[0]) / 2
	if objectIDBytes != 20 && objectIDBytes != 32 {
		return "", errors.New("audit tree has an unsupported object id")
	}
	snapshot, err = os.MkdirTemp("", "coop-audit-trees-")
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, os.RemoveAll(snapshot))
		}
	}()
	initArgs := []string{"init", "--bare", "--quiet"}
	if objectIDBytes == sha256.Size {
		initArgs = append(initArgs, "--object-format=sha256")
	}
	initCmd := forkspace.GitRefCommand(context.Background(), snapshot, initArgs...) // no repository exists yet to view
	if _, err = auditCommandOutput(initCmd, auditMetadataOutputLimit); err != nil {
		return "", fmt.Errorf("initialize audit tree snapshot: %w", err)
	}
	batch, err := openAuditCommitBatch(repo)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, batch.close()) }()

	pending := make([]string, 0, len(roots))
	queued := make(map[string]bool, len(roots))
	for _, root := range roots {
		if !validAuditReopenHead(root) || len(root) != objectIDBytes*2 {
			return "", errors.New("audit tree has an invalid object id")
		}
		if !queued[root] {
			pending = append(pending, root)
			queued[root] = true
		}
	}
	emptyTree := auditObjectID("tree", nil, objectIDBytes*2)
	seen := map[string]bool{}
	var totalBytes int64
	totalEntries := 0
	for len(pending) > 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		delete(queued, current)
		if seen[current] {
			continue
		}
		if len(seen) >= auditTreeObjectCountLimit {
			return "", fmt.Errorf("audit tree history exceeds %d tree objects", auditTreeObjectCountLimit)
		}
		var raw []byte
		if current != emptyTree {
			var readErr error
			raw, readErr = batch.tree(current)
			if readErr != nil {
				return "", readErr
			}
		}
		totalBytes += int64(len(raw))
		if totalBytes > auditTreeByteLimit {
			return "", fmt.Errorf("audit tree history exceeds %d bytes", auditTreeByteLimit)
		}
		children, entries, parseErr := auditTreeChildren(raw, objectIDBytes)
		if parseErr != nil {
			return "", fmt.Errorf("parse raw audit tree %s: %w", current, parseErr)
		}
		totalEntries += entries
		if totalEntries > auditTreeEntryLimit {
			return "", fmt.Errorf("audit tree history exceeds %d entries", auditTreeEntryLimit)
		}
		if err := writeAuditSnapshotObject(snapshot, "tree", current, raw); err != nil {
			return "", err
		}
		seen[current] = true
		for _, child := range children {
			if !seen[child] && !queued[child] {
				pending = append(pending, child)
				queued[child] = true
			}
		}
	}
	return snapshot, nil
}

func writeAuditSnapshotObject(snapshot, objectType, objectID string, raw []byte) (retErr error) {
	if auditObjectID(objectType, raw, len(objectID)) != objectID {
		return fmt.Errorf("verify audit snapshot %s %s", objectType, objectID)
	}
	dir := filepath.Join(snapshot, "objects", objectID[:2])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(dir, objectID[2:]), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, file.Close()) }()
	compressed := zlib.NewWriter(file)
	header := []byte(fmt.Sprintf("%s %d%c", objectType, len(raw), 0))
	if _, err := compressed.Write(header); err != nil {
		_ = compressed.Close()
		return err
	}
	if _, err := compressed.Write(raw); err != nil {
		_ = compressed.Close()
		return err
	}
	return compressed.Close()
}

func auditTreeChildren(raw []byte, objectIDBytes int) ([]string, int, error) {
	var children []string
	entries := 0
	for len(raw) > 0 {
		modeEnd := bytes.IndexByte(raw, ' ')
		if modeEnd <= 0 {
			return nil, 0, errors.New("invalid tree mode")
		}
		mode := string(raw[:modeEnd])
		switch mode {
		case "40000", "100644", "100755", "120000", "160000":
		default:
			return nil, 0, errors.New("invalid or noncanonical tree mode")
		}
		nameStart := modeEnd + 1
		nameEnd := bytes.IndexByte(raw[nameStart:], 0)
		if nameEnd <= 0 {
			return nil, 0, errors.New("invalid tree name")
		}
		nameEnd += nameStart
		if bytes.IndexByte(raw[nameStart:nameEnd], '/') >= 0 {
			return nil, 0, errors.New("invalid tree name")
		}
		objectStart := nameEnd + 1
		objectEnd := objectStart + objectIDBytes
		if objectEnd > len(raw) {
			return nil, 0, errors.New("truncated tree object id")
		}
		entries++
		if mode == "40000" {
			children = append(children, hex.EncodeToString(raw[objectStart:objectEnd]))
		}
		raw = raw[objectEnd:]
	}
	return children, entries, nil
}

func auditCommitTree(repo, sha string) (tree string, err error) {
	if !validAuditReopenHead(sha) {
		return "", fmt.Errorf("resolve audit history commit %s tree", sha)
	}
	batch, err := openAuditCommitBatch(repo)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, batch.close()) }()
	raw, err := batch.commit(sha)
	if err != nil {
		return "", err
	}
	tree, _, err = auditCommitHeaderFromRaw(sha, raw)
	return tree, err
}

func auditCommitHeaderFromRaw(sha string, raw []byte) (string, []string, error) {
	lines := strings.Split(string(raw), "\n")
	tree, ok := strings.CutPrefix(lines[0], "tree ")
	if !ok || !validAuditReopenHead(tree) {
		return "", nil, fmt.Errorf("resolve audit history commit %s parent", sha)
	}
	tree = strings.Clone(tree)
	var parents []string
	parentSet := map[string]bool{}
	parentsDone := false
	headersDone := false
	for _, line := range lines[1:] {
		if line == "" {
			headersDone = true
			break
		}
		candidate, isParent := strings.CutPrefix(line, "parent ")
		if line == "parent" || (isParent && parentsDone) {
			return "", nil, fmt.Errorf("resolve audit history commit %s parent", sha)
		}
		if isParent {
			if !validAuditReopenHead(candidate) {
				return "", nil, fmt.Errorf("resolve audit history commit %s parent", sha)
			}
			candidate = strings.Clone(candidate)
			if parentSet[candidate] {
				return "", nil, fmt.Errorf("audit history commit %s repeats parent %s", sha, candidate)
			}
			parentSet[candidate] = true
			parents = append(parents, candidate)
			continue
		}
		parentsDone = true
	}
	if !headersDone {
		return "", nil, fmt.Errorf("resolve audit history commit %s parent", sha)
	}
	return tree, parents, nil
}

func auditTaskTrailersFromRaw(raw []byte) ([]string, bool) {
	separator := bytes.Index(raw, []byte("\n\n"))
	if separator < 0 {
		return nil, false
	}
	return auditTaskTrailersFromMessage(raw[separator+2:])
}

func auditTaskTrailersFromMessage(raw []byte) ([]string, bool) {
	message := strings.TrimRight(string(raw), "\n")
	lines := strings.Split(message, "\n")
	start := len(lines) - 1
	for start > 0 && strings.TrimSpace(lines[start-1]) != "" {
		start--
	}
	var values []string
	invalid := false
	currentCoop := false
	sawTrailer := false
	invalidBlock := false
	for _, line := range lines[start:] {
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			if !sawTrailer {
				invalidBlock = true
			}
			if currentCoop {
				invalid = true
			}
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			invalidBlock = true
			if strings.EqualFold(strings.TrimSpace(line), CoopTaskTrailer) {
				invalid = true
			}
			currentCoop = false
			continue
		}
		sawTrailer = true
		currentCoop = strings.EqualFold(strings.TrimSpace(key), CoopTaskTrailer)
		if !currentCoop {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "" || len(value) > auditTaskTrailerValueLimit {
			invalid = true
			continue
		}
		values = append(values, strings.Clone(value))
	}
	if len(values) > 0 && invalidBlock {
		invalid = true
	}
	return values, invalid
}

type rawAuditCommit struct {
	sha, tree, parent  string
	authorName         string
	authorEmail        string
	authorDate         string
	commitMessage      string
	taskValues         []string
	taskBindingInvalid bool
}

func rawAuditCommitFromObject(sha string, raw []byte) (rawAuditCommit, error) {
	tree, parents, err := auditCommitHeaderFromRaw(sha, raw)
	if err != nil {
		return rawAuditCommit{}, err
	}
	if len(parents) > 1 {
		return rawAuditCommit{}, fmt.Errorf("audit history merge commit %s cannot be replayed safely", sha)
	}
	parent := ""
	if len(parents) == 1 {
		parent = parents[0]
	}
	separator := bytes.Index(raw, []byte("\n\n"))
	if separator < 0 {
		return rawAuditCommit{}, fmt.Errorf("parse raw audit commit %s message", sha)
	}
	var authorLine string
	for _, line := range strings.Split(string(raw[:separator]), "\n") {
		if candidate, ok := strings.CutPrefix(line, "author "); ok {
			if authorLine != "" {
				return rawAuditCommit{}, fmt.Errorf("parse raw audit commit %s author", sha)
			}
			authorLine = candidate
		}
	}
	authorName, authorEmail, authorDate, err := auditAuthorIdentity(authorLine)
	if err != nil {
		return rawAuditCommit{}, fmt.Errorf("parse raw audit commit %s author: %w", sha, err)
	}
	message := bytes.Clone(raw[separator+2:])
	taskValues, taskBindingInvalid := auditTaskTrailersFromMessage(message)
	return rawAuditCommit{
		sha: sha, tree: tree, parent: parent,
		authorName: authorName, authorEmail: authorEmail, authorDate: authorDate,
		commitMessage: string(message),
		taskValues:    taskValues, taskBindingInvalid: taskBindingInvalid,
	}, nil
}

func auditAuthorIdentity(raw string) (string, string, string, error) {
	emailEnd := strings.LastIndex(raw, "> ")
	if emailEnd < 0 {
		return "", "", "", errors.New("missing email")
	}
	emailStart := strings.LastIndex(raw[:emailEnd], " <")
	if emailStart < 0 {
		return "", "", "", errors.New("missing email")
	}
	dateFields := strings.Fields(raw[emailEnd+2:])
	if len(dateFields) != 2 || len(dateFields[1]) != 5 ||
		(dateFields[1][0] != '+' && dateFields[1][0] != '-') {
		return "", "", "", errors.New("invalid date")
	}
	unixSeconds, err := strconv.ParseInt(dateFields[0], 10, 64)
	if err != nil {
		return "", "", "", errors.New("invalid date")
	}
	hours, hoursErr := strconv.Atoi(dateFields[1][1:3])
	minutes, minutesErr := strconv.Atoi(dateFields[1][3:5])
	if hoursErr != nil || minutesErr != nil || hours > 23 || minutes > 59 {
		return "", "", "", errors.New("invalid timezone")
	}
	offset := (hours*60 + minutes) * 60
	if dateFields[1][0] == '-' {
		offset = -offset
	}
	date := time.Unix(unixSeconds, 0).
		In(time.FixedZone("", offset)).
		Format(time.RFC3339)
	return strings.Clone(raw[:emailStart]), strings.Clone(raw[emailStart+2 : emailEnd]), date, nil
}

type auditCommitBatch struct {
	cmd     *exec.Cmd
	input   io.WriteCloser
	output  *bufio.Reader
	stderr  bytes.Buffer
	aborted bool
}

func openAuditCommitBatch(repo string) (*auditCommitBatch, error) {
	batch := &auditCommitBatch{}
	cmd, err := forkspace.GitCommand(context.Background(), repo, "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	batch.cmd = cmd
	input, err := batch.cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	output, err := batch.cmd.StdoutPipe()
	if err != nil {
		_ = input.Close()
		return nil, err
	}
	batch.input = input
	batch.output = bufio.NewReader(output)
	batch.cmd.Stderr = &batch.stderr
	if err := batch.cmd.Start(); err != nil {
		_ = input.Close()
		return nil, err
	}
	return batch, nil
}

func (b *auditCommitBatch) close() error {
	closeErr := b.input.Close()
	waitErr := b.cmd.Wait()
	if b.aborted {
		return nil
	}
	if waitErr != nil && b.stderr.Len() > 0 {
		waitErr = fmt.Errorf("%w: %s", waitErr, strings.TrimSpace(b.stderr.String()))
	}
	return errors.Join(closeErr, waitErr)
}

func (b *auditCommitBatch) abort() {
	b.aborted = true
	_ = b.input.Close()
	if b.cmd.Process != nil {
		_ = b.cmd.Process.Kill()
	}
}

func (b *auditCommitBatch) object(
	sha, peel, objectType string,
	sizeLimit int,
) ([]byte, error) {
	if _, err := fmt.Fprintln(b.input, sha+peel); err != nil {
		b.abort()
		return nil, err
	}
	header, err := b.output.ReadString('\n')
	if err != nil {
		b.abort()
		return nil, err
	}
	fields := strings.Fields(header)
	if len(fields) != 3 || !strings.EqualFold(fields[0], sha) || fields[1] != objectType {
		b.abort()
		return nil, fmt.Errorf("resolve raw audit %s %s", objectType, sha)
	}
	size, err := strconv.Atoi(fields[2])
	if err != nil || size < 0 || size > sizeLimit {
		b.abort()
		return nil, fmt.Errorf("read raw audit %s %s size", objectType, sha)
	}
	raw := make([]byte, size)
	if _, err := io.ReadFull(b.output, raw); err != nil {
		b.abort()
		return nil, err
	}
	if terminator, err := b.output.ReadByte(); err != nil || terminator != '\n' {
		b.abort()
		return nil, fmt.Errorf("read raw audit %s %s terminator", objectType, sha)
	}
	if auditObjectID(objectType, raw, len(sha)) != strings.ToLower(sha) {
		b.abort()
		return nil, fmt.Errorf("verify raw audit %s %s content", objectType, sha)
	}
	return raw, nil
}

func auditObjectID(objectType string, raw []byte, hexLength int) string {
	header := []byte(fmt.Sprintf("%s %d%c", objectType, len(raw), 0))
	switch hexLength {
	case sha1.Size * 2:
		hash := sha1.New()
		_, _ = hash.Write(header)
		_, _ = hash.Write(raw)
		return hex.EncodeToString(hash.Sum(nil))
	case sha256.Size * 2:
		hash := sha256.New()
		_, _ = hash.Write(header)
		_, _ = hash.Write(raw)
		return hex.EncodeToString(hash.Sum(nil))
	default:
		return ""
	}
}

func (b *auditCommitBatch) commit(sha string) ([]byte, error) {
	return b.object(sha, "^{commit}", "commit", auditReopenCommitSizeLimit)
}

func (b *auditCommitBatch) tree(sha string) ([]byte, error) {
	return b.object(sha, "", "tree", auditTreeObjectSizeLimit)
}

type rawReachableAuditCommit struct {
	sha                string
	taskValues         []string
	taskBindingInvalid bool
}

func rawReachableAuditCommits(repo, head string) (commits []rawReachableAuditCommit, err error) {
	return rawReachableAuditCommitsLimit(
		repo,
		head,
		auditReachableHistoryLimit,
		auditReachableEdgeLimit,
		auditReachableByteLimit,
	)
}

func rawReachableAuditCommitsLimit(
	repo, head string,
	commitLimit, edgeLimit int,
	byteLimit int64,
) (commits []rawReachableAuditCommit, err error) {
	current := forkspace.GitOut(repo, "rev-parse", "--verify", head+"^{commit}")
	if !validAuditReopenHead(current) {
		return nil, fmt.Errorf("resolve raw audit terminal %s", head)
	}
	batch, err := openAuditCommitBatch(repo)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, batch.close()) }()
	pending := []string{current}
	seen := map[string]bool{}
	queued := map[string]bool{current: true}
	edges := 0
	var rawBytes int64
	for len(pending) > 0 {
		current = pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		delete(queued, current)
		if seen[current] {
			continue
		}
		if len(seen) >= commitLimit {
			return nil, fmt.Errorf("raw reachable audit history exceeds %d commits", commitLimit)
		}
		raw, readErr := batch.commit(current)
		if readErr != nil {
			return nil, readErr
		}
		rawBytes += int64(len(raw))
		if rawBytes > byteLimit {
			return nil, fmt.Errorf("raw reachable audit history exceeds %d bytes", byteLimit)
		}
		_, parents, parseErr := auditCommitHeaderFromRaw(current, raw)
		if parseErr != nil {
			return nil, parseErr
		}
		taskValues, taskBindingInvalid := auditTaskTrailersFromRaw(raw)
		seen[current] = true
		commits = append(commits, rawReachableAuditCommit{
			sha: current, taskValues: taskValues, taskBindingInvalid: taskBindingInvalid,
		})
		for i := len(parents) - 1; i >= 0; i-- {
			edges++
			if edges > edgeLimit {
				return nil, fmt.Errorf("raw reachable audit history exceeds %d parent edges", edgeLimit)
			}
			if !seen[parents[i]] && !queued[parents[i]] {
				pending = append(pending, parents[i])
				queued[parents[i]] = true
			}
		}
	}
	return commits, nil
}

func rawTaskBindings(repo, head string) (map[string][]string, bool) {
	commits, err := rawReachableAuditCommits(repo, head)
	if err != nil {
		return nil, false
	}
	return taskBindingsOf(commits), true
}

// taskBindingsOf is the commits that bind a task, by task: those with exactly one valid Coop-Task
// trailer.
func taskBindingsOf(commits []rawReachableAuditCommit) map[string][]string {
	bindings := map[string][]string{}
	for _, commit := range commits {
		if !commit.taskBindingInvalid && len(commit.taskValues) == 1 {
			bindings[commit.taskValues[0]] = append(bindings[commit.taskValues[0]], commit.sha)
		}
	}
	return bindings
}

func rawAuditHistoryCount(repo, head string, count int) (history []rawAuditCommit, err error) {
	if count < 1 || count > auditReopenHistoryLimit+1 {
		return nil, errors.New("audit history length is outside its bound")
	}
	current := forkspace.GitOut(repo, "rev-parse", "--verify", head+"^{commit}")
	if !validAuditReopenHead(current) {
		return nil, fmt.Errorf("resolve raw audit terminal %s", head)
	}
	batch, err := openAuditCommitBatch(repo)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, batch.close()) }()
	history = make([]rawAuditCommit, count)
	var rawBytes int64
	for i := count - 1; i >= 0; i-- {
		raw, readErr := batch.commit(current)
		if readErr != nil {
			return nil, readErr
		}
		if err := addAuditLinearHistoryBytes(&rawBytes, raw); err != nil {
			return nil, err
		}
		commit, parseErr := rawAuditCommitFromObject(current, raw)
		if parseErr != nil {
			return nil, parseErr
		}
		history[i] = commit
		if i > 0 {
			if commit.parent == "" {
				return nil, errors.New("audit history exceeds its raw ancestry")
			}
			current = commit.parent
		}
	}
	if history[0].parent != "" {
		raw, err := batch.commit(history[0].parent)
		if err != nil {
			return nil, err
		}
		if err := addAuditLinearHistoryBytes(&rawBytes, raw); err != nil {
			return nil, err
		}
	}
	return history, nil
}

func rawAuditHistoryUntil(
	repo, head string,
	limit int,
	stop func(rawAuditCommit) bool,
) (history []rawAuditCommit, err error) {
	current := forkspace.GitOut(repo, "rev-parse", "--verify", head+"^{commit}")
	if !validAuditReopenHead(current) {
		return nil, fmt.Errorf("resolve raw audit terminal %s", head)
	}
	batch, err := openAuditCommitBatch(repo)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, batch.close()) }()
	var rawBytes int64
	for len(history) < limit {
		raw, readErr := batch.commit(current)
		if readErr != nil {
			return nil, readErr
		}
		if err := addAuditLinearHistoryBytes(&rawBytes, raw); err != nil {
			return nil, err
		}
		commit, parseErr := rawAuditCommitFromObject(current, raw)
		if parseErr != nil {
			return nil, parseErr
		}
		history = append(history, commit)
		if stop(commit) {
			if commit.parent != "" {
				raw, err := batch.commit(commit.parent)
				if err != nil {
					return nil, err
				}
				if err := addAuditLinearHistoryBytes(&rawBytes, raw); err != nil {
					return nil, err
				}
			}
			slices.Reverse(history)
			return history, nil
		}
		if commit.parent == "" {
			return nil, errAuditBoundaryMissing
		}
		current = commit.parent
	}
	return nil, fmt.Errorf("raw audit history exceeds %d commits", limit)
}

// errAuditBoundaryMissing is a straight line of commits that ends without reaching the boundary
// commit, which is therefore not in that history.
var errAuditBoundaryMissing = errors.New("raw audit history ended before its required boundary")

func addAuditLinearHistoryBytes(total *int64, raw []byte) error {
	*total += int64(len(raw))
	if *total > auditLinearHistoryByteLimit {
		return fmt.Errorf("raw linear audit history exceeds %d bytes", auditLinearHistoryByteLimit)
	}
	return nil
}

// auditReviewedHead is the commit that ends a record's reviewed Subject and History.
func auditReviewedHead(record AuditReopenRecord) string {
	if record.ReviewedHead != "" {
		return record.ReviewedHead
	}
	return record.BaselineHead
}

func auditReviewedSubject(repo, id string, record AuditReopenRecord) (rawAuditCommit, error) {
	rawHistory, err := rawAuditHistoryCount(repo, auditReviewedHead(record), len(record.History)+1)
	if err != nil {
		return rawAuditCommit{}, err
	}
	semantics, err := semanticHistoryCommitsExact(repo, rawHistory)
	if err != nil || len(semantics) != len(record.History)+1 {
		return rawAuditCommit{}, errors.New("read raw recorded audit history")
	}
	if semantics[0].semantic != record.Subject {
		return rawAuditCommit{}, errors.New("raw audit subject does not match its recorded semantic identity")
	}
	for i := range record.History {
		if semantics[i+1].semantic != record.History[i] {
			return rawAuditCommit{}, fmt.Errorf("raw audit history commit %d does not match its recorded semantic identity", i+1)
		}
	}
	return rawHistory[0], nil
}

func auditReviewedSubjectParent(repo, id string, record AuditReopenRecord) (string, error) {
	subject, err := auditReviewedSubject(repo, id, record)
	if err != nil {
		return "", err
	}
	return subject.parent, nil
}

func rawAuditHistory(repo, head string, count int) ([]semanticHistoryCommit, string, error) {
	rawHistory, err := rawAuditHistoryCount(repo, head, count)
	if err != nil {
		return nil, "", err
	}
	semantics, err := semanticHistoryCommitsExact(repo, rawHistory)
	if err != nil {
		return nil, "", err
	}
	return semantics, rawHistory[0].parent, nil
}

func rawAuditRewriteHistory(repo, head, reviewedParent string, limit int) ([]rawAuditCommit, error) {
	return rawAuditHistoryUntil(
		repo,
		head,
		limit,
		func(commit rawAuditCommit) bool { return commit.parent == reviewedParent },
	)
}

func CaptureAuditReopen(repo, id string) (AuditReopenRecord, error) {
	head := forkspace.GitOut(repo, "rev-parse", "--verify", "HEAD^{commit}")
	if !validAuditReopenHead(head) {
		return AuditReopenRecord{}, errors.New("resolve audit reopen baseline HEAD")
	}
	bindings, ok := rawTaskBindings(repo, head)
	if !ok {
		return AuditReopenRecord{}, errors.New("read raw reachable task bindings for audit reopen")
	}
	subjects := bindings[id]
	if len(subjects) != 1 {
		return AuditReopenRecord{}, fmt.Errorf(
			"review subject %s needs exactly one reachable %s binding before reopen", id, CoopTaskTrailer,
		)
	}
	rawHistory, err := rawAuditHistoryUntil(
		repo,
		head,
		auditReopenHistoryLimit+1,
		func(commit rawAuditCommit) bool { return commit.sha == subjects[0] },
	)
	if err != nil {
		return AuditReopenRecord{}, err
	}
	commits, err := semanticHistoryCommitsExact(repo, rawHistory)
	if err != nil {
		return AuditReopenRecord{}, err
	}
	if len(commits) == 0 || commits[0].semantic.TaskID != id {
		return AuditReopenRecord{}, fmt.Errorf("resolve raw review subject %s", id)
	}
	seen := map[string]bool{id: true}
	history := make([]AuditReopenCommit, 0, len(commits)-1)
	for _, commit := range commits[1:] {
		taskID := commit.semantic.TaskID
		if taskID != "" && (seen[taskID] || len(bindings[taskID]) != 1) {
			return AuditReopenRecord{}, fmt.Errorf(
				"descendant task %s needs exactly one reachable %s binding before review reopens %s",
				taskID, CoopTaskTrailer, id,
			)
		}
		if taskID != "" {
			seen[taskID] = true
		}
		history = append(history, commit.semantic)
	}
	generation := newAuditReopenGeneration()
	return AuditReopenRecord{
		Version: auditReopenVersion, Generation: generation, TaskID: id, BaselineHead: head,
		Subject: commits[0].semantic, History: history,
	}, nil
}

func auditReopenCurrentHistory(repo, head, id string, record AuditReopenRecord) ([]semanticHistoryCommit, error) {
	if err := validateAuditReopenRecord(record, id); err != nil {
		return nil, fmt.Errorf("validate persisted audit authority: %w", err)
	}
	if !auditReopenRecordActive(record) {
		return nil, errors.New("audit authority is not active")
	}
	resolvedHead := forkspace.GitOut(repo, "rev-parse", "--verify", head+"^{commit}")
	if !validAuditReopenHead(resolvedHead) {
		return nil, fmt.Errorf("resolve current audit HEAD %s", head)
	}
	reviewedSubject, err := auditReviewedSubject(repo, id, record)
	if err != nil {
		return nil, fmt.Errorf("locate recorded raw audit subject: %w", err)
	}
	rawCurrent, err := rawAuditHistoryUntil(
		repo,
		resolvedHead,
		auditReopenHistoryLimit+1,
		func(commit rawAuditCommit) bool { return commit.sha == reviewedSubject.sha },
	)
	if err != nil {
		return nil, fmt.Errorf("read current raw audit history: %w", err)
	}
	rawCurrent, err = auditReopenWithoutAccepted(rawCurrent, id, record)
	if err != nil {
		return nil, err
	}
	complete, err := semanticHistoryCommitsExact(repo, rawCurrent)
	if err != nil {
		return nil, fmt.Errorf("read current complete audit history: %w", err)
	}
	if len(complete) < len(record.History)+1 {
		return nil, fmt.Errorf(
			"current audit history has %d commits, shorter than recorded prefix %d",
			len(complete)-1, len(record.History),
		)
	}
	if complete[0].semantic != record.Subject {
		return nil, fmt.Errorf("task %s subject no longer matches its recorded semantic identity", id)
	}
	for i := range record.History {
		if complete[i+1].semantic != record.History[i] {
			return nil, fmt.Errorf("current audit history commit %d does not match the recorded prefix", i+1)
		}
	}
	if complete[len(record.History)].sha != auditReviewedHead(record) {
		return nil, fmt.Errorf("recorded history prefix does not terminate at baseline %s", auditReviewedHead(record))
	}
	bindings, ok := rawTaskBindings(repo, resolvedHead)
	if !ok {
		return nil, errors.New("read reachable task bindings for current audit history")
	}
	for _, commit := range complete {
		if commit.semantic.TaskID != "" &&
			len(bindings[commit.semantic.TaskID]) != 1 {
			return nil, fmt.Errorf("task %s no longer has exactly one reachable history binding", commit.semantic.TaskID)
		}
	}
	// The raw parse above judged every commit after the baseline, and a moved record's accepted
	// commits, by their last paragraph alone; Git, which reads bindings everywhere else, may not agree.
	if err := auditReopenGitBindings(repo, resolvedHead, id, record, bindings); err != nil {
		return nil, err
	}
	return complete[1:], nil
}

// auditReopenGitBindings refuses when Git's own reading of HEAD's history binds the task, or a task
// of its History, to anything but the one commit the raw parse binds it to. Git skips trailing
// comment lines and everything after a scissors line, which the raw parse takes as the message's
// last paragraph, so a commit can bind a task for Git alone.
func auditReopenGitBindings(repo, head, id string, record AuditReopenRecord, raw map[string][]string) error {
	commits, err := TaskTrailerCommits(repo, head, false)
	if err != nil {
		return fmt.Errorf("read the commits Git binds to tasks in HEAD's history: %w", err)
	}
	tasks := []string{id}
	for _, later := range record.History {
		if later.TaskID != "" {
			tasks = append(tasks, later.TaskID)
		}
	}
	for _, task := range tasks {
		var read []string
		for _, commit := range commits {
			if !commit.Malformed && len(commit.Values) == 1 && commit.Values[0] == task {
				read = append(read, commit.Info.SHA)
			}
		}
		if len(read) == 1 && len(raw[task]) == 1 && strings.HasPrefix(raw[task][0], read[0]) {
			continue
		}
		shown := "none"
		if len(read) > 0 {
			shown = shortCommits(read)
		}
		return fmt.Errorf("the review recorded one commit for task %s, but Git reads %s as %s: %s in HEAD's history",
			bindingLabel(task), shown, CoopTaskTrailer, bindingLabel(task))
	}
	return nil
}

// auditReopenWithoutAccepted leaves out of chain, the raw history from the reviewed subject to HEAD,
// the commits a person's unblock accepted after the reviewed history (see reanchoredAuditReopen).
// They belong to other work and may bind other tasks more than once, so they are never replayed;
// each must still name neither this task nor a task of its History, and the chain must still pass
// through BaselineHead, where rework starts. An ordinary record accepted none.
func auditReopenWithoutAccepted(chain []rawAuditCommit, id string, record AuditReopenRecord) ([]rawAuditCommit, error) {
	if record.ReviewedHead == "" {
		return chain, nil
	}
	reviewedEnd := len(record.History)
	if len(chain) <= reviewedEnd || chain[reviewedEnd].sha != record.ReviewedHead {
		return nil, fmt.Errorf("recorded history prefix does not terminate at %s", record.ReviewedHead)
	}
	for i := reviewedEnd + 1; i < len(chain); i++ {
		if err := auditReopenUnrelated(chain[i], id, record); err != nil {
			return nil, err
		}
		if chain[i].sha == record.BaselineHead {
			return slices.Concat(chain[:reviewedEnd+1], chain[i+1:]), nil
		}
	}
	return nil, fmt.Errorf("HEAD no longer contains commit %s, where rework on task %s starts", shortCommit(record.BaselineHead), id)
}

// auditReopenUnrelated refuses a commit after a reopened task's reviewed history that names the task,
// or a task its History records, in any Coop-Task trailer: such a commit is not other work.
func auditReopenUnrelated(commit rawAuditCommit, id string, record AuditReopenRecord) error {
	for _, value := range commit.taskValues {
		if value == id {
			return fmt.Errorf("commit %s, made after the review, also carries %s: %s",
				shortCommit(commit.sha), CoopTaskTrailer, bindingLabel(id))
		}
		for _, later := range record.History {
			if later.TaskID != "" && later.TaskID == value {
				return fmt.Errorf("commit %s, made after the review, also carries %s: %s, a task the review recorded after this one",
					shortCommit(commit.sha), CoopTaskTrailer, bindingLabel(value))
			}
		}
	}
	return nil
}

func AuditReopenCurrentValid(repo, head, id string, record AuditReopenRecord) bool {
	_, err := auditReopenCurrentHistory(repo, head, id, record)
	return err == nil
}

// auditReopenAppendedRepair accepts the boring rework shape: keep the reviewed history intact and
// add one real, unbound repair commit. The host's reopen record supplies task authority, so adding
// another Coop-Task trailer would only create a duplicate binding.
func auditReopenAppendedRepair(repo, base, head, id string, record AuditReopenRecord) ([]semanticHistoryCommit, bool) {
	resolvedBase := forkspace.GitOut(repo, "rev-parse", "--verify", base+"^{commit}")
	if !validAuditReopenHead(resolvedBase) {
		return nil, false
	}
	raw, err := rawAuditHistoryCount(repo, head, 1)
	if err != nil || len(raw) != 1 || raw[0].parent != resolvedBase ||
		raw[0].taskBindingInvalid || len(raw[0].taskValues) != 0 {
		return nil, false
	}
	baseTree, err := auditCommitTree(repo, resolvedBase)
	if err != nil || raw[0].tree == baseTree {
		return nil, false
	}
	baseHistory, err := auditReopenCurrentHistory(repo, resolvedBase, id, record)
	if err != nil {
		return nil, false
	}
	current, err := auditReopenCurrentHistory(repo, head, id, record)
	if err != nil || len(current) != len(baseHistory)+1 {
		return nil, false
	}
	return current, true
}

func auditReopenCompletionValid(repo, base, head, id string, record AuditReopenRecord) bool {
	if base == head {
		return AuditReopenCurrentValid(repo, head, id, record)
	}
	if _, ok := auditReopenAppendedRepair(repo, base, head, id, record); ok {
		return true
	}
	// The commits a re-anchored record accepted belong to other work. A rewrite of the subject would
	// have to replay them, and the check below compares only the history the record still replays,
	// so it would let a rewrite drop them all.
	if record.ReviewedHead != "" {
		return false
	}
	// A rewrite generation authorizes a transition only from the exact semantic state the host
	// reviewed. Without this baseline check, a raw-moved stale record could authorize a second
	// rewrite merely because the new subject differed from the much older recorded tree.
	baseHistory, err := auditReopenCurrentHistory(repo, base, id, record)
	if err != nil {
		return false
	}
	reviewedParent, err := auditReviewedSubjectParent(repo, id, record)
	if err != nil {
		return false
	}
	bindings, ok := rawTaskBindings(repo, head)
	if !ok || len(bindings[id]) != 1 {
		return false
	}
	rangeCommits, rewrittenParent, err := rawAuditHistory(repo, head, len(baseHistory)+1)
	if err != nil || rewrittenParent != reviewedParent {
		return false
	}
	if len(rangeCommits) != len(baseHistory)+1 || rangeCommits[0].semantic.TaskID != id ||
		rangeCommits[0].semantic.ChangeTree == record.Subject.ChangeTree {
		return false
	}
	for i := range baseHistory {
		if rangeCommits[i+1].semantic != baseHistory[i].semantic {
			return false
		}
	}
	for _, commit := range rangeCommits {
		if commit.semantic.TaskID != "" &&
			len(bindings[commit.semantic.TaskID]) != 1 {
			return false
		}
	}
	return true
}

func rebasedAuditReopenRecord(repo, base, head, id string, record AuditReopenRecord) (AuditReopenRecord, error) {
	if !auditReopenCompletionValid(repo, base, head, id, record) {
		return AuditReopenRecord{}, fmt.Errorf("audit rework for task %s changed history outside the host authority", id)
	}
	if current, ok := auditReopenAppendedRepair(repo, base, head, id, record); ok {
		rebased := record
		rebased.UnblockPending = false
		rebased.Repaired = true
		if record.ReviewedHead != "" {
			// A re-anchored record keeps its baseline and the repair stays a later commit. Moving the
			// baseline onto the repair would pin a commit the loop signs right away, and a re-anchored
			// record cannot be found again by trailer after signing, as an ordinary one is.
			return rebased, nil
		}
		history := make([]AuditReopenCommit, len(current))
		for i := range current {
			history[i] = current[i].semantic
		}
		rebased.BaselineHead = forkspace.GitOut(repo, "rev-parse", "--verify", head+"^{commit}")
		rebased.History = history
		return rebased, nil
	}
	baseHistory, err := auditReopenCurrentHistory(repo, base, id, record)
	if err != nil {
		return AuditReopenRecord{}, err
	}
	replacement, _, err := rawAuditHistory(repo, head, len(baseHistory)+1)
	if err != nil {
		return AuditReopenRecord{}, err
	}
	if len(replacement) != len(baseHistory)+1 {
		return AuditReopenRecord{}, fmt.Errorf("resolve complete audit rewrite for task %s", id)
	}
	history := make([]AuditReopenCommit, len(replacement)-1)
	for i := range history {
		history[i] = replacement[i+1].semantic
	}
	baselineHead := forkspace.GitOut(repo, "rev-parse", "--verify", head+"^{commit}")
	if !validAuditReopenHead(baselineHead) {
		return AuditReopenRecord{}, fmt.Errorf("resolve rebased audit HEAD for task %s", id)
	}
	rebased := record
	rebased.Version = auditReopenVersion
	rebased.BaselineHead = baselineHead
	rebased.Subject = replacement[0].semantic
	rebased.History = history
	rebased.UnblockPending = false
	rebased.Repaired = true
	return rebased, nil
}

type auditCompletionRecoveryError interface {
	error
	auditCompletionRecovery()
}

type auditCompletionStateError struct{ message string }

func (e *auditCompletionStateError) Error() string          { return e.message }
func (*auditCompletionStateError) auditCompletionRecovery() {}

// rebaseBlockedAuditReopen recovers accepted rework from the exact baseline recorded by the host,
// for a person's explicit unblock only. A single appended repair is preferred. When HEAD still
// reaches the end of the reviewed history in a straight line, that history is unchanged and the
// commits after it decide alone (movedAuditReopen). The complete-rewrite path remains for older
// attempts, never for a moved record.
func rebaseBlockedAuditReopen(repo, head, id string, record AuditReopenRecord) (AuditReopenRecord, error) {
	if validateAuditReopenRecord(record, id) != nil || !auditReopenRecordActive(record) {
		return AuditReopenRecord{}, fmt.Errorf("invalid audit reopen authority for task %s", id)
	}
	reviewed := unmovedAuditReopen(record)
	if !AuditReopenCurrentValid(repo, reviewed.BaselineHead, id, reviewed) {
		return AuditReopenRecord{}, fmt.Errorf(
			"the review's record of task %s no longer matches its recorded baseline %s: the record was changed, or that commit is gone",
			id, reviewed.BaselineHead,
		)
	}
	// Checked against the record itself, so a moved record stays moved: it never regains the rewrite
	// authority an ordinary one has.
	if _, ok := auditReopenAppendedRepair(repo, record.BaselineHead, head, id, record); ok {
		return rebasedAuditReopenRecord(repo, record.BaselineHead, head, id, record)
	}
	reviewedParent, err := auditReviewedSubjectParent(repo, id, reviewed)
	if err != nil {
		return AuditReopenRecord{}, fmt.Errorf("resolve blocked audit subject parent for task %s: %w", id, err)
	}
	// Reaching a child of the reviewed subject's parent first means the reviewed history is not in
	// HEAD's. Stopping there also keeps a merge below the subject out of this walk.
	landed, err := rawAuditHistoryUntil(repo, head, auditReopenHistoryLimit+1, func(commit rawAuditCommit) bool {
		return commit.sha == reviewed.BaselineHead || commit.parent == reviewedParent
	})
	where := fmt.Sprintf("commit %s, where the review left task %s", shortCommit(reviewed.BaselineHead), id)
	switch {
	case errors.Is(err, errAuditBoundaryMissing):
		return AuditReopenRecord{}, fmt.Errorf("HEAD no longer contains %s, nor the commit the task's commit was built on", where)
	case err != nil:
		return AuditReopenRecord{}, fmt.Errorf("HEAD does not reach %s through commits Coop can check: %w", where, err)
	case landed[0].sha == reviewed.BaselineHead:
		return movedAuditReopen(repo, id, reviewed, landed[1:])
	case record.ReviewedHead != "":
		return AuditReopenRecord{}, fmt.Errorf(
			"HEAD no longer contains %s: the task's commit or the commits the review recorded after it were rewritten, "+
				"and a record moved past other work accepts no rewrite", where)
	}
	replacement, err := blockedAuditRewrite(repo, head, id, record, reviewedParent)
	if err != nil {
		return AuditReopenRecord{}, fmt.Errorf("HEAD no longer contains %s: %w", where, err)
	}
	return replacement, nil
}

// unmovedAuditReopen is record as the review left it, before any unblock moved it past later work.
func unmovedAuditReopen(record AuditReopenRecord) AuditReopenRecord {
	if record.ReviewedHead != "" {
		record.BaselineHead, record.ReviewedHead = record.ReviewedHead, ""
	}
	return record
}

// movedAuditReopen moves where rework on a reopened task starts to the end of landed: the commits
// after its reviewed history, which HEAD reaches in a straight line. reviewed is the record as the
// review left it (unmovedAuditReopen). It refuses when one of those commits names this task or a
// task of its History, or when Git binds either to any other commit (auditReopenGitBindings).
// Subject and History stay as reviewed. The commits it accepts belong to other work, so the record
// authorizes no rewrite from then on (auditReopenCompletionValid), and it moves again from the same
// reviewed history after anything rewrote them, such as the host signing them or an amend.
func movedAuditReopen(repo, id string, reviewed AuditReopenRecord, landed []rawAuditCommit) (AuditReopenRecord, error) {
	if len(landed) == 0 {
		return reviewed, nil
	}
	unbound := false
	for _, commit := range landed {
		if err := auditReopenUnrelated(commit, id, reviewed); err != nil {
			return AuditReopenRecord{}, err
		}
		unbound = unbound || len(commit.taskValues) == 0
	}
	head := landed[len(landed)-1].sha
	moved := reviewed
	moved.ReviewedHead, moved.BaselineHead = reviewed.BaselineHead, head
	// An unbound commit may be a repair this task's worker made before it blocked, so a re-close
	// without a commit no longer counts as no work (carryAuditReopen decides the same way).
	moved.Repaired = reviewed.Repaired || unbound
	if _, err := auditReopenCurrentHistory(repo, head, id, moved); err != nil {
		return AuditReopenRecord{}, err
	}
	return moved, nil
}

// carriedMovedAuditReopen follows a moved record across the host's rewrite of the commits after its
// reviewed history, from oldHead to the current HEAD. They must come back in the same order, each
// with the same change, author and message, as a signing pass leaves them; otherwise the record
// stays where it was. The baseline moves to the rewritten copy of itself, never further, so a commit
// that landed after the rewrite still gets the strict check at the next lease.
func carriedMovedAuditReopen(repo, oldHead, id string, record AuditReopenRecord) (AuditReopenRecord, bool) {
	reviewed := unmovedAuditReopen(record)
	stop := func(commit rawAuditCommit) bool { return commit.sha == reviewed.BaselineHead }
	before, err := rawAuditHistoryUntil(repo, oldHead, auditReopenHistoryLimit+1, stop)
	if err != nil {
		return AuditReopenRecord{}, false
	}
	head := forkspace.GitOut(repo, "rev-parse", "--verify", "HEAD^{commit}")
	after, err := rawAuditHistoryUntil(repo, head, auditReopenHistoryLimit+1, stop)
	if err != nil || len(after) < len(before) {
		return AuditReopenRecord{}, false
	}
	after = after[:len(before)]
	beforeChanges, beforeErr := semanticRawHistoryChangeTrees(repo, before[1:])
	afterChanges, afterErr := semanticRawHistoryChangeTrees(repo, after[1:])
	if beforeErr != nil || afterErr != nil {
		return AuditReopenRecord{}, false
	}
	anchor := 0
	for i := 1; i < len(before); i++ {
		was, now := before[i], after[i]
		if beforeChanges[i-1] != afterChanges[i-1] || was.authorName != now.authorName ||
			was.authorEmail != now.authorEmail || was.authorDate != now.authorDate || was.commitMessage != now.commitMessage {
			return AuditReopenRecord{}, false
		}
		if was.sha == record.BaselineHead {
			anchor = i
		}
	}
	if anchor == 0 {
		return AuditReopenRecord{}, false
	}
	moved, err := movedAuditReopen(repo, id, reviewed, after[1:anchor+1])
	if err != nil {
		return AuditReopenRecord{}, false
	}
	return moved, true
}

// blockedAuditRewrite accepts the complete rewrite an older attempt left: a new version of the
// reviewed subject on its exact parent, followed by an exact replay of the recorded history.
func blockedAuditRewrite(repo, head, id string, record AuditReopenRecord, reviewedParent string) (AuditReopenRecord, error) {
	replaced := errors.New("the task's commit was replaced, but not by a new version of it followed by the same later commits")
	current, err := rawAuditRewriteHistory(repo, head, reviewedParent, auditReopenHistoryLimit+1)
	rewriteLen := len(record.History) + 1
	if err != nil || len(current) < rewriteLen {
		return AuditReopenRecord{}, replaced
	}
	replacement, err := rebasedAuditReopenRecord(repo, record.BaselineHead, current[rewriteLen-1].sha, id, record)
	if err != nil {
		return AuditReopenRecord{}, replaced
	}
	if _, err := auditReopenCurrentHistory(repo, head, id, replacement); err != nil {
		return AuditReopenRecord{}, fmt.Errorf("the task's commit was rewritten, but the commits after the rewrite cannot be checked: %w", err)
	}
	return replacement, nil
}

// auditReopenForUnblock is record as a person's unblock activates it at head: unchanged while it
// still holds there, else rebased over the rework or the later commits HEAD gained.
func auditReopenForUnblock(repo, head, id string, record AuditReopenRecord) (AuditReopenRecord, error) {
	if AuditReopenCurrentValid(repo, head, id, record) {
		return record, nil
	}
	return rebaseBlockedAuditReopen(repo, head, id, record)
}

type blockedAuditUnblock struct {
	authority                      *os.File
	root                           string
	previous, pending, replacement *AuditReopenRecord // previous is nil when this unblock grants new authority
	pendingPersisted               bool
	committedBlock                 string // the task whose committed-block note this unblock spends
}

// movedNote is the task log's account of an unblock that moved the record past later work, or ""
// when it did not move it.
func (u *blockedAuditUnblock) movedNote() string {
	if u == nil || u.previous == nil || u.replacement == nil || u.replacement.ReviewedHead == "" ||
		u.replacement.BaselineHead == u.previous.BaselineHead {
		return ""
	}
	return fmt.Sprintf("Unblock accepted the commits after %s, which leave this task alone; its rework starts at %s",
		shortCommit(u.replacement.ReviewedHead), shortCommit(u.replacement.BaselineHead))
}

func (u *blockedAuditUnblock) markPending() error {
	if u == nil || u.pending == nil {
		return nil
	}
	if u.previous == nil {
		if _, exists, err := ReadAuditReopenRecord(u.root, u.pending.TaskID); err != nil || exists {
			return errors.Join(err, fmt.Errorf("audit reopen authority changed while unblocking task %s", u.pending.TaskID))
		}
		if err := WriteAuditReopenRecord(u.root, *u.pending); err != nil {
			return err
		}
	} else if err := replaceAuditReopenRecordIfMatches(u.root, *u.previous, *u.pending); err != nil {
		return err
	}
	u.pendingPersisted = true
	return nil
}

func (u *blockedAuditUnblock) persist() error {
	if u == nil || u.replacement == nil {
		return nil
	}
	previous := u.previous
	if u.pendingPersisted {
		previous = u.pending
	}
	if err := replaceAuditReopenRecordIfMatches(u.root, *previous, *u.replacement); err != nil {
		return err
	}
	return nil
}

func (u *blockedAuditUnblock) restorePrevious() error {
	if u == nil || !u.pendingPersisted {
		return nil
	}
	if u.previous == nil {
		return removeAuditReopenRecordIfMatches(u.root, u.pending.TaskID, u.pending.Generation)
	}
	return replaceAuditReopenRecordIfMatches(u.root, *u.pending, *u.previous)
}

// finish releases the authority flock after the caller has either completed the transition or
// restored the folder state. A durable pending bit makes every crash boundary unusable as
// completion authority while retaining enough host proof for the next lease or explicit retry.
func (u *blockedAuditUnblock) finish(operationErr error) error {
	if u == nil {
		return operationErr
	}
	return errors.Join(operationErr, unlockLeaseFile(u.authority))
}

// prepareBlockedAuditReopenUnblock validates a blocked task and computes any required authority rebase
// of the same host generation. The returned transaction holds the authority flock; before moving,
// it persists a non-authorizing pending form that makes a crash safe and recoverable.
func prepareBlockedAuditReopenUnblock(project, root string, task Item) (*blockedAuditUnblock, error) {
	observed, ok, err := ReadAuditReopenRecord(root, task.ID)
	if err != nil {
		return nil, fmt.Errorf("read blocked audit reopen authority for task %s: %w", task.ID, err)
	}
	if !ok {
		authority, err := lockLeaseAuthority(root, task.ID, true, syscall.LOCK_EX|syscall.LOCK_NB)
		if err != nil {
			return nil, fmt.Errorf("lock blocked task authority for task %s: %w", task.ID, err)
		}
		transition := &blockedAuditUnblock{authority: authority, root: root}
		current, currentOK, currentErr := CurrentTask(root, task.ID)
		if currentErr != nil {
			return nil, transition.finish(currentErr)
		}
		if !currentOK || current.State != StateBlocked || current.Dir != task.Dir {
			return nil, transition.finish(fmt.Errorf("task %s changed state while its blocked authority was locked", task.ID))
		}
		if err := transition.answerCommittedBlock(project, task.ID); err != nil {
			return nil, transition.finish(err)
		}
		return transition, nil
	}
	return lockBlockedAuditReopenUnblock(project, root, task, observed)
}

func lockBlockedAuditReopenUnblock(project, root string, task Item, observed AuditReopenRecord) (*blockedAuditUnblock, error) {
	authority, err := lockLeaseAuthority(root, task.ID, false, syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		return nil, fmt.Errorf("lock blocked audit reopen authority for task %s: %w", task.ID, err)
	}
	transition := &blockedAuditUnblock{authority: authority, root: root}
	fail := func(err error) (*blockedAuditUnblock, error) {
		return nil, transition.finish(err)
	}
	record, ok, err := ReadAuditReopenRecord(root, task.ID)
	if err != nil {
		return fail(fmt.Errorf("re-read blocked audit reopen authority for task %s: %w", task.ID, err))
	}
	if !ok || !AuditReopenRecordsEqual(record, observed) {
		return fail(fmt.Errorf("audit reopen authority changed while unblocking task %s", task.ID))
	}
	current, ok, err := CurrentTask(root, task.ID)
	if err != nil {
		return fail(err)
	}
	if !ok || current.State != StateBlocked || current.Dir != task.Dir {
		return fail(fmt.Errorf("task %s changed state while its blocked audit authority was locked", task.ID))
	}
	repo, head := auditHistory(project)
	if repo == "" || head == "" {
		return fail(fmt.Errorf("resolve repository history for blocked audit task %s", task.ID))
	}
	if record.UnblockPending {
		active := record
		active.Version = auditReopenVersion
		active.UnblockPending = false
		replacement, err := auditReopenForUnblock(repo, head, task.ID, active)
		if err != nil {
			return fail(fmt.Errorf("pending audit unblock for task %s no longer matches repository history: %w", task.ID, err))
		}
		transition.previous = &record
		transition.replacement = &replacement
		return transition, nil
	}
	replacement := record
	if auditReopenCompletionValid(repo, head, head, task.ID, record) {
		if record.DisputedReceipt == "" {
			return transition, nil
		}
	} else {
		replacement, err = rebaseBlockedAuditReopen(repo, head, task.ID, record)
		if err != nil {
			return fail(err)
		}
	}
	if record.DisputedReceipt != "" {
		// A human's resolution starts a new attempt, not another automatic dispute cycle.
		replacement.Finding, replacement.DisputedReceipt, replacement.Repaired = "", "", false
	}
	pending := replacement
	pending.Version = auditReopenPendingVersion
	pending.UnblockPending = true
	transition.previous = &record
	transition.pending = &pending
	transition.replacement = &replacement
	return transition, nil
}

// finishPendingAuditUnblock is the explicit host recovery for a crash after the authorized folder
// move. Pending authority never self-activates from queue state or lease acquisition.
func finishPendingAuditUnblock(project, root string, task Item, observed AuditReopenRecord) (bool, error) {
	authority, err := lockLeaseAuthority(root, task.ID, false, syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		return false, fmt.Errorf("lock pending audit unblock authority for task %s: %w", task.ID, err)
	}
	fail := func(err error) (bool, error) {
		return false, errors.Join(err, unlockLeaseFile(authority))
	}
	record, ok, err := ReadAuditReopenRecord(root, task.ID)
	if err != nil {
		return fail(fmt.Errorf("re-read pending audit unblock authority for task %s: %w", task.ID, err))
	}
	if !ok || !record.UnblockPending || !AuditReopenRecordsEqual(record, observed) {
		return fail(fmt.Errorf("pending audit unblock authority changed for task %s", task.ID))
	}
	current, ok, err := CurrentTask(root, task.ID)
	if err != nil {
		return fail(err)
	}
	if !ok || current.State != StateTodo || current.Dir != task.Dir {
		return fail(fmt.Errorf("task %s is no longer the todo task from its pending audit unblock", task.ID))
	}
	repo, head := auditHistory(project)
	if repo == "" || head == "" {
		return fail(fmt.Errorf("resolve repository history for the pending audit unblock of task %s", task.ID))
	}
	active := record
	active.Version = auditReopenVersion
	active.UnblockPending = false
	replacement, err := auditReopenForUnblock(repo, head, task.ID, active)
	if err != nil {
		return fail(fmt.Errorf("pending audit unblock for task %s no longer matches repository history: %w", task.ID, err))
	}
	if _, err := reconcileTodoUnblockOwnerLocked(root, task, true); err != nil {
		return fail(err)
	}
	if err := replaceAuditReopenRecordIfMatches(root, record, replacement); err != nil {
		return fail(err)
	}
	if err := unlockLeaseFile(authority); err != nil {
		return true, err
	}
	return true, nil
}

// reconcileTodoUnblockOwnerLocked completes the owner half of a crash-interrupted unblock while
// the caller holds the canonical task-authority flock. Fork ownership is retained but paused;
// human ownership is removed only when an audit-pending record proves this really is an unblock.
func reconcileTodoUnblockOwnerLocked(root string, task Item, allowHuman bool) (bool, error) {
	current, ok, err := CurrentTask(root, task.ID)
	if err != nil {
		return false, err
	}
	if !ok || current.State != StateTodo || current.Dir != task.Dir {
		return false, fmt.Errorf("task %s changed while recovering its unblock owner", task.ID)
	}
	ownerLock, err := lockTaskOwner(root, task.ID)
	if err != nil {
		return false, err
	}
	defer ownerLock.Close()
	record, owned, err := ownerLock.Read()
	if err != nil || !owned {
		return false, err
	}
	if record.Kind != TaskOwnerFork {
		if !allowHuman {
			return false, nil
		}
		return true, removeTaskOwnerRecordFile(root, task.ID)
	}
	if record.Fork == nil || (record.Fork.Phase != ForkAssignmentBlocked && record.Fork.Phase != ForkAssignmentPaused) {
		return false, fmt.Errorf("fork assignment for task %s is %s, not a recoverable unblock", task.ID, func() ForkAssignmentPhase {
			if record.Fork == nil {
				return ""
			}
			return record.Fork.Phase
		}())
	}
	if record.Fork.Phase == ForkAssignmentPaused {
		return true, nil
	}
	record.Fork.Phase = ForkAssignmentPaused
	record.Fork.CandidateID = ""
	record.Fork.ProjectionDigest = ""
	record.Fork.UpdatedAt = time.Now().UTC()
	return true, ownerLock.Write(record)
}

func finishInterruptedForkUnblock(root string, task Item) (bool, error) {
	authority, err := lockLeaseAuthority(root, task.ID, true, syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		return false, err
	}
	committed, reconcileErr := reconcileTodoUnblockOwnerLocked(root, task, false)
	return committed, errors.Join(reconcileErr, unlockLeaseFile(authority))
}

// PreserveBlockedAuditReopen rebases an accepted review rewrite without consuming its single-use
// generation when the worker parks the task for external acceptance. The same semantic replay
// validation as completion prevents the provider from changing descendants, and the still-held
// authority lock makes replacement of the host record fail closed.
func (l *TaskLease) PreserveBlockedAuditReopen(repo, base, head string) error {
	if l.Reopen == nil || base == head {
		return nil
	}
	current, ok, err := CurrentTask(l.root, l.id)
	if err != nil {
		return err
	}
	if !ok || current.State != StateBlocked {
		return nil
	}
	replacement, err := rebasedAuditReopenRecord(repo, base, head, l.id, *l.Reopen)
	if err != nil {
		return err
	}
	if err := replaceAuditReopenRecordIfMatches(l.root, *l.Reopen, replacement); err != nil {
		return err
	}
	l.Reopen = &replacement
	return nil
}

// RebaseTimedOutAuditReopen keeps held audit authority truthful after a timed-out attempt.
// An unchanged tree leaves the recorded baseline valid. A tree-changing attempt is accepted
// only when it is a complete, semantically valid rewrite of the reviewed subject — then the
// authority rebases onto the new head without consuming its single-use generation. Anything
// else errors so the caller can park the task fail-closed instead of retrying on a tree the
// audit never authorized.
func (l *TaskLease) RebaseTimedOutAuditReopen(repo, base, head string) error {
	if l.Reopen == nil || base == head {
		return nil
	}
	replacement, err := rebasedAuditReopenRecord(repo, base, head, l.id, *l.Reopen)
	if err != nil {
		return err
	}
	if err := replaceAuditReopenRecordIfMatches(l.root, *l.Reopen, replacement); err != nil {
		return err
	}
	l.Reopen = &replacement
	return nil
}

// CompletionUnbindableTasks is unbindableTasks' entry point from the work loop. Under ordinary
// authority it defers straight to unbindableTasks, threading touched through unchanged. A lease
// carrying host audit-reopen authority for exactly the finished task instead validates by semantic
// replay (auditReopenCompletionValid): that authority already proves the descendant history is a
// faithful reproduction, which trailer counting would otherwise reject as the very foreign-binding
// shape reopened work necessarily has. touched is unused on that branch — replay validation subsumes
// the foreign-binding question for it — so it always returns a nil tolerated set.
func CompletionUnbindableTasks(repo, base, head string, finished []string, reopen *AuditReopenRecord, touched map[string]string) (missing, tolerated []string) {
	if reopen == nil || len(finished) != 1 || finished[0] != reopen.TaskID {
		return unbindableTasks(repo, base, head, finished, touched)
	}
	if auditReopenCompletionValid(repo, base, head, finished[0], *reopen) &&
		ordinaryBindingMatchesRaw(repo, head, finished[0]) {
		return nil, nil
	}
	return slices.Clone(finished), nil
}

func ordinaryBindingMatchesRaw(repo, head, id string) bool {
	ordinary := CommitsForTask(repo, head, id)
	raw, ok := rawTaskBindings(repo, head)
	if !ok || len(ordinary) != 1 || len(raw[id]) != 1 {
		return false
	}
	resolved := forkspace.GitOut(repo, "rev-parse", "--verify", ordinary[0]+"^{commit}")
	return resolved == raw[id][0]
}

// unbindableTasks returns finished ids without exactly one Coop-Task binding both in this
// iteration's range and across history reachable from the proposed HEAD. Reopened work therefore
// has to rewrite its existing bound commit instead of adding a second one. A no-HEAD-change
// completion always fails closed — a zero-commit close is valid ONLY under fresh host audit
// authority, so task prose claiming a reopen can never buy one; crash recovery restores it for a
// fresh range.
//
// A commit in range that binds a task OUTSIDE finished is foreign. It fails the WHOLE completion
// closed when it names a task this iteration's authority consumption could touch: touched is
// host-side knowledge the box cannot influence (built once per attempt by internal/loop from the
// leased task id, the audit-reopen record's task, every id whose queue state this completion window
// observed change, and every id windows.BaselineDoneIDs() reports — every task already archived
// before the box ever ran), each mapped to why it counts, the clause a refusal quotes.
// That last member matters even when the archived task's folder never moves: its history is meant to
// be closed, so a forged extra commit corrupts that closed record without needing to touch its
// folder at all — unbindableTasks alone cannot tell "genuinely untracked" from "already closed" by
// git content alone, so the caller must supply both. That is the guard's actual job
// (.agent/kb/loop-range-rejects-outside-commits.md): stop an iteration from smuggling in authority
// over a task it could touch, not censor every unrelated trailer that happens to share the range by
// timing. A foreign binding for an id never seen reachable before base is the genuinely unrelated
// case — a human's own commit landing mid-iteration, for a task this queue never tracked at all or
// that is still live and untouched — so outside touched it is tolerated: named in the returned
// tolerated slice for the caller to report and journal, while the completion proceeds unrejected. A
// foreign binding that instead REPLACES one already reachable from base (its old commit no longer
// reachable from head) is never tolerated regardless of touched: that can only happen if an ancestor
// was rewritten, which means some OTHER task's already-bound commit was reparented — silently
// altering another task's committed content without ever moving its queue folder is exactly the
// smuggling this guard exists to stop.
func unbindableTasks(repo, base, head string, finished []string, touched map[string]string) (missing, tolerated []string) {
	if base == "" || head == "" || base == head {
		return slices.Clone(finished), nil
	}
	scan, ok := scanTaskBindings(repo, base, head, "", allowedTasks(finished), touched)
	if !ok {
		return slices.Clone(finished), nil
	}
	for _, id := range finished {
		if scan.inRange[id] != 1 || scan.reachable[id] != 1 || scan.invalid[id] != "" {
			missing = append(missing, id)
		}
	}
	return missing, scan.tolerated
}

func allowedTasks(ids []string) map[string]bool {
	allowed := make(map[string]bool, len(ids))
	for _, id := range ids {
		allowed[id] = true
	}
	return allowed
}

// UnbindableTaskProblems says why unbindableTasks found ids unbindable over base..head, one sentence
// each, naming the commits and task ids, so a refusal can say what broke the binding instead of only
// that something did. It reads the same immutable objects again, so it sees what the check saw.
func UnbindableTaskProblems(repo, base, head string, ids []string, touched map[string]string) []string {
	if base != "" && base == head {
		problems := make([]string, 0, len(ids))
		for _, id := range ids {
			problems = append(problems, fmt.Sprintf("this attempt made no commit, so nothing new binds task %s", bindingLabel(id)))
		}
		return problems
	}
	scan, ok := scanTaskBindings(repo, base, head, "", allowedTasks(ids), touched)
	if !ok {
		return scan.refused
	}
	var problems []string
	for _, id := range ids {
		if problem := scan.countProblem(id); problem != "" {
			problems = append(problems, problem)
		}
	}
	return problems
}

// AttemptBindingsRefused applies unbindableTasks' refusal to one ordinary work attempt, base..head,
// whether or not it completed its task. Checking only completing attempts let a box forge or
// rewrite a binding in an attempt that then failed, below every later attempt's base. The assigned
// task's own bindings are its count check's business, not a refusal here. runBase, the run's
// recorded base, also counts as a base for rewrites, so a binding rewritten below base outside any
// checked attempt still refuses a trailer that replaces it. refused is nil when the attempt passes,
// else one sentence per commit that broke it, naming the commit, the task ids and why they count.
func AttemptBindingsRefused(repo, runBase, base, head, assigned string, touched map[string]string) (tolerated, refused []string) {
	if base != "" && base == head {
		return nil, nil
	}
	scan, _ := scanTaskBindings(repo, base, head, runBase, map[string]bool{assigned: true}, touched)
	return scan.tolerated, scan.refused
}

// taskBindingScan is what one walk over the Coop-Task trailers head reaches learns about allowed
// ids, split at base.
type taskBindingScan struct {
	tolerated          []string            // sorted outside ids whose in-range trailers were tolerated
	refused            []string            // why the scan failed, one sentence per offending commit
	reachable, inRange map[string]int      // single valid bindings per allowed id
	bound              map[string][]string // each allowed id's single valid bindings, walked from head
	newBound           map[string][]string // the in-range subset of bound
	invalid            map[string]string   // allowed ids on an invalid or shared reachable trailer, by commit
}

// countProblem says why id lacks exactly one valid binding both in range and reachable, or "".
func (scan taskBindingScan) countProblem(id string) string {
	label := bindingLabel(id)
	if commit := scan.invalid[id]; commit != "" {
		return fmt.Sprintf("commit %s names task %s in a trailer block that also names another task or cannot be read, so it binds nothing", shortCommit(commit), label)
	}
	older := slices.DeleteFunc(slices.Clone(scan.bound[id]), func(sha string) bool { return slices.Contains(scan.newBound[id], sha) })
	switch added := scan.newBound[id]; {
	case len(added) == 0 && len(older) == 0:
		return fmt.Sprintf("no commit carries the trailer %s", CoopTaskTrailer+": "+label)
	case len(added) == 0:
		return fmt.Sprintf("task %s's commit %s is from before this attempt, which added no commit with its trailer", label, shortCommits(older))
	case len(added) > 1:
		return fmt.Sprintf("this attempt added %d commits carrying %s (%s); a task binds one commit, so make the follow-ups unbound or file a new task for them", len(added), CoopTaskTrailer+": "+label, shortCommits(added))
	case len(older) > 0:
		return fmt.Sprintf("task %s already has commit %s carrying its trailer, and this attempt added %s; a task binds one commit, so make the follow-up unbound or file a new task for it", label, shortCommits(older), shortCommits(added))
	}
	return ""
}

// The most commits or problems a refusal names before it says how many more there are: a box can
// plant any number.
const bindingProblemsShown = 5

// bindingLabel is a trailer value fit to quote: a box writes trailers, so hidden characters are
// escaped and a long value is cut.
func bindingLabel(value string) string {
	const maxRunes = 80
	if runes := []rune(value); len(runes) > maxRunes {
		value = string(runes[:maxRunes]) + "…"
	}
	return ui.SafeInline(value)
}

func shortCommit(sha string) string { return sha[:min(12, len(sha))] }

func shortCommits(shas []string) string {
	shown := make([]string, 0, min(len(shas), bindingProblemsShown)+1)
	for i, sha := range shas {
		if i == bindingProblemsShown {
			shown = append(shown, fmt.Sprintf("and %d more", len(shas)-i))
			break
		}
		shown = append(shown, shortCommit(sha))
	}
	return strings.Join(shown, ", ")
}

func bindingLabels(values []string) string {
	labels := make([]string, len(values))
	for i, value := range values {
		labels[i] = bindingLabel(value)
	}
	return strings.Join(labels, ", ")
}

// scanTaskBindings walks the history head reaches. ok is false when that history cannot be read or
// a commit in base..head carries a binding no attempt may add: an invalid or several trailers, or a
// trailer outside allowed for a touched task or for one whose single bound commit base (or
// runBase, when set) reached and head no longer does. refused then says why, one sentence per
// offending commit, so the scan walks on to name each of them.
func scanTaskBindings(repo, base, head, runBase string, allowed map[string]bool, touched map[string]string) (scan taskBindingScan, ok bool) {
	unreadable := func(err error) (taskBindingScan, bool) {
		return taskBindingScan{refused: []string{fmt.Sprintf("Coop could not read this attempt's commits: %v", err)}}, false
	}
	if base == "" || head == "" {
		return unreadable(errors.New("the range has no commit at one end"))
	}
	headCommits, headErr := rawReachableAuditCommits(repo, head)
	baseCommits, baseErr := rawReachableAuditCommits(repo, base)
	if err := errors.Join(headErr, baseErr); err != nil {
		return unreadable(err)
	}
	baseReachable := make(map[string]bool, len(baseCommits))
	for _, commit := range baseCommits {
		baseReachable[commit.sha] = true
	}
	headReachable := make(map[string]bool, len(headCommits))
	for _, commit := range headCommits {
		headReachable[commit.sha] = true
	}
	// rewritten[id] names id's valid single-trailer binding reachable from base whose commit fell out
	// of head's reachable set — only possible if something rebased or amended an ancestor. A
	// fast-forward (the ordinary case) never drops a base-reachable commit.
	rewritten := make(map[string]string)
	markRewritten := func(commits []rawReachableAuditCommit) {
		for _, commit := range commits {
			if commit.taskBindingInvalid || len(commit.taskValues) != 1 || headReachable[commit.sha] {
				continue
			}
			rewritten[commit.taskValues[0]] = commit.sha
		}
	}
	markRewritten(baseCommits)
	// The run base is normally below base, so base's walk already covered it.
	if runBase != "" && !baseReachable[forkspace.GitOut(repo, "rev-parse", "--verify", runBase+"^{commit}")] {
		runCommits, err := rawReachableAuditCommits(repo, runBase)
		if err != nil {
			return unreadable(err)
		}
		markRewritten(runCommits)
	}
	scan = taskBindingScan{reachable: map[string]int{}, inRange: map[string]int{}, invalid: map[string]string{},
		bound: map[string][]string{}, newBound: map[string][]string{}}
	toleratedSeen := map[string]bool{}
	refuse := func(format string, args ...any) {
		scan.refused = append(scan.refused, fmt.Sprintf(format, args...))
	}
	for _, commit := range headCommits {
		inRange := !baseReachable[commit.sha]
		if inRange && commit.taskBindingInvalid {
			refuse("commit %s has a %s trailer Coop cannot read as one task id", shortCommit(commit.sha), CoopTaskTrailer)
			continue
		}
		if inRange && len(commit.taskValues) > 1 {
			refuse("commit %s carries %d %s trailers (%s); one commit can bind only one task",
				shortCommit(commit.sha), len(commit.taskValues), CoopTaskTrailer, bindingLabels(commit.taskValues))
			continue
		}
		for _, id := range commit.taskValues {
			if !allowed[id] {
				if inRange {
					switch {
					case rewritten[id] != "":
						refuse("commit %s binds task %s and replaces its bound commit %s, which is no longer in history: another task's history was rewritten",
							shortCommit(commit.sha), bindingLabel(id), shortCommit(rewritten[id]))
					case touched[id] != "":
						refuse("commit %s binds task %s, which %s", shortCommit(commit.sha), bindingLabel(id), touched[id])
					case !toleratedSeen[id]:
						toleratedSeen[id] = true
						scan.tolerated = append(scan.tolerated, id)
					}
				}
				continue
			}
			if commit.taskBindingInvalid || len(commit.taskValues) != 1 {
				scan.invalid[id] = commit.sha
				continue
			}
			scan.reachable[id]++
			scan.bound[id] = append(scan.bound[id], commit.sha)
			if inRange {
				scan.inRange[id]++
				scan.newBound[id] = append(scan.newBound[id], commit.sha)
			}
		}
	}
	if len(scan.refused) > 0 {
		if len(scan.refused) > bindingProblemsShown {
			scan.refused = append(scan.refused[:bindingProblemsShown], fmt.Sprintf("%d more commits break the binding the same way", len(scan.refused)-bindingProblemsShown))
		}
		return taskBindingScan{refused: scan.refused}, false
	}
	slices.Sort(scan.tolerated)
	return scan, true
}

// ReportToleratedForeignBindings surfaces every foreign Coop-Task trailer unbindableTasks tolerated
// instead of rejecting (see its doc comment and .agent/kb/loop-range-rejects-outside-commits.md). It
// never fails the iteration: a task id that no longer resolves to a folder on any host is skipped,
// not treated as an error, since bookkeeping about a task this iteration never touched must never
// block a legitimate completion. The operator sees one line naming all of them; each named task also
// gets its own best-effort log.md entry, so a human who later resumes that task is pointed at the
// truth by resumeLine's existing "read log.md" instruction — CommitsForTask's unconditional history
// scan will already surface the stray commit and trigger that instruction once the task is picked up
// — instead of mistaking it for that task's own completion. The mark is deliberately informational,
// not mechanical: unbindableTasks' own reachable-binding count already refuses to let that task's
// real next completion silently ride on a binding it did not itself just create.
func ReportToleratedForeignBindings(repo string, hosts []string, base, head, leasedID string, tolerated []string) error {
	if len(tolerated) == 0 {
		return nil
	}
	ui.Warn("task %s's commit range also carries Coop-Task trailer(s) for %s, which this iteration's authority never touched — tolerated, not rejected; see each task's log.md", leasedID, strings.Join(tolerated, ", "))
	for _, id := range tolerated {
		sha := ""
		if commits := CommitsForTask(repo, base+".."+head, id); len(commits) == 1 {
			sha = " (" + commits[0] + ")"
		}
		note := fmt.Sprintf(
			"a commit in task %s's completion range carries this task's Coop-Task trailer%s, without this task's queue state changing during that iteration — tolerated as harmless rather than rejecting %s's completion (see .agent/kb/loop-range-rejects-outside-commits.md); this is NOT a valid completion for this task, and this task's own next completion still needs exactly one fresh reachable Coop-Task binding of its own",
			leasedID, sha, leasedID,
		)
		for _, host := range hosts {
			t, ok, err := CurrentTask(host, id)
			if err != nil {
				return err
			}
			if ok {
				appendTaskLog(t.Dir, note)
				break
			}
		}
	}
	return nil
}

// RestoreQueuedCompletion returns a task Coop rejected to in progress, from whichever folder its
// attempt moved it to, and records why: problems, when the caller knows them, name what broke the
// binding, so the next attempt does not mistake another task's commit for its own mistake.
func RestoreQueuedCompletion(task QueuedTask, audit bool, problems ...string) error {
	id := task.Item.ID
	if task.Item.State != StateInProgress {
		if err := MoveTaskDir(task.Root, task.Item, StateInProgress); err != nil {
			return fmt.Errorf("restore task %s: %w", id, err)
		}
	}
	dir := filepath.Join(task.Root, StateInProgress, id)
	note := fmt.Sprintf("implementation completion rejected: expected exactly one commit with one matching %s trailer in the iteration's range and exactly one reachable binding overall; %s; rewrite or squash duplicate bindings down to one, then re-run `coop loop`", CoopTaskTrailer, taskBindingRecovery(id))
	if len(problems) > 0 {
		note = "implementation completion rejected because " + strings.Join(problems, "; ") + ". " +
			fmt.Sprintf("Expected exactly one commit with one matching %s trailer in the iteration's range and exactly one reachable binding overall; %s; then re-run `coop loop`", CoopTaskTrailer, taskBindingRecovery(id))
	}
	normalize := normalizeRejectedTaskState
	if audit {
		note = fmt.Sprintf("completion rejected: %s; then re-run `coop loop`", auditBindingRecovery(id))
		normalize = normalizeAuditRejectedTaskState
	}
	var errs []error
	if err := AppendTaskLogStrict(dir, note); err != nil {
		errs = append(errs, fmt.Errorf("record rejection for task %s: %w", id, err))
	}
	if err := normalize(id, dir); err != nil {
		errs = append(errs, fmt.Errorf("refresh rejected task %s: %w", id, err))
	}
	return errors.Join(errs...)
}

// RestoreBackgroundHandoffCompletion rejects a completion produced before the provider observed
// its background gate/consult result. The next fresh provider gets a precise resume note instead
// of treating an incomplete asynchronous attempt as success.
func RestoreBackgroundHandoffCompletion(task QueuedTask) error {
	id := task.Item.ID
	if task.Item.State == StateDone {
		if err := MoveTaskDir(task.Root, task.Item, StateInProgress); err != nil {
			return fmt.Errorf("restore background handoff task %s: %w", id, err)
		}
	}
	dir := filepath.Join(task.Root, StateInProgress, id)
	return errors.Join(
		AppendTaskLogStrict(dir, "provider exited while an agent-owned background job remained live; host drained or terminated it, so this completion is restored for a fresh observed attempt"),
		NormalizeTaskState(id, dir, "in progress — background handoff", "inspect the background result and rerun any ambiguous gate in the foreground", "the provider ended before its background work settled", "do not mark complete until every started gate, consult, or delegate has finished"),
	)
}

// RestoreProviderTimeoutCompletion rejects a completion produced by an attempt the watchdog
// killed for proven silence. The provider may have moved the folder and then wedged before the
// host could observe a trustworthy finish, so the completion is restored for a fresh observed
// attempt with the standard recovery contract for any commit it already landed.
func RestoreProviderTimeoutCompletion(task QueuedTask, audit bool) error {
	id := task.Item.ID
	if task.Item.State == StateDone {
		if err := MoveTaskDir(task.Root, task.Item, StateInProgress); err != nil {
			return fmt.Errorf("restore timed-out task %s: %w", id, err)
		}
	}
	dir := filepath.Join(task.Root, StateInProgress, id)
	trap := "if the prior attempt already committed, follow the informed-resume recovery: verify the work, then amend with a unique Coop-Recovery trailer while preserving exactly one Coop-Task binding"
	if audit {
		trap = "the next completion stays under the host audit authority: zero new commits or a real tree change, never a Coop-Recovery receipt"
	}
	return errors.Join(
		AppendTaskLogStrict(dir, "the host watchdog killed this provider attempt after it stopped producing observable progress; its completion was restored for a fresh observed attempt"),
		NormalizeTaskState(id, dir, "in progress — provider timeout", "resume the task and complete it under a live provider attempt", "the provider went silent and was killed before its completion could be trusted", trap),
	)
}

func restoreUnownedCompletion(task QueuedTask) error {
	id := task.Item.ID
	if task.Item.State == StateDone {
		if err := MoveTaskDir(task.Root, task.Item, StateInProgress); err != nil {
			return fmt.Errorf("restore unowned task %s: %w", id, err)
		}
	}
	dir := filepath.Join(task.Root, StateInProgress, id)
	note := "completion rejected: this provider iteration moved a task it did not lease; work exactly the assigned task, then re-run `coop loop`"
	return errors.Join(
		AppendTaskLogStrict(dir, note),
		NormalizeTaskState(id, dir, "in progress — completion rejected", "work this task only when it is assigned", "completion was rejected as unowned", "another iteration moved this task without its lease"),
	)
}

// restoreChangedReviewSubject restores a final-review subject whose archived folder changed while
// the review ran. The review's verdict no longer covers what is in the folder, but nobody needs to
// have moved it, so the note names the change. The next loop start returns it to todo.
func restoreChangedReviewSubject(task QueuedTask) error {
	id := task.Item.ID
	if task.Item.State == StateDone {
		if err := MoveTaskDir(task.Root, task.Item, StateInProgress); err != nil {
			return fmt.Errorf("restore review subject %s: %w", id, err)
		}
	}
	dir := filepath.Join(task.Root, StateInProgress, id)
	note := "completion restored: this task's archived folder changed while a final review was running, so the review could not keep its verdict; the next `coop loop` works and reviews it again"
	return errors.Join(
		AppendTaskLogStrict(dir, note),
		NormalizeTaskState(id, dir, "in progress — archive changed during review", "re-run `coop loop`; it works and reviews this task again", "its archived folder changed during a final review", "—"),
	)
}

func RestoreCompromisedCompletion(task QueuedTask, audit bool) error {
	id := task.Item.ID
	if task.Item.State == StateDone {
		if err := MoveTaskDir(task.Root, task.Item, StateInProgress); err != nil {
			return fmt.Errorf("restore assigned task %s: %w", id, err)
		}
	}
	dir := filepath.Join(task.Root, StateInProgress, id)
	// The restored task's next completion is validated by its lease authority: a Coop-Recovery
	// receipt closes an ordinary rejected range, but under audit authority that same receipt is the
	// message-only rewrite audit validation rejects.
	trap := "the next completion needs a unique Coop-Recovery trailer"
	if audit {
		trap = "the next completion stays under the host audit authority: zero new commits or a real tree change, never a Coop-Recovery receipt"
	}
	note := "completion rejected: this iteration also moved an unleased task, so its assigned completion was restored for a clean reviewed attempt"
	return errors.Join(
		AppendTaskLogStrict(dir, note),
		NormalizeTaskState(id, dir, "in progress — completion rejected", "resume the assigned task and complete it without touching another task", "the assigned work committed but its iteration violated task ownership", trap),
	)
}

// RestoreRefAuthorityFailure restores a completion the box already folder-moved when the ref
// authority window could not be trusted through consumption — either the lock itself could not be
// acquired, or the compare-and-swap proved HEAD moved since validation. Neither is a binding or
// ownership problem, so it gets its own reason text instead of reusing unbindable/unowned wording.
// The tree the provider committed is untouched: only the folder move and its state notes are undone,
// so the same commit is still there for the next iteration to resume, exactly as
// .agent/kb/loop-range-rejects-outside-commits.md describes for a foreign in-range commit.
func RestoreRefAuthorityFailure(task QueuedTask, reason string) error {
	id := task.Item.ID
	if task.Item.State == StateDone {
		if err := MoveTaskDir(task.Root, task.Item, StateInProgress); err != nil {
			return fmt.Errorf("restore task %s: %w", id, err)
		}
	}
	dir := filepath.Join(task.Root, StateInProgress, id)
	note := fmt.Sprintf("completion rejected: %s; the commit is still in history, so re-running `coop loop` resumes it", reason)
	return errors.Join(
		AppendTaskLogStrict(dir, note),
		NormalizeTaskState(id, dir, "in progress — completion rejected", "re-run `coop loop`; it resumes this task", "completion was rejected: ref authority could not be confirmed", reason),
	)
}

func RestoreUnrecordedCompletion(task QueuedTask) error {
	id := task.Item.ID
	if task.Item.State == StateDone {
		if err := MoveTaskDir(task.Root, task.Item, StateInProgress); err != nil {
			return fmt.Errorf("restore unrecorded task %s: %w", id, err)
		}
	}
	dir := filepath.Join(task.Root, StateInProgress, id)
	note := "completion rejected: host-only completion evidence could not be recorded before releasing the task lease"
	return errors.Join(
		AppendTaskLogStrict(dir, note),
		NormalizeTaskState(id, dir, "in progress — finalization failed", "fix the host completion-receipt error, then re-run `coop loop`", "the implementation committed but host finalization did not finish", "completion evidence must be recorded under the task authority lock"),
	)
}

func normalizeRejectedTaskState(id, taskDir string) error {
	return NormalizeTaskState(
		id,
		taskDir,
		"in progress — completion rejected",
		"repair the commit binding or record an eligible no-change outcome, then re-run `coop loop`",
		"completion was rejected as unbindable",
		"implementation needs exactly one matching Coop-Task trailer; no-change completion needs an explicit outcome with reason and evidence",
	)
}

func normalizeAuditRejectedTaskState(id, taskDir string) error {
	return NormalizeTaskState(
		id,
		taskDir,
		"in progress — completion rejected",
		"independently verify the audit finding, then re-close with zero commits or one real unbound repair commit, and re-run `coop loop`",
		"completion was rejected by the host audit authority",
		"preserve reviewed history; never add a Coop-Task or Coop-Recovery trailer to the repair",
	)
}

func UnbindableCompletionError(ids, problems []string, restoreErr error) error {
	recoveries := make([]string, 0, len(ids))
	for _, id := range ids {
		recoveries = append(recoveries, fmt.Sprintf("%s: %s", id, taskBindingRecovery(id)))
	}
	cause := ""
	if len(problems) > 0 {
		cause = " because " + strings.Join(problems, "; ")
	}
	msg := fmt.Sprintf("implementation completion rejected for task(s) %s%s: the new commit range and reachable HEAD each need exactly one commit with one parseable `%s: <id>` trailer per task; task(s) restored to in_progress — %s; rewrite/squash duplicate bindings down to one, then re-run `coop loop`", strings.Join(ids, ", "), cause, CoopTaskTrailer, strings.Join(recoveries, "; "))
	if restoreErr != nil {
		return fmt.Errorf("%s; recovery bookkeeping also failed: %w", msg, restoreErr)
	}
	return errors.New(msg)
}

// taskBindingRecovery describes the safe history shapes. A bare amend is deliberately absent:
// without --only it can absorb unrelated staged work, and when the implementation is not HEAD it
// would attach the task to the wrong commit. Rewriting a commit that is not HEAD is never
// prescribed — it reparents every descendant, and a binding that is already reachable needs no
// rewrite to count.
func taskBindingRecovery(id string) string {
	return fmt.Sprintf(
		"if this task has no existing binding and no source or Git history changed in this attempt, verify its requirements and use tasks_complete with outcome already_satisfied, or could_not_reproduce/wont_fix for an investigation, plus concrete reason and evidence; do not create an empty commit or report unfinished work as complete; human decisions use tasks_block; "+
			"if the implementation commit is HEAD and only lacks the trailer, amend its message without touching the index "+
			"(`git commit --amend --only --no-edit --trailer %q`); if a commit carrying that trailer is already "+
			"reachable but is NOT HEAD, do not rewrite it — that reparents every commit after it — and never add a "+
			"second task-bound commit: verify the work and park the task in 50_blocked/, its decision.md naming "+
			"what you verified, so a human can finish it",
		CoopTaskTrailer+": "+id,
	)
}

// auditBindingRecovery replaces the generic trailer recipe when the host's audit-reopen authority
// owns the completion. The case-(a) receipt (a Coop-Recovery trailer on an unchanged tree) is
// exactly the shape audit validation rejects, so the remedy never prescribes it: either the
// finding is false and the re-close needs zero commits, or it is real and one unbound repair commit
// must actually change the tree while preserving reviewed history.
func auditBindingRecovery(id string) string {
	return fmt.Sprintf(
		"task %s is host-authorized review rework: independently verify the recorded finding; if it is false, "+
			"re-close with zero new commits; if it is real, add one real repair commit without a %s trailer, "+
			"preserving the reviewed history; a second repair commit, a task trailer on the repair, or a message-only commit will be rejected",
		id, CoopTaskTrailer,
	)
}

func AuditCompletionError(id string, restoreErr error) error {
	msg := fmt.Sprintf("completion rejected for audit-reopened task %s: the host audit authority accepts a zero-commit verification-only re-close or one real unbound repair commit preserving reviewed history; task restored to in_progress — %s; then re-run `coop loop`", id, auditBindingRecovery(id))
	if restoreErr != nil {
		return fmt.Errorf("%s; recovery bookkeeping also failed: %w", msg, restoreErr)
	}
	return errors.New(msg)
}

// RefAuthorityFailureError reports why the ref-authority window refused to consume task authority
// for id — either the lock itself could not be acquired (a stuck holder, named in reason) or the
// compare-and-swap proved HEAD moved since validation (reason names the observed and expected SHAs).
// Either way the task is restored, actionable, and nothing was consumed.
func RefAuthorityFailureError(id, reason string, restoreErr error) error {
	msg := fmt.Sprintf("completion rejected for task %s: %s; task restored to in_progress with its commit intact — re-run `coop loop` to resume it", id, reason)
	if restoreErr != nil {
		return fmt.Errorf("%s; recovery bookkeeping also failed: %w", msg, restoreErr)
	}
	return errors.New(msg)
}

func UnownedCompletionError(ids []string, restoreErr error) error {
	msg := "could not validate a completion outside this iteration's lease"
	if len(ids) > 0 {
		msg = fmt.Sprintf("completion rejected for unleased task(s) %s: this iteration may complete only its assigned task; task(s) restored to in_progress", strings.Join(ids, ", "))
	}
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
func resumeLine(id string, commits []string, atHead bool) string {
	if len(commits) == 0 {
		return ""
	}
	line := "Task " + id + " has commit(s) " + strings.Join(commits, ", ") + " already in history carrying " +
		"its Coop-Task trailer. Read its log.md/state.md and determine which case applies: (a) a prior " +
		"attempt COMMITTED then was interrupted before moving the folder to 99_done/ — verify that work " +
		"against the acceptance criteria, amend the commit with a unique `Coop-Recovery: <current UTC timestamp>` " +
		"trailer while preserving exactly one Coop-Task trailer, and finish the move, but do NOT redo it; or (b) the review REOPENED it " +
		"(its log.md will say what's wrong) — independently reproduce the finding; if it is false, re-close without a receipt-only commit; " +
		"otherwise do the rework by amending or rewriting the already-bound implementation commit (at HEAD: `git commit --amend --no-edit --only -- <changed paths>`; " +
		"a follow-up commit cannot bind), leaving exactly one reachable Coop-Task " +
		"binding and semantically unchanged later task commits; do not add a second task-bound commit. Disambiguate before acting."
	if atHead {
		return line
	}
	// Both recipes above end at "amend/rewrite the bound commit". That is safe only while it IS
	// HEAD. Deeper, the same instruction reparents every descendant — it rewrote a whole 286-commit
	// branch once — and the result can never pass validation anyway, because the reparented
	// descendants carry OTHER tasks' trailers and trip the foreign-binding guard.
	return line + " STOP — that bound commit is NOT HEAD: amending or rewriting it would reparent every " +
		"commit after it and rewrite the branch. Never do that, by rebase, cherry-pick, or plumbing. " +
		"Neither recipe is workable here, so move this task's folder into 50_blocked/ and record in its " +
		"decision.md which case applies and what you verified, so a human can finish it. Then stop."
}

// boundTaskCommitIsHead reports whether the task's single bound commit is HEAD itself. Only then is
// the amend recipe safe: rewriting any older commit reparents everything after it.
func boundTaskCommitIsHead(repo string, commits []string) bool {
	if len(commits) != 1 {
		return false
	}
	head := forkspace.GitOut(repo, "rev-parse", "--verify", "HEAD")
	return head != "" && head == forkspace.GitOut(repo, "rev-parse", "--verify", commits[0]+"^{commit}")
}

// auditResumeLine is the informed-resume preamble when the assigned lease carries the host's
// audit-reopen authority. The record — not commit presence — selects the remedy: this is
// host-authorized review rework, never case-(a) crash recovery, and the case-(a) recipe (a
// Coop-Recovery receipt on an unchanged tree) is exactly what audit completion validation rejects.
func auditResumeLine(id string) string {
	return "Task " + id + " is host-authorized review rework: a host audit reopened it, and its Coop-Task " +
		"commit is already in history — this is NOT crash recovery. Read its log.md/state.md for the recorded " +
		"finding and independently verify it. If the finding is false, re-close with ZERO new commits: leave " +
		"history untouched and move the task folder to 99_done/ — the host audit authority accepts a " +
		"verification-only completion. A zero-commit re-close of a finding asking for missing evidence must include " +
		"that evidence in state.md or durable artifacts: run the requested checks and record exact commands, results, " +
		"and artifact paths. A bare assertion that the finding is false is not evidence. If the finding is real, add exactly one real repair commit WITHOUT a " +
		"Coop-Task or Coop-Recovery trailer on top of the current HEAD, preserving the reviewed history; Coop's commit hook adds no trailer during rework. " +
		"The host audit authority binds that repair to this task. Never amend, rebase, squash, or fold the bound commit or any reviewed commit, and do not " +
		"add a receipt-only commit."
}

// answeredResumeLine is the informed-resume preamble when the lease's authority came from a person's
// unblock of a task its own attempt committed and then blocked (answeredBlockReopen). The rules are
// the audit rework's, with the answer in place of a review finding.
func answeredResumeLine(id string) string {
	return "Task " + id + " already has its Coop-Task commit in history: an earlier attempt committed it, then blocked the " +
		"task for a person, who has answered in its decision.md. This is NOT crash recovery: do not amend that commit or add a " +
		"Coop-Recovery trailer. The host lets you re-close it under the audit rework rules below, with the answer in place of a " +
		"review finding. If the answer asks only for checks, run them, record the exact commands and results in state.md, and " +
		"re-close with ZERO new commits: leave history untouched and call tasks_complete without outcome. If the answer needs a " +
		"change, add exactly one real repair commit WITHOUT a Coop-Task or Coop-Recovery trailer on top of the current HEAD. " +
		"Never amend, rebase, squash, or fold the bound commit or any later one."
}

// ResumePrefixFor builds the informed-resume preamble for the assigned task. A lease carrying the
// host's audit-reopen authority selects the audit-rework preamble regardless of commit presence, or
// the answered-block one when a person's unblock granted it; otherwise the Coop-Task trailer already
// in history selects the crash/reopen disambiguation line. With neither, an interrupted attempt may
// still have left UNCOMMITTED work behind, so that case gets its own line. Empty only for a
// genuinely untouched resume.
func ResumePrefixFor(repo, id, state string, reopen *AuditReopenRecord) string {
	if reopen != nil && reopen.Answered {
		return answeredResumeLine(id)
	}
	if reopen != nil {
		return auditResumeLine(id)
	}
	commits := CommitsForTask(repo, "", id)
	if len(commits) == 0 {
		// Only for a RESUMED task. A fresh claim in a dirty checkout means someone else's work is
		// in the tree, and telling a new task to go read it would be noise at best and an
		// invitation to touch another task's files at worst.
		if state != StateInProgress {
			return ""
		}
		return uncommittedResumeLine(id, interruptedWorkFiles(repo))
	}
	return resumeLine(id, commits, boundTaskCommitIsHead(repo, commits))
}

// uncommittedResumeLine covers the OTHER interrupted shape: an in_progress task with NO bound
// commit, resumed while the working tree carries changes. state.md cannot be trusted to reveal
// this — a hard kill (OOM, a crashed container runtime, `docker kill`) never runs the agent's
// checkpoint, so the snapshot keeps whatever it last said. Measured: a task killed after ~13h left
// four modified files and a new rule card uncommitted while its state.md still read "not started";
// a fresh agent had every reason to start over and redo all of it.
//
// It deliberately does NOT claim the changes belong to this task — in a shared checkout they may be
// another task's, or a human's. It says what is there and makes reading it the first step, because
// the expensive mistake is not reading at all.
func uncommittedResumeLine(id string, files []string) string {
	if len(files) == 0 {
		return ""
	}
	shown := files
	if len(shown) > 12 {
		shown = shown[:12]
	}
	line := "Task " + id + " is being resumed and has NO commit in history, but the working tree already " +
		"carries uncommitted changes: " + strings.Join(shown, ", ")
	if len(files) > len(shown) {
		line += fmt.Sprintf(" (+%d more)", len(files)-len(shown))
	}
	return line + ". A previous attempt may have been killed before it could commit or checkpoint, so " +
		"its state.md may understate what was done — do not trust it over the tree. FIRST run `git diff` " +
		"and `git status` and read that work; judge it on its merits and finish or correct it rather than " +
		"starting over. Some of it may belong to a DIFFERENT task or to a human, so commit only the paths " +
		"that are yours, never `git add -A`."
}

// interruptedWorkFiles lists the working tree's changed paths for the resume hint. The task queue
// is gitignored working state, so it never appears here; a clean tree yields nothing and the
// blind-resume prompt stays byte-identical.
func interruptedWorkFiles(repo string) []string {
	out := forkspace.GitOut(repo, "status", "--porcelain")
	if out == "" {
		return nil
	}
	var files []string
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 4 {
			continue
		}
		// Porcelain v1: XY then a space then the path; a rename reads "old -> new", keep the new one.
		path := strings.TrimSpace(line[3:])
		if i := strings.LastIndex(path, " -> "); i >= 0 {
			path = path[i+4:]
		}
		if path = strings.Trim(path, `"`); path != "" {
			files = append(files, path)
		}
	}
	return files
}

func ValidateLeasedAuditReopen(repo, head, id string, reopen *AuditReopenRecord) error {
	if reopen == nil {
		return nil
	}
	if _, err := auditReopenCurrentHistory(repo, head, id, *reopen); err == nil {
		return nil
	} else {
		return fmt.Errorf(
			"task %s host audit-reopen authority no longer matches current HEAD; "+
				"its exact recorded baseline is %s — no provider started: %w",
			id, reopen.BaselineHead, err,
		)
	}
}

func StaleAuditReopenRecovery(id, baseline string) string {
	return fmt.Sprintf(
		"task parked in blocked: first run `coop tasks unblock %s \"<answer>\"`, which accepts other work "+
			"that landed and changes nothing when it refuses; if it refuses because this task's history was "+
			"rewritten, preserve any wanted work separately, restore the exact audited "+
			"pre-attempt baseline %s from reflog or backup, verify `git rev-parse HEAD` prints %s, "+
			"then run `coop tasks unblock %s \"restored audited pre-attempt HEAD\"`; blocking and "+
			"unblocking alone cannot repair Git history",
		id, baseline, baseline, id,
	)
}

// ParkStaleAuditReopen removes a stale host authority from the unattended work queue without
// discarding the rejected Git rewrite. The operator must restore the exact audited baseline before
// explicit unblock can reactivate that generation. Metadata and the folder move roll back together
// so a failed decision/state write never leaves a malformed blocked task.
func ParkStaleAuditReopen(task QueuedTask, baseline string) error {
	id := task.Item.ID
	if task.Item.State != StateInProgress {
		return fmt.Errorf("park stale audit task %s from %s: want in progress", id, StateLabel(task.Item.State))
	}
	if !validAuditReopenHead(baseline) {
		return fmt.Errorf("park stale audit task %s without a valid recorded baseline", id)
	}
	metadata, err := snapshotTaskMetadata(task.Item.Dir, "decision.md", "log.md", "state.md")
	if err != nil {
		return fmt.Errorf("snapshot stale audit task %s: %w", id, err)
	}
	if err := MoveTaskDir(task.Root, task.Item, StateBlocked); err != nil {
		return fmt.Errorf("move stale audit task %s to blocked: %w", id, err)
	}
	blockedDir := filepath.Join(task.Root, StateBlocked, id)
	rollback := func(cause error) error {
		metadataErr := restoreTaskMetadata(blockedDir, metadata)
		current := task.Item
		current.State = StateBlocked
		current.Dir = blockedDir
		moveErr := MoveTaskDir(task.Root, current, StateInProgress)
		return errors.Join(cause, metadataErr, moveErr)
	}
	if err := writeStaleAuditReopenDecision(blockedDir, id, task.Item.Title, baseline, metadata["decision.md"]); err != nil {
		return rollback(fmt.Errorf("write stale audit decision for task %s: %w", id, err))
	}
	if err := AppendTaskLogStrict(blockedDir, fmt.Sprintf(
		"host preflight parked this task because current HEAD no longer matches its audit-reopen authority; "+
			"an explicit unblock accepts other work that landed; if it refuses because this task's history was "+
			"rewritten, restore exact baseline %s and verify `git rev-parse HEAD` before unblocking again — "+
			"blocking and unblocking alone cannot repair Git history",
		baseline,
	)); err != nil {
		return rollback(fmt.Errorf("record stale audit park for task %s: %w", id, err))
	}
	if err := NormalizeTaskState(
		id,
		blockedDir,
		"blocked — stale audit authority",
		fmt.Sprintf("a person unblocks this task; if the unblock refuses because its history was rewritten, "+
			"preserve wanted work separately, restore exact baseline %s, verify `git rev-parse HEAD`, then unblock again", baseline),
		"the host stopped before provider launch because current HEAD no longer matches its audit authority",
		"blocking and unblocking alone cannot repair Git history; never add a Coop-Recovery receipt",
	); err != nil {
		return rollback(fmt.Errorf("refresh stale audit task %s: %w", id, err))
	}
	return nil
}

func writeStaleAuditReopenDecision(taskDir, id, title, baseline string, previous taskMetadataSnapshot) error {
	body := fmt.Sprintf(
		"# Decision: HEAD moved under reopened task %q\n\n"+
			"**Blocks:** this task (`%s`).\n\n"+
			"**The decision:** Current HEAD no longer matches the host authority captured before "+
			"this audit-rework attempt. Coop cannot safely infer which rejected history to discard.\n\n"+
			"**Options:**\n"+
			"- **A — accept other work:** Run `coop tasks unblock %s \"<answer>\"`. It moves the task's "+
			"audit record past other work that landed, or names what stopped it and changes nothing.\n"+
			"- **B — restore the audited baseline:** Preserve any wanted work separately, then restore "+
			"the exact pre-attempt baseline `%s` from reflog or backup; verify "+
			"`git rev-parse HEAD` prints `%s`.\n"+
			"- **C — stop for manual authority repair:** Use this when the exact baseline is unavailable; "+
			"do not manufacture a recovery receipt or merely cycle the task state.\n\n"+
			"**Recommendation:** Try A first. If it refuses because this task's history was rewritten, "+
			"choose B, verify the repository is back at the exact audited baseline, then run "+
			"`coop tasks unblock %s \"restored audited pre-attempt HEAD\"`. Blocking and "+
			"unblocking alone cannot repair Git history.\n\n"+
			"---\n\n"+
			"**Resolution:** <!-- HUMAN: record what you chose, then explicitly unblock -->\n",
		title, id, id, baseline, baseline, id,
	)
	if previous.exists && strings.TrimSpace(string(previous.body)) != "" {
		var quoted strings.Builder
		for _, line := range strings.Split(strings.TrimRight(string(previous.body), "\n"), "\n") {
			quoted.WriteString("> ")
			quoted.WriteString(line)
			quoted.WriteByte('\n')
		}
		body += "\n## Previous decision record\n\n" + quoted.String()
	}
	root, err := OpenTaskMetadataRoot(taskDir)
	if err != nil {
		return err
	}
	defer root.Close()
	return AtomicWriteTaskFile(root, "decision.md", []byte(body))
}

type TaskAssignmentOutcome uint8

const (
	AssignmentDrained TaskAssignmentOutcome = iota
	AssignmentUnavailable
	assignmentReady
)

type TaskAssignment struct {
	Counts  TaskCounts
	Task    QueuedTask
	Lease   *TaskLease
	Outcome TaskAssignmentOutcome
	Busy    TaskLeaseSummary
}

const maxLeaseRescans = 3

// skipOwnedCandidate reports whether id carries a durable human-claim record (TaskOwnerRecord) — if
// so, the loop must not adopt it, lease or no lease: `coop tasks claim` holds no flock, so an
// unheld lease proves nothing about whether a human still owns the work. Checked BEFORE
// TryTaskLease, not after, for two reasons: it avoids taking (and then having to release) a flock on
// work the loop was never going to adopt anyway, and `coop tasks claim` writes this record BEFORE it
// moves the task's folder (see claimTaskOwnerRecord), so the record protects an in-flight claim from
// the instant it starts — before a concurrent scan could even see the folder move. noted dedupes the
// human-facing notice per AssignLoopTaskOnly call, so a candidate seen again on an internal rescan
// (maxLeaseRescans) is skipped again silently rather than re-announced.
func skipOwnedCandidate(root, id string, noted map[string]bool) (bool, error) {
	rec, ok, err := ReadTaskOwnerRecord(root, id)
	if err != nil {
		// A claim nobody can read is still a claim: skip the task rather than stop the whole loop,
		// which a box could otherwise do on purpose by damaging the identity of a task it claimed.
		if !noted[id] {
			noted[id] = true
			ui.Warn("%s has an owner record Coop cannot read (%v) — the loop will not adopt it", id, err)
		}
		return true, nil
	}
	if !ok {
		return false, nil
	}
	if !noted[id] {
		noted[id] = true
		if rec.Kind == TaskOwnerFork {
			ui.Note("%s is %s — the local loop will not adopt it", id, TaskOwnerLabel(rec))
		} else {
			ui.Note("%s is %s — the loop will not adopt it; release it first: coop tasks release %s",
				id, TaskOwnerLabel(rec), id)
		}
	}
	return true, nil
}

// beforeCandidateLease, when a test sets it, runs between a candidate's claim check and its lease:
// the window releaseClaimedLease closes.
var beforeCandidateLease func(QueuedTask)

// releaseClaimedLease re-reads a just-leased candidate's claim. The read before the lease is only a
// cheap filter: a box session claims under this same lease authority, so a claim that landed while
// the loop was choosing is visible once the loop holds the lease, and it wins — the loop lets go.
func releaseClaimedLease(candidate QueuedTask, lease *TaskLease, noted map[string]bool, busy *TaskLeaseSummary) (bool, error) {
	claimed, err := skipOwnedCandidate(candidate.Root, candidate.Item.ID, noted)
	if err != nil || !claimed {
		if err != nil {
			err = errors.Join(err, lease.Release())
		}
		return false, err
	}
	busy.Owned++
	return true, lease.Release()
}

// AssignLoopTaskOnly scopes assignment to the current task in a limited run. Counts still cover the
// whole queue for truthful banners, but another actionable task can never be claimed while the
// selected task is retrying or has been reopened by its between-task audit.
func AssignLoopTaskOnly(hosts []string, owner TaskLeaseOwner, onlyID string) (TaskAssignment, error) {
	noted := map[string]bool{} // spans every rescan attempt below, so an owned skip is reported once
	for attempt := 0; attempt < maxLeaseRescans; attempt++ {
		var counts TaskCounts
		var inProgress, todo []QueuedTask
		index := queueIndex{}
		for _, root := range hosts {
			items, err := ReadTaskTree(root)
			if err != nil {
				return TaskAssignment{}, err
			}
			for _, item := range items {
				index.add(item)
				switch item.State {
				case StateTodo:
					counts.Todo++
					if onlyID == "" || item.ID == onlyID {
						todo = append(todo, QueuedTask{Root: root, Item: item})
					}
				case StateInProgress:
					counts.Doing++
					if onlyID == "" || item.ID == onlyID {
						inProgress = append(inProgress, QueuedTask{Root: root, Item: item})
					}
				case StateBlocked:
					counts.Blocked++
				case StateDone:
					counts.Done++
				}
			}
		}

		claimOrder(todo, index)
		var busy TaskLeaseSummary
		changed := false
		for _, candidate := range inProgress {
			if owned, err := skipOwnedCandidate(candidate.Root, candidate.Item.ID, noted); err != nil {
				return TaskAssignment{}, err
			} else if owned {
				busy.Owned++
				continue
			}
			if beforeCandidateLease != nil {
				beforeCandidateLease(candidate)
			}
			lease, observed, err := TryTaskLease(candidate.Root, candidate.Item, owner)
			if errors.Is(err, errLeaseCandidateGone) {
				changed = true
				break
			}
			if err != nil {
				return TaskAssignment{}, fmt.Errorf("lease task %s: %w", candidate.Item.ID, err)
			}
			if lease == nil {
				busy.add(observed)
				continue
			}
			if claimed, err := releaseClaimedLease(candidate, lease, noted, &busy); err != nil {
				return TaskAssignment{}, err
			} else if claimed {
				continue
			}
			return TaskAssignment{
				Counts: counts, Task: candidate, Lease: lease, Outcome: assignmentReady, Busy: busy,
			}, nil
		}
		if changed {
			continue
		}

		for _, candidate := range todo {
			if index.waiting(candidate.Item) {
				busy.Waiting++ // SettleTaskWaits releases it, or hands a wait it can never meet to a person
				continue
			}
			if owned, err := skipOwnedCandidate(candidate.Root, candidate.Item.ID, noted); err != nil {
				return TaskAssignment{}, err
			} else if owned {
				busy.Owned++
				continue
			}
			if beforeCandidateLease != nil {
				beforeCandidateLease(candidate)
			}
			lease, observed, err := TryTaskLease(candidate.Root, candidate.Item, owner)
			if errors.Is(err, errLeaseCandidateGone) {
				changed = true
				break
			}
			if err != nil {
				return TaskAssignment{}, fmt.Errorf("lease task %s: %w", candidate.Item.ID, err)
			}
			if lease == nil {
				busy.add(observed)
				continue
			}
			if claimed, err := releaseClaimedLease(candidate, lease, noted, &busy); err != nil {
				return TaskAssignment{}, err
			} else if claimed {
				continue
			}
			if err := MoveTaskDir(candidate.Root, candidate.Item, StateInProgress); err != nil {
				_ = lease.Release()
				if strings.Contains(err.Error(), "changed state under us") {
					changed = true
					break
				}
				return TaskAssignment{}, fmt.Errorf("claim task %s: %w", candidate.Item.ID, err)
			}
			candidate.Item.State = StateInProgress
			candidate.Item.Dir = filepath.Join(candidate.Root, StateInProgress, candidate.Item.ID)
			counts.Todo--
			counts.Doing++
			return TaskAssignment{
				Counts: counts, Task: candidate, Lease: lease, Outcome: assignmentReady, Busy: busy,
			}, nil
		}
		if changed {
			continue
		}
		// A scoped task that waits for another one is not workable now, as a parked one is not.
		if onlyID != "" && len(inProgress)+len(todo) == busy.Waiting {
			return TaskAssignment{Counts: counts, Outcome: AssignmentDrained}, nil
		}
		if counts.Todo+counts.Doing == 0 {
			return TaskAssignment{Counts: counts, Outcome: AssignmentDrained}, nil
		}
		return TaskAssignment{Counts: counts, Outcome: AssignmentUnavailable, Busy: busy}, nil
	}
	return TaskAssignment{}, fmt.Errorf("task queue kept changing while leasing — retry the loop")
}

// UnblockResolved is the loop's built-in preflight, run host-side (no box, no model): every
// ordinary blocked task whose decision.md now carries a filled-in Resolution — the same bar
// `coop tasks unblock` applies (decisionResolved) — moves back to 00_todo/ with a log note.
// Audit-reopened tasks, and tasks their own attempt committed before blocking them, require an
// explicit host `coop tasks unblock`: provider-writable prose can never invoke the host-authority
// rebase path or grant re-close authority.
// A task with no decision.md or an unanswered one stays parked. An unreadable decision or queue
// stops the pass before it can be mistaken for an unanswered/empty state; move failures still
// warn and skip the affected task.
// Returns the unblocked ids in ReadTaskTree order.
func UnblockResolved(hosts []string) ([]string, error) {
	var ids []string
	for _, host := range hosts {
		items, err := ReadTaskTree(host)
		if err != nil {
			return nil, err
		}
		for _, t := range items {
			if t.State != StateBlocked || !t.HasDecision {
				continue
			}
			resolved, err := decisionResolved(filepath.Join(t.Dir, "decision.md"))
			if err != nil {
				return nil, err
			}
			if !resolved {
				continue
			}
			_, hasAuthority, err := ReadAuditReopenRecord(host, t.ID)
			if err != nil {
				ui.Warn("pre-flight: could not inspect host audit authority for %s: %v — task remains blocked; repair the host authority registry, then retry: coop tasks unblock %s", t.ID, err, t.ID)
				continue
			}
			if hasAuthority {
				ui.Warn("pre-flight: %s has host audit-reopen authority — unblock it explicitly: coop tasks unblock %s", t.ID, t.ID)
				continue
			}
			// Only a person's unblock may turn the note into re-close authority, and a box can write
			// the Resolution this pass reads.
			if _, committed, err := readCommittedBlock(host, t.ID); err != nil {
				ui.Warn("pre-flight: could not inspect host records for %s: %v — task remains blocked; repair the host authority registry, then retry: coop tasks unblock %s", t.ID, err, t.ID)
				continue
			} else if committed {
				ui.Warn("pre-flight: %s was committed before it blocked — unblock it explicitly so the next worker can close it: coop tasks unblock %s", t.ID, t.ID)
				continue
			}
			if err := MoveTaskDir(host, t, StateTodo); err != nil {
				ui.Warn("pre-flight: could not unblock %s: %v", t.ID, err)
				continue
			}
			appendTaskLog(filepath.Join(host, StateTodo, t.ID), "preflight: resolution filled in — unblocked")
			ids = append(ids, t.ID)
		}
	}
	return ids, nil
}

// ReleaseGoneOwners is the pre-flight tidy for process-bound claims: a human claim whose owning
// process is provably gone (a dead pid, or a reused pid with a different start token) is released
// so the loop can resume the task from its state.md. A claim with no process identity — a person
// at a keyboard — is never touched, and an identity that cannot be inspected counts as alive.
// Returns the released ids in ReadTaskTree order.
func ReleaseGoneOwners(hosts []string) ([]string, error) {
	var ids []string
	for _, host := range hosts {
		items, err := ReadTaskTree(host)
		if err != nil {
			return nil, err
		}
		for _, t := range items {
			if t.State != StateInProgress {
				continue
			}
			record, owned, err := readTaskOwnerRecordOf(host, t)
			if err != nil {
				ui.Warn("pre-flight: could not read the owner of %s: %v — claim left in place", t.ID, err)
				continue
			}
			if !owned || record.Kind != TaskOwnerHuman || record.ActorPID == 0 || ownerProcessLive(record) {
				continue
			}
			released, err := releaseGoneOwner(host, t.ID, record)
			if err != nil {
				ui.Warn("pre-flight: could not release %s: %v", t.ID, err)
				continue
			}
			if !released {
				continue
			}
			appendTaskLog(t.Dir, fmt.Sprintf("preflight: released the claim — %s", TaskOwnerLabel(record)))
			ids = append(ids, t.ID)
		}
	}
	return ids, nil
}

// releaseGoneOwner removes one process-bound human claim under the owner lock, re-reading the
// record and re-inspecting the process there so a claim replaced or revived in between survives.
func releaseGoneOwner(root, id string, expected TaskOwnerRecord) (bool, error) {
	lock, err := lockTaskOwner(root, id)
	if err != nil {
		return false, err
	}
	defer lock.Close()
	record, ok, err := lock.Read()
	if err != nil || !ok {
		return false, err
	}
	if record.Kind != TaskOwnerHuman || record.ActorPID != expected.ActorPID ||
		record.ActorStart != expected.ActorStart || ownerProcessLive(record) {
		return false, nil
	}
	return true, removeTaskOwnerRecordFile(root, id)
}

// AppendTaskLogStrict appends one host note to a task's log.md as a bullet line. A new note's opening
// words belong in coopLogNotes, so a log holding only Coop's notes still reads as having no entry.
func AppendTaskLogStrict(taskDir, note string) error {
	return appendTaskLogChunk(taskDir, taskLogNote(note))
}

// appendTaskLogNoteIn is AppendTaskLogStrict for a task folder already open as root.
func appendTaskLogNoteIn(root *os.Root, note string) error {
	return appendTaskLogChunkIn(root, taskLogNote(note))
}

func taskLogNote(note string) string { return "\n- " + note + "\n" }

// AppendTaskLogEntry appends an agent-authored entry (a heading, bullets, prose — any lines) to a
// task's log.md, separated from what precedes it by a blank line. The in-box task channel uses it
// so a box appends under the same single-link, size-bounded discipline as a host note.
func AppendTaskLogEntry(taskDir, entry string) error {
	return appendTaskLogChunk(taskDir, "\n"+strings.TrimRight(entry, "\n")+"\n")
}

func appendTaskLogChunk(taskDir, line string) error {
	root, err := OpenTaskMetadataRoot(taskDir)
	if err != nil {
		return err
	}
	defer root.Close()
	return appendTaskLogChunkIn(root, line)
}

// appendTaskLogChunkIn is appendTaskLogChunk for a task folder already open as root.
func appendTaskLogChunkIn(root *os.Root, line string) error {
	before, statErr := root.Lstat("log.md")
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if statErr == nil {
		if err := validateTaskMetadataFile("log.md", before); err != nil {
			return err
		}
	}
	f, after, err := openTaskLogForAppend(root, before)
	if err != nil {
		return err
	}
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

// openTaskLogForAppend pins the checked regular file without waiting if its
// directory entry was replaced by a FIFO between inspection and open.
func openTaskLogForAppend(root *os.Root, before os.FileInfo) (*os.File, os.FileInfo, error) {
	f, err := root.OpenFile("log.md", os.O_APPEND|os.O_CREATE|os.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o644)
	if err != nil {
		return nil, nil, err
	}
	after, err := f.Stat()
	if err != nil || (before != nil && !os.SameFile(before, after)) {
		_ = f.Close()
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, errors.New("task log changed while opening")
	}
	if err := validateTaskMetadataFile("log.md", after); err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return f, after, nil
}

// appendTaskLog appends a one-line note to a task folder's log.md, best-effort.
func appendTaskLog(taskDir, note string) {
	_ = AppendTaskLogStrict(taskDir, note)
}
