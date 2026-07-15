package cli

import (
	"slices"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/preset"
)

func TestResolveFusionCouncil(t *testing.T) {
	peer := func(names ...string) []agents.Target {
		out := make([]agents.Target, len(names))
		for i, name := range names {
			out[i] = agents.Target{Provider: name}
		}
		return out
	}
	consult := func(name, provider string) preset.Role {
		return preset.Role{Name: name, Mode: preset.ModeConsult, Agent: provider}
	}

	t.Run("empty council", func(t *testing.T) {
		if _, err := resolveFusionCouncil("claude", nil, nil, fusionRejectSelf, nil); err == nil || !strings.Contains(err.Error(), "council") {
			t.Fatalf("empty council error = %v, want a council error", err)
		}
	})

	t.Run("terminal rejects self and duplicate providers", func(t *testing.T) {
		if _, err := resolveFusionCouncil("claude", peer("claude"), nil, fusionRejectSelf, nil); err == nil || !strings.Contains(err.Error(), "governor") {
			t.Errorf("self peer error = %v, want governor/self rejection", err)
		}
		if _, err := resolveFusionCouncil("claude", peer("codex", "codex"), nil, fusionRejectSelf, nil); err == nil || !strings.Contains(err.Error(), "more than once") {
			t.Errorf("duplicate peer error = %v, want duplicate rejection", err)
		}
	})

	t.Run("ACP filters only the active self peer", func(t *testing.T) {
		got, err := resolveFusionCouncil("claude", peer("claude", "gemini"), nil, fusionExcludeSelf, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Peers) != 1 || got.Peers[0].Provider != "gemini" || !slices.Equal(got.Members, []string{"gemini"}) {
			t.Errorf("resolved ACP council = %+v, want only gemini", got)
		}
	})

	t.Run("preset roles keep role identity and may share the governor provider", func(t *testing.T) {
		p := &preset.Preset{Roles: []preset.Role{consult("critic", "claude"), consult("skeptic", "claude")}}
		got, err := resolveFusionCouncil("claude", nil, p, fusionRejectSelf, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got.Members, []string{"critic", "skeptic"}) {
			t.Errorf("same-provider role members = %v, want [critic skeptic]", got.Members)
		}
	})

	t.Run("role may share the governor token when no explicit provider collides", func(t *testing.T) {
		p := &preset.Preset{Roles: []preset.Role{consult("claude", "codex")}}
		got, err := resolveFusionCouncil("claude", nil, p, fusionRejectSelf, []string{"codex"})
		if err != nil || !slices.Equal(got.Members, []string{"claude"}) {
			t.Errorf("governor-named role council = (%+v, %v), want role label claude", got, err)
		}
	})

	t.Run("native roles degrade only under a non-Claude governor", func(t *testing.T) {
		p := &preset.Preset{Roles: []preset.Role{{Name: "thinker", Mode: preset.ModeNative, Agent: "claude"}}}
		if _, err := resolveFusionCouncil("claude", nil, p, fusionRejectSelf, nil); err == nil {
			t.Error("a Claude-native role is in-session, not a Fusion council member")
		}
		got, err := resolveFusionCouncil("codex", nil, p, fusionRejectSelf, []string{"claude"})
		if err != nil || !slices.Equal(got.Members, []string{"thinker"}) {
			t.Errorf("degraded native council = (%+v, %v), want thinker", got, err)
		}
	})

	t.Run("role needs one mounted target but fallback may supply it", func(t *testing.T) {
		p := &preset.Preset{Roles: []preset.Role{{Name: "critic", Mode: preset.ModeConsult, Ladder: []agents.Target{
			{Provider: "codex"}, {Provider: "gemini"},
		}}}}
		if _, err := resolveFusionCouncil("claude", nil, p, fusionRejectSelf, nil); err == nil || !strings.Contains(err.Error(), "mounted credentials") {
			t.Errorf("all-unavailable role error = %v, want mounted-credential rejection", err)
		}
		got, err := resolveFusionCouncil("claude", nil, p, fusionRejectSelf, []string{"gemini"})
		if err != nil || !slices.Equal(got.Members, []string{"critic"}) {
			t.Errorf("fallback-available council = (%+v, %v), want critic", got, err)
		}
	})

	t.Run("provider and role invocation collision is ambiguous", func(t *testing.T) {
		p := &preset.Preset{Roles: []preset.Role{consult("codex", "gemini")}}
		if _, err := resolveFusionCouncil("claude", peer("codex"), p, fusionRejectSelf, []string{"gemini"}); err == nil || !strings.Contains(err.Error(), "conflicts") {
			t.Errorf("provider/role collision error = %v, want conflict rejection", err)
		}
	})
}

func TestResolveACPFusionCouncilValidatesEveryPresetProvider(t *testing.T) {
	p := &preset.Preset{
		Name: "rotate", LeadAgent: "claude",
		LeadLadder: []agents.Target{{Provider: "claude", Model: "one"}, {Provider: "codex", Model: "two"}},
	}
	peers := []agents.Target{{Provider: "codex"}}
	reachable := slices.Clone(p.LeadLadder)
	if _, err := resolveACPFusionCouncil("claude", peers, p, nil, reachable); err == nil || !strings.Contains(err.Error(), "codex") {
		t.Fatalf("later empty rung error = %v, want it to name codex", err)
	}

	p.Roles = []preset.Role{{Name: "critic", Mode: preset.ModeConsult, Agent: "gemini"}}
	got, err := resolveACPFusionCouncil("claude", peers, p, []string{"gemini"}, reachable)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Members, []string{"codex", "critic"}) {
		t.Errorf("initial resolved members = %v, want [codex critic]", got.Members)
	}

	// A declared rung whose pinned account is unavailable never reaches the ACP controller and
	// therefore must not invalidate a council that is valid on every concrete rotation target.
	p.Roles = nil
	p.LeadLadder = []agents.Target{
		{Provider: "claude", Model: "one", Accounts: []string{"work"}},
		{Provider: "codex", Model: "two", Accounts: []string{"missing"}},
	}
	cfg := &config.Config{ConfigDir: t.TempDir()}
	signInCred(t, cfg, "claude", "work")
	signInCred(t, cfg, "codex", "work")
	cfg.SetActiveProfile("claude", "work")
	cfg.SetActiveProfile("codex", "work")
	reachable, err = expandLadder(cfg, p.LeadAgent, p.LeadLadder)
	if err != nil || len(reachable) != 1 || reachable[0].Provider != "claude" {
		t.Fatalf("expanded reachable ladder = (%v, %v), want only claude", reachable, err)
	}
	got, err = resolveACPFusionCouncil("claude", peers, p, []string{"codex"}, reachable)
	if err != nil || !slices.Equal(got.Members, []string{"codex"}) {
		t.Errorf("skipped codex rung council = (%+v, %v), want reachable claude + peer codex", got, err)
	}
}
