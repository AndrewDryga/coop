package cli

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
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
const (
	coopTaskTrailer             = "Coop-Task"
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

type taskTrailerCommit struct {
	info          commitInfo
	fullSHA       string
	parents       string
	authorName    string
	authorEmail   string
	authorDate    string
	commitMessage string
	values        []string
	malformed     bool
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
	raw, err := auditCommandOutput(cmd, auditHistoryOutputLimit)
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

func auditHistoryCommitsLimited(repo, rangeExpr string, limit int) ([]taskTrailerCommit, bool) {
	args := []string{"log", "--reverse", fmt.Sprintf("--max-count=%d", limit)}
	return auditHistoryCommits(repo, args, []string{rangeExpr})
}

func auditHistoryCommits(repo string, args, revisions []string) ([]taskTrailerCommit, bool) {
	format := "%h%x00%H%x00%s%x00%P%x00%an%x00%ae%x00%aI%x00%B"
	args = append(args, "-z", "--format="+format)
	args = append(args, revisions...)
	cmd := exec.Command("git", gitArgs(repo, args)...)
	raw, err := auditCommandOutput(cmd, auditHistoryOutputLimit)
	if err != nil {
		return nil, false
	}
	fields := strings.Split(string(raw), "\x00")
	if len(fields) == 0 || fields[len(fields)-1] != "" || (len(fields)-1)%8 != 0 {
		return nil, false
	}
	commits := make([]taskTrailerCommit, 0, (len(fields)-1)/8)
	for i := 0; i < len(fields)-1; i += 8 {
		record := taskTrailerCommit{
			info:          commitInfo{sha: fields[i], subject: fields[i+2]},
			fullSHA:       fields[i+1],
			parents:       fields[i+3],
			authorName:    fields[i+4],
			authorEmail:   fields[i+5],
			authorDate:    fields[i+6],
			commitMessage: fields[i+7],
		}
		record.values, record.malformed = auditTaskTrailersFromMessage([]byte(record.commitMessage))
		commits = append(commits, record)
	}
	return commits, true
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
	if reopened {
		repo := gitOut(root, "rev-parse", "--show-toplevel")
		head := gitOut(root, "rev-parse", "--verify", "HEAD^{commit}")
		if auditReopenRecordLegacy(reopen) {
			return &legacyAuditAdoptionRequiredError{id: task.ID}
		}
		if reopen.UnblockPending {
			return &auditCompletionStateError{message: fmt.Sprintf(
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
			if current.State != stateBlocked {
				recovery = fmt.Sprintf(
					"run `coop tasks block %s` without changing Git history, then %s",
					task.ID, recovery,
				)
			}
			return &auditCompletionStateError{message: fmt.Sprintf(
				"task %s complete-history audit authority no longer matches current HEAD; "+
					"its exact recorded baseline is %s: %v — %s before completion",
				task.ID, reopen.BaselineHead, matchErr, recovery,
			)}
		}
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
				record, hasAuditAuthority, authorityErr := readAuditReopenRecord(host, current.ID)
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
					restoreErr := restoreQueuedCompletion(
						queuedTask{Root: host, Item: current},
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
					removeLeaseMetadata(host, current.ID),
					lock.release(),
				)
				if cleanupErr != nil {
					restoreErrs = append(restoreErrs, fmt.Errorf("clean accepted task %s lease metadata: %w", task.ID, cleanupErr))
				}
				continue
			}
			record, hasAuditAuthority, authorityErr := readAuditReopenRecord(host, current.ID)
			if authorityErr != nil {
				restoreErrs = append(restoreErrs, errors.Join(
					fmt.Errorf("inspect interrupted task %s audit authority: %w", current.ID, authorityErr),
					lock.release(),
				))
				continue
			}
			restoreErr := restoreQueuedCompletion(
				queuedTask{Root: host, Item: current},
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

type semanticHistoryCommit struct {
	sha      string
	semantic auditReopenCommit
}

// semanticHistoryCommits identifies every commit by its exact introduced content and author
// intent, retaining an optional task binding as ownership metadata. The raw diff-tree includes
// paths, modes, and old/new blob ids, so an unrelated ancestor repair does not change a replayed
// descendant's identity while any change to that descendant does. Author identity/date and the
// complete message make this deliberately stricter than patch-id.
func semanticHistoryCommits(repo, rangeExpr string) ([]semanticHistoryCommit, error) {
	return semanticHistoryCommitsLimit(repo, rangeExpr, auditReopenHistoryLimit)
}

func semanticHistoryCommitsLimit(repo, rangeExpr string, limit int) ([]semanticHistoryCommit, error) {
	commits, ok := auditHistoryCommitsLimited(repo, rangeExpr, limit+1)
	if !ok {
		return nil, errors.New("read complete audit history")
	}
	return semanticHistoryCommitsFromRecords(repo, commits, limit)
}

func semanticHistoryCommitsExact(repo string, rawHistory []rawAuditCommit) ([]semanticHistoryCommit, error) {
	commits := make([]taskTrailerCommit, len(rawHistory))
	for i := range rawHistory {
		raw := rawHistory[i]
		if raw.sha == "" || raw.authorDate == "" {
			return nil, errors.New("exact raw audit history metadata is incomplete")
		}
		commits[i] = taskTrailerCommit{
			fullSHA: raw.sha, authorName: raw.authorName, authorEmail: raw.authorEmail,
			authorDate: raw.authorDate, commitMessage: raw.commitMessage,
			values: slices.Clone(raw.taskValues), malformed: raw.taskBindingInvalid,
		}
	}
	changeTrees, err := semanticRawHistoryChangeTrees(repo, rawHistory)
	if err != nil {
		return nil, err
	}
	return semanticHistoryCommitsFromChangeTrees(commits, changeTrees, len(commits), false)
}

func semanticHistoryCommitsFromRecords(
	repo string,
	commits []taskTrailerCommit,
	limit int,
) ([]semanticHistoryCommit, error) {
	changeTrees, err := semanticHistoryChangeTrees(repo, commits)
	if err != nil {
		return nil, err
	}
	return semanticHistoryCommitsFromChangeTrees(commits, changeTrees, limit, true)
}

func semanticHistoryCommitsFromChangeTrees(
	commits []taskTrailerCommit,
	changeTrees []string,
	limit int,
	rejectTraversalMerges bool,
) ([]semanticHistoryCommit, error) {
	if len(commits) > limit {
		return nil, fmt.Errorf("audit history exceeds %d commits", limit)
	}
	if len(changeTrees) != len(commits) {
		return nil, errors.New("complete audit history changes are incomplete")
	}
	taskIDs := make([]string, len(commits))
	seen := map[string]bool{}
	for i, commit := range commits {
		if commit.malformed || len(commit.values) > 1 || (len(commit.values) == 1 && commit.values[0] == "") {
			return nil, errors.New("history contains an invalid task binding")
		}
		if !validAuditReopenHead(commit.fullSHA) {
			return nil, errors.New("history contains an invalid commit id")
		}
		if rejectTraversalMerges && len(strings.Fields(commit.parents)) > 1 {
			return nil, fmt.Errorf("audit history merge commit %s cannot be replayed safely", commit.fullSHA)
		}
		if len(commit.values) == 1 {
			taskIDs[i] = commit.values[0]
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
			semantic: auditReopenCommit{
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
	cmd := exec.Command("git", gitArgs(treeSnapshot,
		[]string{"diff-tree", "--stdin", "--root", "--always", "--raw", "-z", "-r", "--no-renames"})...)
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
	initCmd := exec.Command("git", gitArgs(snapshot, initArgs)...)
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

func auditEmptyTree(repo string) (string, error) {
	format := gitOut(repo, "rev-parse", "--show-object-format")
	switch format {
	case "sha1":
		return auditObjectID("tree", nil, sha1.Size*2), nil
	case "sha256":
		return auditObjectID("tree", nil, sha256.Size*2), nil
	default:
		return "", fmt.Errorf("derive repository empty tree for object format %q", format)
	}
}

// semanticHistoryChangeTrees batches raw diff extraction for the bounded history. Git prefixes
// each --stdin result with its full commit id; raw entries then carry exactly one NUL-delimited
// path because rename detection is disabled. Parsing that structure avoids spawning one Git
// process per commit while hashing the exact same bytes as semanticCommit.
func semanticHistoryChangeTrees(repo string, commits []taskTrailerCommit) ([]string, error) {
	if len(commits) == 0 {
		return []string{}, nil
	}
	var input strings.Builder
	for _, commit := range commits {
		input.WriteString(commit.fullSHA)
		input.WriteByte('\n')
	}
	cmd := exec.Command("git", gitArgs(repo,
		[]string{"diff-tree", "--stdin", "--root", "--always", "--raw", "-z", "-r", "--no-renames"})...)
	cmd.Stdin = strings.NewReader(input.String())
	raw, err := auditCommandOutput(cmd, auditDiffOutputLimit)
	if err != nil {
		return nil, fmt.Errorf("read complete audit history changes: %w", err)
	}
	expected := make([]string, len(commits))
	for i := range commits {
		expected[i] = commits[i].fullSHA
	}
	return parseSemanticHistoryChangeTrees(raw, expected)
}

func parseSemanticHistoryChangeTrees(raw []byte, expected []string) ([]string, error) {
	fields := bytes.Split(raw, []byte{0})
	if len(fields) == 0 || len(fields[len(fields)-1]) != 0 {
		return nil, errors.New("parse complete audit history changes")
	}
	fields = fields[:len(fields)-1]
	trees := make([]string, 0, len(expected))
	var diff bytes.Buffer
	current := -1
	flush := func() {
		if current < 0 {
			return
		}
		sum := sha256.Sum256(diff.Bytes())
		trees = append(trees, fmt.Sprintf("%x", sum))
		diff.Reset()
	}
	for i := 0; i < len(fields); {
		field := fields[i]
		if len(field) > 0 && field[0] == ':' {
			if current < 0 || i+1 >= len(fields) {
				return nil, errors.New("parse complete audit history raw change")
			}
			diff.Write(field)
			diff.WriteByte(0)
			diff.Write(fields[i+1])
			diff.WriteByte(0)
			i += 2
			continue
		}
		flush()
		current++
		if current >= len(expected) || string(field) != expected[current] {
			return nil, errors.New("complete audit history changes are out of order")
		}
		i++
	}
	flush()
	if len(trees) != len(expected) {
		return nil, errors.New("complete audit history changes are incomplete")
	}
	return trees, nil
}

func semanticCommit(repo, sha, taskID string) (auditReopenCommit, error) {
	semantic, _, err := semanticCommitAndParent(repo, sha, taskID)
	return semantic, err
}

func semanticCommitAndParent(repo, sha, taskID string) (auditReopenCommit, string, error) {
	rawParent, err := auditCommitParent(repo, sha)
	if err != nil {
		return auditReopenCommit{}, "", err
	}
	parents := strings.Fields(gitOut(repo, "rev-list", "--parents", "-n", "1", sha))
	if len(parents) == 0 {
		return auditReopenCommit{}, "", fmt.Errorf("resolve audit history commit %s", sha)
	}
	if (rawParent == "" && len(parents) != 1) ||
		(rawParent != "" && (len(parents) != 2 || parents[1] != rawParent)) {
		return auditReopenCommit{}, "", fmt.Errorf("audit history commit %s traversal parent differs from its raw object", sha)
	}
	diffCmd := exec.Command("git", gitArgs(repo,
		[]string{"diff-tree", "--root", "--no-commit-id", "--raw", "-z", "-r", "--no-renames", sha})...)
	diff, err := auditCommandOutput(diffCmd, auditDiffOutputLimit)
	if err != nil {
		return auditReopenCommit{}, "", fmt.Errorf("read audit history commit %s changes: %w", sha, err)
	}
	sum := sha256.Sum256(diff)
	metaCmd := exec.Command("git", gitArgs(repo,
		[]string{"show", "-s", "--format=format:%an%x00%ae%x00%aI%x00%B", sha})...)
	meta, err := auditCommandOutput(metaCmd, auditMetadataOutputLimit)
	if err != nil {
		return auditReopenCommit{}, "", fmt.Errorf("read audit history commit %s metadata: %w", sha, err)
	}
	fields := strings.SplitN(string(meta), "\x00", 4)
	if len(fields) != 4 {
		return auditReopenCommit{}, "", fmt.Errorf("parse audit history commit %s metadata", sha)
	}
	return auditReopenCommit{
		TaskID:        taskID,
		ChangeTree:    fmt.Sprintf("%x", sum),
		AuthorName:    fields[0],
		AuthorEmail:   fields[1],
		AuthorDate:    fields[2],
		CommitMessage: fields[3],
	}, rawParent, nil
}

// auditCommitParent returns the raw object's sole parent, or "" for a root commit. Reading the
// commit object directly keeps grafts and shallow boundaries from rewriting parent identity;
// gitHardening separately disables agent-writable replacement objects. Missing objects, malformed
// parents, and merges fail closed.
func auditCommitParent(repo, sha string) (parent string, err error) {
	resolved := gitOut(repo, "rev-parse", "--verify", sha+"^{commit}")
	if !validAuditReopenHead(resolved) {
		return "", fmt.Errorf("resolve audit history commit %s parent", sha)
	}
	batch, err := openAuditCommitBatch(repo)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, batch.close()) }()
	raw, err := batch.commit(resolved)
	if err != nil {
		return "", fmt.Errorf("resolve audit history commit %s parent: %w", sha, err)
	}
	parent, err = auditCommitParentFromRaw(resolved, raw)
	if err != nil || parent == "" {
		return parent, err
	}
	if _, err := batch.commit(parent); err != nil {
		return "", fmt.Errorf("resolve audit history commit %s parent: %w", sha, err)
	}
	return parent, nil
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

func auditCommitParentFromRaw(sha string, raw []byte) (string, error) {
	_, parents, err := auditCommitHeaderFromRaw(sha, raw)
	if err != nil {
		return "", err
	}
	if len(parents) > 1 {
		return "", fmt.Errorf("audit history merge commit %s cannot be replayed safely", sha)
	}
	if len(parents) == 0 {
		return "", nil
	}
	return parents[0], nil
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
			if strings.EqualFold(strings.TrimSpace(line), coopTaskTrailer) {
				invalid = true
			}
			currentCoop = false
			continue
		}
		sawTrailer = true
		currentCoop = strings.EqualFold(strings.TrimSpace(key), coopTaskTrailer)
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
	batch.cmd = exec.Command("git", gitArgs(repo, []string{"cat-file", "--batch"})...)
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
	current := gitOut(repo, "rev-parse", "--verify", head+"^{commit}")
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
	bindings := map[string][]string{}
	for _, commit := range commits {
		if !commit.taskBindingInvalid && len(commit.taskValues) == 1 {
			bindings[commit.taskValues[0]] = append(bindings[commit.taskValues[0]], commit.sha)
		}
	}
	return bindings, true
}

func rawAuditHistoryCount(repo, head string, count int) (history []rawAuditCommit, err error) {
	if count < 1 || count > auditReopenHistoryLimit+1 {
		return nil, errors.New("audit history length is outside its bound")
	}
	current := gitOut(repo, "rev-parse", "--verify", head+"^{commit}")
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
	current := gitOut(repo, "rev-parse", "--verify", head+"^{commit}")
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
			return nil, errors.New("raw audit history ended before its required boundary")
		}
		current = commit.parent
	}
	return nil, fmt.Errorf("raw audit history exceeds %d commits", limit)
}

func addAuditLinearHistoryBytes(total *int64, raw []byte) error {
	*total += int64(len(raw))
	if *total > auditLinearHistoryByteLimit {
		return fmt.Errorf("raw linear audit history exceeds %d bytes", auditLinearHistoryByteLimit)
	}
	return nil
}

func auditReviewedSubject(repo, id string, record auditReopenRecord) (rawAuditCommit, error) {
	rawHistory, err := rawAuditHistoryCount(repo, record.BaselineHead, len(record.History)+1)
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

func auditReviewedSubjectParent(repo, id string, record auditReopenRecord) (string, error) {
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

func rawAuditHistoryFromSubject(repo, head, subject string) ([]semanticHistoryCommit, error) {
	rawHistory, err := rawAuditHistoryUntil(
		repo,
		head,
		auditReopenHistoryLimit+1,
		func(commit rawAuditCommit) bool { return commit.sha == subject },
	)
	if err != nil {
		return nil, err
	}
	return semanticHistoryCommitsExact(repo, rawHistory)
}

func rawAuditRewriteHistory(repo, head, reviewedParent string, limit int) ([]rawAuditCommit, error) {
	return rawAuditHistoryUntil(
		repo,
		head,
		limit,
		func(commit rawAuditCommit) bool { return commit.parent == reviewedParent },
	)
}

func captureAuditReopen(repo, id string) (auditReopenRecord, error) {
	head := gitOut(repo, "rev-parse", "--verify", "HEAD^{commit}")
	if !validAuditReopenHead(head) {
		return auditReopenRecord{}, errors.New("resolve audit reopen baseline HEAD")
	}
	bindings, ok := rawTaskBindings(repo, head)
	if !ok {
		return auditReopenRecord{}, errors.New("read raw reachable task bindings for audit reopen")
	}
	subjects := bindings[id]
	if len(subjects) != 1 {
		return auditReopenRecord{}, fmt.Errorf(
			"review subject %s needs exactly one reachable %s binding before reopen", id, coopTaskTrailer,
		)
	}
	rawHistory, err := rawAuditHistoryUntil(
		repo,
		head,
		auditReopenHistoryLimit+1,
		func(commit rawAuditCommit) bool { return commit.sha == subjects[0] },
	)
	if err != nil {
		return auditReopenRecord{}, err
	}
	commits, err := semanticHistoryCommitsExact(repo, rawHistory)
	if err != nil {
		return auditReopenRecord{}, err
	}
	if len(commits) == 0 || commits[0].semantic.TaskID != id {
		return auditReopenRecord{}, fmt.Errorf("resolve raw review subject %s", id)
	}
	seen := map[string]bool{id: true}
	history := make([]auditReopenCommit, 0, len(commits)-1)
	for _, commit := range commits[1:] {
		taskID := commit.semantic.TaskID
		if taskID != "" && (seen[taskID] || len(bindings[taskID]) != 1) {
			return auditReopenRecord{}, fmt.Errorf(
				"descendant task %s needs exactly one reachable %s binding before review reopens %s",
				taskID, coopTaskTrailer, id,
			)
		}
		if taskID != "" {
			seen[taskID] = true
		}
		history = append(history, commit.semantic)
	}
	generation, err := newAuditReopenGeneration()
	if err != nil {
		return auditReopenRecord{}, err
	}
	return auditReopenRecord{
		Version: auditReopenVersion, Generation: generation, TaskID: id, BaselineHead: head,
		Subject: commits[0].semantic, History: history,
	}, nil
}

func auditReopenCurrentHistory(repo, head, id string, record auditReopenRecord) ([]semanticHistoryCommit, error) {
	if err := validateAuditReopenRecord(record, id); err != nil {
		return nil, fmt.Errorf("validate persisted audit authority: %w", err)
	}
	if !auditReopenRecordActive(record) {
		return nil, errors.New("audit authority is not active")
	}
	resolvedHead := gitOut(repo, "rev-parse", "--verify", head+"^{commit}")
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
	if complete[len(record.History)].sha != record.BaselineHead {
		return nil, fmt.Errorf("recorded history prefix does not terminate at baseline %s", record.BaselineHead)
	}
	bindingCounts, ok := taskBindingCounts(repo, resolvedHead)
	if !ok {
		return nil, errors.New("read reachable task bindings for current audit history")
	}
	for _, commit := range complete {
		if commit.semantic.TaskID != "" &&
			bindingCounts[commit.semantic.TaskID] != 1 {
			return nil, fmt.Errorf("task %s no longer has exactly one reachable history binding", commit.semantic.TaskID)
		}
	}
	return complete[1:], nil
}

func auditReopenCurrentValid(repo, head, id string, record auditReopenRecord) bool {
	_, err := auditReopenCurrentHistory(repo, head, id, record)
	return err == nil
}

func auditReopenCompletionValid(repo, base, head, id string, record auditReopenRecord) bool {
	if base == head {
		return auditReopenCurrentValid(repo, head, id, record)
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

func rebasedAuditReopenRecord(repo, base, head, id string, record auditReopenRecord) (auditReopenRecord, error) {
	if !auditReopenCompletionValid(repo, base, head, id, record) {
		return auditReopenRecord{}, fmt.Errorf("audit rewrite for task %s changed its reviewed subject or descendants outside the host authority", id)
	}
	baseHistory, err := auditReopenCurrentHistory(repo, base, id, record)
	if err != nil {
		return auditReopenRecord{}, err
	}
	replacement, _, err := rawAuditHistory(repo, head, len(baseHistory)+1)
	if err != nil {
		return auditReopenRecord{}, err
	}
	if len(replacement) != len(baseHistory)+1 {
		return auditReopenRecord{}, fmt.Errorf("resolve complete audit rewrite for task %s", id)
	}
	history := make([]auditReopenCommit, len(replacement)-1)
	for i := range history {
		history[i] = replacement[i+1].semantic
	}
	baselineHead := gitOut(repo, "rev-parse", "--verify", head+"^{commit}")
	if !validAuditReopenHead(baselineHead) {
		return auditReopenRecord{}, fmt.Errorf("resolve rebased audit HEAD for task %s", id)
	}
	rebased := record
	rebased.Version = auditReopenVersion
	rebased.BaselineHead = baselineHead
	rebased.Subject = replacement[0].semantic
	rebased.History = history
	rebased.Descendants = nil
	rebased.UnblockPending = false
	return rebased, nil
}

func auditReopenLegacyBaselineMatches(repo, head, id string, record auditReopenRecord) bool {
	if validateAuditReopenRecord(record, id) != nil || !auditReopenRecordLegacy(record) {
		return false
	}
	bindings, ok := rawTaskBindings(repo, head)
	if !ok || len(bindings[id]) != 1 {
		return false
	}
	history, err := rawAuditHistoryFromSubject(repo, head, bindings[id][0])
	if err != nil || len(history) == 0 || history[0].semantic != record.Subject {
		return false
	}
	var descendants []auditReopenCommit
	for _, commit := range history[1:] {
		if commit.semantic.TaskID != "" {
			descendants = append(descendants, commit.semantic)
		}
	}
	if len(descendants) != len(record.Descendants) {
		return false
	}
	for i := range descendants {
		if descendants[i] != record.Descendants[i] {
			return false
		}
	}
	return true
}

type auditCompletionRecoveryError interface {
	error
	auditCompletionRecovery()
}

type legacyAuditAdoptionRequiredError struct{ id string }

func (e *legacyAuditAdoptionRequiredError) Error() string {
	return fmt.Sprintf(
		"task %s has legacy task-bound-only audit authority; if it is not blocked, run "+
			"`coop tasks block %s` without changing Git history; preserve wanted work, restore the "+
			"audited pre-attempt HEAD, verify `git rev-parse HEAD`, then run "+
			"`coop tasks unblock %s --adopt-audit-head <full-sha> \"<answer>\"`; "+
			"do not retry completion until that authority is active",
		e.id, e.id, e.id,
	)
}

func (*legacyAuditAdoptionRequiredError) auditCompletionRecovery() {}

type auditCompletionStateError struct{ message string }

func (e *auditCompletionStateError) Error() string          { return e.message }
func (*auditCompletionStateError) auditCompletionRecovery() {}

func adoptLegacyAuditReopen(
	repo, head, id string,
	record auditReopenRecord,
	adoptionHead string,
) (auditReopenRecord, error) {
	if !validAuditReopenHead(adoptionHead) || adoptionHead != head {
		return auditReopenRecord{}, fmt.Errorf(
			"legacy audit adoption for task %s was authorized for %s, but current HEAD is %s; "+
				"preserve wanted work, restore %s exactly, verify `git rev-parse HEAD` prints %s, "+
				"then retry with the same --adopt-audit-head value",
			id, adoptionHead, head, adoptionHead, adoptionHead,
		)
	}
	if !auditReopenLegacyBaselineMatches(repo, head, id, record) {
		return auditReopenRecord{}, fmt.Errorf(
			"current HEAD %s does not match task %s's legacy subject and task-bound descendant projection",
			head, id,
		)
	}
	bindings, ok := rawTaskBindings(repo, head)
	if !ok || len(bindings[id]) != 1 {
		return auditReopenRecord{}, fmt.Errorf("read reachable task bindings before legacy adoption of %s", id)
	}
	complete, err := rawAuditHistoryFromSubject(repo, head, bindings[id][0])
	if err != nil || len(complete) == 0 {
		return auditReopenRecord{}, fmt.Errorf("read complete legacy audit history for %s", id)
	}
	history := make([]auditReopenCommit, len(complete)-1)
	for i := range history {
		history[i] = complete[i+1].semantic
		if history[i].TaskID != "" && len(bindings[history[i].TaskID]) != 1 {
			return auditReopenRecord{}, fmt.Errorf(
				"descendant task %s needs exactly one reachable %s binding before legacy adoption of %s",
				history[i].TaskID, coopTaskTrailer, id,
			)
		}
	}
	replacement := auditReopenRecord{
		Version: auditReopenVersion, Generation: record.Generation, TaskID: id,
		BaselineHead: head, Subject: record.Subject, History: history,
	}
	if validateAuditReopenRecord(replacement, id) != nil ||
		!auditReopenCurrentValid(repo, head, id, replacement) {
		return auditReopenRecord{}, fmt.Errorf("capture complete legacy audit history for task %s", id)
	}
	return replacement, nil
}

// upgradeBlockedAuditReopen recovers a rewrite from the exact baseline recorded by the host.
// The complete replay must be the first sequence after the rewritten subject; unrelated work that
// landed later may remain as a suffix, but no reflog guess or task-only projection can authorize it.
func upgradeBlockedAuditReopen(repo, head, id string, record auditReopenRecord) (auditReopenRecord, error) {
	if validateAuditReopenRecord(record, id) != nil || !auditReopenRecordActive(record) {
		return auditReopenRecord{}, fmt.Errorf("invalid audit reopen authority for task %s", id)
	}
	if !auditReopenCurrentValid(repo, record.BaselineHead, id, record) {
		return auditReopenRecord{}, fmt.Errorf(
			"blocked audit task %s recorded baseline %s is unavailable or no longer matches its host authority",
			id, record.BaselineHead,
		)
	}
	reviewedParent, err := auditReviewedSubjectParent(repo, id, record)
	if err != nil {
		return auditReopenRecord{}, fmt.Errorf("resolve blocked audit subject parent for task %s: %w", id, err)
	}
	current, err := rawAuditRewriteHistory(
		repo,
		head,
		reviewedParent,
		auditReopenHistoryLimit+1,
	)
	if err != nil {
		return auditReopenRecord{}, fmt.Errorf("read blocked audit rewrite for task %s: %w", id, err)
	}
	rewriteLen := len(record.History) + 1
	if len(current) < rewriteLen {
		return auditReopenRecord{}, fmt.Errorf("blocked audit task %s has no complete replay after its recorded baseline", id)
	}
	rewriteHead := current[rewriteLen-1].sha
	if rewriteHead == "" {
		return auditReopenRecord{}, fmt.Errorf("resolve blocked audit rewrite terminal for task %s", id)
	}
	replacement, err := rebasedAuditReopenRecord(repo, record.BaselineHead, rewriteHead, id, record)
	if err != nil || !auditReopenCurrentValid(repo, head, id, replacement) {
		return auditReopenRecord{}, fmt.Errorf("blocked audit task %s changed its subject or complete descendant history", id)
	}
	return replacement, nil
}

type blockedAuditUnblock struct {
	authority                      *os.File
	root                           string
	previous, pending, replacement *auditReopenRecord
	pendingPersisted               bool
}

func (u *blockedAuditUnblock) markPending() error {
	if u == nil || u.pending == nil {
		return nil
	}
	if err := replaceAuditReopenRecordIfMatches(u.root, *u.previous, *u.pending); err != nil {
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

// prepareBlockedAuditReopenUnblock validates and, for a pre-upgrade blocked task, computes a rebase
// of the same host generation. The returned transaction holds the authority flock; before moving,
// it persists a non-authorizing pending form that makes a crash safe and recoverable.
func prepareBlockedAuditReopenUnblock(root string, task taskItem) (*blockedAuditUnblock, error) {
	return prepareBlockedAuditReopenUnblockWithAdoption(root, task, "")
}

func prepareBlockedAuditReopenUnblockWithAdoption(root string, task taskItem, adoptionHead string) (*blockedAuditUnblock, error) {
	observed, ok, err := readAuditReopenRecord(root, task.ID)
	if err != nil {
		return nil, fmt.Errorf("read blocked audit reopen authority for task %s: %w", task.ID, err)
	}
	if !ok {
		if adoptionHead != "" {
			return nil, fmt.Errorf("task %s has no legacy audit-reopen authority to adopt", task.ID)
		}
		return nil, nil
	}
	return lockBlockedAuditReopenUnblockWithAdoption(root, task, observed, adoptionHead)
}

func lockBlockedAuditReopenUnblock(root string, task taskItem, observed auditReopenRecord) (*blockedAuditUnblock, error) {
	return lockBlockedAuditReopenUnblockWithAdoption(root, task, observed, "")
}

func lockBlockedAuditReopenUnblockWithAdoption(
	root string,
	task taskItem,
	observed auditReopenRecord,
	adoptionHead string,
) (*blockedAuditUnblock, error) {
	authority, err := openLeaseAuthority(root, task.ID, false)
	if err != nil {
		return nil, fmt.Errorf("open blocked audit reopen authority for task %s: %w", task.ID, err)
	}
	if err := syscall.Flock(int(authority.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = authority.Close()
		return nil, fmt.Errorf("lock blocked audit reopen authority for task %s: %w", task.ID, err)
	}
	upgrade := &blockedAuditUnblock{authority: authority, root: root}
	fail := func(err error) (*blockedAuditUnblock, error) {
		return nil, upgrade.finish(err)
	}
	record, ok, err := readAuditReopenRecord(root, task.ID)
	if err != nil {
		return fail(fmt.Errorf("re-read blocked audit reopen authority for task %s: %w", task.ID, err))
	}
	if !ok || !auditReopenRecordsEqual(record, observed) {
		return fail(fmt.Errorf("audit reopen authority changed while unblocking task %s", task.ID))
	}
	current, ok := currentTask(root, task.ID)
	if !ok || current.State != stateBlocked || current.Dir != task.Dir {
		return fail(fmt.Errorf("task %s changed state while its blocked audit authority was locked", task.ID))
	}
	repo := gitOut(root, "rev-parse", "--show-toplevel")
	head := gitOut(root, "rev-parse", "HEAD")
	if repo == "" || head == "" {
		return fail(fmt.Errorf("resolve repository history for blocked audit task %s", task.ID))
	}
	if auditReopenRecordLegacy(record) {
		if adoptionHead == "" {
			return fail(&legacyAuditAdoptionRequiredError{id: task.ID})
		}
		replacement, err := adoptLegacyAuditReopen(repo, head, task.ID, record, adoptionHead)
		if err != nil {
			return fail(err)
		}
		pending := replacement
		pending.Version = auditReopenPendingVersion
		pending.UnblockPending = true
		upgrade.previous = &record
		upgrade.pending = &pending
		upgrade.replacement = &replacement
		return upgrade, nil
	}
	if adoptionHead != "" {
		return fail(fmt.Errorf("task %s already has complete-history audit authority; --adopt-audit-head is only for legacy records", task.ID))
	}
	if record.UnblockPending {
		replacement := record
		replacement.Version = auditReopenVersion
		replacement.UnblockPending = false
		if !auditReopenCurrentValid(repo, head, task.ID, replacement) {
			return fail(fmt.Errorf("pending audit unblock for task %s no longer matches repository history", task.ID))
		}
		upgrade.previous = &record
		upgrade.replacement = &replacement
		return upgrade, nil
	}
	if auditReopenCompletionValid(repo, head, head, task.ID, record) {
		return upgrade, nil
	}
	replacement, err := upgradeBlockedAuditReopen(repo, head, task.ID, record)
	if err != nil {
		return fail(err)
	}
	pending := replacement
	pending.Version = auditReopenPendingVersion
	pending.UnblockPending = true
	upgrade.previous = &record
	upgrade.pending = &pending
	upgrade.replacement = &replacement
	return upgrade, nil
}

// finishPendingAuditUnblock is the explicit host recovery for a crash after the authorized folder
// move. Pending authority never self-activates from queue state or lease acquisition.
func finishPendingAuditUnblock(root string, task taskItem, observed auditReopenRecord) (bool, error) {
	authority, err := openLeaseAuthority(root, task.ID, false)
	if err != nil {
		return false, fmt.Errorf("open pending audit unblock authority for task %s: %w", task.ID, err)
	}
	if err := syscall.Flock(int(authority.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = authority.Close()
		return false, fmt.Errorf("lock pending audit unblock authority for task %s: %w", task.ID, err)
	}
	fail := func(err error) (bool, error) {
		return false, errors.Join(err, unlockLeaseFile(authority))
	}
	record, ok, err := readAuditReopenRecord(root, task.ID)
	if err != nil {
		return fail(fmt.Errorf("re-read pending audit unblock authority for task %s: %w", task.ID, err))
	}
	if !ok || !record.UnblockPending || !auditReopenRecordsEqual(record, observed) {
		return fail(fmt.Errorf("pending audit unblock authority changed for task %s", task.ID))
	}
	current, ok := currentTask(root, task.ID)
	if !ok || current.State != stateTodo || current.Dir != task.Dir {
		return fail(fmt.Errorf("task %s is no longer the todo task from its pending audit unblock", task.ID))
	}
	repo := gitOut(root, "rev-parse", "--show-toplevel")
	head := gitOut(root, "rev-parse", "HEAD")
	replacement := record
	replacement.Version = auditReopenVersion
	replacement.UnblockPending = false
	if repo == "" || head == "" || !auditReopenCurrentValid(repo, head, task.ID, replacement) {
		return fail(fmt.Errorf("pending audit unblock for task %s no longer matches repository history", task.ID))
	}
	if err := replaceAuditReopenRecordIfMatches(root, record, replacement); err != nil {
		return fail(err)
	}
	if err := unlockLeaseFile(authority); err != nil {
		return true, err
	}
	return true, nil
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
	replacement, err := rebasedAuditReopenRecord(repo, base, head, l.id, *l.reopen)
	if err != nil {
		return err
	}
	if err := replaceAuditReopenRecordIfMatches(l.root, *l.reopen, replacement); err != nil {
		return err
	}
	l.reopen = &replacement
	return nil
}

// rebaseTimedOutAuditReopen keeps held audit authority truthful after a timed-out attempt.
// An unchanged tree leaves the recorded baseline valid. A tree-changing attempt is accepted
// only when it is a complete, semantically valid rewrite of the reviewed subject — then the
// authority rebases onto the new head without consuming its single-use generation. Anything
// else errors so the caller can park the task fail-closed instead of retrying on a tree the
// audit never authorized.
func (l *taskLease) rebaseTimedOutAuditReopen(repo, base, head string) error {
	if l.reopen == nil || base == head {
		return nil
	}
	replacement, err := rebasedAuditReopenRecord(repo, base, head, l.id, *l.reopen)
	if err != nil {
		return err
	}
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
	if auditReopenCompletionValid(repo, base, head, finished[0], *reopen) &&
		ordinaryBindingMatchesRaw(repo, head, finished[0]) {
		return nil
	}
	return slices.Clone(finished)
}

func ordinaryBindingMatchesRaw(repo, head, id string) bool {
	ordinary := commitsForTask(repo, head, id)
	raw, ok := rawTaskBindings(repo, head)
	if !ok || len(ordinary) != 1 || len(raw[id]) != 1 {
		return false
	}
	resolved := gitOut(repo, "rev-parse", "--verify", ordinary[0]+"^{commit}")
	return resolved == raw[id][0]
}

// unbindableTasks returns finished ids without exactly one Coop-Task binding both in this
// iteration's range and across history reachable from the proposed HEAD. Reopened work therefore
// has to rewrite its existing bound commit instead of adding a second one. A no-HEAD-change
// completion always fails closed; crash recovery restores it for a fresh range.
func unbindableTasks(repo, base, head string, finished []string) []string {
	if base == "" || head == "" || base == head {
		return slices.Clone(finished)
	}
	headCommits, headErr := rawReachableAuditCommits(repo, head)
	baseCommits, baseErr := rawReachableAuditCommits(repo, base)
	if headErr != nil || baseErr != nil {
		return slices.Clone(finished)
	}
	baseReachable := make(map[string]bool, len(baseCommits))
	for _, commit := range baseCommits {
		baseReachable[commit.sha] = true
	}
	allowed := make(map[string]bool, len(finished))
	for _, id := range finished {
		allowed[id] = true
	}
	rangeBindings := make(map[string]int, len(finished))
	reachableBindings := make(map[string]int, len(finished))
	reachableInvalid := make(map[string]bool, len(finished))
	for _, commit := range headCommits {
		inRange := !baseReachable[commit.sha]
		if inRange && (commit.taskBindingInvalid || len(commit.taskValues) > 1) {
			return slices.Clone(finished)
		}
		for _, id := range commit.taskValues {
			if !allowed[id] {
				if inRange {
					return slices.Clone(finished)
				}
				continue
			}
			if commit.taskBindingInvalid || len(commit.taskValues) != 1 {
				reachableInvalid[id] = true
				continue
			}
			reachableBindings[id]++
			if inRange {
				rangeBindings[id]++
			}
		}
	}
	var missing []string
	for _, id := range finished {
		if rangeBindings[id] != 1 || reachableBindings[id] != 1 || reachableInvalid[id] {
			missing = append(missing, id)
		}
	}
	return missing
}

func restoreQueuedCompletion(task queuedTask, audit bool) error {
	id := task.Item.ID
	if task.Item.State == stateDone {
		if err := moveTaskDir(task.Root, task.Item, stateInProgress); err != nil {
			return fmt.Errorf("restore task %s: %w", id, err)
		}
	}
	dir := filepath.Join(task.Root, stateInProgress, id)
	note := fmt.Sprintf("completion rejected: expected exactly one commit with one matching %s trailer in the iteration's range and exactly one reachable binding overall; %s; rewrite or squash duplicate bindings down to one, then re-run `coop loop`", coopTaskTrailer, taskBindingRecovery(id))
	normalize := normalizeRejectedTaskState
	if audit {
		note = fmt.Sprintf("completion rejected: %s; then re-run `coop loop`", auditBindingRecovery(id))
		normalize = normalizeAuditRejectedTaskState
	}
	var errs []error
	if err := appendTaskLogStrict(dir, note); err != nil {
		errs = append(errs, fmt.Errorf("record rejection for task %s: %w", id, err))
	}
	if err := normalize(id, dir); err != nil {
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

// restoreProviderTimeoutCompletion rejects a completion produced by an attempt the watchdog
// killed for proven silence. The provider may have moved the folder and then wedged before the
// host could observe a trustworthy finish, so the completion is restored for a fresh observed
// attempt with the standard recovery contract for any commit it already landed.
func restoreProviderTimeoutCompletion(task queuedTask, audit bool) error {
	id := task.Item.ID
	if task.Item.State == stateDone {
		if err := moveTaskDir(task.Root, task.Item, stateInProgress); err != nil {
			return fmt.Errorf("restore timed-out task %s: %w", id, err)
		}
	}
	dir := filepath.Join(task.Root, stateInProgress, id)
	trap := "if the prior attempt already committed, follow the informed-resume recovery: verify the work, then amend with a unique Coop-Recovery trailer while preserving exactly one Coop-Task binding"
	if audit {
		trap = "the next completion stays under the host audit authority: zero new commits or a real tree change, never a Coop-Recovery receipt"
	}
	return errors.Join(
		appendTaskLogStrict(dir, "the host watchdog killed this provider attempt after it stopped producing observable progress; its completion was restored for a fresh observed attempt"),
		normalizeTaskState(id, dir, "in progress — provider timeout", "resume the task and complete it under a live provider attempt", "the provider went silent and was killed before its completion could be trusted", trap),
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

func restoreCompromisedCompletion(task queuedTask, audit bool) error {
	id := task.Item.ID
	if task.Item.State == stateDone {
		if err := moveTaskDir(task.Root, task.Item, stateInProgress); err != nil {
			return fmt.Errorf("restore assigned task %s: %w", id, err)
		}
	}
	dir := filepath.Join(task.Root, stateInProgress, id)
	// The restored task's next completion is validated by its lease authority: a Coop-Recovery
	// receipt closes an ordinary rejected range, but under audit authority that same receipt is the
	// message-only rewrite audit validation rejects.
	trap := "the next completion needs a unique Coop-Recovery trailer"
	if audit {
		trap = "the next completion stays under the host audit authority: zero new commits or a real tree change, never a Coop-Recovery receipt"
	}
	note := "completion rejected: this iteration also moved an unleased task, so its assigned completion was restored for a clean reviewed attempt"
	return errors.Join(
		appendTaskLogStrict(dir, note),
		normalizeTaskState(id, dir, "in progress — completion rejected", "resume the assigned task and complete it without touching another task", "the assigned work committed but its iteration violated task ownership", trap),
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

func normalizeAuditRejectedTaskState(id, taskDir string) error {
	return normalizeTaskState(
		id,
		taskDir,
		"in progress — completion rejected",
		"independently verify the audit finding, then re-close with zero commits or a real tree change, and re-run `coop loop`",
		"completion was rejected by the host audit authority",
		"a message-only rewrite or a recovery-only descendant replay is rejected; never add a Coop-Recovery trailer",
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

// auditBindingRecovery replaces the generic trailer recipe when the host's audit-reopen authority
// owns the completion. The case-(a) receipt (a Coop-Recovery trailer on an unchanged tree) is
// exactly the shape audit validation rejects, so the remedy never prescribes it: either the
// finding is false and the re-close needs zero commits, or it is real and the subject tree must
// actually change.
func auditBindingRecovery(id string) string {
	return fmt.Sprintf(
		"task %s is host-authorized review rework: independently verify the recorded finding; if it is false, "+
			"re-close with zero new commits; if it is real, amend or rewrite the already-bound implementation "+
			"commit so its tree actually changes, keeping exactly one reachable %s binding and semantically "+
			"unchanged later commits, including commits with no task binding; a Coop-Recovery trailer, a message-only commit, or a recovery-only "+
			"replay of unchanged descendants will be rejected again",
		id, coopTaskTrailer,
	)
}

func auditCompletionError(id string, restoreErr error) error {
	msg := fmt.Sprintf("completion rejected for audit-reopened task %s: the host audit authority accepts only a zero-commit verification-only re-close or a rewrite whose subject tree actually changes with semantically unchanged descendants; task restored to in_progress — %s; then re-run `coop loop`", id, auditBindingRecovery(id))
	if restoreErr != nil {
		return fmt.Errorf("%s; recovery bookkeeping also failed: %w", msg, restoreErr)
	}
	return errors.New(msg)
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

// auditResumeLine is the informed-resume preamble when the assigned lease carries the host's
// audit-reopen authority. The record — not commit presence — selects the remedy: this is
// host-authorized review rework, never case-(a) crash recovery, and the case-(a) recipe (a
// Coop-Recovery receipt on an unchanged tree) is exactly what audit completion validation rejects.
func auditResumeLine(id string) string {
	return "Task " + id + " is host-authorized review rework: a host audit reopened it, and its Coop-Task " +
		"commit is already in history — this is NOT crash recovery. Read its log.md/state.md for the recorded " +
		"finding and independently verify it. If the finding is false, re-close with ZERO new commits: leave " +
		"history untouched and move the task folder to 99_done/ — the host audit authority accepts a " +
		"verification-only completion. If the finding is real, do the rework by amending or rewriting the " +
		"already-bound implementation commit so its tree actually changes, keeping exactly one reachable " +
		"Coop-Task binding and semantically unchanged later commits, including commits with no task binding. " +
		"Do NOT add a Coop-Recovery trailer, " +
		"a message-only or receipt-only commit, or a recovery-only replay of unchanged descendants — a history " +
		"rewrite without a real tree change will be rejected."
}

// resumePrefixFor builds the informed-resume preamble for the assigned task. A lease carrying the
// host's audit-reopen authority selects the audit-rework preamble regardless of commit presence;
// otherwise the Coop-Task trailer already in history selects the crash/reopen disambiguation line.
// Empty when neither applies, so a fresh claim keeps the ordinary prompt.
func (a *app) resumePrefixFor(repo, id string, reopen *auditReopenRecord) string {
	if reopen != nil {
		return auditResumeLine(id)
	}
	return resumeLine(id, commitsForTask(repo, "", id))
}

func validateLeasedAuditReopen(repo, head, id string, reopen *auditReopenRecord) error {
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

func staleAuditReopenRecovery(id, baseline string) string {
	return fmt.Sprintf(
		"task parked in blocked: preserve any wanted work separately, restore the exact audited "+
			"pre-attempt baseline %s from reflog or backup, verify `git rev-parse HEAD` prints %s, "+
			"then run `coop tasks unblock %s \"restored audited pre-attempt HEAD\"`; blocking and "+
			"unblocking alone cannot repair Git history",
		baseline, baseline, id,
	)
}

// parkStaleAuditReopen removes a stale host authority from the unattended work queue without
// discarding the rejected Git rewrite. The operator must restore the exact audited baseline before
// explicit unblock can reactivate that generation. Metadata and the folder move roll back together
// so a failed decision/state write never leaves a malformed blocked task.
func parkStaleAuditReopen(task queuedTask, baseline string) error {
	id := task.Item.ID
	if task.Item.State != stateInProgress {
		return fmt.Errorf("park stale audit task %s from %s: want in progress", id, stateLabel(task.Item.State))
	}
	if !validAuditReopenHead(baseline) {
		return fmt.Errorf("park stale audit task %s without a valid recorded baseline", id)
	}
	metadata, err := snapshotTaskMetadata(task.Item.Dir, "decision.md", "log.md", "state.md")
	if err != nil {
		return fmt.Errorf("snapshot stale audit task %s: %w", id, err)
	}
	if err := moveTaskDir(task.Root, task.Item, stateBlocked); err != nil {
		return fmt.Errorf("move stale audit task %s to blocked: %w", id, err)
	}
	blockedDir := filepath.Join(task.Root, stateBlocked, id)
	rollback := func(cause error) error {
		metadataErr := restoreTaskMetadata(blockedDir, metadata)
		current := task.Item
		current.State = stateBlocked
		current.Dir = blockedDir
		moveErr := moveTaskDir(task.Root, current, stateInProgress)
		return errors.Join(cause, metadataErr, moveErr)
	}
	if err := writeStaleAuditReopenDecision(blockedDir, id, task.Item.Title, baseline, metadata["decision.md"]); err != nil {
		return rollback(fmt.Errorf("write stale audit decision for task %s: %w", id, err))
	}
	if err := appendTaskLogStrict(blockedDir, fmt.Sprintf(
		"host preflight parked this task because current HEAD no longer matches its audit-reopen authority; "+
			"restore exact baseline %s and verify `git rev-parse HEAD` before explicit unblock — "+
			"blocking and unblocking alone cannot repair Git history",
		baseline,
	)); err != nil {
		return rollback(fmt.Errorf("record stale audit park for task %s: %w", id, err))
	}
	if err := normalizeTaskState(
		id,
		blockedDir,
		"blocked — stale audit authority",
		fmt.Sprintf("preserve wanted work separately, restore exact baseline %s, verify `git rev-parse HEAD`, then explicitly unblock this task", baseline),
		"the host stopped before provider launch because current HEAD no longer matches its audit authority",
		"blocking and unblocking alone cannot repair Git history; never add a Coop-Recovery receipt",
	); err != nil {
		return rollback(fmt.Errorf("refresh stale audit task %s: %w", id, err))
	}
	return nil
}

func writeStaleAuditReopenDecision(taskDir, id, title, baseline string, previous taskMetadataSnapshot) error {
	body := fmt.Sprintf(
		"# Decision: restore the host-audited Git baseline for %q\n\n"+
			"**Blocks:** this task (`%s`).\n\n"+
			"**The decision:** Current HEAD no longer matches the host authority captured before "+
			"this audit-rework attempt. Coop cannot safely infer which rejected history to discard.\n\n"+
			"**Options:**\n"+
			"- **A — restore the audited baseline:** Preserve any wanted work separately, then restore "+
			"the exact pre-attempt baseline `%s` from reflog or backup; verify "+
			"`git rev-parse HEAD` prints `%s`.\n"+
			"- **B — stop for manual authority repair:** Use this when the exact baseline is unavailable; "+
			"do not manufacture a recovery receipt or merely cycle the task state.\n\n"+
			"**Recommendation:** Choose A, verify the repository is back at the exact audited baseline, "+
			"then run `coop tasks unblock %s \"restored audited pre-attempt HEAD\"`. Blocking and "+
			"unblocking alone cannot repair Git history.\n\n"+
			"---\n\n"+
			"**Resolution:** <!-- HUMAN: record how the audited baseline was restored, then explicitly unblock -->\n",
		title, id, baseline, baseline, id,
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
	root, err := openTaskMetadataRoot(taskDir)
	if err != nil {
		return err
	}
	defer root.Close()
	return atomicWriteTaskFile(root, "decision.md", []byte(body))
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
// ordinary blocked task whose decision.md now carries a filled-in Resolution — the same bar
// `coop tasks unblock` applies (decisionResolved) — moves back to 00_todo/ with a log note.
// Audit-reopened tasks require an explicit host `coop tasks unblock`: provider-writable prose can
// never invoke the host-authority upgrade path.
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
			_, hasAuthority, err := readAuditReopenRecord(host, t.ID)
			if err != nil {
				ui.Warn("pre-flight: could not inspect host audit authority for %s: %v — task remains blocked; repair the host authority registry, then retry: coop tasks unblock %s", t.ID, err, t.ID)
				continue
			}
			if hasAuthority {
				ui.Warn("pre-flight: %s has host audit-reopen authority — unblock it explicitly: coop tasks unblock %s", t.ID, t.ID)
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
