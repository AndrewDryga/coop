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
// So the loop notes in the host-only registry when one attempt made the task's binding and then
// blocked it (RecordCommittedBlock), and a person's `coop tasks unblock <id> "<answer>"` turns that
// note into the review-reopen authority a verification-only re-close already uses
// (answeredBlockReopen), naming the commit it covers. Only an answer given to that command spends the
// note. The answer in decision.md proves no person, since a box can write it, so neither a plain
// unblock nor the loop's pre-flight (UnblockResolved) does.

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
// binding before it blocked the task. blocked is the commit HEAD named when the worker blocked it
// with its task tools (taskmcp's AssignedBlockHead), or "" when it did not. A task under review
// authority keeps that instead (PreserveBlockedAuditReopen). A binding made before the attempt is
// not its work, so a crash recovery that blocks keeps the crash-recovery rule. Nor is one made after
// the block: the worker has stopped working the task by then, and another session in this checkout
// can commit with its trailer.
func (l *TaskLease) RecordCommittedBlock(repo, base, blocked, head string) error {
	if l.Reopen != nil || base == head || blocked == "" {
		return nil
	}
	current, ok, err := CurrentTask(l.root, l.id)
	if err != nil || !ok || current.State != StateBlocked {
		return err
	}
	subject, ok := attemptBinding(repo, base, blocked, head, l.id)
	if !ok {
		return nil
	}
	return writeCommittedBlock(l.root, committedBlockRecord{Version: committedBlockVersion, TaskID: l.id, Subject: subject})
}

// attemptBinding is the content of id's one binding when the commits from base to head made it before
// the task blocked at blocked: the raw walk from head finds exactly one, and Git reads that one in
// base..head and in base..blocked.
func attemptBinding(repo, base, blocked, head, id string) (AuditReopenCommit, bool) {
	bindings, ok := rawTaskBindings(repo, head)
	if !ok || len(bindings[id]) != 1 {
		return AuditReopenCommit{}, false
	}
	bound := bindings[id][0]
	for _, end := range []string{head, blocked} {
		made, err := TaskCommits(repo, base+".."+end, id)
		if err != nil || len(made) != 1 || !strings.HasPrefix(bound, made[0]) {
			return AuditReopenCommit{}, false
		}
	}
	return bindingContent(repo, bound)
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

// answerCommittedBlock prepares the authority an unblock with the person's answer grants from the
// task's committed-block note, and marks the note spent: the answer is given whether or not the
// task's commit still matches it. The caller holds the task's authority lock.
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

// answeredCommit names the commit this unblock lets the next worker close the task over without a new
// commit, as `git log --oneline` does, or "" when it grants no such authority. A box writes commit
// messages, so the subject is escaped and cut like a trailer value.
func (u *blockedAuditUnblock) answeredCommit() string {
	if u == nil || u.previous != nil || u.replacement == nil || !u.replacement.Answered {
		return ""
	}
	subject, _, _ := strings.Cut(strings.TrimLeft(u.replacement.Subject.CommitMessage, "\n"), "\n")
	return shortCommit(auditReviewedHead(*u.replacement)) + " " + bindingLabel(strings.TrimSpace(subject))
}
