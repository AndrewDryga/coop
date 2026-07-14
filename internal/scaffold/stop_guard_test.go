package scaffold

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestStopGuard runs the embedded hook through its session-scoping and queue-discovery contracts.
func TestStopGuard(t *testing.T) {
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
	write(filepath.Join("service", ".agent", "tasks", "00_todo", "2026-01-02-y", "task.md"), "id: y\n")
	// A `coop` stub answering `tasks queues` with the queue's absolute path, so the guard's
	// host path works regardless of the hook's cwd.
	rootQueue := filepath.Join(proj, ".agent", "tasks")
	serviceQueue := filepath.Join(proj, "service", ".agent", "tasks")
	write(filepath.Join("bin", "coop"), "#!/bin/bash\n[ \"$1\" = tasks ] && [ \"$2\" = queues ] && printf '%s\\n' \""+rootQueue+"\" \""+serviceQueue+"\"\n")

	// Build a PATH with every external command the hook needs but no coop. This models the
	// box without relying on where the host installed its coop binary.
	toolBin := filepath.Join(proj, "tools")
	if err := os.MkdirAll(toolBin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"cat", "find", "grep", "head", "sed", "tr", "wc"} {
		src, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s not available", tool)
		}
		if err := os.Symlink(src, filepath.Join(toolBin, tool)); err != nil {
			t.Fatal(err)
		}
	}

	hook := filepath.Join(proj, "stop-guard.sh")
	bin := filepath.Join(proj, "bin")
	run := func(marker, payload, path string) (int, string) {
		t.Helper()
		if marker == "\x00" {
			os.Remove(filepath.Join(proj, ".agent", "active"))
		} else {
			write(filepath.Join(".agent", "active"), marker)
		}
		cmd := exec.Command("bash", hook)
		cmd.Env = append(os.Environ(), "CLAUDE_PROJECT_DIR="+proj, "PATH="+path)
		cmd.Stdin = strings.NewReader(payload)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				return ee.ExitCode(), stderr.String()
			}
			t.Fatalf("hook run: %v", err)
		}
		return 0, stderr.String()
	}
	hostPath := bin + string(os.PathListSeparator) + toolBin

	// The sweep's OWN session is held while work remains — the guarantee must not weaken.
	if code, _ := run("sweep-1", `{"session_id":"sweep-1"}`, hostPath); code != 2 {
		t.Errorf("same-session with work: exit %d, want 2 (held)", code)
	}
	// A concurrent, different-id session is released.
	if code, _ := run("sweep-1", `{"session_id":"other-2"}`, hostPath); code != 0 {
		t.Errorf("different-session: exit %d, want 0 (released)", code)
	}
	// A stop-loop (stop_hook_active) is released even for the arming session, so it can't wedge.
	if code, _ := run("sweep-1", `{"session_id":"sweep-1","stop_hook_active":true}`, hostPath); code != 0 {
		t.Errorf("stop_hook_active: exit %d, want 0 (let go)", code)
	}
	// A legacy `touch` marker (empty id) falls through to the count — held while work remains,
	// so the fix degrades safely to today's behavior when no id is armed.
	if code, _ := run("\n", `{"session_id":"anyone"}`, hostPath); code != 2 {
		t.Errorf("legacy empty marker with work: exit %d, want 2 (safe fallback to block)", code)
	}
	// No marker at all: not armed, so never held.
	if code, _ := run("\x00", `{"session_id":"sweep-1"}`, hostPath); code != 0 {
		t.Errorf("no marker: exit %d, want 0 (not armed)", code)
	}

	// Inside a box there is no coop binary. The find fallback must discover every queue in
	// the mounted repo, hold the sweep, and report the aggregate count.
	if code, stderr := run("sweep-1", `{"session_id":"sweep-1"}`, toolBin); code != 2 {
		t.Errorf("in-box with work: exit %d, want 2 (held); stderr=%q", code, stderr)
	} else if !strings.Contains(stderr, "2 unclaimed task(s)") {
		t.Errorf("in-box count stderr = %q, want aggregate count", stderr)
	}

	if err := os.RemoveAll(filepath.Join(proj, ".agent", "tasks", "00_todo")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(proj, "service", ".agent", "tasks", "00_todo")); err != nil {
		t.Fatal(err)
	}
	if code, stderr := run("sweep-1", `{"session_id":"sweep-1"}`, toolBin); code != 0 {
		t.Errorf("in-box with empty queues: exit %d, want 0 (released); stderr=%q", code, stderr)
	}
}
