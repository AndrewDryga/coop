package agent

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

// cleanCmdEnv unsets the per-agent command overrides so the defaults are exercised.
func cleanCmdEnv(t *testing.T) {
	t.Helper()
	for _, e := range []string{"COOP_CLAUDE_CMD", "COOP_CODEX_CMD", "COOP_GEMINI_CMD"} {
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
	if got := Names(); !slices.Equal(got, []string{"claude", "codex", "gemini"}) {
		t.Errorf("Names() = %v, want [claude codex gemini]", got)
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
	}
	for _, c := range cases {
		a, _ := Get(c.name)
		if got := a.Interactive(cfg); !slices.Equal(got, c.interactive) {
			t.Errorf("%s Interactive = %v", c.name, got)
		}
		if got := a.Headless(cfg, "go"); !slices.Equal(got, c.headless) {
			t.Errorf("%s Headless = %v", c.name, got)
		}
		if got := a.ACP(); !slices.Equal(got, c.acp) {
			t.Errorf("%s ACP = %v", c.name, got)
		}
		if got := a.ConsultCmd("q"); !slices.Equal(got, c.csult) {
			t.Errorf("%s ConsultCmd = %v", c.name, got)
		}
	}
}

func TestClaudeMCPConfig(t *testing.T) {
	cleanCmdEnv(t)
	dir := t.TempDir()
	mcp := filepath.Join(dir, "mcp.json")
	mustWrite(t, mcp, "{}")
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
	cfg := &config.Config{ConfigDir: cfgDir}

	// No session yet → fresh command, resumed=false.
	for _, name := range Names() {
		a, _ := Get(name)
		if cmd, resumed := a.Resume(cfg, ws); resumed {
			t.Errorf("Resume(%s) resumed with no session: %v", name, cmd)
		}
	}

	claude, _ := Get("claude")
	mustWrite(t, filepath.Join(cfgDir, "claude", "projects", claudeProjectKey(ws), "s.jsonl"), "{}")
	if cmd, ok := claude.Resume(cfg, ws); !ok ||
		!slices.Equal(cmd, []string{"claude", "--dangerously-skip-permissions", "--continue"}) {
		t.Errorf("claude Resume = (%v, %v)", cmd, ok)
	}

	gemini, _ := Get("gemini")
	mustWrite(t, filepath.Join(cfgDir, "gemini", "tmp", "demo", "chats", "session.jsonl"), "{}")
	if cmd, ok := gemini.Resume(cfg, ws); !ok ||
		!slices.Equal(cmd, []string{"gemini", "--yolo", "--resume", "latest"}) {
		t.Errorf("gemini Resume = (%v, %v)", cmd, ok)
	}

	codex, _ := Get("codex")
	mustWrite(t, filepath.Join(cfgDir, "codex", "sessions", "2026", "06", "16", "rollout-x.jsonl"),
		`{"type":"session_meta","payload":{"id":"abc-123","cwd":"`+ws+`"}}`+"\n")
	if cmd, ok := codex.Resume(cfg, ws); !ok ||
		!slices.Equal(cmd, []string{"codex", "resume", "abc-123", "--dangerously-bypass-approvals-and-sandbox"}) {
		t.Errorf("codex Resume = (%v, %v)", cmd, ok)
	}
	// A session recorded for a DIFFERENT cwd must not match.
	if cmd, ok := codex.Resume(cfg, "/work/myrepo-forks/other"); ok {
		t.Errorf("codex Resume(other fork) wrongly matched: %v", cmd)
	}
}

func TestMetadata(t *testing.T) {
	cases := []struct{ name, instr, authFile, authEnv string }{
		{"claude", "CLAUDE.md", ".credentials.json", "ANTHROPIC_API_KEY"},
		{"codex", "AGENTS.md", "auth.json", "OPENAI_API_KEY"},
		{"gemini", "GEMINI.md", "gemini-credentials.json", "GEMINI_API_KEY"},
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
	// gemini/codex generate a config file at their native path.
	for name, boxPath := range map[string]string{
		"gemini": "/home/node/.gemini/settings.json",
		"codex":  "/home/node/.codex/config.toml",
	} {
		ag, _ := Get(name)
		m, err := ag.MCP(cfg)
		if err != nil || len(m) != 1 || m[0].BoxPath != boxPath || m[0].Content == "" {
			t.Errorf("%s MCP = %v, %v; want one non-empty mount at %s", name, m, err, boxPath)
		}
	}
}

func TestLogin(t *testing.T) {
	cfg := &config.Config{}
	for name, want := range map[string][]string{
		"claude": {"claude"},
		"gemini": {"gemini"},
		"codex":  {"codex", "login", "--device-auth"},
	} {
		a, _ := Get(name)
		if got := a.Login(cfg); !slices.Equal(got, want) {
			t.Errorf("%s Login = %v, want %v", name, got, want)
		}
	}
}
