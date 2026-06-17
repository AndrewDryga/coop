package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// clearAgentEnv unsets every COOP_* var for the duration of the test and
// restores them afterward, so the host's environment can't skew the defaults.
func clearAgentEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		k, _, ok := strings.Cut(kv, "=")
		if ok && strings.HasPrefix(k, "COOP_") {
			orig := os.Getenv(k)
			t.Cleanup(func() { os.Setenv(k, orig) })
			os.Unsetenv(k)
		}
	}
}

func TestDefaults(t *testing.T) {
	clearAgentEnv(t)
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	c := Load()
	if c.BaseImage != "coop-box" {
		t.Errorf("BaseImage = %q, want coop-box", c.BaseImage)
	}
	// Workdir defaults to empty, which box.resolveWorkdir reads as "mount the
	// repo at its real host path" so history is shared across run/loop/acp.
	if c.Workdir != "" || c.HomeInBox != "/home/node" {
		t.Errorf("Workdir=%q HomeInBox=%q", c.Workdir, c.HomeInBox)
	}
	wantDir := filepath.Join(tmp, "coop", "agents")
	if c.ConfigDir != wantDir {
		t.Errorf("ConfigDir = %q, want %q", c.ConfigDir, wantDir)
	}
	if !c.Homes || !c.Network || !c.Cache {
		t.Errorf("toggles default on: Homes=%v Network=%v Cache=%v", c.Homes, c.Network, c.Cache)
	}
	if c.MCPFile != filepath.Join(wantDir, "mcp.json") {
		t.Errorf("MCPFile = %q", c.MCPFile)
	}
	if c.MCPInBox != "/home/node/.mcp.json" {
		t.Errorf("MCPInBox = %q", c.MCPInBox)
	}
	if c.FusionGovernor != "codex" {
		t.Errorf("FusionGovernor = %q, want codex", c.FusionGovernor)
	}
}

func TestEnvOverrides(t *testing.T) {
	clearAgentEnv(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("COOP_BASE_IMAGE", "custom-box")
	t.Setenv("COOP_WORKDIR", "/code")
	t.Setenv("COOP_CLAUDE_CMD", "claude --foo bar")
	t.Setenv("COOP_CACHE", "0")
	t.Setenv("COOP_NETWORK", "false")

	c := Load()
	if c.BaseImage != "custom-box" || c.Workdir != "/code" {
		t.Errorf("env overrides not applied: %q %q", c.BaseImage, c.Workdir)
	}
	if c.Cache || c.Network {
		t.Errorf("toggles should be off: Cache=%v Network=%v", c.Cache, c.Network)
	}
}

func TestConfFilePrecedence(t *testing.T) {
	clearAgentEnv(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	conf := filepath.Join(t.TempDir(), "coop.conf")
	os.WriteFile(conf, []byte("# comment\nexport COOP_BASE_IMAGE=\"from-conf\"\nCOOP_WORKDIR=/from/conf\n"), 0o644)
	t.Setenv("COOP_CONF", conf)

	c := Load()
	if c.BaseImage != "from-conf" {
		t.Errorf("conf value not used: BaseImage=%q", c.BaseImage)
	}
	if c.Workdir != "/from/conf" {
		t.Errorf("conf value not used: Workdir=%q", c.Workdir)
	}

	// Environment must win over the conf file.
	t.Setenv("COOP_BASE_IMAGE", "from-env")
	if c := Load(); c.BaseImage != "from-env" {
		t.Errorf("env should beat conf: BaseImage=%q", c.BaseImage)
	}
}

func TestCmd(t *testing.T) {
	clearAgentEnv(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	conf := filepath.Join(t.TempDir(), "coop.conf")
	os.WriteFile(conf, []byte("COOP_FOO_CMD=foo --from-conf\n"), 0o644)
	t.Setenv("COOP_CONF", conf)
	c := Load()

	// default → split into words
	if got := c.Cmd("COOP_BAR_CMD", "bar --baz"); !slices.Equal(got, []string{"bar", "--baz"}) {
		t.Errorf("default: Cmd = %v", got)
	}
	// conf file → split
	if got := c.Cmd("COOP_FOO_CMD", "ignored"); !slices.Equal(got, []string{"foo", "--from-conf"}) {
		t.Errorf("conf: Cmd = %v", got)
	}
	// env beats conf
	t.Setenv("COOP_FOO_CMD", "foo --from-env")
	if got := Load().Cmd("COOP_FOO_CMD", "ignored"); !slices.Equal(got, []string{"foo", "--from-env"}) {
		t.Errorf("env: Cmd = %v", got)
	}
}

func TestShellSplit(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"bar --baz", []string{"bar", "--baz"}},
		{`bash -lc "npm test && npm run lint"`, []string{"bash", "-lc", "npm test && npm run lint"}},
		{`bash -lc 'a b'`, []string{"bash", "-lc", "a b"}},
		{`a\ b`, []string{"a b"}}, // escaped space joins a word
		{`a "b c" d`, []string{"a", "b c", "d"}},
		{`"a""b"`, []string{"ab"}},       // adjacent quotes concatenate
		{`''`, []string{""}},             // an empty quoted arg survives
		{`a "b c`, []string{"a", "b c"}}, // unterminated quote runs to the end
		{`trail\`, []string{`trail\`}},   // a trailing backslash is literal
	}
	for _, c := range cases {
		if got := shellSplit(c.in); !slices.Equal(got, c.want) {
			t.Errorf("shellSplit(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}
}

// Command-like settings honor shell quoting through both the environment and the conf
// file, with the environment winning — so a gate like `bash -lc "a && b"` parses as
// three args, not five.
func TestCommandQuoting(t *testing.T) {
	clearAgentEnv(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	conf := filepath.Join(t.TempDir(), "coop.conf")
	os.WriteFile(conf, []byte(`COOP_GATE=bash -lc "make check"`+"\n"), 0o644)
	t.Setenv("COOP_CONF", conf)

	if got := Load().Gate; !slices.Equal(got, []string{"bash", "-lc", "make check"}) {
		t.Errorf("conf gate = %#v", got)
	}
	t.Setenv("COOP_GATE", `bash -lc "npm test && npm run lint"`)
	if got := Load().Gate; !slices.Equal(got, []string{"bash", "-lc", "npm test && npm run lint"}) {
		t.Errorf("env gate = %#v", got)
	}
}

// MCPActive flips on when an mcp.json exists (the claude adapter turns it into
// --mcp-config; gemini/codex get generated config files in box.Run).
func TestMCPActive(t *testing.T) {
	clearAgentEnv(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if Load().MCPActive() {
		t.Error("MCPActive should be false with no mcp.json")
	}
	mcp := filepath.Join(t.TempDir(), "mcp.json")
	os.WriteFile(mcp, []byte(`{"mcpServers":{}}`), 0o644)
	t.Setenv("COOP_MCP_FILE", mcp)
	if !Load().MCPActive() {
		t.Error("MCPActive should be true with an mcp.json")
	}
}
