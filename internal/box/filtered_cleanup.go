package box

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/AndrewDryga/coop/internal/networkgateway"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/runtime"
)

func (f *filteredExecution) resource(role string) networkstate.Resource {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, resource := range f.record.Resources {
		if resource.Role == role {
			return resource
		}
	}
	return networkstate.Resource{}
}

// Cleanup starts only after host generators and the observation goroutine have
// stopped. Each operation has its own deadline so one wedged resource cannot
// prevent containment attempts on the others.
func (f *filteredExecution) cleanup(workload string) (agentGone bool, result error) {
	if f.preparedServices != nil && f.preparedServices.cleanup != nil {
		defer f.preparedServices.cleanup()
	}
	var evidence *networkstate.Evidence
	// contained is what THIS containment pass proved absent at the runtime, for
	// the exits where host storage can no longer record it.
	contained := map[string]bool{}
	defer func() {
		if !agentGone {
			// Lost host storage must not prevent containment of a surviving
			// daemon workload. These stops cannot claim completed custody.
			for _, role := range []string{"agent", "guard", "controller"} {
				ctx, cancel := context.WithTimeout(context.Background(), filteredControlTimeout)
				ref := f.ref(role)
				value, present, err := f.docker.InspectContainer(ctx, ref)
				if err == nil && present {
					ref.ID = value.ID
					err = f.docker.RemoveContainer(ctx, ref)
					if err == nil {
						f.confirmGone(evidence, role, ref)
					}
				}
				contained[role] = err == nil
				result = errors.Join(result, err)
				cancel()
			}
		}
		// The two named volumes are exact-owned custody too. An exit that returned
		// before the ordered walk — lost evidence, an unreadable registry — must
		// still attempt them, or every interrupted run leaks a pair.
		result = errors.Join(result, f.containVolumes(evidence, contained))
		if evidence != nil {
			result = errors.Join(result, evidence.Close())
		}
		result = errors.Join(result, f.docker.Close())
	}()
	if f.record.ID == "" {
		return true, nil
	}
	evidence, err := networkstate.OpenEvidence(f.store.Path(), nil)
	if err != nil {
		return false, err
	}
	// Refresh even after a failed initial publication. A failed read never
	// authorizes abandoning resources whose runtime outcome might be unknown.
	if current, err := f.store.Execution(f.record.ID); err != nil {
		return false, err
	} else {
		f.record = current
	}
	result = errors.Join(result, f.removeResource(evidence, "agent"))
	agentGone = f.resource("agent").State == "gone"
	retainEvidence := false
	guard, resolveErr := f.resolveResource("guard")
	result = errors.Join(result, resolveErr)
	// A guard that was only created never ran: there is nothing to stop and no final observation to
	// take — asking the daemon for one reports a missing file, a failure that never happened.
	if resolveErr == nil && guard.ID != "" && f.resource("guard").State != "created" {
		if agentGone {
			ctx, cancel := context.WithTimeout(context.Background(), filteredControlTimeout)
			// This round-trip happens after exact agent absence. A guard that
			// already terminated cannot produce the causal readiness reply.
			if _, err := f.docker.ExecRead(ctx, guard, 4096, "/usr/local/bin/coop-net", "probe"); err == nil {
				result = errors.Join(result, f.update(ctx, func(r networkstate.Execution) (networkstate.Execution, error) {
					return f.store.RecordObserverAfterWorkload(ctx, r.ID, r.Revision)
				}))
			}
			cancel()
		}
		ctx, cancel := context.WithTimeout(context.Background(), filteredControlTimeout)
		stopErr := f.docker.StopContainer(ctx, guard, 10)
		cancel()
		result = errors.Join(result, stopErr)
		if stopErr == nil && agentGone {
			// Terminal collector evidence is not workload-wide coverage when an
			// agent might still survive. Never accept that misleading final frame.
			ctx, cancel := context.WithTimeout(context.Background(), filteredControlTimeout)
			data, captureErr := f.docker.CopyArchive(ctx, guard, networkgateway.FinalObservationPath, networkgateway.MaxSnapshotBytes+64<<10)
			if captureErr == nil {
				var observation []byte
				observation, captureErr = finalObservationFile(data)
				if captureErr == nil {
					snapshot, decodeErr := f.readObservation(observation)
					if decodeErr != nil || !snapshot.Terminal {
						captureErr = errors.New("terminal network observation is invalid")
					} else if persistErr := f.accept(ctx, snapshot); persistErr != nil {
						// Do not destroy the last source after ambiguous persistence.
						retainEvidence, captureErr = true, persistErr
					}
				}
			}
			cancel()
			// Missing/corrupt final evidence becomes a partial receipt. It must
			// not prevent stopping the workload or exact-owned resource cleanup.
			result = errors.Join(result, captureErr)
		}
	}
	// Removing the guard and stopping the controller are independent — the guard has stopped and its
	// final observation is taken — so they run together; only the controller's removal needs the
	// guard gone. Each runtime call is a CLI process of about a tenth of a second.
	var guardErr, controllerErr error
	var steps sync.WaitGroup
	if !retainEvidence {
		steps.Go(func() { guardErr = f.removeResource(evidence, "guard") })
	}
	steps.Go(func() {
		controller, err := f.resolveResource("controller")
		if err != nil || controller.ID == "" {
			controllerErr = err
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), filteredControlTimeout)
		controllerErr = f.docker.StopContainer(ctx, controller, 0)
		cancel()
	})
	steps.Wait()
	result = errors.Join(result, guardErr, controllerErr)
	if !retainEvidence && f.resource("guard").State == "gone" && agentGone {
		result = errors.Join(result, f.removeResource(evidence, "controller"))
	}
	consumersGone := agentGone && f.resource("guard").State == "gone" && f.resource("controller").State == "gone"
	if consumersGone {
		// With every consumer gone the two volumes are independent too.
		var ipcErr, observationsErr error
		var volumes sync.WaitGroup
		volumes.Go(func() { ipcErr = f.removeResource(evidence, "ipc") })
		volumes.Go(func() { observationsErr = f.removeResource(evidence, "observations") })
		volumes.Wait()
		result = errors.Join(result, ipcErr, observationsErr)
		ctx, cancel := context.WithTimeout(context.Background(), filteredControlTimeout)
		result = errors.Join(result, f.update(ctx, func(r networkstate.Execution) (networkstate.Execution, error) {
			return evidence.CleanupArtifacts(ctx, r.ID, r.Revision)
		}))
		cancel()
	}
	if !retainEvidence {
		ctx, cancel := context.WithTimeout(context.Background(), filteredControlTimeout)
		result = errors.Join(result, f.update(ctx, func(r networkstate.Execution) (networkstate.Execution, error) {
			return f.store.SealExecution(ctx, r.ID, r.Revision, workload)
		}))
		cancel()
	}
	return agentGone, result
}

