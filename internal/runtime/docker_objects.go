package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"
)

// DockerRef is exact owner custody, not a discovery filter. Before creation ID
// is empty; once a container ID is known every mutation uses that full ID.
type DockerRef struct {
	Name, ID string
	Labels   map[string]string
}

func (r DockerRef) valid(volume bool) bool {
	if !dockerName(r.Name) || len(r.Labels) == 0 || len(r.Labels) > 32 || r.ID != "" && (volume && r.ID != r.Name || !volume && !dockerHexID(r.ID)) {
		return false
	}
	for key, value := range r.Labels {
		if !dockerToken(key, 128) || !dockerToken(value, 512) || strings.Contains(key, "=") {
			return false
		}
	}
	return true
}

func (r DockerRef) matches(name, id string, labels map[string]string) bool {
	if name != r.Name || r.ID != "" && id != r.ID {
		return false
	}
	for key, value := range r.Labels {
		if labels[key] != value {
			return false
		}
	}
	return true
}

type DockerContainer struct {
	ID, Name, Image, User, NetworkMode, PidMode, IpcMode, UsernsMode, CgroupnsMode string
	CgroupParent                                                                   string
	Entrypoint, Cmd, Shell                                                         []string
	Healthcheck                                                                    *DockerHealthcheck
	RestartPolicy                                                                  string
	Labels                                                                         map[string]string
	CapAdd, CapDrop, SecurityOpt                                                   []string
	Privileged, AutoRemove, ReadonlyRootfs, Tty, OpenStdin                         bool
	RestartCount                                                                   int
	State                                                                          DockerContainerState
	Mounts                                                                         []DockerMount
	Tmpfs                                                                          map[string]string
}

type DockerContainerState struct {
	Status                string
	Running, Paused       bool
	OOMKilled             bool
	ExitCode              int
	StartedAt, FinishedAt time.Time
}

type DockerMount struct {
	Type, Name, Source, Destination string
	RW                              bool
}

// Secret file case covers can require thousands of mounts. Observation must
// accommodate the mount plans accepted at creation, including their JSON overhead.
const maxDockerMounts = 8192

// The Engine wire response repeats bind sources in HostConfig and Mounts.
const maxDockerContainerObservation = 8 << 20

// InspectContainer distinguishes confirmed current absence from unavailable
// inspection. Absence does not settle a still-in-flight creating intention.
func (d *Docker) InspectContainer(ctx context.Context, ref DockerRef) (DockerContainer, bool, error) {
	if !ref.valid(false) {
		return DockerContainer{}, false, errors.New("invalid exact Docker container reference")
	}
	if err := d.Verify(ctx); err != nil {
		return DockerContainer{}, false, err
	}
	target := ref.ID
	if target == "" {
		target = ref.Name
	}
	value, present, err := d.inspectContainerJSON(ctx, target)
	if err != nil || !present {
		return DockerContainer{}, false, err
	}
	if !dockerHexID(value.ID) || !strings.HasPrefix(value.Name, "/") || len(value.Mounts) > maxDockerMounts || len(value.Tmpfs) > 32 || len(value.Labels) > 256 || value.RestartCount < 0 ||
		!slices.Contains([]string{"created", "running", "paused", "restarting", "removing", "exited", "dead"}, value.State.Status) {
		return DockerContainer{}, false, errors.New("invalid Docker container observation")
	}
	value.Name = strings.TrimPrefix(value.Name, "/")
	if !ref.matches(value.Name, value.ID, value.Labels) {
		return DockerContainer{}, false, ErrDockerOwnership
	}
	return value, true, nil
}

type DockerVolume struct {
	Name, Driver, Scope, CreatedAt string
	Labels, Options                map[string]string
}

