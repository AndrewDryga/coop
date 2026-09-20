package box

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/mcp"
	"github.com/AndrewDryga/coop/internal/networkgateway"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/runtime"
)

const brokeredMCPSnapshot = `{"mcpServers":{
  "emisar":{"type":"http","url":"https://emisar.example/mcp","bearer_token_env_var":"EMISAR_TOKEN"},
  "mirror":{"type":"http","url":"https://mirror.example/v1/mcp/","bearer_token_env_var":"EMISAR_TOKEN"},
  "open":{"type":"http","url":"https://open.example/mcp"}
}}`

// A filtered run brokers each bearer MCP server beside its provider routes: an exact-path route per
// server after the provider's, the real token in the guard's secret only, and in the box a
// Coop-owned stand-in where the operator's variable was — two servers sharing one variable still
// get two stand-ins, each bound to its own listener.
func TestFilteredRunBrokersEveryBearerMCPServer(t *testing.T) {
	cfg, spec := brokerFixture(t, "ANTHROPIC_API_KEY=claude-secret\nEMISAR_TOKEN=emisar-secret\nSHARED=kept\n")
	plan, err := selectCredentialPlan(cfg, spec)
	if err == nil {
		plan, err = planMCPRoutes(cfg, spec, []byte(brokeredMCPSnapshot), plan)
	}
	if err != nil || len(plan.routes) != 1 || len(plan.mcp) != 2 {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	routes := plan.gatewayRoutes()
	for i, want := range []networkgateway.CredentialBrokerRoute{
		{Name: "mcp-1", Kind: networkgateway.CredentialBrokerMCP, Upstream: "emisar.example", Header: "authorization", HeaderPrefix: "Bearer ",
			Methods: []string{"POST", "GET", "DELETE"}, Path: "/mcp", Port: 443},
		{Name: "mcp-2", Kind: networkgateway.CredentialBrokerMCP, Upstream: "mirror.example", Header: "authorization", HeaderPrefix: "Bearer ",
			Methods: []string{"POST", "GET", "DELETE"}, Path: "/v1/mcp/", Port: 443},
	} {
		if got := routes[i+1]; got.Name != want.Name || got.Kind != want.Kind || got.Upstream != want.Upstream || got.Path != want.Path ||
			got.Header != want.Header || got.HeaderPrefix != want.HeaderPrefix || !slices.Equal(got.Methods, want.Methods) || got.Port != want.Port {
			t.Fatalf("MCP route %d = %+v, want %+v", i, got, want)
		}
	}
	if routes[0].Name != "claude" || routes[0].Kind != networkgateway.CredentialBrokerProvider {
		t.Fatalf("the provider route moved: %+v", routes[0])
	}

	f, _ := filteredFixture(t)
	f.broker = &credentialBrokerRun{plan: plan}
	f.mcpScrub = []string{"EMISAR_TOKEN"}
	artifacts := defaultCompositionArtifactOps()
	artifacts.parent = f.runfiles
	if err := f.prepareCredentialBroker(artifacts); err != nil {
		t.Fatal(err)
	}
	config := networkgateway.LaunchConfig{RunID: f.record.ID, Epoch: f.record.Epoch, Brokers: routes}
	secrets, err := networkgateway.ReadCredentialBrokerSecrets(bytes.NewReader(mustReadFile(t, f.broker.configPath)), config)
	if err != nil {
		t.Fatalf("the guard would refuse this secret: %v", err)
	}
	if secrets.Routes[1].Credential != "emisar-secret" || secrets.Routes[2].Credential != "emisar-secret" ||
		secrets.Routes[1].Substitute == secrets.Routes[2].Substitute {
		t.Fatalf("each MCP route does not hold its own capability for the real token: %+v", secrets.Routes)
	}
	for _, route := range plan.mcp {
		if route.token != "" {
			t.Fatalf("the host kept %s's token after writing the guard's secret", route.server)
		}
	}

	envFile, err := f.credentialBrokerEnv(artifacts, cfg.EnvFile())
	if err != nil {
		t.Fatal(err)
	}
	values := EnvFileValues(envFile)
	if data := string(mustReadFile(t, envFile)); strings.Contains(data, "emisar-secret") || strings.Contains(data, "claude-secret") {
		t.Fatalf("a real token reached the box environment: %s", data)
	}
	if _, ok := values["EMISAR_TOKEN"]; ok || values["COOP_MCP_TOKEN_1"] != f.broker.substitutes[1] ||
		values["COOP_MCP_TOKEN_2"] != f.broker.substitutes[2] || values["SHARED"] != "kept" {
		t.Fatalf("box environment = %#v", values)
	}

	rewritten, err := mcp.RouteThroughBroker([]byte(brokeredMCPSnapshot), plan.brokeredServers(networkgateway.CredentialBrokerAddress))
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(rewritten, &root); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][2]string{
		"emisar": {"http://" + networkgateway.CredentialBrokerAddress(1) + "/mcp", "COOP_MCP_TOKEN_1"},
		"mirror": {"http://" + networkgateway.CredentialBrokerAddress(2) + "/v1/mcp/", "COOP_MCP_TOKEN_2"},
	} {
		if server := root.MCPServers[name]; server["url"] != want[0] || server["bearer_token_env_var"] != want[1] {
			t.Fatalf("%s in the box = %#v, want %v", name, server, want)
		}
	}
	if root.MCPServers["open"]["url"] != "https://open.example/mcp" {
		t.Fatal("a tokenless server was routed")
	}
}

