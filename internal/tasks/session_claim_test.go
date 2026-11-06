package tasks

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionClaimActorBindsOnlyToAReadableProcess(t *testing.T) {
	actor, err := SessionClaimActor("claude", os.Getpid())
	if err != nil || !actor.Bound() || actor.Label != "claude" || actor.PID != os.Getpid() {
		t.Fatalf("SessionClaimActor(self) = %+v, %v", actor, err)
	}
	if actor, err := SessionClaimActor("not a label", os.Getpid()); err != nil || actor.Label != "" {
		t.Fatalf("a label the queue cannot show must be dropped, got %+v, %v", actor, err)
	}
	for _, pid := range []int{0, 1, -5} {
		if actor, err := SessionClaimActor("claude", pid); err == nil {
			t.Fatalf("SessionClaimActor(%d) = %+v; a claim bound to no process must be refused", pid, actor)
		}
	}
}

// A session claim moves a todo task to in progress under a human-kind claim bound to the session's
// process, so the loop's adoption skips it; claiming it again is idempotent.
// A claim an agent session makes records the full target the session runs, and coop tasks watch
// names the claim by it, as it names a lease. A target that does not read as one is not recorded,
// and a person's claim at a terminal records none: the board names the person.
func TestASessionClaimNamesTheSessionsTarget(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	root := filepath.Join(repo, ".agent", "tasks")
	for id, target := range map[string]string{"by-session": "claude:opus/high@work", "garbled": "claude\x1b[2J; rm -rf"} {
		actor, err := SessionClaimActor("claude", os.Getpid())
		if err != nil {
			t.Fatal(err)
		}
		actor.Target = target
		if _, err := ClaimSessionTask(root, taskForLease(t, root, StateTodo, id), actor); err != nil {
			t.Fatal(err)
		}
	}
	taskForLease(t, root, StateTodo, "by-person")
	if code, err := tasksFolderMoveWith(root, []string{"by-person"}, StateInProgress, "claim", "claimed", claimOptions{}); code != 0 || err != nil {
		t.Fatalf("claim at a terminal = %d, %v", code, err)
	}
	for id, want := range map[string]string{"by-session": "claude:opus/high@work", "garbled": "", "by-person": ""} {
		if record, ok := taskOwned(t, root, id); !ok || record.ActorTarget != want {
			t.Errorf("%s: claim target = %q, want %q", id, record.ActorTarget, want)
		}
	}
	user, _ := taskOwnerIdentity()
	out := captureStdout(t, func() { _, _ = TasksWatch(Host{}, repo, []string{root}) })
	for _, want := range []string{" by-session\n    claude:opus/high@work · working", " garbled\n    claude · working", " by-person\n    " + user + " · working"} {
		if !strings.Contains(out, want) {
			t.Errorf("watch is missing %q:\n%s", want, out)
		}
	}
}

func TestSessionClaimMovesATodoTaskAndBindsItToTheSession(t *testing.T) {
	root := t.TempDir()
	item := taskForLease(t, root, StateTodo, "work")
	me := selfActor(t, "claude")
	replaced, err := ClaimSessionTask(root, item, me)
	if err != nil || replaced != "" {
		t.Fatalf("ClaimSessionTask = %q, %v", replaced, err)
	}
	claimed, ok := mustCurrentTask(t, root, "work")
	if !ok || claimed.State != StateInProgress {
		t.Fatalf("claimed task = %+v, %t; want it in progress", claimed, ok)
	}
	rec, owned := taskOwned(t, root, "work")
	if !owned || rec.Kind != TaskOwnerHuman || rec.Actor != "claude" || rec.ActorPID != os.Getpid() || rec.ActorStart != me.StartToken {
		t.Fatalf("owner record = %+v, %t", rec, owned)
	}
	if skip, err := skipOwnedCandidate(root, "work", map[string]bool{"work": true}); err != nil || !skip {
		t.Fatalf("the loop must skip a session-claimed task: skip=%t, %v", skip, err)
	}
	if replaced, err := ClaimSessionTask(root, claimed, me); err != nil || replaced != "" {
		t.Fatalf("re-claiming the session's own task = %q, %v", replaced, err)
	}
}

