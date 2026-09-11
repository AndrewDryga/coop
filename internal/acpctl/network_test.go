package acpctl

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
)

func TestACPNetworkScopeFencesSelectorsAndSpawns(t *testing.T) {
	c := newTestControl(t)
	for _, provider := range []string{"codex", "gemini", "grok"} {
		signInCred(t, c.cfg, provider, "default")
	}
	if got := c.SpawnableProviders("claude"); len(got) != 3 {
		t.Fatalf("open ACP lost providers: %v", got)
	}
	c.LimitNetworkProviders([]string{"claude", "codex"})
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
	c.LimitNetworkProviders([]string{"claude"})
	signInCred(t, c.cfg, "codex", "default")
	if got := c.SpawnableProviders("claude"); len(got) != 0 {
		t.Fatalf("new sign-in widened frozen provider scope: %v", got)
	}
}

func TestACPNetworkScopeRevalidatesWholePreset(t *testing.T) {
	c := newTestControl(t)
	c.LimitNetworkProviders([]string{"claude", "codex"})
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
