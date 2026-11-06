package tasks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/processidentity"
	"github.com/AndrewDryga/coop/internal/ui"
)

// leaseHoldPoll is how often a held lease re-checks that the bound process still exists and that
// the task is still in progress; tests shorten it.
var leaseHoldPoll = 2 * time.Second

// leaseHolderRunPrefix marks a lease taken by `coop tasks lease` rather than a loop iteration; only
// such a holder may be told to stand down by the agent that started it.
const leaseHolderRunPrefix = "lease:"

// stopOwnLeaseHolder lets `coop tasks done` (and block) finish a task whose lease is held by a
// `coop tasks lease` holder the SAME agent started: the holder is bound to the caller's own process
// identity, so telling it to stand down is the agent stopping its own worker, not stealing a lock.
// A loop iteration's lease, a holder bound to another identity, or a holder whose process cannot
// be proved are all left alone — the caller then gets the usual "leased by another controller".
func stopOwnLeaseHolder(root string, item Item, actor ClaimActor) (bool, error) {
	holder, err := stopLeaseHolderBoundTo(root, item, actor.PID, actor.StartToken)
	if holder == 0 || err != nil {
		return false, err
	}
	ui.Note("stopped your lease holder (pid %d) so %s could move", holder, item.ID)
	return true, nil
}

// stopLeaseHolderBoundTo stops the `coop tasks lease` holder of item that is bound to the process
// pid started at start, and waits for its lease to end. It returns the holder's pid, or 0 when no
// such holder holds the lease: a loop iteration's lease and a holder whose own identity cannot be
// proved are never signalled. Done stops the caller's own holder this way, and a forced claim the
// holder of the process it took the task from.
func stopLeaseHolderBoundTo(root string, item Item, pid int, start string) (int, error) {
	if pid == 0 || observeTaskLease(item, time.Now()).State == leaseUnleased {
		return 0, nil
	}
	meta, ok := readLeaseAuthorityMetadata(root, item.ID)
	if !ok || !strings.HasPrefix(meta.RunID, leaseHolderRunPrefix) ||
		meta.ActorPID != pid || !processidentity.Same(meta.ActorStart, start) ||
		processidentity.Inspect(meta.ControllerPID, meta.ControllerStart) != processidentity.Match {
		return 0, nil
	}
	if err := syscall.Kill(meta.ControllerPID, syscall.SIGTERM); err != nil {
		return 0, fmt.Errorf("stop lease holder (pid %d): %w", meta.ControllerPID, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for observeTaskLease(item, time.Now()).State != leaseUnleased {
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("lease holder (pid %d) did not release within 5s", meta.ControllerPID)
		}
		time.Sleep(25 * time.Millisecond)
	}
	return meta.ControllerPID, nil
}

// leaseRequest is what `coop tasks lease` learned from its arguments.
type leaseRequest struct {
	id       string
	actor    ClaimActor
	claimant bool // actor came from the task's claim, not from this holder's own ancestry
	command  []string
}

const leaseUsage = "coop tasks lease <task-id> [--as <label>] [--pid <n>] [-- <command>...]"

// parseLeaseArgs reads `coop tasks lease <id> [--as <label>] [--pid <n>] [-- <command...>]`.
func parseLeaseArgs(args []string) (leaseRequest, error) {
	var req leaseRequest
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			req.command = args[i+1:]
			if len(req.command) == 0 {
				return req, ui.MissingArgument("command after --", "coop tasks lease", leaseUsage)
			}
			break
		}
		key, value, hasValue := strings.Cut(args[i], "=")
		switch key {
		case "--as", "--pid":
			if !hasValue {
				if i+1 >= len(args) {
					return req, ui.MissingOptionValue(key, "coop tasks lease", actorOptionExample("coop tasks lease", key))
				}
				i++
				value = args[i]
			}
			if key == "--as" {
				if label := claimActorLabel(value); label == "" || label != value {
					return req, invalidActorLabel(value, "coop tasks lease")
				}
				req.actor.Label = value
				continue
			}
			n, err := strconv.Atoi(value)
			if err != nil || n <= 1 {
				return req, invalidActorPID(value, "coop tasks lease", "working")
			}
			req.actor.PID = n
		default:
			if strings.HasPrefix(args[i], "-") && args[i] != "-" {
				return req, unknownOptionErr(key, "coop tasks lease", []string{"--as", "--pid"})
			}
			if req.id != "" {
				return req, ui.UnexpectedArgument(args[i], "coop tasks lease", leaseUsage)
			}
			req.id = args[i]
		}
	}
	if req.id == "" {
		return req, ui.MissingArgument("task ID", "coop tasks lease", leaseUsage)
	}
	return req, nil
}

