package box

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/networkgateway"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/networkview"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/wait"
	"github.com/AndrewDryga/coop/internal/ui"
)

type filteredDaemonFixture struct {
	serviceDefinition                      string // what LabeledComposeServiceID finds on composeServices
	mu                                     sync.Mutex
	f                                      *filteredExecution
	containers                             map[string]runtime.DockerContainer
	volumes                                map[string]runtime.DockerVolume
	log                                    []string
	sequence                               networkview.Count
	ambiguous, refuseRemoval, corruptRole  string
	brokenProbe, holdAgent, malformedFinal bool
	slowFinal                              bool // the final observation's archive read outlasts its deadline
	ctrlCAtStart                           bool // the agent's start is recorded as ended by Ctrl-C, not as a refusal
	resolverStatus                         *string
	corruptMount                           string
	earlyGuardEnd, failStartupPersistence  bool
	notAttempted, refused                  string // roles whose create was never sent, or Docker refused
	transientProbes, successfulProbes      int
	stopGrace                              map[string]int
	probeRecovered                         chan struct{}
	attached                               chan struct{}
	volumeExposure                         runtime.VolumeExposure
	smoke                                  *networkstate.QualificationSmoke
	networksErr                            error
	extraNetworks                          []runtime.DockerNetwork // networks that appeared after launch
	applied                                []string                // ExecApply argv, joined
	onApply                                func()                  // runs inside ExecApply, after it applied
	applyErr                               error
	networkMembers                         map[string]netip.Addr
	composeServices                        map[string]string
	connected                              []string
	onStart                                func(string)
	failStart                              string
	beforeCall                             func(op string) // runs before the daemon lock, so a test can hold calls in flight together
	images                                 map[string]fixtureImage
	layerReads, fileReads, treeReads       map[string]int
	created                                map[string]time.Time // by container name; one without an entry is never stale
	prunes                                 []string             // every PruneContainers call, "<label> older than <age>"
	pruneErr                               error
	// The ambiguous role's create reports no outcome, as a client that timed out does; Docker
	// finishes it, into these, only when the test calls finishLateCreates.
	lateContainers map[string]runtime.DockerContainer
	lateVolumes    map[string]runtime.DockerVolume
}

// finishLateCreates is Docker completing the creates whose clients had already given up.
func (d *filteredDaemonFixture) finishLateCreates() {
	d.mu.Lock()
	defer d.mu.Unlock()
	maps.Copy(d.containers, d.lateContainers)
	maps.Copy(d.volumes, d.lateVolumes)
	d.lateContainers, d.lateVolumes = nil, nil
}

func (d *filteredDaemonFixture) ConnectNetwork(_ context.Context, network string, ref runtime.DockerRef, aliases ...string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ref.ID == "" {
		return errors.New("network attachment needs an exact container id")
	}
	d.log = append(d.log, "connect:"+ref.Labels["coop.network.role"])
	d.connected = append(d.connected, network+"/"+ref.ID+"/"+strings.Join(aliases, ","))
	return nil
}

// Networks is the daemon's own topology: one bridge, and the compose network
// the fixture's approved sidecars sit on.
func (d *filteredDaemonFixture) Networks(context.Context) ([]runtime.DockerNetwork, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.networksErr != nil {
		return nil, d.networksErr
	}
	return append([]runtime.DockerNetwork{
		{Name: "bridge", Subnets: []netip.Prefix{netip.MustParsePrefix("172.17.0.0/16")}, Gateways: []netip.Addr{netip.MustParseAddr("172.17.0.1")}},
		{Name: "coop-fixture_default", Subnets: []netip.Prefix{netip.MustParsePrefix("172.31.0.0/16")}, Gateways: []netip.Addr{netip.MustParseAddr("172.31.0.1")}},
	}, d.extraNetworks...), nil
}

// ExecApply records the host's one post-launch mutation (the protected-set
// re-render) and fails it on demand.
func (d *filteredDaemonFixture) ExecApply(_ context.Context, _ runtime.DockerRef, command ...string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.applyErr != nil {
		return d.applyErr
	}
	d.applied = append(d.applied, strings.Join(command, " "))
	if d.onApply != nil {
		d.onApply()
	}
	return nil
}

func (d *filteredDaemonFixture) NetworkMembers(context.Context, string) (map[string]netip.Addr, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return maps.Clone(d.networkMembers), nil
}

func (d *filteredDaemonFixture) ComposeServiceID(_ context.Context, _, service string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if id, ok := d.composeServices[service]; ok {
		return id, nil
	}
	return "", errors.New("service " + service + " is not running as exactly one container")
}

func (d *filteredDaemonFixture) LabeledComposeServiceID(_ context.Context, _, service, _, value string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if id, ok := d.composeServices[service]; ok && d.serviceDefinition == value {
		return id, nil
	}
	return "", errors.New("service " + service + " is not running as exactly one container")
}