func (d *Docker) InspectVolume(ctx context.Context, ref DockerRef) (DockerVolume, bool, error) {
	if !ref.valid(true) {
		return DockerVolume{}, false, errors.New("invalid exact Docker volume reference")
	}
	if err := d.Verify(ctx); err != nil {
		return DockerVolume{}, false, err
	}
	data, err := d.output(ctx, 64<<10, "volume", "inspect", "--format", `{"Name":{{json .Name}},"Driver":{{json .Driver}},"Scope":{{json .Scope}},"CreatedAt":{{json .CreatedAt}},"Labels":{{json .Labels}},"Options":{{json .Options}}}`, ref.Name)
	if err != nil {
		absent, checkErr := d.volumeAbsent(ctx, ref.Name)
		if checkErr == nil && absent {
			return DockerVolume{}, false, nil
		}
		return DockerVolume{}, false, errors.Join(err, checkErr)
	}
	var value DockerVolume
	if json.Unmarshal(data, &value) != nil || value.Driver != "local" || value.Scope != "local" || len(value.Options) != 0 || value.CreatedAt == "" {
		return DockerVolume{}, false, errors.New("Docker volume driver conflict")
	}
	if !ref.matches(value.Name, value.Name, value.Labels) {
		return DockerVolume{}, false, ErrDockerOwnership
	}
	return value, true, nil
}

func (d *Docker) volumeAbsent(ctx context.Context, name string) (bool, error) {
	if err := d.Verify(ctx); err != nil {
		return false, err
	}
	data, err := d.output(ctx, 64<<10, "volume", "ls", "--filter", "name="+name, "--format", "{{json .Name}}")
	if err != nil {
		return false, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		var item string
		if err := decoder.Decode(&item); errors.Is(err, io.EOF) {
			return true, nil
		} else if err != nil || !dockerName(item) {
			return false, errors.New("invalid Docker volume absence observation")
		}
		if item == name {
			return false, nil
		}
	}
}

func dockerLabelArgs(labels map[string]string) []string {
	var args []string
	for _, key := range slices.Sorted(maps.Keys(labels)) {
		args = append(args, "--label", key+"="+labels[key])
	}
	return args
}

// CreateVolume never treats an existing volume as a successful fresh create.
// Reconciliation of an ambiguous prior call must use InspectVolume explicitly.
func (d *Docker) CreateVolume(ctx context.Context, ref DockerRef) (DockerVolume, error) {
	if err := d.VerifyLaunch(ctx); err != nil {
		return DockerVolume{}, errors.Join(ErrDockerCreateNotAttempted, err)
	}
	_, present, err := d.InspectVolume(ctx, ref)
	if err != nil || present || ref.ID != "" {
		return DockerVolume{}, errors.Join(ErrDockerCreateNotAttempted, errors.New("Docker volume creation requires a fresh intention"), err)
	}
	args := append([]string{"volume", "create", "--driver", "local"}, dockerLabelArgs(ref.Labels)...)
	data, err := d.output(ctx, 1024, append(args, ref.Name)...)
	if err != nil {
		if errors.Is(err, errDockerCommandNotStarted) {
			err = errors.Join(ErrDockerCreateNotAttempted, err)
		}
		return DockerVolume{}, err
	}
	if strings.TrimSpace(string(data)) != ref.Name {
		return DockerVolume{}, errors.New("Docker volume create outcome is invalid")
	}
	value, present, err := d.InspectVolume(ctx, ref)
	if err != nil || !present {
		return DockerVolume{}, errors.Join(errors.New("Docker volume create outcome is unavailable"), err)
	}
	return value, nil
}

func (d *Docker) RemoveVolume(ctx context.Context, ref DockerRef) error {
	_, present, err := d.InspectVolume(ctx, ref)
	if err != nil || !present {
		return err
	}
	_, removeErr := d.output(ctx, 1024, "volume", "rm", ref.Name)
	absent, err := d.volumeAbsent(ctx, ref.Name)
	if err == nil && absent {
		return nil
	}
	return errors.Join(errors.New("Docker volume cleanup remains pending"), removeErr, err)
}

