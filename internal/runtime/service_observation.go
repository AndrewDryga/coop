package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ServicePortBinding is one host publication on a Compose-owned service container.
type ServicePortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

// ServiceContainerObservation is the bounded, non-secret subset Coop needs to decide whether an
// already-created Compose service may be advertised to a new box. It deliberately omits the
// container environment, command, mounts and healthcheck logs.
type ServiceContainerObservation struct {
	ID          string
	Status      string
	Completed   bool // it exited with status 0: how a one-shot step ends, and also how a service shut down cleanly ends
	Running     bool
	Paused      bool
	Healthcheck bool
	Health      string
	Ports       map[string][]ServicePortBinding
	Networks    map[string]bool
}

const serviceContainerFormat = `{"ID":{{json .Id}},"Labels":{{json .Config.Labels}},` +
	`"Status":{{json .State.Status}},"ExitCode":{{json .State.ExitCode}},"Running":{{json .State.Running}},"Paused":{{json .State.Paused}},` +
	`"Healthcheck":{{with index .Config "Healthcheck"}}{{with index . "Test"}}{{if eq (index . 0) "NONE"}}false{{else}}true{{end}}{{else}}false{{end}}{{else}}false{{end}},` +
	`"Health":{{with index .State "Health"}}{{json .Status}}{{else}}""{{end}},` +
	`"Ports":{{json .NetworkSettings.Ports}},"Networks":{{json .NetworkSettings.Networks}}}`

