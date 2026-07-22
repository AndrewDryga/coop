package box

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// ServicePort is a sidecar service port coop publishes per-workspace: a service's `expose`d
// container port, mapped to a stable host port (project.HostPort of the workspace's canonical
// path). The SAME host port feeds the compose publish (on the host), the in-box forwarder, and the
// COOP_SERVICE_<NAME>_URL env — so one URL, localhost:<HostPort>, works identically both sides.
type ServicePort struct {
	Service       string
	ContainerPort int
	HostPort      int
	Scheme        string // "http" (default) or the service's `coop.service.scheme` compose label
}

// ServicePorts returns the per-workspace host-port mapping for a repo's sidecars: it asks the
// runtime for the resolved compose config and reads each service's `expose` ports. Best-effort —
// no compose file, no docker, or a parse error yields nothing (sidecars just aren't published).
// `expose` (not `ports`) is the opt-in marker: it publishes nothing on its own, so coop's override
// adds the only host mapping (no double-publish).
func ServicePorts(rt runtime.Runtime, workspacePath, composeFile string) []ServicePort {
	var buf bytes.Buffer
	code, err := rt.Run(nil, &buf, io.Discard, "compose", "-f", composeFile, "config", "--format", "json")
	if err != nil || code != 0 {
		return nil
	}
	return parseServicePorts(buf.Bytes(), workspacePath)
}

// parseServicePorts is the pure core of ServicePorts: given `docker compose config --format json`
// output and a workspace path, it returns the expose→host-port mapping, in a deterministic order
// (services sorted, ports as listed). Only plain integer container ports are handled in v1.
func parseServicePorts(configJSON []byte, workspacePath string) []ServicePort {
	var cfg struct {
		Services map[string]struct {
			Expose []string          `json:"expose"`
			Labels map[string]string `json:"labels"`
		} `json:"services"`
	}
	if json.Unmarshal(configJSON, &cfg) != nil {
		return nil
	}
	canon := canonicalWorkspace(workspacePath)
	names := make([]string, 0, len(cfg.Services))
	for n := range cfg.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []ServicePort
	for _, name := range names {
		svc := cfg.Services[name]
		scheme := svc.Labels["coop.service.scheme"] // coop.service.scheme: https — else default http
		if scheme == "" {
			scheme = "http"
		}
		for _, e := range svc.Expose {
			port, err := strconv.Atoi(strings.TrimSpace(e))
			if err != nil || port < 1 || port > 65535 {
				continue
			}
			out = append(out, ServicePort{
				Service:       name,
				ContainerPort: port,
				// Key on service+port, so two services sharing a container port — or a sidecar port
				// equal to a serve.port — never collide on one host port.
				HostPort: project.HostPortFor(canon, name+":"+strconv.Itoa(port)),
				Scheme:   scheme,
			})
		}
	}
	return out
}

// writeServiceOverride writes a temp compose override publishing each ServicePort to
// 127.0.0.1:<HostPort>:<ContainerPort>, and returns its path + a cleanup func. Merged as a second
// `-f`, it adds the loopback host mapping the base file's `expose` deliberately left off.
func writeServiceOverride(sp []ServicePort) (path string, cleanup func(), err error) {
	bySvc := map[string][]ServicePort{}
	var order []string
	for _, p := range sp {
		if _, ok := bySvc[p.Service]; !ok {
			order = append(order, p.Service)
		}
		bySvc[p.Service] = append(bySvc[p.Service], p)
	}
	var b strings.Builder
	b.WriteString("services:\n")
	for _, svc := range order {
		fmt.Fprintf(&b, "  %s:\n    ports:\n", svc)
		for _, p := range bySvc[svc] {
			fmt.Fprintf(&b, "      - \"127.0.0.1:%d:%d\"\n", p.HostPort, p.ContainerPort)
		}
	}
	f, err := os.CreateTemp("", "coop-compose-override-*.yml")
	if err != nil {
		return "", nil, err
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", nil, err
	}
	f.Close()
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}

// forwardEnv renders the COOP_FORWARD value coop-entry consumes: "<hostPort>:<service>:<containerPort>"
// entries, comma-joined — one in-box loopback forwarder per sidecar port. Empty when there are none.
func forwardEnv(sp []ServicePort) string {
	parts := make([]string, 0, len(sp))
	for _, p := range sp {
		parts = append(parts, fmt.Sprintf("%d:%s:%d", p.HostPort, p.Service, p.ContainerPort))
	}
	return strings.Join(parts, ",")
}

// serviceEnvName upcases a service name into the COOP_SERVICE_<NAME>_URL env slot (non-alphanumerics
// → underscore), e.g. "keycloak" → "KEYCLOAK", "auth-db" → "AUTH_DB".
func serviceEnvName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}
