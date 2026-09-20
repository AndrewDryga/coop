package box

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/mcp"
	"github.com/AndrewDryga/coop/internal/networkgateway"
)

// A run brokers at most this many provider routes, one per provider; its MCP routes, one per
// bearer server, follow them in the same listener range, bounded by the shared file's own limit of
// 64 servers (mcp.NetworkServers) — the gateway's 72.
const maxProviderRoutes = 8

// SessionMCPHandoffEnv names the host file a session child writes its final ACP mcpServers list to.
// The daemon renders session/new from it, because only the child knows the broker listeners and
// stand-ins its adapter must be handed.
const SessionMCPHandoffEnv = "COOP_SESSION_MCP_HANDOFF"

// mcpRoute is one secret-bearing MCP server a run brokers: the upstream its literal URL names, the
// header its secret rides (lower-case for the gateway, as spelled for the snapshot) after its
// literal prefix, and the secret — read on the host from the variable the box would have carried it
// in, and cleared once written into the broker's secret. The box reaches it as COOP_MCP_TOKEN_<i>
// at listener i.
type mcpRoute struct {
	server            string
	upstream          string
	path              string // as the client sends it, escaped
	header, headerKey string
	prefix            string
	bearer            bool
	variable          string
	token             string
}

// planMCPRoutes adds a route for every secret-bearing server of a run's MCP snapshot: a
// bearer_token_env_var, or one header that is literal text then one ${VARIABLE}. It refuses a
// server no fixed route can carry (mcp.SecretServers, mcpRouteFor) and a missing secret. Under
// filtered networking admission already proved every definition literal, so only bearer servers
// reach this. (-e: mcpScrub.)
func planMCPRoutes(cfg *config.Config, spec RunSpec, snapshot []byte, plan *credentialPlan) (*credentialPlan, error) {
	servers, unbrokerable, err := mcp.SecretServers(snapshot)
	if err != nil {
		return nil, err
	}
	if len(unbrokerable) != 0 {
		return nil, errors.New(unbrokerable[0].Reason)
	}
	values := effectiveMCPEnv(cfg, spec)
	for _, server := range servers {
		route, reason, err := mcpRouteFor(server, values)
		if err != nil {
			return nil, err
		}
		if reason != "" {
			return nil, errors.New(reason)
		}
		if plan == nil {
			plan = &credentialPlan{}
		}
		plan.mcp = append(plan.mcp, route)
	}
	return plan, nil
}

// mcpRouteFor is the route for one secret-bearing server, or the sentence saying why no fixed route
// can carry it: an SSE server names its own message endpoint at runtime, a header the broker itself
// controls cannot carry a secret, and the broker reaches only plain https on 443, at a path it would
// admit. A missing secret is an error its caller decides what to do with.
func mcpRouteFor(server mcp.SecretServer, values map[string]string) (*mcpRoute, string, error) {
	if server.Transport == "sse" {
		return nil, fmt.Sprintf("MCP server %q uses the SSE transport, whose token Coop cannot keep outside the box; give it its streamable HTTP URL", server.Name), nil
	}
	// The gateway holds a secret header to names it does not control, after a bounded literal
	// prefix; one it would reject is named HERE, so a filtered run refuses by server name instead
	// of failing the whole launch configuration later, and an open run keeps that server as it is.
	if !networkgateway.MCPSecretHeader(server.Header, server.Prefix) {
		return nil, fmt.Sprintf("MCP server %q's %s header cannot carry its secret through Coop: that header, or the text before the secret, is one the broker itself controls", server.Name, server.HeaderKey), nil
	}
	parsed, err := url.Parse(server.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" ||
		parsed.Fragment != "" || parsed.Port() != "" && parsed.Port() != "443" {
		return nil, fmt.Sprintf("MCP server %q's URL must be plain https:// on port 443 for Coop to keep its secret outside the box", server.Name), nil
	}
	// The broker admits only a clean path; one it would refuse must fail here, by name.
	if clean := path.Clean(parsed.Path); parsed.Path != "" && clean != parsed.Path && clean+"/" != parsed.Path {
		return nil, fmt.Sprintf("MCP server %q's URL path must be plain — no dot segments or doubled slashes — for Coop to keep its secret outside the box", server.Name), nil
	}
	token := values[server.Variable]
	if strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\x00\r\n") {
		return nil, "", fmt.Errorf("MCP server %q needs %s, which has no usable value", server.Name, server.Variable)
	}
	escaped := parsed.EscapedPath()
	if escaped == "" {
		escaped = "/"
	}
	return &mcpRoute{server: server.Name, upstream: parsed.Hostname(), path: escaped, header: server.Header, headerKey: server.HeaderKey,
		prefix: server.Prefix, bearer: server.Bearer, variable: server.Variable, token: token}, "", nil
}

