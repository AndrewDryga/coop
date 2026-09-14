package loop

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/preset"
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
type taskLine struct {
	id, title, scope, state string
	doneSubtasks, subtasks  int
}

func taskReportLine(item tasks.Item, scope string) taskLine {
	done := 0
	for _, checked := range item.Subtasks {
		if checked {
			done++
		}
	}
	return taskLine{
		id: item.ID, title: item.Title, scope: scope,
		doneSubtasks: done, subtasks: len(item.Subtasks),
	}
}

func taskTitleWithProgress(t taskLine) string {
	title := cleanDiagnosticLine(t.title)
	if t.subtasks == 0 {
		return title
	}
	return fmt.Sprintf("%s · %d/%d subtasks", title, t.doneSubtasks, t.subtasks)
}

// rememberCompletion keeps the closing digest to one current row per task. A review may reopen and
// re-complete the same task; its latest authoritative checklist replaces the earlier snapshot.
func rememberCompletion(lines []taskLine, completed taskLine) []taskLine {
	for i := range lines {
		if lines[i].id == completed.id {
			lines[i] = completed
			return lines
		}
	}
	return append(lines, completed)
}

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
	id, scope = cleanDiagnosticLine(id), cleanDiagnosticLine(scope)
	if scope == "" {
		return id
	}
	return id + " · " + scope
}

func loopConfigLine(configured bool) string {
	if !configured {
		return "Using built-in defaults"
	}
	return "Using " + loopConfigName
}

// loopConfigName is the committed loop configuration's repo-relative path, as the intro names it.
const loopConfigName = ".agent/loop.yaml"

func printLoopPreparation(prep Preparation, nudges []string, awake bool) {
	ui.Section("Preparing loop")
	if prep.RemovedBoxes > 0 {
		ui.Pass("Removed %s whose Coop processes had stopped", ui.Count(prep.RemovedBoxes, "box", "boxes"))
	}
	if prep.RemovedNetworks > 0 {
		ui.Pass("Removed %s", ui.Count(prep.RemovedNetworks, "unused Coop network"))
	}
	if prep.RecoveredFilteredRuns > 0 {
		ui.Caution("Recovered %s", ui.Count(prep.RecoveredFilteredRuns, "interrupted filtered network run"))
		ui.Note("    Their Coop processes are no longer running.")
		ui.Note("    Details: coop net runs")
	}
	for _, nudge := range nudges {
		printLoopStaleness(nudge)
	}
	if awake {
		if prep.RemovedBoxes+prep.RemovedNetworks+prep.RecoveredFilteredRuns > 0 || len(nudges) > 0 {
			ui.Note("")
		}
		ui.Pass("Keeping this Mac awake using caffeinate")
	}
}

func printLoopStaleness(nudge string) {
	ui.Note("")
	switch {
	case strings.HasPrefix(nudge, "box image is stale —"):
		ui.Caution("Box image is out of date")
		ui.Note("    The box Dockerfile or .tool-versions changed since it was built.")
		ui.Note("    To rebuild: coop build")
	case strings.HasPrefix(nudge, "box image was built by coop "):
		ui.Caution("Box image does not match this Coop version")
		ui.Note("    %s", nudge)
		ui.Note("    To rebuild: coop build")
	case strings.HasPrefix(nudge, "box image is ") && strings.Contains(nudge, " days old"):
		ui.Caution("Box image is old")
		ui.Note("    %s", nudge)
		ui.Note("    To refresh: coop update")
	default:
		ui.Caution("%s", nudge)
	}
}

// printTaskHeader announces the attempt about to start: its ordinal in this run, the task's title,
// its scope when needed, and the agent (or custom command) that will work it. Ordinals are stable
// per task for the whole run, so a retry keeps the number a reader already saw.
func printTaskHeader(ordinal, attempt int, task taskLine, runner string, custom bool, counts tasks.TaskCounts) {
	width := ui.TermWidth(os.Stderr)
	if width > 64 {
		width = 64
	}
	if width < 24 {
		width = 24
	}
	ui.Note("")
	for _, line := range taskBannerLines(ui.For(os.Stderr), width, ordinal, attempt, task, runner, custom, counts) {
		ui.Note("%s", line)
	}
	ui.Note("")
}

