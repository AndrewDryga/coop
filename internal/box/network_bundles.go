package box

import (
	"errors"
	"fmt"
	"net/url"
	"slices"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/mcp"
)

// networkClient is the client kind a run launches. The zero value is the
// ordinary CLI, so an ACP or session path must name its variant deliberately.
func (s RunSpec) networkClient() egress.Client {
	if s.NetworkClient == "" {
		return egress.ClientCLI
	}
	return s.NetworkClient
}

// NetworkProviderBundles derives the release-owned core endpoints for the
// providers this run actually mounts credentials for — the lead plus the exact
// peers and preset roles it may invoke, never every installed provider. A raw
// run mounts no credentials and therefore gets no provider endpoints.
//
// An unsupported provider fails admission here. Launching it under a policy
// that cannot reach its API would only produce a confusing mid-session denial.
func NetworkProviderBundles(cfg *config.Config, spec RunSpec) ([]egress.Bundle, error) {
	brokered := map[string]bool{}
	if spec.NetworkAdmission {
		var err error
		brokered, err = networkAdmissionBrokerProviders(cfg, spec)
		if err != nil {
			return nil, err
		}
	} else {
		broker, err := selectCredentialBroker(cfg, spec)
		if err != nil {
			return nil, err
		}
		if broker != nil {
			brokered[broker.provider] = true
		}
	}
	var bundles []egress.Bundle
	for _, name := range credentialScope(cfg, spec) {
		if brokered[name] {
			continue // broker helper authority is distinct from the agent's captured policy
		}
		ag, ok := agents.Get(name)
		if !ok {
			return nil, errors.New("unknown provider in the restricted credential scope")
		}
		bundle, err := ag.NetworkBundle(agents.NetworkBundleInput{Client: spec.networkClient()})
		if err != nil {
			return nil, err
		}
		bundles = append(bundles, bundle)
	}
	return egress.SelectedBundles(bundles)
}

func networkAdmissionBrokerProviders(cfg *config.Config, spec RunSpec) (map[string]bool, error) {
	result := map[string]bool{}
	accounts := map[string][]string{spec.Agent: {cfg.ActiveProfile(spec.Agent)}}
	for _, target := range spec.Peers {
		account := target.Account()
		if account == "" {
			account = cfg.ActiveProfile(target.Provider)
		}
		if !slices.Contains(accounts[target.Provider], account) {
			accounts[target.Provider] = append(accounts[target.Provider], account)
		}
	}
	for _, name := range credentialScope(cfg, spec) {
		brokered, plain := false, false
		for _, account := range accounts[name] {
			candidate, err := credentialBrokerCandidateFor(cfg, spec, name, account, nil)
			if err != nil {
				return nil, err
			}
			if candidate != nil {
				brokered = true
			} else {
				plain = true
			}
		}
		if !brokered {
			continue
		}
		if !spec.CredentialBrokerLoop {
			return nil, fmt.Errorf("%s API-key brokering does not support a loop with peers or a preset; run a direct loop without them", credentialBrokerAgentName(name))
		}
		if plain {
			return nil, fmt.Errorf("%s cannot mix brokered API-key and stored-credential accounts in one filtered loop", credentialBrokerAgentName(name))
		}
		result[name] = true
	}
	return result, nil
}

// NetworkMCPDependencies returns the automatic HTTP destinations of the trusted
// shared MCP configuration. They are Admission.Automatic, never operator
// requests: the operator chose the servers, not their hostnames.
//
// Only what admission froze is enforced, so editing the MCP file after a launch
// was admitted can produce a visible denial — never a wider policy.
func NetworkMCPDependencies(cfg *config.Config, spec RunSpec) ([]egress.Input, error) {
	snapshot, err := networkMCPSnapshot(cfg, spec)
	if err != nil {
		return nil, err
	}
	servers, err := mcp.NetworkServers(snapshot)
	if err != nil {
		return nil, err
	}
	var dependencies []egress.Input
	for _, server := range servers {
		if server.URL == "" {
			continue // a stdio command is not a destination grant
		}
		parsed, _ := url.Parse(server.URL) // NetworkServers validated the literal URL
		rules, err := egress.NormalizeRules([]egress.Rule{{To: egress.Destination{Domain: parsed.Hostname()}, Protocol: "tls", Ports: []int{443}}})
		if err != nil {
			return nil, err
		}
		dependencies = append(dependencies, egress.Input{Origin: egress.Origin{Kind: "mcp", Name: server.Name}, Rules: rules})
	}
	return dependencies, nil
}

// networkMCPSnapshot reads the same validated shared configuration box.Run
// mounts. A run without agent homes has no MCP at all, so it derives nothing.
func networkMCPSnapshot(cfg *config.Config, spec RunSpec) ([]byte, error) {
	if !spec.Homes || spec.Login || cfg.MCPFile == "" {
		return nil, nil
	}
	source, err := validateMCPSourceIsolation(cfg, spec)
	if err != nil {
		return nil, err
	}
	snapshot, _, err := mcp.ReadValidatedSnapshot(source)
	return snapshot, err
}
