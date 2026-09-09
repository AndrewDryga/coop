package networkstate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkview"
)

func unobservedSessionFixture(t *testing.T, mode egress.Mode) (*Store, UnobservedSessionExecutionSpec) {
	t.Helper()
	f := newCatalogFixture(t)
	policy, err := f.store.Admit(f.repo, Admission{InvocationMode: &mode})
	if err != nil {
		t.Fatal(err)
	}
	owner := SessionCatalogOwner{SessionID: "session-fixture", OperationID: "operation-fixture", ForkName: "fork-fixture",
		ForkGeneration: strings.Repeat("a", 32), AuthorityDigest: strings.Repeat("b", 64)}
	record := f.record
	record.Fingerprint, record.Session = policy.Fingerprint, &owner
	record.ReferenceDigest = strings.Repeat("d", 64)
	record.Members[0].InputsID, err = f.store.RecordInputs(LaunchInputs{Environment: []byte("CAPTURED=value\n")})
	if err != nil {
		t.Fatal(err)
	}
	id, err := f.store.RecordCatalog(record)
	if err != nil {
		t.Fatal(err)
	}
	return f.store, UnobservedSessionExecutionSpec{CatalogID: id, AttemptID: strings.Repeat("c", 32), Owner: owner}
}

func TestUnobservedSessionCreationRetainsAmbiguousPublicationIdentity(t *testing.T) {
	s, spec := unobservedSessionFixture(t, egress.Open)
	wanted := errors.New("injected session publication sync failure")
	s.syncDir = func(dir *os.File) error {
		entries, err := os.ReadDir(s.Path())
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "execution-") && strings.HasSuffix(entry.Name(), ".json") {
				return wanted
			}
		}
		return dir.Sync()
	}
	record, err := s.CreateUnobservedSessionExecution(t.Context(), spec)
	if !errors.Is(err, wanted) || record.ID == "" || record.Epoch == "" || record.Revision != 1 {
		t.Fatal("ambiguous session creation lost its exact identity", err)
	}
	if _, err := s.ConfirmExecution(t.Context(), record.ID, record.Revision); !errors.Is(err, wanted) {
		t.Fatal("readable publication was mistaken for durable proof", err)
	}
	s.syncDir = nil
	confirmed, err := s.ConfirmExecution(t.Context(), record.ID, record.Revision)
	if err != nil || !equalJSON(confirmed, record) {
		t.Fatal("durability reconciliation recreated or changed the attempt", err)
	}
	sealed, err := s.FinalizeUnobservedSessionExecution(t.Context(), record.ID, record.Revision, "launch_failed")
	if err != nil || sealed.Receipt == nil || sealed.Receipt.Workload != "launch_failed" {
		t.Fatal("unstarted ambiguous attempt was not settled", err)
	}
}

