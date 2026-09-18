package box

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

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
	seenBundles := map[string]bool{}
	for _, name := range networkCredentialScope(cfg, spec) {
		if brokered[name] {
			continue // broker helper authority is distinct from the agent's captured policy
		}
		ag, ok := agents.Get(name)
		if !ok {
			return nil, errors.New("unknown provider in the restricted credential scope")
		}
		targets := networkTargetsForProvider(cfg, spec, name)
		for _, target := range targets {
			var bundle egress.Bundle
			var err error
			if spec.Login {
				// Login creates the credential, so it cannot require that credential first.
				bundle, err = ag.NetworkBundle(agents.NetworkBundleInput{Client: spec.networkClient()})
			} else {
				bundle, err = NetworkTargetBundle(cfg, target, spec.networkClient())
			}
			if err != nil {
				return nil, err
			}
			identity := strings.Join([]string{bundle.Provider, string(bundle.Client), bundle.Backend, bundle.AuthMode, bundle.Version}, "\x00")
			if seenBundles[identity] {
				continue
			}
			seenBundles[identity] = true
			bundles = append(bundles, bundle)
		}
	}
	return egress.SelectedBundles(bundles)
}

// networkCredentialScope is stricter than the mount planner: a missing preset
// role credential is a launch refusal, not permission to silently omit that
// role from network validation. Successful runs return the same providers as
// credentialScope; only broken required roles differ.
func networkCredentialScope(cfg *config.Config, spec RunSpec) []string {
	scope := credentialScope(cfg, spec)
	if !spec.Homes || runPrimary(spec) == "" || spec.Preset == nil || spec.Login {
		return scope
	}
	for _, provider := range spec.Preset.RunnableRoleAgents(runPrimary(spec)) {
		if !slices.Contains(scope, provider) {
			scope = append(scope, provider)
		}
	}
	return scope
}

func networkTargetsForProvider(cfg *config.Config, spec RunSpec, provider string) []agents.Target {
	var targets []agents.Target
	for _, target := range spec.Peers {
		if target.Provider != provider {
			continue
		}
		if len(target.Accounts) == 0 {
			target.Accounts = []string{cfg.ActiveProfile(provider)}
		}
		for _, account := range target.Accounts {
			t := target
			t.Accounts = []string{account}
			targets = append(targets, t)
		}
	}
	if len(targets) == 0 {
		targets = append(targets, agents.Target{Provider: provider, Accounts: []string{cfg.ActiveProfile(provider)}})
	}
	return targets
}

// NetworkTargetBundle binds one exact account's selected authentication family
// to its release-owned endpoint bundle without reading credential bytes. It is
// shared by initial admission and ACP's pre-spawn revalidation.
func NetworkTargetBundle(cfg *config.Config, target agents.Target, client egress.Client) (egress.Bundle, error) {
	ag, ok := agents.Get(target.Provider)
	if !ok {
		return egress.Bundle{}, errors.New("unknown provider in the restricted credential scope")
	}
	if len(target.Accounts) > 1 {
		return egress.Bundle{}, fmt.Errorf("restricted networking requires one concrete %s account", target.Provider)
	}
	account := target.Account()
	if account == "" {
		account = cfg.ActiveProfile(target.Provider)
	}
	if !ProfileCredentialReady(cfg, target.Provider, account, time.Now()) {
		return egress.Bundle{}, fmt.Errorf("%s account %q is not ready for restricted networking", target.Provider, account)
	}
	profileDir := cfg.AgentProfileDir(target.Provider, account)
	markerPresent := ProfileMarkerPresent(cfg, target.Provider, account)
	input := agents.NetworkBundleInput{Client: client}
	if selector, ok := ag.(agents.NetworkAuthSelector); ok {
		selection, err := selector.NetworkAuthSelection(profileDir, markerPresent)
		if err != nil {
			return egress.Bundle{}, err
		}
		if client == egress.ClientACP && selection.EnvKey != "" {
			return egress.Bundle{}, fmt.Errorf("%s account %q uses reusable %s authentication, which ACP sessions do not support", target.Provider, account, selection.EnvKey)
		}
		if selection.EnvKey != "" {
			available := ProfileHostCredentialPresent(cfg, target.Provider, account)
			if account == cfg.DefaultProfileOf(target.Provider) {
				available = available || envFileKeys(cfg.EnvFile())[selection.EnvKey]
			}
			if !available {
				return egress.Bundle{}, fmt.Errorf("%s account %q has no portable %s credential", target.Provider, account, selection.EnvKey)
			}
		}
		if selection.RequirePortable {
			live := ag.LiveCredentials()
			if live.Portability == nil || live.Portability(profileDir, time.Now().Add(RestrictedCredentialHorizon)) != agents.CredentialPortable {
				return egress.Bundle{}, fmt.Errorf("%s account %q has no portable credential for restricted networking", target.Provider, account)
			}
		}
		input.AuthMode = strings.TrimSpace(selection.AuthMode)
		if input.AuthMode == "" {
			return egress.Bundle{}, fmt.Errorf("%s account %q has no qualified authentication family", target.Provider, account)
		}
	}
	if client == egress.ClientACP {
		activeEnv := ag.ActiveCredentialEnvKeys(profileDir, markerPresent)
		if account == cfg.DefaultProfileOf(target.Provider) && anyCredentialEnvPresent(activeEnv, envFileKeys(cfg.EnvFile())) {
			return egress.Bundle{}, fmt.Errorf("%s account %q uses a reusable environment credential, which ACP sessions do not support", target.Provider, account)
		}
		if ProfileHostCredentialPresent(cfg, target.Provider, account) {
			return egress.Bundle{}, fmt.Errorf("%s account %q uses a reusable host credential, which ACP sessions do not support", target.Provider, account)
		}
		if detector, ok := ag.(agents.StoredAPIKeyDetector); ok && markerPresent {
			stored, err := detector.StoredAPIKey(profileDir)
			if err != nil {
				return egress.Bundle{}, fmt.Errorf("inspect %s account %q credential: %w", target.Provider, account, err)
			}
			if stored {
				return egress.Bundle{}, fmt.Errorf("%s account %q uses a reusable API key in its native credential file, which ACP sessions do not support", target.Provider, account)
			}
		}
	}
	return ag.NetworkBundle(input)
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
	return networkMCPDependenciesOf(snapshot)
}

// networkMCPDependenciesOf derives the destinations from an already validated snapshot. It carries
// the filtered gateway's MCP qualification rules — literal definitions only (an environment
// variable could re-route a destination after approval), a literal HTTPS origin on 443, no
// headersHelper or oauth — so it belongs on the filtered path alone. On an open box those same
// rules would refuse ordinary client features: a ${VARIABLE} header, an http:// dev server.
func networkMCPDependenciesOf(snapshot []byte) ([]egress.Input, error) {
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
