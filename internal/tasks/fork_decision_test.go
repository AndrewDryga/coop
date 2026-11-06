package tasks

import (
	"bytes"
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
