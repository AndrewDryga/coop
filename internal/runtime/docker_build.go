package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// DockerBuild describes an explicit local candidate build, not a qualification.
// There are no implicit environment-valued args, external contexts or exporters.
type DockerBuild struct {
	Tag, Platform string
	Args, Labels  map[string]string
}

type DockerBuiltImage struct {
	ID, OS, Architecture string
	Labels               map[string]string
}

func (s DockerBuild) valid() bool {
	repository, tag, ok := strings.Cut(s.Tag, ":")
	if !ok || !dockerName(repository) || !dockerName(tag) || !slices.Contains([]string{"linux/amd64", "linux/arm64"}, s.Platform) || len(s.Args) > 16 || !(DockerRef{Name: repository, Labels: s.Labels}).valid(false) {
		return false
	}
	for key, value := range s.Args {
		if key == "" || len(key) > 64 || strings.Trim(key, "ABCDEFGHIJKLMNOPQRSTUVWXYZ_0123456789") != "" || strings.ContainsAny(key[:1], "0123456789") || value != "" && !dockerToken(value, 2048) {
			return false
		}
	}
	return true
}

// BuildImage accepts only a bounded materialized context and direct file
// descriptors. Arbitrary Readers/Writers can leave uninterruptible os/exec copy
// goroutines behind even after a build's process group has been killed.
//
// A canceled or failed build has no accepted result, even if it wrote an ID.
// Reaping its client does not prove the daemon retained no build/cache state.
func (d *Docker) BuildImage(ctx context.Context, spec DockerBuild, contextTar []byte, stdout, stderr *os.File) (result DockerBuiltImage, err error) {
	if ctx == nil || !spec.valid() || len(contextTar) == 0 || len(contextTar) > 4<<20 {
		return result, errors.New("invalid bounded Docker candidate build")
	}
	if err = d.VerifyLaunch(ctx); err != nil {
		return result, err
	}
	if spec.Platform != "linux/"+Architecture(d.info.Architecture) {
		return result, errors.New("candidate build requires the bound daemon's native platform")
	}
	buildx, err := d.buildxExecutable(ctx)
	if err != nil {
		return result, err
	}
	scratch, err := os.MkdirTemp("", "coop-docker-build-")
	if err != nil {
		return result, errors.New("private Docker build state unavailable")
	}
	root, err := os.OpenRoot(scratch)
	if err != nil {
		return result, errors.Join(err, os.Remove(scratch))
	}
	defer func() {
		// Only this build's two declared children may be removed. Unexpected
		// siblings remain visible as a cleanup error, never a recursive host reap.
		cleanup := root.RemoveAll("client")
		if removeErr := root.Remove("image.id"); !errors.Is(removeErr, os.ErrNotExist) {
			cleanup = errors.Join(cleanup, removeErr)
		}
		anchored, anchorErr := root.Stat(".")
		current, currentErr := os.Lstat(scratch)
		if anchorErr != nil || currentErr != nil || !os.SameFile(anchored, current) {
			cleanup = errors.Join(cleanup, errors.New("private Docker build directory identity changed"))
		} else {
			cleanup = errors.Join(cleanup, os.Remove(scratch))
		}
		cleanup = errors.Join(cleanup, root.Close())
		if cleanup != nil {
			result = DockerBuiltImage{}
			err = errors.Join(err, fmt.Errorf("private Docker build state cleanup incomplete at %q: %w", scratch, cleanup))
		}
	}()
	if err := root.Mkdir("client", 0700); err != nil {
		return result, err
	}
	if err := root.Mkdir("client/cli-plugins", 0700); err != nil {
		return result, err
	}
	if err := root.Mkdir("client/tmp", 0700); err != nil {
		return result, err
	}
	if err := root.Symlink(buildx, "client/cli-plugins/docker-buildx"); err != nil {
		return result, err
	}
	clientConfig := filepath.Join(scratch, "client")
	args := []string{"--config", clientConfig, "--host", d.endpoint, "build", "--builder", "default", "--platform", spec.Platform, "--progress", "plain", "--iidfile", filepath.Join(scratch, "image.id"), "--tag", spec.Tag}
	for _, key := range slices.Sorted(maps.Keys(spec.Args)) {
		args = append(args, "--build-arg", key+"="+spec.Args[key])
	}
	args = append(args, dockerLabelArgs(spec.Labels)...)
	cmd := contextCommand(ctx, d.binary, append(args, "-")...)
	cmd.Dir = scratch
	cmd.Env = dockerBuildEnvironment(d.binary, clientConfig)
	cmd.Stdin = bytes.NewReader(contextTar)
	if stdout != nil {
		cmd.Stdout = stdout
	}
	if stderr != nil {
		cmd.Stderr = stderr
	}
	if runErr := cmd.Run(); runErr != nil || ctx.Err() != nil {
		code := -1
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}
		return result, errors.Join(fmt.Errorf("Docker candidate build did not complete (client exit %d); no image accepted", code), ctx.Err())
	}
	if err := d.VerifyLaunch(ctx); err != nil {
		return result, err
	}
	id, err := readDockerBuildID(root)
	if err != nil {
		return result, err
	}
	// Inspect the build result, never its replaceable convenience tag.
	data, err := d.output(ctx, 64<<10, "image", "inspect", "--format", `{"ID":{{json .Id}},"OS":{{json .Os}},"Architecture":{{json .Architecture}},"Labels":{{json (index .Config "Labels")}}}`, id)
	if err != nil {
		return result, err
	}
	var observed DockerBuiltImage
	if json.Unmarshal(data, &observed) != nil || observed.ID != id || observed.OS+"/"+observed.Architecture != spec.Platform || len(observed.Labels) > 256 {
		return result, errors.New("Docker candidate identity or platform mismatch")
	}
	for key, value := range spec.Labels {
		if observed.Labels[key] != value {
			return result, errors.New("Docker candidate build labels mismatch")
		}
	}
	if err := d.VerifyLaunch(ctx); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	return observed, nil
}