func filteredFixture(t *testing.T) (*filteredExecution, *filteredDaemonFixture) {
	t.Helper()
	store, err := networkstate.Open(filepath.Join(t.TempDir(), "network"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	project, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mode := egress.Filtered
	policy, err := store.Admit(project, networkstate.Admission{InvocationMode: &mode})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	smoke, err := store.BeginQualification(networkstate.CandidateSpec{
		Runtime:     networkstate.RuntimeBinding{HostFamily: "darwin", Endpoint: "unix:///fixture.sock", DaemonID: "fixture-daemon", OS: "linux", Architecture: "arm64", ServerVersion: "29.4.0", KernelVersion: "fixture"},
		ClientImage: "sha256:" + strings.Repeat("b", 64), GatewayImage: "sha256:" + strings.Repeat("a", 64),
		ClientDefinition: strings.Repeat("c", 64), ClientClosure: strings.Repeat("d", 64), GatewaySource: strings.Repeat("e", 64),
		Libc: "glibc", NodeBase: "node@sha256:" + strings.Repeat("f", 64), GoBase: "golang@sha256:" + strings.Repeat("0", 64)},
		[]networkstate.QualifiedClient{{Provider: "claude", Client: egress.ClientCLI, Version: "2.1.260"}})
	if err != nil {
		t.Fatal(err)
	}
	host, _ := thisHost() // as prepareFilteredExecution records it
	record, err := smoke.CreateExecution(ctx, networkstate.ExecutionSpec{Host: host, Project: project, PolicyFingerprint: policy.Fingerprint,
		Runtime: "docker", DaemonID: "fixture-daemon", Endpoint: "unix:///fixture.sock", GatewayImage: "sha256:" + strings.Repeat("a", 64), ClientImage: "sha256:" + strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(networkgateway.LaunchConfig{Version: 1, RunID: record.ID, Epoch: record.Epoch, AgentUID: uint32(boxAgentIdentity().uid), Policy: policy})
	record, err = store.PrepareArtifacts(ctx, record.ID, record.Revision, data)
	if err != nil {
		t.Fatal(err)
	}
	f := &filteredExecution{store: store, record: record, policy: policy, image: "sha256:" + strings.Repeat("b", 64), agentIdentity: boxAgentIdentity(), ids: map[string]string{}}
	f.config, err = store.LaunchConfigPath(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.runfiles, err = store.RunFilesPath(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.hostAddresses = func() ([]netip.Prefix, error) { return []netip.Prefix{netip.MustParsePrefix("192.0.2.1/32")}, nil }
	d := &filteredDaemonFixture{f: f, smoke: smoke, containers: map[string]runtime.DockerContainer{}, volumes: map[string]runtime.DockerVolume{},
		images: map[string]fixtureImage{}, layerReads: map[string]int{}, fileReads: map[string]int{}, treeReads: map[string]int{}, attached: make(chan struct{})}
	f.docker = d
	// The envelope a real launch inventories: this host plus the daemon's networks.
	networks, _ := d.Networks(ctx)
	if f.protected, _, err = filteredProtectedAddresses(networks, f.hostAddresses); err != nil {
		t.Fatal(err)
	}
	return f, d
}

// arrive runs the test's hook for op outside the daemon lock, where two calls can meet.
func (d *filteredDaemonFixture) arrive(op string) {
	if d.beforeCall != nil {
		d.beforeCall(op)
	}
}

// runningID reports whether the container with this exact id is running. The caller holds d.mu.
func (d *filteredDaemonFixture) runningID(id string) bool {
	for _, container := range d.containers {
		if container.ID == id {
			return container.State.Running
		}
	}
	return false
}

// faultSelects reports whether a fixture fault names this role. An unset fault
// names NO role — including the roleless container an image proof creates.
func faultSelects(fault, role string) bool { return fault != "" && fault == role }

// fixtureImage is one image this daemon has: what `image inspect` would report
// plus the files an image proof may read out of it.
type fixtureImage struct {
	id     string
	labels map[string]string
	layers []string
	files  map[string]runtime.DockerFile
	tree   runtime.DockerTree
}

func (d *filteredDaemonFixture) Image(_ context.Context, name string) (string, map[string]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	image, ok := d.images[name]
	if !ok {
		return "", nil, runtime.ErrDockerImageMissing
	}
	return image.id, maps.Clone(image.labels), nil
}

func (d *filteredDaemonFixture) ImageLayers(_ context.Context, name string) (string, []string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	image, ok := d.images[name]
	if !ok {
		return "", nil, errors.New("fixture has no image " + name)
	}
	d.layerReads[image.id]++
	return image.id, slices.Clone(image.layers), nil
}

// TagImage names an image the fixture has, as `docker image tag` would.
func (d *filteredDaemonFixture) TagImage(_ context.Context, id, tag string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	image, ok := d.images[id]
	if !ok || image.id != id {
		return errors.New("fixture has no image " + id)
	}
	d.images[tag] = image
	return nil
}

// FileDigest answers from the image the named container was created from, and
// only for a container that exists — the proof never starts one.
func (d *filteredDaemonFixture) FileDigest(_ context.Context, ref runtime.DockerRef, source string, limit int64) (runtime.DockerFile, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	container, ok := d.containers[ref.Name]
	if !ok || ref.ID == "" || limit <= 0 {
		return runtime.DockerFile{}, errors.New("fixture has no created container " + ref.Name)
	}
	image, ok := d.images[container.Image]
	if !ok {
		return runtime.DockerFile{}, errors.New("fixture has no image " + container.Image)
	}
	file, ok := image.files[source]
	if !ok {
		return runtime.DockerFile{}, errors.New("no such file or directory")
	}
	d.fileReads[image.id]++
	return file, nil
}

func (d *filteredDaemonFixture) TreeDigest(_ context.Context, ref runtime.DockerRef, source string, limit int64) (runtime.DockerTree, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	container, ok := d.containers[ref.Name]
	if !ok || ref.ID == "" || source == "" || limit <= 0 {
		return runtime.DockerTree{}, errors.New("fixture has no created container " + ref.Name)
	}
	image, ok := d.images[container.Image]
	if !ok || image.tree.Size == 0 {
		return runtime.DockerTree{}, errors.New("no such directory")
	}
	d.treeReads[image.id]++
	return image.tree, nil
}

// ExistingNamedVolumeExposure reports no backing sources: the fixture's ordinary
// volumes are daemon-managed, so exposure inspection has nothing to refuse.
func (d *filteredDaemonFixture) ExistingNamedVolumeExposure(context.Context, []string) (runtime.VolumeExposure, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.log = append(d.log, "volume-exposure")
	return d.volumeExposure, nil
}

func (d *filteredDaemonFixture) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.log = append(d.log, "close")
	return nil
}
func (d *filteredDaemonFixture) CreateVolume(_ context.Context, ref runtime.DockerRef) (runtime.DockerVolume, error) {
	role := ref.Labels["coop.network.role"]
	d.arrive("create:" + role)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.log = append(d.log, "create:"+role)
	if faultSelects(d.notAttempted, role) {
		return runtime.DockerVolume{}, runtime.ErrDockerCreateNotAttempted
	}
	value := runtime.DockerVolume{Name: ref.Name, Driver: "local", Labels: ref.Labels}
	if faultSelects(d.ambiguous, role) {
		if d.lateVolumes == nil {
			d.lateVolumes = map[string]runtime.DockerVolume{}
		}
		d.lateVolumes[ref.Name] = value
		return runtime.DockerVolume{}, context.DeadlineExceeded
	}
	d.volumes[ref.Name] = value
	return value, nil
}
func (d *filteredDaemonFixture) InspectVolume(_ context.Context, ref runtime.DockerRef) (runtime.DockerVolume, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	v, ok := d.volumes[ref.Name]
	if ok && !fixtureLabelsMatch(ref.Labels, v.Labels) {
		return runtime.DockerVolume{}, false, runtime.ErrDockerOwnership
	}
	return v, ok, nil
}

// fixtureLabelsMatch is Docker's exact-owner rule: every label the reference names, as it names it.
func fixtureLabelsMatch(want, have map[string]string) bool {
	for key, value := range want {
		if have[key] != value {
			return false
		}
	}
	return true
}
func (d *filteredDaemonFixture) RemoveVolume(_ context.Context, ref runtime.DockerRef) error {
	d.arrive("remove:" + ref.Labels["coop.network.role"])
	d.mu.Lock()
	defer d.mu.Unlock()
	role := ref.Labels["coop.network.role"]
	d.log = append(d.log, "remove:"+role)
	if faultSelects(d.refuseRemoval, role) {
		return errors.New("fixture removal unknown")
	}
	// Docker refuses to remove a volume any container still has, running or not.
	for _, container := range d.containers {
		for _, mount := range container.Mounts {
			if mount.Type == "volume" && mount.Name == ref.Name {
				return errors.New("fixture volume is in use")
			}
		}
	}
	delete(d.volumes, ref.Name)
	return nil
}
func (d *filteredDaemonFixture) CreateContainer(_ context.Context, spec runtime.DockerCreate) (string, error) {
	role := spec.Ref.Labels["coop.network.role"]
	d.arrive("create:" + role)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.log = append(d.log, "create:"+role)
	if faultSelects(d.notAttempted, role) {
		return "", runtime.ErrDockerCreateNotAttempted
	}
	if faultSelects(d.refused, role) {
		return "", runtime.ErrDockerCreateRefused
	}
	hash := sha256.Sum256([]byte(spec.Ref.Name))
	id := hex.EncodeToString(hash[:])
	v := runtime.DockerContainer{ID: id, Name: spec.Ref.Name, Image: spec.Image, Labels: spec.Ref.Labels, RestartPolicy: "no", State: runtime.DockerContainerState{Status: "created"}}
	for i := 0; i < len(spec.Options); i++ {
		switch spec.Options[i] {
		case "--user":
			i++
			v.User = spec.Options[i]
		case "--network":
			i++
			v.NetworkMode = spec.Options[i]
		case "--cap-drop":
			i++
			v.CapDrop = append(v.CapDrop, spec.Options[i])
		case "--cap-add":
			i++
			v.CapAdd = append(v.CapAdd, spec.Options[i])
		case "--security-opt":
			i++
			v.SecurityOpt = append(v.SecurityOpt, spec.Options[i])
		case "--read-only":
			v.ReadonlyRootfs = true
		}
	}
	if faultSelects(d.corruptRole, role) {
		v.User = "0"
	}
	if d.corruptRole == "agent-wrong-user" && role == "agent" {
		v.User = "1000:1000"
	}
	plan, err := networkMountPlan(spec.Options)
	if err != nil {
		return "", err
	}
	for _, mount := range plan {
		if mount.Type != "tmpfs" {
			v.Mounts = append(v.Mounts, mount)
		}
	}
	if faultSelects(d.corruptMount, role) && len(v.Mounts) > 0 {
		v.Mounts[0].RW = !v.Mounts[0].RW
	}
	v.Tmpfs = map[string]string{}
	for i := 0; i < len(spec.Options); i++ {
		if spec.Options[i] == "--tmpfs" {
			i++
			dest, constraints, _ := strings.Cut(spec.Options[i], ":")
			v.Tmpfs[dest] = constraints
		}
	}
	if faultSelects(d.ambiguous, role) {
		if d.lateContainers == nil {
			d.lateContainers = map[string]runtime.DockerContainer{}
		}
		d.lateContainers[spec.Ref.Name] = v
		return "", context.DeadlineExceeded
	}
	d.containers[spec.Ref.Name] = v
	if d.created == nil {
		d.created = map[string]time.Time{}
	}
	d.created[spec.Ref.Name] = time.Now()
	return id, nil
}

// PruneContainers is Docker's `container prune` with a label and an `until` filter: every container
// carrying key that was created more than age ago goes, unless it is running.
func (d *filteredDaemonFixture) PruneContainers(_ context.Context, key string, age time.Duration) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.prunes = append(d.prunes, key+" older than "+age.String())
	if d.pruneErr != nil {
		return d.pruneErr
	}
	for name, container := range d.containers {
		created, known := d.created[name]
		if _, labeled := container.Labels[key]; labeled && known && !container.State.Running && time.Since(created) > age {
			delete(d.containers, name)
		}
	}
	return nil
}
func (d *filteredDaemonFixture) InspectContainer(_ context.Context, ref runtime.DockerRef) (runtime.DockerContainer, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	v, ok := d.containers[ref.Name]
	if ok && (!fixtureLabelsMatch(ref.Labels, v.Labels) || ref.ID != "" && ref.ID != v.ID) {
		return runtime.DockerContainer{}, false, runtime.ErrDockerOwnership
	}
	return v, ok, nil
}
func (d *filteredDaemonFixture) StartContainer(_ context.Context, ref runtime.DockerRef) error {
	role := ref.Labels["coop.network.role"]
	d.arrive("start:" + role)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.log = append(d.log, "start:"+role)
	if d.onStart != nil {
		d.onStart(role)
	}
	if faultSelects(d.failStart, role) {
		return errors.New("fixture start refused")
	}
	v := d.containers[ref.Name]
	// Docker starts a container that joins another's network namespace only while that one runs.
	if joined, ok := strings.CutPrefix(v.NetworkMode, "container:"); ok && !d.runningID(joined) {
		return errors.New("cannot join network namespace of a non running container")
	}
	v.State = runtime.DockerContainerState{Status: "running", Running: true, StartedAt: time.Now()}
	d.containers[ref.Name] = v
	return nil
}
func (d *filteredDaemonFixture) StartAttached(ctx context.Context, ref runtime.DockerRef, _ io.Reader, stdout, _ io.Writer, started func() error) (int, error) {
	if d.ctrlCAtStart {
		// Docker's client held the terminal raw, so the Ctrl-C reached the box's own first process.
		d.arrive("start:agent")
		d.mu.Lock()
		defer d.mu.Unlock()
		d.log = append(d.log, "start:agent")
		v := d.containers[ref.Name]
		v.State = runtime.DockerContainerState{Status: "created", ExitCode: 130}
		d.containers[ref.Name] = v
		return -1, runtime.ErrDockerStartInterrupted
	}
	if err := d.StartContainer(ctx, ref); err != nil {
		return -1, err
	}
	// A caller cancelled before coop saw the daemon start the workload never learns it started.
	if err := ctx.Err(); err != nil {
		return -1, err
	}
	if d.failStartupPersistence {
		file := filepath.Join(d.f.store.Path(), "execution-"+d.f.record.ID+".json")
		if err := os.Rename(file, file+".held"); err != nil {
			return -1, err
		}
		err := started()
		if restoreErr := os.Rename(file+".held", file); restoreErr != nil {
			return -1, restoreErr
		}
		return 0, err // adversarial client success alongside failed host persistence
	}
	if err := started(); err != nil {
		return -1, err
	}
	close(d.attached)
	if d.holdAgent {
		<-ctx.Done()
		return -1, ctx.Err()
	}
	_, _ = io.WriteString(stdout, "first provider output\n")
	d.mu.Lock()
	defer d.mu.Unlock()
	v := d.containers[ref.Name]
	v.State.Status = "exited"
	v.State.Running = false
	v.State.ExitCode = 7
	d.containers[ref.Name] = v
	return 7, nil
}
func (d *filteredDaemonFixture) StopContainer(_ context.Context, ref runtime.DockerRef, grace int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.log = append(d.log, "stop:"+ref.Labels["coop.network.role"])
	if d.stopGrace == nil {
		d.stopGrace = map[string]int{}
	}
	d.stopGrace[ref.Labels["coop.network.role"]] = grace
	if v, ok := d.containers[ref.Name]; ok {
		v.State.Running = false
		v.State.Status = "exited"
		d.containers[ref.Name] = v
	}
	return nil
}
func (d *filteredDaemonFixture) RemoveContainer(_ context.Context, ref runtime.DockerRef) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	role := ref.Labels["coop.network.role"]
	d.log = append(d.log, "remove:"+role)
	if faultSelects(d.refuseRemoval, role) {
		return errors.New("fixture removal unknown")
	}
	delete(d.containers, ref.Name)
	return nil
}
func (d *filteredDaemonFixture) observation(terminal bool) []byte {
	d.sequence++
	d.f.mu.Lock()
	runID, epoch := d.f.record.ID, d.f.record.Epoch
	d.f.mu.Unlock()
	s := networkview.Snapshot{Version: networkview.Version, RunID: runID, Epoch: epoch, PolicyFingerprint: d.f.policy.Fingerprint,
		Mode: egress.Filtered, Sequence: d.sequence, Terminal: terminal, AsOf: time.Now().UTC(), Availability: "available", Projection: "owner-local"}
	s.Health = networkview.HealthLayers{Enforcer: networkview.Health{Status: "ready"}, Gateway: networkview.Health{Status: "ready"}, Collector: networkview.Health{Status: "ready"}, Resolver: networkview.Health{Status: "ready"}}
	if d.resolverStatus != nil {
		s.Health.Resolver.Status = *d.resolverStatus
	}
	exact := networkview.MetricCoverage{Status: "exact"}
	s.Coverage = networkview.Coverage{ProxyBytes: exact, Connections: exact, UpstreamFailures: exact, KernelPackets: exact, GuardDenials: exact, MaintenanceQueries: exact, MaintenanceBytes: exact, SocketInventory: exact, BoundaryAttribution: exact}
	s.Counters = &networkview.Counters{SentBytes: networkview.Value(123)}
	identity := networkgateway.Identity{RunID: s.RunID, Epoch: s.Epoch, PolicyFingerprint: s.PolicyFingerprint,
		Clock: networkgateway.ClockDomain{BootID: "12345678-1234-1234-1234-123456789abc", TimeNamespace: "42", BootOffset: "0:0"}}
	data, _ := json.Marshal(networkgateway.RuntimeObservation{Version: networkgateway.PrivateProtocolVersion, Identity: identity, Network: s})
	return append(data, '\n')
}
func (d *filteredDaemonFixture) ExecRead(_ context.Context, _ runtime.DockerRef, _ int, command ...string) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if command[len(command)-1] == "probe" {
		if d.earlyGuardEnd && d.f.resource("agent").State == "gone" {
			return nil, errors.New("fixture collector already ended")
		}
		select {
		case <-d.attached:
			if d.transientProbes > 0 {
				d.transientProbes--
				return nil, errors.New("fixture daemon temporarily unavailable")
			}
			if d.brokenProbe {
				return nil, errors.New("fixture guard unhealthy")
			}
			d.successfulProbes++
			if d.probeRecovered != nil {
				select {
				case d.probeRecovered <- struct{}{}:
				default:
				}
			}
		default:
		}
		return nil, nil
	}
	d.log = append(d.log, "snapshot")
	return d.observation(false), nil
}
func (d *filteredDaemonFixture) CopyArchive(_ context.Context, ref runtime.DockerRef, path string, _ int) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.log = append(d.log, "capture-final")
	// A container that never started wrote nothing: Docker reports the file missing.
	if d.containers[ref.Name].State.StartedAt.IsZero() {
		return nil, errors.New("Could not find the file " + path)
	}
	if d.slowFinal {
		// What the runtime returns when a loaded Docker outlasts the archive read's bound.
		return nil, errors.Join(errors.New("Docker archive read failed"), context.DeadlineExceeded)
	}
	if d.malformedFinal {
		return []byte("invalid archive"), nil
	}
	data := d.observation(true)
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	_ = w.WriteHeader(&tar.Header{Name: "final.json", Mode: 0600, Size: int64(len(data))})
	_, _ = w.Write(data)
	_ = w.Close()
	return b.Bytes(), nil
}

// rendezvous holds each pair of daemon calls until both have arrived, so a pair gets through only if
// the launch has both in flight at once; one run after the other never meets, and fails the test.
func rendezvous(t *testing.T, pairs ...[2]string) func(string) {
	arrived, partner, once := map[string]chan struct{}{}, map[string]string{}, map[string]*sync.Once{}
	for _, pair := range pairs {
		for i, op := range pair {
			arrived[op], partner[op], once[op] = make(chan struct{}), pair[1-i], &sync.Once{}
		}
	}
	return func(op string) {
		if _, ok := arrived[op]; !ok {
			return
		}
		once[op].Do(func() { close(arrived[op]) })
		select {
		case <-arrived[partner[op]]:
		case <-time.After(5 * time.Second):
			t.Errorf("%s was never in flight together with %s", op, partner[op])
		}
	}
}

// matchGroups consumes log from the front: each group's entries come next, in any order among
// themselves. It returns what is left and whether every group matched.
func matchGroups(log []string, groups ...[]string) ([]string, bool) {
	for _, group := range groups {
		if len(log) < len(group) {
			return log, false
		}
		next, want := slices.Clone(log[:len(group)]), slices.Clone(group)
		slices.Sort(next)
		slices.Sort(want)
		if !slices.Equal(next, want) {
			return log, false
		}
		log = log[len(group):]
	}
	return log, true
}

func TestFilteredLaunchOrdersReadinessAndExactCleanup(t *testing.T) {
	f, d := filteredFixture(t)
	meet := rendezvous(t, [2]string{"create:ipc", "create:observations"}, [2]string{"start:controller", "create:guard"})
	// Hold the controller's start a moment: a guard start issued before the controller runs reaches
	// the daemon first — signalled from inside it, so its refusal is decided before the hold ends.
	guardStarting := make(chan struct{})
	var guardStartingOnce sync.Once
	d.onStart = func(role string) {
		if role == "guard" {
			guardStartingOnce.Do(func() { close(guardStarting) })
		}
	}
	d.beforeCall = func(op string) {
		meet(op)
		if op == "start:controller" {
			select {
			case <-guardStarting:
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	var output bytes.Buffer
	launches := 0
	code, err := f.launch(context.Background(), RunSpec{Cmd: []string{"fixture"}, OnRuntimeLaunch: func() { launches++ }}, nil, nil, &output, io.Discard)
	if err != nil || code != 7 || launches != 1 || output.String() != "first provider output\n" {
		t.Fatalf("launch: code=%d hook=%d output=%q err=%v", code, launches, output.String(), err)
	}
	gone, err := f.cleanup("exited")
	if err != nil || !gone {
		t.Fatal("cleanup", gone, err)
	}
	// The causal order is exact wherever one step needs another: the controller exists before the
	// guard is created in its namespace and runs before the guard starts; the guard is ready before the
	// agent is created, and ready again before it starts. Teardown: the agent gone before the guard
	// stops, the final observation taken from the stopped guard, the controller removed only once the
	// guard is gone, the volumes last. Independent steps run together — the two volumes, the guard's
	// creation beside the controller's start, the guard's removal beside the controller's stop — so
	// only their membership is pinned.
	rest, ordered := matchGroups(d.log,
		[]string{"create:ipc", "create:observations"}, []string{"create:controller"},
		[]string{"start:controller", "create:guard"}, []string{"start:guard"},
		[]string{"snapshot"}, []string{"create:agent"}, []string{"snapshot"}, []string{"start:agent"},
		[]string{"stop:agent"}, []string{"remove:agent"}, []string{"stop:guard"}, []string{"capture-final"})
	if ordered {
		together := rest[:min(3, len(rest))]
		guardStop, guardRemove := slices.Index(together, "stop:guard"), slices.Index(together, "remove:guard")
		rest, ordered = matchGroups(rest, []string{"stop:controller", "stop:guard", "remove:guard"}, []string{"stop:controller"},
			[]string{"remove:controller"}, []string{"remove:ipc", "remove:observations"}, []string{"close"})
		ordered = ordered && len(rest) == 0 && guardRemove > guardStop
	}
	if !ordered {
		t.Fatalf("lifecycle order:\n%v\nwant the volumes together, the controller, its start beside the guard's creation, the guard's start, readiness, the agent, then teardown", d.log)
	}
	record, err := f.store.Execution(f.record.ID)
	if err != nil || record.Receipt == nil || record.Receipt.Completeness != "complete" || record.Receipt.Cleanup != "complete" || *record.Receipt.Snapshot.Counters.SentBytes != 123 {
		t.Fatal("receipt was missing, partial, or double counted", err)
	}
	if _, err := os.Stat(f.runfiles); !os.IsNotExist(err) {
		t.Fatal("generated files retained after complete cleanup", err)
	}
}

// A filtered box gets the gate's memory folder like any Docker box. The daemon reports exactly the
// planned folder before the agent starts, and the agent runs.
func TestFilteredLaunchCreatesTheAgentWithTheGateTmpfs(t *testing.T) {
	f, d := filteredFixture(t)
	gate := gateTmpfs("4g")
	code, err := f.launch(context.Background(), RunSpec{Cmd: []string{"fixture"}}, []string{"--tmpfs", gate}, nil, io.Discard, io.Discard)
	if err != nil || code != 7 {
		t.Fatalf("launch with the gate's tmpfs: code=%d err=%v", code, err)
	}
	if got, want := d.containers[f.ref("agent").Name].Tmpfs, map[string]string{GateTmpPath: "rw,exec,nosuid,nodev,size=1g"}; !maps.Equal(got, want) {
		t.Fatalf("the agent's tmpfs mounts = %v, want %v", got, want)
	}
	if gone, err := f.cleanup("exited"); err != nil || !gone {
		t.Fatal("cleanup", gone, err)
	}
}

func TestFilteredLaunchRevalidatesMountsBeforeCreatingAgent(t *testing.T) {
	f, d := filteredFixture(t)
	want := errors.New("mount authority changed before agent creation")
	calls := 0
	f.mountRevalidate = func() error {
		calls++
		return want
	}
	_, err := f.launch(context.Background(), RunSpec{Repo: f.record.Project}, nil, nil, io.Discard, io.Discard)
	if !errors.Is(err, want) || calls != 1 {
		t.Fatalf("launch error = %v with %d revalidations; want %v exactly once", err, calls, want)
	}
	if slices.Contains(d.log, "create:agent") {
		t.Fatalf("agent was created after mount revalidation failed: %v", d.log)
	}
	if gone, cleanupErr := f.cleanup("launch_failed"); cleanupErr != nil || !gone {
		t.Fatalf("cleanup after revalidation failure = gone %v, err %v", gone, cleanupErr)
	}
}

// A first approval takes the launch lock exclusively, so it waits for every box whose agent could
// replace project paths. A box that holds prompts is not one: it sees the project read-only, and it
// may be waiting for that very approval, which would otherwise never come.
func TestFilteredLaunchKeepsTheMountWindowOnlyForABoxThatCanWrite(t *testing.T) {
	for name, handshake := range map[string]bool{"a real box": false, "a box that holds prompts": true} {
		t.Run(name, func(t *testing.T) {
			f, d := filteredFixture(t)
			d.holdAgent = true
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := f.launch(ctx, RunSpec{Repo: f.record.Project, HandshakeOnly: handshake}, nil, nil, io.Discard, io.Discard)
				done <- err
			}()
			select {
			case <-d.attached:
			case err := <-done:
				t.Fatalf("the launch ended before its box ran: %v", err)
			}
			try, giveUp := context.WithTimeout(ctx, 200*time.Millisecond)
			defer giveUp()
			unlock, err := forkspace.LockServiceLaunch(try, f.record.Project, true)
			if err == nil {
				unlock()
			}
			if (err == nil) != handshake {
				t.Fatalf("a first approval getting past the running box: %v, want %v", err == nil, handshake)
			}
			cancel()
			<-done
		})
	}
}

// A step that fails beside another does not strand it: the sibling's request settles, the launch
// stops there, and cleanup removes everything either one created — with no failure it did not have.
func TestFilteredLaunchStepFailureSettlesItsSibling(t *testing.T) {
	for _, test := range []struct {
		name, notAttempted, failStart string
		pair                          [2]string
		want                          string
	}{
		{"one volume never reaches the daemon", "ipc", "", [2]string{"create:ipc", "create:observations"}, runtime.ErrDockerCreateNotAttempted.Error()},
		{"the controller does not start", "", "controller", [2]string{"start:controller", "create:guard"}, "fixture start refused"},
		{"the guard is never created", "guard", "", [2]string{"start:controller", "create:guard"}, runtime.ErrDockerCreateNotAttempted.Error()},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, d := filteredFixture(t)
			d.notAttempted, d.failStart = test.notAttempted, test.failStart
			d.beforeCall = rendezvous(t, test.pair)
			_, err := f.launch(context.Background(), RunSpec{}, nil, nil, io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("launch error = %v, want %q", err, test.want)
			}
			if slices.Contains(d.log, "start:guard") || slices.Contains(d.log, "create:agent") {
				t.Fatalf("the launch went on past a failed step: %v", d.log)
			}
			gone, err := f.cleanup("launch_failed")
			if err != nil || !gone {
				t.Fatal("cleanup after one failed step", gone, err)
			}
			if slices.Contains(d.log, "capture-final") {
				t.Fatal("cleanup asked a guard that never ran for its final observation")
			}
			d.mu.Lock()
			defer d.mu.Unlock()
			if len(d.containers) != 0 || len(d.volumes) != 0 {
				t.Fatalf("cleanup left %v and %v", slices.Collect(maps.Keys(d.containers)), slices.Collect(maps.Keys(d.volumes)))
			}
			record, err := f.store.Execution(f.record.ID)
			if err != nil || record.Receipt == nil || record.Receipt.Cleanup != "complete" {
				t.Fatal("the receipt did not record complete cleanup", err)
			}
		})
	}
}

// An interrupt that lands while two steps are in flight stops the launch once: both requests settle,
// the error names the cancellation a single time — keeping any real fault beside it — and nothing
// either one created survives cleanup.
func TestFilteredLaunchCancelledMidPairDrainsBoth(t *testing.T) {
	for _, test := range []struct {
		name         string
		pair         [2]string
		notAttempted string
	}{
		{"volumes", [2]string{"create:ipc", "create:observations"}, ""},
		{"controller start and guard creation", [2]string{"start:controller", "create:guard"}, ""},
		{"a real fault beside the interrupt", [2]string{"create:ipc", "create:observations"}, "observations"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, d := filteredFixture(t)
			d.notAttempted = test.notAttempted
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			meet := rendezvous(t, test.pair)
			d.beforeCall = func(op string) {
				meet(op)
				if op == test.pair[0] {
					cancel() // both calls are in flight now
				}
			}
			_, err := f.launch(ctx, RunSpec{}, nil, nil, io.Discard, io.Discard)
			if !errors.Is(err, context.Canceled) || strings.Count(err.Error(), context.Canceled.Error()) != 1 {
				t.Fatalf("launch error = %q, want the cancellation named once", err)
			}
			if test.notAttempted != "" && !errors.Is(err, runtime.ErrDockerCreateNotAttempted) {
				t.Fatalf("launch error = %q, want the fault beside the interrupt kept", err)
			}
			if slices.Contains(d.log, "start:guard") || slices.Contains(d.log, "create:agent") {
				t.Fatalf("a cancelled launch went on: %v", d.log)
			}
			if gone, err := f.cleanup("cancelled"); err != nil || !gone {
				t.Fatal("cleanup after cancellation", gone, err)
			}
			d.mu.Lock()
			defer d.mu.Unlock()
			if len(d.containers) != 0 || len(d.volumes) != 0 {
				t.Fatalf("cancellation leaked %v and %v", slices.Collect(maps.Keys(d.containers)), slices.Collect(maps.Keys(d.volumes)))
			}
		})
	}
}

