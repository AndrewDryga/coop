package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/acpctl"
	"github.com/AndrewDryga/coop/internal/acpproxy"
	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/preset"
)

// Every provider is signed in; Gemini's login is host-bound OAuth, which filtered mode refuses by
// design. Grok's carries a refresh token, so its box renews an expired access token itself and it
// is offered like Claude and Codex.
func TestACPNetworkScope(t *testing.T) {
	for _, tc := range []struct {
		name   string
		lead   string
		peers  []agents.Target
		preset *preset.Preset
		refuse bool
	}{
		{name: "unrelated signed-in providers"},
		{name: "explicit unsupported lead", lead: "gemini", refuse: true},
		{name: "explicit unsupported peer", peers: []agents.Target{{Provider: "gemini"}}, refuse: true},
		{name: "entire selected ladder", preset: &preset.Preset{LeadTargets: []agents.Target{{Provider: "codex"}, {Provider: "gemini"}}}, refuse: true},
		{name: "entire selected role", preset: &preset.Preset{LeadTargets: []agents.Target{{Provider: "codex"}}, Roles: []preset.Role{{Mode: preset.ModeConsult, Targets: []agents.Target{{Provider: "gemini"}}}}}, refuse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{ConfigDir: t.TempDir(), RepoOverride: t.TempDir()}
			for _, provider := range agents.Names() {
				signInCred(t, cfg, provider, "default")
			}
			a := &app{cfg: cfg}
			lead := tc.lead
			if lead == "" {
				lead = "codex"
			}
			scope, err := a.acpNetworkScope(cfg.RepoOverride, agents.Target{Provider: lead}, tc.peers, tc.preset)
			if err != nil {
				t.Fatal(err)
			}
			bundles, err := box.NetworkProviderBundles(cfg, box.RunSpec{
				Agent: lead, Homes: true, Peers: scope, Preset: tc.preset, NetworkClient: egress.ClientACP,
			})
			if tc.refuse {
				if err == nil || !strings.Contains(err.Error(), "gemini") {
					t.Fatalf("explicit unsupported scope was pruned: %v, %v", bundles, err)
				}
				return
			}
			if err != nil || len(bundles) != 3 {
				t.Fatalf("supported ACP startup blocked by optional providers: %v, %v", bundles, err)
			}
			for _, bundle := range bundles {
				if bundle.Client != egress.ClientACP || bundle.Provider != "codex" && bundle.Provider != "claude" && bundle.Provider != "grok" {
					t.Fatalf("unexpected ACP bundle: %+v", bundle)
				}
			}
		})
	}
}

