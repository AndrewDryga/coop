package box

import (
	"encoding/json"
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
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "filtered"}
	seedCanonicalFixture(t, cfg, "claude", "default")
	seedCanonicalFixture(t, cfg, "codex", "default")
	for _, client := range []egress.Client{egress.ClientCLI, egress.ClientACP} {
		for _, peers := range [][]agents.Target{nil, {{Provider: "codex"}}} {
			spec := RunSpec{Repo: t.TempDir(), Agent: "claude", Homes: true, Peers: peers, NetworkClient: client}
			bundles, err := NetworkProviderBundles(cfg, spec)
			if err != nil || len(bundles) != 0 {
				t.Fatal("protected origins entered workload policy", bundles, err)
			}
			protected, err := brokeredProviders(cfg, spec)
			if err != nil || len(protected) != 1+len(peers) || !protected["claude"] || len(peers) > 0 && !protected["codex"] {
				t.Fatal("guard scope lost", protected, err)
			}
			bundle, err := NetworkTargetBundle(cfg, agents.Target{Provider: "claude"}, client)
			if err != nil || bundle.Client != client || bundle.Backend != "native-broker" || len(bundle.Core) != 0 {
				t.Fatal("native descriptor incorrect", bundle, err)
			}
		}
	}
	for _, spec := range []RunSpec{{Repo: t.TempDir()}, {Agent: "claude"}} {
		if bundles, err := NetworkProviderBundles(cfg, spec); err != nil || len(bundles) != 0 {
			t.Fatal("raw/homes-off derived endpoints", bundles, err)
		}
	}
	if _, err := NetworkProviderBundles(cfg, RunSpec{Agent: "claude", Homes: true, Peers: []agents.Target{{Provider: "gemini"}}}); err == nil {
		t.Fatal("missing peer account admitted")
	}
}

func TestNetworkProviderBundlesRefusesMissingRequiredPresetRole(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir(), Egress: "filtered"}
	seedCanonicalFixture(t, cfg, "codex", "default")
	p := &preset.Preset{LeadTargets: []agents.Target{{Provider: "codex"}}, Roles: []preset.Role{{
		Name: "critic", Mode: preset.ModeConsult, Targets: []agents.Target{{Provider: "gemini"}},
	}}}
	if _, err := NetworkProviderBundles(cfg, RunSpec{
		Agent: "codex", Homes: true, Preset: p, NetworkClient: egress.ClientACP,
	}); err == nil || !strings.Contains(err.Error(), `gemini account "default" is not ready`) {
		t.Fatal("missing required role was silently removed from network validation", err)
	}
}