func taskBannerLines(p ui.Palette, width, ordinal, attempt int, task taskLine, runner string, custom bool, counts tasks.TaskCounts) []string {
	label := "Agent  "
	if custom {
		label = "Command  "
	}
	rule := p.Dim(strings.Repeat("━", width))
	lines := []string{rule, p.Dim(fmt.Sprintf(" Task %d - Attempt %d", ordinal, attempt)), ""}
	for _, line := range wrapDisplay(cleanDiagnosticLine(task.title), width-1) {
		lines = append(lines, " "+line)
	}
	lines = append(lines, "")
	if task.scope != "" {
		for _, line := range prefixedDisplayLines("Project  ", cleanDiagnosticLine(task.scope), width-1) {
			lines = append(lines, p.Dim(" "+line))
		}
	}
	if runner != "" {
		for _, line := range prefixedDisplayLines(label, cleanDiagnosticLine(runner), width-1) {
			lines = append(lines, p.Dim(" "+line))
		}
	}
	queue := fmt.Sprintf("%d completed · %d active · %d pending · %d blocked", counts.Done, counts.Doing, counts.Todo, counts.Blocked)
	for _, line := range prefixedDisplayLines("Queue  ", queue, width-1) {
		lines = append(lines, p.Dim(" "+line))
	}
	return append(lines, rule)
}

func prefixedDisplayLines(prefix, value string, width int) []string {
	return ui.PrefixedLines(prefix, value, width)
}

func wrapDisplay(text string, width int) []string {
	return ui.WrapLines(text, width)
}

func wrappedLoopText(text string, indent int) string {
	return strings.Join(ui.WrapLines(cleanDiagnosticLine(text), loopOutputWidth(nil)-indent), "\n")
}

func alreadyCommittedCause(title, commit string) string {
	return wrappedLoopText(fmt.Sprintf("%s is linked to %s in this branch. Check its work before marking it done.", title, commit), 6)
}

// printTaskCompleted marks an accepted completion. It says the task finished, not that it passed
// the final review — that verdict is the run's, at the end.
func printTaskCompleted(task taskLine) {
	ui.Note("")
	lines := prefixedDisplayLines("Task completed: ", taskTitleWithProgress(task), loopOutputWidth(nil)-2)
	ui.OK("%s", lines[0])
	for _, line := range lines[1:] {
		ui.Note("  %s", line)
	}
}

type reviewField struct {
	label  string
	values []string
}

// printReviewBanner opens a review stage with the agent that was actually selected for it.
// Review rotations are independent of the work rotation, so the target is read at the stage.
func printReviewBanner(title, target string, fields ...reviewField) {
	width := ui.TermWidth(os.Stderr)
	if width > 64 {
		width = 64
	}
	if width < 24 {
		width = 24
	}
	p := ui.For(os.Stderr)
	rule := p.Dim(strings.Repeat("─", width))
	ui.Note("")
	ui.Note("%s", rule)
	for _, line := range wrapDisplay(cleanDiagnosticLine(title), width-1) {
		ui.Note(" %s", line)
	}
	if target != "" || len(fields) > 0 {
		ui.Note("")
	}
	if target != "" {
		for _, line := range prefixedDisplayLines("Agent  ", cleanDiagnosticLine(target), width-1) {
			ui.Note("%s", p.Dim(" "+line))
		}
	}
	for _, field := range fields {
		prefix := field.label + "  "
		for _, value := range field.values {
			for _, line := range prefixedDisplayLines(prefix, cleanDiagnosticLine(value), width-1) {
				ui.Note("%s", p.Dim(" "+line))
			}
			prefix = strings.Repeat(" ", len([]rune(prefix)))
		}
	}
	ui.Note("%s", rule)
	ui.Note("")
}

func reviewHeader(text string, target string) {
	printReviewBanner(text, target)
}

// printProtectedReview names the gate-defining files a completed task changed and what the review
// is for. It is mandatory, so it is never described as optional.
func printProtectedReview(target string, files []string) {
	printReviewBanner("Reviewing project checks", target, reviewField{label: "Files", values: files})
	ui.Note("Checking that these changes did not weaken the checks.")
}

