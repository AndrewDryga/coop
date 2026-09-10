package box

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/networkgateway"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/networkview"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/ui"
)

// CapturedEgress is trusted host-selected authority, never a remote request DTO.
// The loop owner retains one frozen capture across provider rotation.
type CapturedEgress struct {
	Store                                 *networkstate.Store
	Project, Fingerprint, QualificationID string
	SessionID, AttemptID                  string
}

// Close releases the authority handle a capture holds. A nil capture closes
// nothing, so a caller can defer it unconditionally after admission.
func (c *CapturedEgress) Close() error {
	if c == nil || c.Store == nil {
		return nil
	}
	return c.Store.Close()
}

// capturedSessionWorkspace reports whether this run's checkout is exactly the
// fork workspace the captured project would give this run's fork name. Only a
// remote session's capture carries a session identity, and only the fork the
// daemon named resolves to that path, so a capture from another project — or a
// launch that simply claims a fork name — cannot satisfy it.
func capturedSessionWorkspace(capture *CapturedEgress, spec RunSpec, runRepo string) bool {
	if capture.SessionID == "" || spec.ForkName == "" {
		return false
	}
	workspace, err := filepath.EvalSymlinks(forkspace.Workspace(capture.Project, spec.ForkName))
	return err == nil && workspace == runRepo
}

type filteredExecution struct {
	store          *networkstate.Store
	docker         filteredDocker
	policy         egress.Snapshot
	image          string
	config         string
	runfiles       string
	identity       networkgateway.Identity
	protected      []netip.Prefix
	hostAddresses  func() ([]netip.Prefix, error) // nil uses live host inventory; fixtures own their topology
	mu             sync.Mutex                     // host callbacks serialize registry CAS, never runtime I/O
	record         networkstate.Execution
	attempted      map[string]bool
	startAttempted bool // monotonic host launch boundary, independent of registry publication
	mainStarted    bool // the daemon reported the workload running: teardown may then speak of its main process
	bindSources    map[string]os.FileInfo
	unsafeRoots    []string
	publish        []string // -p options the controller carries for serve.ports
	serveEnv       []string // COOP_SERVE_URL_* the agent container still gets
	servicesNet    string   // the Compose network the controller joins, if any
	services       []networkgateway.ServiceBinding
	// taskVolume is the run-private task-channel volume coop created THIS run (taskchannel.go).
	// It is coop-owned, holds only the coop socket, and is mounted read-only, so it is exempt from
	// the workload volume-exposure check the same way this run's generated files are — its backing
	// mountpoint lives inside the daemon VM and is not a host path an agent could redirect.
	taskVolume string
}

func (f *filteredExecution) workloadOutcome(code int, err error, cancelled bool) string {
	if cancelled {
		return "cancelled"
	}
	if err == nil && code >= 0 {
		return "exited"
	}
	if f.startAttempted {
		return "runtime_failed"
	}
	return "launch_failed"
}

// Only this bounded, exact-owner runtime surface is available to the launch
// supervisor. Tests replace the daemon, not the policy or custody checks.
type filteredDocker interface {
	Close() error
	CreateVolume(context.Context, runtime.DockerRef) (runtime.DockerVolume, error)
	InspectVolume(context.Context, runtime.DockerRef) (runtime.DockerVolume, bool, error)
	RemoveVolume(context.Context, runtime.DockerRef) error
	CreateContainer(context.Context, runtime.DockerCreate) (string, error)
	InspectContainer(context.Context, runtime.DockerRef) (runtime.DockerContainer, bool, error)
	StartContainer(context.Context, runtime.DockerRef) error
	StartAttached(context.Context, runtime.DockerRef, io.Reader, io.Writer, io.Writer, func() error) (int, error)
	StopContainer(context.Context, runtime.DockerRef, int) error
	RemoveContainer(context.Context, runtime.DockerRef) error
	ExecRead(context.Context, runtime.DockerRef, int, ...string) ([]byte, error)
	CopyArchive(context.Context, runtime.DockerRef, string, int) ([]byte, error)
	Image(context.Context, string) (string, map[string]string, error)
	ImageLayers(context.Context, string) (string, []string, error)
	FileDigest(context.Context, runtime.DockerRef, string, int64) (runtime.DockerFile, error)
	ExistingNamedVolumeExposure(context.Context, []string) (runtime.VolumeExposure, error)
	ConnectNetwork(context.Context, string, runtime.DockerRef) error
	NetworkMembers(context.Context, string) (map[string]netip.Addr, error)
	Networks(context.Context) ([]runtime.DockerNetwork, error)
	ComposeServiceID(context.Context, string, string) (string, error)
}

