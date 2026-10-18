package networkstate

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/networkview"
	"github.com/AndrewDryga/coop/internal/processidentity"
)

func (s *Store) ownedExecution(record *Execution) error {
	if len(s.key) != 32 || record.Supervisor.PID != os.Getpid() || !processidentity.Same(record.Supervisor.StartToken, processidentity.StartToken(os.Getpid())) {
		return errors.New("network execution is not owned by this supervisor")
	}
	if record.Receipt != nil {
		return errors.New("network execution evidence is sealed")
	}
	return nil
}

func resourceAt(record *Execution, role string) (*Resource, error) {
	for i := range record.Resources {
		if record.Resources[i].Role == role {
			return &record.Resources[i], nil
		}
	}
	return nil, errors.New("unknown network resource role")
}

// AuthorizeLaunch is the check before each runtime create or start of a run's resource: the
// run is this supervisor's and unsealed, its epoch is not over, the network authority and the
// qualification it was launched under still hold, and a container's launch artifacts are in
// place. It records nothing: whether a resource exists is asked of the runtime by its name.
func (s *Store) AuthorizeLaunch(ctx context.Context, id, role string) error {
	return s.lockExecution(ctx, id, func() error {
		record, err := s.execution(id)
		if err != nil {
			return err
		}
		if err := s.ownedExecution(&record); err != nil {
			return err
		}
		if record.Snapshot.Terminal {
			return errors.New("terminal network epoch cannot authorize new runtime work")
		}
		if err := s.intactAuthority(); err != nil {
			return err
		}
		if err := s.executionLaunchAuthority(record); err != nil {
			return err
		}
		resource, err := resourceAt(&record, role)
		if err != nil {
			return err
		}
		if resource.State == "gone" {
			return errors.New("network resource was already removed")
		}
		if resource.Kind != "container" {
			return nil
		}
		if record.Artifact.State != "prepared" {
			return errors.New("network launch artifacts are not durably prepared")
		}
		if _, err := s.readArtifactLaunch(record.Artifact); err != nil {
			return err
		}
		if role == "agent" {
			files, err := s.openArtifactFiles(record.Artifact)
			if err != nil {
				return err
			}
			_ = files.Close()
		}
		return nil
	})
}

// RecordWorkloadStarted notes that the agent container started, which a smoke run must show.
func (s *Store) RecordWorkloadStarted(ctx context.Context, id string, revision networkview.Count) (Execution, error) {
	return s.mutateExecution(ctx, id, revision, func(record *Execution) (bool, error) {
		if err := s.ownedExecution(record); err != nil {
			return false, err
		}
		changed := !record.WorkloadStarted
		record.WorkloadStarted = true
		return changed, nil
	})
}

// Read/cleanup stay structural. Only new runtime mutations recheck the retained
// qualification; losing it must not hide exact resource custody.
func (s *Store) executionLaunchAuthority(record Execution) error {
	if record.QualificationContract != QualificationContract {
		return errors.New("historical network execution is inspection and cleanup only")
	}
	if record.Purpose != "workload" {
		return nil
	}
	qualification, err := s.Qualification(record.QualificationID)
	if err != nil {
		return errors.New("completed network qualification is unavailable or changed")
	}
	candidate := qualification.Candidate
	if candidate.ClientImage != record.ClientImage || candidate.GatewayImage != record.GatewayImage ||
		candidate.Runtime.DaemonID != record.DaemonID || candidate.Runtime.Endpoint != record.Endpoint {
		return errors.New("network execution launch binding changed")
	}
	return nil
}

// RecordProtected replaces the run's protection envelope with a grown one. The
// envelope only ever grows — a host address that disappears keeps its kernel
// denial — so a candidate that drops a recorded prefix, passes the qualified cap
// or is not canonical is refused. It is evidence for a later `why`, written
// after the kernel set already refuses the new ranges.
func (s *Store) RecordProtected(ctx context.Context, id string, revision networkview.Count, protected []netip.Prefix) (Execution, error) {
	return s.mutateExecution(ctx, id, revision, func(record *Execution) (bool, error) {
		if err := s.ownedExecution(record); err != nil {
			return false, err
		}
		if len(protected) > maxProtectedRanges {
			return false, errors.New("network protection envelope exceeds the qualified cap")
		}
		for _, prefix := range protected {
			if !prefix.IsValid() || prefix != prefix.Masked() || prefix.Addr().Is4In6() {
				return false, errors.New("network protection envelope is not canonical")
			}
		}
		for _, prior := range record.Protected {
			if !slices.Contains(protected, prior) {
				return false, errors.New("network protection envelope never shrinks")
			}
		}
		next := slices.Clone(protected)
		slices.SortFunc(next, func(a, b netip.Prefix) int { return strings.Compare(a.String(), b.String()) })
		if slices.Equal(next, record.Protected) {
			return false, nil
		}
		record.Protected = next
		return true, nil
	})
}

