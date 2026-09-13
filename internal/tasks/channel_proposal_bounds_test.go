package tasks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteForkProposalSerializedLimitMatchesImport(t *testing.T) {
	for _, delta := range []int{-1, 0, 1} {
		t.Run(map[int]string{-1: "below", 0: "exact", 1: "over"}[delta], func(t *testing.T) {
			repo, _, assignment := proposalAssignment(t, "serialized-limit")
			outbox := ForkProposalOutbox(assignment.Owner)
			draft := TaskDraft{
				Kind: ForkProposalTask, Title: "Serialized limit",
				Context:    strings.Repeat("x", TaskBlockLimit),
				Acceptance: strings.Repeat("a", TaskBlockLimit), Approach: "p",
			}
			for range 20 {
				draft.Subtasks = append(draft.Subtasks, strings.Repeat("s", TaskLineLimit))
			}
			proposal, err := draft.proposal()
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.MarshalIndent(proposal, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			// Generated IDs have fixed width; include the writer's final newline.
			draft.Approach += strings.Repeat("p", TaskProposalFileLimit+delta-len(encoded)-1)
			if len(draft.Approach) > TaskBlockLimit {
				t.Fatal("fixture exceeds an individual field bound")
			}
			path, err := WriteForkProposal(outbox, draft)
			if delta > 0 {
				if err == nil || !strings.Contains(err.Error(), "serialized JSON") {
					t.Fatalf("oversized write = %q, %v", path, err)
				}
				entries, readErr := os.ReadDir(outbox)
				if readErr != nil || len(entries) != 0 {
					t.Fatalf("refusal left an outbox file: %v, %v", entries, readErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(outbox)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			data, _, err := readForkProposalFile(root, filepath.Base(path))
			if err != nil || len(data) != TaskProposalFileLimit+delta {
				t.Fatalf("reader boundary = %d bytes, %v", len(data), err)
			}
			imported, err := ImportForkProposals(repo, assignment.Owner.Fork)
			if err != nil || len(imported) != 1 {
				t.Fatalf("accepted proposal cannot import: %v, %v", imported, err)
			}
		})
	}
}
