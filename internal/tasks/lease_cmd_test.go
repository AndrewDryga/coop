package tasks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/wait"

	"time"

	"github.com/AndrewDryga/coop/internal/processidentity"
)

func TestParseLeaseArgs(t *testing.T) {
	req, err := parseLeaseArgs([]string{"--as", "codex@zed", "2026-01-01-x", "--pid", "42", "--", "make", "check"})
	if err != nil || req.id != "2026-01-01-x" || req.actor.Label != "codex@zed" || req.actor.PID != 42 || strings.Join(req.command, " ") != "make check" {
		t.Fatalf("parsed = %+v, %v", req, err)
	}
	for name, args := range map[string][]string{
		"no id":           {"--as", "codex"},
		"unknown flag":    {"x", "--bogus"},
		"bad label":       {"x", "--as", "not ok"},
		"bad pid":         {"x", "--pid", "zero"},
		"two ids":         {"x", "y"},
		"empty command":   {"x", "--"},
		"value-less --as": {"x", "--as"},
	} {
		if _, err := parseLeaseArgs(args); err == nil {
			t.Errorf("%s: %v parsed without error", name, args)
		}
	}
}

func inProgressTask(t *testing.T, root, id string) Item {
	t.Helper()
	writeTaskFile(t, filepath.Join(root, StateTodo, id, "task.md"), "# "+id+"\n")
	if code, err := tasksFolderMove(root, []string{id}, StateInProgress, "claim", "claimed"); code != 0 || err != nil {
		t.Fatalf("claim %s = %d, %v", id, code, err)
	}
	item, ok := mustCurrentTask(t, root, id)
	if !ok {
		t.Fatalf("task %s vanished", id)
	}
	return item
}

// waitLease polls the task's lease until it reaches state (busy/unleased) or the deadline passes.
func waitLease(t *testing.T, item Item, state taskLeaseState) TaskLeaseObservation {
	t.Helper()
	deadline := time.Now().Add(wait.Deadline) // a fixture guard, not the behavior under test
	for {
		observed := observeTaskLease(item, time.Now())
		if observed.State == state || time.Now().After(deadline) {
			return observed
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLeaseRequiresAnInProgressTask(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateTodo, "todo", "task.md"), "# Todo\n")
	code, err := holdTaskLease(context.Background(), root, leaseRequest{id: "todo"}, time.Millisecond)
	if code != 1 || err == nil || !strings.Contains(err.Error(), "coop tasks claim todo") {
		t.Fatalf("lease on a todo task = %d, %v; want a refusal pointing at claim", code, err)
	}
}

func TestLeaseRunsACommandUnderTheLockAndPropagatesItsExit(t *testing.T) {
	root := t.TempDir()
	item := inProgressTask(t, root, "cmd")
	done := make(chan struct{})
	var code int
	var err error
	go func() {
		defer close(done)
		code, err = holdTaskLease(context.Background(), root, leaseRequest{
			id: "cmd", actor: ClaimActor{Label: "codex"}, command: []string{"sh", "-c", "sleep 0.4; exit 3"},
		}, time.Millisecond)
	}()
	if observed := waitLease(t, item, leaseBusy); observed.State != leaseBusy || observed.Provider != "codex" {
		t.Fatalf("lease while the command runs = %+v, want busy codex", observed)
	}
	<-done
	if err != nil || code != 3 {
		t.Fatalf("lease with an exiting command = %d, %v; want the command's exit code", code, err)
	}
	if observed := waitLease(t, item, leaseUnleased); observed.State != leaseUnleased {
		t.Fatalf("lease after the command = %+v, want released", observed)
	}
}

func TestLeaseHoldReleasesWhenTheBoundProcessDies(t *testing.T) {
	root := t.TempDir()
	item := inProgressTask(t, root, "held")
	sleeper, stop := sleeperActor(t, "codex")
	done := make(chan struct{})
	var code int
	var err error
	go func() {
		defer close(done)
		code, err = holdTaskLease(context.Background(), root, leaseRequest{id: "held", actor: sleeper}, 20*time.Millisecond)
	}()
	if observed := waitLease(t, item, leaseBusy); observed.State != leaseBusy {
		t.Fatalf("held lease = %+v, want busy", observed)
	}
	stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the holder did not notice its bound process dying")
	}
	if err != nil || code != 0 {
		t.Fatalf("holder after its process died = %d, %v", code, err)
	}
	if observed := waitLease(t, item, leaseUnleased); observed.State != leaseUnleased {
		t.Fatalf("lease after the holder exited = %+v, want released", observed)
	}
}

