package loop

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

// This file is the loop's VOICE: every line a person reads while an unattended run works the
// queue. The run itself is bookkeeping (leases, completion windows, receipts); what shows up on
// the terminal is a short narration — which task is being worked by which agent, that it
// completed, which agent reviewed it, and how the run ended. Each block is built here so the
// wording lives in one file instead of being scattered through the state machine.

// taskLine is one task as a report names it: the title a human recognizes, the id that identifies
// it, the subproject scope it belongs to (empty at the repo root) and — where the report describes
// what is LEFT to do — the queue state it is actually in. Completions are recorded as they are
// accepted, so the closing report describes work that landed rather than what a commit range
// happens to contain.
type taskLine struct{ id, title, scope, state string }

// queueScope is the subproject a task queue belongs to — "internal/auth" for
// internal/auth/.agent/tasks, "" for the repository's own queue. It is the scope shown beside a
// task id, so a monorepo run says which member the work is in without a separate ledger.
func queueScope(rel string) string {
	dir := filepath.Dir(filepath.Clean(rel)) // …/.agent/tasks -> …/.agent
	dir = filepath.Dir(dir)                  // …/.agent       -> …
	if dir == "." || dir == "/" || dir == "" {
		return "" // the repository's own queue: there is nothing to distinguish it from
	}
	return filepath.ToSlash(dir)
}

// taskIdentity is the "<id> · <scope>" line under a task's title. Scope is omitted when the task
// belongs to the repository's own queue: there is nothing to distinguish it from.
func taskIdentity(id, scope string) string {
	if scope == "" {
		return id
	}
	return id + " · " + scope
}

// introLine opens the run: how much work it starts with and which configuration it derives from.
// n is the startup snapshot of actionable tasks — not a promise that every one finishes here.
func introLine(n int, configured bool) string {
	source := "built-in defaults"
	if configured {
		source = "configuration " + loopConfigName
	}
	return fmt.Sprintf("Working through %s using %s", ui.Count(n, "task"), source)
}

// loopConfigName is the committed loop configuration's repo-relative path, as the intro names it.
const loopConfigName = ".agent/loop.yaml"

// printTaskHeader announces the attempt about to start: its ordinal in this run, the task's title,
// its id and scope, and the agent (or custom command) that will work it. Ordinals are stable per
// task for the whole run, so a retry keeps the number a reader already saw.
func printTaskHeader(ordinal int, task taskLine, runner string, custom bool) {
	label := "Agent:  "
	if custom {
		label = "Command: "
	}
	ui.Note("")
	ui.Note("Task %d · %s", ordinal, task.title)
	ui.Note("  %s", taskIdentity(task.id, task.scope))
	if runner != "" {
		ui.Note("  %s%s", label, runner)
	}
}

// printTaskCompleted marks an accepted completion. It says the task finished, not that it passed
// the final review — that verdict is the run's, at the end.
func printTaskCompleted(title string) {
	ui.Note("")
	ui.OK("Task completed: %s", title)
}

// reviewHeader opens a review stage with the agent that was actually selected for it. Review
// rotations are independent of the work rotation, so the target is read at the stage, never
// assumed to be the worker's.
func reviewHeader(text string, target string) {
	ui.Note("")
	if target == "" {
		ui.Note("%s", text)
		return
	}
	ui.Note("%s · %s", text, target)
}

// printProtectedReview names the gate-defining files a completed task changed and what the review
// is for. It is mandatory, so it is never described as optional.
func printProtectedReview(target string, files []string) {
	reviewHeader("Reviewing changes to project checks", target)
	for _, f := range files {
		ui.Note("  %s", f)
	}
	ui.Note("")
	ui.Note("  Checking that these changes did not weaken the checks.")
}

