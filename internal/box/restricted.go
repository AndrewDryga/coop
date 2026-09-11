package box

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/ui"
)

// The restricted filesystem profile the readonly and bare execution modes share. The promise is
// that the agent has NO writable persistent storage: the container root is read-only, the only
// writable places are run-private tmpfs scratch (the box home, /tmp, and bare's empty cwd), and
// every host path that enters the box enters read-only. Nothing the normal launch mounts writable
// exists here — no credential home, no dependency cache, no asdf volume, no synthesized skills, no
// shared ACP transcripts — and nothing the project defines is loaded: no project policy, env,
// services, hooks, MCP or toolchain provisioning. Readonly mounts the selected repository and its
// approved companions read-only; bare mounts no repository at all.
//
// The provider still needs its login and first-run defaults, and it writes into its home, so it
// cannot simply be handed the host profile read-only. Instead the run builds a SEED on the host —
// the access-only credential projection (refresh authority never leaves the host), the adapter's
// first-run defaults rendered into an empty profile, and the mode's instruction note — mounts it
// read-only OUTSIDE the home, and copies it into the tmpfs home before the command starts. A bind
// under the home would not do: Docker creates a missing bind parent inside the tmpfs as root, and
// the provider, running as node, could then write nothing beside it ([[box-home-nested-mounts]]).
// Nothing the agent writes is ever synchronized back.
const (
	// restrictedSeedPath is where the seed tree is bound read-only. Deliberately not under the
	// home: see the note above.
	restrictedSeedPath = "/coop/seed"
	// BareWorkdir is bare's cwd, an empty owned tmpfs over the image's WORKDIR. Never a
	// repository. Exported because an ACP client outside the box (the session daemon) names the
	// session's cwd in session/new, and for a bare session this is the only directory there is.
	BareWorkdir = "/workspace"
	// restrictedBoxUID is the shared base image's `node` user. A tmpfs is root-owned unless told
	// otherwise, so every scratch mount names the user that must write it. Restricted modes run
	// the base image only, which is what makes this a constant rather than an inspection.
	restrictedBoxUID = 1000
	// restrictedScratchSize caps each scratch tmpfs. A cap, not an allocation: pages are charged
	// only as they are written, and a runaway fills this rather than the runtime host's memory.
	restrictedScratchSize = "1g"
	// RestrictedCredentialHorizon is how long the seeded access token must stay usable. The host
	// renews an expiring login before projection, exactly as a session turn does with its own
	// deadline; a run that outlives the token fails its next request explicitly rather than
	// carrying refresh authority into the box. Exported because the session daemon projects a
	// session's credential BEFORE this launch re-checks it, against a deadline that must reach
	// at least this far — the projection carries no refresh authority for the box to renew with.
	RestrictedCredentialHorizon = time.Hour
	restrictedArtifactLimit     = 1 << 20
)

// executionMode reads the spec's mode with the zero value meaning normal — every launch that
// predates the modes — and a misspelling refused rather than run wide.
func executionMode(spec RunSpec) (agents.ExecutionMode, error) {
	if spec.Mode == "" {
		return agents.ModeNormal, nil
	}
	return agents.ParseExecutionMode(string(spec.Mode))
}

