// Package config resolves Coop settings from environment variables and an
// optional conf file, with XDG-based defaults. Every COOP_* setting follows the
// same precedence: environment variable, then conf file, then built-in default.
package config

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// Config is the fully-resolved settings for one invocation. It is computed once
// in Load and passed down; process-only presentation settings are the narrow
// exception because the leaf ui package cannot import config.
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

	Homes         bool // COOP_HOMES — mount the per-agent home dirs
	Network       bool // COOP_NETWORK — join the sibling-services network
	AutoUp        bool // COOP_AUTO_UP — auto-start sibling services (compose up) before a box when a compose file is present
	Cache         bool // COOP_CACHE — mount the shared dependency cache volume
	Caffeinate    bool // COOP_CAFFEINATE — hold a system sleep inhibitor (caffeinate on macOS) while a loop runs
	NoUpdateCheck bool // COOP_NO_UPDATE_CHECK — opt out of the once-a-day update-available check
	StreamTrace   bool // COOP_STREAM_TRACE — persist raw and rendered output for streaming loop attempts
	ACPWarm       bool // COOP_ACP_WARM — keep alternate ACP providers warm (environment only)

	ServicesNet    string   // COOP_SERVICES_NET — override the services network name
	ACPCarryTokens int      // COOP_ACP_CARRY_TOKENS — per-session budget (≈tokens, ~4 bytes each) for the conversation carried across an ACP provider switch (default 200000)
	TasksFiles     []string // COOP_TASKS — explicit task queue(s) override; empty = derive from .agent/project.yaml (subprojects) else .agent/tasks
	Gate           []string // COOP_GATE — revalidation gate run in the box before a fork merge lands
	ExtraRunArgs   []string // COOP_RUN_ARGS — extra args passed to the container runtime

	// Box resource/privilege caps (docker & podman; skipped on Apple `container`).
	Memory          string // COOP_MEMORY — memory cap, e.g. "4g" (empty = unset)
	CPUs            string // COOP_CPUS — cpu cap, e.g. "2" (empty = unset)
	Pids            string // COOP_PIDS — pids-limit (fork-bomb cap), default 4096; "0"/"unlimited"/"" = off
	NoNewPrivileges bool   // COOP_NO_NEW_PRIVILEGES — pass --security-opt no-new-privileges (default on)
	Egress          string // COOP_EGRESS — "open" (default, full outbound) or "none" (--network none, offline)

	ConsultTimeout string // COOP_CONSULT_TIMEOUT — per-peer coop-consult timeout in seconds (empty/0 = unlimited)

	// ProviderTimeouts is the INTERNAL provider-attempt watchdog override
	// ("start=2s,idle=3s,tool=6s"), read only so deterministic fixture tests can shorten the
	// fixed deadlines; it is deliberately not a documented user knob.
	ProviderTimeouts string // COOP_PROVIDER_TIMEOUTS

	Editor    string // COOP_EDITOR — editor for `coop fork review --open` (else $VISUAL/$EDITOR or a detected GUI editor)
	ReviewCmd string // COOP_REVIEW_CMD — full override for `coop fork review` (run via sh -c; gets $COOP_FORK_PATH/$COOP_FORK_NAME/$COOP_REVIEW_REF)

	// BoxHome is ~/.config/coop: the home for conf, mcp.json, and agents/.
	BoxHome string

	conf     map[string]string // the parsed conf file, kept for late per-agent lookups (Cmd)
	explicit map[string]bool   // keys the user explicitly set (env or conf) — vs built-in defaults

	activeProfiles  map[string]string // per-run selected credential profile; AgentDir resolves to it
	defaultProfiles map[string]string // per-agent default profile (from DefaultsFile), used when none is selected

	activeModels   map[string]string // per-run explicit one-off model — the top tier
	targetModels   map[string]string // the active pool target's model (credential@model), below explicit
	fallbackModels map[string]string // standing default (a preset lead's model), below a target

	activeEfforts   map[string]string // per-run EXPLICIT reasoning effort (target /effort) — the top tier
	targetEfforts   map[string]string // the active rotation target's effort, below explicit
	fallbackEfforts map[string]string // standing default (a preset lead's effort), below a target
}

