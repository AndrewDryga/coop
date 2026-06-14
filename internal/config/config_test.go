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
	if want := []string{"claude", "--dangerously-skip-permissions"}; !slices.Equal(c.ClaudeCmd, want) {
		t.Errorf("ClaudeCmd = %v, want %v (no --mcp-config when mcp.json absent)", c.ClaudeCmd, want)
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
	if want := []string{"claude", "--foo", "bar"}; !slices.Equal(c.ClaudeCmd, want) {
		t.Errorf("ClaudeCmd = %v, want %v", c.ClaudeCmd, want)
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

func TestMCPFlagInjected(t *testing.T) {
	clearAgentEnv(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	mcp := filepath.Join(t.TempDir(), "mcp.json")
	os.WriteFile(mcp, []byte(`{"mcpServers":{}}`), 0o644)
	t.Setenv("COOP_MCP_FILE", mcp)

	c := Load()
	if got := strings.Join(c.ClaudeCmd, " "); !strings.HasSuffix(got, "--mcp-config /home/node/.mcp.json") {
		t.Errorf("ClaudeCmd should carry --mcp-config, got %q", got)
	}
}
