package box

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/gatewayimage"
	"github.com/AndrewDryga/coop/internal/mcp"
	"github.com/AndrewDryga/coop/internal/networkgateway"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// openBrokerSnapshot has one server of every shape an open run meets: two a fixed route carries, and
// three it cannot — an SSE server, a server with its secret in two places, and one that is not plain
// https on 443 — plus a local server and a remote one with no secret at all.
const openBrokerSnapshot = `{"mcpServers":{
	"docs":{"type":"http","url":"https://docs.example/mcp","bearer_token_env_var":"DOCS_TOKEN"},
	"tickets":{"type":"http","url":"https://tickets.example/mcp","headers":{"X-Api-Key":"key ${TICKETS_KEY}"}},
	"stream":{"type":"sse","url":"https://stream.example/sse","bearer_token_env_var":"STREAM_TOKEN"},
	"twice":{"type":"http","url":"https://twice.example/mcp","bearer_token_env_var":"TWICE_TOKEN","headers":{"X-Key":"${TWICE_KEY}"}},
	"inside":{"type":"http","url":"https://inside.example:8443/mcp","bearer_token_env_var":"INSIDE_TOKEN"},
	"public":{"type":"http","url":"https://public.example/mcp"},
	"local":{"command":"true"}}}`

const openBrokerEnv = "DOCS_TOKEN=docs-secret\nTICKETS_KEY=tickets-secret\nSTREAM_TOKEN=stream-secret\n" +
	"TWICE_TOKEN=twice-secret\nTWICE_KEY=twice-key\nINSIDE_TOKEN=inside-secret\nKEPT=kept\n"

func openBrokerFixture(t *testing.T) (*config.Config, RunSpec) {
	t.Helper()
	run := openBrokerFixtureWithEnv(t, openBrokerEnv)
	return run.cfg, run.spec
}

type openBrokerRun struct {
	cfg  *config.Config
	spec RunSpec
}

func openBrokerFixtureWithEnv(t *testing.T, env string) openBrokerRun {
	t.Helper()
	configDir := t.TempDir()
	mcpFile := filepath.Join(configDir, "mcp.json")
	writeRepoFile(t, mcpFile, openBrokerSnapshot)
	writeRepoFile(t, filepath.Join(configDir, "env"), env)
	cfg := &config.Config{ConfigDir: configDir, HomeInBox: "/home/node", MCPFile: mcpFile, MCPInBox: "/home/node/.mcp.json", Egress: "open"}
	claude, _ := agents.Get("claude")
	return openBrokerRun{cfg: cfg, spec: RunSpec{Image: "i", Repo: t.TempDir(), Cmd: claude.Interactive(cfg), Agent: "claude",
		AgentCommand: true, Homes: true, Batch: true, Quiet: true}}
}

