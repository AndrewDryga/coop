package box

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/fsfilter"
	"github.com/AndrewDryga/coop/internal/runtime"
)

func filesystemControlOverlap(target string) bool {
	target = path.Clean(target)
	return target == "/" || target == fsfilter.ControlPath || strings.HasPrefix(target, fsfilter.ControlPath+"/")
}

func filesystemImageSupported(image runtime.DockerImageExecution) error {
	if err := filesystemImageVolumesSupported(image); err != nil {
		return err
	}
	// Docker's --health-cmd is CMD-SHELL, and even /bin/sh belongs to the image.
	// It could execute image code before the guard following a VM reboot.
	if image.Healthcheck != nil && len(image.Healthcheck.Test) > 0 && image.Healthcheck.Test[0] != "NONE" {
		return errors.New("image healthcheck requires the existing filesystem backend")
	}
	return nil
}

func filesystemImageVolumesSupported(image runtime.DockerImageExecution) error {
	for target := range image.Volumes {
		if filesystemControlOverlap(target) {
			return errors.New("image volume overlaps filesystem guard control")
		}
	}
	return nil
}

func selectFilesystemAdmission(ctx context.Context, rt runtime.Runtime, cfg *config.Config, spec RunSpec, mounts []Mount, binding *runtime.Docker) (*filesystemAdmission, error) {
	if spec.Login || spec.GradeSnapshot || spec.FormatCorrection || !cfg.NoNewPrivileges && spec.CapturedEgress == nil || rt.Name != "docker" || !filesystemProfileCandidate(spec.Repo) {
		return nil, nil
	}
	if _, err := filteredExtraArgs(cfg.ExtraRunArgs, spec.ExtraArgs); err != nil {
		return nil, nil
	}
	endpoint, daemon := rt.ComposeBinding()
	if binding != nil {
		endpoint, daemon = binding.Endpoint(), binding.Info().ID
	}
	docker, err := runtime.BindDockerForSetup(ctx, rt, endpoint, daemon)
	if err != nil {
		return nil, nil
	}
	defer docker.Close()
	if docker.Info().CgroupDriver != "cgroupfs" || docker.Info().CgroupVersion != "2" {
		return nil, nil
	}
	image, _, err := docker.Image(ctx, spec.Image)
	if err != nil {
		return nil, err
	}
	execution, err := docker.ImageExecution(ctx, image)
	if err != nil {
		return nil, err
	}
	if filesystemImageSupported(execution) != nil {
		return nil, nil
	}
	var roots []string
	for _, m := range mounts {
		if m.Kind == Bind {
			roots = append(roots, m.Source)
		}
	}
	a, err := prepareFilesystemAdmission(ctx, rt, roots, make([][]string, len(roots)), docker)
	if errors.Is(err, errFilesystemUnsupported) && !errors.Is(err, errFilesystemCustodyPending) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a.workloadImage = image
	return a, nil
}

func (a *filesystemAdmission) guarded(ctx context.Context, image string, command, options []string) ([]string, []string, error) {
	execution, err := a.docker.ImageExecution(ctx, image)
	if err != nil {
		return nil, nil, err
	}
	if err := filesystemImageSupported(execution); err != nil {
		return nil, nil, err
	}
	for i := 0; i < len(options); i++ {
		switch options[i] {
		case "--entrypoint", "--cgroup-parent", "--privileged", "--cap-add", "--pid", "--userns", "--cgroupns":
			return nil, nil, fmt.Errorf("runtime option %s conflicts with filesystem protection", options[i])
		case "-v", "--mount":
			kind := options[i]
			i++
			if i >= len(options) {
				return nil, nil, errors.New("incomplete filesystem mount")
			}
			mount := options[i]
			if kind == "--mount" {
				mount, err = bindMountShorthand(mount)
				if err != nil {
					return nil, nil, err
				}
			}
			parts := strings.SplitN(mount, ":", 3)
			if len(parts) < 2 || filesystemControlOverlap(parts[1]) {
				return nil, nil, errors.New("workload mount overlaps filesystem guard control")
			}
		}
	}
	if len(command) == 0 {
		command = execution.Cmd
	}
	manifest := fsfilter.ExecutionManifest{Command: append(slices.Clone(execution.Entrypoint), command...)}
	dir, err := a.writeGuard(ctx, manifest)
	if err != nil {
		return nil, nil, err
	}
	options = slices.Clone(options)
	options = append(options, "--cgroup-parent", "/"+a.record.Parent, "--entrypoint", fsfilter.ControlPath+"/guard", "--mount", networkMount("bind", dir, fsfilter.ControlPath, true))
	return options, []string{"guard", fsfilter.ControlPath + "/manifest.json"}, nil
}

func (a *filesystemAdmission) verify(ctx context.Context, ref runtime.DockerRef, image string) error {
	value, present, err := a.docker.InspectContainer(ctx, ref)
	if err != nil || !present {
		return errors.Join(errors.New("filesystem workload observation unavailable"), err)
	}
	nnp := slices.Contains(value.SecurityOpt, "no-new-privileges") || slices.Contains(value.SecurityOpt, "no-new-privileges=true") || slices.Contains(value.SecurityOpt, "no-new-privileges:true")
	if value.Image != image || value.CgroupParent != "/"+a.record.Parent || !slices.Equal(value.Entrypoint, []string{fsfilter.ControlPath + "/guard"}) || !slices.Equal(value.Cmd, []string{"guard", fsfilter.ControlPath + "/manifest.json"}) || value.Privileged || len(value.CapAdd) != 0 || a.record.Service == "" && !slices.Contains(value.CapDrop, "ALL") || !nnp || value.PidMode != "" || value.UsernsMode != "" || value.CgroupnsMode != "" && value.CgroupnsMode != "private" {
		return errors.New("filesystem workload does not match its protection contract")
	}
	var health []string
	if value.Healthcheck != nil {
		health = value.Healthcheck.Test
	}
	if len(a.record.HealthTest) > 0 && !slices.Equal(health, a.record.HealthTest) || len(a.record.HealthTest) == 0 && len(health) > 0 && health[0] != "NONE" {
		return errors.New("filesystem workload healthcheck bypasses its guard")
	}
	found := false
	for _, mount := range value.Mounts {
		if !filesystemControlOverlap(mount.Destination) {
			continue
		}
		if found || mount.Type != "bind" || mount.Source != filepath.Join(a.dir, "control") || mount.Destination != fsfilter.ControlPath || mount.RW {
			return fmt.Errorf("filesystem control mount changed: got %s %q at %q (writable=%t), expected read-only %q", mount.Type, mount.Source, mount.Destination, mount.RW, filepath.Join(a.dir, "control"))
		}
		found = true
	}
	if !found {
		return errors.New("filesystem control mount is absent")
	}
	return a.revalidate()
}

func (a *filesystemAdmission) run(ctx context.Context, image string, command, options []string, stdin io.Reader, stdout, stderr io.Writer, started func() error) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	options, command, err := a.guarded(ctx, image, command, options)
	if err != nil {
		return -1, err
	}
	ref := runtime.DockerRef{Name: a.record.Parent + "-agent", Labels: map[string]string{LabelKey: LabelBox, filesystemLabel: a.record.Parent}}
	if err := a.consumer(ref); err != nil {
		return -1, err
	}
	id, err := a.create(ctx, runtime.DockerCreate{Ref: ref, Image: image, Options: options, Command: command})
	ref.ID = id
	if err != nil {
		return -1, err
	}
	if err := a.verify(ctx, ref, image); err != nil {
		return -1, err
	}
	return a.docker.StartAttached(ctx, ref, stdin, stdout, stderr, started)
}
