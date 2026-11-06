package tasks

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/ui"
)

// `coop tasks block <id> --question … --why … --option … --recommendation …` parks the task AND saves
// the whole decision request, so an agent needs no second write: the question is the heading, and
// why the task is blocked follows the line naming it. Without the text flags the editable template
// is untouched.
func TestBlockWithADecisionRequest(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateInProgress, "choose-upload-storage", "task.md"), "# Choose upload storage\n")
	args := []string{
		"choose-upload-storage",
		"--question", "Where should uploads be stored?",
		"--why", "Uploads outgrew one disk, and moving them later means a migration.",
		"--option", "A — Local disk: simplest, tied to one machine.",
		"--option", "B — Object storage: shared across machines.",
		"--recommendation", "B — production runs on more than one machine.",
	}
	out := captureStderr(t, func() {
		if code, err := tasksFolderBlock(root, args); code != 0 || err != nil {
			t.Fatalf("block = %d, %v", code, err)
		}
	})
	if !strings.Contains(out, "Blocked task: Choose upload storage") {
		t.Errorf("block said %q", out)
	}
	if strings.Contains(out, "Write the question") {
		t.Errorf("a filled request must not tell the agent to write the file:\n%s", out)
	}
	dec := filepath.Join(root, StateBlocked, "choose-upload-storage", "decision.md")
	body := readFile(t, dec)
	for _, want := range []string{
		"# Decision: Where should uploads be stored?\n\n**Blocks:** this task (choose-upload-storage).\n\n" +
			"**Why it is blocked:** Uploads outgrew one disk, and moving them later means a migration.\n",
		"- A — Local disk: simplest, tied to one machine.",
		"- B — Object storage: shared across machines.",
		"**Recommendation:** B — production runs on more than one machine.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("decision.md lacks %q:\n%s", want, body)
		}
	}
	// The human's answer is the human's to write.
	if resolved, err := decisionResolved(dec); err != nil || resolved {
		t.Errorf("a request must never populate Resolution: resolved=%v err=%v", resolved, err)
	}

	// The identical request again is idempotent, not a refusal and not a rewrite.
	before := readFile(t, dec)
	if code, err := tasksFolderBlock(root, args); code != 0 || err != nil {
		t.Fatalf("identical retry = %d, %v", code, err)
	}
	if readFile(t, dec) != before {
		t.Error("an identical retry rewrote decision.md")
	}
}

// A new question on a task the human already answered is asked, and the answer is never lost: the
// answered decision goes into log.md, whole, before the new one replaces it, and the result says so.
// (A question nobody has answered yet is still never overwritten — see the edited-scaffold case.)
func TestBlockWithANewQuestionKeepsTheEarlierAnswer(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateBlocked, "picked", "task.md"), "# Picked\n")
	dec := filepath.Join(root, StateBlocked, "picked", "decision.md")
	answered := "# Decision: Picked?\n\n**The decision:** Which database?\n\n**Resolution:** Postgres.\n"
	writeTaskFile(t, dec, answered)
	out := captureStderr(t, func() {
		if code, err := tasksFolderBlock(root, []string{
			"picked", "--question", "Which region?", "--why", "W", "--option", "A — eu-west", "--recommendation", "A",
		}); code != 0 || err != nil {
			t.Fatalf("new question on an answered decision = %d, %v", code, err)
		}
	})
	if !strings.Contains(out, `It had been answered ("Postgres.")`) {
		t.Errorf("the result does not say there was an earlier answer:\n%s", out)
	}
	if body := readFile(t, dec); !strings.Contains(body, "# Decision: Which region?") || strings.Contains(body, "Postgres") {
		t.Errorf("decision.md does not carry the new question alone:\n%s", body)
	}
	// The whole decision is filed, once: its question, what was offered, and the answer.
	log := readFile(t, filepath.Join(root, StateBlocked, "picked", "log.md"))
	for _, want := range []string{"## Answered decision, replaced by a new question", "> # Decision: Picked?",
		"> **The decision:** Which database?", "> **Resolution:** Postgres."} {
		if n := strings.Count(log, want); n != 1 {
			t.Errorf("log.md carries %q %d times:\n%s", want, n, log)
		}
	}
}