// AcceptSnapshot replaces one cumulative producer value; it never adds periodic
// totals. Host observation times belong outside the producer replay comparison.
func (s *Store) AcceptSnapshot(ctx context.Context, id string, revision networkview.Count, snapshot networkview.Snapshot) (Execution, error) {
	return s.mutateExecution(ctx, id, revision, func(record *Execution) (bool, error) {
		if err := s.ownedExecution(record); err != nil {
			return false, err
		}
		prior := record.Snapshot
		if snapshot.Version != networkview.Version || snapshot.RunID != record.ID || snapshot.Epoch != record.Epoch || snapshot.PolicyFingerprint != prior.PolicyFingerprint || snapshot.Mode != prior.Mode || snapshot.Sequence == 0 {
			return false, errors.New("network snapshot identity mismatch")
		}
		if snapshot.Sequence < prior.Sequence {
			return false, ErrSnapshotStale
		}
		if snapshot.Sequence == prior.Sequence {
			if equalJSON(prior, snapshot) {
				return false, nil
			}
			return false, errors.New("network snapshot changed under the same producer sequence")
		}
		if prior.Terminal {
			return false, errors.New("terminal network observation cannot reopen")
		}
		data, err := json.Marshal(snapshot)
		if err != nil || len(data) > 1<<20 {
			return false, errors.New("network snapshot exceeds its encoding envelope")
		}
		record.Snapshot = snapshot
		if record.ReadySequence == 0 && snapshot.Availability == "available" && !snapshot.Terminal && snapshot.Health.Enforcer.Status == "ready" &&
			snapshot.Health.Gateway.Status == "ready" && snapshot.Health.Collector.Status == "ready" && snapshot.Health.Resolver.Status == "ready" {
			record.ReadySequence = snapshot.Sequence
		}
		return true, nil
	})
}

// ConfirmResourceGone records that the runtime the run used no longer has the resource: its
// supervisor or recovery removed it, or found its name absent. It removes nothing and never infers
// absence from process death. Cleanup can advance after sealing without changing the receipt.
func (e *Evidence) ConfirmResourceGone(ctx context.Context, id string, revision networkview.Count, daemon, role, name string) (Execution, error) {
	return e.files.mutateExecution(ctx, id, revision, func(record *Execution) (bool, error) {
		resource, err := resourceAt(record, role)
		if err != nil {
			return false, err
		}
		if daemon != record.DaemonID || name != resource.Name {
			return false, errors.New("cleanup observation belongs to another network resource")
		}
		if resource.State == "gone" {
			return false, nil
		}
		resource.State = "gone"
		return true, nil
	})
}

func (s *Store) SealExecution(ctx context.Context, id string, revision networkview.Count, workload string) (Execution, error) {
	return s.mutateExecution(ctx, id, revision, func(record *Execution) (bool, error) {
		if err := s.ownedExecution(record); err != nil {
			return false, err
		}
		if !slices.Contains([]string{"exited", "cancelled", "launch_failed", "runtime_failed"}, workload) {
			return false, errors.New("unknown network workload outcome")
		}
		return sealExecution(record, workload, false)
	})
}

// RecordObserverAfterWorkload records a causal boundary established by the
// trusted host: agent absence was confirmed, THEN the same guard replied ready.
// An old terminal file plus eventual agent removal is not equivalent evidence.
func (s *Store) RecordObserverAfterWorkload(ctx context.Context, id string, revision networkview.Count) (Execution, error) {
	return s.mutateExecution(ctx, id, revision, func(record *Execution) (bool, error) {
		if err := s.ownedExecution(record); err != nil {
			return false, err
		}
		agent, err := resourceAt(record, "agent")
		if err != nil || agent.State != "gone" || record.Snapshot.Terminal {
			return false, errors.New("observer boundary requires confirmed agent absence and a nonterminal epoch")
		}
		changed := !record.ObserverAfterWorkload
		record.ObserverAfterWorkload = true
		return changed, nil
	})
}

