package tasks

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/processidentity"
)

// A box session — an editor's `coop acp` box or an interactive `coop <agent>` box — has no task
// assigned to it: its agent picks its own work and claims it through the box's task tools
// (internal/taskmcp). These are the host-side moves those tools make. They hold the same task
// authority as the host CLI but take over less, since the box is the less trusted party: a session
// replaces only a claim that is its own or whose process provably stopped.

var errTaskChangedBeforeClaim = errors.New("task changed before it could be claimed")

// SessionClaimActor is the identity a session's claims bind to: the host process serving the
// session, so a claim ends with it — the queue then shows its owner process stopped, and the loop's
// pre-flight releases it. An unreadable identity is an error, never a claim bound to nothing,
// which would read as a person's claim that only a person releases.
func SessionClaimActor(label string, pid int) (ClaimActor, error) {
	actor := ClaimActor{Label: claimActorLabel(label), PID: pid}
	if pid > 1 {
		actor.StartToken = processidentity.StartToken(pid)
	}
	if !actor.Bound() {
		return ClaimActor{}, fmt.Errorf("cannot read the identity of process %d, which this session's task claims would bind to", pid)
	}
	return actor, nil
}

// Bound reports whether the actor names a process whose identity Coop can check later.
func (a ClaimActor) Bound() bool { return a.PID > 1 && processidentity.Stable(a.StartToken) }

// ClaimSessionTask claims a task for a box session: the in-box half of `coop tasks claim`. Like
// ReleaseTrustedTask it holds the lease-authority flock and the owner lock while it re-resolves the
// exact task, moves the folder and writes the claim, so a controller leasing the task is refused,
// never overtaken, and no loop can lease it in between (the loop re-reads the claim once it holds
// the lease). The folder moves first: a crash before the record lands leaves an unclaimed task in
// progress, which the loop resumes, never a todo task held by a claim nothing releases. A blocked
// task waits for a human's answer and completed work stays archived, so both are refused.
// replaced names a stopped owner whose claim this took over.
func ClaimSessionTask(root string, t Item, actor ClaimActor) (replaced string, err error) {
	switch t.State {
	case StateTodo, StateInProgress:
	case StateBlocked:
		return "", fmt.Errorf("%s is blocked on a decision only a human can answer; use tasks_unblock with their answer before claiming it", t.ID)
	default:
		return "", fmt.Errorf("%s is done; completed work stays in the archive", t.ID)
	}
	if !actor.Bound() {
		return "", errors.New("a session claim needs the identity of the process serving the session")
	}
	authority, err := lockLeaseAuthorityExclusive(root, t.ID, true)
	if err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return "", fmt.Errorf("task %s is %w", t.ID, ErrTaskLeased)
		}
		return "", err
	}
	ownerLock, err := lockTaskOwner(root, t.ID)
	if err != nil {
		return "", errors.Join(err, unlockLeaseFile(authority))
	}
	unlock := func(err error) error { return errors.Join(err, ownerLock.Close(), unlockLeaseFile(authority)) }
	current, ok, err := CurrentTask(root, t.ID)
	if err != nil {
		return "", unlock(err)
	}
	if !ok || current.State != t.State || current.Dir != t.Dir {
		return "", unlock(errTaskChangedBeforeClaim)
	}
	// A crash inside `coop tasks unblock` leaves authority no lease may start on (TryTaskLease): a
	// claim would hold a task nobody could then work, so it waits for the person to finish.
	if reopen, reopened, err := ReadAuditReopenRecord(root, t.ID); err != nil {
		return "", unlock(fmt.Errorf("read audit reopen authority for task %s: %w", t.ID, err))
	} else if reopened && reopen.UnblockPending {
		return "", unlock(fmt.Errorf("%s is halfway through a person's unblock; it cannot be claimed until they finish it", t.ID))
	}
	previous, owned, err := ownerLock.Read()
	if err != nil {
		return "", unlock(err)
	}
	if owned {
		if err := boxClaimRefusal(t.ID, previous, actor); err != nil {
			return "", unlock(err)
		}
		if !sameClaimant(previous, actor) {
			replaced = TaskOwnerLabel(previous)
		}
	}
	if current.State == StateTodo {
		if err := MoveTaskDir(root, current, StateInProgress); err != nil {
			return "", unlock(err)
		}
		if current, ok, err = CurrentTask(root, t.ID); err != nil || !ok {
			return "", unlock(errors.Join(errTaskChangedBeforeClaim, err))
		}
	}
	// The record names the folder where it now sits; a rename keeps the folder's identity.
	claim, err := humanClaimRecord(root, current, actor)
	if err != nil {
		return "", unlock(err)
	}
	return replaced, unlock(ownerLock.Write(claim))
}

// ReleaseSessionTask hands a session's claim back: the task returns to todo with its notes, under
// the authority ReleaseTrustedTask holds. A session gives up only a claim that is its own or
// provably stopped — or none, for a task left in progress unclaimed.
func ReleaseSessionTask(root string, t Item, actor ClaimActor) error {
	return releaseUnderAuthority(root, t, func(record TaskOwnerRecord) error {
		return boxClaimRefusal(t.ID, record, actor)
	})
}