// containVolumes removes this run's named volumes once nothing can still be
// using them. A container whose absence is unproven keeps them: a volume under a
// live consumer is not cleanup, and the receipt says pending rather than lying.
func (f *filteredExecution) containVolumes(evidence *networkstate.Evidence, contained map[string]bool) error {
	if f.record.ID == "" {
		return nil
	}
	for _, role := range []string{"agent", "guard", "controller"} {
		if f.resource(role).State != "gone" && !contained[role] {
			return nil
		}
	}
	var result error
	for _, role := range []string{"ipc", "observations"} {
		if f.resource(role).State == "gone" {
			continue
		}
		if evidence != nil {
			result = errors.Join(result, f.removeResource(evidence, role))
			continue
		}
		// Without an evidence handle the runtime object is still contained; the
		// record cannot be updated, so the receipt stays honestly pending.
		ctx, cancel := context.WithTimeout(context.Background(), filteredControlTimeout)
		ref := f.ref(role)
		if _, present, err := f.docker.InspectVolume(ctx, ref); err != nil {
			result = errors.Join(result, err)
		} else if present {
			result = errors.Join(result, f.docker.RemoveVolume(ctx, ref))
		}
		cancel()
	}
	return result
}

// confirmGone records a containment removal when host storage still answers. It
// is best effort by design: the runtime object is already gone, and a failed
// note must not turn successful containment into an error nobody can act on.
func (f *filteredExecution) confirmGone(evidence *networkstate.Evidence, role string, ref runtime.DockerRef) {
	if evidence == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), filteredControlTimeout)
	defer cancel()
	_ = f.update(ctx, func(r networkstate.Execution) (networkstate.Execution, error) {
		return evidence.ConfirmResourceGone(ctx, r.ID, r.Revision, r.DaemonID, role, ref.Name, ref.ID)
	})
}