// A helper is planned for every secret a fixed route can carry, and only its variables are kept out
// of the box. A server no route can carry keeps today's behaviour — its secret rides in, as it did
// before — and says why, so nothing changes silently.
func TestOpenRunPlansAHelperOnlyForTheSecretsItCanKeepOut(t *testing.T) {
	cfg, spec := openBrokerFixture(t)
	broker, kept, err := planOpenBroker(cfg, runtime.Runtime{Name: "docker"}, spec, []byte(openBrokerSnapshot))
	if err != nil {
		t.Fatal(err)
	}
	if got := broker.servers(); strings.Join(got, ",") != "docs,tickets" {
		t.Fatalf("brokered servers = %v", got)
	}
	if strings.Join(broker.scrub, ",") != "DOCS_TOKEN,TICKETS_KEY" {
		t.Fatalf("scrubbed variables = %v", broker.scrub)
	}
	for _, server := range []string{"stream", "twice", "inside"} {
		if !strings.Contains(strings.Join(kept, "\n"), `"`+server+`"`) {
			t.Errorf("the launch does not say why %q keeps its secret: %v", server, kept)
		}
	}
	if len(kept) != 3 {
		t.Fatalf("kept servers = %v", kept)
	}
	// Two stand-ins, each 64 hex, and a listener per route the box can name before one exists.
	if len(broker.substitutes) != 2 || len(broker.substitutes[0]) != 64 || broker.substitutes[0] == broker.substitutes[1] {
		t.Fatalf("stand-ins = %v", broker.substitutes)
	}
	servers := broker.plan.brokeredServers(openBrokerListener)
	if servers["docs"].URL != "http://coop-broker:15580/mcp" || servers["tickets"].URL != "http://coop-broker:15581/mcp" ||
		servers["tickets"].HeaderKey != "X-Api-Key" || servers["tickets"].Prefix != "key " {
		t.Fatalf("brokered servers = %+v", servers)
	}
	// A run whose box could not reach a helper keeps every secret where it is today, by name.
	for name, unreachable := range map[string]func() (*openBroker, []string, error){
		"another runtime": func() (*openBroker, []string, error) {
			return planOpenBroker(cfg, runtime.Runtime{Name: "container"}, spec, []byte(openBrokerSnapshot))
		},
		"a box on no network": func() (*openBroker, []string, error) {
			without := spec
			without.ExtraArgs = []string{"--network", "none"}
			return planOpenBroker(cfg, runtime.Runtime{Name: "docker"}, without, []byte(openBrokerSnapshot))
		},
	} {
		broker, kept, err := unreachable()
		if broker != nil || err != nil || len(kept) != 5 {
			t.Errorf("%s: %v, %v, %v", name, broker, kept, err)
		}
	}
	// A secret with no value is not this run's to refuse: without a broker that server simply
	// failed to authenticate, and it still does — the run starts, and the launch says so.
	blank := openBrokerFixtureWithEnv(t, "TICKETS_KEY=tickets-secret\n")
	broker, kept, err = planOpenBroker(blank.cfg, runtime.Runtime{Name: "docker"}, blank.spec, []byte(openBrokerSnapshot))
	if err != nil {
		t.Fatalf("an unset MCP token refused the run: %v", err)
	}
	if got := broker.servers(); strings.Join(got, ",") != "tickets" {
		t.Fatalf("brokered servers without DOCS_TOKEN = %v", got)
	}
	if !strings.Contains(strings.Join(kept, "\n"), "DOCS_TOKEN") || slices.Contains(broker.scrub, "DOCS_TOKEN") {
		t.Fatalf("a server with no token was not left as it is: kept=%v scrub=%v", kept, broker.scrub)
	}
	// The hosts entry is how the box finds the helper; an override in the run's own arguments
	// would send its stand-ins somewhere else.
	hijacked := spec
	hijacked.ExtraArgs = []string{"--add-host", "coop-broker:10.0.0.9"}
	if _, _, err := planOpenBroker(cfg, runtime.Runtime{Name: "docker"}, hijacked, []byte(openBrokerSnapshot)); err == nil ||
		!strings.Contains(err.Error(), "remove its --add-host") {
		t.Fatalf("an --add-host for the broker's own name = %v", err)
	}
	// A secret it would broker cannot be smuggled back in through -e.
	smuggled := spec
	smuggled.ExtraArgs = []string{"-e", "DOCS_TOKEN=smuggled"}
	if _, _, err := planOpenBroker(cfg, runtime.Runtime{Name: "docker"}, smuggled, []byte(openBrokerSnapshot)); err == nil ||
		!strings.Contains(err.Error(), "cannot enter an agent box through -e") {
		t.Fatalf("a brokered token through -e = %v", err)
	}
}

// The helper joins the box's own network: the one Coop gives it, else the one its runtime arguments
// name — and the default bridge for a box on the host's network, which reaches it there.
func TestOpenBrokerJoinsTheBoxsNetwork(t *testing.T) {
	for name, test := range map[string]struct {
		joined string
		args   []string
		want   string
	}{
		"the services network": {joined: "coop-repo_default", args: []string{"--network", "other"}, want: "coop-repo_default"},
		"no network at all":    {want: "bridge"},
		"the host's network":   {args: []string{"--network=host"}, want: "bridge"},
		"a named network":      {args: []string{"--net", "lab"}, want: "lab"},
		"the last one named":   {args: []string{"--network", "first", "--network=second"}, want: "second"},
	} {
		if got := openBrokerNetwork(test.joined, test.args); got != test.want {
			t.Errorf("%s: helper joins %q, want %q", name, got, test.want)
		}
	}
}

