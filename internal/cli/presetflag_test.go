package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/preset"
)

func cliFrontier() *preset.Preset {
	return &preset.Preset{
		Name: "frontier", LeadAgent: "claude",
		LeadLadder: []agents.Target{{Provider: "claude", Model: "claude-fable-5", Accounts: []string{"work"}}},
		Roles: []preset.Role{
			{Name: "critic", Mode: preset.ModeConsult, Agent: "codex", Model: "gpt-5.5"},
			{Name: "fast", Mode: preset.ModeDelegate, Agent: "gemini", Model: "gemini-3.5-flash"},
			{Name: "thinker", Mode: preset.ModeNative, Agent: "claude", Model: "claude-opus-4-8", Subagent: "deep-reasoner"},
		},
	}
}

// presetLeadAgent: an explicitly named agent wins; otherwise the preset's lead fills in.
func TestPresetLeadAgent(t *testing.T) {
	p := cliFrontier()
	if got := presetLeadAgent(p, "claude", false); got != "claude" {
		t.Errorf("defaulted agent under preset = %q, want the preset lead claude", got)
	}
	if got := presetLeadAgent(p, "gemini", true); got != "gemini" {
		t.Errorf("explicit agent = %q, want gemini (explicit wins over the preset lead)", got)
	}
	if got := presetLeadAgent(nil, "codex", false); got != "codex" {
		t.Errorf("no preset = %q, want the given default codex", got)
	}
}

// fusionLadderGuard: a cross-provider lead ladder is rejected only when it would drive this
// run's governor — fusion runs one governor for the whole council. Inert ladders (another
// governor, no preset) and single-provider ladders pass.
func TestFusionLadderGuard(t *testing.T) {
	cross := &preset.Preset{
		Name: "x", LeadAgent: "claude",
		LeadLadder: []agents.Target{{Provider: "claude", Model: "opus"}, {Provider: "codex", Model: "gpt-5.5"}},
	}
	if err := fusionLadderGuard(cross, "claude"); err == nil || !strings.Contains(err.Error(), "cross-provider") {
		t.Errorf("cross-provider ladder driving the governor: err = %v, want the cross-provider rejection", err)
	}
	if err := fusionLadderGuard(cross, "gemini"); err != nil {
		t.Errorf("another governor (ladder inert): err = %v, want nil", err)
	}
	if err := fusionLadderGuard(cliFrontier(), "claude"); err != nil {
		t.Errorf("single-provider ladder: err = %v, want nil", err)
	}
	if err := fusionLadderGuard(nil, "claude"); err != nil {
		t.Errorf("no preset: err = %v, want nil", err)
	}
}

// acpLadder filters a cross-provider ladder to the LEAD's own rungs (ACP's respawn env can't
// swap providers); a same-provider ladder passes through whole.
func TestACPLadder(t *testing.T) {
	cross := &preset.Preset{
		Name: "x", LeadAgent: "claude",
		LeadLadder: []agents.Target{
			{Provider: "claude", Model: "fable"},
			{Provider: "codex", Model: "gpt-5.5"},
			{Provider: "claude", Model: "opus", Accounts: []string{"work"}},
		},
	}
	got := acpLadder(cross)
	if len(got) != 2 || got[0].String() != "claude:fable" || got[1].String() != "claude:opus@work" {
		t.Errorf("acpLadder(cross) = %v, want the two claude rungs only", got)
	}
	same := cliFrontier()
	if got := acpLadder(same); len(got) != len(same.LeadLadder) {
		t.Errorf("acpLadder(same-provider) = %v, want the whole ladder", got)
	}
}

