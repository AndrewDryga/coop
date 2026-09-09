package box

import (
	"encoding/json"
	"errors"
	"net/url"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/mcp"
	"github.com/AndrewDryga/coop/internal/networkstate"
)

// NetworkProviderBundles derives the release-owned core endpoints for the
// providers this run actually mounts credentials for — the lead plus the exact
// peers and preset roles it may invoke, never every installed provider. A raw
// run mounts no credentials and therefore gets no provider endpoints.
//
// An unsupported provider fails admission here. Launching it under a policy
// that cannot reach its API would only produce a confusing mid-session denial.
func NetworkProviderBundles(cfg *config.Config, spec RunSpec) ([]egress.Bundle, error) {
	var bundles []egress.Bundle
	for _, name := range credentialScope(cfg, spec) {
		ag, ok := agents.Get(name)
		if !ok {
			return nil, errors.New("unknown provider in the restricted credential scope")
		}
		// Only the ordinary CLI is qualified in this slice; an ACP launch is a
		// separate client variant that its own qualification case must cover.
		bundle, err := ag.NetworkBundle(agents.NetworkBundleInput{Client: egress.ClientCLI})
		if err != nil {
			return nil, err
		}
		bundles = append(bundles, bundle)
	}
	return egress.SelectedBundles(bundles)
}

// NetworkMCPDependencies returns the automatic HTTP destinations of the trusted
// shared MCP configuration plus the owner-keyed projection of its routing shape.
// The hosts are Admission.Automatic, never operator requests: the operator
// chose the servers, not their hostnames. The projection identifies the exact
// configuration a qualification covered, so an edited MCP file cannot silently
// reuse a qualification taken against a different transport.
//
// Both admission and launch call this, so a configuration change between them
// surfaces as a qualification mismatch instead of an unqualified launch.
func NetworkMCPDependencies(cfg *config.Config, spec RunSpec, store *networkstate.Store) ([]egress.Input, string, error) {
	if store == nil {
		return nil, "", errors.New("MCP dependency derivation requires host network state")
	}
	snapshot, err := networkMCPSnapshot(cfg, spec)
	if err != nil {
		return nil, "", err
	}
	servers, err := mcp.NetworkServers(snapshot)
	if err != nil {
		return nil, "", err
	}
	if len(servers) == 0 {
		return nil, "none", nil
	}
	var dependencies []egress.Input
	for _, server := range servers {
		if server.URL == "" {
			continue // a stdio command is not a destination grant
		}
		parsed, _ := url.Parse(server.URL) // NetworkServers validated the literal URL
		rules, err := egress.NormalizeRules([]egress.Rule{{To: egress.Destination{Domain: parsed.Hostname()}, Protocol: "tls", Ports: []int{443}}})
		if err != nil {
			return nil, "", err
		}
		dependencies = append(dependencies, egress.Input{Origin: egress.Origin{Kind: "mcp", Name: server.Name}, Rules: rules})
	}
	shape, err := json.Marshal(servers)
	if err != nil {
		return nil, "", err
	}
	projection, err := store.MCPProjection(shape)
	return dependencies, projection, err
}

// networkMCPSnapshot reads the same validated shared configuration box.Run
// mounts. A run without agent homes has no MCP at all, so it derives nothing.
func networkMCPSnapshot(cfg *config.Config, spec RunSpec) ([]byte, error) {
	if !spec.Homes || cfg.MCPFile == "" {
		return nil, nil
	}
	source, err := validateMCPSourceIsolation(cfg, spec)
	if err != nil {
		return nil, err
	}
	snapshot, _, err := mcp.ReadValidatedSnapshot(source)
	return snapshot, err
}
