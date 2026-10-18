package networkstate

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/networkview"
)

// finishedRun seals a run whose custody confirmed every resource and the artifact gone: the only
// kind of record retention may remove.
func finishedRun(t *testing.T, s *Store, record Execution) Execution {
	t.Helper()
	sealed, err := s.mutateExecution(context.Background(), record.ID, record.Revision, func(r *Execution) (bool, error) {
		for i := range r.Resources {
			r.Resources[i].State = "gone"
		}
		r.Artifact.State = "gone"
		r.Snapshot.Terminal, r.WorkloadStarted, r.ObserverAfterWorkload = true, true, true
		r.ReadySequence, r.Snapshot.Sequence = 1, 1
		exact := networkview.MetricCoverage{Status: "exact"}
		r.Snapshot.Coverage = networkview.Coverage{ProxyBytes: exact, Connections: exact, UpstreamFailures: exact, KernelPackets: exact,
			GuardDenials: exact, MaintenanceQueries: exact, MaintenanceBytes: exact, SocketInventory: exact, BoundaryAttribution: exact}
		setSmokeObservationFixture(&r.Snapshot)
		return sealExecution(r, "exited", false)
	})
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

// sealedWithCleanupOwed seals a run whose resources were never confirmed gone.
func sealedWithCleanupOwed(t *testing.T, s *Store, record Execution) Execution {
	t.Helper()
	snapshot := record.Snapshot
	snapshot.Sequence, snapshot.Terminal = 1, true
	record, err := s.AcceptSnapshot(context.Background(), record.ID, record.Revision, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if record, err = s.SealExecution(context.Background(), record.ID, record.Revision, "exited"); err != nil {
		t.Fatal(err)
	}
	return record
}

func anotherRun(t *testing.T, s *Store, like Execution) Execution {
	t.Helper()
	return runUnder(t, s, like, like.Project, like.Snapshot.PolicyFingerprint)
}

// runUnder starts another setup check's run like the given one, under one project's snapshot.
func runUnder(t *testing.T, s *Store, like Execution, project, fingerprint string) Execution {
	t.Helper()
	record, err := executionSmoke(t, s).CreateExecution(context.Background(), ExecutionSpec{Project: project,
		PolicyFingerprint: fingerprint, Runtime: like.Runtime, DaemonID: like.DaemonID,
		Endpoint: like.Endpoint, GatewayImage: like.GatewayImage, ClientImage: like.ClientImage})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestRetentionRemovesOnlyFinishedRunsPastNinetyDays(t *testing.T) {
	if ExecutionRetention != 90*24*time.Hour {
		t.Fatalf("ExecutionRetention = %v; docs/networking.md promises 90 days", ExecutionRetention)
	}
	s, live := executionFixture(t)
	finished := finishedRun(t, s, anotherRun(t, s, live))
	owed := sealedWithCleanupOwed(t, s, anotherRun(t, s, live))
	evidence, err := OpenEvidence(s.Path(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer evidence.Close()
	ctx := context.Background()
	exists := func(id string) bool {
		_, err := os.Lstat(filepath.Join(s.Path(), "execution-"+id+".json"))
		return err == nil
	}

	// Within the window nothing goes, finished or not: a day short of 90 days after it ended.
	soon := time.Now().Add(ExecutionRetention - 24*time.Hour)
	for _, id := range []string{live.ID, finished.ID, owed.ID} {
		if removed, err := evidence.RemoveExpired(ctx, id, soon); removed || err != nil || !exists(id) {
			t.Fatalf("a run inside the retention window was removed: %s %v %v", id, removed, err)
		}
	}

	later := time.Now().Add(ExecutionRetention + time.Hour)
	page, err := evidence.Executions("")
	if err != nil || page.Incomplete || len(page.Executions) != 3 {
		t.Fatalf("listing = %+v, %v", page, err)
	}
	for _, summary := range page.Executions {
		if summary.Expired(later) != (summary.ID == finished.ID) {
			t.Errorf("Expired(%s) = %v; only the finished run with nothing owed may expire", summary.ID, summary.Expired(later))
		}
		if removed, err := evidence.RemoveExpired(ctx, summary.ID, later); err != nil || removed != (summary.ID == finished.ID) {
			t.Errorf("RemoveExpired(%s) = %v, %v", summary.ID, removed, err)
		}
	}
	if exists(finished.ID) || !exists(live.ID) || !exists(owed.ID) {
		t.Fatalf("after retention: finished %v, live %v, owed cleanup %v; want only the finished run gone",
			exists(finished.ID), exists(live.ID), exists(owed.ID))
	}
	// Removing it again, as a second settle pass would, is not an error.
	if removed, err := evidence.RemoveExpired(ctx, finished.ID, later); removed || err != nil {
		t.Fatalf("second removal = %v, %v", removed, err)
	}
	if page, err := evidence.Executions(""); err != nil || page.Incomplete || len(page.Executions) != 2 {
		t.Fatalf("listing after retention = %+v, %v", page, err)
	}
}

func TestRetentionKeepsARecordItCannotReadOrThatIsALink(t *testing.T) {
	s, live := executionFixture(t)
	finished := finishedRun(t, s, anotherRun(t, s, live))
	later := time.Now().Add(ExecutionRetention + time.Hour)
	ctx := context.Background()
	evidence, err := OpenEvidence(s.Path(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer evidence.Close()

	// A link planted under a run's name is never followed or removed, even to an expired record.
	moved := filepath.Join(t.TempDir(), "elsewhere.json")
	name := filepath.Join(s.Path(), "execution-"+finished.ID+".json")
	if err := os.Rename(name, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, name); err != nil {
		t.Fatal(err)
	}
	if removed, _ := evidence.RemoveExpired(ctx, finished.ID, later); removed {
		t.Fatal("retention removed a link")
	}
	if _, err := os.Lstat(name); err != nil {
		t.Fatal("the planted link is gone", err)
	}
	if _, err := os.Stat(moved); err != nil {
		t.Fatal("the link's target is gone", err)
	}

	// A record that does not parse stays for a person to look at.
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if removed, _ := evidence.RemoveExpired(ctx, finished.ID, later); removed {
		t.Fatal("retention removed a record it could not read")
	}
	if _, err := os.Lstat(name); err != nil {
		t.Fatal("the unreadable record is gone", err)
	}
}