// humanTokenCount renders a token total the way the closing report quotes it: grouped in
// thousands, exact. A usage number a person compares against an invoice is not rounded.
func humanTokenCount(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// rungPhrase names one rotation rung the way a person says it — "Claude … personal" — splitting
// the provider from its account so a sentence can put its own verb between them. A rung that pins
// a model, an effort, or several accounts is quoted EXACTLY and gets no account clause: two rungs
// of one provider must never read as the same thing.
func rungPhrase(t agents.Target) (subject, account string) {
	if t.Model != "" || t.Effort != "" || len(t.Accounts) != 1 {
		return t.String(), ""
	}
	return titleCase(t.Provider), t.Accounts[0]
}

// limitSentence is the routine rotation notice's first line: this rung is out of usage for now.
// It is not an error — the run continues on the next rung, or waits for the reset.
func limitSentence(t agents.Target) string {
	subject, account := rungPhrase(t)
	if account == "" {
		return subject + " reached its usage limit."
	}
	return subject + " reached its usage limit for " + account + "."
}

// authHeadline is the headline of a failed sign-in, with no trailing period: a headline is a
// label, and the concrete consequence follows it as the block's cause.
func authHeadline(t agents.Target) string {
	subject, account := rungPhrase(t)
	if account == "" {
		return subject + " could not sign in"
	}
	return subject + " could not sign in with " + account
}

// titleCase capitalizes a provider id for prose ("claude" -> "Claude"). Providers are ASCII ids,
// so this is the whole rule.
func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// taskOrdinals numbers the tasks a run works, in the order it first selected them. A retry keeps
// its task's number, and a signoff round that sends work back does not renumber it: the ordinal
// answers "which task is this" for a person reading a long log, not "how many boxes have run".
type taskOrdinals struct {
	byID     map[string]int
	attempts map[string]int
	next     int
}

func newTaskOrdinals() *taskOrdinals { return &taskOrdinals{byID: map[string]int{}} }

// attempt counts, and returns, how many times this run has launched a box for one task — the
// number a refusal or a retry names, so "task attempt 2" always means the second try at THAT task.
func (o *taskOrdinals) attempt(id string) int {
	if o.attempts == nil {
		o.attempts = map[string]int{}
	}
	o.attempts[id]++
	return o.attempts[id]
}

func (o *taskOrdinals) of(id string) int {
	if n, seen := o.byID[id]; seen {
		return n
	}
	o.next++
	o.byID[id] = o.next
	return o.next
}

// queueTaskLines reads what the run is LEAVING behind: the tasks still actionable (todo or in
// progress) and the tasks parked on a human decision, each with the title and scope a report
// names it by. Both lists come from the queue itself, never from a commit range.
func queueTaskLines(hosts []string, scopes map[string]string) (actionable, blocked []taskLine, err error) {
	for _, host := range hosts {
		items, readErr := tasks.ReadTaskTree(host)
		if readErr != nil {
			return nil, nil, readErr
		}
		for _, item := range items {
			line := taskLine{id: item.ID, title: item.Title, scope: scopes[host], state: stateWords(item.State)}
			switch item.State {
			case tasks.StateTodo, tasks.StateInProgress:
				actionable = append(actionable, line)
			case tasks.StateBlocked:
				blocked = append(blocked, line)
			}
		}
	}
	return actionable, blocked, nil
}

// stateWords is a queue state as prose — "todo", "in progress" — for a sentence that says where a
// task actually sits. It never forces every reopened task to read as todo.
func stateWords(state string) string {
	return strings.ReplaceAll(tasks.StateLabel(state), "_", " ")
}

// taskTitle is one task's human title, or its id when the queue cannot be read: a report never
// invents a title, but it also never fails because of one.
func taskTitle(hosts []string, id string) string {
	for _, host := range hosts {
		items, err := tasks.ReadTaskTree(host)
		if err != nil {
			continue
		}
		for _, item := range items {
			if item.ID == id {
				return item.Title
			}
		}
	}
	return id
}

// titlesOf is taskTitle for a set, preserving the caller's order.
func titlesOf(hosts []string, ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = taskTitle(hosts, id)
	}
	return out
}

// printReopened says what the final review sent back, by title, and that the queue continues.
func printReopened(ids []string, titles []string) {
	ui.Note("")
	ui.Note("Final review found more work in %s.", ui.Count(len(ids), "task"))
	for i, title := range titles {
		if i == maxReportedTasks {
			ui.Note("  … and %d more", len(titles)-maxReportedTasks)
			break
		}
		ui.Note("  %s", title)
	}
	ui.Note("")
	ui.Note("Continuing the task queue.")
}

// printSigningFailure reports commits the host key could not sign. It never implies the commits
// were lost — they exist, unsigned — and it names the command that signs them.
func printSigningFailure(commits int, err error) {
	what := "some commits"
	if commits > 0 {
		what = ui.Count(commits, "commit")
	}
	ui.Alert("Could not sign "+what, fmt.Sprintf("%v", err), [2]string{"Sign them:", "coop sign"})
}

