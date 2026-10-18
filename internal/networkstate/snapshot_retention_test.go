package networkstate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/egress"
)

// filteredSnapshot admits a filtered policy for a new project and returns its fingerprint.
func filteredSnapshot(t *testing.T, s *Store) string {
	t.Helper()
	mode := egress.Filtered
	policy, err := s.Admit(t.TempDir(), Admission{InvocationMode: &mode})
	if err != nil {
		t.Fatal(err)
	}
	return policy.Fingerprint
}

// ageRecord makes a snapshot or its marker look last touched longer ago than retention keeps one.
func ageRecord(t *testing.T, s *Store, name string) {
	t.Helper()
	old := time.Now().Add(-ExecutionRetention - 24*time.Hour)
	if err := os.Chtimes(filepath.Join(s.Path(), name), old, old); err != nil {
		t.Fatal(err)
	}
}

func recordExists(t *testing.T, s *Store, name string) bool {
	t.Helper()
	_, err := os.Lstat(filepath.Join(s.Path(), name))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return err == nil
}

func retentionEvidence(t *testing.T, s *Store) *Evidence {
	t.Helper()
	evidence, err := OpenEvidence(s.Path(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = evidence.Close() })
	return evidence
}

func TestSnapshotRetentionRemovesOnlyOldAdmittedSnapshotsNothingUses(t *testing.T) {
	s := openStore(t)
	unused, recent, ran, older := filteredSnapshot(t, s), filteredSnapshot(t, s), filteredSnapshot(t, s), filteredSnapshot(t, s)
	ref := JobSnapshotRef{JobDigest: strings.Repeat("a", 64), SessionID: "remote_one"}
	job, err := s.CaptureJob(ref, []egress.Rule{rule("controller.example.com")}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if recordExists(t, s, admittedRecord(job.Fingerprint)) {
		t.Fatal("a session's snapshot was marked removable")
	}
	// A snapshot written before markers existed looks like one with its marker gone.
	if err := os.Remove(filepath.Join(s.Path(), admittedRecord(older))); err != nil {
		t.Fatal(err)
	}
	for _, fingerprint := range []string{unused, ran} {
		ageRecord(t, s, admittedRecord(fingerprint))
	}
	for _, fingerprint := range []string{job.Fingerprint, older} {
		ageRecord(t, s, snapshotRecord(fingerprint))
	}
	evidence := retentionEvidence(t, s)
	ctx := context.Background()

	removed, err := evidence.RemoveUnusedSnapshots(ctx, map[string]bool{ran: true}, time.Now())
	if err != nil || removed != 1 {
		t.Fatalf("RemoveUnusedSnapshots = %d, %v; want only the old unused snapshot removed", removed, err)
	}
	if recordExists(t, s, snapshotRecord(unused)) || recordExists(t, s, admittedRecord(unused)) {
		t.Fatal("an old snapshot nothing uses was kept")
	}
	for _, kept := range []struct{ fingerprint, why string }{
		{recent, "a snapshot used inside the retention window"},
		{ran, "a snapshot a retained run names"},
		{job.Fingerprint, "a snapshot a session was created with"},
		{older, "a snapshot written before markers existed"},
	} {
		if !recordExists(t, s, snapshotRecord(kept.fingerprint)) {
			t.Errorf("%s was removed", kept.why)
		}
	}
	if _, err := s.LoadJobSnapshot(ref, job.Fingerprint); err != nil {
		t.Fatal("the session can no longer load its snapshot", err)
	}
	// A second pass, as the next launch would run, finds nothing more to do.
	if removed, err := evidence.RemoveUnusedSnapshots(ctx, map[string]bool{ran: true}, time.Now()); removed != 0 || err != nil {
		t.Fatalf("second pass = %d, %v", removed, err)
	}
}

// An unchanged policy admitted again names the snapshot already on disk. Without restarting its
// window, a project reopened after 90 days would lose the snapshot its own launch just admitted:
// the launch settles earlier runs between its admission and the moment its run loads the snapshot.
func TestAdmittingOrLoadingASnapshotRestartsItsRetention(t *testing.T) {
	s := openStore(t)
	project := t.TempDir()
	mode := egress.Filtered
	admitted, err := s.Admit(project, Admission{InvocationMode: &mode})
	if err != nil {
		t.Fatal(err)
	}
	marker := admittedRecord(admitted.Fingerprint)
	evidence := retentionEvidence(t, s)
	ctx := context.Background()

	ageRecord(t, s, marker)
	again, err := s.Admit(project, Admission{InvocationMode: &mode})
	if err != nil || again.Fingerprint != admitted.Fingerprint {
		t.Fatalf("readmission = %s, %v; want the same snapshot", again.Fingerprint, err)
	}
	if removed, err := evidence.RemoveUnusedSnapshots(ctx, map[string]bool{}, time.Now()); removed != 0 || err != nil {
		t.Fatalf("a snapshot admitted again was removed: %d, %v", removed, err)
	}

	ageRecord(t, s, marker)
	if _, err := s.LoadSnapshot(project, admitted.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if removed, err := evidence.RemoveUnusedSnapshots(ctx, map[string]bool{}, time.Now()); removed != 0 || err != nil {
		t.Fatalf("a snapshot just loaded was removed: %d, %v", removed, err)
	}

	ageRecord(t, s, marker)
	if removed, err := evidence.RemoveUnusedSnapshots(ctx, map[string]bool{}, time.Now()); removed != 1 || err != nil {
		t.Fatalf("an unused snapshot was kept: %d, %v", removed, err)
	}
	if _, err := s.LoadSnapshot(project, admitted.Fingerprint); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("loading a removed snapshot = %v, want it gone", err)
	}
	// The next admission of the same rules writes it, and marks it, again.
	if _, err := s.Admit(project, Admission{InvocationMode: &mode}); err != nil {
		t.Fatal(err)
	}
	if !recordExists(t, s, snapshotRecord(admitted.Fingerprint)) || !recordExists(t, s, marker) {
		t.Fatal("admitting removed rules again did not restore their snapshot")
	}
}

// A snapshot retention never removes is loaded as before: no lock, nothing written. A session
// daemon's network reads load such snapshots all the time.
func TestLoadingASnapshotRetentionNeverRemovesWritesNothing(t *testing.T) {
	s := openStore(t)
	ref := JobSnapshotRef{JobDigest: strings.Repeat("a", 64), SessionID: "remote_one"}
	job, err := s.CaptureJob(ref, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(s.Path(), "snapshot-lock")); err != nil {
		t.Fatal(err)
	}
	ageRecord(t, s, snapshotRecord(job.Fingerprint))
	before, err := os.Lstat(filepath.Join(s.Path(), snapshotRecord(job.Fingerprint)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadJobSnapshot(ref, job.Fingerprint); err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(filepath.Join(s.Path(), snapshotRecord(job.Fingerprint)))
	if err != nil || !after.ModTime().Equal(before.ModTime()) || recordExists(t, s, "snapshot-lock") {
		t.Fatalf("a session's snapshot load wrote: modified %v -> %v, lock created %v",
			before.ModTime(), after.ModTime(), recordExists(t, s, "snapshot-lock"))
	}
}

// A removal that stopped after the snapshot leaves its marker. A load then fails as one after the
// removal would, and the next pass clears the marker.
func TestAnInterruptedSnapshotRemovalIsFinishedByTheNextPass(t *testing.T) {
	s := openStore(t)
	project := t.TempDir()
	mode := egress.Filtered
	admitted, err := s.Admit(project, Admission{InvocationMode: &mode})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(s.Path(), snapshotRecord(admitted.Fingerprint))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadSnapshot(project, admitted.Fingerprint); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("loading a half-removed snapshot = %v, want it gone", err)
	}
	later := time.Now().Add(ExecutionRetention + time.Hour)
	if removed, err := retentionEvidence(t, s).RemoveUnusedSnapshots(context.Background(), map[string]bool{}, later); removed != 0 || err != nil {
		t.Fatalf("finishing the removal = %d, %v", removed, err)
	}
	if recordExists(t, s, admittedRecord(admitted.Fingerprint)) {
		t.Fatal("a marker without its snapshot was kept")
	}
}

// The last look happens under the lock, so a launch that used the snapshot after the directory
// was listed still keeps it.
func TestSnapshotRetentionLooksAgainBeforeRemoving(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	used := filteredSnapshot(t, s)
	ageRecord(t, s, admittedRecord(used))
	if err := s.lockSnapshots(ctx, func() error { return s.touchAdmitted(used) }); err != nil {
		t.Fatal(err)
	}
	if gone, err := s.removeUnusedSnapshot(ctx, used, time.Now()); gone || err != nil {
		t.Fatalf("a snapshot used since the listing was removed: %v, %v", gone, err)
	}
	later := time.Now().Add(ExecutionRetention + time.Hour)
	if gone, err := s.removeUnusedSnapshot(ctx, used, later); !gone || err != nil {
		t.Fatalf("an unused snapshot past its window was kept: %v, %v", gone, err)
	}
	if gone, err := s.removeUnusedSnapshot(ctx, used, later); gone || err != nil {
		t.Fatalf("removing it again = %v, %v; want nothing to do", gone, err)
	}
}

func TestSnapshotRetentionKeepsLinksAndWhatItCannotRead(t *testing.T) {
	s := openStore(t)
	evidence := retentionEvidence(t, s)
	later := time.Now().Add(ExecutionRetention + time.Hour)
	ctx := context.Background()
	path := func(name string) string { return filepath.Join(s.Path(), name) }

	// A link planted under a snapshot's name, or its marker's, is never followed or removed.
	linked := filteredSnapshot(t, s)
	moved := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.Rename(path(snapshotRecord(linked)), moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, path(snapshotRecord(linked))); err != nil {
		t.Fatal(err)
	}
	linkedMarker := filteredSnapshot(t, s)
	movedMarker := filepath.Join(t.TempDir(), "marker.json")
	if err := os.Rename(path(admittedRecord(linkedMarker)), movedMarker); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(movedMarker, path(admittedRecord(linkedMarker))); err != nil {
		t.Fatal(err)
	}
	// A snapshot that does not parse, one whose content names another snapshot, a directory, and
	// one whose marker anyone may read stay for a person to look at.
	well := filteredSnapshot(t, s)
	data, err := os.ReadFile(path(snapshotRecord(well)))
	if err != nil {
		t.Fatal(err)
	}
	garbled, misnamed, directory := filteredSnapshot(t, s), filteredSnapshot(t, s), filteredSnapshot(t, s)
	if err := os.WriteFile(path(snapshotRecord(garbled)), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path(snapshotRecord(misnamed)), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path(snapshotRecord(directory))); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path(snapshotRecord(directory)), 0o700); err != nil {
		t.Fatal(err)
	}
	shared := filteredSnapshot(t, s)
	if err := os.Chmod(path(admittedRecord(shared)), 0o644); err != nil {
		t.Fatal(err)
	}
	// A marker whose snapshot was removed by hand marks nothing and goes.
	orphan := filteredSnapshot(t, s)
	if err := os.Remove(path(snapshotRecord(orphan))); err != nil {
		t.Fatal(err)
	}

	removed, _ := evidence.RemoveUnusedSnapshots(ctx, map[string]bool{}, later)
	if removed != 1 || recordExists(t, s, snapshotRecord(well)) {
		t.Fatalf("removed %d; want only the one well-formed snapshot", removed)
	}
	if recordExists(t, s, admittedRecord(orphan)) {
		t.Fatal("a marker without its snapshot was kept")
	}
	for _, name := range []string{snapshotRecord(linked), admittedRecord(linkedMarker), snapshotRecord(linkedMarker)} {
		if !recordExists(t, s, name) {
			t.Fatalf("%s was removed", name)
		}
	}
	for _, target := range []string{moved, movedMarker} {
		if _, err := os.Stat(target); err != nil {
			t.Fatal("a link's target is gone", err)
		}
	}
	for _, fingerprint := range []string{garbled, misnamed, directory, shared} {
		if !recordExists(t, s, snapshotRecord(fingerprint)) || !recordExists(t, s, admittedRecord(fingerprint)) {
			t.Fatalf("snapshot-%s.json or its marker was removed", fingerprint[:8])
		}
	}
}

// unmarkedRun admits these rules for a new project, drops the marker as a coop without markers never
// wrote one, and finishes a setup check's run under that snapshot.
func unmarkedRun(t *testing.T, s *Store, like Execution, inputs ...egress.Input) Execution {
	t.Helper()
	mode := egress.Filtered
	project := t.TempDir()
	policy, err := s.Admit(project, Admission{InvocationMode: &mode, Operator: inputs})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(s.Path(), admittedRecord(policy.Fingerprint))); err != nil {
		t.Fatal(err)
	}
	return finishedRun(t, s, runUnder(t, s, like, project, policy.Fingerprint))
}