func TestUnobservedSessionExecutionIsModeHonestAndKeylessReadable(t *testing.T) {
	for _, mode := range []egress.Mode{egress.Open, egress.None} {
		t.Run(string(mode), func(t *testing.T) {
			s, spec := unobservedSessionFixture(t, mode)
			ctx := t.Context()
			record, err := s.CreateUnobservedSessionExecution(ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			if record.ID == record.Epoch || record.Purpose != SessionUnobservedPurpose || record.Supervisor.PID != os.Getpid() ||
				record.Runtime != "" || record.GatewayImage != "" || record.QualificationID != "" || record.InputsID != "" ||
				len(record.Resources) != 0 || record.Receipt != nil || record.Snapshot.Mode != mode || record.Snapshot.Sequence != 0 ||
				record.Snapshot.Terminal || record.Snapshot.Availability != "unavailable" || record.Snapshot.Scope != "not-observed" ||
				record.Snapshot.Counters != nil || record.Snapshot.Coverage != (networkview.Coverage{}) || !record.Snapshot.Loss.Unknown {
				t.Fatal("unobserved session invented gateway authority or a measurement")
			}
			if _, err := s.CreateExecution(ctx, ExecutionSpec{Project: record.Project, PolicyFingerprint: record.Snapshot.PolicyFingerprint}); err == nil {
				t.Fatal("gateway creation accepted open/none capture")
			}
			if _, err := s.RecoverQualification(record.ID); err == nil {
				t.Fatal("unobserved execution became a qualification")
			}
			for name, operation := range map[string]func() error{
				"snapshot": func() error { _, err := s.AcceptSnapshot(ctx, record.ID, record.Revision, record.Snapshot); return err },
				"resource": func() error { _, err := s.BeginResourceCreation(ctx, record.ID, record.Revision, "agent"); return err },
				"artifact": func() error {
					_, err := s.PrepareLaunchConfig(ctx, record.ID, record.Revision, []byte(`{"forbidden":true}`))
					return err
				},
				"filtered seal": func() error { _, err := s.SealExecution(ctx, record.ID, record.Revision, "exited"); return err },
			} {
				if err := operation(); err == nil {
					t.Fatal("unobserved execution entered gateway method", name)
				}
			}
			if retained, err := s.Execution(record.ID); err != nil || !equalJSON(retained, record) {
				t.Fatal("rejected gateway operation changed custody", err)
			}
			record, err = s.FinalizeUnobservedSessionExecution(ctx, record.ID, record.Revision, "exited")
			if err != nil {
				t.Fatal(err)
			}
			expected := record.Snapshot
			expected.Terminal = true
			if record.Receipt == nil || record.Receipt.Finality != "final" || record.Receipt.Completeness != "unknown" ||
				record.Receipt.Cleanup != "pending" || record.Receipt.CollectorVersion != "" || !equalJSON(record.Receipt.Snapshot, expected) {
				t.Fatal("unobserved receipt is not an exact terminal projection")
			}
			if _, err := s.FinalizeUnobservedSessionExecution(ctx, record.ID, record.Revision, "cancelled"); err == nil {
				t.Fatal("sealed receipt accepted a changed outcome")
			}
			if err := s.root.Remove("owner.key"); err != nil {
				t.Fatal(err)
			}
			evidence, err := OpenEvidence(s.Path(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer evidence.Close()
			view, err := evidence.Inspect(record.ID, time.Now().UTC(), false)
			if err != nil || view.Receipt == nil || view.Cleanup != "pending" || view.Current != nil ||
				view.Receipt.Completeness != "unknown" || view.AggregateObservation.Receipt.CollectorVersion != "" {
				t.Fatal("keyless read changed unobserved evidence", err)
			}
			if _, err := evidence.CleanupRunFiles(ctx, record.ID, record.Revision); err == nil {
				t.Fatal("unobserved files were cleaned without workload absence proof")
			}
			if _, err := evidence.RemoveLaunchConfig(ctx, record.ID, record.Revision); err == nil {
				t.Fatal("unobserved record entered artifact cleanup")
			}
		})
	}
}

func TestUnobservedSessionExecutionRejectsGatewayFieldsAndReceiptDrift(t *testing.T) {
	s, spec := unobservedSessionFixture(t, egress.Open)
	record, err := s.CreateUnobservedSessionExecution(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Execution){
		"runtime":       func(r *Execution) { r.Runtime = "docker" },
		"endpoint":      func(r *Execution) { r.Endpoint = "unix:///daemon.sock" },
		"gateway":       func(r *Execution) { r.GatewayImage = "sha256:" + strings.Repeat("a", 64) },
		"client":        func(r *Execution) { r.ClientImage = "sha256:" + strings.Repeat("b", 64) },
		"candidate":     func(r *Execution) { r.CandidateID = strings.Repeat("c", 64) },
		"qualification": func(r *Execution) { r.QualificationID = strings.Repeat("d", 64) },
		"inputs":        func(r *Execution) { r.InputsID = strings.Repeat("e", 64) },
		"resource":      func(r *Execution) { r.Resources = []Resource{{Role: "agent"}} },
		"artifact":      func(r *Execution) { r.LaunchConfig.Name = "unexpected" },
		"files":         func(r *Execution) { r.RunFiles.Name = "unexpected" },
		"started":       func(r *Execution) { r.WorkloadStarted = true },
		"observer":      func(r *Execution) { r.ObserverAfterWorkload = true },
		"purpose":       func(r *Execution) { r.Purpose = "workload" },
		"mode":          func(r *Execution) { r.Snapshot.Mode = egress.Filtered },
		"sequence":      func(r *Execution) { r.Snapshot.Sequence = 1 },
		"counters":      func(r *Execution) { r.Snapshot.Counters = &networkview.Counters{} },
		"coverage":      func(r *Execution) { r.Snapshot.Coverage.ProxyBytes.Status = "exact" },
		"loss":          func(r *Execution) { r.Snapshot.Loss.Unknown = false },
	} {
		changed := record
		mutate(&changed)
		if validExecution(changed) == nil {
			t.Fatal("accepted unexpected authority or observation", name)
		}
	}
	record, err = s.FinalizeUnobservedSessionExecution(t.Context(), record.ID, record.Revision, "exited")
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*networkview.Receipt){
		"version":           func(r *networkview.Receipt) { r.Version++ },
		"finality":          func(r *networkview.Receipt) { r.Finality = "provisional" },
		"completeness":      func(r *networkview.Receipt) { r.Completeness = "partial" },
		"cleanup":           func(r *networkview.Receipt) { r.Cleanup = "complete" },
		"runtime":           func(r *networkview.Receipt) { r.Runtime = "docker" },
		"collector":         func(r *networkview.Receipt) { r.CollectorVersion = "gateway-v1" },
		"snapshot version":  func(r *networkview.Receipt) { r.Snapshot.Version++ },
		"snapshot mode":     func(r *networkview.Receipt) { r.Snapshot.Mode = egress.None },
		"snapshot terminal": func(r *networkview.Receipt) { r.Snapshot.Terminal = false },
		"snapshot asof":     func(r *networkview.Receipt) { r.Snapshot.AsOf = r.Snapshot.AsOf.Add(time.Nanosecond) },
		"snapshot counters": func(r *networkview.Receipt) { r.Snapshot.Counters = &networkview.Counters{} },
		"snapshot coverage": func(r *networkview.Receipt) { r.Snapshot.Coverage.ProxyBytes.Status = "unavailable" },
	} {
		changed := record
		receipt := *record.Receipt
		changed.Receipt = &receipt
		mutate(changed.Receipt)
		if err := changed.Receipt.SealDigest(); err != nil {
			t.Fatal(err)
		}
		if validExecution(changed) == nil {
			t.Fatal("accepted resealed receipt drift", name)
		}
	}
}

func TestRecoverInterruptedUnobservedSessionKeepsUnknownCoverage(t *testing.T) {
	s, spec := unobservedSessionFixture(t, egress.None)
	record, err := s.CreateUnobservedSessionExecution(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := OpenEvidence(s.Path(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer evidence.Close()
	if _, err := evidence.RecoverInterrupted(t.Context(), record.ID, record.Revision); err == nil {
		t.Fatal("live supervisor was declared interrupted")
	}
	record.Supervisor.StartToken += "-different-generation"
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.publish("execution-"+record.ID+".json", data, true); err != nil {
		t.Fatal(err)
	}
	if err := s.root.Remove("owner.key"); err != nil {
		t.Fatal(err)
	}
	recovered, err := evidence.RecoverInterrupted(t.Context(), record.ID, record.Revision)
	if err != nil || recovered.Receipt == nil || recovered.Receipt.Workload != "supervisor_lost" ||
		recovered.Receipt.Completeness != "unknown" || recovered.Receipt.Cleanup != "pending" ||
		recovered.Receipt.Snapshot.Counters != nil || recovered.Receipt.Snapshot.Coverage != (networkview.Coverage{}) {
		t.Fatal("recovery fabricated observed evidence", err)
	}
}

func TestUnobservedSessionCleanupRequiresAbsenceAndPreservesSealedReceipt(t *testing.T) {
	s, spec := unobservedSessionFixture(t, egress.Open)
	record, err := s.CreateUnobservedSessionExecution(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	record, err = s.PrepareRunFiles(t.Context(), record.ID, record.Revision)
	if err != nil {
		t.Fatal(err)
	}
	root, err := s.RunFilesPath(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "renewable.env"), []byte("TOKEN=transient-only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "unrelated")
	if err := os.WriteFile(outside, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	record, err = s.FinishRunFiles(t.Context(), record.ID, record.Revision)
	if err != nil {
		t.Fatal(err)
	}
	record, err = s.FinalizeUnobservedSessionExecution(t.Context(), record.ID, record.Revision, "cancelled")
	if err != nil || record.Receipt == nil || record.Receipt.Cleanup != "pending" {
		t.Fatal("client completion was mistaken for cleanup proof", err)
	}
	sealed := *record.Receipt
	evidence, err := OpenEvidence(s.Path(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer evidence.Close()
	if _, err := evidence.MarkUnobservedSessionWorkloadGone(t.Context(), record.ID, record.Revision); err == nil {
		t.Fatal("live supervisor accepted cleanup proof")
	}
	record.Supervisor.StartToken += "-departed"
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.publish("execution-"+record.ID+".json", data, true); err != nil {
		t.Fatal(err)
	}
	if err := s.root.Remove("owner.key"); err != nil {
		t.Fatal(err)
	}
	if _, err := evidence.CleanupRunFiles(t.Context(), record.ID, record.Revision); err == nil {
		t.Fatal("supervisor death was mistaken for daemon workload absence")
	}
	record, err = evidence.MarkUnobservedSessionWorkloadGone(t.Context(), record.ID, record.Revision)
	if err != nil {
		t.Fatal(err)
	}
	record, err = evidence.CleanupRunFiles(t.Context(), record.ID, record.Revision)
	if err != nil {
		t.Fatal(err)
	}
	view, err := evidence.Inspect(record.ID, time.Now().UTC(), false)
	if err != nil || view.Cleanup != "complete" || !equalJSON(*record.Receipt, sealed) {
		t.Fatal("later cleanup rewrote the sealed outcome", err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("transient credential tree remains", err)
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "preserve" {
		t.Fatal("cleanup followed a symlink outside owned files", err)
	}
	if _, err := evidence.CleanupRunFiles(t.Context(), record.ID, record.Revision); err != nil {
		t.Fatal("exact cleanup retry failed", err)
	}
}