// CompleteBoxTask is CompleteTrustedTask for a box's task tools: the same move, receipt and
// normalization, refused when the task is someone else's (boxClaimRefusal). The claim is judged
// while the completion holds the lease authority, the lock a box session needs to claim, so a
// claim that lands after the tool's first check still wins. actor is the box session's, or the
// zero actor for a loop box, which owns no claim. A task several commits carry the trailer of
// closes only for the session that claimed it.
func CompleteBoxTask(project, root string, task Item, actor ClaimActor) error {
	_, err := completeTrustedTask(project, root, task, completionOptions{actor: actor, claimed: true, refuse: func(record TaskOwnerRecord) error {
		return boxClaimRefusal(task.ID, record, actor)
	}})
	return err
}

// BlockBoxTask is BlockTrustedTask for a box's task tools, refused like CompleteBoxTask under the
// same locks.
func BlockBoxTask(root string, t Item, actor ClaimActor) error {
	return blockTrustedTask(root, t, actor, func(record TaskOwnerRecord) error {
		return boxClaimRefusal(t.ID, record, actor)
	})
}

// releaseUnderAuthority returns a live task to 00_todo/. It takes the authority flock and holds it
// with the owner lock through re-resolving the exact task instance, the move, and clearing the
// claim, so a fork assignment cannot appear after a stale precheck and be stranded in todo. The
// claim is held ACROSS the move and dropped only afterwards: a crash in between leaves the task in
// todo still claimed, which an identical retry finishes. A fork-owned task and a task another
// controller leases are refused; refuse decides whether the caller may give up an existing claim.
func releaseUnderAuthority(root string, t Item, refuse func(TaskOwnerRecord) error) error {
	return withReturnToTodoLocks(root, t, func(current Item, record TaskOwnerRecord, owned bool) error {
		if owned && record.Kind == TaskOwnerFork {
			return fmt.Errorf("%w: cannot release %s while it is %s", ErrTaskSandboxOwned, t.ID, TaskOwnerLabel(record))
		}
		if owned {
			if err := refuse(record); err != nil {
				return err
			}
		}
		return moveToTodoAndClearClaim(root, current, owned)
	})
}

// UnblockSessionTask records the answer a person gave an interactive session.
// The host CLI also resolves fork and review authority, which a box cannot exercise.
func UnblockSessionTask(root string, t Item, actor ClaimActor, answer string) error {
	if !actor.Bound() {
		return errors.New("unblocking needs an interactive session")
	}
	if !ValidTaskText(answer, false, TaskLineLimit) {
		return fmt.Errorf("answer must be %s", TaskTextRequirement(false, TaskLineLimit))
	}
	answer = strings.TrimSpace(answer)
	if decisionResolution(withResolution("", answer)) != answer {
		return errors.New("give the user's answer as plain text on one line; a placeholder or pending decision cannot unblock a task")
	}
	return unblockOrdinaryTask(root, t, actor, answer)
}

// Preflight supplies no new answer and rechecks the saved resolution under the same locks.
func unblockOrdinaryTask(root string, t Item, actor ClaimActor, answer string) error {
	return withReturnToTodoLocks(root, t, func(current Item, owner TaskOwnerRecord, owned bool) error {
		if owned && owner.Kind == TaskOwnerFork {
			return fmt.Errorf("%s belongs to a fork — unblock it explicitly: coop tasks unblock %s", t.ID, t.ID)
		}
		if owned {
			if err := boxClaimRefusal(t.ID, owner, actor); err != nil {
				return err
			}
		}
		if _, audited, err := ReadAuditReopenRecord(root, t.ID); err != nil {
			return err
		} else if audited {
			return fmt.Errorf("%s has host audit-reopen authority — unblock it explicitly: coop tasks unblock %s \"<answer>\"", t.ID, t.ID)
		}
		if _, committed, err := readCommittedBlock(root, t.ID); err != nil {
			return err
		} else if committed {
			return fmt.Errorf("%s was committed before it blocked — give your answer on the host: coop tasks unblock %s \"<answer>\"", t.ID, t.ID)
		}
		decision := filepath.Join(current.Dir, "decision.md")
		if current.State != StateBlocked && !(current.State == StateTodo && answer != "") {
			return fmt.Errorf("%s is %s; only a blocked task takes an answer", t.ID, StateLabel(current.State))
		}
		saved, err := recordedAnswer(decision)
		if err != nil {
			return err
		}
		if current.State == StateTodo {
			if saved != answer {
				return fmt.Errorf("%s is already in Todo with a different or missing resolution", t.ID)
			}
			// A move may have succeeded before clearing the session's claim failed.
			return moveToTodoAndClearClaim(root, current, owned)
		}
		note := "preflight: resolution filled in — unblocked"
		if answer != "" {
			if saved != "" && saved != answer {
				return fmt.Errorf("%s already has a different resolution in decision.md; review it before changing the answer", t.ID)
			}
			if saved == "" {
				if err := recordResolution(decision, answer); err != nil {
					return err
				}
			}
			note = unblockedNote(answer)
		} else if saved == "" {
			return fmt.Errorf("%s still needs a resolution", t.ID)
		}
		if err := moveToTodoAndClearClaim(root, current, owned); err != nil {
			return err
		}
		appendTaskLog(filepath.Join(root, StateTodo, t.ID), note)
		return nil
	})
}

