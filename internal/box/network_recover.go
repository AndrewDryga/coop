package box

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/processidentity"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// NetworkRecovery is what one pass could settle for one interrupted run: the
// exact resources it proved gone, what remains pending and why, and whether the
// receipt became final. A run nobody could touch reports Skipped instead.
type NetworkRecovery struct {
	RunID    string
	Removed  []string
	Pending  []string
	Sealed   bool
	Skipped  string
	Failures []error
	// Live is set with Skipped when the run's supervisor is still running or
	// its identity could not be proved: nothing was touched because cleanup is
	// that process's job, not because anything external is in the way.
	Live bool
	// The counts a report may state as fact, by the kind the record itself
	// names. Removed counts only what this pass actually removed — a resource
	// observed already absent was settled, not cleaned up — so a report can
	// never claim a container it never touched.
	RemovedContainers, RemovedVolumes int
	PendingContainers, PendingVolumes int
	// Unverified is set when something was left in place because a consumer
	// could not be proved gone: ownership coop will not guess at, which is a
	// different thing from a removal that failed.
	Unverified bool
}

// recoverDocker is the bounded exact-owner surface recovery needs. It removes
// by recorded identity and ownership labels only: never a name prefix, never a
// prune, and nothing a live supervisor still owns.
type recoverDocker interface {
	Close() error
	InspectContainer(context.Context, runtime.DockerRef) (runtime.DockerContainer, bool, error)
	StopContainer(context.Context, runtime.DockerRef, int) error
	RemoveContainer(context.Context, runtime.DockerRef) error
	InspectVolume(context.Context, runtime.DockerRef) (runtime.DockerVolume, bool, error)
	RemoveVolume(context.Context, runtime.DockerRef) error
}

// recoverConnect binds the daemon one interrupted run recorded. The returned
// id is that daemon's identity, so a recovery can refuse to touch resources
// belonging to a different daemon that happens to answer the same endpoint.
type recoverConnect func(ctx context.Context, endpoint string) (recoverDocker, string, error)

// RecoverNetworkRuns settles the filtered runs whose supervising process died
// before it could clean up: it removes exactly the resources that run recorded,
// then seals an honest `supervisor_lost` receipt. It is the only path that
// finishes an interrupted run's custody, and it never touches a live one.
//
// runID selects one run; empty means every cleanup-pending run this host kept.
func RecoverNetworkRuns(ctx context.Context, rt runtime.Runtime, runID string) ([]NetworkRecovery, error) {
	root, err := NetworkStatePath()
	if err != nil {
		return nil, err
	}
	evidence, err := networkstate.OpenEvidence(root, nil)
	if err != nil {
		return nil, err
	}
	defer evidence.Close()
	connect := func(ctx context.Context, endpoint string) (recoverDocker, string, error) {
		docker, err := runtime.BindDocker(ctx, rt, endpoint, "")
		if err != nil {
			return nil, "", err
		}
		return docker, docker.Info().ID, nil
	}
	return recoverNetworkRuns(ctx, evidence, runID, connect)
}

func recoverNetworkRuns(ctx context.Context, evidence *networkstate.Evidence, runID string, connect recoverConnect) ([]NetworkRecovery, error) {
	records, err := pendingNetworkRuns(evidence, runID)
	if err != nil {
		return nil, err
	}
	out := make([]NetworkRecovery, 0, len(records))
	for _, record := range records {
		out = append(out, recoverNetworkRun(ctx, evidence, record, connect))
	}
	return out, nil
}

// pendingNetworkRuns lists the runs recovery may settle: an explicit id, or
// every retained run whose cleanup is still pending. A page it could not read
// whole is an error — recovery must not report "nothing pending" from a partial
// inventory.
func pendingNetworkRuns(evidence *networkstate.Evidence, runID string) ([]networkstate.Execution, error) {
	if runID != "" {
		record, err := evidence.Execution(runID)
		if err != nil {
			return nil, err
		}
		return []networkstate.Execution{record}, nil
	}
	var out []networkstate.Execution
	cursor := ""
	for {
		page, err := evidence.Executions(cursor)
		if err != nil {
			return nil, err
		}
		if page.Incomplete {
			return nil, errors.New("the list of recorded runs could not be read whole — recover one run by id instead")
		}
		for _, summary := range page.Executions {
			if !summary.CleanupPending {
				continue
			}
			record, err := evidence.Execution(summary.ID)
			if err != nil {
				return nil, err
			}
			out = append(out, record)
		}
		if page.Next == "" {
			return out, nil
		}
		cursor = page.Next
	}
}

// networkRecoveryOrder is the same order a live cleanup uses: consumers before
// the volumes they mount, so a volume is only ever removed once nothing holds
// it.
var networkRecoveryOrder = []string{"agent", "guard", "controller", "ipc", "observations"}

