package box

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// EnsureServices brings the repo's sibling services up (compose up -d --wait) so a box can
// reach them by name. It is idempotent — already-running services are a fast no-op — and a
// no-op (nil, nil) when the repo has no compose file. On success it returns the non-empty
// service names in Compose's resolved order, from the same project and file selection it
// started. Progress is written to stdout/stderr; the caller decides where to point them and
// gates on a compose-capable runtime (Apple `container` has no compose). Shared by `coop up`
// and box.Run's auto-start.
func EnsureServices(rt runtime.Runtime, workspace, policyRepo string, stdout, stderr io.Writer, exposedRoots ...string) ([]string, error) {
	p, err := project.Load(policyRepo)
	if err != nil {
		return nil, err
	}
	return EnsureServicesFile(rt, workspace, ComposeFileAt(workspace, p.ComposeRel()), stdout, stderr, append([]string{policyRepo}, exposedRoots...)...)
}

// EnsureServicesFile is the explicit-file form used by trusted review policy. The file must live
// inside workspace; ValidateComposeFile enforces that its bind mounts cannot escape that boundary.
func EnsureServicesFile(rt runtime.Runtime, workspace, file string, stdout, stderr io.Writer, exposedRoots ...string) ([]string, error) {
	started, err := startServicesFile(rt, workspace, file, stdout, stderr, false, exposedRoots...)
	return started.names, err
}

type startedServices struct {
	names []string
	ports []ServicePort
}

func startServicesFile(rt runtime.Runtime, workspace, file string, stdout, stderr io.Writer, repoReadOnly bool, exposedRoots ...string) (startedServices, error) {
	if file == "" {
		return startedServices{}, nil
	}
	// coop runs this file on the HOST daemon, so validate it first: an in-box agent may author it
	// (the compose path is no longer shadowed), but the host refuses anything that reaches outside a
	// repo-scoped, loopback-only container. The specific violation rides out to `coop up` / the
	// auto-up warning, so a refused file names exactly why.
	args, cleanup, err := snapshotComposeArgs(workspace, file, repoReadOnly, exposedRoots...)
	if err != nil {
		return startedServices{}, fmt.Errorf("refusing to run %s: %w", filepath.Base(file), err)
	}
	defer cleanup()
	// Publish each `expose`d sidecar port to its stable per-workspace host port via a merged
	// override (the base file's `expose` publishes nothing, so this adds the only host mapping).
	ports := servicePortsWithArgs(rt, workspace, args)
	if sp := ports; len(sp) > 0 {
		override, cleanup, err := writeServiceOverride(sp, workspace, exposedRoots...)
		if err != nil {
			return startedServices{}, err
		}
		defer cleanup()
		args = append(args, "-f", override)
	}
	services, err := resolvedComposeServices(rt, args, stderr)
	if err != nil {
		return startedServices{}, err
	}
	upArgs := append(append([]string(nil), args...), "up", "-d", "--wait", "--remove-orphans")
	if err := runCompose(rt, stdout, stderr, "up", upArgs); err != nil {
		return startedServices{}, err
	}
	return startedServices{names: services, ports: ports}, nil
}

// Snapshot approved bytes outside the writable workspace. All commands in one operation use
// this file; the explicit project directory preserves relative binds and ownership labels.
func snapshotComposeArgs(workspace, file string, repoReadOnly bool, exposedRoots ...string) ([]string, func(), error) {
	data, err := readValidatedCompose(file, workspace, repoReadOnly)
	if err != nil {
		return nil, nil, err
	}
	abs, err := filepath.Abs(file)
	if err != nil {
		return nil, nil, err
	}
	dir, err := privateWorkspaceTempDir(workspace, "coop-compose-", exposedRoots...)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	path := filepath.Join(dir, "compose.yml")
	if err := os.WriteFile(path, data, 0o400); err != nil {
		cleanup()
		return nil, nil, err
	}
	return []string{"compose", "-p", ComposeProject(workspace),
		"--project-directory", filepath.Dir(abs), "--env-file", os.DevNull, "-f", path}, cleanup, nil
}

