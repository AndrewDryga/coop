package box

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/preset"
)

func TestAuthedAgents(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{ConfigDir: dir}

	// codex authed via its credential file; gemini via an env-file API key; claude
	// has neither → not authed. A commented or empty key must not count. Creds live in
	// profiles/default (the flat vault is retired).
	os.MkdirAll(filepath.Join(dir, "codex", "profiles", "default"), 0o755)
	os.WriteFile(filepath.Join(dir, "codex", "profiles", "default", "auth.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(dir, "env"), []byte("# OPENAI_API_KEY=ignored\nGEMINI_API_KEY=real\nEMPTY=\n"), 0o644)

	if got := AuthedAgents(cfg); !slices.Equal(got, []string{"codex", "gemini"}) {
		t.Errorf("AuthedAgents = %v, want [codex gemini]", got)
	}
}

func peerTargets(names ...string) []agents.Target {
	ts := make([]agents.Target, len(names))
	for i, n := range names {
		ts[i] = agents.Target{Provider: n}
	}
	return ts
}

// TestCredentialScope: a plain agent run mounts only its own home; a fusion/consult run ALSO
// mounts exactly the EXPLICIT peers it named (spec.Peers) plus a preset's role agents — never a
// blanket "every authed agent". A raw run (no agent) and a homes-off run get nothing.
func TestCredentialScope(t *testing.T) {
	dir := t.TempDir()
	// claude + gemini authed (so they're consultable peers); codex is not. Creds live in the
	// profiles/default layout (the flat vault is retired — migrateFlatVaults moves it there).
	os.MkdirAll(filepath.Join(dir, "claude", "profiles", "default"), 0o755)
	os.WriteFile(filepath.Join(dir, "claude", "profiles", "default", ".credentials.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(dir, "env"), []byte("GEMINI_API_KEY=real\n"), 0o644)
	cfg := &config.Config{ConfigDir: dir}

	// A preset whose only role is a native claude thinker: in-session under a Claude lead
	// (adds nothing to the scope), but degrades to a consult on claude under a codex lead —
	// which then must mount claude's creds.
	nativePreset := &preset.Preset{Roles: []preset.Role{{Name: "thinker", Mode: preset.ModeNative, Agent: "claude"}}}

	cases := []struct {
		name string
		spec RunSpec
		want []string
	}{
		{"plain claude", RunSpec{Homes: true, Agent: "claude"}, []string{"claude"}},
		{"raw run", RunSpec{Homes: true}, nil},
		{"homes off", RunSpec{Agent: "claude"}, nil},
		// Narrowing: a consult with NO named peer mounts the lead alone — gemini is authed but
		// unnamed, so its credentials stay out (the old policy would have widened to it).
		{"consult, no named peer → lead only", RunSpec{Homes: true, Agent: "claude", ConsultLead: "claude"}, []string{"claude"}},
		{"consult names gemini", RunSpec{Homes: true, Agent: "claude", ConsultLead: "claude", Peers: peerTargets("gemini")}, []string{"claude", "gemini"}},
		{"fusion names its council", RunSpec{Homes: true, Agent: "codex", FusionGovernor: "codex", Peers: peerTargets("claude", "gemini")}, []string{"codex", "claude", "gemini"}},
		{"claude lead keeps native in-session", RunSpec{Homes: true, Agent: "claude", ConsultLead: "claude", Preset: nativePreset}, []string{"claude"}},
		{"codex lead degrades native to a claude consult", RunSpec{Homes: true, Agent: "codex", ConsultLead: "codex", Preset: nativePreset}, []string{"codex", "claude"}},
	}
	for _, c := range cases {
		if got := credentialScope(cfg, c.spec); !slices.Equal(got, c.want) {
			t.Errorf("%s: credentialScope = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestEnvKeysOutsideScope: the token keys stripped are exactly the out-of-scope agents' —
// every credential key they honor, not just the primary API key.
func TestEnvKeysOutsideScope(t *testing.T) {
	// A claude-only scope strips Codex's and Gemini's keys (including Gemini's GOOGLE_API_KEY
	// alternate), keeps every Claude key.
	drop := envKeysOutsideScope([]string{"claude"})
	if !drop["OPENAI_API_KEY"] || !drop["GEMINI_API_KEY"] || !drop["GOOGLE_API_KEY"] {
		t.Errorf("claude scope should drop the peer token keys (incl. GOOGLE_API_KEY), got %v", drop)
	}
	for _, keep := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"} {
		if drop[keep] {
			t.Errorf("claude scope must keep %s", keep)
		}
	}
	// A codex-only scope strips Claude's alternates too, so a peer's OAuth token can't leak in.
	if cx := envKeysOutsideScope([]string{"codex"}); !cx["ANTHROPIC_AUTH_TOKEN"] || !cx["CLAUDE_CODE_OAUTH_TOKEN"] {
		t.Errorf("codex scope should drop Claude's alternate tokens, got %v", cx)
	}
	// A raw run (empty scope) strips every agent key, alternates included.
	all := envKeysOutsideScope(nil)
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "OPENAI_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY"} {
		if !all[k] {
			t.Errorf("empty scope should drop every agent key, missing %s: %v", k, all)
		}
	}
}

// TestWriteFilteredEnvFile: dropped keys vanish (both KEY=val AND a bare KEY, which
// docker imports from the ambient env), everything else (the in-scope key, a non-agent
// runtime var, a bare non-credential flag, a comment) is preserved verbatim.
func TestWriteFilteredEnvFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "env")
	os.WriteFile(src, []byte("# creds\nANTHROPIC_API_KEY=keep\nOPENAI_API_KEY=secret\nGEMINI_API_KEY\nMY_VAR=v\nMY_FLAG\n"), 0o644)

	out, err := writeFilteredEnvFile(src, map[string]bool{"OPENAI_API_KEY": true, "GEMINI_API_KEY": true})
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(out)
	data, _ := os.ReadFile(out)
	got := string(data)
	for _, dropped := range []string{"OPENAI_API_KEY", "GEMINI_API_KEY"} {
		if strings.Contains(got, dropped) {
			t.Errorf("filtered env still contains the dropped peer key %q (bare or =):\n%s", dropped, got)
		}
	}
	for _, keep := range []string{"# creds", "ANTHROPIC_API_KEY=keep", "MY_VAR=v", "MY_FLAG"} {
		if !strings.Contains(got, keep) {
			t.Errorf("filtered env dropped %q it should keep:\n%s", keep, got)
		}
	}
}