// All policy and exposure checks precede runtime mutation. A returned execution
// on error still owns any published intent; the caller must run its cleanup.
func prepareFilteredExecution(ctx context.Context, cfg *config.Config, rt runtime.Runtime, spec RunSpec, capture *CapturedEgress, composeFile string, smoke *networkSmokeLaunch) (*filteredExecution, error) {
	var servePorts []int
	if capture == nil || capture.Store == nil || ctx == nil {
		return nil, errors.New("a filtered launch needs the rules admission froze for it")
	}
	if cfg.Egress != "open" && cfg.Egress != "filtered" {
		return nil, errors.New("this box was started with networking off, so filtered egress cannot apply — turn networking on, or drop --egress filtered")
	}
	runRepo, err := filepath.Abs(projectPolicyRepo(spec))
	if err == nil {
		runRepo, err = filepath.EvalSymlinks(runRepo)
	}
	// The capture stays bound to the project whose approval and remembered
	// posture it was admitted against. A direct or loop launch runs IN that
	// project; a remote session's box mounts a fork workspace of it, so the two
	// paths differ there and the fork identity is what ties them together.
	project := capture.Project
	if err != nil || runRepo != project && !capturedSessionWorkspace(capture, spec, runRepo) {
		return nil, errors.New("these rules were approved for a different project directory")
	}
	policy, err := capture.Store.LoadSnapshot(project, capture.Fingerprint)
	if err != nil {
		return nil, err
	}
	if policy.Mode != egress.Filtered {
		return nil, errors.New("a filtered launch needs filtered rules; these are not")
	}
	if err := policy.RequireSupported(); err != nil {
		return nil, err
	}
	// The captured policy decides which ports this run captures, so the serve
	// check needs it — and it still runs before anything is created.
	if spec.Serve {
		if err := checkFilteredServePorts(spec.servePorts, policy.TLSPorts()); err != nil {
			return nil, err
		}
	}
	approvedServices := serviceGrants(policy)
	// box.network is the old join-everything switch; in filtered mode a sidecar
	// is reached through an approved `to: {service: <name>}` grant, one exact
	// container at a time, never by joining a shared network.
	if len(approvedServices) == 0 && spec.Network && (composeFile != "" || cfg.ServicesNet != "") {
		return nil, errors.New("a filtered box does not join the shared services network — ask for the one sidecar you need with a `to: {service: <name>}` rule in .agent/project.yaml, then run 'coop net approve'")
	}
	exposed := []string{spec.Repo, project}
	exposed = append(exposed, ConfigExposureRoots(cfg)...)
	for _, companion := range spec.CompanionRepositories {
		exposed = append(exposed, companion.HostPath)
	}
	for _, name := range credentialScope(cfg, spec) {
		exposed = append(exposed, cfg.AgentDir(name))
	}
	if err := capture.Store.CheckExposure(exposed); err != nil {
		return nil, err
	}
	candidate, err := capturedNetworkCandidate(capture, policy, smoke)
	if err != nil {
		return nil, err
	}
	// The qualified candidate owns the endpoint. Replaying an admitted launch
	// must not rediscover a later ambient Docker context. An empty daemon
	// argument permits launch; the full candidate tuple is checked below.
	docker, err := runtime.BindDocker(ctx, rt, candidate.Runtime.Endpoint, "")
	if err != nil {
		return nil, err
	}
	if !spec.Quiet {
		// One line, once: a filtered launch waiting on a busy daemon should say
		// so rather than look hung.
		docker.OnSlowStart = func(elapsed time.Duration) {
			ui.Detail("waiting for Docker to start this box (%s so far)", elapsed.Round(time.Second))
		}
	}
	f := &filteredExecution{store: capture.Store, docker: docker, policy: policy, attempted: map[string]bool{}, taskVolume: spec.taskVolume}
	f.unsafeRoots = []string{spec.Repo, project}
	if roots := ConfigExposureRoots(cfg); len(roots) > 1 {
		f.unsafeRoots = append(f.unsafeRoots, roots[1:]...)
	}
	for _, name := range credentialScope(cfg, spec) {
		f.unsafeRoots = append(f.unsafeRoots, cfg.AgentDir(name))
	}
	if err := verifyNetworkCandidate(candidate, networkRuntimeBinding(docker.Info(), docker.Endpoint()), cfg.ImageOverride); err != nil {
		return f, err
	}
	for _, expected := range []string{candidate.ClientImage, candidate.GatewayImage} {
		observed, _, err := docker.Image(ctx, expected)
		if err != nil || observed != expected {
			return f, errors.New("the images this host was set up with disappeared while the box was starting — run it again")
		}
	}
	f.image = candidate.ClientImage
	// A project that ships its own box Dockerfile runs its own image, built on
	// the locked client image and proven derived from it with its clients intact
	// (derived_image.go) — before any container of this run exists. The preflight
	// smoke is deliberately excluded: it qualifies the locked image itself, and
	// what it proves must be that image, not a project's layers on top of it.
	if smoke == nil {
		derived, err := filteredProjectImage(ctx, rt, f.docker, f.store, spec, candidate)
		if err != nil {
			return f, err
		}
		if derived != "" {
			f.image = derived
		}
	}
	f.publish, servePorts, f.serveEnv = filteredPublish(cfg, spec, hostPortFree)
	if len(approvedServices) != 0 {
		privateRoots := append(ConfigExposureRoots(cfg), project)
		for _, companion := range spec.CompanionRepositories {
			privateRoots = append(privateRoots, companion.HostPath)
		}
		// The remembered approval, not the snapshot, carries what each service
		// was approved to BE. A capture cannot answer that: it froze the rules.
		approval, err := capture.Store.Approval(project)
		if err != nil {
			return f, err
		}
		f.servicesNet, f.services, err = resolveServiceBindings(ctx, docker, rt, spec, composeFile, approval, approvedServices, privateRoots)
		if err != nil {
			return f, err
		}
	}
	// The protection envelope is inventoried AFTER the approved sidecars are up:
	// starting them can add a runtime network, and this run must protect the
	// host topology it will actually launch into, not the one before it.
	networks, err := docker.Networks(ctx)
	if err != nil {
		return f, err
	}
	protected, ingress, err := filteredProtectedAddresses(networks, f.hostAddresses)
	if err != nil {
		return f, err
	}
	f.protected = protected
	if len(servePorts) != 0 && !ingress.IsValid() {
		return f, errors.New("this Docker has no bridge gateway address, so a published serve port could not be limited to your host — drop serve.ports, or run without --egress filtered")
	}
	// The record stays bound to the QUALIFIED client image; a project image the
	// two proofs accepted is recorded beside it as what the workload actually
	// ran, never as what this run was qualified for.
	projectImage := ""
	if f.image != candidate.ClientImage {
		projectImage = f.image
	}
	executionSpec := networkstate.ExecutionSpec{Project: project, PolicyFingerprint: policy.Fingerprint,
		QualificationID: capture.QualificationID, ClientImage: candidate.ClientImage, ProjectImage: projectImage,
		Runtime: "docker", DaemonID: docker.Info().ID, Endpoint: docker.Endpoint(), GatewayImage: candidate.GatewayImage,
		SessionID: capture.SessionID, AttemptID: capture.AttemptID, Protected: protected}
	if smoke == nil {
		f.record, err = f.store.CreateExecution(ctx, executionSpec)
	} else {
		f.record, err = smoke.authority.CreateExecution(ctx, executionSpec)
	}
	if err != nil {
		return f, err
	}
	if smoke != nil && smoke.registered != nil {
		smoke.registered(f.record)
	}
	launch := networkgateway.LaunchConfig{Version: 1, RunID: f.record.ID, Epoch: f.record.Epoch, Policy: policy, Protected: protected,
		Services: f.services, Serve: servePorts, Ingress: ingress}
	if err := launch.Validate(); err != nil {
		return f, err
	}
	data, err := json.Marshal(launch)
	if err != nil {
		return f, err
	}
	f.record, err = f.store.PrepareArtifacts(ctx, f.record.ID, f.record.Revision, data)
	if err != nil {
		return f, err
	}
	if f.config, err = f.store.LaunchConfigPath(f.record.ID); err != nil {
		return f, err
	}
	f.runfiles, err = f.store.RunFilesPath(f.record.ID)
	return f, err
}

