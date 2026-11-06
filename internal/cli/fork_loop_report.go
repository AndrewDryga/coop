package cli

import (
	"slices"
	"strings"

	"github.com/AndrewDryga/coop/internal/forkctl"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/loop"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

// What a fork loop says when it has nothing to do. A foreground loop prints it, and a background
// worker's output is its log, which `coop fork logs` shows: both used to end in silence, after a
// "✓ Started" that said work had begun.

// refuseClosedFork refuses a loop on a fork whose reviewed work already landed (or was discarded). Its
// candidate history is closed, so the scheduler gives it no task: a kept merged fork is evidence, not
// a scheduler. The loop would end at once with nothing said, so the refusal says why and what works
// instead. args are the fork command as typed, whose spelling the next steps repeat.
func refuseClosedFork(repo string, identity forkspace.Identity, args []string) error {
	status, ok, err := tasks.ReadForkCandidateRoundStatus(repo, identity)
	if err != nil {
		return err
	}
	if !ok || !status.Closed() {
		return nil
	}
	return closedForkErr(identity.Name, status.Landed(), args)
}

// closedForkErr is the refusal block: why the fork takes no task, then the two ways on. --fresh
// recreates the fork and deletes its kept files, so a new fork is offered beside it.
func closedForkErr(name string, landed bool, args []string) *ui.UsageError {
	headline, cause := "Fork "+name+" already merged its work", "A merged fork keeps its files for you to look at, and takes no new tasks."
	if !landed {
		headline, cause = "Fork "+name+"'s reviewed work was discarded", "A fork whose work was discarded takes no new tasks."
	}
	return ui.CommandFailed(headline, cause,
		[2]string{"Start it over:", forkCommandLine(shellWord(name), args, "--fresh")},
		[2]string{"Use a new fork:", forkCommandLine("<new-name>", args)},
	)
}

// forkCommandLine is the fork command a person typed, as one line to paste, with the fork named name
// and each option in add that it lacks. The detached worker's marker is not something a person types.
func forkCommandLine(name string, args []string, add ...string) string {
	parts := []string{"coop", "fork", name}
	if len(args) > 1 {
		for _, arg := range args[1:] {
			if arg != "--_detached" {
				parts = append(parts, shellWord(arg))
			}
		}
	}
	for _, option := range add {
		if !slices.Contains(args, option) {
			parts = append(parts, option)
		}
	}
	return strings.Join(parts, " ")
}

// forkNoWork ends a loop that took no task and has no reviewed work to publish. It says why the way
// coop loop does for an empty queue, and notes it for coop fork ls, which shows it beside the idle
// fork. A foreground loop goes on to the usual next steps.
func forkNoWork(ws string, identity forkspace.Identity, queues []string, detached bool) (int, error) {
	counts, _, err := tasks.QueueProgress(queues)
	if err != nil {
		return 1, err
	}
	reason, code := "", 0
	if counts.Todo+counts.Doing == 0 {
		reason, code = loop.NoActionableTasks(counts)
	} else {
		// Tasks remain, so the queue is not empty: other agents hold them, or they wait for tasks that
		// are not done, which a fork never takes (the scheduler leaves them to the plain loop).
		reason = "No task is available for fork " + identity.Name + "."
		ui.Note("%s", reason)
		ui.Note("  Its remaining tasks are held by other agents, or wait for tasks that are not done.")
	}
	forkctl.NoteForkLoopEnd(ws, identity, reason)
	if !detached {
		forkctl.ForkNextSteps(identity.Name)
	}
	return code, nil
}

// printForkReady says a fork's reviewed work waits for a merge, and the two commands that go on.
func printForkReady(name string) {
	ui.Note("")
	ui.OK("Fork %s is ready to merge", name)
	ui.Note("")
	ui.Note("  Review: coop fork review %s", name)
	ui.Note("  Merge:  coop fork merge %s", name)
}
