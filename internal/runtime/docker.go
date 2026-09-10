package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// Docker binds lifecycle operations to one local endpoint and daemon. It is
// intentionally separate from Runtime: existing open/none commands keep their
// runtime dialect, while restricted launches cannot follow a changed context.
type Docker struct {
	binary, endpoint, clientConfig string
	pluginConfig                   string // captured path, read only during explicit image builds
	env                            []string
	info                           DockerInfo
	launchAllowed                  bool
	closed                         atomic.Bool
	// OnSlowStart, when set, is called once if the daemon has not reported the
	// attached workload started yet. A slow start is not a failure — this is how
	// the operator hears about it instead of watching a silent terminal.
	OnSlowStart func(time.Duration)
}

type DockerInfo struct {
	ID, OSType, Architecture, ServerVersion, KernelVersion string
	SecurityOptions                                        []string
}

const dockerInfoFormat = `{"ID":{{json .ID}},"OSType":{{json .OSType}},"Architecture":{{json .Architecture}},"ServerVersion":{{json .ServerVersion}},"KernelVersion":{{json .KernelVersion}},"SecurityOptions":{{json .SecurityOptions}}}`

var errDockerCommandNotStarted = errors.New("Docker client command was not started")

// BindDocker discovers only when endpoint is empty. Recovery supplies the
// recorded endpoint and daemon ID; neither a missing context nor daemon failure
// permits selecting another runtime. Remote/rootless engines need qualification.
func BindDocker(ctx context.Context, rt Runtime, endpoint, expectedID string) (*Docker, error) {
	return bindDocker(ctx, rt, endpoint, expectedID, expectedID == "")
}

// InspectDocker binds read-only inventory before policy admission determines
// whether a launch needs gateway qualification. It grants no create/start
// authority, including on ordinary rootless or otherwise unqualified engines.
func InspectDocker(ctx context.Context, rt Runtime) (*Docker, error) {
	return bindDocker(ctx, rt, "", "", false)
}

func bindDocker(ctx context.Context, rt Runtime, endpoint, expectedID string, launchAllowed bool) (*Docker, error) {
	if rt.kind() != runtimeDocker || ctx == nil {
		return nil, errors.New("restricted networking requires a local Docker runtime")
	}
	binary, err := exec.LookPath(rt.Name)
	if err != nil {
		return nil, errors.New("Docker executable unavailable")
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		return nil, err
	}
	env := interruptibleProcessEnvironment()
	if env == nil {
		env = os.Environ()
	}
	if endpoint == "" {
		endpoint, err = discoverDockerEndpoint(ctx, binary, env)
		if err != nil {
			return nil, err
		}
	}
	if !validDockerEndpoint(endpoint) {
		return nil, errors.New("restricted networking requires an absolute local unix:// Docker endpoint")
	}
	pluginConfig := ""
	for _, item := range env {
		if value, ok := strings.CutPrefix(item, "DOCKER_CONFIG="); ok {
			pluginConfig = value
		}
	}
	if pluginConfig == "" {
		configHome, homeErr := os.UserHomeDir()
		if homeErr == nil {
			pluginConfig = filepath.Join(configHome, ".docker")
		}
	}
	if pluginConfig != "" {
		// Plugin discovery is build-only. Even an unavailable build config
		// must not prevent exact-endpoint recovery of an existing workload.
		pluginConfig, _ = filepath.Abs(pluginConfig)
	}
	// Docker otherwise injects proxy URLs from the mutable host CLI config into
	// container env, potentially including credentials. Lifecycle calls need no
	// registry login or plugin settings; use an empty private config after discovery.
	clientConfig, err := os.MkdirTemp("", "coop-docker-client-")
	if err != nil {
		return nil, errors.New("private Docker client configuration unavailable")
	}
	d := &Docker{binary: binary, endpoint: endpoint, clientConfig: clientConfig, pluginConfig: pluginConfig, env: boundDockerEnvironment(env), launchAllowed: launchAllowed}
	bound := false
	defer func() {
		if !bound {
			_ = d.Close()
		}
	}()
	info, err := d.readInfo(ctx)
	if err != nil {
		return nil, err
	}
	if expectedID != "" && info.ID != expectedID {
		return nil, errors.New("Docker daemon identity changed; runtime custody remains pending")
	}
	if d.launchAllowed && (info.OSType != "linux" || !slices.Contains([]string{"aarch64", "arm64", "amd64", "x86_64"}, info.Architecture)) {
		return nil, errors.New("Docker platform is not qualified for restricted networking")
	}
	for _, option := range info.SecurityOptions {
		if d.launchAllowed && (strings.Contains(option, "rootless") || strings.Contains(option, "userns")) {
			return nil, errors.New("Docker rootless/user-remapped networking is not qualified")
		}
	}
	d.info = info
	bound = true
	return d, nil
}