// A setup check's snapshot from before markers is recognized by its own rule when retention removes
// the record of the check's run, the last thing that names it. Dated to when that run ended, it goes
// with the next pass. A launch's from before markers stays, and so does one a setup rule is only part
// of, though a setup check's run names them too: a session this store cannot see may use them.
func TestRetentionMarksASetupCheckSnapshotAsItsRunsRecordGoes(t *testing.T) {
	s, live := executionFixture(t)
	setup := egress.Input{Origin: SetupOrigin(), Rules: []egress.Rule{rule("example.com")}}
	check := unmarkedRun(t, s, live, setup)
	launch := unmarkedRun(t, s, live)
	mixed := unmarkedRun(t, s, live, setup, egress.Input{Origin: egress.Origin{Kind: "operator", Name: "allow-domain"}, Rules: []egress.Rule{rule("docs.example.com")}})
	evidence := retentionEvidence(t, s)
	ctx := context.Background()
	marked := func(run Execution) bool { return recordExists(t, s, admittedRecord(run.Snapshot.PolicyFingerprint)) }

	soon := time.Now().Add(ExecutionRetention - time.Hour)
	if removed, err := evidence.RemoveExpired(ctx, check.ID, soon); removed || err != nil || marked(check) {
		t.Fatalf("a setup check's record inside its window: removed %v, %v, snapshot marked %v", removed, err, marked(check))
	}
	later := time.Now().Add(ExecutionRetention + time.Hour)
	for _, run := range []Execution{check, launch, mixed} {
		if removed, err := evidence.RemoveExpired(ctx, run.ID, later); !removed || err != nil {
			t.Fatalf("RemoveExpired = %v, %v; want the finished run's record removed", removed, err)
		}
	}
	marker, err := os.Lstat(filepath.Join(s.Path(), admittedRecord(check.Snapshot.PolicyFingerprint)))
	if err != nil {
		t.Fatal("the setup check's snapshot was not marked", err)
	}
	if ended := *check.Receipt.EndedAt; !marker.ModTime().Equal(ended) {
		t.Fatalf("marked as last used at %v; want when its run ended, %v", marker.ModTime(), ended)
	}
	if marked(launch) || marked(mixed) {
		t.Fatalf("marked a snapshot setup did not grant alone: launch %v, mixed %v", marked(launch), marked(mixed))
	}

	removed, err := evidence.RemoveUnusedSnapshots(ctx, map[string]bool{live.Snapshot.PolicyFingerprint: true}, later)
	if err != nil || removed != 1 || recordExists(t, s, snapshotRecord(check.Snapshot.PolicyFingerprint)) {
		t.Fatalf("RemoveUnusedSnapshots = %d, %v; want only the setup check's snapshot removed", removed, err)
	}
	for _, run := range []Execution{launch, mixed, live} {
		if !recordExists(t, s, snapshotRecord(run.Snapshot.PolicyFingerprint)) {
			t.Fatal("a snapshot that is not an unused setup check's was removed")
		}
	}
}

