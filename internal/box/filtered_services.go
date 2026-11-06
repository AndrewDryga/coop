package box

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkgateway"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/ui"
	"go.yaml.in/yaml/v3"
)

// servicesNeedRules refuses a filtered box in a project that has services but
// no approved service grant. An open box joins the services' network; a
// filtered one never does, so each service it uses is a rule the person asks
// for and approves. The refusal drafts those rules from the Compose file, whole,
// so they can be pasted as they are wherever the file declares a service's port.
func servicesNeedRules(spec RunSpec, composeFile string) error {
	refusal := &ServicesNeedRules{name: networkLaunchName(spec)}
	if composeFile == "" {
		refusal.Reason = "a filtered box does not join the network COOP_SERVICES_NET names"
		refusal.TurnOff = "Unset COOP_SERVICES_NET, or set box.network: false in " + project.File
		return refusal
	}
	refusal.Reason = "filtered boxes reach project services only through approved network rules"
	refusal.Ask = "Ask for each service it uses under box.egress_rules in " + project.File + ":"
	refusal.TurnOff = "To start without project services, set box.network: false."
	refusal.Services, refusal.digests = serviceRuleDrafts(composeFile, spec.Repo, spec.RepoReadOnly)
	drafts := refusal.Services
	if len(drafts) == 0 { // a Compose file coop cannot read still gets the rule's whole shape
		drafts = []ServiceDraft{{Name: "<service>"}}
	}
	rules := make([]string, 0, len(drafts))
	for _, draft := range drafts {
		if len(draft.Ports) == 0 {
			refusal.Fill = "Replace <port> with the port the service listens on."
		}
		rules = append(rules, draft.RuleText())
	}
	refusal.Rules = strings.Join(rules, "\n")
	return refusal
}

// forHandshake applies HandshakeOnly: nothing runs in that box to use a port or join a service network,
// or to change the project, so it sees the project read-only.
func (spec RunSpec) forHandshake() RunSpec {
	if spec.HandshakeOnly {
		spec.Network, spec.Serve, spec.servePorts, spec.RepoReadOnly = false, false, nil, true
	}
	return spec
}

// checkServicesHaveRules is the refusal itself, shared by the launch and an editor session asking early.
func checkServicesHaveRules(cfg *config.Config, spec RunSpec, composeFile string, approved []egress.Grant) error {
	if len(approved) == 0 && spec.Network && (composeFile != "" || cfg.ServicesNet != "") {
		return servicesNeedRules(spec, composeFile)
	}
	return nil
}

// ServicesNeedRules is a filtered box that cannot start until a person gives the project's services
// network rules or turns them off. An editor session waits for that instead of exiting. The refusal
// comes in parts because a thread and `coop approve` each show it their own way. Every part names only
// Coop's words and validated service names and ports (serviceRuleDrafts).
type ServicesNeedRules struct {
	name     string            // who cannot start
	Reason   string            // why, as a clause that reads after "cannot start because"
	Ask      string            // where the rules go; "" when there are none
	Rules    string            // the drafted rules, indented to paste under box.egress_rules; "" with no Compose file
	Fill     string            // what to replace in Rules before the file accepts them; "" when nothing
	TurnOff  string            // how to start without the services instead
	Services []ServiceDraft    // the services Rules drafts, without the placeholder an unreadable file gets
	digests  map[string]string // the definitions behind the ports shown; nil when they could not be pinned
}

func (e *ServicesNeedRules) Error() string { return e.Headline() + "\n\n  " + e.TerminalRemedy() }

// Headline says who cannot start and why, as a terminal's first line.
func (e *ServicesNeedRules) Headline() string { return e.name + " cannot start because " + e.Reason }

// TerminalRemedy is everything a person can do about it, as a terminal shows it below Headline: the
// lines carry their own indent, except the first, which is the caller's.
func (e *ServicesNeedRules) TerminalRemedy() string {
	if e.Rules == "" {
		return e.TurnOff
	}
	remedy := e.pasteRules() + "  Then review it: coop approve\n"
	if len(e.Offer()) > 0 {
		remedy += "  Or run coop approve in a terminal, which offers to add them for you.\n"
	}
	return remedy + "  " + e.TurnOff
}