// applyPreset seeds role + lead selections; explicit CLI flags applied after still win;
// and a native role must never clobber the lead's own model.
func TestApplyPresetPrecedence(t *testing.T) {
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	p := cliFrontier()
	a.applyPreset(p, "claude")

	// Lead gets the preset lead model; the native thinker (same agent) must not clobber it.
	if got := a.cfg.ModelFor("claude"); got != "claude-fable-5" {
		t.Errorf("lead model = %q, want claude-fable-5 (a native role must not clobber the lead)", got)
	}
	// Consult/delegate roles pin their agents' models and credentials.
	if got := a.cfg.ModelFor("codex"); got != "gpt-5.5" {
		t.Errorf("critic model = %q, want gpt-5.5", got)
	}
	if got := a.cfg.ModelFor("gemini"); got != "gemini-3.5-flash" {
		t.Errorf("fast model = %q, want gemini-3.5-flash", got)
	}
	if got := a.cfg.ActiveProfile("codex"); got != "default" {
		t.Errorf("critic credential = %q, want default (roles run on the agent's default account)", got)
	}
	if got := a.cfg.ActiveProfile("claude"); got != "work" {
		t.Errorf("lead credential = %q, want work", got)
	}
	if a.preset != p {
		t.Error("applyPreset must remember the preset for the RunSpecs")
	}

	// An explicit CLI --model applied after (the caller's order) wins over the preset.
	a.selectRunModel("claude", "claude-opus-4-8")
	if got := a.cfg.ModelFor("claude"); got != "claude-opus-4-8" {
		t.Errorf("explicit model = %q, want it to beat the preset's", got)
	}

	// A different effective lead: the preset's lead model/credentials must NOT transfer
	// (claude's model id pinned onto gemini would be nonsense), and a role that happens
	// to share the lead's agent must not pollute the lead's own selection either — the
	// fast role still carries its model via the delegate wrapper env, not via the lead.
	b := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	b.applyPreset(cliFrontier(), "gemini")
	if got := b.cfg.ModelFor("gemini"); got != "" {
		t.Errorf("gemini lead model = %q, want \"\" (neither the claude lead's nor the fast role's)", got)
	}
	if got := b.cfg.ActiveProfile("gemini"); got != "default" {
		t.Errorf("gemini lead credential = %q, want default (the claude lead's 'work' must not transfer)", got)
	}
	if got := b.cfg.ModelFor("codex"); got != "gpt-5.5" {
		t.Errorf("roles on OTHER agents still apply: codex = %q, want gpt-5.5", got)
	}
}

// --preset parses on the run surfaces (`--`-aware) and a fork accepts it plus the
// --credential alias; the detached re-exec carries the preset forward.
func TestPresetFlagParsing(t *testing.T) {
	name, rest, err := extractRunPreset([]string{"--preset", "frontier", "-p", "hi"})
	if err != nil || name != "frontier" || strings.Join(rest, " ") != "-p hi" {
		t.Errorf("extractRunPreset = (%q, %v, %v)", name, rest, err)
	}
	if name, rest, _ := extractRunPreset([]string{"--", "--preset", "x"}); name != "" || len(rest) != 3 {
		t.Errorf("--preset after -- must pass through: (%q, %v)", name, rest)
	}
	if _, _, err := extractRunPreset([]string{"--preset"}); err == nil {
		t.Error("a bare --preset must error")
	}

	fa, err := parseForkCreate([]string{"api", "claude@work", "--loop", "--preset", "frontier"})
	if err != nil {
		t.Fatal(err)
	}
	if fa.preset != "frontier" || fa.credential != "work" || fa.agent != "claude" {
		t.Errorf("fork parse: agent=%q preset=%q credential=%q", fa.agent, fa.preset, fa.credential)
	}
	if _, err := parseForkCreate([]string{"api", "--loop", "--preset"}); err == nil {
		t.Error("fork: a bare --preset must error")
	}
}

// loadRunPreset resolves the repo and loads + validates the preset from
// .agent/presets/<name>/, so a typo fails loud before any box or worker starts.
func TestLoadRunPreset(t *testing.T) {
	repo := t.TempDir()
	dir := filepath.Join(repo, ".agent", "presets", "frontier")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "preset.yaml"), []byte("lead: {agent: claude}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo, ConfigDir: t.TempDir()}}
	p, err := a.loadRunPreset("frontier")
	if err != nil || p == nil || p.LeadAgent != "claude" {
		t.Fatalf("loadRunPreset = (%+v, %v)", p, err)
	}
	if p, err := a.loadRunPreset(""); p != nil || err != nil {
		t.Errorf("empty name = (%v, %v), want (nil, nil)", p, err)
	}
	if _, err := a.loadRunPreset("ghost"); err == nil || !strings.Contains(err.Error(), "no preset") {
		t.Errorf("missing preset should fail loud: %v", err)
	}
}