// A person who stops the start, at any step of it, ends it where it stands: the launch ends before any
// main process, cleanup removes every container and volume it made, and the receipt says cancelled.
// The person is told once that the box never started, with none of what the stopped calls said, and
// coop exits 130. A Ctrl-C typed while Docker's client holds the terminal never reaches coop as a
// signal; the daemon's record of it, a start that nothing survived, ends the same way.
func TestStoppingTheStartAtAnyStepCleansUpAndExits130(t *testing.T) {
	type stop struct {
		name, op  string
		interrupt *hostInterrupt // the signal coop heard; nil for a Ctrl-C only the box heard
		want      error
	}
	var stops []stop
	for _, op := range []string{"create:ipc", "create:controller", "start:controller", "create:guard", "start:guard", "create:agent", "start:agent"} {
		stops = append(stops, stop{"Ctrl-C at " + op, op, &hostInterrupt{sig: os.Interrupt}, context.Canceled})
	}
	stops = append(stops, stop{"Ctrl-C held by Docker's client", "", nil, runtime.ErrDockerStartInterrupted})
	for _, test := range stops {
		t.Run(test.name, func(t *testing.T) {
			f, d := filteredFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var once sync.Once
			if test.interrupt == nil {
				d.ctrlCAtStart = true
			} else {
				d.beforeCall = func(op string) {
					if op == test.op {
						once.Do(cancel) // the signal lands while this step is in flight
					}
				}
			}
			code, err := f.launch(ctx, RunSpec{}, nil, nil, io.Discard, io.Discard)
			if !errors.Is(err, test.want) || f.started() {
				t.Fatalf("launch = (%d, %v) with the main process started=%v, want %v and no main process", code, err, f.started(), test.want)
			}
			if gone, cleanupErr := f.cleanup(f.workloadOutcome(code, err, ctx.Err() != nil)); cleanupErr != nil || !gone {
				t.Fatal("cleanup after the stop", gone, cleanupErr)
			}
			d.mu.Lock()
			left := slices.Concat(slices.Collect(maps.Keys(d.containers)), slices.Collect(maps.Keys(d.volumes)))
			d.mu.Unlock()
			if len(left) != 0 {
				t.Fatalf("the stop left %v behind", left)
			}
			record, recordErr := f.store.Execution(f.record.ID)
			if recordErr != nil || record.Receipt == nil || record.Receipt.Workload != "cancelled" || record.Receipt.Cleanup != "complete" {
				t.Fatalf("the receipt = %+v, %v; want a cancelled run with its cleanup complete", record.Receipt, recordErr)
			}
			var status int
			var shown error
			output := captureStderr(t, func() {
				status, shown = newLaunchSections(RunSpec{Cmd: []string{"sleep"}}).ended(f.started(), test.interrupt, nil, code, err)
			})
			if want := "\nStopped before the box started.\n"; output != want {
				t.Fatalf("the person was told %q, want exactly %q", output, want)
			}
			if status != 130 || !errors.Is(shown, ui.ErrReported) || !errors.Is(shown, err) {
				t.Fatalf("exit status %d, error %v; want 130 and the stop marked reported", status, shown)
			}
		})
	}
}

