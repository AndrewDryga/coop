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
	if !c.Homes || !c.Network || !c.Cache || !c.Caffeinate {
		t.Errorf("toggles default on: Homes=%v Network=%v Cache=%v Caffeinate=%v", c.Homes, c.Network, c.Cache, c.Caffeinate)
	}
	if c.Preflight {
		t.Error("Preflight is opt-in — must default off")
	}
	if c.NoUpdateCheck {
		t.Error("NoUpdateCheck is opt-in — must default off (the daily check runs by default)")
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
	t.Setenv("COOP_PREFLIGHT", "1")
	t.Setenv("COOP_CAFFEINATE", "off")
	t.Setenv("COOP_NO_UPDATE_CHECK", "1")

	c := Load()
	if c.BaseImage != "custom-box" || c.Workdir != "/code" {
		t.Errorf("env overrides not applied: %q %q", c.BaseImage, c.Workdir)
	}
	if c.Cache || c.Network || c.Caffeinate {
		t.Errorf("toggles should be off: Cache=%v Network=%v Caffeinate=%v", c.Cache, c.Network, c.Caffeinate)
	}
	if !c.Preflight {
		t.Error("COOP_PREFLIGHT=1 should turn Preflight on")
	}
	if !c.NoUpdateCheck {
		t.Error("COOP_NO_UPDATE_CHECK=1 should opt out of the update check")
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

// MCPActive flips on only when mcp.json declares at least one server (the claude adapter turns
// it into --mcp-config; gemini/codex get generated config files in box.Run). An absent, empty,
// or malformed file is inactive — so the empty stub `coop init` scaffolds is a pure no-op.
func TestMCPActive(t *testing.T) {
	clearAgentEnv(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if Load().MCPActive() {
		t.Error("MCPActive should be false with no mcp.json")
	}
	mcp := filepath.Join(t.TempDir(), "mcp.json")
	t.Setenv("COOP_MCP_FILE", mcp)

	// The scaffolded stub (no servers) must stay inactive.
	os.WriteFile(mcp, []byte("{\n  \"mcpServers\": {}\n}\n"), 0o644)
	if Load().MCPActive() {
		t.Error("an empty (no-server) mcp.json must be inactive")
	}
	// Malformed → inactive (a broken file never trips MCP wiring).
	os.WriteFile(mcp, []byte("{ not json"), 0o644)
	if Load().MCPActive() {
		t.Error("a malformed mcp.json must be inactive")
	}
	// At least one server → active.
	os.WriteFile(mcp, []byte(`{"mcpServers":{"fs":{"command":"npx","args":["-y","server"]}}}`), 0o644)
	if !Load().MCPActive() {
		t.Error("MCPActive should be true once a server is declared")
	}
}

func TestAgentProfileResolution(t *testing.T) {
	dir := t.TempDir()
	c := &Config{ConfigDir: dir}
	claudeDir := filepath.Join(dir, "claude")

	// Every profile — "default" included — resolves under profiles/. The legacy flat layout is
	// retired (migrateFlatVaults moves any old login into profiles/default at startup), so there's
	// no longer a branch where the agent dir itself is the default.
	if got, want := c.AgentProfileDir("claude", "default"), filepath.Join(claudeDir, "profiles", "default"); got != want {
		t.Errorf("default = %q, want %q", got, want)
	}
	if got, want := c.AgentDir("claude"), filepath.Join(claudeDir, "profiles", "default"); got != want {
		t.Errorf("AgentDir = %q, want %q", got, want)
	}
	if got, want := c.AgentProfileDir("claude", "work"), filepath.Join(claudeDir, "profiles", "work"); got != want {
		t.Errorf("named profile = %q, want %q", got, want)
	}

	// SetActiveProfile changes what AgentDir resolves to; empty resets to default.
	c.SetActiveProfile("claude", "work")
	if got, want := c.AgentDir("claude"), filepath.Join(claudeDir, "profiles", "work"); got != want {
		t.Errorf("AgentDir after SetActiveProfile = %q, want %q", got, want)
	}
	c.SetActiveProfile("claude", "")
	if got, want := c.AgentDir("claude"), filepath.Join(claudeDir, "profiles", "default"); got != want {
		t.Errorf("AgentDir after reset = %q, want %q", got, want)
	}
}

func TestProfilesListing(t *testing.T) {
	dir := t.TempDir()
	c := &Config{ConfigDir: dir}

	// Never used → no profiles.
	if got := c.Profiles("codex"); got != nil {
		t.Errorf("unused agent Profiles = %v, want nil", got)
	}
	// A bare agent dir with no profiles/ reports nothing — the flat layout is retired, so there's no
	// "default" until the profiles/ dir exists (migrateFlatVaults creates it from an old flat login).
	if err := os.MkdirAll(filepath.Join(dir, "codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := c.Profiles("codex"); got != nil {
		t.Errorf("bare agent dir (no profiles/) Profiles = %v, want nil", got)
	}
	// The profiles/ subdirs are the profile names.
	for _, p := range []string{"default", "work", "personal"} {
		if err := os.MkdirAll(filepath.Join(dir, "codex", "profiles", p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got := c.Profiles("codex")
	slices.Sort(got)
	if !slices.Equal(got, []string{"default", "personal", "work"}) {
		t.Errorf("Profiles = %v", got)
	}
}

func TestDefaultProfileMark(t *testing.T) {
	dir := t.TempDir()
	c := &Config{ConfigDir: dir}

	// No mark → the built-in default, for both the getter and AgentDir resolution.
	if got := c.DefaultProfileOf("claude"); got != DefaultProfile {
		t.Errorf("unmarked default = %q, want %q", got, DefaultProfile)
	}
	// Mark a profile → it becomes the default and AgentDir resolves to it.
	if err := c.SetDefaultProfile("claude", "personal"); err != nil {
		t.Fatal(err)
	}
	if got := c.DefaultProfileOf("claude"); got != "personal" {
		t.Errorf("marked default = %q, want personal", got)
	}
	if got, want := c.AgentDir("claude"), c.AgentProfileDir("claude", "personal"); got != want {
		t.Errorf("AgentDir = %q, want the marked default's dir %q", got, want)
	}
	// A per-run override (a --credential login, or the loop's rotation) still wins.
	c.SetActiveProfile("claude", "work")
	if got, want := c.AgentDir("claude"), c.AgentProfileDir("claude", "work"); got != want {
		t.Errorf("AgentDir = %q, want the override's dir %q", got, want)
	}
	// The mark is persisted to DefaultsFile (read back by a fresh load).
	if m := loadConfFile(c.DefaultsFile()); m["claude"] != "personal" {
		t.Errorf("DefaultsFile not persisted: %v", m)
	}
}

// TestModelResolution: ModelFor's precedence — a per-run selection (--model) beats the
// agent-wide COOP_<AGENT>_MODEL, and clearing the selection falls back to it. The model is
// an axis of its own, never a credential property (see TestModelForTiers for the full ladder).
func TestModelResolution(t *testing.T) {
	clearAgentEnv(t)
	dir := t.TempDir()
	c := &Config{ConfigDir: dir}

	// Nothing chosen anywhere → "" (the agent CLI's own default).
	if got := c.ModelFor("claude"); got != "" {
		t.Errorf("ModelFor with nothing set = %q, want \"\"", got)
	}
	// Agent-wide env default.
	t.Setenv("COOP_CLAUDE_MODEL", "sonnet")
	if got := c.ModelFor("claude"); got != "sonnet" {
		t.Errorf("ModelFor with env = %q, want sonnet", got)
	}
	// A per-run selection (--model / COOP_LOOP_MODEL) beats everything.
	c.SetActiveModel("claude", "fable")
	if got := c.ModelFor("claude"); got != "fable" {
		t.Errorf("ModelFor with active selection = %q, want fable", got)
	}
	c.SetActiveModel("claude", "") // clearing falls back to the env default
	if got := c.ModelFor("claude"); got != "sonnet" {
		t.Errorf("ModelFor after clearing selection = %q, want sonnet", got)
	}
}

func TestTasksFiles(t *testing.T) {
	clearAgentEnv(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// Default is EMPTY: the queue set is derived downstream (taskQueues → .agent/project.yaml, else
	// .agent/tasks), so a monorepo needn't hand-maintain COOP_TASKS. An explicit COOP_TASKS overrides.
	if got := Load().TasksFiles; len(got) != 0 {
		t.Errorf("default TasksFiles = %v, want [] (derived downstream)", got)
	}
	t.Setenv("COOP_TASKS", "portal/.agent/tasks runner/.agent/tasks")
	if got := Load().TasksFiles; !slices.Equal(got, []string{"portal/.agent/tasks", "runner/.agent/tasks"}) {
		t.Errorf("COOP_TASKS list = %v", got)
	}
}

func TestNormalizeEgress(t *testing.T) {
	for _, tc := range []struct {
		in     string
		want   string
		wantOk bool
	}{
		{"open", "open", true},
		{"none", "none", true},
		{" open ", "open", true}, // stray whitespace is trimmed, not a fail-closed foot-gun
		{"\tnone\n", "none", true},
		{"None", "none", false}, // a case typo of the security toggle must fail CLOSED
		{"off", "none", false},
		{"disabled", "none", false},
		{"", "none", false},
		{"   ", "none", false}, // whitespace-only is empty → fail closed
	} {
		if got, ok := normalizeEgress(tc.in); got != tc.want || ok != tc.wantOk {
			t.Errorf("normalizeEgress(%q) = (%q,%v), want (%q,%v)", tc.in, got, ok, tc.want, tc.wantOk)
		}
	}
}

func TestLoadEgressFailsClosed(t *testing.T) {
	clearAgentEnv(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("COOP_EGRESS", "None") // a typo of "none"
	if c := Load(); c.Egress != "none" || len(c.Warnings) == 0 {
		t.Errorf("typo'd COOP_EGRESS: Egress=%q warnings=%v, want none (fail closed) + a warning", c.Egress, c.Warnings)
	}
	t.Setenv("COOP_EGRESS", "open")
	if c := Load(); c.Egress != "open" || len(c.Warnings) != 0 {
		t.Errorf("open: Egress=%q warnings=%v, want open + no warnings", c.Egress, c.Warnings)
	}
}

// ModelFor resolves through four tiers, most specific first: explicit (--model), the
// rotation target's model, the standing fallback (preset lead / COOP_LOOP_MODEL), then
// COOP_<AGENT>_MODEL. The model is its own axis — never a credential property.
func TestModelForTiers(t *testing.T) {
	clearAgentEnv(t)
	t.Setenv("COOP_CLAUDE_MODEL", "env-model")
	dir := t.TempDir()
	c := &Config{ConfigDir: dir}

	if got := c.ModelFor("claude"); got != "env-model" {
		t.Errorf("env is the floor: got %q", got)
	}
	c.SetFallbackModel("claude", "fallback")
	if got := c.ModelFor("claude"); got != "fallback" {
		t.Errorf("fallback beats env: got %q", got)
	}
	c.SetTargetModel("claude", "target")
	if got := c.ModelFor("claude"); got != "target" {
		t.Errorf("target beats fallback: got %q", got)
	}
	c.SetActiveModel("claude", "explicit")
	if got := c.ModelFor("claude"); got != "explicit" {
		t.Errorf("explicit beats target: got %q", got)
	}
	// Clearing each tier falls through to the next.
	c.SetActiveModel("claude", "")
	c.SetTargetModel("claude", "")
	if got := c.ModelFor("claude"); got != "fallback" {
		t.Errorf("cleared target falls to fallback: got %q", got)
	}
	c.SetFallbackModel("claude", "")
	if got := c.ModelFor("claude"); got != "env-model" {
		t.Errorf("cleared fallback falls to env: got %q", got)
	}
}
