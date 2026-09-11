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
			scope, err := a.acpNetworkScope(cfg.RepoOverride, tc.peers, tc.preset)
			if err != nil {
				t.Fatal(err)
			}
			bundles, err := box.NetworkProviderBundles(cfg, box.RunSpec{
				Agent: lead, Homes: true, Peers: scope, Preset: tc.preset, NetworkClient: egress.ClientACP,
			})
			if tc.refuse {
				if err == nil || !strings.Contains(err.Error(), "gemini is unsupported") {
					t.Fatalf("explicit unsupported scope was pruned: %v, %v", bundles, err)
				}
				return
			}
			if err != nil || len(bundles) != 2 {
				t.Fatalf("supported ACP startup blocked by optional providers: %v, %v", bundles, err)
			}
			for _, bundle := range bundles {
				if bundle.Client != egress.ClientACP || bundle.Provider != "codex" && bundle.Provider != "claude" {
					t.Fatalf("unexpected ACP bundle: %+v", bundle)
				}
			}
		})
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
	scope, err := a.acpNetworkScope(cfg.RepoOverride, nil, nil)
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
	scope, err := a.acpNetworkScope(cfg.RepoOverride, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	providers := map[string]bool{}
	for _, target := range scope {
		providers[target.Provider] = true
	}
	if !providers["codex"] || !providers["claude"] || providers["gemini"] {
		t.Fatalf("optional presets did not contribute whole supported closures: %v", providers)
	}
}
