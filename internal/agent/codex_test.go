package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCodexPeerRowShell runs the ACTUAL generated codex_peer_row (from ShellPrelude) against a
// real-shaped `codex exec --json` turn.completed event and asserts it appends one peer-usage row:
// input_tokens as "in", output_tokens+reasoning_output_tokens as "out". With no COOP_RUN_ID (not a
// loop) it must write nothing. Skipped where jq is absent (the box always has it).
func TestCodexPeerRowShell(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	// The turn.completed shape verified against a live `codex exec --json` run (2026-07-13).
	stream := `{"type":"turn.completed","usage":{"input_tokens":15880,"cached_input_tokens":9984,"output_tokens":5,"reasoning_output_tokens":0}}`
	script := codexAgent{}.ShellPrelude() + "\nprintf '%s\\n' \"$1\" | codex_peer_row thinker gpt-5.6-terra\n"

	run := func(runID string) string {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, ".agent", "runs"), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("sh", "-c", script, "sh", stream)
		cmd.Dir = dir
		cmd.Env = os.Environ()
		if runID != "" {
			cmd.Env = append(cmd.Env, "COOP_RUN_ID="+runID)
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("shell failed: %v\n%s", err, out)
		}
		b, _ := os.ReadFile(filepath.Join(dir, ".agent", "runs", runID+".peers.jsonl"))
		return strings.TrimSpace(string(b))
	}

	row := run("r1")
	for _, want := range []string{`"run":"r1"`, `"role":"thinker"`, `"provider":"codex"`, `"model":"gpt-5.6-terra"`, `"in":15880`, `"out":5`} {
		if !strings.Contains(row, want) {
			t.Errorf("peer row missing %q: %q", want, row)
		}
	}
	if row := run(""); row != "" {
		t.Errorf("no COOP_RUN_ID must write no row, got %q", row)
	}
}
