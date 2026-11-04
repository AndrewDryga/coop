package runtime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	coopconfig "github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/testutil/dockersock"
)

func TestComposeContextCancellationPreservesCauseAndCleansPrivateConfig(t *testing.T) {
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	t.Setenv("DOCKER_AUTH_CONFIG", "")
	t.Setenv("DOCKER_HOST", composeFixtureDaemon(t))
	root := t.TempDir()
	ready := filepath.Join(root, "ready")
	t.Setenv("COOP_COMPOSE_TEST_READY", ready)
	binary := filepath.Join(root, "docker")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf '%s' \"$2\" > \"$COOP_COMPOSE_TEST_READY\"\nexec sleep 60\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := (Runtime{Name: binary}).RunComposeContext(ctx, nil, io.Discard, io.Discard, "compose", "up")
		done <- err
	}()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	var configPath string
started:
	for {
		select {
		case err := <-done:
			t.Fatalf("Compose returned before cancellation: %v", err)
		case <-deadline.C:
			t.Fatal("Compose never started")
		case <-ticker.C:
			if data, err := os.ReadFile(ready); err == nil && len(data) > 0 {
				configPath = string(data)
				break started
			}
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("lost cancellation cause: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Compose ignored cancellation")
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("retained registry config: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(configPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "coop-compose-client-") || strings.HasPrefix(entry.Name(), "coop-docker-client-") {
			t.Fatalf("retained private client directory %s", entry.Name())
		}
	}
}

func TestComposeRegistryConfigStaysUnderProtectedCoopHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	root, cleanup, err := privateComposeClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	protected, err := filepath.EvalSymlinks(filepath.Join(coopconfig.LocalRoot(), "runfiles"))
	if err != nil {
		t.Fatal(err)
	}
	if relative, err := filepath.Rel(protected, root); err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		t.Fatalf("Compose registry config escaped protected Coop home: %s (%v)", root, err)
	}
}

