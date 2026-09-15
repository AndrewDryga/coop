package box

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkgateway"
)

type credentialBrokerCandidate struct {
	provider     string
	spec         agents.CredentialBrokerSpec
	credential   string
	shadowMarker string
}

type credentialBrokerRun struct {
	candidate  *credentialBrokerCandidate
	configPath string
	substitute string
	configInfo os.FileInfo
}

// selectCredentialBroker recognizes reusable credentials before a box is assembled. A supported
// direct filtered run gets a broker; every other shape refuses instead of falling back to putting
// the credential in the container.
func selectCredentialBroker(cfg *config.Config, spec RunSpec) (*credentialBrokerCandidate, error) {
	return selectCredentialBrokerWithMarkers(cfg, spec, nil)
}

func selectCredentialBrokerWithMarkers(cfg *config.Config, spec RunSpec, markers map[string]bool) (*credentialBrokerCandidate, error) {
	if cfg == nil {
		return nil, nil
	}
	if key := extraProviderCredential(spec, cfg.ExtraRunArgs, spec.ExtraArgs); key != "" {
		if spec.Login {
			return nil, fmt.Errorf("sign-in does not accept %s through -e; Coop keeps existing reusable credentials outside the login box", key)
		}
		return nil, fmt.Errorf("%s cannot enter an agent box through -e; put the selected provider's supported key in Coop's agent env file for a filtered brokered run", key)
	}
	if spec.Agent == "" {
		return nil, nil
	}
	if !spec.Homes {
		if agent, ok := agents.Get(spec.Agent); ok {
			for _, key := range agent.CredentialEnvKeys() {
				if strings.TrimSpace(spec.projectEnv[key]) != "" {
					return nil, fmt.Errorf("%s cannot enter an agent box with credential homes disabled; remove %s or enable homes for a filtered brokered run", agent.DisplayName(), key)
				}
			}
		}
		return nil, nil
	}
	if spec.Login {
		agent, ok := agents.Get(spec.Agent)
		if !ok {
			return nil, nil
		}
		profileDir := cfg.AgentProfileDir(spec.Agent, cfg.ActiveProfile(spec.Agent))
		if detector, ok := agent.(agents.StoredAPIKeyDetector); ok && profileMarkerPresent(agent, profileDir) {
			stored, err := detector.StoredAPIKey(profileDir)
			if err != nil {
				return nil, fmt.Errorf("inspect %s stored credential: %w", agent.DisplayName(), err)
			}
			if stored {
				return nil, fmt.Errorf("%s sign-in cannot mount its existing reusable API key; remove that account with 'coop credentials %s %s rm' before signing in again", agent.DisplayName(), spec.Agent, cfg.ActiveProfile(spec.Agent))
			}
		}
		return nil, nil
	}
	var selected *credentialBrokerCandidate
	for _, name := range credentialScope(cfg, spec) {
		candidate, err := credentialBrokerCandidateFor(cfg, spec, name, cfg.ActiveProfile(name), markers)
		if err != nil {
			return nil, err
		}
		if candidate == nil {
			continue
		}
		if selected != nil || name != spec.Agent {
			return nil, fmt.Errorf("%s API-key brokering is not yet supported as a peer; run it as the only direct agent", credentialBrokerAgentName(name))
		}
		selected = candidate
	}
	if selected == nil {
		return nil, nil
	}
	if conflict := credentialBrokerConflict(spec); conflict != "" {
		return nil, fmt.Errorf("%s API-key brokering does not support %s; run %s directly without peers or a preset", credentialBrokerAgentName(selected.provider), conflict, selected.provider)
	}
	if cfg.Egress != string(egress.Filtered) {
		return nil, fmt.Errorf("%s %s requires filtered networking so Coop can keep it outside the box; use --egress filtered or sign in with the provider instead", credentialBrokerAgentName(selected.provider), selected.spec.CredentialEnv)
	}
	return selected, nil
}

func credentialBrokerAgentName(name string) string {
	if agent, ok := agents.Get(name); ok {
		return agent.DisplayName()
	}
	return name
}

