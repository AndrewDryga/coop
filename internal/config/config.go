// Package config resolves Coop settings from environment variables and an
// optional conf file, with XDG-based defaults. Every COOP_* setting follows the
// same precedence: environment variable, then conf file, then built-in default.
package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// Config is the fully-resolved settings for one invocation. It is computed once
// in Load and passed down; nothing else reads the environment directly.
type Config struct {
	BaseImage string // COOP_BASE_IMAGE — shared base image tag
	Workdir   string // COOP_WORKDIR — where the repo mounts in the box (empty = its real host path)
	HomeInBox string // COOP_HOME_IN_BOX — the box user's home
	Shell     string // COOP_SHELL — `coop shell`'s shell

	ConfigDir string // COOP_CONFIG_DIR — per-agent auth + settings folder

	MCPFile  string // COOP_MCP_FILE — the one MCP source of truth
	MCPInBox string // where MCPFile mounts in the box (Claude's --mcp-config)

	RuntimeName   string // COOP_RUNTIME — "" means autodetect
	RepoOverride  string // COOP_REPO — overrides git-toplevel detection
	ImageOverride string // COOP_IMAGE — overrides image selection

	Homes   bool // COOP_HOMES — mount the per-agent home dirs
	Network bool // COOP_NETWORK — join the sibling-services network
	Cache   bool // COOP_CACHE — mount the shared dependency cache volume

	ServicesNet  string   // COOP_SERVICES_NET — override the services network name
	LoopCmd      []string // COOP_LOOP_CMD — override the loop's per-iteration command
	Gate         []string // COOP_GATE — revalidation gate run in the box before a fork merge lands
	ExtraRunArgs []string // COOP_RUN_ARGS — extra args passed to the container runtime

	FusionGovernor string // COOP_FUSION_GOVERNOR — default governing agent for `coop fusion`

	Editor    string // COOP_EDITOR — editor for `coop fork review --open` (else $VISUAL/$EDITOR or a detected GUI editor)
	ReviewCmd string // COOP_REVIEW_CMD — full override for `coop fork review` (run via sh -c; gets $COOP_FORK_PATH/$COOP_FORK_NAME/$COOP_REVIEW_REF)

	// BoxHome is ~/.config/coop: the home for conf, mcp.json, and agents/.
	BoxHome string

	conf map[string]string // the parsed conf file, kept for late per-agent lookups (Cmd)
}

// Cmd resolves a command setting (COOP_<NAME>_CMD) the same way Load resolves every
// other: environment variable, then conf file, then the built-in default — then splits
// it into words. It lets an agent adapter own its own default command without config
// knowing the agent set.
func (c *Config) Cmd(env, def string) []string {
	if v, ok := os.LookupEnv(env); ok {
		return fields(v)
	}
	if v, ok := c.conf[env]; ok {
		return fields(v)
	}
	return fields(def)
}

// Load resolves the configuration from the environment and conf file.
func Load() *Config {
	boxHome := filepath.Join(xdgConfigHome(), "coop")
	conf := loadConfFile(envOr("COOP_CONF", filepath.Join(boxHome, "coop.conf")))

	get := func(key, def string) string {
		if v, ok := os.LookupEnv(key); ok {
			return v
		}
		if v, ok := conf[key]; ok {
			return v
		}
		return def
	}
	// Toggles default on; only an explicit "0" / "false" turns them off.
	flag := func(key string) bool {
		switch strings.ToLower(get(key, "1")) {
		case "0", "false", "no", "off":
			return false
		default:
			return true
		}
	}

	c := &Config{
		BaseImage: get("COOP_BASE_IMAGE", "coop-box"),
		Workdir:   get("COOP_WORKDIR", ""),
		HomeInBox: get("COOP_HOME_IN_BOX", "/home/node"),
		Shell:     get("COOP_SHELL", "bash"),
		ConfigDir: get("COOP_CONFIG_DIR", filepath.Join(boxHome, "agents")),

		RuntimeName:   get("COOP_RUNTIME", ""),
		RepoOverride:  get("COOP_REPO", ""),
		ImageOverride: get("COOP_IMAGE", ""),

		Homes:   flag("COOP_HOMES"),
		Network: flag("COOP_NETWORK"),
		Cache:   flag("COOP_CACHE"),

		ServicesNet:  get("COOP_SERVICES_NET", ""),
		LoopCmd:      fields(get("COOP_LOOP_CMD", "")),
		Gate:         fields(get("COOP_GATE", "")),
		ExtraRunArgs: fields(get("COOP_RUN_ARGS", "")),

		FusionGovernor: get("COOP_FUSION_GOVERNOR", "codex"),

		Editor:    get("COOP_EDITOR", ""),
		ReviewCmd: get("COOP_REVIEW_CMD", ""),

		BoxHome: boxHome,
		conf:    conf,
	}

	c.MCPFile = get("COOP_MCP_FILE", filepath.Join(c.ConfigDir, "mcp.json"))
	c.MCPInBox = c.HomeInBox + "/.mcp.json"
	return c
}

// EnvFile is the optional file of KEY=VALUE pairs passed into every box.
func (c *Config) EnvFile() string { return filepath.Join(c.ConfigDir, "env") }

// Instructions is the optional shared instruction file wired into each agent.
func (c *Config) Instructions() string { return filepath.Join(c.ConfigDir, "INSTRUCTIONS.md") }

// AgentDir is the host folder mounted at the box's ~/.<agent>.
func (c *Config) AgentDir(agent string) string { return filepath.Join(c.ConfigDir, agent) }

// MCPActive reports whether a shared mcp.json exists, so an agent should be wired to it.
func (c *Config) MCPActive() bool { return fileExists(c.MCPFile) }

func xdgConfigHome() string {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".config"
	}
	return filepath.Join(home, ".config")
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

// fields splits a command string into words. Empty input yields a nil slice.
func fields(s string) []string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return nil
	}
	return f
}

func fileExists(path string) bool {
	if path == "" {
		return false
	}
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

// loadConfFile parses a simple KEY=VALUE file: blank lines and #-comments are
// ignored, a leading "export " is allowed, and surrounding quotes are stripped.
// A missing file yields an empty map — the conf file is always optional.
func loadConfFile(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"'`)
		if key != "" {
			out[key] = val
		}
	}
	return out
}