// ObserveServiceContainer returns the one container carrying every exact ownership label. No
// match is a normal false result; duplicate, malformed or unavailable observations fail closed.
// Both runtime calls are time-bounded and retain at most the declared output cap. A runtime frozen by
// FreezeCompose asks that daemon through its socket, never the current Docker context, and fails
// unless the same daemon still answers there once both calls are done.
func (r Runtime) ObserveServiceContainer(ctx context.Context, labels map[string]string) (ServiceContainerObservation, bool, error) {
	if r.kind() == runtimeAppleContainer {
		return ServiceContainerObservation{}, false, errors.New("compose service observation is unsupported by Apple container")
	}
	if ctx == nil || len(labels) == 0 || len(labels) > 8 {
		return ServiceContainerObservation{}, false, errors.New("invalid Compose service observation")
	}
	keys := make([]string, 0, len(labels))
	for key, value := range labels {
		if !dockerToken(key, 128) || value == "" || len(value) > 4096 || strings.ContainsAny(value, "\x00\r\n") {
			return ServiceContainerObservation{}, false, errors.New("invalid Compose service ownership label")
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	output := func(limit int, args ...string) ([]byte, error) { return dockerOutput(ctx, r.Name, nil, limit, args...) }
	verify := func() error { return nil }
	if r.composeEndpoint != "" {
		docker, err := bindDocker(ctx, r, r.composeEndpoint, r.composeDaemon, false)
		if err != nil {
			return ServiceContainerObservation{}, false, fmt.Errorf("bind the frozen Docker daemon: %w", err)
		}
		defer docker.Close()
		output = func(limit int, args ...string) ([]byte, error) { return docker.output(ctx, limit, args...) }
		verify = func() error { return docker.Verify(ctx) }
	}
	args := []string{"ps", "-q", "-a", "--no-trunc"}
	for _, key := range keys {
		args = append(args, "--filter", "label="+key+"="+labels[key])
	}
	out, err := output(64<<10, args...)
	if err != nil {
		return ServiceContainerObservation{}, false, errors.New("runtime service listing failed; availability is unknown")
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		if err := verify(); err != nil {
			return ServiceContainerObservation{}, false, err
		}
		return ServiceContainerObservation{}, false, nil
	}
	if len(ids) != 1 || !dockerHexID(ids[0]) {
		return ServiceContainerObservation{}, false, errors.New("runtime service ownership is ambiguous or malformed")
	}
	out, err = output(256<<10,
		"inspect", "--format", serviceContainerFormat, ids[0])
	if err != nil {
		return ServiceContainerObservation{}, false, errors.New("runtime service inspection failed; availability is unknown")
	}
	var wire struct {
		ID          *string                          `json:"ID"`
		Labels      map[string]string                `json:"Labels"`
		Status      *string                          `json:"Status"`
		ExitCode    *int                             `json:"ExitCode"`
		Running     *bool                            `json:"Running"`
		Paused      *bool                            `json:"Paused"`
		Healthcheck *bool                            `json:"Healthcheck"`
		Health      *string                          `json:"Health"`
		Ports       *map[string][]ServicePortBinding `json:"Ports"`
		Networks    *map[string]json.RawMessage      `json:"Networks"`
	}
	if json.Unmarshal(out, &wire) != nil || wire.ID == nil || wire.Status == nil || wire.Running == nil ||
		wire.Paused == nil || wire.Healthcheck == nil || wire.Health == nil || wire.Ports == nil || wire.Networks == nil ||
		*wire.ID != ids[0] || wire.Labels == nil || len(wire.Labels) > 256 || len(*wire.Ports) > 256 || len(*wire.Networks) > 256 {
		return ServiceContainerObservation{}, false, errors.New("runtime service inspection was malformed")
	}
	for _, key := range keys {
		if wire.Labels[key] != labels[key] {
			return ServiceContainerObservation{}, false, errors.New("runtime service ownership changed during inspection")
		}
	}
	for _, bindings := range *wire.Ports {
		if len(bindings) > 64 {
			return ServiceContainerObservation{}, false, errors.New("runtime service port inspection was malformed")
		}
	}
	networks := make(map[string]bool, len(*wire.Networks))
	for name := range *wire.Networks {
		if !dockerToken(name, 255) {
			return ServiceContainerObservation{}, false, errors.New("runtime service network inspection was malformed")
		}
		networks[name] = true
	}
	observed := ServiceContainerObservation{
		ID: *wire.ID, Status: *wire.Status, Running: *wire.Running,
		Completed: *wire.Status == "exited" && wire.ExitCode != nil && *wire.ExitCode == 0,
		Paused:    *wire.Paused, Healthcheck: *wire.Healthcheck, Health: *wire.Health,
		Ports: *wire.Ports, Networks: networks,
	}
	if err := verify(); err != nil {
		return ServiceContainerObservation{}, false, err
	}
	return observed, true, nil
}

// FailedService is one Compose service container that ended a start badly: it exited with a
// nonzero status or its healthcheck failed. Output is the end of what it printed, as the service
// wrote it: untrusted text the caller must make safe before showing it.
type FailedService struct {
	Service   string
	ExitCode  int
	Unhealthy bool
	Output    []byte
}

const failedServiceFormat = `{"Service":{{json (index .Config.Labels "com.docker.compose.service")}},` +
	`"Status":{{json .State.Status}},"ExitCode":{{json .State.ExitCode}},` +
	`"Health":{{with index .State "Health"}}{{json .Status}}{{else}}""{{end}}}`

// FailedServices lists the project's service containers that exited with a nonzero status or
// report unhealthy, each with the last tail lines it printed. A failed start reads them before its
// cleanup can remove them, so coop up can name the service from the Compose file and show why it
// stopped. Every call is bounded; a runtime frozen by FreezeCompose asks that daemon.
func (r Runtime) FailedServices(ctx context.Context, project string, tail int) ([]FailedService, error) {
	if r.kind() == runtimeAppleContainer {
		return nil, errors.New("compose service observation is unsupported by Apple container")
	}
	if ctx == nil || !dockerToken(project, 256) || tail < 1 || tail > 200 {
		return nil, errors.New("invalid failed-service observation")
	}
	output := func(limit int, combined bool, args ...string) ([]byte, error) {
		return dockerCommandOutput(ctx, r.Name, nil, dockerControlTimeout, limit, combined, args...)
	}
	if r.composeEndpoint != "" {
		docker, err := bindDocker(ctx, r, r.composeEndpoint, r.composeDaemon, false)
		if err != nil {
			return nil, fmt.Errorf("bind the frozen Docker daemon: %w", err)
		}
		defer docker.Close()
		output = func(limit int, combined bool, args ...string) ([]byte, error) {
			return dockerCommandOutput(ctx, docker.binary, docker.env, dockerControlTimeout, limit, combined,
				append([]string{"--config", docker.clientConfig, "--host", docker.endpoint}, args...)...)
		}
	}
	out, err := output(64<<10, false, "ps", "-q", "-a", "--no-trunc", "--filter", "label=com.docker.compose.project="+project)
	if err != nil {
		return nil, errors.New("runtime service listing failed")
	}
	var failed []FailedService
	for i, id := range strings.Fields(string(out)) {
		if i == 32 || !dockerHexID(id) {
			break
		}
		data, err := output(16<<10, false, "inspect", "--format", failedServiceFormat, id)
		var state struct {
			Service, Status, Health string
			ExitCode                int
		}
		if err != nil || json.Unmarshal(data, &state) != nil || state.Service == "" {
			continue // removed since the listing, or not a Compose service: nothing to name
		}
		exited := (state.Status == "exited" || state.Status == "dead") && state.ExitCode != 0
		if !exited && state.Health != "unhealthy" {
			continue
		}
		logs, _ := output(64<<10, true, "logs", "--tail", strconv.Itoa(tail), id)
		failed = append(failed, FailedService{Service: state.Service, ExitCode: state.ExitCode, Unhealthy: !exited, Output: logs})
	}
	sort.Slice(failed, func(i, j int) bool { return failed[i].Service < failed[j].Service })
	return failed, nil
}
