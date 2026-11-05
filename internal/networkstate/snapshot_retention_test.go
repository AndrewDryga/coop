package networkstate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
