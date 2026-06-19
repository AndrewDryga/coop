package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestFleetDashboard(t *testing.T) {
	rows := []fleetRow{
		{name: "api", agent: "claude", running: true, counts: taskCounts{Done: 4, Todo: 6}, active: "refresh-token rotation", lastLog: "⚙ Bash go test"},
		{name: "deps", agent: "codex", running: false, counts: taskCounts{Done: 6, Todo: 3, Blocked: 1}, active: "bump axios"},
		{name: "web", agent: "gemini", running: false, counts: taskCounts{Done: 1, Todo: 11}, active: "fix hydration"},
		{name: "infra", agent: "claude", running: false, counts: taskCounts{Done: 8}, active: ""}, // all done
		{name: "fresh", running: false, counts: taskCounts{}, active: ""},                         // no queue
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
	// state glyphs: running → spinner frame "⠋", all-done → ✓ done, empty queue → (no queue), idle → ◦.
	if !strings.Contains(joined, "⠋") {
		t.Errorf("running fork should show a spinner:\n%s", joined)
	}
	if !strings.Contains(joined, "✓ done") {
		t.Errorf("all-done fork should show ✓ done:\n%s", joined)
	}
	if !strings.Contains(joined, "(no queue)") {
		t.Errorf("empty fork should show (no queue):\n%s", joined)
	}
	if !strings.Contains(joined, "‖") {
		t.Errorf("idle fork should show the ‖ pause glyph:\n%s", joined)
	}

	// The bottom roll-up bar's right edge lines up with the per-fork bars' right edge. Colors
	// are off under `go test`, so columns are plain runes — compare the ] of each bar.
	barEnd := func(line string) int {
		for i, r := range []rune(line) {
			if r == ']' {
				return i
			}
		}
		return -1
	}
	forkRow, global := out[2], out[len(out)-1] // out = [header, "", rows…, "", bar]
	if a, b := barEnd(forkRow), barEnd(global); a < 0 || a != b {
		t.Errorf("bar right-edges misaligned: fork ] at col %d, global ] at col %d\n%q\n%q", a, b, forkRow, global)
	}

	// done/total counts right-align in one column — the per-fork rows AND the global roll-up.
	// Take each count token's right edge (rune column); require a single shared column.
	countRe := regexp.MustCompile(`[0-9]+/[0-9]+`)
	countEnd := func(line string) int {
		if m := countRe.FindStringIndex(line); m != nil {
			return len([]rune(line[:m[1]]))
		}
		return -1
	}
	ends := map[int]bool{}
	for _, l := range out {
		if e := countEnd(l); e >= 0 {
			ends[e] = true
		}
	}
	if len(ends) != 1 {
		t.Errorf("done/total counts not right-aligned to one column: ends=%v\n%s", ends, joined)
	}
}

// When nothing is running, the roll-up bar must not animate a spinner — the spinner implies
// motion, so a still fleet leads with the idle ‖ (or ✓ when all done), matching the per-fork rows.
func TestFleetDashboardIdleBarNoSpinner(t *testing.T) {
	const spin0 = "⠋" // ui.SpinFrames[0] — what a running bar shows at spin=0
	bar := func(rows []fleetRow) string {
		out := fleetDashboard("repo", rows, 0)
		return out[len(out)-1] // out = [header, "", rows…, "", bar]
	}

	// Idle with work left → ‖, no spinner (the reported case: 0 running, tasks remaining).
	idle := bar([]fleetRow{
		{name: "a", agent: "claude", running: false, counts: taskCounts{Done: 1, Todo: 1}},
		{name: "b", agent: "gemini", running: false, counts: taskCounts{Todo: 5}},
	})
	if strings.Contains(idle, spin0) || !strings.HasPrefix(idle, "‖") {
		t.Errorf("idle fleet bar should lead with ‖ and never spin:\n%q", idle)
	}

	// Everything done → ✓, still no spinner.
	allDone := bar([]fleetRow{{name: "a", running: false, counts: taskCounts{Done: 3}}})
	if strings.Contains(allDone, spin0) || !strings.HasPrefix(allDone, "✓") {
		t.Errorf("all-done fleet bar should lead with ✓ and never spin:\n%q", allDone)
	}

	// At least one running → the spinner is back; suppression is only for a still fleet.
	if busy := bar([]fleetRow{{name: "a", running: true, counts: taskCounts{Todo: 2}}}); !strings.Contains(busy, spin0) {
		t.Errorf("a running fleet bar should spin:\n%q", busy)
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
	// coop's own banner lines are skipped — they'd just echo what the bar/task name already show.
	if got := lastLogLine(write("agent did a thing\ncoop: iteration 1 · 0/20 done · now: Task 1\ncoop: shadowed 4 secret path(s)\n")); got != "agent did a thing" {
		t.Errorf("lastLogLine should skip coop: lines, got %q", got)
	}
	// Missing or empty logs are empty, not an error.
	if got := lastLogLine(filepath.Join(t.TempDir(), "nope.log")); got != "" {
		t.Errorf("missing log = %q, want empty", got)
	}
	if got := lastLogLine(write("\n\n")); got != "" {
		t.Errorf("empty log = %q, want empty", got)
	}
}

func TestFleetOrphans(t *testing.T) {
	// Forks not named in the fleet are the prune candidates, in forkNames order.
	if got := fleetOrphans([]string{"api", "deps"}, []string{"api", "deps", "old1", "old2"}); len(got) != 2 || got[0] != "old1" || got[1] != "old2" {
		t.Errorf("orphans = %v, want [old1 old2]", got)
	}
	// Everything is in the fleet → nothing to prune.
	if got := fleetOrphans([]string{"api", "deps"}, []string{"api", "deps"}); len(got) != 0 {
		t.Errorf("orphans = %v, want none", got)
	}
	// An empty fleet → every fork is an orphan.
	if got := fleetOrphans(nil, []string{"a", "b"}); len(got) != 2 {
		t.Errorf("orphans = %v, want [a b]", got)
	}
}

func TestAgentBadge(t *testing.T) {
	// Colors are off under `go test`, so the badge is just its letter.
	for agent, want := range map[string]string{"claude": "c", "codex": "x", "gemini": "g", "": "?"} {
		if got := agentBadge(agent); got != want {
			t.Errorf("agentBadge(%q) = %q, want %q", agent, got, want)
		}
	}
	if got := agentBadge("mistral"); got != "m" { // unknown agent → its first letter
		t.Errorf("agentBadge(mistral) = %q, want %q", got, "m")
	}
}
