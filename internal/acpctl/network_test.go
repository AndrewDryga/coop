package acpctl

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
)

func TestACPNetworkScopeFencesSelectorsAndSpawns(t *testing.T) {
	c := newTestControl(t)
	c.host = withTestAccounts(c.host, map[string][]string{"codex": {"default"}, "gemini": {"default"}, "grok": {"default"}})
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

func TestACPNetworkScopeDoesNotGrowAfterAccountAvailabilityChanges(t *testing.T) {
	c := newTestControl(t)
	c.LimitNetworkTargets([]agents.Target{{Provider: "claude", Accounts: []string{"personal"}}})
	c.host = withTestAccounts(c.host, map[string][]string{"codex": {"default"}})
	if got := c.SpawnableProviders("claude"); len(got) != 0 {
		t.Fatalf("new sign-in widened frozen provider scope: %v", got)
	}
}

func TestACPNetworkScopeFiltersAccountsWithinAProvider(t *testing.T) {
	c := newTestControlWithHost(t, withTestAccounts(testHost(), map[string][]string{
		"claude": {"personal", "work"}, "gemini": {"portable", "oauth"},
	}))
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
	c.host = withTestAccounts(c.host, map[string][]string{"codex": {"default"}})
	c.host = withTestLadderReplies(t, c.host,
		testLadderReply{provider: "claude", rungs: []agents.Target{{Provider: "claude"}}, targets: []agents.Target{{Provider: "claude", Accounts: []string{"personal"}}, {Provider: "claude", Accounts: []string{"work"}}}},
		testLadderReply{provider: "claude", rungs: []agents.Target{{Provider: "codex"}}, targets: []agents.Target{{Provider: "codex", Accounts: []string{"default"}}}},
		testLadderReply{provider: "claude", rungs: []agents.Target{{Provider: "gemini"}}, err: fmt.Errorf("no Gemini account available")},
	)
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
	c.host = withTestAccounts(c.host, map[string][]string{"codex": {"default"}, "gemini": {"default", "portable"}})
	c.host = withTestLadderReplies(t, c.host,
		testLadderReply{provider: "codex", rungs: []agents.Target{{Provider: "codex"}}, targets: []agents.Target{{Provider: "codex", Accounts: []string{"default"}}}},
		testLadderReply{provider: "codex", rungs: []agents.Target{{Provider: "gemini", Accounts: []string{"default"}}}, targets: []agents.Target{{Provider: "gemini", Accounts: []string{"default"}}}},
	)
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
