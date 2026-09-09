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

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkgateway"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/networkview"
	"github.com/AndrewDryga/coop/internal/runtime"
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
	bindSources    map[string]os.FileInfo
	unsafeRoots    []string
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
	ExistingNamedVolumeExposure(context.Context, []string) (runtime.VolumeExposure, error)
}

// All policy and exposure checks precede runtime mutation. A returned execution
// on error still owns any published intent; the caller must run its cleanup.
func prepareFilteredExecution(ctx context.Context, cfg *config.Config, rt runtime.Runtime, spec RunSpec, capture *CapturedEgress, composeFile string, smoke *networkSmokeLaunch) (*filteredExecution, error) {
	if capture == nil || capture.Store == nil || ctx == nil {
		return nil, errors.New("filtered launch requires a trusted captured network policy")
	}
	if cfg.Egress != "open" && cfg.Egress != "filtered" {
		return nil, errors.New("filtered capture conflicts with the offline network ceiling")
	}
	if spec.Network && (composeFile != "" || cfg.ServicesNet != "") || spec.Serve && len(spec.servePorts) != 0 {
		return nil, errors.New("restricted networking does not yet qualify sibling services or published ports")
	}
	project, err := filepath.Abs(projectPolicyRepo(spec))
	if err == nil {
		project, err = filepath.EvalSymlinks(project)
	}
	if err != nil || project != capture.Project {
		return nil, errors.New("filtered capture belongs to a different canonical project")
	}
	policy, err := capture.Store.LoadSnapshot(project, capture.Fingerprint)
	if err != nil {
		return nil, err
	}
	if policy.Mode != egress.Filtered || policy.RequireTLS443(true) != nil {
		return nil, errors.New("network policy exceeds the qualified visible-SNI TLS443 subset")
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
	protected, err := filteredHostAddresses()
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
	f := &filteredExecution{store: capture.Store, docker: docker, policy: policy, protected: protected, attempted: map[string]bool{}}
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
			return f, errors.New("qualified network image is unavailable; explicit setup is required")
		}
	}
	f.image = candidate.ClientImage
	executionSpec := networkstate.ExecutionSpec{Project: project, PolicyFingerprint: policy.Fingerprint,
		QualificationID: capture.QualificationID, ClientImage: f.image,
		Runtime: "docker", DaemonID: docker.Info().ID, Endpoint: docker.Endpoint(), GatewayImage: candidate.GatewayImage,
		SessionID: capture.SessionID, AttemptID: capture.AttemptID}
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
	launch := networkgateway.LaunchConfig{Version: 1, RunID: f.record.ID, Epoch: f.record.Epoch, Policy: policy, Protected: protected}
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

func filteredHostAddresses() ([]netip.Prefix, error) {
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return nil, errors.New("cannot inventory protected host interface addresses")
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
