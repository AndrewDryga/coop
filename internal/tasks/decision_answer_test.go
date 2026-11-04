package tasks

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What has been, or could be, left on the Resolution line in place of an answer. The first three are
// real: the first two made the owner take an open question for finished on 2026-11-03.
var pendingNotes = []string{
	"pending user answer to the existing asynchronous question.",
	"pending owner direction.",
	"Pending the user's answer. The current optimization is committed and tested; no idle helper is running.",
	"pending owner direction",
	"awaiting the owner",
	"TBD",
	"tbd by the owner",
	"TBC",
	"Pending",
	"PENDING.",
	"_pending_",
	"**Pending**",
	"(pending)",
	"[awaiting owner]",
	"Awaiting owner decision",
	"Awaits the owner's answer",
	"Waiting on the owner",
	"waiting for your answer",
	"Waiting to hear back",
	"Waiting.",
	"Still pending",
	"still waiting for the owner",
	"Currently undecided",
	"Still open",
	"Open",
	"Open — awaiting owner",
	"Open question for the owner",
	"Answer pending",
	"Decision pending the owner's review",
	"The answer is still pending",
	"Your answer is pending",
	"Owner's decision pending",
	"Owner approval pending",
	"Owner decision awaited",
	"Blocked pending owner decision",
	"2026-11-03: awaiting the owner",
	"To be decided",
	"to be determined by the owner",
	"To be filled in by the owner",
	"Not yet answered",
	"not decided yet",
	"Not resolved",
	"The owner has not answered yet",
	"Owner hasn't decided",
	"This question is still open",
	"Question remains open",
	"Not yet",
	"No answer yet",
	"No decision",
	"None yet",
	"None",
	"Nothing yet",
	"Undecided",
	"Unanswered",
	"Unresolved",
	"TODO",
	"To do",
	"Needs your answer",
	"Needs an owner decision",
	"Need human input",
	"<your answer>",
	"—",
	"?",
	"...",
	"",
	"<!-- Human: write your answer here, or use coop tasks unblock. -->",
	"<!-- unterminated",
	// A person may mean this one, but nothing tells it from a note, so it is asked again.
	"Pending review is fine, publish it",
}

// Real answers, several of them close to a note. The ones naming an owner, a date or an option number
// have the shape of answers in real decision files.
var realAnswers = []string{
	"Publish it, the pending review is fine",
	"The pending review is fine, publish it",
	"This pending review is fine, publish it",
	"Postgres.",
	"A",
	"B — go SQLite",
	"Yes",
	"No",
	"Yes, pending legal review",
	"Approve, pending CI",
	"Hold pending the legal review",
	"Wait for the review, then publish",
	"Keep waiting for the vendor",
	"Waiting is fine",
	"Open a follow-up task and ship this one",
	"None of them, drop the feature",
	"Not yet, revisit it after the launch",
	"No answer needed: no folders remain.",
	"No decision needed, it is moot.",
	"Needs more testing first, keep it on hold",
	"Todo list, not a wiki",
	"Decision: ship A",
	"Answer: Postgres",
	"Owner approved option A",
	"Owner 2026-10-06: looks great, commit it.",
	"Resolved by evidence, not judgement.",
	"Has been decided: go with A",
	"Close the four old waiting conversations.",
	"A — wait for the full qualification.",
	"2 - go with A or C",
	"N/A, the question went away",
	"Use <Button> in the header",
}

// Both readers take their answer from one place, so neither can see an answer the other does not.
func TestAPendingNoteIsNoAnswerAndARealAnswerIs(t *testing.T) {
	check := func(resolution, want string) {
		t.Helper()
		body := "# Decision: Which one?\n\n**Resolution:** " + resolution + "\n"
		if got := decisionResolution(body); got != want {
			t.Errorf("decisionResolution(%q) = %q, want %q", resolution, got, want)
		}
		dec := filepath.Join(t.TempDir(), "decision.md")
		writeTaskFile(t, dec, body)
		if got := mustDecisionResolved(t, dec); got != (want != "") {
			t.Errorf("decisionResolved(%q) = %v, want %v", resolution, got, want != "")
		}
	}
	for _, note := range pendingNotes {
		check(note, "")
	}
	for _, answer := range realAnswers {
		check(answer, answer)
	}
}