// The stops above land on a daemon whose create always answers. Docker's client does not: a Ctrl-C
// that killed it mid-create left Docker finishing the create with nobody told, so a stopped launch
// ended "network container creation outcome is unknown" and its run stayed "cleanup pending" until
// coop net recover. Against the real runtime and a client that takes its time, the create is left to
// answer: the launch stops there knowing the container by its ID, and removing it takes its exact name
// and nothing beside it.
func TestStopWhileDockerCreatesAGatewayContainerKnowsAndRemovesExactlyThatContainer(t *testing.T) {
	f, _ := filteredFixture(t)
	d := newFilesystemRecoveryDaemon(t)
	d.image = f.record.GatewayImage
	mine := f.createRef("controller")
	sum := sha256.Sum256([]byte(mine.Name))
	id := hex.EncodeToString(sum[:]) // what filesystemRecoveryDaemon.add calls it
	// Another run's controller: the same role under another name and run.
	other := runtime.DockerRef{Name: "coop-net-0000000000000000-controller", Labels: map[string]string{LabelNetworkRun: "0000000000000000", "coop.network.role": "controller"}}
	d.add(other)
	creating := filepath.Join(t.TempDir(), "creating")
	shim := filepath.Join(t.TempDir(), "docker")
	// The client says it is running, takes its time, then answers with the container's ID.
	script := "#!/bin/sh\nfor arg; do [ \"$arg\" = create ] && { : > " + creating + "; sleep 0.5; echo " + id + "; }; done\nexit 0\n"
	if err := os.WriteFile(shim, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	docker, err := runtime.BindDockerForSetup(t.Context(), runtime.Runtime{Name: shim}, d.endpoint, "fixture-daemon")
	if err != nil {
		t.Fatal(err)
	}
	defer docker.Close()
	f.docker = docker

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	created := make(chan error, 1)
	go func() {
		created <- f.createContainer(ctx, "controller", f.record.GatewayImage, f.helperOptions("controller"), []string{"controller"})
	}()
	wait.ForFile(t, creating) // Docker's client is creating the container
	cancel()                  // Ctrl-C
	d.add(mine)               // Docker finishes the create
	// Nothing else: not the unknown outcome, and not a fault of the container the stop cut short.
	if err := <-created; err == nil || err.Error() != context.Canceled.Error() {
		t.Fatalf("the stopped create = %v; want only the cancellation", err)
	}
	if got := f.ref("controller").ID; got != id || f.createUnfinished("controller") {
		t.Fatalf("the launch knows the controller as %q (create unfinished: %v); want its ID %q", got, f.createUnfinished("controller"), id)
	}
	evidence, err := networkstate.OpenEvidence(f.store.Path(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer evidence.Close()
	if err := f.removeResource(evidence, "controller"); err != nil {
		t.Fatalf("removing the controller = %v", err)
	}
	if d.has(mine.Name) || !d.has(other.Name) {
		t.Fatalf("after the removal this run's controller is still there: %v, another run's is: %v; want it gone and the other kept", d.has(mine.Name), d.has(other.Name))
	}
	record, err := f.store.Execution(f.record.ID)
	if state := recoveredResource(record, "controller").State; err != nil || state != "gone" {
		t.Fatalf("the record calls the controller %q (%v); want gone", state, err)
	}
}

// A step that panics does not end the process around a half-built gateway: the panic waits for its
// sibling to settle, then rises on the launch's own goroutine — through the teardown its caller
// deferred — with the stack of where it happened.
func TestFilteredLaunchStepPanicReachesTheCallersTeardown(t *testing.T) {
	f, d := filteredFixture(t)
	d.beforeCall = func(op string) {
		if op == "start:controller" {
			panic("fixture step panicked")
		}
	}
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_, _ = f.launch(context.Background(), RunSpec{}, nil, nil, io.Discard, io.Discard)
	}()
	if message := fmt.Sprint(recovered); !strings.Contains(message, "fixture step panicked") || !strings.Contains(message, "goroutine") {
		t.Fatalf("the step's panic did not reach the launch with its stack: %v", recovered)
	}
	if f.ref("guard").ID == "" {
		t.Fatal("the panic rose before its sibling settled: the guard was not created")
	}
	if gone, err := f.cleanup("launch_failed"); err != nil || !gone {
		t.Fatal("teardown after a step panic", gone, err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.containers) != 0 || len(d.volumes) != 0 {
		t.Fatalf("teardown after a panic left %v and %v", slices.Collect(maps.Keys(d.containers)), slices.Collect(maps.Keys(d.volumes)))
	}
}

// Overlapping the volumes does not move them ahead of their own authority: when the registry refuses
// the intent, neither step reaches the daemon.
func TestFilteredLaunchRefusedIntentAllocatesNothing(t *testing.T) {
	f, d := filteredFixture(t)
	if err := os.Chmod(f.store.Path(), 0o755); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(f.store.Path(), 0o700)
	_, err := f.launch(context.Background(), RunSpec{}, nil, nil, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("a launch whose intent was refused went ahead")
	}
	// Both steps were refused for the one reason, and the launch says it once.
	if lines := strings.Split(err.Error(), "\n"); len(lines) != 1 {
		t.Fatalf("one refusal was reported %d times: %q", len(lines), err)
	}
	for _, entry := range d.log {
		if strings.HasPrefix(entry, "create:") {
			t.Fatalf("a refused intent reached the daemon: %v", d.log)
		}
	}
}

func TestFilteredCleanupRetainsUnknownCreateAndSurvivingAgent(t *testing.T) {
	for _, phase := range []string{"unknown-removal"} {
		t.Run(phase, func(t *testing.T) {
			f, d := filteredFixture(t)
			d.refuseRemoval = "agent"
			_, _ = f.launch(context.Background(), RunSpec{}, nil, nil, io.Discard, io.Discard)
			gone, err := f.cleanup("runtime_failed")
			if gone || err == nil {
				t.Fatal("unknown workload removal reported clean")
			}
			r, err := f.store.Execution(f.record.ID)
			if err != nil || r.Receipt == nil || r.Receipt.Completeness != "partial" || r.Receipt.Cleanup != "pending" || r.Snapshot.Terminal {
				t.Fatal("surviving workload got a terminal complete receipt", err)
			}
			if _, err := os.Stat(f.runfiles); err != nil {
				t.Fatal("possible consumer lost generated files", err)
			}
			if slices.Contains(d.log, "capture-final") {
				t.Fatal("agent-unknown collector terminal was treated as workload end")
			}
		})
	}
}

func TestFilteredHealthLossCancelsAttachmentAndRemovesAgent(t *testing.T) {
	f, d := filteredFixture(t)
	d.holdAgent = true
	d.brokenProbe = true
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := f.launch(ctx, RunSpec{}, nil, nil, io.Discard, io.Discard)
	if err == nil || ctx.Err() != nil {
		t.Fatal("health loss did not stop attachment", err)
	}
	// The loop shows an attempt's first line. It once read only "context canceled", and later ended on
	// Docker's own words for a busy host ("Docker evidence source unavailable"). The first line now
	// says what failed and the likely reason; the failed check's own error is kept on the next.
	first, rest, _ := strings.Cut(err.Error(), "\n")
	want := fmt.Sprintf("the network gateway stopped answering %d checks in a row", gatewayProbeFailureLimit)
	if !strings.HasPrefix(first, want) || !strings.Contains(first, "may be busy") || strings.Contains(first, "evidence source") || strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("health loss did not lead with its cause and the busy host: %q", err)
	}
	if !strings.Contains(rest, "fixture guard unhealthy") {
		t.Fatalf("the failed check's own error was dropped: %q", err)
	}
	if gone, err := f.cleanup("runtime_failed"); err != nil || !gone {
		t.Fatal("health cleanup", gone, err)
	}
}

func TestFilteredCreatedSecurityMismatchNeverStartsAgent(t *testing.T) {
	f, d := filteredFixture(t)
	d.corruptRole = "agent"
	_, err := f.launch(context.Background(), RunSpec{}, nil, nil, io.Discard, io.Discard)
	if err == nil || slices.Contains(d.log, "start:agent") {
		t.Fatal("security mismatch started workload")
	}
	if gone, err := f.cleanup("launch_failed"); err != nil || !gone {
		t.Fatal("mismatch cleanup", gone, err)
	}
}

func TestFilteredNativeLinuxUserRejectsAnotherContainerIdentity(t *testing.T) {
	f, d := filteredFixture(t)
	f.agentIdentity = agentIdentity{1001, 1002}
	d.corruptRole = "agent-wrong-user"
	_, err := f.launch(context.Background(), RunSpec{}, nil, nil, io.Discard, io.Discard)
	if err == nil || slices.Contains(d.log, "start:agent") {
		t.Fatal("UID-1000 container started for UID-1001 host user", err)
	}
	if gone, err := f.cleanup("launch_failed"); err != nil || !gone {
		t.Fatal("mismatch cleanup", gone, err)
	}
}

func TestFilteredMountsRequireExactGeneratedSources(t *testing.T) {
	f, _ := filteredFixture(t)
	file, err := writeTempFile(f.runfiles, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name        string
		args, files []string
		valid       bool
	}{
		{"generated", []string{"-v", file + ":/generated:ro"}, []string{file}, true},
		{"unregistered", []string{"-v", file + ":/generated:ro"}, nil, false},
		{"authority", []string{"-v", f.store.Path() + ":/authority"}, nil, false},
		{"root", []string{"-v", f.runfiles + ":/runfiles"}, []string{f.runfiles}, false},
		{"root-user", []string{"--user", "0"}, nil, false},
		{"host-network", []string{"--network", "host"}, nil, false},
		{"alternate-mount", []string{"--mount", "type=bind,src=/,dst=/host"}, nil, false},
		{"relax-seccomp", []string{"--security-opt", "seccomp=unconfined"}, nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := f.validateMounts(test.args, test.files, nil); (err == nil) != test.valid {
				t.Fatal("mount decision", err)
			}
		})
	}
	alias := filepath.Join(t.TempDir(), "transcripts")
	if err := os.Symlink(f.store.Path(), alias); err != nil {
		t.Fatal(err)
	}
	if err := f.validateMounts([]string{"-v", alias + ":/transcripts"}, nil, nil); err == nil {
		t.Fatal("ACP alias exposed authority")
	}
}

// A filtered workload may carry one tmpfs, the gate's temp folder, and exactly the size coop plans
// for the memory limit it runs with. A larger size, a second tmpfs, another path or other options
// are refused, wherever the --memory sits among the options.
func TestFilteredMountsTakeOnlyThePlannedGateTmpfs(t *testing.T) {
	f, _ := filteredFixture(t)
	planned, noLimit := gateTmpfs("4g"), gateTmpfs("")
	for _, test := range []struct {
		name  string
		args  []string
		valid bool
	}{
		{"no tmpfs", []string{"--memory", "4g"}, true},
		{"planned for the limit", []string{"--memory", "4g", "--tmpfs", planned}, true},
		{"planned for the limit, memory last", []string{"--tmpfs", planned, "--memory", "4g"}, true},
		{"planned for no limit", []string{"--tmpfs", noLimit}, true},
		{"far larger than planned", []string{"--memory", "4g", "--tmpfs", GateTmpPath + ":rw,exec,nosuid,nodev,size=99999g"}, false},
		{"far larger, no limit", []string{"--tmpfs", GateTmpPath + ":rw,exec,nosuid,nodev,size=99999g"}, false},
		{"planned for no limit, in a limited box", []string{"--memory", "4g", "--tmpfs", noLimit}, false},
		{"planned for a limit, in a box with none", []string{"--tmpfs", planned}, false},
		{"twice", []string{"--memory", "4g", "--tmpfs", planned, "--tmpfs", planned}, false},
		{"twice, no limit", []string{"--tmpfs", noLimit, "--tmpfs", noLimit}, false},
		{"another path", []string{"--tmpfs", "/home/node:rw,exec,size=1g"}, false},
		{"wider options", []string{"--tmpfs", GateTmpPath + ":rw,exec,suid,nodev,size=2g"}, false},
		{"no value", []string{"--tmpfs"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := f.validateMounts(test.args, nil, nil); (err == nil) != test.valid {
				t.Fatal("tmpfs decision", err)
			}
		})
	}
}

func TestFilteredPublicLaunchRequiresCaptureAndRejectsExtraArgs(t *testing.T) {
	for _, test := range []struct {
		mode    string
		capture *CapturedEgress
		extra   []string
	}{
		{"filtered", nil, nil}, {"open", &CapturedEgress{}, []string{"--network", "host"}},
	} {
		cfg := &config.Config{Egress: test.mode, ExtraRunArgs: test.extra}
		_, err := Run(cfg, runtime.Runtime{Name: "must-not-execute"}, RunSpec{Repo: t.TempDir(), CapturedEgress: test.capture})
		if err == nil || !strings.Contains(err.Error(), "network") {
			t.Fatal("missing authority reached runtime", err)
		}
	}
}

func TestProjectFilteredLaunchRequiresCaptureBeforeRuntime(t *testing.T) {
	repo := t.TempDir()
	writeCopyFixture(t, filepath.Join(repo, ".agent", "project.yaml"), "box:\n  egress: filtered\n")
	recorder := filepath.Join(t.TempDir(), "runtime.log")
	_, err := Run(&config.Config{Egress: "open"}, recorderRuntime(t, recorder), RunSpec{Repo: repo})
	if err == nil || !strings.Contains(err.Error(), "host policy capture") {
		t.Fatalf("project-filtered launch without capture = %v", err)
	}
	if _, statErr := os.Stat(recorder); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("missing admission reached runtime: %v", statErr)
	}
}

