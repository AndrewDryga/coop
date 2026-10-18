package networkstate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/AndrewDryga/coop/internal/egress"
)

func approvalGenerationOf(t *testing.T, s *Store) uint64 {
	t.Helper()
	generation, err := s.ApprovalGeneration()
	if err != nil {
		t.Fatal(err)
	}
	return generation
}

// Every approval change moves the counter — an approval, a changed approval, another project's
// approval and a withdrawal alike — the first one to a random non-zero start, each later one
// exactly one step, and nothing else moves it.
func TestApprovalGenerationAdvancesOnEveryApprovalChange(t *testing.T) {
	s, project, other := openStore(t), t.TempDir(), t.TempDir()
	if got := approvalGenerationOf(t, s); got != 0 {
		t.Fatalf("a fresh store starts at generation %d, want 0", got)
	}
	steps := []func() error{
		func() error {
			return approve(s, project, egress.Filtered, []egress.Rule{rule("first.example.com")}, nil)
		},
		func() error {
			return approve(s, project, egress.Filtered, []egress.Rule{rule("second.example.com")}, nil)
		},
		func() error { return approve(s, other, egress.Filtered, []egress.Rule{rule("other.example.com")}, nil) },
		func() error { withdraw(t, s, project); return nil },
	}
	var last uint64
	for i, step := range steps {
		if err := step(); err != nil {
			t.Fatal(err)
		}
		got := approvalGenerationOf(t, s)
		if got == 0 || i > 0 && got != last+1 {
			t.Fatalf("after change %d the generation is %d, after %d", i+1, got, last)
		}
		last = got
	}
	// Reviewing and admitting read approvals; they change none.
	mode := egress.Filtered
	if _, err := s.Admit(other, Admission{InvocationMode: &mode, Requests: []egress.Rule{rule("other.example.com")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReviewApproval(other, egress.Filtered, []egress.Rule{rule("next.example.com")}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := approvalGenerationOf(t, s); got != last {
		t.Fatalf("a review or an admission moved the generation from %d to %d", last, got)
	}
}

// A counter that disappears starts again at a random point: restarting at 1 would repeat a value
// an open session may already hold, and that session would miss this change.
func TestApprovalGenerationNeverRepeatsAfterTheRecordIsGone(t *testing.T) {
	s, project := openStore(t), t.TempDir()
	if err := s.publish(approvalGenerationRecord, []byte(`{"version":1,"generation":1}`), true); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(s.Path(), approvalGenerationRecord)); err != nil {
		t.Fatal(err)
	}
	if err := approve(s, project, egress.Filtered, []egress.Rule{rule("after.example.com")}, nil); err != nil {
		t.Fatal(err)
	}
	if got := approvalGenerationOf(t, s); got == 0 || got == 1 {
		t.Fatalf("the generation after the record was removed is %d: a session that saw 1 misses the change", got)
	}
}

// What an open editor session does when the generation moves, from the store's side: admitting again
// after a widened approval yields a NEW snapshot that reaches the new host, while the snapshot its
// running box proved stays exactly what it was — a box's policy never changes, it is replaced.
func TestReadmissionAfterAnApprovalIsANewSnapshot(t *testing.T) {
	s, project := openStore(t), t.TempDir()
	mode := egress.Filtered
	first := []egress.Rule{rule("proxy.golang.org")}
	if err := approve(s, project, egress.Filtered, first, nil); err != nil {
		t.Fatal(err)
	}
	seen := approvalGenerationOf(t, s)
	running, err := s.Admit(project, Admission{InvocationMode: &mode, Requests: first})
	if err != nil {
		t.Fatal(err)
	}
	widened := []egress.Rule{rule("proxy.golang.org"), rule("storage.googleapis.com")}
	if err := approve(s, project, egress.Filtered, widened, nil); err != nil {
		t.Fatal(err)
	}
	if approvalGenerationOf(t, s) == seen {
		t.Fatal("a widened approval did not move the generation an open session polls")
	}
	next, err := s.Admit(project, Admission{InvocationMode: &mode, Requests: widened})
	if err != nil {
		t.Fatal(err)
	}
	if next.Fingerprint == running.Fingerprint || !next.Domain("storage.googleapis.com", 443).Allowed {
		t.Fatal("admitting again after the approval did not produce a snapshot that reaches the new host")
	}
	kept, err := s.LoadSnapshot(project, running.Fingerprint)
	if err != nil || kept.Domain("storage.googleapis.com", 443).Allowed || !kept.Domain("proxy.golang.org", 443).Allowed {
		t.Fatalf("the running box's snapshot changed after the approval (%v)", err)
	}
}

// Two projects approve under different record locks. The counter has its own lock, so no step is
// lost: a supervisor that read one step must still see the other.
func TestApprovalGenerationLosesNoConcurrentStep(t *testing.T) {
	s := openStore(t)
	if err := s.publish(approvalGenerationRecord, []byte(`{"version":1,"generation":100}`), true); err != nil {
		t.Fatal(err)
	}
	const projects = 6
	var wg sync.WaitGroup
	errs := make(chan error, projects)
	for i := range projects {
		project := t.TempDir()
		wg.Go(func() {
			errs <- approve(s, project, egress.Filtered, []egress.Rule{rule(strings.Repeat("a", i+1) + ".example.com")}, nil)
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := approvalGenerationOf(t, s); got != 100+projects {
		t.Fatalf("%d concurrent approvals advanced the generation from 100 to %d", projects, got)
	}
}

// The record is owner-only like every other authority file, and it carries a count only: it can
// tell a reader that some approval changed, never which project's.
func TestApprovalGenerationIsPrivateAndNamesNoProject(t *testing.T) {
	// A distinctive folder name: a temp dir's own ("002") can occur among the counter's digits.
	s, project := openStore(t), filepath.Join(t.TempDir(), "private-project")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := approve(s, project, egress.Filtered, []egress.Rule{rule("private.example.com")}, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.Path(), approvalGenerationRecord)
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("the approval generation is %v, want a 0600 regular file", info.Mode())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.projectID(project)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := canonicalPath(project)
	if err != nil {
		t.Fatal(err)
	}
	for _, identity := range []string{id, canonical, filepath.Base(project), "private.example.com"} {
		if bytes.Contains(data, []byte(identity)) {
			t.Fatalf("the approval generation names %q: %s", identity, data)
		}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || len(fields) != 2 || fields["version"] == nil || fields["generation"] == nil {
		t.Fatalf("the approval generation holds more than a version and a count: %s (%v)", data, err)
	}
}

// An unreadable counter is never read as "nothing changed", and the next change repairs it from a
// fresh random point instead of failing every later approval.
func TestUnreadableApprovalGenerationIsRepaired(t *testing.T) {
	s, project := openStore(t), t.TempDir()
	if err := s.publish(approvalGenerationRecord, []byte(`{"version":1,"generation":"seven"}`), true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApprovalGeneration(); err == nil {
		t.Fatal("a malformed approval generation read as a count")
	}
	if err := approve(s, project, egress.Filtered, []egress.Rule{rule("kept.example.com")}, nil); err != nil {
		t.Fatalf("approve with an unreadable generation = %v, want the counter repaired", err)
	}
	if got := approvalGenerationOf(t, s); got == 0 {
		t.Fatal("the repaired generation reads as nothing changed")
	}
}

// A change that cannot be announced is still saved: it applies to new runs, and the error says
// only that running editor sessions were not told.
func TestUnannouncedApprovalChangeIsStillSaved(t *testing.T) {
	s, project := openStore(t), t.TempDir()
	if err := os.Mkdir(filepath.Join(s.Path(), approvalGenerationRecord), 0o700); err != nil {
		t.Fatal(err)
	}
	err := approve(s, project, egress.Filtered, []egress.Rule{rule("kept.example.com")}, nil)
	if !errors.Is(err, ErrSessionsNotTold) {
		t.Fatalf("approve with an unwritable generation = %v, want ErrSessionsNotTold", err)
	}
	if approval, err := s.Approval(project); err != nil || approval == nil {
		t.Fatalf("the approval did not survive an unannounced change: %+v %v", approval, err)
	}
	withdrew := false
	record, err := s.ApprovalAt(project)
	if err == nil {
		withdrew, err = s.Forget(context.Background(), record)
	}
	if !errors.Is(err, ErrSessionsNotTold) || !withdrew {
		t.Fatalf("forget with an unwritable generation = (%v, %v), want the withdrawal saved and ErrSessionsNotTold", withdrew, err)
	}
	// Forgetting again removes nothing, so there is nothing to announce and nothing to report as done.
	if withdrew, err = s.Forget(context.Background(), record); withdrew || err != nil {
		t.Fatalf("a second forget = (%v, %v), want (false, nil)", withdrew, err)
	}
}