// blockedWithResolution writes the blocked task "brand" with a hand-written decision.md holding
// resolution on its Resolution line, and returns that file's path.
func blockedWithResolution(t *testing.T, root, resolution string) string {
	t.Helper()
	dir := filepath.Join(root, StateBlocked, "brand")
	writeTaskFile(t, filepath.Join(dir, "task.md"), "# Choose the brand\n")
	dec := filepath.Join(dir, "decision.md")
	writeTaskFile(t, dec, "# Decision: Which brand should we use?\n\n**Recommendation:** The short one.\n\n---\n\n**Resolution:** "+resolution+"\n")
	return dec
}

// assertStillBlocked fails unless the task "brand" is blocked with its decision.md unchanged.
func assertStillBlocked(t *testing.T, root, dec, before string) {
	t.Helper()
	if current, ok := mustCurrentTask(t, root, "brand"); !ok || current.State != StateBlocked {
		t.Errorf("the task moved: %+v, found=%v", current, ok)
	}
	if after := readFile(t, dec); after != before {
		t.Errorf("decision.md changed:\n%s", after)
	}
}

// brandBrowser opens the decision browser on the task "brand".
func brandBrowser(root string) []decisionRef { return []decisionRef{{root: root, id: "brand"}} }

// The incident: a note on the line read as the owner's answer. It must read as an open question in
// the list, the decisions listing and the browser, and be refused by every path that needs an answer.
func TestAPendingNoteStaysAQuestionInEveryCommand(t *testing.T) {
	pipedStdin(t)
	for _, note := range []string{
		"pending owner direction", "awaiting the owner", "TBD",
		"pending user answer to the existing asynchronous question.",
	} {
		t.Run(note, func(t *testing.T) {
			root := t.TempDir()
			dec := blockedWithResolution(t, root, note)
			before := readFile(t, dec)

			list := captureStdout(t, func() { _, _ = tasksFolderList(root, false) })
			if !strings.Contains(list, "Needs your answer") || !strings.Contains(list, "Answer blocked tasks: coop tasks decisions -i") ||
				strings.Contains(list, "Answered") {
				t.Errorf("coop tasks does not ask:\n%s", list)
			}

			decisions := captureStdout(t, func() { _, _ = tasksFolderDecisions("", root, nil) })
			if !strings.Contains(decisions, "Questions waiting for your answer · 1") || !strings.Contains(decisions, "Which brand should we use?") ||
				!strings.Contains(decisions, "Recommendation: The short one.") ||
				strings.Contains(decisions, "Answered") || strings.Contains(decisions, "Answer:") || strings.Contains(decisions, note) {
				t.Errorf("coop tasks decisions does not ask:\n%s", decisions)
			}

			// decisions -i picks it as a question. With no terminal it says it cannot ask, where an
			// answered task would have nothing to ask and succeed.
			code, err := tasksFolderDecisions("", root, []string{"-i"})
			if code != 2 || err == nil || !strings.Contains(err.Error(), "needs an interactive terminal") {
				t.Errorf("decisions -i = (%d, %v), want it to go on to ask", code, err)
			}
			var browser bytes.Buffer
			if code, err := runDecisionBrowser("", brandBrowser(root), strings.NewReader(":q\n"), &browser); code != 0 || err != nil {
				t.Fatalf("browser = (%d, %v)", code, err)
			}
			if out := browser.String(); !strings.Contains(out, "Which brand should we use?") || !strings.Contains(out, "Your answer (Enter to skip):") ||
				strings.Contains(out, "Answered") || strings.Contains(out, note) {
				t.Errorf("the browser does not ask:\n%s", out)
			}

			code, err = tasksFolderUnblock("", root, []string{"brand"})
			if code != 2 || err == nil || !strings.Contains(err.Error(), "has no resolution yet") || !strings.Contains(err.Error(), waitingAnswerNote) {
				t.Errorf("unblock with no answer = (%d, %v), want the refusal naming the rule", code, err)
			}
			if ids := mustUnblockResolved(t, []string{root}); len(ids) != 0 {
				t.Errorf("preflight unblocked %v", ids)
			}
			assertStillBlocked(t, root, dec, before)
		})
	}
}