// A question nobody has answered yet is not lost either: a new one from the box replaces it only
// after the old one is filed in log.md, whole — and a retry after a failed write files it once.
func TestANewQuestionKeepsTheOneNobodyAnsweredYet(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, StateBlocked, "picked")
	writeTaskFile(t, filepath.Join(dir, "task.md"), "# Picked\n")
	first := Decision{Question: "Which database?", Why: "W", Options: []string{"A — Postgres"}, Recommendation: "A"}
	if err := WriteDecision(dir, "picked", "Picked", first); err != nil {
		t.Fatal(err)
	}
	unanswered := readFile(t, filepath.Join(dir, "decision.md"))
	second := Decision{Question: "Which region?", Why: "W", Options: []string{"A — eu-west"}, Recommendation: "A"}
	answer, err := ReplaceDecision(dir, "picked", "Picked", second)
	if err != nil || answer != "" {
		t.Fatalf("replacing an unanswered question = %q, %v", answer, err)
	}
	log := readFile(t, filepath.Join(dir, "log.md"))
	for _, want := range []string{"## Unanswered question, replaced by a new one", "> # Decision: Which database?"} {
		if !strings.Contains(log, want) {
			t.Fatalf("log.md lacks %q:\n%s", want, log)
		}
	}
	if body := readFile(t, filepath.Join(dir, "decision.md")); !strings.Contains(body, "Which region?") || strings.Contains(body, "Which database?") {
		t.Fatalf("decision.md does not carry the new question alone:\n%s", body)
	}
	// The same question again changes nothing, and a retry after a write that failed mid-way (the
	// old decision still in place) files the archived question once, not twice.
	if _, err := ReplaceDecision(dir, "picked", "Picked", second); err != nil {
		t.Fatal(err)
	}
	writeTaskFile(t, filepath.Join(dir, "decision.md"), unanswered)
	if _, err := ReplaceDecision(dir, "picked", "Picked", second); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(readFile(t, filepath.Join(dir, "log.md")), "## Unanswered question, replaced by a new one"); n != 1 {
		t.Fatalf("the replaced question was filed %d times:\n%s", n, readFile(t, filepath.Join(dir, "log.md")))
	}
}

// The Resolution line is the human's alone. Text that carries one would make a decision read as
// answered — to the listing, to unblock, and to the refusal above — so it is refused before the
// task moves, whichever field carries it.
func TestADecisionCannotCarryTheHumansAnswerLine(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateTodo, "picked", "task.md"), "# Picked\n")
	for name, args := range map[string][]string{
		"in why":                {"picked", "--question", "Ship it?", "--why", "W\n**Resolution:** approved", "--option", "A — yes", "--recommendation", "A"},
		"in an option":          {"picked", "--question", "Ship it?", "--why", "W", "--option", "A — yes\n**Resolution:** approved", "--recommendation", "A"},
		"in the recommendation": {"picked", "--question", "Ship it?", "--why", "W", "--option", "A — yes", "--recommendation", "A\n  **Resolution:** approved"},
	} {
		code, err := tasksFolderBlock(root, args)
		if code == 0 || err == nil || !strings.Contains(err.Error(), "**Resolution:**") {
			t.Errorf("%s = %d, %v; want a refusal naming the line", name, code, err)
		}
		if _, err := os.Stat(filepath.Join(root, StateTodo, "picked", "task.md")); err != nil {
			t.Errorf("%s: the refused block moved the task: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(root, StateTodo, "picked", "decision.md")); !os.IsNotExist(err) {
			t.Errorf("%s: the refused block wrote a decision: %v", name, err)
		}
	}
}

