package box

import (
	"bytes"
	"context"
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
	"gopkg.in/yaml.v3"
)

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
	runtime   runtime.Runtime
	args      []string
	selected  []string
	names     []string
	sections  *launchSections
	addresses map[string]netip.Addr
	cleanup   func()
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

func (s *preparedFilteredServices) start() error {
	if s == nil {
		return nil
	}
	var stderr bytes.Buffer
	args := append(append([]string(nil), s.args...), "up", "-d", "--wait")
	args = append(args, s.selected...)
	if err := runCompose(s.runtime, io.Discard, &stderr, "up", args); err != nil {
		return fmt.Errorf("a filtered box needs this project's approved sidecars running, and starting them failed: %w", err)
	}
	if s.sections != nil && s.sections.loop {
		s.sections.services(s.names)
	}
	return nil
}

func resolveServiceBindings(ctx context.Context, docker filteredDocker, rt runtime.Runtime, spec RunSpec, composeFile string,
	approval *networkstate.Approval, grants []egress.Grant, sections *launchSections, exposedRoots []string) (string, []networkgateway.ServiceBinding, []networkgateway.ServiceProxyClient, *preparedFilteredServices, error) {
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
	// The approval named a DEFINITION, not just a name: this is the file that is
	// about to run, so it is the one the digest has to match. Check it before
	// anything is started.
	if err := checkApprovedServices(approval, composeFile, spec.Repo, spec.RepoReadOnly); err != nil {
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
	live, err := LiveBoxes(projectRepo, spec.activityID)
	if err != nil {
		return "", nil, nil, nil, err
	}
	if len(live) != 0 {
		return "", nil, nil, nil, fmt.Errorf("another box is already using this project's filtered services (%s) — stop it before starting another filtered service box", DescribeLiveBoxes(live))
	}
	selected := make([]string, 0, len(grants))
	for _, grant := range grants {
		if name := grant.Rule.To.Service; !slices.Contains(selected, name) {
			selected = append(selected, name)
		}
	}
	slices.Sort(selected)
	data, err := readValidatedCompose(composeFile, spec.Repo, spec.RepoReadOnly)
	if err != nil {
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
	args, cleanupSnapshot, hidden, err := snapshotComposeArgsForStart(spec.Repo, composeFile, spec.RepoReadOnly, true, exposedRoots...)
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
		ui.Warn("services get an empty file in place of %s (looks like a secret) — to let them read the real file, run `coop up` in a terminal and approve %s; the approval lasts until that file changes",
			strings.Join(hidden, ", "), filepath.Base(composeFile))
	}
	keepSnapshot := false
	defer func() {
		if !keepSnapshot {
			cleanupSnapshot()
		}
	}()
	network := ComposeProject(spec.Repo) + "_filtered"
	seed, err := filteredServiceOverride(network, closure, netip.Prefix{}, nil)
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
		return "", nil, nil, nil, fmt.Errorf("prepare filtered services: %w", err)
	}
	networks, err := docker.Networks(ctx)
	if err != nil {
		return "", nil, nil, nil, err
	}
	var serviceNetwork runtime.DockerNetwork
	for _, candidate := range networks {
		if candidate.Name == network {
			serviceNetwork = candidate
			break
		}
	}
	subnet, addresses, err := filteredServiceAddresses(serviceNetwork, closure)
	if err != nil {
		return "", nil, nil, nil, err
	}
	final, err := filteredServiceOverride(network, closure, subnet, addresses)
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
		return "", nil, nil, nil, fmt.Errorf("prepare filtered services: %w", err)
	}
	var bindings []networkgateway.ServiceBinding
	for _, grant := range grants {
		name := grant.Rule.To.Service
		address, ok := addresses[name]
		if !ok {
			cleanupFinal()
			return "", nil, nil, nil, fmt.Errorf("the approved service %q has no prepared address on %s", name, network)
		}
		bindings = append(bindings, networkgateway.ServiceBinding{Name: name, RuleID: grant.ID, Address: address})
	}
	clients := make([]networkgateway.ServiceProxyClient, 0, len(closure))
	for _, name := range closure {
		clients = append(clients, networkgateway.ServiceProxyClient{Name: name, Address: addresses[name]})
	}
	keepSnapshot = true
	prepared := &preparedFilteredServices{runtime: rt, args: finalArgs, selected: selected, names: selected, sections: sections, addresses: addresses,
		cleanup: func() { cleanupFinal(); cleanupSnapshot() }}
	return network, bindings, clients, prepared, nil
}

func filteredServiceAddresses(network runtime.DockerNetwork, services []string) (netip.Prefix, map[string]netip.Addr, error) {
	if network.Name == "" || !network.Internal {
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

func filteredServiceOverride(network string, services []string, subnet netip.Prefix, addresses map[string]netip.Addr) ([]byte, error) {
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
// a port already taken does not stop the box.
func filteredPublish(cfg *config.Config, spec RunSpec, free func(int) bool) (options []string, published []int, env []string) {
	if !spec.Serve || len(spec.servePorts) == 0 {
		return nil, nil, nil
	}
	// The URLs are collected and printed as ONE section after the loop, in the same shape an open
	// run prints them — a filtered box is the same box to the person opening the link.
	var urls []string
	for _, port := range spec.servePorts {
		host := project.HostPort(spec.Repo, port)
		env = append(env, "-e", fmt.Sprintf("COOP_SERVE_URL_%d=http://localhost:%d", port, host))
		if !free(host) {
			// Both ports are named: one is the URL to open, the other is what the dev server
			// inside the box listens on, and they can differ.
			ui.Warning(fmt.Sprintf("Could not publish box port %d", port),
				fmt.Sprintf("Host port %d is already in use.", host),
				"Free that port, then start the box again.")
			continue
		}
		options = append(options, "-p", fmt.Sprintf("127.0.0.1:%d:%d", host, port))
		published = append(published, port)
		urls = append(urls, fmt.Sprintf("  http://localhost:%d → box port %d", host, port))
	}
	if len(urls) > 0 {
		ui.Section("Available on this host")
		for _, url := range urls {
			ui.Note("%s", url)
		}
	}
	return options, published, env
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