// filteredProtectedAddresses is the envelope this run's kernel refuses before
// any grant: this host's own interface addresses AND the runtime's networks —
// every subnet it allocates plus the gateway the daemon holds in it. Without
// the second half a granted CIDR that happens to cover a bridge subnet would
// reach every sibling container, another session's box, and the daemon's own
// gateway; none of those is a destination any rule may grant.
//
// It also returns the default bridge's gateway: that is the source address a
// host-published serve port arrives from, and the only ingress a served port
// accepts. An invalid address means this runtime cannot publish one.
func filteredProtectedAddresses(networks []runtime.DockerNetwork, hostAddresses func() ([]netip.Prefix, error)) ([]netip.Prefix, netip.Addr, error) {
	if hostAddresses == nil {
		hostAddresses = filteredHostAddresses
	}
	protected, err := hostAddresses()
	if err != nil {
		return nil, netip.Addr{}, err
	}
	var ingress netip.Addr
	for _, network := range networks {
		for _, subnet := range network.Subnets {
			if subnet.Addr().Is4() && !slices.Contains(protected, subnet) {
				protected = append(protected, subnet)
			}
		}
		for _, gateway := range network.Gateways {
			if !gateway.Is4() {
				continue
			}
			if network.Name == "bridge" && !ingress.IsValid() {
				ingress = gateway
			}
			prefix := netip.PrefixFrom(gateway, gateway.BitLen())
			if !slices.Contains(protected, prefix) {
				protected = append(protected, prefix)
			}
		}
	}
	if len(protected) == 0 || len(protected) > networkgateway.MaxProtectedRanges {
		return nil, netip.Addr{}, errors.New("host topology exceeds the qualified protection envelope")
	}
	slices.SortFunc(protected, func(a, b netip.Prefix) int { return strings.Compare(a.String(), b.String()) })
	return protected, ingress, nil
}

