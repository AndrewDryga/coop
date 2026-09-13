package acpctl

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
)

func TestACPNetworkScopeFencesSelectorsAndSpawns(t *testing.T) {
	c := newTestControl(t)
	for _, provider := range []string{"codex", "gemini", "grok"} {
		signInCred(t, c.cfg, provider, "default")
	}
	if got := c.SpawnableProviders("claude"); len(got) != 3 {
		t.Fatalf("open ACP lost providers: %v", got)
	}
	c.LimitNetworkTargets([]agents.Target{{Provider: "claude", Accounts: []string{"personal"}}, {Provider: "codex", Accounts: []string{"default"}}})
	if got := c.SpawnableProviders("claude"); !slices.Equal(got, []string{"codex"}) {
		t.Fatalf("selector/warm pool escaped admitted providers: %v", got)
	}
	for _, provider := range []string{"gemini", "grok"} {
		if next, known := c.SelectorSelection(CoopProviderID, provider); !known || next != (Selection{}) {
			t.Fatalf("saved editor setting selected %s outside the capture: %+v", provider, next)
		}
		if err := c.ValidateNetworkTarget(agents.Target{Provider: provider}, ""); err == nil {
			t.Fatalf("restored/warm %s target was allowed", provider)
		}
	}
	if next, _ := c.SelectorSelection(CoopProviderID, "codex"); next.Provider != "codex" {
		t.Fatalf("admitted provider cannot be selected: %+v", next)
	}
}

func TestACPNetworkScopeDoesNotGrowAfterSignIn(t *testing.T) {
	c := newTestControl(t)
	c.LimitNetworkTargets([]agents.Target{{Provider: "claude", Accounts: []string{"personal"}}})
	signInCred(t, c.cfg, "codex", "default")
	if got := c.SpawnableProviders("claude"); len(got) != 0 {
		t.Fatalf("new sign-in widened frozen provider scope: %v", got)
	}
}

func TestACPNetworkScopeFiltersAccountsWithinAProvider(t *testing.T) {
	c := newTestControlWithHost(t, Host{
		ExpandLadder: testExpandLadder,
		AccountsFor: func(cfg *config.Config, provider string) []string {
			if provider == "gemini" {
				return []string{"portable", "oauth"}
			}
			return testAccountsFor(cfg, provider)
		},
		WriteModelsCache: func(*config.Config, string, []agents.Model) error { return nil },
	})
	c.LimitNetworkTargets([]agents.Target{{Provider: "claude", Accounts: []string{"personal"}}, {Provider: "gemini", Accounts: []string{"portable"}}})
	if got := c.networkAccountsFor("gemini"); !slices.Equal(got, []string{"portable"}) {
		t.Fatalf("filtered account selector = %v, want portable only", got)
	}
	if err := c.ValidateNetworkTarget(agents.Target{Provider: "gemini", Accounts: []string{"oauth"}}, ""); err == nil {
		t.Fatal("same-provider host-bound account escaped the frozen scope")
	}
}

func TestACPNetworkScopeRevalidatesWholePreset(t *testing.T) {
	c := newTestControl(t)
	signInCred(t, c.cfg, "codex", "default")
	c.LimitNetworkTargets([]agents.Target{
		{Provider: "claude", Accounts: []string{"personal", "work"}},
		{Provider: "codex", Accounts: []string{"default"}},
	})
	path := filepath.Join(c.repo, ".agent", "presets", "frontier", "preset.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("lead:\n  agent: [claude, codex]\n")
	if next, _ := c.SelectorSelection(CoopPresetID, "frontier"); next.Preset != "frontier" {
		t.Fatalf("supported preset was refused: %+v", next)
	}
	for _, body := range []string{
		"lead:\n  agent: [claude, gemini]\n",
		"lead:\n  agent: claude\nroles:\n  critic:\n    mode: consult\n    agent: grok\n",
	} {
		write(body)
		if next, _ := c.SelectorSelection(CoopPresetID, "frontier"); next != (Selection{}) {
			t.Fatalf("changed preset silently lost its unsupported rung/role: %+v", next)
		}
		if err := c.ValidateNetworkTarget(agents.Target{Provider: "claude"}, "frontier"); err == nil {
			t.Fatal("preset changed after selection escaped spawn validation")
		}
	}
}

func TestACPNetworkScopeRefusesPresetAccountBorrowing(t *testing.T) {
	c := newTestControl(t)
	signInCred(t, c.cfg, "codex", "default")
	signInCred(t, c.cfg, "gemini", "portable")
	oauth := c.cfg.AgentProfileDir("gemini", "default")
	if err := os.MkdirAll(oauth, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oauth, "settings.json"), []byte(`{"security":{"auth":{"selectedType":"oauth-personal"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oauth, "gemini-credentials.json"), []byte(`{"encrypted":"host-bound"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c.LimitNetworkTargets([]agents.Target{
		{Provider: "claude", Accounts: []string{"personal", "work"}},
		{Provider: "codex", Accounts: []string{"default"}},
		{Provider: "gemini", Accounts: []string{"portable"}},
	})
	path := filepath.Join(c.repo, ".agent", "presets", "frontier", "preset.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		"lead:\n  agent: [codex, gemini@default]\n",
		"lead:\n  agent: codex\nroles:\n  critic:\n    mode: consult\n    agent: gemini\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := c.ValidateNetworkTarget(agents.Target{Provider: "codex", Accounts: []string{"default"}}, "frontier"); err == nil {
			t.Fatal("portable Gemini sibling qualified a preset that mounts the OAuth default")
		}
	}
}
