package cli

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/ui"
)

// The Coop-Task trailer binds a commit to the task it completes. The agent writes it (loopWorkPrompt
// instructs it); the HOST controller reads it to verify attempts, resume informed after a crash, and
// reconcile the parent queue after a fork merge — the LLM still moves folders, the controller only
// supplies evidence and repairs drift. Before this, nothing linked a commit to a task
// (git log --grep <id> was 0 repo-wide), so "one task = one commit" was unobservable and a crash
// between commit and folder-move was ambiguous.
const coopTaskTrailer = "Coop-Task"

type taskTrailerCommit struct {
	info      commitInfo
	values    []string
	malformed bool
}

// taskTrailerCommits uses one NUL-delimited Git stream, so a trailer value can never be confused
// with the next commit record without paying one process launch per commit. Git's trailer parser
// identifies the final trailer block; the explicit inner separator preserves empty and duplicate
// Coop-Task occurrences so callers can fail closed.
func taskTrailerCommits(repo, rangeExpr string, reverse bool) ([]taskTrailerCommit, bool) {
	args := []string{"log"}
	if reverse {
		args = append(args, "--reverse")
	}
	format := "%h%x00%s%x00%(trailers:key=" + coopTaskTrailer + ",only,unfold,separator=%x1f)"
	args = append(args, "-z", "--format="+format)
	if rangeExpr != "" {
		args = append(args, rangeExpr)
	}
	cmd := exec.Command("git", gitArgs(repo, args)...)
	raw, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	fields := strings.Split(string(raw), "\x00")
	if len(fields) == 0 || fields[len(fields)-1] != "" || (len(fields)-1)%3 != 0 {
		return nil, false
	}
	commits := make([]taskTrailerCommit, 0, (len(fields)-1)/3)
	for i := 0; i < len(fields)-1; i += 3 {
		record := taskTrailerCommit{info: commitInfo{sha: fields[i], subject: fields[i+1]}}
		if fields[i+2] != "" {
			for _, trailer := range strings.Split(fields[i+2], "\x1f") {
				key, value, ok := strings.Cut(trailer, ":")
				if !ok || !strings.EqualFold(strings.TrimSpace(key), coopTaskTrailer) {
					record.malformed = true
					continue
				}
				record.values = append(record.values, strings.TrimSpace(value))
			}
		}
		commits = append(commits, record)
	}
	return commits, true
}

