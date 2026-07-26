//go:build boxruntimee2e

package box

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
)

const runtimeInitTestImage = "alpine:3.21"

func TestRuntimeEntrypointDescendantSupervision(t *testing.T) {
	rt := runtimeInitTestRuntime(t)
	image := buildRuntimeEntrypointImage(t, rt)
	repo := runtimeInitTestRepo(t)
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "none"}

	run := func(command string, extra ...string) (int, error) {
		t.Helper()
		return Run(cfg, rt, RunSpec{
			Image: image, Repo: repo, Workdir: "/workspace", Batch: true, Quiet: true,
			Cmd: []string{"sh", "-c", command}, SuperviseDescendants: true, ExtraArgs: extra,
		})
	}

	t.Run("clean provider exit stays successful", func(t *testing.T) {
		code, err := run("exit 0", "-e", "COOP_DESCENDANT_TIMEOUT=1")
		if err != nil || code != 0 {
			t.Fatalf("clean supervised provider = exit %d, err %v; want exit 0, nil", code, err)
		}
	})

	t.Run("double setsid descendant drains", func(t *testing.T) {
		code, err := run("setsid setsid sh -c 'sleep 1; echo drained > /workspace/drained' &", "-e", "COOP_DESCENDANT_TIMEOUT=5")
		if err != nil || code != DescendantsDrainedExit {
			t.Fatalf("drained descendant = exit %d, err %v; want exit %d, nil", code, err, DescendantsDrainedExit)
		}
		awaitRuntimeInitMarker(t, filepath.Join(repo, "drained"), "drained\n")
	})

	t.Run("term resistant descendant times out and is killed", func(t *testing.T) {
		code, err := run("setsid setsid sh -c 'trap \"echo term > /workspace/term\" TERM; while :; do sleep 1; done' &", "-e", "COOP_DESCENDANT_TIMEOUT=1")
		if err != nil || code != DescendantsTimedOutExit {
			t.Fatalf("timed-out descendant = exit %d, err %v; want exit %d, nil", code, err, DescendantsTimedOutExit)
		}
		awaitRuntimeInitMarker(t, filepath.Join(repo, "term"), "term\n")
	})

	t.Run("forwarder does not delay a failed provider", func(t *testing.T) {
		start := time.Now()
		code, err := run("exit 7", "-e", "COOP_FORWARD=45678:missing:45678", "-e", "COOP_DESCENDANT_TIMEOUT=1")
		if err != nil || code != 7 {
			t.Fatalf("failed provider with forwarder = exit %d, err %v; want exit 7, nil", code, err)
		}
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Fatalf("forwarder delayed failed provider for %s", elapsed)
		}
	})

	t.Run("failed provider terminates detached descendants", func(t *testing.T) {
		command := "setsid setsid sh -c 'trap \"echo killed > /workspace/failed-killed; exit 0\" TERM; echo ready > /workspace/failed-ready; while :; do sleep 1; done' & " +
			"while [ ! -e /workspace/failed-ready ]; do :; done; exit 7"
		code, err := run(command, "-e", "COOP_DESCENDANT_TIMEOUT=1")
		if err != nil || code != 7 {
			t.Fatalf("failed provider with descendant = exit %d, err %v; want exit 7, nil", code, err)
		}
		awaitRuntimeInitMarker(t, filepath.Join(repo, "failed-killed"), "killed\n")
	})

	t.Run("provider cannot spoof a handoff", func(t *testing.T) {
		code, err := run("exit 190", "-e", "COOP_DESCENDANT_TIMEOUT=1")
		if err != nil || code != 1 {
			t.Fatalf("spoofed reserved exit = exit %d, err %v; want exit 1, nil", code, err)
		}
	})
}

func TestRuntimeInitReapsKilledOrphan(t *testing.T) {
	rt := runtimeInitTestRuntime(t)
	helper := buildRuntimeInitProbe(t, rt)
	repo := runtimeInitTestRepo(t)
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "none"}

	var stderr strings.Builder
	code, err := Run(cfg, rt, RunSpec{
		Image: runtimeInitTestImage, Repo: repo, Workdir: "/workspace",
		Cmd:       []string{"/coop-init-probe", "orphan", "/tmp/coop-orphan-pid"},
		Batch:     true,
		Quiet:     true,
		Stderr:    &stderr,
		ExtraArgs: []string{"-v", helper + ":/coop-init-probe:ro"},
	})
	if err != nil || code != 23 {
		t.Fatalf("orphan probe = exit %d, err %v; want exit 23, nil\nstderr:\n%s", code, err, stderr.String())
	}
}

