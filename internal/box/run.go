package box

import (
	"io"
	"os"
	"path/filepath"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/mcp"
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
		wire := func(label, content string, genErr error, boxPath string) {
			if genErr != nil {
				ui.Info("mcp.json: skipped %s wiring: %v", label, genErr)
				return
			}
			p, err := writeTempFile(content)
			if err != nil {
				ui.Info("mcp.json: skipped %s wiring: %v", label, err)
				return
			}
			tmpFiles = append(tmpFiles, p)
			mcpMounts = append(mcpMounts, extraMount{p, boxPath})
		}
		gm, gerr := mcp.GenerateGemini(cfg.MCPFile, filepath.Join(cfg.AgentDir("gemini"), "settings.json"))
		wire("Gemini", gm, gerr, cfg.HomeInBox+"/.gemini/settings.json")
		cx, cerr := mcp.GenerateCodex(cfg.MCPFile, filepath.Join(cfg.AgentDir("codex"), "config.toml"))
		wire("Codex", cx, cerr, cfg.HomeInBox+"/.codex/config.toml")
	}

	// Ensure the per-agent home dirs exist so their bind mounts resolve, and
	// pre-answer Claude's first-run prompts (theme/trust/bypass) so the box is
	// ready to work on a fresh install.
	if spec.Homes {
		for _, agent := range cfg.Agents {
			os.MkdirAll(cfg.AgentDir(agent), 0o755)
		}
		ensureClaudeDefaults(cfg, workdir)
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

	args := assembleArgs(cfg, spec, mounts, decoy.Name(), workdir, mode, mcpPresent, mcpMounts, networkName)
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
func assembleArgs(cfg *config.Config, spec RunSpec, mounts []Mount, decoy, workdir string, mode ttyMode, mcpPresent bool, mcpMounts []extraMount, networkName string) []string {
	args := []string{"run", "--rm"}
	switch mode {
	case ttyInteractive:
		args = append(args, "-it")
	case ttyStdinOnly:
		args = append(args, "-i")
	}
	args = append(args, RenderMounts(mounts, decoy)...)

	if spec.Homes {
		for _, agent := range cfg.Agents {
			args = append(args, "-v", cfg.AgentDir(agent)+":"+cfg.HomeInBox+"/."+agent)
		}
		// Claude keeps its account + onboarding state in $CLAUDE_CONFIG_DIR — by
		// default ~/.claude.json in $HOME, which the disposable box would lose,
		// re-prompting login every run. Point it at the mounted ~/.claude dir so
		// the config persists alongside the credentials. (Codex and Gemini already
		// store everything under their mounted ~/.codex and ~/.gemini dirs.)
		args = append(args, "-e", "CLAUDE_CONFIG_DIR="+cfg.HomeInBox+"/.claude")
		if fileExists(cfg.EnvFile()) {
			args = append(args, "--env-file", cfg.EnvFile())
		}
		// One canonical instruction file → each agent's native global path,
		// unless that agent has its own override in its folder.
		if ins := cfg.Instructions(); fileExists(ins) {
			if !fileExists(filepath.Join(cfg.AgentDir("claude"), "CLAUDE.md")) {
				args = append(args, "-v", ins+":"+cfg.HomeInBox+"/.claude/CLAUDE.md:ro")
			}
			if !fileExists(filepath.Join(cfg.AgentDir("codex"), "AGENTS.md")) {
				args = append(args, "-v", ins+":"+cfg.HomeInBox+"/.codex/AGENTS.md:ro")
			}
			if !fileExists(filepath.Join(cfg.AgentDir("gemini"), "GEMINI.md")) {
				args = append(args, "-v", ins+":"+cfg.HomeInBox+"/.gemini/GEMINI.md:ro")
			}
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