func TestSessionClaimRefusesWhatTheSessionDoesNotOwn(t *testing.T) {
	root := t.TempDir()
	me := selfActor(t, "claude")
	other, stop := sleeperActor(t, "codex")
	for id, actor := range map[string]ClaimActor{"theirs": other, "person": {}} {
		taskForLease(t, root, StateTodo, id)
		if code, err := tasksFolderMoveWith(root, []string{id}, StateInProgress, "claim", "claimed", claimOptions{actor: actor}); code != 0 || err != nil {
			t.Fatalf("fixture claim %s = %d, %v", id, code, err)
		}
	}
	leased := taskForLease(t, root, StateInProgress, "leased")
	lease, _, err := TryTaskLease(root, leased, testLeaseOwner())
	if err != nil || lease == nil {
		t.Fatalf("fixture lease: %v", err)
	}
	defer lease.Release()
	blocked := taskForLease(t, root, StateBlocked, "blocked")
	done := taskForLease(t, root, StateDone, "done")

	for id, want := range map[string]error{"theirs": errTaskClaimedByOther, "person": errTaskClaimedByOther, "leased": ErrTaskLeased} {
		item, _ := mustCurrentTask(t, root, id)
		before, _, _ := ReadTaskOwnerRecord(root, id)
		if _, err := ClaimSessionTask(root, item, me); !errors.Is(err, want) {
			t.Fatalf("claim %s = %v, want %v", id, err, want)
		}
		if after, _, _ := ReadTaskOwnerRecord(root, id); after.ActorPID != before.ActorPID || after.Actor != before.Actor {
			t.Fatalf("a refused claim on %s changed its owner: %+v → %+v", id, before, after)
		}
	}
	for _, item := range []Item{blocked, done} {
		if _, err := ClaimSessionTask(root, item, me); err == nil {
			t.Fatalf("claiming a %s task must be refused", item.State)
		}
		if current, _ := mustCurrentTask(t, root, item.ID); current.State != item.State {
			t.Fatalf("a refused claim moved %s to %s", item.ID, current.State)
		}
		if _, owned := taskOwned(t, root, item.ID); owned {
			t.Fatalf("a refused claim recorded an owner for %s", item.ID)
		}
	}
	if err := boxClaimRefusal("forked", TaskOwnerRecord{Kind: TaskOwnerFork, Fork: &ForkTaskOwner{Phase: ForkAssignmentWorking}}, me); !errors.Is(err, ErrTaskSandboxOwned) {
		t.Fatalf("a fork assignment must stay with its fork, got %v", err)
	}

	// Once the other claimant's process is gone, its claim is the session's to take over.
	stop()
	item, _ := mustCurrentTask(t, root, "theirs")
	replaced, err := ClaimSessionTask(root, item, me)
	if err != nil || !strings.Contains(replaced, "codex") || !strings.Contains(replaced, "stopped") {
		t.Fatalf("takeover of a stopped claim = %q, %v", replaced, err)
	}
	if rec, _ := taskOwned(t, root, "theirs"); rec.ActorPID != os.Getpid() {
		t.Fatalf("owner after the takeover = %+v", rec)
	}
}

// A failed move must not leave a claim on a task that never reached in progress.
func TestSessionClaimLeavesNoClaimWhenTheMoveFails(t *testing.T) {
	root := t.TempDir()
	item := taskForLease(t, root, StateTodo, "stuck")
	inProgress := filepath.Join(root, StateInProgress)
	if err := os.MkdirAll(inProgress, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(inProgress, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(inProgress, 0o755) })
	if _, err := ClaimSessionTask(root, item, selfActor(t, "claude")); err == nil {
		t.Fatal("a claim whose move failed reported success")
	}
	if current, _ := mustCurrentTask(t, root, "stuck"); current.State != StateTodo {
		t.Fatalf("task moved to %s", current.State)
	}
	if rec, owned := taskOwned(t, root, "stuck"); owned {
		t.Fatalf("the failed claim left an owner record: %+v", rec)
	}
}

