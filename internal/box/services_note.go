package box

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/project"
)

// servePublication is one serve port's outcome, decided once so the note the agent reads and the
// publish arguments the runtime gets cannot disagree about whether a port reached the host.
type servePublication struct {
	Port, Host int
	Published  bool
}

type serviceLaunchState int

const (
	servicesNotConfigured serviceLaunchState = iota
	servicesRunning
	servicesSkipped
	servicesFailed
	servicesUnknown
)

type serviceLaunchOutcome struct {
	state serviceLaunchState
	err   error
}

// servePublicationPlan decides each serve port once. The host port is stable workspace discovery
// (project.HostPort hashes the workspace path), so its URL is announced even when another process
// from this workspace already holds it; only this box's publish mapping is conditional.
func servePublicationPlan(cfg *config.Config, spec RunSpec, free func(int) bool) []servePublication {
	if len(spec.servePorts) == 0 || cfg.Egress != "open" {
		return nil
	}
	plan := make([]servePublication, 0, len(spec.servePorts))
	for _, port := range spec.servePorts {
		host := project.HostPort(spec.Repo, port)
		plan = append(plan, servePublication{Port: port, Host: host, Published: free(host)})
	}
	return plan
}

// requestedServePublicationPlan is the Run boundary: callers may use servePublicationPlan to
// assemble an explicitly requested publication, but a launch that did not opt into Serve must not
// probe host ports or write host-facing URLs into the agent's instructions.
func requestedServePublicationPlan(cfg *config.Config, spec RunSpec, free func(int) bool) []servePublication {
	if !spec.Serve {
		return nil
	}
	return servePublicationPlan(cfg, spec, free)
}

// servicesNote is the few lines an agent reads once so it never burns a turn diagnosing a host
// condition it cannot see: whether sidecar startup was proved, where each proved service answers
// from INSIDE the box, which
// serve ports reached the host, and — the case that matters most — that the run continued without
// its services, and why. Everything here is known before the box starts. It is empty when there is
// nothing to say (no configured services and nothing to publish), so a plain project pays nothing.
func servicesNote(services serviceLaunchOutcome, ports []ServicePort, joined bool, serve []servePublication) string {
	var lines []string
	switch {
	case services.state == servicesNotConfigured:
	case services.state == servicesFailed:
		detail := "service startup failed"
		if services.err != nil {
			detail = strings.TrimSpace(services.err.Error())
		}
		lines = append(lines,
			"- Sibling services did NOT start: "+detail+".",
			"  This box runs without them: a connection to a sidecar fails because it is not there,",
			"  not because of your work. A human retries them with `coop up`.")
	case services.state == servicesSkipped || services.state == servicesUnknown:
		detail := "startup was not requested"
		if services.err != nil {
			detail = strings.TrimSpace(services.err.Error())
		}
		lines = append(lines, "- Sibling service availability was not checked: "+detail+".")
	case len(ports) == 0:
		lines = append(lines, "- Sibling services started, but none exposes a port, so there is nothing to connect to.")
	case !joined:
		lines = append(lines, "- Sibling services are running, but this box is not on their network (networking is off), so it cannot reach them.")
	default:
		sorted := append([]ServicePort(nil), ports...)
		sort.SliceStable(sorted, func(i, j int) bool {
			if sorted[i].Service != sorted[j].Service {
				return sorted[i].Service < sorted[j].Service
			}
			return sorted[i].ContainerPort < sorted[j].ContainerPort
		})
		for _, p := range sorted {
			lines = append(lines, fmt.Sprintf("- sidecar %s: %s://localhost:%d from this box (its own port %d; also %s:%d by name on the services network)",
				p.Service, p.Scheme, p.HostPort, p.ContainerPort, p.Service, p.ContainerPort))
		}
	}
	for _, s := range serve {
		if s.Published {
			lines = append(lines, fmt.Sprintf("- your port %d is published: a browser on the host reaches it at http://localhost:%d", s.Port, s.Host))
		} else {
			lines = append(lines, fmt.Sprintf("- your port %d is NOT published: host port %d is already in use by another process", s.Port, s.Host))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "\n# Services and ports (coop) — what this box actually got\n" + strings.Join(lines, "\n") + "\n"
}

// appendInstructionNote adds a launch-time section to every agent's already written instruction
// file. The files are generated under this run's private parent and are not mounted yet, so
// rewriting them is the same act as writing them; the facts simply became known after the
// instructions were assembled, because sidecars start after the box's files are laid out.
func appendInstructionNote(mounts []extraMount, note string) error {
	if note == "" {
		return nil
	}
	for _, m := range mounts {
		f, err := os.OpenFile(m.host, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return fmt.Errorf("append services note to %s: %w", m.host, err)
		}
		_, err = f.WriteString(note)
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return fmt.Errorf("append services note to %s: %w", m.host, err)
		}
	}
	return nil
}