// PasteRemedy is TerminalRemedy for a place that has already failed to add the rules itself.
func (e *ServicesNeedRules) PasteRemedy() string {
	return e.pasteRules() + "  Then review it: coop approve\n  " + e.TurnOff
}

func (e *ServicesNeedRules) pasteRules() string {
	remedy := e.Ask + "\n" + e.Rules + "\n"
	if e.Fill != "" {
		remedy += "  " + e.Fill + "\n"
	}
	return remedy
}

// Offer is what `coop approve` may offer to add: the services whose ports the Compose file states,
// with their definitions pinned so that what gets approved is what was shown. A service with no
// stated port is never given a guessed one, so it stays the person's (NoPort).
func (e *ServicesNeedRules) Offer() []ServiceDraft {
	if e.digests == nil {
		return nil
	}
	return slices.DeleteFunc(slices.Clone(e.Services), func(draft ServiceDraft) bool { return len(draft.Ports) == 0 })
}

// NoPort is the services Offer leaves out, whose rules only a person can finish.
func (e *ServicesNeedRules) NoPort() []ServiceDraft {
	return slices.DeleteFunc(slices.Clone(e.Services), func(draft ServiceDraft) bool { return len(draft.Ports) != 0 })
}

// ServiceDraft is one Compose service a filtered box could ask for, with the TCP ports its definition
// declares (none when it declares none).
type ServiceDraft struct {
	Name  string
	Ports []int
}

// RuleText is the draft as a rule to paste, with a placeholder for a port nobody stated.
func (d ServiceDraft) RuleText() string {
	ports := make([]string, 0, len(d.Ports))
	for _, port := range d.Ports {
		ports = append(ports, strconv.Itoa(port))
	}
	if len(ports) == 0 {
		ports = []string{"<port>"}
	}
	return fmt.Sprintf("    - to: {service: %q}\n      protocol: tcp\n      ports: [%s]", d.Name, strings.Join(ports, ", "))
}

// serviceRuleDrafts lists the services of a Compose file in name order, and pins from those same bytes
// the definitions of the ones with stated ports. A name no rule can carry, which is anything but one
// lowercase DNS label, is left out, and a file coop cannot read or validate drafts nothing.
func serviceRuleDrafts(composeFile, repo string, repoReadOnly bool) ([]ServiceDraft, map[string]string) {
	data, err := readValidatedCompose(composeFile, repo, repoReadOnly)
	if err != nil {
		return nil, nil
	}
	var doc composeDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, nil
	}
	var drafts []ServiceDraft
	var known []string
	for name, service := range doc.Services {
		if label, err := egress.NormalizeLabel(name); err == nil && label == name {
			ports := declaredPorts(service)
			drafts = append(drafts, ServiceDraft{Name: name, Ports: ports})
			if len(ports) != 0 {
				known = append(known, name)
			}
		}
	}
	slices.SortFunc(drafts, func(a, b ServiceDraft) int { return strings.Compare(a.Name, b.Name) })
	digests, err := composeServiceDigestsData(data, known)
	if err != nil {
		return drafts, nil
	}
	return drafts, digests
}

// declaredPorts are the TCP container ports a Compose service declares: its
// expose entries and the container side of its ports. A range, a variable or
// a UDP port names no single TCP port the rule could carry.
func declaredPorts(service serviceSpec) []int {
	var ports []int
	add := func(entry any) {
		var spec string
		switch value := entry.(type) {
		case int:
			spec = strconv.Itoa(value)
		case string:
			spec = value
		case map[string]any: // the long form: {target: 5432, published: …, protocol: tcp}
			if protocol, _ := value["protocol"].(string); protocol != "" && protocol != "tcp" {
				return
			}
			spec = fmt.Sprint(value["target"])
		}
		spec, protocol, _ := strings.Cut(spec, "/")
		if protocol != "" && protocol != "tcp" {
			return
		}
		// The short form is [host_ip:][host_port:]container_port; the container side is last.
		spec = spec[strings.LastIndex(spec, ":")+1:]
		if port, err := strconv.Atoi(spec); err == nil && port > 0 && port <= 65535 && !slices.Contains(ports, port) {
			ports = append(ports, port)
		}
	}
	if expose, ok := service.Expose.([]any); ok {
		for _, entry := range expose {
			add(entry)
		}
	}
	for _, entry := range service.Ports {
		add(entry)
	}
	slices.Sort(ports)
	return ports
}