// Explicit reports whether the user explicitly set key (env var or conf file) — false when the
// loaded value is just the built-in default. The .agent/project.yaml box: overlay (box.Run) uses
// it so a committed repo policy fills only the slots the user left unset: an explicit setting
// always wins, and — since the built-in egress default is the loosest — a repo can only tighten.
func (c *Config) Explicit(key string) bool { return c.explicit[key] }

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

// Load resolves the configuration from the environment and conf file. A genuinely absent default
// file is optional; an explicitly selected file or a present invalid file is an operator error.
func Load() (*Config, error) {
	boxHome := filepath.Join(xdgConfigHome(), "coop")
	confPath, explicitConf := os.LookupEnv("COOP_CONF")
	if !explicitConf {
		confPath = filepath.Join(boxHome, "coop.conf")
	}
	conf, err := loadMainConf(confPath, explicitConf)
	if err != nil {
		return nil, err
	}

	// explicit records every key the user actually SET (env or conf file), as opposed to one that
	// fell to its built-in default — so a committed .agent/project.yaml box: policy can fill the
	// unset ones without ever overriding the user's own choice (Config.Explicit; box.Run overlays).
	explicit := map[string]bool{}
	get := func(key, def string) string {
		if v, ok := os.LookupEnv(key); ok {
			explicit[key] = true
			return v
		}
		if v, ok := conf[key]; ok {
			explicit[key] = true
			return v
		}
		return def
	}
	flag := func(key string, def bool) (bool, error) {
		raw := get(key, strconv.FormatBool(def))
		value, err := parseBool(raw)
		if err != nil {
			return false, configValueError(key, raw, err)
		}
		return value, nil
	}
	positiveInt := func(key string, def int) (int, error) {
		raw := get(key, strconv.Itoa(def))
		value, err := parsePositiveInt(raw)
		if err != nil {
			return 0, configValueError(key, raw, err)
		}
		return value, nil
	}

	homes, err := flag("COOP_HOMES", true)
	if err != nil {
		return nil, err
	}
	network, err := flag("COOP_NETWORK", true)
	if err != nil {
		return nil, err
	}
	autoUp, err := flag("COOP_AUTO_UP", true)
	if err != nil {
		return nil, err
	}
	cache, err := flag("COOP_CACHE", true)
	if err != nil {
		return nil, err
	}
	caffeinate, err := flag("COOP_CAFFEINATE", true)
	if err != nil {
		return nil, err
	}
	noUpdateCheck, err := flag("COOP_NO_UPDATE_CHECK", false)
	if err != nil {
		return nil, err
	}
	streamTrace, err := flag("COOP_STREAM_TRACE", false)
	if err != nil {
		return nil, err
	}
	acpWarm, err := environmentFlag("COOP_ACP_WARM", true)
	if err != nil {
		return nil, err
	}
	if _, err := environmentFlag("COOP_SPINNER", true); err != nil {
		return nil, err
	}
	noNewPrivileges, err := flag("COOP_NO_NEW_PRIVILEGES", true)
	if err != nil {
		return nil, err
	}
	carryTokens, err := positiveInt("COOP_ACP_CARRY_TOKENS", 200_000)
	if err != nil {
		return nil, err
	}
	if carryTokens > math.MaxInt/4 {
		raw := get("COOP_ACP_CARRY_TOKENS", strconv.Itoa(200_000))
		return nil, configValueError("COOP_ACP_CARRY_TOKENS", raw, fmt.Errorf("must be at most %d", math.MaxInt/4))
	}
	pids, err := parsePids(get("COOP_PIDS", "4096"))
	if err != nil {
		return nil, configValueError("COOP_PIDS", get("COOP_PIDS", "4096"), err)
	}
	egress, err := parseEgress(get("COOP_EGRESS", "open"))
	if err != nil {
		return nil, configValueError("COOP_EGRESS", get("COOP_EGRESS", "open"), err)
	}
	consultTimeout, err := parseConsultTimeout(get("COOP_CONSULT_TIMEOUT", "0"))
	if err != nil {
		return nil, configValueError("COOP_CONSULT_TIMEOUT", get("COOP_CONSULT_TIMEOUT", "0"), err)
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

		Homes:         homes,
		Network:       network,
		AutoUp:        autoUp,
		Cache:         cache,
		Caffeinate:    caffeinate,
		NoUpdateCheck: noUpdateCheck,
		StreamTrace:   streamTrace,
		ACPWarm:       acpWarm,

		ServicesNet:    get("COOP_SERVICES_NET", ""),
		ACPCarryTokens: carryTokens,
		TasksFiles:     shellSplit(get("COOP_TASKS", "")), // empty → taskQueues derives from .agent/project.yaml
		Gate:           shellSplit(get("COOP_GATE", "")),
		ExtraRunArgs:   shellSplit(get("COOP_RUN_ARGS", "")),

		Memory:          get("COOP_MEMORY", ""),
		CPUs:            get("COOP_CPUS", ""),
		Pids:            pids,
		NoNewPrivileges: noNewPrivileges,
		Egress:          egress,

		ConsultTimeout: consultTimeout,

		ProviderTimeouts: get("COOP_PROVIDER_TIMEOUTS", ""),

		Editor:    get("COOP_EDITOR", ""),
		ReviewCmd: get("COOP_REVIEW_CMD", ""),

		BoxHome:  boxHome,
		conf:     conf,
		explicit: explicit,
	}

	c.MCPFile = get("COOP_MCP_FILE", filepath.Join(c.ConfigDir, "mcp.json"))
	c.MCPInBox = c.HomeInBox + "/.mcp.json"
	c.defaultProfiles = loadDefaultsFile(c.DefaultsFile())
	return c, nil
}

