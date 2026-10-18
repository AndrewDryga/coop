package networkstate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
	record, err := executionSmoke(t, s).CreateExecution(context.Background(), ExecutionSpec{Project: project, PolicyFingerprint: policy.Fingerprint,
		Runtime: "docker", DaemonID: "fixture-daemon", Endpoint: "unix:///fixture.sock", GatewayImage: "sha256:" + strings.Repeat("a", 64), ClientImage: "sha256:" + strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	return s, record
}

// AuthorizeLaunch records nothing: whether a resource exists is asked of the runtime by its fixed
// name. It still refuses a container whose launch artifacts are not in place.
func TestAuthorizeLaunchChecksWithoutRecording(t *testing.T) {
	s, record := executionFixture(t)
	ctx := context.Background()
	if record.Revision != 1 || record.ID == record.Epoch || len(record.Resources) != 5 {
		t.Fatal("missing unique run identity")
	}
	if err := s.AuthorizeLaunch(ctx, record.ID, "guard"); err == nil {
		t.Fatal("a container launched before its artifacts were prepared")
	}
	record = prepareFixtureLaunch(t, s, record)
	for _, role := range []string{"ipc", "guard", "agent"} {
		if err := s.AuthorizeLaunch(ctx, record.ID, role); err != nil {
			t.Fatal(role, err)
		}
	}
	persisted, err := s.Execution(record.ID)
	if err != nil || persisted.Revision != record.Revision {
		t.Fatalf("an authorization wrote the record: %v, revision %d to %d", err, record.Revision, persisted.Revision)
	}
	if err := s.AuthorizeLaunch(ctx, record.ID, "unknown"); err == nil {
		t.Fatal("an unknown role was authorized")
	}
}

// A version 1 record journaled each create and start. It reads as version 2, with every resource
// short of gone planned, and the next write stores version 2.
func TestExecutionVersionOneReadsAsVersionTwo(t *testing.T) {
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
	for i, state := range []string{"started", "created", "creating", "gone", "planned"} {
		resource := raw["resources"].([]any)[i].(map[string]any)
		resource["state"] = state
		if state == "started" || state == "created" {
			resource["id"] = strings.Repeat("c", 64)
		}
	}
	if data, err = json.Marshal(raw); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := s.Execution(record.ID)
	if err != nil || got.Version != ExecutionVersion {
		t.Fatalf("version 1 record = %+v, %v", got, err)
	}
	for i, want := range []string{"planned", "planned", "planned", "gone", "planned"} {
		if got.Resources[i].State != want {
			t.Fatalf("resource %s = %q, want %q", got.Resources[i].Role, got.Resources[i].State, want)
		}
	}
	if _, err := s.RecordWorkloadStarted(context.Background(), got.ID, got.Revision); err != nil {
		t.Fatal(err)
	}
	if data, err = os.ReadFile(path); err != nil || !strings.Contains(string(data), `"version":2`) || strings.Contains(string(data), `"id":"cccc`) {
		t.Fatalf("the next write did not store version 2: %s, %v", data, err)
	}
}

func TestExecutionConcurrentCASHasOnlyOneWinner(t *testing.T) {
	s, record := executionFixture(t)
	record = prepareFixtureLaunch(t, s, record)
	var workers sync.WaitGroup
	results := make(chan error, 20)
	for range 20 {
		workers.Go(func() {
			_, err := s.RecordWorkloadStarted(context.Background(), record.ID, record.Revision)
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
	if err := s.AuthorizeLaunch(ctx, record.ID, "agent"); err == nil {
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
		Runtime: record.Runtime, DaemonID: record.DaemonID, Endpoint: record.Endpoint, GatewayImage: record.GatewayImage}); err == nil {
		t.Fatal("stale open Store reused missing key for a new launch")
	}
	if err := approve(s, record.Project, egress.Open, nil, nil); err == nil {
		t.Fatal("stale open Store approved access after key loss")
	}
	if err := os.Remove(record.Project); err != nil {
		t.Fatal(err)
	}
	if _, err := evidence.Execution(record.ID); err != nil {
		t.Fatal("project/key loss hid cleanup custody", err)
	}
	resource := record.Resources[0]
	if _, err := evidence.ConfirmResourceGone(ctx, record.ID, record.Revision, "another-daemon", resource.Role, resource.Name); err == nil {
		t.Fatal("cleanup crossed runtime identity")
	}
	record, err = evidence.ConfirmResourceGone(ctx, record.ID, record.Revision, record.DaemonID, resource.Role, resource.Name)
	if err != nil || record.Resources[0].State != "gone" || !reflect.DeepEqual(sealed, *record.Receipt) {
		t.Fatal("cleanup rewrote sealed receipt", err)
	}
}

func TestExecutionLockCancellationAndLiveSupervisorRecoveryRefuse(t *testing.T) {
	s, record := executionFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.AuthorizeLaunch(ctx, record.ID, "agent"); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled check entered lock", err)
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

func TestExecutionKeyLossFencesNewWork(t *testing.T) {
	s, record := executionFixture(t)
	record = prepareFixtureLaunch(t, s, record)
	ctx := context.Background()
	if err := s.AuthorizeLaunch(ctx, record.ID, "guard"); err != nil {
		t.Fatal(err)
	}
	if err := s.root.Remove("owner.key"); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"guard", "agent"} {
		if err := s.AuthorizeLaunch(ctx, record.ID, role); err == nil {
			t.Fatalf("cached key authorized a new %s create or start", role)
		}
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

func TestExecutionListingHasBoundedSummaryPagesAndContinuation(t *testing.T) {
	s, record := executionFixture(t)
	for range ExecutionPageSize {
		if _, err := executionSmoke(t, s).CreateExecution(context.Background(), ExecutionSpec{Project: record.Project, PolicyFingerprint: record.Snapshot.PolicyFingerprint,
			Runtime: record.Runtime, DaemonID: record.DaemonID, Endpoint: record.Endpoint, GatewayImage: record.GatewayImage, ClientImage: record.ClientImage}); err != nil {
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

// Every page of the run listing reads the whole folder, so a file no run is named after counts as
// unreadable on the first page only, and is named there for the operator to find.
func TestExecutionsCountAStrayFileOnce(t *testing.T) {
	s := openStore(t)
	if err := os.WriteFile(filepath.Join(s.Path(), "execution-copy.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	evidence, err := OpenEvidence(s.Path(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer evidence.Close()
	first, err := evidence.Executions("")
	if err != nil || first.Unreadable != 1 || !slices.Equal(first.UnreadableNames, []string{"execution-copy.json"}) {
		t.Fatalf("first page = %+v, %v; want the stray file counted and named", first, err)
	}
	later, err := evidence.Executions(strings.Repeat("0", 32))
	if err != nil || later.Unreadable != 0 || len(later.UnreadableNames) != 0 {
		t.Fatalf("a later page = %+v, %v; want the stray file counted once", later, err)
	}
}