// Close removes only the exact empty client directory, never any runtime
// resource. An unexpected file is preserved and reported, not recursively erased.
func (d *Docker) Close() error {
	if d.closed.Swap(true) {
		return nil
	}
	return os.Remove(d.clientConfig)
}

func (d *Docker) Endpoint() string { return d.endpoint }
func (d *Docker) Binary() string   { return d.binary }
func (d *Docker) Info() DockerInfo {
	value := d.info
	value.SecurityOptions = slices.Clone(value.SecurityOptions)
	return value
}

func discoverDockerEndpoint(ctx context.Context, binary string, env []string) (string, error) {
	values := make(map[string]string)
	for _, item := range env {
		key, value, _ := strings.Cut(item, "=")
		values[key] = value
	}
	selected := values["DOCKER_CONTEXT"]
	if selected == "" && values["DOCKER_HOST"] != "" {
		return values["DOCKER_HOST"], nil
	}
	if selected == "" {
		data, err := dockerOutput(ctx, binary, env, 1024, "context", "show")
		if err != nil {
			return "", err
		}
		selected = strings.TrimSpace(string(data))
	}
	if !dockerToken(selected, 128) || strings.HasPrefix(selected, "-") {
		return "", errors.New("Docker context identity unavailable")
	}
	// Passing the context explicitly matters: context inspect without a name
	// can describe the default endpoint despite a selected DOCKER_CONTEXT.
	data, err := dockerOutput(ctx, binary, env, 8192, "context", "inspect", "--format", "{{json .Endpoints.docker}}", selected)
	if err != nil {
		return "", err
	}
	var endpoint struct {
		Host          string
		SkipTLSVerify bool
	}
	if json.Unmarshal(data, &endpoint) != nil || endpoint.SkipTLSVerify {
		return "", errors.New("Docker context endpoint is invalid")
	}
	return endpoint.Host, nil
}

func validDockerEndpoint(value string) bool {
	if len(value) > 4096 {
		return false
	}
	u, err := url.Parse(value)
	return err == nil && u.Scheme == "unix" && u.Host == "" && u.User == nil && u.Opaque == "" && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" &&
		filepath.IsAbs(u.Path) && filepath.Clean(u.Path) == u.Path && u.Path != "/" && !strings.ContainsAny(u.Path, "\x00\r\n")
}

func boundDockerEnvironment(env []string) []string {
	var result []string
	for _, item := range env {
		key, _, _ := strings.Cut(item, "=")
		if strings.HasPrefix(key, "DOCKER_") {
			continue
		}
		result = append(result, item)
	}
	return result
}

func dockerToken(value string, limit int) bool {
	return value != "" && len(value) <= limit && strings.IndexFunc(value, func(r rune) bool { return r < 33 || r > 126 }) < 0
}

func dockerHexID(value string) bool {
	return len(value) == 64 && strings.Trim(value, "0123456789abcdef") == ""
}
func dockerName(value string) bool {
	return value != "" && len(value) <= 128 && !strings.HasPrefix(value, "-") && strings.Trim(value, "abcdefghijklmnopqrstuvwxyz0123456789-_") == ""
}

