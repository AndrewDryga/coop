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
	file := ComposeFile(workspace, policyRepo)
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
	if err := reconcileLegacyServices(rt, workspace, file, stdout, stderr); err != nil {
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

// DownServices stops the current workspace's hashed Compose project and reconciles a positively
// owned basename-era project. Volumes are optional for the current project only: legacy volumes
// carry no workspace ownership label, so Coop never removes them automatically.
func DownServices(rt runtime.Runtime, workspace, policyRepo string, volumes bool, stdout, stderr io.Writer) error {
	file := ComposeFile(workspace, policyRepo)
	if file == "" {
		return nil
	}
	args := []string{"compose", "-p", ComposeProject(workspace), "-f", file, "down", "--remove-orphans"}
	if volumes {
		args = append(args, "--volumes")
	}
	currentErr := runCompose(rt, stdout, stderr, "down", args)
	legacyErr := reconcileLegacyServices(rt, workspace, file, stdout, stderr)
	return errors.Join(currentErr, legacyErr)
}

const (
	composeProjectLabel    = "com.docker.compose.project"
	composeWorkingDirLabel = "com.docker.compose.project.working_dir"
)

// reconcileLegacyServices removes the old basename-only Compose project only after every one of
// its containers proves it belongs to this workspace. The current validated file is enough for
// `compose down --remove-orphans`; Compose addresses the legacy stack by project label and creates
// nothing. Its volumes are intentionally retained because they have no path ownership labels.
func reconcileLegacyServices(rt runtime.Runtime, workspace, file string, stdout, stderr io.Writer) error {
	owned, reason, err := legacyComposeOwnership(rt, workspace)
	if err != nil {
		return fmt.Errorf("inspect legacy Compose project: %w", err)
	}
	if !owned {
		if reason != "" && stderr != nil {
			fmt.Fprintf(stderr, "legacy Compose project %s left unchanged: %s\n", ServicesProject(workspace), reason)
		}
		return nil
	}
	proj := ServicesProject(workspace)
	args := []string{"compose", "-p", proj, "-f", file, "down", "--remove-orphans"}
	if err := runCompose(rt, stdout, stderr, "legacy down", args); err != nil {
		return err
	}
	if stdout != nil {
		fmt.Fprintf(stdout, "removed legacy Compose project %s (volumes preserved)\n", proj)
	}
	return nil
}

// legacyComposeOwnership returns owned only for a non-empty, uniformly owned container set.
// An ambiguous set is a warning reason, while runtime query/inspect failures are errors.
func legacyComposeOwnership(rt runtime.Runtime, workspace string) (owned bool, reason string, err error) {
	proj := ServicesProject(workspace)
	items, err := rt.LabelsByLabel(context.Background(), composeProjectLabel, proj)
	if err != nil {
		return false, "", err
	}
	if len(items) == 0 {
		return false, "", nil
	}
	for _, labels := range items {
		if labels[composeProjectLabel] != proj {
			return false, "a matching container has a different project label", nil
		}
		workingDir := labels[composeWorkingDirLabel]
		if workingDir == "" {
			return false, "a matching container has no Compose working-directory label", nil
		}
		if !pathInsideWorkspace(workspace, workingDir) {
			return false, fmt.Sprintf("working directory %q is outside this workspace", workingDir), nil
		}
	}
	return true, "", nil
}

func pathInsideWorkspace(workspace, candidate string) bool {
	if !filepath.IsAbs(candidate) {
		return false
	}
	root := canonicalWorkspace(workspace)
	path := canonicalWorkspace(candidate)
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !filepath.IsAbs(rel) &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator))
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