// GlobalPresetsDir is the per-user presets root (~/.config/coop/presets): a second
// location `coop <preset>` and `coop presets` load from when the repo doesn't define
// the name (a repo preset wins a collision). COOP_PRESETS_DIR overrides the path — free
// testability, and it lets a user relocate the folder.
func (c *Config) GlobalPresetsDir() string {
	if v, ok := os.LookupEnv("COOP_PRESETS_DIR"); ok {
		return v
	}
	if v, ok := c.conf["COOP_PRESETS_DIR"]; ok {
		return v
	}
	return filepath.Join(c.BoxHome, "presets")
}

// ACPCarryBytes is the per-session budget, in bytes, for the conversation carried across an
// ACP provider switch — COOP_ACP_CARRY_TOKENS at ~4 bytes per token, defaulting here too so a
// Config built directly (tests) doesn't silently zero the budget.
func (c *Config) ACPCarryBytes() int {
	if c.ACPCarryTokens <= 0 {
		return 200_000 * 4
	}
	return c.ACPCarryTokens * 4
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
// (a target's @account, or the loop's rotation), then the agent's marked default, then the
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
// an interactive run uses when none is given on the CLI. Managed by
// `coop credentials <agent> <credential> default`.
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
// (e.g. two `coop credentials default` for different agents) don't lose each other's edit.
func (c *Config) SetDefaultProfile(agent, name string) error {
	if err := os.MkdirAll(c.ConfigDir, 0o700); err != nil {
		return err
	}
	err := WithLock(c.DefaultsFile(), func() error {
		m := loadDefaultsFile(c.DefaultsFile())
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
// load→modify→write of path can't lose a concurrent process's update. Fails CLOSED: when the lock
// file can't be opened or flocked, fn does NOT run and the caller gets an error naming the lock —
// these files pick which credential an unattended run uses, so a silently lost update is worse than
// a refused command (on a healthy system flock on a local file effectively never fails, so this
// costs nothing in normal operation). Linux/darwin only — coop's only targets.
func WithLock(path string, fn func() error) error {
	lock := path + ".lock"
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open config lock %s: %w", lock, err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock config file %s: %w", lock, err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

// WriteFileAtomic writes data to path via a uniquely-named temp file in the same dir, fsyncs it,
// then renames it into place and fsyncs the parent dir. The rename is atomic (no truncated file on
// a crash) and a UNIQUE temp name means concurrent writers don't clobber a shared "<path>.tmp"
// mid-write; the two fsyncs mean a power loss leaves either the old contents or the new ones —
// never a live-but-empty file, which for a credential pointer reads back as "unset" and silently
// changes which account the next run picks.
func WriteFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	if werr == nil {
		werr = tmp.Sync()
	}
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
	if err := syncDir(filepath.Dir(path)); err != nil {
		// The new contents are already in place — only their survival of a crash is unconfirmed.
		// Say that, so this doesn't read as "the write was lost" and get retried blind.
		return fmt.Errorf("wrote %s but could not sync its directory: %w", path, err)
	}
	return nil
}

// syncDir fsyncs a directory so a rename into it survives a power loss: the renamed file's contents
// are already durable, but without this the directory entry pointing at them may not be.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
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

// SetActiveModel selects the model a run of agent uses, overriding every other tier —
// only an explicit one-off choice lands here. Empty
// clears the selection, falling back to the lower tiers.
func (c *Config) SetActiveModel(agent, model string) {
	if c.activeModels == nil {
		c.activeModels = map[string]string{}
	}
	c.activeModels[agent] = model
}

// ActiveModel returns the run's EXPLICIT top-tier model for agent ("" when none), so a caller
// that temporarily overrides it (the loop swapping in a step's loop.yaml model for the review pass) can
// snapshot and restore the prior value.
func (c *Config) ActiveModel(agent string) string { return c.activeModels[agent] }

// SetTargetModel selects the active rotation target's model — a loop applies it at start and
// on every rotation, so an `opus@work` target runs opus until the rotation moves on. It
// ranks below an explicit --model and above every static default. Empty clears it (a bare
// credential target), so resolution falls through to the fallback/env tiers.
func (c *Config) SetTargetModel(agent, model string) {
	if c.targetModels == nil {
		c.targetModels = map[string]string{}
	}
	c.targetModels[agent] = model
}

// SetFallbackModel sets the run's standing default model — a preset lead's model — ranking
// below an explicit --model and any rotation target's model, but above COOP_<AGENT>_MODEL.
func (c *Config) SetFallbackModel(agent, model string) {
	if c.fallbackModels == nil {
		c.fallbackModels = map[string]string{}
	}
	c.fallbackModels[agent] = model
}

// FallbackModel returns the run's standing default model for agent ("" when none set).
func (c *Config) FallbackModel(agent string) string { return c.fallbackModels[agent] }

// splitModelEffort splits a model spec "model[/effort]" — the shared shape of the COOP_*_MODEL
// env knobs and the target grammar's model+effort slot — into its parts; either may be empty.
// One value carries both axes, so there is no separate COOP_*_EFFORT env var.
func splitModelEffort(spec string) (model, effort string) {
	m, e, _ := strings.Cut(spec, "/")
	return strings.TrimSpace(m), strings.TrimSpace(e)
}

// agentModelSpec is the raw COOP_<AGENT>_MODEL value (env, then conf file — the same precedence
// as every other setting), or "". Its shape is model[/effort], so it seeds BOTH the model and
// the effort default.
func (c *Config) agentModelSpec(agent string) string {
	key := "COOP_" + strings.ToUpper(agent) + "_MODEL"
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return c.conf[key]
}

// AgentModelDefault is the agent-wide default model — the model part of COOP_<AGENT>_MODEL
// (model[/effort]), or "". Resolved late (not in Load) because config doesn't know the agent set.
func (c *Config) AgentModelDefault(agent string) string {
	m, _ := splitModelEffort(c.agentModelSpec(agent))
	return m
}

// ModelFor resolves the model a run of agent should use, most specific first:
//  1. the explicit per-run choice,
//  2. the active rotation target's model (a loop's `opus@work` — re-set on each rotation),
//  3. the run's standing default (a preset lead's model),
//  4. the agent-wide COOP_<AGENT>_MODEL.
//
// The model is its own axis — never a property of a credential (a credential is just an
// account). "" means no coop-level choice — the agent CLI's own default runs (including a
// model baked into COOP_<AGENT>_CMD, which the adapters never override; see agent.withModel).
func (c *Config) ModelFor(agent string) string {
	if m := c.activeModels[agent]; m != "" {
		return m
	}
	if m := c.targetModels[agent]; m != "" {
		return m
	}
	if m := c.fallbackModels[agent]; m != "" {
		return m
	}
	return c.AgentModelDefault(agent)
}

// SetActiveEffort selects the reasoning effort a run of agent uses, overriding every other
// tier — only an EXPLICIT target /effort lands here. Empty clears it, falling back to the
// lower tiers.
func (c *Config) SetActiveEffort(agent, effort string) {
	if c.activeEfforts == nil {
		c.activeEfforts = map[string]string{}
	}
	c.activeEfforts[agent] = effort
}

// ActiveEffort returns the run's EXPLICIT top-tier effort for agent ("" when none), so a
// caller that temporarily overrides it (the review pass) can snapshot and restore it.
func (c *Config) ActiveEffort(agent string) string { return c.activeEfforts[agent] }

// SetTargetEffort selects the active rotation target's effort — applied at loop start and on
// every rotation, below an explicit target /effort and above every static default. Empty
// clears it.
func (c *Config) SetTargetEffort(agent, effort string) {
	if c.targetEfforts == nil {
		c.targetEfforts = map[string]string{}
	}
	c.targetEfforts[agent] = effort
}

// SetFallbackEffort sets the run's standing default effort — a preset lead's effort —
// below a target's effort but above COOP_<AGENT>_MODEL's.
func (c *Config) SetFallbackEffort(agent, effort string) {
	if c.fallbackEfforts == nil {
		c.fallbackEfforts = map[string]string{}
	}
	c.fallbackEfforts[agent] = effort
}

// FallbackEffort returns the run's standing default effort for agent ("" when none set).
func (c *Config) FallbackEffort(agent string) string { return c.fallbackEfforts[agent] }

// AgentEffortDefault is the agent-wide default reasoning effort — the effort part of
// COOP_<AGENT>_MODEL (its shape is model[/effort], e.g. "opus/high"), or "". One var carries
// both axes, so there is no separate COOP_<AGENT>_EFFORT.
func (c *Config) AgentEffortDefault(agent string) string {
	_, e := splitModelEffort(c.agentModelSpec(agent))
	return e
}

// EffortFor resolves the reasoning effort a run of agent should use, most specific first:
//  1. the explicit per-run choice (target /effort),
//  2. the active rotation target's effort,
//  3. the run's standing default (a preset lead's effort),
//  4. the agent-wide COOP_<AGENT>_MODEL's /effort.
//
// Like the model, effort is its own axis — never a property of a credential. "" means no
// coop-level choice, so the agent CLI's own default applies (see agent.withEffort).
func (c *Config) EffortFor(agent string) string {
	if e := c.activeEfforts[agent]; e != "" {
		return e
	}
	if e := c.targetEfforts[agent]; e != "" {
		return e
	}
	if e := c.fallbackEfforts[agent]; e != "" {
		return e
	}
	return c.AgentEffortDefault(agent)
}

// AgentProfileDir is the host folder for one named credential profile of an agent:
// <ConfigDir>/<agent>/profiles/<name>/. "default" is just the profile named "default" —
// every login lives under profiles/, so this always resolves there.
func (c *Config) AgentProfileDir(agent, name string) string {
	if name == "" {
		name = DefaultProfile
	}
	return filepath.Join(c.ConfigDir, agent, profilesSubdir, name)
}

// Profiles lists agent's credential profile names from its profiles/ dir, or nothing when the
// agent has never been used (or has no profiles/ dir yet).
func (c *Config) Profiles(agent string) []string {
	entries, err := os.ReadDir(filepath.Join(c.ConfigDir, agent, profilesSubdir))
	if err != nil {
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

// ShellSplit exposes shellSplit so other packages can split a committed command setting (e.g. the
// .agent/project.yaml gate:) into argv exactly as Load splits COOP_GATE — one splitter, one rule.
func ShellSplit(s string) []string { return shellSplit(s) }

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

const maxMainConfLineBytes = 64 << 10

var mainConfigKeys = map[string]struct{}{
	"COOP_ACP_CARRY_TOKENS":  {},
	"COOP_AGENT_PACKAGES":    {},
	"COOP_AUTO_UP":           {},
	"COOP_BASE_IMAGE":        {},
	"COOP_CACHE":             {},
	"COOP_CAFFEINATE":        {},
	"COOP_CONFIG_DIR":        {},
	"COOP_CONSULT_TIMEOUT":   {},
	"COOP_CPUS":              {},
	"COOP_EDITOR":            {},
	"COOP_EGRESS":            {},
	"COOP_GATE":              {},
	"COOP_HOMES":             {},
	"COOP_HOME_IN_BOX":       {},
	"COOP_IMAGE":             {},
	"COOP_MCP_FILE":          {},
	"COOP_MEMORY":            {},
	"COOP_NETWORK":           {},
	"COOP_NO_NEW_PRIVILEGES": {},
	"COOP_NO_UPDATE_CHECK":   {},
	"COOP_PIDS":              {},
	"COOP_PRESETS_DIR":       {},
	"COOP_PROVIDER_TIMEOUTS": {},
	"COOP_REPO":              {},
	"COOP_REVIEW_CMD":        {},
	"COOP_RUNTIME":           {},
	"COOP_RUN_ARGS":          {},
	"COOP_SERVICES_NET":      {},
	"COOP_SHELL":             {},
	"COOP_STREAM_TRACE":      {},
	"COOP_TASKS":             {},
	"COOP_WORKDIR":           {},
}

var retiredMainConfigKeys = map[string]struct{}{
	"COOP_LOOP_CMD":          {},
	"COOP_LOOP_MODEL":        {},
	"COOP_MAX_REVIEW_ROUNDS": {},
	"COOP_PREFLIGHT":         {},
	"COOP_REVIEW_MODEL":      {},
}

var booleanMainConfigKeys = map[string]struct{}{
	"COOP_AUTO_UP":           {},
	"COOP_CACHE":             {},
	"COOP_CAFFEINATE":        {},
	"COOP_HOMES":             {},
	"COOP_NETWORK":           {},
	"COOP_NO_NEW_PRIVILEGES": {},
	"COOP_NO_UPDATE_CHECK":   {},
	"COOP_STREAM_TRACE":      {},
}

// adapterConfigKeys is populated by the agent registry during package initialization. That
// keeps the provider set in internal/agent while still making a misspelled coop.conf key fail
// closed. Registration is complete before Load can run.
var adapterConfigKeys = map[string]struct{}{}

// RegisterAdapterConfig registers the two coop.conf keys owned by an agent adapter. The agent
// registry calls it from its existing single registration point, so adding an adapter does not
// require a second provider list in config.
func RegisterAdapterConfig(name string) {
	prefix := "COOP_" + strings.ToUpper(name)
	adapterConfigKeys[prefix+"_CMD"] = struct{}{}
	adapterConfigKeys[prefix+"_MODEL"] = struct{}{}
}

// loadMainConf reads the operator-owned coop.conf. Only the implicit default may be absent;
// once a path is explicit or exists, malformed state is an error rather than permission to fall
// back to Coop's comparatively permissive defaults.
func loadMainConf(path string, explicit bool) (map[string]string, error) {
	out := map[string]string{}
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) && !explicit {
			return out, nil
		}
		return nil, mainConfFileError(path, explicit, err)
	}
	if !info.Mode().IsRegular() {
		return nil, mainConfFileError(path, explicit, fmt.Errorf("expected a regular file"))
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, mainConfFileError(path, explicit, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, maxMainConfLineBytes), maxMainConfLineBytes)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, mainConfLineError(path, lineNumber, "expected KEY=VALUE")
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" {
			return nil, mainConfLineError(path, lineNumber, "configuration key is empty")
		}
		if _, exists := out[key]; exists {
			return nil, mainConfLineError(path, lineNumber, "duplicate configuration key %s", key)
		}
		if err := validateMainConfigKey(key); err != nil {
			return nil, mainConfLineError(path, lineNumber, "%v", err)
		}
		if len(value) > 0 && (value[0] == '\'' || value[0] == '"') {
			if len(value) < 2 || value[len(value)-1] != value[0] {
				return nil, mainConfLineError(path, lineNumber, "unmatched outer quote for %s", key)
			}
			value = value[1 : len(value)-1]
		}
		if err := validateMainConfigValue(key, value); err != nil {
			return nil, mainConfLineError(path, lineNumber, "%s=%q: %v", key, value, err)
		}
		out[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, mainConfLineError(path, lineNumber+1, "read line: %v", err)
	}
	return out, nil
}

func mainConfFileError(path string, explicit bool, err error) error {
	if explicit {
		return fmt.Errorf("read coop config %s: %w; create or fix that file, or unset or repoint COOP_CONF", path, err)
	}
	return fmt.Errorf("read coop config %s: %w; fix or remove that file", path, err)
}

func mainConfLineError(path string, line int, format string, args ...any) error {
	return fmt.Errorf("%s:%d: %s", path, line, fmt.Sprintf(format, args...))
}

func validateMainConfigKey(key string) error {
	if _, retired := retiredMainConfigKeys[key]; retired {
		return fmt.Errorf("%s is retired", key)
	}
	if _, ok := mainConfigKeys[key]; ok {
		return nil
	}
	if _, ok := adapterConfigKeys[key]; ok {
		return nil
	}
	return fmt.Errorf("unknown configuration key %s", key)
}

func validateMainConfigValue(key, value string) error {
	if _, ok := booleanMainConfigKeys[key]; ok {
		_, err := parseBool(value)
		return err
	}
	switch key {
	case "COOP_ACP_CARRY_TOKENS":
		n, err := parsePositiveInt(value)
		if err == nil && n > math.MaxInt/4 {
			return fmt.Errorf("must be at most %d", math.MaxInt/4)
		}
		return err
	case "COOP_CONSULT_TIMEOUT":
		_, err := parseConsultTimeout(value)
		return err
	case "COOP_EGRESS":
		_, err := parseEgress(value)
		return err
	case "COOP_PIDS":
		_, err := parsePids(value)
		return err
	default:
		return nil
	}
}

func configValueError(key, value string, err error) error {
	return fmt.Errorf("%s=%q: %w", key, value, err)
}

func environmentFlag(key string, def bool) (bool, error) {
	raw, ok := os.LookupEnv(key)
	if !ok {
		return def, nil
	}
	value, err := parseBool(raw)
	if err != nil {
		return false, configValueError(key, raw, err)
	}
	return value, nil
}

func parseBool(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("expected one of 1|true|yes|on|0|false|no|off")
	}
}

func parsePositiveInt(value string) (int, error) {
	value = strings.TrimSpace(value)
	if !decimalDigits(value) {
		return 0, fmt.Errorf("expected a positive integer")
	}
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("expected a positive integer")
	}
	return n, nil
}

func parsePids(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || value == "0" || value == "unlimited" {
		return value, nil
	}
	if !decimalDigits(value) {
		return "", fmt.Errorf("expected 0, unlimited, empty, or a positive integer")
	}
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 {
		return "", fmt.Errorf("expected 0, unlimited, empty, or a positive integer")
	}
	return strconv.Itoa(n), nil
}

func parseEgress(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value != "open" && value != "none" {
		return "", fmt.Errorf("expected open or none")
	}
	return value, nil
}

func parseConsultTimeout(value string) (string, error) {
	value = strings.TrimSpace(value)
	if !decimalDigits(value) {
		return "", fmt.Errorf("expected whole seconds from 0 through 86400")
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 0 || n > 86400 {
		return "", fmt.Errorf("expected whole seconds from 0 through 86400")
	}
	if n == 0 {
		return "", nil
	}
	return strconv.Itoa(n), nil
}

func decimalDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// loadDefaultsFile parses the small agent=profile defaults file. It is provider-owned advisory
// state, not the operator policy in coop.conf; missing or malformed entries retain the historical
// best-effort behavior while the main config reader above stays strict.
func loadDefaultsFile(path string) map[string]string {
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
