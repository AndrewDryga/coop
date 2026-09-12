package tasks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func proposalAssignment(t *testing.T, name string) (string, string, ForkAssignment) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "tasks")
	taskForLease(t, root, StateTodo, "assigned")
	repo := filepath.Join(t.TempDir(), "project")
	workspace, identity := testAssignmentFork(t, repo, name)
	assignment, err := AssignForkTask([]string{root}, ForkAssignmentRequest{
		AuthorityRepo: repo, Fork: identity, WorkspaceRoot: workspace,
		BaselineHead: strings.Repeat("a", 40), LeaseOwner: testLeaseOwner(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	return repo, root, assignment
}

func testForkProposal(id, title string) ForkTaskProposal {
	return ForkTaskProposal{
		Version: forkProposalVersion, ID: id, Kind: ForkProposalTask, Title: title,
		Context:    "A distinct defect was found while implementing the assigned task.",
		Acceptance: "The defect has a focused regression test and the repository gate is green.",
		Approach:   "Reproduce it, fix the narrow cause, and run the focused and repository gates.",
		Subtasks:   []string{"add the regression test", "implement and verify the fix"},
	}
}

func writeForkProposal(t *testing.T, owner ForkTaskOwner, proposal ForkTaskProposal) []byte {
	t.Helper()
	body, err := json.Marshal(proposal)
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, '\n')
	if err := os.WriteFile(filepath.Join(ForkProposalOutbox(owner), proposal.ID+".json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	return body
}

func assignmentIndex(t *testing.T, repo string, assignment ForkAssignment) ForkAssignmentIndex {
	t.Helper()
	indexes, problems := IndexedForkAssignments(repo, assignment.Owner.Fork)
	if len(problems) != 0 || len(indexes) != 1 {
		t.Fatalf("assignment indexes = %+v, problems=%v", indexes, problems)
	}
	return indexes[0]
}

func TestImportForkProposalCreatesOneCanonicalTaskIdempotently(t *testing.T) {
	repo, root, assignment := proposalAssignment(t, "proposal")
	proposal := testForkProposal(strings.Repeat("a", 32), "Fix retry race")
	body := writeForkProposal(t, assignment.Owner, proposal)

	imported, err := ImportForkProposals(repo, assignment.Owner.Fork)
	if err != nil || len(imported) != 1 {
		t.Fatalf("import proposals = %+v, %v", imported, err)
	}
	item, ok := mustCurrentTask(t, root, imported[0].TaskID)
	if !ok || item.State != StateTodo {
		t.Fatalf("imported canonical task = %+v, ok=%v", item, ok)
	}
	content, err := os.ReadFile(filepath.Join(item.Dir, "task.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Fix retry race", "**Context:** A distinct defect", "**Acceptance criteria:** The defect", "- [ ] add the regression test"} {
		if !strings.Contains(string(content), want) {
			t.Errorf("imported task.md missing %q:\n%s", want, content)
		}
	}
	records, problems := forkProposalRecords(repo, assignment.Owner.Fork)
	if len(problems) != 0 || len(records) != 1 || records[0].Phase != ForkProposalImported {
		t.Fatalf("durable imported receipt = %+v, problems=%v", records, problems)
	}
	if err := os.WriteFile(filepath.Join(ForkProposalOutbox(assignment.Owner), proposal.ID+".json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if again, err := ImportForkProposals(repo, assignment.Owner.Fork); err != nil || len(again) != 0 {
		t.Fatalf("same-id replay = %+v, %v", again, err)
	}
	if got := len(mustReadTaskTree(t, root)); got != 2 {
		t.Fatalf("task count after idempotent replay = %d, want 2", got)
	}

	proposal.Title = "Different content with the same identity"
	writeForkProposal(t, assignment.Owner, proposal)
	if _, err := ImportForkProposals(repo, assignment.Owner.Fork); err == nil || !strings.Contains(err.Error(), "reused with different content") {
		t.Fatalf("changed stable id reuse = %v", err)
	}
}

func TestImportForkProposalReplaysCrashAfterCanonicalCreate(t *testing.T) {
	repo, root, assignment := proposalAssignment(t, "proposal-crash")
	proposal := testForkProposal(strings.Repeat("b", 32), "Recover proposal import")
	data := writeForkProposal(t, assignment.Owner, proposal)
	record, err := newForkProposalRecord(assignmentIndex(t, repo, assignment), proposal, data)
	if err != nil {
		t.Fatal(err)
	}
	if err := createForkProposalRecord(repo, record); err != nil {
		t.Fatal(err)
	}
	next, transitioned, err := materializeForkProposal(record)
	if err != nil || !transitioned || next.Phase != ForkProposalImported {
		t.Fatalf("materialize before simulated crash = %+v, transitioned=%v err=%v", next, transitioned, err)
	}
	// Deliberately omit updateForkProposalRecord: the host died after canonical rename.
	imported, err := ImportForkProposals(repo, assignment.Owner.Fork)
	if err != nil || len(imported) != 1 || imported[0].TaskID != record.Task.Ref.ID {
		t.Fatalf("crash replay = %+v, %v", imported, err)
	}
	item, ok := mustCurrentTask(t, root, record.Task.Ref.ID)
	if !ok || item.State != StateTodo {
		t.Fatalf("replayed canonical task = %+v, ok=%v", item, ok)
	}
	records, problems := forkProposalRecords(repo, assignment.Owner.Fork)
	if len(problems) != 0 || len(records) != 1 || records[0].Phase != ForkProposalImported {
		t.Fatalf("replay receipt = %+v, problems=%v", records, problems)
	}
}

func TestForkProposalValidationFailsClosed(t *testing.T) {
	t.Run("unknown field", func(t *testing.T) {
		repo, _, assignment := proposalAssignment(t, "proposal-unknown")
		id := strings.Repeat("c", 32)
		body := `{"version":1,"id":"` + id + `","kind":"task","title":"Unsafe proposal","context":"context","acceptance":"acceptance","approach":"approach","subtasks":["test"],"destination":"/tmp"}`
		if err := os.WriteFile(filepath.Join(ForkProposalOutbox(assignment.Owner), id+".json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ImportForkProposals(repo, assignment.Owner.Fork); err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("unknown-field proposal = %v", err)
		}
	})

	t.Run("control character", func(t *testing.T) {
		repo, _, assignment := proposalAssignment(t, "proposal-control")
		proposal := testForkProposal(strings.Repeat("d", 32), "Unsafe\u001btitle")
		writeForkProposal(t, assignment.Owner, proposal)
		if _, err := ImportForkProposals(repo, assignment.Owner.Fork); err == nil || !strings.Contains(err.Error(), "safe line") {
			t.Fatalf("control-character proposal = %v", err)
		}
	})

	t.Run("symlink file", func(t *testing.T) {
		repo, _, assignment := proposalAssignment(t, "proposal-symlink")
		id := strings.Repeat("e", 32)
		target := filepath.Join(t.TempDir(), "outside.json")
		if err := os.WriteFile(target, []byte(`{"version":1}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(ForkProposalOutbox(assignment.Owner), id+".json")); err != nil {
			t.Fatal(err)
		}
		if _, err := ImportForkProposals(repo, assignment.Owner.Fork); err == nil || !strings.Contains(err.Error(), "single-link regular file") {
			t.Fatalf("symlink proposal = %v", err)
		}
	})

	t.Run("hardlink file", func(t *testing.T) {
		repo, _, assignment := proposalAssignment(t, "proposal-hardlink")
		proposal := testForkProposal(strings.Repeat("f", 32), "Hardlink proposal")
		body, _ := json.Marshal(proposal)
		target := filepath.Join(t.TempDir(), "proposal.json")
		if err := os.WriteFile(target, body, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(target, filepath.Join(ForkProposalOutbox(assignment.Owner), proposal.ID+".json")); err != nil {
			t.Skipf("hardlinks unavailable: %v", err)
		}
		if _, err := ImportForkProposals(repo, assignment.Owner.Fork); err == nil || !strings.Contains(err.Error(), "single-link regular file") {
			t.Fatalf("hardlink proposal = %v", err)
		}
	})

	t.Run("symlink ancestor", func(t *testing.T) {
		repo, _, assignment := proposalAssignment(t, "proposal-ancestor")
		outbox := ForkProposalOutbox(assignment.Owner)
		if err := os.Remove(outbox); err != nil {
			t.Fatal(err)
		}
		outside := t.TempDir()
		if err := os.Symlink(outside, outbox); err != nil {
			t.Fatal(err)
		}
		if _, err := ImportForkProposals(repo, assignment.Owner.Fork); err == nil || !strings.Contains(err.Error(), "not a real directory") {
			t.Fatalf("symlink-ancestor proposal = %v", err)
		}
	})
}

func TestForkProposalOutboxEnumerationIsBounded(t *testing.T) {
	repo, _, assignment := proposalAssignment(t, "proposal-bounded")
	for i := 0; i <= forkProposalCountLimit; i++ {
		id := strings.Repeat(string(rune('a'+i%6)), 31) + string("0123456789abcdef"[i%16])
		if err := os.WriteFile(filepath.Join(ForkProposalOutbox(assignment.Owner), id+".json"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ImportForkProposals(repo, assignment.Owner.Fork); err == nil || !strings.Contains(err.Error(), "32-entry limit") {
		t.Fatalf("oversized proposal outbox = %v", err)
	}
}

func TestForkProposalReceiptsDoNotCreateALifetimeCap(t *testing.T) {
	repo, root, assignment := proposalAssignment(t, "proposal-long-lived")
	for i := 0; i < forkProposalCountLimit+1; i++ {
		proposal := testForkProposal(fmt.Sprintf("%032x", i+1), fmt.Sprintf("Follow up %d", i+1))
		writeForkProposal(t, assignment.Owner, proposal)
		imported, err := ImportForkProposals(repo, assignment.Owner.Fork)
		if err != nil || len(imported) != 1 {
			t.Fatalf("proposal %d import = %+v, %v", i+1, imported, err)
		}
	}
	records, problems := forkProposalRecords(repo, assignment.Owner.Fork)
	if len(problems) != 0 || len(records) != forkProposalCountLimit+1 {
		t.Fatalf("long-lived proposal receipts = %d, problems=%v", len(records), problems)
	}
	if got := len(mustReadTaskTree(t, root)); got != forkProposalCountLimit+2 {
		t.Fatalf("canonical tasks after long-lived proposal stream = %d", got)
	}
}

func TestForkCandidateRequiresProposalOutboxDrained(t *testing.T) {
	repo, root, assignment := proposalAssignment(t, "proposal-candidate")
	projected, _ := mustCurrentTask(t, assignment.Owner.Projection, "assigned")
	if err := RewriteSubtasks(projected.Dir, []Subtask{{Text: "required checks passed", Done: true}}); err != nil {
		t.Fatal(err)
	}
	if err := MoveTaskDir(assignment.Owner.Projection, projected, StateDone); err != nil {
		t.Fatal(err)
	}
	if _, err := AcceptForkProjection(repo, root, "assigned", assignment.Owner); err != nil {
		t.Fatal(err)
	}
	writeForkProposal(t, assignment.Owner, testForkProposal(strings.Repeat("1", 32), "Follow up"))
	if _, _, err := PublishForkCandidate(repo, assignment.Owner.Fork, strings.Repeat("b", 40), strings.Repeat("c", 40)); err == nil || !strings.Contains(err.Error(), "unimported task proposals") {
		t.Fatalf("candidate with pending proposal = %v", err)
	}
	if _, err := ImportForkProposals(repo, assignment.Owner.Fork); err != nil {
		t.Fatal(err)
	}
	if _, published, err := PublishForkCandidate(repo, assignment.Owner.Fork, strings.Repeat("b", 40), strings.Repeat("c", 40)); err != nil || !published {
		t.Fatalf("candidate after proposal import = published %v err %v", published, err)
	}
}

func TestForkProposalBacklogUsesCanonicalQueueBacklog(t *testing.T) {
	repo, root, assignment := proposalAssignment(t, "proposal-backlog")
	proposal := testForkProposal(strings.Repeat("2", 32), "Large follow-up")
	proposal.Kind = ForkProposalBacklog
	writeForkProposal(t, assignment.Owner, proposal)
	imported, err := ImportForkProposals(repo, assignment.Owner.Fork)
	if err != nil || len(imported) != 1 || imported[0].Kind != ForkProposalBacklog {
		t.Fatalf("backlog import = %+v, %v", imported, err)
	}
	backlog := mustReadBacklog(t, root)
	if len(backlog) != 1 || backlog[0].ID != imported[0].TaskID {
		t.Fatalf("canonical backlog = %+v", backlog)
	}
}

func TestForcedForkDiscardKeepsImportedTaskAndDropsPreparedProposal(t *testing.T) {
	repo, root, assignment := proposalAssignment(t, "proposal-discard")
	index := assignmentIndex(t, repo, assignment)
	preparedProposal := testForkProposal(strings.Repeat("3", 32), "Discard pending proposal")
	preparedData := writeForkProposal(t, assignment.Owner, preparedProposal)
	prepared, err := newForkProposalRecord(index, preparedProposal, preparedData)
	if err != nil {
		t.Fatal(err)
	}
	if err := createForkProposalRecord(repo, prepared); err != nil {
		t.Fatal(err)
	}

	importedProposal := testForkProposal(strings.Repeat("4", 32), "Retain imported proposal")
	importedData := writeForkProposal(t, assignment.Owner, importedProposal)
	imported, err := newForkProposalRecord(index, importedProposal, importedData)
	if err != nil {
		t.Fatal(err)
	}
	if err := createForkProposalRecord(repo, imported); err != nil {
		t.Fatal(err)
	}
	next, transitioned, err := materializeForkProposal(imported)
	if err != nil || !transitioned {
		t.Fatalf("materialize imported proposal = %+v, %v", next, err)
	}
	if err := updateForkProposalRecord(repo, imported, next); err != nil {
		t.Fatal(err)
	}
	if err := removeImportedProposalSource(repo, assignment.Owner, next); err != nil {
		t.Fatal(err)
	}

	if err := DiscardForkTaskStateLocked(repo, assignment.Owner.Fork); err != nil {
		t.Fatal(err)
	}
	if _, ok := mustCurrentTask(t, root, prepared.Task.Ref.ID); ok {
		t.Fatalf("forced discard materialized prepared proposal %s", prepared.Task.Ref.ID)
	}
	if _, ok := mustCurrentTask(t, root, next.Task.Ref.ID); !ok {
		t.Fatalf("forced discard removed imported canonical task %s", next.Task.Ref.ID)
	}
	if records, problems := forkProposalRecords(repo, assignment.Owner.Fork); len(records) != 0 || len(problems) != 0 {
		t.Fatalf("forced discard left proposal state %+v problems %v", records, problems)
	}
	item, ok := mustCurrentTask(t, root, "assigned")
	if !ok || item.State != StateTodo {
		t.Fatalf("discarded assignment task = %+v, ok=%v", item, ok)
	}
}

func TestForkProposalCollisionDoesNotMutateExistingTask(t *testing.T) {
	repo, root, assignment := proposalAssignment(t, "proposal-collision")
	proposal := testForkProposal(strings.Repeat("5", 32), "Existing collision")
	data := writeForkProposal(t, assignment.Owner, proposal)
	record, err := newForkProposalRecord(assignmentIndex(t, repo, assignment), proposal, data)
	if err != nil {
		t.Fatal(err)
	}
	existing := taskForLease(t, root, StateTodo, record.Task.Ref.ID)
	before, err := os.ReadFile(filepath.Join(existing.Dir, "task.md"))
	if err != nil {
		t.Fatal(err)
	}
	if err := createForkProposalRecord(repo, record); err != nil {
		t.Fatal(err)
	}
	_, err = ImportForkProposals(repo, assignment.Owner.Fork)
	if err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("proposal collision = %v", err)
	}
	after, readErr := os.ReadFile(filepath.Join(existing.Dir, "task.md"))
	if readErr != nil || !bytes.Equal(before, after) {
		t.Fatalf("collision mutated existing task: %v", readErr)
	}
}