// A plain block on an answered decision would park the human's answer as a fresh question — it
// happened, from a re-run block whose first success `| tail` hid. It is refused before anything
// moves, quoting the answer, with what to do instead.
func TestPlainBlockRefusesAnAnsweredDecisionBeforeMoving(t *testing.T) {
	answered := "# Decision: Picked?\n\n**The decision:** Which database?\n\n**Resolution:** Postgres.\n"
	for state, want := range map[string]string{
		StateTodo:    "work it instead of blocking it again",
		StateBlocked: "finish it instead of blocking it again: coop tasks unblock picked",
	} {
		root := t.TempDir()
		writeTaskFile(t, filepath.Join(root, state, "picked", "task.md"), "# Picked\n")
		dec := filepath.Join(root, state, "picked", "decision.md")
		writeTaskFile(t, dec, answered)
		code, err := tasksFolderBlock(root, []string{"picked"})
		if code != 1 || err == nil || !strings.Contains(err.Error(), `("Postgres.")`) || !strings.Contains(err.Error(), want) {
			t.Fatalf("plain block on an answered %s task = %d, %v; want a refusal quoting the answer and saying %q", state, code, err, want)
		}
		if readFile(t, dec) != answered {
			t.Errorf("%s: the answered decision was modified", state)
		}
		if _, err := os.Stat(filepath.Join(root, state, "picked", "task.md")); err != nil {
			t.Errorf("%s: the refused block moved the task: %v", state, err)
		}
	}
}

func TestBlockRefusesToOverwriteAnEditedScaffold(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateTodo, "picked", "task.md"), "# Picked\n")
	if code, err := tasksFolderBlock(root, []string{"picked"}); code != 0 || err != nil {
		t.Fatalf("seed scaffold = %d, %v", code, err)
	}
	dec := filepath.Join(root, StateBlocked, "picked", "decision.md")
	edited := strings.Replace(readFile(t, dec), "- **A — <name>:** <consequence>", "- **A — Postgres:** familiar", 1)
	writeTaskFile(t, dec, edited)

	code, err := tasksFolderBlock(root, []string{
		"picked", "--question", "Which database?", "--why", "W", "--option", "A — SQLite", "--recommendation", "A",
	})
	if code != 1 || !errors.Is(err, errDecisionAlreadyWritten) {
		t.Fatalf("edited scaffold request = %d, %v; want existing-decision refusal", code, err)
	}
	if got := readFile(t, dec); got != edited {
		t.Errorf("edited scaffold was overwritten:\n%s", got)
	}
}

// Without text flags the editable template is written and the result says how to fill it in.
func TestBlockWithoutTextFlagsKeepsTheTemplate(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateTodo, "fix-login-retries", "task.md"), "# Fix login retries\n")
	out := captureStderr(t, func() {
		if code, err := tasksFolderBlock(root, []string{"fix-login-retries"}); code != 0 || err != nil {
			t.Fatalf("block = %d, %v", code, err)
		}
	})
	if !strings.Contains(out, "Blocked task: Fix login retries") || !strings.Contains(out, "Write the question, why the task is blocked, the options, and your recommendation in this file.") {
		t.Errorf("block said %q", out)
	}
	body := readFile(t, filepath.Join(root, StateBlocked, "fix-login-retries", "decision.md"))
	if !strings.Contains(body, decisionScaffoldMarker) || !strings.Contains(body, "**Blocks:** this task (fix-login-retries).\n\n**Why it is blocked:** <") {
		t.Errorf("the editable template was not written with a place for why the task is blocked:\n%s", body)
	}
}

