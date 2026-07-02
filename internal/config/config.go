// Package config resolves Coop settings from environment variables and an
// optional conf file, with XDG-based defaults. Every COOP_* setting follows the
// same precedence: environment variable, then conf file, then built-in default.
package config

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
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
	AgentPackages string // COOP_AGENT_PACKAGES — pin/override the global agent+ACP npm specs (e.g. "@anthropic-ai/claude-code@1.2.3 …")

	Homes      bool // COOP_HOMES — mount the per-agent home dirs
	Network    bool // COOP_NETWORK — join the sibling-services network
	AutoUp     bool // COOP_AUTO_UP — auto-start sibling services (compose up) before a box when a compose file is present
	Cache      bool // COOP_CACHE — mount the shared dependency cache volume
	Preflight  bool // COOP_PREFLIGHT — run a one-shot cleanup pass (log/tasks/decisions) before `coop loop`
	Caffeinate bool // COOP_CAFFEINATE — hold a system sleep inhibitor (caffeinate on macOS) while a loop runs

	ServicesNet  string   // COOP_SERVICES_NET — override the services network name
	LoopModel    string   // COOP_LOOP_MODEL — model for loop iterations (falls back to the per-agent default)
	LoopCmd      []string // COOP_LOOP_CMD — override the loop's per-iteration command
	TasksFiles   []string // COOP_TASKS — task queue(s) the loop and `coop tasks` work (repo-relative; default .agent/tasks)
	Gate         []string // COOP_GATE — revalidation gate run in the box before a fork merge lands
	ExtraRunArgs []string // COOP_RUN_ARGS — extra args passed to the container runtime

	// Box resource/privilege caps (docker & podman; skipped on Apple `container`).
	Memory          string // COOP_MEMORY — memory cap, e.g. "4g" (empty = unset)
	CPUs            string // COOP_CPUS — cpu cap, e.g. "2" (empty = unset)
	Pids            string // COOP_PIDS — pids-limit (fork-bomb cap), default 4096; "0"/"unlimited"/"" = off
	NoNewPrivileges bool   // COOP_NO_NEW_PRIVILEGES — pass --security-opt no-new-privileges (default on)
	Egress          string // COOP_EGRESS — "open" (default, full outbound) or "none" (--network none, offline); an unrecognized value fails CLOSED to "none"

	FusionGovernor string // COOP_FUSION_GOVERNOR — default governing agent for `coop fusion`
	ConsultTimeout string // COOP_CONSULT_TIMEOUT — per-peer coop-consult timeout in seconds (default 1800, owned by the wrapper)

	Editor    string // COOP_EDITOR — editor for `coop fork review --open` (else $VISUAL/$EDITOR or a detected GUI editor)
	ReviewCmd string // COOP_REVIEW_CMD — full override for `coop fork review` (run via sh -c; gets $COOP_FORK_PATH/$COOP_FORK_NAME/$COOP_REVIEW_REF)

	// BoxHome is ~/.config/coop: the home for conf, mcp.json, and agents/.
	BoxHome string

	// Warnings are non-fatal config problems found during Load (e.g. an unrecognized COOP_EGRESS
	// that failed closed); the CLI entry point surfaces them once per invocation.
	Warnings []string

	conf map[string]string // the parsed conf file, kept for late per-agent lookups (Cmd)

	activeProfiles  map[string]string // per-run selected credential profile; AgentDir resolves to it
	defaultProfiles map[string]string // per-agent default profile (from DefaultsFile), used when none is selected

	activeModels  map[string]string // per-run selected model (--model / the loop's COOP_LOOP_MODEL)
	profileModels map[string]string // stored per-(agent,profile) default model (from ModelsFile), key "agent/profile"
}

