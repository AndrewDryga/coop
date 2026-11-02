package box

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/fsfilter"
	"github.com/AndrewDryga/coop/internal/fsimage"
	"github.com/AndrewDryga/coop/internal/randid"
	"github.com/AndrewDryga/coop/internal/runtime"
)

const filesystemLabel = "coop.filesystem.parent"

var errFilesystemUnsupported = errors.New("filesystem runtime is not qualified")
var errFilesystemCustodyPending = errors.New("filesystem cleanup requires reconciliation")

type filesystemRecord struct {
	Version                               int `json:"version"`
	Parent, Endpoint, DaemonID, Image     string
	Helper                                runtime.DockerRef
	Consumers                             []runtime.DockerRef
	Ready                                 *fsfilter.Ready
	Pending                               string
	Supervisor, Workspace                 string
	Services                              bool
	Probes                                []filesystemProbeRecord
	Project, Service, WorkingDir          string
	HealthTest                            []string
	ServiceConfigDigest, ProtectionDigest string
	Profile                               fsfilter.Profile
	WorkloadImage                         string
	HelperStartedAt                       time.Time
}

type filesystemAdmission struct {
	docker            *runtime.Docker
	dir               string
	record            filesystemRecord
	snapshots         []*filesystemSnapshot
	probes            []*filesystemProbe
	workloadImage     string
	workloadExecution *runtime.DockerImageExecution
	lease             *os.File
	reused            bool
}

func filesystemRef(parent, role string) runtime.DockerRef {
	return runtime.DockerRef{Name: parent + "-" + role, Labels: map[string]string{LabelKey: "filesystem", filesystemLabel: parent, "coop.filesystem.role": role}}
}

func filesystemJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := config.WriteFileAtomicMode(path, data, 0600); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func (a *filesystemAdmission) persist() error {
	if err := filesystemJSON(filepath.Join(a.dir, "record.json"), a.record); err != nil {
		return err
	}
	parent, err := os.Open(filepath.Dir(a.dir))
	if err != nil {
		return err
	}
	return errors.Join(parent.Sync(), parent.Close())
}

// prepareFilesystemAdmission is shared by agents and services. No workload is
// created until every hook and every actual share has passed qualification.
func prepareFilesystemAdmission(ctx context.Context, rt runtime.Runtime, roots []string, grants [][]string, binding *runtime.Docker) (_ *filesystemAdmission, result error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if rt.Name != "docker" {
		return nil, errFilesystemUnsupported
	}
	endpoint, daemon := rt.ComposeBinding()
	if binding != nil {
		endpoint, daemon = binding.Endpoint(), binding.Info().ID
	}
	docker, err := runtime.BindDockerForSetup(ctx, rt, endpoint, daemon)
	if err != nil {
		return nil, errors.Join(errFilesystemUnsupported, err)
	}
	return prepareBoundFilesystemAdmission(ctx, roots, grants, docker, nil)
}

