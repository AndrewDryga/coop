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
	if actor.PID == 0 || observeTaskLease(item, time.Now()).State == leaseUnleased {
		return false, nil
	}
	meta, ok := readLeaseAuthorityMetadata(root, item.ID)
	if !ok || !strings.HasPrefix(meta.RunID, leaseHolderRunPrefix) ||
		meta.ActorPID != actor.PID || meta.ActorStart != actor.StartToken ||
		processidentity.Inspect(meta.ControllerPID, meta.ControllerStart) != processidentity.Match {
		return false, nil
	}
	if err := syscall.Kill(meta.ControllerPID, syscall.SIGTERM); err != nil {
		return false, fmt.Errorf("stop own lease holder (pid %d): %w", meta.ControllerPID, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for observeTaskLease(item, time.Now()).State != leaseUnleased {
		if time.Now().After(deadline) {
			return false, fmt.Errorf("own lease holder (pid %d) did not release within 5s", meta.ControllerPID)
		}
		time.Sleep(25 * time.Millisecond)
	}
	ui.Note("stopped your lease holder (pid %d) so %s could move", meta.ControllerPID, item.ID)
	return true, nil
}

// leaseRequest is what `coop tasks lease` learned from its arguments.
type leaseRequest struct {
	id      string
	actor   ClaimActor
	command []string
}

// parseLeaseArgs reads `coop tasks lease <id> [--as <label>] [--pid <n>] [-- <command...>]`.
func parseLeaseArgs(args []string) (leaseRequest, error) {
	var req leaseRequest
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			req.command = args[i+1:]
			if len(req.command) == 0 {
				return req, errors.New("coop tasks lease: nothing follows -- (drop it to hold the lease until stopped)")
			}
			break
		}
		key, value, hasValue := strings.Cut(args[i], "=")
		switch key {
		case "--as", "--pid":
			if !hasValue {
				if i+1 >= len(args) {
					return req, fmt.Errorf("coop tasks lease: %s needs a value", key)
				}
				i++
				value = args[i]
			}
			if key == "--as" {
				if label := claimActorLabel(value); label == "" || label != value {
					return req, errors.New("coop tasks lease: --as takes a short label made of letters, digits, and the characters -_@.: only")
				}
				req.actor.Label = value
				continue
			}
			n, err := strconv.Atoi(value)
			if err != nil || n <= 1 {
				return req, errors.New("coop tasks lease: --pid takes the process id of the working agent")
			}
			req.actor.PID = n
		default:
			if strings.HasPrefix(args[i], "-") && args[i] != "-" {
				return req, fmt.Errorf("coop tasks lease: unknown flag %q (supported: --as, --pid, and -- <command>)", args[i])
			}
			if req.id != "" {
				return req, errors.New("coop tasks lease: too many arguments (expected one task id; put a command after --)")
			}
			req.id = args[i]
		}
	}
	if req.id == "" {
		return req, errors.New("usage: coop tasks lease <id> [--as <label>] [--pid <n>] [-- <command...>]")
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
	requested := req.actor.PID
	req.actor = captureClaimActor(realClaimActorProbe, os.Getppid(), ui.IsTerminal(os.Stdin), req.actor)
	if requested != 0 && req.actor.PID == 0 {
		return 1, fmt.Errorf("coop tasks lease: no live process with a readable identity at pid %d — it is not running, or its identity cannot be read", requested)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return holdTaskLease(ctx, root, req, leaseHoldPoll)
}

// holdTaskLease is the testable core of `coop tasks lease`.
func holdTaskLease(ctx context.Context, root string, req leaseRequest, poll time.Duration) (int, error) {
	t, err := FindTask(root, req.id)
	if err != nil {
		return 1, err
	}
	if t.State != StateInProgress {
		return 1, fmt.Errorf("%s is %s — a lease covers work in progress; claim it first: coop tasks claim %s", t.ID, StateLabel(t.State), t.ID)
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
	released := "released " + t.ID
	defer func() { _ = lease.Release() }()

	if len(req.command) > 0 {
		ui.OK("leased %s%s — running: %s", t.ID, claimSuffix(req.actor), strings.Join(req.command, " "))
		cmd := exec.CommandContext(ctx, req.command[0], req.command[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
		cmd.WaitDelay = 5 * time.Second
		err := cmd.Run()
		var exit *exec.ExitError
		switch {
		case err == nil:
			ui.Note("%s — command finished", released)
			return 0, nil
		case errors.As(err, &exit):
			ui.Note("%s — command exited %d", released, exit.ExitCode())
			return exit.ExitCode(), nil
		default:
			return 1, fmt.Errorf("run %s: %w", req.command[0], err)
		}
	}

	switch {
	case req.actor.PID != 0:
		ui.OK("leased %s%s — holding until that process exits, the task moves, or this holder is stopped", t.ID, claimSuffix(req.actor))
	default:
		ui.OK("leased %s%s — holding until the task moves or this holder is stopped (Ctrl-C)", t.ID, claimSuffix(req.actor))
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			ui.Note("%s — stopped", released)
			return 0, nil
		case <-ticker.C:
		}
		if req.actor.PID != 0 && !ownerProcessLive(TaskOwnerRecord{ActorPID: req.actor.PID, ActorStart: req.actor.StartToken}) {
			ui.Note("%s — %s (pid %d) is gone", released, label, req.actor.PID)
			return 0, nil
		}
		current, ok, err := CurrentTask(root, t.ID)
		if err != nil {
			return -1, err
		}
		if !ok || current.State != StateInProgress {
			state := "gone"
			if ok {
				state = StateLabel(current.State)
			}
			ui.Note("%s — the task is now %s", released, state)
			return 0, nil
		}
	}
}