// The multi-queue roll-up picks its questions apart from the single-queue listing, so it is checked
// on its own, in both of its forms.
func TestTheRollUpAsksAPendingNote(t *testing.T) {
	pipedStdin(t)
	repo := t.TempDir()
	rels := []string{"svc-a/.agent/tasks", "svc-b/.agent/tasks"}
	blockedWithResolution(t, filepath.Join(repo, rels[1]), "pending owner direction")
	out := captureStdout(t, func() {
		if code, err := tasksDecisionsAll(repo, rels, nil); code != 0 || err != nil {
			t.Errorf("roll-up = (%d, %v)", code, err)
		}
	})
	if !strings.Contains(out, "Questions waiting for your answer · 1") || strings.Contains(out, "Answered") {
		t.Errorf("the roll-up does not ask:\n%s", out)
	}
	code, err := tasksDecisionsAll(repo, rels, []string{"-i"})
	if code != 2 || err == nil || !strings.Contains(err.Error(), "needs an interactive terminal") {
		t.Errorf("roll-up -i = (%d, %v), want it to go on to ask", code, err)
	}
}

// A real answer written by hand keeps counting, including one that uses the word pending.
func TestAHandWrittenAnswerCountsInEveryCommand(t *testing.T) {
	pipedStdin(t)
	for _, answer := range []string{"Publish it, the pending review is fine", "Yes, pending legal review", "Postgres."} {
		t.Run(answer, func(t *testing.T) {
			root := t.TempDir()
			blockedWithResolution(t, root, answer)

			list := captureStdout(t, func() { _, _ = tasksFolderList(root, false) })
			if !strings.Contains(list, "Answered · finish it: coop tasks unblock brand") || strings.Contains(list, "Needs your answer") ||
				strings.Contains(list, "Answer blocked tasks") {
				t.Errorf("coop tasks asks again:\n%s", list)
			}

			decisions := captureStdout(t, func() {
				if code, err := tasksFolderDecisions("", root, nil); code != 0 || err != nil {
					t.Errorf("decisions = (%d, %v)", code, err)
				}
			})
			if strings.Contains(decisions, "Questions waiting") || !strings.Contains(decisions, "Answered, still blocked · 1") ||
				!strings.Contains(decisions, "Answer: "+answer) || !strings.Contains(decisions, "Finish it: coop tasks unblock brand") {
				t.Errorf("coop tasks decisions asks again:\n%s", decisions)
			}

			interactive := captureStdout(t, func() {
				if code, err := tasksFolderDecisions("", root, []string{"-i"}); code != 0 || err != nil {
					t.Errorf("decisions -i = (%d, %v), want nothing left to ask", code, err)
				}
			})
			if !strings.Contains(interactive, "Answer: "+answer) {
				t.Errorf("decisions -i lost the answer:\n%s", interactive)
			}

			// The browser shows the answer when it comes back to it, and Enter keeps it.
			var browser bytes.Buffer
			if code, err := runDecisionBrowser("", brandBrowser(root), strings.NewReader("\n"), &browser); code != 0 || err != nil {
				t.Fatalf("browser = (%d, %v)", code, err)
			}
			if out := browser.String(); !strings.Contains(out, "Answered: "+answer) || !strings.Contains(out, "Enter to keep it") {
				t.Errorf("the browser does not show the answer:\n%s", out)
			}

			if code, err := tasksFolderUnblock("", root, []string{"brand"}); code != 0 || err != nil {
				t.Fatalf("unblock with the written answer = (%d, %v)", code, err)
			}
			if current, ok := mustCurrentTask(t, root, "brand"); !ok || current.State != StateTodo {
				t.Fatalf("the answered task is not in todo: %+v, found=%v", current, ok)
			}
			if !strings.Contains(readFile(t, filepath.Join(root, StateTodo, "brand", "log.md")), "Unblocked: "+answer) {
				t.Errorf("the log does not carry the answer")
			}
			lint := captureStdout(t, func() { _, _ = tasksFolderLint(root) })
			if strings.Contains(lint, "unresolved decision.md") {
				t.Errorf("lint calls the answered task unresolved:\n%s", lint)
			}
		})
	}
}

