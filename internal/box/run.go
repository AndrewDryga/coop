package box

import (
	"io"
	"os"
	"path/filepath"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/fusion"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/ui"
)

// RunSpec describes a single container run.
type RunSpec struct {
	Image   string
	Repo    string   // host repo to mount
	Workdir string   // where Repo mounts; empty defers to resolveWorkdir (the repo's real host path)
	Cmd     []string // command + args to run in the box

	Homes   bool // mount per-agent home dirs, env-file, INSTRUCTIONS, and MCP configs
	Network bool // join the sibling-services network if `coop up` created one
	Cache   bool // mount the shared dependency cache volume

	ForceNoTTY bool      // ACP: attach stdin (-i) but never allocate a tty
	Batch      bool      // loop/doctor: no tty, stdin from /dev/null
	Quiet      bool      // suppress the "shadowed N secret path(s)" line (doctor)
	Stdout     io.Writer // capture output (doctor); nil means inherit os.Stdout
	Stderr     io.Writer // capture/discard the container's stderr; nil means inherit os.Stderr
	ExtraArgs  []string  // extra runtime args for this run (e.g. doctor's probe mount)

	// FusionGovernor, when set, marks this run as fusion mode: the named agent
	// governs (fronts the session) and gets the fusion instruction merged into its
	// instruction file; its peers are consulted read-only. Empty = not fusion.
	FusionGovernor string

	// ConsultLead names the lead agent of a normal (non-fusion) run: it gets a
	// light, optional "second opinion" directive merged into its instruction file,
	// naming the authenticated peers it may consult read-only on hard calls. Scoped
	// to the lead so peers it spawns don't recurse. Empty = no consult directive.
	ConsultLead string
}

// ttyMode is how stdin and the tty are wired for a run.
type ttyMode int

const (
	ttyNone        ttyMode = iota // no -i/-t; stdin not attached (batch, piped)
	ttyInteractive                // -it; an interactive terminal
	ttyStdinOnly                  // -i; stdin attached without a tty (ACP)
)

// extraMount is a generated host file mounted read-only at a box path.
type extraMount struct{ host, box string }

// instructionFile is the agent's native global instruction filename — where coop
// mounts the shared INSTRUCTIONS.md (and, in fusion mode, the governor's augmented
// instruction) — or "" for an unknown agent. Owned by each adapter.
func instructionFile(name string) string {
	if ag, ok := agents.Get(name); ok {
		return ag.InstructionFile()
	}
	return ""
}

