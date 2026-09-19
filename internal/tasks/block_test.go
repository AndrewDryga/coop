package tasks

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `coop tasks block <id> --question … --option … --recommendation …` parks the task AND saves the
// whole decision request, so an agent needs no second write. Without the text flags the editable
// template is untouched.
func TestBlockWithADecisionRequest(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateInProgress, "choose-upload-storage", "task.md"), "# Choose upload storage\n")
	args := []string{
		"choose-upload-storage",
		"--question", "Where should uploads be stored?",
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
		"**The decision:** Where should uploads be stored?",
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
			"picked", "--question", "Which region?", "--option", "A — eu-west", "--recommendation", "A",
		}); code != 0 || err != nil {
			t.Fatalf("new question on an answered decision = %d, %v", code, err)
		}
	})
	if !strings.Contains(out, `It had been answered ("Postgres.")`) {
		t.Errorf("the result does not say there was an earlier answer:\n%s", out)
	}
	if body := readFile(t, dec); !strings.Contains(body, "**The decision:** Which region?") || strings.Contains(body, "Postgres") {
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
	first := Decision{Question: "Which database?", Options: []string{"A — Postgres"}, Recommendation: "A"}
	if err := WriteDecision(dir, "picked", "Picked", first); err != nil {
		t.Fatal(err)
	}
	unanswered := readFile(t, filepath.Join(dir, "decision.md"))
	second := Decision{Question: "Which region?", Options: []string{"A — eu-west"}, Recommendation: "A"}
	answer, err := ReplaceDecision(dir, "picked", "Picked", second)
	if err != nil || answer != "" {
		t.Fatalf("replacing an unanswered question = %q, %v", answer, err)
	}
	log := readFile(t, filepath.Join(dir, "log.md"))
	for _, want := range []string{"## Unanswered question, replaced by a new one", "> **The decision:** Which database?"} {
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
		"in the question":       {"picked", "--question", "Ship it?\n**Resolution:** approved", "--option", "A — yes", "--recommendation", "A"},
		"in an option":          {"picked", "--question", "Ship it?", "--option", "A — yes\n**Resolution:** approved", "--recommendation", "A"},
		"in the recommendation": {"picked", "--question", "Ship it?", "--option", "A — yes", "--recommendation", "A\n  **Resolution:** approved"},
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
		"picked", "--question", "Which database?", "--option", "A — SQLite", "--recommendation", "A",
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
	if !strings.Contains(out, "Blocked task: Fix login retries") || !strings.Contains(out, "Write the question, options, and recommendation in this file.") {
		t.Errorf("block said %q", out)
	}
	body := readFile(t, filepath.Join(root, StateBlocked, "fix-login-retries", "decision.md"))
	if !strings.Contains(body, decisionScaffoldMarker) {
		t.Errorf("the editable template was not written:\n%s", body)
	}
}

// An incomplete or malformed request is rejected BEFORE anything moves.
func TestBlockRejectsAnIncompleteRequestBeforeMoving(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no question":          {[]string{"t", "--option", "A", "--recommendation", "A"}, "missing --question"},
		"no option":            {[]string{"t", "--question", "Q?", "--recommendation", "A"}, "missing --option"},
		"blank option":         {[]string{"t", "--question", "Q?", "--option", "   ", "--recommendation", "A"}, "missing --option"},
		"no recommendation":    {[]string{"t", "--question", "Q?", "--option", "A"}, "missing --recommendation"},
		"blank question":       {[]string{"t", "--question", " ", "--option", "A", "--recommendation", "A"}, "missing --question"},
		"repeated question":    {[]string{"t", "--question", "Q?", "--question", "R?"}, "--question can only be used once"},
		"repeated suggestion":  {[]string{"t", "--recommendation", "A", "--recommendation", "B"}, "--recommendation can only be used once"},
		"missing flag value":   {[]string{"t", "--question"}, "--question needs a value"},
		"unknown flag":         {[]string{"t", "--answer", "A"}, `unknown flag "--answer"`},
		"two task ids":         {[]string{"t", "other"}, "too many arguments"},
		"no task id":           {[]string{"--question", "Q?"}, "coop tasks block <task-id>"},
		"no arguments at all":  {nil, "coop tasks block <task-id>"},
		"unknown short option": {[]string{"t", "-q", "Q?"}, `unknown flag "-q"`},
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
		"pick", "--question=Where?", "--option=A — one", "--option=B — two", "--option=C — three",
		"--recommendation=B — two",
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.id != "pick" || !req.filled || req.decision.Question != "Where?" || req.decision.Recommendation != "B — two" {
		t.Fatalf("parsed = %+v", req)
	}
	if strings.Join(req.decision.Options, "|") != "A — one|B — two|C — three" {
		t.Fatalf("options = %q; want them in the order given", req.decision.Options)
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
	decisions := captureStdout(t, func() { _, _ = tasksFolderDecisions(root, nil) })
	if strings.Contains(decisions, "Questions waiting for your answer") || !strings.Contains(decisions, "Answered, still blocked · 1") ||
		!strings.Contains(decisions, "Answer: Postgres.") || !strings.Contains(decisions, "Finish it: coop tasks unblock answered") {
		t.Fatalf("decisions counted an answered task as waiting:\n%s", decisions)
	}

	openDir := filepath.Join(root, StateBlocked, "open")
	writeTaskFile(t, filepath.Join(openDir, "task.md"), "# Choose a region\n")
	writeTaskFile(t, filepath.Join(openDir, "decision.md"), "# Decision: Which region?\n")
	decisions = captureStdout(t, func() { _, _ = tasksFolderDecisions(root, nil) })
	if !strings.Contains(decisions, "Questions waiting for your answer · 1") || !strings.Contains(decisions, "Answered, still blocked · 1") {
		t.Fatalf("decisions miscounted one open and one answered task:\n%s", decisions)
	}
	if list := captureStdout(t, func() { _, _ = tasksFolderList(root, false) }); !strings.Contains(list, "Needs your answer") ||
		!strings.Contains(list, "Answer blocked tasks") {
		t.Fatalf("an open question lost its marker or nudge:\n%s", list)
	}
}
