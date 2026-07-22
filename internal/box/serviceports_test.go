package box

import (
	"os"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/project"
)

// parseServicePorts reads each service's `expose` (the shape `docker compose config --format json`
// emits) into a stable per-workspace host-port mapping; non-integer/absent expose yields nothing.
func TestParseServicePorts(t *testing.T) {
	repo := t.TempDir()
	sp := parseServicePorts([]byte(`{"services":{"keycloak":{"expose":["8443"]},"db":{}}}`), repo)
	if len(sp) != 1 || sp[0].Service != "keycloak" || sp[0].ContainerPort != 8443 {
		t.Fatalf("want one keycloak:8443, got %+v", sp)
	}
	if want := project.HostPort(canonicalWorkspace(repo), 8443); sp[0].HostPort != want {
		t.Errorf("host port = %d, want HostPort(canonical, 8443) = %d", sp[0].HostPort, want)
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

// writeServiceOverride emits a compose override that publishes each port to its loopback host port.
func TestWriteServiceOverride(t *testing.T) {
	path, cleanup, err := writeServiceOverride([]ServicePort{{Service: "keycloak", ContainerPort: 8443, HostPort: 28443}})
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