func (d *Docker) buildxExecutable(ctx context.Context) (string, error) {
	// Some distributions install Buildx only in the owner's plugin directory.
	// Inspect metadata with that config, then expose only the resolved binary;
	// no proxy, login, builder or registry setting crosses into the build config.
	if d.pluginConfig == "" {
		return "", errors.New("Docker build plugin configuration path unavailable")
	}
	data, err := dockerOutput(ctx, d.binary, d.env, 64<<10, "--config", d.pluginConfig, "--host", d.endpoint, "info", "--format", "{{json .ClientInfo.Plugins}}")
	if err != nil {
		return "", err
	}
	var plugins []struct{ Name, Path, Err string }
	if json.Unmarshal(data, &plugins) != nil || len(plugins) > 64 {
		return "", errors.New("invalid Docker plugin observation")
	}
	selected := ""
	for _, plugin := range plugins {
		if plugin.Name != "buildx" {
			continue
		}
		if selected != "" || plugin.Err != "" || !filepath.IsAbs(plugin.Path) || len(plugin.Path) > 4096 || strings.ContainsAny(plugin.Path, "\x00\r\n") {
			return "", errors.New("invalid Docker Buildx executable")
		}
		selected = plugin.Path
	}
	if selected == "" {
		return "", errors.New("Docker Buildx executable unavailable")
	}
	selected, err = filepath.EvalSymlinks(selected)
	if err != nil {
		return "", errors.New("Docker Buildx executable unavailable")
	}
	info, err := os.Stat(selected)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return "", errors.New("Docker Buildx is not a regular executable")
	}
	return selected, nil
}

func dockerBuildEnvironment(binary, clientConfig string) []string {
	// Buildx has controls beyond DOCKER_* (source policies, remote builders,
	// source epoch, Git metadata). Start from a finite host-process environment.
	return []string{"PATH=" + filepath.Dir(binary) + ":/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + clientConfig, "TMPDIR=" + filepath.Join(clientConfig, "tmp"), "LANG=C", "LC_ALL=C", "DOCKER_BUILDKIT=1", "BUILDX_CONFIG=" + filepath.Join(clientConfig, "buildx")}
}

func readDockerBuildID(root *os.Root) (string, error) {
	f, err := root.OpenFile("image.id", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", errors.New("Docker build image ID unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 72 {
		return "", errors.New("invalid Docker build image ID file")
	}
	data, err := io.ReadAll(io.LimitReader(f, 73))
	id := strings.TrimSuffix(string(data), "\n")
	if err != nil || len(data) > 72 || !strings.HasPrefix(id, "sha256:") || !dockerHexID(strings.TrimPrefix(id, "sha256:")) {
		return "", errors.New("invalid Docker build image ID")
	}
	return id, nil
}
