package box

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
func EnsureServices(rt runtime.Runtime, workspace, policyRepo string, stdout, stderr io.Writer) ([]string, error) {
	return EnsureServicesFile(rt, workspace, ComposeFile(workspace, policyRepo), stdout, stderr)
}

// EnsureServicesFile is the explicit-file form used by trusted review policy. The file must live
// inside workspace; ValidateComposeFile enforces that its bind mounts cannot escape that boundary.
func EnsureServicesFile(rt runtime.Runtime, workspace, file string, stdout, stderr io.Writer) ([]string, error) {
	if file == "" {
		return nil, nil
	}
	// coop runs this file on the HOST daemon, so validate it first: an in-box agent may author it
	// (the compose path is no longer shadowed), but the host refuses anything that reaches outside a
	// repo-scoped, loopback-only container. The specific violation rides out to `coop up` / the
	// auto-up warning, so a refused file names exactly why.
	if err := ValidateComposeFile(file, workspace); err != nil {
		return nil, fmt.Errorf("refusing to run %s: %w", filepath.Base(file), err)
	}
	proj := ComposeProject(workspace)
	args := []string{"compose", "-p", proj, "-f", file}
	// Publish each `expose`d sidecar port to its stable per-workspace host port via a merged
	// override (the base file's `expose` publishes nothing, so this adds the only host mapping).
	if sp := ServicePorts(rt, workspace, file); len(sp) > 0 {
		override, cleanup, err := writeServiceOverride(sp)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		args = append(args, "-f", override)
	}
	services, err := resolvedComposeServices(rt, args, stderr)
	if err != nil {
		return nil, err
	}
	upArgs := append(append([]string(nil), args...), "up", "-d", "--wait", "--remove-orphans")
	if err := runCompose(rt, stdout, stderr, "up", upArgs); err != nil {
		return nil, err
	}
	return services, nil
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
func DownServices(rt runtime.Runtime, workspace, policyRepo string, volumes bool, stdout, stderr io.Writer) error {
	file := ComposeFile(workspace, policyRepo)
	if file == "" {
		return nil
	}
	return DownServicesFile(rt, workspace, file, volumes, stdout, stderr)
}

// DownServicesFile is the explicit-file counterpart to EnsureServicesFile. Review runs use it to
// remove their short-lived project, network, and volumes before the disposable candidate goes away.
func DownServicesFile(rt runtime.Runtime, workspace, file string, volumes bool, stdout, stderr io.Writer) error {
	if file == "" {
		return nil
	}
	if err := ValidateComposeFile(file, workspace); err != nil {
		return fmt.Errorf("refusing to stop %s: %w", filepath.Base(file), err)
	}
	args := []string{"compose", "-p", ComposeProject(workspace), "-f", file, "down", "--remove-orphans"}
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