// A pass that cannot mark a setup check's snapshot keeps the record of the check's run, the last
// thing that names the snapshot, so the next pass tries both again. Removing the record anyway would
// leave the snapshot unmarked for good.
func TestRetentionKeepsASetupCheckRecordUntilItsSnapshotIsMarked(t *testing.T) {
	s, live := executionFixture(t)
	check := unmarkedRun(t, s, live, egress.Input{Origin: SetupOrigin(), Rules: []egress.Rule{rule("example.com")}})
	evidence := retentionEvidence(t, s)
	later := time.Now().Add(ExecutionRetention + time.Hour)
	record, marker := "execution-"+check.ID+".json", admittedRecord(check.Snapshot.PolicyFingerprint)

	// Another process holds the snapshot lock past the pass's deadline.
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var holding error
	go func() {
		defer close(done)
		holding = s.lockSnapshots(context.Background(), func() error { close(held); <-release; return nil })
	}()
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(func() { unblock(); <-done })
	select {
	case <-held:
	case <-done:
		t.Fatal("the snapshot lock was never held:", holding)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if removed, err := evidence.RemoveExpired(ctx, check.ID, later); removed || !errors.Is(err, context.DeadlineExceeded) ||
		!recordExists(t, s, record) || recordExists(t, s, marker) {
		t.Fatalf("RemoveExpired while the snapshot lock is held = %v, %v; record kept %v, snapshot marked %v; want an error and the record kept",
			removed, err, recordExists(t, s, record), recordExists(t, s, marker))
	}

	unblock()
	<-done
	if removed, err := evidence.RemoveExpired(context.Background(), check.ID, later); !removed || err != nil || !recordExists(t, s, marker) {
		t.Fatalf("the next pass = %v, %v, snapshot marked %v; want the record removed and the snapshot marked",
			removed, err, recordExists(t, s, marker))
	}
}

// A setup check's snapshot the pass fails to read keeps the record of the check's run, so the next
// pass reads it again. Only a snapshot that is gone, or a link read refuses, lets the record go with
// nothing marked. No seam injects EMFILE or EIO, so a mode that forbids reading fails the same read.
func TestRetentionKeepsASetupCheckRecordWhileItsSnapshotCannotBeRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file whatever its mode")
	}
	s, live := executionFixture(t)
	setup := egress.Input{Origin: SetupOrigin(), Rules: []egress.Rule{rule("example.com")}}
	unreadable, gone := unmarkedRun(t, s, live, setup), unmarkedRun(t, s, live, setup)
	evidence := retentionEvidence(t, s)
	ctx := context.Background()
	later := time.Now().Add(ExecutionRetention + time.Hour)
	path := func(run Execution) string {
		return filepath.Join(s.Path(), snapshotRecord(run.Snapshot.PolicyFingerprint))
	}
	kept := func(run Execution) bool { return recordExists(t, s, "execution-"+run.ID+".json") }
	marked := func(run Execution) bool { return recordExists(t, s, admittedRecord(run.Snapshot.PolicyFingerprint)) }

	if err := os.Chmod(path(unreadable), 0); err != nil {
		t.Fatal(err)
	}
	if removed, err := evidence.RemoveExpired(ctx, unreadable.ID, later); removed || !errors.Is(err, os.ErrPermission) || !kept(unreadable) || marked(unreadable) {
		t.Fatalf("RemoveExpired while the snapshot cannot be read = %v, %v; record kept %v, snapshot marked %v; want an error and the record kept",
			removed, err, kept(unreadable), marked(unreadable))
	}
	if err := os.Chmod(path(unreadable), 0o600); err != nil {
		t.Fatal(err)
	}
	if removed, err := evidence.RemoveExpired(ctx, unreadable.ID, later); !removed || err != nil || !marked(unreadable) {
		t.Fatalf("the next pass = %v, %v, snapshot marked %v; want the record removed and the snapshot marked", removed, err, marked(unreadable))
	}

	if err := os.Remove(path(gone)); err != nil {
		t.Fatal(err)
	}
	if removed, err := evidence.RemoveExpired(ctx, gone.ID, later); !removed || err != nil || marked(gone) {
		t.Fatalf("RemoveExpired with the snapshot gone = %v, %v, snapshot marked %v; want the record removed and nothing marked",
			removed, err, marked(gone))
	}
}

