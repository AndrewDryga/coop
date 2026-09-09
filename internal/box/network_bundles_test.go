package box

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
)

// Core dependencies follow the credential scope this run actually mounts, not
// every installed provider. A raw run mounts none and therefore gets none.
func TestNetworkProviderBundlesFollowTheMountedCredentialScope(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node"}
	repo := t.TempDir()
	raw, err := NetworkProviderBundles(cfg, RunSpec{Repo: repo})
	if err != nil || len(raw) != 0 {
		t.Fatal("a raw run derived provider endpoints", raw, err)
	}
	if _, err := NetworkProviderBundles(cfg, RunSpec{Repo: repo, Agent: "claude"}); err != nil {
		t.Fatal("homes-off run derived endpoints", err)
	}
	bundles, err := NetworkProviderBundles(cfg, RunSpec{Repo: repo, Agent: "claude", Homes: true})
	if err != nil || len(bundles) != 1 || bundles[0].Provider != "claude" || bundles[0].Client != egress.ClientCLI {
		t.Fatal("selected provider lost its core endpoints", bundles, err)
	}
	domains := map[string]bool{}
	for _, rule := range bundles[0].Core {
		domains[rule.To.Domain] = true
	}
	if !domains["api.anthropic.com"] {
		t.Fatal("core provider API endpoint missing", bundles[0].Core)
	}
	// An explicitly named peer mounts its credentials, so it needs its own
	// endpoints too — and an unsupported provider fails the whole admission
	// rather than launching under a policy that cannot reach it.
	peered, err := NetworkProviderBundles(cfg, RunSpec{Repo: repo, Agent: "claude", Homes: true,
		Peers: []agents.Target{{Provider: "codex"}}})
	if err != nil || len(peered) != 2 {
		t.Fatal("named peer lost its core endpoints", peered, err)
	}
	if _, err := NetworkProviderBundles(cfg, RunSpec{Repo: repo, Agent: "claude", Homes: true,
		Peers: []agents.Target{{Provider: "gemini"}}}); err == nil || !strings.Contains(err.Error(), "unsupported for restricted networking") {
		t.Fatal("unqualified provider was admitted", err)
	}
	// The client kind rides the spec: an ACP launch is a different variant and
	// must not silently inherit the CLI's captured endpoints.
	acp, err := NetworkProviderBundles(cfg, RunSpec{Repo: repo, Agent: "claude", Homes: true, NetworkClient: egress.ClientACP})
	if err != nil || len(acp) != 1 || acp[0].Client != egress.ClientACP {
		t.Fatal("declared client variant was ignored", acp, err)
	}
}

func mcpDependencyFixture(t *testing.T, snapshot string) (*config.Config, RunSpec) {
	t.Helper()
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node"}
	if snapshot != "" {
		cfg.MCPFile = filepath.Join(t.TempDir(), "mcp.json")
		if err := os.WriteFile(cfg.MCPFile, []byte(snapshot), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return cfg, RunSpec{Repo: t.TempDir(), Agent: "claude", Homes: true}
}

// The operator chooses the servers; their hostnames are derived automatically
// from the trusted shared configuration, and only for HTTP transports.
func TestNetworkMCPDependenciesDeriveTheirHosts(t *testing.T) {
	cfg, spec := mcpDependencyFixture(t, `{"mcpServers":{"local":{"command":"tool"},"remote":{"url":"https://mcp.example.com"}}}`)
	automatic, err := NetworkMCPDependencies(cfg, spec)
	if err != nil || len(automatic) != 1 {
		t.Fatal("HTTP dependency derivation", automatic, err)
	}
	if automatic[0].Origin.Kind != "mcp" || automatic[0].Origin.Name != "remote" ||
		len(automatic[0].Rules) != 1 || automatic[0].Rules[0].To.Domain != "mcp.example.com" {
		t.Fatal("derived dependency is not the exact declared origin", automatic[0])
	}
	if !slices.Equal(automatic[0].Rules[0].Ports, []int{443}) || automatic[0].Rules[0].Protocol != "tls" {
		t.Fatal("derived dependency left the qualified TLS443 subset", automatic[0].Rules[0])
	}
}

func TestNetworkMCPDependenciesAreAbsentWithoutTrustedConfiguration(t *testing.T) {
	cfg, spec := mcpDependencyFixture(t, "")
	automatic, err := NetworkMCPDependencies(cfg, spec)
	if err != nil || automatic != nil {
		t.Fatal("absent MCP configuration derived dependencies", automatic, err)
	}
	// A bare run omits both the configuration and anything derived from it.
	cfg, spec = mcpDependencyFixture(t, `{"mcpServers":{"remote":{"url":"https://mcp.example.com"}}}`)
	spec.Homes = false
	if automatic, err = NetworkMCPDependencies(cfg, spec); err != nil || automatic != nil {
		t.Fatal("a no-tools run inherited MCP dependencies", automatic, err)
	}
}

// A destination coop cannot read literally is refused, not guessed at.
func TestNetworkMCPDependenciesRefuseUnqualifiedRouting(t *testing.T) {
	for _, snapshot := range []string{
		`{"mcpServers":{"remote":{"url":"https://${TENANT}.example.com"}}}`,
		`{"mcpServers":{"remote":{"url":"http://mcp.example.com"}}}`,
		`{"mcpServers":{"remote":{"url":"https://mcp.example.com","headersHelper":"helper"}}}`,
	} {
		cfg, spec := mcpDependencyFixture(t, snapshot)
		if _, err := NetworkMCPDependencies(cfg, spec); err == nil {
			t.Fatal("unqualified MCP routing was admitted:", snapshot)
		}
	}
}
