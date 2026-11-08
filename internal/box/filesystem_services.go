package box

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/fsfilter"
	"github.com/AndrewDryga/coop/internal/fsimage"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/ui"
	"go.yaml.in/yaml/v3"
	"golang.org/x/sys/unix"
)

type deferServiceShadowsKey struct{}
type serviceCaseExposureKey struct{}

type serviceFilesystemStart struct {
	args       []string
	admissions []*filesystemAdmission
	temporary  string
	retained   bool
}

func serviceFilesystemDigest(doc, service map[string]any, image string, manifest fsfilter.ExecutionManifest, health map[string]any) (string, error) {
	data, err := json.Marshal(struct {
		Service           map[string]any
		Networks, Volumes any
		Image, Helper     string
		Manifest          fsfilter.ExecutionManifest
		Health            map[string]any
	}{service, doc["networks"], doc["volumes"], image, fsimage.Fingerprint(), manifest, health})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func reuseServiceFilesystem(ctx context.Context, rt runtime.Runtime, repo, project, service, workingDir, digest, image string, grants []string, binding *runtime.Docker) (*filesystemAdmission, error) {
	state, err := serviceStateRoot("filesystem")
	if err != nil {
		return nil, err
	}
	// Admission creates its mount sources below the resolved private root.
	// Recover that same spelling when the state root has a host alias.
	state, err = filepath.EvalSymlinks(state)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(state)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	host, err := thisHost()
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "coop-fs-") {
			continue
		}
		dir := filepath.Join(state, entry.Name())
		r, err := readFilesystemRecord(dir)
		if err != nil || !r.Services || r.Workspace != repo || r.Project != project || r.Service != service || r.WorkingDir != workingDir || r.Endpoint != binding.Endpoint() || r.DaemonID != binding.Info().ID {
			continue
		}
		lease, err := lockFilesystemAdmission(dir, false)
		if os.IsNotExist(err) {
			continue // recovery retired this candidate after the directory scan
		}
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, errors.New("service filesystem admission is busy")
		}
		if err != nil {
			return nil, err
		}
		a, err := reuseLockedServiceFilesystem(ctx, rt, dir, lease, digest, image, grants, host)
		if a != nil {
			return a, nil
		}
		lease.Close()
		if err != nil {
			return nil, err
		}
	}
	return nil, nil
}

func reuseLockedServiceFilesystem(ctx context.Context, rt runtime.Runtime, dir string, lease *os.File, digest, image string, grants []string, host string) (_ *filesystemAdmission, result error) {
	r, err := readFilesystemRecord(dir)
	if os.IsNotExist(err) {
		return nil, nil // the previous owner completed cleanup before lease acquisition
	}
	if err != nil {
		return nil, err
	}
	supervisor, _ := parseSupervisorLabel(r.Supervisor)
	if !r.Services || r.Pending != "" || r.Ready == nil || r.ServiceConfigDigest != digest || r.WorkloadImage != image || r.ProtectionDigest == "" || r.HelperStartedAt.IsZero() || supervisor.origin(host) != originHere {
		return nil, nil
	}
	snapshot, err := captureFilesystemSnapshot(r.Workspace, r.Profile, grants, nil)
	if err != nil {
		return nil, err
	}
	protection, err := snapshot.digest()
	if err != nil {
		return nil, err
	}
	if protection != r.ProtectionDigest {
		return nil, nil
	}
	docker, err := runtime.BindDockerForSetup(ctx, rt, r.Endpoint, r.DaemonID)
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			docker.Close()
		}
	}()
	helper, present, err := docker.InspectContainer(ctx, r.Helper)
	if err != nil {
		return nil, err
	}
	if !present || !helper.State.Running || helper.Image != r.Image || !helper.State.StartedAt.Equal(r.HelperStartedAt) {
		return nil, nil
	}
	var ref runtime.DockerRef
	for _, consumer := range r.Consumers {
		if consumer.Name == r.Parent+"-service" {
			ref = consumer
		}
	}
	if ref.ID == "" {
		return nil, errors.New("retained service lacks immutable consumer identity")
	}
	_, present, err = docker.InspectContainer(ctx, ref)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, nil
	}
	a := &filesystemAdmission{docker: docker, dir: dir, record: r, snapshots: []*filesystemSnapshot{snapshot}, workloadImage: image, lease: lease, reused: true}
	if r.Controller != nil {
		a.controller, err = leaseRecordedFilesystemController(ctx, docker, *r.Controller, true, r.Workspace)
		if errors.Is(err, errFilesystemControllerStopped) {
			return nil, nil // replace the denied service under a fresh controller
		}
		if err != nil {
			return nil, err
		}
		defer func() {
			if !keep {
				a.controller.closeLease()
			}
		}()
	}
	if err := a.verify(ctx, ref, image); err != nil {
		return nil, err
	}
	keep = true
	return a, nil
}