// tasksFolderLease is `coop tasks lease`: it takes the same per-task lock a loop iteration takes,
// so ls/watch show the task as busy and a loop in this checkout skips it, and holds it for exactly
// as long as the work lasts — the given command, or, without one, until the bound process is gone,
// the task leaves in_progress, or the holder is stopped. The lock dies with this process, so an
// abandoned agent can never leave a task looking busy.
func tasksFolderLease(root string, args []string) (int, error) {
	req, err := parseLeaseArgs(args)
	if err != nil {
		return 2, err
	}
	requested, interactive := req.actor.PID, stdinIsTerminal()
	req.actor = captureClaimActor(realClaimActorProbe, os.Getppid(), interactive, req.actor)
	if requested != 0 && req.actor.PID == 0 {
		return 1, noLiveActorProcess(requested)
	}
	if requested == 0 && req.actor.PID == 0 {
		req = bindLeaseToClaimant(root, req)
	}
	if interactive && req.actor.PID == 0 && req.actor.Label == "" {
		// A person holds this lease at a terminal: name them, as their claim does.
		user, _ := taskOwnerIdentity()
		if req.actor.Label = claimActorLabel(user); req.actor.Label == "" {
			req.actor.Label = "person"
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return holdTaskLease(ctx, root, req, leaseHoldPoll)
}

// bindLeaseToClaimant binds a holder that could not identify its own agent to the task's claimant
// instead. An IDE agent starts the holder from a tool-call shell that exits right after the fork,
// so by the time the holder looks, its parent is already init and the ancestor walk finds nothing —
// while the claim the same agent made a moment earlier (synchronous, its shell still alive) is
// bound to that agent. Following the live claim keeps `done` and `block` able to stop the holder.
// A person's claim, an absent record, or a claimant that is gone leaves the holder unbound, and so
// does a claimant on another operating system: this holder could never see that process exit.
func bindLeaseToClaimant(root string, req leaseRequest) leaseRequest {
	owner, owned, err := ReadTaskOwnerRecord(root, req.id)
	if err != nil || !owned || owner.ActorPID == 0 || claimantElsewhere(owner) || !ownerProcessLive(owner) {
		return req
	}
	req.actor.PID, req.actor.StartToken, req.claimant = owner.ActorPID, owner.ActorStart, true
	if req.actor.Label == "" {
		req.actor.Label = owner.Actor
	}
	return req
}

// holdTaskLease is the testable core of `coop tasks lease`.
func holdTaskLease(ctx context.Context, root string, req leaseRequest, poll time.Duration) (int, error) {
	t, err := FindTask(root, req.id)
	if err != nil {
		return lookupExit(err), err
	}
	if t.State != StateInProgress {
		return 1, fmt.Errorf("%s is %s — a lease covers work in progress; claim it first: coop tasks claim %s", t.ID, StateLabel(t.State), t.ID)
	}
	if record, owned, err := readTaskOwnerRecordOf(root, t); err != nil {
		return -1, err
	} else if refused := takenOverRefusal("lease", record, req.actor); owned && refused != nil {
		return 1, refused
	}
	label := req.actor.Label
	if label == "" {
		label = "agent"
	}
	lease, observed, err := TryTaskLease(root, t, TaskLeaseOwner{
		RunID: leaseHolderRunPrefix + label, PID: os.Getpid(), Provider: label, Target: label,
		ControllerStart: processidentity.StartToken(os.Getpid()),
		ActorPID:        req.actor.PID, ActorStart: req.actor.StartToken,
	})
	if errors.Is(err, errLeaseCandidateGone) {
		return 1, fmt.Errorf("%s moved while the lease was being taken — re-run", t.ID)
	}
	if err != nil {
		return -1, err
	}
	if lease == nil {
		return 1, fmt.Errorf("%s is already leased (%s) — one worker at a time; wait for it or stop it", t.ID, observed.label())
	}
	defer func() { _ = lease.Release() }()

	// The lease line and its end are one pair: "✓ Leased <id>" and "✓ Lease on <id> ended: why",
	// then where that leaves the task. "Released" would read as coop tasks release, which a lease
	// ending is not: the task stays in progress and claimed.
	ui.OK("Leased %s%s", t.ID, claimSuffix(req.actor))
	if len(req.command) > 0 {
		ui.Note("  Running: %s", shellJoin(req.command))
		cmd := exec.CommandContext(ctx, req.command[0], req.command[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
		cmd.WaitDelay = 5 * time.Second
		err := cmd.Run()
		var exit *exec.ExitError
		switch {
		case err == nil:
			ui.OK("Lease on %s ended: the command finished", t.ID)
			noteLeaseEnd(root, t.ID, req.actor)
			return 0, nil
		case errors.As(err, &exit):
			// No ✓: the lease ended, but the command failed and its status is coop's exit status.
			if ctx.Err() != nil {
				ui.Note("Lease on %s ended: this holder was stopped, and its command with it", t.ID)
			} else {
				ui.Note("Lease on %s ended: the command exited with status %d", t.ID, exit.ExitCode())
			}
			noteLeaseEnd(root, t.ID, req.actor)
			return exit.ExitCode(), nil
		default:
			return 1, fmt.Errorf("run %s: %w", req.command[0], err)
		}
	}

	switch {
	case req.claimant:
		ui.Note("  Bound to the task's claimant. Holding until that process exits, the task moves, or this holder is stopped.")
	case req.actor.PID != 0:
		ui.Note("  Holding until that process exits, the task moves, or this holder is stopped.")
	default:
		ui.Note("  Holding until the task moves or this holder is stopped (Ctrl-C).")
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			ui.OK("Lease on %s ended: this holder was stopped", t.ID)
			noteLeaseEnd(root, t.ID, req.actor)
			return 0, nil
		case <-ticker.C:
		}
		if req.actor.PID != 0 && !ownerProcessLive(TaskOwnerRecord{ActorPID: req.actor.PID, ActorStart: req.actor.StartToken}) {
			ui.OK("Lease on %s ended: %s (PID %d) has exited", t.ID, label, req.actor.PID)
			noteLeaseEnd(root, t.ID, req.actor)
			return 0, nil
		}
		current, ok, err := CurrentTask(root, t.ID)
		if err != nil {
			return -1, err
		}
		if !ok {
			ui.OK("Lease on %s ended: the task is gone", t.ID)
			return 0, nil
		}
		if current.State != StateInProgress {
			ui.OK("Lease on %s ended: the task is now %s", t.ID, stateWords(current.State))
			return 0, nil
		}
	}
}

// noteLeaseEnd says where a task stands once its lease ended. A lease is not a claim, so a task
// still in progress stays claimed by whoever claimed it — unless a forced claim took it from this
// holder's process, which is why the holder was stopped.
func noteLeaseEnd(root, id string, actor ClaimActor) {
	current, ok, err := CurrentTask(root, id)
	if err != nil || !ok {
		return
	}
	if current.State != StateInProgress {
		ui.Note("  The task is now %s.", stateWords(current.State))
		return
	}
	record, owned, err := readTaskOwnerRecordOf(root, current)
	switch {
	case err != nil || !owned:
		ui.Note("  The task stays in progress.")
	case takenFrom(record, actor):
		ui.Note("  %s took the task over.", claimHolder(record, true))
	case record.Kind == TaskOwnerHuman:
		ui.Note("  The task stays in progress, claimed by %s.", claimHolder(record, true))
	default:
		ui.Note("  The task stays in progress, %s.", TaskOwnerLabel(record))
	}
}

// stateWords names a lifecycle state as a sentence says it: "in progress", not "in_progress".
func stateWords(state string) string { return strings.ReplaceAll(StateLabel(state), "_", " ") }
