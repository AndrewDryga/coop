package box

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkgateway"
)

// credentialRoute is one brokered provider account in a run: the adapter's request shape, the real
// key (host side only, cleared once it is written into the guard's secret), and the auth marker the
// box must not see.
type credentialRoute struct {
	provider     string
	account      string
	spec         agents.CredentialBrokerSpec
	credential   string
	shadowMarker string
}

// credentialPlan is every brokered route one run selects, in listener order: the provider routes,
// then the MCP routes, route i served at networkgateway.CredentialBrokerAddress(i). A box holds one
// account per provider, so a plan has at most one route per provider — every teammate of that
// provider in the box shares it — and one route per bearer-authenticated MCP server.
type credentialPlan struct {
	routes []*credentialRoute
	mcp    []*mcpRoute
}

func (p *credentialPlan) route(provider string) (int, *credentialRoute) {
	if p != nil {
		for i, route := range p.routes {
			if route.provider == provider {
				return i, route
			}
		}
	}
	return -1, nil
}

func (p *credentialPlan) routesOrNil() []*credentialRoute {
	if p == nil {
		return nil
	}
	return p.routes
}

// gatewayRoutes is the run's broker plan as the gateway reads it; none without a broker.
func (r *credentialBrokerRun) gatewayRoutes() []networkgateway.CredentialBrokerRoute {
	if r == nil {
		return nil
	}
	return r.plan.gatewayRoutes()
}

func (p *credentialPlan) baseURL(i int) string {
	return "http://" + networkgateway.CredentialBrokerAddress(i) + p.routes[i].spec.ClientBasePath
}

type credentialBrokerRun struct {
	plan        *credentialPlan
	substitutes []string // route i's capability, bound to route i's listener
	configPath  string
	configInfo  os.FileInfo
}

// selectCredentialPlan recognizes reusable credentials before a box is assembled. Every provider
// account the run selects that holds a brokerable key gets a route of one filtered run's broker;
// a shape the broker cannot serve refuses instead of falling back to putting a key in the container.
func selectCredentialPlan(cfg *config.Config, spec RunSpec) (*credentialPlan, error) {
	return selectCredentialPlanWithMarkers(cfg, spec, nil)
}

func selectCredentialPlanWithMarkers(cfg *config.Config, spec RunSpec, markers map[string]bool) (*credentialPlan, error) {
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
	plan := &credentialPlan{}
	for _, name := range credentialScope(cfg, spec) {
		route, err := credentialBrokerCandidateFor(cfg, spec, name, cfg.ActiveProfile(name), markers)
		if err != nil {
			return nil, err
		}
		if route != nil {
			plan.routes = append(plan.routes, route)
		}
	}
	if len(plan.routes) == 0 {
		return nil, nil
	}
	first := plan.routes[0]
	// The agent's own command or its ACP adapter; a maintenance command under an agent's
	// credential scope is neither.
	if !spec.AgentCommand && spec.networkClient() != egress.ClientACP {
		return nil, fmt.Errorf("%s API-key brokering serves agents, not this command; run the agent itself", credentialBrokerAgentName(first.provider))
	}
	if spec.Mode.Restricted() {
		return nil, fmt.Errorf("%s %s needs filtered networking, which the read-only and bare modes cannot use yet; sign in with the provider instead", credentialBrokerAgentName(first.provider), first.spec.CredentialEnv)
	}
	if len(plan.routes) > maxProviderRoutes {
		return nil, fmt.Errorf("one run can protect at most %d API-key accounts", maxProviderRoutes)
	}
	return plan, nil
}

// requireFiltered refuses a plan in a run without the filtered gateway: the broker is the only way
// a key stays outside the box. Whether a run is filtered is its caller's to say — an ACP child's or
// a session's filtered authority arrives as a capture, not as its configured egress.
func (p *credentialPlan) requireFiltered() error {
	if len(p.routesOrNil()) == 0 {
		return nil
	}
	first := p.routes[0]
	return fmt.Errorf("%s %s requires filtered networking so Coop can keep it outside the box; use --egress filtered or sign in with the provider instead", credentialBrokerAgentName(first.provider), first.spec.CredentialEnv)
}