// checkRestrictedSpec refuses everything a restricted run does not enforce. Each refusal names
// the field, because a caller that set one expected an effect the profile would silently drop.
func checkRestrictedSpec(cfg *config.Config, rt runtime.Runtime, spec RunSpec, mode agents.ExecutionMode) error {
	if !rt.SupportsRestrictedFilesystem() {
		return fmt.Errorf("a %s run is qualified on docker only; %s is not", mode, filepath.Base(rt.Name))
	}
	if spec.CapturedEgress != nil || spec.networkSmoke != nil || cfg.Egress == "filtered" {
		return fmt.Errorf("a %s run is not qualified under restricted networking — run it with --egress open or none", mode)
	}
	// The repository mounts at its own host path; the profile's scratch tmpfs are fixed paths.
	// A repository that IS one of them (a run from /tmp outside any checkout) would ask the
	// runtime for two mounts at one point, which it refuses after the box was already narrated.
	if mode == agents.ModeReadOnly {
		for _, scratch := range []string{"/tmp", cfg.HomeInBox, BareWorkdir} {
			if scratch != "" && (spec.Repo == scratch || strings.HasPrefix(spec.Repo, scratch+"/")) {
				return fmt.Errorf("a %s run cannot mount %s as the repository — that path is the box's own scratch; run it from a checkout", mode, spec.Repo)
			}
		}
	}
	if cfg.BaseImage == "" || spec.Image != cfg.BaseImage {
		return fmt.Errorf("a %s run uses the shared base image only, never %q", mode, spec.Image)
	}
	if spec.Preset != nil || spec.ConsultLead != "" || len(spec.Peers) > 0 {
		return fmt.Errorf("a %s run consults no peers and runs no preset roles", mode)
	}
	if spec.ShareACPSessions || spec.SupervisorID != "" {
		return fmt.Errorf("a %s run shares no ACP transcript and answers to no editor supervisor", mode)
	}
	// The agent's own CLI, or its ACP adapter driven by one client outside the box (the session
	// daemon, an editor) — each has an adapter-declared switch for the mode. A maintenance
	// command run under an agent's credential scope has neither.
	if spec.Agent != "" && !spec.AgentCommand && spec.NetworkClient != egress.ClientACP {
		return fmt.Errorf("a %s run drives the agent's own command or its ACP adapter; maintenance commands are not qualified", mode)
	}
	if spec.Review || len(spec.RepoReadOnlyPaths) > 0 || spec.ActivityKind != "" {
		return fmt.Errorf("a %s run takes no review stage, protected path or activity record", mode)
	}
	if spec.ForkWorker {
		return fmt.Errorf("a %s run is never a detached fork worker", mode)
	}
	switch mode {
	case agents.ModeBare:
		if spec.Repo != "" || spec.PolicyRepo != "" || spec.Workdir != "" || len(spec.CompanionRepositories) > 0 {
			return errors.New("a bare run names no repository, workdir or companion — it mounts none")
		}
	case agents.ModeReadOnly:
		if spec.Repo == "" {
			return errors.New("a readonly run needs the repository it mounts read-only")
		}
	}
	return nil
}

// restrictedRuntimeArgs admits only `-e KEY=VALUE` from runtime arguments. A bind mount would be
// a host path the profile never planned, a volume would be persistent storage, and any privilege
// or namespace flag a different boundary; each is refused by name, with the one-run escape for the
// common case — a host-wide COOP_RUN_ARGS that mounts build caches into ordinary boxes. Environment
// changes what the box knows, never what it can reach.
func restrictedRuntimeArgs(args []string, mode agents.ExecutionMode, source string) ([]string, error) {
	var out []string
	for i := 0; i < len(args); i++ {
		name, inline, hasInline := strings.Cut(args[i], "=")
		if name != "-e" && name != "--env" {
			return nil, fmt.Errorf("a %s run takes only -e KEY=VALUE in %s; %q is not — for this run clear them with an empty COOP_RUN_ARGS= in front of the command, or run without --%s", mode, source, name, mode)
		}
		value := inline
		if !hasInline {
			if i+1 >= len(args) {
				return nil, fmt.Errorf("%s needs a KEY=VALUE assignment after it", name)
			}
			i++
			value = args[i]
		}
		key, _, ok := strings.Cut(value, "=")
		if !ok || key == "" || strings.ContainsAny(value, "\x00\r\n") {
			return nil, fmt.Errorf("-e %s needs a plain KEY=VALUE assignment, on one line: a %s run does not pass your shell's environment through", value, mode)
		}
		out = append(out, "-e", value)
	}
	return out, nil
}