// effectiveMCPEnv is what an MCP token variable would hold in the box: project defaults, then the
// operator's env file (a session child's private one), later assignments winning.
func effectiveMCPEnv(cfg *config.Config, spec RunSpec) map[string]string {
	values := make(map[string]string, len(spec.projectEnv))
	for key, value := range spec.projectEnv {
		values[key] = value
	}
	if spec.Homes {
		for key, value := range EnvFileValues(cfg.EnvFile()) {
			values[key] = value
		}
	}
	return values
}

// mcpScrub is what a filtered box keeps out of its env: every MCP token variable of the configured
// file. One set through -e cannot enter it either, whether this box loads MCP or not.
func mcpScrub(cfg *config.Config, spec RunSpec) ([]string, error) {
	names, err := mcpScrubNames(cfg, spec)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		if extraEnvAssigns(cfg.ExtraRunArgs, name) || extraEnvAssigns(spec.ExtraArgs, name) {
			return nil, fmt.Errorf("%s holds an MCP server's token, which cannot enter an agent box through -e", name)
		}
	}
	return names, nil
}

// dropEnvNames is the box's env file without names — a copy only when one of them is there; the
// input itself when none is, so there is nothing new to mount or remove.
func dropEnvNames(artifacts compositionArtifactOps, source string, names []string) (string, error) {
	if source == "" || len(names) == 0 {
		return source, nil
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return "", fmt.Errorf("read the box environment: %w", err)
	}
	drop := make(map[string]bool, len(names))
	for _, name := range names {
		drop[name] = true
	}
	kept := filteredEnvContent(data, drop)
	if kept == string(data) {
		return source, nil
	}
	return artifacts.writeFile(artifacts.parent, kept)
}

// mcpScrubNames are the variables the run's configured MCP file reads a secret from. A box that
// keeps them out — filtered, or offline — never receives them whether it loads MCP or not, so they
// come from the configured file itself, not from what this box mounts. A box that loads none
// (sign-in, a review's format correction) reads that file's CONTENT leniently — a malformed file
// must not stop it — but its LOCATION is still held to the same isolation rule as a box that loads
// it: a source inside a mounted root could be rewritten by the box it is being read for. Without
// homes there is no env file to scrub.
func mcpScrubNames(cfg *config.Config, spec RunSpec) ([]string, error) {
	if spec.mcpSnapshot != nil {
		return mcp.CredentialReferences(spec.mcpSnapshot)
	}
	if !spec.Homes || cfg.MCPFile == "" {
		return nil, nil
	}
	source, err := validateMCPSourceIsolation(cfg, spec)
	if err != nil {
		return nil, err
	}
	return mcp.ReferencedCredentialNames(source), nil
}

// writeMCPSnapshots writes the two files every projection reads — the snapshot the generators,
// nested arms and a session's ACP list take, and claude's view of it — AFTER routing a filtered
// run's bearer servers through the broker, so no rendering can see a real server URL or variable.
// written lists what it created, for the caller to remove, even on failure.
func writeMCPSnapshots(artifacts compositionArtifactOps, snapshot []byte, brokered map[string]mcp.BrokeredServer) (path, claudePath string, written []string, err error) {
	if snapshot, err = mcp.RouteThroughBroker(snapshot, brokered); err != nil {
		return "", "", nil, fmt.Errorf("route MCP servers through the credential broker: %w", err)
	}
	if path, err = artifacts.writeFile(artifacts.parent, string(snapshot)); err != nil {
		return "", "", nil, fmt.Errorf("snapshot mcp.json: %w", err)
	}
	written = append(written, path)
	claudeView, err := mcp.ClaudeView(snapshot)
	if err == nil {
		claudePath, err = artifacts.writeFile(artifacts.parent, string(claudeView))
	}
	if err != nil {
		return "", "", written, fmt.Errorf("snapshot mcp.json for claude: %w", err)
	}
	return path, claudePath, append(written, claudePath), nil
}

// mcpListener is MCP route j's listener index, after the provider routes.
func (p *credentialPlan) mcpListener(j int) int { return len(p.routes) + j }

// mcpTokenEnv is the Coop-owned variable carrying MCP route j's substitute into the box.
func (p *credentialPlan) mcpTokenEnv(j int) string {
	return mcp.BrokerTokenPrefix + strconv.Itoa(p.mcpListener(j))
}

// brokeredServers is how the box reaches each brokered server: its listener, as address names it, and
// its stand-in.
func (p *credentialPlan) brokeredServers(address func(listener int) string) map[string]mcp.BrokeredServer {
	if p == nil || len(p.mcp) == 0 {
		return nil
	}
	servers := make(map[string]mcp.BrokeredServer, len(p.mcp))
	for j, route := range p.mcp {
		brokered := mcp.BrokeredServer{
			URL:      "http://" + address(p.mcpListener(j)) + route.path,
			TokenEnv: p.mcpTokenEnv(j),
		}
		if !route.bearer {
			brokered.HeaderKey, brokered.Prefix = route.headerKey, route.prefix
		}
		servers[route.server] = brokered
	}
	return servers
}

