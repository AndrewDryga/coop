package box

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

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
				}
				result = errors.Join(result, err)
				cancel()
			}
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
	defer evidence.Close()
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
	if resolveErr == nil && guard.ID != "" {
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
	if !retainEvidence {
		result = errors.Join(result, f.removeResource(evidence, "guard"))
	}
	if controller, err := f.resolveResource("controller"); err == nil && controller.ID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), filteredControlTimeout)
		result = errors.Join(result, f.docker.StopContainer(ctx, controller, 0))
		cancel()
	} else {
		result = errors.Join(result, err)
	}
	if !retainEvidence && f.resource("guard").State == "gone" && agentGone {
		result = errors.Join(result, f.removeResource(evidence, "controller"))
	}
	consumersGone := agentGone && f.resource("guard").State == "gone" && f.resource("controller").State == "gone"
	if consumersGone {
		for _, role := range []string{"ipc", "observations"} {
			result = errors.Join(result, f.removeResource(evidence, role))
		}
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
