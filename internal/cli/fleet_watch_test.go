package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFleetDashboard(t *testing.T) {
	rows := []fleetRow{
		{name: "api", running: true, counts: taskCounts{Done: 4, Todo: 6}, active: "refresh-token rotation", lastLog: "⚙ Bash go test"},
		{name: "deps", running: false, counts: taskCounts{Done: 6, Todo: 3, Blocked: 1}, active: "bump axios"},
		{name: "web", running: false, counts: taskCounts{Done: 1, Todo: 11}, active: "fix hydration"},
		{name: "infra", running: false, counts: taskCounts{Done: 8}, active: ""}, // all done
		{name: "fresh", running: false, counts: taskCounts{}, active: ""},        // no queue
	}
	out := fleetDashboard("myrepo", rows, 0)
	joined := strings.Join(out, "\n")

	if !strings.Contains(out[0], "myrepo fleet") || !strings.Contains(out[0], "1 running") || !strings.Contains(out[0], "1 blocked") {
		t.Errorf("header wrong: %q", out[0])
	}
	// every fork, the rolled-up totals (done 4+6+1+8 = 19; total 10+10+12+8 = 40), and counts.
	for _, want := range []string{"api", "deps", "web", "infra", "fresh", "19/40 tasks", "refresh-token rotation"} {
		if !strings.Contains(joined, want) {
			t.Errorf("dashboard missing %q\n%s", want, joined)
		}
	}
	// state glyphs: running → spinner frame "⠋", all-done → ✓ done, empty queue → (no queue), idle → ⏸.
	if !strings.Contains(joined, "⠋") {
		t.Errorf("running fork should show a spinner:\n%s", joined)
	}
	if !strings.Contains(joined, "✓ done") {
		t.Errorf("all-done fork should show ✓ done:\n%s", joined)
	}
	if !strings.Contains(joined, "(no queue)") {
		t.Errorf("empty fork should show (no queue):\n%s", joined)
	}
	if !strings.Contains(joined, "⏸") {
		t.Errorf("idle fork should show ⏸:\n%s", joined)
	}
}

func TestLastLogLine(t *testing.T) {
	write := func(body string) string {
		p := filepath.Join(t.TempDir(), "f.log")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// The last non-empty line, ignoring trailing blank/whitespace lines.
	if got := lastLogLine(write("first line\nsecond line\n\n  \n")); got != "second line" {
		t.Errorf("lastLogLine = %q, want %q", got, "second line")
	}
	// Missing or empty logs are empty, not an error.
	if got := lastLogLine(filepath.Join(t.TempDir(), "nope.log")); got != "" {
		t.Errorf("missing log = %q, want empty", got)
	}
	if got := lastLogLine(write("\n\n")); got != "" {
		t.Errorf("empty log = %q, want empty", got)
	}
}
