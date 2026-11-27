package box

import (
	"context"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
)

// fakeLaunchProcesses gives every box record the same supervisor, supervisorPID, running the program
// supervisorCommand names (the boxes run "coop"), and records each signal instead of sending it. Every
// box runs until a signal arrives, or for ever when stops is false, or once a test clears alive.
func fakeLaunchProcesses(t *testing.T, supervisorPID int, supervisorCommand string, stops bool) (sent *[]string, alive *bool) {
	t.Helper()
	sent, alive = new([]string), new(bool)
	*alive = true
	parent, command, isAlive, signal, poll := launchHolderParent, launchHolderCommand, launchHolderAlive, signalLaunchHolder, launchHolderPoll
	launchHolderParent = func(int) int { return supervisorPID }
	launchHolderCommand = func(pid int) string {
		if pid == supervisorPID {
			return supervisorCommand
		}
		return "coop"
	}
	launchHolderAlive = func(int, string) bool { return *alive && (!stops || len(*sent) == 0) }
	signalLaunchHolder = func(pid int, sig syscall.Signal) error {
		who := "box"
		if pid == supervisorPID {
			who = "supervisor"
		}
		*sent = append(*sent, who+" "+sig.String())
		return nil
	}
	launchHolderPoll = time.Millisecond
	t.Cleanup(func() {
		launchHolderParent, launchHolderCommand, launchHolderAlive, signalLaunchHolder, launchHolderPoll = parent, command, isAlive, signal, poll
	})
	return sent, alive
}

// Only a lock held entirely by editor sessions is offered: a free lock or one only a command holds has
// nothing to restart, and any other holder keeps the wait going whatever a person restarts. A fork's
// editor supervisor has no reload, a terminal box no teardown on SIGTERM, and a loop's debug shell is
// recorded as a terminal box.
func TestOnlyEditorSessionsAreOfferedARestart(t *testing.T) {
	repo := t.TempDir()
	fakeLaunchProcesses(t, 4242, "coop", true)
	if _, ok := RestartableLaunchHolders(repo); ok {
		t.Fatal("a free lock was offered a restart")
	}
	holdLaunchLock(t, repo)
	if _, ok := RestartableLaunchHolders(repo); ok {
		t.Fatal("a lock only a command holds was offered a restart")
	}
	recordHolder(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionACP, Role: forkspace.ExecutionRoleActive})
	recordHolder(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionACP, Role: forkspace.ExecutionRoleWarm})
	sessions, ok := RestartableLaunchHolders(repo)
	// The session's parked box and its active one have one supervisor: one session, one restart.
	if !ok || sessions.Sessions() != 1 {
		t.Fatalf("one editor session = %+v, %v; want it offered once", sessions, ok)
	}
	for _, kind := range []forkspace.ExecutionKind{forkspace.ExecutionInteractive, forkspace.ExecutionForkACP, forkspace.ExecutionLocalLoop} {
		t.Run(string(kind), func(t *testing.T) {
			record := recordHolder(t, repo, forkspace.ExecutionSpec{Kind: kind})
			if sessions, ok := RestartableLaunchHolders(repo); ok {
				t.Fatalf("a %s among the holders was offered a restart: %+v", kind, sessions)
			}
			_ = forkspace.EndExecution(repo, record)
		})
	}
}

// An editor box whose parent is not the program that runs it has lost its supervisor: init, or a pid
// some other process took. Nothing is offered then, since a signal would reach a stranger.
func TestAnEditorBoxWithoutItsSupervisorIsNotOffered(t *testing.T) {
	for name, parent := range map[string]struct {
		pid     int
		command string
	}{
		"reparented to init": {1, "coop"},
		"another program":    {4242, "zsh"},
		"unreadable":         {4242, ""},
	} {
		t.Run(name, func(t *testing.T) {
			repo := t.TempDir()
			fakeLaunchProcesses(t, parent.pid, parent.command, true)
			holdLaunchLock(t, repo)
			recordHolder(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionACP})
			if sessions, ok := RestartableLaunchHolders(repo); ok {
				t.Fatalf("offered %+v", sessions)
			}
		})
	}
}

// A yes reloads the session's supervisor once, the reload that keeps its threads, and returns once
// its boxes are gone.
func TestRestartReloadsTheSupervisorOnceAndWaitsForItsBoxes(t *testing.T) {
	repo := t.TempDir()
	sent, _ := fakeLaunchProcesses(t, 4242, "coop", true)
	holdLaunchLock(t, repo)
	recordHolder(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionACP, Role: forkspace.ExecutionRoleActive})
	recordHolder(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionACP, Role: forkspace.ExecutionRoleWarm})
	sessions, ok := RestartableLaunchHolders(repo)
	if !ok {
		t.Fatal("not offered")
	}
	if err := sessions.Restart(t.Context()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"supervisor hangup"}; !slices.Equal(*sent, want) {
		t.Fatalf("signals = %v, want %v", *sent, want)
	}
}

// The answer can come long after the question. A box that ended meanwhile, or one whose supervisor
// changed, may name a pid that is someone else's now: nothing is signalled for it.
func TestRestartSignalsNothingForABoxThatChangedSinceTheQuestion(t *testing.T) {
	for name, change := range map[string]func(alive *bool){
		"the box ended":          func(alive *bool) { *alive = false },
		"its supervisor changed": func(*bool) { launchHolderParent = func(int) int { return 777 } },
	} {
		t.Run(name, func(t *testing.T) {
			repo := t.TempDir()
			sent, alive := fakeLaunchProcesses(t, 4242, "coop", true)
			holdLaunchLock(t, repo)
			recordHolder(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionACP})
			sessions, ok := RestartableLaunchHolders(repo)
			if !ok {
				t.Fatal("not offered")
			}
			change(alive)
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()
			_ = sessions.Restart(ctx)
			if len(*sent) != 0 {
				t.Fatalf("signalled %v", *sent)
			}
		})
	}
}

// Boxes that outlast the context leave Restart with its error, so the caller can say so and fall back
// to the ordinary wait, which names them.
func TestRestartGivesUpWhenTheBoxesOutlastTheContext(t *testing.T) {
	repo := t.TempDir()
	fakeLaunchProcesses(t, 4242, "coop", false)
	holdLaunchLock(t, repo)
	recordHolder(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionACP})
	sessions, ok := RestartableLaunchHolders(repo)
	if !ok {
		t.Fatal("not offered")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := sessions.Restart(ctx); err != context.DeadlineExceeded {
		t.Fatalf("Restart = %v, want the deadline", err)
	}
}
