package tasks

import (
	"encoding/json"
	"fmt"
	"strings"
)

// A worker may commit its task and then block it for a person, because a required check cannot run
// in the box, say. The answer often asks only for that check, but by then the task's commit is in
// history, usually under later work, so the next worker can close it neither with a new binding nor
// with crash recovery's amend, which is safe only at HEAD.
//
// So the loop notes in the host-only registry when one attempt both made the task's binding and
// blocked it (RecordCommittedBlock), and a person's explicit `coop tasks unblock` turns that note into
// the review-reopen authority a verification-only re-close already uses (answeredBlockReopen). The
// answer in decision.md proves no person: a box can write it, so the loop's pre-flight never spends
// the note (UnblockResolved).

const committedBlockVersion = 1

// committedBlockRecord names the binding the blocking attempt made by its content, not its commit
// id: host signing rewrites the commit right after the attempt.
type committedBlockRecord struct {
	Version int               `json:"version"`
	TaskID  string            `json:"task_id"`
	Subject AuditReopenCommit `json:"subject"`
}

func committedBlockName(root, id string) (string, error) {
	key, err := LeaseAuthorityKey(root, id)
	if err != nil {
		return "", err
	}
	return key + ".committed-block.json", nil
}

func readCommittedBlock(root, id string) (committedBlockRecord, bool, error) {
	name, err := committedBlockName(root, id)
	if err != nil {
		return committedBlockRecord{}, false, err
	}
	var record committedBlockRecord
	ok, err := readLeaseRecord(name, &record)
	if err != nil || !ok {
		return committedBlockRecord{}, false, err
	}
	if record.Version != committedBlockVersion || record.TaskID != id || record.Subject.TaskID != id || record.Subject.ChangeTree == "" {
		return committedBlockRecord{}, false, fmt.Errorf("invalid committed-block record for task %s", id)
	}
	return record, true, nil
}

func writeCommittedBlock(root string, record committedBlockRecord) error {
	name, err := committedBlockName(root, record.TaskID)
	if err != nil {
		return err
	}
	registry, err := OpenLeaseAuthorityRoot()
	if err != nil {
		return err
	}
	defer registry.Close()
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return AtomicWriteTaskFile(registry, name, append(data, '\n'))
}

func removeCommittedBlock(root, id string) error {
	name, err := committedBlockName(root, id)
	if err != nil {
		return err
	}
	return removeLeaseAuthorityRecord(name)
}

// RecordCommittedBlock notes that this attempt, the commits from base to head, made its task's one
// binding and left the task blocked. A task under review authority keeps that instead
// (PreserveBlockedAuditReopen). A binding made before the attempt is not its work, so a crash
// recovery that blocks keeps the crash-recovery rule.
func (l *TaskLease) RecordCommittedBlock(repo, base, head string) error {
	if l.Reopen != nil || base == head {
		return nil
	}
	current, ok, err := CurrentTask(l.root, l.id)
	if err != nil || !ok || current.State != StateBlocked {
		return err
	}
	subject, ok := attemptBinding(repo, base, head, l.id)
	if !ok {
		return nil
	}
	return writeCommittedBlock(l.root, committedBlockRecord{Version: committedBlockVersion, TaskID: l.id, Subject: subject})
}

// attemptBinding is the content of id's one binding when the commits from base to head made it: the
// raw walk from head finds exactly one, and Git reads that one in base..head.
func attemptBinding(repo, base, head, id string) (AuditReopenCommit, bool) {
	bindings, ok := rawTaskBindings(repo, head)
	if !ok || len(bindings[id]) != 1 {
		return AuditReopenCommit{}, false
	}
	made, err := TaskCommits(repo, base+".."+head, id)
	if err != nil || len(made) != 1 || !strings.HasPrefix(bindings[id][0], made[0]) {
		return AuditReopenCommit{}, false
	}
	return bindingContent(repo, bindings[id][0])
}

func bindingContent(repo, sha string) (AuditReopenCommit, bool) {
	raw, err := rawAuditHistoryCount(repo, sha, 1)
	if err != nil {
		return AuditReopenCommit{}, false
	}
	commits, err := semanticHistoryCommitsExact(repo, raw)
	if err != nil || len(commits) != 1 {
		return AuditReopenCommit{}, false
	}
	return commits[0].semantic, true
}

// answeredBlockReopen is the authority a person's unblock grants a task its own attempt committed
// and then blocked: the review-reopen record of that commit, moved past the commits that landed
// after it, so the next worker may close the task with no new commit or with one unbound repair, and
// may rewrite nothing before HEAD. It grants nothing unless the task's one binding is still the
// commit that attempt made.
func answeredBlockReopen(repo, head string, block committedBlockRecord) (AuditReopenRecord, bool) {
	id := block.TaskID
	if repo == "" || head == "" {
		return AuditReopenRecord{}, false
	}
	bindings, ok := rawTaskBindings(repo, head)
	if !ok || len(bindings[id]) != 1 {
		return AuditReopenRecord{}, false
	}
	bound := bindings[id][0]
	subject, ok := bindingContent(repo, bound)
	if !ok || subject != block.Subject {
		return AuditReopenRecord{}, false
	}
	landed, err := rawAuditHistoryUntil(repo, head, auditReopenHistoryLimit+1, func(commit rawAuditCommit) bool {
		return commit.sha == bound
	})
	if err != nil {
		return AuditReopenRecord{}, false
	}
	reviewed := AuditReopenRecord{
		Version: auditReopenVersion, Generation: newAuditReopenGeneration(), TaskID: id,
		BaselineHead: bound, Subject: subject, History: []AuditReopenCommit{}, Answered: true,
	}
	answered, err := movedAuditReopen(repo, id, reviewed, landed[1:])
	if err != nil || !AuditReopenCurrentValid(repo, head, id, answered) {
		return AuditReopenRecord{}, false
	}
	return answered, true
}

// answerCommittedBlock prepares the authority this unblock grants from the task's committed-block
// note, and marks the note spent: the unblock answers it whether or not the task's commit still
// matches it. The caller holds the task's authority lock.
func (u *blockedAuditUnblock) answerCommittedBlock(project, id string) error {
	block, ok, err := readCommittedBlock(u.root, id)
	if err != nil || !ok {
		return err
	}
	u.committedBlock = id
	repo, head := auditHistory(project)
	answered, ok := answeredBlockReopen(repo, head, block)
	if !ok {
		return nil
	}
	pending := answered
	pending.Version, pending.UnblockPending = auditReopenPendingVersion, true
	u.pending, u.replacement = &pending, &answered
	return nil
}

// answeredNote is the task log's account of an unblock that lets the next worker close the task
// without a new commit, or "" when it did not.
func (u *blockedAuditUnblock) answeredNote() string {
	if u == nil || u.previous != nil || u.replacement == nil || !u.replacement.Answered {
		return ""
	}
	return fmt.Sprintf("Unblock lets the next worker close this task without a new commit: its commit %s is already in history",
		shortCommit(auditReviewedHead(*u.replacement)))
}