func privateWorkspaceTempDir(workspace, pattern string, exposedRoots ...string) (string, error) {
	absParent, err := filepath.Abs(os.TempDir())
	if err != nil {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(absParent)
	if err != nil {
		return "", err
	}
	for _, root := range append([]string{workspace}, exposedRoots...) {
		if root == "" {
			continue
		}
		abs, err := filepath.Abs(root)
		if err != nil {
			return "", err
		}
		inside, err := futurePathWithin(abs, parent)
		if err != nil {
			return "", err
		}
		if inside {
			return "", errors.New("temporary directory must resolve outside agent-exposed directories — choose an external TMPDIR")
		}
	}
	// Resolve before allocating: a TMPDIR alias inside the repo must not remain in any
	// later write/read/cleanup path, even when its current target is outside the repo.
	return os.MkdirTemp(parent, pattern)
}

func resolvedComposeServices(rt runtime.Runtime, args []string, stderr io.Writer) ([]string, error) {
	var stdout bytes.Buffer
	configArgs := append(append([]string(nil), args...), "config", "--services")
	if err := runCompose(rt, &stdout, stderr, "config --services", configArgs); err != nil {
		return nil, err
	}
	var services []string
	for _, line := range strings.Split(stdout.String(), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			services = append(services, name)
		}
	}
	if len(services) == 0 {
		return nil, errors.New("compose config --services returned no services")
	}
	return services, nil
}

// DownServices stops the current workspace's hashed Compose project. Volumes are optional.
func DownServices(rt runtime.Runtime, workspace, policyRepo string, volumes bool, stdout, stderr io.Writer, exposedRoots ...string) error {
	p, err := project.Load(policyRepo)
	if err != nil {
		return err
	}
	file := ComposeFileAt(workspace, p.ComposeRel())
	if file == "" {
		return nil
	}
	return DownServicesFile(rt, workspace, file, volumes, stdout, stderr, append([]string{policyRepo}, exposedRoots...)...)
}

// DownServicesFile is the explicit-file counterpart to EnsureServicesFile. Review runs use it to
// remove their short-lived project, network, and volumes before the disposable candidate goes away.
func DownServicesFile(rt runtime.Runtime, workspace, file string, volumes bool, stdout, stderr io.Writer, exposedRoots ...string) error {
	if file == "" {
		return nil
	}
	args, cleanup, err := snapshotComposeArgs(workspace, file, false, exposedRoots...)
	if err != nil {
		return fmt.Errorf("refusing to stop %s: %w", filepath.Base(file), err)
	}
	defer cleanup()
	args = append(args, "down", "--remove-orphans")
	if volumes {
		args = append(args, "--volumes")
	}
	return runCompose(rt, stdout, stderr, "down", args)
}

const (
	composeProjectLabel    = "com.docker.compose.project"
	composeWorkingDirLabel = "com.docker.compose.project.working_dir"
)

// StopSessionServices removes only the current workspace's Compose containers while preserving
// its volumes. It uses immutable runtime ownership labels instead of the workspace's mutable
// Compose file, so interrupted agent edits cannot prevent cleanup. The next turn starts services
// again through EnsureServices.
func StopSessionServices(ctx context.Context, rt runtime.Runtime, workspace, policyRepo string) error {
	// Cleanup must survive a policy typo introduced after the services started. ComposePath's
	// default-on-error behavior preserves the existing immutable-label cleanup path; launch and
	// ordinary service control still reject the invalid policy before touching the runtime.
	composeDir := filepath.Dir(filepath.Join(workspace, filepath.FromSlash(project.ComposePath(policyRepo))))
	_, err := rt.RemoveByLabels(ctx, map[string]string{
		composeProjectLabel:    ComposeProject(workspace),
		composeWorkingDirLabel: filepath.Clean(composeDir),
	})
	if err != nil {
		return fmt.Errorf("stop session services: %w", err)
	}
	return nil
}

func runCompose(rt runtime.Runtime, stdout, stderr io.Writer, action string, args []string) error {
	code, err := rt.Run(nil, stdout, stderr, args...)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("compose %s exited with code %d", action, code)
	}
	return nil
}

// autoUpServices reports whether box.Run should auto-start sibling services before launching a
// box: the COOP_AUTO_UP toggle is on (default), the box joins the services network (so it could
// reach them), it isn't offline (COOP_EGRESS=none, where there's nothing to reach), and the
// runtime supports compose — Apple `container` does not. Whether a compose file actually exists
// is checked separately, by EnsureServices.
func autoUpServices(cfg *config.Config, spec RunSpec, rtName string) bool {
	return cfg.AutoUp && spec.Network && cfg.Egress == "open" && rtName != "container"
}