// serviceGrants lists the sidecars this policy actually approved. A Compose
// file is a request; only an approved `service:` grant is authority.
func serviceGrants(policy egress.Snapshot) []egress.Grant {
	var out []egress.Grant
	for _, grant := range policy.Grants {
		if grant.Rule.To.Service != "" {
			out = append(out, grant)
		}
	}
	return out
}

// resolveServiceBindings creates the approved service closure without starting
// it, then binds the exact addresses the gateway and Compose will use.
const (
	filteredServiceNetworkKey = "coop_filtered"
	filteredServiceProxyAlias = "coop-gateway"
)

type preparedFilteredServices struct {
	runtime        runtime.Runtime
	repo           string
	file           string
	data           []byte
	owner          string
	isolatedParent string
	readOnly       bool
	override       string
	roots          []string
	selected       []string
	names          []string
	sections       *launchSections
	addresses      map[string]netip.Addr
	cleanup        func()
	allowCases     bool
	joined         bool // an earlier box of this session started them, and this one must not touch them
}

func (s *preparedFilteredServices) check(ctx context.Context, docker filteredDocker, network, project string) error {
	if s == nil {
		return nil
	}
	members, err := docker.NetworkMembers(ctx, network)
	if err != nil {
		return err
	}
	for name, expected := range s.addresses {
		id, err := docker.ComposeServiceID(ctx, project, name)
		if err != nil {
			return fmt.Errorf("prepared service %q: %w", name, err)
		}
		if members[id] != expected {
			return fmt.Errorf("prepared service %q did not start at its restricted address", name)
		}
	}
	return nil
}

func (s *preparedFilteredServices) start(ctx context.Context) (result error) {
	if s == nil {
		return nil
	}
	ctx = context.WithValue(ctx, isolatedServiceParentKey{}, s.isolatedParent)
	ctx = context.WithValue(ctx, deferServiceShadowsKey{}, s.runtime.Name == "docker" && filesystemProfileCandidate(s.repo))
	if err := refuseRunningServiceWriters(ctx, s.runtime, s.repo); err != nil {
		return err
	}
	// Preparation releases the repository launch lock while the gateway starts. Recheck bind
	// paths and rebuild shadows under the final lock: a box may have changed a symlink or
	// introduced a new secret beneath a directory bind in the meantime.
	if err := validateComposeData(s.data, s.file, s.repo, s.readOnly); err != nil {
		return fmt.Errorf("service bind sources changed before launch: %w", err)
	}
	args, cleanup, _, err := snapshotComposeArgsForStartPinned(ctx, s.runtime, s.repo, s.file, s.owner, s.data, s.selected, false, s.roots...)
	if err != nil {
		return err
	}
	defer cleanup()
	if s.override != "" {
		args = append(args, "-f", s.override)
	}
	var stderr bytes.Buffer
	filesystem, err := prepareServiceFilesystemStart(ctx, s.runtime, s.repo, s.file, s.data, args, s.selected, s.allowCases, false)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, filesystem.abort()) }()
	args = filesystem.args
	// Prep and launch are separated by network setup. Replace the stopped candidates immediately
	// before starting so no stale daemon mount survives from the earlier preparation phase.
	create := append(append([]string(nil), args...), "up", "--no-start", "--force-recreate")
	create = append(create, s.selected...)
	if len(filesystem.admissions) > 0 {
		if err := filesystem.create(ctx, s.runtime, s.selected, true, io.Discard, &stderr); err != nil {
			return err
		}
	} else {
		if err := runCompose(s.runtime, io.Discard, &stderr, "up --no-start", create); err != nil {
			return composeFailure("prepare filtered services at launch", err, stderr.String())
		}
	}
	upArgs := append(append([]string(nil), args...), "up", "-d", "--wait")
	upArgs = append(upArgs, s.selected...)
	stderr.Reset()
	if err := runCompose(s.runtime, io.Discard, &stderr, "up", upArgs); err != nil {
		return &ServicesNotStarted{Err: composeFailure("a filtered box needs this project's approved sidecars running, and starting them failed", err, stderr.String())}
	}
	if err := filesystem.retain(); err != nil {
		return err
	}
	if len(filesystem.admissions) > 0 {
		if _, err := RecoverFilesystemAdmissions(ctx, s.runtime, s.repo); err != nil {
			return err
		}
	}
	if s.sections != nil && s.sections.loop {
		s.sections.services(s.names)
	}
	return nil
}

