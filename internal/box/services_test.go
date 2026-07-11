package box

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// TestAutoUpServices: box.Run auto-starts sibling services only when enabled (COOP_AUTO_UP),
// the box is on the services network, it's online, and the runtime has compose — Apple
// `container` does not.
func TestAutoUpServices(t *testing.T) {
	cases := []struct {
		name    string
		autoUp  bool
		network bool
		egress  string
		rtName  string
		want    bool
	}{
		{"defaults: on, networked, online, docker", true, true, "open", "docker", true},
		{"podman too", true, true, "open", "podman", true},
		{"COOP_AUTO_UP=0 opts out", false, true, "open", "docker", false},
		{"no services network (COOP_NETWORK=0)", true, false, "open", "docker", false},
		{"offline box (COOP_EGRESS=none)", true, true, "none", "docker", false},
		{"Apple container has no compose", true, true, "open", "container", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &config.Config{AutoUp: c.autoUp, Egress: c.egress}
			spec := RunSpec{Network: c.network}
			if got := autoUpServices(cfg, spec, c.rtName); got != c.want {
				t.Errorf("autoUpServices = %v, want %v", got, c.want)
			}
		})
	}
}

// recorderRuntime returns a runtime whose binary is a shim that appends its args to a recorder
// file and exits 0 — so a test can assert whether `compose up` was actually invoked.
func recorderRuntime(t *testing.T, recorder string) runtime.Runtime {
	t.Helper()
	shim := filepath.Join(t.TempDir(), "rt")
	script := "#!/bin/sh\necho \"$@\" >> " + recorder + "\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return runtime.Runtime{Name: shim}
}

// EnsureServices validates the compose file before running it: a valid file reaches `compose up`,
// an unsafe one is refused with a naming error and NO compose command is ever run.
func TestEnsureServicesValidates(t *testing.T) {
	t.Run("valid file runs compose up", func(t *testing.T) {
		repo := t.TempDir()
		os.MkdirAll(filepath.Join(repo, ".agent"), 0o755)
		os.WriteFile(filepath.Join(repo, ".agent", "compose.yml"),
			[]byte("services:\n  db:\n    image: postgres:18\n"), 0o644)
		rec := filepath.Join(t.TempDir(), "rec")
		if err := EnsureServices(recorderRuntime(t, rec), repo, io.Discard, io.Discard); err != nil {
			t.Fatalf("valid file should run: %v", err)
		}
		out, _ := os.ReadFile(rec)
		if !strings.Contains(string(out), "compose") || !strings.Contains(string(out), "up") {
			t.Errorf("expected `compose ... up` to run, recorder has: %q", out)
		}
	})

	t.Run("unsafe file is refused, compose never runs", func(t *testing.T) {
		repo := t.TempDir()
		os.MkdirAll(filepath.Join(repo, ".agent"), 0o755)
		os.WriteFile(filepath.Join(repo, ".agent", "compose.yml"),
			[]byte("services:\n  x:\n    image: a\n    privileged: true\n"), 0o644)
		rec := filepath.Join(t.TempDir(), "rec")
		err := EnsureServices(recorderRuntime(t, rec), repo, io.Discard, io.Discard)
		if err == nil {
			t.Fatal("an unsafe compose file must be refused")
		}
		if !strings.Contains(err.Error(), "refusing to run compose.yml") {
			t.Errorf("error should name the refused file, got: %v", err)
		}
		if _, statErr := os.Stat(rec); statErr == nil {
			out, _ := os.ReadFile(rec)
			t.Errorf("compose must NOT run for a refused file, but recorder has: %q", out)
		}
	})

	t.Run("no compose file is a no-op", func(t *testing.T) {
		rec := filepath.Join(t.TempDir(), "rec")
		if err := EnsureServices(recorderRuntime(t, rec), t.TempDir(), io.Discard, io.Discard); err != nil {
			t.Fatalf("no compose file should be a nil no-op: %v", err)
		}
		if _, statErr := os.Stat(rec); statErr == nil {
			t.Error("compose must not run when there's no file")
		}
	})
}
