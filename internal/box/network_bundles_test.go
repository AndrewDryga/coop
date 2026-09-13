package box

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/preset"
)

// Core dependencies follow the credential scope this run actually mounts, not
// every installed provider. A raw run mounts none and therefore gets none.
func TestNetworkProviderBundlesFollowTheMountedCredentialScope(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node"}
	if err := os.WriteFile(cfg.EnvFile(), []byte("ANTHROPIC_AUTH_TOKEN=claude-test\nOPENAI_API_KEY=codex-test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
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
	// The API, the OAuth token endpoint and the claude.ai connector proxy: what a signed-in
	// session needs to function, and nothing the client only chats to (see
	// agent.TestProviderBundlesCarryFunctionNotChatter).
	for _, host := range []string{"api.anthropic.com", "platform.claude.com", "mcp-proxy.anthropic.com"} {
		if !domains[host] {
			t.Fatal("core provider endpoint missing:", host, bundles[0].Core)
		}
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
		Peers: []agents.Target{{Provider: "gemini"}}}); err == nil {
		t.Fatal("unqualified provider was admitted", err)
	}
	// The client kind rides the spec: an ACP launch is a different variant and
	// must not silently inherit the CLI's captured endpoints.
	acp, err := NetworkProviderBundles(cfg, RunSpec{Repo: repo, Agent: "claude", Homes: true, NetworkClient: egress.ClientACP})
	if err != nil || len(acp) != 1 || acp[0].Client != egress.ClientACP {
		t.Fatal("declared client variant was ignored", acp, err)
	}
}

func TestNetworkProviderBundlesRefusesMissingRequiredPresetRole(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	if err := os.WriteFile(cfg.EnvFile(), []byte("OPENAI_API_KEY=codex-test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &preset.Preset{LeadTargets: []agents.Target{{Provider: "codex"}}, Roles: []preset.Role{{
		Name: "critic", Mode: preset.ModeConsult, Targets: []agents.Target{{Provider: "gemini"}},
	}}}
	if _, err := NetworkProviderBundles(cfg, RunSpec{
		Agent: "codex", Homes: true, Preset: p, NetworkClient: egress.ClientACP,
	}); err == nil || !strings.Contains(err.Error(), `gemini account "default" is not ready`) {
		t.Fatal("missing required role was silently removed from network validation", err)
	}
}

func TestNetworkTargetBundleBindsPortableAccountAuthentication(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	gemini, _ := agents.Get("gemini")
	if err := SaveHostCredential(cfg, gemini, "key", []byte("portable-key")); err != nil {
		t.Fatal(err)
	}
	key := agents.Target{Provider: "gemini", Accounts: []string{"key"}}
	bundle, err := NetworkTargetBundle(cfg, key, egress.ClientACP)
	if err != nil || bundle.AuthMode != "api-key" || len(bundle.Core) != 1 || bundle.Core[0].To.Domain != "generativelanguage.googleapis.com" {
		t.Fatalf("portable Gemini key was not qualified exactly: %+v, %v", bundle, err)
	}

	oauthDir := cfg.AgentProfileDir("gemini", "oauth")
	if err := os.MkdirAll(oauthDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oauthDir, "settings.json"), []byte(`{"security":{"auth":{"selectedType":"oauth-personal"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oauthDir, "gemini-credentials.json"), []byte(`{"encrypted":"host-bound"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NetworkTargetBundle(cfg, agents.Target{Provider: "gemini", Accounts: []string{"oauth"}}, egress.ClientACP); err == nil || !strings.Contains(err.Error(), "oauth-personal") {
		t.Fatal("host-bound Gemini OAuth was admitted", err)
	}

	if err := os.WriteFile(cfg.EnvFile(), []byte("GEMINI_API_KEY=default-only\nGOOGLE_API_KEY=unqualified-vertex\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	vertexDir := cfg.AgentProfileDir("gemini", "default")
	if err := os.MkdirAll(vertexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vertexDir, "settings.json"), []byte(`{"security":{"auth":{"selectedType":"vertex-ai"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NetworkTargetBundle(cfg, agents.Target{Provider: "gemini", Accounts: []string{"default"}}, egress.ClientACP); err == nil || !strings.Contains(err.Error(), "vertex-ai") {
		t.Fatal("unqualified Vertex mode was admitted", err)
	}
	if _, err := NetworkTargetBundle(cfg, agents.Target{Provider: "gemini", Accounts: []string{"named"}}, egress.ClientACP); err == nil {
		t.Fatal("named Gemini account borrowed the default env key")
	}
}

func TestNetworkTargetBundleRequiresPortableGrokAccessFile(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	dir := cfg.AgentProfileDir("grok", "personal")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(expiry time.Time) {
		t.Helper()
		body := fmt.Sprintf(`{"https://auth.x.ai::client":{"key":"access","expires_at":%q,"auth_mode":"oauth","oidc_issuer":"https://auth.x.ai","oidc_client_id":"client","principal_id":"principal","principal_type":"user","user_id":"user","team_id":"team","create_time":"2026-09-13T00:00:00Z"}}`, expiry.UTC().Format(time.RFC3339Nano))
		if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(time.Now().Add(2 * time.Hour))
	target := agents.Target{Provider: "grok", Accounts: []string{"personal"}}
	bundle, err := NetworkTargetBundle(cfg, target, egress.ClientACP)
	if err != nil || bundle.AuthMode != "access-file" || len(bundle.Core) != 2 {
		t.Fatalf("portable Grok access file was not qualified: %+v, %v", bundle, err)
	}
	write(time.Now().Add(30 * time.Minute))
	if _, err := NetworkTargetBundle(cfg, target, egress.ClientACP); err == nil || !strings.Contains(err.Error(), "no portable credential") {
		t.Fatal("short-lived Grok access file was admitted", err)
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