// dockerNetworkName is the bounded Compose network spelling this launcher will
// touch. It is never an operator string: the caller derives it from the run's
// own Compose project.
func dockerNetworkName(value string) bool {
	if len(value) == 0 || len(value) > 128 || strings.HasPrefix(value, "-") {
		return false
	}
	return strings.Trim(value, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_.-") == ""
}

// ConnectNetwork attaches ONE exactly-owned container to one existing network.
// It is not `docker network create`: a network this daemon does not already
// have is an error, so a launch cannot invent connectivity for itself.
func (d *Docker) ConnectNetwork(ctx context.Context, network string, ref DockerRef, aliases ...string) error {
	if !dockerNetworkName(network) || !ref.valid(false) || ref.ID == "" {
		return errors.New("invalid exact Docker network attachment")
	}
	for _, alias := range aliases {
		if !dockerName(alias) {
			return errors.New("invalid exact Docker network attachment")
		}
	}
	if err := d.VerifyLaunch(ctx); err != nil {
		return err
	}
	// Confirm ownership before mutating: the ref's labels prove this is the
	// container this run created, not another box that reused the name.
	if _, present, err := d.InspectContainer(ctx, ref); err != nil || !present {
		return errors.Join(errors.New("network attachment target is not this run's container"), err)
	}
	args := []string{"network", "connect"}
	for _, alias := range aliases {
		args = append(args, "--alias", alias)
	}
	args = append(args, network, ref.ID)
	if _, err := d.output(ctx, 64<<10, args...); err != nil {
		return errors.Join(errors.New("cannot attach the gateway to the project's services network"), err)
	}
	return nil
}

// NetworkMembers returns the IPv4 address each container on this network holds,
// keyed by full container ID. It reads the runtime once, at launch: the box
// never resolves a sidecar itself, so an address that changes later cannot
// silently redirect a grant — the run keeps the address it was launched with.
func (d *Docker) NetworkMembers(ctx context.Context, network string) (map[string]netip.Addr, error) {
	if !dockerNetworkName(network) {
		return nil, errors.New("invalid Docker network name")
	}
	if err := d.Verify(ctx); err != nil {
		return nil, err
	}
	data, err := d.output(ctx, 256<<10, "network", "inspect", "--format", "{{json .Containers}}", network)
	if err != nil {
		return nil, errors.Join(errors.New("the project's services network is unavailable"), err)
	}
	var members map[string]struct{ IPv4Address string }
	if json.Unmarshal(data, &members) != nil || len(members) > 512 {
		return nil, errors.New("invalid Docker network observation")
	}
	out := make(map[string]netip.Addr, len(members))
	for id, member := range members {
		prefix, err := netip.ParsePrefix(member.IPv4Address)
		if !dockerHexID(id) || err != nil || !prefix.Addr().Is4() {
			continue // an IPv6-only or malformed endpoint is not an IPv4 grant
		}
		out[id] = prefix.Addr()
	}
	return out, nil
}

// ComposeServiceID resolves one Compose service of one project to its single
// running container ID. Ambiguity fails: two replicas are two addresses, and a
// grant names exactly one destination.
func (d *Docker) ComposeServiceID(ctx context.Context, project, service string) (string, error) {
	if !dockerToken(project, 256) || !dockerToken(service, 256) {
		return "", errors.New("invalid Compose service reference")
	}
	if err := d.Verify(ctx); err != nil {
		return "", err
	}
	data, err := d.output(ctx, 64<<10, "container", "ls", "--no-trunc", "--filter", "label=com.docker.compose.project="+project,
		"--filter", "label=com.docker.compose.service="+service, "--format", "{{.ID}}")
	if err != nil {
		return "", err
	}
	ids := strings.Fields(string(data))
	if len(ids) != 1 || !dockerHexID(ids[0]) {
		return "", errors.New("service " + service + " is not running as exactly one container")
	}
	return ids[0], nil
}

// maxDockerNetworks bounds one inventory. A daemon with more networks than this
// is not a topology a filtered run can promise to protect.
const maxDockerNetworks = 256

// DockerNetwork is one daemon network's IPv4 topology: the subnets it allocates
// to containers and the gateway addresses the daemon itself holds in them.
type DockerNetwork struct {
	Name     string
	Internal bool
	Subnets  []netip.Prefix
	Gateways []netip.Addr
}

// Networks inventories every network this daemon has. A filtered run protects
// them: a granted CIDR that happens to cover a bridge subnet would otherwise
// reach sibling containers, other sessions' boxes and the daemon's own gateway,
// none of which any rule may grant.
//
// Local networks include their IPAM configuration in the list response. Swarm
// summaries can omit node-local allocations, so those need exact-ID inspection.
// An incomplete response never establishes which networks the policy must protect.
func (d *Docker) Networks(ctx context.Context) ([]DockerNetwork, error) {
	if ctx == nil {
		return nil, errors.Join(errDockerCommandNotStarted, errors.New("invalid bounded Docker operation"))
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := d.Verify(ctx); err != nil {
		return nil, err
	}
	// Bound all wire data, including optional Swarm inspections, before decoding.
	remaining := int64(4 << 20)
	read := func(path string) ([]byte, error) {
		response, err := d.daemonGet(ctx, path)
		if err != nil {
			return nil, errors.Join(errors.New("Docker network inventory unavailable"), ctx.Err())
		}
		defer response.Body.Close()
		data, err := io.ReadAll(io.LimitReader(response.Body, remaining+1))
		if err != nil {
			return nil, errors.Join(errors.New("Docker network inventory incomplete"), ctx.Err())
		}
		remaining -= int64(len(data))
		if remaining < 0 {
			return nil, errors.New("Docker observation exceeded its bound; outcome unknown")
		}
		if err := d.Verify(ctx); err != nil {
			return nil, err
		}
		if response.StatusCode != http.StatusOK {
			return nil, errors.New("Docker network inventory unavailable")
		}
		return data, nil
	}
	data, err := read("/networks")
	if err != nil {
		return nil, err
	}
	type observation struct {
		ID, Name, Scope string
		Internal        bool
		IPAM            *struct {
			Config []struct{ Subnet, Gateway string }
		}
	}
	var values []observation
	if json.Unmarshal(data, &values) != nil {
		return nil, errors.New("invalid Docker network inventory")
	}
	if len(values) == 0 {
		return nil, errors.New("the container runtime reported no networks at all")
	}
	if len(values) > maxDockerNetworks {
		return nil, errors.New("the container runtime has more networks than a filtered run can protect")
	}
	var out []DockerNetwork
	seen := map[string]bool{}
	for _, value := range values {
		// Swarm IDs are opaque strings, not container-style hex IDs.
		if !dockerToken(value.ID, 128) || seen[value.ID] || value.Name == "" || value.IPAM == nil {
			return nil, errors.New("invalid Docker network inventory")
		}
		seen[value.ID] = true
		if value.Scope != "local" && value.Scope != "global" && value.Scope != "swarm" {
			return nil, errors.New("invalid Docker network scope")
		}
		if value.Scope == "swarm" {
			// List prefers the cluster record; inspect prefers the local record
			// and includes subnets allocated by node-local drivers such as bridge.
			data, err := read("/networks/" + url.PathEscape(value.ID))
			if err != nil {
				return nil, err
			}
			var inspected observation
			if json.Unmarshal(data, &inspected) != nil || inspected.ID != value.ID || inspected.Name != value.Name || inspected.IPAM == nil {
				return nil, errors.New("invalid Docker network inspection")
			}
			value = inspected
		}
		network := DockerNetwork{Name: value.Name, Internal: value.Internal}
		for _, config := range value.IPAM.Config {
			if prefix, err := netip.ParsePrefix(config.Subnet); err == nil && prefix.Addr().Is4() {
				network.Subnets = append(network.Subnets, prefix.Masked())
			}
			if address, err := netip.ParseAddr(config.Gateway); err == nil && address.Is4() {
				network.Gateways = append(network.Gateways, address)
			}
		}
		out = append(out, network)
	}
	if projection, err := json.Marshal(out); err != nil || len(projection) > 1<<20 {
		return nil, errors.New("Docker network inventory exceeded its bound")
	}
	return out, nil
}
