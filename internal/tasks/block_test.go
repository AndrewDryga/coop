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

// Existing decision content — a human's answer, or another agent's question — is never overwritten.
func TestBlockRefusesToOverwriteAnExistingDecision(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateBlocked, "picked", "task.md"), "# Picked\n")
	dec := filepath.Join(root, StateBlocked, "picked", "decision.md")
	answered := "# Decision: Picked?\n\n**The decision:** Which database?\n\n**Resolution:** Postgres.\n"
	writeTaskFile(t, dec, answered)
	code, err := tasksFolderBlock(root, []string{
		"picked", "--question", "Something else?", "--option", "A — no", "--recommendation", "A",
	})
	if code != 1 || err == nil || !strings.Contains(err.Error(), dec) {
		t.Fatalf("conflicting request = %d, %v; want a refusal naming the file to edit", code, err)
	}
	if !strings.Contains(err.Error(), "is blocked") {
		t.Errorf("the refusal must state the task's actual state: %v", err)
	}
	if readFile(t, dec) != answered {
		t.Errorf("the existing decision was modified:\n%s", readFile(t, dec))
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