// Preflight returns a task only on a real answer, whichever words the answer uses.
func TestPreflightUnblocksARealAnswerThatSaysPending(t *testing.T) {
	root := t.TempDir()
	blockedWithResolution(t, root, "Publish it, the pending review is fine")
	if ids := mustUnblockResolved(t, []string{root}); len(ids) != 1 || ids[0] != "brand" {
		t.Fatalf("preflight unblocked %v, want [brand]", ids)
	}
}

// An answer given to unblock replaces whatever the line held, a note included, and reads back as the
// answer. This is the one path that writes the line for the person answering.
func TestUnblockRecordsARealAnswerOverANote(t *testing.T) {
	const answer = "Publish it, the pending review is fine"
	root := t.TempDir()
	blockedWithResolution(t, root, "pending owner direction")
	if code, err := tasksFolderUnblock("", root, []string{"brand", answer}); code != 0 || err != nil {
		t.Fatalf("unblock with an answer = (%d, %v)", code, err)
	}
	dir := filepath.Join(root, StateTodo, "brand")
	body := readFile(t, filepath.Join(dir, "decision.md"))
	if !strings.Contains(body, "**Resolution:** "+answer+"\n") || strings.Contains(body, "pending owner direction") {
		t.Errorf("the answer did not replace the note:\n%s", body)
	}
	if !mustDecisionResolved(t, filepath.Join(dir, "decision.md")) {
		t.Error("the recorded answer does not read back as an answer")
	}
	if !strings.Contains(readFile(t, filepath.Join(dir, "log.md")), "Unblocked: "+answer) {
		t.Error("the log does not carry the answer")
	}
	if lint := captureStdout(t, func() { _, _ = tasksFolderLint(root) }); strings.Contains(lint, "unresolved decision.md") {
		t.Errorf("lint calls the answered task unresolved:\n%s", lint)
	}
}

// Recording a note as the answer would send the task to todo with its question open, the state lint
// rejects, so unblock asks again. Nothing moves and nothing is written.
func TestUnblockRefusesANoteGivenAsTheAnswer(t *testing.T) {
	for _, note := range []string{"pending owner direction", "TBD", "<answer>", "-", "Pending review is fine, publish it"} {
		t.Run(note, func(t *testing.T) {
			root := t.TempDir()
			dec := blockedWithResolution(t, root, "<!-- Human: write your answer here -->")
			before := readFile(t, dec)
			code, err := tasksFolderUnblock("", root, []string{"brand", note})
			usage := refusal(t, err)
			if want := fmt.Sprintf("%q reads as no answer yet", note); code != 2 || usage.Headline != want || usage.Cause != waitingAnswerNote {
				t.Errorf("unblock = (%d, %+v), want exit 2 and %q", code, usage, want)
			}
			assertStillBlocked(t, root, dec, before)
			if _, err := os.Stat(filepath.Join(root, StateBlocked, "brand", "log.md")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a refused unblock wrote the log: %v", err)
			}
		})
	}
}

// Typed into the browser, a note saves nothing and the same question comes back, the way a refused
// park does. A real answer then replaces whatever note the file held.
func TestDecisionBrowserAsksAgainAfterANote(t *testing.T) {
	const answer = "Publish it, the pending review is fine"
	root := t.TempDir()
	dec := blockedWithResolution(t, root, "awaiting the owner")
	before := readFile(t, dec)

	var stopped bytes.Buffer
	if code, err := runDecisionBrowser("", brandBrowser(root), strings.NewReader("pending owner direction\n"), &stopped); code != 0 || err != nil {
		t.Fatalf("browser = (%d, %v)", code, err)
	}
	if out := stopped.String(); !strings.Contains(out, waitingAnswerNote) || strings.Count(out, "Question 1 of 1") != 2 {
		t.Errorf("the browser did not ask again after a note:\n%s", out)
	}
	assertStillBlocked(t, root, dec, before)

	var answered bytes.Buffer
	if code, err := runDecisionBrowser("", brandBrowser(root), strings.NewReader("TBD\n"+answer+"\n"), &answered); code != 0 || err != nil {
		t.Fatalf("browser = (%d, %v)", code, err)
	}
	body := readFile(t, filepath.Join(root, StateTodo, "brand", "decision.md"))
	if !strings.Contains(body, "**Resolution:** "+answer+"\n") || strings.Contains(body, "awaiting the owner") || strings.Contains(body, "TBD") {
		t.Errorf("the real answer did not replace the note:\n%s", body)
	}
}