// restrictedFilesystemArgs are the runtime flags that ARE the profile: the read-only root and
// the owned scratch tmpfs set. They ride in the limits slot so they land beside the other
// privilege caps, before any mount.
func restrictedFilesystemArgs(cfg *config.Config, mode agents.ExecutionMode) []string {
	// exec is spelled out: Docker mounts a --tmpfs noexec unless told otherwise, and scratch is
	// where an experiment's script, a built test binary or an npm bin has to run from. nosuid
	// and nodev stay; under --cap-drop ALL and no-new-privileges they cost nothing to keep.
	owned := fmt.Sprintf("rw,exec,nosuid,nodev,uid=%d,gid=%d,size=%s", restrictedBoxUID, restrictedBoxUID, restrictedScratchSize)
	args := []string{"--read-only",
		"--tmpfs", cfg.HomeInBox + ":" + owned + ",mode=0700",
		"--tmpfs", "/tmp:" + owned + ",mode=1777"}
	if mode == agents.ModeBare {
		args = append(args, "--tmpfs", BareWorkdir+":"+owned+",mode=0700")
	}
	return args
}

// seedPrelude wraps the command so the box copies the seed into its tmpfs home first, as the
// box user, then execs the command with its arguments intact. Positional parameters, not
// interpolation, so no path or argument is ever shell-parsed.
func seedPrelude(homeInBox string, cmd []string) []string {
	return append([]string{"sh", "-c", `cp -R ` + restrictedSeedPath + `/. "$1"/ && shift && exec "$@"`, "coop-seed", homeInBox}, cmd...)
}

// restrictedInstructions is the mode's whole instruction file: the box environment note for a
// run whose contract differs from the normal one. The user's shared INSTRUCTIONS.md is not
// appended — it describes how to work in a writable checkout.
func restrictedInstructions(mode agents.ExecutionMode, workdir, homeInBox string) string {
	if mode == agents.ModeBare {
		// The tool situation is stated by the adapter on the provider's own system-prompt channel
		// (claudeBareSystemPrompt); a memory file that argued with the CLI's system prompt was
		// taken for an injection in a live run. This note only states what the box is.
		return `# Environment (coop box, bare run)
This run mounts no repository and loads no project context. Nothing written here is kept;
your answer is the only output of the run.
`
	}
	return `# Environment (coop box, read-only run) — ground truth, don't reprobe it
You run inside a coop container that IS your sandbox and security boundary, in READ-ONLY mode.
- The repository at ` + workdir + ` is mounted read-only, git history included: read, search and
  inspect it freely, but no write to it can succeed and no commit can be made. That is this
  run's contract, not a fault — never try to work around it.
- The only writable places are private, disposable scratch: your home directory (` + homeInBox + `)
  and /tmp. Both start empty, live in memory, and are discarded when the run ends; copy files
  there to experiment. Nothing you write is kept or handed back — your answer is the only output.
- Nothing from the project is loaded on your behalf: no MCP servers, hooks, skills, project
  settings, services or toolchain provisioning. The image's own tools are ready: node, npm,
  python, pip, git, gcc/make, jq, rg, fd, curl.
- OS-level sandboxing (bubblewrap) is intentionally absent. A "bubblewrap is required" notice
  is expected, not a bug — don't investigate or work around it, just proceed.
- Files that look like secrets (.env*, *.key, *.pem, id_rsa*, .ssh, …) are shadowed with empty
  read-only decoys. You can't read them, by design — don't try to bypass it.
`
}

