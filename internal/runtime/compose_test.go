package runtime

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	coopconfig "github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/testutil/dockersock"
)

func TestComposeRegistryConfigStaysUnderProtectedCoopHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	root, cleanup, err := privateComposeClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	protected := filepath.Join(coopconfig.RootDir(), "runfiles")
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
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + strconv.Quote(recorder) + "\n" +
		"case \"$*\" in\n" +
		"  *\"ps -q\"*) printf 'fixture-container\\n' ;;\n" +
		"  *\"inspect --format\"*) printf '[{\"Type\":\"bind\",\"Source\":\"/fixture/source\",\"RW\":true}]\\n' ;;\n" +
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
