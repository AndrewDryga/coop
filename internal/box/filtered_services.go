package box

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkgateway"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/runtime"
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
	approval *networkstate.Approval, grants []egress.Grant, exposedRoots []string) (string, []networkgateway.ServiceBinding, error) {
	if composeFile == "" {
		names := make([]string, 0, len(grants))
		for _, grant := range grants {
			names = append(names, grant.Rule.To.Service)
		}
		slices.Sort(names)
		return "", nil, fmt.Errorf("the approved rules name the Compose service(s) %v, but this project has no %s",
			names, project.DefaultCompose)
	}
	// The approval named a DEFINITION, not just a name: this is the file that is
	// about to run, so it is the one the digest has to match. Check it before
	// anything is started.
	if err := checkApprovedServices(approval, composeFile, spec.Repo, spec.RepoReadOnly); err != nil {
		return "", nil, err
	}
	var composeErr bytes.Buffer
	if _, err := startServicesFile(rt, spec.Repo, composeFile, io.Discard, &composeErr, spec.RepoReadOnly, exposedRoots...); err != nil {
		return "", nil, fmt.Errorf("a filtered box needs this project's approved sidecars running, and starting them failed: %w", err)
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
	for _, port := range spec.servePorts {
		host := project.HostPort(spec.Repo, port)
		env = append(env, "-e", fmt.Sprintf("COOP_SERVE_URL_%d=http://localhost:%d", port, host))
		if !free(host) {
			fmt.Fprintf(os.Stderr, "Host port %d (for :%d) is in use — not publishing this box\n", host, port)
			continue
		}
		options = append(options, "-p", fmt.Sprintf("127.0.0.1:%d:%d", host, port))
		published = append(published, port)
		fmt.Fprintf(os.Stderr, "Serving box :%d at http://localhost:%d\n", port, host)
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