func filteredHostAddresses() ([]netip.Prefix, error) {
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return nil, errors.New("this host's own addresses could not be read, and a filtered box must protect them")
	}
	var protected []netip.Prefix
	for _, value := range addresses {
		prefix, err := netip.ParsePrefix(value.String())
		if err != nil {
			return nil, errors.New("host interface address cannot be protected unambiguously")
		}
		address := prefix.Addr().Unmap()
		prefix = netip.PrefixFrom(address, address.BitLen())
		if !slices.Contains(protected, prefix) {
			protected = append(protected, prefix)
		}
	}
	if len(protected) == 0 || len(protected) > networkgateway.MaxProtectedRanges {
		return nil, errors.New("host topology exceeds the qualified protection envelope")
	}
	slices.SortFunc(protected, func(a, b netip.Prefix) int { return strings.Compare(a.String(), b.String()) })
	return protected, nil
}

func (f *filteredExecution) ref(role string) runtime.DockerRef {
	f.mu.Lock()
	defer f.mu.Unlock()
	return networkResourceRef(f.record, role)
}

func networkResourceRef(record networkstate.Execution, role string) runtime.DockerRef {
	for _, resource := range record.Resources {
		if resource.Role == role {
			return runtime.DockerRef{Name: resource.Name, ID: resource.ID, Labels: map[string]string{
				"coop.network.run": record.ID, "coop.network.epoch": record.Epoch,
				"coop.network.scope": record.Scope, "coop.network.role": role,
			}}
		}
	}
	return runtime.DockerRef{}
}

// Reread before each CAS: a failed rename/fsync may have published the previous
// revision. No runtime operation happens while the registry lock is held.
func (f *filteredExecution) update(ctx context.Context, change func(networkstate.Execution) (networkstate.Execution, error)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	current, err := f.store.Execution(f.record.ID)
	if err != nil {
		return err
	}
	f.record = current
	next, err := change(current)
	if err == nil {
		f.record = next
	}
	return err
}

func (f *filteredExecution) transition(ctx context.Context, role, phase string) error {
	return f.update(ctx, func(r networkstate.Execution) (networkstate.Execution, error) {
		switch phase {
		case "creating":
			return f.store.BeginResourceCreation(ctx, r.ID, r.Revision, role)
		case "starting":
			return f.store.BeginResourceStart(ctx, r.ID, r.Revision, role)
		case "started":
			return f.store.RecordResourceStarted(ctx, r.ID, r.Revision, role)
		default:
			return r, errors.New("invalid filtered launch phase")
		}
	})
}

func (f *filteredExecution) created(ctx context.Context, role, id string) error {
	return f.update(ctx, func(r networkstate.Execution) (networkstate.Execution, error) {
		return f.store.RecordResourceCreated(ctx, r.ID, r.Revision, role, id)
	})
}

func (f *filteredExecution) accept(ctx context.Context, snapshot networkview.Snapshot) error {
	return f.update(ctx, func(r networkstate.Execution) (networkstate.Execution, error) {
		return f.store.AcceptSnapshot(ctx, r.ID, r.Revision, snapshot)
	})
}