// Run assembles and executes one container run, shadowing secrets and wiring up
// agent homes + MCP. It returns the container's exit code (with a nil error when
// the container merely exited non-zero); a non-nil error means it never started.
func Run(cfg *config.Config, rt runtime.Runtime, spec RunSpec) (int, error) {
	if err := rt.EnsureDaemon(); err != nil {
		return -1, err
	}
	workdir := resolveWorkdir(spec, cfg)

	mounts, err := ComputeMounts(spec.Repo, workdir)
	if err != nil {
		return -1, err
	}
	if n := ShadowCount(mounts); n > 0 && !spec.Quiet {
		ui.Info("shadowed %d secret path(s)", n)
	}

	// A single empty read-only file shadows every secret file.
	decoy, err := os.CreateTemp("", "coop-decoy-")
	if err != nil {
		return -1, err
	}
	decoy.Close()
	defer os.Remove(decoy.Name())

	mode := decideTTY(spec, ui.IsTerminal(os.Stdin))
	var stdin io.Reader
	if mode == ttyInteractive || mode == ttyStdinOnly {
		stdin = os.Stdin
	}
	stdout := spec.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := spec.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	// Ensure the per-agent home dirs exist and pre-answer first-run prompts —
	// Claude's theme/trust/bypass and Codex's directory-trust — BEFORE generating
	// MCP configs, so a fresh box is ready to work and the generated Codex config
	// carries the trust entry on the very first run.
	if spec.Homes {
		for _, agent := range agents.Names() {
			os.MkdirAll(cfg.AgentDir(agent), 0o755)
		}
		ensureClaudeDefaults(cfg, workdir)
		ensureCodexDefaults(cfg, workdir)
		ensureGeminiDefaults(cfg)
	}

	// Generate MCP configs into temp files that live for the container's run.
	var tmpFiles []string
	defer func() {
		for _, f := range tmpFiles {
			os.Remove(f)
		}
	}()
	var mcpMounts []extraMount
	mcpPresent := spec.Homes && fileExists(cfg.MCPFile)
	if mcpPresent {
		// Each agent's adapter says how it consumes the shared mcp.json (a generated
		// config to mount, or none — claude reads it raw via --mcp-config, below).
		for _, name := range agents.Names() {
			ag, _ := agents.Get(name)
			gen, genErr := ag.MCP(cfg)
			if genErr != nil {
				ui.Info("mcp.json: skipped %s wiring: %v", name, genErr)
				continue
			}
			for _, m := range gen {
				p, err := writeTempFile(m.Content)
				if err != nil {
					ui.Info("mcp.json: skipped %s wiring: %v", name, err)
					continue
				}
				tmpFiles = append(tmpFiles, p)
				mcpMounts = append(mcpMounts, extraMount{p, m.BoxPath})
			}
		}
	}

	// Fusion: the governor gets the fusion instruction (consult peers + synthesize)
	// merged into its native instruction file — only the governor, so the peers it
	// spawns read their normal instructions and never recurse into a council.
	var fusionMounts []extraMount
	if spec.Homes && spec.FusionGovernor != "" {
		if file := instructionFile(spec.FusionGovernor); file != "" {
			base := governorBaseInstructions(cfg, spec.FusionGovernor, file)
			content := fusion.GovernorInstructions(base, spec.FusionGovernor, agents.Names())
			if p, err := writeTempFile(content); err != nil {
				ui.Info("fusion: skipped instruction wiring: %v", err)
			} else {
				tmpFiles = append(tmpFiles, p)
				fusionMounts = append(fusionMounts, extraMount{p, cfg.HomeInBox + "/." + spec.FusionGovernor + "/" + file})
			}
		}
	}

	// Second opinions: a normal lead may consult its authenticated peers read-only
	// on hard calls. The directive is merged into the lead's instruction file only
	// (so peers it spawns read their normal instructions and never recurse), and
	// only when a peer is actually authenticated — otherwise there's nothing to
	// consult and nothing is injected. (Fusion's stronger directive takes over when
	// FusionGovernor is set, so the two never both apply.)
	if spec.Homes && spec.FusionGovernor == "" && spec.ConsultLead != "" {
		if peers := authedPeers(cfg, spec.ConsultLead); len(peers) > 0 {
			if file := instructionFile(spec.ConsultLead); file != "" {
				base := governorBaseInstructions(cfg, spec.ConsultLead, file)
				content := fusion.LeadInstructions(base, peers)
				if p, err := writeTempFile(content); err != nil {
					ui.Info("consult: skipped instruction wiring: %v", err)
				} else {
					tmpFiles = append(tmpFiles, p)
					fusionMounts = append(fusionMounts, extraMount{p, cfg.HomeInBox + "/." + spec.ConsultLead + "/" + file})
				}
			}
		}
	}

	// Git environment: a curated ~/.gitconfig (your identity + signing off, since the
	// box holds no key) and your global gitignore, mounted into every box run. Without
	// it the agent would commit with no author and ignore none of your global patterns.
	var gitMounts []extraMount
	if spec.Homes {
		if p, err := writeTempFile(gitConfigForBox()); err == nil {
			tmpFiles = append(tmpFiles, p)
			gitMounts = append(gitMounts, extraMount{p, cfg.HomeInBox + "/.gitconfig"})
		}
		if gi := hostGlobalGitignore(); gi != "" {
			if p, err := writeTempFile(gi); err == nil {
				tmpFiles = append(tmpFiles, p)
				gitMounts = append(gitMounts, extraMount{p, cfg.HomeInBox + "/.config/git/ignore"})
			}
		}
	}

	networkName := ""
	if spec.Network {
		net := cfg.ServicesNet
		if net == "" {
			net = ServicesProject(spec.Repo) + "_default"
		}
		if rt.Silent("network", "inspect", net) {
			networkName = net
		}
	}

	args := assembleArgs(cfg, spec, mounts, decoy.Name(), workdir, mode, mcpPresent, mcpMounts, fusionMounts, gitMounts, networkName)
	return rt.Run(stdin, stdout, stderr, args...)
}

// resolveWorkdir picks where the repo mounts inside the box — and thus the
// agent's cwd. The default is the repo's real host path, so each agent's
// per-project session history (~/.<agent>/projects/<cwd>) is identical across
// `coop`, `coop loop`, and `coop acp`; a loop's thread is then visible and
// resumable when you open the same repo in an ACP editor like Zed. An explicit
// spec.Workdir (doctor's self-contained fixture) or COOP_WORKDIR (cfg.Workdir)
// overrides it, in that order.
func resolveWorkdir(spec RunSpec, cfg *config.Config) string {
	switch {
	case spec.Workdir != "":
		return spec.Workdir
	case cfg.Workdir != "":
		return cfg.Workdir
	default:
		return spec.Repo
	}
}

// governorBaseInstructions returns the instructions the governor would normally
// receive (its own per-agent override if present, else the shared INSTRUCTIONS.md),
// so fusion augments rather than replaces what the user wrote.
func governorBaseInstructions(cfg *config.Config, governor, file string) string {
	if data, err := os.ReadFile(filepath.Join(cfg.AgentDir(governor), file)); err == nil {
		return string(data)
	}
	if ins := cfg.Instructions(); fileExists(ins) {
		if data, err := os.ReadFile(ins); err == nil {
			return string(data)
		}
	}
	return ""
}

