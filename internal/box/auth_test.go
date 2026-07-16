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
	// profiles/default.
	os.MkdirAll(filepath.Join(dir, "codex", "profiles", "default"), 0o755)
	os.WriteFile(filepath.Join(dir, "codex", "profiles", "default", "auth.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(dir, "env"), []byte("# OPENAI_API_KEY=ignored\nGEMINI_API_KEY=real\nEMPTY=\n"), 0o644)

	if got := AuthedAgents(cfg); !slices.Equal(got, []string{"codex", "gemini"}) {
		t.Errorf("AuthedAgents = %v, want [codex gemini]", got)
	}
}

func TestAuthedAgentsCredentialMatrix(t *testing.T) {
	for _, name := range agents.Names() {
		ag, _ := agents.Get(name)
		marker, _ := ag.AuthMarker()
		sources := append([]string{"file"}, ag.CredentialEnvKeys()...)
		for _, source := range sources {
			t.Run(name+"/"+source, func(t *testing.T) {
				cfg := &config.Config{ConfigDir: t.TempDir()}
				if source == "file" {
					dir := cfg.AgentProfileDir(name, "default")
					if err := os.MkdirAll(dir, 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(dir, marker), []byte("token"), 0o600); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(cfg.EnvFile(), []byte(source+"=token\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if got := AuthedAgents(cfg); !slices.Equal(got, []string{name}) {
					t.Errorf("AuthedAgents(%s via %s) = %v, want [%s]", name, source, got, name)
				}
			})
		}
	}
}

func TestAuthedAgentsUsesConcreteActiveProfile(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	def := cfg.AgentProfileDir("codex", "default")
	if err := os.MkdirAll(def, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(def, "auth.json"), []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.SetActiveProfile("codex", "unsigned-work")
	if got := AuthedAgents(cfg); slices.Contains(got, "codex") {
		t.Errorf("default marker authenticated an unsigned active account: %v", got)
	}
	if err := os.WriteFile(cfg.EnvFile(), []byte("OPENAI_API_KEY=global\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := AuthedAgents(cfg); slices.Contains(got, "codex") {
		t.Errorf("default env token authenticated an unsigned active account: %v", got)
	}
	work := cfg.AgentProfileDir("codex", "unsigned-work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "auth.json"), []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := AuthedAgents(cfg); !slices.Contains(got, "codex") {
		t.Errorf("active profile marker was not discovered: %v", got)
	}
}

func TestEnvFileKeysFollowRuntimeSemantics(t *testing.T) {
	t.Setenv("IMPORTED_TOKEN", "ambient")
	t.Setenv("EMPTY_IMPORT", "")
	path := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(path, []byte(strings.Join([]string{
		"IMPORTED_TOKEN",
		"CLEARED_TOKEN=first",
		"CLEARED_TOKEN=",
		"RESTORED_TOKEN=",
		"RESTORED_TOKEN=last",
		"PRESERVED_TOKEN=first",
		"PRESERVED_TOKEN",
		"EMPTY_IMPORT=first",
		"EMPTY_IMPORT",
		"MISSING_IMPORT",
		"=ignored",
	}, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	keys := envFileKeys(path)
	for _, want := range []string{"IMPORTED_TOKEN", "RESTORED_TOKEN", "PRESERVED_TOKEN"} {
		if !keys[want] {
			t.Errorf("envFileKeys missing effective non-empty %s: %v", want, keys)
		}
	}
	for _, absent := range []string{"CLEARED_TOKEN", "EMPTY_IMPORT", "MISSING_IMPORT"} {
		if keys[absent] {
			t.Errorf("envFileKeys retained ineffective %s: %v", absent, keys)
		}
	}
	if keys[""] {
		t.Errorf("envFileKeys accepted an empty key: %v", keys)
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
	// profiles/default layout.
	os.MkdirAll(filepath.Join(dir, "claude", "profiles", "default"), 0o755)
	os.WriteFile(filepath.Join(dir, "claude", "profiles", "default", ".credentials.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(dir, "env"), []byte("GEMINI_API_KEY=real\n"), 0o644)
	cfg := &config.Config{ConfigDir: dir}

	// A preset whose only role is a native claude thinker: in-session under a Claude lead
	// (adds nothing to the scope), but degrades to a consult on claude under a codex lead —
	// which then must mount claude's creds.
	nativePreset := &preset.Preset{Roles: []preset.Role{{Name: "thinker", Mode: preset.ModeNative, Agent: "claude"}}}
	sameProviderConsult := &preset.Preset{Roles: []preset.Role{{Name: "probe", Mode: preset.ModeConsult, Agent: "claude"}}}

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
		{"same-provider consult mounts one credential", RunSpec{Homes: true, Agent: "claude", ConsultLead: "claude", Preset: sameProviderConsult}, []string{"claude"}},
		{"codex lead degrades native to a claude consult", RunSpec{Homes: true, Agent: "codex", ConsultLead: "codex", Preset: nativePreset}, []string{"codex", "claude"}},
	}
	for _, c := range cases {
		if got := credentialScope(cfg, c.spec); !slices.Equal(got, c.want) {
			t.Errorf("%s: credentialScope = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCredentialScopeRoleCredentialMatrix(t *testing.T) {
	for _, name := range agents.Names() {
		ag, _ := agents.Get(name)
		marker, _ := ag.AuthMarker()
		sources := append([]string{"file"}, ag.CredentialEnvKeys()...)
		for _, source := range sources {
			t.Run(name+"/"+source, func(t *testing.T) {
				cfg := &config.Config{ConfigDir: t.TempDir()}
				if source == "file" {
					dir := cfg.AgentProfileDir(name, "default")
					if err := os.MkdirAll(dir, 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(dir, marker), []byte("token"), 0o600); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(cfg.EnvFile(), []byte(source+"=token\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				lead := "claude"
				if name == lead {
					lead = "codex"
				}
				p := &preset.Preset{Roles: []preset.Role{{Name: "reviewer", Mode: preset.ModeConsult, Agent: name}}}
				got := credentialScope(cfg, RunSpec{Homes: true, Agent: lead, ConsultLead: lead, Preset: p})
				if !slices.Equal(got, []string{lead, name}) {
					t.Errorf("credentialScope role %s via %s = %v, want [%s %s]", name, source, got, lead, name)
				}
			})
		}
	}
}

// TestEnvKeysOutsideScope: the token keys stripped are exactly the out-of-scope agents' —
// every credential key they honor, not just the primary API key.
func TestEnvKeysOutsideScope(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	// A claude-only scope strips Codex's and Gemini's keys (including Gemini's GOOGLE_API_KEY
	// alternate), keeps every Claude key.
	drop := envKeysOutsideScope(cfg, []string{"claude"})
	if !drop["OPENAI_API_KEY"] || !drop["GEMINI_API_KEY"] || !drop["GOOGLE_API_KEY"] {
		t.Errorf("claude scope should drop the peer token keys (incl. GOOGLE_API_KEY), got %v", drop)
	}
	for _, keep := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"} {
		if drop[keep] {
			t.Errorf("claude scope must keep %s", keep)
		}
	}
	// A codex-only scope strips Claude's alternates too, so a peer's OAuth token can't leak in.
	if cx := envKeysOutsideScope(cfg, []string{"codex"}); !cx["ANTHROPIC_AUTH_TOKEN"] || !cx["CLAUDE_CODE_OAUTH_TOKEN"] {
		t.Errorf("codex scope should drop Claude's alternate tokens, got %v", cx)
	}
	// A raw run (empty scope) strips every agent key, alternates included.
	all := envKeysOutsideScope(cfg, nil)
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "OPENAI_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY", "XAI_API_KEY"} {
		if !all[k] {
			t.Errorf("empty scope should drop every agent key, missing %s: %v", k, all)
		}
	}
}

func TestEnvKeysOutsideScopeStripsTokenFromNamedAccount(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{ConfigDir: dir}
	cfg.SetActiveProfile("claude", "work")
	cfg.SetActiveProfile("codex", "default")

	drop := envKeysOutsideScope(cfg, []string{"claude", "codex"})
	for _, key := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"} {
		if !drop[key] {
			t.Errorf("named Claude account must drop provider-wide %s: %v", key, drop)
		}
	}
	if drop["OPENAI_API_KEY"] {
		t.Errorf("default Codex account should keep its env token: %v", drop)
	}

	src := filepath.Join(dir, "env")
	if err := os.WriteFile(src, []byte("ANTHROPIC_API_KEY=global\nOPENAI_API_KEY=keep\nMY_VAR=value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	filtered, err := writeFilteredEnvFile(src, drop)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(filtered)
	data, err := os.ReadFile(filtered)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if strings.Contains(got, "ANTHROPIC_API_KEY") || !strings.Contains(got, "OPENAI_API_KEY=keep") || !strings.Contains(got, "MY_VAR=value") {
		t.Errorf("account-scoped filtered env is wrong:\n%s", got)
	}
}

func TestEnvKeysOutsideScopeMarkerBackedDefaultWinsMatrix(t *testing.T) {
	for _, name := range agents.Names() {
		ag, _ := agents.Get(name)
		marker, _ := ag.AuthMarker()
		for _, source := range ag.CredentialEnvKeys() {
			t.Run(name+"/"+source, func(t *testing.T) {
				cfg := &config.Config{ConfigDir: t.TempDir()}
				if err := cfg.SetDefaultProfile(name, "work"); err != nil {
					t.Fatal(err)
				}
				if drop := envKeysOutsideScope(cfg, []string{name}); drop[source] {
					t.Fatalf("env-only default unexpectedly dropped %s: %v", source, drop)
				}
				dir := cfg.AgentProfileDir(name, "work")
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, marker), []byte("file-token"), 0o600); err != nil {
					t.Fatal(err)
				}
				drop := envKeysOutsideScope(cfg, []string{name})
				for _, key := range ag.CredentialEnvKeys() {
					if !drop[key] {
						t.Errorf("marker-backed default must drop provider-wide %s: %v", key, drop)
					}
				}

				src := cfg.EnvFile()
				if err := os.WriteFile(src, []byte(source+"=global\nSHARED=value\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				filtered, err := writeFilteredEnvFile(src, drop)
				if err != nil {
					t.Fatal(err)
				}
				defer os.Remove(filtered)
				data, err := os.ReadFile(filtered)
				if err != nil {
					t.Fatal(err)
				}
				if got := string(data); strings.Contains(got, source) || !strings.Contains(got, "SHARED=value") {
					t.Errorf("marker-backed default filter is wrong:\n%s", got)
				}
			})
		}
	}
}

func TestEnvKeysOutsideScopeHonorsGeminiAPIKeySelection(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	if err := cfg.SetDefaultProfile("gemini", "work"); err != nil {
		t.Fatal(err)
	}
	dir := cfg.AgentProfileDir("gemini", "work")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gemini-credentials.json"), []byte("stale-keychain"), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(settings, []byte(`{"security":{"auth":{"selectedType":"gemini-api-key"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	drop := envKeysOutsideScope(cfg, []string{"gemini"})
	if drop["GEMINI_API_KEY"] || !drop["GOOGLE_API_KEY"] {
		t.Errorf("Gemini API-key selection authority = %v, want only GEMINI_API_KEY", drop)
	}
	if err := os.WriteFile(settings, []byte(`{"security":{"auth":{"selectedType":"vertex-ai"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	drop = envKeysOutsideScope(cfg, []string{"gemini"})
	if !drop["GEMINI_API_KEY"] || drop["GOOGLE_API_KEY"] {
		t.Errorf("Gemini Vertex selection authority = %v, want only GOOGLE_API_KEY", drop)
	}
	if err := os.WriteFile(settings, []byte(`{"security":{"auth":{"selectedType":"oauth-personal"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"} {
		if drop := envKeysOutsideScope(cfg, []string{"gemini"}); !drop[key] {
			t.Errorf("Gemini OAuth selection kept provider-wide %s: %v", key, drop)
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
