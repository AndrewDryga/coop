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
	// state glyphs: running → first orbit frame, all-done → ✓ done, empty queue → (no queue), idle → ◦.
	if !strings.Contains(joined, "⠉") {
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

	// done/total counts left-align in one column — they START one space after the bar (the bars are
	// fixed-width, so the start column is shared by every per-fork row AND the global roll-up),
	// instead of sitting behind a wide fixed gap. Take each count token's left edge (rune column).
	countRe := regexp.MustCompile(`[0-9]+/[0-9]+`)
	countStart := func(line string) int {
		if m := countRe.FindStringIndex(line); m != nil {
			return len([]rune(line[:m[0]]))
		}
		return -1
	}
	starts := map[int]bool{}
	for _, l := range out {
		if s := countStart(l); s >= 0 {
			starts[s] = true
		}
	}
	if len(starts) != 1 {
		t.Errorf("done/total counts not left-aligned to one column: starts=%v\n%s", starts, joined)
	}
	// and that column is exactly one space past the bar's ] — no wide gap.
	if s, b := countStart(forkRow), barEnd(forkRow); s != b+2 {
		t.Errorf("count should sit one space after the bar (] at col %d, count at col %d)\n%q", b, s, forkRow)
	}
}

// When nothing is running, the roll-up bar must not animate a spinner — the spinner implies
// motion, so a still fleet leads with the idle ‖ (or ✓ when all done), matching the per-fork rows.
// Per-fork cost shows on its row; the fleet total sums into the roll-up bar. A fork with no cost
// shows no dollar cell.
func TestFleetDashboardCost(t *testing.T) {
	rows := []fleetRow{
		{name: "api", agent: "claude", running: true, counts: taskCounts{Done: 2, Todo: 1}, cost: 12.50},
		{name: "deps", agent: "codex", running: false, counts: taskCounts{Done: 3}, cost: 4.00},
		{name: "web", agent: "gemini", running: false, counts: taskCounts{Todo: 5}}, // no cost → no $ cell
	}
	joined := strings.Join(fleetDashboard("r", rows, 0), "\n")
	for _, want := range []string{"$12.50", "$4.00", "$16.50"} { // two per-fork rows + the fleet total
		if !strings.Contains(joined, want) {
			t.Errorf("cost dashboard missing %q\n%s", want, joined)
		}
	}
}