func TestNetworkProviderBundlesAllowLoginWithoutAnExistingCredential(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	bundles, err := NetworkProviderBundles(cfg, RunSpec{Agent: "grok", Homes: true, Login: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(bundles) != 1 || bundles[0].Provider != "grok" || bundles[0].AuthMode != "access-file" {
		t.Fatalf("Grok login bundle = %+v", bundles)
	}
	// A key already in the env file is not the login's credential: the sign-in box gets none of it,
	// so its provider's sign-in endpoints stay granted rather than left to a broker it never runs.
	if err := os.WriteFile(cfg.EnvFile(), []byte("ANTHROPIC_API_KEY=env-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundles, err = NetworkProviderBundles(cfg, RunSpec{Agent: "claude", Homes: true, Login: true})
	if err != nil || len(bundles) != 1 || bundles[0].Provider != "claude" {
		t.Fatalf("Claude login beside an env key = %+v, %v; want its sign-in endpoints granted", bundles, err)
	}
}

// A portable key qualifies exactly one authentication family, for the CLI and — through the
// broker, which keeps the key outside the box — for ACP sessions alike.
func TestNetworkTargetBundleBindsAuthenticationAndOffersAPIKeysToACP(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	gemini, _ := agents.Get("gemini")
	if err := SaveHostCredential(cfg, gemini, "key", []byte("portable-key")); err != nil {
		t.Fatal(err)
	}
	key := agents.Target{Provider: "gemini", Accounts: []string{"key"}}
	bundle, err := NetworkTargetBundle(cfg, key, egress.ClientCLI)
	if err != nil || bundle.Backend != "native-broker" || bundle.AuthMode != "gemini-api-key" || len(bundle.Core) != 0 {
		t.Fatalf("portable Gemini key was not qualified exactly: %+v, %v", bundle, err)
	}
	if acp, err := NetworkTargetBundle(cfg, key, egress.ClientACP); err != nil || acp.AuthMode != "gemini-api-key" {
		t.Fatalf("Gemini API key was not offered to ACP: %+v, %v", acp, err)
	}
	// The client's own key file beside Coop's host key is shadowed by the broker, as in a direct
	// run; a key only that file holds would enter the box.
	if err := os.MkdirAll(cfg.AgentProfileDir("gemini", "key"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.AgentProfileDir("gemini", "key"), "gemini-credentials.json"), []byte(`{"encrypted":"native"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if acp, err := NetworkTargetBundle(cfg, key, egress.ClientACP); err != nil || acp.AuthMode != "gemini-api-key" {
		t.Fatalf("Gemini host key beside a native key file was not offered to ACP: %+v, %v", acp, err)
	}
	importCanonicalFixture(t, cfg, "codex", "native", map[string][]byte{"auth.json": []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"inert-native-key"}`)})
	if bundle, err := NetworkTargetBundle(cfg, agents.Target{Provider: "codex", Accounts: []string{"native"}}, egress.ClientACP); err != nil || bundle.Backend != "native-broker" || bundle.AuthMode != "apikey" || len(bundle.Core) != 0 {
		t.Fatal("canonical Codex key not offered to ACP", bundle, err)
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
	if _, err := NetworkTargetBundle(cfg, agents.Target{Provider: "gemini", Accounts: []string{"oauth"}}, egress.ClientACP); err == nil || !strings.Contains(err.Error(), "encrypted cache") {
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

// A preset role runs its active account, so a policy classifies the role the way its run's plan
// does: a role's key belongs to the broker, never an API granted to the agent that the launch would
// then refuse — whether one run or a whole loop is being admitted.
func TestNetworkProviderBundlesBrokerAPresetRolesKey(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "filtered"}
	if err := os.WriteFile(cfg.EnvFile(), []byte("GEMINI_API_KEY=gemini-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedCanonicalFixture(t, cfg, "codex", "default")

	p := &preset.Preset{Roles: []preset.Role{{Name: "reviewer", Mode: preset.ModeConsult, Targets: []agents.Target{{Provider: "gemini"}}}}}
	for _, admission := range []bool{false, true} {
		spec := RunSpec{Agent: "codex", AgentCommand: true, Homes: true, Preset: p, NetworkAdmission: admission}
		bundles, err := NetworkProviderBundles(cfg, spec)
		if err != nil || len(bundles) != 0 {
			t.Fatalf("admission=%v: bundles = %+v, %v; want protected origins excluded from workload policy", admission, bundles, err)
		}
	}
}

// A filtered box mounts the Grok profile itself, and the pinned client renews its own access token
// through auth.x.ai, which the bundle allows — so a login that carries a refresh token starts however
// little its access token has left, even none. Only a credential with no refresh token, like a
// session's access-only projection, has to outlive the restricted horizon by itself.
func TestNetworkTargetBundleRequiresUsableGrokAuthority(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	seedCanonicalFixture(t, cfg, "grok", "personal")
	ag, _ := agents.Get("grok")
	for _, tc := range []struct {
		name    string
		expiry  time.Duration
		refresh string
		admit   bool
	}{
		{"lasting", 2 * time.Hour, "", true}, {"short", 30 * time.Minute, "", true}, {"expired", -time.Hour, "", false},
		{"renewable", 30 * time.Minute, "refresh", true}, {"expired renewable", -time.Hour, "refresh", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := canonicalFixtureFiles(t, "grok", "personal")
			var doc map[string]map[string]any
			if err := json.Unmarshal(files["auth.json"], &doc); err != nil {
				t.Fatal(err)
			}
			for _, entry := range doc {
				entry["expires_at"] = time.Now().Add(tc.expiry).UTC().Format(time.RFC3339Nano)
				entry["refresh_token"] = tc.refresh
			}
			files["auth.json"] = canonicalFixtureJSON(t, doc)
			state, err := ag.NativeCredentials().Inspect(files, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			// Simulate a published grant aging; fresh sign-in refuses expired access.
			if _, _, err := replaceAccountAuthority(t.Context(), cfg, nativeAccountSpec(ag), "personal", &accountAuthority{Selection: state.Selection, Principal: state.Principal, Artifacts: files}); err != nil {
				t.Fatal(err)
			}
			bundle, err := NetworkTargetBundle(cfg, agents.Target{Provider: "grok", Accounts: []string{"personal"}}, egress.ClientCLI)
			if tc.admit && (err != nil || bundle.Backend != "native-broker" || bundle.AuthMode != "oauth" || len(bundle.Core) != 0) {
				t.Fatal("usable authority refused", bundle, err)
			}
			if !tc.admit && err == nil {
				t.Fatal("expired access-only authority admitted")
			}
		})
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
