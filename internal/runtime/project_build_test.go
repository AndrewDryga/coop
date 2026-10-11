package runtime

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/dockersock"
)

func TestDockerImageAbsenceRequiresAnExactDaemon404(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		replace bool
		missing bool
	}{
		{"missing", http.StatusNotFound, false, true},
		{"CLI observation failure", http.StatusOK, false, false},
		{"daemon failure", http.StatusInternalServerError, false, false},
		{"daemon replaced after absence", http.StatusNotFound, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var replaced atomic.Bool
			endpoint := dockersock.ServeHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/info" {
					id := "reviewed"
					if replaced.Load() {
						id = "replacement"
					}
					_ = json.NewEncoder(w).Encode(dockersock.Info{ID: id, ServerVersion: "29.4.0", KernelVersion: "fixture", OSType: "linux", Architecture: "arm64"})
					return
				}
				if r.URL.Path != "/images/coop-client:fixture/json" {
					t.Errorf("unexpected absence probe: %s", r.URL.Path)
				}
				if tc.replace {
					replaced.Store(true)
				}
				w.WriteHeader(tc.status)
			}))
			script := filepath.Join(t.TempDir(), "docker")
			if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
				t.Fatal(err)
			}
			d, err := BindDocker(t.Context(), Runtime{Name: script}, endpoint, "")
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			_, _, err = d.Image(t.Context(), "coop-client:fixture")
			if err == nil || errors.Is(err, ErrDockerImageMissing) != tc.missing {
				t.Fatalf("ambiguous inspection acquired absence: %v", err)
			}
		})
	}
}

func TestProjectBuildBindsTheReviewedDaemon(t *testing.T) {
	var replaced atomic.Bool
	endpoint := dockersock.Serve(t, func() (dockersock.Info, error) {
		id := "reviewed-daemon"
		if replaced.Load() {
			id = "unreviewed-daemon"
		}
		return dockersock.Info{ID: id, ServerVersion: "29.4.0", KernelVersion: "fixture", OSType: "linux", Architecture: "arm64"}, nil
	})
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", endpoint)
	dir := t.TempDir()
	calls, script := filepath.Join(dir, "calls"), filepath.Join(dir, "docker")
	t.Setenv("COOP_PROJECT_BUILD_CALLS", calls)
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"$COOP_PROJECT_BUILD_CALLS\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	rt, err := (Runtime{Name: script}).FreezeCompose(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONTEXT", "unreviewed-context")
	t.Setenv("DOCKER_HOST", "unix:///must-not-follow.sock")
	if code, err := rt.RunProjectBuild(t.Context(), nil, nil, nil, "build", "/reviewed-context"); err != nil || code != 0 {
		t.Fatalf("frozen build: %d / %v", code, err)
	}
	before, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(before), "--host\n"+endpoint+"\nbuild\n--builder\ndefault\n") {
		t.Fatalf("build was redirected: %s", before)
	}
	replaced.Store(true)
	if _, err := rt.RunProjectBuild(t.Context(), nil, nil, nil, "build", "/reviewed-context"); err == nil {
		t.Fatal("replacement daemon ran reviewed instructions")
	}
	after, _ := os.ReadFile(calls)
	if string(after) != string(before) {
		t.Fatal("custody failure executed a build")
	}
}

func TestFrozenSetupBindingRetainsCreationAuthority(t *testing.T) {
	rt, _ := fixtureDocker(t, dockerFixture{})
	d, err := BindDocker(t.Context(), rt, fixtureSock(), "")
	if err != nil {
		t.Fatal(err)
	}
	id := d.Info().ID
	_ = d.Close()
	setup, err := BindDockerForSetup(t.Context(), rt, fixtureSock(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer setup.Close()
	if err := setup.VerifyLaunch(t.Context()); err != nil {
		t.Fatalf("frozen setup lost image/probe authority: %v", err)
	}
	recovery, err := BindDocker(t.Context(), rt, fixtureSock(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer recovery.Close()
	if err := recovery.VerifyLaunch(t.Context()); err == nil {
		t.Fatal("recovery acquired setup authority")
	}
	if changed, err := BindDockerForSetup(t.Context(), rt, fixtureSock(), "replaced-daemon"); err == nil {
		_ = changed.Close()
		t.Fatal("setup accepted a different frozen daemon")
	}
}
