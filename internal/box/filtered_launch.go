package box

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/networkgateway"
	"github.com/AndrewDryga/coop/internal/networkview"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// filteredControlTimeout bounds one exact-owned runtime control call (create,
// start, inspect). It is generous on purpose: these calls carry a box's whole
// mount set, and a loaded host makes a slow create look like a broken one.
const filteredControlTimeout = 60 * time.Second

func networkMount(kind, source, target string, readonly bool) string {
	fields := []string{"type=" + kind, "source=" + source, "target=" + target}
	if readonly {
		fields = append(fields, "readonly")
	}
	var buffer bytes.Buffer
	writer := csv.NewWriter(&buffer)
	_ = writer.Write(fields)
	writer.Flush()
	return strings.TrimSuffix(buffer.String(), "\n")
}

func (f *filteredExecution) createVolume(ctx context.Context, role string) error {
	ctx, cancel := context.WithTimeout(ctx, filteredControlTimeout)
	defer cancel()
	if err := f.transition(ctx, role, "creating"); err != nil {
		return err
	}
	f.attempted[role] = true
	volume, err := f.docker.CreateVolume(ctx, f.ref(role))
	if err != nil {
		if errors.Is(err, runtime.ErrDockerCreateNotAttempted) {
			f.attempted[role] = false
		}
		return err // current absence cannot settle an ambiguous daemon request
	}
	// The daemon confirmed creation. Caller cancellation must not discard that
	// custody, but it still stops the launch after the independent bounded write.
	persist, stop := context.WithTimeout(context.WithoutCancel(ctx), filteredControlTimeout)
	defer stop()
	return errors.Join(f.created(persist, role, volume.Name), ctx.Err())
}

func (f *filteredExecution) createContainer(ctx context.Context, role, image string, options, command []string) error {
	ctx, cancel := context.WithTimeout(ctx, filteredControlTimeout)
	defer cancel()
	if err := f.transition(ctx, role, "creating"); err != nil {
		return err
	}
	f.attempted[role] = true
	id, createErr := f.docker.CreateContainer(ctx, runtime.DockerCreate{Ref: f.ref(role), Image: image, Options: options, Command: command})
	if id == "" {
		if errors.Is(createErr, runtime.ErrDockerCreateNotAttempted) {
			f.attempted[role] = false
			return createErr
		}
		return errors.Join(createErr, errors.New("network container creation outcome is unknown"))
	}
	// Even a failed post-create inspection can return a known immutable ID.
	// Record it using an independent bounded context before cleanup starts.
	persist, stop := context.WithTimeout(context.WithoutCancel(ctx), filteredControlTimeout)
	defer stop()
	if err := errors.Join(createErr, f.created(persist, role, id)); err != nil {
		return err
	}
	return f.verifyContainer(ctx, role, image, options)
}

func (f *filteredExecution) verifyContainer(ctx context.Context, role, image string, options []string) error {
	value, present, err := f.docker.InspectContainer(ctx, f.ref(role))
	if err != nil || !present {
		return errors.Join(errors.New("restricted workload security state unavailable"), err)
	}
	user, network := "1000:1000", "container:"+f.ref("controller").ID
	if role == "guard" {
		user = "65532:65532"
	} else if role == "controller" {
		user, network = "0:65532", "bridge"
	}
	capabilities := len(value.CapAdd) == 0
	if role == "controller" {
		capabilities = len(value.CapAdd) == 1 && strings.TrimPrefix(value.CapAdd[0], "CAP_") == "NET_ADMIN"
	}
	nnp := slices.Contains(value.SecurityOpt, "no-new-privileges") || slices.Contains(value.SecurityOpt, "no-new-privileges=true")
	if value.Image != image || value.User != user || value.NetworkMode != network || value.Privileged || value.AutoRemove ||
		value.RestartPolicy != "no" || value.RestartCount != 0 || value.PidMode != "" || value.UsernsMode != "" ||
		value.IpcMode != "" && value.IpcMode != "private" || value.CgroupnsMode != "" && value.CgroupnsMode != "private" ||
		len(value.CapDrop) == 0 || slices.ContainsFunc(value.CapDrop, func(cap string) bool { return cap != "ALL" }) ||
		!capabilities || !nnp || role != "agent" && !value.ReadonlyRootfs ||
		value.State.Status != "created" || !value.State.StartedAt.IsZero() {
		return errors.New("restricted workload security state does not match its launch contract")
	}
	return verifyNetworkMounts(value.Mounts, value.Tmpfs, options)
}

