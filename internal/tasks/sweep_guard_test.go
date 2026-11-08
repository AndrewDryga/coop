package tasks

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The /sweep Stop hook reads claims from this listing: a task another live agent holds is not the
// sweep's to finish or block. The hook runs here against the listing this code prints, so a layout
// change fails this test instead of quietly making the hook count every claimed task.
func TestSweepQueueGuardReadsClaimsFromTheListing(t *testing.T) {
	guard, err := filepath.Abs(filepath.Join("..", "..", ".agent", "skills", "sweep", "queue-guard.sh"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateTodo, "held", "task.md"), "# Held\n\n- [ ] one step\n")
	other, stop := sleeperActor(t, "claude")
	defer stop()
	if code, err := tasksFolderMoveWith(root, []string{"held"}, StateInProgress, "claim", "claimed", claimOptions{actor: other}); code != 0 || err != nil {
		t.Fatalf("claim = %d, %v", code, err)
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash unavailable") // the unit tests' minimal PATH has none; the guard is a bash script
	}
	bin, project := t.TempDir(), t.TempDir()
	listing := filepath.Join(bin, "listing.txt")
	fake := "#!/bin/bash\ncase \"$1 $2\" in\n\"tasks queues\") printf '%s\\n' \"$GUARD_QUEUE\" ;;\n\"tasks ls\") cat \"$GUARD_LISTING\" ;;\n*) exit 1 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "coop"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	hook := func() (int, string) {
		t.Helper()
		if err := os.WriteFile(listing, []byte(captureStdout(t, func() { _, _ = tasksFolderList(root, false) })), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("bash", guard)
		cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
			"CLAUDE_PROJECT_DIR="+project, "GUARD_QUEUE="+root, "GUARD_LISTING="+listing)
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
