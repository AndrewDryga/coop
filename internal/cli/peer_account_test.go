package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/acpctl"
	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
)

func TestACPSameProviderPeerUsesSelectedAccount(t *testing.T) {
	for _, presetLaunch := range []bool{false, true} {
		t.Run(map[bool]string{false: "target", true: "preset"}[presetLaunch], func(t *testing.T) {
			cfg := &config.Config{ConfigDir: t.TempDir(), RepoOverride: t.TempDir()}
			signInCred(t, cfg, "claude", "personal")
			who := "claude:opus@personal"
			if presetLaunch {
				who = "audit"
				dir := filepath.Join(cfg.RepoOverride, ".agent", "presets", who)
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "preset.yaml"), []byte("lead:\n  agent: [claude:opus@personal]\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			called := false
			a := &app{cfg: cfg, acpSupervise: func(_ []string, ctrl *acpctl.Control) (int, error) {
				called = true
				target, _, ok := ctrl.SpawnTarget()
				if !ok || target.Account() != "personal" {
					t.Fatalf("spawn = %v, %v", target, ok)
				}
				return 0, nil
			}}
			if code, err := a.cmdACP([]string{who, "--peer", "claude:haiku"}); code != 0 || err != nil || !called {
				t.Fatalf("named-account peer = (%d, %v), supervised=%v", code, err, called)
			}
			if cfg.ActiveProfile("claude") != "default" {
				t.Fatal("supervisor mutated default account selection")
			}
		})
	}
}

func TestLoopSameProviderPeerUsesSelectedAccount(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir(), RepoOverride: t.TempDir()}
	signInCred(t, cfg, "claude", "personal")
	if err := os.MkdirAll(filepath.Join(cfg.RepoOverride, ".agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.RepoOverride, ".agent", "project.yaml"), []byte("peer_validation_sentinel: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: cfg}
	_, err := a.cmdLoop([]string{"claude:opus@personal", "--peer", "claude:haiku"})
	// A deterministic later prerequisite proves validation without detecting a host runtime.
	if err == nil || !strings.Contains(err.Error(), "peer_validation_sentinel") {
		t.Fatalf("named-account loop did not pass peer validation: %v", err)
	}
}

func TestSameProviderPeerChecksEveryLeadAccount(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	signInCred(t, cfg, "claude", "personal")
	signInCred(t, cfg, "codex", "work")
	a := &app{cfg: cfg}
	leads := []agents.Target{{Provider: "claude", Accounts: []string{"personal"}}}
	if _, err := a.resolvePeersForLeads("coop loop", []string{"claude:haiku"}, leads); err != nil {
		t.Fatal(err)
	}
	leads = append(leads, agents.Target{Provider: "codex", Accounts: []string{"work"}})
	if _, err := a.resolvePeersForLeads("coop loop", []string{"claude:haiku"}, leads); err == nil {
		t.Fatal("cross-provider rung borrowed the other rung's Claude account")
	}
	if cfg.ActiveProfile("claude") != "default" || cfg.ActiveProfile("codex") != "default" {
		t.Fatal("peer validation mutated supervisor selections")
	}
}

func TestACPFilteredSameProviderPeerBindsLeadAccount(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir(), RepoOverride: t.TempDir()}
	signInCred(t, cfg, "claude", "personal")
	signInCred(t, cfg, "claude", "other")
	a := &app{cfg: cfg, acpPeers: []agents.Target{{Provider: "claude", Model: "haiku"}}}
	for _, account := range []string{"personal", "other"} {
		lead := agents.Target{Provider: "claude", Model: "opus", Accounts: []string{account}}
		targets, bindings, err := a.acpFilteredSpawnScope(lead, "")
		if err != nil || len(targets) != 2 || len(bindings) != 1 || bindings["claude"].Account != account {
			t.Fatalf("spawn account %s = (%v, %v, %v)", account, targets, bindings, err)
		}
		for _, target := range targets {
			if target.Account() != account {
				t.Fatalf("peer uses wrong account: %v", target)
			}
		}
		scope, err := a.acpNetworkScope(cfg.RepoOverride, lead, a.acpPeers, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, target := range scope {
			if target.Provider == "claude" && target.Account() == "default" {
				t.Fatalf("admitted unsigned default: %v", scope)
			}
		}
	}
}

func TestACPNetworkScopeSkipsOptionalLeadMissingPeerAccount(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir(), RepoOverride: t.TempDir()}
	signInCred(t, cfg, "claude", "personal")
	signInCred(t, cfg, "codex", "work")
	a := &app{cfg: cfg}
	lead := agents.Target{Provider: "claude", Accounts: []string{"personal"}}
	scope, err := a.acpNetworkScope(cfg.RepoOverride, lead, []agents.Target{{Provider: "claude"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range scope {
		if target.Provider == "codex" {
			t.Fatal("optional provider would require unsigned Claude peer default")
		}
	}
	signInCred(t, cfg, "claude", "default")
	scope, err = a.acpNetworkScope(cfg.RepoOverride, lead, []agents.Target{{Provider: "claude"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, target := range scope {
		found = found || target.Provider == "codex"
	}
	if !found {
		t.Fatal("complete optional provider closure was omitted")
	}
}

func TestACPNetworkScopeSkipsOptionalLeadWithIneligiblePeerAccount(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir(), RepoOverride: t.TempDir()}
	// Unlike canonical portable fixtures, this legacy opaque account cannot be brokered.
	dir := cfg.AgentProfileDir("gemini", "default")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"settings.json":           `{"security":{"auth":{"selectedType":"oauth-personal"}}}`,
		"gemini-credentials.json": `{"encrypted":"host-bound"}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	signInCred(t, cfg, "codex", "default")
	ag, _ := agents.Get("gemini")
	if err := box.SaveHostCredential(cfg, ag, "portable", []byte("fixture-key")); err != nil {
		t.Fatal(err)
	}
	if !box.ProfileAuthed(cfg, "gemini", "default") {
		t.Fatal("fixture lacks ordinary peer account")
	}
	if _, err := box.NetworkTargetBundle(cfg, agents.Target{Provider: "gemini", Accounts: []string{"default"}}, egress.ClientACP); err == nil {
		t.Fatal("fixture peer must be authenticated but network-ineligible")
	}
	a := &app{cfg: cfg}
	lead := agents.Target{Provider: "gemini", Accounts: []string{"portable"}}
	scope, err := a.acpNetworkScope(cfg.RepoOverride, lead, []agents.Target{{Provider: "gemini"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range scope {
		if target.Provider == "codex" {
			t.Fatal("offered optional lead with ineligible peer account")
		}
	}
}