func printFinalReview(round, rounds int, target string, tasks int) {
	title := fmt.Sprintf("Final review · Round %d of %d", round, rounds)
	if round > rounds {
		title = "Final review · After decision"
	}
	printReviewBanner(title, target,
		reviewField{label: "Tasks", values: []string{fmt.Sprintf("%d completed", tasks)}})
}

func printVerification(target string, tasks int) {
	printReviewBanner("Verification", target,
		reviewField{label: "Tasks", values: []string{fmt.Sprintf("%d completed", tasks)}})
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

// authHeadline names the exact failed target, keeps legacy profile names terminal-safe, and wraps
// before Alert adds its two-column mark. The continuation indent is part of the returned headline
// because Alert colors the headline as one value.
func authHeadline(t agents.Target) string {
	message := cleanDiagnosticLine(agents.DisplayTarget(t.String())) + " could not sign in"
	return strings.Join(ui.WrapLines(message, loopOutputWidth(nil)-2), "\n  ")
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
func printReopened(stage string, ids []string, titles []string) {
	width := ui.TermWidth(os.Stderr)
	if width > 64 {
		width = 64
	}
	if width < 24 {
		width = 24
	}
	p := ui.For(os.Stderr)
	rule := p.Dim(strings.Repeat("─", width))
	ui.Note("")
	ui.Note("%s", rule)
	verb := "need"
	if len(ids) == 1 {
		verb = "needs"
	}
	ui.Note(" %s · %s", stage, ui.Yellow(ui.Count(len(ids), "task")+" "+verb+" more work"))
	ui.Note("")
	for i, title := range titles {
		if i == maxReportedTasks {
			ui.Note("  … and %d more", len(titles)-maxReportedTasks)
			break
		}
		for _, line := range wrapDisplay(cleanDiagnosticLine(title), width-2) {
			ui.Note("  %s", line)
		}
	}
	ui.Note("")
	ui.Note("%s", p.Dim(" Continuing the task queue"))
	ui.Note("%s", rule)
	ui.Note("")
}

func printReviewLimit(titles []string, rounds int) {
	width := ui.TermWidth(os.Stderr)
	if width > 64 {
		width = 64
	}
	if width < 24 {
		width = 24
	}
	p := ui.For(os.Stderr)
	rule := p.Dim(strings.Repeat("─", width))
	ui.Note("")
	ui.Note("%s", rule)
	ui.Note(" Final review · %s", ui.Yellow("Review limit reached"))
	ui.Note("")
	for i, title := range titles {
		if i == maxReportedTasks {
			ui.Note("  … and %d more", len(titles)-maxReportedTasks)
			break
		}
		for _, line := range wrapDisplay(cleanDiagnosticLine(title), width-1) {
			ui.Note(" %s", line)
		}
	}
	ui.Note("")
	work := "this work"
	if len(titles) == 1 {
		work = "this task"
	}
	ui.Note("%s", p.Dim(fmt.Sprintf(" Could not resolve %s after %d rounds.", work, rounds)))
	ui.Note("%s", p.Dim(" Blocked for your decision."))
	ui.Note("%s", rule)
	ui.Note("")
}

// printSigningFailure reports commits the host key could not sign. It never implies the commits
// were lost — they exist, unsigned — and it names the command that signs them.
func printSigningFailure(commits int, err error) {
	what := "some commits"
	if commits > 0 {
		what = ui.Count(commits, "commit")
	}
	ui.Alert("Could not sign "+what, cleanDiagnosticLine(fmt.Sprintf("%v", err)), [2]string{"Sign them:", "coop sign"})
}

// printRunSummary closes the run with what it completed and what that cost. Both sections are
// omitted when there is nothing to report: a zero ledger is noise, and an invented total is worse.
func printRunSummary(completed []taskLine, cost runCost, h *loopHealth) {
	if len(completed) > 0 {
		ui.Note("")
		ui.Note("Completed this run")
		for _, t := range completed {
			ui.Note("  %s", taskTitleWithProgress(t))
			ui.Note("    %s", taskIdentity(t.id, t.scope))
		}
	}
	printUsage(cost)
	printFlagged(completed, h)
}

// printRoleHealth says what happened to each configured preset role without making an optional,
// unused role look like a failed run. Consult/delegate wrappers emit these rows; native roles stay
// explicitly unobserved because their provider does not expose a reliable per-role lifecycle.
func printRoleHealth(p *preset.Preset, records []PeerRecord) {
	if p == nil || len(p.Roles) == 0 {
		return
	}
	ui.Note("")
	ui.Note("Preset roles")
	for _, role := range p.Roles {
		var successes, failures []PeerRecord
		for _, record := range records {
			if record.Kind != "role_health" || record.Role != role.Name {
				continue
			}
			if record.Outcome == "success" {
				successes = append(successes, record)
			} else if record.Outcome == "failed" {
				failures = append(failures, record)
			}
		}
		switch {
		case len(successes) > 0:
			last := successes[len(successes)-1]
			detail := "succeeded on " + cleanDiagnosticLine(last.Target)
			if len(failures) > 0 {
				detail += " after " + ui.Count(len(failures), "failed target")
			}
			ui.Pass("%s · %s", role.Name, detail)
		case len(failures) > 0:
			last := failures[len(failures)-1]
			detail := fmt.Sprintf("failed on %s after %s", cleanDiagnosticLine(last.Target), ui.Count(last.Attempts, "attempt"))
			if last.Cause != "" {
				detail += " · " + cleanDiagnosticLine(last.Cause)
			}
			ui.Caution("%s · %s", role.Name, detail)
		case role.Mode == preset.ModeNative:
			ui.Note("  %s · usage not reported by the lead provider", cleanDiagnosticLine(role.Name))
		default:
			ui.Note("  %s · not used", cleanDiagnosticLine(role.Name))
		}
	}
}

// printUsage reports what the providers said this run cost. Cost the provider never reported is
// "not reported", never $0.00; a run with no usage data at all prints no section.
func printUsage(cost runCost) {
	if len(cost.byModel) == 0 && !cost.total.costReported && cost.total.usd == 0 && cost.total.inTok == 0 && cost.total.outTok == 0 {
		return
	}
	ui.Note("")
	ui.Note("Usage")
	if len(cost.byModel) > 1 {
		w := 0
		for _, m := range cost.byModel {
			if n := len([]rune(cleanDiagnosticLine(m.model))); n > w {
				w = n
			}
		}
		for _, m := range cost.byModel {
			model := cleanDiagnosticLine(m.model)
			ui.Note("  %s%s  %s · %s", model, strings.Repeat(" ", w-len([]rune(model))), reportedCost(m.cost), tokenText(m.cost.inTok, m.cost.outTok))
			ui.Note("    %s · output %s · provider time %s", inputBreakdown(m.cost), reportedToken(m.cost.output), reportedDuration(m.cost.elapsed))
		}
		return
	}
	ui.Note("  Reported cost: %s", reportedCost(cost.total))
	ui.Note("  Tokens: %s", tokenText(cost.total.inTok, cost.total.outTok))
	ui.Note("  Input: %s", inputBreakdown(cost.total))
	ui.Note("  Output: %s", reportedToken(cost.total.output))
	ui.Note("  Provider time: %s", reportedDuration(cost.total.elapsed))
}

func reportedToken(value reportedInt) string {
	if !value.reported {
		return "not reported"
	}
	if value.missing {
		return humanTokenCount(value.value) + " reported; some usage not split"
	}
	return humanTokenCount(value.value)
}

func inputBreakdown(cost stageCost) string {
	return "fresh " + reportedToken(cost.fresh) + " · cache write " + reportedToken(cost.cacheWrite) + " · cache read " + reportedToken(cost.cacheRead)
}

func reportedDuration(value reportedInt) string {
	if !value.reported {
		return "not reported"
	}
	duration := (time.Duration(value.value) * time.Millisecond).Round(time.Second)
	if value.missing {
		return duration.String() + " reported; some usage not timed"
	}
	return duration.String()
}

// reportedCost renders a cost the provider actually reported. An absent cost says so in words: a
// $0.00 would read as a free run.
func reportedCost(cost stageCost) string {
	if !cost.costReported && cost.usd <= 0 {
		return "not reported"
	}
	return fmt.Sprintf("$%.2f", cost.usd)
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
			lines = append(lines, fmt.Sprintf("%s was reopened %d times by the review.", cleanDiagnosticLine(t.title), th.reopens))
		default:
			lines = append(lines, fmt.Sprintf("%s changed the project checks that judge it.", cleanDiagnosticLine(t.title)))
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