func (s *serviceFilesystemStart) abort() error {
	var problems []error
	if !s.retained {
		for _, a := range s.admissions {
			if a.reused {
				a.releaseLease()
				problems = append(problems, a.docker.Close())
			} else {
				problems = append(problems, a.close())
			}
		}
	}
	if s.temporary != "" {
		problems = append(problems, os.RemoveAll(s.temporary))
	}
	return errors.Join(problems...)
}

func (s *serviceFilesystemStart) retain() error {
	for _, a := range s.admissions {
		a.record.Services = true
		if err := a.persist(); err != nil {
			return err
		}
	}
	s.retained = true
	var problems []error
	for _, a := range s.admissions {
		a.releaseLease()
		problems = append(problems, a.docker.Close())
	}
	return errors.Join(problems...)
}

func serviceFilesystemGrants(repo, file string, data []byte) (map[string][]string, error) {
	decoys, _, _, err := serviceShadowPlanWithSpellings(repo, file, data, false)
	if err != nil {
		return nil, err
	}
	grants := map[string][]string{}
	approval, ok := ApprovedServiceSecrets(repo, file, data)
	if !ok {
		return grants, nil
	}
	kept, _ := keepDecoysOutside(decoys, approval.Paths)
	for name, entries := range decoys {
		for _, entry := range entries {
			if !slices.Contains(kept[name], entry) && !slices.Contains(grants[name], entry.source) {
				grants[name] = append(grants[name], entry.source)
			}
		}
		slices.Sort(grants[name])
	}
	return grants, nil
}

func resolvedFilesystemServices(ctx context.Context, rt runtime.Runtime, args, selected []string) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out := boundedServiceConfig{limit: maxResolvedServiceConfigBytes}
	configArgs := append(slices.Clone(args), "config", "--format", "json")
	configArgs = append(configArgs, selected...)
	code, err := rt.RunComposeContext(ctx, nil, &out, io.Discard, configArgs...)
	if err != nil || code != 0 || out.overflow {
		return nil, errors.Join(errors.New("cannot resolve bounded service configuration for filesystem protection"), err)
	}
	var doc map[string]any
	if json.Unmarshal(out.Bytes(), &doc) != nil {
		return nil, errors.New("invalid resolved service configuration")
	}
	return doc, nil
}

