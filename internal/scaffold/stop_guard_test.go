package scaffold

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestStopGuardSessionScope runs the embedded stop-guard hook through the session-scoping
// contract: /sweep writes its session id into .agent/active, and the guard blocks only its
// OWN session while 00_todo has work — a concurrent (different-id) session, a stop-loop, or
// a stale/legacy marker with no id must not be held by someone else's sweep.
func TestStopGuardSessionScope(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	hookBytes, err := templates.ReadFile("templates/claude/hooks/stop-guard.sh")
	if err != nil {
		t.Fatal(err)
	}
	proj := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(proj, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("stop-guard.sh", string(hookBytes))
	write(filepath.Join(".agent", "tasks", "00_todo", "2026-01-01-x", "task.md"), "id: x\n")
	// A `coop` stub answering `tasks queues` with the queue's absolute path, so the guard's
	// count works regardless of the hook's cwd.
	queueAbs := filepath.Join(proj, ".agent", "tasks")
	write(filepath.Join("bin", "coop"), "#!/bin/bash\n[ \"$1\" = tasks ] && [ \"$2\" = queues ] && echo \""+queueAbs+"\"\n")

	hook := filepath.Join(proj, "stop-guard.sh")
	bin := filepath.Join(proj, "bin")
	run := func(marker, payload string) int {
		t.Helper()
		if marker == "\x00" {
			os.Remove(filepath.Join(proj, ".agent", "active"))
		} else {
			write(filepath.Join(".agent", "active"), marker)
		}
		cmd := exec.Command("bash", hook)
		cmd.Env = append(os.Environ(), "CLAUDE_PROJECT_DIR="+proj, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		cmd.Stdin = strings.NewReader(payload)
		if err := cmd.Run(); err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				return ee.ExitCode()
			}
			t.Fatalf("hook run: %v", err)
		}
		return 0
	}

	// The sweep's OWN session is held while work remains — the guarantee must not weaken.
	if code := run("sweep-1", `{"session_id":"sweep-1"}`); code != 2 {
		t.Errorf("same-session with work: exit %d, want 2 (held)", code)
	}
	// A concurrent, different-id session is released.
	if code := run("sweep-1", `{"session_id":"other-2"}`); code != 0 {
		t.Errorf("different-session: exit %d, want 0 (released)", code)
	}
	// A stop-loop (stop_hook_active) is released even for the arming session, so it can't wedge.
	if code := run("sweep-1", `{"session_id":"sweep-1","stop_hook_active":true}`); code != 0 {
		t.Errorf("stop_hook_active: exit %d, want 0 (let go)", code)
	}
	// A legacy `touch` marker (empty id) falls through to the count — held while work remains,
	// so the fix degrades safely to today's behavior when no id is armed.
	if code := run("\n", `{"session_id":"anyone"}`); code != 2 {
		t.Errorf("legacy empty marker with work: exit %d, want 2 (safe fallback to block)", code)
	}
	// No marker at all: not armed, so never held.
	if code := run("\x00", `{"session_id":"sweep-1"}`); code != 0 {
		t.Errorf("no marker: exit %d, want 0 (not armed)", code)
	}
}