func finalObservationFile(data []byte) ([]byte, error) {
	reader := tar.NewReader(bytes.NewReader(data))
	header, err := reader.Next()
	if err != nil || header.Name != "final.json" || header.Typeflag != tar.TypeReg || header.Size < 1 || header.Size > networkgateway.MaxSnapshotBytes || header.Linkname != "" {
		return nil, errors.New("terminal network archive is invalid")
	}
	value, err := io.ReadAll(io.LimitReader(reader, networkgateway.MaxSnapshotBytes+1))
	if err != nil || int64(len(value)) != header.Size {
		return nil, errors.New("terminal network archive is incomplete")
	}
	if _, err := reader.Next(); err != io.EOF {
		return nil, errors.New("terminal network archive contains unexpected entries")
	}
	return value, nil
}

// A positive exact-owner inspection may reconcile an ambiguous creation. A
// negative observation cannot prove a timed-out daemon request will not appear.
func (f *filteredExecution) resolveResource(role string) (runtime.DockerRef, error) {
	ref, resource := f.ref(role), f.resource(role)
	if resource.State != "creating" || ref.ID != "" {
		return ref, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), filteredControlTimeout)
	defer cancel()
	id := ""
	if resource.Kind == "container" {
		value, present, err := f.docker.InspectContainer(ctx, ref)
		if err != nil {
			return ref, err
		}
		if present {
			id = value.ID
		}
	} else {
		value, present, err := f.docker.InspectVolume(ctx, ref)
		if err != nil {
			return ref, err
		}
		if present {
			id = value.Name
		}
	}
	if id == "" {
		if f.attempted[role] {
			return ref, fmt.Errorf("network %s creation outcome remains unknown", role)
		}
		return ref, nil
	}
	if err := f.created(ctx, role, id); err != nil {
		return ref, err
	}
	return f.ref(role), nil
}

func (f *filteredExecution) removeResource(evidence *networkstate.Evidence, role string) error {
	ref, err := f.resolveResource(role)
	if err != nil {
		return err
	}
	resource := f.resource(role)
	if resource.State == "gone" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), filteredControlTimeout)
	defer cancel()
	if resource.Kind == "container" {
		if ref.ID != "" {
			// Force removal remains available after a failed graceful stop.
			stopCtx, stop := context.WithTimeout(context.Background(), filteredControlTimeout)
			grace := 0
			if role == "agent" {
				grace = 10 // give provider transcripts the same bounded drain as the guard
			}
			_ = f.docker.StopContainer(stopCtx, ref, grace)
			stop()
			cancel()
			ctx, cancel = context.WithTimeout(context.Background(), filteredControlTimeout)
			defer cancel()
			if err := f.docker.RemoveContainer(ctx, ref); err != nil {
				return err
			}
		} else if _, present, err := f.docker.InspectContainer(ctx, ref); err != nil || present {
			return errors.Join(errors.New("network container absence is unconfirmed"), err)
		}
	} else {
		if ref.ID != "" {
			if err := f.docker.RemoveVolume(ctx, ref); err != nil {
				return err
			}
		} else if _, present, err := f.docker.InspectVolume(ctx, ref); err != nil || present {
			return errors.Join(errors.New("network volume absence is unconfirmed"), err)
		}
	}
	return f.update(ctx, func(r networkstate.Execution) (networkstate.Execution, error) {
		return evidence.ConfirmResourceGone(ctx, r.ID, r.Revision, r.DaemonID, role, ref.Name, ref.ID)
	})
}