// What the broker cannot keep outside the box stops the launch, naming the server: an SSE server (it
// names its own message endpoint at runtime), a missing token, and a path the broker would refuse
// once running.
func TestFilteredRunRefusesAnMCPTokenItCannotBroker(t *testing.T) {
	for name, c := range map[string]struct {
		env, definition string
	}{
		"SSE":           {"EMISAR_TOKEN=secret\n", `{"type":"sse","url":"https://emisar.example/sse","bearer_token_env_var":"EMISAR_TOKEN"}`},
		"missing token": {"", `{"type":"http","url":"https://emisar.example/mcp","bearer_token_env_var":"EMISAR_TOKEN"}`},
		"dirty path":    {"EMISAR_TOKEN=secret\n", `{"type":"http","url":"https://emisar.example/a/../mcp","bearer_token_env_var":"EMISAR_TOKEN"}`},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, spec := brokerFixture(t, c.env)
			_, err := planMCPRoutes(cfg, spec, []byte(`{"mcpServers":{"emisar":`+c.definition+`}}`), nil)
			if err == nil || !strings.Contains(err.Error(), `"emisar"`) {
				t.Fatalf("planMCPRoutes = %v, want a refusal naming the server", err)
			}
		})
	}
}

// A filtered box that loads no MCP — a shell, a sign-in box — still never receives the variables the
// configured file reads a token from: the names come from that file, read leniently (a broken file
// must not stop a sign-in), not from what the box mounts. Nor can -e put one in, in any box.
func TestFilteredBoxWithoutMCPDropsTheConfiguredTokenVariables(t *testing.T) {
	cfg, spec := brokerFixture(t, "EMISAR_TOKEN=emisar-secret\nSHARED=kept\n")
	cfg.MCPFile = filepath.Join(t.TempDir(), "mcp.json")
	// Valid JSON the strict validator refuses (a reserved stand-in name beside a real reference).
	if err := os.WriteFile(cfg.MCPFile, []byte(`{"mcpServers":{"emisar":{"type":"http","url":"https://emisar.example/mcp","bearer_token_env_var":"EMISAR_TOKEN"},"odd":{"type":"http","url":"https://odd.example/mcp","bearer_token_env_var":"COOP_MCP_TOKEN_9"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	spec.Agent, spec.AgentCommand, spec.Login = "claude", true, true // a sign-in box reads no snapshot
	names, err := mcpScrub(cfg, spec)
	if err != nil || !slices.Contains(names, "EMISAR_TOKEN") {
		t.Fatalf("scrub names = %q, %v", names, err)
	}
	for _, extra := range [][]string{{"-e", "EMISAR_TOKEN=smuggled"}, {"--env", "EMISAR_TOKEN"}} {
		spec.ExtraArgs = extra
		if _, err := mcpScrub(cfg, spec); err == nil || !strings.Contains(err.Error(), "EMISAR_TOKEN") {
			t.Fatalf("%q = %v, want the token variable refused", extra, err)
		}
	}
	spec.ExtraArgs = nil
	f := &filteredExecution{mcpScrub: names}
	artifacts := defaultCompositionArtifactOps()
	artifacts.parent = t.TempDir()
	envFile, err := f.credentialBrokerEnv(artifacts, cfg.EnvFile())
	if err != nil {
		t.Fatal(err)
	}
	if values := EnvFileValues(envFile); values["EMISAR_TOKEN"] != "" || values["SHARED"] != "kept" {
		t.Fatalf("box environment = %#v", values)
	}
}

// The agent's own policy never grants a bearer server's host: only the broker reaches it, with the
// token. A tokenless server keeps its grant.
func TestFilteredPolicyWithholdsBearerMCPHosts(t *testing.T) {
	dependencies, err := networkMCPDependenciesOf([]byte(brokeredMCPSnapshot))
	if err != nil {
		t.Fatal(err)
	}
	var granted []string
	for _, dependency := range dependencies {
		for _, rule := range dependency.Rules {
			granted = append(granted, rule.To.Domain)
		}
	}
	if !slices.Equal(granted, []string{"open.example"}) {
		t.Fatalf("automatic MCP grants = %q, want only the tokenless server", granted)
	}
}

// A filtered session child hands the daemon its lead adapter's ACP list — broker URLs and stand-ins,
// never a real token — bound to its run; a list from another run is refused.
func TestSessionMCPHandoffCarriesOnlyStandInsForItsOwnRun(t *testing.T) {
	cfg, spec := brokerFixture(t, "EMISAR_TOKEN=emisar-secret\n")
	plan, err := planMCPRoutes(cfg, spec, []byte(brokeredMCPSnapshot), nil)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := filteredFixture(t)
	f.broker = &credentialBrokerRun{plan: plan}
	artifacts := defaultCompositionArtifactOps()
	artifacts.parent = f.runfiles
	if err := f.prepareCredentialBroker(artifacts); err != nil {
		t.Fatal(err)
	}
	rewritten, err := mcp.RouteThroughBroker([]byte(brokeredMCPSnapshot), plan.brokeredServers(networkgateway.CredentialBrokerAddress))
	if err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(snapshot, rewritten, 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "handoff.json")
	child := RunSpec{Agent: "claude", RunID: "session-run-1", NetworkClient: egress.ClientACP}
	// Only an ACP child the daemon asked answers: a CLI box and a run nobody asked write nothing.
	cli := child
	cli.NetworkClient = egress.ClientCLI
	for name, handOff := range map[string]func() error{
		"a CLI box":      func() error { return handOffSessionMCP(cli, target, snapshot, f.mcpStandIns()) },
		"an unasked run": func() error { return handOffSessionMCP(child, "", snapshot, f.mcpStandIns()) },
	} {
		err := handOff()
		if _, statErr := os.Stat(target); err != nil || !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("%s handed over MCP servers: %v", name, err)
		}
	}
	if err := handOffSessionMCP(child, target, snapshot, f.mcpStandIns()); err != nil {
		t.Fatal(err)
	}
	if data := string(mustReadFile(t, target)); strings.Contains(data, "emisar-secret") ||
		!strings.Contains(data, "Bearer "+f.broker.substitutes[0]) || !strings.Contains(data, networkgateway.CredentialBrokerAddress(0)+"/mcp") {
		t.Fatalf("handoff = %s", data)
	}
	servers, err := ReadSessionMCPHandoff(target, "session-run-1")
	if err != nil || len(servers) != 3 {
		t.Fatalf("read handoff = %v, %v", servers, err)
	}
	if _, err := ReadSessionMCPHandoff(target, "another-run"); err == nil {
		t.Fatal("a handoff from another run was accepted")
	}
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSessionMCPHandoff(target, "session-run-1"); err == nil {
		t.Fatal("a handoff others can read was accepted")
	}
}

// An MCP route must admit what each pinned client really sends to a streamable-HTTP server — the
// lines were captured offline from the locked clients against a local endpoint (recipe:
// .agent/kb/provider-client-qualification.md): POST the messages, GET the server's stream, and a
// secret written as a header ("X-Api-Key: ${VAR}", "X-Auth: Token ${VAR}") sent as its text with the
// variable's value in place — byte for byte what the broker compares.
func TestMCPRoutesAdmitWhatThePinnedClientsSend(t *testing.T) {
	type capture struct {
		version  string
		requests []string
		headers  []string // each header secret as it arrived, its variable holding <stand-in>
	}
	lines := []string{"POST /mcp", "GET /mcp"}
	headers := []string{"x-api-key: <stand-in>", "x-auth: Token <stand-in>"}
	captured := map[string]map[egress.Client]capture{
		"claude": {egress.ClientCLI: {"2.1.260", lines, headers}, egress.ClientACP: {"0.75.1", lines, headers}}, // the adapter's SDK claude, 2.1.257
		// codex-acp drives the same codex, and Coop refuses text before a reference for Codex.
		"codex":  {egress.ClientCLI: {"0.153.4", lines, headers[:1]}, egress.ClientACP: {"1.10.0", lines, headers[:1]}},
		"gemini": {egress.ClientCLI: {"0.59.0", lines, headers}, egress.ClientACP: {"0.59.0", lines, headers}},
		"grok":   {egress.ClientCLI: {"1.0.25", lines, headers}, egress.ClientACP: {"1.0.25", lines, headers}}, // grok first POSTs server/discover
	}
	cfg, spec := brokerFixture(t, "TOKEN=secret\nKEY=secret\nAUTH=secret\n")
	plan, err := planMCPRoutes(cfg, spec, []byte(`{"mcpServers":{
		"a":{"type":"http","url":"https://a.example/mcp","bearer_token_env_var":"TOKEN"},
		"b":{"type":"http","url":"https://b.example/mcp","headers":{"X-Api-Key":"${KEY}"}},
		"c":{"type":"http","url":"https://c.example/mcp","headers":{"X-Auth":"Token ${AUTH}"}}}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	routes := plan.gatewayRoutes()
	for _, name := range agents.Names() {
		agent, _ := agents.Get(name)
		for _, client := range agent.LockedClients(agents.ClientPlatform{OS: "linux", Architecture: "arm64", Libc: "glibc"}) {
			seen, ok := captured[name][client.Client]
			if !ok || seen.version != client.Version {
				t.Errorf("locked %s %s client is %s, but its MCP request lines were not captured from that version: capture them, then move this pin", name, client.Client, client.Version)
				continue
			}
			for _, line := range seen.requests {
				method, target, _ := strings.Cut(line, " ")
				parsed, err := url.ParseRequestURI(target)
				if err != nil || !routes[0].Admits(method, parsed) {
					t.Errorf("an MCP route refuses %s %s's %q", name, client.Client, line)
				}
			}
			for _, header := range seen.headers {
				key, value, _ := strings.Cut(header, ": ")
				if !slices.ContainsFunc(routes, func(route networkgateway.CredentialBrokerRoute) bool {
					return route.Header == key && value == route.HeaderPrefix+"<stand-in>"
				}) {
					t.Errorf("no MCP route accepts %s %s's %q", name, client.Client, header)
				}
			}
		}
	}
}

// With nothing to drop or add — a setup smoke, a run with no keys and no configured MCP tokens —
// the env step writes no file and hands back its input, which box.Run then must not record as its
// own: an empty or foreign path among the run's files fails the mount check or gets deleted.
func TestFilteredEnvStepWritesNothingWhenThereIsNothingToKeepOut(t *testing.T) {
	f := &filteredExecution{}
	artifacts := defaultCompositionArtifactOps()
	artifacts.parent = t.TempDir()
	wrote := false
	original := artifacts.writeFile
	artifacts.writeFile = func(parent, content string) (string, error) {
		wrote = true
		return original(parent, content)
	}
	for _, source := range []string{"", filepath.Join(t.TempDir(), "env")} {
		if got, err := f.credentialBrokerEnv(artifacts, source); err != nil || got != source || wrote {
			t.Fatalf("env step for %q = %q, %v (wrote %v); want its input back and no file", source, got, err, wrote)
		}
	}
}

// A raw box consumes no MCP configuration, so it gets no MCP route — a listener there would be a
// capability nobody asked for. Planning is proven by a server whose token is missing: an agent box
// refuses on it, a raw box never plans far enough to notice (and still keeps the names out).
func TestFilteredRawBoxPlansNoMCPRoute(t *testing.T) {
	store, err := networkstate.Open(filepath.Join(t.TempDir(), "network"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mode := egress.Filtered
	policy, err := store.Admit(repo, networkstate.Admission{InvocationMode: &mode})
	if err != nil {
		t.Fatal(err)
	}
	capture := &CapturedEgress{Store: store, Project: repo, Fingerprint: policy.Fingerprint}
	cfg := &config.Config{Egress: "filtered", ConfigDir: t.TempDir()}
	snapshot := []byte(`{"mcpServers":{"emisar":{"type":"http","url":"https://emisar.example/mcp","bearer_token_env_var":"EMISAR_TOKEN"}}}`)
	for _, c := range []struct {
		spec RunSpec
		mcp  bool
	}{
		{RunSpec{Repo: repo, Agent: "claude", AgentCommand: true, Homes: true, mcpSnapshot: snapshot}, true},
		{RunSpec{Repo: repo, Cmd: []string{"sh"}, Homes: true, mcpSnapshot: snapshot}, false},
	} {
		_, err := prepareFilteredExecution(t.Context(), cfg, recorderRuntime(t, filepath.Join(t.TempDir(), "runtime.log")), c.spec, capture, "", nil, nil)
		if planned := err != nil && strings.Contains(err.Error(), `MCP server "emisar" needs EMISAR_TOKEN`); planned != c.mcp {
			t.Fatalf("agent=%q: planned MCP routes = %v (err %v), want %v", c.spec.Agent, planned, err, c.mcp)
		}
	}
}

// Every projection reads files written AFTER the broker rewrite: the snapshot the generators take (a
// codex overlay built from it names only the stand-in and the listener) and claude's view (the
// stand-in as an Authorization header). No rendering can see the operator's variable or real URL.
func TestMCPSnapshotsAreWrittenOnlyAfterTheBrokerRewrite(t *testing.T) {
	cfg, spec := brokerFixture(t, "EMISAR_TOKEN=emisar-secret\n")
	plan, err := planMCPRoutes(cfg, spec, []byte(brokeredMCPSnapshot), nil)
	if err != nil {
		t.Fatal(err)
	}
	artifacts := defaultCompositionArtifactOps()
	artifacts.parent = t.TempDir()
	path, claudePath, written, err := writeMCPSnapshots(artifacts, []byte(brokeredMCPSnapshot), plan.brokeredServers(networkgateway.CredentialBrokerAddress))
	if err != nil || len(written) != 2 {
		t.Fatalf("writeMCPSnapshots = %v, %v", written, err)
	}
	codex, required, err := mcp.GenerateCodex(path, "")
	if err != nil {
		t.Fatal(err)
	}
	listener := networkgateway.CredentialBrokerAddress(0)
	if !strings.Contains(codex, `bearer_token_env_var = "COOP_MCP_TOKEN_0"`) || !strings.Contains(codex, "http://"+listener+"/mcp") ||
		!slices.Contains(required, "COOP_MCP_TOKEN_0") || slices.Contains(required, "EMISAR_TOKEN") {
		t.Fatalf("codex overlay from the rewritten snapshot = %s (required %q)", codex, required)
	}
	claude := string(mustReadFile(t, claudePath))
	if !strings.Contains(claude, `"Bearer ${COOP_MCP_TOKEN_0}"`) || !strings.Contains(claude, listener) {
		t.Fatalf("claude's view = %s", claude)
	}
	for _, file := range []string{codex, claude, string(mustReadFile(t, path))} {
		if strings.Contains(file, "EMISAR_TOKEN") || strings.Contains(file, "https://emisar.example") {
			t.Fatalf("a projection still names the operator's variable or real URL:\n%s", file)
		}
	}
}

// A session's Responder binding is one more bearer server, so a filtered session brokers its state
// tools like any other server and keeps the Responder token out of the box.
func TestFilteredSessionBrokersTheResponderBinding(t *testing.T) {
	cfg, spec := brokerFixture(t, mcp.ResponderStateTokenEnv+"=responder-secret\n")
	snapshot, err := mcp.BindResponderState(nil, "https://responder.example/v1/state-tools/mcp")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planMCPRoutes(cfg, spec, snapshot, nil)
	if err != nil || len(plan.mcp) != 1 {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	if route := plan.gatewayRoutes()[0]; route.Upstream != "responder.example" || route.Path != "/v1/state-tools/mcp" {
		t.Fatalf("Responder route = %+v", route)
	}
	if names, err := mcp.CredentialReferences(snapshot); err != nil || !slices.Contains(names, mcp.ResponderStateTokenEnv) {
		t.Fatalf("scrub names = %q, %v", names, err)
	}
}

// An offline box cannot reach a remote MCP server, so it gets none: no projection carries one, its
// token variables stay out of the box's env, and a -e of one is refused. A local server stays.
func TestOfflineRunLeavesRemoteMCPServersOut(t *testing.T) {
	configDir := t.TempDir()
	mcpFile := filepath.Join(configDir, "mcp.json")
	writeRepoFile(t, mcpFile, `{"mcpServers":{
		"local":{"command":"true"},
		"remote":{"type":"http","url":"https://remote.example/mcp","bearer_token_env_var":"REMOTE_TOKEN"},
		"headers":{"type":"http","url":"https://headers.example/mcp","headers":{"X-Key":"${HEADER_KEY}"}}}}`)
	writeRepoFile(t, filepath.Join(configDir, "env"), "REMOTE_TOKEN=remote-secret\nHEADER_KEY=header-secret\nKEPT=kept\n")
	cfg := &config.Config{ConfigDir: configDir, HomeInBox: "/home/node", MCPFile: mcpFile, MCPInBox: "/home/node/.mcp.json", Egress: "none"}
	// The runtime keeps the env file the box was handed: it is removed once the run returns.
	dir := t.TempDir()
	boxEnv, calls := filepath.Join(dir, "box-env"), filepath.Join(dir, "calls")
	shim := filepath.Join(dir, "rt")
	writeRepoFile(t, shim, "#!/bin/sh\necho \"$@\" >> "+strconv.Quote(calls)+"\n"+
		"prev=\nfor a in \"$@\"; do [ \"$prev\" = --env-file ] && cat \"$a\" > "+strconv.Quote(boxEnv)+"; prev=$a; done\n")
	if err := os.Chmod(shim, 0o755); err != nil {
		t.Fatal(err)
	}
	claude, _ := agents.Get("claude")
	spec := RunSpec{Image: "i", Repo: t.TempDir(), Cmd: claude.Interactive(cfg), Agent: "claude", AgentCommand: true, Homes: true,
		Batch: true, Quiet: true, Peers: []agents.Target{{Provider: "codex"}, {Provider: "gemini"}, {Provider: "grok"}}}
	artifacts := defaultCompositionArtifactOps()
	var written []string
	write := artifacts.writeFile
	artifacts.writeFile = func(dir, content string) (string, error) {
		written = append(written, content)
		return write(dir, content)
	}
	if code, err := runWithCompositionArtifacts(cfg, runtime.Runtime{Name: shim}, spec, artifacts); err != nil || code != 0 {
		t.Fatalf("offline Run = %d, %v", code, err)
	}
	sawLocal := false
	for _, content := range written {
		if strings.Contains(content, "remote.example") || strings.Contains(content, "headers.example") {
			t.Fatalf("a remote server reached an offline projection:\n%s", content)
		}
		sawLocal = sawLocal || strings.Contains(content, `"local"`)
	}
	if !sawLocal {
		t.Fatal("the local server was left out too")
	}
	if env := string(mustReadFile(t, boxEnv)); strings.Contains(env, "secret") || !strings.Contains(env, "KEPT=kept") {
		t.Fatalf("offline box env = %q", env)
	}
	// Not only the env file: no argument the box is started with carries a secret either.
	if args := string(mustReadFile(t, calls)); strings.Contains(args, "secret") {
		t.Fatalf("a secret reached the box's arguments: %s", args)
	}
	spec.ExtraArgs = []string{"-e", "REMOTE_TOKEN=smuggled"}
	if _, err := runWithCompositionArtifacts(cfg, runtime.Runtime{Name: shim}, spec, defaultCompositionArtifactOps()); err == nil ||
		!strings.Contains(err.Error(), "cannot enter an agent box through -e") {
		t.Fatalf("offline Run with the token through -e = %v", err)
	}
}

// The launch says which remote servers an offline box goes without, so they do not vanish silently
// — from the run itself, not only from the section that renders the line.
func TestOfflineLaunchNamesTheMCPServersItLeavesOut(t *testing.T) {
	s := &launchSections{on: true}
	if got := captureStderr(t, func() { s.offlineMCP([]string{"docs", "tickets"}) }); got != "  MCP servers that need internet are left out: docs, tickets\n" {
		t.Fatalf("offline narration = %q", got)
	}
	if got := captureStderr(t, func() { s.offlineMCP(nil) }); got != "" {
		t.Fatalf("nothing left out, but the launch said %q", got)
	}
	configDir := t.TempDir()
	mcpFile := filepath.Join(configDir, "mcp.json")
	writeRepoFile(t, mcpFile, `{"mcpServers":{
		"local":{"command":"true"},
		"tickets":{"type":"http","url":"https://tickets.example/mcp"},
		"docs":{"type":"http","url":"https://docs.example/mcp","bearer_token_env_var":"DOCS_TOKEN"}}}`)
	writeRepoFile(t, filepath.Join(configDir, "env"), "DOCS_TOKEN=docs-secret\n")
	cfg := &config.Config{ConfigDir: configDir, HomeInBox: "/home/node", MCPFile: mcpFile, MCPInBox: "/home/node/.mcp.json", Egress: "none"}
	shim := filepath.Join(t.TempDir(), "rt")
	writeRepoFile(t, shim, "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(shim, 0o755); err != nil {
		t.Fatal(err)
	}
	claude, _ := agents.Get("claude")
	spec := RunSpec{Image: "i", Repo: t.TempDir(), Cmd: claude.Interactive(cfg), Agent: "claude", AgentCommand: true, Homes: true,
		LoopPresentation: true}
	out := captureStderr(t, func() {
		if code, err := runWithCompositionArtifacts(cfg, runtime.Runtime{Name: shim}, spec, defaultCompositionArtifactOps()); err != nil || code != 0 {
			t.Fatalf("offline Run = %d, %v", code, err)
		}
	})
	if !strings.Contains(out, "MCP servers that need internet are left out: docs, tickets") {
		t.Fatalf("the offline launch did not name what it left out:\n%s", out)
	}
}

// A header secret plans a route of its own: the gateway gets the header lower-cased with its prefix,
// and the box's snapshot keeps the key as written with the stand-in behind the prefix — an
// Authorization header written out is rewritten there too, not taken for a bearer_token_env_var. A
// URL no fixed route can carry is refused by name.
func TestPlanMCPRoutesCarriesAHeaderSecret(t *testing.T) {
	cfg, spec := brokerFixture(t, "DOCS_KEY=docs-secret\nTICKETS_TOKEN=tickets-secret\nWIKI_TOKEN=wiki-secret\n")
	snapshot := []byte(`{"mcpServers":{
		"docs":{"type":"http","url":"https://docs.example/mcp","headers":{"X-Api-Key":"key ${DOCS_KEY}","X-Client":"coop"}},
		"tickets":{"type":"http","url":"https://tickets.example/mcp","headers":{"Authorization":"Token ${TICKETS_TOKEN}"}},
		"wiki":{"type":"http","url":"https://wiki.example/mcp","headers":{"Authorization":"Bearer ${WIKI_TOKEN}"}}}}`)
	plan, err := planMCPRoutes(cfg, spec, snapshot, nil)
	if err != nil || len(plan.mcp) != 3 {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	routes := plan.gatewayRoutes()
	if routes[0].Header != "x-api-key" || routes[0].HeaderPrefix != "key " || routes[1].Header != "authorization" || routes[1].HeaderPrefix != "Token " {
		t.Fatalf("header routes = %+v", routes)
	}
	routed, err := mcp.RouteThroughBroker(snapshot, plan.brokeredServers(networkgateway.CredentialBrokerAddress))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"X-Api-Key":"key ${COOP_MCP_TOKEN_0}"`, `"Authorization":"Token ${COOP_MCP_TOKEN_1}"`,
		`"Authorization":"Bearer ${COOP_MCP_TOKEN_2}"`, `"X-Client":"coop"`} {
		if !strings.Contains(string(routed), want) {
			t.Errorf("routed snapshot lacks %s:\n%s", want, routed)
		}
	}
	for name, url := range map[string]string{"plain http": "http://docs.example/mcp", "another port": "https://docs.example:8443/mcp"} {
		bad := []byte(`{"mcpServers":{"docs":{"type":"http","url":"` + url + `","headers":{"X-Api-Key":"${DOCS_KEY}"}}}}`)
		if _, err := planMCPRoutes(cfg, spec, bad, nil); err == nil || !strings.Contains(err.Error(), `"docs"`) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A header the gateway would reject is refused HERE, by server name and header: planning it
	// would fail the whole launch configuration later with nothing a person could act on.
	for name, definition := range map[string]string{
		"a header the transport owns": `{"Cookie":"session=${DOCS_KEY}"}`,
		"a header the proxy appends":  `{"X-Forwarded-For":"${DOCS_KEY}"}`,
		"a name no header may carry":  `{"X_Api_Key":"${DOCS_KEY}"}`,
		"a prefix past the bound":     `{"X-Api-Key":"` + strings.Repeat("k", 65) + `${DOCS_KEY}"}`,
	} {
		bad := []byte(`{"mcpServers":{"docs":{"type":"http","url":"https://docs.example/mcp","headers":` + definition + `}}}`)
		if _, err := planMCPRoutes(cfg, spec, bad, nil); err == nil || !strings.Contains(err.Error(), `"docs"`) ||
			!strings.Contains(err.Error(), "header") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
