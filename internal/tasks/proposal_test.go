package tasks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	writeQueueTemplate(t, root, strings.Replace(fallbackTaskTemplate,
		"- [ ] <a small step with a way to check it worked>",
		"- [ ] Run the queue gate\n- [ ] <task-specific step>", 1))
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
	// An imported task folder has the mode coop tasks add gives one, not its private staging mode.
	if info, err := os.Stat(item.Dir); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("imported task folder mode = %v, %v; want 0755", info.Mode().Perm(), err)
	}
	content, err := os.ReadFile(filepath.Join(item.Dir, "task.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Fix retry race", "**Context:** A distinct defect", "**Acceptance criteria:** The defect", "- [ ] Run the queue gate", "- [ ] add the regression test"} {
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

// The fork loop adds a discovered task with the canonical queue's template, read as `coop tasks add`
// reads it: a README.md with another hard link works, and a symbolic link is refused with what to
// do about it.
func TestImportForkProposalReadsAHardLinkedTemplateAndRefusesASymlink(t *testing.T) {
	repo, root, assignment := proposalAssignment(t, "proposal-template")
	shared := t.TempDir()
	writeQueueTemplate(t, shared, strings.Replace(fallbackTaskTemplate,
		"- [ ] <a small step with a way to check it worked>", "- [ ] Run the shared gate\n- [ ] <task-specific step>", 1))
	readme := filepath.Join(root, "README.md")
	if err := os.Link(filepath.Join(shared, "README.md"), readme); err != nil {
		t.Fatal(err)
	}
	writeForkProposal(t, assignment.Owner, testForkProposal(strings.Repeat("c", 32), "Fix the hard link import"))
	imported, err := ImportForkProposals(repo, assignment.Owner.Fork)
	if err != nil || len(imported) != 1 {
		t.Fatalf("import with a hard-linked README = %+v, %v", imported, err)
	}
	item, ok := mustCurrentTask(t, root, imported[0].TaskID)
	if body := readFileString(filepath.Join(item.Dir, "task.md")); !ok || !strings.Contains(body, "- [ ] Run the shared gate") {
		t.Fatalf("imported task did not use the hard-linked template: ok=%v\n%s", ok, body)
	}

	if err := os.Remove(readme); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(shared, "README.md"), readme); err != nil {
		t.Fatal(err)
	}
	writeForkProposal(t, assignment.Owner, testForkProposal(strings.Repeat("d", 32), "Fix the symbolic link import"))
	if _, err := ImportForkProposals(repo, assignment.Owner.Fork); err == nil || !strings.Contains(err.Error(), "replace the link with a copy of the file") {
		t.Fatalf("import with a symlinked README = %v", err)
	}
}

// A box can write the canonical queue, so it can swap the folder an import writes into, or the one
// it stages in, for a link. The import then fails and writes nothing where the link points, also
// when the swap comes after the import checked the queue: publishing goes through folder handles.
func TestImportForkProposalRefusesAQueueFolderSwappedForALink(t *testing.T) {
	for _, folder := range []string{StateTodo, ".coop-proposal-staging"} {
		t.Run(folder, func(t *testing.T) {
			repo, root, assignment := proposalAssignment(t, "proposal-link")
			outside := t.TempDir()
			path := filepath.Join(root, folder)
			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, path); err != nil {
				t.Fatal(err)
			}
			writeForkProposal(t, assignment.Owner, testForkProposal(strings.Repeat("e", 32), "Fix the planted link"))
			if _, err := ImportForkProposals(repo, assignment.Owner.Fork); err == nil {
				t.Fatalf("import through a linked %s succeeded", folder)
			}
			files := map[string]string{"task.md": "# Planted\n", TaskIdentityFile: "{}\n"}
			if err := publishProposalTask(root, StateTodo, strings.Repeat("e", 32), "2026-01-01-planted", files); err == nil ||
				!strings.Contains(err.Error(), "is a link or not a real directory") {
				t.Fatalf("publishing through a linked %s = %v, want a refusal", folder, err)
			}
			if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
				t.Fatalf("import wrote where the link points: %v, %v", entries, err)
			}
		})
	}
}