func withReturnToTodoLocks(root string, t Item, apply func(Item, TaskOwnerRecord, bool) error) error {
	authority, err := lockLeaseAuthorityExclusive(root, t.ID, true)
	if err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return fmt.Errorf("task %s is %w", t.ID, ErrTaskLeased)
		}
		return err
	}
	ownerLock, err := lockTaskOwner(root, t.ID)
	if err != nil {
		return errors.Join(err, unlockLeaseFile(authority))
	}
	unlock := func(err error) error { return errors.Join(err, ownerLock.Close(), unlockLeaseFile(authority)) }
	current, ok, err := CurrentTask(root, t.ID)
	if err != nil {
		return unlock(err)
	}
	if !ok || current.State != t.State || current.Dir != t.Dir {
		return unlock(errTaskChangedBeforeRelease)
	}
	record, owned, err := ownerLock.Read()
	if err != nil {
		return unlock(err)
	}
	return unlock(apply(current, record, owned))
}

// Keep the claim across the move: a failure clearing it can be finished with release.
func moveToTodoAndClearClaim(root string, current Item, owned bool) error {
	if current.State != StateTodo {
		if err := MoveTaskDir(root, current, StateTodo); err != nil {
			return err
		}
	}
	if owned {
		if err := removeTaskOwnerRecordFile(root, current.ID); err != nil {
			return fmt.Errorf("task %s is now in todo, but clearing your claim failed: %w", current.ID, err)
		}
	}
	return nil
}

// BoxWriteRefusal says why a box's task tools may not change a task (boxClaimRefusal), or nil.
// actor is the box session's, or the zero actor for a loop box, which owns no claim.
func BoxWriteRefusal(root, id string, actor ClaimActor) error {
	record, owned, err := ReadTaskOwnerRecord(root, id)
	if err != nil || !owned {
		return err
	}
	return boxClaimRefusal(id, record, actor)
}

// SessionClaimLabel says who holds a task, for a session's task listing: "yours" for the session's
// own claim, the owner's label for anyone else's, and "" when nobody holds it.
func SessionClaimLabel(root, id string, actor ClaimActor) (string, error) {
	record, owned, err := ReadTaskOwnerRecord(root, id)
	switch {
	case err != nil || !owned:
		return "", err
	case sameClaimant(record, actor):
		return "yours", nil
	}
	return TaskOwnerLabel(record), nil
}

// boxClaimRefusal is the one rule a box's task tools follow on a task someone may hold: a box
// changes, claims or releases only a task that is unclaimed, its own session's, or claimed by a
// process that provably stopped. A fork assignment, a person's claim (bound to no process) and a
// live claim stay with their owner, so a person taking a task back with `coop tasks claim --force`
// stops the session at once — unlike the host CLI, which takes a claim bound to nothing as the
// person's own to re-take. The session a forced claim displaced is told who holds the task now, in
// the words the CLI uses (takenOver).
func boxClaimRefusal(id string, record TaskOwnerRecord, actor ClaimActor) error {
	switch {
	case record.Kind == TaskOwnerFork:
		return fmt.Errorf("%w: %s is %s", ErrTaskSandboxOwned, id, TaskOwnerLabel(record))
	case sameClaimant(record, actor), record.ActorPID != 0 && !ownerProcessLive(record):
		return nil
	}
	if taken := takenOver(record, actor); taken != nil {
		return taken
	}
	return fmt.Errorf("%w: %s is %s", errTaskClaimedByOther, id, TaskOwnerLabel(record))
}

func sameClaimant(record TaskOwnerRecord, actor ClaimActor) bool {
	return record.ActorPID == actor.PID && processidentity.Same(record.ActorStart, actor.StartToken)
}

// humanClaimRecord is the durable claim `coop tasks claim` and a session claim write: a human-kind
// owner record for the exact task instance, bound to actor. The loop's adoption skips any task that
// carries one.
func humanClaimRecord(root string, item Item, actor ClaimActor) (TaskOwnerRecord, error) {
	instance, err := EnsureTaskInstance(root, item)
	if err != nil {
		return TaskOwnerRecord{}, err
	}
	user, host := taskOwnerIdentity()
	return TaskOwnerRecord{
		Version:     taskOwnershipRecordVersion,
		TaskID:      item.ID,
		Kind:        TaskOwnerHuman,
		Task:        &instance,
		Source:      taskOwnerSourceInteractiveClaim,
		User:        user,
		Host:        host,
		ClaimedAt:   time.Now(),
		Actor:       actor.Label,
		ActorPID:    actor.PID,
		ActorStart:  actor.StartToken,
		ActorTarget: knownTarget(actor.Target, ""),
	}, nil
}