func TestFleetDashboardIdleBarNoSpinner(t *testing.T) {
	const spin0 = "⠉" // ui.SpinFrames[0] — what a running bar shows at spin=0
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

// A fork whose loop exited with work left (ran, not running, not done) reads as "stopped" — not as
// if it's still on its next task — so a fork that quit at 0/20 isn't mistaken for paused/working.
func TestFleetRowStopped(t *testing.T) {
	stopped := fleetRowLine(fleetRow{name: "codex3", agent: "codex", running: false, ran: true, counts: taskCounts{Todo: 20}, active: "Task 1"}, 0, 5)
	if !strings.Contains(stopped, "stopped") {
		t.Errorf("a stopped fork should say stopped:\n%q", stopped)
	}
	if strings.Contains(stopped, "Task 1") {
		t.Errorf("a stopped fork should not show its next task as if active:\n%q", stopped)
	}
	// A fork that never started (no log → ran=false) still shows its pending task, as before.
	idle := fleetRowLine(fleetRow{name: "pending", agent: "gemini", running: false, ran: false, counts: taskCounts{Todo: 20}, active: "Task 1"}, 0, 5)
	if strings.Contains(idle, "stopped") || !strings.Contains(idle, "Task 1") {
		t.Errorf("an idle, never-started fork should show its pending task, not 'stopped':\n%q", idle)
	}
	// A done fork is still ✓ done, never "stopped", even though it isn't running.
	doneRow := fleetRowLine(fleetRow{name: "claude1", agent: "claude", running: false, ran: true, counts: taskCounts{Done: 20}, active: ""}, 0, 5)
	if strings.Contains(doneRow, "stopped") || !strings.Contains(doneRow, "✓ done") {
		t.Errorf("a finished fork should be ✓ done, not stopped:\n%q", doneRow)
	}
}

// A fork that is unfinished but has no actionable task left (the remainder is all in blocked/) must
// read as "blocked", never "✓ done" — taskTreeCounts returns active=="" for an all-blocked queue
// exactly as it does for an all-done one, so the row can't use that as the done signal. Regression
// for the watch flashing "✓ done" at e.g. 2/5 with 3 blocked (even while still running).
func TestFleetRowBlockedNotDone(t *testing.T) {
	cases := []struct {
		desc string
		row  fleetRow
	}{
		{"running, only blocked left", fleetRow{name: "a", agent: "claude", running: true, counts: taskCounts{Done: 2, Blocked: 3}, active: ""}},
		{"stopped with blocked left", fleetRow{name: "b", agent: "codex", running: false, ran: true, counts: taskCounts{Done: 2, Blocked: 3}, active: ""}},
		{"never-ran, all blocked", fleetRow{name: "c", agent: "gemini", running: false, ran: false, counts: taskCounts{Blocked: 5}, active: ""}},
	}
	for _, c := range cases {
		got := fleetRowLine(c.row, 0, 5)
		if strings.Contains(got, "✓ done") {
			t.Errorf("%s: a fork at %d/%d must not show ✓ done:\n%q", c.desc, c.row.counts.Done, c.row.counts.total(), got)
		}
		if !strings.Contains(got, "blocked") && !strings.Contains(got, "stopped") {
			t.Errorf("%s: an unfinished, non-actionable fork should read blocked/stopped:\n%q", c.desc, got)
		}
	}
	// Only a fork where every task is [x] is "done".
	if got := fleetRowLine(fleetRow{name: "d", running: false, ran: true, counts: taskCounts{Done: 5}}, 0, 5); !strings.Contains(got, "✓ done") {
		t.Errorf("a fully-done fork should show ✓ done:\n%q", got)
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
	// A wide (e.g. CJK) initial would render 2 cells and break the row's alignment → fall back to "?".
	if got := agentBadge("日本語"); got != "?" {
		t.Errorf("agentBadge(wide) = %q, want %q (a 2-cell glyph must not land in the 1-cell column)", got, "?")
	}
}

// keepLastGood rides out a torn read of a fork's task tree: an empty fresh read keeps the prior
// counts (a queue doesn't vanish), but a real read — even a fresh fork going to zero from nothing —
// passes through, and non-tree fields (running/lastLog) always stay fresh.
func TestKeepLastGood(t *testing.T) {
	prev := fleetRow{name: "a", counts: taskCounts{Done: 4, Todo: 6}, active: "task X", running: true}
	// Torn read: fresh has no tasks but prev did → keep prev's counts/active, take fresh running.
	torn := keepLastGood(fleetRow{name: "a", counts: taskCounts{}, active: "", running: false, lastLog: "new"}, prev)
	if torn.counts != prev.counts || torn.active != prev.active {
		t.Errorf("torn read should keep last-good counts/active: got %+v", torn)
	}
	if torn.running || torn.lastLog != "new" {
		t.Errorf("torn read should still take fresh running/lastLog: got running=%v lastLog=%q", torn.running, torn.lastLog)
	}
	// Real update: fresh has tasks → it wins.
	upd := keepLastGood(fleetRow{name: "a", counts: taskCounts{Done: 5, Todo: 5}, active: "task Y"}, prev)
	if upd.counts.Done != 5 || upd.active != "task Y" {
		t.Errorf("a real read should win: got %+v", upd)
	}
	// No prior (first tick) and an empty fork → stays empty, no panic.
	if first := keepLastGood(fleetRow{name: "b"}, fleetRow{}); first.counts.total() != 0 {
		t.Errorf("first-tick empty fork should stay empty: got %+v", first)
	}
}

// A fork whose loop is alive but hasn't finished copying its --tasks slice yet (0 tasks, running)
// reads as "starting", not "(no queue)" — that empty state is transient seeding, not a real absence
// of work. A non-running fork with no queue is still "(no queue)".
func TestFleetRowStarting(t *testing.T) {
	starting := fleetRowLine(fleetRow{name: "api", agent: "claude", running: true, counts: taskCounts{}}, 0, 5)
	if !strings.Contains(starting, "starting") {
		t.Errorf("a running fork still seeding its queue should read 'starting':\n%q", starting)
	}
	if strings.Contains(starting, "(no queue)") {
		t.Errorf("a seeding fork must not read '(no queue)':\n%q", starting)
	}
	noQueue := fleetRowLine(fleetRow{name: "shell", agent: "codex", running: false, counts: taskCounts{}}, 0, 5)
	if !strings.Contains(noQueue, "(no queue)") {
		t.Errorf("a non-running fork with no queue should read '(no queue)':\n%q", noQueue)
	}
}

// fleetSettled is the startup-safe auto-exit predicate: true only when every fork has finished and
// nothing is left to start (so watch can exit even when launched on an already-done fleet).
func TestFleetSettled(t *testing.T) {
	// Every fork seeded a queue and none is running → finished.
	if !fleetSettled([]fleetRow{
		{name: "a", running: false, ran: true, counts: taskCounts{Done: 5}},
		{name: "b", running: false, ran: true, counts: taskCounts{Done: 2, Blocked: 1}},
	}) {
		t.Error("all forks done/blocked and idle → settled")
	}
	// One still running → not settled.
	if fleetSettled([]fleetRow{{name: "a", running: true, counts: taskCounts{Todo: 3}}}) {
		t.Error("a running fork → not settled")
	}
	// A fork that hasn't seeded a queue and never ran (could be starting) → not settled.
	if fleetSettled([]fleetRow{
		{name: "a", running: false, ran: true, counts: taskCounts{Done: 5}},
		{name: "b", running: false, ran: false, counts: taskCounts{}},
	}) {
		t.Error("an unseeded, never-ran fork blocks the 'finished' conclusion")
	}
	// A seeded-empty fork that ran and exited (0 tasks, ran) is finished, not blocking.
	if !fleetSettled([]fleetRow{{name: "a", running: false, ran: true, counts: taskCounts{}}}) {
		t.Error("a fork that ran an empty queue and exited → settled")
	}
	// No forks → not settled (nothing to conclude).
	if fleetSettled(nil) {
		t.Error("empty fleet → not settled")
	}
}