func TestLeaseHoldStopsWhenCancelledOrWhenTheTaskMoves(t *testing.T) {
	root := t.TempDir()
	item := inProgressTask(t, root, "moving")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	var code int
	var err error
	go func() {
		defer close(done)
		code, err = holdTaskLease(ctx, root, leaseRequest{id: "moving", actor: ClaimActor{Label: "claude"}}, 20*time.Millisecond)
	}()
	if observed := waitLease(t, item, leaseBusy); observed.State != leaseBusy || observed.Provider != "claude" {
		t.Fatalf("held lease = %+v, want busy claude", observed)
	}
	cancel()
	<-done
	if err != nil || code != 0 {
		t.Fatalf("stopped holder = %d, %v", code, err)
	}

	item = inProgressTask(t, root, "moved")
	done = make(chan struct{})
	go func() {
		defer close(done)
		code, err = holdTaskLease(context.Background(), root, leaseRequest{id: "moved"}, 20*time.Millisecond)
	}()
	waitLease(t, item, leaseBusy)
	if err := MoveTaskDir(root, item, StateBlocked); err != nil {
		t.Fatalf("move the leased task: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the holder did not notice the task leaving in_progress")
	}
	if err != nil || code != 0 {
		t.Fatalf("holder after the task moved = %d, %v", code, err)
	}
}

func TestLeaseRefusesAHeldLease(t *testing.T) {
	root := t.TempDir()
	item := inProgressTask(t, root, "taken")
	lease, _, err := TryTaskLease(root, item, testLeaseOwner())
	if err != nil || lease == nil {
		t.Fatalf("first lease: %v", err)
	}
	defer lease.Release()
	code, err := holdTaskLease(context.Background(), root, leaseRequest{id: "taken"}, time.Millisecond)
	// testLeaseOwner's clock is frozen in the past, so its lease reads as stalled — still held.
	if code != 1 || err == nil || !strings.Contains(err.Error(), "already leased") || !strings.Contains(err.Error(), "codex") {
		t.Fatalf("second lease = %d, %v; want a refusal naming the holder", code, err)
	}
}

