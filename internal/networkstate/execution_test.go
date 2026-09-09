package networkstate

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkview"
)

func executionFixture(t *testing.T) (*Store, Execution) {
	t.Helper()
	s := openStore(t)
	project := t.TempDir()
	mode := egress.Filtered
	policy, err := s.Admit(project, Admission{InvocationMode: &mode})
	if err != nil {
		t.Fatal(err)
	}
	inputsID, err := s.RecordInputs(LaunchInputs{})
	if err != nil {
		t.Fatal(err)
	}
	record, err := executionTrial(t, s).CreateExecution(context.Background(), ExecutionSpec{Project: project, PolicyFingerprint: policy.Fingerprint, InputsID: inputsID,
		Runtime: "docker", DaemonID: "fixture-daemon", Endpoint: "unix:///fixture.sock", GatewayImage: "sha256:" + strings.Repeat("a", 64), ClientImage: "sha256:" + strings.Repeat("b", 64)}, "enforcement", nil)
	if err != nil {
		t.Fatal(err)
	}
	return s, record
}

func executionTrial(t *testing.T, s *Store) *QualificationTrial {
	t.Helper()
	spec := candidateFixture()
	spec.ClientImage, spec.GatewayImage = spec.GatewayImage, spec.ClientImage
	candidate, err := s.RecordCandidate(spec)
	if err != nil {
		t.Fatal(err)
	}
	trial, err := s.BeginQualification(candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	return trial
}

func TestExecutionIntentsBindExactResourcesWithoutRestartOrRebind(t *testing.T) {
	s, record := executionFixture(t)
	ctx := context.Background()
	if record.Revision != 1 || record.ID == record.Epoch || len(record.Resources) != 5 {
		t.Fatal("missing unique intent")
	}
	object := strings.Repeat("b", 64)
	if _, err := s.RecordResourceCreated(ctx, record.ID, record.Revision, "guard", object); err == nil {
		t.Fatal("created without durable intent")
	}
	var err error
	record = prepareFixtureLaunch(t, s, record)
	record, err = s.BeginResourceCreation(ctx, record.ID, record.Revision, "guard")
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := s.Execution(record.ID)
	if err != nil || persisted.Resources[1].State != "creating" || persisted.Resources[1].ID != "" {
		t.Fatal("unknown create outcome became absence")
	}
	record, err = s.RecordResourceCreated(ctx, record.ID, record.Revision, "guard", object)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordResourceCreated(ctx, record.ID, record.Revision, "guard", strings.Repeat("c", 64)); err == nil {
		t.Fatal("resource rebound")
	}
	record, err = s.BeginResourceStart(ctx, record.ID, record.Revision, "guard")
	if err != nil {
		t.Fatal(err)
	}
	record, err = s.RecordResourceStarted(ctx, record.ID, record.Revision, "guard")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginResourceStart(ctx, record.ID, record.Revision, "guard"); err == nil {
		t.Fatal("same-epoch helper restarted")
	}
}

func TestExecutionConcurrentCASHasOnlyOneWinner(t *testing.T) {
	s, record := executionFixture(t)
	record = prepareFixtureLaunch(t, s, record)
	var workers sync.WaitGroup
	results := make(chan error, 20)
	for range 20 {
		workers.Go(func() {
			_, err := s.BeginResourceCreation(context.Background(), record.ID, record.Revision, "controller")
			results <- err
		})
	}
	workers.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		} else if !errors.Is(err, ErrExecutionConflict) {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("CAS winners=%d", winners)
	}
}

