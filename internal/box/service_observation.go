package box

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
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

func serviceContainerUp(container runtime.ServiceContainerObservation) bool {
	return container.Status == "running" && container.Running && !container.Paused &&
		(!container.Healthcheck || container.Health == "healthy")
}

func eligibleObservedServicePorts(container runtime.ServiceContainerObservation, network string, candidates []ServicePort) []ServicePort {
	if !serviceContainerUp(container) || network != "" && !container.Networks[network] {
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
	args, cleanup, _, err := snapshotComposeArgsForInspect(workspace, composeFile, owner, repoReadOnly, exposedRoots...)
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

// markServiceDefinitionKey asks a service start to label what it started from. Only a session asks: on
// a loop's services, a comment edited in the file would recreate every one of them at the next `up`.
type markServiceDefinitionKey struct{}

// sessionServiceDefinition is serviceDefinition for an open session, which starts every service the file
// resolves to, in whatever order Compose lists them.
func sessionServiceDefinition(data []byte, services []string) string {
	return serviceDefinition(data, slices.Sorted(slices.Values(services)))
}

// joinOpenSessionServices is joinSessionServices for an open session. It has no approval to compare
// with, so it matches what the session itself started: the file's bytes and service list, on the label
// that start wrote. Every service must still be up, with the ports it was given, or be a declared one-shot
// that finished (declaredOneShots), or `up` has work to do and the full launch path does it, with its wait.
// Reading the file opens no bind source, so the shared side of the lock is enough. It reports whether this
// box joined.
func joinOpenSessionServices(ctx context.Context, rt runtime.Runtime, spec RunSpec, projectRepo, file, owner string, sections *launchSections, exposedRoots ...string) (startedServices, bool) {
	if spec.SupervisorID == "" || owner == "" || spec.IsolatedParent != "" || spec.Review {
		return startedServices{}, false
	}
	unlock, err := lockServiceLaunch(ctx, serviceStartRepo(spec, projectRepo), false, RunSpec{}, sections)
	if err != nil {
		return startedServices{}, false
	}
	defer unlock()
	return openSessionServicesRunning(ctx, rt, spec, file, owner, exposedRoots...)
}

// openSessionServicesRunning is the check joinOpenSessionServices makes, for a caller that holds the shared
// side of the lock. A session asks it before it starts a box too, so a box that would join does not wait.
func openSessionServicesRunning(ctx context.Context, rt runtime.Runtime, spec RunSpec, file, owner string, exposedRoots ...string) (startedServices, bool) {
	if spec.SupervisorID == "" || owner == "" || spec.IsolatedParent != "" || spec.Review {
		return startedServices{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	bound, err := rt.FreezeCompose(ctx)
	if err != nil {
		return startedServices{}, false
	}
	data, err := readValidatedCompose(file, spec.Repo, spec.RepoReadOnly)
	if err != nil || refuseRunningServiceWriters(ctx, bound, spec.Repo) != nil {
		return startedServices{}, false
	}
	args, cleanup, _, err := snapshotComposeArgsForStartData(spec.Repo, file, owner, data, true, exposedRoots...)
	if err != nil {
		return startedServices{}, false
	}
	defer cleanup()
	services, err := resolvedComposeServices(bound, args, io.Discard)
	if err != nil {
		return startedServices{}, false
	}
	config, err := resolvedServiceConfig(ctx, bound, args)
	if err != nil {
		return startedServices{}, false
	}
	ports, err := parseServicePortsChecked(config, servicePortScope(spec.Repo, owner))
	if err != nil {
		return startedServices{}, false
	}
	oneShots, err := declaredOneShots(config)
	if err != nil {
		return startedServices{}, false
	}
	abs, err := filepath.Abs(file)
	if err != nil {
		return startedServices{}, false
	}
	definition := sessionServiceDefinition(data, services)
	for _, service := range services {
		container, found, err := bound.ObserveServiceContainer(ctx, map[string]string{
			composeProjectLabel:    ComposeProjectFor(spec.Repo, owner),
			composeWorkingDirLabel: filepath.Clean(filepath.Dir(abs)),
			composeServiceLabel:    service,
			composeOneoffLabel:     "False",
			serviceDefinitionLabel: definition,
		})
		if err != nil || !found {
			return startedServices{}, false
		}
		published := slices.DeleteFunc(slices.Clone(ports), func(port ServicePort) bool { return port.Service != service })
		switch {
		case container.Completed && oneShots[service] && len(published) == 0: // a one-shot step that did its work: `up` would only run it again
		case !serviceContainerUp(container) || len(eligibleObservedServicePorts(container, "", published)) != len(published):
			return startedServices{}, false
		}
	}
	return startedServices{names: services, ports: ports}, true
}

// declaredOneShots names the services the resolved file says finish by design: another service waits for
// one to complete, or it says it never restarts. Exiting with status 0 cannot tell on its own, since a
// long-running service that is shut down cleanly ends the same way, and `up` has to start that one again.
func declaredOneShots(configJSON []byte) (map[string]bool, error) {
	var cfg struct {
		Services map[string]struct {
			Restart   string `json:"restart"`
			DependsOn map[string]struct {
				Condition string `json:"condition"`
			} `json:"depends_on"`
		} `json:"services"`
	}
	if err := json.Unmarshal(configJSON, &cfg); err != nil {
		return nil, fmt.Errorf("parse resolved service configuration: %w", err)
	}
	oneShots := make(map[string]bool)
	for name, service := range cfg.Services {
		if service.Restart == "no" {
			oneShots[name] = true
		}
		for dependency, wait := range service.DependsOn {
			if wait.Condition == "service_completed_successfully" {
				oneShots[dependency] = true
			}
		}
	}
	return oneShots, nil
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