// The point of the lease: a loop in the same checkout treats an outside holder exactly like another
// iteration and moves on, then resumes the task once the holder is gone.
func TestLoopSkipsATaskLeasedOutside(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateInProgress, "outside", "task.md"), "# Outside\n")
	item, _ := mustCurrentTask(t, root, "outside")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = holdTaskLease(ctx, root, leaseRequest{id: "outside", actor: ClaimActor{Label: "codex"}}, 20*time.Millisecond)
	}()
	waitLease(t, item, leaseBusy)
	assignment, err := AssignLoopTaskOnly([]string{root}, testLeaseOwner(), "")
	if err != nil || assignment.Outcome != AssignmentUnavailable || assignment.Busy.Busy != 1 {
		t.Fatalf("loop assignment beside an outside lease = %+v, %v; want unavailable with one busy task", assignment, err)
	}
	cancel()
	<-done
	waitLease(t, item, leaseUnleased)
	assignment, err = AssignLoopTaskOnly([]string{root}, testLeaseOwner(), "")
	if err != nil || assignment.Outcome != assignmentReady || assignment.Task.Item.ID != "outside" {
		t.Fatalf("loop assignment after the holder stopped = %+v, %v; want the task resumed", assignment, err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestCmdTasksFolderLeaseDispatch(t *testing.T) {
	root := t.TempDir()
	inProgressTask(t, root, "dispatch")
	if code, err := CmdTasksFolder("", root, []string{"lease", "dispatch", "--as", "codex", "--", "true"}); code != 0 || err != nil {
		t.Fatalf("lease via the dispatcher = %d, %v", code, err)
	}
	if code, err := CmdTasksFolder("", root, []string{"lease", "dispatch", "--bogus"}); code != 2 || err == nil {
		t.Fatalf("unknown lease flag = %d, %v; want a usage error", code, err)
	}
	if code, err := CmdTasksFolder("", root, []string{"lease", "dispatch", "--", "sh", "-c", "exit 7"}); code != 7 || err != nil {
		t.Fatalf("lease command exit via the dispatcher = %d, %v; want 7", code, err)
	}
	if !slices.Contains(TasksVerbs, "lease") {
		t.Error("lease must be a recognized tasks verb")
	}
	if code, err := CmdTasksFolder("", root, []string{"lease", "dispatch", "--pid", "999999999"}); code != 1 || err == nil || !errors.Is(err, err) {
		t.Fatalf("lease bound to a missing process = %d, %v; want a refusal", code, err)
	}
	if _, err := os.Stat(filepath.Join(root, StateInProgress, "dispatch")); err != nil {
		t.Fatalf("leasing must never move the task: %v", err)
	}
}

// A holder the same agent started stands down when that agent finishes the task; any other lease
// — a loop iteration's, or a holder bound to someone else — still refuses the completion.
func TestDoneStopsTheCallersOwnLeaseHolderOnly(t *testing.T) {
	if id := os.Getenv("COOP_TEST_LEASE_HOLDER_TASK"); id != "" {
		pid, _ := strconv.Atoi(os.Getenv("COOP_TEST_LEASE_HOLDER_ACTOR_PID"))
		actor := ClaimActor{Label: "codex", PID: pid, StartToken: os.Getenv("COOP_TEST_LEASE_HOLDER_ACTOR_START")}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM) // as the CLI does
		defer stop()
		code, err := holdTaskLease(ctx, os.Getenv("COOP_TEST_LEASE_HOLDER_ROOT"), leaseRequest{id: id, actor: actor}, 20*time.Millisecond)
		fmt.Printf("HOLDER_EXIT code=%d err=%v\n", code, err)
		return
	}
	root := t.TempDir()
	me := selfActor(t, "codex")
	startHolder := func(id string, actor ClaimActor) *exec.Cmd {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestDoneStopsTheCallersOwnLeaseHolderOnly$", "-test.v")
		cmd.Env = append(os.Environ(),
			"COOP_LEASE_HELPER=1", TestLeaseAuthorityRootEnv+"="+os.Getenv(TestLeaseAuthorityRootEnv),
			"COOP_TEST_LEASE_HOLDER_ROOT="+root, "COOP_TEST_LEASE_HOLDER_TASK="+id,
			"COOP_TEST_LEASE_HOLDER_ACTOR_PID="+strconv.Itoa(actor.PID), "COOP_TEST_LEASE_HOLDER_ACTOR_START="+actor.StartToken)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		return cmd
	}

	mine := inProgressTask(t, root, "mine")
	holder := startHolder("mine", me)
	if observed := waitLease(t, mine, leaseBusy); observed.State != leaseBusy {
		t.Fatalf("own holder never took the lease: %+v", observed)
	}
	if code, err := tasksFolderMoveWith(root, []string{"mine"}, StateDone, "done", "done", claimOptions{actor: me}); code != 0 || err != nil {
		t.Fatalf("done beside my own holder = %d, %v; want the holder stopped and the task completed", code, err)
	}
	if _, err := os.Stat(filepath.Join(root, StateDone, "mine")); err != nil {
		t.Fatalf("task not completed: %v", err)
	}
	if err := holder.Wait(); err != nil {
		t.Fatalf("own holder exited with %v after being told to stand down", err)
	}

	other, _ := sleeperActor(t, "claude")
	theirs := inProgressTask(t, root, "theirs")
	startHolder("theirs", other)
	waitLease(t, theirs, leaseBusy)
	if code, err := tasksFolderMoveWith(root, []string{"theirs"}, StateDone, "done", "done", claimOptions{actor: me}); code == 0 || err == nil || !strings.Contains(err.Error(), "leased by another controller") {
		t.Fatalf("done beside someone else's holder = %d, %v; want a refusal", code, err)
	}
	if observed := observeTaskLease(theirs, time.Now()); observed.State == leaseUnleased {
		t.Fatal("someone else's holder must not be stopped")
	}

	loop := inProgressTask(t, root, "loop")
	lease, _, err := TryTaskLease(root, loop, TaskLeaseOwner{RunID: "run-1", PID: os.Getpid(), Provider: "codex", Target: "codex", ActorPID: me.PID, ActorStart: me.StartToken})
	if err != nil || lease == nil {
		t.Fatalf("loop lease: %v", err)
	}
	defer lease.Release()
	if code, err := tasksFolderMoveWith(root, []string{"loop"}, StateDone, "done", "done", claimOptions{actor: me}); code == 0 || err == nil || !strings.Contains(err.Error(), "leased by another controller") {
		t.Fatalf("done beside a loop iteration's lease = %d, %v; want a refusal (and this process alive)", code, err)
	}
}

func TestListShowsAHeldLeaseBesideTheClaim(t *testing.T) {
	root := t.TempDir()
	item := inProgressTask(t, root, "both")
	if code, err := tasksFolderMoveWith(root, []string{"both"}, StateInProgress, "claim", "claimed", claimOptions{actor: selfActor(t, "codex")}); code != 0 || err != nil {
		t.Fatalf("bound claim = %d, %v", code, err)
	}
	lease, _, err := TryTaskLease(root, item, TaskLeaseOwner{RunID: "lease:codex", PID: os.Getpid(), Provider: "codex", Target: "codex"})
	if err != nil || lease == nil {
		t.Fatalf("lease: %v", err)
	}
	defer lease.Release()
	out := captureStdout(t, func() { _, _ = tasksFolderList(root, false) })
	if !strings.Contains(out, fmt.Sprintf("claimed by codex (pid %d) · busy codex", os.Getpid())) {
		t.Fatalf("a claimed and leased task must show both:\n%s", out)
	}
}

func TestLeaseBindsToTheTasksClaimantWhenItsOwnAncestryIsGone(t *testing.T) {
	root := t.TempDir()
	me := selfActor(t, "claude")
	inProgressTask(t, root, "mine")
	if _, err := claimTaskOwnerRecord(root, "mine", claimOptions{actor: me}); err != nil {
		t.Fatal(err)
	}
	// The holder found no agent above itself (its tool-call shell already exited): it follows the
	// live claim the same agent made a moment earlier, label included.
	req := bindLeaseToClaimant(root, leaseRequest{id: "mine"})
	if req.actor.PID != me.PID || req.actor.StartToken != me.StartToken || req.actor.Label != "claude" || !req.claimant {
		t.Fatalf("unbound holder did not follow the claimant: %+v (claimant %+v)", req, me)
	}
	if req := bindLeaseToClaimant(root, leaseRequest{id: "mine", actor: ClaimActor{Label: "codex@zed"}}); req.actor.Label != "codex@zed" || req.actor.PID != me.PID {
		t.Fatalf("--as label lost while following the claimant: %+v", req)
	}

	// A claimant that is gone (its pid reused by another process here) binds nothing: the holder
	// then holds until the task moves or it is stopped, exactly as before.
	inProgressTask(t, root, "stale")
	stale := ClaimActor{Label: "codex", PID: me.PID, StartToken: processidentity.StartToken(1)}
	if _, err := claimTaskOwnerRecord(root, "stale", claimOptions{actor: stale}); err != nil {
		t.Fatal(err)
	}
	if req := bindLeaseToClaimant(root, leaseRequest{id: "stale"}); req.actor.PID != 0 || req.claimant {
		t.Fatalf("holder bound to a claimant that is gone: %+v", req)
	}

	// A person's claim carries no process, so there is nothing to follow.
	inProgressTask(t, root, "human")
	if _, err := claimTaskOwnerRecord(root, "human", claimOptions{}); err != nil {
		t.Fatal(err)
	}
	if req := bindLeaseToClaimant(root, leaseRequest{id: "human"}); req.actor.PID != 0 || req.claimant {
		t.Fatalf("holder bound to a person's claim: %+v", req)
	}
}