// The loop filters claimed candidates before it leases one, then checks again once it holds the
// lease: a session claim that landed in between wins, and the loop lets the lease go.
func TestLoopLetsGoOfALeaseOnATaskClaimedWhileItChose(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"claimed", "free"} {
		taskForLease(t, root, StateInProgress, id)
	}
	if _, err := ClaimSessionTask(root, mustTask(t, root, "claimed"), selfActor(t, "claude")); err != nil {
		t.Fatal(err)
	}
	for id, wantClaimed := range map[string]bool{"claimed": true, "free": false} {
		candidate := QueuedTask{Root: root, Item: mustTask(t, root, id)}
		lease, _, err := TryTaskLease(root, candidate.Item, testLeaseOwner())
		if err != nil || lease == nil {
			t.Fatalf("lease %s: %v", id, err)
		}
		var busy TaskLeaseSummary
		claimed, err := releaseClaimedLease(candidate, lease, map[string]bool{id: true}, &busy)
		if err != nil || claimed != wantClaimed || (busy.Owned == 1) != wantClaimed {
			t.Fatalf("%s: claimed=%t owned=%d err=%v, want claimed=%t", id, claimed, busy.Owned, err, wantClaimed)
		}
		again, _, err := TryTaskLease(root, candidate.Item, testLeaseOwner())
		if err != nil || (again != nil) != wantClaimed {
			t.Fatalf("%s: after the check the lease is %v (err %v); a claimed task must be let go, a free one kept", id, again, err)
		}
		if again != nil {
			_ = again.Release()
		}
		if !wantClaimed {
			_ = lease.Release()
		}
	}
}

func mustTask(t *testing.T, root, id string) Item {
	t.Helper()
	item, ok := mustCurrentTask(t, root, id)
	if !ok {
		t.Fatalf("no task %s", id)
	}
	return item
}

func TestSessionReleaseGivesBackOnlyTheSessionsOwnClaim(t *testing.T) {
	root := t.TempDir()
	me := selfActor(t, "claude")
	other, _ := sleeperActor(t, "codex")
	for id, actor := range map[string]ClaimActor{"mine": me, "theirs": other, "person": {}} {
		taskForLease(t, root, StateTodo, id)
		if code, err := tasksFolderMoveWith(root, []string{id}, StateInProgress, "claim", "claimed", claimOptions{actor: actor}); code != 0 || err != nil {
			t.Fatalf("fixture claim %s = %d, %v", id, code, err)
		}
	}
	taskForLease(t, root, StateInProgress, "unclaimed")
	leased := taskForLease(t, root, StateInProgress, "leased")
	lease, _, err := TryTaskLease(root, leased, testLeaseOwner())
	if err != nil || lease == nil {
		t.Fatalf("fixture lease: %v", err)
	}
	defer lease.Release()
	if err := ReleaseSessionTask(root, leased, me); !errors.Is(err, ErrTaskLeased) {
		t.Fatalf("releasing a task a controller leases = %v, want a refusal", err)
	}

	for _, id := range []string{"mine", "unclaimed"} {
		item, _ := mustCurrentTask(t, root, id)
		if err := ReleaseSessionTask(root, item, me); err != nil {
			t.Fatalf("release %s: %v", id, err)
		}
		if current, _ := mustCurrentTask(t, root, id); current.State != StateTodo {
			t.Fatalf("released %s is %s, want todo", id, current.State)
		}
		if _, owned := taskOwned(t, root, id); owned {
			t.Fatalf("released %s kept a claim", id)
		}
	}
	for _, id := range []string{"theirs", "person"} {
		item, _ := mustCurrentTask(t, root, id)
		if err := ReleaseSessionTask(root, item, me); !errors.Is(err, errTaskClaimedByOther) {
			t.Fatalf("release of %s = %v, want a refusal", id, err)
		}
		if current, _ := mustCurrentTask(t, root, id); current.State != StateInProgress {
			t.Fatalf("a refused release moved %s to %s", id, current.State)
		}
		if _, owned := taskOwned(t, root, id); !owned {
			t.Fatalf("a refused release dropped the claim on %s", id)
		}
	}
}