func credentialBrokerCandidateFor(cfg *config.Config, spec RunSpec, name, profile string, markers map[string]bool) (*credentialBrokerCandidate, error) {
	agent, ok := agents.Get(name)
	if !ok {
		return nil, nil
	}
	broker := agent.CredentialBroker()
	profileDir := cfg.AgentProfileDir(name, profile)
	markerPresent := profileMarkerPresent(agent, cfg.AgentProfileDir(name, profile))
	if markers != nil {
		markerPresent = markers[name]
	}
	active := agent.ActiveCredentialEnvKeys(profileDir, markerPresent)
	values, err := effectiveCredentialEnv(cfg, spec, agent, name, profile, markerPresent)
	if err != nil {
		return nil, err
	}
	var credentialKey, credential string
	for _, key := range active {
		if value := strings.TrimSpace(values[key]); value != "" {
			if credentialKey != "" {
				return nil, fmt.Errorf("%s has more than one active environment credential; keep only one", agent.DisplayName())
			}
			credentialKey, credential = key, value
		}
	}
	if credentialKey == "" && markerPresent {
		if detector, ok := agent.(agents.StoredAPIKeyDetector); ok {
			stored, err := detector.StoredAPIKey(profileDir)
			if err != nil {
				return nil, fmt.Errorf("inspect %s stored credential: %w", agent.DisplayName(), err)
			}
			if stored {
				if broker.Valid() {
					return nil, fmt.Errorf("%s has a reusable API key in its native credential file; remove it and use %s with --egress filtered instead", agent.DisplayName(), broker.CredentialEnv)
				}
				return nil, fmt.Errorf("%s has a reusable API key in its native credential file; remove it and sign in with the provider instead", agent.DisplayName())
			}
		}
	}
	if credentialKey == "" {
		return nil, nil
	}
	if !broker.Valid() {
		return nil, fmt.Errorf("%s %s cannot be brokered yet; sign in with the provider instead", agent.DisplayName(), credentialKey)
	}
	if credentialKey != broker.CredentialEnv {
		return nil, fmt.Errorf("%s %s cannot be brokered yet; use %s with --egress filtered or sign in with the provider instead", agent.DisplayName(), credentialKey, broker.CredentialEnv)
	}
	if base := strings.TrimSpace(values[broker.BaseURLEnv]); base != "" && base != "https://"+broker.Upstream && base != "https://"+broker.Upstream+"/" {
		defaultBase := "https://" + broker.Upstream + broker.ClientBasePath
		if base != defaultBase && base != defaultBase+"/" {
			return nil, fmt.Errorf("%s uses a custom %s; the filtered credential broker is qualified only for %s", agent.DisplayName(), broker.BaseURLEnv, defaultBase)
		}
	}
	for _, key := range append(append([]string{}, agent.CredentialEnvKeys()...), broker.BaseURLEnv) {
		if extraEnvAssigns(cfg.ExtraRunArgs, key) || extraEnvAssigns(spec.ExtraArgs, key) {
			return nil, fmt.Errorf("a filtered credential broker owns %s; remove its override from COOP_RUN_ARGS or this run", key)
		}
	}
	shadowMarker := ""
	if markerPresent {
		shadowMarker, _ = agent.AuthMarker()
	}
	return &credentialBrokerCandidate{provider: name, spec: broker, credential: credential, shadowMarker: shadowMarker}, nil
}

// effectiveCredentialEnv mirrors prepareBoxEnvFile's order for one account: project defaults,
// then its Coop-owned host credential, then the default account's explicit user env.
func effectiveCredentialEnv(cfg *config.Config, spec RunSpec, agent agents.Agent, name, profile string, markerPresent bool) (map[string]string, error) {
	values := make(map[string]string, len(spec.projectEnv)+1)
	for key, value := range spec.projectEnv {
		values[key] = value
	}
	profileDir := cfg.AgentProfileDir(name, profile)
	if hostCredentialSelected(agent, profileDir, markerPresent) {
		key, value, found, err := LoadHostCredential(cfg, agent, profile)
		if err != nil {
			return nil, fmt.Errorf("load %s account %q credential: %w", agent.DisplayName(), profile, err)
		}
		if found {
			values[key] = value
		}
	}
	if profile == cfg.DefaultProfileOf(name) {
		for key, value := range EnvFileValues(cfg.EnvFile()) {
			values[key] = value
		}
	}
	return values, nil
}

func credentialBrokerConflict(spec RunSpec) string {
	switch {
	case spec.Login:
		return "sign-in"
	case spec.Preset != nil:
		return "a preset run"
	case spec.ConsultLead != "" || len(spec.Peers) != 0:
		return "a peer or consult run"
	case spec.ShareACPSessions || spec.ForceNoTTY || spec.networkClient() == egress.ClientACP:
		return "ACP or a remote session"
	case spec.Mode.Restricted():
		return "restricted-filesystem mode"
	case !spec.AgentCommand:
		return "this non-agent command"
	case spec.networkClient() != egress.ClientCLI:
		return "this client variant"
	default:
		return ""
	}
}

func extraEnvAssigns(args []string, key string) bool {
	for index := 0; index < len(args); index++ {
		name, value, inline := strings.Cut(args[index], "=")
		if name != "-e" && name != "--env" {
			continue
		}
		if !inline {
			index++
			if index >= len(args) {
				return false // filteredExtraArgs reports the malformed option itself
			}
			value = args[index]
		}
		assigned, _, _ := strings.Cut(value, "=")
		if assigned == key {
			return true
		}
	}
	return false
}

func extraProviderCredential(spec RunSpec, argSets ...[]string) string {
	for _, name := range agents.Names() {
		agent, _ := agents.Get(name)
		for _, key := range agent.CredentialEnvKeys() {
			for _, args := range argSets {
				if extraEnvAssigns(args, key) {
					return key
				}
			}
		}
	}
	return ""
}