// buildRestrictedSeed renders the seed tree for the one scoped agent: the access-only credential
// projection, the adapter's first-run defaults into an otherwise EMPTY profile (so no hook, skill
// or setting of the host profile comes along), and the mode's instruction note. It returns the
// run-private root the caller removes after the run; the tree to copy is root/home.
func buildRestrictedSeed(cfg *config.Config, agent string, mode agents.ExecutionMode, workdir string, artifacts compositionArtifactOps) (root string, err error) {
	ag, ok := agents.Get(agent)
	if !ok {
		return "", fmt.Errorf("unknown agent %q", agent)
	}
	root, err = os.MkdirTemp(artifacts.parent, "coop-seed-")
	if err != nil {
		return "", fmt.Errorf("prepare %s seed: %w", mode, err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(root)
		}
	}()
	// The adapter's own AgentDir layout, rooted in the seed, so EnsureDefaults renders exactly the
	// files it would render for a fresh host profile — into this run's profile, not the host's.
	scratch := *cfg
	scratch.ConfigDir = filepath.Join(root, "config")
	profile := scratch.AgentDir(agent)
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return "", fmt.Errorf("prepare %s seed: %w", mode, err)
	}
	source := cfg.AgentDir(agent)
	if profileMarkerPresent(ag, source) {
		if err := projectRestrictedCredential(ag, source, profile); err != nil {
			return "", err
		}
	}
	if err := ag.EnsureDefaults(&scratch, workdir); err != nil {
		return "", fmt.Errorf("prepare %s defaults for the %s run: %w", agent, mode, err)
	}
	if file := ag.InstructionFile(); file != "" {
		if err := os.WriteFile(filepath.Join(profile, file), []byte(restrictedInstructions(mode, workdir, cfg.HomeInBox)), 0o600); err != nil {
			return "", fmt.Errorf("prepare %s instructions: %w", mode, err)
		}
	}
	home := filepath.Join(root, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		return "", fmt.Errorf("prepare %s seed: %w", mode, err)
	}
	if err := os.Rename(profile, filepath.Join(home, "."+agent)); err != nil {
		return "", fmt.Errorf("prepare %s seed: %w", mode, err)
	}
	if err := os.RemoveAll(scratch.ConfigDir); err != nil {
		return "", fmt.Errorf("prepare %s seed: %w", mode, err)
	}
	return root, nil
}

// projectRestrictedCredential seeds the adapter's access-only projection of the selected login,
// after the host renewed it for the credential horizon. Refresh authority stays in the source
// profile; a login the adapter cannot make portable (Gemini's host-bound keychain) refuses here.
func projectRestrictedCredential(ag agents.Agent, source, target string) error {
	live := ag.LiveCredentials()
	if len(live.Artifacts) == 0 || live.Portability == nil {
		return fmt.Errorf("%s has no portable credential projection", ag.Name())
	}
	deadline := time.Now().Add(RestrictedCredentialHorizon)
	if live.Prepare != nil {
		if err := live.Prepare(source, deadline); err != nil {
			return fmt.Errorf("%s credential needs sign-in or renewal: %w", ag.Name(), err)
		}
	}
	primary := false
	for _, artifact := range live.Artifacts {
		if artifact.Project == nil || filepath.Base(artifact.Name) != artifact.Name || artifact.Name == "." || artifact.Name == ".." {
			return fmt.Errorf("%s credential projection is invalid", ag.Name())
		}
		data, present, err := readSeedArtifact(filepath.Join(source, artifact.Name))
		if err != nil {
			return fmt.Errorf("%s credential %s: %w", ag.Name(), artifact.Name, err)
		}
		if present {
			if data, err = artifact.Project(data); err != nil {
				return fmt.Errorf("project %s credential: %w", ag.Name(), err)
			}
		}
		if data == nil {
			if artifact.Primary {
				return fmt.Errorf("%s has no portable stored login — run 'coop login %s'", ag.Name(), ag.Name())
			}
			continue
		}
		if err := os.WriteFile(filepath.Join(target, artifact.Name), data, 0o600); err != nil {
			return fmt.Errorf("seed %s credential: %w", ag.Name(), err)
		}
		primary = primary || artifact.Primary
	}
	if !primary {
		return fmt.Errorf("%s has no portable stored login — run 'coop login %s'", ag.Name(), ag.Name())
	}
	if live.Portability(target, deadline) != agents.CredentialPortable {
		return fmt.Errorf("%s credential is not usable for the next %s without refresh authority — run 'coop login %s'", ag.Name(), RestrictedCredentialHorizon, ag.Name())
	}
	return nil
}

