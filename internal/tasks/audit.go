// Package tasks is coop's task-authority engine: the folder-mode task/backlog queue (a task's
// lifecycle state IS the directory it sits in), the durable claim + kernel-flock lease + ref
// authorities that decide who may act on a task and its checkout right now, and the trusted
// completion audit that a host applies to a box's finished work before trusting it. Command-line
// wiring (argv parsing, the terminal) and the loop engine's own box-spawn/provider-rotation
// machinery stay in internal/cli, which is this package's only production caller.
//
// The full four-authority map — claim, lease, checkout (stays in internal/cli), ref — with their
// mechanisms, hold times, and the lock-ordering invariant (ref authority is acquired before lease
// authority, never the reverse) lives in .agent/kb/task-authority-model.md; read that first, this
// comment does not repeat it.
package tasks

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

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/ui"
)

// The Coop-Task trailer binds a commit to the task it completes. The agent writes it (loopWorkPrompt
// instructs it); the HOST controller reads it to verify attempts, resume informed after a crash, and
// reconcile the parent queue after a fork merge — the LLM still moves folders, the controller only
// supplies evidence and repairs drift. Before this, nothing linked a commit to a task
// (git log --grep <id> was 0 repo-wide), so "one task = one commit" was unobservable and a crash
// between commit and folder-move was ambiguous.
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

// CommitInfo is a commit's short sha and subject line — the same shape internal/cli's own
// commitInfo (loopchanges.go) mirrors rather than shares; see ParseLoopCommits' caller for the
// one-site conversion between the two.
type CommitInfo struct{ SHA, Subject string }

type TaskTrailerCommit struct {
	Info          CommitInfo
	fullSHA       string
	parents       string
	authorName    string
	authorEmail   string
	authorDate    string
	commitMessage string
	Values        []string
	Malformed     bool
}

// taskTrailerCommits uses one NUL-delimited Git stream, so a trailer value can never be confused
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
	cmd := exec.Command("git", gitArgs(repo, args)...)
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

func auditHistoryCommitsLimited(repo, rangeExpr string, limit int) ([]TaskTrailerCommit, bool) {
	args := []string{"log", "--reverse", fmt.Sprintf("--max-count=%d", limit)}
	return auditHistoryCommits(repo, args, []string{rangeExpr})
}

