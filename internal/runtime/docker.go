package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
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
	return bindDocker(ctx, rt, rt.composeEndpoint, rt.composeDaemon, false)
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

// NamedVolumeIdentity is the inspected object behind an operator-approved volume name.
// A replacement under the same name needs a fresh approval.
type NamedVolumeIdentity struct {
	Name, CreatedAt, Mountpoint string
}

// InspectNamedVolume accepts only ordinary local storage, never a plugin or a
// bind-backed local-driver volume hidden behind a friendly name.
func (d *Docker) InspectNamedVolume(ctx context.Context, name string) (NamedVolumeIdentity, bool, error) {
	if !volumeName(name) {
		return NamedVolumeIdentity{}, false, errors.New("invalid selected Docker volume name")
	}
	if err := d.Verify(ctx); err != nil {
		return NamedVolumeIdentity{}, false, err
	}
	definition, err := (volumeReader{runtimeDocker, d.output}).readVolumeDefinition(ctx, name)
	if err != nil {
		absent, checkErr := d.volumeAbsent(ctx, name)
		if checkErr == nil && absent {
			return NamedVolumeIdentity{}, false, nil
		}
		return NamedVolumeIdentity{}, false, errors.Join(err, checkErr)
	}
	if definition.Driver != "local" || definition.Scope != "local" || len(definition.Options) != 0 ||
		definition.CreatedAt == "" || !filepath.IsAbs(definition.Mountpoint) || filepath.Clean(definition.Mountpoint) != definition.Mountpoint {
		return NamedVolumeIdentity{}, false, errors.New("Docker volume is not plain local storage; use a plain volume or an explicit repository bind")
	}
	if err := d.Verify(ctx); err != nil {
		return NamedVolumeIdentity{}, false, err
	}
	return NamedVolumeIdentity{Name: name, CreatedAt: definition.CreatedAt, Mountpoint: definition.Mountpoint}, true, nil
}

