package box

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

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
func ServicePorts(rt runtime.Runtime, workspacePath, composeFile string, exposedRoots ...string) []ServicePort {
	if composeFile == "" {
		return nil
	}
	args, cleanup, _, err := snapshotComposeArgs(workspacePath, composeFile, false, exposedRoots...)
	if err != nil {
		return nil
	}
	defer cleanup()
	return servicePortsWithArgs(rt, workspacePath, args)
}

func servicePortsWithArgs(rt runtime.Runtime, workspacePath string, args []string) []ServicePort {
	var buf bytes.Buffer
	configArgs := append(append([]string(nil), args...), "config", "--format", "json")
	if err := runCompose(rt, &buf, io.Discard, "config --format json", configArgs); err != nil {
		return nil
	}
	return parseServicePorts(buf.Bytes(), workspacePath)
}

const maxResolvedServiceConfigBytes = 4 << 20

type boundedServiceConfig struct {
	buf      bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedServiceConfig) Write(p []byte) (int, error) {
	written := len(p)
	remaining := b.limit - b.buf.Len()
	if remaining < 0 {
		remaining = 0
	}
	if len(p) > remaining {
		p = p[:remaining]
		b.overflow = true
	}
	_, _ = b.buf.Write(p)
	return written, nil
}

func (b *boundedServiceConfig) Bytes() []byte { return b.buf.Bytes() }

// servicePortsWithArgsContext is the strict discovery form: it is bounded, cancellable and
// distinguishes a valid empty Compose config from an unavailable or malformed observation.
func servicePortsWithArgsContext(ctx context.Context, rt runtime.Runtime, workspacePath string, args []string) ([]ServicePort, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var out boundedServiceConfig
	out.limit = maxResolvedServiceConfigBytes
	configArgs := append(append([]string(nil), args...), "config", "--format", "json")
	code, err := rt.RunInterruptible(ctx, nil, &out, io.Discard, configArgs...)
	if err != nil {
		return nil, fmt.Errorf("inspect resolved service configuration: %w", err)
	}
	if out.overflow {
		return nil, errors.New("resolved service configuration exceeds 4 MiB")
	}
	if code != 0 {
		return nil, fmt.Errorf("compose config --format json exited with status %d", code)
	}
	return parseServicePortsChecked(out.Bytes(), workspacePath)
}

// parseServicePorts is the pure core of ServicePorts: given `docker compose config --format json`
// output and a workspace path, it returns the expose→host-port mapping, in a deterministic order
// (services sorted, ports as listed). Only plain integer container ports are handled in v1.
func parseServicePorts(configJSON []byte, workspacePath string) []ServicePort {
	ports, _ := parseServicePortsChecked(configJSON, workspacePath)
	return ports
}

func parseServicePortsChecked(configJSON []byte, workspacePath string) ([]ServicePort, error) {
	var cfg struct {
		Services map[string]struct {
			Expose []string          `json:"expose"`
			Labels map[string]string `json:"labels"`
		} `json:"services"`
	}
	if err := json.Unmarshal(configJSON, &cfg); err != nil {
		return nil, fmt.Errorf("parse resolved service configuration: %w", err)
	}
	if cfg.Services == nil {
		return nil, errors.New("parse resolved service configuration: missing services object")
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
	return out, nil
}

// writeServiceOverride writes a temp compose override publishing each ServicePort to
// 127.0.0.1:<HostPort>:<ContainerPort>, and returns its path + a cleanup func. Merged as a second
// `-f`, it adds the loopback host mapping the base file's `expose` deliberately left off.
func writeServiceOverride(sp []ServicePort, workspace string, exposedRoots ...string) (path string, cleanup func(), err error) {
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
	dir, err := privateWorkspaceTempDir(workspace, "coop-compose-", exposedRoots...)
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	path = filepath.Join(dir, "coop-compose-override-ports.yml")
	if err := os.WriteFile(path, []byte(b.String()), 0o400); err != nil {
		cleanup()
		return "", nil, err
	}
	return path, cleanup, nil
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