func auditHistoryCommits(repo string, args, revisions []string) ([]TaskTrailerCommit, bool) {
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
	commits := make([]TaskTrailerCommit, 0, (len(fields)-1)/8)
	for i := 0; i < len(fields)-1; i += 8 {
		record := TaskTrailerCommit{
			Info:          CommitInfo{SHA: fields[i], Subject: fields[i+2]},
			fullSHA:       fields[i+1],
			parents:       fields[i+3],
			authorName:    fields[i+4],
			authorEmail:   fields[i+5],
			authorDate:    fields[i+6],
			commitMessage: fields[i+7],
		}
		record.Values, record.Malformed = auditTaskTrailersFromMessage([]byte(record.commitMessage))
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
func CommitsForTask(repo, rangeExpr, id string) []string {
	var shas []string
	commits, err := TaskTrailerCommits(repo, rangeExpr, false)
	if err != nil {
		return nil
	}
	for _, commit := range commits {
		if !commit.Malformed && len(commit.Values) == 1 && commit.Values[0] == id {
			shas = append(shas, commit.Info.SHA)
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
func ProtectedGateFiles(files []string) []string {
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
func ProtectedGateChanges(repo, base, head string) []string {
	if base == "" || head == "" || base == head {
		return nil
	}
	return ProtectedGateFiles(strings.Split(gitOut(repo, "diff", "--no-renames", "--name-only", "-z", base+".."+head), "\x00"))
}

// queueSnapshot maps task id → state across the hosts for UI and audit bookkeeping.
func QueueSnapshot(hosts []string) map[string]string {
	m := map[string]string{}
	for _, h := range hosts {
		for _, t := range ReadTaskTree(h) {
			m[t.ID] = t.State
		}
	}
	return m
}

func aggregateDuplicateTaskIDs(hosts []string) []string {
	return taskIDDuplicates(hosts, false)
}

func NonArchivedDuplicateTaskIDs(hosts []string) []string {
	return taskIDDuplicates(hosts, true)
}

func taskIDDuplicates(hosts []string, requireLive bool) []string {
	counts := map[string]int{}
	live := map[string]bool{}
	for _, host := range hosts {
		for _, task := range ReadTaskTree(host) {
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
	return duplicates
}

// completeTrustedTask is the host equivalent of the provider completion boundary. It holds the
// task authority lock across move, finalization, and receipt creation so a concurrent loop can
// observe either the old state or an accepted done inode, never a transient.
func CompleteTrustedTask(root string, task Item) (retErr error) {
	windows, err := BeginCompletionWindows([]string{root})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCompletionWindowSetup, err)
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
			return fmt.Errorf("task %s is leased by another controller", task.ID)
		}
		return err
	}
	defer func() { retErr = errors.Join(retErr, unlockLeaseFile(authority)) }()
	current, ok := CurrentTask(root, task.ID)
	if !ok || current.Dir != task.Dir || current.State != task.State {
		return errLeaseCandidateGone
	}
	reopen, reopened, err := ReadAuditReopenRecord(root, task.ID)
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
			if current.State != StateBlocked {
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
	if current.State != StateDone {
		if err := MoveTaskDir(root, current, StateDone); err != nil {
			return err
		}
		current.State = StateDone
		current.Dir = filepath.Join(root, StateDone, current.ID)
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
	// done ends a human claim same as block/unblock/release — a task in 99_done/ is never a loop
	// candidate again, so a leftover record would only ever be silent dead weight, but clearing it
	// here (not just in tasksFolderMove) covers every caller: the interactive verb, fork-merge
	// reconciliation, and a re-run of `done` on an already-done task all funnel through this one place.
	if err := removeTaskOwnerRecord(root, task.ID); err != nil {
		return err
	}
	acceptedTask = QueuedTask{Root: root, Item: current}
	accepted = true
	return nil
}

// moveTrustedTaskFromDone invalidates completion evidence under the same task authority lock before
// a host command reopens or blocks an archived task. On a failed move it restores the old receipt,
// so concurrent supervised windows never see a false unowned completion.
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
}

// moveTrustedTaskFromDoneWith is the one-task form of the atomic host move used for review
// verdicts. The callback may update only log.md and state.md.
func moveTrustedTaskFromDoneWith(root string, task Item, newState string, afterMove func(string) error) error {
	return MoveTrustedTasksFromDoneWith([]TrustedTaskMove{{
		Root: root, Task: task, NewState: newState, AfterMove: afterMove,
	}})
}

// moveTrustedTasksFromDoneWith holds every task authority lock before the first mutation. Review
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
		current, ok := CurrentTask(move.Root, move.Task.ID)
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
		var metadataErr error
		if state.move.AfterMove != nil {
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
// finalized the folder; provider-writable task metadata is never ownership evidence.
func rejectUnownedCompletions(tasks []QueuedTask, assigned QueuedTask) ([]string, error) {
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
		errs = append(errs, restoreUnownedCompletion(QueuedTask{Root: task.Root, Item: current}), lock.clearCompleted(), lock.release())
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
		recoveryErr := NormalizeTaskState(
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
func ReconcileInterruptedCompletions(hosts []string) error {
	var restoreErrs []error
	for _, host := range hosts {
		for _, task := range ReadTaskTree(host) {
			if task.State != StateDone {
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
					removeLeaseMetadata(host, current.ID),
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

func lockCrashCompletion(root string, task Item) (crashCompletionLock, Item, bool, error) {
	return lockCompletionForAudit(root, task, false)
}

// lockInterruptedCompletion preserves compatibility with a pre-authority controller whose held
// task-local lock is still authoritative. Completion-window audits use lockCrashCompletion instead:
// their journal must not be retired merely because a non-authoritative local reader was transient.
func lockInterruptedCompletion(root string, task Item) (crashCompletionLock, Item, bool, error) {
	return lockCompletionForAudit(root, task, true)
}

func lockCompletionForAudit(root string, task Item, allowLegacyLocalOwner bool) (crashCompletionLock, Item, bool, error) {
	authority, err := lockLeaseAuthorityForAudit(root, task.ID, true, "task "+task.ID+" authority", func() bool {
		return leaseAuthorityMetadataExists(root, task.ID)
	})
	if errors.Is(err, errCompletionAuditLockOwned) {
		return crashCompletionLock{}, Item{}, false, nil
	}
	if err != nil {
		return crashCompletionLock{}, Item{}, false, err
	}
	locks := crashCompletionLock{authority: authority, files: []*os.File{authority}}
	local, err := openLeaseLock(task.Dir, false)
	if err == nil {
		lockErr := syscall.Flock(int(local.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if allowLegacyLocalOwner && (errors.Is(lockErr, syscall.EWOULDBLOCK) || errors.Is(lockErr, syscall.EAGAIN)) {
			_ = local.Close()
			_ = locks.release()
			return crashCompletionLock{}, Item{}, false, nil
		}
		if lockErr != nil {
			lockErr = lockExclusiveForCompletionAudit(local, "task "+task.ID+" local lease", nil)
		}
		if lockErr != nil {
			_ = local.Close()
			_ = locks.release()
			return crashCompletionLock{}, Item{}, false, lockErr
		}
		locks.files = append(locks.files, local)
	} else if !errors.Is(err, os.ErrNotExist) {
		_ = locks.release()
		return crashCompletionLock{}, Item{}, false, err
	}
	current, ok := CurrentTask(root, task.ID)
	if !ok || current.State != StateDone || current.Dir != task.Dir {
		return crashCompletionLock{}, Item{}, false, locks.release()
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

func crashCompletionCandidate(root string, task Item) bool {
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
func BlockedTaskIDs(hosts []string) []string {
	var ids []string
	for id, st := range QueueSnapshot(hosts) {
		if st == StateBlocked {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// alreadyCommittedInProgress reports the in_progress tasks whose implementation commit is ALREADY
// reachable from HEAD, with that commit and how many commits sit on top of it. That state means the
// run died between the commit and the folder move — a hard Ctrl-C, a crash, or a background-timeout
// handoff un-completing finished work — and nothing else surfaces it: the loop just re-picks the
// task, and the resume recipe is only safe while the commit is still HEAD. Reporting it at startup
// turns a silent trap into something a human can confirm or close. Sorted; read-only.
func AlreadyCommittedInProgress(repo string, hosts []string) []struct {
	ID, Commit string
	Depth      int
} {
	var out []struct {
		ID, Commit string
		Depth      int
	}
	for id, st := range QueueSnapshot(hosts) {
		if st != StateInProgress {
			continue
		}
		commits := CommitsForTask(repo, "", id)
		if len(commits) == 0 {
			continue
		}
		depth := 0
		if n := gitOut(repo, "rev-list", "--count", commits[0]+"..HEAD"); n != "" {
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
	return out
}

type semanticHistoryCommit struct {
	sha      string
	semantic AuditReopenCommit
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
	return semanticHistoryCommitsFromChangeTrees(commits, changeTrees, len(commits), false)
}

func semanticHistoryCommitsFromRecords(
	repo string,
	commits []TaskTrailerCommit,
	limit int,
) ([]semanticHistoryCommit, error) {
	changeTrees, err := semanticHistoryChangeTrees(repo, commits)
	if err != nil {
		return nil, err
	}
	return semanticHistoryCommitsFromChangeTrees(commits, changeTrees, limit, true)
}

func semanticHistoryCommitsFromChangeTrees(
	commits []TaskTrailerCommit,
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
		if commit.Malformed || len(commit.Values) > 1 || (len(commit.Values) == 1 && commit.Values[0] == "") {
			return nil, errors.New("history contains an invalid task binding")
		}
		if !validAuditReopenHead(commit.fullSHA) {
			return nil, errors.New("history contains an invalid commit id")
		}
		if rejectTraversalMerges && len(strings.Fields(commit.parents)) > 1 {
			return nil, fmt.Errorf("audit history merge commit %s cannot be replayed safely", commit.fullSHA)
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
func semanticHistoryChangeTrees(repo string, commits []TaskTrailerCommit) ([]string, error) {
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

func semanticCommit(repo, sha, taskID string) (AuditReopenCommit, error) {
	semantic, _, err := semanticCommitAndParent(repo, sha, taskID)
	return semantic, err
}

func semanticCommitAndParent(repo, sha, taskID string) (AuditReopenCommit, string, error) {
	rawParent, err := auditCommitParent(repo, sha)
	if err != nil {
		return AuditReopenCommit{}, "", err
	}
	parents := strings.Fields(gitOut(repo, "rev-list", "--parents", "-n", "1", sha))
	if len(parents) == 0 {
		return AuditReopenCommit{}, "", fmt.Errorf("resolve audit history commit %s", sha)
	}
	if (rawParent == "" && len(parents) != 1) ||
		(rawParent != "" && (len(parents) != 2 || parents[1] != rawParent)) {
		return AuditReopenCommit{}, "", fmt.Errorf("audit history commit %s traversal parent differs from its raw object", sha)
	}
	diffCmd := exec.Command("git", gitArgs(repo,
		[]string{"diff-tree", "--root", "--no-commit-id", "--raw", "-z", "-r", "--no-renames", sha})...)
	diff, err := auditCommandOutput(diffCmd, auditDiffOutputLimit)
	if err != nil {
		return AuditReopenCommit{}, "", fmt.Errorf("read audit history commit %s changes: %w", sha, err)
	}
	sum := sha256.Sum256(diff)
	metaCmd := exec.Command("git", gitArgs(repo,
		[]string{"show", "-s", "--format=format:%an%x00%ae%x00%aI%x00%B", sha})...)
	meta, err := auditCommandOutput(metaCmd, auditMetadataOutputLimit)
	if err != nil {
		return AuditReopenCommit{}, "", fmt.Errorf("read audit history commit %s metadata: %w", sha, err)
	}
	fields := strings.SplitN(string(meta), "\x00", 4)
	if len(fields) != 4 {
		return AuditReopenCommit{}, "", fmt.Errorf("parse audit history commit %s metadata", sha)
	}
	return AuditReopenCommit{
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
// forkspace.GitHardening separately disables agent-writable replacement objects. Missing objects,
// malformed parents, and merges fail closed.
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

func auditReviewedSubject(repo, id string, record AuditReopenRecord) (rawAuditCommit, error) {
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

func CaptureAuditReopen(repo, id string) (AuditReopenRecord, error) {
	head := gitOut(repo, "rev-parse", "--verify", "HEAD^{commit}")
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
	generation, err := newAuditReopenGeneration()
	if err != nil {
		return AuditReopenRecord{}, err
	}
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

func AuditReopenCurrentValid(repo, head, id string, record AuditReopenRecord) bool {
	_, err := auditReopenCurrentHistory(repo, head, id, record)
	return err == nil
}

func auditReopenCompletionValid(repo, base, head, id string, record AuditReopenRecord) bool {
	if base == head {
		return AuditReopenCurrentValid(repo, head, id, record)
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
		return AuditReopenRecord{}, fmt.Errorf("audit rewrite for task %s changed its reviewed subject or descendants outside the host authority", id)
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
	baselineHead := gitOut(repo, "rev-parse", "--verify", head+"^{commit}")
	if !validAuditReopenHead(baselineHead) {
		return AuditReopenRecord{}, fmt.Errorf("resolve rebased audit HEAD for task %s", id)
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

func auditReopenLegacyBaselineMatches(repo, head, id string, record AuditReopenRecord) bool {
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
	var descendants []AuditReopenCommit
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
	record AuditReopenRecord,
	adoptionHead string,
) (AuditReopenRecord, error) {
	if !validAuditReopenHead(adoptionHead) || adoptionHead != head {
		return AuditReopenRecord{}, fmt.Errorf(
			"legacy audit adoption for task %s was authorized for %s, but current HEAD is %s; "+
				"preserve wanted work, restore %s exactly, verify `git rev-parse HEAD` prints %s, "+
				"then retry with the same --adopt-audit-head value",
			id, adoptionHead, head, adoptionHead, adoptionHead,
		)
	}
	if !auditReopenLegacyBaselineMatches(repo, head, id, record) {
		return AuditReopenRecord{}, fmt.Errorf(
			"current HEAD %s does not match task %s's legacy subject and task-bound descendant projection",
			head, id,
		)
	}
	bindings, ok := rawTaskBindings(repo, head)
	if !ok || len(bindings[id]) != 1 {
		return AuditReopenRecord{}, fmt.Errorf("read reachable task bindings before legacy adoption of %s", id)
	}
	complete, err := rawAuditHistoryFromSubject(repo, head, bindings[id][0])
	if err != nil || len(complete) == 0 {
		return AuditReopenRecord{}, fmt.Errorf("read complete legacy audit history for %s", id)
	}
	history := make([]AuditReopenCommit, len(complete)-1)
	for i := range history {
		history[i] = complete[i+1].semantic
		if history[i].TaskID != "" && len(bindings[history[i].TaskID]) != 1 {
			return AuditReopenRecord{}, fmt.Errorf(
				"descendant task %s needs exactly one reachable %s binding before legacy adoption of %s",
				history[i].TaskID, CoopTaskTrailer, id,
			)
		}
	}
	replacement := AuditReopenRecord{
		Version: auditReopenVersion, Generation: record.Generation, TaskID: id,
		BaselineHead: head, Subject: record.Subject, History: history,
	}
	if validateAuditReopenRecord(replacement, id) != nil ||
		!AuditReopenCurrentValid(repo, head, id, replacement) {
		return AuditReopenRecord{}, fmt.Errorf("capture complete legacy audit history for task %s", id)
	}
	return replacement, nil
}

// upgradeBlockedAuditReopen recovers a rewrite from the exact baseline recorded by the host.
// The complete replay must be the first sequence after the rewritten subject; unrelated work that
// landed later may remain as a suffix, but no reflog guess or task-only projection can authorize it.
func upgradeBlockedAuditReopen(repo, head, id string, record AuditReopenRecord) (AuditReopenRecord, error) {
	if validateAuditReopenRecord(record, id) != nil || !auditReopenRecordActive(record) {
		return AuditReopenRecord{}, fmt.Errorf("invalid audit reopen authority for task %s", id)
	}
	if !AuditReopenCurrentValid(repo, record.BaselineHead, id, record) {
		return AuditReopenRecord{}, fmt.Errorf(
			"blocked audit task %s recorded baseline %s is unavailable or no longer matches its host authority",
			id, record.BaselineHead,
		)
	}
	reviewedParent, err := auditReviewedSubjectParent(repo, id, record)
	if err != nil {
		return AuditReopenRecord{}, fmt.Errorf("resolve blocked audit subject parent for task %s: %w", id, err)
	}
	current, err := rawAuditRewriteHistory(
		repo,
		head,
		reviewedParent,
		auditReopenHistoryLimit+1,
	)
	if err != nil {
		return AuditReopenRecord{}, fmt.Errorf("read blocked audit rewrite for task %s: %w", id, err)
	}
	rewriteLen := len(record.History) + 1
	if len(current) < rewriteLen {
		return AuditReopenRecord{}, fmt.Errorf("blocked audit task %s has no complete replay after its recorded baseline", id)
	}
	rewriteHead := current[rewriteLen-1].sha
	if rewriteHead == "" {
		return AuditReopenRecord{}, fmt.Errorf("resolve blocked audit rewrite terminal for task %s", id)
	}
	replacement, err := rebasedAuditReopenRecord(repo, record.BaselineHead, rewriteHead, id, record)
	if err != nil || !AuditReopenCurrentValid(repo, head, id, replacement) {
		return AuditReopenRecord{}, fmt.Errorf("blocked audit task %s changed its subject or complete descendant history", id)
	}
	return replacement, nil
}

type blockedAuditUnblock struct {
	authority                      *os.File
	root                           string
	previous, pending, replacement *AuditReopenRecord
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
func prepareBlockedAuditReopenUnblock(root string, task Item) (*blockedAuditUnblock, error) {
	return prepareBlockedAuditReopenUnblockWithAdoption(root, task, "")
}

func prepareBlockedAuditReopenUnblockWithAdoption(root string, task Item, adoptionHead string) (*blockedAuditUnblock, error) {
	observed, ok, err := ReadAuditReopenRecord(root, task.ID)
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

func lockBlockedAuditReopenUnblock(root string, task Item, observed AuditReopenRecord) (*blockedAuditUnblock, error) {
	return lockBlockedAuditReopenUnblockWithAdoption(root, task, observed, "")
}

func lockBlockedAuditReopenUnblockWithAdoption(
	root string,
	task Item,
	observed AuditReopenRecord,
	adoptionHead string,
) (*blockedAuditUnblock, error) {
	authority, err := lockLeaseAuthority(root, task.ID, false, syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		return nil, fmt.Errorf("lock blocked audit reopen authority for task %s: %w", task.ID, err)
	}
	upgrade := &blockedAuditUnblock{authority: authority, root: root}
	fail := func(err error) (*blockedAuditUnblock, error) {
		return nil, upgrade.finish(err)
	}
	record, ok, err := ReadAuditReopenRecord(root, task.ID)
	if err != nil {
		return fail(fmt.Errorf("re-read blocked audit reopen authority for task %s: %w", task.ID, err))
	}
	if !ok || !AuditReopenRecordsEqual(record, observed) {
		return fail(fmt.Errorf("audit reopen authority changed while unblocking task %s", task.ID))
	}
	current, ok := CurrentTask(root, task.ID)
	if !ok || current.State != StateBlocked || current.Dir != task.Dir {
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
		if !AuditReopenCurrentValid(repo, head, task.ID, replacement) {
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
func finishPendingAuditUnblock(root string, task Item, observed AuditReopenRecord) (bool, error) {
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
	current, ok := CurrentTask(root, task.ID)
	if !ok || current.State != StateTodo || current.Dir != task.Dir {
		return fail(fmt.Errorf("task %s is no longer the todo task from its pending audit unblock", task.ID))
	}
	repo := gitOut(root, "rev-parse", "--show-toplevel")
	head := gitOut(root, "rev-parse", "HEAD")
	replacement := record
	replacement.Version = auditReopenVersion
	replacement.UnblockPending = false
	if repo == "" || head == "" || !AuditReopenCurrentValid(repo, head, task.ID, replacement) {
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
func (l *TaskLease) PreserveBlockedAuditReopen(repo, base, head string) error {
	if l.Reopen == nil || base == head {
		return nil
	}
	current, ok := CurrentTask(l.root, l.id)
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

// rebaseTimedOutAuditReopen keeps held audit authority truthful after a timed-out attempt.
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

// completionUnbindableTasks is unbindableTasks' entry point from the work loop. Under ordinary
// authority it defers straight to unbindableTasks, threading touched through unchanged. A lease
// carrying host audit-reopen authority for exactly the finished task instead validates by semantic
// replay (auditReopenCompletionValid): that authority already proves the descendant history is a
// faithful reproduction, which trailer counting would otherwise reject as the very foreign-binding
// shape reopened work necessarily has. touched is unused on that branch — replay validation subsumes
// the foreign-binding question for it — so it always returns a nil tolerated set.
func CompletionUnbindableTasks(repo, base, head string, finished []string, reopen *AuditReopenRecord, touched map[string]bool) (missing, tolerated []string) {
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
	resolved := gitOut(repo, "rev-parse", "--verify", ordinary[0]+"^{commit}")
	return resolved == raw[id][0]
}

// unbindableTasks returns finished ids without exactly one Coop-Task binding both in this
// iteration's range and across history reachable from the proposed HEAD. Reopened work therefore
// has to rewrite its existing bound commit instead of adding a second one. A no-HEAD-change
// completion always fails closed — a zero-commit close is valid ONLY under fresh host audit
// authority, so task prose claiming a reopen can never buy one; crash recovery restores it for a
// fresh range.
//
// A commit in range that binds a task OUTSIDE finished is foreign. It still fails the WHOLE
// completion closed — unchanged from before this split — when it names a task this iteration's
// authority consumption could touch: touched is host-side knowledge the box cannot influence (built
// by completionUnbindableTasks' caller in internal/loop from the finished set, the leased task id, the
// audit-reopen record's task, every id whose queue state this completion window observed change, and
// every id windows.baselineDoneIDs() reports — every task already archived before the box ever ran).
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
func unbindableTasks(repo, base, head string, finished []string, touched map[string]bool) (missing, tolerated []string) {
	if base == "" || head == "" || base == head {
		return slices.Clone(finished), nil
	}
	headCommits, headErr := rawReachableAuditCommits(repo, head)
	baseCommits, baseErr := rawReachableAuditCommits(repo, base)
	if headErr != nil || baseErr != nil {
		return slices.Clone(finished), nil
	}
	baseReachable := make(map[string]bool, len(baseCommits))
	for _, commit := range baseCommits {
		baseReachable[commit.sha] = true
	}
	headReachable := make(map[string]bool, len(headCommits))
	for _, commit := range headCommits {
		headReachable[commit.sha] = true
	}
	// rewritten[id] means id already had a valid single-trailer binding reachable from base whose
	// commit fell out of head's reachable set — only possible if something rebased or amended an
	// ancestor. A fast-forward (the ordinary case) never drops a base-reachable commit.
	rewritten := make(map[string]bool)
	for _, commit := range baseCommits {
		if commit.taskBindingInvalid || len(commit.taskValues) != 1 || headReachable[commit.sha] {
			continue
		}
		rewritten[commit.taskValues[0]] = true
	}
	allowed := make(map[string]bool, len(finished))
	for _, id := range finished {
		allowed[id] = true
	}
	rangeBindings := make(map[string]int, len(finished))
	reachableBindings := make(map[string]int, len(finished))
	reachableInvalid := make(map[string]bool, len(finished))
	toleratedSeen := map[string]bool{}
	for _, commit := range headCommits {
		inRange := !baseReachable[commit.sha]
		if inRange && (commit.taskBindingInvalid || len(commit.taskValues) > 1) {
			return slices.Clone(finished), nil
		}
		for _, id := range commit.taskValues {
			if !allowed[id] {
				if inRange {
					if touched[id] || rewritten[id] {
						return slices.Clone(finished), nil
					}
					if !toleratedSeen[id] {
						toleratedSeen[id] = true
						tolerated = append(tolerated, id)
					}
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
	for _, id := range finished {
		if rangeBindings[id] != 1 || reachableBindings[id] != 1 || reachableInvalid[id] {
			missing = append(missing, id)
		}
	}
	slices.Sort(tolerated)
	return missing, tolerated
}

// reportToleratedForeignBindings surfaces every foreign Coop-Task trailer unbindableTasks tolerated
// instead of rejecting (see its doc comment and .agent/kb/loop-range-rejects-outside-commits.md). It
// never fails the iteration: a task id that no longer resolves to a folder on any host is skipped,
// not treated as an error, since bookkeeping about a task this iteration never touched must never
// block a legitimate completion. The operator sees one line naming all of them; each named task also
// gets its own best-effort log.md entry, so a human who later resumes that task is pointed at the
// truth by resumeLine's existing "read log.md" instruction — commitsForTask's unconditional history
// scan will already surface the stray commit and trigger that instruction once the task is picked up
// — instead of mistaking it for that task's own completion. The mark is deliberately informational,
// not mechanical: unbindableTasks' own reachable-binding count already refuses to let that task's
// real next completion silently ride on a binding it did not itself just create.
func ReportToleratedForeignBindings(repo string, hosts []string, base, head, leasedID string, tolerated []string) {
	if len(tolerated) == 0 {
		return
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
			if t, ok := CurrentTask(host, id); ok {
				appendTaskLog(t.Dir, note)
				break
			}
		}
	}
}

func RestoreQueuedCompletion(task QueuedTask, audit bool) error {
	id := task.Item.ID
	if task.Item.State == StateDone {
		if err := MoveTaskDir(task.Root, task.Item, StateInProgress); err != nil {
			return fmt.Errorf("restore task %s: %w", id, err)
		}
	}
	dir := filepath.Join(task.Root, StateInProgress, id)
	note := fmt.Sprintf("completion rejected: expected exactly one commit with one matching %s trailer in the iteration's range and exactly one reachable binding overall; %s; rewrite or squash duplicate bindings down to one, then re-run `coop loop`", CoopTaskTrailer, taskBindingRecovery(id))
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

// restoreBackgroundHandoffCompletion rejects a completion produced before the provider observed
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

// restoreProviderTimeoutCompletion rejects a completion produced by an attempt the watchdog
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

// restoreRefAuthorityFailure restores a completion the box already folder-moved when the ref
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
		"repair the commit binding, then re-run `coop loop`",
		"completion was rejected as unbindable",
		"the task needs exactly one matching Coop-Task trailer",
	)
}

func normalizeAuditRejectedTaskState(id, taskDir string) error {
	return NormalizeTaskState(
		id,
		taskDir,
		"in progress — completion rejected",
		"independently verify the audit finding, then re-close with zero commits or a real tree change, and re-run `coop loop`",
		"completion was rejected by the host audit authority",
		"a message-only rewrite or a recovery-only descendant replay is rejected; never add a Coop-Recovery trailer",
	)
}

func UnbindableCompletionError(ids []string, restoreErr error) error {
	recoveries := make([]string, 0, len(ids))
	for _, id := range ids {
		recoveries = append(recoveries, fmt.Sprintf("%s: %s", id, taskBindingRecovery(id)))
	}
	msg := fmt.Sprintf("completion rejected for task(s) %s: the new commit range and reachable HEAD each need exactly one commit with one parseable `%s: <id>` trailer per task; task(s) restored to in_progress — %s; rewrite/squash duplicate bindings down to one, then re-run `coop loop`", strings.Join(ids, ", "), CoopTaskTrailer, strings.Join(recoveries, "; "))
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
// finding is false and the re-close needs zero commits, or it is real and the subject tree must
// actually change.
func auditBindingRecovery(id string) string {
	return fmt.Sprintf(
		"task %s is host-authorized review rework: independently verify the recorded finding; if it is false, "+
			"re-close with zero new commits; if it is real, amend or rewrite the already-bound implementation "+
			"commit so its tree actually changes, keeping exactly one reachable %s binding and semantically "+
			"unchanged later commits, including commits with no task binding; a Coop-Recovery trailer, a message-only commit, or a recovery-only "+
			"replay of unchanged descendants will be rejected again",
		id, CoopTaskTrailer,
	)
}

func AuditCompletionError(id string, restoreErr error) error {
	msg := fmt.Sprintf("completion rejected for audit-reopened task %s: the host audit authority accepts only a zero-commit verification-only re-close or a rewrite whose subject tree actually changes with semantically unchanged descendants; task restored to in_progress — %s; then re-run `coop loop`", id, auditBindingRecovery(id))
	if restoreErr != nil {
		return fmt.Errorf("%s; recovery bookkeeping also failed: %w", msg, restoreErr)
	}
	return errors.New(msg)
}

// refAuthorityFailureError reports why the ref-authority window refused to consume task authority
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
		"otherwise do the rework by amending or rewriting the already-bound implementation commit, leaving exactly one reachable Coop-Task " +
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
	head := gitOut(repo, "rev-parse", "--verify", "HEAD")
	return head != "" && head == gitOut(repo, "rev-parse", "--verify", commits[0]+"^{commit}")
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

// ResumePrefixFor builds the informed-resume preamble for the assigned task. A lease carrying the
// host's audit-reopen authority selects the audit-rework preamble regardless of commit presence;
// otherwise the Coop-Task trailer already in history selects the crash/reopen disambiguation line.
// With neither, an interrupted attempt may still have left UNCOMMITTED work behind, so that case
// gets its own line. Empty only for a genuinely untouched resume.
func ResumePrefixFor(repo, id, state string, reopen *AuditReopenRecord) string {
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
	out := gitOut(repo, "status", "--porcelain")
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
			"restore exact baseline %s and verify `git rev-parse HEAD` before explicit unblock — "+
			"blocking and unblocking alone cannot repair Git history",
		baseline,
	)); err != nil {
		return rollback(fmt.Errorf("record stale audit park for task %s: %w", id, err))
	}
	if err := NormalizeTaskState(
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

// assignLoopTask scans in stable queue/id order and atomically leases exactly one task before the
// box starts. An available in-progress task remains preferred, but a foreign-held one is skipped so
// another controller can take independent todo work. The flock is obtained while a todo folder is
// still in todo, then rides its atomic rename to in_progress by inode.
func assignLoopTask(hosts []string, owner TaskLeaseOwner) (TaskAssignment, error) {
	return AssignLoopTaskOnly(hosts, owner, "")
}

// skipOwnedCandidate reports whether id carries a durable human-claim record (taskOwnerRecord) — if
// so, the loop must not adopt it, lease or no lease: `coop tasks claim` holds no flock, so an
// unheld lease proves nothing about whether a human still owns the work. Checked BEFORE
// tryTaskLease, not after, for two reasons: it avoids taking (and then having to release) a flock on
// work the loop was never going to adopt anyway, and `coop tasks claim` writes this record BEFORE it
// moves the task's folder (see claimTaskOwnerRecord), so the record protects an in-flight claim from
// the instant it starts — before a concurrent scan could even see the folder move. noted dedupes the
// human-facing notice per assignLoopTaskOnly call, so a candidate seen again on an internal rescan
// (maxLeaseRescans) is skipped again silently rather than re-announced.
func skipOwnedCandidate(root, id string, noted map[string]bool) (bool, error) {
	rec, ok, err := ReadTaskOwnerRecord(root, id)
	if err != nil {
		return false, fmt.Errorf("read owner record for task %s: %w", id, err)
	}
	if !ok {
		return false, nil
	}
	if !noted[id] {
		noted[id] = true
		ui.Info("%s is claimed by %s@%s — the loop will not adopt it; release it first: coop tasks release %s",
			id, rec.User, rec.Host, id)
	}
	return true, nil
}

// assignLoopTaskOnly scopes assignment to the current task in a limited run. Counts still cover the
// whole queue for truthful banners, but another actionable task can never be claimed while the
// selected task is retrying or has been reopened by its between-task audit.
func AssignLoopTaskOnly(hosts []string, owner TaskLeaseOwner, onlyID string) (TaskAssignment, error) {
	noted := map[string]bool{} // spans every rescan attempt below, so an owned skip is reported once
	for attempt := 0; attempt < maxLeaseRescans; attempt++ {
		var counts TaskCounts
		var inProgress, todo []QueuedTask
		for _, root := range hosts {
			for _, item := range ReadTaskTree(root) {
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

		var busy TaskLeaseSummary
		changed := false
		for _, candidate := range inProgress {
			if owned, err := skipOwnedCandidate(candidate.Root, candidate.Item.ID, noted); err != nil {
				return TaskAssignment{}, err
			} else if owned {
				busy.Owned++
				continue
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
			return TaskAssignment{
				Counts: counts, Task: candidate, Lease: lease, Outcome: assignmentReady, Busy: busy,
			}, nil
		}
		if changed {
			continue
		}

		for _, candidate := range todo {
			if owned, err := skipOwnedCandidate(candidate.Root, candidate.Item.ID, noted); err != nil {
				return TaskAssignment{}, err
			} else if owned {
				busy.Owned++
				continue
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
		if onlyID != "" && len(inProgress)+len(todo) == 0 {
			return TaskAssignment{Counts: counts, Outcome: AssignmentDrained}, nil
		}
		if counts.Todo+counts.Doing == 0 {
			return TaskAssignment{Counts: counts, Outcome: AssignmentDrained}, nil
		}
		return TaskAssignment{Counts: counts, Outcome: AssignmentUnavailable, Busy: busy}, nil
	}
	return TaskAssignment{}, fmt.Errorf("task queue kept changing while leasing — retry the loop")
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
		case StateTodo, StateInProgress:
			acts = append(acts, reconcileAction{ID: id, Move: true})
		case StateBlocked:
			acts = append(acts, reconcileAction{ID: id, Move: false})
		}
	}
	slices.SortFunc(acts, func(a, b reconcileAction) int { return strings.Compare(a.ID, b.ID) })
	return acts
}

// landedTasks is the set of task ids whose Coop-Task trailer appears in the exact landed range. A
// failed history read is an ERROR, never an empty set: reconciling nothing is indistinguishable from
// "this fork landed no tasks", and that silence is what makes the loop redo landed work.
func landedTasks(repo, revRange string) (map[string]bool, error) {
	commits, err := TaskTrailerCommits(repo, revRange, false)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, commit := range commits {
		if !commit.Malformed && len(commit.Values) == 1 && commit.Values[0] != "" {
			set[commit.Values[0]] = true
		}
	}
	return set, nil
}

// unreconciledQueueRecovery is the recovery a land leaves behind when coop could not work out what
// it landed: the exact commands to list the ids and close them, so the human — not the next loop
// iteration — decides what happens to work that already sits in parent history.
func unreconciledQueueRecovery(repo, revRange string) string {
	return fmt.Sprintf("the parent queue was NOT reconciled, so `coop loop` may redo work this fork already landed; list what landed with `git -C %s log --format=%%b %s | grep %s`, then close each id with `coop tasks done <id>`", repo, revRange, CoopTaskTrailer)
}

// ReconcileQueueAfterMerge moves any parent-queue task whose Coop-Task trailer now sits in parent
// history (landed by the just-merged fork) from todo/ or in_progress/ to done/, with a reconcile
// note; a blocked task with a landed trailer is flagged for a human, never moved. Prevents the parent
// loop from redoing work a fork already landed.
//
// A per-task obstruction stays best-effort (warn and skip) — the merge already succeeded, so one
// stuck folder must not fail it. Failing to work out WHAT landed is different: an unreadable queue
// set or landed range reconciles nothing while looking exactly like a fork that landed no tasks, so
// it comes back as an error for the caller to surface. The merge is never rolled back for it — it
// already stuck; only the bookkeeping is missing.
func ReconcileQueueAfterMerge(cfg *config.Config, repo, forkName, revRange string) error {
	queues, err := TaskQueues(cfg, repo, nil)
	if err != nil {
		return fmt.Errorf("fork %s landed, but its parent task queues could not be resolved: %w — %s", forkName, err, unreconciledQueueRecovery(repo, revRange))
	}
	landed, err := landedTasks(repo, revRange)
	if err != nil {
		return fmt.Errorf("fork %s landed, but reading the task ids it landed in %s failed: %w — %s", forkName, revRange, err, unreconciledQueueRecovery(repo, revRange))
	}
	hosts := make([]string, len(queues))
	for i, queue := range queues {
		hosts[i] = filepath.Join(repo, queue)
	}
	for _, id := range aggregateDuplicateTaskIDs(hosts) {
		delete(landed, id)
		ui.Warn("reconcile: task id %s exists in multiple queues; skipped automatic fork reconciliation", id)
	}
	// completeTrustedTask's audit-reopen branch reads HEAD, validates it, and consumes authority
	// several operations later — the same validate-then-consume shape the work loop closes for its
	// own completion path. The ref-authority lock covers that window here too, so a concurrent
	// process (a loop, a signing rewrite, another land) can never move HEAD in the gap.
	release, lockErr := LockRefAuthority(cfg, repo)
	if lockErr != nil {
		return fmt.Errorf("fork %s landed, but reconciling the parent queue could not acquire ref authority: %w — %s", forkName, lockErr, unreconciledQueueRecovery(repo, revRange))
	}
	defer release()
	for _, q := range queues {
		host := filepath.Join(repo, q)
		states := map[string]string{}
		items := map[string]Item{}
		for _, t := range ReadTaskTree(host) {
			states[t.ID] = t.State
			items[t.ID] = t
		}
		for _, act := range reconcileMerged(states, landed) {
			if !act.Move {
				ui.Warn("task %s is blocked but its work landed via fork %s — a human should reconcile it", act.ID, forkName)
				continue
			}
			doneDir := filepath.Join(host, StateDone, act.ID)
			if err := CompleteTrustedTask(host, items[act.ID]); err != nil {
				ui.Warn("reconcile: %v — fix the obstruction, then retry: coop tasks done %s", err, act.ID)
				continue
			}
			appendTaskLog(doneDir, "reconciled: landed by fork "+forkName)
		}
	}
	return nil
}

// unblockResolved is the loop's built-in preflight, run host-side (no box, no model): every
// ordinary blocked task whose decision.md now carries a filled-in Resolution — the same bar
// `coop tasks unblock` applies (decisionResolved) — moves back to 00_todo/ with a log note.
// Audit-reopened tasks require an explicit host `coop tasks unblock`: provider-writable prose can
// never invoke the host-authority upgrade path.
// A task with no decision.md, or one whose format decisionResolved can't read, stays parked:
// never act on a file we can't parse confidently. Best-effort; a move failure warns and skips.
// Returns the unblocked ids in readTaskTree order.
func UnblockResolved(hosts []string) []string {
	var ids []string
	for _, host := range hosts {
		for _, t := range ReadTaskTree(host) {
			if t.State != StateBlocked || !decisionResolved(filepath.Join(t.Dir, "decision.md")) {
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
			if err := MoveTaskDir(host, t, StateTodo); err != nil {
				ui.Warn("pre-flight: could not unblock %s: %v", t.ID, err)
				continue
			}
			appendTaskLog(filepath.Join(host, StateTodo, t.ID), "preflight: resolution filled in — unblocked")
			ids = append(ids, t.ID)
		}
	}
	return ids
}

func AppendTaskLogStrict(taskDir, note string) error {
	root, err := OpenTaskMetadataRoot(taskDir)
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
	_ = AppendTaskLogStrict(taskDir, note)
}