// CreatePlainNamedVolume fills only a confirmed absence on the bound daemon;
// an intervening conflicting creation fails during the mandatory reinspection.
func (d *Docker) CreatePlainNamedVolume(ctx context.Context, name string) (NamedVolumeIdentity, error) {
	if _, present, err := d.InspectNamedVolume(ctx, name); err != nil || present {
		return NamedVolumeIdentity{}, errors.Join(err, errors.New("Docker volume already exists or cannot be inspected"))
	}
	if _, err := d.output(ctx, 1024, "volume", "create", "--driver", "local", name); err != nil {
		return NamedVolumeIdentity{}, err
	}
	identity, present, err := d.InspectNamedVolume(ctx, name)
	if err != nil || !present {
		return NamedVolumeIdentity{}, errors.Join(err, errors.New("created Docker volume could not be verified"))
	}
	return identity, nil
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
	// %-escapes would let the decoded path checked here differ from the literal one dialed, and the
	// docker CLI trims surrounding whitespace that the identity request would keep
	if len(value) > 4096 || strings.Contains(value, "%") || strings.TrimSpace(value) != value {
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

// read runs one finite Docker command and hands its stdout to consume as it
// arrives. Unlike output it retains nothing: a file inside an image is far
// larger than any bounded observation, so the consumer keeps a digest instead of
// the bytes. The consumer pulls, so no copy goroutine outlives the call.
func (d *Docker) read(ctx context.Context, timeout time.Duration, consume func(io.Reader) error, args ...string) error {
	if d.closed.Load() {
		return errors.Join(errDockerCommandNotStarted, errors.New("Docker lifecycle binding is closed"))
	}
	if ctx == nil || consume == nil {
		return errors.Join(errDockerCommandNotStarted, errors.New("invalid bounded Docker read"))
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := contextCommand(ctx, d.binary, append([]string{"--config", d.clientConfig, "--host", d.endpoint}, args...)...)
	cmd.Env, cmd.Stderr = d.env, io.Discard
	stream, err := cmd.StdoutPipe()
	if err != nil {
		return errors.Join(errDockerCommandNotStarted, err)
	}
	if err := cmd.Start(); err != nil {
		return errors.Join(errDockerCommandNotStarted, ctx.Err())
	}
	consumeErr := consume(stream)
	// A consumer that stopped early leaves the rest of the stream unread. Kill
	// the client rather than drain output nobody wants; a satisfied consumer has
	// already read to EOF, so this drain returns at once.
	if consumeErr != nil {
		cancel()
	} else if n, _ := io.Copy(io.Discard, io.LimitReader(stream, 1<<20)); n > 0 {
		consumeErr = errors.New("Docker read left unclaimed output")
		cancel()
	}
	waitErr := cmd.Wait()
	if consumeErr != nil {
		return consumeErr
	}
	if waitErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("Docker operation failed; outcome unknown")
	}
	return nil
}

// readInfo asks the daemon who it is: GET /info on the bound socket, the request the docker CLI
// would send there for `docker info`. A filtered launch asks 74 times, and a CLI process for each
// cost about 40 ms where the request itself takes 4.
func (d *Docker) readInfo(ctx context.Context) (DockerInfo, error) {
	if d.closed.Load() {
		return DockerInfo{}, errors.Join(errDockerCommandNotStarted, errors.New("Docker lifecycle binding is closed"))
	}
	if ctx == nil {
		return DockerInfo{}, errors.Join(errDockerCommandNotStarted, errors.New("invalid bounded Docker operation"))
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// The socket is the endpoint's literal bytes after unix://, as the docker CLI reads it for every
	// other operation: a URL-decoded path could name a different socket, and so another daemon.
	socket, ok := strings.CutPrefix(d.endpoint, "unix://")
	if !ok || !validDockerEndpoint(d.endpoint) {
		return DockerInfo{}, errors.Join(errDockerCommandNotStarted, errors.New("Docker endpoint is not a local socket"))
	}
	client := http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "unix", socket)
			},
			DisableKeepAlives: true,
		},
		// the Docker client follows no redirect, and neither does this
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/info", nil)
	if err != nil {
		return DockerInfo{}, errors.Join(errDockerCommandNotStarted, err)
	}
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return DockerInfo{}, ctx.Err()
		}
		return DockerInfo{}, errors.New("Docker daemon observation failed")
	}
	defer response.Body.Close()
	// a daemon's whole /info runs to tens of KiB; its plugins and registries are not read
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if err != nil && ctx.Err() != nil {
		return DockerInfo{}, ctx.Err()
	}
	if err != nil || response.StatusCode != http.StatusOK {
		return DockerInfo{}, errors.New("Docker daemon observation failed")
	}
	if len(data) > 1<<20 {
		return DockerInfo{}, errors.New("Docker observation exceeded its bound; outcome unknown")
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

// RunWorkload keeps a joining workload on its namespace owner's endpoint and
// empty private CLI config, rather than re-reading mutable host proxy settings.
func (d *Docker) RunWorkload(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) (int, error) {
	return d.runWorkload(ctx, false, stdin, stdout, stderr, args...)
}

// RunWorkloadInterruptible preserves the same binding while supervising cancellation.
func (d *Docker) RunWorkloadInterruptible(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) (int, error) {
	return d.runWorkload(ctx, true, stdin, stdout, stderr, args...)
}

func (d *Docker) runWorkload(ctx context.Context, interruptible bool, stdin io.Reader, stdout, stderr io.Writer, args ...string) (int, error) {
	if len(args) == 0 || args[0] != "run" {
		return -1, errors.New("invalid bound Docker workload")
	}
	if err := d.VerifyLaunch(ctx); err != nil {
		return -1, err
	}
	argv := append([]string{"--config", d.clientConfig, "--host", d.endpoint}, args...)
	cmd := exec.Command(d.binary, argv...)
	cmd.Env = slices.Clone(d.env)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	if !interruptible {
		return exitCode(d.binary, cmd.Run())
	}
	if attachedToTerminal(stdin, stdout, stderr) {
		return runForegroundCommand(ctx, cmd)
	}
	return runInterruptibleCommand(ctx, cmd)
}

func (d *Docker) boundContainerIDs(ctx context.Context, filters ...string) ([]string, error) {
	if err := d.Verify(ctx); err != nil {
		return nil, err
	}
	args := []string{"container", "ls", "--all", "--no-trunc", "--format", "{{.ID}}"}
	for _, filter := range filters {
		args = append(args, "--filter", filter)
	}
	data, err := d.output(ctx, 64<<10, args...)
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(string(data))
	for _, id := range ids {
		if !dockerHexID(id) {
			return nil, errors.New("invalid exact Docker container inventory")
		}
	}
	return ids, d.Verify(ctx)
}

// RemoveByLabels confirms absence of each selected immutable ID on this daemon.
// A concurrent --rm is success only when exact absence can be observed.
func (d *Docker) RemoveByLabels(ctx context.Context, labels map[string]string) (int, error) {
	if len(labels) == 0 || len(labels) > 32 {
		return 0, errors.New("invalid Docker cleanup labels")
	}
	keys := make([]string, 0, len(labels))
	for key, value := range labels {
		if !dockerToken(key, 128) || strings.Contains(key, "=") || !dockerToken(value, 512) {
			return 0, errors.New("invalid Docker cleanup label")
		}
		keys = append(keys, key)
	}
	slices.Sort(keys)
	filters := make([]string, 0, len(keys))
	for _, key := range keys {
		filters = append(filters, "label="+key+"="+labels[key])
	}
	ids, err := d.boundContainerIDs(ctx, filters...)
	if err != nil {
		return 0, err
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	removed := 0
	for _, id := range ids {
		if err := d.Verify(ctx); err != nil {
			return removed, err
		}
		_, removeErr := d.output(ctx, 1024, "container", "rm", "--force", id)
		for {
			remaining, err := d.boundContainerIDs(ctx, "id="+id)
			if err != nil {
				return removed, errors.Join(removeErr, err)
			}
			if !slices.Contains(remaining, id) {
				removed++
				break
			}
			select {
			case <-ctx.Done():
				return removed, errors.Join(removeErr, ctx.Err())
			case <-ticker.C:
			}
		}
	}
	return removed, nil
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