// An imported task's ID carries the local date, as coop tasks add's IDs do, so it sorts beside
// the tasks made the same day. The zone is picked so that its date differs from UTC's right now.
func TestForkProposalIDUsesTheLocalDate(t *testing.T) {
	offset := -13
	if time.Now().UTC().Hour() >= 12 {
		offset = 13
	}
	saved := time.Local
	time.Local = time.FixedZone("away", offset*3600)
	t.Cleanup(func() { time.Local = saved })

	repo, _, assignment := proposalAssignment(t, "proposal-date")
	proposal := testForkProposal(strings.Repeat("f", 32), "Date the import")
	data := writeForkProposal(t, assignment.Owner, proposal)
	record, err := newForkProposalRecord(assignmentIndex(t, repo, assignment), proposal, data)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Now().Format("2006-01-02") + "-"; !strings.HasPrefix(record.Task.Ref.ID, want) {
		t.Fatalf("imported task ID = %q, want the local date %q", record.Task.Ref.ID, want)
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
	if _, published, err := reviewAndPublishForkCandidate(t, repo, assignment.Owner.Fork, strings.Repeat("b", 40), strings.Repeat("c", 40)); err != nil || !published {
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

// A host process killed while writing a proposal record leaves the writer's temp file, or (from an
// older Coop) a zero-byte record. Neither blocks the fork: the import prepares the proposal again
// and a discard succeeds. A stray name of any other shape still refuses.
func TestForkProposalRecordsSurviveAKilledWriter(t *testing.T) {
	leftovers := func(t *testing.T, repo string, assignment ForkAssignment, id string) string {
		t.Helper()
		dir := forkProposalAssignmentRecordRoot(repo, assignment.Owner.Fork, assignment.Owner.AssignmentID)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string]string{
			"." + id + ".json-4242-1700000000000000000": `{"version":1}`,
			id + ".json": "",
		} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	t.Run("import", func(t *testing.T) {
		repo, root, assignment := proposalAssignment(t, "proposal-killed-import")
		proposal := testForkProposal(strings.Repeat("5", 32), "Survive a killed writer")
		writeForkProposal(t, assignment.Owner, proposal)
		dir := leftovers(t, repo, assignment, proposal.ID)
		imported, err := ImportForkProposals(repo, assignment.Owner.Fork)
		if err != nil || len(imported) != 1 {
			t.Fatalf("import over a killed writer's leftovers = %+v, %v", imported, err)
		}
		if _, ok := mustCurrentTask(t, root, imported[0].TaskID); !ok {
			t.Fatal("the proposal was not imported")
		}
		records, problems := forkProposalRecords(repo, assignment.Owner.Fork)
		if len(problems) != 0 || len(records) != 1 || records[0].Phase != ForkProposalImported {
			t.Fatalf("records = %+v, problems = %v", records, problems)
		}
		if err := os.WriteFile(filepath.Join(dir, "planted.json-1-2"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ImportForkProposals(repo, assignment.Owner.Fork); err == nil || !strings.Contains(err.Error(), "unsupported fork proposal record entry") {
			t.Fatalf("a planted name = %v, want it refused", err)
		}
	})
	t.Run("discard", func(t *testing.T) {
		repo, _, assignment := proposalAssignment(t, "proposal-killed-discard")
		leftovers(t, repo, assignment, strings.Repeat("6", 32))
		if err := DiscardForkTaskStateLocked(repo, assignment.Owner.Fork); err != nil {
			t.Fatalf("discard over a killed writer's leftovers = %v", err)
		}
	})
}

// A host process killed inside removeImportedProposalSource leaves the renamed source in the box's
// outbox. The import and the drain check clear it when the host's record says that proposal was
// imported from exactly those bytes; a leftover name the host has no imported record for, one with
// other content, and any other stray name still refuse, since the box can write any name there.
func TestAnImportedProposalsLeftoverIsClearedOnlyWhenTheHostImportedIt(t *testing.T) {
	leftoverName := func(id string) string { return "." + id + ".json-imported-4242-1700000000000000000" }
	setup := func(t *testing.T, name string) (string, ForkAssignment, string, []byte) {
		t.Helper()
		repo, _, assignment := proposalAssignment(t, name)
		proposal := testForkProposal(strings.Repeat("7", 32), "Clear an interrupted removal")
		data := writeForkProposal(t, assignment.Owner, proposal)
		if imported, err := ImportForkProposals(repo, assignment.Owner.Fork); err != nil || len(imported) != 1 {
			t.Fatalf("import = %+v, %v", imported, err)
		}
		return repo, assignment, proposal.ID, data
	}
	plant := func(t *testing.T, owner ForkTaskOwner, name string, data []byte) string {
		t.Helper()
		path := filepath.Join(ForkProposalOutbox(owner), name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	t.Run("cleared", func(t *testing.T) {
		repo, assignment, id, data := setup(t, "proposal-leftover-cleared")
		leftover := plant(t, assignment.Owner, leftoverName(id), data)
		if err := forkProposalsDrained(repo, assignment.Owner.Fork); err != nil {
			t.Fatalf("drain check over the host's own leftover = %v", err)
		}
		if _, err := os.Lstat(leftover); !os.IsNotExist(err) {
			t.Fatalf("the drain check left the leftover: %v", err)
		}
		plant(t, assignment.Owner, leftoverName(id), data)
		if _, err := ImportForkProposals(repo, assignment.Owner.Fork); err != nil {
			t.Fatalf("import over the host's own leftover = %v", err)
		}
		if _, err := os.Lstat(leftover); !os.IsNotExist(err) {
			t.Fatalf("the import left the leftover: %v", err)
		}
	})
	for _, tc := range []struct{ name, file, body string }{
		{"other content", leftoverName(strings.Repeat("7", 32)), "{}\n"},
		{"no imported record", leftoverName(strings.Repeat("8", 32)), ""},
		{"other stray name", "." + strings.Repeat("7", 32) + ".json-imported-x-1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, assignment, _, data := setup(t, "proposal-leftover-"+strings.ReplaceAll(tc.name, " ", "-"))
			body := data
			if tc.body != "" {
				body = []byte(tc.body)
			}
			stray := plant(t, assignment.Owner, tc.file, body)
			if err := forkProposalsDrained(repo, assignment.Owner.Fork); err == nil || !strings.Contains(err.Error(), "unimported task proposals") {
				t.Fatalf("drain check = %v, want a refusal", err)
			}
			if _, err := ImportForkProposals(repo, assignment.Owner.Fork); err == nil || !strings.Contains(err.Error(), "unsupported entry") {
				t.Fatalf("import = %v, want a refusal", err)
			}
			if _, err := os.Lstat(stray); err != nil {
				t.Fatalf("a refused entry was removed: %v", err)
			}
		})
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