func resolveServiceBindings(ctx context.Context, docker filteredDocker, rt runtime.Runtime, spec RunSpec, composeFile string,
	approval *networkstate.Approval, grants []egress.Grant, sections *launchSections, exposedRoots []string) (string, []networkgateway.ServiceBinding, []networkgateway.ServiceProxyClient, *preparedFilteredServices, error) {
	ctx = context.WithValue(ctx, isolatedServiceParentKey{}, spec.IsolatedParent)
	if sections != nil {
		sections.servicesPreparing()
	}
	if composeFile == "" {
		names := make([]string, 0, len(grants))
		for _, grant := range grants {
			names = append(names, grant.Rule.To.Service)
		}
		slices.Sort(names)
		err := fmt.Errorf("the approved rules name the Compose service(s) %v, but this project has no %s",
			names, project.DefaultCompose)
		if sections != nil && sections.loop {
			sections.servicesRefused(err.Error())
			return "", nil, nil, nil, ui.Reported(err)
		}
		return "", nil, nil, nil, err
	}
	projectRepo := spec.ActivityRepo
	if projectRepo == "" {
		projectRepo = spec.Repo
	}
	if joined := joinSessionServices(ctx, docker, rt, spec, projectRepo, composeFile, approval, grants, sections); joined != nil {
		bindings, clients, err := filteredServiceGrants(grants, joined.closure, joined.addresses, joined.network)
		if err != nil {
			return "", nil, nil, nil, err
		}
		return joined.network, bindings, clients, &preparedFilteredServices{joined: true, addresses: joined.addresses}, nil
	}
	unlockLaunch, err := lockServiceStart(ctx, serviceStartRepo(spec, projectRepo), spec, sections)
	if err != nil {
		return "", nil, nil, nil, err
	}
	defer unlockLaunch()
	// Review the exact validated bytes that every Compose invocation below uses, before anything
	// reaches the daemon: a service changed since its approval is refused without binding, querying
	// or starting anything.
	data, err := readValidatedCompose(composeFile, spec.Repo, spec.RepoReadOnly)
	if err != nil {
		return "", nil, nil, nil, err
	}
	if err := checkApprovedServicesData(approval, data); err != nil {
		if sections != nil && sections.loop {
			sections.servicesRefused(err.Error())
			return "", nil, nil, nil, ui.Reported(err)
		}
		return "", nil, nil, nil, err
	}
	bound, err := rt.FreezeCompose(ctx)
	if err != nil {
		return "", nil, nil, nil, fmt.Errorf("bind filtered services to Docker daemon: %w", err)
	}
	if live, ok := docker.(interface {
		Endpoint() string
		Info() runtime.DockerInfo
	}); ok {
		endpoint, daemon := bound.ComposeBinding()
		if endpoint != live.Endpoint() || daemon != live.Info().ID {
			return "", nil, nil, nil, errors.New("filtered service Docker daemon differs from the gateway daemon")
		}
	}
	rt = bound
	selected := grantedServices(grants)
	if err := refuseRunningServiceWriters(ctx, rt, spec.Repo); err != nil {
		return "", nil, nil, nil, err
	}
	var doc composeDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return "", nil, nil, nil, err
	}
	closure, err := composeServiceClosure(doc.Services, selected)
	if err != nil {
		return "", nil, nil, nil, err
	}
	noticeHidden := sections == nil || !sections.loop
	owner := runServiceOwner(spec)
	args, cleanupSnapshot, hidden, err := snapshotComposeArgsForStartPinned(ctx, rt, spec.Repo, composeFile, owner, data, selected, false, exposedRoots...)
	if err != nil {
		var refused *ComposeRefused
		if sections != nil && sections.loop && errors.As(err, &refused) {
			sections.servicesRefused(err.Error())
			return "", nil, nil, nil, ui.Reported(fmt.Errorf("a filtered box needs this project's approved sidecars running: %w", err))
		}
		return "", nil, nil, nil, err
	}
	if sections != nil {
		sections.serviceSecrets(hidden, composeFile)
	}
	if len(hidden) > 0 && noticeHidden {
		ui.Note("")
		ui.Warn("Services cannot read %s because it looks like a secret. Run `coop up` in a terminal to review eligible read-only files; secret directories and writable binds remain hidden",
			safeServicePaths(hidden))
	}
	keepSnapshot := false
	defer func() {
		if !keepSnapshot {
			cleanupSnapshot()
		}
	}()
	network := ComposeProjectFor(spec.Repo, owner) + "_filtered"
	serviceNetwork := func() (runtime.DockerNetwork, error) {
		networks, err := docker.Networks(ctx)
		if err != nil {
			return runtime.DockerNetwork{}, err
		}
		for _, candidate := range networks {
			if candidate.Name == network {
				return candidate, nil
			}
		}
		return runtime.DockerNetwork{}, nil
	}
	// Once the network exists, the seed declares the subnet the final override pins. Declared
	// differently, Compose recreates the network and first disconnects the services from it, which
	// fails for one `coop up` moved to the plain network ("container ... is not connected to the
	// network ..._filtered"). Declared alike, Compose recreates only the containers, and a recreated
	// container keeps its anonymous volumes. A first launch leaves the subnet to Docker.
	existing, err := serviceNetwork()
	if err != nil {
		return "", nil, nil, nil, err
	}
	seedSubnet, _, _ := filteredServiceAddresses(existing, closure)
	labels := runServiceLabels(spec, owner)
	if labels != nil {
		labels[serviceDefinitionLabel] = serviceDefinition(data, closure)
	}
	seed, err := filteredServiceOverride(network, closure, seedSubnet, nil, labels)
	if err != nil {
		return "", nil, nil, nil, err
	}
	seedPath, cleanupSeed, err := writeFilteredServiceOverride(spec.Repo, seed, exposedRoots...)
	if err != nil {
		return "", nil, nil, nil, err
	}
	seedArgs := append(append([]string(nil), args...), "-f", seedPath, "up", "--no-start", "--force-recreate")
	seedArgs = append(seedArgs, selected...)
	var composeErr bytes.Buffer
	err = runCompose(rt, io.Discard, &composeErr, "up --no-start", seedArgs)
	cleanupSeed()
	if err != nil {
		return "", nil, nil, nil, composeFailure("prepare filtered services", err, composeErr.String())
	}
	seeded, err := serviceNetwork()
	if err != nil {
		return "", nil, nil, nil, err
	}
	subnet, addresses, err := filteredServiceAddresses(seeded, closure)
	if err != nil {
		return "", nil, nil, nil, err
	}
	final, err := filteredServiceOverride(network, closure, subnet, addresses, labels)
	if err != nil {
		return "", nil, nil, nil, err
	}
	finalPath, cleanupFinal, err := writeFilteredServiceOverride(spec.Repo, final, exposedRoots...)
	if err != nil {
		return "", nil, nil, nil, err
	}
	finalArgs := append(append([]string(nil), args...), "-f", finalPath)
	createArgs := append(append([]string(nil), finalArgs...), "up", "--no-start", "--force-recreate")
	createArgs = append(createArgs, selected...)
	if err := runCompose(rt, io.Discard, &composeErr, "up --no-start", createArgs); err != nil {
		cleanupFinal()
		return "", nil, nil, nil, composeFailure("prepare filtered services", err, composeErr.String())
	}
	bindings, clients, err := filteredServiceGrants(grants, closure, addresses, network)
	if err != nil {
		cleanupFinal()
		return "", nil, nil, nil, err
	}
	keepSnapshot = true
	prepared := &preparedFilteredServices{runtime: rt, repo: spec.Repo, file: composeFile, data: data, owner: owner,
		isolatedParent: spec.IsolatedParent,
		allowCases:     spec.allowCaseExposed,
		readOnly:       spec.RepoReadOnly, override: finalPath, roots: slices.Clone(exposedRoots), selected: selected, names: selected, sections: sections, addresses: addresses,
		cleanup: func() { cleanupFinal(); cleanupSnapshot() }}
	return network, bindings, clients, prepared, nil
}

