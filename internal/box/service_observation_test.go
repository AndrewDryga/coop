package box

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/runtime"
)

func TestEligibleObservedServicePortsFailsClosed(t *testing.T) {
	candidate := ServicePort{Service: "db", ContainerPort: 5432, HostPort: 25432, Scheme: "postgresql"}
	ready := runtime.ServiceContainerObservation{
		Status: "running", Running: true, Healthcheck: true, Health: "healthy",
		Ports: map[string][]runtime.ServicePortBinding{
			"5432/tcp": {{HostIP: "127.0.0.1", HostPort: "25432"}},
		},
		Networks: map[string]bool{"coop_default": true},
	}
	cases := []struct {
		name string
		edit func(*runtime.ServiceContainerObservation)
		want bool
	}{
		{"healthy exact binding", func(*runtime.ServiceContainerObservation) {}, true},
		{"running without healthcheck", func(o *runtime.ServiceContainerObservation) { o.Healthcheck, o.Health = false, "" }, true},
		{"exited", func(o *runtime.ServiceContainerObservation) { o.Status, o.Running = "exited", false }, false},
		{"running flag false", func(o *runtime.ServiceContainerObservation) { o.Running = false }, false},
		{"paused", func(o *runtime.ServiceContainerObservation) { o.Paused = true }, false},
		{"health starting", func(o *runtime.ServiceContainerObservation) { o.Health = "starting" }, false},
		{"health missing", func(o *runtime.ServiceContainerObservation) { o.Health = "" }, false},
		{"wrong host", func(o *runtime.ServiceContainerObservation) { o.Ports["5432/tcp"][0].HostIP = "0.0.0.0" }, false},
		{"wrong host port", func(o *runtime.ServiceContainerObservation) { o.Ports["5432/tcp"][0].HostPort = "25433" }, false},
		{"missing binding", func(o *runtime.ServiceContainerObservation) { o.Ports = nil }, false},
		{"wrong network", func(o *runtime.ServiceContainerObservation) { o.Networks = map[string]bool{"other": true} }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ready
			got.Ports = map[string][]runtime.ServicePortBinding{
				"5432/tcp": append([]runtime.ServicePortBinding(nil), ready.Ports["5432/tcp"]...),
			}
			tc.edit(&got)
			if available := len(eligibleObservedServicePorts(got, "coop_default", []ServicePort{candidate})) == 1; available != tc.want {
				t.Fatalf("availability = %v, want %v for %+v", available, tc.want, got)
			}
		})
	}
}

func TestFailedServiceObservationKeepsComposeError(t *testing.T) {
	repo, compose := writeCompose(t, "services:\n  db:\n    image: postgres:18\n    expose: [5432]\n")
	shim := filepath.Join(t.TempDir(), "runtime")
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  *'config --format json'*) printf '%s\\n' '{\"services\":{\"db\":{\"expose\":[\"5432\"]}}}' ;;\n" +
		"  *'config --services'*) printf '%s\\n' db ;;\n" +
		"  *'up -d --wait --remove-orphans'*) printf '%s\\n' 'keycloak failed its healthcheck' >&2; exit 1 ;;\n" +
		"  *'ps -q -a --no-trunc'*) exit 7 ;;\n" +
		"esac\n"
	if err := os.WriteFile(shim, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	started, err := startServicesFileContext(context.Background(), runtime.Runtime{Name: shim}, repo, compose, "", nil, &stderr, false, false, false, nil)
	if err == nil || !strings.Contains(err.Error(), "compose up exited with status "+strconv.Itoa(1)) ||
		!strings.Contains(err.Error(), "availability is unknown") {
		t.Fatalf("combined startup/observation error = %v", err)
	}
	if len(started.ports) != 0 || !strings.Contains(stderr.String(), "keycloak failed its healthcheck") {
		t.Fatalf("failed observation lost or contradicted startup evidence: ports=%v stderr=%q", started.ports, stderr.String())
	}
}
