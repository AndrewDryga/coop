package box

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// parseServicePorts reads each service's `expose` (the shape `docker compose config --format json`
// emits) into a stable per-workspace host-port mapping; non-integer/absent expose yields nothing.
func TestParseServicePorts(t *testing.T) {
	repo := t.TempDir()
	sp := parseServicePorts([]byte(`{"services":{"keycloak":{"expose":["8443"],"labels":{"coop.service.scheme":"https"}},"db":{}}}`), repo)
	if len(sp) != 1 || sp[0].Service != "keycloak" || sp[0].ContainerPort != 8443 {
		t.Fatalf("want one keycloak:8443, got %+v", sp)
	}
	if want := project.HostPortFor(canonicalWorkspace(repo), "keycloak:8443"); sp[0].HostPort != want {
		t.Errorf("host port = %d, want HostPortFor(canonical, keycloak:8443) = %d", sp[0].HostPort, want)
	}
	if sp[0].Scheme != "https" {
		t.Errorf("scheme = %q, want https (from coop.service.scheme label)", sp[0].Scheme)
	}
	if def := parseServicePorts([]byte(`{"services":{"x":{"expose":["80"]}}}`), repo); len(def) != 1 || def[0].Scheme != "http" {
		t.Errorf("scheme should default to http, got %+v", def)
	}
	// v1: only plain integer container ports (a "/tcp" suffix or garbage is skipped, not fatal).
	if got := parseServicePorts([]byte(`{"services":{"x":{"expose":["8443/tcp","nope"]}}}`), repo); len(got) != 0 {
		t.Errorf("non-integer expose should be skipped, got %+v", got)
	}
	// no services / no expose → nothing.
	if got := parseServicePorts([]byte(`{"services":{}}`), repo); len(got) != 0 {
		t.Errorf("no services → no ports, got %+v", got)
	}
}

func TestServicePortDiscoveryIsBoundedAndStrict(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "runtime")
	large := filepath.Join(dir, "large")
	largeJSON := `{"services":{},"padding":"` + strings.Repeat("x", maxResolvedServiceConfigBytes) + `"}`
	if err := os.WriteFile(large, []byte(largeJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_TEST_LARGE_CONFIG", large)
	if err := os.WriteFile(shim, []byte(`#!/bin/sh
case "$COOP_TEST_CONFIG_MODE" in
  valid) printf '%s\n' '{"services":{"db":{"expose":["5432"]}}}' ;;
  missing) printf '%s\n' '{}' ;;
  malformed) printf '%s\n' '{' ;;
  oversized) cat "$COOP_TEST_LARGE_CONFIG" ;;
  failed) exit 7 ;;
  stalled) sleep 30 ;;
esac
`), 0o700); err != nil {
		t.Fatal(err)
	}
	rt := runtime.Runtime{Name: shim}
	repo := t.TempDir()
	t.Setenv("COOP_TEST_CONFIG_MODE", "valid")
	ports, err := servicePortsWithArgsContext(context.Background(), rt, repo, nil)
	if err != nil || len(ports) != 1 || ports[0].Service != "db" {
		t.Fatalf("valid bounded discovery = (%+v, %v)", ports, err)
	}
	for _, mode := range []string{"missing", "malformed", "oversized", "failed"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("COOP_TEST_CONFIG_MODE", mode)
			if ports, err := servicePortsWithArgsContext(context.Background(), rt, repo, nil); err == nil || len(ports) != 0 {
				t.Fatalf("%s discovery = (%+v, %v), want no ports and an error", mode, ports, err)
			} else if mode == "oversized" && !strings.Contains(err.Error(), "exceeds 4 MiB") {
				t.Fatalf("oversized discovery bypassed its cap: %v", err)
			}
		})
	}
	t.Setenv("COOP_TEST_CONFIG_MODE", "stalled")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	if ports, err := servicePortsWithArgsContext(ctx, rt, repo, nil); err == nil || len(ports) != 0 || time.Since(started) > 2*time.Second {
		t.Fatalf("canceled discovery = (%+v, %v) after %s", ports, err, time.Since(started))
	}
}

// Host ports are keyed on service+port, so two services sharing a container port — and a sidecar
// port equal to a serve.port — never collide.
func TestServicePortNoCollision(t *testing.T) {
	repo := t.TempDir()
	canon := canonicalWorkspace(repo)
	// Two services both expose 8080 → distinct host ports.
	sp := parseServicePorts([]byte(`{"services":{"a":{"expose":["8080"]},"b":{"expose":["8080"]}}}`), repo)
	if len(sp) != 2 {
		t.Fatalf("want two ports, got %+v", sp)
	}
	if sp[0].HostPort == sp[1].HostPort {
		t.Errorf("two services on 8080 must get distinct host ports, both %d", sp[0].HostPort)
	}
	// A sidecar on 4000 must not collide with the box's own serve.port 4000 (HostPort(repo, 4000)).
	sc := parseServicePorts([]byte(`{"services":{"web":{"expose":["4000"]}}}`), repo)
	if len(sc) != 1 || sc[0].HostPort == project.HostPort(canon, 4000) {
		t.Errorf("sidecar web:4000 must not reuse serve.port 4000's host port: %+v", sc)
	}
}

func TestServicePortsArePrivateToLogicalOwner(t *testing.T) {
	repo := t.TempDir()
	config := []byte(`{"services":{"db":{"expose":["5432"]}}}`)
	dev := parseServicePorts(config, servicePortScope(repo, ""))
	first := parseServicePorts(config, servicePortScope(repo, "loop-one"))
	second := parseServicePorts(config, servicePortScope(repo, "loop-two"))
	if len(dev) != 1 || len(first) != 1 || len(second) != 1 ||
		dev[0].HostPort == first[0].HostPort || first[0].HostPort == second[0].HostPort {
		t.Fatalf("owner ports collide: dev=%+v first=%+v second=%+v", dev, first, second)
	}
}

// writeServiceOverride emits a compose override that publishes each port to its loopback host port.
func TestWriteServiceOverride(t *testing.T) {
	path, cleanup, err := writeServiceOverride([]ServicePort{{Service: "keycloak", ContainerPort: 8443, HostPort: 28443}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(data); !strings.Contains(s, "keycloak:") || !strings.Contains(s, `"127.0.0.1:28443:8443"`) {
		t.Errorf("override should pin the loopback publish:\n%s", s)
	}
}

// forwardEnv + serviceEnvName render the coop-entry mapping and the env-var slot.
func TestForwardEnvAndServiceName(t *testing.T) {
	sp := []ServicePort{{Service: "keycloak", ContainerPort: 8443, HostPort: 28443}, {Service: "auth-db", ContainerPort: 5432, HostPort: 39127}}
	if got, want := forwardEnv(sp), "28443:keycloak:8443,39127:auth-db:5432"; got != want {
		t.Errorf("forwardEnv = %q, want %q", got, want)
	}
	if got := serviceEnvName("auth-db"); got != "AUTH_DB" {
		t.Errorf("serviceEnvName(auth-db) = %q, want AUTH_DB", got)
	}
}