// mcpServerNames are the MCP servers the plan brokers, in route order.
func (p *credentialPlan) mcpServerNames() []string {
	var names []string
	for _, route := range p.mcpRoutesOrNil() {
		names = append(names, route.server)
	}
	return names
}

// mcpRoutesOrNil is the plan's MCP routes, none without a plan.
func (p *credentialPlan) mcpRoutesOrNil() []*mcpRoute {
	if p == nil {
		return nil
	}
	return p.mcp
}

// checkBrokerServePorts refuses, by name, a published serve port a broker listener needs — the
// gateway would otherwise refuse the whole configuration without saying which port.
func (p *credentialPlan) checkBrokerServePorts(serve []int) error {
	if p == nil {
		return nil
	}
	for i := range len(p.routes) + len(p.mcp) + len(p.downloads) {
		if port := networkgateway.CredentialBrokerPort + i; slices.Contains(serve, port) {
			return fmt.Errorf("serve port %d is one this filtered run needs to keep its credentials outside the box; serve another port", port)
		}
	}
	return nil
}

// sessionMCPHandoff is what a session child hands the daemon: the final ACP mcpServers list for its
// lead adapter, bound to the run and broker generation that minted its stand-ins.
type sessionMCPHandoff struct {
	RunID      string           `json:"run_id"`
	Epoch      string           `json:"gateway_epoch"`
	MCPServers []map[string]any `json:"mcpServers"`
}

// mcpStandIns is what a session handoff resolves the box's MCP variables with: each brokered
// server's stand-in by the variable the box reads it from, then — in an open box — the values a
// server Coop could not broker still reads in the box, exactly as the box holds them.
type mcpStandIns struct {
	epoch  string
	values map[string]string
	kept   map[string]string
}

func (f *filteredExecution) mcpStandIns() mcpStandIns {
	if f == nil {
		return mcpStandIns{}
	}
	standIns := mcpStandIns{epoch: f.record.Epoch, values: map[string]string{}}
	if f.broker != nil {
		for j := range f.broker.plan.mcpRoutesOrNil() {
			standIns.values[f.broker.plan.mcpTokenEnv(j)] = f.broker.substitutes[f.broker.plan.mcpListener(j)]
		}
	}
	return standIns
}

// handOffSessionMCP answers the daemon's request (target, from SessionMCPHandoffEnv) when this run is
// a session child: only the child knows the listeners and stand-ins, and the daemon must never render
// its ACP list from the real private env. Any other run writes nothing.
func handOffSessionMCP(spec RunSpec, target, snapshotPath string, standIns mcpStandIns) error {
	if target == "" || spec.networkClient() != egress.ClientACP {
		return nil
	}
	lead, ok := agents.Get(spec.Agent)
	if !ok {
		return errors.New("a session's MCP handoff needs its lead agent")
	}
	return writeSessionMCPHandoff(target, spec.RunID, lead, snapshotPath, standIns)
}

// writeSessionMCPHandoff renders the lead adapter's ACP mcpServers from the box's own snapshot —
// listeners and stand-ins for every brokered server, never a real token Coop kept out of the box —
// and writes them where the daemon asked, atomically and owner-only, before the container starts.
func writeSessionMCPHandoff(target, runID string, lead agents.Agent, snapshotPath string, standIns mcpStandIns) error {
	if !filepath.IsAbs(target) {
		return errors.New("the session's MCP handoff path must be absolute")
	}
	servers, err := lead.ACPMCPServers(snapshotPath, func(key string) (string, bool) {
		if value, ok := standIns.values[key]; ok {
			return value, true
		}
		value, ok := standIns.kept[key]
		return value, ok
	})
	if err != nil {
		return err
	}
	if servers == nil {
		servers = []map[string]any{}
	}
	data, err := json.Marshal(sessionMCPHandoff{RunID: runID, Epoch: standIns.epoch, MCPServers: servers})
	if err != nil {
		return err
	}
	if err := config.WriteFileAtomic(target, data); err != nil {
		return fmt.Errorf("hand the session its MCP servers: %w", err)
	}
	return nil
}

// ReadSessionMCPHandoff is the daemon's side of the handoff: one bounded, owner-only regular file,
// written by this run's own child — a stale or foreign run's list never becomes this session's.
func ReadSessionMCPHandoff(path, runID string) ([]map[string]any, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("the session child handed over no MCP servers: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > 4<<20 {
		return nil, errors.New("the session child's MCP handoff is not an owner-only regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var handoff sessionMCPHandoff
	if err := json.Unmarshal(data, &handoff); err != nil || handoff.RunID == "" || handoff.RunID != runID || handoff.MCPServers == nil {
		return nil, errors.New("the session child's MCP handoff belongs to another run")
	}
	return handoff.MCPServers, nil
}