func (f *filteredExecution) startHelper(ctx context.Context, role string) error {
	ctx, cancel := context.WithTimeout(ctx, filteredControlTimeout)
	defer cancel()
	if err := f.transition(ctx, role, "starting"); err != nil {
		return err
	}
	if err := f.docker.StartContainer(ctx, f.ref(role)); err != nil {
		return err
	}
	return f.transition(ctx, role, "started")
}

func (f *filteredExecution) helperOptions(role string) []string {
	options := []string{"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--read-only",
		"--memory", "256m", "--pids-limit", "64", "--log-driver", "local", "--log-opt", "max-size=1m", "--log-opt", "max-file=2",
		"--mount", networkMount("bind", f.config, networkgateway.LaunchConfigPath, true)}
	if role == "controller" {
		// The controller owns the namespace the agent runs in, so a published
		// serve port has to be published HERE. The agent still has no runtime
		// authority of its own; it only inherits the namespace.
		options = append(options, "--user", "0:65532", "--cap-add", "NET_ADMIN", "--network", "bridge",
			"--mount", networkMount("volume", f.ref("ipc").Name, "/ipc", false))
		return append(options, f.publish...)
	}
	return append(options, "--user", "65532:65532", "--network", "container:"+f.ref("controller").ID,
		"--mount", networkMount("volume", f.ref("ipc").Name, "/ipc", true),
		"--mount", networkMount("volume", f.ref("observations").Name, networkgateway.ObservationDirectory, false),
		"--tmpfs", "/private:rw,nosuid,nodev,noexec,uid=65532,gid=65532,mode=0700,size=16m")
}

func (f *filteredExecution) observe(ctx context.Context) (networkview.Snapshot, error) {
	data, err := f.docker.ExecRead(ctx, f.ref("guard"), networkgateway.MaxSnapshotBytes, "/usr/local/bin/coop-net", "snapshot")
	if err != nil {
		return networkview.Snapshot{}, err
	}
	return f.readObservation(data)
}

func (f *filteredExecution) readObservation(data []byte) (networkview.Snapshot, error) {
	if len(data) > networkgateway.MaxSnapshotBytes || bytes.Count(data, []byte{'\n'}) != 1 || !bytes.HasSuffix(data, []byte{'\n'}) {
		return networkview.Snapshot{}, errors.New("network observation framing is invalid")
	}
	identity := f.identity
	if !identity.Valid() {
		// The first clock identity comes from our exact trusted guard, not the
		// host kernel (Docker Desktop has a separate boot clock).
		var first networkgateway.RuntimeObservation
		f.mu.Lock()
		runID, epoch := f.record.ID, f.record.Epoch
		f.mu.Unlock()
		if json.Unmarshal(data, &first) != nil || !first.Identity.Valid() || first.Identity.RunID != runID ||
			first.Identity.Epoch != epoch || first.Identity.PolicyFingerprint != f.policy.Fingerprint {
			return networkview.Snapshot{}, errors.New("network observation identity is invalid")
		}
		identity = first.Identity
	}
	value, err := networkgateway.ReadObservation(bytes.NewReader(data), identity)
	if err != nil || value.Network.Sequence == 0 || value.Network.Mode != f.policy.Mode {
		return networkview.Snapshot{}, errors.New("network observation identity is invalid")
	}
	f.identity = identity
	return value.Network, nil
}

func (f *filteredExecution) ready(ctx context.Context) error {
	if _, err := f.docker.ExecRead(ctx, f.ref("guard"), 4096, "/usr/local/bin/coop-net", "probe"); err != nil {
		return err
	}
	snapshot, err := f.observe(ctx)
	if err != nil {
		return err
	}
	if snapshot.Terminal || snapshot.Availability != "available" || snapshot.Health.Enforcer.Status != "ready" ||
		snapshot.Health.Gateway.Status != "ready" || snapshot.Health.Collector.Status != "ready" || snapshot.Health.Resolver.Status != "ready" {
		return errors.New("network enforcement or observation is not ready")
	}
	return f.accept(ctx, snapshot)
}

func (f *filteredExecution) waitReady(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, filteredControlTimeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		probe, stop := context.WithTimeout(ctx, 3*time.Second)
		err := f.ready(probe)
		stop()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.Join(errors.New("network gateway did not become ready; no agent started"), ctx.Err())
		case <-ticker.C:
		}
	}
}