func prepareServiceFilesystemStart(ctx context.Context, rt runtime.Runtime, repo, file string, data []byte, args, selected []string, allowCases, reuse bool) (_ *serviceFilesystemStart, result error) {
	s := &serviceFilesystemStart{args: slices.Clone(args)}
	defer func() {
		if result != nil {
			result = errors.Join(result, s.abort())
		}
	}()
	deferred, _ := ctx.Value(deferServiceShadowsKey{}).(bool)
	fallback := func() (*serviceFilesystemStart, error) {
		decoys, _, uncovered, err := serviceShadowPlan(repo, file, data)
		if err != nil {
			return nil, err
		}
		if !allowCases {
			if pending := UnapprovedCaseExposures(repo, uncovered); len(pending) > 0 {
				return nil, &UncoveredSpellings{Paths: pending}
			}
		}
		if len(uncovered) > 0 {
			ui.Warn("Accepted case exposure: %d hidden files may be read by your services under another letter case.", len(uncovered))
		}
		if !deferred {
			return s, nil
		}
		if approval, ok := ApprovedServiceSecrets(repo, file, data); ok {
			decoys, _ = keepDecoysOutside(decoys, approval.Paths)
		}
		dir, err := privateWorkspaceTempDir(repo, "coop-service-filesystem-")
		if err != nil {
			return nil, err
		}
		s.temporary = dir
		shadow, needed, err := writeServiceShadowOverride(repo, decoys, dir)
		if needed {
			s.args = append(s.args, "-f", shadow)
		}
		return s, err
	}
	if !deferred {
		return fallback()
	}
	endpoint, daemon := rt.ComposeBinding()
	docker, err := runtime.BindDockerForSetup(ctx, rt, endpoint, daemon)
	if err != nil {
		return fallback()
	}
	defer docker.Close()
	if _, err := filesystemCgroups(docker.Info()); err != nil {
		return fallback()
	}
	doc, err := resolvedFilesystemServices(ctx, rt, args, selected)
	if err != nil {
		return nil, err
	}
	services, ok := doc["services"].(map[string]any)
	if !ok || len(services) == 0 || len(services) > 512 {
		return nil, errors.New("invalid filesystem service set")
	}
	var raw composeDoc
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	names := slices.Clone(selected)
	if len(names) == 0 {
		for name := range services {
			names = append(names, name)
		}
		slices.Sort(names)
	} else if names, err = composeServiceClosure(raw.Services, names); err != nil {
		return nil, err
	}
	grants, err := serviceFilesystemGrants(repo, file, data)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		svc, ok := services[name].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("missing resolved service %q", name)
		}
		imageName, _ := svc["image"].(string)
		image, _, err := docker.Image(ctx, imageName)
		if errors.Is(err, runtime.ErrDockerImageMissing) {
			if err = rt.PullComposeImage(imageName); err == nil {
				image, _, err = docker.Image(ctx, imageName)
			}
		}
		if err != nil {
			return nil, err
		}
		execution, err := docker.ImageExecution(ctx, image)
		if err != nil {
			return nil, err
		}
		if err := filesystemImageVolumesSupported(execution); err != nil {
			return nil, err
		}
		manifest, health, err := serviceFilesystemExecution(svc, execution)
		if err != nil {
			return nil, err
		}
		volumes, _ := svc["volumes"].([]any)
		for _, v := range volumes {
			volume, ok := v.(map[string]any)
			target, _ := volume["target"].(string)
			if !ok || target == "" || filesystemControlOverlap(target) {
				return nil, errors.New("service volume overlaps filesystem guard control")
			}
		}
		project := ""
		for i := range args {
			if args[i] == "-p" && i+1 < len(args) {
				project = args[i+1]
			}
		}
		identity, err := serviceFilesystemDigest(doc, svc, image, manifest, health)
		if err != nil {
			return nil, err
		}
		var a *filesystemAdmission
		if reuse {
			a, err = reuseServiceFilesystem(ctx, rt, repo, project, name, filepath.Dir(file), identity, image, grants[name], docker)
			if err != nil {
				return nil, err
			}
		}
		if a == nil {
			a, err = prepareFilesystemAdmission(ctx, rt, []string{repo}, [][]string{grants[name]}, docker)
		}
		if errors.Is(err, errFilesystemUnsupported) && !errors.Is(err, errFilesystemCustodyPending) {
			if cleanup := s.abort(); cleanup != nil {
				return nil, errors.Join(err, cleanup)
			}
			s.admissions = nil
			return fallback()
		}
		if err != nil {
			return nil, err
		}
		s.admissions = append(s.admissions, a)
		a.workloadImage = image
		a.record.Service, a.record.WorkingDir = name, filepath.Dir(file)
		a.record.Project = project
		control := filepath.Join(a.dir, "control")
		if !a.reused {
			a.record.ServiceConfigDigest, a.record.WorkloadImage = identity, image
			a.record.ProtectionDigest, err = a.snapshots[0].digest()
			if err != nil {
				return nil, err
			}
			a.record.Profile = a.snapshots[0].policy.Profile
			helper, present, err := a.docker.InspectContainer(ctx, a.record.Helper)
			if err != nil || !present || !helper.State.Running {
				return nil, errors.Join(errors.New("filesystem helper stopped during admission"), err)
			}
			a.record.HelperStartedAt = helper.State.StartedAt
			control, err = a.writeGuard(ctx, manifest)
			if err != nil {
				return nil, err
			}
		}
		ref := filesystemRef(a.record.Parent, "service")
		ref.Labels[LabelKey] = LabelService
		ref.Labels[composeProjectLabel], ref.Labels[composeServiceLabel] = a.record.Project, name
		if !a.reused {
			if err := a.consumer(ref); err != nil {
				return nil, err
			}
		}
		svc["container_name"], svc["image"], svc["pull_policy"] = ref.Name, image, "never"
		svc["cgroup_parent"] = a.record.cgroupParent()
		svc["entrypoint"] = []string{fsfilter.ControlPath + "/guard"}
		svc["command"] = []string{"guard", fsfilter.ControlPath + "/manifest.json"}
		svc["security_opt"] = []string{"no-new-privileges:true"}
		svc["volumes"] = append(volumes, map[string]any{"type": "bind", "source": strings.ReplaceAll(control, "$", "$$"), "target": fsfilter.ControlPath, "read_only": true, "bind": map[string]any{"create_host_path": false}})
		labels, _ := svc["labels"].(map[string]any)
		if labels == nil {
			labels = map[string]any{}
		}
		for key, value := range ref.Labels {
			if !strings.HasPrefix(key, "com.docker.compose.") {
				labels[key] = value
			}
		}
		svc["labels"] = labels
		if len(manifest.Healthcheck) > 0 {
			a.record.HealthTest = []string{"CMD", fsfilter.ControlPath + "/guard", "healthcheck", fsfilter.ControlPath + "/manifest.json"}
			health["test"] = a.record.HealthTest
			svc["healthcheck"] = health
		}
		if err := a.persist(); err != nil {
			return nil, err
		}
	}
	dir, err := privateWorkspaceTempDir(repo, "coop-service-filesystem-")
	if err != nil {
		return nil, err
	}
	s.temporary = dir
	encoded, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "compose.json")
	if err := os.WriteFile(path, encoded, 0400); err != nil {
		return nil, err
	}
	s.args = nil
	for i := 0; i < len(args); i++ {
		if args[i] == "-f" {
			i++
			continue
		}
		s.args = append(s.args, args[i])
	}
	s.args = append(s.args, "-f", path)
	return s, nil
}