// An agent that wrote decision.md before blocking is not told to write it; a task already blocked
// is said to be so, and nothing about it changes.
func TestBlockSaysWhatIsLeftToDo(t *testing.T) {
	root := t.TempDir()
	written := "# Decision: Which disk?\n\n**Why it is blocked:** W\n\n**Options:**\n\n- A — one\n\n**Recommendation:** A\n\n---\n\n" + decisionResolutionLine
	writeTaskFile(t, filepath.Join(root, StateInProgress, "pick-disk", "task.md"), "# Pick disk\n")
	writeTaskFile(t, filepath.Join(root, StateInProgress, "pick-disk", "decision.md"), written)
	out := captureStderr(t, func() {
		if code, err := tasksFolderBlock(root, []string{"pick-disk"}); code != 0 || err != nil {
			t.Fatalf("block = %d, %v", code, err)
		}
	})
	dec := filepath.Join(root, StateBlocked, "pick-disk", "decision.md")
	if !strings.Contains(out, "Blocked task: Pick disk") || strings.Contains(out, "Write the question") || readFile(t, dec) != written {
		t.Errorf("blocking with a written decision said:\n%s\nand left:\n%s", out, readFile(t, dec))
	}

	out = captureStderr(t, func() {
		if code, err := tasksFolderBlock(root, []string{"pick-disk"}); code != 0 || err != nil {
			t.Fatalf("block again = %d, %v", code, err)
		}
	})
	if !strings.Contains(out, "Pick disk is already blocked.") || strings.Contains(out, "Blocked task") || readFile(t, dec) != written {
		t.Errorf("blocking a blocked task said:\n%s", out)
	}
	writeTaskFile(t, filepath.Join(root, StateBlocked, "no-decision", "task.md"), "# No decision\n")
	if code, err := tasksFolderBlock(root, []string{"no-decision"}); code != 0 || err != nil {
		t.Fatalf("block a blocked task without decision.md = %d, %v", code, err)
	}
	if _, err := os.Stat(filepath.Join(root, StateBlocked, "no-decision", "decision.md")); !os.IsNotExist(err) {
		t.Errorf("blocking a blocked task wrote a decision.md: %v", err)
	}
}

// An incomplete or malformed request is rejected BEFORE anything moves.
func TestBlockRejectsAnIncompleteRequestBeforeMoving(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no question":       {[]string{"t", "--why", "W", "--option", "A", "--recommendation", "A"}, `Missing --question for`},
		"no why":            {[]string{"t", "--question", "Q?", "--option", "A", "--recommendation", "A"}, `Missing --why for`},
		"no option":         {[]string{"t", "--question", "Q?", "--why", "W", "--recommendation", "A"}, `Missing --option for`},
		"blank option":      {[]string{"t", "--question", "Q?", "--why", "W", "--option", "   ", "--recommendation", "A"}, `Missing --option for`},
		"no recommendation": {[]string{"t", "--question", "Q?", "--why", "W", "--option", "A"}, `Missing --recommendation for`},
		"blank question":    {[]string{"t", "--question", " ", "--why", "W", "--option", "A", "--recommendation", "A"}, `Missing --question for`},
		"question on two lines": {[]string{"t", "--question", "Q?\nMore", "--why", "W", "--option", "A", "--recommendation", "A"},
			`Option "--question" must be one line`},
		"repeated question":    {[]string{"t", "--question", "Q?", "--question", "R?"}, `Option "--question" can only be used once`},
		"repeated suggestion":  {[]string{"t", "--recommendation", "A", "--recommendation", "B"}, `Option "--recommendation" can only be used once`},
		"missing flag value":   {[]string{"t", "--question"}, `Missing value for "--question"`},
		"unknown flag":         {[]string{"t", "--answer", "A"}, `Unknown option "--answer"`},
		"two task ids":         {[]string{"t", "other"}, `Unexpected argument "other"`},
		"no task id":           {[]string{"--question", "Q?"}, "coop tasks block <task-id>"},
		"no arguments at all":  {nil, "coop tasks block <task-id>"},
		"unknown short option": {[]string{"t", "-q", "Q?"}, `Unknown option "-q"`},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeTaskFile(t, filepath.Join(root, StateTodo, "t", "task.md"), "# T\n")
			code, err := tasksFolderBlock(root, tc.args)
			if code != 2 || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("block %v = %d, %v; want a usage error containing %q", tc.args, code, err, tc.want)
			}
			if !pathExists(filepath.Join(root, StateTodo, "t")) {
				t.Fatal("a rejected request moved the task")
			}
			if pathExists(filepath.Join(root, StateBlocked, "t")) {
				t.Fatal("a rejected request blocked the task")
			}
		})
	}
}