// decideTTY chooses the stdin/tty wiring. Stdin is attached only for an
// interactive terminal (-it) or ACP (-i); batch and piped runs get neither,
// matching the original tool's behavior.
func decideTTY(spec RunSpec, stdinIsTTY bool) ttyMode {
	switch {
	case spec.ForceNoTTY:
		return ttyStdinOnly
	case spec.Batch:
		return ttyNone
	case stdinIsTTY:
		return ttyInteractive
	default:
		return ttyNone
	}
}

// assembleArgs builds the full container-runtime argument list. It is pure given
// its inputs and the on-disk presence of the env/instruction files, so the whole
// run plan can be unit-tested without a container daemon.
func assembleArgs(cfg *config.Config, spec RunSpec, mounts []Mount, decoy, workdir string, mode ttyMode, mcpPresent bool, mcpMounts, fusionMounts, gitMounts []extraMount, networkName string) []string {
	args := []string{"run", "--rm", "--label", "coop=box"}
	switch mode {
	case ttyInteractive:
		// -e TERM propagates the host terminal type so the agents' TUIs render in
		// full color (without it the box reports a basic terminal — e.g. Gemini
		// warns about missing 256-color support).
		args = append(args, "-it", "-e", "TERM")
	case ttyStdinOnly:
		args = append(args, "-i")
	}
	args = append(args, RenderMounts(mounts, decoy)...)

	if spec.Homes {
		for _, agent := range agents.Names() {
			args = append(args, "-v", cfg.AgentDir(agent)+":"+cfg.HomeInBox+"/."+agent)
		}
		// Claude keeps its account + onboarding state in $CLAUDE_CONFIG_DIR — by
		// default ~/.claude.json in $HOME, which the disposable box would lose,
		// re-prompting login every run. Point it at the mounted ~/.claude dir so
		// the config persists alongside the credentials. (Codex and Gemini already
		// store everything under their mounted ~/.codex and ~/.gemini dirs.)
		args = append(args, "-e", "CLAUDE_CONFIG_DIR="+cfg.HomeInBox+"/.claude")
		// Claude Code wraps every subprocess in bubblewrap to scrub env vars from it.
		// The box ships no bubblewrap (and is itself the sandbox), so without this it
		// warns "bubblewrap is required for subprocess env scrubbing" before each
		// command. Turn the scrub off — the container is the isolation boundary.
		args = append(args, "-e", "CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=0")
		if fileExists(cfg.EnvFile()) {
			args = append(args, "--env-file", cfg.EnvFile())
		}
		// One canonical instruction file → each agent's native global path,
		// unless that agent has its own override. The lead is skipped
		// here (fusion governor or consult lead) — its augmented file is below.
		if ins := cfg.Instructions(); fileExists(ins) {
			for _, agent := range agents.Names() {
				if agent == spec.FusionGovernor || agent == spec.ConsultLead {
					continue
				}
				file := instructionFile(agent)
				if !fileExists(filepath.Join(cfg.AgentDir(agent), file)) {
					args = append(args, "-v", ins+":"+cfg.HomeInBox+"/."+agent+"/"+file+":ro")
				}
			}
		}
		// Fusion: the governor's augmented instruction file (peers + synthesis).
		for _, m := range fusionMounts {
			args = append(args, "-v", m.host+":"+m.box+":ro")
		}
		// Your git environment: identity + signing-off + global gitignore.
		for _, m := range gitMounts {
			args = append(args, "-v", m.host+":"+m.box+":ro")
		}
		if mcpPresent {
			args = append(args, "-v", cfg.MCPFile+":"+cfg.MCPInBox+":ro")
			for _, m := range mcpMounts {
				args = append(args, "-v", m.host+":"+m.box+":ro")
			}
		}
	}

	args = append(args, cfg.ExtraRunArgs...)
	args = append(args, spec.ExtraArgs...)
	if networkName != "" {
		args = append(args, "--network", networkName)
	}
	if spec.Cache {
		args = append(args, "-v", "coop-cache:"+cfg.HomeInBox+"/.cache")
	}
	// The base box provisions a repo's .tool-versions toolchain via asdf at run
	// time; persist ~/.asdf in a volume so installs survive the disposable box and
	// are reused across repos. Only the base image carries the asdf entrypoint.
	if spec.Homes && spec.Image == cfg.BaseImage {
		args = append(args, "-v", "coop-asdf:"+cfg.HomeInBox+"/.asdf")
	}
	args = append(args, "-w", workdir, spec.Image)
	return append(args, spec.Cmd...)
}

func writeTempFile(content string) (string, error) {
	f, err := os.CreateTemp("", "coop-mcp-")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}