// serviceDefinitionLabel marks a session's service container with what it was started from.
const serviceDefinitionLabel = "coop.service-definition"

// serviceDefinition names the approved file's exact bytes and the services started from it.
func serviceDefinition(data []byte, closure []string) string {
	sum := sha256.Sum256(append(append(slices.Clone(data), 0), strings.Join(closure, "\x00")...))
	return hex.EncodeToString(sum[:])
}

func grantedServices(grants []egress.Grant) []string {
	selected := make([]string, 0, len(grants))
	for _, grant := range grants {
		if name := grant.Rule.To.Service; !slices.Contains(selected, name) {
			selected = append(selected, name)
		}
	}
	slices.Sort(selected)
	return selected
}

func filteredServiceGrants(grants []egress.Grant, closure []string, addresses map[string]netip.Addr, network string) ([]networkgateway.ServiceBinding, []networkgateway.ServiceProxyClient, error) {
	var bindings []networkgateway.ServiceBinding
	for _, grant := range grants {
		name := grant.Rule.To.Service
		address, ok := addresses[name]
		if !ok {
			return nil, nil, fmt.Errorf("the approved service %q has no prepared address on %s", name, network)
		}
		bindings = append(bindings, networkgateway.ServiceBinding{Name: name, RuleID: grant.ID, Address: address})
	}
	clients := make([]networkgateway.ServiceProxyClient, 0, len(closure))
	for _, name := range closure {
		clients = append(clients, networkgateway.ServiceProxyClient{Name: name, Address: addresses[name]})
	}
	return bindings, clients, nil
}

