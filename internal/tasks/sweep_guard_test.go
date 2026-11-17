package tasks

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

// `coop tasks queues --held` names the in-progress tasks the sweep cannot finish or block: a fork's
// assignment, a held lease, and another live agent's claim. The sweeping agent's own claim, a
// stopped claimant's, and an unowned task stay the sweep's.
func TestHeldTaskFoldersApplyClaimsOwnershipRules(t *testing.T) {
	_, root, assignment := proposalAssignment(t, "held")
	forkTask, ok := mustCurrentTask(t, root, assignment.Task.Item.ID)
	if !ok || forkTask.State != StateInProgress {
		t.Fatalf("fork-assigned task = %+v, %v; want in progress", forkTask, ok)
	}
	leased := taskForLease(t, root, StateInProgress, "leased")
	lease, observed, err := TryTaskLease(root, leased, testLeaseOwner())
	if err != nil || lease == nil {
		t.Fatalf("lease = %+v, %+v, %v", lease, observed, err)
	}
	defer lease.Release()
	claim := func(id string, actor ClaimActor) Item {
		t.Helper()
		writeTaskFile(t, filepath.Join(root, StateTodo, id, "task.md"), "# "+id+"\n\n- [ ] one step\n")
		if code, err := tasksFolderMoveWith(root, []string{id}, StateInProgress, "claim", claimOptions{actor: actor}); code != 0 || err != nil {
			t.Fatalf("claim %s = %d, %v", id, code, err)
		}
		item, _ := mustCurrentTask(t, root, id)
		return item
	}
	other, _ := sleeperActor(t, "codex")
	mine, _ := sleeperActor(t, "claude")
	gone, stop := sleeperActor(t, "gemini")
	foreign := claim("foreign-claim", other)
	claim("own-claim", mine)
	claim("stopped-claim", gone)
	stop()
	taskForLease(t, root, StateInProgress, "free")

	got, err := heldTaskFolders([]string{root}, []int{mine.PID})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{forkTask.Dir, foreign.Dir, leased.Dir}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("held = %q\nwant %q", got, want)
	}
}

// The /sweep Stop hook reads held tasks from `coop tasks queues --held`: a task another live agent
// holds is not the sweep's to finish or block. The hook runs here against what this code prints, so
// a change on either side fails this test instead of quietly making the hook count every held task.
func TestSweepQueueGuardReadsHeldTasks(t *testing.T) {
	guard, err := filepath.Abs(filepath.Join("..", "..", ".agent", "skills", "sweep", "queue-guard.sh"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateTodo, "held", "task.md"), "# Held\n\n- [ ] one step\n")
	other, stop := sleeperActor(t, "claude")
	defer stop()
	if code, err := tasksFolderMoveWith(root, []string{"held"}, StateInProgress, "claim", claimOptions{actor: other}); code != 0 || err != nil {
		t.Fatalf("claim = %d, %v", code, err)
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash unavailable") // the unit tests' minimal PATH has none; the guard is a bash script
	}
	bin, project := t.TempDir(), t.TempDir()
	listing := filepath.Join(bin, "held.txt")
	fake := "#!/bin/bash\ncase \"$1 $2 $3\" in\n\"tasks queues \") printf '%s\\n' \"$GUARD_QUEUE\" ;;\n\"tasks queues --held\") cat \"$GUARD_HELD\" ;;\n*) exit 1 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "coop"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	hook := func() (int, string) {
		t.Helper()
		held, err := heldTaskFolders([]string{root}, nil)
		if err != nil {
			t.Fatal(err)
		}
		var body []byte
		for _, dir := range held {
			body = append(body, dir+"\n"...)
		}
		if err := os.WriteFile(listing, body, 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("bash", guard)
		cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
			"CLAUDE_PROJECT_DIR="+project, "GUARD_QUEUE="+root, "GUARD_HELD="+listing)
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode(), string(out)
		}
		if err != nil {
			t.Fatal(err)
		}
		return 0, string(out)
	}
	if code, out := hook(); code != 0 {
		t.Fatalf("a task a live foreign agent holds held the sweep: exit %d\n%s", code, out)
	}
	stop() // the claimant exits, so its claim protects nothing
	if code, out := hook(); code != 2 {
		t.Fatalf("a task whose claimant has stopped let the sweep stop: exit %d\n%s", code, out)
	}
}
