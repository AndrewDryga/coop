package networkstate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/egress"
)

// plantOld files a record an older coop wrote and returns its name.
func plantOld(t *testing.T, s *Store, name string, record any) string {
	t.Helper()
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Path(), name), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return name
}

func approvedProject(t *testing.T, s *Store) string {
	t.Helper()
	project := t.TempDir()
	if err := approve(s, project, egress.Filtered, []egress.Rule{rule("docs.example.com")}, nil); err != nil {
		t.Fatal(err)
	}
	return project
}

func canonicalOf(t *testing.T, project string) string {
	t.Helper()
	canonical, err := canonicalPath(project)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

// wantOldRecords asserts err refuses by name, with the fix, and returns the refusal.
func wantOldRecords(t *testing.T, err error, names ...string) *OldRecordsError {
	t.Helper()
	var refusal *OldRecordsError
	if !errors.As(err, &refusal) {
		t.Fatalf("error = %v; want a refusal that names the old records", err)
	}
	for _, name := range names {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("refusal %q does not name %s", err, name)
		}
	}
	if !strings.Contains(err.Error(), "coop net forget") && !strings.Contains(err.Error(), "Coop v12.1.0") {
		t.Fatalf("refusal %q names no fix", err)
	}
	return refusal
}

// An approval an older build filed under the keyed id is not read, and its project is refused by
// name through every entry: launch, preview, review and the withdrawal lookup. A project it cannot
// belong to is not.
func TestKeyedApprovalRefusesItsProjectByNameAndNoOther(t *testing.T) {
	s := openStore(t)
	project, other := approvedProject(t, s), approvedProject(t, s)
	legacy := legacyProjectRecords(t, s, project)
	name := approvalRecord(legacy)
	requests := []egress.Rule{rule("docs.example.com")}

	_, err := s.Admit(project, Admission{Requests: requests})
	wantOldRecords(t, err, name, "coop net forget --project "+canonicalOf(t, project))
	_, err = s.admissionPreview(project, Admission{Requests: requests})
	wantOldRecords(t, err, name)
	_, err = s.ReviewApproval(project, egress.Filtered, requests, nil, nil)
	wantOldRecords(t, err, name)
	_, err = s.Withdrawn(project)
	wantOldRecords(t, err, name)
	_, err = PreviewAdmission(s.Path(), project, nil, Admission{Requests: requests})
	wantOldRecords(t, err, name)

	if _, err := s.Admit(other, Admission{Requests: requests}); err != nil {
		t.Fatalf("an unrelated project was refused over another project's old record: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(s.Path(), name)); err != nil {
		t.Fatalf("the refusal changed the old record: %v", err)
	}
}

// A withdrawal is a barrier. An older build's, under the keyed id, must still refuse its project
// (it used to move to the plain id); Forget then files the current barrier and removes the old
// files, so the project stays refused and nothing needs the old reader.
func TestKeyedWithdrawalStillRefusesAndForgetKeepsTheBarrier(t *testing.T) {
	s := openStore(t)
	project := t.TempDir()
	legacy := s.keyedProjectID(canonicalOf(t, project))
	name := plantOld(t, s, withdrawalRecord(legacy), Withdrawal{Version: 1, ProjectID: legacy})
	for _, input := range []Admission{{}, {Requests: []egress.Rule{rule("example.com")}}} {
		_, err := s.Admit(project, input)
		wantOldRecords(t, err, name)
	}
	_, err := s.Withdrawn(project)
	wantOldRecords(t, err, name)

	record, err := s.ApprovalAt(project)
	if err != nil {
		t.Fatal(err)
	}
	if removed, err := s.Forget(context.Background(), record); err != nil || !removed {
		t.Fatalf("Forget = %v, %v", removed, err)
	}
	if _, err := os.Lstat(filepath.Join(s.Path(), name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the old withdrawal survived Forget: %v", err)
	}
	if withdrawn, err := s.Withdrawn(project); err != nil || !withdrawn {
		t.Fatalf("after Forget withdrawn = %v, %v; the barrier was lost", withdrawn, err)
	}
	_, err = s.Admit(project, Admission{})
	if pending := (*PendingApproval)(nil); !errors.As(err, &pending) || !strings.Contains(pending.Reason, "withdrawn") {
		t.Fatalf("a launch after Forget = %v; want the withdrawal's refusal", err)
	}
}

// Forget takes back an older build's approval with its private anchor and leaves the project
// refused; approving afterwards works.
func TestForgetRemovesAnOlderApprovalAndItsAnchor(t *testing.T) {
	s := openStore(t)
	project := approvedProject(t, s)
	legacy := legacyProjectRecords(t, s, project)
	record, err := s.ApprovalAt(project)
	if err != nil {
		t.Fatal(err)
	}
	if removed, err := s.Forget(context.Background(), record); err != nil || !removed {
		t.Fatalf("Forget = %v, %v", removed, err)
	}
	matches, err := filepath.Glob(filepath.Join(s.Path(), "project-"+legacy+"-*.anchor"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("the old anchor survived: %v, %v", matches, err)
	}
	if _, err := os.Lstat(filepath.Join(s.Path(), approvalRecord(legacy))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the old approval survived: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(project, ProjectApprovalMarker)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the checkout's marker survived the forget: %v", err)
	}
	if pending := pendingAfter(t, s, project, Admission{}); pending == nil {
		t.Fatal("a launch after forgetting was not refused")
	}
	if withdrawn, err := s.Withdrawn(project); err != nil || !withdrawn {
		t.Fatalf("withdrawn after forgetting = %v, %v", withdrawn, err)
	}
	if err := approve(s, project, egress.Filtered, []egress.Rule{rule("docs.example.com")}, nil); err != nil {
		t.Fatalf("approving after the forget: %v", err)
	}
}

// A version 3 approval or version 1 withdrawal under the project's own plain id is refused too,
// and Forget clears it without losing the barrier.
func TestOldVersionUnderThePlainIDIsRefused(t *testing.T) {
	s := openStore(t)
	project := t.TempDir()
	id := s.projectKey(canonicalOf(t, project))
	name := plantOld(t, s, withdrawalRecord(id), Withdrawal{Version: 1, ProjectID: id})
	_, err := s.Admit(project, Admission{})
	wantOldRecords(t, err, name)
	record, err := s.ApprovalAt(project)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Forget(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if found, err := s.readWithdrawal(id, withdrawalVersion); err != nil || !found {
		t.Fatalf("withdrawal under the plain id after Forget = %v, %v", found, err)
	}

	approved := approvedProject(t, s)
	aid := s.projectKey(canonicalOf(t, approved))
	approval, err := s.approval(aid)
	if err != nil {
		t.Fatal(err)
	}
	approval.Version = 3
	aname := plantOld(t, s, approvalRecord(aid), approval)
	_, err = s.Admit(approved, Admission{})
	wantOldRecords(t, err, aname)
}

// A run record at version 1 refuses the project it names, by name, and is not read.
func TestExecutionVersionOneRefusesItsProject(t *testing.T) {
	s, record := executionFixture(t)
	path := filepath.Join(s.Path(), "execution-"+record.ID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	raw["version"] = 1
	if data, err = json.Marshal(raw); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Execution(record.ID); err == nil || !strings.Contains(err.Error(), "older coop") {
		t.Fatalf("Execution of a version 1 record = %v; want a refusal that says so", err)
	}
	mode := egress.Filtered
	_, err = s.Admit(record.Project, Admission{InvocationMode: &mode})
	wantOldRecords(t, err, "execution-"+record.ID+".json")
	if _, err := s.Admit(t.TempDir(), Admission{InvocationMode: &mode}); err != nil {
		t.Fatalf("a project the run does not name was refused: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(data) {
		t.Fatalf("the refusal changed the run record: %v", err)
	}
}

// Without the owner key nothing proves whose a keyed file is, so any old-format approval or
// withdrawal refuses every project.
func TestOldRecordsWithoutTheKeyRefuseEveryProject(t *testing.T) {
	s := openStore(t)
	stranger := strings.Repeat("ab", 32)
	name := plantOld(t, s, withdrawalRecord(stranger), Withdrawal{Version: 1, ProjectID: stranger})
	project := t.TempDir()
	if found, err := s.oldRecordsFor(canonicalOf(t, project)); err != nil || found != nil {
		t.Fatalf("with the key, another project's old record refused this one: %v, %v", found, err)
	}
	key := s.key
	s.key = nil
	t.Cleanup(func() { s.key = key })
	found, err := s.oldRecordsFor(canonicalOf(t, project))
	if err != nil || found == nil || len(found.Files) != 1 || found.Files[0] != name {
		t.Fatalf("without the key = %+v, %v; want the old withdrawal named", found, err)
	}
}
