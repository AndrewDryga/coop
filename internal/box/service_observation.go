package box

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/runtime"
)

const composeServiceLabel = "com.docker.compose.service"
const composeOneoffLabel = "com.docker.compose.oneoff"

// observedServicePorts narrows a frozen requested mapping to ports whose exact Compose-owned
// container is currently running, unpaused and healthy when it has a healthcheck, with the exact
// loopback binding Coop requested. An observation failure withholds only that service and remains
// available to supplement the original startup error.
func observedServicePorts(ctx context.Context, rt runtime.Runtime, workspace, composeFile, owner, network string, candidates []ServicePort) ([]ServicePort, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	if len(candidates) > 512 {
		return nil, errors.New("service port observation exceeds its bound")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	abs, err := filepath.Abs(composeFile)
	if err != nil {
		return nil, err
	}
	projectName := ComposeProjectFor(workspace, owner)
	workingDir := filepath.Clean(filepath.Dir(abs))
	byService := make(map[string][]ServicePort)
	var order []string
	for _, candidate := range candidates {
		if _, exists := byService[candidate.Service]; !exists {
			order = append(order, candidate.Service)
		}
		byService[candidate.Service] = append(byService[candidate.Service], candidate)
	}
	var (
		observed []ServicePort
		problems []error
	)
	for _, service := range order {
		container, found, err := rt.ObserveServiceContainer(ctx, map[string]string{
			composeProjectLabel:    projectName,
			composeWorkingDirLabel: workingDir,
			composeServiceLabel:    service,
			composeOneoffLabel:     "False",
		})
		if err != nil {
			problems = append(problems, fmt.Errorf("observe service %q: %w", service, err))
			continue
		}
		if !found {
			continue
		}
		observed = append(observed, eligibleObservedServicePorts(container, network, byService[service])...)
	}
	return observed, errors.Join(problems...)
}

func eligibleObservedServicePorts(container runtime.ServiceContainerObservation, network string, candidates []ServicePort) []ServicePort {
	if container.Status != "running" || !container.Running || container.Paused ||
		container.Healthcheck && container.Health != "healthy" || network != "" && !container.Networks[network] {
		return nil
	}
	var observed []ServicePort
	for _, candidate := range candidates {
		key := strconv.Itoa(candidate.ContainerPort) + "/tcp"
		wantPort := strconv.Itoa(candidate.HostPort)
		for _, binding := range container.Ports[key] {
			if binding.HostIP == "127.0.0.1" && binding.HostPort == wantPort {
				observed = append(observed, candidate)
				break
			}
		}
	}
	return observed
}

func discoverObservedServicePorts(ctx context.Context, rt runtime.Runtime, workspace, composeFile, owner, network string, repoReadOnly bool, exposedRoots ...string) ([]ServicePort, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	args, cleanup, _, err := snapshotComposeArgsForStart(workspace, composeFile, owner, repoReadOnly, true, exposedRoots...)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	candidates, err := servicePortsWithArgsContext(ctx, rt, servicePortScope(workspace, owner), args)
	if err != nil {
		return nil, err
	}
	return observedServicePorts(ctx, rt, workspace, composeFile, owner, network, candidates)
}

func servicePortNames(ports []ServicePort) []string {
	seen := make(map[string]bool)
	var names []string
	for _, port := range ports {
		if name := strings.TrimSpace(port.Service); name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names
}