// An open run starts one helper before its box, points every projection at it, keeps the brokered
// secrets out of the box's environment, and removes the helper and its secrets when the run ends.
func TestOpenRunBrokersItsSecretMCPServers(t *testing.T) {
	cfg, spec := openBrokerFixture(t)
	dir := t.TempDir()
	calls, boxEnv := filepath.Join(dir, "calls"), filepath.Join(dir, "box-env")
	shim := filepath.Join(dir, "docker") // its base name is how the runtime reads as Docker
	// The runtime answers as Docker would: the helper image is present, and the helper itself names
	// the address it listens on and then holds its stdin open, as the real one does.
	writeRepoFile(t, shim, "#!/bin/sh\necho \"$@\" >> "+strconv.Quote(calls)+"\n"+
		"case \"$1 $2\" in \"image inspect\") echo "+gatewayimage.Fingerprint()+"; exit 0 ;; esac\n"+
		"case \"$*\" in *\" broker\") echo 172.18.0.5; cat > /dev/null; exit 0 ;; esac\n"+
		"case \"$1\" in network) exit 1 ;; ps) exit 0 ;; esac\n"+
		"prev=\nfor a in \"$@\"; do [ \"$prev\" = --env-file ] && cat \"$a\" > "+strconv.Quote(boxEnv)+"; prev=$a; done\n")
	if err := os.Chmod(shim, 0o755); err != nil {
		t.Fatal(err)
	}
	artifacts := defaultCompositionArtifactOps()
	var written []string
	write := artifacts.writeFile
	artifacts.writeFile = func(parent, content string) (string, error) {
		written = append(written, content)
		return write(parent, content)
	}
	if code, err := runWithCompositionArtifacts(cfg, runtime.Runtime{Name: shim}, spec, artifacts); err != nil || code != 0 {
		t.Fatalf("open Run = %d, %v", code, err)
	}
	helper, box := "", ""
	for _, line := range strings.Split(strings.TrimSpace(string(mustReadFile(t, calls))), "\n") {
		switch {
		case strings.Contains(line, "--name coop-broker-"):
			helper = line
		case strings.Contains(line, "--label coop=box"):
			box = line
		}
	}
	for _, want := range []string{"run --rm -i ", "--user 65532:65532", "--cap-drop ALL", "--security-opt no-new-privileges", "--read-only",
		"--network bridge", "--label coop=broker", "--label coop.broker=", "--label coop.host=", ",target=" + networkgateway.OpenBrokerConfigPath + ",readonly",
		",target=" + networkgateway.CredentialBrokerPath + ",readonly", gatewayimage.Tag() + " broker"} {
		if !strings.Contains(helper, want) {
			t.Errorf("the helper was started without %q:\n%s", want, helper)
		}
	}
	if !strings.Contains(box, "--add-host=coop-broker:172.18.0.5") {
		t.Fatalf("the box was not given the helper's address:\n%s", box)
	}
	env := string(mustReadFile(t, boxEnv))
	for _, gone := range []string{"docs-secret", "tickets-secret"} {
		if strings.Contains(env, gone) {
			t.Fatalf("a brokered secret entered the box: %q", env)
		}
	}
	for _, want := range []string{"COOP_MCP_TOKEN_0=", "COOP_MCP_TOKEN_1=", "STREAM_TOKEN=stream-secret", "KEPT=kept"} {
		if !strings.Contains(env, want) {
			t.Fatalf("the box environment lacks %q: %q", want, env)
		}
	}
	projections := strings.Join(written, "\n")
	for _, want := range []string{"http://coop-broker:15580/mcp", "http://coop-broker:15581/mcp", "${COOP_MCP_TOKEN_0}", `"key ${COOP_MCP_TOKEN_1}"`,
		"https://stream.example/sse", "https://public.example/mcp"} {
		if !strings.Contains(projections, want) {
			t.Errorf("no projection names %q", want)
		}
	}
	for _, gone := range []string{"docs.example", "tickets.example", "DOCS_TOKEN", "TICKETS_KEY"} {
		if strings.Contains(projections, gone) {
			t.Errorf("a projection still names %q", gone)
		}
	}
	// The secrets the helper read are the host's, and the run takes them with it.
	secrets := ""
	for _, field := range strings.Fields(helper) {
		if source, ok := strings.CutPrefix(field, "type=bind,source="); ok && strings.Contains(field, networkgateway.CredentialBrokerPath) {
			secrets, _, _ = strings.Cut(source, ",")
		}
	}
	if secrets == "" {
		t.Fatalf("the helper got no secrets mount:\n%s", helper)
	}
	if _, err := os.Stat(secrets); !os.IsNotExist(err) {
		t.Fatalf("the helper's secrets outlived the run: %v", err)
	}
}

// A session child hands the daemon what its box holds: the helper's listener and a stand-in for a
// brokered server, and the real value only for a server whose secret the box carries anyway.
func TestAnOpenSessionHandsOverStandInsAndNothingElse(t *testing.T) {
	cfg, spec := openBrokerFixture(t)
	broker, _, err := planOpenBroker(cfg, runtime.Runtime{Name: "docker"}, spec, []byte(openBrokerSnapshot))
	if err != nil {
		t.Fatal(err)
	}
	// Built the way box.Run builds it, so a regression in the real rewrite fails this too.
	snapshot := filepath.Join(t.TempDir(), "mcp.json")
	routed, err := mcp.RouteThroughBroker([]byte(openBrokerSnapshot), broker.plan.brokeredServers(openBrokerListener))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshot, routed, 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "handoff.json")
	child := spec
	child.RunID, child.NetworkClient = "session-run-1", egress.ClientACP
	if err := handOffSessionMCP(child, target, snapshot, broker.mcpStandIns(cfg, spec)); err != nil {
		t.Fatal(err)
	}
	data := string(mustReadFile(t, target))
	for _, want := range []string{"Bearer " + broker.substitutes[0], "coop-broker:15580", "Bearer stream-secret"} {
		if !strings.Contains(data, want) {
			t.Errorf("the handoff lacks %q:\n%s", want, data)
		}
	}
	for _, gone := range []string{"docs-secret", "tickets-secret", "docs.example"} {
		if strings.Contains(data, gone) {
			t.Errorf("the handoff carries %q:\n%s", gone, data)
		}
	}
}
