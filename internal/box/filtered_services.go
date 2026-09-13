package box

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkgateway"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/ui"
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

// resolveServiceBindings starts the project's sidecars and reads, once, the
// exact address each approved service holds on the project's Compose network.
//
// The network itself is never a grant: the gateway joins it so packets can
// reach ONE container, and every other member stays behind the same default
// deny as the public internet.
func resolveServiceBindings(ctx context.Context, docker filteredDocker, rt runtime.Runtime, spec RunSpec, composeFile string,
	approval *networkstate.Approval, grants []egress.Grant, sections *launchSections, exposedRoots []string) (string, []networkgateway.ServiceBinding, error) {
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
			return "", nil, ui.Reported(err)
		}
		return "", nil, err
	}
	// The approval named a DEFINITION, not just a name: this is the file that is
	// about to run, so it is the one the digest has to match. Check it before
	// anything is started.
	if err := checkApprovedServices(approval, composeFile, spec.Repo, spec.RepoReadOnly); err != nil {
		if sections != nil && sections.loop {
			sections.servicesRefused(err.Error())
			return "", nil, ui.Reported(err)
		}
		return "", nil, err
	}
	projectRepo := spec.ActivityRepo
	if projectRepo == "" {
		projectRepo = spec.Repo
	}
	live, err := LiveBoxes(projectRepo, spec.activityID)
	if err != nil {
		return "", nil, err
	}
	startServices := len(live) == 0
	if !startServices && sections != nil {
		sections.servicesHeldByLiveBox("Another box is running in this project (" + DescribeLiveBoxes(live) + ").")
	}
	var composeErr bytes.Buffer
	noticeHidden := sections == nil || !sections.loop
	var started startedServices
	if startServices {
		selected := make([]string, 0, len(grants))
		for _, grant := range grants {
			if name := grant.Rule.To.Service; !slices.Contains(selected, name) {
				selected = append(selected, name)
			}
		}
		slices.Sort(selected)
		started, err = startServicesFileContext(ctx, rt, spec.Repo, composeFile, "", io.Discard, &composeErr, spec.RepoReadOnly, noticeHidden, true, selected, exposedRoots...)
		if sections != nil {
			sections.serviceSecrets(started.hidden, composeFile)
		}
		if err != nil {
			var refused *ComposeRefused
			if sections != nil && sections.loop && errors.As(err, &refused) {
				sections.servicesRefused(err.Error())
				return "", nil, ui.Reported(fmt.Errorf("a filtered box needs this project's approved sidecars running: %w", err))
			}
			return "", nil, fmt.Errorf("a filtered box needs this project's approved sidecars running, and starting them failed: %w", err)
		}
	}
	if startServices && sections != nil && sections.loop {
		sections.services(started.names)
	}
	network := ComposeProject(spec.Repo) + "_default"
	members, err := docker.NetworkMembers(ctx, network)
	if err != nil {
		return "", nil, err
	}
	var bindings []networkgateway.ServiceBinding
	for _, grant := range grants {
		name := grant.Rule.To.Service
		id, err := docker.ComposeServiceID(ctx, ComposeProject(spec.Repo), name)
		if err != nil {
			return "", nil, fmt.Errorf("approved service %q: %w", name, err)
		}
		address, ok := members[id]
		if !ok || !address.Is4() || egress.Protected(address, nil) {
			return "", nil, fmt.Errorf("the approved service %q has no usable IPv4 address on %s", name, network)
		}
		bindings = append(bindings, networkgateway.ServiceBinding{Name: name, RuleID: grant.ID, Address: address})
	}
	return network, bindings, nil
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