// RecoverInterrupted seals only a provably departed supervisor's evidence.
// A reused pid is departed; unreadable process identity is not. Resource cleanup
// remains independently required even if the receipt has now become final.
func (e *Evidence) RecoverInterrupted(ctx context.Context, id string, revision networkview.Count) (Execution, error) {
	return e.files.mutateExecution(ctx, id, revision, func(record *Execution) (bool, error) {
		if record.Receipt != nil {
			return false, nil
		}
		if !processidentity.Inspect(record.Supervisor.PID, record.Supervisor.StartToken).ProvesGone() {
			return false, errors.New("network supervisor is live or its identity is uncertain")
		}
		return sealExecution(record, "supervisor_lost", true)
	})
}

func sealExecution(record *Execution, workload string, interrupted bool) (bool, error) {
	if record.Receipt != nil {
		return false, errors.New("network receipt is immutable")
	}
	snapshot := record.Snapshot.Project(true)
	snapshot.Projection = "owner-local"
	if !record.ObserverAfterWorkload {
		snapshot.Loss.Unknown = true
		snapshot.Loss.Reasons = append(snapshot.Loss.Reasons, "observer_end_after_workload_unproven")
	}
	complete := snapshot.Terminal && !snapshot.Loss.Unknown && snapshot.Loss.Records == 0 && !snapshot.Loss.DetailTruncated && !interrupted
	for _, resource := range record.Resources {
		if resource.Role == "agent" && resource.State != "gone" {
			// A collector's terminal sample is not proof that the workload
			// stopped producing traffic. Runtime custody owns that conclusion.
			complete = false
		}
	}
	for _, coverage := range []networkview.MetricCoverage{snapshot.Coverage.ProxyBytes, snapshot.Coverage.Connections, snapshot.Coverage.UpstreamFailures,
		snapshot.Coverage.KernelPackets, snapshot.Coverage.GuardDenials, snapshot.Coverage.MaintenanceQueries, snapshot.Coverage.MaintenanceBytes,
		snapshot.Coverage.SocketInventory, snapshot.Coverage.BoundaryAttribution} {
		complete = complete && coverage.Status == "exact"
	}
	if interrupted || !snapshot.Terminal {
		snapshot.Loss.Unknown = true
		snapshot.Loss.Reasons = append(snapshot.Loss.Reasons, "terminal_observation_unavailable")
		snapshot.Availability = "unavailable"
		snapshot.Health = networkview.HealthLayers{Enforcer: networkview.Health{Status: "unknown", Reason: "terminal_observation_unavailable"},
			Gateway: networkview.Health{Status: "unknown", Reason: "terminal_observation_unavailable"}, Resolver: networkview.Health{Status: "unknown", Reason: "terminal_observation_unavailable"},
			Collector: networkview.Health{Status: "unavailable", Reason: "terminal_observation_unavailable"}}
		snapshot.Rate = nil
		snapshot.LiveConnections, snapshot.UnknownConnections, snapshot.KernelClosingSockets = nil, nil, nil
		for i := range snapshot.Connections {
			row := &snapshot.Connections[i]
			row.Rate = nil
			if row.State != "closed" && row.State != "failed" {
				row.State, row.Partial = "unknown", true
			}
		}
	}
	cleanup := "complete"
	for _, resource := range record.Resources {
		if resource.State != "gone" {
			cleanup = "pending"
		}
	}
	if record.Artifact.State != "gone" {
		cleanup = "pending"
	}
	completeness := "partial"
	if complete {
		completeness = "complete"
	}
	now := time.Now().UTC()
	receipt := networkview.Receipt{Version: networkview.Version, ID: record.ID, Snapshot: snapshot, StartedAt: record.StartedAt, EndedAt: &now,
		Finality: "final", Completeness: completeness, Workload: workload, Cleanup: cleanup, Runtime: record.Runtime,
		GatewayImage: record.GatewayImage, SessionID: record.SessionID, AttemptID: record.AttemptID,
		CollectorVersion: "gateway-v1", BundleReferences: slices.Clone(record.BundleReferences), DigestScope: "owner-local"}
	if err := receipt.SealDigest(); err != nil {
		return false, err
	}
	record.Receipt = &receipt
	return true, nil
}
