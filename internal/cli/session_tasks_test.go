package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/taskmcp"
	"github.com/AndrewDryga/coop/internal/tasks"
)

// An agent session gets task tools only in a project with a task queue, bound to the process that
// serves it; what cannot be set up is an error the launch reports as a warning, never a server
// whose claims would bind to nothing.
func TestSessionTaskToolsServeAProjectWithAQueue(t *testing.T) {
	repo := t.TempDir()
	cfg := &config.Config{}
	if server, err := newSessionTaskTools(cfg, repo, "claude", os.Getpid()); err != nil || server != nil {
		t.Fatalf("a folder with no task queue = %v, %v; want no tools", server, err)
	}
	if err := tasks.ScaffoldStateDirs(filepath.Join(repo, ".agent", "tasks")); err != nil {
		t.Fatal(err)
	}
	server, err := newSessionTaskTools(cfg, repo, "claude", os.Getpid())
	if err != nil || server == nil {
		t.Fatalf("a project with a queue = %v, %v; want a session server", server, err)
	}
	authority := server.(*taskmcp.Server).Authority()
	if authority.Session == nil || !authority.Session.Bound() || authority.Session.PID != os.Getpid() || authority.Session.Label != "claude" ||
		authority.Workspace != repo || authority.Assigned != "" || authority.ProposalOutbox != "" {
		t.Fatalf("session authority = %+v", authority)
	}
	// Its claims and leases name the target the session's box runs, as a loop's leases do.
	cfg.SetActiveProfile("claude", "work")
	cfg.SetTargetModel("claude", "opus")
	cfg.SetTargetEffort("claude", "high")
	server, err = newSessionTaskTools(cfg, repo, "claude", os.Getpid())
	if err != nil || server == nil {
		t.Fatalf("a session with a target = %v, %v", server, err)
	}
	if authority := server.(*taskmcp.Server).Authority(); authority.Session.Target != "claude:opus/high@work" || authority.Owner.Target != "claude:opus/high@work" {
		t.Fatalf("session target = %q, lease target = %q, want claude:opus/high@work", authority.Session.Target, authority.Owner.Target)
	}
	if _, err := newSessionTaskTools(cfg, repo, "claude", 0); err == nil {
		t.Fatal("a claimant with no process identity must fail setup, not bind claims to nothing")
	}
	cfg.TasksFiles = []string{filepath.Join(t.TempDir(), "elsewhere")}
	if _, err := newSessionTaskTools(cfg, repo, "claude", os.Getpid()); err == nil {
		t.Fatal("a queue outside the project must fail setup")
	}
}

// Only agent sessions get task tools, and an editor child binds its claims to the supervisor that
// started it — the process that outlives a provider switch — never to itself.
func TestSessionTaskClaimantPicksSessionsAndTheirOwner(t *testing.T) {
	for name, tc := range map[string]struct {
		spec box.RunSpec
		want int
		ok   bool
	}{
		"editor child":        {box.RunSpec{ActivityKind: forkspace.ExecutionACP, SupervisorID: "s"}, os.Getppid(), true},
		"warm editor child":   {box.RunSpec{ActivityKind: forkspace.ExecutionACP, SupervisorID: "s", ActivityRole: forkspace.ExecutionRoleWarm}, os.Getppid(), true},
		"unsupervised editor": {box.RunSpec{ActivityKind: forkspace.ExecutionACP}, os.Getpid(), true},
		"agent session":       {box.RunSpec{ActivityKind: forkspace.ExecutionInteractive, AgentCommand: true}, os.Getpid(), true},
		"plain command":       {box.RunSpec{ActivityKind: forkspace.ExecutionInteractive}, 0, false},
		"sign-in":             {box.RunSpec{ActivityKind: forkspace.ExecutionInteractive, AgentCommand: true, Login: true}, 0, false},
		"fork editor child":   {box.RunSpec{ActivityKind: forkspace.ExecutionForkACP, ForkName: "f", SupervisorID: "s"}, 0, false},
		"fork agent session":  {box.RunSpec{ActivityKind: forkspace.ExecutionForkInteractive, ForkName: "f", AgentCommand: true}, 0, false},
		"controller job":      {box.RunSpec{ActivityKind: forkspace.ExecutionACP, SupervisorID: "s", ControllerJob: true}, 0, false},
		"model probe":         {box.RunSpec{ActivityKind: forkspace.ExecutionACP, SupervisorID: "s", ActivityRole: forkspace.ExecutionRoleProbe}, 0, false},
		"loop debug shell":    {box.RunSpec{ActivityKind: forkspace.ExecutionLocalLoop}, 0, false},
	} {
		if got, ok := sessionTaskClaimant(tc.spec); got != tc.want || ok != tc.ok {
			t.Errorf("%s: sessionTaskClaimant = %d, %t; want %d, %t", name, got, ok, tc.want, tc.ok)
		}
	}
}

// The supervisor removes a killed child's channel volume by the name it chose, waiting while the
// child's helper still mounts it, and does not wait on a volume that was never made.
func TestRemoveACPTaskChannelWaitsForTheHelperToLetGo(t *testing.T) {
	dir := t.TempDir()
	volume, tries := filepath.Join(dir, "volume"), filepath.Join(dir, "tries")
	if err := os.WriteFile(volume, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "runtime")
	body := "#!/bin/sh\ncase \"$1 $2\" in\n" +
		"\"volume inspect\") [ \"$3\" = coop-tasks-0123456789abcdef ] && [ -e '" + volume + "' ] ;;\n" +
		"\"volume rm\") n=$(cat '" + tries + "' 2>/dev/null || echo 0); n=$((n+1)); echo $n > '" + tries + "'\n" +
		"  [ \"$n\" -ge 3 ] || { echo 'volume is in use' >&2; exit 1; }\n  rm -f '" + volume + "' ;;\n" +
		"*) exit 1 ;;\nesac\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{}, rt: runtime.Runtime{Name: script}, rtSet: true}
	a.removeACPTaskChannel("coop-tasks-0123456789abcdef")
	if _, err := os.Stat(volume); !os.IsNotExist(err) {
		t.Fatalf("the volume outlived its helper: %v", err)
	}
	start := time.Now()
	a.removeACPTaskChannel("coop-tasks-fedcba9876543210")
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("waited %s on a volume that was never made", waited)
	}
}
