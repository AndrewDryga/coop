package cli

import (
	"os"
	"path/filepath"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/loop"
	"github.com/AndrewDryga/coop/internal/taskmcp"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

// sessionTaskTools is the task-tools server an interactive agent box gets — an editor's `coop acp`
// box or a `coop <agent>` box in a plain checkout — so its agent changes task state the way a loop
// box does, without the coop CLI. Nothing is assigned to a session: its agent claims the task it
// works, and those claims bind to claimant, the host process that serves the session. A project
// with no task queue gets no tools, and so does one whose tools cannot be set up: the session is
// the work, the tools only help with it, so it starts without them and says why.
func (a *app) sessionTaskTools(repo, provider string, claimant int) box.TaskToolServer {
	server, err := newSessionTaskTools(a.cfg, repo, provider, claimant)
	if err != nil {
		ui.Warning("Task tools are unavailable in this session", err.Error(), "")
		return nil
	}
	return server
}

// sessionTaskClaimant reports whether a launch is an agent session that gets task tools, and the
// process its claims bind to. An editor child binds them to the supervisor that started it, which
// lives as long as the editor session while a child is replaced on a provider switch, a network
// refresh or a rebuild; a child run on its own binds them to itself, as `coop <agent>` does. Fork
// sessions, controller jobs, sign-in, model probes and plain commands get none.
func sessionTaskClaimant(spec box.RunSpec) (int, bool) {
	switch {
	case spec.ForkName != "", spec.ControllerJob, spec.Login, spec.ActivityRole == forkspace.ExecutionRoleProbe:
		return 0, false
	case spec.ActivityKind == forkspace.ExecutionACP && spec.SupervisorID != "":
		return os.Getppid(), true
	case spec.ActivityKind == forkspace.ExecutionACP, spec.ActivityKind == forkspace.ExecutionInteractive && spec.AgentCommand:
		return os.Getpid(), true
	}
	return 0, false
}

func newSessionTaskTools(cfg *config.Config, repo, provider string, claimant int) (box.TaskToolServer, error) {
	queues, err := tasks.TaskQueues(cfg, repo, nil)
	if err != nil {
		return nil, err
	}
	roots := make([]string, len(queues))
	found := false
	for i, queue := range queues {
		roots[i] = filepath.Join(repo, queue)
		if info, err := os.Stat(roots[i]); err == nil && info.IsDir() {
			found = true
		}
	}
	if !found {
		return nil, nil
	}
	actor, err := tasks.SessionClaimActor(provider, claimant)
	if err != nil {
		return nil, err
	}
	// What the session's box runs, as a loop's lease records it, so coop tasks watch names the
	// session's claims and leases by it: cfg is set up for this session's provider and account.
	actor.Target = loop.RecordedTarget(cfg, agents.Target{Provider: provider, Accounts: []string{cfg.ActiveProfile(provider)}})
	server, err := taskmcp.New(taskmcp.Authority{
		Workspace: repo, QueueRoots: roots, Session: &actor,
		Owner: tasks.TaskLeaseOwner{PID: os.Getpid(), Provider: provider, Target: actor.Target},
	})
	if err != nil {
		return nil, err
	}
	return server, nil
}