// Every control call is finite and retains bounded output. Error prose never
// contains runtime stderr: image labels and daemon errors can contain secrets.
func dockerOutput(ctx context.Context, binary string, env []string, limit int, args ...string) ([]byte, error) {
	if ctx == nil || limit <= 0 || limit > 4<<20 {
		return nil, errors.Join(errDockerCommandNotStarted, errors.New("invalid bounded Docker operation"))
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var out dockerBoundedOutput
	out.limit = limit
	cmd := contextCommand(ctx, binary, args...)
	cmd.Env, cmd.Stdout, cmd.Stderr = env, &out, io.Discard
	if err := cmd.Start(); err != nil {
		// Start failed before a client could submit a daemon request. Keep this
		// distinct from a failed Wait, whose external outcome is ambiguous.
		return nil, errors.Join(errDockerCommandNotStarted, ctx.Err())
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("Docker operation failed; outcome unknown")
	}
	if out.overflow {
		return nil, errors.New("Docker observation exceeded its bound; outcome unknown")
	}
	return out.Bytes(), nil
}

type dockerBoundedOutput struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
}

func (w *dockerBoundedOutput) Bytes() []byte { return w.buffer.Bytes() }
func (w *dockerBoundedOutput) Len() int      { return w.buffer.Len() }

func (w *dockerBoundedOutput) Write(data []byte) (int, error) {
	n := min(len(data), max(0, w.limit-w.Len()))
	_, _ = w.buffer.Write(data[:n])
	w.overflow = w.overflow || n < len(data)
	return len(data), nil
}

func (d *Docker) output(ctx context.Context, limit int, args ...string) ([]byte, error) {
	if d.closed.Load() {
		return nil, errors.Join(errDockerCommandNotStarted, errors.New("Docker lifecycle binding is closed"))
	}
	return dockerOutput(ctx, d.binary, d.env, limit, append([]string{"--config", d.clientConfig, "--host", d.endpoint}, args...)...)
}

func (d *Docker) readInfo(ctx context.Context) (DockerInfo, error) {
	data, err := d.output(ctx, 8192, "info", "--format", dockerInfoFormat)
	if err != nil {
		return DockerInfo{}, err
	}
	var info DockerInfo
	if json.Unmarshal(data, &info) != nil || !dockerToken(info.ID, 128) || !dockerToken(info.ServerVersion, 128) || !dockerToken(info.KernelVersion, 128) || len(info.SecurityOptions) > 32 {
		return DockerInfo{}, errors.New("Docker daemon observation is invalid")
	}
	return info, nil
}

func (d *Docker) Verify(ctx context.Context) error {
	info, err := d.readInfo(ctx)
	if err != nil {
		return err
	}
	if info.ID != d.info.ID {
		return errors.New("Docker daemon identity changed; custody remains pending")
	}
	return nil
}

// VerifyLaunch is stricter than cleanup. A recovery-only binding cannot start
// work, while exact cleanup still works after a host/runtime qualification change.
func (d *Docker) VerifyLaunch(ctx context.Context) error {
	if !d.launchAllowed {
		return errors.New("Docker recovery binding cannot authorize runtime creation or start")
	}
	return d.verifyLaunchIdentity(ctx)
}

func (d *Docker) verifyLaunchIdentity(ctx context.Context) error {
	info, err := d.readInfo(ctx)
	if err != nil {
		return err
	}
	if info.ID != d.info.ID || info.OSType != d.info.OSType || info.Architecture != d.info.Architecture || info.ServerVersion != d.info.ServerVersion || info.KernelVersion != d.info.KernelVersion || !slices.Equal(info.SecurityOptions, d.info.SecurityOptions) {
		return errors.New("Docker daemon identity or qualification changed; custody remains pending")
	}
	return nil
}