// A box changes only a task that is unclaimed, its own session's, or claimed by a stopped process:
// another live claim and a person's claim keep a session off it, and a loop box — which owns no
// claim — is kept off every live claim.
func TestBoxWriteRefusalKeepsBoxesOffTasksSomeoneHolds(t *testing.T) {
	root := t.TempDir()
	me := selfActor(t, "claude")
	other, stop := sleeperActor(t, "codex")
	gone, stopGone := sleeperActor(t, "gemini")
	for id, actor := range map[string]ClaimActor{"theirs": other, "person": {}, "mine": me, "stopped": gone} {
		taskForLease(t, root, StateTodo, id)
		if code, err := tasksFolderMoveWith(root, []string{id}, StateInProgress, "claim", "claimed", claimOptions{actor: actor}); code != 0 || err != nil {
			t.Fatalf("fixture claim %s = %d, %v", id, code, err)
		}
	}
	taskForLease(t, root, StateTodo, "free")
	stopGone()
	for _, tc := range []struct {
		actor   ClaimActor
		refused map[string]bool
	}{
		{me, map[string]bool{"theirs": true, "person": true, "mine": false, "stopped": false, "free": false}},
		{ClaimActor{}, map[string]bool{"theirs": true, "person": true, "mine": true, "stopped": false, "free": false}},
	} {
		for id, refused := range tc.refused {
			if err := BoxWriteRefusal(root, id, tc.actor); (err != nil) != refused || (refused && !errors.Is(err, errTaskClaimedByOther)) {
				t.Fatalf("BoxWriteRefusal(%s, actor %+v) = %v, want refused=%t", id, tc.actor, err, refused)
			}
		}
	}
	for id, want := range map[string]string{"mine": "yours", "free": "", "theirs": "claimed by codex"} {
		if label, err := SessionClaimLabel(root, id, me); err != nil || !strings.HasPrefix(label, want) || (want == "" && label != "") {
			t.Fatalf("SessionClaimLabel(%s) = %q, %v, want %q", id, label, err, want)
		}
	}
	stop()
}

// Complete and block judge the claim again under the locks they take, so a claim that lands after a
// tool's first check still keeps the box off the task.
func TestBoxCompleteAndBlockRefuseATaskSomeoneElseHolds(t *testing.T) {
	root := t.TempDir()
	me := selfActor(t, "claude")
	other, _ := sleeperActor(t, "codex")
	for _, id := range []string{"theirs", "mine"} {
		taskWithCompletedChecklist(t, root, StateTodo, id)
	}
	if code, err := tasksFolderMoveWith(root, []string{"theirs"}, StateInProgress, "claim", "claimed", claimOptions{actor: other}); code != 0 || err != nil {
		t.Fatalf("fixture claim = %d, %v", code, err)
	}
	if _, err := ClaimSessionTask(root, mustTask(t, root, "mine"), me); err != nil {
		t.Fatal(err)
	}
	if err := CompleteBoxTask("", root, mustTask(t, root, "theirs"), me); !errors.Is(err, errTaskClaimedByOther) {
		t.Fatalf("completing another session's task = %v, want a refusal", err)
	}
	if err := BlockBoxTask(root, mustTask(t, root, "theirs"), me); !errors.Is(err, errTaskClaimedByOther) {
		t.Fatalf("blocking another session's task = %v, want a refusal", err)
	}
	if current := mustTask(t, root, "theirs"); current.State != StateInProgress {
		t.Fatalf("a refused complete or block moved the task to %s", current.State)
	}
	if rec, owned := taskOwned(t, root, "theirs"); !owned || rec.ActorPID != other.PID {
		t.Fatalf("a refused complete or block dropped the claim: %+v, %t", rec, owned)
	}
	if err := CompleteBoxTask("", root, mustTask(t, root, "mine"), me); err != nil {
		t.Fatalf("completing the session's own task: %v", err)
	}
	if current := mustTask(t, root, "mine"); current.State != StateDone {
		t.Fatalf("own task is %s after completion", current.State)
	}
}