// printRunSummary closes the run with what it completed and what that cost. Both sections are
// omitted when there is nothing to report: a zero ledger is noise, and an invented total is worse.
func printRunSummary(completed []taskLine, cost runCost, h *loopHealth) {
	if len(completed) > 0 {
		ui.Note("")
		ui.Note("Completed this run")
		for _, t := range completed {
			ui.Note("  %s", t.title)
			ui.Note("    %s", taskIdentity(t.id, t.scope))
		}
	}
	printUsage(cost)
	printFlagged(completed, h)
}

// printUsage reports what the providers said this run cost. Cost the provider never reported is
// "not reported", never $0.00; a run with no usage data at all prints no section.
func printUsage(cost runCost) {
	if len(cost.byModel) == 0 && cost.total.usd == 0 && cost.total.inTok == 0 && cost.total.outTok == 0 {
		return
	}
	ui.Note("")
	ui.Note("Usage")
	if len(cost.byModel) > 1 {
		w := 0
		for _, m := range cost.byModel {
			if n := len([]rune(m.model)); n > w {
				w = n
			}
		}
		for _, m := range cost.byModel {
			ui.Note("  %s%s  %s · %s", m.model, strings.Repeat(" ", w-len([]rune(m.model))), reportedCost(m.cost.usd), tokenText(m.cost.inTok, m.cost.outTok))
		}
		return
	}
	ui.Note("  Reported cost: %s", reportedCost(cost.total.usd))
	ui.Note("  Tokens: %s", tokenText(cost.total.inTok, cost.total.outTok))
}

// reportedCost renders a cost the provider actually reported. An absent cost says so in words: a
// $0.00 would read as a free run.
func reportedCost(usd float64) string {
	if usd <= 0 {
		return "not reported"
	}
	return fmt.Sprintf("$%.2f", usd)
}

func tokenText(in, out int) string {
	return humanTokenCount(in) + " in · " + humanTokenCount(out) + " out"
}

// printFlagged points at completed work whose run had a risk signal — the review reopened it, or
// it edited the checks that judge it — so the reader knows where to look first.
func printFlagged(completed []taskLine, h *loopHealth) {
	if h == nil {
		return
	}
	var lines []string
	for _, t := range completed {
		th := h.byTask[t.id]
		if th == nil || !th.shaky() {
			continue
		}
		switch {
		case th.reopens > 0:
			lines = append(lines, fmt.Sprintf("%s was reopened %d times by the review.", t.title, th.reopens))
		default:
			lines = append(lines, fmt.Sprintf("%s changed the project checks that judge it.", t.title))
		}
	}
	if len(lines) == 0 {
		return
	}
	ui.Note("")
	ui.Note("Worth a look")
	for _, line := range lines {
		ui.Note("  %s", line)
	}
}

// silenceDetail is what the watchdog actually observed, in the operator's words: the clock that
// fired and the silence it measured. It never claims the model was idle — only that nothing it
// recognizes was recorded.
func silenceDetail(c iterationClassification) string {
	if detail := strings.TrimPrefix(c.timeoutDetail(), " after "); detail != "" {
		return detail
	}
	return "no provider activity"
}

// capitalize upper-cases the first rune of a sentence fragment, so a detail can open a line.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// humanWait renders a wait the way a person says it — "14 minutes", "2 hours 15 minutes" — so a
// reset time reads as a plan rather than a duration literal.
func humanWait(d time.Duration) string {
	d = d.Round(time.Minute)
	if d < time.Minute {
		return "less than a minute"
	}
	h, m := int(d/time.Hour), int((d%time.Hour)/time.Minute)
	switch {
	case h == 0:
		return ui.Count(m, "minute")
	case m == 0:
		return ui.Count(h, "hour")
	}
	return ui.Count(h, "hour") + " " + ui.Count(m, "minute")
}

// resetClock is when a usage limit lifts, in the reader's own zone — the clock time plus the zone
// name, so a person can compare it with their watch.
func resetClock(t time.Time) string {
	local := t.Local()
	zone := time.Local.String()
	if zone == "" || zone == "Local" {
		zone, _ = local.Zone()
	}
	return local.Format("15:04") + " " + zone
}