type joinedServices struct {
	network   string
	closure   []string
	addresses map[string]netip.Addr
}

// joinSessionServices lets a box of an editor session use the services an earlier box of that session
// started, if they still run what this box would start. Starting them again would need the exclusive
// launch lock, which every running box holds shared for its life, and would recreate them under those
// boxes. Joining opens no bind source, which is all the exclusive lock guards, so the shared side is
// enough to keep a launch out while this looks. Anything else returns nil, and the full launch path
// decides and reports.
func joinSessionServices(ctx context.Context, docker filteredDocker, rt runtime.Runtime, spec RunSpec, projectRepo, composeFile string, approval *networkstate.Approval, grants []egress.Grant, sections *launchSections) *joinedServices {
	if !sessionMayJoinServices(spec) {
		return nil
	}
	unlock, err := lockServiceLaunch(ctx, serviceStartRepo(spec, projectRepo), false, RunSpec{}, sections)
	if err != nil {
		return nil
	}
	defer unlock()
	return sessionServicesRunning(ctx, docker, rt, spec, composeFile, approval, grants)
}

func sessionMayJoinServices(spec RunSpec) bool {
	return spec.SupervisorID != "" && runServiceOwner(spec) != "" && spec.IsolatedParent == ""
}

// sessionServicesRunning is the check joinSessionServices makes, for a caller that holds the shared side of
// the lock. A session asks it before it starts a box too, so a box that would join does not wait.
func sessionServicesRunning(ctx context.Context, docker filteredDocker, rt runtime.Runtime, spec RunSpec, composeFile string, approval *networkstate.Approval, grants []egress.Grant) *joinedServices {
	if !sessionMayJoinServices(spec) {
		return nil
	}
	owner := runServiceOwner(spec)
	data, err := readValidatedCompose(composeFile, spec.Repo, spec.RepoReadOnly)
	if err != nil || checkApprovedServicesData(approval, data) != nil || refuseRunningServiceWriters(ctx, rt, spec.Repo) != nil {
		return nil
	}
	var doc composeDoc
	if yaml.Unmarshal(data, &doc) != nil {
		return nil
	}
	closure, err := composeServiceClosure(doc.Services, grantedServices(grants))
	if err != nil {
		return nil
	}
	composeProject := ComposeProjectFor(spec.Repo, owner)
	definition := serviceDefinition(data, closure)
	for _, name := range closure {
		if _, err := docker.LabeledComposeServiceID(ctx, composeProject, name, serviceDefinitionLabel, definition); err != nil {
			return nil
		}
	}
	networks, err := docker.Networks(ctx)
	if err != nil {
		return nil
	}
	name := composeProject + "_filtered"
	for _, candidate := range networks {
		if candidate.Name != name {
			continue
		}
		// The launch's own check proves each service sits at the address worked out here.
		if _, addresses, err := filteredServiceAddresses(candidate, closure); err == nil {
			return &joinedServices{network: name, closure: closure, addresses: addresses}
		}
	}
	return nil
}

