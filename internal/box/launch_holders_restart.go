package box

import (
	"context"
	"errors"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/processidentity"
)

// RestartableLaunch is the editor sessions holding a workspace's service launch lock, when they are all
// that holds it: a person waiting at a terminal may restart them so the launch goes on. A session's
// parked boxes hold the lock too and never stop by themselves, so closing one editor thread did not end
// the wait (2026-11-26).
//
// Only `coop acp` sessions are offered, since a restart is their SIGHUP reload: the threads stay, the
// boxes are replaced. A fork's editor supervisor has no reload, and SIGHUP would kill it. A box in a
// terminal is not offered either: an unfiltered one has no SIGTERM teardown, so its container would
// outlive the process the lock waits on, and a loop's debug shell is recorded as one, with the loop as
// its process. Loops, reviews and gates leave a task half done when stopped; `coop fork stop` ends those.
type RestartableLaunch struct {
	holders     []launchHolder
	supervisors []launchSupervisor
}

// launchHolder is one box the restart ends, by the process its record names.
type launchHolder struct {
	pid   int
	token string
}

// launchSupervisor is one editor session's supervisor, and a box of its that named it, checked again
// just before the signal.
type launchSupervisor struct {
	pid   int
	child launchHolder
}

var (
	// launchHolderParent and launchHolderCommand read the kernel's process table; tests replace them.
	launchHolderParent  = processidentity.Parent
	launchHolderCommand = processidentity.Command
	// launchHolderAlive reports whether the process a box record names still runs as that process.
	launchHolderAlive = func(pid int, token string) bool {
		return forkspace.ProcessIdentityOf(pid, token) == forkspace.ProcessIdentityMatch
	}
	signalLaunchHolder = func(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }
	launchHolderPoll   = 250 * time.Millisecond
)

// RestartableLaunchHolders reports the editor sessions holding repo's service launch lock, and false
// when the lock is free, a command holds it, a record cannot be read, or anything else holds it too:
// restarting some holders while others keep the wait going would promise a start that does not come.
func RestartableLaunchHolders(repo string) (RestartableLaunch, bool) {
	if taken, err := forkspace.ServiceLaunchTaken(repo); err != nil || !taken {
		return RestartableLaunch{}, false
	}
	boxes, _, err := launchHolderBoxes(repo, false, "", "")
	if err != nil || len(boxes) == 0 {
		return RestartableLaunch{}, false
	}
	var restart RestartableLaunch
	seen := map[int]bool{}
	for _, observation := range boxes {
		record := observation.Record
		if record.Kind != forkspace.ExecutionACP {
			return RestartableLaunch{}, false
		}
		// An editor box is a child of its session's supervisor, the coop the editor started: the same
		// program. Any other parent means that supervisor is gone, and nothing here may signal a
		// process it cannot name.
		child := launchHolder{pid: record.PID, token: record.Token}
		supervisor, command := launchHolderParent(record.PID), launchHolderCommand(record.PID)
		if supervisor <= 1 || command == "" || launchHolderCommand(supervisor) != command {
			return RestartableLaunch{}, false
		}
		restart.holders = append(restart.holders, child)
		if !seen[supervisor] {
			seen[supervisor] = true
			restart.supervisors = append(restart.supervisors, launchSupervisor{pid: supervisor, child: child})
		}
	}
	return restart, true
}

// Sessions is how many editor sessions a restart reaches.
func (r RestartableLaunch) Sessions() int { return len(r.supervisors) }

// Restart reloads each session's supervisor, then waits until none of the boxes that held the lock
// runs, or ctx ends. The reloaded sessions' new boxes then most likely queue behind the launch the
// caller takes next, as reloading takes seconds, but flock promises no order: if one gets in first, the
// caller's ordinary wait names it.
func (r RestartableLaunch) Restart(ctx context.Context) error {
	for _, supervisor := range r.supervisors {
		// The box that named this supervisor must still run as itself, under it: a pid read before the
		// question may belong to someone else by the time the answer comes.
		child := supervisor.child
		if !launchHolderAlive(child.pid, child.token) || launchHolderParent(child.pid) != supervisor.pid {
			continue
		}
		if err := signalLaunchHolder(supervisor.pid, syscall.SIGHUP); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
	}
	for {
		running := false
		for _, holder := range r.holders {
			running = running || launchHolderAlive(holder.pid, holder.token)
		}
		if !running {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(launchHolderPoll):
		}
	}
}