func credentialBrokerAgentName(name string) string {
	if agent, ok := agents.Get(name); ok {
		return agent.DisplayName()
	}
	return name
}

func credentialBrokerCandidateFor(cfg *config.Config, spec RunSpec, name, profile string, markers map[string]bool) (*credentialRoute, error) {
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
	return &credentialRoute{provider: name, account: profile, spec: broker, credential: credential, shadowMarker: shadowMarker}, nil
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

// gatewayRoutes is the plan as the gateway's non-secret launch authority, in listener order.
func (p *credentialPlan) gatewayRoutes() []networkgateway.CredentialBrokerRoute {
	if p == nil {
		return nil
	}
	routes := make([]networkgateway.CredentialBrokerRoute, 0, len(p.routes)+len(p.mcp))
	for _, r := range p.routes {
		routes = append(routes, networkgateway.CredentialBrokerRoute{Name: r.provider, Kind: networkgateway.CredentialBrokerProvider,
			Upstream: r.spec.Upstream, Header: r.spec.Header, HeaderPrefix: r.spec.HeaderPrefix, Methods: []string{r.spec.Method},
			Path: r.spec.Path, PathPrefix: r.spec.PathPrefix, AllowQuery: r.spec.AllowQuery, Port: r.spec.Port})
	}
	for j, r := range p.mcp {
		// The path a request carries is decoded before the gateway compares it.
		decoded, err := url.PathUnescape(r.path)
		if err != nil {
			decoded = r.path // planMCPRoutes took it from a parsed URL, so this cannot happen
		}
		routes = append(routes, networkgateway.CredentialBrokerRoute{Name: "mcp-" + strconv.Itoa(p.mcpListener(j)),
			Kind: networkgateway.CredentialBrokerMCP, Upstream: r.upstream, Header: r.header, HeaderPrefix: r.prefix,
			Methods: []string{"POST", "GET", "DELETE"}, Path: decoded, Port: 443})
	}
	return routes
}

func (f *filteredExecution) prepareCredentialBroker(artifacts compositionArtifactOps) error {
	if f.broker == nil || f.broker.plan == nil {
		return nil
	}
	root, err := f.store.RunFilesPath(f.record.ID)
	if err != nil || root != artifacts.parent {
		return errors.New("credential broker artifact ownership changed")
	}
	secrets := networkgateway.CredentialBrokerSecrets{Version: 2, RunID: f.record.ID, Epoch: f.record.Epoch}
	routes := f.broker.plan.gatewayRoutes()
	f.broker.substitutes = make([]string, len(routes))
	for i, route := range routes {
		random := make([]byte, 32)
		if _, err := rand.Read(random); err != nil {
			return errors.New("create credential broker substitute")
		}
		f.broker.substitutes[i] = hex.EncodeToString(random)
		var credential string
		if i < len(f.broker.plan.routes) {
			credential, f.broker.plan.routes[i].credential = f.broker.plan.routes[i].credential, ""
		} else {
			mcpRoute := f.broker.plan.mcp[i-len(f.broker.plan.routes)]
			credential, mcpRoute.token = mcpRoute.token, ""
		}
		secrets.Routes = append(secrets.Routes, networkgateway.CredentialBrokerSecret{Name: route.Name,
			Substitute: f.broker.substitutes[i], Credential: credential})
	}
	data, err := json.Marshal(secrets)
	for i := range secrets.Routes {
		secrets.Routes[i].Credential = ""
	}
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

// credentialBrokerEnv rewrites the box environment for every route: the provider's own credential
// keys and base URL are dropped, and its capability and route URL take their place. One environment
// serves the whole box, so every teammate of that provider reaches the same route.
func (f *filteredExecution) credentialBrokerEnv(artifacts compositionArtifactOps, source string) (string, error) {
	var plan *credentialPlan
	if f.broker != nil {
		plan = f.broker.plan
	}
	// The configured MCP file's token variables never enter a filtered box, whatever it loads.
	drop := map[string]bool{}
	for _, name := range f.mcpScrub {
		drop[name] = true
	}
	for _, route := range plan.routesOrNil() {
		drop[route.spec.BaseURLEnv] = true
		if agent, ok := agents.Get(route.provider); ok {
			for _, key := range agent.CredentialEnvKeys() {
				drop[key] = true
			}
		}
	}
	if len(drop) == 0 && len(plan.mcpRoutesOrNil()) == 0 {
		return source, nil
	}
	content := ""
	if source != "" {
		data, err := os.ReadFile(source)
		if err != nil {
			return "", fmt.Errorf("read environment for credential broker: %w", err)
		}
		content = strings.TrimRight(filteredEnvContent(data, drop), "\n")
	}
	if content != "" {
		content += "\n"
	}
	for i, route := range plan.routesOrNil() {
		content += route.spec.CredentialEnv + "=" + f.broker.substitutes[i] + "\n"
		content += route.spec.BaseURLEnv + "=" + plan.baseURL(i) + "\n"
	}
	for j := range plan.mcpRoutesOrNil() {
		content += plan.mcpTokenEnv(j) + "=" + f.broker.substitutes[plan.mcpListener(j)] + "\n"
	}
	path, err := artifacts.writeFile(artifacts.parent, content)
	if err != nil {
		return "", fmt.Errorf("prepare credential broker environment: %w", err)
	}
	return path, nil
}

// credentialBrokerMounts are each route's read-only files: an empty auth marker over the account's
// native one, so a client that prefers its stored credential still meets only the route, and the
// client's own configuration where the adapter needs one to reach the route.
func (f *filteredExecution) credentialBrokerMounts(artifacts compositionArtifactOps, homeInBox string) ([]extraMount, []string, error) {
	if f.broker == nil || f.broker.plan == nil {
		return nil, nil, nil
	}
	var mounts []extraMount
	var paths []string
	write := func(content, target, what string) error {
		path, err := artifacts.writeFile(artifacts.parent, content)
		if err != nil {
			return fmt.Errorf("prepare %s: %w", what, err)
		}
		paths = append(paths, path)
		mounts = append(mounts, extraMount{path, target})
		return nil
	}
	for i, route := range f.broker.plan.routes {
		if route.shadowMarker != "" {
			if err := write("{}\n", filepath.Join(homeInBox, "."+route.provider, route.shadowMarker), "credential marker shadow"); err != nil {
				return nil, paths, err
			}
		}
		if route.spec.Config != nil {
			file := route.spec.Config(f.broker.plan.baseURL(i))
			if err := write(file.Content, file.Path, route.provider+" broker configuration"); err != nil {
				return nil, paths, err
			}
		}
	}
	return mounts, paths, nil
}

// launchAccounts is every account the run selected, in its scope order, as the launch presents it:
// a brokered route is a protected key, any other account a signed-in login, and a provider with no
// credential at all is not an account the run connects.
func launchAccounts(cfg *config.Config, spec RunSpec, plan *credentialPlan) []accountRow {
	if spec.Login {
		return nil // a sign-in box creates the account; it connects none
	}
	var rows []accountRow
	for _, name := range credentialScope(cfg, spec) {
		account := cfg.ActiveProfile(name)
		if _, route := plan.route(name); route != nil {
			rows = append(rows, accountRow{provider: name, account: route.account, protected: true})
		} else if ProfileAuthed(cfg, name, account) {
			rows = append(rows, accountRow{provider: name, account: account})
		}
	}
	return rows
}