// The `--flag=value` spellings work too, and options accumulate in order.
func TestBlockAcceptsEqualsSpellingsAndRepeatedOptions(t *testing.T) {
	req, err := parseBlockArgs([]string{
		"pick", "--question=Where?", "--why=W", "--option=A — one", "--option=B — two", "--option=C — three",
		"--recommendation=B — two",
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.id != "pick" || !req.filled || req.decision.Question != "Where?" || req.decision.Why != "W" || req.decision.Recommendation != "B — two" {
		t.Fatalf("parsed = %+v", req)
	}
	if strings.Join(req.decision.Options, "|") != "A — one|B — two|C — three" {
		t.Fatalf("options = %q; want them in the order given", req.decision.Options)
	}
}

// The question is what `coop tasks decisions` and its -i browser show, with the one-line
// recommendation, whether decision.md came from a request or was written before it carried why
// the task is blocked.
func TestDecisionsShowTheQuestionOfANewAndAnOldDecision(t *testing.T) {
	root := t.TempDir()
	newDir := filepath.Join(root, StateBlocked, "pick-storage")
	writeTaskFile(t, filepath.Join(newDir, "task.md"), "# Pick storage\n")
	if err := WriteDecision(newDir, "pick-storage", "Pick storage", Decision{Question: "Where should uploads be stored?",
		Why: "Uploads outgrew one disk.", Options: []string{"A — disk", "B — object storage"}, Recommendation: "B — more than one machine."}); err != nil {
		t.Fatal(err)
	}
	oldDir := filepath.Join(root, StateBlocked, "pick-region")
	writeTaskFile(t, filepath.Join(oldDir, "task.md"), "# Pick region\n")
	writeTaskFile(t, filepath.Join(oldDir, "decision.md"), "# Decision: Pick region?\n\n**Blocks:** this task (pick-region).\n\n"+
		"**The decision:** Which region?\n\n**Options:**\n\n- A — eu-west\n\n**Recommendation:** A — closest to users.\n\n---\n\n"+decisionResolutionLine)
	list := captureStdout(t, func() { _, _ = tasksFolderDecisions("", root, nil) })
	for _, want := range []string{"Where should uploads be stored?", "Recommendation: B — more than one machine.", "Pick region?", "Recommendation: A — closest to users."} {
		if !strings.Contains(list, want) {
			t.Errorf("coop tasks decisions lacks %q:\n%s", want, list)
		}
	}
	var browser strings.Builder
	fprintDecisionBody(&browser, ui.Palette{}, readFile(t, filepath.Join(newDir, "decision.md")))
	for _, want := range []string{"Where should uploads be stored?\n", "  Why it is blocked: Uploads outgrew one disk.", "  Recommendation: B — more than one machine."} {
		if !strings.Contains(browser.String(), want) {
			t.Errorf("the decision browser lacks %q:\n%s", want, browser.String())
		}
	}
	if strings.Contains(browser.String(), "**") {
		t.Errorf("the decision browser prints markdown:\n%s", browser.String())
	}
}

// A decision nobody wrote yet has no question to show: decisions marks it not written, with the file
// to fill in, instead of the scaffold's "<title>?" and its recommendation placeholder. So does the
// -i browser.
func TestDecisionsMarkAnUnwrittenDecision(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, StateBlocked, "2026-11-03-pick-storage")
	writeTaskFile(t, filepath.Join(dir, "task.md"), "# Pick storage\n")
	writeTaskFile(t, filepath.Join(dir, "decision.md"), renderDecisionScaffold("2026-11-03-pick-storage", "Pick storage"))
	list := captureStdout(t, func() { _, _ = tasksFolderDecisions("", root, nil) })
	want := "\n  Pick storage\n    Decision not written yet: " + displayPath(filepath.Join(dir, "decision.md")) + "\n    2026-11-03-pick-storage\n"
	if !strings.Contains(list, want) || strings.Contains(list, "Pick storage?") || strings.Contains(list, "<your choice and why>") {
		t.Fatalf("coop tasks decisions shows the unwritten scaffold:\n%s\nwant it to contain:\n%s", list, want)
	}
	var browser strings.Builder
	if code, err := runDecisionBrowser("", []decisionRef{{root: root, id: "2026-11-03-pick-storage"}}, strings.NewReader(":q\n"), &browser); code != 0 || err != nil {
		t.Fatalf("browser = (%d, %v)", code, err)
	}
	if !strings.Contains(browser.String(), "Decision not written yet: ") || strings.Contains(browser.String(), "<your choice and why>") {
		t.Fatalf("the decision browser shows the unwritten scaffold:\n%s", browser.String())
	}
}

// lint names a blocked task whose decision.md asks nothing, empty or still the template nobody
// filled in, and leaves alone a written decision and a template a person already answered.
func TestLintNamesABlockedTaskWhoseDecisionAsksNothing(t *testing.T) {
	root := t.TempDir()
	if err := ScaffoldStateDirs(root); err != nil {
		t.Fatal(err)
	}
	for id, decision := range map[string]string{
		"empty":    "",
		"blank":    "\n  \n",
		"template": renderDecisionScaffold("template", "Template"),
		"answered": strings.Replace(renderDecisionScaffold("answered", "Answered"), decisionResolutionLine, "**Resolution:** Use the disk.\n", 1),
		"written":  RenderDecision("written", Decision{Question: "Which store?", Why: "W", Options: []string{"A — disk"}, Recommendation: "A"}),
	} {
		lintedTask(t, root, StateBlocked, id, "")
		writeTaskFile(t, filepath.Join(root, StateBlocked, id, "decision.md"), decision)
	}
	var code int
	out := captureStdout(t, func() { _ = captureStderr(t, func() { code, _ = tasksFolderLint(root) }) })
	const fill = " — write the question, why it is blocked, the options and your recommendation in it\n"
	want := "  blank: blocked but its decision.md is empty" + fill +
		"  empty: blocked but its decision.md is empty" + fill +
		"  template: blocked but its decision.md is still the template" + fill
	if code != 1 || out != want {
		t.Fatalf("lint = %d:\n%s\nwant:\n%s", code, out, want)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// A blocked task the human answered (in decision.md, not yet unblocked) is no question to ask again:
// the list marks it answered with the command that finishes it, the "answer blocked tasks" nudge
// counts only real questions, and `coop tasks decisions` lists it apart from them.
func TestAnAnsweredBlockedTaskIsShownAsAnswered(t *testing.T) {
	root := t.TempDir()
	answeredDir := filepath.Join(root, StateBlocked, "answered")
	writeTaskFile(t, filepath.Join(answeredDir, "task.md"), "# Choose a database\n")
	writeTaskFile(t, filepath.Join(answeredDir, "decision.md"), "# Decision: Which database?\n\n**Resolution:** Postgres.\n")
	list := captureStdout(t, func() { _, _ = tasksFolderList(root, false) })
	if !strings.Contains(list, "Answered · finish it: coop tasks unblock answered") || strings.Contains(list, "Needs your answer") ||
		strings.Contains(list, "Answer blocked tasks") {
		t.Fatalf("an answered blocked task is listed as a question:\n%s", list)
	}
	decisions := captureStdout(t, func() { _, _ = tasksFolderDecisions("", root, nil) })
	if strings.Contains(decisions, "Questions waiting for your answer") || !strings.Contains(decisions, "Answered, still blocked · 1") ||
		!strings.Contains(decisions, "Answer: Postgres.") || !strings.Contains(decisions, "Finish it: coop tasks unblock answered") {
		t.Fatalf("decisions counted an answered task as waiting:\n%s", decisions)
	}

	openDir := filepath.Join(root, StateBlocked, "open")
	writeTaskFile(t, filepath.Join(openDir, "task.md"), "# Choose a region\n")
	writeTaskFile(t, filepath.Join(openDir, "decision.md"), "# Decision: Which region?\n")
	decisions = captureStdout(t, func() { _, _ = tasksFolderDecisions("", root, nil) })
	if !strings.Contains(decisions, "Questions waiting for your answer · 1") || !strings.Contains(decisions, "Answered, still blocked · 1") {
		t.Fatalf("decisions miscounted one open and one answered task:\n%s", decisions)
	}
	if list := captureStdout(t, func() { _, _ = tasksFolderList(root, false) }); !strings.Contains(list, "Needs your answer") ||
		!strings.Contains(list, "Answer blocked tasks") {
		t.Fatalf("an open question lost its marker or nudge:\n%s", list)
	}
}
