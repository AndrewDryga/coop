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
	"github.com/AndrewDryga/coop/internal/networkgateway"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/networkview"
	"github.com/AndrewDryga/coop/internal/runtime"
)

type filteredDaemonFixture struct {
	mu                                     sync.Mutex
	f                                      *filteredExecution
	containers                             map[string]runtime.DockerContainer
	volumes                                map[string]runtime.DockerVolume
	log                                    []string
	sequence                               networkview.Count
	ambiguous, refuseRemoval, corruptRole  string
	brokenProbe, holdAgent, malformedFinal bool
	resolverStatus                         *string
	corruptMount                           string
	earlyGuardEnd, failStartupPersistence  bool
	notAttempted                           string
	transientProbes, successfulProbes      int
	stopGrace                              map[string]int
	probeRecovered                         chan struct{}
	attached                               chan struct{}
	volumeExposure                         runtime.VolumeExposure
	smoke                                  *networkstate.QualificationSmoke
	networkMembers                         map[string]netip.Addr
	composeServices                        map[string]string
	connected                              []string
}

func (d *filteredDaemonFixture) ConnectNetwork(_ context.Context, network string, ref runtime.DockerRef) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ref.ID == "" {
		return errors.New("network attachment needs an exact container id")
	}
	d.connected = append(d.connected, network+"/"+ref.ID)
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
	record, err := smoke.CreateExecution(ctx, networkstate.ExecutionSpec{Project: project, PolicyFingerprint: policy.Fingerprint,
		Runtime: "docker", DaemonID: "fixture-daemon", Endpoint: "unix:///fixture.sock", GatewayImage: "sha256:" + strings.Repeat("a", 64), ClientImage: "sha256:" + strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(networkgateway.LaunchConfig{Version: 1, RunID: record.ID, Epoch: record.Epoch, Policy: policy})
	record, err = store.PrepareArtifacts(ctx, record.ID, record.Revision, data)
	if err != nil {
		t.Fatal(err)
	}
	f := &filteredExecution{store: store, record: record, policy: policy, image: "sha256:" + strings.Repeat("b", 64), attempted: map[string]bool{}}
	f.config, err = store.LaunchConfigPath(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.runfiles, err = store.RunFilesPath(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.protected = []netip.Prefix{netip.MustParsePrefix("192.0.2.1/32")}
	f.hostAddresses = func() ([]netip.Prefix, error) { return slices.Clone(f.protected), nil }
	d := &filteredDaemonFixture{f: f, smoke: smoke, containers: map[string]runtime.DockerContainer{}, volumes: map[string]runtime.DockerVolume{}, attached: make(chan struct{})}
	f.docker = d
	return f, d
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
	d.mu.Lock()
	defer d.mu.Unlock()
	role := ref.Labels["coop.network.role"]
	d.log = append(d.log, "create:"+role)
	if d.notAttempted == role {
		return runtime.DockerVolume{}, runtime.ErrDockerCreateNotAttempted
	}
	value := runtime.DockerVolume{Name: ref.Name, Driver: "local", Labels: ref.Labels}
	d.volumes[ref.Name] = value
	return value, nil
}
func (d *filteredDaemonFixture) InspectVolume(_ context.Context, ref runtime.DockerRef) (runtime.DockerVolume, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	v, ok := d.volumes[ref.Name]
	return v, ok, nil
}
func (d *filteredDaemonFixture) RemoveVolume(_ context.Context, ref runtime.DockerRef) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	role := ref.Labels["coop.network.role"]
	d.log = append(d.log, "remove:"+role)
	if d.refuseRemoval == role {
		return errors.New("fixture removal unknown")
	}
	delete(d.volumes, ref.Name)
	return nil
}
func (d *filteredDaemonFixture) CreateContainer(_ context.Context, spec runtime.DockerCreate) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	role := spec.Ref.Labels["coop.network.role"]
	d.log = append(d.log, "create:"+role)
	if d.notAttempted == role {
		return "", runtime.ErrDockerCreateNotAttempted
	}
	if d.ambiguous == role {
		return "", errors.New("fixture ambiguous create")
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
	if d.corruptRole == role {
		v.User = "0"
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
	if d.corruptMount == role && len(v.Mounts) > 0 {
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
	d.containers[spec.Ref.Name] = v
	return id, nil
}
func (d *filteredDaemonFixture) InspectContainer(_ context.Context, ref runtime.DockerRef) (runtime.DockerContainer, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	v, ok := d.containers[ref.Name]
	return v, ok, nil
}
func (d *filteredDaemonFixture) StartContainer(_ context.Context, ref runtime.DockerRef) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.log = append(d.log, "start:"+ref.Labels["coop.network.role"])
	v := d.containers[ref.Name]
	v.State = runtime.DockerContainerState{Status: "running", Running: true, StartedAt: time.Now()}
	d.containers[ref.Name] = v
	return nil
}
func (d *filteredDaemonFixture) StartAttached(ctx context.Context, ref runtime.DockerRef, _ io.Reader, stdout, _ io.Writer, started func() error) (int, error) {
	if err := d.StartContainer(ctx, ref); err != nil {
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
	if d.refuseRemoval == role {
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
		Clock: networkgateway.ClockDomain{BootID: "12345678-1234-1234-1234-123456789abc", TimeNamespace: "42"}}
	data, _ := json.Marshal(networkgateway.RuntimeObservation{Version: 1, Identity: identity, Network: s})
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
func (d *filteredDaemonFixture) CopyArchive(_ context.Context, _ runtime.DockerRef, _ string, _ int) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.log = append(d.log, "capture-final")
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

func TestFilteredLaunchOrdersReadinessAndExactCleanup(t *testing.T) {
	f, d := filteredFixture(t)
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
	want := []string{"create:ipc", "create:observations", "create:controller", "start:controller", "create:guard", "start:guard", "snapshot", "create:agent", "snapshot", "start:agent",
		"stop:agent", "remove:agent", "stop:guard", "capture-final", "stop:guard", "remove:guard", "stop:controller", "stop:controller", "remove:controller", "remove:ipc", "remove:observations", "close"}
	if !slices.Equal(d.log, want) {
		t.Fatalf("lifecycle order:\n%v\nwant:\n%v", d.log, want)
	}
	record, err := f.store.Execution(f.record.ID)
	if err != nil || record.Receipt == nil || record.Receipt.Completeness != "complete" || record.Receipt.Cleanup != "complete" || *record.Receipt.Snapshot.Counters.SentBytes != 123 {
		t.Fatal("receipt was missing, partial, or double counted", err)
	}
	if _, err := os.Stat(f.runfiles); !os.IsNotExist(err) {
		t.Fatal("generated files retained after complete cleanup", err)
	}
}

func TestFilteredCleanupRetainsUnknownCreateAndSurvivingAgent(t *testing.T) {
	for _, phase := range []string{"ambiguous-create", "unknown-removal"} {
		t.Run(phase, func(t *testing.T) {
			f, d := filteredFixture(t)
			if phase == "ambiguous-create" {
				d.ambiguous = "agent"
			} else {
				d.refuseRemoval = "agent"
			}
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
	if err == nil || !strings.Contains(err.Error(), "restricted network health lost") || ctx.Err() != nil {
		t.Fatal("health loss did not stop attachment", err)
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
	if err == nil || f.workloadOutcome(code, err, false) != "runtime_failed" || f.resource("agent").State != "starting" {
		t.Fatal("failed startup publication reclassified the runtime", code, err)
	}
	if gone, err := f.cleanup("runtime_failed"); err != nil || !gone {
		t.Fatal("startup failure cleanup", gone, err)
	}
}