// launch never returns with a running observation goroutine. Its caller always
// performs exact daemon cleanup, including when attachment or setup was canceled.
func (f *filteredExecution) launch(ctx context.Context, spec RunSpec, options []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	for _, role := range []string{"ipc", "observations"} {
		if err := f.createVolume(ctx, role); err != nil {
			return -1, err
		}
	}
	for _, role := range []string{"controller", "guard"} {
		if err := f.createContainer(ctx, role, f.record.GatewayImage, f.helperOptions(role), []string{role}); err != nil {
			return -1, err
		}
		// The approved sidecar's network is attached before the controller runs,
		// so the address the rules were rendered for exists from the first
		// packet. Joining it grants nothing on its own: every other member of
		// that network is still refused by the same default deny.
		if role == "controller" && f.servicesNet != "" {
			attach, stop := context.WithTimeout(ctx, filteredControlTimeout)
			err := f.docker.ConnectNetwork(attach, f.servicesNet, f.ref("controller"))
			stop()
			if err != nil {
				return -1, err
			}
		}
		if err := f.startHelper(ctx, role); err != nil {
			return -1, err
		}
	}
	if err := f.waitReady(ctx); err != nil {
		return -1, err
	}
	if err := f.checkTopology(); err != nil {
		return -1, err
	}
	if err := f.checkBindings(); err != nil {
		return -1, err
	}
	options = append(slices.Clone(options), "--user", "1000:1000", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--network", "container:"+f.ref("controller").ID)
	options = append(options, f.serveEnv...)
	if err := f.createContainer(ctx, "agent", f.image, options, spec.Cmd); err != nil {
		return -1, err
	}
	if err := f.waitReady(ctx); err != nil {
		return -1, err
	}
	if err := f.checkTopology(); err != nil {
		return -1, err
	}
	if err := f.checkBindings(); err != nil {
		return -1, err
	}
	if err := f.transition(ctx, "agent", "starting"); err != nil {
		return -1, err
	}
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	watchDone := make(chan error, 1)
	go func() {
		watchDone <- f.watch(run, cancel)
	}()
	if spec.OnRuntimeLaunch != nil {
		spec.OnRuntimeLaunch()
	}
	f.startAttempted = true
	code, runErr := f.docker.StartAttached(run, f.ref("agent"), stdin, stdout, stderr, func() error {
		f.mu.Lock()
		f.mainStarted = true
		f.mu.Unlock()
		return f.transition(run, "agent", "started")
	})
	cancel()
	watchErr := <-watchDone
	outcome := errors.Join(runErr, watchErr)
	// An interrupt is not a fault to describe twice: the workload and the watcher both stop on the
	// same cancellation, and "context canceled context canceled" tells a person nothing. Cleanup
	// still runs after this returns, so the box and its gateway are gone either way.
	if ctx.Err() != nil && errors.Is(outcome, context.Canceled) {
		return code, interruptedRun{}
	}
	return code, outcome
}

func (f *filteredExecution) watch(ctx context.Context, cancel context.CancelFunc) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	probeFailures := 0
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		if err := f.checkTopology(); err != nil {
			cancel()
			return err // topology drift is a changed protection envelope, not a slow probe
		}
		probe, stop := context.WithTimeout(ctx, 3*time.Second)
		_, err := f.docker.ExecRead(probe, f.ref("guard"), 4096, "/usr/local/bin/coop-net", "probe")
		stop()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			probeFailures++
			if probeFailures < 3 {
				continue // nft remains fail-closed while bounded observation retries run
			}
			cancel()
			return fmt.Errorf("restricted network health lost: %w", err)
		}
		probeFailures = 0
		// Observation loss is not permission to open networking, nor an
		// implicit stop-on-audit-loss policy. Retain the last producer time so
		// readers can report stale/unavailable evidence without inventing zeros.
		probe, stop = context.WithTimeout(ctx, 3*time.Second)
		if snapshot, observeErr := f.observe(probe); observeErr == nil {
			_ = f.accept(probe, snapshot)
		}
		stop()
	}
}

func (f *filteredExecution) checkTopology() error {
	inventory := f.hostAddresses
	if inventory == nil {
		inventory = filteredHostAddresses
	}
	protected, err := inventory()
	if err != nil {
		return err
	}
	// Interface GC can remove an address without weakening the original deny
	// set. Keep that set installed; only newly unprotected addresses require a
	// replacement execution. Never advance the baseline to a smaller inventory.
	if len(protected) == 0 {
		return errors.New("protected host topology is empty; start a new network execution")
	}
	for _, prefix := range protected {
		if !slices.Contains(f.protected, prefix) {
			// Name the address: "topology changed" alone leaves an operator with
			// nothing to look at, and a VPN or a new interface is a fact they can.
			return fmt.Errorf("host address %s appeared after this run's protected addresses were set; start the run again", prefix)
		}
	}
	return nil
}

// interruptedRun reads as one word to a person and is still a cancellation to code: the loop and
// the session runner both decide what an interrupted attempt means by asking errors.Is.
type interruptedRun struct{}

func (interruptedRun) Error() string { return "interrupted" }

func (interruptedRun) Unwrap() error { return context.Canceled }
