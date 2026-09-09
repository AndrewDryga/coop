package networkstate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/AndrewDryga/coop/internal/processidentity"
)

func TestExecutionCrossProcessCAS(t *testing.T) {
	if path := os.Getenv("COOP_TEST_NETWORK_CAS_ROOT"); path != "" {
		evidence, err := OpenEvidence(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer evidence.Close()
		id := os.Getenv("COOP_TEST_NETWORK_CAS_ID")
		record, err := evidence.Execution(id)
		if err != nil {
			t.Fatal(err)
		}
		resource := record.Resources[0]
		_, err = evidence.ConfirmResourceGone(context.Background(), id, 1, record.DaemonID, resource.Role, resource.Name, resource.ID)
		if errors.Is(err, ErrExecutionConflict) {
			os.Exit(42)
		}
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	s, record := executionFixture(t)
	var workers sync.WaitGroup
	results := make(chan error, 6)
	for range 6 {
		workers.Go(func() {
			cmd := exec.Command(os.Args[0], "-test.run=^TestExecutionCrossProcessCAS$")
			cmd.Env = append(os.Environ(), "COOP_TEST_NETWORK_CAS_ROOT="+s.Path(), "COOP_TEST_NETWORK_CAS_ID="+record.ID)
			results <- cmd.Run()
		})
	}
	workers.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
			continue
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 42 {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("cross-process CAS winners=%d", winners)
	}
}

func TestExecutionReplacementKeyDoesNotAuthorizeExistingStore(t *testing.T) {
	s, record := executionFixture(t)
	record = prepareFixtureLaunch(t, s, record)
	if err := s.publish("owner.key", []byte(strings.Repeat("x", 32)), true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginResourceCreation(context.Background(), record.ID, record.Revision, "agent"); err == nil {
		t.Fatal("a replaced key authorized work through the old store")
	}
	evidence, err := OpenEvidence(s.Path(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer evidence.Close()
	if _, err := evidence.Execution(record.ID); err != nil {
		t.Fatal("key replacement hid cleanup custody", err)
	}
}

func TestExecutionMismatchedSupervisorRecoveryPreservesCleanupAndUnknownWorkload(t *testing.T) {
	s, record := executionFixture(t)
	record.Supervisor.StartToken += "-different-generation"
	if processidentity.Inspect(record.Supervisor.PID, record.Supervisor.StartToken) != processidentity.Mismatch {
		t.Fatal("fixture did not establish a departed supervisor generation")
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.publish("execution-"+record.ID+".json", data, true); err != nil {
		t.Fatal(err)
	}
	evidence, err := OpenEvidence(s.Path(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer evidence.Close()
	got, err := evidence.RecoverInterrupted(context.Background(), record.ID, record.Revision)
	if err != nil || got.Receipt.Workload != "supervisor_lost" || got.Receipt.Cleanup != "pending" || got.Receipt.Completeness != "partial" || !got.Receipt.Snapshot.Loss.Unknown {
		t.Fatal("recovery asserted clean/stopped workload", err)
	}
	for _, resource := range got.Resources {
		if resource.State != "planned" {
			t.Fatal("process loss erased runtime cleanup intent")
		}
	}
}

func TestExecutionInterruptedCreationReconciliationRequiresExactDepartedCustody(t *testing.T) {
	s, record := executionFixture(t)
	record = prepareFixtureLaunch(t, s, record)
	ctx := context.Background()
	record, err := s.BeginResourceCreation(ctx, record.ID, record.Revision, "guard")
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := OpenEvidence(s.Path(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer evidence.Close()
	resource, err := resourceAt(&record, "guard")
	if err != nil {
		t.Fatal(err)
	}
	name, id := resource.Name, strings.Repeat("a", 64)
	if _, err := evidence.ReconcileInterruptedResource(ctx, record.ID, record.Revision, record.DaemonID, "guard", name, id); err == nil {
		t.Fatal("live owner allowed reconciliation")
	}
	record.Supervisor.StartToken += "-different-generation"
	if processidentity.Inspect(record.Supervisor.PID, record.Supervisor.StartToken) != processidentity.Mismatch {
		t.Fatal("fixture owner did not depart")
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.publish("execution-"+record.ID+".json", data, true); err != nil {
		t.Fatal(err)
	}
	for _, change := range []struct{ daemon, role, name, id string }{
		{"different", "guard", name, id},
		{record.DaemonID, "guard", name + "-replacement", id},
		{record.DaemonID, "guard", name, ""},
		{record.DaemonID, "guard", name, strings.Repeat("A", 64)},
		{record.DaemonID, "missing", name, id},
	} {
		if _, err := evidence.ReconcileInterruptedResource(ctx, record.ID, record.Revision, change.daemon, change.role, change.name, change.id); err == nil {
			t.Fatal("invalid reconciliation accepted", change)
		}
	}
	// Cleanup can bind a positive observation after a partial receipt is sealed;
	// that receipt is immutable and does not acquire invented terminal evidence.
	record, err = evidence.RecoverInterrupted(ctx, record.ID, record.Revision)
	if err != nil {
		t.Fatal(err)
	}
	digest := record.Receipt.Digest
	record, err = evidence.ReconcileInterruptedResource(ctx, record.ID, record.Revision, record.DaemonID, "guard", name, id)
	if err != nil || record.Receipt.Digest != digest {
		t.Fatal("positive reconciliation", err)
	}
	revision := record.Revision
	record, err = evidence.ReconcileInterruptedResource(ctx, record.ID, record.Revision, record.DaemonID, "guard", name, id)
	if err != nil || record.Revision != revision {
		t.Fatal("idempotent reconciliation", err)
	}
	if _, err := evidence.ReconcileInterruptedResource(ctx, record.ID, record.Revision, record.DaemonID, "guard", name, strings.Repeat("b", 64)); err == nil {
		t.Fatal("rebound cleanup identity")
	}
}

func TestExecutionLockSymlinkRefusesWithoutTouchingTarget(t *testing.T) {
	s, record := executionFixture(t)
	entries, err := os.ReadDir(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "execution-lock-") {
			continue
		}
		// Isolated test fixture only: production retention must never remove locks.
		if err := s.root.Remove(entry.Name()); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(s.Path(), "owner.key"), filepath.Join(s.Path(), entry.Name())); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.BeginResourceCreation(context.Background(), record.ID, record.Revision, "agent"); err == nil {
		t.Fatal("symlink lock entered the authority transaction")
	}
	if err := s.authorityAvailable(); err != nil {
		t.Fatal("rejected lock damaged the key", err)
	}
}