func (c *credentialBrokerCandidate) route() *networkgateway.CredentialBrokerRoute {
	if c == nil {
		return nil
	}
	return &networkgateway.CredentialBrokerRoute{Provider: c.provider, Upstream: c.spec.Upstream,
		Header: c.spec.Header, HeaderPrefix: c.spec.HeaderPrefix, Method: c.spec.Method,
		Path: c.spec.Path, PathPrefix: c.spec.PathPrefix, AllowQuery: c.spec.AllowQuery, Port: c.spec.Port}
}

func (c *credentialBrokerCandidate) command(cmd []string) []string {
	if c == nil || c.spec.CommandArgs == nil || len(cmd) == 0 {
		return cmd
	}
	baseURL := "http://" + networkgateway.CredentialBrokerAddress + c.spec.ClientBasePath
	args := c.spec.CommandArgs(baseURL)
	out := make([]string, 0, len(cmd)+len(args))
	out = append(out, cmd[0])
	out = append(out, args...)
	return append(out, cmd[1:]...)
}

func (f *filteredExecution) prepareCredentialBroker(artifacts compositionArtifactOps) error {
	if f.broker == nil || f.broker.candidate == nil {
		return nil
	}
	root, err := f.store.RunFilesPath(f.record.ID)
	if err != nil || root != artifacts.parent {
		return errors.New("credential broker artifact ownership changed")
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return errors.New("create credential broker substitute")
	}
	f.broker.substitute = hex.EncodeToString(random)
	secret := networkgateway.CredentialBrokerSecret{Version: 1, RunID: f.record.ID, Epoch: f.record.Epoch,
		Provider: f.broker.candidate.provider, Substitute: f.broker.substitute,
		Credential: f.broker.candidate.credential}
	data, err := json.Marshal(secret)
	secret.Credential = ""
	f.broker.candidate.credential = ""
	if err != nil {
		return errors.New("encode credential broker configuration")
	}
	path, err := artifacts.writeFile(artifacts.parent, string(data)+"\n")
	for i := range data {
		data[i] = 0
	}
	if err != nil {
		return fmt.Errorf("prepare credential broker: %w", err)
	}
	if err := artifacts.chmod(path, 0o444); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("protect credential broker configuration: %w", err)
	}
	canonical, resolveErr := filepath.EvalSymlinks(path)
	relative, relativeErr := filepath.Rel(root, path)
	if resolveErr != nil || relativeErr != nil || canonical != path || relative == "." || !filepath.IsLocal(relative) {
		_ = os.Remove(path)
		return errors.New("credential broker configuration must be an owned regular file")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		_ = os.Remove(path)
		return errors.New("credential broker configuration must be an owned regular file")
	}
	f.broker.configPath = path
	f.broker.configInfo = info
	return nil
}

func (f *filteredExecution) checkCredentialBrokerBinding() error {
	if f.broker == nil {
		return nil
	}
	info, err := os.Lstat(f.broker.configPath)
	if err != nil || f.broker.configInfo == nil || !os.SameFile(info, f.broker.configInfo) || !info.Mode().IsRegular() || info.Mode().Perm() != 0o444 {
		return errors.New("credential broker configuration changed before launch")
	}
	return nil
}

func (f *filteredExecution) credentialBrokerEnv(artifacts compositionArtifactOps, source string) (string, error) {
	if f.broker == nil || f.broker.candidate == nil {
		return source, nil
	}
	content := ""
	if source != "" {
		data, err := os.ReadFile(source)
		if err != nil {
			return "", fmt.Errorf("read environment for credential broker: %w", err)
		}
		drop := map[string]bool{f.broker.candidate.spec.BaseURLEnv: true}
		if agent, ok := agents.Get(f.broker.candidate.provider); ok {
			for _, key := range agent.CredentialEnvKeys() {
				drop[key] = true
			}
		}
		content = strings.TrimRight(filteredEnvContent(data, drop), "\n")
	}
	if content != "" {
		content += "\n"
	}
	content += f.broker.candidate.spec.CredentialEnv + "=" + f.broker.substitute + "\n"
	content += f.broker.candidate.spec.BaseURLEnv + "=http://" + networkgateway.CredentialBrokerAddress + f.broker.candidate.spec.ClientBasePath + "\n"
	path, err := artifacts.writeFile(artifacts.parent, content)
	if err != nil {
		return "", fmt.Errorf("prepare credential broker environment: %w", err)
	}
	return path, nil
}

func (f *filteredExecution) credentialBrokerMarkerMount(artifacts compositionArtifactOps, homeInBox string) (extraMount, string, error) {
	if f.broker == nil || f.broker.candidate == nil || f.broker.candidate.shadowMarker == "" {
		return extraMount{}, "", nil
	}
	path, err := artifacts.writeFile(artifacts.parent, "{}\n")
	if err != nil {
		return extraMount{}, "", fmt.Errorf("prepare credential marker shadow: %w", err)
	}
	target := filepath.Join(homeInBox, "."+f.broker.candidate.provider, f.broker.candidate.shadowMarker)
	return extraMount{path, target}, path, nil
}