// composeFailure is a failed Compose step with Compose's own last lines: the exit status alone hides
// why (a network it could not leave, a port already taken) from the person and from whoever fixes it.
func composeFailure(what string, err error, stderr string) error {
	if detail := strings.TrimSpace(lastLines(stderr, 2)); detail != "" {
		return fmt.Errorf("%s: %w: %s", what, err, detail)
	}
	return fmt.Errorf("%s: %w", what, err)
}

func filteredServiceAddresses(network runtime.DockerNetwork, services []string) (netip.Prefix, map[string]netip.Addr, error) {
	// The lookup leaves the name empty when no network has it: say that, since "not internal" sent a
	// reader looking for a wrong setting on a network that was gone.
	if network.Name == "" {
		return netip.Prefix{}, nil, errors.New("filtered service network is missing")
	}
	if !network.Internal {
		return netip.Prefix{}, nil, errors.New("filtered service network is not internal")
	}
	for _, subnet := range network.Subnets {
		if !subnet.Addr().Is4() {
			continue
		}
		candidate := subnet.Masked().Addr()
		for range 15 {
			candidate = candidate.Next()
		}
		addresses := make(map[string]netip.Addr, len(services))
		for _, service := range services {
			candidate = candidate.Next()
			for slices.Contains(network.Gateways, candidate) {
				candidate = candidate.Next()
			}
			if !subnet.Contains(candidate) || !subnet.Contains(candidate.Next()) {
				addresses = nil
				break
			}
			addresses[service] = candidate
		}
		if len(addresses) == len(services) {
			return subnet, addresses, nil
		}
	}
	return netip.Prefix{}, nil, errors.New("filtered service network has no usable IPv4 address range")
}

