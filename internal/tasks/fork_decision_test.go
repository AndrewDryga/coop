package tasks

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/forkspace"
)

const forkDecisionQuestion = "Should a partial write be retried?"

// acceptedForkDecision follows the real assignment lifecycle through a fork asking a question:
// only the projected task is blocked directly, then host acceptance records that state canonically.
func acceptedForkDecision(t *testing.T, name string, withArtifact bool) (string, string, ForkAssignment) {
	t.Helper()
	repo, root, assignment := proposalAssignment(t, name)
	acceptForkDecision(t, repo, root, assignment, withArtifact)
	return repo, root, assignment
}

func acceptForkDecision(t *testing.T, repo, root string, assignment ForkAssignment, withArtifact bool) {
	t.Helper()
	projected, ok := mustCurrentTask(t, assignment.Owner.Projection, "assigned")
	if !ok {
		t.Fatal("assigned projection is missing")
	}
	if err := WriteDecision(projected.Dir, projected.ID, projected.Title, Decision{
		Question:       forkDecisionQuestion,
		Why:            "Retrying may duplicate a partial write.",
		Options:        []string{"A — retry the write", "B — leave it for manual repair"},
		Recommendation: "A — retry the write",
	}); err != nil {
		t.Fatal(err)
	}
	if withArtifact {
		artifact := filepath.Join(projected.Dir, "artifacts", "fork-note.txt")
		if err := os.MkdirAll(filepath.Dir(artifact), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(artifact, []byte("keep this task artifact\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := MoveTaskDir(assignment.Owner.Projection, projected, StateBlocked); err != nil {
		t.Fatal(err)
	}
	if _, err := AcceptForkProjection(repo, root, projected.ID, assignment.Owner); err != nil {
		t.Fatal(err)
	}
	canonical, ok := mustCurrentTask(t, root, projected.ID)
	if !ok || canonical.State != StateBlocked {
		t.Fatalf("accepted canonical task = %+v, found=%v; want blocked", canonical, ok)
	}
	owner, owned, err := ReadTaskOwnerRecord(root, projected.ID)
	if err != nil || !owned || owner.Fork == nil || owner.Fork.Phase != ForkAssignmentBlocked {
		t.Fatalf("accepted fork owner = %+v, owned=%v, err=%v; want blocked authority", owner, owned, err)
	}
}

func selectResumedForkDecision(t *testing.T, repo, root string, original ForkAssignment, answer string) ForkAssignment {
	t.Helper()
	if code, err := tasksFolderUnblock(repo, root, []string{"assigned", answer}); code != 0 || err != nil {
		t.Fatalf("tasks unblock fork decision = (%d, %v)", code, err)
	}
	resumed, err := AssignForkTask([]string{root}, ForkAssignmentRequest{
		AuthorityRepo: repo,
		Fork:          original.Owner.Fork,
		WorkspaceRoot: forkspace.Workspace(repo, original.Owner.Fork.Name),
		BaselineHead:  original.Owner.BaselineHead,
		LeaseOwner:    testLeaseOwner(),
	})
	if err != nil || resumed.Outcome != ForkAssignmentSelected {
		t.Fatalf("same-generation assignment = %+v, err=%v", resumed, err)
	}
	t.Cleanup(func() { _ = resumed.Lease.Release() })
	if resumed.Owner.Fork != original.Owner.Fork || resumed.Owner.AssignmentID != original.Owner.AssignmentID {
		t.Fatalf("resumed identity = %s/%s, assignment %s; want %s/%s, assignment %s",
			resumed.Owner.Fork.Name, resumed.Owner.Fork.Generation, resumed.Owner.AssignmentID,
			original.Owner.Fork.Name, original.Owner.Fork.Generation, original.Owner.AssignmentID)
	}
	return resumed
}

func resumeForkDecision(t *testing.T, repo, root string, original ForkAssignment, answer string) Item {
	t.Helper()
	resumed := selectResumedForkDecision(t, repo, root, original, answer)
	if err := PrepareForkProjectionForRun(repo, root, "assigned", resumed.Owner); err != nil {
		t.Fatal(err)
	}
	if err := PrepareForkProjectionForRun(repo, root, "assigned", resumed.Owner); err != nil {
		t.Fatal(err)
	}
	projected, ok := mustCurrentTask(t, resumed.Owner.Projection, "assigned")
	if !ok || projected.State != StateInProgress {
		t.Fatalf("resumed projection = %+v, found=%v; want in progress", projected, ok)
	}
	got := decisionResolution(readFileString(filepath.Join(projected.Dir, "decision.md")))
	if got != answer {
		t.Errorf("resumed projection answer = %q, want exact human answer %q", got, answer)
	}
	return projected
}

func TestForkBlockedDecisionListingNamesFork(t *testing.T) {
	const name = "decision-list"
	repo, root, _ := acceptedForkDecision(t, name, false)
	var code int
	var err error
	out := captureStdout(t, func() { code, err = tasksFolderDecisions(repo, root, nil) })
	if code != 0 || err != nil {
		t.Fatalf("tasks decisions = (%d, %v)", code, err)
	}
	for _, want := range []string{forkDecisionQuestion, "assigned · in fork " + name} {
		if !strings.Contains(out, want) {
			t.Errorf("tasks decisions is missing %q:\n%s", want, out)
		}
	}
}

func TestForkBlockedDecisionBrowserAnswersAndResumesCanonicalTask(t *testing.T) {
	const name = "decision-browser"
	const answer = "Retry only after checking the write marker."
	repo, root, assignment := acceptedForkDecision(t, name, false)
	canonical, ok := mustCurrentTask(t, root, "assigned")
	if !ok {
		t.Fatal("canonical blocked task is missing")
	}
	var out bytes.Buffer
	refs := decisionRefs(root, "packages/service/.agent/tasks", []Item{canonical})
	if code, err := runDecisionBrowser(repo, refs, strings.NewReader(answer+"\n"), &out); code != 0 || err != nil {
		t.Fatalf("decision browser = (%d, %v)", code, err)
	}
	for _, want := range []string{"packages/service/.agent/tasks · assigned · in fork " + name, forkResumeNote(name)} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("decision browser is missing %q:\n%s", want, out.String())
		}
	}
	canonical, ok = mustCurrentTask(t, root, "assigned")
	if !ok || canonical.State != StateTodo {
		t.Fatalf("browser answer left canonical task = %+v, found=%v; want todo", canonical, ok)
	}
	if got := decisionResolution(readFileString(filepath.Join(canonical.Dir, "decision.md"))); got != answer {
		t.Errorf("canonical decision answer = %q, want %q", got, answer)
	}
	resumeForkDecision(t, repo, root, assignment, answer)
}

func TestForkBlockedDecisionUnblockResumesWithAnswerAndKeepsArtifact(t *testing.T) {
	const name = "decision-unblock"
	const answer = "Retry after verifying the marker."
	repo, root, assignment := acceptedForkDecision(t, name, true)
	canonical, ok := mustCurrentTask(t, root, "assigned")
	if !ok {
		t.Fatal("canonical blocked task is missing")
	}
	decision := filepath.Join(canonical.Dir, "decision.md")
	before := readFileString(decision)
	if code, err := tasksFolderUnblock(repo, root, []string{"assigned", "pending owner direction"}); code == 0 || err == nil || !strings.Contains(err.Error(), waitingAnswerNote) {
		t.Fatalf("invalid no-answer unblock = (%d, %v), want the no-answer refusal", code, err)
	}
	canonical, ok = mustCurrentTask(t, root, "assigned")
	if !ok || canonical.State != StateBlocked || readFileString(filepath.Join(canonical.Dir, "decision.md")) != before {
		t.Fatalf("refused answer changed canonical task or decision: %+v, found=%v", canonical, ok)
	}

	var code int
	var err error
	out := captureStderr(t, func() { code, err = tasksFolderUnblock(repo, root, []string{"assigned", answer}) })
	if code != 0 || err != nil {
		t.Fatalf("tasks unblock with answer = (%d, %v)", code, err)
	}
	if !strings.Contains(out, "coop fork "+name+" --loop") {
		t.Errorf("unblock does not say how to resume the fork:\n%s", out)
	}
	canonical, ok = mustCurrentTask(t, root, "assigned")
	if !ok || canonical.State != StateTodo {
		t.Fatalf("inline answer left canonical task = %+v, found=%v; want todo", canonical, ok)
	}
	if got := decisionResolution(readFileString(filepath.Join(canonical.Dir, "decision.md"))); got != answer {
		t.Errorf("canonical decision answer = %q, want %q", got, answer)
	}
	projected := resumeForkDecision(t, repo, root, assignment, answer)
	artifact := filepath.Join(projected.Dir, "artifacts", "fork-note.txt")
	if got := readFileString(artifact); got != "keep this task artifact\n" {
		t.Errorf("resumed projection artifact = %q, want its original content", got)
	}
}

func TestProjectBoardPlacesBlockedForkAssignmentUnderNeedsYou(t *testing.T) {
	const name = "decision-board"
	repo, root, _ := acceptedForkDecision(t, name, false)
	var code int
	var err error
	out := captureStdout(t, func() { code, err = TasksWatch(Host{}, repo, []string{root}) })
	if code != 0 || err != nil {
		t.Fatalf("project board = (%d, %v)", code, err)
	}
	section := strings.Index(out, "Needs you (1)")
	row := strings.Index(out, "⚑ assigned")
	if section < 0 || row < section || !strings.Contains(out, "in fork "+name) {
		t.Errorf("project board does not place the blocked fork assignment under Needs you:\n%s", out)
	}
}

func TestMultiQueueDecisionListingNamesForkOnUnwrittenQuestion(t *testing.T) {
	const name = "decision-unwritten"
	repo := filepath.Join(t.TempDir(), "project")
	firstRoot := filepath.Join(repo, ".agent", "tasks")
	secondRoot := filepath.Join(repo, "packages", "service", ".agent", "tasks")
	ordinary := taskForLease(t, firstRoot, StateBlocked, "ordinary")
	if err := WriteDecision(ordinary.Dir, ordinary.ID, ordinary.Title, Decision{
		Question: "Keep the ordinary task?", Why: "It is waiting for direction.", Options: []string{"Keep it"}, Recommendation: "Keep it",
	}); err != nil {
		t.Fatal(err)
	}
	taskForLease(t, secondRoot, StateTodo, "assigned")
	workspace, identity := testAssignmentFork(t, repo, name)
	assignment, err := AssignForkTask([]string{secondRoot}, ForkAssignmentRequest{
		AuthorityRepo: repo, Fork: identity, WorkspaceRoot: workspace,
		BaselineHead: strings.Repeat("a", 40), LeaseOwner: testLeaseOwner(),
	})
	if err != nil || assignment.Outcome != ForkAssignmentSelected {
		t.Fatalf("assign fork task = %+v, %v", assignment, err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	acceptForkDecision(t, repo, secondRoot, assignment, false)
	canonical, ok := mustCurrentTask(t, secondRoot, "assigned")
	if !ok {
		t.Fatal("canonical blocked task is missing")
	}
	if err := os.Remove(filepath.Join(canonical.Dir, "decision.md")); err != nil {
		t.Fatal(err)
	}

	var code int
	var listErr error
	out := captureStdout(t, func() {
		code, listErr = tasksDecisionsRollup(repo, []string{".agent/tasks", "packages/service/.agent/tasks"}, nil)
	})
	if code != 0 || listErr != nil {
		t.Fatalf("multi-queue decisions = (%d, %v)", code, listErr)
	}
	for _, want := range []string{".agent/tasks", "packages/service/.agent/tasks", "Decision not written yet:", "assigned · in fork " + name, "ordinary"} {
		if !strings.Contains(out, want) {
			t.Errorf("multi-queue decisions is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ordinary · in fork ") {
		t.Errorf("ordinary task was labeled as fork-owned:\n%s", out)
	}
}

// The project's log notes how its task came back, and the fork's next acceptance copies the fork's
// log over the project's, so the unblock notes it in the fork's copy too, once.
func TestForkAcceptanceKeepsTheProjectsUnblockedLine(t *testing.T) {
	const answer = "Retry after verifying the marker."
	repo, root, assignment := acceptedForkDecision(t, "decision-log", false)
	resumed := selectResumedForkDecision(t, repo, root, assignment, answer)
	if err := PrepareForkProjectionForRun(repo, root, "assigned", resumed.Owner); err != nil {
		t.Fatal(err)
	}
	projected, _ := mustCurrentTask(t, resumed.Owner.Projection, "assigned")
	if err := AppendTaskLogEntry(projected.Dir, "Retried the write once the marker checked out."); err != nil {
		t.Fatal(err)
	}
	if _, err := AcceptForkProjection(repo, root, "assigned", resumed.Owner); err != nil {
		t.Fatal(err)
	}
	canonical, _ := mustCurrentTask(t, root, "assigned")
	log := readFileString(filepath.Join(canonical.Dir, "log.md"))
	if strings.Count(log, "- "+unblockedNote(answer)+"\n") != 1 || !strings.Contains(log, "Retried the write") {
		t.Errorf("project log after the fork's acceptance:\n%s\nwant the fork's entry and one %q line", log, unblockedNote(answer))
	}
}

// coop fork stop holds the lock the worker's acceptance waits on, so a stop that lands after the fork
// parked its task on a question carries that question to the project itself, whether the task was a
// new one or came back answered. A task answered and waiting for the fork's next pass keeps its copy
// blocked, with the answer, until that pass restores it: the stop leaves that one alone.
func TestForkStopCarriesAQuestionTheForkJustParked(t *testing.T) {
	repo, root, assignment := proposalAssignment(t, "decision-stop")
	park := func(owner ForkTaskOwner, question string) {
		t.Helper()
		projected, _ := mustCurrentTask(t, owner.Projection, "assigned")
		if err := WriteDecision(projected.Dir, projected.ID, projected.Title, Decision{
			Question: question, Why: "Only the owner can choose.", Options: []string{"this", "that"}, Recommendation: "this",
		}); err != nil {
			t.Fatal(err)
		}
		if err := MoveTaskDir(owner.Projection, projected, StateBlocked); err != nil {
			t.Fatal(err)
		}
	}
	stop := func() {
		t.Helper()
		if err := PauseForkAssignments(repo, assignment.Owner.Fork); err != nil {
			t.Fatal(err)
		}
	}
	waitsOn := func(question string) {
		t.Helper()
		canonical, _ := mustCurrentTask(t, root, "assigned")
		body := readFileString(filepath.Join(canonical.Dir, "decision.md"))
		owner, owned, err := ReadTaskOwnerRecord(root, "assigned")
		if canonical.State != StateBlocked || decisionQuestion(body) != question || decisionResolution(body) != "" ||
			err != nil || !owned || owner.Fork == nil || owner.Fork.Phase != ForkAssignmentBlocked {
			t.Fatalf("after the stop the project's task is %s asking %q (answer %q), owner %+v, %v; want blocked on %q",
				canonical.State, decisionQuestion(body), decisionResolution(body), owner.Fork, err, question)
		}
		out := captureStdout(t, func() {
			if code, err := tasksFolderDecisions(repo, root, nil); code != 0 || err != nil {
				t.Errorf("tasks decisions = (%d, %v)", code, err)
			}
		})
		if !strings.Contains(out, question) {
			t.Errorf("coop tasks decisions does not list %q:\n%s", question, out)
		}
	}

	park(assignment.Owner, "Which way first?")
	stop()
	waitsOn("Which way first?")

	if code, err := tasksFolderUnblock(repo, root, []string{"assigned", "Go this way."}); code != 0 || err != nil {
		t.Fatalf("tasks unblock = (%d, %v)", code, err)
	}
	waiting := func(state string) {
		t.Helper()
		stop()
		canonical, _ := mustCurrentTask(t, root, "assigned")
		owner, _, err := ReadTaskOwnerRecord(root, "assigned")
		if canonical.State != state || err != nil || owner.Fork == nil || owner.Fork.Phase != ForkAssignmentPaused {
			t.Fatalf("the stop left an answered task that waits for its pass %s, owner %+v, %v; want %s and paused", canonical.State, owner.Fork, err, state)
		}
	}
	waiting(StateTodo)
	resumed, err := AssignForkTask([]string{root}, ForkAssignmentRequest{
		AuthorityRepo: repo, Fork: assignment.Owner.Fork, WorkspaceRoot: forkspace.Workspace(repo, assignment.Owner.Fork.Name),
		BaselineHead: assignment.Owner.BaselineHead, LeaseOwner: testLeaseOwner(),
	})
	if err != nil || resumed.Outcome != ForkAssignmentSelected {
		t.Fatalf("resumed assignment = %+v, %v", resumed, err)
	}
	waiting(StateInProgress) // between the assignment and the restore of its copy
	if err := errors.Join(PrepareForkProjectionForRun(repo, root, "assigned", resumed.Owner), resumed.Lease.Release()); err != nil {
		t.Fatal(err)
	}
	park(resumed.Owner, "Which way now?")
	stop()
	waitsOn("Which way now?")
	resumeForkDecision(t, repo, root, assignment, "Go that way.")
}

// answeredWhileStopped is a fork task a person answered after the fork stopped: in todo, its
// assignment paused, and its copy still blocked, now asking another question than the project's
// answered one, as a box that rewrote decision.md would leave it.
func answeredWhileStopped(t *testing.T, name string) (string, string, ForkAssignment, string) {
	t.Helper()
	repo, root, assignment := acceptedForkDecision(t, name, false)
	if code, err := tasksFolderUnblock(repo, root, []string{"assigned", "Go this way."}); code != 0 || err != nil {
		t.Fatalf("tasks unblock = (%d, %v)", code, err)
	}
	projected, _ := mustCurrentTask(t, assignment.Owner.Projection, "assigned")
	writeTaskFile(t, filepath.Join(projected.Dir, "decision.md"), RenderDecision("assigned", Decision{
		Question: "Which way now?", Why: "The box wrote it.", Options: []string{"this", "that"}, Recommendation: "this",
	}))
	canonical, _ := mustCurrentTask(t, root, "assigned")
	return repo, root, assignment, readFileString(filepath.Join(canonical.Dir, "decision.md"))
}

func assertAnsweredTaskUntouched(t *testing.T, root, answered string) {
	t.Helper()
	canonical, _ := mustCurrentTask(t, root, "assigned")
	owner, owned, err := ReadTaskOwnerRecord(root, "assigned")
	if canonical.State != StateTodo || err != nil || !owned || owner.Fork == nil || owner.Fork.Phase != ForkAssignmentPaused {
		t.Errorf("the answered task is %s, owner %+v, %v; want todo and paused", canonical.State, owner.Fork, err)
	}
	if got := readFileString(filepath.Join(canonical.Dir, "decision.md")); got != answered {
		t.Errorf("the project's decision.md became\n%s\nwant the person's answer\n%s", got, answered)
	}
}

// A stop leaves an answered task that waits for the fork's next pass alone, whatever its copy says:
// accepting that copy wrote the box's decision.md over the person's answer before it refused todo.
func TestForkStopLeavesATaskAnsweredWhileStoppedAlone(t *testing.T) {
	repo, root, assignment, answered := answeredWhileStopped(t, "decision-answered")
	if err := PauseForkAssignments(repo, assignment.Owner.Fork); err != nil {
		t.Fatalf("stop over an answered task = %v", err)
	}
	assertAnsweredTaskUntouched(t, root, answered)
}

// Acceptance refuses a blocked copy of a task in todo before any of the copy's files reach the
// project's folder.
func TestForkAcceptanceRefusesATaskInTodoBeforeCopyingAnything(t *testing.T) {
	repo, root, _, answered := answeredWhileStopped(t, "decision-todo")
	owner, owned, err := ReadTaskOwnerRecord(root, "assigned")
	if err != nil || !owned || owner.Fork == nil {
		t.Fatalf("owner = %+v, %v", owner.Fork, err)
	}
	if _, err := AcceptForkProjection(repo, root, "assigned", *owner.Fork); err == nil || !strings.Contains(err.Error(), "unsupported state "+StateTodo) {
		t.Errorf("acceptance over a task in todo = %v, want the refusal", err)
	}
	assertAnsweredTaskUntouched(t, root, answered)
}

// An unblock notes itself in the fork's copy of the log even when it changes no decision.md: the
// answer was already in both copies (a plain unblock of a decision the person answered by hand in
// each), or the task had no decision.md. The fork's next acceptance copies that log over the
// project's, and a retried unblock notes itself once.
func TestForkUnblockNotesItselfWithoutAChangedDecision(t *testing.T) {
	for name, tc := range map[string]struct {
		decision, answer, note string
	}{
		"answered":    {decision: "# Decision: Which way?\n\n**Resolution:** Go this way.\n", answer: "Go this way.", note: unblockedNote("Go this way.")},
		"no decision": {note: unblockedNote("")},
	} {
		t.Run(name, func(t *testing.T) {
			repo, root, assignment := proposalAssignment(t, "decision-note")
			projected, _ := mustCurrentTask(t, assignment.Owner.Projection, "assigned")
			if tc.decision != "" {
				writeTaskFile(t, filepath.Join(projected.Dir, "decision.md"), tc.decision)
			}
			if err := MoveTaskDir(assignment.Owner.Projection, projected, StateBlocked); err != nil {
				t.Fatal(err)
			}
			if _, err := AcceptForkProjection(repo, root, "assigned", assignment.Owner); err != nil {
				t.Fatal(err)
			}
			if tc.answer != "" { // acceptance never takes the fork's answer, so the person writes it in the project's copy too
				canonical, _ := mustCurrentTask(t, root, "assigned")
				if err := recordResolution(filepath.Join(canonical.Dir, "decision.md"), tc.answer); err != nil {
					t.Fatal(err)
				}
			}
			if code, err := tasksFolderUnblock(repo, root, []string{"assigned"}); code != 0 || err != nil {
				t.Fatalf("plain unblock = (%d, %v)", code, err)
			}
			owner, _, err := ReadTaskOwnerRecord(root, "assigned")
			if err != nil || owner.Fork == nil {
				t.Fatalf("owner = %+v, %v", owner.Fork, err)
			}
			if err := syncForkDecision(repo, root, "assigned", *owner.Fork, false); err != nil { // a retried unblock
				t.Fatal(err)
			}
			resumed := selectResumedForkDecisionAfterUnblock(t, repo, root, assignment)
			if err := PrepareForkProjectionForRun(repo, root, "assigned", resumed.Owner); err != nil {
				t.Fatal(err)
			}
			if _, err := AcceptForkProjection(repo, root, "assigned", resumed.Owner); err != nil {
				t.Fatal(err)
			}
			canonical, _ := mustCurrentTask(t, root, "assigned")
			if log := readFileString(filepath.Join(canonical.Dir, "log.md")); strings.Count(log, "- "+tc.note+"\n") != 1 {
				t.Errorf("project log after the fork's acceptance:\n%s\nwant one %q line", log, tc.note)
			}
		})
	}
}

// The fork's box can write its own copy of decision.md, Resolution line and all, and acceptance
// copies that copy over the project's. An answer the box wrote never reaches the project's file,
// where it would read as the person's: the project keeps the fork's question, unanswered, the task
// stays blocked, and the fork resumes with the answer a person gives in the project.
func TestForkAcceptanceNeverTakesTheForksAnswer(t *testing.T) {
	const forged, answer = "Retry the write without checking.", "Leave it for manual repair."
	for name, plant := range map[string]func(string) string{
		"on the Resolution line": func(rendered string) string {
			return strings.Replace(rendered, decisionResolutionLine, decisionResolutionPrefix+" "+forged+"\n", 1)
		},
		"above the Resolution line": func(rendered string) string {
			return strings.Replace(rendered, "---\n", decisionResolutionPrefix+" "+forged+"\n\n---\n", 1)
		},
		"below the Resolution line": func(rendered string) string {
			return rendered + decisionResolutionPrefix + " " + forged + "\n"
		},
		"indented": func(rendered string) string {
			return strings.Replace(rendered, decisionResolutionLine, "  "+decisionResolutionPrefix+" "+forged+"\n", 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			repo, root, assignment := proposalAssignment(t, "decision-forged")
			projected, _ := mustCurrentTask(t, assignment.Owner.Projection, "assigned")
			writeTaskFile(t, filepath.Join(projected.Dir, "decision.md"), plant(RenderDecision(projected.ID, Decision{
				Question: forkDecisionQuestion, Why: "Retrying may duplicate a partial write.",
				Options: []string{"A — retry the write", "B — leave it for manual repair"}, Recommendation: "A — retry the write",
			})))
			if err := MoveTaskDir(assignment.Owner.Projection, projected, StateBlocked); err != nil {
				t.Fatal(err)
			}
			if _, err := AcceptForkProjection(repo, root, "assigned", assignment.Owner); err != nil {
				t.Fatal(err)
			}
			canonical, _ := mustCurrentTask(t, root, "assigned")
			body := readFileString(filepath.Join(canonical.Dir, "decision.md"))
			if canonical.State != StateBlocked || decisionQuestion(body) != forkDecisionQuestion ||
				decisionResolution(body) != "" || strings.Contains(body, forged) {
				t.Fatalf("after acceptance the project's task is %s with decision.md\n%s\nwant it blocked on the fork's question, unanswered and without the box's answer", canonical.State, body)
			}
			if code, err := tasksFolderUnblock(repo, root, []string{"assigned"}); code != 2 || err == nil {
				t.Errorf("unblock without an answer = (%d, %v), want the no-resolution refusal", code, err)
			}
			if ids, err := UnblockResolved([]string{root}); err != nil || len(ids) != 0 {
				t.Errorf("coop loop's pre-flight unblocked %v, %v; want the task left for the person", ids, err)
			}
			if canonical, _ = mustCurrentTask(t, root, "assigned"); canonical.State != StateBlocked {
				t.Fatalf("the project's task is %s, want it still blocked", canonical.State)
			}
			resumeForkDecision(t, repo, root, assignment, answer)
		})
	}
}

// The person's answer comes back from the fork as it went, and stays the project's. An answer the box
// writes over it in its copy of the task while it works never reaches the project.
func TestForkAcceptanceKeepsOnlyThePersonsAnswer(t *testing.T) {
	const answer, forged = "Retry after verifying the marker.", "Retry the write without checking."
	repo, root, assignment := acceptedForkDecision(t, "decision-kept", false)
	resumed := selectResumedForkDecision(t, repo, root, assignment, answer)
	if err := PrepareForkProjectionForRun(repo, root, "assigned", resumed.Owner); err != nil {
		t.Fatal(err)
	}
	accept := func() string {
		t.Helper()
		if _, err := AcceptForkProjection(repo, root, "assigned", resumed.Owner); err != nil {
			t.Fatal(err)
		}
		canonical, _ := mustCurrentTask(t, root, "assigned")
		return readFileString(filepath.Join(canonical.Dir, "decision.md"))
	}
	if body := accept(); decisionResolution(body) != answer {
		t.Errorf("after the fork's acceptance the project's decision.md is\n%s\nwant the person's answer %q", body, answer)
	}
	projected, _ := mustCurrentTask(t, resumed.Owner.Projection, "assigned")
	if err := recordResolution(filepath.Join(projected.Dir, "decision.md"), forged); err != nil {
		t.Fatal(err)
	}
	if body := accept(); strings.Contains(body, forged) {
		t.Errorf("the project's decision.md took the box's answer:\n%s", body)
	}
}

// selectResumedForkDecisionAfterUnblock is the fork's next pass taking back a task already unblocked.
func selectResumedForkDecisionAfterUnblock(t *testing.T, repo, root string, original ForkAssignment) ForkAssignment {
	t.Helper()
	resumed, err := AssignForkTask([]string{root}, ForkAssignmentRequest{
		AuthorityRepo: repo, Fork: original.Owner.Fork, WorkspaceRoot: forkspace.Workspace(repo, original.Owner.Fork.Name),
		BaselineHead: original.Owner.BaselineHead, LeaseOwner: testLeaseOwner(),
	})
	if err != nil || resumed.Outcome != ForkAssignmentSelected {
		t.Fatalf("resumed assignment = %+v, %v", resumed, err)
	}
	t.Cleanup(func() { _ = resumed.Lease.Release() })
	return resumed
}
