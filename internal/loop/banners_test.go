package loop

import (
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/tasks"
)

// The loop's closing banner must not claim "verified done" when the signoff reopened work — which it
// does by moving done tasks back into 10_in_progress/, not 00_todo/. Regression: the check looked at
// 00_todo/ only, so a reopened task in in_progress fell through to the green "verified done".
func TestLoopClosingBanner(t *testing.T) {
	// Reopened INTO in_progress (the bug): not done, and names the count.
	if b := loopClosingBanner(tasks.TaskCounts{Done: 2, Doing: 3}, 5); !strings.Contains(b, "review left") ||
		!strings.Contains(b, "3 tasks") || strings.Contains(b, "verified done") {
		t.Errorf("reopened-into-in_progress banner = %q", b)
	}
	// Reopened into todo: same outcome, singular count.
	if b := loopClosingBanner(tasks.TaskCounts{Done: 4, Todo: 1}, 4); !strings.Contains(b, "review left") ||
		!strings.Contains(b, "1 task") || strings.Contains(b, "verified done") {
		t.Errorf("reopened-into-todo banner = %q", b)
	}
	// Nothing reopened, some blocked on a decision: not done.
	if b := loopClosingBanner(tasks.TaskCounts{Done: 3, Blocked: 2}, 3); !strings.Contains(b, "blocked on a decision") ||
		strings.Contains(b, "verified done") {
		t.Errorf("blocked banner = %q", b)
	}
	// Clean audit: verified done, unchanged.
	if b := loopClosingBanner(tasks.TaskCounts{Done: 5}, 5); !strings.Contains(b, "queue verified done") ||
		!strings.Contains(b, "5/5") {
		t.Errorf("clean banner = %q", b)
	}
}

// The loop's exit code lets cron/fleet/CI branch without parsing stderr: a review-reopened queue is
// a failure, 3 means only human-blocked work remains, and 0 means verified done.
func TestLoopExitCode(t *testing.T) {
	cases := []struct {
		cf   tasks.TaskCounts
		want int
	}{
		{tasks.TaskCounts{Done: 3, Blocked: 2}, 3}, // blocked, nothing actionable → 3
		{tasks.TaskCounts{Done: 5}, 0},             // verified done → 0
		{tasks.TaskCounts{Done: 3, Doing: 1}, 1},   // audit reopened into in_progress → unverified
		{tasks.TaskCounts{Todo: 2, Blocked: 1}, 1}, // actionable work takes precedence over blocked
	}
	for _, c := range cases {
		if got := loopExitCode(c.cf); got != c.want {
			t.Errorf("loopExitCode(%+v) = %d, want %d", c.cf, got, c.want)
		}
	}
}

func TestLoopIntentionalAndInterruptedStopsAreDistinct(t *testing.T) {
	if LoopInterruptedExitCode != 130 {
		t.Fatalf("loop interrupt exit = %d, want conventional SIGINT status 130", LoopInterruptedExitCode)
	}
	cf := tasks.TaskCounts{Done: 3, Todo: 2}
	if got := loopInterruptedBanner(cf); !strings.Contains(got, "interrupted before queue verification") || !strings.Contains(got, "3/5 done") {
		t.Errorf("interrupt banner = %q", got)
	}
	limit := loopTaskLimit{max: 1, settled: 1, lastID: "task-a", lastState: stateDone}
	if got := loopTaskLimitBanner(cf, limit); !strings.Contains(got, "task limit reached") ||
		!strings.Contains(got, "last: task-a done") || !strings.Contains(got, "paused before another task or final signoff") || strings.Contains(got, "verified done") {
		t.Errorf("task-limit banner = %q", got)
	}
	if got := loopTaskLimitBanner(tasks.TaskCounts{Blocked: 2}, loopTaskLimit{max: 3}); !strings.Contains(got, "no actionable task") ||
		!strings.Contains(got, "no box started") || !strings.Contains(got, "2 blocked") {
		t.Errorf("task-limit idle banner = %q", got)
	}
	partial := loopTaskLimit{max: 3, settled: 1, lastID: "task-a", lastState: stateDone}
	if got := loopTaskLimitBanner(tasks.TaskCounts{Done: 1}, partial); !strings.Contains(got, "1/3 tasks settled") ||
		!strings.Contains(got, "no actionable task remains") || !strings.Contains(got, "final signoff not run") {
		t.Errorf("partial task-limit banner = %q", got)
	}
	blocked := loopTaskLimit{max: 2, settled: 2, lastID: "task-b", lastState: stateBlocked}
	if got := loopTaskLimitBanner(tasks.TaskCounts{Done: 1, Blocked: 1}, blocked); !strings.Contains(got, "task limit reached") ||
		!strings.Contains(got, "last: task-b blocked") || !strings.Contains(got, "■") {
		t.Errorf("blocked task-limit banner = %q", got)
	}
}

func TestProgressBanner(t *testing.T) {
	// Colors are off when stderr isn't a tty (as under `go test`), so the banner renders
	// plain — assert the structure.
	if got := progressBanner(3, tasks.TaskCounts{Todo: 9, Doing: 1, Done: 4}, "Wire up the portal auth callback"); got != "iteration 3 · 4/14 done · now: Wire up the portal auth callback" {
		t.Errorf("banner = %q", got)
	}
	// Blocked is shown only when nonzero.
	if got := progressBanner(1, tasks.TaskCounts{Done: 2, Blocked: 1, Todo: 1}, ""); got != "iteration 1 · 2/4 done · 1 blocked" {
		t.Errorf("blocked banner = %q", got)
	}
	// No active task → no "now:" clause; no blocked → no blocked clause.
	if got := progressBanner(2, tasks.TaskCounts{Done: 5}, ""); got != "iteration 2 · 5/5 done" {
		t.Errorf("plain banner = %q", got)
	}
	// A long title is truncated, not printed whole.
	long := strings.Repeat("x", 80)
	if got := progressBanner(1, tasks.TaskCounts{Todo: 1}, long); !strings.Contains(got, "…") || strings.Contains(got, long) {
		t.Errorf("long title not truncated: %q", got)
	}
}

func TestProgressBannerWidthUsesAvailableTerminalWidth(t *testing.T) {
	activity := "Reconcile every provider credential rotation before the deployment cutover"
	wide := progressBannerWidth(3, tasks.TaskCounts{Doing: 1}, activity, 120)
	if !strings.Contains(wide, activity) {
		t.Errorf("wide progress banner should show activity past the old fixed cap: %q", wide)
	}

	const narrowWidth = 44
	narrow := progressBannerWidth(3, tasks.TaskCounts{Doing: 1}, activity, narrowWidth)
	if strings.Contains(narrow, activity) || !strings.Contains(narrow, "…") {
		t.Errorf("narrow progress banner should elide activity: %q", narrow)
	}
	if got := len([]rune(narrow)); got > narrowWidth {
		t.Errorf("narrow progress banner width = %d, want at most %d: %q", got, narrowWidth, narrow)
	}
}

func TestProgressLine(t *testing.T) {
	// The mid-iteration line the monitor prints live: done/total, blocked only when there
	// is some, and the active task — no "iteration N" prefix.
	if got := progressLine(tasks.TaskCounts{Done: 8, Blocked: 1, Todo: 11}, "Task 9"); got != "8/20 done · 1 blocked · now: Task 9" {
		t.Errorf("progressLine = %q", got)
	}
	if got := progressLine(tasks.TaskCounts{Done: 20}, ""); got != "20/20 done" {
		t.Errorf("done-only progressLine = %q", got)
	}
}