func filteredServiceOverride(network string, services []string, subnet netip.Prefix, addresses map[string]netip.Addr, labels map[string]string) ([]byte, error) {
	if network == "" || len(services) == 0 || addresses != nil && len(addresses) != len(services) {
		return nil, errors.New("invalid filtered service network configuration")
	}
	proxy := "http://" + filteredServiceProxyAlias + ":" + strconv.Itoa(networkgateway.ServiceProxyPort)
	noProxy := strings.Join(append([]string{"localhost", "127.0.0.1"}, services...), ",")
	var out strings.Builder
	out.WriteString("services:\n")
	for _, service := range services {
		fmt.Fprintf(&out, "  %s:\n    networks: !override\n      %s:", strconv.Quote(service), filteredServiceNetworkKey)
		if address, ok := addresses[service]; ok {
			if !address.Is4() || !subnet.Contains(address) {
				return nil, errors.New("invalid filtered service address")
			}
			fmt.Fprintf(&out, "\n        ipv4_address: %s", address)
		}
		fmt.Fprintf(&out, "\n    environment:\n      HTTP_PROXY: %s\n      HTTPS_PROXY: %s\n      NO_PROXY: %s\n      http_proxy: %s\n      https_proxy: %s\n      no_proxy: %s\n",
			strconv.Quote(proxy), strconv.Quote(proxy), strconv.Quote(noProxy), strconv.Quote(proxy), strconv.Quote(proxy), strconv.Quote(noProxy))
		if len(labels) > 0 {
			out.WriteString(serviceLabelsYAML(labels))
		}
	}
	fmt.Fprintf(&out, "networks:\n  %s:\n    name: %s\n    internal: true\n", filteredServiceNetworkKey, strconv.Quote(network))
	if subnet.IsValid() {
		fmt.Fprintf(&out, "    ipam:\n      config:\n        - subnet: %s\n", subnet)
	}
	return []byte(out.String()), nil
}

func writeFilteredServiceOverride(workspace string, data []byte, exposedRoots ...string) (string, func(), error) {
	dir, err := privateWorkspaceTempDir(workspace, "coop-filtered-services-", exposedRoots...)
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	path := filepath.Join(dir, "compose.yml")
	if err := os.WriteFile(path, data, 0o400); err != nil {
		cleanup()
		return "", nil, err
	}
	return path, cleanup, nil
}

// filteredPublish is the `-p` set for the CONTROLLER container: it owns the
// network namespace the agent runs in, so a published port has to be published
// there. Same host-port allocation and same best-effort skip as an open run —
// a port already taken does not stop the box. The plan is that one decision per
// port, so the agent's note reports what the controller was actually given.
func filteredPublish(cfg *config.Config, spec RunSpec, free func(int) bool) (options []string, plan []servePublication, env []string) {
	if !spec.Serve || len(spec.servePorts) == 0 {
		return nil, nil, nil
	}
	// The URLs are collected and printed as ONE section after the loop, in the same shape an open
	// run prints them — a filtered box is the same box to the person opening the link.
	var urls []string
	for _, port := range spec.servePorts {
		host := project.HostPort(spec.Repo, port)
		env = append(env, "-e", fmt.Sprintf("COOP_SERVE_URL_%d=http://localhost:%d", port, host))
		s := spec.servePublicationFor(port, free)
		plan = append(plan, s)
		if !s.Published {
			// Both ports are named: one is the URL to open, the other is what the dev server
			// inside the box listens on, and they can differ.
			ui.Warning(fmt.Sprintf("Could not publish box port %d", port),
				fmt.Sprintf("Host port %d is already in use.", s.bound()),
				"Free that port, then start the box again.")
			continue
		}
		options = append(options, "-p", fmt.Sprintf("127.0.0.1:%d:%d", s.bound(), port))
		urls = append(urls, fmt.Sprintf("  http://localhost:%d → box port %d", host, port))
	}
	if len(urls) > 0 {
		ui.Section("Available on this host")
		for _, url := range urls {
			ui.Note("%s", url)
		}
	}
	return options, plan, env
}

// checkFilteredServePorts refuses a serve port the gateway itself captures,
// before anything is created: 443 and 53 always belong to the TLS and DNS
// boundary, and so does every port this policy grants TLS on.
func checkFilteredServePorts(ports, captured []int) error {
	for _, port := range ports {
		if port == 443 || port == 53 || slices.Contains(captured, port) {
			return errors.New("port " + strconv.Itoa(port) + " is one the gateway uses for TLS and DNS — serve on another port in filtered mode")
		}
	}
	return nil
}