func TestRuntimeInitForwardsTerminationSignal(t *testing.T) {
	rt := runtimeInitTestRuntime(t)
	helper := buildRuntimeInitProbe(t, rt)
	repo := runtimeInitTestRepo(t)
	ready := filepath.Join(repo, "ready")
	received := filepath.Join(repo, "received")
	for _, path := range []string{ready, received} {
		if err := os.WriteFile(path, nil, 0o666); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o666); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "none"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan struct {
		code int
		err  error
	}, 1)
	go func() {
		code, err := Run(cfg, rt, RunSpec{
			Image: runtimeInitTestImage, Repo: repo, Workdir: "/workspace", Ctx: ctx,
			Cmd:       []string{"/coop-init-probe", "signal", "/workspace/ready", "/workspace/received"},
			Batch:     true,
			Quiet:     true,
			ExtraArgs: []string{"-v", helper + ":/coop-init-probe:ro"},
		})
		result <- struct {
			code int
			err  error
		}{code: code, err: err}
	}()

	awaitRuntimeInitMarker(t, ready, "ready\n")
	cancel()
	select {
	case got := <-result:
		if got.code != -1 || !errors.Is(got.err, context.Canceled) {
			t.Fatalf("canceled run = exit %d, err %v; want -1, context canceled", got.code, got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("canceled runtime did not return")
	}
	awaitRuntimeInitMarker(t, received, "term\n")
}

func runtimeInitTestRuntime(t *testing.T) runtime.Runtime {
	t.Helper()
	name := os.Getenv("COOP_RUNTIME")
	if name == "" {
		t.Skip("COOP_RUNTIME is required; run make box-runtime-e2e with Docker or Podman")
	}
	rt, err := runtime.Detect(name)
	if err != nil {
		t.Fatal(err)
	}
	if !rt.SupportsInit() {
		t.Fatalf("runtime %q does not have Coop's verified init contract", name)
	}
	if err := rt.EnsureDaemon(); err != nil {
		t.Fatal(err)
	}
	return rt
}

func TestRuntimeInitProbeTarget(t *testing.T) {
	for _, tc := range []struct {
		platform string
		want     string
		wantErr  bool
	}{
		{platform: "linux/amd64", want: "amd64"},
		{platform: " linux/arm64\n", want: "arm64"},
		{platform: "darwin/arm64", wantErr: true},
		{platform: "linux/amd64/v3", wantErr: true},
		{platform: "linux/", wantErr: true},
	} {
		got, err := runtimeInitProbeArchitecture(tc.platform)
		if got != tc.want || (err != nil) != tc.wantErr {
			t.Errorf("runtimeInitProbeArchitecture(%q) = %q, %v; want %q, error %v",
				tc.platform, got, err, tc.want, tc.wantErr)
		}
	}
}

func buildRuntimeInitProbe(t *testing.T, rt runtime.Runtime) string {
	t.Helper()
	arch := runtimeInitTestImageArchitecture(t, rt)
	path := filepath.Join(t.TempDir(), "coop-init-probe")
	cmd := exec.Command("go", "build", "-trimpath", "-o", path, "./testdata/initprobe")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build init probe: %v\n%s", err, output)
	}
	return path
}

func buildRuntimeEntrypointImage(t *testing.T, rt runtime.Runtime) string {
	t.Helper()
	dir := t.TempDir()
	entrypoint := BaseDockerfile()
	const start = "COPY <<'ENTRY' /usr/local/bin/coop-entry\n"
	const end = "\nENTRY\nRUN chmod +x /usr/local/bin/coop-entry"
	_, entrypoint, ok := strings.Cut(entrypoint, start)
	if !ok {
		t.Fatal("base Dockerfile has no coop-entry heredoc")
	}
	entrypoint, _, ok = strings.Cut(entrypoint, end)
	if !ok {
		t.Fatal("base Dockerfile coop-entry heredoc is unterminated")
	}
	if err := os.WriteFile(filepath.Join(dir, "coop-entry"), []byte(entrypoint), 0o755); err != nil {
		t.Fatal(err)
	}
	dockerfile := "FROM alpine:3.21\nRUN apk add --no-cache util-linux socat\nCOPY coop-entry /usr/local/bin/coop-entry\nENTRYPOINT [\"/usr/local/bin/coop-entry\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	image := fmt.Sprintf("coop-entry-runtime-test-%d", time.Now().UnixNano())
	t.Cleanup(func() { _, _ = rt.Run(nil, nil, nil, "image", "rm", "-f", image) })
	var stdout, stderr strings.Builder
	code, err := rt.Run(nil, &stdout, &stderr, "build", "-t", image, dir)
	if err != nil || code != 0 {
		t.Fatalf("build entrypoint runtime image = exit %d, err %v\n%s%s", code, err, stdout.String(), stderr.String())
	}
	return image
}

func runtimeInitTestImageArchitecture(t *testing.T, rt runtime.Runtime) string {
	t.Helper()
	inspect := func() (int, string, string, error) {
		var stdout, stderr strings.Builder
		code, err := rt.Run(nil, &stdout, &stderr,
			"image", "inspect", "--format", "{{.Os}}/{{.Architecture}}", runtimeInitTestImage)
		return code, stdout.String(), stderr.String(), err
	}

	code, platform, stderr, err := inspect()
	if err != nil {
		t.Fatalf("inspect runtime test image: %v", err)
	}
	if code != 0 {
		var stdout strings.Builder
		var pullStderr strings.Builder
		code, err = rt.Run(nil, &stdout, &pullStderr, "pull", runtimeInitTestImage)
		if err != nil || code != 0 {
			t.Fatalf("pull runtime test image = exit %d, err %v\n%s%s",
				code, err, stdout.String(), pullStderr.String())
		}
		code, platform, stderr, err = inspect()
	}
	if err != nil || code != 0 {
		t.Fatalf("inspect runtime test image = exit %d, err %v\n%s", code, err, stderr)
	}
	arch, err := runtimeInitProbeArchitecture(platform)
	if err != nil {
		t.Fatal(err)
	}
	return arch
}

func runtimeInitProbeArchitecture(platform string) (string, error) {
	platform = strings.TrimSpace(platform)
	osName, arch, ok := strings.Cut(platform, "/")
	if !ok || osName != "linux" || arch == "" || strings.Contains(arch, "/") {
		return "", fmt.Errorf("runtime test image platform %q is not a Linux GOOS/GOARCH pair", platform)
	}
	return arch, nil
}

func runtimeInitTestRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.Chmod(repo, 0o777); err != nil {
		t.Fatal(err)
	}
	return repo
}

func awaitRuntimeInitMarker(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && string(data) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	data, err := os.ReadFile(path)
	t.Fatalf("marker %s = %q, %v; want %q", filepath.Base(path), data, err, want)
}