func serviceFilesystemExecution(service map[string]any, image runtime.DockerImageExecution) (fsfilter.ExecutionManifest, map[string]any, error) {
	var manifest fsfilter.ExecutionManifest
	stringsAt := func(value any) ([]string, error) {
		values, ok := value.([]any)
		if !ok {
			return nil, errors.New("unresolved service execution command")
		}
		out := make([]string, len(values))
		for i, value := range values {
			var ok bool
			out[i], ok = value.(string)
			if !ok || strings.ContainsRune(out[i], 0) {
				return nil, errors.New("invalid service execution argument")
			}
			// `compose config` escapes dollars for its own next parse. The
			// manifest executes directly, so consume that one escape layer.
			out[i] = strings.ReplaceAll(out[i], "$$", "$")
		}
		return out, nil
	}
	entry, command := slices.Clone(image.Entrypoint), slices.Clone(image.Cmd)
	var err error
	if value := service["entrypoint"]; value != nil {
		entry, err = stringsAt(value)
		command = nil // Compose suppresses image CMD when entrypoint is explicit.
	}
	if err == nil && service["command"] != nil {
		command, err = stringsAt(service["command"])
	}
	if err != nil {
		return manifest, nil, err
	}
	manifest.Command = append(entry, command...)
	if len(manifest.Command) == 0 {
		return manifest, nil, errors.New("service has no execution command")
	}
	health := map[string]any{}
	var test []string
	if h := image.Healthcheck; h != nil {
		test = slices.Clone(h.Test)
		for key, duration := range map[string]time.Duration{"interval": h.Interval, "timeout": h.Timeout, "start_period": h.StartPeriod, "start_interval": h.StartInterval} {
			if duration != 0 {
				health[key] = duration.String()
			}
		}
		if h.Retries != 0 {
			health["retries"] = h.Retries
		}
	}
	if h, ok := service["healthcheck"].(map[string]any); ok {
		for key, value := range h {
			health[key] = value
		}
		if disabled, _ := h["disable"].(bool); disabled {
			return manifest, health, nil
		}
		if value := h["test"]; value != nil {
			test, err = stringsAt(value)
			if err != nil {
				return manifest, nil, err
			}
		}
	}
	if len(test) == 0 || test[0] == "NONE" {
		return manifest, health, nil
	}
	switch test[0] {
	case "CMD":
		manifest.Healthcheck = slices.Clone(test[1:])
	case "CMD-SHELL":
		if len(test) != 2 {
			return manifest, nil, errors.New("invalid shell healthcheck")
		}
		shell := image.Shell
		if len(shell) == 0 {
			shell = []string{"/bin/sh", "-c"}
		}
		manifest.Healthcheck = append(slices.Clone(shell), test[1])
	default:
		return manifest, nil, errors.New("invalid healthcheck execution mode")
	}
	if len(manifest.Healthcheck) == 0 {
		return manifest, nil, errors.New("empty healthcheck command")
	}
	return manifest, health, nil
}

func (s *serviceFilesystemStart) create(ctx context.Context, rt runtime.Runtime, selected []string, force bool, stdout, stderr io.Writer) error {
	if len(s.admissions) == 0 {
		return nil
	}
	for _, a := range s.admissions {
		if a.reused {
			continue
		}
		if a.record.Pending != "" {
			return errors.New("filesystem service creation is already pending")
		}
		a.record.Pending = a.record.Parent + "-service"
		if err := a.persist(); err != nil {
			return err
		}
	}
	args := append(slices.Clone(s.args), "up", "--no-start")
	if force {
		args = append(args, "--force-recreate")
	}
	args = append(args, selected...)
	var detail bytes.Buffer
	code, createErr := rt.RunComposeContext(ctx, nil, stdout, io.MultiWriter(stderr, &detail), args...)
	if code != 0 || createErr != nil {
		return errors.Join(errors.New("filesystem service creation requires reconciliation"), createErr)
	}
	for _, a := range s.admissions {
		ref := a.record.Consumers[len(a.record.Consumers)-1]
		value, present, err := a.docker.InspectContainer(ctx, ref)
		if err != nil || !present {
			return errors.Join(errors.New("filesystem service creation is unconfirmed"), err)
		}
		ref.ID = value.ID
		a.record.Pending = ""
		if err := a.consumer(ref); err != nil {
			return err
		}
		if err := a.verify(ctx, ref, a.workloadImage); err != nil {
			return err
		}
	}
	return nil
}