// readSeedArtifact reads one host credential file without following a link, bounded, or reports
// it absent.
func readSeedArtifact(path string) ([]byte, bool, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, false, errors.New("not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, restrictedArtifactLimit+1))
	if err != nil || len(data) > restrictedArtifactLimit {
		return nil, false, errors.New("unreadable or too large")
	}
	return data, true, nil
}

// restrictedPlan is what the assembled runtime options must be checked against before launch:
// the one workdir, the exact tmpfs set, and the only host sources a (read-only) bind may name.
type restrictedPlan struct {
	workdir string
	tmpfs   map[string]bool
	sources map[string]bool
}

// validateRestrictedOptions proves the final options against the plan. It is an allowlist on
// purpose: an option this profile has not admitted refuses the launch by name, so a mount or flag
// added to the shared assembly later cannot widen a restricted run without being admitted here.
func validateRestrictedOptions(options []string, plan restrictedPlan) error {
	readOnly := 0
	tmpfs := map[string]bool{}
	for i := 0; i < len(options); i++ {
		opt := options[i]
		switch opt {
		case "--init", "-it", "-i":
			continue
		case "--read-only":
			readOnly++
			continue
		}
		if i+1 >= len(options) {
			return fmt.Errorf("restricted launch: %s has no value", opt)
		}
		i++
		value := options[i]
		switch opt {
		case "--label", "-e", "--env", "--env-file", "--cap-drop", "--pids-limit", "--memory", "--cpus":
		case "--security-opt":
			if value != "no-new-privileges" {
				return fmt.Errorf("restricted launch: security option %q is not part of the profile", value)
			}
		case "--network":
			if value != "none" {
				return fmt.Errorf("restricted launch: joins no network (%q)", value)
			}
		case "-w":
			if value != plan.workdir {
				return fmt.Errorf("restricted launch: workdir %q is not the planned %q", value, plan.workdir)
			}
		case "--tmpfs":
			if !plan.tmpfs[value] {
				return fmt.Errorf("restricted launch: tmpfs %q is not in the profile", value)
			}
			tmpfs[value] = true
		case "-v", "--volume":
			rest, ok := strings.CutSuffix(value, ":ro")
			if !ok {
				return fmt.Errorf("restricted launch: writable bind %q", value)
			}
			sep := strings.LastIndex(rest, ":")
			if sep <= 0 || !plan.sources[rest[:sep]] {
				return fmt.Errorf("restricted launch: bind %q is not from a planned source", value)
			}
		default:
			return fmt.Errorf("restricted launch: runtime option %q is not part of the profile", opt)
		}
	}
	if readOnly != 1 {
		return errors.New("restricted launch: the root filesystem is not read-only")
	}
	if len(tmpfs) != len(plan.tmpfs) {
		return errors.New("restricted launch: a planned scratch tmpfs is missing")
	}
	return nil
}

// runRestricted is the launch of a readonly or bare run: the normal launch's spec checks, the
// mount plan, the seed, then the shared option assembly on a spec with every optional exposure
// off, and the proof of the result against the plan before the runtime starts.
func runRestricted(cfg *config.Config, rt runtime.Runtime, spec RunSpec, artifacts compositionArtifactOps, mode agents.ExecutionMode) (exitCode int, result error) {
	// The same narration an ordinary interactive launch gets (launch_sections.go): the secrets it
	// hid, the one Internet access section, the agent's name, and why the box stopped. A bare run
	// mounts no repository, so it has nothing to hide and says so.
	sections := newLaunchSections(spec)
	started := false
	defer func() {
		if !started {
			result = sections.failed(result)
		}
	}()
	if err := checkRestrictedSpec(cfg, rt, spec, mode); err != nil {
		return -1, err
	}
	// assembleOptions appends COOP_RUN_ARGS from the config it is handed, so the admitted form
	// rides a copy; this run's own arguments are normalized in place.
	runArgs, err := restrictedRuntimeArgs(cfg.ExtraRunArgs, mode, "COOP_RUN_ARGS")
	if err != nil {
		return -1, err
	}
	admitted := *cfg
	admitted.ExtraRunArgs = runArgs
	cfg = &admitted
	if spec.ExtraArgs, err = restrictedRuntimeArgs(spec.ExtraArgs, mode, "its own runtime arguments"); err != nil {
		return -1, err
	}
	workdir := BareWorkdir
	if mode == agents.ModeReadOnly {
		workdir = resolveWorkdir(spec, cfg)
	}
	if spec.Homes {
		if err := ensureAgentHomes(cfg, spec); err != nil {
			return -1, err
		}
	}
	scope := credentialScope(cfg, spec)
	filesystem := restrictedFilesystemArgs(cfg, mode)
	plan := restrictedPlan{workdir: workdir, tmpfs: map[string]bool{}, sources: map[string]bool{}}
	for i, arg := range filesystem {
		if arg == "--tmpfs" {
			plan.tmpfs[filesystem[i+1]] = true
		}
	}

	var tmpFiles, tmpDirs []string
	defer func() {
		for _, f := range tmpFiles {
			os.Remove(f)
		}
		for _, d := range tmpDirs {
			os.RemoveAll(d)
		}
	}()
	var mounts []Mount
	decoy, decoyDir := "", ""
	if mode == agents.ModeReadOnly {
		mounts, err = ComputeMounts(spec.Repo, workdir)
		if err != nil {
			return -1, err
		}
		mounts[0].RO = true // ComputeMounts guarantees the primary repo bind is first
		companionMounts, companionEnvironment, err := companionRepositoryMounts(spec.CompanionRepositories)
		if err != nil {
			return -1, err
		}
		mounts = append(mounts, companionMounts...)
		if len(companionEnvironment) > 0 {
			data, err := json.Marshal(companionEnvironment)
			if err != nil {
				return -1, fmt.Errorf("encode companion repository environment: %w", err)
			}
			spec.ExtraArgs = append(spec.ExtraArgs, "-e", "COOP_COMPANION_REPOSITORIES_JSON="+string(data))
		}
		if n := ShadowCount(mounts); sections.on {
			sections.secrets(n)
		} else if n > 0 && !spec.Quiet {
			ui.Note("shadowed %d secret path(s)", n)
		}
		// One empty read-only file shadows every secret file, one empty read-only dir every
		// secret directory — the same decoys the normal launch uses.
		decoyFile, err := os.CreateTemp(artifacts.parent, "coop-decoy-")
		if err != nil {
			return -1, err
		}
		decoyFile.Close()
		decoy = decoyFile.Name()
		tmpFiles = append(tmpFiles, decoy)
		if decoyDir, err = os.MkdirTemp(artifacts.parent, "coop-decoy-dir-"); err != nil {
			return -1, err
		}
		tmpDirs = append(tmpDirs, decoyDir)
		plan.sources[spec.Repo], plan.sources[decoy], plan.sources[decoyDir] = true, true, true
		for _, companion := range spec.CompanionRepositories {
			plan.sources[companion.HostPath] = true
		}
	}

	cmd := spec.Cmd
	if spec.Agent != "" {
		ag, ok := agents.Get(spec.Agent)
		if !ok {
			return -1, fmt.Errorf("unknown agent %q", spec.Agent)
		}
		if spec.AgentCommand {
			if cmd, err = ag.RestrictedCommand(mode, cmd); err != nil {
				return -1, err
			}
		} else if _, err := ag.ACPRestrictedSessionMeta(mode); err != nil {
			// The adapter takes no flags; its switches ride the session/new the client outside
			// the box sends. The box still refuses an adapter with no proven switch, so an
			// unqualified provider cannot be started under a mode nothing enforces.
			return -1, err
		}
	}
	var extras []string
	if len(scope) > 0 {
		seed, err := buildRestrictedSeed(cfg, scope[0], mode, workdir, artifacts)
		if err != nil {
			return -1, err
		}
		tmpDirs = append(tmpDirs, seed)
		seedHome := filepath.Join(seed, "home")
		plan.sources[seedHome] = true
		extras = append(extras, "-v", seedHome+":"+restrictedSeedPath+":ro")
		cmd = seedPrelude(cfg.HomeInBox, cmd)
	}
	// No .tool-versions provisioning: it would compile a toolchain into scratch on every run.
	extras = append(extras, "-e", "COOP_NO_ASDF=1")
	extras = append(extras, modelEnvArgs(cfg, spec, scope)...)
	// Each agent's own box env, as the normal launch exports it (claude's CLAUDE_CONFIG_DIR points
	// into the tmpfs home, which is exactly where the seed lands).
	for _, name := range agents.Names() {
		if a, ok := agents.Get(name); ok {
			for _, kv := range a.BoxEnv(cfg.HomeInBox) {
				extras = append(extras, "-e", kv)
			}
		}
	}
	envFile, envTmp, err := prepareBoxEnvFile(cfg, spec, artifacts, nil)
	if err != nil {
		return -1, err
	}
	if envTmp != "" {
		tmpFiles = append(tmpFiles, envTmp)
	}
	if err := rt.EnsureDaemon(); err != nil {
		return -1, err
	}

	// The shared assembly on a spec with every optional exposure off: no homes (so no credential
	// bind, skills, transcripts, instruction or git mounts), no cache or asdf volume, no services
	// network, no published port. What the profile adds rides in the limits slot and in extras.
	plain := spec
	plain.Homes, plain.Cache, plain.Network, plain.Serve, plain.ShareACPSessions = false, false, false, false, false
	plain.servePorts, plain.servePlan = nil, nil
	network := "none" // egress fails closed exactly as in assembleArgs; open means the plain bridge
	if cfg.Egress == "open" {
		network = ""
	}
	tty := decideTTY(spec, ui.IsTerminal(os.Stdin))
	limits := append(boxLimits(cfg, rt), filesystem...)
	options := assembleOptions(cfg, rt.SupportsInit(), plain, mounts, decoy, decoyDir, workdir, tty, false,
		nil, nil, nil, nil, nil, network, envFile, limits...)
	options = append(options, extras...)
	if err := validateRestrictedOptions(options, plan); err != nil {
		return -1, err
	}
	args := append([]string{"run", "--rm"}, options...)
	args = append(append(args, spec.Image), cmd...)

	var stdin io.Reader
	if tty == ttyInteractive || tty == ttyStdinOnly {
		stdin = os.Stdin
	}
	stdout, stderr := spec.Stdout, spec.Stderr
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	if err := ctxStep(spec.Ctx, "argument assembly"); err != nil {
		return -1, err
	}
	if spec.OnRuntimeLaunch != nil {
		spec.OnRuntimeLaunch()
	}
	if mode == agents.ModeBare {
		sections.secrets(0) // nothing mounted, nothing to hide — said rather than skipped
	}
	sections.internet(cfg, spec, nil)
	sections.starting()
	if spec.Ctx != nil {
		code, runErr := rt.RunInterruptible(spec.Ctx, stdin, stdout, stderr, args...)
		started = true
		reason := stopReason(code, runErr, nil)
		if spec.Ctx.Err() == nil || spec.RunID == "" {
			sections.stopped(reason) // the client ran to its end and --rm took the box with it
			return code, runErr
		}
		// A cancelled client leaves the daemon's container behind, so the stop is only real once
		// this backstop removal proves it.
		settled := sections.stopping()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, cleanupErr := rt.RemoveByLabel(cleanupCtx, LabelRun, spec.RunID)
		settled()
		if cleanupErr == nil {
			sections.stopped(reason)
		}
		return code, errors.Join(runErr, cleanupErr)
	}
	code, runErr := rt.Run(stdin, stdout, stderr, args...)
	// The plain client ran the box to its end, so its exit status is the main process's; a client
	// that could not start is the one case with no box to stop, narrated as the failure above.
	if started = runErr == nil; started {
		sections.stopped(stopReason(code, nil, nil))
	}
	return code, runErr
}