func TestACPNetworkScopeExcludesGeminiAPIKeyAccounts(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir(), RepoOverride: t.TempDir()}
	signInCred(t, cfg, "codex", "default")
	gemini, _ := agents.Get("gemini")
	if err := box.SaveHostCredential(cfg, gemini, "portable", []byte("fixture-key")); err != nil {
		t.Fatal(err)
	}
	oauth := cfg.AgentProfileDir("gemini", "oauth")
	if err := os.MkdirAll(oauth, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oauth, "settings.json"), []byte(`{"security":{"auth":{"selectedType":"oauth-personal"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oauth, "gemini-credentials.json"), []byte(`{"encrypted":"host-bound"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: cfg}
	scope, err := a.acpNetworkScope(cfg.RepoOverride, agents.Target{Provider: "codex"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var geminiAccounts []string
	for _, target := range scope {
		if target.Provider == "gemini" {
			geminiAccounts = append(geminiAccounts, target.Account())
		}
	}
	if len(geminiAccounts) != 0 {
		t.Fatalf("filtered ACP offered Gemini API-key accounts: %v", geminiAccounts)
	}
}

func TestACPNetworkScopePreservesPresetRoleAccountIdentity(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir(), RepoOverride: t.TempDir()}
	signInCred(t, cfg, "codex", "default")
	signInCred(t, cfg, "gemini", "portable")
	oauth := cfg.AgentProfileDir("gemini", "default")
	if err := os.MkdirAll(oauth, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oauth, "settings.json"), []byte(`{"security":{"auth":{"selectedType":"oauth-personal"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oauth, "gemini-credentials.json"), []byte(`{"encrypted":"host-bound"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &preset.Preset{LeadTargets: []agents.Target{{Provider: "codex"}}, Roles: []preset.Role{{
		Name: "critic", Mode: preset.ModeConsult, Targets: []agents.Target{{Provider: "gemini"}},
	}}}
	a := &app{cfg: cfg}
	scope, err := a.acpNetworkScope(cfg.RepoOverride, agents.Target{Provider: "codex"}, nil, p)
	if err != nil {
		t.Fatal(err)
	}
	var sawDefault bool
	for _, target := range scope {
		sawDefault = sawDefault || target.Provider == "gemini" && target.Account() == "default"
	}
	if !sawDefault {
		t.Fatalf("preset's concrete OAuth role account was collapsed into a sibling: %v", scope)
	}
	if _, err := box.NetworkProviderBundles(cfg, box.RunSpec{
		Agent: "codex", Homes: true, Peers: scope, Preset: p, NetworkClient: egress.ClientACP,
	}); err == nil || !strings.Contains(err.Error(), "oauth-personal") {
		t.Fatal("portable Gemini sibling qualified the preset's OAuth role account", err)
	}
	// The inner child revalidates this actual scope without the admission-only peers too.
	if _, err := box.NetworkProviderBundles(cfg, box.RunSpec{
		Agent: "codex", Homes: true, Preset: p, NetworkClient: egress.ClientACP,
	}); err == nil || !strings.Contains(err.Error(), "oauth-personal") {
		t.Fatal("filtered inner launch accepted the preset's OAuth role account", err)
	}
}

func TestACPNetworkScopeKeepsResumeForSupervisor(t *testing.T) {
	state := acpctl.ResumeState{
		Proxy: acpproxy.Snapshot{Initialize: []byte(`{"method":"initialize"}`)},
		Ctrl:  acpctl.Snapshot{Lead: "gemini", Target: agents.Target{Provider: "gemini"}},
	}
	path, err := acpctl.WriteResumeState(state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	t.Setenv("COOP_ACP_RESUME_STATE", path)
	cfg := &config.Config{ConfigDir: t.TempDir(), RepoOverride: t.TempDir()}
	signInCred(t, cfg, "codex", "default")
	a := &app{cfg: cfg}
	scope, err := a.acpNetworkScope(cfg.RepoOverride, agents.Target{Provider: "codex"}, nil, nil)
	if err != nil || a.acpResume == nil || a.acpResume.Ctrl.Lead != "gemini" {
		t.Fatalf("admission consumed the supervisor's resume state: %v, %v", a.acpResume, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("resume handoff was not consumed exactly once: %v", err)
	}
	if _, err := box.NetworkProviderBundles(cfg, box.RunSpec{Agent: "codex", Homes: true, Peers: scope, NetworkClient: egress.ClientACP}); err == nil {
		t.Fatal("open Gemini resume could be restored into an incompatible filtered session")
	}
}

func TestACPNetworkScopeIncludesOptionalPresetClosure(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir(), RepoOverride: t.TempDir()}
	signInCred(t, cfg, "codex", "default")
	for name, body := range map[string]string{
		"supported":   "lead:\n  agent: [codex, claude]\n",
		"unsupported": "lead:\n  agent: [codex, gemini]\n",
	} {
		dir := filepath.Join(cfg.RepoOverride, ".agent", "presets", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "preset.yaml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	a := &app{cfg: cfg}
	scope, err := a.acpNetworkScope(cfg.RepoOverride, agents.Target{Provider: "codex"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	providers := map[string]bool{}
	for _, target := range scope {
		providers[target.Provider] = true
	}
	if !providers["codex"] || providers["claude"] || providers["gemini"] {
		t.Fatalf("optional presets did not contribute whole supported closures: %v", providers)
	}
}