// Takes ownership of this binding, including on preparation failure. Services
// retain their independent bindings; only agent selection transfers its own.
func prepareBoundFilesystemAdmission(ctx context.Context, roots []string, grants [][]string, docker *runtime.Docker, progress func(string)) (_ *filesystemAdmission, result error) {
	if len(roots) == 0 || len(roots) > 32 || len(grants) != len(roots) {
		docker.Close()
		return nil, errFilesystemUnsupported
	}
	if err := nonoverlappingFilesystemRoots(roots); err != nil {
		docker.Close()
		return nil, errors.Join(errFilesystemUnsupported, err)
	}
	for _, root := range roots {
		if !filesystemProfileCandidate(root) {
			docker.Close()
			return nil, errFilesystemUnsupported
		}
	}
	info := docker.Info()
	if info.CgroupVersion != "2" || info.CgroupDriver != "cgroupfs" {
		docker.Close()
		return nil, errFilesystemUnsupported
	}
	state, err := serviceStateRoot("filesystem")
	if err != nil {
		docker.Close()
		return nil, err
	}
	if err := os.MkdirAll(state, 0700); err != nil {
		docker.Close()
		return nil, err
	}
	parent := "coop-fs-" + randid.Hex(16)
	dir, err := privateTempDirUnder(state, roots[0], parent+"-", roots[1:]...)
	if err != nil {
		docker.Close()
		return nil, err
	}
	a := &filesystemAdmission{docker: docker, dir: dir, record: filesystemRecord{Version: 1, Parent: parent, Endpoint: docker.Endpoint(), DaemonID: info.ID, Helper: filesystemRef(parent, "owner"), Workspace: roots[0], Supervisor: supervisorLabelValue(workspaceScope(roots[0]), os.Getpid())}}
	a.lease, err = lockFilesystemAdmission(dir, true)
	if err != nil {
		docker.Close()
		return nil, err
	}
	transferred := false
	defer a.cleanupUntransferred(&result, &transferred)
	if err := a.persist(); err != nil {
		return nil, err
	}
	if err := a.record.validate(); err != nil {
		return nil, err
	}
	for _, root := range roots {
		probe, err := newFilesystemProbe(root, func(record filesystemProbeRecord) error {
			a.record.Probes = append(a.record.Probes, record)
			return a.persist()
		})
		if err != nil {
			return nil, errors.Join(errFilesystemUnsupported, err)
		}
		a.probes = append(a.probes, probe)
	}
	// Only the owner branch writes custody. Policy publication and cleanup wait
	// for both branches; the trusted probe stays stopped until qualification.
	var policies fsfilter.Admission
	var probeRef runtime.DockerRef
	prepareOwner := func() error {
		image, labels, err := docker.Image(ctx, fsimage.Tag())
		if errors.Is(err, runtime.ErrDockerImageMissing) {
			if progress != nil {
				progress("Building the filesystem protection helper")
			}
			log, openErr := os.OpenFile(filepath.Join(dir, "build.log"), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
			if openErr != nil {
				return openErr
			}
			image, err = fsimage.Build(ctx, docker, log)
			err = errors.Join(err, log.Close())
			if err != nil {
				err = filesystemBuildError(err, filepath.Join(dir, "build.log"))
			}
		} else if err == nil && labels[fsimage.BuildLabel] != fsimage.Fingerprint() {
			err = errors.New("filesystem helper image has an unexpected build identity")
		}
		if err != nil {
			return err
		}
		a.record.Image = image
		if err := a.persist(); err != nil {
			return err
		}
		options := a.helperOptions(false)
		for i, root := range roots {
			options = append(options, "--mount", networkMount("bind", root, fmt.Sprintf("/sources/%d", i+1), true))
		}
		id, err := a.create(ctx, runtime.DockerCreate{Ref: a.record.Helper, Image: image, Options: options, Command: []string{"admit", "/sources", "/state/policy.json", "/host-cgroup/" + parent}})
		a.record.Helper.ID = id
		if saveErr := a.persist(); err != nil || saveErr != nil {
			return errors.Join(err, saveErr)
		}
		if err := docker.StartContainer(ctx, a.record.Helper); err != nil {
			return err
		}
		probeRef, err = a.createShareProbe(ctx, roots)
		return err
	}
	capture := func() error {
		for i, root := range roots {
			probe := a.probes[i]
			// Remember captured identities before publication so a rejected admission
			// retains old aliases across an ordinary host atomic save and fresh retry.
			snapshot, err := captureRememberedFilesystemSnapshot(ctx, root, probe.profile, grants[i], probe.relative)
			if err != nil {
				return err
			}
			a.snapshots = append(a.snapshots, snapshot)
			policy := snapshot.policy
			policy.Entries = slices.Clone(policy.Entries)
			probe.add(&policy)
			policies.Roots = append(policies.Roots, policy)
		}
		return nil
	}
	if err := together(ctx, prepareOwner, capture); err != nil {
		return nil, err
	}
	if err := filesystemJSON(filepath.Join(dir, "policy.json"), policies); err != nil {
		return nil, err
	}
	if err := a.waitReady(ctx); err != nil {
		return nil, errors.Join(errFilesystemUnsupported, err)
	}
	if err := a.qualifyShares(ctx, probeRef); err != nil {
		return nil, errors.Join(errFilesystemUnsupported, err)
	}
	// Only release-owned helpers have run. The stopped workload's final
	// verification rechecks the complete inventory before its first start.
	for _, probe := range a.probes {
		if err := probe.close(); err != nil {
			return nil, err
		}
	}
	a.probes = nil
	transferred = true
	return a, nil
}

// A preparation panic must release its resources after every parallel branch settles.
func (a *filesystemAdmission) cleanupUntransferred(result *error, transferred *bool) {
	panicked := recover()
	if !*transferred {
		if cleanup := a.close(); cleanup != nil {
			*result = errors.Join(*result, errFilesystemCustodyPending, fmt.Errorf("filesystem custody retained at %s: %w", a.dir, cleanup))
		}
	}
	if panicked != nil {
		if *result != nil {
			panic(fmt.Sprintf("%v\nfilesystem cleanup: %v", panicked, *result))
		}
		panic(panicked)
	}
}

func (a *filesystemAdmission) helperOptions(readonlyState bool) []string {
	// Every admitted object stays open; leave room for maps, programs and roots.
	descriptors := fsfilter.MaxPaths + 128
	return []string{"--network", "none", "--user", "0", "--read-only", "--no-healthcheck", "--cap-drop", "ALL", "--cap-add", "SYS_ADMIN", "--security-opt", "seccomp=unconfined", "--cgroupns", "host", "--tmpfs", "/tmp:exec", "--pids-limit", "32", "--memory", "512m",
		"--ulimit", fmt.Sprintf("nofile=%d:%d", descriptors, descriptors),
		"--mount", networkMount("bind", "/sys/fs/cgroup", "/host-cgroup", false), "--mount", networkMount("bind", "/sys/fs/bpf", "/host-bpf", false), "--mount", networkMount("bind", a.dir, "/state", readonlyState)}
}

func (a *filesystemAdmission) waitReady(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		value, data, err := a.docker.ObserveContainerLogs(ctx, a.record.Helper, 30, 1<<20)
		if err != nil {
			return err
		}
		for _, line := range bytes.Split(data, []byte{'\n'}) {
			var ready fsfilter.Ready
			if json.Unmarshal(line, &ready) == nil && ready.Validate(a.record.Parent) == nil {
				a.record.Ready = &ready
				return a.persist()
			}
		}
		if !value.State.Running {
			return fmt.Errorf("filesystem helper did not become ready: %s", strings.TrimSpace(string(data)))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (a *filesystemAdmission) consumer(ref runtime.DockerRef) error {
	for i, existing := range a.record.Consumers {
		if existing.Name == ref.Name {
			a.record.Consumers[i] = ref
			return a.persist()
		}
	}
	a.record.Consumers = append(a.record.Consumers, ref)
	return a.persist()
}

func (a *filesystemAdmission) createShareProbe(ctx context.Context, roots []string) (runtime.DockerRef, error) {
	ref := filesystemRef(a.record.Parent, "probe")
	if err := a.consumer(ref); err != nil {
		return ref, err
	}
	options := []string{"--network", "none", "--user", "0", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--read-only", "--cgroup-parent", "/" + a.record.Parent, "--mount", networkMount("bind", a.dir, "/state", true)}
	for i, root := range roots {
		options = append(options, "--mount", networkMount("bind", root, fmt.Sprintf("/sources/%d", i+1), true))
	}
	id, err := a.create(ctx, runtime.DockerCreate{Ref: ref, Image: a.record.Image, Options: options, Command: []string{"probe", "/state/probe.json"}})
	ref.ID = id
	if saveErr := a.consumer(ref); err != nil || saveErr != nil {
		return ref, errors.Join(err, saveErr)
	}
	return ref, nil
}

func (a *filesystemAdmission) qualifyShares(ctx context.Context, ref runtime.DockerRef) error {
	var warm fsfilter.ShareProbe
	for i, p := range a.probes {
		warm.Readable = append(warm.Readable, fmt.Sprintf("/sources/%d/%s/warm-alias", i+1, p.relative))
	}
	if err := filesystemJSON(filepath.Join(a.dir, "probe-warm.json"), warm); err != nil {
		return err
	}
	if _, err := a.docker.ExecRead(ctx, a.record.Helper, 4096, "/usr/local/bin/coop-fs", "probe-warm", "/state/probe-warm.json"); err != nil {
		return err
	}
	var probe fsfilter.ShareProbe
	for i, p := range a.probes {
		if err := p.replace(i, &probe); err != nil {
			return err
		}
	}
	if err := filesystemJSON(filepath.Join(a.dir, "probe.json"), probe); err != nil {
		return err
	}
	var output bytes.Buffer
	code, err := a.docker.StartAttached(ctx, ref, nil, &output, &output, nil)
	if code != 0 || err != nil {
		return errors.Join(fmt.Errorf("filesystem share qualification failed: %s", output.String()), err)
	}
	return a.docker.RemoveContainer(ctx, ref)
}

func (a *filesystemAdmission) revalidate() error {
	for i, s := range a.snapshots {
		exclude := ""
		if i < len(a.probes) {
			exclude = a.probes[i].relative
		}
		if err := s.revalidate(exclude); err != nil {
			return err
		}
	}
	return nil
}

func (a *filesystemAdmission) create(ctx context.Context, spec runtime.DockerCreate) (string, error) {
	if a.record.Pending != "" {
		return "", errors.New("filesystem resource creation needs reconciliation")
	}
	a.record.Pending = spec.Ref.Name
	if err := a.persist(); err != nil {
		return "", err
	}
	id, err := a.docker.CreateContainer(ctx, spec)
	if id != "" || errors.Is(err, runtime.ErrDockerCreateNotAttempted) {
		a.record.Pending = ""
		// Save the immutable ID together with clearing the pending intention.
		if spec.Ref.Name == a.record.Helper.Name {
			a.record.Helper.ID = id
		}
		for i, ref := range a.record.Consumers {
			if ref.Name == spec.Ref.Name {
				a.record.Consumers[i].ID = id
			}
		}
		err = errors.Join(err, a.persist())
	}
	return id, err
}

func (a *filesystemAdmission) remove(ctx context.Context, ref runtime.DockerRef) error {
	value, present, err := a.docker.InspectContainer(ctx, ref)
	if err != nil {
		return err
	}
	if !present {
		if a.record.Pending == ref.Name {
			return errors.New("filesystem resource creation outcome remains unknown")
		}
		return nil
	}
	ref.ID = value.ID
	if a.record.Pending == ref.Name {
		// Observation resolves creation before removal. A crash after removal
		// must not turn its now-absent name back into an unknown create.
		a.record.Pending = ""
		if ref.Name == a.record.Helper.Name {
			a.record.Helper.ID = ref.ID
		}
		for i, existing := range a.record.Consumers {
			if existing.Name == ref.Name {
				a.record.Consumers[i].ID = ref.ID
			}
		}
		if err := a.persist(); err != nil {
			return err
		}
	}
	if err := a.docker.RemoveContainer(ctx, ref); err != nil {
		return err
	}
	return nil
}

// Guard files are copied from the release-owned helper, never the workload
// image. The containing directory stays outside every exposed source tree.
func (a *filesystemAdmission) writeGuard(ctx context.Context, manifest fsfilter.ExecutionManifest) (string, error) {
	dir := filepath.Join(a.dir, "control")
	if err := os.Mkdir(dir, 0755); err != nil {
		return "", err
	}
	file, err := os.OpenFile(filepath.Join(dir, "guard"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0555)
	if err != nil {
		return "", err
	}
	err = a.docker.CopyFile(ctx, a.record.Helper, "/usr/local/bin/coop-fs", file, 8<<20)
	if err := errors.Join(err, file.Close()); err != nil {
		return "", err
	}
	manifest.BootID = a.record.Ready.BootID
	data, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0444); err != nil {
		return "", err
	}
	// Private host umasks must not make the mounted guard unreadable to the
	// unprivileged workload. Only these public control leaves gain access.
	for name, mode := range map[string]os.FileMode{dir: 0755, filepath.Join(dir, "guard"): 0555, filepath.Join(dir, "manifest.json"): 0444} {
		if err := os.Chmod(name, mode); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// close retains both enforcement and records whenever a consumer's removal or
// daemon observation is uncertain. It is safe to retry after any completed step.
func (a *filesystemAdmission) close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return a.closeContext(ctx)
}

func (a *filesystemAdmission) closeContext(ctx context.Context) error {
	defer a.docker.Close()
	defer a.releaseLease()
	for _, ref := range a.record.Consumers {
		if err := a.remove(ctx, ref); err != nil {
			return err
		}
	}
	if err := a.docker.CgroupParentUnused(ctx, "/"+a.record.Parent); err != nil {
		return err
	}
	if err := a.remove(ctx, a.record.Helper); err != nil {
		return err
	}
	if a.record.Image != "" {
		ref := filesystemRef(a.record.Parent, "recovery")
		if err := a.remove(ctx, ref); err != nil {
			return err
		}
		command := []string{"abandon", a.record.Parent}
		if _, err := os.Lstat(filepath.Join(a.dir, "birth.json")); err == nil {
			command = []string{"release", "/state/birth.json"}
		} else if !os.IsNotExist(err) {
			return err
		}
		id, err := a.create(ctx, runtime.DockerCreate{Ref: ref, Image: a.record.Image, Options: a.helperOptions(true), Command: command})
		ref.ID = id
		if err != nil {
			return err
		}
		var output bytes.Buffer
		code, err := a.docker.StartAttached(ctx, ref, nil, &output, &output, nil)
		if code != 0 || err != nil {
			return errors.Join(fmt.Errorf("filesystem cleanup failed: %s", output.String()), err)
		}
		if err := a.docker.RemoveContainer(ctx, ref); err != nil {
			return err
		}
	}
	for _, p := range a.record.Probes {
		if err := p.close(); err != nil {
			return err
		}
	}
	return os.RemoveAll(a.dir)
}
