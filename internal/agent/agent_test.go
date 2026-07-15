package agent

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

// cleanCmdEnv unsets the per-agent command and model overrides so the defaults are exercised.
func cleanCmdEnv(t *testing.T) {
	t.Helper()
	for _, e := range []string{
		"COOP_CLAUDE_CMD", "COOP_CODEX_CMD", "COOP_GEMINI_CMD", "COOP_GROK_CMD",
		"COOP_CLAUDE_MODEL", "COOP_CODEX_MODEL", "COOP_GEMINI_MODEL", "COOP_GROK_MODEL",
	} {
		if v, ok := os.LookupEnv(e); ok {
			os.Unsetenv(e)
			t.Cleanup(func() { os.Setenv(e, v) })
		}
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRegistry(t *testing.T) {
	if got := Names(); !slices.Equal(got, []string{"claude", "codex", "gemini", "grok"}) {
		t.Errorf("Names() = %v, want [claude codex gemini grok]", got)
	}
	if !Valid("codex") || Valid("nope") {
		t.Error("Valid: codex should be valid, nope should not")
	}
	if _, ok := Get("gemini"); !ok {
		t.Error("Get(gemini) missing")
	}
	if _, ok := Get("nope"); ok {
		t.Error("Get(nope) should be absent")
	}
	// Every agent carries a human product name for the UX surfaces (the ACP dropdowns) —
	// distinct from its grammar token, never empty.
	for _, n := range Names() {
		a, _ := Get(n)
		if d := a.DisplayName(); d == "" || d == n {
			t.Errorf("%s: DisplayName() = %q, want a human product name (e.g. Codex)", n, d)
		}
	}
	// Packages is the union across agents (claude 2 + codex 2 + gemini 1; grok is a native
	// binary, not npm, so it adds none).
	if got := Packages(); len(got) != 5 ||
		!slices.Contains(got, claudeCLIPackage) ||
		!slices.Contains(got, claudeACPPackage) ||
		!slices.Contains(got, codexCLIPackage) ||
		!slices.Contains(got, codexACPPackage) ||
		!slices.Contains(got, geminiCLIPackage) {
		t.Errorf("Packages() = %v", got)
	}
}

func TestCommands(t *testing.T) {
	cleanCmdEnv(t)
	cfg := &config.Config{} // no mcp.json → no --mcp-config
	cases := []struct {
		name                              string
		interactive, headless, acp, csult []string
	}{
		{"claude",
			[]string{"claude", "--dangerously-skip-permissions"},
			[]string{"claude", "--dangerously-skip-permissions", "-p", "go"},
			[]string{"claude-agent-acp"},
			[]string{"claude", "-p", "--permission-mode", "plan", "q"}},
		{"codex",
			[]string{"codex", "--dangerously-bypass-approvals-and-sandbox"},
			[]string{"codex", "exec", "--dangerously-bypass-approvals-and-sandbox", "go"},
			[]string{"codex-acp"},
			[]string{"codex", "exec", "-s", "read-only", "q"}},
		{"gemini",
			[]string{"gemini", "--yolo"},
			[]string{"gemini", "--yolo", "-p", "go"},
			[]string{"gemini", "--acp"},
			[]string{"gemini", "--approval-mode", "plan", "-p", "q"}},
		{"grok",
			[]string{"grok", "--permission-mode", "bypassPermissions"},
			// -p takes the prompt as its value, so the prompt is last, after the mode flags.
			[]string{"grok", "--permission-mode", "bypassPermissions", "-p", "go"},
			[]string{"grok", "agent", "stdio"},
			// read-only via the tool allowlist, NOT --permission-mode plan (a no-op in headless).
			[]string{"grok", "--tools", "read_file,grep,list_dir", "-p", "q"}},
	}
	for _, c := range cases {
		a, _ := Get(c.name)
		if got := a.Interactive(cfg); !slices.Equal(got, c.interactive) {
			t.Errorf("%s Interactive = %v", c.name, got)
		}
		if got := a.Headless(cfg, "go"); !slices.Equal(got, c.headless) {
			t.Errorf("%s Headless = %v", c.name, got)
		}
		if got := a.ACP(cfg); !slices.Equal(got, c.acp) {
			t.Errorf("%s ACP = %v", c.name, got)
		}
		if got := a.ConsultCmd("q"); !slices.Equal(got, c.csult) {
			t.Errorf("%s ConsultCmd = %v", c.name, got)
		}
	}
}

func TestStreamSpecs(t *testing.T) {
	cases := []struct {
		name string
		want StreamSpec
	}{
		{"claude", StreamSpec{Format: StreamClaudeJSON, Flags: []string{"--output-format", "stream-json", "--verbose"}}},
		{"codex", StreamSpec{Format: StreamCodexJSON, Flags: []string{"--json"}, TrailingArgs: 1}},
		{"gemini", StreamSpec{Format: StreamGeminiJSON, Flags: []string{"-o", "stream-json"}, TrailingArgs: 2}},
		{"grok", StreamSpec{Format: StreamGrokJSON, Flags: []string{"--output-format", "streaming-json"}, TrailingArgs: 2}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, _ := Get(c.name)
			got := a.Stream()
			if got.Format != c.want.Format || got.TrailingArgs != c.want.TrailingArgs || !slices.Equal(got.Flags, c.want.Flags) {
				t.Errorf("Stream() = %#v, want %#v", got, c.want)
			}
		})
	}
}

// TestModelSelection: a resolved model rides every command form as a --model flag; a
// COOP_<AGENT>_CMD that bakes its own --model stays authoritative (no duplicate, which
// clap-based CLIs reject); and every agent answers Models() (non-empty menu for `coop models`).
func TestModelSelection(t *testing.T) {
	cleanCmdEnv(t)
	cfg := &config.Config{}
	cfg.SetActiveModel("claude", "opus")
	cfg.SetActiveModel("codex", "gpt-5")
	cfg.SetActiveModel("gemini", "gemini-2.5-pro")
	cfg.SetActiveModel("grok", "grok-4.5")
	cases := []struct {
		name                 string
		interactive, acp     []string
		headlessHasModelFlag bool
	}{
		{"claude", []string{"claude", "--dangerously-skip-permissions", "--model", "opus"}, []string{"claude-agent-acp"}, true},
		{"codex", []string{"codex", "--dangerously-bypass-approvals-and-sandbox", "--model", "gpt-5"}, []string{"codex-acp"}, true},
		// gemini's ACP is its own binary, so the model rides the ACP command too.
		{"gemini", []string{"gemini", "--yolo", "--model", "gemini-2.5-pro"}, []string{"gemini", "--acp", "--model", "gemini-2.5-pro"}, true},
		// grok's ACP is its own binary; the model flag goes BEFORE the `stdio` mode.
		{"grok", []string{"grok", "--permission-mode", "bypassPermissions", "--model", "grok-4.5"}, []string{"grok", "agent", "--model", "grok-4.5", "stdio"}, true},
	}
	for _, c := range cases {
		a, _ := Get(c.name)
		if got := a.Interactive(cfg); !slices.Equal(got, c.interactive) {
			t.Errorf("%s Interactive with model = %v, want %v", c.name, got, c.interactive)
		}
		if got := a.ACP(cfg); !slices.Equal(got, c.acp) {
			t.Errorf("%s ACP with model = %v, want %v", c.name, got, c.acp)
		}
		if got := a.Headless(cfg, "go"); hasModelFlag(got) != c.headlessHasModelFlag {
			t.Errorf("%s Headless with model = %v, want a --model flag", c.name, got)
		}
		if len(a.Models()) == 0 {
			t.Errorf("%s Models() is empty — `coop models` would show nothing", c.name)
		}
	}
	// An env-var default (COOP_<AGENT>_MODEL) reaches base() the same way.
	cfg2 := &config.Config{}
	t.Setenv("COOP_CLAUDE_MODEL", "haiku")
	a, _ := Get("claude")
	want := []string{"claude", "--dangerously-skip-permissions", "--model", "haiku"}
	if got := a.Interactive(cfg2); !slices.Equal(got, want) {
		t.Errorf("claude Interactive with COOP_CLAUDE_MODEL = %v, want %v", got, want)
	}
	// A CMD override that already names a model wins — no second --model is appended.
	t.Setenv("COOP_CLAUDE_CMD", "claude --model sonnet")
	want = []string{"claude", "--model", "sonnet"}
	if got := a.Interactive(cfg2); !slices.Equal(got, want) {
		t.Errorf("claude Interactive with a baked --model = %v, want %v (no duplicate)", got, want)
	}
}

func TestWithModel(t *testing.T) {
	if got := withModel([]string{"claude"}, ""); !slices.Equal(got, []string{"claude"}) {
		t.Errorf("empty model must be a no-op, got %v", got)
	}
	for _, baked := range [][]string{
		{"codex", "--model", "x"},
		{"codex", "--model=x"},
		{"codex", "-m", "x"},
		{"codex", "-m=x"},
	} {
		if got := withModel(baked, "y"); !slices.Equal(got, baked) {
			t.Errorf("withModel(%v) must not append a duplicate, got %v", baked, got)
		}
	}
}

func TestEffortSelection(t *testing.T) {
	cleanCmdEnv(t)
	cfg := &config.Config{}
	cfg.SetActiveEffort("claude", "xhigh")
	cfg.SetActiveEffort("codex", "high")
	cfg.SetActiveEffort("gemini", "high") // gemini has no effort control → the flag never appears
	cfg.SetActiveEffort("grok", "high")
	cases := []struct {
		name             string
		interactive, acp []string
	}{
		{"claude", []string{"claude", "--dangerously-skip-permissions", "--effort", "xhigh"}, []string{"claude-agent-acp"}},
		{"codex", []string{"codex", "--dangerously-bypass-approvals-and-sandbox", "-c", "model_reasoning_effort=high"}, []string{"codex-acp"}},
		{"gemini", []string{"gemini", "--yolo"}, []string{"gemini", "--acp"}}, // no effort flag anywhere
		// grok's ACP is its own binary; the effort flag goes BEFORE the `stdio` mode, like the model.
		{"grok", []string{"grok", "--permission-mode", "bypassPermissions", "--reasoning-effort", "high"}, []string{"grok", "agent", "--reasoning-effort", "high", "stdio"}},
	}
	for _, c := range cases {
		a, _ := Get(c.name)
		if got := a.Interactive(cfg); !slices.Equal(got, c.interactive) {
			t.Errorf("%s Interactive with effort = %v, want %v", c.name, got, c.interactive)
		}
		if got := a.ACP(cfg); !slices.Equal(got, c.acp) {
			t.Errorf("%s ACP with effort = %v, want %v", c.name, got, c.acp)
		}
	}
	for name, want := range map[string]bool{"claude": true, "codex": true, "grok": true, "gemini": false} {
		a, _ := Get(name)
		if SupportsEffort(a) != want {
			t.Errorf("SupportsEffort(%s) = %v, want %v", name, SupportsEffort(a), want)
		}
	}
	// claude-agent-acp takes no flags, so claude's effort rides an env var instead.
	if claude, _ := Get("claude"); claude.EffortEnv() != "CLAUDE_CODE_EFFORT_LEVEL" {
		t.Errorf("claude EffortEnv = %q, want CLAUDE_CODE_EFFORT_LEVEL", claude.EffortEnv())
	}
}

func TestNativeSubagentCapabilityIsAdapterOwned(t *testing.T) {
	for name, want := range map[string]bool{"claude": true, "codex": false, "gemini": false, "grok": false} {
		a, ok := Get(name)
		if !ok {
			t.Fatalf("agent %q not registered", name)
		}
		support := a.NativeSubagents()
		if got := support.Render != nil; got != want {
			t.Errorf("%s native renderer present = %v, want %v", name, got, want)
		}
		if want && support.HomeDir == "" {
			t.Errorf("%s native support has no destination", name)
		}
	}
	claude, _ := Get("claude")
	support := claude.NativeSubagents()
	name, content := support.Render(NativeSubagent{
		Name: "coop-thinker", Description: "Use for: architecture.", Model: "opus",
		Effort: "xhigh", Prompt: "Think hard.",
	})
	if name != "coop-thinker.md" || support.HomeDir != ".claude/agents" {
		t.Errorf("Claude native destination = (%q, %q), want coop-thinker.md under .claude/agents", name, support.HomeDir)
	}
	for _, want := range []string{"name: coop-thinker", "description: Use for: architecture.", "model: opus", "effort: xhigh", "Think hard."} {
		if !strings.Contains(content, want) {
			t.Errorf("Claude native rendering missing %q:\n%s", want, content)
		}
	}
}

func TestWithEffort(t *testing.T) {
	claude, _ := Get("claude")
	gemini, _ := Get("gemini")
	codex, _ := Get("codex")
	if got := withEffort([]string{"claude"}, claude, ""); !slices.Equal(got, []string{"claude"}) {
		t.Errorf("empty effort must be a no-op, got %v", got)
	}
	if got := withEffort([]string{"gemini"}, gemini, "high"); !slices.Equal(got, []string{"gemini"}) {
		t.Errorf("withEffort for an effortless agent must be a no-op, got %v", got)
	}
	for _, baked := range [][]string{
		{"claude", "--effort", "low"},
		{"codex", "-c", "model_reasoning_effort=low"},
	} {
		a := claude
		if baked[0] == "codex" {
			a = codex
		}
		if got := withEffort(baked, a, "high"); !slices.Equal(got, baked) {
			t.Errorf("withEffort(%v) must not append a duplicate, got %v", baked, got)
		}
	}
}

func TestEmptyCmdOverrideStillRunnable(t *testing.T) {
	cfg := &config.Config{} // no mcp.json → no --mcp-config trailing claude's base
	// An explicitly-empty override (COOP_<AGENT>_CMD="") must still produce a runnable command:
	// base()[0] is the executable, and the headless/exec forms index it — an empty argv would
	// otherwise try to exec the first flag (or run the image with no command).
	for _, c := range []struct{ name, env, want string }{
		{"claude", "COOP_CLAUDE_CMD", "claude"},
		{"codex", "COOP_CODEX_CMD", "codex"},
		{"gemini", "COOP_GEMINI_CMD", "gemini"},
		{"grok", "COOP_GROK_CMD", "grok"},
	} {
		t.Setenv(c.env, "")
		a, _ := Get(c.name)
		if got := a.Interactive(cfg); len(got) == 0 || got[0] != c.want {
			t.Errorf("%s Interactive with empty override = %v, want it to start with %q", c.name, got, c.want)
		}
		if got := a.Headless(cfg, "go"); len(got) == 0 || got[0] != c.want {
			t.Errorf("%s Headless with empty override = %v, want it to start with %q", c.name, got, c.want)
		}
	}
}

func TestClaudeMCPConfig(t *testing.T) {
	cleanCmdEnv(t)
	dir := t.TempDir()
	mcp := filepath.Join(dir, "mcp.json")
	mustWrite(t, mcp, `{"mcpServers":{"fs":{"command":"npx","args":["-y","server"]}}}`) // a declared server → MCP active
	cfg := &config.Config{MCPFile: mcp, MCPInBox: "/home/node/.mcp.json"}
	a, _ := Get("claude")
	want := []string{"claude", "--dangerously-skip-permissions", "--mcp-config", "/home/node/.mcp.json"}
	if got := a.Interactive(cfg); !slices.Equal(got, want) {
		t.Errorf("claude Interactive with mcp = %v, want %v", got, want)
	}
}

func TestResume(t *testing.T) {
	cleanCmdEnv(t)
	cfgDir := t.TempDir()
	ws := "/work/myrepo-forks/demo"
	id := "11111111-2222-4333-8444-555555555555"
	cfg := &config.Config{ConfigDir: cfgDir}

	// No session yet → fresh command, resumed=false (for every agent).
	for _, name := range Names() {
		a, _ := Get(name)
		if cmd, resumed := a.Resume(cfg, ws, id); resumed {
			t.Errorf("Resume(%s) resumed with no session: %v", name, cmd)
		}
	}

	// claude resumes the exact coop-owned id (projects/<cwd>/<id>.jsonl), not --continue.
	claude, _ := Get("claude")
	mustWrite(t, filepath.Join(cfg.AgentDir("claude"), "projects", ClaudeProjectKey(ws), id+".jsonl"), "{}")
	if cmd, ok := claude.Resume(cfg, ws, id); !ok ||
		!slices.Equal(cmd, []string{"claude", "--dangerously-skip-permissions", "--resume", id}) {
		t.Errorf("claude Resume = (%v, %v)", cmd, ok)
	}
	// A different id (no session file) must not resume.
	if cmd, ok := claude.Resume(cfg, ws, "99999999-2222-4333-8444-555555555555"); ok {
		t.Errorf("claude Resume matched an id with no session file: %v", cmd)
	}

	// gemini resumes the exact id, matched by file content (not "latest").
	gemini, _ := Get("gemini")
	mustWrite(t, filepath.Join(cfg.AgentDir("gemini"), "tmp", "demo", "chats", "session-x.jsonl"),
		`{"sessionId":"`+id+`"}`)
	if cmd, ok := gemini.Resume(cfg, ws, id); !ok ||
		!slices.Equal(cmd, []string{"gemini", "--yolo", "--resume", id}) {
		t.Errorf("gemini Resume = (%v, %v)", cmd, ok)
	}
	// gemini's tmp bucket is a version-dependent slug/hash of the path, not the raw basename — so
	// resume must find the id regardless of the bucket name (the old raw-basename lookup silently
	// missed a fork named e.g. "My.Repo" whose real bucket is "my-repo").
	slugWs := filepath.Join(t.TempDir(), "My.Cool_Repo")
	mustWrite(t, filepath.Join(cfg.AgentDir("gemini"), "tmp", "my-cool-repo", "chats", "s.jsonl"), `{"sessionId":"`+id+`"}`)
	if _, ok := gemini.Resume(cfg, slugWs, id); !ok {
		t.Error("gemini Resume must match a session in a slug-named bucket, not only the raw basename")
	}
	// gemini 0.46+ also writes 64-char-hash buckets alongside slug ones (seen on real hosts). A
	// DISTINCT id that lives ONLY in a hash bucket must still resolve — proving the content scan
	// spans every bucket scheme, not just slugs. (A raw-basename lookup would silently miss it.)
	hashID := "77777777-2222-4333-8444-555555555555"
	hashBucket := "00019aef076a44ed361af8d31415c187d0650aad947127fd02c5617717734f4f"
	mustWrite(t, filepath.Join(cfg.AgentDir("gemini"), "tmp", hashBucket, "chats", "h.jsonl"), `{"sessionId":"`+hashID+`"}`)
	if _, ok := gemini.Resume(cfg, filepath.Join(t.TempDir(), "hashed-repo"), hashID); !ok {
		t.Error("gemini Resume must match a session in a 64-char-hash bucket (the gemini 0.46+ scheme)")
	}

	// grok resumes the exact coop-owned id, matched by file content anywhere under sessions/
	// (its working-dir bucketing is version-dependent, so a content scan can't silently miss).
	grok, _ := Get("grok")
	mustWrite(t, filepath.Join(cfg.AgentDir("grok"), "sessions", "some-cwd-bucket", "s.jsonl"),
		`{"sessionId":"`+id+`"}`)
	if cmd, ok := grok.Resume(cfg, ws, id); !ok ||
		!slices.Equal(cmd, []string{"grok", "--permission-mode", "bypassPermissions", "--resume", id}) {
		t.Errorf("grok Resume = (%v, %v)", cmd, ok)
	}
	// A different id (no matching session) must not resume.
	if cmd, ok := grok.Resume(cfg, ws, "88888888-2222-4333-8444-555555555555"); ok {
		t.Errorf("grok Resume matched an id with no session: %v", cmd)
	}

	// codex ignores the id and resumes its most-recent INTERACTIVE session for the cwd,
	// skipping a newer `codex exec` (source=="exec") loop/consult session.
	codex, _ := Get("codex")
	sess := filepath.Join(cfg.AgentDir("codex"), "sessions", "2026", "06")
	mustWrite(t, filepath.Join(sess, "16", "rollout-interactive.jsonl"),
		`{"type":"session_meta","payload":{"id":"abc-123","cwd":"`+ws+`","source":"cli"}}`+"\n")
	mustWrite(t, filepath.Join(sess, "17", "rollout-exec.jsonl"),
		`{"type":"session_meta","payload":{"id":"exec-999","cwd":"`+ws+`","source":"exec"}}`+"\n")
	if cmd, ok := codex.Resume(cfg, ws, id); !ok ||
		!slices.Equal(cmd, []string{"codex", "resume", "abc-123", "--dangerously-bypass-approvals-and-sandbox"}) {
		t.Errorf("codex Resume = (%v, %v) — want the interactive session, not the newer exec one", cmd, ok)
	}
	// A session recorded for a DIFFERENT cwd must not match.
	if cmd, ok := codex.Resume(cfg, "/work/myrepo-forks/other", id); ok {
		t.Errorf("codex Resume(other fork) wrongly matched: %v", cmd)
	}
}

func TestStartSessionAndPreset(t *testing.T) {
	cleanCmdEnv(t)
	cfg := &config.Config{ConfigDir: t.TempDir()}
	id := "11111111-2222-4333-8444-555555555555"

	// claude/gemini/grok preset a caller-chosen id; codex cannot.
	for name, want := range map[string]bool{"claude": true, "gemini": true, "grok": true, "codex": false} {
		a, _ := Get(name)
		if a.PresetSessionID() != want {
			t.Errorf("%s PresetSessionID = %v, want %v", name, a.PresetSessionID(), want)
		}
	}

	claude, _ := Get("claude")
	if cmd := claude.StartSession(cfg, id); !slices.Equal(cmd, []string{"claude", "--dangerously-skip-permissions", "--session-id", id}) {
		t.Errorf("claude StartSession = %v", cmd)
	}
	gemini, _ := Get("gemini")
	if cmd := gemini.StartSession(cfg, id); !slices.Equal(cmd, []string{"gemini", "--yolo", "--session-id", id}) {
		t.Errorf("gemini StartSession = %v", cmd)
	}
	grok, _ := Get("grok")
	if cmd := grok.StartSession(cfg, id); !slices.Equal(cmd, []string{"grok", "--permission-mode", "bypassPermissions", "--session-id", id}) {
		t.Errorf("grok StartSession = %v", cmd)
	}
	// codex ignores the id and just starts interactively.
	codex, _ := Get("codex")
	if cmd := codex.StartSession(cfg, id); !slices.Equal(cmd, codex.Interactive(cfg)) {
		t.Errorf("codex StartSession = %v, want Interactive", cmd)
	}
	// Empty id → Interactive for the preset agents too.
	if cmd := claude.StartSession(cfg, ""); !slices.Equal(cmd, claude.Interactive(cfg)) {
		t.Errorf("claude StartSession(empty) = %v, want Interactive", cmd)
	}
}

func TestMetadata(t *testing.T) {
	cases := []struct{ name, instr, authFile, authEnv string }{
		{"claude", "CLAUDE.md", ".credentials.json", "ANTHROPIC_API_KEY"},
		{"codex", "AGENTS.md", "auth.json", "OPENAI_API_KEY"},
		{"gemini", "GEMINI.md", "gemini-credentials.json", "GEMINI_API_KEY"},
		{"grok", "AGENTS.md", "auth.json", "XAI_API_KEY"},
	}
	for _, c := range cases {
		a, _ := Get(c.name)
		if a.InstructionFile() != c.instr {
			t.Errorf("%s InstructionFile = %q, want %q", c.name, a.InstructionFile(), c.instr)
		}
		if f, e := a.AuthMarker(); f != c.authFile || e != c.authEnv {
			t.Errorf("%s AuthMarker = (%q,%q), want (%q,%q)", c.name, f, e, c.authFile, c.authEnv)
		}
	}
}

func TestMCP(t *testing.T) {
	dir := t.TempDir()
	mcpFile := filepath.Join(dir, "mcp.json")
	mustWrite(t, mcpFile, `{"mcpServers":{"x":{"command":"y"}}}`)
	cfg := &config.Config{MCPFile: mcpFile, ConfigDir: dir, HomeInBox: "/home/node"}

	// claude reads mcp.json raw (--mcp-config) → no generated mounts.
	claude, _ := Get("claude")
	if m, err := claude.MCP(cfg); err != nil || len(m) != 0 {
		t.Errorf("claude MCP = %v, %v; want none (reads mcp.json directly)", m, err)
	}
	// gemini/codex/grok generate a config file at their native path (grok reuses codex's
	// [mcp_servers.*] TOML shape).
	for name, boxPath := range map[string]string{
		"gemini": "/home/node/.gemini/settings.json",
		"codex":  "/home/node/.codex/config.toml",
		"grok":   "/home/node/.grok/config.toml",
	} {
		ag, _ := Get(name)
		m, err := ag.MCP(cfg)
		if err != nil || len(m) != 1 || m[0].BoxPath != boxPath || m[0].Content == "" {
			t.Errorf("%s MCP = %v, %v; want one non-empty mount at %s", name, m, err, boxPath)
		}
	}
}

// TestClaudeProjectKey: the session-dir key dashes every non-alphanumeric char (matching
// Claude Code), so a dotted segment like ".agent" maps to "-agent" and coop resolves the
// right project dir. Ground truth: Claude stores "/x/.config" as "-x--config".
func TestClaudeProjectKey(t *testing.T) {
	cases := map[string]string{
		"/Users/a/Projects/os/coop": "-Users-a-Projects-os-coop",
		"/x/.config/hammerspoon":    "-x--config-hammerspoon",
		"/repo/.agent":              "-repo--agent",
		"/has_underscore/and.dot":   "-has-underscore-and-dot",
	}
	for in, want := range cases {
		if got := ClaudeProjectKey(in); got != want {
			t.Errorf("ClaudeProjectKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLogin(t *testing.T) {
	cfg := &config.Config{}
	for name, want := range map[string][]string{
		"claude": {"claude", "auth", "login"},
		"gemini": {"gemini"},
		"codex":  {"codex", "login", "--device-auth"},
		"grok":   {"grok", "login", "--device-auth"},
	} {
		a, _ := Get(name)
		if got := a.Login(cfg); !slices.Equal(got, want) {
			t.Errorf("%s Login = %v, want %v", name, got, want)
		}
	}
}

// TestACPRateLimitSignalsPinned pins each adapter's structured limit markers — the wire
// format the ACP controller rotates on. A change here must be a conscious adapter edit
// (the controller itself carries no provider constants).
func TestACPRateLimitSignalsPinned(t *testing.T) {
	want := map[string][]ACPSignal{
		"claude": {{Key: "errorKind", Value: "rate_limit"}},
		"codex":  {{Value: "usageLimitExceeded"}},
		"gemini": {{Value: "RESOURCE_EXHAUSTED"}},
		// grok's ACP limit marker isn't captured yet (needs a live limit in a box) — pin the
		// current honest state: no structured signal, so it rotates only on the output-token axis.
		"grok": nil,
	}
	for name, w := range want {
		a, ok := Get(name)
		if !ok {
			t.Fatalf("agent %s not registered", name)
		}
		got := a.ACPRateLimitSignals()
		if len(got) != len(w) {
			t.Fatalf("%s signals = %+v, want %+v", name, got, w)
		}
		for i := range w {
			if got[i] != w[i] {
				t.Errorf("%s signal[%d] = %+v, want %+v", name, i, got[i], w[i])
			}
		}
	}
}

// TestACPSessionSettingsAndBoxEnv pins each adapter's ordered target settings and box env.
func TestACPSessionSettingsAndBoxEnv(t *testing.T) {
	claude, _ := Get("claude")
	target := Target{Model: "model-x", Effort: "xhigh"}
	if got := claude.ACPSessionSettings(target); !slices.Equal(got, []ACPSessionSetting{
		{Method: ACPSetConfigOption, ConfigID: "mode", Value: "bypassPermissions"},
		{Method: ACPSetConfigOption, ConfigID: "model", Value: "model-x"},
		{Method: ACPSetConfigOption, ConfigID: "effort", Value: "xhigh"},
	}) {
		t.Errorf("claude ACPSessionSettings = %v", got)
	}
	wantEnv := []string{"CLAUDE_CONFIG_DIR=/home/node/.claude", "CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=0"}
	if got := claude.BoxEnv("/home/node"); len(got) != 2 || got[0] != wantEnv[0] || got[1] != wantEnv[1] {
		t.Errorf("claude BoxEnv = %v, want %v", got, wantEnv)
	}
	// codex redirects its single-writer sqlite state OFF the shared home to a container-local
	// path, so parallel codex boxes on one account don't collide on the state runtime.
	codex, _ := Get("codex")
	if got := codex.ACPSessionSettings(target); !slices.Equal(got, []ACPSessionSetting{
		{Method: ACPSetConfigOption, ConfigID: "model", Value: "model-x"},
		{Method: ACPSetConfigOption, ConfigID: "reasoning_effort", Value: "xhigh"},
	}) {
		t.Errorf("codex ACPSessionSettings = %v", got)
	}
	if got := codex.BoxEnv("/home/node"); len(got) != 1 || got[0] != "CODEX_SQLITE_HOME=/home/node/.codex-state" {
		t.Errorf("codex BoxEnv = %v, want [CODEX_SQLITE_HOME=/home/node/.codex-state]", got)
	}
	gemini, _ := Get("gemini")
	if got := gemini.ACPSessionSettings(target); !slices.Equal(got, []ACPSessionSetting{{Method: ACPSetModel, Value: "model-x"}}) {
		t.Errorf("gemini ACPSessionSettings = %v", got)
	}
	for _, n := range Names() {
		a, _ := Get(n)
		_ = a.ACPSessionSettings(target)
		_ = a.BoxEnv("/home/node")
		if n == "grok" {
			if settings := a.ACPSessionSettings(target); len(settings) != 0 {
				t.Errorf("grok should force no session settings, got %v", settings)
			}
		}
		if n != "claude" && n != "codex" {
			if env := a.BoxEnv("/home/node"); len(env) != 0 {
				t.Errorf("%s should need no box env, got %v", n, env)
			}
		}
	}
}
