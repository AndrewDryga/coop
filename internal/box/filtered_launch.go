package box

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/networkgateway"
	"github.com/AndrewDryga/coop/internal/networkstate"
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
	if role == "guard" {
		if err := f.checkCredentialBrokerBinding(); err != nil {
			return err
		}
	}
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
	options = append(options, "--user", "65532:65532", "--network", "container:"+f.ref("controller").ID,
		"--mount", networkMount("volume", f.ref("ipc").Name, "/ipc", true),
		"--mount", networkMount("volume", f.ref("observations").Name, networkgateway.ObservationDirectory, false),
		"--tmpfs", "/private:rw,nosuid,nodev,noexec,uid=65532,gid=65532,mode=0700,size=16m")
	if f.broker != nil {
		options = append(options, "--mount", networkMount("bind", f.broker.configPath, networkgateway.CredentialBrokerPath, true))
	}
	return options
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
		// The internal service network is attached before the controller runs.
		// Its fixed alias is the only route prepared services get to approved TLS.
		if role == "controller" && f.servicesNet != "" {
			attach, stop := context.WithTimeout(ctx, filteredControlTimeout)
			err := f.docker.ConnectNetwork(attach, f.servicesNet, f.ref("controller"), filteredServiceProxyAlias)
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
	projectRepo := spec.ActivityRepo
	if projectRepo == "" {
		projectRepo = spec.Repo
	}
	if f.preparedServices != nil {
		unlock, err := forkspace.LockServiceLaunch(ctx, projectRepo, true)
		if err != nil {
			return -1, fmt.Errorf("wait for a safe service launch: %w", err)
		}
		err = f.preparedServices.start()
		unlock()
		if err != nil {
			return -1, err
		}
	}
	if err := f.preparedServices.check(ctx, f.docker, f.servicesNet, ComposeProjectFor(spec.Repo, runServiceOwner(spec))); err != nil {
		return -1, err
	}
	if err := f.reconcileTopology(ctx); err != nil {
		return -1, err
	}
	if err := f.checkBindings(); err != nil {
		return -1, err
	}
	unlockMounts, err := forkspace.LockServiceLaunch(ctx, projectRepo, false)
	if err != nil {
		return -1, fmt.Errorf("enter the sandbox mount window: %w", err)
	}
	defer unlockMounts()
	options = append(slices.Clone(options), "--user", "1000:1000", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--network", "container:"+f.ref("controller").ID)
	options = append(options, f.serveEnv...)
	if err := f.createContainer(ctx, "agent", f.image, options, spec.Cmd); err != nil {
		return -1, err
	}
	if err := f.waitReady(ctx); err != nil {
		return -1, err
	}
	if err := f.reconcileTopology(ctx); err != nil {
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
		if err := f.reconcileTopology(ctx); err != nil {
			cancel()
			return err // an envelope that cannot follow the topology is not a slow probe
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

// filteredTopologyTimeout bounds one reconciliation: the daemon's network
// inventory plus one kernel set update in the controller.
const filteredTopologyTimeout = 3 * time.Second

// reconcileTopology keeps the run's protection envelope equal to the host's live
// topology. The envelope is inventoried at launch — this host's addresses plus
// every subnet and gateway the runtime holds — and rendered into the
// controller's protected4 set, which the kernel refuses before any grant. But
// topology moves while a run lives: every other filtered run creates its own
// Docker network (a subnet, a gateway, on OrbStack a host address too), a VPN
// adds an interface. Ending the run on that, as this once did, made two filtered
// runs kill each other in a loop — each respawn created the network that tripped
// the other's watch. Instead the grown inventory is re-rendered into the live
// kernel set — the one host mutation a run makes after launch — then kept in
// f.protected and the execution record, so the addresses are refused from that
// moment and a later `why` can say so. Growth is monotone: an address that
// disappears keeps its denial. What cannot be reconciled still ends the run: an
// unreadable or empty host inventory, an envelope past the qualified cap, a
// refused kernel update. The message names the address either way, because
// "topology changed" alone leaves an operator with nothing to look at.
func (f *filteredExecution) reconcileTopology(ctx context.Context) error {
	inventory := f.hostAddresses
	if inventory == nil {
		inventory = filteredHostAddresses
	}
	host, err := inventory()
	if err != nil {
		return err
	}
	// Interface GC can remove an address without weakening the original deny
	// set. Keep that set installed; only new addresses need work. Never advance
	// the baseline to a smaller inventory.
	if len(host) == 0 {
		return errors.New("protected host topology is empty; start a new network execution")
	}
	ctx, cancel := context.WithTimeout(ctx, filteredTopologyTimeout)
	defer cancel()
	// A daemon that cannot answer right now is not topology drift: reconcile the
	// host inventory alone — all this watch ever covered — and let the next tick
	// see the runtime's networks.
	networks, _ := f.docker.Networks(ctx)
	protected, _, err := filteredProtectedAddresses(networks, func() ([]netip.Prefix, error) { return host, nil })
	if err != nil {
		return err
	}
	var added []netip.Prefix
	kernel := false
	for _, prefix := range protected {
		if coveredBy(f.protected, prefix) {
			continue
		}
		if !prefix.IsValid() || prefix != prefix.Masked() || prefix.Addr().Is4In6() {
			return fmt.Errorf("host address %s appeared after this run's protected addresses were set and cannot be protected; start the run again", prefix)
		}
		added = append(added, prefix)
		// The gateway refuses every IPv6 packet outright, so only IPv4 changes the
		// kernel set; a new IPv6 address is recorded and nothing more.
		kernel = kernel || prefix.Addr().Is4()
	}
	if len(added) == 0 {
		return nil
	}
	if len(f.protected)+len(added) > networkgateway.MaxProtectedRanges {
		return fmt.Errorf("host address %s appeared after this run's protected addresses were set, and the envelope cannot grow past %d ranges; start the run again", added[0], networkgateway.MaxProtectedRanges)
	}
	grown := append(slices.Clone(f.protected), added...)
	slices.SortFunc(grown, func(a, b netip.Prefix) int { return strings.Compare(a.String(), b.String()) })
	if kernel {
		if err := f.docker.ExecApply(ctx, f.ref("controller"), "/usr/sbin/nft", protectedSetUpdate(grown)); err != nil {
			return fmt.Errorf("host address %s appeared after this run's protected addresses were set and could not be protected in place (%v); start the run again", added[0], err)
		}
	}
	f.protected = grown
	// Evidence, not enforcement: the kernel already refuses the addresses. Like
	// the periodic snapshot, a failed write leaves the live work alone.
	_ = f.update(ctx, func(r networkstate.Execution) (networkstate.Execution, error) {
		return f.store.RecordProtected(ctx, r.ID, r.Revision, grown)
	})
	return nil
}

// protectedSetUpdate is the one-transaction nft script that re-renders the
// controller's protected4 set to a grown inventory. Flush and add commit
// together, so the set never has a gap and a refused element leaves it unchanged
// (verified on the pinned nftables 1.0.2). The rendering mirrors initialRules in
// internal/networkgateway/controller.go — the always-refused ranges plus this
// run's inventory, IPv4 only, sorted and deduplicated — and lives HERE because
// that package is embedded in the gateway image: sharing the code would change
// the image every host has qualified, for no change in what runs inside it. nft
// parses its argument as a script, so only canonical prefixes may reach it.
func protectedSetUpdate(protected []netip.Prefix) string {
	var elements []string
	for _, prefix := range egress.ProtectedRanges(protected) {
		if prefix.Addr().Is4() {
			elements = append(elements, prefix.String())
		}
	}
	slices.Sort(elements)
	elements = slices.Compact(elements)
	return "flush set inet coop_net protected4; add element inet coop_net protected4 { " + strings.Join(elements, ", ") + " }"
}

// coveredBy reports whether prefix lies inside one the run already protects; a
// CIDR either nests in another or is disjoint from it, so containment is the
// whole question.
func coveredBy(protected []netip.Prefix, prefix netip.Prefix) bool {
	for _, installed := range protected {
		if installed.Bits() <= prefix.Bits() && installed.Contains(prefix.Addr()) {
			return true
		}
	}
	return false
}

// interruptedRun reads as one word to a person and is still a cancellation to code: the loop and
// the session runner both decide what an interrupted attempt means by asking errors.Is.
type interruptedRun struct{}

func (interruptedRun) Error() string { return "interrupted" }

func (interruptedRun) Unwrap() error { return context.Canceled }