// commitsForTask returns the short shas whose sole Coop-Task trailer equals id. rangeExpr limits
// the search (e.g. "base..HEAD"); empty scans all of HEAD's reachable history.
func commitsForTask(repo, rangeExpr, id string) []string {
	var shas []string
	commits, ok := taskTrailerCommits(repo, rangeExpr, false)
	if !ok {
		return nil
	}
	for _, commit := range commits {
		if !commit.malformed && len(commit.values) == 1 && commit.values[0] == id {
			shas = append(shas, commit.info.sha)
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

// completeTrustedTask is the host equivalent of the provider completion boundary. It holds the
// task authority lock across move, finalization, and receipt creation so a concurrent loop can
// observe either the old state or an accepted done inode, never a transient.
func completeTrustedTask(root string, task taskItem) (retErr error) {
	windows, err := beginCompletionWindows([]string{root})
	if err != nil {
		return fmt.Errorf("%w: %v", errCompletionWindowSetup, err)
	}
	accepted := false
	var acceptedTask queuedTask
	defer func() {
		if accepted {
			retErr = errors.Join(retErr, windows.rejectAndClose(acceptedTask))
		} else {
			retErr = errors.Join(retErr, windows.abandon())
		}
	}()
	authority, err := openLeaseAuthority(root, task.ID, true)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(authority.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = authority.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return fmt.Errorf("task %s is leased by another controller", task.ID)
		}
		return err
	}
	defer func() { retErr = errors.Join(retErr, unlockLeaseFile(authority)) }()
	current, ok := currentTask(root, task.ID)
	if !ok || current.Dir != task.Dir || current.State != task.State {
		return errLeaseCandidateGone
	}
	reopen, reopened, err := readAuditReopenRecord(root, task.ID)
	if err != nil {
		return err
	}
	if err := clearLeaseCompletionReceipt(authority); err != nil {
		return err
	}
	if current.State != stateDone {
		if err := moveTaskDir(root, current, stateDone); err != nil {
			return err
		}
		current.State = stateDone
		current.Dir = filepath.Join(root, stateDone, current.ID)
	}
	if err := finalizeCompletedTask(current.ID, current.Dir); err != nil {
		return err
	}
	generation := ""
	if reopened {
		generation = reopen.Generation
	}
	if err := writeLeaseCompletionReceipt(authority, current.Dir, generation); err != nil {
		return err
	}
	if reopened {
		if err := removeAuditReopenRecordIfMatches(root, task.ID, generation); err != nil {
			return err
		}
	}
	acceptedTask = queuedTask{Root: root, Item: current}
	accepted = true
	return nil
}

// moveTrustedTaskFromDone invalidates completion evidence under the same task authority lock before
// a host command reopens or blocks an archived task. On a failed move it restores the old receipt,
// so concurrent supervised windows never see a false unowned completion.
func moveTrustedTaskFromDone(root string, task taskItem, newState string) error {
	return moveTrustedTaskFromDoneWith(root, task, newState, nil)
}

type trustedTaskMove struct {
	root          string
	task          taskItem
	newState      string
	sourceStates  []string
	metadataNames []string
	reopen        *auditReopenRecord
	afterMove     func(string) error
}

type trustedTaskMoveState struct {
	move                trustedTaskMove
	current             taskItem
	authority           *os.File
	metadata            map[string]taskMetadataSnapshot
	previous            leaseCompletionReceipt
	previousOK          bool
	previousReopen      auditReopenRecord
	previousReopenOK    bool
	previousDeparture   trustedDoneDeparture
	previousDepartureOK bool
	receiptTouched      bool
	reopenTouched       bool
	departureTouched    bool
	moved               bool
}

// moveTrustedTaskFromDoneWith is the one-task form of the atomic host move used for review
// verdicts. The callback may update only log.md and state.md.
func moveTrustedTaskFromDoneWith(root string, task taskItem, newState string, afterMove func(string) error) error {
	return moveTrustedTasksFromDoneWith([]trustedTaskMove{{
		root: root, task: task, newState: newState, afterMove: afterMove,
	}})
}

// moveTrustedTasksFromDoneWith holds every task authority lock before the first mutation. Review
// verdicts use this all-or-nothing boundary so a later lease or metadata failure cannot leave an
// earlier subject reopened. Every declared metadata file and completion receipt is restored if any
// callback or move fails.
func moveTrustedTasksFromDoneWith(moves []trustedTaskMove) (retErr error) {
	if len(moves) == 0 {
		return nil
	}
	moves = slices.Clone(moves)
	slices.SortFunc(moves, func(a, b trustedTaskMove) int {
		if byRoot := strings.Compare(a.root, b.root); byRoot != 0 {
			return byRoot
		}
		return strings.Compare(a.task.ID, b.task.ID)
	})
	var roots, taskIDs []string
	for i, move := range moves {
		if i > 0 && move.root == moves[i-1].root && move.task.ID == moves[i-1].task.ID {
			return fmt.Errorf("duplicate trusted task move for %s", move.task.ID)
		}
		if i == 0 || move.root != moves[i-1].root {
			roots = append(roots, move.root)
		}
		taskIDs = append(taskIDs, move.task.ID)
	}
	windows, err := beginCompletionWindowsAllowingTasks(roots, taskIDs)
	if err != nil {
		return fmt.Errorf("%w: %v", errCompletionWindowSetup, err)
	}
	windowOpen := true
	defer func() {
		if windowOpen {
			retErr = errors.Join(retErr, windows.abandon())
		}
	}()
	closeWindow := func(audit bool) error {
		windowOpen = false
		if audit {
			return windows.rejectAndClose(queuedTask{})
		}
		return windows.close()
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
		authority, err := openLeaseAuthority(move.root, move.task.ID, true)
		if err != nil {
			return failBeforeMutation(err)
		}
		state := trustedTaskMoveState{move: move, authority: authority}
		states = append(states, state)
		if err := syscall.Flock(int(authority.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
				err = fmt.Errorf("task %s is leased by another controller", move.task.ID)
			}
			return failBeforeMutation(err)
		}
		current, ok := currentTask(move.root, move.task.ID)
		if !ok {
			return failBeforeMutation(errLeaseCandidateGone)
		}
		if len(move.sourceStates) == 0 {
			if current.Dir != move.task.Dir || current.State != stateDone {
				return failBeforeMutation(errLeaseCandidateGone)
			}
		} else if !slices.Contains(move.sourceStates, current.State) {
			return failBeforeMutation(fmt.Errorf(
				"task %s is %s, want one of %s",
				move.task.ID,
				stateLabel(current.State),
				strings.Join(move.sourceStates, ", "),
			))
		}
		states[len(states)-1].current = current
		if move.afterMove != nil {
			names := move.metadataNames
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
		previousReopen, previousReopenOK, err := readAuditReopenRecord(move.root, move.task.ID)
		if err != nil {
			return failBeforeMutation(err)
		}
		states[len(states)-1].previousReopen = previousReopen
		states[len(states)-1].previousReopenOK = previousReopenOK
		previousDeparture, previousDepartureOK, err := readTrustedDoneDeparture(move.root, move.task.ID)
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
		if states[i].current.State == stateDone && states[i].move.newState != stateDone && states[i].previousOK {
			states[i].departureTouched = true
			if err := appendTrustedDoneDeparture(states[i].move.root, states[i].current.ID, states[i].previous.Nonce); err != nil {
				return rollback(err)
			}
		}
		states[i].receiptTouched = true
		if err := clearLeaseCompletionReceipt(states[i].authority); err != nil {
			return rollback(err)
		}
		if states[i].move.reopen != nil {
			states[i].reopenTouched = true
			if err := writeAuditReopenRecord(states[i].move.root, *states[i].move.reopen); err != nil {
				return rollback(err)
			}
		}
	}
	for i := range states {
		state := &states[i]
		if state.current.State != state.move.newState {
			if err := moveTaskDir(state.move.root, state.current, state.move.newState); err != nil {
				return rollback(err)
			}
			state.moved = true
		}
		if state.move.afterMove != nil {
			dir := filepath.Join(state.move.root, state.move.newState, state.current.ID)
			if err := state.move.afterMove(dir); err != nil {
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
		dir := filepath.Join(state.move.root, state.move.newState, state.current.ID)
		var metadataErr error
		if state.move.afterMove != nil {
			metadataErr = restoreTaskMetadata(dir, state.metadata)
		}
		var moveErr error
		if state.moved {
			moved := state.current
			moved.State = state.move.newState
			moved.Dir = dir
			moveErr = moveTaskDir(state.move.root, moved, state.current.State)
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
				err = writeTrustedDoneDeparture(state.move.root, state.previousDeparture)
			} else {
				err = removeTrustedDoneDeparture(state.move.root, state.current.ID)
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("restore task %s trusted done departure: %w", state.current.ID, err))
				restored[i] = false
			}
		}
		if state.reopenTouched {
			var err error
			if state.previousReopenOK {
				err = writeAuditReopenRecord(state.move.root, state.previousReopen)
			} else {
				err = removeAuditReopenRecord(state.move.root, state.current.ID)
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
		}
	}
	return errors.Join(errs...)
}

type taskMetadataSnapshot struct {
	body   []byte
	exists bool
}

func snapshotTaskMetadata(taskDir string, names ...string) (map[string]taskMetadataSnapshot, error) {
	root, err := openTaskMetadataRoot(taskDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	snapshots := make(map[string]taskMetadataSnapshot, len(names))
	for _, name := range names {
		body, err := readTaskMetadataFile(root, name)
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
	root, err := openTaskMetadataRoot(taskDir)
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
			errs = append(errs, atomicWriteTaskFile(root, name, snapshot.body))
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
// finalized the folder; provider-writable task metadata is never ownership evidence.
func rejectUnownedCompletions(tasks []queuedTask, assigned queuedTask) ([]string, error) {
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
		errs = append(errs, restoreUnownedCompletion(queuedTask{Root: task.Root, Item: current}), lock.clearCompleted(), lock.release())
	}
	slices.Sort(rejected)
	return rejected, errors.Join(errs...)
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

// reconcileInterruptedCompletions closes the crash window around accepted completion. A matching
// host receipt means finalization finished and only stale lease metadata needs removal. Without a
// receipt, the lease does not retain the iteration base, so the task is restored for a new
// range-bound attempt; an older matching commit must never validate new unbound work.
func reconcileInterruptedCompletions(hosts []string) error {
	var restoreErrs []error
	for _, host := range hosts {
		for _, task := range readTaskTree(host) {
			if task.State != stateDone {
				restoreErrs = append(restoreErrs, clearTaskCompletionReceipt(host, task.ID))
				continue
			}
			if !crashCompletionCandidate(host, task) {
				continue
			}
			lock, current, acquired, err := lockInterruptedCompletion(host, task)
			if err != nil {
				restoreErrs = append(restoreErrs, fmt.Errorf("lock interrupted task %s: %w", task.ID, err))
				continue
			}
			if !acquired {
				continue
			}
			if receipt, ok := lock.completionReceipt(current.Dir); ok {
				cleanupErr := errors.Join(
					removeAuditReopenRecordIfMatches(host, current.ID, receipt.AuditReopenGeneration),
					removeLeaseAuthorityMetadata(host, current.ID),
					removeLeaseMetadata(host, current.ID),
					lock.release(),
				)
				if cleanupErr != nil {
					restoreErrs = append(restoreErrs, fmt.Errorf("clean accepted task %s lease metadata: %w", task.ID, cleanupErr))
				}
				continue
			}
			restoreErr := restoreQueuedCompletion(queuedTask{Root: host, Item: current})
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
	files     []*os.File
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
	var errs []error
	for i := len(l.files) - 1; i >= 0; i-- {
		errs = append(errs, unlockLeaseFile(l.files[i]))
	}
	return errors.Join(errs...)
}

func lockCrashCompletion(root string, task taskItem) (crashCompletionLock, taskItem, bool, error) {
	return lockCompletionForAudit(root, task, false)
}

// lockInterruptedCompletion preserves compatibility with a pre-authority controller whose held
// task-local lock is still authoritative. Completion-window audits use lockCrashCompletion instead:
// their journal must not be retired merely because a non-authoritative local reader was transient.
func lockInterruptedCompletion(root string, task taskItem) (crashCompletionLock, taskItem, bool, error) {
	return lockCompletionForAudit(root, task, true)
}

func lockCompletionForAudit(root string, task taskItem, allowLegacyLocalOwner bool) (crashCompletionLock, taskItem, bool, error) {
	authority, err := openLeaseAuthority(root, task.ID, true)
	if err != nil {
		return crashCompletionLock{}, taskItem{}, false, err
	}
	locks := crashCompletionLock{authority: authority, files: []*os.File{authority}}
	if err := lockExclusiveForCompletionAudit(authority, "task "+task.ID+" authority", func() bool {
		return leaseAuthorityMetadataExists(root, task.ID)
	}); err != nil {
		_ = authority.Close()
		if errors.Is(err, errCompletionAuditLockOwned) {
			return crashCompletionLock{}, taskItem{}, false, nil
		}
		return crashCompletionLock{}, taskItem{}, false, err
	}
	local, err := openLeaseLock(task.Dir, false)
	if err == nil {
		lockErr := syscall.Flock(int(local.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if allowLegacyLocalOwner && (errors.Is(lockErr, syscall.EWOULDBLOCK) || errors.Is(lockErr, syscall.EAGAIN)) {
			_ = local.Close()
			_ = locks.release()
			return crashCompletionLock{}, taskItem{}, false, nil
		}
		if lockErr != nil {
			lockErr = lockExclusiveForCompletionAudit(local, "task "+task.ID+" local lease", nil)
		}
		if lockErr != nil {
			_ = local.Close()
			_ = locks.release()
			return crashCompletionLock{}, taskItem{}, false, lockErr
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

var errCompletionAuditLockOwned = errors.New("completion audit lock is owned")

// lockExclusiveForCompletionAudit waits out short host-only receipt reads and compatibility-lock
// observers. A real controller is identified by authority metadata; any other unresolved lock
// fails the audit so its durable journal is retained instead of accepting uncertainty.
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

func crashCompletionCandidate(root string, task taskItem) bool {
	if leaseAuthorityMetadataExists(root, task.ID) {
		return true
	}
	if auditReopenRecordExists(root, task.ID) {
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

type semanticTaskCommit struct {
	sha      string
	semantic auditReopenCommit
}

// semanticTaskCommits identifies task-bound commits by their exact introduced content and author
// intent. The raw diff-tree includes paths, modes, and old/new blob ids, so an unrelated ancestor
// repair does not change a replayed descendant's identity while any change to that descendant
// does. Author identity/date and the complete message make this deliberately stricter than
// patch-id.
func semanticTaskCommits(repo, rangeExpr string) ([]semanticTaskCommit, error) {
	commits, ok := taskTrailerCommits(repo, rangeExpr, true)
	if !ok {
		return nil, errors.New("read task-bound commit history")
	}
	var result []semanticTaskCommit
	for _, commit := range commits {
		if commit.malformed || len(commit.values) > 1 || (len(commit.values) == 1 && commit.values[0] == "") {
			return nil, errors.New("history contains an invalid task binding")
		}
		if len(commit.values) == 0 {
			continue
		}
		semantic, err := semanticCommit(repo, commit.info.sha, commit.values[0])
		if err != nil {
			return nil, err
		}
		result = append(result, semanticTaskCommit{sha: commit.info.sha, semantic: semantic})
	}
	return result, nil
}

func semanticCommit(repo, sha, taskID string) (auditReopenCommit, error) {
	parents := strings.Fields(gitOut(repo, "rev-list", "--parents", "-n", "1", sha))
	if len(parents) == 0 {
		return auditReopenCommit{}, fmt.Errorf("resolve task-bound commit %s", sha)
	}
	if len(parents) > 2 {
		return auditReopenCommit{}, fmt.Errorf("task-bound merge commit %s cannot be replayed safely", sha)
	}
	diffCmd := exec.Command("git", gitArgs(repo,
		[]string{"diff-tree", "--root", "--no-commit-id", "--raw", "-z", "-r", "--no-renames", sha})...)
	diff, err := diffCmd.Output()
	if err != nil {
		return auditReopenCommit{}, fmt.Errorf("read task-bound commit %s changes: %w", sha, err)
	}
	sum := sha256.Sum256(diff)
	metaCmd := exec.Command("git", gitArgs(repo,
		[]string{"show", "-s", "--format=format:%an%x00%ae%x00%aI%x00%B", sha})...)
	meta, err := metaCmd.Output()
	if err != nil {
		return auditReopenCommit{}, fmt.Errorf("read task-bound commit %s metadata: %w", sha, err)
	}
	fields := strings.SplitN(string(meta), "\x00", 4)
	if len(fields) != 4 {
		return auditReopenCommit{}, fmt.Errorf("parse task-bound commit %s metadata", sha)
	}
	return auditReopenCommit{
		TaskID:        taskID,
		ChangeTree:    fmt.Sprintf("%x", sum),
		AuthorName:    fields[0],
		AuthorEmail:   fields[1],
		AuthorDate:    fields[2],
		CommitMessage: fields[3],
	}, nil
}

func captureAuditReopen(repo, id string) (auditReopenRecord, error) {
	subjects := commitsForTask(repo, "", id)
	if len(subjects) != 1 {
		return auditReopenRecord{}, fmt.Errorf(
			"review subject %s needs exactly one reachable %s binding before reopen", id, coopTaskTrailer,
		)
	}
	subject, err := semanticCommit(repo, subjects[0], id)
	if err != nil {
		return auditReopenRecord{}, err
	}
	bound, err := semanticTaskCommits(repo, subjects[0]+"..HEAD")
	if err != nil {
		return auditReopenRecord{}, err
	}
	seen := map[string]bool{id: true}
	descendants := make([]auditReopenCommit, 0, len(bound))
	for _, commit := range bound {
		taskID := commit.semantic.TaskID
		if seen[taskID] || len(commitsForTask(repo, "HEAD", taskID)) != 1 {
			return auditReopenRecord{}, fmt.Errorf(
				"descendant task %s needs exactly one reachable %s binding before review reopens %s",
				taskID, coopTaskTrailer, id,
			)
		}
		seen[taskID] = true
		descendants = append(descendants, commit.semantic)
	}
	generation, err := newAuditReopenGeneration()
	if err != nil {
		return auditReopenRecord{}, err
	}
	return auditReopenRecord{
		Version: auditReopenVersion, Generation: generation, TaskID: id,
		Subject: subject, Descendants: descendants,
	}, nil
}

func auditReopenCompletionValid(repo, base, head, id string, record auditReopenRecord) bool {
	if validateAuditReopenRecord(record, id) != nil || len(commitsForTask(repo, head, id)) != 1 {
		return false
	}
	reachableSubject, err := semanticCommit(repo, commitsForTask(repo, head, id)[0], id)
	if err != nil {
		return false
	}
	if base == head {
		if reachableSubject != record.Subject {
			return false
		}
		current, err := semanticTaskCommits(repo, commitsForTask(repo, head, id)[0]+".."+head)
		if err != nil {
			return false
		}
		recorded := make(map[string]auditReopenCommit, len(record.Descendants))
		for _, descendant := range record.Descendants {
			recorded[descendant.TaskID] = descendant
		}
		var retained []auditReopenCommit
		for _, commit := range current {
			if _, ok := recorded[commit.semantic.TaskID]; ok {
				retained = append(retained, commit.semantic)
			}
		}
		return slices.Equal(retained, record.Descendants)
	}
	rangeCommits, err := semanticTaskCommits(repo, base+".."+head)
	if err != nil {
		return false
	}
	subjects := 0
	var descendants []auditReopenCommit
	for _, commit := range rangeCommits {
		if commit.semantic.TaskID == id {
			subjects++
			if commit.semantic.ChangeTree == record.Subject.ChangeTree {
				return false // a message-only receipt rewrite is not an implementation repair
			}
			continue
		}
		descendants = append(descendants, commit.semantic)
	}
	if subjects != 1 || !slices.Equal(descendants, record.Descendants) {
		return false
	}
	for _, descendant := range record.Descendants {
		if len(commitsForTask(repo, head, descendant.TaskID)) != 1 {
			return false
		}
	}
	return true
}

// preserveBlockedAuditReopen rebases an accepted review rewrite without consuming its single-use
// generation when the worker parks the task for external acceptance. The same semantic replay
// validation as completion prevents the provider from changing descendants, and the still-held
// authority lock makes replacement of the host record fail closed.
func (l *taskLease) preserveBlockedAuditReopen(repo, base, head string) error {
	if l.reopen == nil || base == head {
		return nil
	}
	current, ok := currentTask(l.root, l.id)
	if !ok || current.State != stateBlocked {
		return nil
	}
	if !auditReopenCompletionValid(repo, base, head, l.id, *l.reopen) {
		return fmt.Errorf("blocked audit rewrite for task %s changed its reviewed subject or descendants outside the host authority", l.id)
	}
	subjects := commitsForTask(repo, head, l.id)
	if len(subjects) != 1 {
		return fmt.Errorf("resolve blocked audit rewrite for task %s", l.id)
	}
	subject, err := semanticCommit(repo, subjects[0], l.id)
	if err != nil {
		return err
	}
	replacement := *l.reopen
	replacement.Subject = subject
	if err := replaceAuditReopenRecordIfMatches(l.root, *l.reopen, replacement); err != nil {
		return err
	}
	l.reopen = &replacement
	return nil
}

func completionUnbindableTasks(repo, base, head string, finished []string, reopen *auditReopenRecord) []string {
	if reopen == nil || len(finished) != 1 || finished[0] != reopen.TaskID {
		return unbindableTasks(repo, base, head, finished)
	}
	if auditReopenCompletionValid(repo, base, head, finished[0], *reopen) {
		return nil
	}
	return slices.Clone(finished)
}

// unbindableTasks returns finished ids without exactly one Coop-Task binding both in this
// iteration's range and across history reachable from the proposed HEAD. Reopened work therefore
// has to rewrite its existing bound commit instead of adding a second one. A no-HEAD-change
// completion always fails closed; crash recovery restores it for a fresh range.
func unbindableTasks(repo, base, head string, finished []string) []string {
	if base == "" || head == "" || base == head {
		return slices.Clone(finished)
	}
	search := base + ".." + head
	allowed := make(map[string]bool, len(finished))
	for _, id := range finished {
		allowed[id] = true
	}
	changes := loopChanges(repo, base, head)
	if changes.invalidTaskBindings {
		return slices.Clone(finished)
	}
	for _, id := range changes.taskIDs() {
		if !allowed[id] {
			return slices.Clone(finished)
		}
	}
	var missing []string
	for _, id := range finished {
		if len(commitsForTask(repo, search, id)) != 1 || len(commitsForTask(repo, head, id)) != 1 {
			missing = append(missing, id)
		}
	}
	return missing
}

func restoreQueuedCompletion(task queuedTask) error {
	id := task.Item.ID
	if task.Item.State == stateDone {
		if err := moveTaskDir(task.Root, task.Item, stateInProgress); err != nil {
			return fmt.Errorf("restore task %s: %w", id, err)
		}
	}
	dir := filepath.Join(task.Root, stateInProgress, id)
	note := fmt.Sprintf("completion rejected: expected exactly one commit with one matching %s trailer in the iteration's range and exactly one reachable binding overall; %s; rewrite or squash duplicate bindings down to one, then re-run `coop loop`", coopTaskTrailer, taskBindingRecovery(id))
	var errs []error
	if err := appendTaskLogStrict(dir, note); err != nil {
		errs = append(errs, fmt.Errorf("record rejection for task %s: %w", id, err))
	}
	if err := normalizeRejectedTaskState(id, dir); err != nil {
		errs = append(errs, fmt.Errorf("refresh rejected task %s: %w", id, err))
	}
	return errors.Join(errs...)
}

// restoreBackgroundHandoffCompletion rejects a completion produced before the provider observed
// its background gate/consult result. The next fresh provider gets a precise resume note instead
// of treating an incomplete asynchronous attempt as success.
func restoreBackgroundHandoffCompletion(task queuedTask) error {
	id := task.Item.ID
	if task.Item.State == stateDone {
		if err := moveTaskDir(task.Root, task.Item, stateInProgress); err != nil {
			return fmt.Errorf("restore background handoff task %s: %w", id, err)
		}
	}
	dir := filepath.Join(task.Root, stateInProgress, id)
	return errors.Join(
		appendTaskLogStrict(dir, "provider exited while an agent-owned background job remained live; host drained or terminated it, so this completion is restored for a fresh observed attempt"),
		normalizeTaskState(id, dir, "in progress — background handoff", "inspect the background result and rerun any ambiguous gate in the foreground", "the provider ended before its background work settled", "do not mark complete until every started gate, consult, or delegate has finished"),
	)
}

func restoreUnownedCompletion(task queuedTask) error {
	id := task.Item.ID
	if task.Item.State == stateDone {
		if err := moveTaskDir(task.Root, task.Item, stateInProgress); err != nil {
			return fmt.Errorf("restore unowned task %s: %w", id, err)
		}
	}
	dir := filepath.Join(task.Root, stateInProgress, id)
	note := "completion rejected: this provider iteration moved a task it did not lease; work exactly the assigned task, then re-run `coop loop`"
	return errors.Join(
		appendTaskLogStrict(dir, note),
		normalizeTaskState(id, dir, "in progress — completion rejected", "work this task only when it is assigned", "completion was rejected as unowned", "another iteration moved this task without its lease"),
	)
}

func restoreCompromisedCompletion(task queuedTask) error {
	id := task.Item.ID
	if task.Item.State == stateDone {
		if err := moveTaskDir(task.Root, task.Item, stateInProgress); err != nil {
			return fmt.Errorf("restore assigned task %s: %w", id, err)
		}
	}
	dir := filepath.Join(task.Root, stateInProgress, id)
	note := "completion rejected: this iteration also moved an unleased task, so its assigned completion was restored for a clean reviewed attempt"
	return errors.Join(
		appendTaskLogStrict(dir, note),
		normalizeTaskState(id, dir, "in progress — completion rejected", "resume the assigned task and complete it without touching another task", "the assigned work committed but its iteration violated task ownership", "the next completion needs a unique Coop-Recovery trailer"),
	)
}

func restoreUnrecordedCompletion(task queuedTask) error {
	id := task.Item.ID
	if task.Item.State == stateDone {
		if err := moveTaskDir(task.Root, task.Item, stateInProgress); err != nil {
			return fmt.Errorf("restore unrecorded task %s: %w", id, err)
		}
	}
	dir := filepath.Join(task.Root, stateInProgress, id)
	note := "completion rejected: host-only completion evidence could not be recorded before releasing the task lease"
	return errors.Join(
		appendTaskLogStrict(dir, note),
		normalizeTaskState(id, dir, "in progress — finalization failed", "fix the host completion-receipt error, then re-run `coop loop`", "the implementation committed but host finalization did not finish", "completion evidence must be recorded under the task authority lock"),
	)
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
	recoveries := make([]string, 0, len(ids))
	for _, id := range ids {
		recoveries = append(recoveries, fmt.Sprintf("%s: %s", id, taskBindingRecovery(id)))
	}
	msg := fmt.Sprintf("completion rejected for task(s) %s: the new commit range and reachable HEAD each need exactly one commit with one parseable `%s: <id>` trailer per task; task(s) restored to in_progress — %s; rewrite/squash duplicate bindings down to one, then re-run `coop loop`", strings.Join(ids, ", "), coopTaskTrailer, strings.Join(recoveries, "; "))
	if restoreErr != nil {
		return fmt.Errorf("%s; recovery bookkeeping also failed: %w", msg, restoreErr)
	}
	return errors.New(msg)
}

// taskBindingRecovery describes both safe history shapes. A bare amend is deliberately absent:
// without --only it can absorb unrelated staged work, and when the implementation is not HEAD it
// would attach the task to the wrong commit.
func taskBindingRecovery(id string) string {
	return fmt.Sprintf(
		"if the implementation commit is HEAD and only lacks the trailer, amend its message without touching the index "+
			"(`git commit --amend --only --no-edit --trailer %q`); if the implementation commit is older than HEAD, "+
			"do not amend the current HEAD — reword that implementation commit and replay its descendants; if the "+
			"matching trailer already exists outside the new range, amend or rewrite that same commit with the rework "+
			"and a unique `Coop-Recovery: <current UTC timestamp>` trailer while preserving exactly one reachable %s "+
			"binding; never add a second task-bound commit",
		coopTaskTrailer+": "+id, coopTaskTrailer,
	)
}

func unownedCompletionError(ids []string, restoreErr error) error {
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
func resumeLine(id string, commits []string) string {
	if len(commits) == 0 {
		return ""
	}
	return "Task " + id + " has commit(s) " + strings.Join(commits, ", ") + " already in history carrying " +
		"its Coop-Task trailer. Read its log.md/state.md and determine which case applies: (a) a prior " +
		"attempt COMMITTED then was interrupted before moving the folder to 99_done/ — verify that work " +
		"against the acceptance criteria, amend the commit with a unique `Coop-Recovery: <current UTC timestamp>` " +
		"trailer while preserving exactly one Coop-Task trailer, and finish the move, but do NOT redo it; or (b) the review REOPENED it " +
		"(its log.md will say what's wrong) — independently reproduce the finding; if it is false, re-close without a receipt-only commit; " +
		"otherwise do the rework by amending or rewriting the already-bound implementation commit, leaving exactly one reachable Coop-Task " +
		"binding and semantically unchanged later task commits; do not add a second task-bound commit. Disambiguate before acting."
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

// landedTasks is the set of task ids whose Coop-Task trailer appears in the exact landed range.
func landedTasks(repo, revRange string) map[string]bool {
	set := map[string]bool{}
	commits, ok := taskTrailerCommits(repo, revRange, false)
	if !ok {
		return set
	}
	for _, commit := range commits {
		if !commit.malformed && len(commit.values) == 1 && commit.values[0] != "" {
			set[commit.values[0]] = true
		}
	}
	return set
}

// reconcileQueueAfterMerge moves any parent-queue task whose Coop-Task trailer now sits in parent
// history (landed by the just-merged fork) from todo/ or in_progress/ to done/, with a reconcile
// note; a blocked task with a landed trailer is flagged for a human, never moved. Best-effort — the
// merge already succeeded, so a reconcile hiccup must not fail it. Prevents the parent loop from
// redoing work a fork already landed.
func (a *app) reconcileQueueAfterMerge(repo, forkName, revRange string) {
	queues, err := taskQueues(a.cfg, repo, nil)
	if err != nil {
		return
	}
	landed := landedTasks(repo, revRange)
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
			doneDir := filepath.Join(host, stateDone, act.ID)
			if err := completeTrustedTask(host, items[act.ID]); err != nil {
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
