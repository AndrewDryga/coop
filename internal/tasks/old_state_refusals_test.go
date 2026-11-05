package tasks

import (
	"encoding/json"
	"strings"
	"testing"
)

// readLeaseRegistryFile returns the exact bytes of a record in the host-only lease registry.
func readLeaseRegistryFile(t *testing.T, name string) []byte {
	t.Helper()
	registry, err := OpenLeaseAuthorityRoot()
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	data, err := ReadTaskMetadataFile(registry, name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// A completion window from Coop v11.3.0 or earlier can hold a fingerprint with no tree_format, or
// the singular allowed_done_departure the plural field replaced. Both are refused by name when the
// index is read, and the index file is left exactly as it was.
func TestCompletionWindowRefusesOldRecordsByName(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(record *CompletionWindowRecord, task string)
		want   string
	}{
		{"fingerprint without tree_format", func(record *CompletionWindowRecord, task string) {
			fingerprint := record.Baseline[task]
			fingerprint.TreeFormat = 0
			record.Baseline[task] = fingerprint
		}, "without tree_format"},
		{"singular allowed_done_departure", func(record *CompletionWindowRecord, task string) {
			record.AllowedDoneDeparture = task
		}, "singular allowed_done_departure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			archived := taskWithCompletedChecklist(t, root, StateInProgress, "archived")
			if err := CompleteTrustedTask("", root, archived); err != nil {
				t.Fatal(err)
			}
			windows, err := BeginCompletionWindows([]string{root})
			if err != nil {
				t.Fatal(err)
			}
			id := windows.windows[0].id
			if err := unlockLeaseFile(windows.windows[0].live); err != nil {
				t.Fatal(err)
			}
			windows.windows[0].live = nil
			index, err := ReadCompletionWindowIndex(root)
			if err != nil {
				t.Fatal(err)
			}
			record := index.Windows[id]
			tc.mutate(&record, archived.ID)
			index.Windows[id] = record
			if err := writeCompletionWindowIndex(root, index); err != nil {
				t.Fatal(err)
			}
			name, err := CompletionWindowIndexName(root)
			if err != nil {
				t.Fatal(err)
			}
			before := readLeaseRegistryFile(t, name)
			if _, err := ReadCompletionWindowIndex(root); err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "v12.1.0") {
				t.Fatalf("ReadCompletionWindowIndex = %v; want a refusal naming %q and the converting build", err, tc.want)
			}
			if err := ReconcileCompletionWindows([]string{root}); err == nil {
				t.Fatal("startup recovery went on over a window it cannot read")
			}
			if after := readLeaseRegistryFile(t, name); string(after) != string(before) {
				t.Fatalf("the window index changed: %s", after)
			}
		})
	}
}

// A pending final review recorded before the device left the tree digest has a fingerprint with no
// tree_format. It is refused by name, never read as a changed archive, and its file stays as it was.
func TestPendingReviewRefusesAFingerprintWithoutTreeFormat(t *testing.T) {
	repo, root, _, assignment, done, plan := pendingReviewTestCompletion(t)
	if err := assignment.Lease.MarkCompletedForReview(repo, done, plan); err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	record, _, err := readPendingReviewRecord(root, done.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw["fingerprint"].(map[string]any), "tree_format")
	if data, err = json.Marshal(raw); err != nil {
		t.Fatal(err)
	}
	name, err := pendingReviewRecordName(root, done.ID)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := OpenLeaseAuthorityRoot()
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	if err := AtomicWriteTaskFile(registry, name, append(data, '\n')); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readPendingReviewRecord(root, done.ID); err == nil || !strings.Contains(err.Error(), "without tree_format") || !strings.Contains(err.Error(), "v12.1.0") {
		t.Fatalf("readPendingReviewRecord = %v; want a refusal naming the missing tree_format and the converting build", err)
	}
	if _, err := LoadPendingReviews(repo, []string{root}); err == nil || !strings.Contains(err.Error(), "without tree_format") {
		t.Fatalf("LoadPendingReviews = %v; want the same refusal", err)
	}
	if after := readLeaseRegistryFile(t, name); string(after) != string(append(data, '\n')) {
		t.Fatalf("the pending review record changed: %s", after)
	}
}

// A version 1 task owner record is a claim made by Coop v8.1.0 or earlier. It is refused by name, not
// read as no owner, so the task stays claimed, and the record is left exactly as it was.
func TestTaskOwnerRecordVersion1IsRefusedByName(t *testing.T) {
	root := t.TempDir()
	name, err := taskOwnerRecordName(root, "claimed-long-ago")
	if err != nil {
		t.Fatal(err)
	}
	registry, err := OpenLeaseAuthorityRoot()
	if err != nil {
		t.Fatal(err)
	}
	body := `{"version":1,"task_id":"claimed-long-ago","source":"interactive-claim","user":"ada","host":"workstation","claimed_at":"2026-01-02T03:04:05Z"}` + "\n"
	if err := AtomicWriteTaskFile(registry, name, []byte(body)); err != nil {
		t.Fatal(err)
	}
	_ = registry.Close()
	if record, ok, err := ReadTaskOwnerRecord(root, "claimed-long-ago"); err == nil || ok ||
		!strings.Contains(err.Error(), "version 1 owner record") || !strings.Contains(err.Error(), "coop tasks release claimed-long-ago") {
		t.Fatalf("ReadTaskOwnerRecord = %+v, ok=%v, err=%v; want a refusal naming version 1 and the release command", record, ok, err)
	}
	if after := readLeaseRegistryFile(t, name); string(after) != body {
		t.Fatalf("the version 1 owner record changed: %s", after)
	}
}

// A claim an older Linux build recorded names its process in a token shape this Coop no longer
// reads. It is refused by name with the command that clears it, never as a malformed record.
func TestTaskOwnerRecordWithAnOldLinuxTokenIsRefusedByName(t *testing.T) {
	record := humanOwnerRecordForTest("claimed-on-linux")
	record.Actor, record.ActorPID, record.ActorStart = "codex", 4242, "linux-proc-v1:boot:123"
	err := validateTaskOwnerRecord(record, "claimed-on-linux")
	if err == nil || !strings.Contains(err.Error(), "Linux format") || !strings.Contains(err.Error(), "coop tasks release claimed-on-linux") {
		t.Fatalf("validateTaskOwnerRecord = %v; want a refusal naming the old Linux format and the release command", err)
	}
}