func TestComposeUsesProxyFreeConfigAndFrozenDaemon(t *testing.T) {
	config := t.TempDir()
	if err := os.WriteFile(filepath.Join(config, "config.json"), []byte(`{"auths":{"registry.example":{"auth":"fixture-auth"}},"proxies":{"default":{"httpProxy":"http://user:secret@proxy"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", config)
	t.Setenv("DOCKER_AUTH_CONFIG", `{"auths":{"registry-env.example":{"auth":"Zml4dHVyZTpwYXNz"}}}`)
	t.Setenv("DOCKER_CONTEXT", "")
	endpoint := composeFixtureDaemon(t)
	t.Setenv("DOCKER_HOST", endpoint)
	recorder := filepath.Join(t.TempDir(), "observed")
	path := filepath.Join(t.TempDir(), "docker")
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  *\"compose config --services\"*) printf '%s\\n' \"$*\" >> " + strconv.Quote(recorder) + "; cat \"$2/config.json\" >> " + strconv.Quote(recorder) + "; printf '%s\\n' \"$DOCKER_AUTH_CONFIG\" >> " + strconv.Quote(recorder) + " ;;\n" +
		"esac\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	rt, err := (Runtime{Name: path}).FreezeCompose(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_HOST", "unix:///other.sock")
	var stdout bytes.Buffer
	if code, err := rt.RunCompose(nil, &stdout, &stdout, "compose", "config", "--services"); err != nil || code != 0 {
		t.Fatalf("bound Compose command = (%d, %v)", code, err)
	}
	observed, err := os.ReadFile(recorder)
	if err != nil || !strings.Contains(string(observed), "--host "+endpoint) || !strings.Contains(string(observed), "fixture-auth") || !strings.Contains(string(observed), "registry-env.example") || strings.Contains(string(observed), "secret@proxy") || strings.Contains(string(observed), "proxies") {
		t.Fatalf("Compose received unsafe client configuration: %v\n%s", err, observed)
	}
	if original, err := os.ReadFile(filepath.Join(config, "config.json")); err != nil || !strings.Contains(string(original), "secret@proxy") {
		t.Fatalf("original Docker config was changed: %v", err)
	}
	t.Setenv("COOP_TEST_DAEMON", "daemon-two")
	if _, err := rt.RunCompose(nil, &stdout, &stdout, "compose", "config", "--services"); err == nil {
		t.Fatal("frozen Compose followed a replaced Docker daemon")
	}
}

func TestComposeAuthEnvironmentRejectsNonAuthData(t *testing.T) {
	for _, value := range []string{
		`{"auths":{"registry.example":{"auth":""}}}`,
		`{"auths":{"registry.example":{"auth":"%%%"}}}`,
		`{"auths":{},"proxies":{"default":{"httpProxy":"http://secret"}}}`,
		`{"auths":{}} {"auths":{}}`,
		strings.Repeat("x", (1<<20)+1),
	} {
		t.Setenv("DOCKER_AUTH_CONFIG", value)
		if got, err := composeAuthEnvironment(); err == nil || got != "" {
			t.Fatalf("non-auth Docker environment was accepted (length %d): %v", len(value), err)
		}
	}
}

func TestFrozenComposeInventoriesWritersOnSameDaemon(t *testing.T) {
	t.Setenv("DOCKER_CONTEXT", "")
	endpoint := composeFixtureDaemon(t)
	t.Setenv("DOCKER_HOST", endpoint)
	recorder := filepath.Join(t.TempDir(), "commands")
	path := filepath.Join(t.TempDir(), "docker")
	id := strings.Repeat("a", 64)
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + strconv.Quote(recorder) + "\n" +
		"case \"$*\" in\n" +
		"  *\"ps -q\"*) printf '%s\\n' '" + id + "' ;;\n" +
		"  *\"inspect --type container --format\"*) printf '%s\\n' '{\"ID\":\"" + id + "\",\"Status\":\"running\",\"Mounts\":[{\"Type\":\"bind\",\"Source\":\"/fixture/source\",\"RW\":true}]}' ;;\n" +
		"esac\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	rt, err := (Runtime{Name: path}).FreezeCompose(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_HOST", "unix:///other.sock")
	sources, err := rt.RunningWritableBindSourcesByLabels(t.Context(), map[string]string{"com.docker.compose.project": ""})
	if err != nil || len(sources) != 1 || sources[0] != "/fixture/source" {
		t.Fatalf("frozen inventory = %v, %v", sources, err)
	}
	observed, err := os.ReadFile(recorder)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(observed)), "\n") {
		if !strings.Contains(line, "--host "+endpoint) || strings.Contains(line, "unix:///other.sock") {
			t.Fatalf("inventory followed ambient context: %s", line)
		}
	}
	t.Setenv("COOP_TEST_DAEMON", "daemon-two")
	if _, err := rt.RunningWritableBindSourcesByLabels(t.Context(), map[string]string{"com.docker.compose.project": ""}); err == nil {
		t.Fatal("inventory followed a replaced Docker daemon")
	}
}

func TestFrozenComposeNetworkCleanupStaysOnSameDaemon(t *testing.T) {
	t.Setenv("DOCKER_CONTEXT", "")
	endpoint := composeFixtureDaemon(t)
	t.Setenv("DOCKER_HOST", endpoint)
	recorder := filepath.Join(t.TempDir(), "commands")
	path := filepath.Join(t.TempDir(), "docker")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + strconv.Quote(recorder) + "\n" +
		"case \"$*\" in\n" +
		"  *'network ls -q '*) echo owned-network ;;\n" +
		"  *'ps -q -a --filter network='*) echo stopped-container ;;\n" +
		"esac\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	rt, err := (Runtime{Name: path}).FreezeCompose(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_HOST", "unix:///other.sock")
	operations := []func() error{
		func() error {
			ids, err := rt.NetworkIDs(t.Context(), "label=com.docker.compose.project=fixture")
			if err == nil && (len(ids) != 1 || ids[0] != "owned-network") {
				t.Fatalf("network inventory = %v", ids)
			}
			return err
		},
		func() error {
			ids, err := rt.ContainerIDsOnNetwork(t.Context(), "owned-network")
			if err == nil && (len(ids) != 1 || ids[0] != "stopped-container") {
				t.Fatalf("attached container inventory = %v", ids)
			}
			return err
		},
		func() error { return rt.RemoveNetwork(t.Context(), "owned-network") },
	}
	for _, operation := range operations {
		if err := operation(); err != nil {
			t.Fatal(err)
		}
	}
	observed, err := os.ReadFile(recorder)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(observed)), "\n") {
		if !strings.Contains(line, "--host "+endpoint) || strings.Contains(line, "unix:///other.sock") {
			t.Fatalf("network cleanup followed ambient context: %s", line)
		}
	}
	t.Setenv("COOP_TEST_DAEMON", "daemon-two")
	for _, operation := range operations {
		if err := operation(); err == nil {
			t.Fatal("network cleanup followed a replaced daemon")
		}
	}
	after, err := os.ReadFile(recorder)
	if err != nil || string(after) != string(observed) {
		t.Fatalf("network command ran after daemon replacement: %v\n%s", err, after)
	}
}

// composeFixtureDaemon serves the fixture daemon's identity, daemon-one unless COOP_TEST_DAEMON
// names a replacement.
func composeFixtureDaemon(t *testing.T) string {
	t.Helper()
	return dockersock.Serve(t, func() (dockersock.Info, error) {
		id := os.Getenv("COOP_TEST_DAEMON")
		if id == "" {
			id = "daemon-one"
		}
		return dockersock.Info{ID: id, OSType: "linux", Architecture: "amd64", ServerVersion: "29", KernelVersion: "fixture", SecurityOptions: []string{}}, nil
	})
}