func TestFinalObservationArchiveRejectsUnexpectedEntries(t *testing.T) {
	for _, name := range []string{"../final.json", "nested/final.json", "other.json"} {
		var buffer bytes.Buffer
		writer := tar.NewWriter(&buffer)
		_ = writer.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: 1})
		_, _ = fmt.Fprint(writer, "x")
		_ = writer.Close()
		if _, err := finalObservationFile(buffer.Bytes()); err == nil {
			t.Fatal("untrusted archive path accepted", name)
		}
	}
}

func TestFilteredResolverReadinessIsRequired(t *testing.T) {
	for _, status := range []string{"", "unknown", "degraded"} {
		t.Run(status, func(t *testing.T) {
			f, d := filteredFixture(t)
			d.resolverStatus = &status
			if err := f.ready(context.Background()); err == nil {
				t.Fatal("unready resolver admitted")
			}
			if f.record.Snapshot.Sequence != 0 {
				t.Fatal("unready snapshot accepted as readiness")
			}
		})
	}
}

func TestFilteredDaemonMountMismatchNeverStartsGuard(t *testing.T) {
	f, d := filteredFixture(t)
	d.corruptMount = "guard"
	_, err := f.launch(context.Background(), RunSpec{}, nil, nil, io.Discard, io.Discard)
	if err == nil || slices.Contains(d.log, "start:guard") || slices.Contains(d.log, "create:agent") {
		t.Fatal("changed daemon mount escaped preflight", err)
	}
	if gone, err := f.cleanup("launch_failed"); err != nil || !gone {
		t.Fatal("mount failure cleanup", gone, err)
	}
}

