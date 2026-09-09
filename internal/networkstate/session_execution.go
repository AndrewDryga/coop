package networkstate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkview"
	"github.com/AndrewDryga/coop/internal/processidentity"
)

const SessionUnobservedPurpose = "session-unobserved"

// UnobservedSessionExecutionSpec carries only private catalog custody. Ordinary
// open/none launches must not fabricate a gateway or claim measured zero traffic.
type UnobservedSessionExecutionSpec struct {
	CatalogID, AttemptID string
	Owner                SessionCatalogOwner
}

func (s *Store) CreateUnobservedSessionExecution(ctx context.Context, spec UnobservedSessionExecutionSpec) (Execution, error) {
	if err := s.authorityAvailable(); err != nil {
		return Execution{}, err
	}
	if !safeRecordToken(spec.AttemptID, 128) {
		return Execution{}, errors.New("unobserved session execution requires an exact attempt")
	}
	catalog, err := s.SessionCatalog(spec.CatalogID, spec.Owner)
	if err != nil {
		return Execution{}, err
	}
	policy, err := s.LoadSnapshot(catalog.Project, catalog.Fingerprint)
	if err != nil {
		return Execution{}, err
	}
	if policy.Mode != egress.Open && policy.Mode != egress.None {
		return Execution{}, errors.New("unobserved session execution requires open or none capture")
	}
	id, err := randomExecutionID()
	if err != nil {
		return Execution{}, err
	}
	epoch, err := randomExecutionID()
	if err != nil {
		return Execution{}, err
	}
	supervisor := Supervisor{PID: os.Getpid(), StartToken: processidentity.StartToken(os.Getpid())}
	if processidentity.Inspect(supervisor.PID, supervisor.StartToken) != processidentity.Match {
		return Execution{}, errors.New("unobserved session supervisor identity unavailable")
	}
	now := time.Now().UTC()
	record := Execution{
		Version: ExecutionVersion, Revision: 1, ID: id, Epoch: epoch,
		Project: catalog.Project, Scope: policy.Scope, Supervisor: supervisor,
		Purpose: SessionUnobservedPurpose, SessionID: spec.Owner.SessionID,
		AttemptID: spec.AttemptID, AuthorityDigest: spec.Owner.AuthorityDigest, StartedAt: now,
		RunFiles: RunFiles{Name: "runfiles-" + id, State: "planned"},
		Snapshot: unobservedSessionSnapshot(id, epoch, policy.Fingerprint, policy.Mode, now),
	}
	if err := validExecution(record); err != nil {
		return Execution{}, err
	}
	err = s.lockExecution(ctx, id, func() error {
		data, err := json.Marshal(record)
		if err != nil {
			return err
		}
		return s.publish("execution-"+id+".json", data, false)
	})
	return record, err
}

func unobservedSessionSnapshot(id, epoch, fingerprint string, mode egress.Mode, started time.Time) networkview.Snapshot {
	return networkview.Snapshot{
		Version: networkview.Version, RunID: id, Epoch: epoch, PolicyFingerprint: fingerprint, Mode: mode,
		Availability: "unavailable", AsOf: started, Scope: "not-observed", Projection: "owner-local",
		Loss: networkview.Loss{Unknown: true, Reasons: []string{"network_observation_unavailable"}},
	}
}