// The commands around the question agree too: a note is nobody's answer, so lint flags it on a todo
// task, promote brings a parked task back blocked, and blocking neither stops at "already answered"
// nor reports the note as an earlier answer.
func TestThePendingNoteIsNoAnswerToTheOtherCommands(t *testing.T) {
	const note = "pending owner direction"
	const decision = "# Decision: Which brand should we use?\n\n**Resolution:** " + note + "\n"

	t.Run("lint flags it on a todo task", func(t *testing.T) {
		root := t.TempDir()
		writeTaskFile(t, filepath.Join(root, StateTodo, "brand", "task.md"), "# Choose the brand\n")
		writeTaskFile(t, filepath.Join(root, StateTodo, "brand", "decision.md"), decision)
		lint := captureStdout(t, func() {
			if code, _ := tasksFolderLint(root); code != 1 {
				t.Errorf("lint = %d, want 1", code)
			}
		})
		if !strings.Contains(lint, "has an unresolved decision.md but is todo") {
			t.Errorf("lint did not flag the open question:\n%s", lint)
		}
	})

	t.Run("promote brings a parked task back blocked", func(t *testing.T) {
		root := t.TempDir()
		blockedWithResolution(t, root, note)
		if code, err := tasksFolderPark(root, []string{"brand"}); code != 0 || err != nil {
			t.Fatalf("park = (%d, %v)", code, err)
		}
		if code, err := backlogFolderPromote(root, []string{"brand"}); code != 0 || err != nil {
			t.Fatalf("promote = (%d, %v)", code, err)
		}
		if !pathExists(filepath.Join(root, StateBlocked, "brand")) {
			t.Error("a promoted task with a note should come back blocked")
		}
	})

	t.Run("a plain block of a todo task goes through and keeps the file", func(t *testing.T) {
		root := t.TempDir()
		writeTaskFile(t, filepath.Join(root, StateTodo, "brand", "task.md"), "# Choose the brand\n")
		writeTaskFile(t, filepath.Join(root, StateTodo, "brand", "decision.md"), decision)
		if code, err := tasksFolderBlock(root, []string{"brand"}); code != 0 || err != nil {
			t.Fatalf("block = (%d, %v), want no 'already answered' refusal", code, err)
		}
		if got := readFile(t, filepath.Join(root, StateBlocked, "brand", "decision.md")); got != decision {
			t.Errorf("the block changed decision.md:\n%s", got)
		}
	})

	t.Run("a new question at the terminal protects it as an open question", func(t *testing.T) {
		root := t.TempDir()
		dec := blockedWithResolution(t, root, note)
		before := readFile(t, dec)
		code, err := tasksFolderBlock(root, []string{"brand", "--question", "Which region?", "--why", "W", "--option", "A — eu-west", "--recommendation", "A"})
		if code != 1 || !errors.Is(err, errDecisionAlreadyWritten) {
			t.Fatalf("block = (%d, %v), want the refusal that protects an unanswered question", code, err)
		}
		assertStillBlocked(t, root, dec, before)
	})

	t.Run("a new question from the box files it as unanswered", func(t *testing.T) {
		root := t.TempDir()
		blockedWithResolution(t, root, note)
		dir := filepath.Join(root, StateBlocked, "brand")
		earlier, err := ReplaceDecision(dir, "brand", "Choose the brand", Decision{Question: "Which region?", Why: "W", Options: []string{"A — eu-west"}, Recommendation: "A"})
		if err != nil || earlier != "" {
			t.Fatalf("ReplaceDecision = (%q, %v), want no earlier answer", earlier, err)
		}
		log := readFile(t, filepath.Join(dir, "log.md"))
		if !strings.Contains(log, "## Unanswered question, replaced by a new one") || !strings.Contains(log, "> **Resolution:** "+note) {
			t.Errorf("the log does not keep the note as an unanswered question:\n%s", log)
		}
	})
}
