package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
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
	Labels      map[string]string
	Status      string
	Running     bool
	Paused      bool
	Healthcheck bool
	Health      string
	Ports       map[string][]ServicePortBinding
	Networks    map[string]bool
}

const serviceContainerFormat = `{"ID":{{json .Id}},"Labels":{{json .Config.Labels}},` +
	`"Status":{{json .State.Status}},"Running":{{json .State.Running}},"Paused":{{json .State.Paused}},` +
	`"Healthcheck":{{if .Config.Healthcheck}}{{if .Config.Healthcheck.Test}}{{if eq (index .Config.Healthcheck.Test 0) "NONE"}}false{{else}}true{{end}}{{else}}false{{end}}{{else}}false{{end}},` +
	`"Health":{{if .State.Health}}{{json .State.Health.Status}}{{else}}""{{end}},` +
	`"Ports":{{json .NetworkSettings.Ports}},"Networks":{{json .NetworkSettings.Networks}}}`

// ObserveServiceContainer returns the one container carrying every exact ownership label. No
// match is a normal false result; duplicate, malformed or unavailable observations fail closed.
// Both runtime calls are time-bounded and retain at most the declared output cap.
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
	args := []string{"ps", "-q", "-a", "--no-trunc"}
	for _, key := range keys {
		args = append(args, "--filter", "label="+key+"="+labels[key])
	}
	out, err := dockerOutput(ctx, r.Name, interruptibleProcessEnvironment(), 64<<10, args...)
	if err != nil {
		return ServiceContainerObservation{}, false, errors.New("runtime service listing failed; availability is unknown")
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return ServiceContainerObservation{}, false, nil
	}
	if len(ids) != 1 || !dockerHexID(ids[0]) {
		return ServiceContainerObservation{}, false, errors.New("runtime service ownership is ambiguous or malformed")
	}
	out, err = dockerOutput(ctx, r.Name, interruptibleProcessEnvironment(), 256<<10,
		"inspect", "--format", serviceContainerFormat, ids[0])
	if err != nil {
		return ServiceContainerObservation{}, false, errors.New("runtime service inspection failed; availability is unknown")
	}
	var wire struct {
		ID          *string                          `json:"ID"`
		Labels      map[string]string                `json:"Labels"`
		Status      *string                          `json:"Status"`
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
		ID: *wire.ID, Labels: wire.Labels, Status: *wire.Status, Running: *wire.Running,
		Paused: *wire.Paused, Healthcheck: *wire.Healthcheck, Health: *wire.Health,
		Ports: *wire.Ports, Networks: networks,
	}
	return observed, true, nil
}