func TestFilteredMissingFinalEvidenceIsPartialButCleanupContinues(t *testing.T) {
	f, d := filteredFixture(t)
	d.malformedFinal = true
	if _, err := f.launch(context.Background(), RunSpec{}, nil, nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if gone, err := f.cleanup("exited"); err == nil || !gone {
		t.Fatal("missing evidence was hidden or blocked workload cleanup", gone, err)
	}
	r, err := f.store.Execution(f.record.ID)
	if err != nil || r.Receipt == nil || r.Receipt.Completeness != "partial" || r.Receipt.Cleanup != "complete" {
		t.Fatal("missing final evidence lied about completeness", err)
	}
}

// A Docker too slow to hand over the final record still gets the run sealed as partial and every
// resource removed, and the person reads one plain sentence that says what failed and what is kept,
// not Docker's own "Docker archive read failed" and "context deadline exceeded".
func TestFilteredSlowFinalEvidenceReadsAsOnePlainLine(t *testing.T) {
	f, d := filteredFixture(t)
	d.slowFinal = true
	if _, err := f.launch(context.Background(), RunSpec{}, nil, nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	gone, err := f.cleanup("exited")
	if err == nil || !gone {
		t.Fatalf("slow final evidence was hidden or blocked cleanup: gone %v, err %v", gone, err)
	}
	if !errors.Is(err, errFinalRecordSlow) {
		t.Fatalf("the slow read is reported as %q, want the plain sentence", err)
	}
	text := err.Error()
	for _, raw := range []string{"archive read failed", "context deadline exceeded"} {
		if strings.Contains(text, raw) {
			t.Errorf("the error still carries Docker's %q:\n%s", raw, text)
		}
	}
	if strings.Contains(strings.TrimSpace(text), "\n") {
		t.Errorf("the error is more than one line:\n%s", text)
	}
	for _, want := range []string{"too slow", "final network record", "your work is kept", "less busy"} {
		if !strings.Contains(text, want) {
			t.Errorf("the error does not say %q:\n%s", want, text)
		}
	}
	r, rerr := f.store.Execution(f.record.ID)
	if rerr != nil || r.Receipt == nil || r.Receipt.Completeness != "partial" || r.Receipt.Cleanup != "complete" {
		t.Fatal("a slow final read was not sealed as partial with its cleanup complete", rerr)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.containers) != 0 || len(d.volumes) != 0 {
		t.Fatalf("cleanup stopped at the slow read: %d container(s) and %d volume(s) remain", len(d.containers), len(d.volumes))
	}
}

func TestFilteredLostHostStorageStillContainsDaemonResources(t *testing.T) {
	f, d := filteredFixture(t)
	if _, err := f.launch(context.Background(), RunSpec{}, nil, nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f.store.Path(), 0755); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(f.store.Path(), 0700)
	if gone, err := f.cleanup("runtime_failed"); err == nil || gone {
		t.Fatal("lost custody claimed complete", gone, err)
	}
	for _, role := range []string{"agent", "guard", "controller"} {
		if !slices.Contains(d.log, "remove:"+role) {
			t.Fatal("host storage failure skipped containment", role)
		}
	}
	if _, err := os.Stat(f.runfiles); err != nil {
		t.Fatal("unconfirmed custody lost files", err)
	}
}

func TestFilteredTerminalCollectorCannotCertifySurvivingWorkload(t *testing.T) {
	f, d := filteredFixture(t)
	snapshot, err := f.readObservation(d.observation(true))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.accept(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	r, err := f.store.SealExecution(context.Background(), f.record.ID, f.record.Revision, "runtime_failed")
	if err != nil || r.Receipt == nil || r.Receipt.Completeness != "partial" {
		t.Fatal("collector certified an unconfirmed workload", err)
	}
}

func TestFilteredMountsKeepReadOnlyRepositoryAndWritableOutput(t *testing.T) {
	options := []string{"-v", "/host/repo:/workspace:ro", "-v", "/host/output:/workspace/.coop-output"}
	actual := []runtime.DockerMount{
		{Type: "bind", Source: "/host/repo", Destination: "/workspace", RW: false},
		{Type: "bind", Source: "/host/output", Destination: "/workspace/.coop-output", RW: true},
	}
	if err := verifyNetworkMounts(actual, nil, options); err != nil {
		t.Fatal("canonical session output mount was refused", err)
	}
	actual[1].RW = false
	if err := verifyNetworkMounts(actual, nil, options); err == nil {
		t.Fatal("read-only output accepted instead of writable output")
	}
	actual[1].RW, actual[0].RW = true, true
	if err := verifyNetworkMounts(actual, nil, options); err == nil {
		t.Fatal("writable repository accepted instead of read-only repository")
	}
}

func TestFilteredMountsVerifyTmpfsConstraintsAndRejectAnonymousVolumes(t *testing.T) {
	options := []string{"--tmpfs", "/private:rw,uid=65532,mode=0700"}
	if err := verifyNetworkMounts(nil, map[string]string{"/private": "rw,uid=65532,mode=0700"}, options); err != nil {
		t.Fatal(err)
	}
	if err := verifyNetworkMounts(nil, map[string]string{"/private": "rw,mode=0777"}, options); err == nil {
		t.Fatal("widened tmpfs accepted")
	}
	if err := verifyNetworkMounts([]runtime.DockerMount{{Type: "volume", Name: "anonymous", Destination: "/unexpected", RW: true}}, nil, nil); err == nil {
		t.Fatal("implicit image volume accepted")
	}
}

func TestFilteredNestedMutableBindSourcesAreRefused(t *testing.T) {
	for _, scenario := range []string{"overlay", "fork-companion"} {
		t.Run(scenario, func(t *testing.T) {
			f, _ := filteredFixture(t)
			nested := filepath.Join(f.record.Project, "component")
			if err := os.Mkdir(nested, 0700); err != nil {
				t.Fatal(err)
			}
			primary := f.record.Project
			if scenario == "fork-companion" {
				primary = t.TempDir()
			}
			options := []string{"-v", primary + ":/workspace", "-v", nested + ":/component:ro"}
			if err := f.validateMounts(options, nil, nil); err == nil {
				t.Fatal("agent-writable bind parent accepted")
			}
		})
	}
}

func TestFilteredDefaultCredentialHomeIsNotMistakenForWritableParent(t *testing.T) {
	f, _ := filteredFixture(t)
	boxHome, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{BoxHome: boxHome, ConfigDir: filepath.Join(boxHome, "agents")}
	spec := RunSpec{Repo: f.record.Project, Agent: "gemini", Homes: true}
	profile := filepath.Join(cfg.ConfigDir, "gemini", "native-homes", "default", "repo", "home")
	cfg = cfg.WithNativeHomes(map[string]string{"gemini": profile})
	child := filepath.Join(profile, "agent-owned")
	if err := os.MkdirAll(child, 0o700); err != nil {
		t.Fatal(err)
	}
	f.unsafeRoots = filteredWritableRoots(cfg, spec)
	f.authorityConfig, f.authoritySpec = cfg, spec
	primary := []string{"-v", f.record.Project + ":/workspace", "-v", profile + ":/home/node/.gemini"}
	if err := f.validateMounts(primary, nil, nil); err != nil {
		t.Fatal("selected credential home refused:", err)
	}
	nested := append(append([]string{}, primary...), "-v", child+":/agent-owned:ro")
	if err := f.validateMounts(nested, nil, nil); err == nil {
		t.Fatal("bind beneath agent-writable credential home accepted")
	}
}

func TestFilteredBindIdentityCannotBeRedefinedByReplacement(t *testing.T) {
	f, _ := filteredFixture(t)
	source, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	options := []string{"-v", source + ":/independent:ro"}
	if err := f.validateMounts(options, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(source, source+".held"); err != nil {
		t.Fatal(err)
	}
	defer os.Rename(source+".held", source)
	if err := os.Symlink(f.store.Path(), source); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(source)
	if err := f.checkBindings(); err == nil {
		t.Fatal("replacement redefined the expected source identity")
	}
}

func TestFilteredFrozenAliasDoesNotFollowLaterRetarget(t *testing.T) {
	f, _ := filteredFixture(t)
	source, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(source, alias); err != nil {
		t.Fatal(err)
	}
	options := []string{"-v", alias + ":/independent:ro"}
	if err := f.validateMounts(options, nil, nil); err != nil {
		t.Fatal(err)
	}
	if options[1] != source+":/independent:ro" {
		t.Fatal("mutable alias was emitted to Docker")
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.store.Path(), alias); err != nil {
		t.Fatal(err)
	}
	if err := f.checkBindings(); err != nil {
		t.Fatal("retargeted unused spelling changed frozen source", err)
	}
}

func TestFilteredEarlierCollectorEndIsNotFullWorkloadCoverage(t *testing.T) {
	f, d := filteredFixture(t)
	d.earlyGuardEnd = true
	if _, err := f.launch(context.Background(), RunSpec{}, nil, nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if gone, err := f.cleanup("exited"); err != nil || !gone {
		t.Fatal("early collector cleanup", gone, err)
	}
	r, err := f.store.Execution(f.record.ID)
	if err != nil || r.Receipt == nil || r.Receipt.Completeness != "partial" || r.ObserverAfterWorkload || !slices.Contains(r.Receipt.Snapshot.Loss.Reasons, "observer_end_after_workload_unproven") {
		t.Fatal("earlier collector exit certified workload coverage", err)
	}
}

func TestFilteredStartupPersistenceFailureIsRuntimeFailure(t *testing.T) {
	f, d := filteredFixture(t)
	d.failStartupPersistence = true
	code, err := f.launch(context.Background(), RunSpec{}, nil, nil, io.Discard, io.Discard)
	if err == nil || f.workloadOutcome(code, err, false) != "runtime_failed" {
		t.Fatal("failed startup publication reclassified the runtime", code, err)
	}
	if gone, err := f.cleanup("runtime_failed"); err != nil || !gone {
		t.Fatal("startup failure cleanup", gone, err)
	}
}

// A bind source a filtered box cannot take is refused, by its path, before Docker does any work for
// the run. The QA run saw a missing COOP_RUN_ARGS source refused only at launch, after the gateway
// setup, as "network workload source cannot be frozen unambiguously", naming no path.
func TestFilteredLaunchRefusesABadBindSourceBeforeDocker(t *testing.T) {
	f, d := filteredFixture(t)
	missing := filepath.Join(t.TempDir(), "nope")
	capture := &CapturedEgress{Store: f.store, Project: f.record.Project, Fingerprint: f.policy.Fingerprint}
	recorder := filepath.Join(t.TempDir(), "runtime.log")
	spec := RunSpec{Repo: f.record.Project, Cmd: []string{"true"}, ExtraArgs: []string{"-e", "A=/b:/c", "-v", "data:/data", "-v", missing + ":/x"}}
	_, err := prepareFilteredExecution(t.Context(), &config.Config{Egress: "filtered", ConfigDir: t.TempDir()}, recorderRuntime(t, recorder),
		spec, capture, "", &networkSmokeLaunch{authority: d.smoke}, nil)
	if want := "a filtered box cannot mount " + missing + ": it does not exist on this machine — create it, or remove it from COOP_RUN_ARGS"; err == nil || err.Error() != want {
		t.Fatalf("prepare = %v, want %q", err, want)
	}
	if _, err := os.Stat(recorder); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the refused launch reached the runtime")
	}
	// A mount with no target names a path in the box, not a host folder: the mount plan refuses it
	// as ambiguous, so this check leaves it alone rather than call it missing.
	if err := checkBindSources([]string{"-v", missing}, ""); err != nil {
		t.Errorf("a mount with no target was checked as a host folder: %v", err)
	}
}

// COOP_NO_NEW_PRIVILEGES=0 cannot apply to a filtered box, which always keeps the option on, so a
// filtered launch says so once instead of dropping the setting without a word. Open and offline
// boxes honor it, and the default says nothing.
func TestFilteredLaunchSaysItKeepsNoNewPrivileges(t *testing.T) {
	const note = "Filtered boxes always keep no-new-privileges on; COOP_NO_NEW_PRIVILEGES=0 applies only with --egress open or offline.\n"
	filtered := RunSpec{CapturedEgress: &CapturedEgress{}}
	for _, test := range []struct {
		name, value string
		spec        RunSpec
		want        string
	}{
		{"filtered, turned off", "0", filtered, note},
		{"filtered, default", "", filtered, ""},
		{"filtered, turned on", "1", filtered, ""},
		{"filtered, quiet", "0", RunSpec{CapturedEgress: &CapturedEgress{}, Quiet: true}, ""},
		{"open or offline", "0", RunSpec{}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.value != "" {
				t.Setenv("COOP_NO_NEW_PRIVILEGES", test.value)
			}
			cfg, err := config.Load()
			if err != nil {
				t.Fatal(err)
			}
			if got := captureStderr(t, func() { noticeKeptNoNewPrivileges(cfg, test.spec) }); got != test.want {
				t.Errorf("note = %q, want %q", got, test.want)
			}
		})
	}
}

// The gateway filters PACKETS. A bind of the runtime's own control surface
// hands the agent a way to start a sibling container that never meets the
// gateway at all, so those sources are refused by name before anything runs.
// A control path this host lacks, as a Mac lacks /proc, /run and /sys, still
// exists where Docker runs, so it is refused as one; any other missing source
// is named as missing. Both checks say so: the one before Docker does any work
// (checkBindSources) and the one on the frozen mount plan.
func TestFilteredMountsRefuseTheRuntimeControlSurface(t *testing.T) {
	f, _ := filteredFixture(t)
	f.record.Endpoint = "unix:///fixture/run/docker.sock"
	ordinary := t.TempDir()
	missing := filepath.Join(ordinary, "nope")
	const control = "reaches Docker or the kernel"
	for _, test := range []struct {
		name, source, want string
	}{
		{"docker socket directory", "/var/run", control},
		{"run", "/run", control},
		{"proc", "/proc", control},
		{"sys", "/sys", control},
		{"dev", "/dev", control},
		{"root", "/", control},
		{"missing source", missing, "a filtered box cannot mount " + missing + ": it does not exist on this machine"},
		{"ordinary data directory", ordinary, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			mount := []string{"-v", test.source + ":/x:ro"}
			for check, err := range map[string]error{
				"before Docker": checkBindSources(mount, f.record.Endpoint),
				"mount plan":    f.validateMounts(mount, nil, nil),
			} {
				if test.want == "" {
					if err != nil {
						t.Fatalf("%s: an ordinary directory was refused: %v", check, err)
					}
					continue
				}
				if err == nil {
					t.Fatalf("%s: %s was mounted into a filtered box", check, test.source)
				}
				if !strings.Contains(err.Error(), "a filtered box cannot mount ") || !strings.Contains(err.Error(), test.want) {
					t.Fatalf("%s: the refusal does not name the mount and the reason: %v", check, err)
				}
			}
		})
	}
	// The bound endpoint's own socket path is protected wherever it lives.
	socket := filepath.Join(t.TempDir(), "orbstack", "docker.sock")
	if err := os.MkdirAll(filepath.Dir(socket), 0o755); err != nil {
		t.Fatal(err)
	}
	f.record.Endpoint = "unix://" + socket
	real, err := filepath.EvalSymlinks(filepath.Dir(socket))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.validateMounts([]string{"-v", real + ":/x:ro"}, nil, nil); err == nil || !strings.Contains(err.Error(), "reaches Docker or the kernel") {
		t.Fatalf("the bound daemon socket's directory was mountable: %v", err)
	}
}

// Every exit from cleanup attempts the two named volumes, including when host
// storage failed and in the containment fallback: otherwise an interrupted run
// leaks a volume pair per launch.
func TestFilteredCleanupContainsVolumesOnEveryExit(t *testing.T) {
	t.Run("unreadable registry", func(t *testing.T) {
		f, d := filteredFixture(t)
		if _, err := f.launch(context.Background(), RunSpec{}, nil, nil, io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}
		record := filepath.Join(f.store.Path(), "execution-"+f.record.ID+".json")
		if err := os.WriteFile(record, []byte("not a record"), 0o600); err != nil {
			t.Fatal(err)
		}
		gone, err := f.cleanup("exited")
		if gone || err == nil {
			t.Fatal("a lost registry reported clean custody", gone, err)
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		if len(d.volumes) != 0 {
			t.Fatalf("cleanup leaked volumes after an early return: %v", slices.Collect(maps.Keys(d.volumes)))
		}
		if len(d.containers) != 0 {
			t.Fatalf("cleanup leaked containers: %v", slices.Collect(maps.Keys(d.containers)))
		}
	})
	t.Run("container removal fails", func(t *testing.T) {
		f, d := filteredFixture(t)
		d.refuseRemoval = "agent"
		if _, err := f.launch(context.Background(), RunSpec{}, nil, nil, io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}
		if gone, err := f.cleanup("runtime_failed"); gone || err == nil {
			t.Fatal("a surviving container reported clean custody", gone, err)
		}
		d.mu.Lock()
		volumes := len(d.volumes)
		d.mu.Unlock()
		if volumes != 2 {
			t.Fatalf("volumes were removed under a container whose absence is unproven: %d left", volumes)
		}
		r, err := f.store.Execution(f.record.ID)
		if err != nil || r.Receipt == nil || r.Receipt.Cleanup != "pending" {
			t.Fatal("the receipt did not record pending cleanup", err)
		}
	})
}

// The protected inventory is not just this host process's interfaces. Every
// subnet the runtime allocates and every gateway it holds is protected too, or
// a granted CIDR covering a bridge subnet would reach sibling containers, other
// sessions' boxes and the daemon's own gateway.
func TestFilteredProtectedAddressesCoverTheRuntimeTopology(t *testing.T) {
	networks := []runtime.DockerNetwork{
		{Name: "bridge", Subnets: []netip.Prefix{netip.MustParsePrefix("172.17.0.0/16")}, Gateways: []netip.Addr{netip.MustParseAddr("172.17.0.1")}},
		{Name: "other-session_default", Subnets: []netip.Prefix{netip.MustParsePrefix("192.168.97.0/24")}, Gateways: []netip.Addr{netip.MustParseAddr("192.168.97.1")}},
		{Name: "host", Subnets: nil, Gateways: nil},
	}
	host := func() ([]netip.Prefix, error) { return []netip.Prefix{netip.MustParsePrefix("10.1.2.3/32")}, nil }
	protected, ingress, err := filteredProtectedAddresses(networks, host)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"10.1.2.3/32", "172.17.0.0/16", "172.17.0.1/32", "192.168.97.0/24", "192.168.97.1/32"} {
		if !slices.Contains(protected, netip.MustParsePrefix(want)) {
			t.Errorf("protected set is missing %s: %v", want, protected)
		}
	}
	if ingress != netip.MustParseAddr("172.17.0.1") {
		t.Fatalf("bridge gateway = %v, want the address host-published traffic is NAT'd from", ingress)
	}
	if !slices.IsSortedFunc(protected, func(a, b netip.Prefix) int { return strings.Compare(a.String(), b.String()) }) {
		t.Error("the protected set is not stably ordered")
	}
	// A runtime with no bridge gateway cannot secure a published serve port, and
	// a topology beyond the qualified envelope is a refusal, not a truncation.
	if _, ingress, err := filteredProtectedAddresses(networks[1:], host); err != nil || ingress.IsValid() {
		t.Fatal("a runtime without a bridge reported an ingress source", ingress, err)
	}
	var many []runtime.DockerNetwork
	for i := range networkgateway.MaxProtectedRanges {
		many = append(many, runtime.DockerNetwork{Name: fmt.Sprintf("n%d", i),
			Subnets: []netip.Prefix{netip.MustParsePrefix(fmt.Sprintf("10.%d.0.0/16", i))}})
	}
	if _, _, err := filteredProtectedAddresses(many, host); err == nil {
		t.Fatal("a topology beyond the qualified envelope was silently truncated")
	}
}

// The task-channel volume coop created this run is exempt from the workload volume-exposure check:
// it is coop-owned, run-private, and read-only, and its backing mountpoint lives in the daemon VM
// (not a host path). A filtered loop iteration mounts it, so it must validate — while any OTHER
// named volume still goes through the daemon exposure inspection.
func TestFilteredMountsExemptTheOwnedTaskChannelVolume(t *testing.T) {
	f, d := filteredFixture(t)
	f.taskVolume = "coop-tasks-deadbeefdeadbeef"
	if err := f.validateMounts([]string{"-v", f.taskVolume + ":/coop/tasks:ro"}, nil, nil); err != nil {
		t.Fatalf("the owned task-channel volume was refused: %v", err)
	}
	if slices.Contains(d.log, "volume-exposure") {
		t.Fatal("the exempt task-channel volume was still sent to the daemon exposure inspection")
	}
	// Any OTHER named volume is not exempt — it reaches the daemon exposure inspection.
	if err := f.validateMounts([]string{"-v", "some-other-volume:/coop/tasks:ro"}, nil, nil); err != nil {
		t.Fatalf("unexpected refusal for a fixture volume: %v", err)
	}
	if !slices.Contains(d.log, "volume-exposure") {
		t.Fatal("an unowned named volume skipped the exposure inspection")
	}
}