// Only a snapshot setup granted alone proves a setup check's. A rule another input shares, or an
// agent's provider, which a check never runs, may be a launch's, and a session may still use it.
func TestSetupCheckProofNeedsSetupAloneAndNoProvider(t *testing.T) {
	setup := egress.Grant{Rule: rule("example.com"), Origins: []egress.Origin{SetupOrigin()}}
	// Setup's origin sorts before rules-file's, as a capture orders them, so only the one-origin
	// check rejects this grant.
	shared := egress.Grant{Rule: rule("example.com"), Origins: []egress.Origin{SetupOrigin(), {Kind: "operator", Name: "rules-file"}}}
	agent := []egress.Dependency{{Provider: "model", Client: egress.ClientCLI, Version: "1", Backend: "direct", AuthMode: "key"}}
	for _, c := range []struct {
		name     string
		snapshot egress.Snapshot
		want     bool
	}{
		{"setup's grants alone", egress.Snapshot{Grants: []egress.Grant{setup}}, true},
		{"a grant another input shares", egress.Snapshot{Grants: []egress.Grant{shared}}, false},
		{"setup's grants and an agent's provider", egress.Snapshot{Grants: []egress.Grant{setup}, Dependencies: agent}, false},
	} {
		if got := fromSetupCheck(c.snapshot); got != c.want {
			t.Errorf("fromSetupCheck(%s) = %v, want %v", c.name, got, c.want)
		}
	}
}

// A census nobody took removes nothing: only a caller that read every retained run may pass one,
// and nil, the zero value, is never mistaken for "no run uses anything".
func TestSnapshotRetentionRefusesAMissingCensus(t *testing.T) {
	s := openStore(t)
	well := filteredSnapshot(t, s)
	ageRecord(t, s, admittedRecord(well))
	later := time.Now().Add(ExecutionRetention + time.Hour)
	if removed, err := retentionEvidence(t, s).RemoveUnusedSnapshots(context.Background(), nil, later); removed != 0 || err == nil {
		t.Fatalf("a nil census removed %d, %v; want nothing removed and an error", removed, err)
	}
	if !recordExists(t, s, snapshotRecord(well)) {
		t.Fatal("a nil census removed a snapshot")
	}
}