// The loop checks a candidate's claim, leases it, then checks again: a session claim that lands in
// between wins, and the loop neither adopts the task nor keeps its lease.
func TestLoopLeavesATaskClaimedBetweenItsCheckAndItsLease(t *testing.T) {
	root := t.TempDir()
	taskForLease(t, root, StateInProgress, "contested")
	me := selfActor(t, "claude")
	claimed := false
	beforeCandidateLease = func(candidate QueuedTask) {
		if candidate.Item.ID == "contested" && !claimed {
			claimed = true
			if _, err := ClaimSessionTask(candidate.Root, candidate.Item, me); err != nil {
				t.Errorf("claim in the window: %v", err)
			}
		}
	}
	t.Cleanup(func() { beforeCandidateLease = nil })
	assignment, err := AssignLoopTaskOnly([]string{root}, testLeaseOwner(), "")
	if err != nil || assignment.Lease != nil || assignment.Outcome == assignmentReady {
		t.Fatalf("assignment = %+v, %v; the loop must leave a task claimed while it chose", assignment, err)
	}
	if !claimed {
		t.Fatal("the window between the claim check and the lease was never reached")
	}
	if lease, _, err := TryTaskLease(root, mustTask(t, root, "contested"), testLeaseOwner()); err != nil || lease == nil {
		t.Fatalf("the loop kept its lease on the claimed task: %v", err)
	} else {
		_ = lease.Release()
	}
}

// A claim nobody can read keeps the loop off its task without stopping the loop.
func TestLoopSkipsATaskWhoseClaimCannotBeRead(t *testing.T) {
	root := t.TempDir()
	taskForLease(t, root, StateInProgress, "damaged")
	if _, err := ClaimSessionTask(root, mustTask(t, root, "damaged"), selfActor(t, "claude")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, StateInProgress, "damaged", ".coop-task.json")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadTaskOwnerRecord(root, "damaged"); err == nil {
		t.Fatal("fixture: the claim record still reads after its task lost its identity")
	}
	assignment, err := AssignLoopTaskOnly([]string{root}, testLeaseOwner(), "")
	if err != nil || assignment.Outcome == assignmentReady {
		t.Fatalf("assignment = %+v, %v; want the damaged claim skipped, not adopted or fatal", assignment, err)
	}
}

// A claim waits for a person to finish an interrupted unblock: no lease could start on the task.
func TestSessionClaimWaitsForAPendingUnblock(t *testing.T) {
	repo, git := gitRepo(t)
	t.Setenv(TestLeaseAuthorityRootEnv, t.TempDir())
	git("commit", "-q", "--allow-empty", "-m", "base")
	git("commit", "-q", "--allow-empty", "-m", "pending implementation\n\nCoop-Task: pending")
	root := filepath.Join(repo, TasksRoot)
	task := taskWithCompletedChecklist(t, root, StateTodo, "pending")
	record, err := CaptureAuditReopen(repo, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	record.Version = auditReopenPendingVersion
	record.UnblockPending = true
	if err := WriteAuditReopenRecord(root, record); err != nil {
		t.Fatal(err)
	}
	if _, err := ClaimSessionTask(root, task, selfActor(t, "claude")); err == nil || !strings.Contains(err.Error(), "unblock") {
		t.Fatalf("claim on a pending unblock = %v, want a refusal", err)
	}
	if current := mustTask(t, root, "pending"); current.State != StateTodo {
		t.Fatalf("a refused claim moved the task to %s", current.State)
	}
	if _, owned := taskOwned(t, root, "pending"); owned {
		t.Fatal("a refused claim recorded an owner")
	}
}
