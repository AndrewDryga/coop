package box

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/fusion"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/ui"
)

// Container labels coop stamps on its boxes so it can find and tear them down later. The SET
// sites (assembleArgs, below) and the cli QUERY sites (CountByLabel/KillByLabel) MUST agree —
// a label renamed on only one side would orphan running containers — so both reference these.
const (
	LabelKey        = "coop"            // every coop box: coop=box
	LabelBox        = "box"             //   (its value)
	LabelSupervised = "coop.supervised" // a supervised inner box (build/update restart it): =1
	LabelOn         = "1"               //   (its value)
	LabelSupervisor = "coop.sup"        // value=<supervisor id>, so a supervisor kills only its own
	LabelFork       = "coop.fork"       // value=<fork name>, so `coop fork stop` tears it down
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

	// Agent names the launched agent (claude/codex/gemini) whose credential home and
	// env-file API key this run may mount — so a plain `coop claude` box can't read the
	// Codex/Gemini credentials. Empty for a raw/maintenance run (no agent session), which
	// mounts no agent credentials at all. FusionGovernor/ConsultLead (below) widen the
	// scope to authenticated peers, since the lead is told to invoke them. See
	// credentialScope. Ignored when Homes is false.
	Agent string

	ForceNoTTY   bool   // ACP: attach stdin (-i) but never allocate a tty
	SupervisorID string // non-empty for a supervised inner box: tags it coop.supervised=1
	// (build/update restart it) + coop.sup=<id> (its supervisor kills exactly its boxes)
	ForkName string // non-empty for a detached fork loop's box: tags it coop.fork=<name> so
	// `coop fork stop` can tear the container down by label after SIGKILL (else --rm never fires
	// on a SIGKILL'd run client and the orphaned container keeps mutating the worktree)
	Batch     bool      // loop/doctor: no tty, stdin from /dev/null
	Quiet     bool      // suppress the "shadowed N secret path(s)" line (doctor)
	Stdout    io.Writer // capture output (doctor); nil means inherit os.Stdout
	Stderr    io.Writer // capture/discard the container's stderr; nil means inherit os.Stderr
	ExtraArgs []string  // extra runtime args for this run (e.g. doctor's probe mount)

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
	if !spec.Batch && !spec.Quiet && StaleImageInputs(cfg, spec.Repo, spec.Image) {
		ui.Info("box image is stale — Dockerfile.agent/.tool-versions changed since it was built; run 'coop build'")
	}

	// A single empty read-only file shadows every secret file; a single empty read-only
	// dir shadows every secret directory (an RO bind, not --tmpfs, so it holds on podman).
	decoy, err := os.CreateTemp("", "coop-decoy-")
	if err != nil {
		return -1, err
	}
	decoy.Close()
	defer os.Remove(decoy.Name())
	decoyDir, err := os.MkdirTemp("", "coop-decoy-dir-")
	if err != nil {
		return -1, err
	}
	defer os.RemoveAll(decoyDir)

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
		for _, name := range agents.Names() {
			// Best-effort: EnsureDefaults below is best-effort too, and a real failure to make the
			// vault surfaces with a clearer error when its home is mounted. 0o700 — owner-only.
			_ = os.MkdirAll(cfg.AgentDir(name), 0o700)
			if ag, ok := agents.Get(name); ok {
				ag.EnsureDefaults(cfg, workdir)
			}
		}
	}

	// Generate MCP configs into temp files that live for the container's run.
	var tmpFiles []string
	defer func() {
		for _, f := range tmpFiles {
			os.Remove(f)
		}
	}()
	var mcpMounts []extraMount
	mcpPresent := spec.Homes && cfg.MCPActive()
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
	consultWired := false // true once a fusion/consult directive is injected → mount coop-consult
	if spec.Homes && spec.FusionGovernor != "" {
		if file := instructionFile(spec.FusionGovernor); file != "" {
			base := agentBaseInstructions(cfg, spec.FusionGovernor, file)
			// Name only AUTHENTICATED peers in the council directive — credentials are scoped to
			// authed peers (credentialScope), so telling the governor it MUST consult an unsigned
			// peer just wastes turns on a consult that can't authenticate. With no authed peer,
			// fusion degenerates to a normal run: mount the governor's plain instructions, no directive.
			peers := authedPeers(cfg, spec.FusionGovernor)
			content := base
			if len(peers) > 0 {
				content = fusion.GovernorInstructions(base, spec.FusionGovernor, append([]string{spec.FusionGovernor}, peers...))
			}
			if p, err := writeTempFile(content); err != nil {
				ui.Info("fusion: skipped instruction wiring: %v", err)
			} else {
				tmpFiles = append(tmpFiles, p)
				fusionMounts = append(fusionMounts, extraMount{p, cfg.HomeInBox + "/." + spec.FusionGovernor + "/" + file})
				consultWired = len(peers) > 0 // only mount coop-consult when there's a peer to consult
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
				base := agentBaseInstructions(cfg, spec.ConsultLead, file)
				content := fusion.LeadInstructions(base, peers)
				if p, err := writeTempFile(content); err != nil {
					ui.Info("consult: skipped instruction wiring: %v", err)
				} else {
					tmpFiles = append(tmpFiles, p)
					fusionMounts = append(fusionMounts, extraMount{p, cfg.HomeInBox + "/." + spec.ConsultLead + "/" + file})
					consultWired = true
				}
			}
		}
	}

	// coop-consult: mount the read-only consult wrapper (on PATH) whenever a fusion or
	// --consult directive was injected, so the lead's `coop-consult <peer>` calls resolve.
	// It carries the per-agent session-id mechanics for cross-turn continuity.
	if consultWired {
		if p, err := writeTempFile(fusion.ConsultWrapper); err != nil {
			ui.Info("consult: skipped wrapper wiring: %v", err)
		} else if err := os.Chmod(p, 0o755); err != nil {
			ui.Info("consult: skipped wrapper wiring: %v", err)
		} else {
			tmpFiles = append(tmpFiles, p)
			fusionMounts = append(fusionMounts, extraMount{p, fusion.ConsultWrapperPath})
		}
	}

	// Every agent gets the box environment note, then the user's instructions (a per-agent
	// override if present, else the shared INSTRUCTIONS.md), mounted at its native global path
	// — so it never burns a turn rediscovering the box. The lead (fusion governor / consult
	// lead) is handled above, with its augmented file.
	var instructionMounts []extraMount
	for _, it := range instructionPlan(cfg, spec) {
		if p, err := writeTempFile(it.content); err == nil {
			tmpFiles = append(tmpFiles, p)
			instructionMounts = append(instructionMounts, extraMount{p, cfg.HomeInBox + "/." + it.agent + "/" + it.file})
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

	// Credential scope: the shared env file is passed in, but a scoped run (a plain agent
	// or a raw command) strips the API keys of agents it has no business reading — so a
	// `coop claude` box never sees OPENAI_API_KEY / GEMINI_API_KEY. Non-agent runtime vars
	// always pass through. assembleArgs computes the same scope for the home mounts.
	envFile := ""
	if spec.Homes && fileExists(cfg.EnvFile()) {
		drop := envKeysOutsideScope(credentialScope(cfg, spec))
		switch {
		case len(drop) == 0:
			envFile = cfg.EnvFile() // nothing to strip — pass it through unchanged
		default:
			if p, err := writeFilteredEnvFile(cfg.EnvFile(), drop); err == nil {
				tmpFiles = append(tmpFiles, p)
				envFile = p
			} else {
				// Fail closed: if the peer keys can't be stripped, omit the env file
				// entirely rather than leak them into a scoped box.
				ui.Info("env: omitted (could not filter peer API keys): %v", err)
			}
		}
	}

	// Bring sibling services up first, so the box can reach them by name. Every launch path —
	// agent, fusion governor, acp, loop, fork — funnels through box.Run, so this one call covers
	// them all. Gated like the network join below (on the services net, online, compose-capable
	// runtime) plus COOP_AUTO_UP. Idempotent; progress goes to stderr (never stdout, which may
	// carry ACP/JSON) and only when not Quiet; a failure warns but never blocks the session.
	if autoUpServices(cfg, spec, rt.Name) {
		if cf := ComposeFile(spec.Repo); cf != "" {
			var out, errw io.Writer = io.Discard, io.Discard
			if !spec.Quiet {
				ui.Info("starting sibling services (%s)", filepath.Base(cf))
				out, errw = os.Stderr, os.Stderr
			}
			if err := EnsureServices(rt, spec.Repo, out, errw); err != nil {
				ui.Info("services: auto-start failed (%v) — continuing without them", err)
			}
		}
	}

	networkName := ""
	// Only "open" gets any networking (the same fail-closed test the --network flag uses below):
	// an offline box (COOP_EGRESS=none) has nothing to reach, so skip the services-net join.
	if cfg.Egress == "open" && spec.Network {
		net := cfg.ServicesNet
		if net == "" {
			net = ServicesProject(spec.Repo) + "_default"
		}
		if rt.Silent("network", "inspect", net) {
			networkName = net
		}
	}

	limits := boxLimits(cfg, rt.Name)
	args := assembleArgs(cfg, spec, mounts, decoy.Name(), decoyDir, workdir, mode, mcpPresent, mcpMounts, fusionMounts, gitMounts, instructionMounts, networkName, envFile, limits...)
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

// boxEnvNote is the always-present environment briefing every agent receives up front, so it
// doesn't burn a turn rediscovering the box (the user's INSTRUCTIONS.md, if any, follows it).
// It states the ground truth the agents most often probe or trip over: the missing OS sandbox,
// what's installed (now that the image carries bare python/pip), where it may write, and that
// secrets are read-only decoys.
const boxEnvNote = `# Environment (coop box) — ground truth, don't reprobe it
You run inside a coop container: a Debian box that IS your sandbox and security boundary.
- OS-level sandboxing (bubblewrap) is intentionally absent. A "bubblewrap is required" notice
  is expected, not a bug — don't investigate or work around it, just proceed.
- Installed and ready: node, npm, yarn, python (= python3), pip, git, gcc/make, jq, rg, fd,
  curl, wget, perl, psql. Other toolchains (go, ruby, erlang, …) exist only if the repo pins
  them in .tool-versions, which is provisioned automatically on start.
- Playwright's Chromium system libraries are preinstalled. The browser binary downloads on
  first use (cached in ~/.cache, so once per machine): run "npx playwright install chromium"
  if it's missing. Launch headless and pass args: ['--no-sandbox'] — Chromium's own sandbox
  can't run here (the box already is the sandbox), so without it the launch fails.
- Write inside the repo (your working directory) — that's where your changes belong and your
  file-write tools work. Paths outside the repo may be refused; for scratch, write in-repo or
  use a shell command.
- Files that look like secrets (.env*, *.key, *.pem, id_rsa*, .ssh, …) are shadowed with empty
  read-only decoys. You can't read or write them, by design — don't try to bypass it.
`

// agentBaseInstructions is what an agent receives as its global instructions: the always-on
// box environment note, followed by the user's instructions — a per-agent override if present,
// else the shared INSTRUCTIONS.md. Fusion/consult augment this (they don't replace it).
func agentBaseInstructions(cfg *config.Config, agent, file string) string {
	user := ""
	if data, err := os.ReadFile(filepath.Join(cfg.AgentDir(agent), file)); err == nil {
		user = string(data)
	} else if ins := cfg.Instructions(); fileExists(ins) {
		if data, err := os.ReadFile(ins); err == nil {
			user = string(data)
		}
	}
	if strings.TrimSpace(user) == "" {
		return boxEnvNote
	}
	return boxEnvNote + "\n" + user
}

// instructionItem is one agent's global instruction file and the content it should hold.
type instructionItem struct{ agent, file, content string }

// instructionPlan is the global instruction each non-lead agent should receive: the box env
// note plus the user's instructions (per agentBaseInstructions). The lead (fusion governor /
// consult lead) is excluded — it gets its augmented file instead. Pure (no temp files / mounts),
// so the selection and content are unit-testable; Run writes + mounts the result.
func instructionPlan(cfg *config.Config, spec RunSpec) []instructionItem {
	if !spec.Homes {
		return nil
	}
	var out []instructionItem
	for _, agent := range agents.Names() {
		if agent == spec.FusionGovernor || agent == spec.ConsultLead {
			continue
		}
		if file := instructionFile(agent); file != "" {
			out = append(out, instructionItem{agent, file, agentBaseInstructions(cfg, agent, file)})
		}
	}
	return out
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

// boxLimits returns the resource + privilege caps that keep a runaway agent from
// harming the host: a pids cap (fork bombs), optional memory/cpu caps,
// no-new-privileges, and dropping all Linux capabilities. These are OCI-runtime flags applied for docker and podman;
// Apple's `container` CLI differs, so they're skipped there (its hardening is
// tracked separately). All are config-driven (COOP_PIDS/MEMORY/CPUS,
// COOP_NO_NEW_PRIVILEGES).
func boxLimits(cfg *config.Config, runtimeName string) []string {
	if runtimeName != "docker" && runtimeName != "podman" {
		return nil
	}
	var a []string
	if cfg.NoNewPrivileges {
		a = append(a, "--security-opt", "no-new-privileges")
	}
	// Drop every Linux capability: the agent workloads (node, npm, asdf, git) need none of
	// Docker's default set, and dropping them tightens the posture for a repo Dockerfile.agent
	// that runs USER root — root-in-container then holds no CAP_DAC_OVERRIDE / CAP_NET_RAW /
	// CAP_MKNOD / CAP_SYS_CHROOT to abuse. Add one back only if a concrete need appears.
	a = append(a, "--cap-drop", "ALL")
	switch cfg.Pids {
	case "", "0", "-1", "unlimited": // pids cap off
	default:
		a = append(a, "--pids-limit", cfg.Pids)
	}
	if cfg.Memory != "" {
		a = append(a, "--memory", cfg.Memory)
	}
	if cfg.CPUs != "" {
		a = append(a, "--cpus", cfg.CPUs)
	}
	return a
}

// appendROMounts appends a read-only `-v host:box:ro` bind for each mount.
func appendROMounts(args []string, ms []extraMount) []string {
	for _, m := range ms {
		args = append(args, "-v", m.host+":"+m.box+":ro")
	}
	return args
}

// assembleArgs builds the full container-runtime argument list. It is pure given
// its inputs and the on-disk presence of the env/instruction files, so the whole
// run plan can be unit-tested without a container daemon. limits is the runtime's
// resource/privilege caps (see boxLimits).
func assembleArgs(cfg *config.Config, spec RunSpec, mounts []Mount, decoy, decoyDir, workdir string, mode ttyMode, mcpPresent bool, mcpMounts, fusionMounts, gitMounts, instructionMounts []extraMount, networkName, envFile string, limits ...string) []string {
	args := []string{"run", "--rm", "--label", LabelKey + "=" + LabelBox}
	if spec.SupervisorID != "" {
		// A supervised inner box: coop.supervised=1 lets build/update restart it (the
		// editor reconnects); coop.sup=<id> lets its own supervisor kill exactly its
		// box(es) on teardown, so nothing is orphaned.
		args = append(args, "--label", LabelSupervised+"="+LabelOn, "--label", LabelSupervisor+"="+spec.SupervisorID)
	}
	if spec.ForkName != "" {
		// A detached fork loop's box: `coop fork stop` kills it by this label after SIGKILLing the
		// worker, so a SIGKILL'd `docker run` client can't orphan a container that keeps writing the
		// fork's worktree (the fork name has no whitespace/`=`, so it's a safe label value).
		args = append(args, "--label", LabelFork+"="+spec.ForkName)
	}
	switch mode {
	case ttyInteractive:
		// -e TERM propagates the host terminal type so the agents' TUIs render in
		// full color (without it the box reports a basic terminal — e.g. Gemini
		// warns about missing 256-color support).
		args = append(args, "-it", "-e", "TERM")
	case ttyStdinOnly:
		args = append(args, "-i")
	}
	args = append(args, limits...) // resource/privilege caps (docker/podman; nil elsewhere)
	args = append(args, RenderMounts(mounts, decoy, decoyDir)...)

	if spec.Homes {
		// Only the launched agent's credential home (plus authenticated peers for
		// fusion/consult) — never every agent's, so a plain run can't read the others'.
		for _, agent := range credentialScope(cfg, spec) {
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
		// coop-consult reads COOP_CONSULT_TIMEOUT (seconds) for its per-peer timeout; forward an
		// explicit, valid override so the knob works per-run. Empty/invalid falls back to the
		// wrapper's built-in 30m default. (The wrapper exists only in fusion/consult boxes; the
		// var is inert elsewhere.)
		if n, err := strconv.Atoi(cfg.ConsultTimeout); err == nil && n > 0 {
			args = append(args, "-e", "COOP_CONSULT_TIMEOUT="+cfg.ConsultTimeout)
		}
		if envFile != "" {
			args = append(args, "--env-file", envFile)
		}
		// Per-agent global instructions (the box env note + the user's, built in Run) at each
		// agent's native path. The lead (fusion governor / consult lead) is excluded there; its
		// augmented file is the fusion/consult mount just below.
		args = appendROMounts(args, instructionMounts)
		// Fusion: the governor's augmented instruction file (peers + synthesis).
		args = appendROMounts(args, fusionMounts)
		// Your git environment: identity + signing-off + global gitignore.
		args = appendROMounts(args, gitMounts)
		if mcpPresent {
			args = append(args, "-v", cfg.MCPFile+":"+cfg.MCPInBox+":ro")
			args = appendROMounts(args, mcpMounts)
		}
	}

	args = append(args, cfg.ExtraRunArgs...)
	args = append(args, spec.ExtraArgs...)
	// Egress fails CLOSED at the box boundary: full/services networking only when COOP_EGRESS is
	// explicitly "open" — any other value (the normalized "none", or a value that somehow skipped
	// config.normalizeEgress) cuts the box off the network entirely (--network none), so a missed
	// normalization can never silently grant outbound. "open" keeps the runtime's bridge (full
	// outbound) plus any services-net join; the agent needs npm/the model API, so it's opt-in.
	net := "none"
	if cfg.Egress == "open" {
		net = networkName // "" → default bridge (full outbound); else the joined services net
	}
	if net != "" {
		args = append(args, "--network", net)
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