func TestExecutionSnapshotReplayDoesNotInflateOrCrossEpoch(t *testing.T) {
	s, record := executionFixture(t)
	record = prepareFixtureLaunch(t, s, record)
	ctx := context.Background()
	snapshot := record.Snapshot
	snapshot.Sequence, snapshot.Availability = 1, "available"
	snapshot.Counters = &networkview.Counters{SentBytes: networkview.Value(1<<53 + 7)}
	var err error
	record, err = s.AcceptSnapshot(ctx, record.ID, record.Revision, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	before := record.Revision
	record, err = s.AcceptSnapshot(ctx, record.ID, record.Revision, snapshot)
	if err != nil || record.Revision != before || *record.Snapshot.Counters.SentBytes != 1<<53+7 {
		t.Fatal("identical replay changed cumulative evidence")
	}
	changed := snapshot.Project(true)
	*changed.Counters.SentBytes++
	if _, err := s.AcceptSnapshot(ctx, record.ID, record.Revision, changed); err == nil {
		t.Fatal("same-sequence different content accepted")
	}
	changed.Sequence++
	changed.Epoch = strings.Repeat("f", 32)
	if _, err := s.AcceptSnapshot(ctx, record.ID, record.Revision, changed); err == nil {
		t.Fatal("foreign epoch accepted")
	}
	snapshot.Sequence, snapshot.Terminal = 2, true
	record, err = s.AcceptSnapshot(ctx, record.ID, record.Revision, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	stale := snapshot.Project(true)
	stale.Sequence = 1
	if _, err := s.AcceptSnapshot(ctx, record.ID, record.Revision, stale); !errors.Is(err, ErrSnapshotStale) || errors.Is(err, ErrExecutionConflict) {
		t.Fatal("stale producer observation should be discarded, not retried", err)
	}
	retained, err := s.Execution(record.ID)
	if err != nil || !equalJSON(retained, record) {
		t.Fatal("rejected stale observation changed custody", err)
	}
	snapshot.Sequence, snapshot.Terminal = 3, false
	if _, err := s.AcceptSnapshot(ctx, record.ID, record.Revision, snapshot); err == nil {
		t.Fatal("terminal observation reopened")
	}
	if _, err := s.BeginResourceCreation(ctx, record.ID, record.Revision, "agent"); err == nil {
		t.Fatal("terminal epoch authorized new runtime work before receipt sealing")
	}
}

func TestEvidenceKeyLossPreservesReadsAndCleanupButNotAuthority(t *testing.T) {
	s, record := executionFixture(t)
	ctx := context.Background()
	var err error
	record, err = s.SealExecution(ctx, record.ID, record.Revision, "launch_failed")
	if err != nil || record.Receipt.Completeness != "partial" || record.Receipt.Cleanup != "pending" || record.Receipt.Snapshot.LiveConnections != nil {
		t.Fatal("failed launch invented complete/clean live evidence")
	}
	sealed := *record.Receipt
	if err := s.root.Remove("owner.key"); err != nil {
		t.Fatal(err)
	}
	evidence, err := OpenEvidence(s.Path(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer evidence.Close()
	if _, exists := reflect.TypeOf(evidence).MethodByName("CreateExecution"); exists {
		t.Fatal("keyless evidence exposes launch authority")
	}
	if _, err := Open(s.Path(), nil); err == nil {
		t.Fatal("key loss regenerated authority")
	}
	if _, err := s.CreateExecution(ctx, ExecutionSpec{Project: record.Project, PolicyFingerprint: record.Snapshot.PolicyFingerprint,
		InputsID: record.InputsID,
		Runtime:  record.Runtime, DaemonID: record.DaemonID, Endpoint: record.Endpoint, GatewayImage: record.GatewayImage}); err == nil {
		t.Fatal("stale open Store reused missing key for a new launch")
	}
	if err := s.Approve(record.Project, egress.Open, nil, nil); err == nil {
		t.Fatal("stale open Store approved access after key loss")
	}
	if err := os.Remove(record.Project); err != nil {
		t.Fatal(err)
	}
	if _, err := evidence.Execution(record.ID); err != nil {
		t.Fatal("project/key loss hid cleanup custody", err)
	}
	resource := record.Resources[0]
	if _, err := evidence.ConfirmResourceGone(ctx, record.ID, record.Revision, "another-daemon", resource.Role, resource.Name, resource.ID); err == nil {
		t.Fatal("cleanup crossed runtime identity")
	}
	record, err = evidence.ConfirmResourceGone(ctx, record.ID, record.Revision, record.DaemonID, resource.Role, resource.Name, resource.ID)
	if err != nil || record.Resources[0].State != "gone" || !reflect.DeepEqual(sealed, *record.Receipt) {
		t.Fatal("cleanup rewrote sealed receipt", err)
	}
}

func TestExecutionLockCancellationAndLiveSupervisorRecoveryRefuse(t *testing.T) {
	s, record := executionFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.BeginResourceCreation(ctx, record.ID, record.Revision, "agent"); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled mutation entered lock", err)
	}
	evidence, err := OpenEvidence(s.Path(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer evidence.Close()
	if _, err := evidence.RecoverInterrupted(context.Background(), record.ID, record.Revision); err == nil {
		t.Fatal("live supervisor was called lost")
	}
	got, err := evidence.Execution(record.ID)
	if err != nil || got.Revision != record.Revision || got.Receipt != nil {
		t.Fatal("failed operation changed custody")
	}
}

func TestExecutionKeyLossFencesNewWorkButKeepsInFlightOutcomeCustody(t *testing.T) {
	s, record := executionFixture(t)
	record = prepareFixtureLaunch(t, s, record)
	ctx := context.Background()
	var err error
	record, err = s.BeginResourceCreation(ctx, record.ID, record.Revision, "guard")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.root.Remove("owner.key"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginResourceCreation(ctx, record.ID, record.Revision, "agent"); err == nil {
		t.Fatal("cached key authorized new creation")
	}
	record, err = s.RecordResourceCreated(ctx, record.ID, record.Revision, "guard", strings.Repeat("b", 64))
	if err != nil {
		t.Fatal("key loss erased an already-in-flight create result", err)
	}
	if _, err := s.BeginResourceStart(ctx, record.ID, record.Revision, "guard"); err == nil {
		t.Fatal("cached key authorized a new start")
	}
}

func TestExecutionKnownDetailLossCannotProduceCompleteReceipt(t *testing.T) {
	for _, loss := range []networkview.Loss{{DetailTruncated: true}, {Records: 1}, {Unknown: true}} {
		s, record := executionFixture(t)
		ctx := context.Background()
		snapshot := record.Snapshot
		snapshot.Sequence, snapshot.Terminal, snapshot.Loss = 1, true, loss
		exact := networkview.MetricCoverage{Status: "exact"}
		snapshot.Coverage = networkview.Coverage{ProxyBytes: exact, Connections: exact, UpstreamFailures: exact, KernelPackets: exact, GuardDenials: exact,
			MaintenanceQueries: exact, MaintenanceBytes: exact, SocketInventory: exact, BoundaryAttribution: exact}
		var err error
		record, err = s.AcceptSnapshot(ctx, record.ID, record.Revision, snapshot)
		if err != nil {
			t.Fatal(err)
		}
		record, err = s.SealExecution(ctx, record.ID, record.Revision, "exited")
		if err != nil || record.Receipt.Completeness != "partial" || record.Receipt.Finality != "final" {
			t.Fatal("known evidence loss claimed complete", err)
		}
	}
}

func TestExecutionRenameBeforeFsyncErrorRetainsIntentForDurableReconciliation(t *testing.T) {
	s, record := executionFixture(t)
	record = prepareFixtureLaunch(t, s, record)
	wanted := errors.New("injected directory sync failure")
	s.syncDir = func(dir *os.File) error {
		// Qualification/input reads also confirm durability. Inject only after
		// this execution's intended rename, not during those preflight reads.
		current, err := s.Execution(record.ID)
		if err != nil {
			return err
		}
		if current.Resources[0].State == "creating" {
			return wanted
		}
		return dir.Sync()
	}
	uncertain, err := s.BeginResourceCreation(context.Background(), record.ID, record.Revision, "controller")
	if !errors.Is(err, wanted) || uncertain.ID != record.ID {
		t.Fatal("publication error lost operation identity", err)
	}
	got, err := s.Execution(record.ID)
	if err != nil || got.Resources[0].State != "creating" || got.Revision != record.Revision+1 {
		t.Fatal("rename-before-sync outcome silently treated as absent", err)
	}
	if _, err := s.ConfirmExecution(context.Background(), got.ID, got.Revision); !errors.Is(err, wanted) {
		t.Fatal("read alone was mistaken for durable publication")
	}
	s.syncDir = nil
	confirmed, err := s.ConfirmExecution(context.Background(), got.ID, got.Revision)
	if err != nil || confirmed.Revision != got.Revision {
		t.Fatal("reconciliation repeated the operation or failed", err)
	}
}

func TestExecutionListingHasBoundedSummaryPagesAndContinuation(t *testing.T) {
	s, record := executionFixture(t)
	for range ExecutionPageSize {
		if _, err := executionTrial(t, s).CreateExecution(context.Background(), ExecutionSpec{Project: record.Project, PolicyFingerprint: record.Snapshot.PolicyFingerprint,
			InputsID: record.InputsID,
			Runtime:  record.Runtime, DaemonID: record.DaemonID, Endpoint: record.Endpoint, GatewayImage: record.GatewayImage, ClientImage: record.ClientImage}, "enforcement", nil); err != nil {
			t.Fatal(err)
		}
	}
	evidence, err := OpenEvidence(s.Path(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer evidence.Close()
	first, err := evidence.Executions("")
	if err != nil || len(first.Executions) != ExecutionPageSize || first.Next == "" || first.Incomplete {
		t.Fatal("first summary page lost continuation", err)
	}
	second, err := evidence.Executions(first.Next)
	if err != nil || len(second.Executions) != 1 || second.Next != "" || second.Incomplete || second.Executions[0].ID <= first.Next {
		t.Fatal("summary pagination lost or repeated execution", err)
	}
}
