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
	provider   string
	spec       agents.CredentialBrokerSpec
	credential string
}

type credentialBrokerRun struct {
	candidate  *credentialBrokerCandidate
	configPath string
	substitute string
	configInfo os.FileInfo
}

// selectCredentialBroker recognizes only the exact first qualified flow. Once an API key would
// otherwise enter a filtered Claude box, an unsupported launch shape is refused rather than
// silently falling back to raw credential exposure.
func selectCredentialBroker(cfg *config.Config, spec RunSpec) (*credentialBrokerCandidate, error) {
	return selectCredentialBrokerWithMarkers(cfg, spec, nil)
}

func selectCredentialBrokerWithMarkers(cfg *config.Config, spec RunSpec, markers map[string]bool) (*credentialBrokerCandidate, error) {
	if cfg == nil || cfg.Egress != string(egress.Filtered) || !spec.Homes || spec.Agent == "" {
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
	if !broker.Valid() || profile != cfg.DefaultProfileOf(name) {
		return nil, nil
	}
	markerPresent := profileMarkerPresent(agent, cfg.AgentProfileDir(name, profile))
	if markers != nil {
		markerPresent = markers[name]
	}
	if markerPresent {
		return nil, nil
	}
	values := effectiveRunEnv(cfg, spec.projectEnv)
	credential := values[broker.CredentialEnv]
	if strings.TrimSpace(credential) == "" {
		return nil, nil
	}
	for _, key := range agent.CredentialEnvKeys() {
		if key != broker.CredentialEnv && strings.TrimSpace(values[key]) != "" {
			return nil, fmt.Errorf("%s has more than one active environment credential; a filtered credential broker needs only %s", agent.DisplayName(), broker.CredentialEnv)
		}
	}
	if base := strings.TrimSpace(values[broker.BaseURLEnv]); base != "" && base != "https://"+broker.Upstream && base != "https://"+broker.Upstream+"/" {
		return nil, fmt.Errorf("%s uses a custom %s; the filtered credential broker is qualified only for https://%s", agent.DisplayName(), broker.BaseURLEnv, broker.Upstream)
	}
	for _, key := range append(append([]string{}, agent.CredentialEnvKeys()...), broker.BaseURLEnv) {
		if extraEnvAssigns(cfg.ExtraRunArgs, key) || extraEnvAssigns(spec.ExtraArgs, key) {
			return nil, fmt.Errorf("a filtered credential broker owns %s; remove its override from COOP_RUN_ARGS or this run", key)
		}
	}
	return &credentialBrokerCandidate{provider: name, spec: broker, credential: credential}, nil
}

func effectiveRunEnv(cfg *config.Config, projectEnv map[string]string) map[string]string {
	values := make(map[string]string, len(projectEnv))
	for key, value := range projectEnv {
		values[key] = value
	}
	for key, value := range EnvFileValues(cfg.EnvFile()) {
		values[key] = value
	}
	return values
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

func (c *credentialBrokerCandidate) route() *networkgateway.CredentialBrokerRoute {
	if c == nil {
		return nil
	}
	return &networkgateway.CredentialBrokerRoute{Provider: c.provider, Upstream: c.spec.Upstream,
		Header: c.spec.Header, Method: c.spec.Method, Path: c.spec.Path, Port: c.spec.Port}
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
	content += f.broker.candidate.spec.BaseURLEnv + "=http://" + networkgateway.CredentialBrokerAddress + "\n"
	path, err := artifacts.writeFile(artifacts.parent, content)
	if err != nil {
		return "", fmt.Errorf("prepare credential broker environment: %w", err)
	}
	return path, nil
}