// Cmd resolves a command setting (COOP_<NAME>_CMD) the same way Load resolves every
// other: environment variable, then conf file, then the built-in default — then splits
// it into words. It lets an agent adapter own its own default command without config
// knowing the agent set.
func (c *Config) Cmd(env, def string) []string {
	if v, ok := os.LookupEnv(env); ok {
		return shellSplit(v)
	}
	if v, ok := c.conf[env]; ok {
		return shellSplit(v)
	}
	return shellSplit(def)
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
	// flagOff is the opt-in sibling: default off, only an explicit truthy value turns it on.
	flagOff := func(key string) bool {
		switch strings.ToLower(get(key, "0")) {
		case "1", "true", "yes", "on":
			return true
		default:
			return false
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
		AgentPackages: get("COOP_AGENT_PACKAGES", ""),

		Homes:      flag("COOP_HOMES"),
		Network:    flag("COOP_NETWORK"),
		AutoUp:     flag("COOP_AUTO_UP"),
		Cache:      flag("COOP_CACHE"),
		Preflight:  flagOff("COOP_PREFLIGHT"),
		Caffeinate: flag("COOP_CAFFEINATE"),

		ServicesNet:  get("COOP_SERVICES_NET", ""),
		LoopModel:    get("COOP_LOOP_MODEL", ""),
		LoopCmd:      shellSplit(get("COOP_LOOP_CMD", "")),
		TasksFiles:   shellSplit(get("COOP_TASKS", filepath.Join(".agent", "tasks"))),
		Gate:         shellSplit(get("COOP_GATE", "")),
		ExtraRunArgs: shellSplit(get("COOP_RUN_ARGS", "")),

		Memory:          get("COOP_MEMORY", ""),
		CPUs:            get("COOP_CPUS", ""),
		Pids:            get("COOP_PIDS", "4096"),
		NoNewPrivileges: flag("COOP_NO_NEW_PRIVILEGES"),
		Egress:          get("COOP_EGRESS", "open"),

		FusionGovernor: get("COOP_FUSION_GOVERNOR", "codex"),
		ConsultTimeout: get("COOP_CONSULT_TIMEOUT", ""),

		Editor:    get("COOP_EDITOR", ""),
		ReviewCmd: get("COOP_REVIEW_CMD", ""),

		BoxHome: boxHome,
		conf:    conf,
	}

	c.MCPFile = get("COOP_MCP_FILE", filepath.Join(c.ConfigDir, "mcp.json"))
	c.MCPInBox = c.HomeInBox + "/.mcp.json"
	c.defaultProfiles = loadConfFile(c.DefaultsFile())
	c.profileModels = loadConfFile(c.ModelsFile())

	// COOP_EGRESS is a security toggle — fail CLOSED on an unrecognized value so a typo ("None",
	// "off") can't silently grant full outbound. Only the exact "open"/"none" are honored.
	if eg, ok := normalizeEgress(c.Egress); !ok {
		c.Warnings = append(c.Warnings, "COOP_EGRESS=\""+c.Egress+"\" is not recognized (use open|none) — failing closed to none (offline)")
		c.Egress = eg
	}
	return c
}

// normalizeEgress fails closed: anything other than "open"/"none" becomes "none" (offline), so a
// typo'd COOP_EGRESS never silently grants full outbound. ok reports whether v was recognized.
// Surrounding whitespace is trimmed first so a stray-space value (COOP_EGRESS=" open ", common
// from a config line or copy-paste) is honored instead of silently failing closed; a genuine
// case/spelling variant ("None", "off") still fails closed with a warning.
func normalizeEgress(v string) (egress string, ok bool) {
	v = strings.TrimSpace(v)
	switch v {
	case "open", "none":
		return v, true
	default:
		return "none", false
	}
}

// EnvFile is the optional file of KEY=VALUE pairs passed into every box.
func (c *Config) EnvFile() string { return filepath.Join(c.ConfigDir, "env") }

// Instructions is the optional shared instruction file wired into each agent.
func (c *Config) Instructions() string { return filepath.Join(c.ConfigDir, "INSTRUCTIONS.md") }

// DefaultProfile is the credential profile used when none is selected; profilesSubdir
// is the folder under an agent dir that holds the named profiles.
const (
	DefaultProfile = "default"
	profilesSubdir = "profiles"
)

// AgentDir is the host folder mounted at the box's ~/.<agent>: the active profile's
// credential + session dir (see AgentProfileDir). Defaults to the "default" profile.
func (c *Config) AgentDir(agent string) string {
	return c.AgentProfileDir(agent, c.activeProfile(agent))
}

// activeProfile resolves which profile AgentDir uses for agent: a per-run selection wins
// (a --profile login, or the loop's rotation), then the agent's marked default, then the
// built-in DefaultProfile (so an unmarked single login still resolves to the legacy slot).
func (c *Config) activeProfile(agent string) string {
	if p := c.activeProfiles[agent]; p != "" {
		return p
	}
	return c.DefaultProfileOf(agent)
}

// ActiveProfile is the exported reader for activeProfile — the profile a run of agent resolves
// to right now. Used for display (the run/loop banner names which profile is in play).
func (c *Config) ActiveProfile(agent string) string { return c.activeProfile(agent) }

// DefaultsFile marks each agent's default profile (KEY=VALUE, agent=profile): the profile
// an interactive run uses when none is given on the CLI. Managed by `coop profiles default`.
func (c *Config) DefaultsFile() string { return filepath.Join(c.ConfigDir, "defaults") }

// DefaultProfileOf returns the profile marked default for agent, or the built-in
// DefaultProfile when none is marked.
func (c *Config) DefaultProfileOf(agent string) string {
	if p := c.defaultProfiles[agent]; p != "" {
		return p
	}
	return DefaultProfile
}

// SetDefaultProfile marks name as agent's default profile, persisting it to DefaultsFile and
// updating the in-memory view. The load→modify→write runs under WithLock so concurrent writers
// (e.g. two `coop profiles default` for different agents) don't lose each other's edit.
func (c *Config) SetDefaultProfile(agent, name string) error {
	if err := os.MkdirAll(c.ConfigDir, 0o700); err != nil {
		return err
	}
	err := WithLock(c.DefaultsFile(), func() error {
		m := loadConfFile(c.DefaultsFile())
		m[agent] = name
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		for _, k := range keys {
			b.WriteString(k + "=" + m[k] + "\n")
		}
		return WriteFileAtomic(c.DefaultsFile(), []byte(b.String()))
	})
	if err != nil {
		return err
	}
	if c.defaultProfiles == nil {
		c.defaultProfiles = map[string]string{}
	}
	c.defaultProfiles[agent] = name
	return nil
}

// WithLock runs fn while holding an exclusive advisory lock (flock) on a sibling <path>.lock, so a
// load→modify→write of path can't lose a concurrent process's update. Best-effort: if the lock file
// or flock can't be obtained, fn still runs (these are convenience configs, not a critical section,
// and blocking the command would be worse). Linux/darwin only — coop's only targets.
func WithLock(path string, fn func() error) error {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fn()
	}
	defer f.Close()
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX) != nil {
		return fn()
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

// WriteFileAtomic writes data to path via a uniquely-named temp file in the same dir, then renames
// it into place. The rename is atomic (no truncated file on a crash) and a UNIQUE temp name means
// concurrent writers don't clobber a shared "<path>.tmp" mid-write.
func WriteFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(name)
		if werr != nil {
			return werr
		}
		return cerr
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// SetActiveProfile selects which credential profile of agent AgentDir resolves to —
// and therefore which one the box mounts and the adapters read. The loop calls this to
// rotate between subscriptions; an empty name resets to the default.
func (c *Config) SetActiveProfile(agent, name string) {
	if c.activeProfiles == nil {
		c.activeProfiles = map[string]string{}
	}
	c.activeProfiles[agent] = name
}

// ModelsFile stores each profile's default model (KEY=VALUE, <agent>/<profile>=model): the
// model a run resolves to when none is given on the CLI. Managed by `coop models default`.
func (c *Config) ModelsFile() string { return filepath.Join(c.ConfigDir, "models") }

// SetActiveModel selects the model a run of agent uses, overriding every stored default —
// the CLI's --model flag (and the loop's COOP_LOOP_MODEL) land here. Empty clears the
// selection, falling back to the profile/agent defaults.
func (c *Config) SetActiveModel(agent, model string) {
	if c.activeModels == nil {
		c.activeModels = map[string]string{}
	}
	c.activeModels[agent] = model
}

// ProfileModelOf returns the model marked as agent's default for the named profile
// (via `coop models default`), or "" when none is marked.
func (c *Config) ProfileModelOf(agent, profile string) string {
	if profile == "" {
		profile = DefaultProfile
	}
	return c.profileModels[agent+"/"+profile]
}

// SetProfileModel marks model as the default for agent's named profile, persisting it to
// ModelsFile and updating the in-memory view; an empty model clears the mark. Locked
// load→modify→write like SetDefaultProfile, so concurrent edits don't lose each other.
func (c *Config) SetProfileModel(agent, profile, model string) error {
	if profile == "" {
		profile = DefaultProfile
	}
	if err := os.MkdirAll(c.ConfigDir, 0o700); err != nil {
		return err
	}
	key := agent + "/" + profile
	err := WithLock(c.ModelsFile(), func() error {
		m := loadConfFile(c.ModelsFile())
		if model == "" {
			delete(m, key)
		} else {
			m[key] = model
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		for _, k := range keys {
			b.WriteString(k + "=" + m[k] + "\n")
		}
		return WriteFileAtomic(c.ModelsFile(), []byte(b.String()))
	})
	if err != nil {
		return err
	}
	if c.profileModels == nil {
		c.profileModels = map[string]string{}
	}
	if model == "" {
		delete(c.profileModels, key)
	} else {
		c.profileModels[key] = model
	}
	return nil
}

// AgentModelDefault is the agent-wide default model from COOP_<AGENT>_MODEL (env, then
// conf file — the same precedence as every other setting), or "". Resolved late (not in
// Load) because config doesn't know the agent set.
func (c *Config) AgentModelDefault(agent string) string {
	key := "COOP_" + strings.ToUpper(agent) + "_MODEL"
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return c.conf[key]
}

// ModelFor resolves the model a run of agent should use, most specific first: the per-run
// selection (--model, or the loop applying COOP_LOOP_MODEL), then the ACTIVE profile's
// marked default (`coop models default` — re-resolved per call, so the loop's profile
// rotation picks up each profile's own mark), then the agent-wide COOP_<AGENT>_MODEL.
// "" means no coop-level choice — the agent CLI's own default runs (including a model
// baked into COOP_<AGENT>_CMD, which the adapters never override; see agent.withModel).
func (c *Config) ModelFor(agent string) string {
	if m := c.activeModels[agent]; m != "" {
		return m
	}
	if m := c.ProfileModelOf(agent, c.activeProfile(agent)); m != "" {
		return m
	}
	return c.AgentModelDefault(agent)
}

// AgentProfileDir is the host folder for one named credential profile of an agent.
// Profiles live at <ConfigDir>/<agent>/profiles/<name>/. For back-compat, until a named
// profile exists (no profiles/ dir yet) the "default" profile IS the legacy flat
// <ConfigDir>/<agent>/ — so an existing single login keeps working without a file move.
func (c *Config) AgentProfileDir(agent, name string) string {
	if name == "" {
		name = DefaultProfile
	}
	base := filepath.Join(c.ConfigDir, agent)
	if name == DefaultProfile && !dirExists(filepath.Join(base, profilesSubdir)) {
		return base // legacy flat layout: the agent dir itself is the default profile
	}
	return filepath.Join(base, profilesSubdir, name)
}

// Profiles lists agent's credential profile names. In the migrated layout it reads the
// profiles/ dir; in the legacy flat layout it reports a single "default" when the agent
// dir exists, and nothing when the agent has never been used.
func (c *Config) Profiles(agent string) []string {
	entries, err := os.ReadDir(filepath.Join(c.ConfigDir, agent, profilesSubdir))
	if err != nil {
		if dirExists(filepath.Join(c.ConfigDir, agent)) {
			return []string{DefaultProfile}
		}
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names
}

// MCPActive reports whether the shared mcp.json declares at least one server, so an agent
// should be wired to it. An absent, unparseable, or empty (no-server) file is inactive — so a
// scaffolded stub stays a pure no-op until you add a server.
func (c *Config) MCPActive() bool {
	data, err := os.ReadFile(c.MCPFile)
	if err != nil {
		return false
	}
	var f struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return false
	}
	return len(f.MCPServers) > 0
}

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

// shellSplit splits a command string into argv the way a shell word-splits it:
// whitespace separates words, single and double quotes group, and a backslash
// escapes the next character (outside single quotes). It does NOT run a shell — no
// globbing, no variable expansion — so a quoted command setting like
//
//	COOP_GATE='bash -lc "npm test && npm run lint"'
//
// becomes the three args [bash, -lc, "npm test && npm run lint"], not five. Empty or
// all-whitespace input yields a nil slice; an unterminated quote is tolerated (its
// contents run to the end of the string).
func shellSplit(s string) []string {
	const (
		bare = iota
		inSingle
		inDouble
	)
	var args []string
	var cur strings.Builder
	state, started, escaped := bare, false, false
	for _, r := range s {
		switch {
		case escaped: // previous char was a backslash
			cur.WriteRune(r)
			escaped = false
		case state == inSingle:
			if r == '\'' {
				state = bare
			} else {
				cur.WriteRune(r)
			}
		case state == inDouble:
			switch r {
			case '\\':
				escaped = true
			case '"':
				state = bare
			default:
				cur.WriteRune(r)
			}
		case r == '\\':
			escaped, started = true, true
		case r == '\'':
			state, started = inSingle, true
		case r == '"':
			state, started = inDouble, true
		case r == ' ', r == '\t', r == '\n', r == '\r':
			if started {
				args = append(args, cur.String())
				cur.Reset()
				started = false
			}
		default:
			cur.WriteRune(r)
			started = true
		}
	}
	if escaped { // a trailing backslash is taken literally
		cur.WriteByte('\\')
		started = true
	}
	if started {
		args = append(args, cur.String())
	}
	if len(args) == 0 {
		return nil
	}
	return args
}

func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// loadConfFile parses a simple KEY=VALUE file: blank lines and #-comments are
// ignored, a leading "export " is allowed, and one matched pair of surrounding quotes
// is stripped. A missing file yields an empty map — the conf file is always optional.
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
		// Strip one matched pair of surrounding quotes (not a greedy cutset), so a
		// shell-quoted command value keeps its inner quotes for shellSplit:
		//   COOP_GATE=bash -lc "npm test"   stays intact (no outer pair to strip).
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		}
		if key != "" {
			out[key] = val
		}
	}
	return out
}