func validUnobservedSessionExecution(record Execution) error {
	if record.Version != ExecutionVersion || record.Revision == 0 || !lowerHex(record.ID, 32) || !lowerHex(record.Epoch, 32) ||
		!lowerHex(record.Scope, 64) || !filepath.IsAbs(record.Project) || filepath.Clean(record.Project) != record.Project ||
		record.Supervisor.PID <= 1 || !processidentity.Stable(record.Supervisor.StartToken) || record.StartedAt.IsZero() ||
		!safeRecordToken(record.SessionID, 128) || !safeRecordToken(record.AttemptID, 128) || !lowerHex(record.AuthorityDigest, 64) ||
		!lowerHex(record.Snapshot.PolicyFingerprint, 64) || record.Snapshot.Mode != egress.Open && record.Snapshot.Mode != egress.None {
		return errors.New("invalid unobserved session execution identity")
	}
	// An explicit allowlist keeps this purpose closed when gateway fields grow.
	// Only ordinary workload/file cleanup custody exists; no gateway resources,
	// inputs, qualification, metrics, or observer authority can be introduced.
	if err := validRunFiles(record); err != nil {
		return err
	}
	expected := Execution{
		Version: ExecutionVersion, Revision: record.Revision, ID: record.ID, Epoch: record.Epoch,
		Project: record.Project, Scope: record.Scope, Supervisor: record.Supervisor,
		Purpose: SessionUnobservedPurpose, SessionID: record.SessionID, AttemptID: record.AttemptID,
		AuthorityDigest: record.AuthorityDigest, StartedAt: record.StartedAt, Receipt: record.Receipt,
		RunFiles: record.RunFiles, SessionWorkloadGone: record.SessionWorkloadGone,
		Snapshot: unobservedSessionSnapshot(record.ID, record.Epoch, record.Snapshot.PolicyFingerprint, record.Snapshot.Mode, record.StartedAt),
	}
	if !equalJSON(expected, record) {
		return errors.New("unobserved session execution contains unexpected authority or observations")
	}
	if receipt := record.Receipt; receipt != nil {
		if receipt.EndedAt == nil || receipt.EndedAt.IsZero() ||
			!slices.Contains([]string{"exited", "cancelled", "launch_failed", "runtime_failed", "supervisor_lost"}, receipt.Workload) {
			return errors.New("invalid unobserved session receipt outcome")
		}
		if !slices.Contains([]string{"pending", "complete"}, receipt.Cleanup) ||
			receipt.Cleanup == "complete" && inspectedCleanup(record) != "complete" {
			return errors.New("unobserved session receipt claims unproven cleanup")
		}
		wanted, err := unobservedSessionReceipt(record, receipt.Workload, receipt.Cleanup, *receipt.EndedAt)
		if err != nil || !equalJSON(wanted, *receipt) {
			return errors.New("invalid sealed unobserved session receipt")
		}
	}
	return nil
}

func unobservedSessionReceipt(record Execution, workload, cleanup string, ended time.Time) (networkview.Receipt, error) {
	snapshot := record.Snapshot
	snapshot.Terminal = true
	receipt := networkview.Receipt{
		Version: networkview.Version, ID: record.ID, Snapshot: snapshot,
		StartedAt: record.StartedAt, EndedAt: &ended, Finality: "final", Completeness: "unknown",
		Workload: workload, Cleanup: cleanup, SessionID: record.SessionID, AttemptID: record.AttemptID,
		AuthorityDigest: record.AuthorityDigest, DigestScope: "owner-local",
	}
	err := receipt.SealDigest()
	return receipt, err
}

func (s *Store) FinalizeUnobservedSessionExecution(ctx context.Context, id string, revision networkview.Count, workload string) (Execution, error) {
	return s.mutateExecution(ctx, id, revision, func(record *Execution) (bool, error) {
		if len(s.key) != 32 || record.Supervisor.PID != os.Getpid() || record.Supervisor.StartToken != processidentity.StartToken(os.Getpid()) {
			return false, errors.New("unobserved session execution is not owned by this supervisor")
		}
		if !slices.Contains([]string{"exited", "cancelled", "launch_failed", "runtime_failed"}, workload) {
			return false, errors.New("unknown session workload outcome")
		}
		return sealUnobservedSessionExecution(record, workload)
	})
}

func sealUnobservedSessionExecution(record *Execution, workload string) (bool, error) {
	if record.Purpose != SessionUnobservedPurpose || record.Receipt != nil {
		return false, errors.New("unobserved session execution is not open")
	}
	receipt, err := unobservedSessionReceipt(*record, workload, inspectedCleanup(*record), time.Now().UTC())
	if err != nil {
		return false, err
	}
	record.Receipt = &receipt
	return true, nil
}

// MarkUnobservedSessionWorkloadGone follows the parent's exact captured-runtime
// cleanup, including process-group quiescence. Supervisor departure alone cannot
// prove a daemon-owned workload is gone. This keyless transition grants no launch
// authority and does not rewrite an already sealed receipt.
func (e *Evidence) MarkUnobservedSessionWorkloadGone(ctx context.Context, id string, revision networkview.Count) (Execution, error) {
	return e.files.mutateExecution(ctx, id, revision, func(record *Execution) (bool, error) {
		if record.Purpose != SessionUnobservedPurpose {
			return false, errors.New("ordinary session cleanup requires an unobserved execution")
		}
		state := processidentity.Inspect(record.Supervisor.PID, record.Supervisor.StartToken)
		if state != processidentity.Gone && state != processidentity.Mismatch {
			return false, errors.New("ordinary session supervisor is live or its identity is uncertain")
		}
		changed := !record.SessionWorkloadGone
		record.SessionWorkloadGone = true
		return changed, nil
	})
}