func recoverNetworkRun(ctx context.Context, evidence *networkstate.Evidence, record networkstate.Execution, connect recoverConnect) (out NetworkRecovery) {
	out = NetworkRecovery{RunID: record.ID}
	switch processidentity.Inspect(record.Supervisor.PID, record.Supervisor.StartToken) {
	case processidentity.Gone, processidentity.Mismatch:
	default:
		out.Live = true
		out.Skipped = "its supervisor (pid " + strconv.Itoa(record.Supervisor.PID) + ") is still running or its identity is uncertain"
		return out
	}
	docker, daemon, err := connect(ctx, record.Endpoint)
	if err != nil {
		out.Skipped = "the runtime it ran on is unavailable: " + err.Error()
		return out
	}
	defer func() { out.Failures = appendError(out.Failures, docker.Close()) }()
	if daemon != record.DaemonID {
		out.Skipped = "the runtime at " + record.Endpoint + " is a different daemon than the one this run used"
		return out
	}
	for _, role := range networkRecoveryOrder {
		resource := recoveredResource(record, role)
		if resource.Role == "" || resource.State == "gone" {
			continue
		}
		if !consumersGoneFor(record, role) {
			out.Pending = append(out.Pending, role+" (a container that could still use it is not proved gone)")
			out.Unverified = true
			out.countPending(resource.Kind)
			continue
		}
		next, outcome, err := recoverResource(ctx, evidence, docker, record, resource)
		record = next
		switch {
		case err != nil:
			out.Failures = appendError(out.Failures, fmt.Errorf("%s: %w", role, err))
			out.Pending = append(out.Pending, role)
			out.countPending(resource.Kind)
		case outcome == resourceRemoved:
			out.Removed = append(out.Removed, role)
			out.countRemoved(resource.Kind)
		case outcome == resourceAbsent:
			// Settled without a removal: it was already gone before this pass.
		default:
			out.Pending = append(out.Pending, role)
			out.countPending(resource.Kind)
		}
	}
	if record.Artifact.State != "gone" && !slices.ContainsFunc(record.Resources, func(r networkstate.Resource) bool {
		return r.Kind == "container" && r.State != "gone"
	}) {
		next, err := evidence.CleanupArtifacts(ctx, record.ID, record.Revision)
		if err != nil {
			out.Failures = appendError(out.Failures, fmt.Errorf("artifacts: %w", err))
		} else {
			record = next
		}
	}
	// The receipt is sealed LAST, so its cleanup outcome describes what this
	// pass actually settled rather than what it was about to attempt.
	sealed, err := evidence.RecoverInterrupted(ctx, record.ID, record.Revision)
	if err != nil {
		out.Failures = appendError(out.Failures, fmt.Errorf("receipt: %w", err))
		return out
	}
	out.Sealed = sealed.Receipt != nil
	return out
}

func recoveredResource(record networkstate.Execution, role string) networkstate.Resource {
	for _, resource := range record.Resources {
		if resource.Role == role {
			return resource
		}
	}
	return networkstate.Resource{}
}

// consumersGoneFor keeps the live cleanup's rule: a volume is removed only once
// every container that could mount it is proved gone.
func consumersGoneFor(record networkstate.Execution, role string) bool {
	if role != "ipc" && role != "observations" {
		return true
	}
	for _, resource := range record.Resources {
		if resource.Kind == "container" && resource.State != "gone" {
			return false
		}
	}
	return true
}

// The three outcomes one recorded resource can reach in a pass: coop removed
// it, it was already gone, or it is still there.
const (
	resourceRemoved = "removed"
	resourceAbsent  = "absent"
	resourcePending = "pending"
)

// countRemoved and countPending tally by the kind the record names, so a report
// can say "2 temporary containers" only where the evidence says container.
func (r *NetworkRecovery) countRemoved(kind string) {
	if kind == "container" {
		r.RemovedContainers++
		return
	}
	r.RemovedVolumes++
}

func (r *NetworkRecovery) countPending(kind string) {
	if kind == "container" {
		r.PendingContainers++
		return
	}
	r.PendingVolumes++
}

// recoverResource removes ONE recorded resource and records the outcome. A
// positive inspection reconciles an ambiguous creation first, so the removal
// always names an exact id; an exact absence is what proves it gone — and is
// reported as absent, never as a removal this pass performed.
func recoverResource(ctx context.Context, evidence *networkstate.Evidence, docker recoverDocker,
	record networkstate.Execution, resource networkstate.Resource) (networkstate.Execution, string, error) {
	ref := networkResourceRef(record, resource.Role)
	if ref.Name == "" {
		return record, resourcePending, errors.New("this run recorded no exact id for that container or volume, so coop will not remove anything by guess")
	}
	present, id := false, ""
	var err error
	if resource.Kind == "container" {
		var value runtime.DockerContainer
		if value, present, err = docker.InspectContainer(ctx, ref); err == nil && present {
			id = value.ID
		}
	} else {
		var value runtime.DockerVolume
		if value, present, err = docker.InspectVolume(ctx, ref); err == nil && present {
			id = value.Name
		}
	}
	if err != nil {
		return record, resourcePending, err
	}
	if present && resource.ID == "" {
		// The supervisor died between submitting the create and recording its
		// outcome. This exact-owner observation binds the identity it left.
		if record, err = evidence.ReconcileInterruptedResource(ctx, record.ID, record.Revision, record.DaemonID, resource.Role, ref.Name, id); err != nil {
			return record, resourcePending, err
		}
		ref = networkResourceRef(record, resource.Role)
	}
	if present {
		if resource.Kind == "container" {
			if err := docker.StopContainer(ctx, ref, 0); err != nil {
				return record, resourcePending, err
			}
			err = docker.RemoveContainer(ctx, ref)
		} else {
			err = docker.RemoveVolume(ctx, ref)
		}
		if err != nil {
			return record, resourcePending, err
		}
	}
	record, err = evidence.ConfirmResourceGone(ctx, record.ID, record.Revision, record.DaemonID, resource.Role, ref.Name, ref.ID)
	switch {
	case err != nil:
		return record, resourcePending, err
	case present:
		return record, resourceRemoved, nil
	}
	return record, resourceAbsent, nil
}

func appendError(list []error, err error) []error {
	if err == nil {
		return list
	}
	return append(list, err)
}
