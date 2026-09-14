package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/forkctl"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/preset"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/sessionsvc"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

func loadProject(repoOverride string) (string, *project.Project, error) {
	repo, err := box.ResolveRepo(repoOverride)
	if err != nil {
		return "", nil, err
	}
	p, err := project.Load(repo)
	if err != nil {
		return "", nil, err
	}
	return repo, p, nil
}

// resolveImage resolves the repo and its image, verifying the image is built.
func (a *app) resolveImage() (repo, img string, err error) {
	repo, _, err = loadProject(a.cfg.RepoOverride)
	if err != nil {
		return "", "", err
	}
	if err := a.ensureRuntime(); err != nil { // the choke point for box commands not eagerly detected in dispatch (fork)
		return "", "", err
	}
	img = box.ImageForRepo(repo, a.cfg.BaseImage, a.cfg.ImageOverride)
	if a.loginProvider != "" && a.cfg.ImageOverride == "" {
		img = a.cfg.BaseImage // authentication must not depend on the project's toolchain image
	}
	if !box.ImageExists(a.rt, img) {
		// `image inspect` fails the same way whether the image is missing or the daemon is gone, so
		// probe the daemon before blaming the image — a Docker restart otherwise tells every box
		// command to run a build that would not have helped. Only on this branch: the happy path
		// must not pay for an extra `docker info`.
		if err := a.rt.EnsureDaemon(); err != nil {
			return "", "", err
		}
		return "", "", fmt.Errorf("image %q not built — run 'coop build'", img)
	}
	return repo, img, nil
}

// runInBox runs a command in the box against the current repo with the default
// homes/network/cache toggles (the common interactive path). agent names the registered agent
// being driven so its credentials are mounted and, with named peers,
// it gets the second-opinion directive plus exactly those peers' credentials. Pass
// "" for raw commands (coop run/shell) that aren't an agent session — they mount no
// agent credentials.
func (a *app) runInBox(cmd []string, agent string, peers []agents.Target) (int, error) {
	return a.runInBoxMode(cmd, agent, peers, false)
}

// takeNetworkFlags strips coop's own --egress/--allow-domain/--egress-rules from
// a launch's arguments and remembers them for the admission this run performs.
// Parsing stops at `--`: everything after it belongs to the agent or command.
func (a *app) takeNetworkFlags(args []string) ([]string, error) {
	flags, rest, err := extractNetworkFlags(a.launchCommand(), args)
	if err != nil {
		return nil, err
	}
	a.network = flags
	return rest, nil
}

func (a *app) runAgentInBox(cmd []string, agent string, peers []agents.Target) (int, error) {
	return a.runInBoxMode(cmd, agent, peers, true)
}

func (a *app) runAgentCommandInBox(cmd []string, agent string, peers []agents.Target, args []string) (int, error) {
	// `codex exec` writes source:"exec" rollouts, which discovery excludes. It remains fully
	// concurrent with interactive Codex sessions; only source:"cli" producers need serialization.
	if !agentCommandProducesInteractiveSession(agent, args) {
		return a.runInBox(cmd, agent, peers)
	}
	return a.runAgentInBox(cmd, agent, peers)
}

func agentCommandProducesInteractiveSession(agent string, args []string) bool {
	ag, ok := agents.Get(agent)
	if !ok {
		return true
	}
	// Only a session-discovering adapter (codex) can distinguish a headless invocation from
	// an interactive one; everything else always produces an interactive session.
	if d, ok := ag.(agents.SessionDiscoverer); ok {
		return d.ProducesSession(args)
	}
	return true
}

func (a *app) lockInteractiveSession(agent, repo string) (func(), error) {
	if ag, ok := agents.Get(agent); ok {
		if _, discovers := ag.(agents.SessionDiscoverer); discovers {
			return lockSessionProducer(a.cfg, agent, box.Workdir(a.cfg, repo))
		}
	}
	return func() {}, nil
}

// lockSessionProducer excludes every Coop-owned interactive producer from one native history
// scope while a fork attributes a new ID. ConfigDir/.locks is host-only and shared across repos.
// Contention fails fast because an interactive session can remain open for hours.
func lockSessionProducer(cfg *config.Config, provider, cwd string) (func(), error) {
	dir := filepath.Join(cfg.ConfigDir, ".locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	profile := cfg.AgentDir(provider)
	if resolved, err := filepath.EvalSymlinks(profile); err == nil {
		profile = resolved
	} else if absolute, absErr := filepath.Abs(profile); absErr == nil {
		profile = absolute
	}
	sum := sha256.Sum256([]byte(profile + "\x00" + cwd))
	path := filepath.Join(dir, fmt.Sprintf("session-%x.lock", sum[:12]))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("another interactive %s session is active for account %q in workdir %q", provider, cfg.ActiveProfile(provider), cwd)
		}
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

func (a *app) runInBoxMode(cmd []string, agent string, peers []agents.Target, session bool) (int, error) {
	if a.mode.Restricted() {
		if len(peers) > 0 {
			return 2, fmt.Errorf("a %s run consults no peers — drop --peer", a.mode)
		}
		return a.runRestrictedInBox(cmd, agent)
	}
	companionRepositories, err := sessionCompanionRepositoriesFromEnvironment()
	if err != nil {
		return -1, err
	}
	repo, img, err := a.resolveImage()
	if err != nil {
		return -1, err
	}
	if session {
		release, err := a.lockInteractiveSession(agent, repo)
		if err != nil {
			return 1, err
		}
		defer release()
	}
	lead := ""
	if len(peers) > 0 || (a.preset != nil && agent != "") {
		lead = agent // a preset makes the agent a lead too: its routing contract mounts via ConsultLead
	}
	pre := gitOut(repo, "rev-parse", "HEAD")
	activityRepo, forkIdentity, err := forkspace.ResolveProjectBinding(repo)
	if err != nil {
		return 1, err
	}
	spec := box.RunSpec{
		Image: img, Repo: repo, Cmd: cmd, Agent: agent, ConsultLead: lead, Peers: peers, Preset: a.preset,
		ActivityRepo: activityRepo, ActivityKind: forkspace.ExecutionInteractive,
		AgentCommand: agent != "",
		Homes:        a.cfg.Homes, Network: a.cfg.Network, Cache: a.cfg.Cache, Serve: true,
		CompanionRepositories: companionRepositories,
		StartingNotice:        a.loginStartingNotice(agent),
		Login:                 a.loginProvider == agent && agent != "",
	}
	if spec.Login {
		spec.Preset, spec.Peers, spec.ConsultLead = nil, nil, ""
		spec.CompanionRepositories = nil
		spec.Network, spec.Serve, spec.AgentCommand = false, false, false
	}
	if forkIdentity != nil {
		spec.ActivityKind = forkspace.ExecutionForkInteractive
		spec.ForkName = forkIdentity.Name
		spec.ForkGeneration = string(forkIdentity.Generation)
		spec.ForkOwner = forkctl.ForkContainerOwner(activityRepo, forkIdentity.Name, forkIdentity.Generation)
	}
	// Network admission runs on the HOST, before any container: it resolves this
	// launch's posture and, for filtered mode, freezes the policy the gateway
	// will enforce. An open or offline run gets a nil capture and proceeds
	// exactly as it does today.
	capture, err := box.AdmitNetwork(a.cfg, a.rt, spec, a.network.admission())
	if err != nil {
		return 1, err
	}
	defer capture.Close()
	spec.CapturedEgress = capture
	// A filtered box runs the qualified client image, so the shared image's currency is not its
	// concern; an open or offline one runs this repo's, repaired here when its definition drifted.
	if capture == nil {
		if err := a.checkCoopBox(repo, img); err != nil {
			return 1, err
		}
	}
	code, err := box.Run(a.cfg, a.rt, spec)
	// An interactive/run box makes unsigned commits; sign what THIS session produced on exit so a
	// protected remote accepts them. Best-effort, session-scoped, skipped for a dirty tree.
	if !spec.Login {
		a.signOnBoxExit(repo, pre, false)
	}
	return code, err
}

// restrictedImage is the one image a restricted launch may run — the shared base image, built —
// with the usage refusals every restricted launch shares reported first, so a wrong flag is
// reported as such and never as a missing docker. Bare has no project to admit a network policy
// for, so it takes only an open or offline --egress; the code is the exit status to return.
func (a *app) restrictedImage() (img string, code int, err error) {
	mode := a.mode
	if a.cfg.ImageOverride != "" {
		return "", 2, fmt.Errorf("a %s run uses the shared base image — unset COOP_IMAGE", mode)
	}
	if mode == agents.ModeBare && (a.network.Domains != nil || a.network.RulesFile != "" || (a.network.Mode != nil && *a.network.Mode == egress.Filtered)) {
		return "", 2, errors.New("a bare run has no project to admit network policy for — it takes --egress open or none only")
	}
	if err := a.ensureRuntime(); err != nil {
		return "", -1, err
	}
	img = a.cfg.BaseImage
	if !box.ImageExists(a.rt, img) {
		if err := a.rt.EnsureDaemon(); err != nil { // as resolveImage: blame a stopped daemon, not the image
			return "", -1, err
		}
		return "", 1, fmt.Errorf("image %q not built — run 'coop build'", img)
	}
	return img, 0, nil
}

// runRestrictedInBox is the launch behind --readonly and --bare. Both run the shared base image
// under box's restricted filesystem profile and neither publishes activity, starts services or
// signs on exit — there is nothing a read-only checkout could have committed. Readonly resolves
// the repository like every other launch and admits its network posture the same way (a filtered
// result is refused by box.Run: the modes are not qualified under it). Bare resolves no project at
// all — it must work outside any Git repository — so it takes only an open or offline --egress.
func (a *app) runRestrictedInBox(cmd []string, agent string) (int, error) {
	mode := a.mode
	img, code, err := a.restrictedImage()
	if err != nil {
		return code, err
	}
	spec := box.RunSpec{Image: img, Cmd: cmd, Agent: agent, AgentCommand: agent != "", Homes: a.cfg.Homes, Mode: mode}
	spec.StartingNotice = a.loginStartingNotice(agent)
	switch mode {
	case agents.ModeBare:
		if a.network.Mode != nil {
			a.cfg.SetEgress(string(*a.network.Mode))
		}
	case agents.ModeReadOnly:
		repo, err := box.ResolveRepo(a.cfg.RepoOverride)
		if err != nil {
			return -1, err
		}
		companionRepositories, err := sessionCompanionRepositoriesFromEnvironment()
		if err != nil {
			return -1, err
		}
		spec.Repo, spec.CompanionRepositories = repo, companionRepositories
		capture, err := box.AdmitNetwork(a.cfg, a.rt, spec, a.network.admission())
		if err != nil {
			return 1, err
		}
		defer capture.Close()
		spec.CapturedEgress = capture
		// The shared base image is this launch's image too, so a definition that drifted is
		// repaired here as on an ordinary launch. Bare has no project to build from and skips it.
		if err := a.checkCoopBox(repo, img); err != nil {
			return 1, err
		}
	}
	return box.Run(a.cfg, a.rt, spec)
}

func sessionCompanionRepositoriesFromEnvironment() ([]box.CompanionRepository, error) {
	raw := os.Getenv("COOP_SESSION_COMPANIONS")
	if raw == "" {
		return nil, nil
	}
	if len(raw) > sessionsvc.PolicyFileLimit {
		return nil, errors.New("session companion repository binding is too large")
	}
	var bindings []struct {
		Name       string `json:"name"`
		Repository string `json:"repository"`
		Workspace  string `json:"workspace"`
		BaseCommit string `json:"base_commit"`
	}
	if err := json.Unmarshal([]byte(raw), &bindings); err != nil {
		return nil, errors.New("session companion repository binding is malformed")
	}
	repositories := make([]box.CompanionRepository, 0, len(bindings))
	for _, binding := range bindings {
		repositories = append(repositories, box.CompanionRepository{
			Name: binding.Name, HostPath: binding.Workspace,
			BaseCommit: binding.BaseCommit,
		})
	}
	return repositories, nil
}

// runOptions are the options `coop run` itself takes; everything else belongs to the command after
// `--`. They are what a mistyped leading option is measured against.
var runOptions = []string{"--readonly", "--bare", "--egress", "--allow-domain", "--egress-rules"}

func (a *app) cmdRun(args []string) (int, error) {
	args, err := a.takeNetworkFlags(args)
	if err != nil {
		return 2, err
	}
	if args, err = a.takeExposureFlags("coop run", args); err != nil {
		return 2, err
	}
	// Intercept the meta cases before entering the box. We can't lean on the dispatch's --help
	// handling here: it's `--`-blind, so it would mistake `coop run -- --help` (run --help in the
	// box) for a help request. Honor -- ourselves.
	if len(args) > 0 && args[0] == "--" {
		args = args[1:] // everything after -- runs verbatim
	} else if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		printHelpPage(runHelp) // not forwarded to the box, where it would exec `--help` and crash
		return 0, nil
	}
	if len(args) == 0 {
		// `coop run` runs a raw command; it does not default to an agent (use `coop claude`).
		return 2, ui.MissingArgument("command", "coop run", "coop run [<options>] -- <command> [<args>...]")
	}
	// A leading option Coop did not take is a typo, not a program: refuse it HERE, before the box
	// is checked or built. Past `--` the same token is the command's own and is forwarded verbatim.
	if strings.HasPrefix(args[0], "-") {
		return 2, unknownOptionErr(args[0], "coop run", runOptions)
	}
	return a.runInBox(args, "", nil) // raw command runner — not an agent session
}

// launchAgent runs a named agent target: its autonomous default command, with any extra CLI
// args you pass appended — so `coop claude --continue` keeps coop's autonomy + MCP
// flags and just adds yours. The agents' autonomous flags are global, so this is safe
// even before subcommands (e.g. `coop codex resume --last`). coop's own --peer and
// -- separator are stripped first so they aren't forwarded to the agent. A preset lead runs
// via launchPreset instead (the who-runs positional names a target OR a preset, never both).
func (a *app) launchAgent(target string, args []string) (int, error) {
	// The head is a target: provider[:model][/effort][@account]. Model, effort, and account ride it.
	t, err := agents.ParseTarget(target)
	if err != nil {
		return 2, targetUsage(err, "coop", "coop <agent>[:<model>][/<effort>][@<account>]")
	}
	tool := t.Provider
	args, err = a.takeNetworkFlags(args)
	if err != nil {
		return 2, err
	}
	if args, err = a.takeExposureFlags("coop "+target, args); err != nil {
		return 2, err
	}
	peerVals, args, err := extractPeer(a.launchCommand(), args)
	if err != nil {
		return 2, err
	}
	// `coop claude login` reads as "log in to claude" — route it to the sign-in flow like
	// `coop login claude`; the account rides the target (`coop claude@work login`).
	if len(args) >= 1 && args[0] == "login" {
		if a.mode.Restricted() {
			return 2, fmt.Errorf("'coop %s login' signs in on the host; it takes no --%s", tool, a.mode)
		}
		acct, aerr := singleAccount(t, "coop "+tool+" login")
		if aerr != nil {
			return 2, aerr
		}
		if len(args) > 1 {
			return 2, ui.UnexpectedArgument(args[1], "coop "+tool+" login", "coop "+tool+"[@<account>] login")
		}
		return a.loginTo(tool, acct)
	}
	if err := a.applyRunTarget(t, "coop "+tool); err != nil {
		return 2, err
	}
	a.nudgeIfUnauthed(tool)
	peers, err := a.resolvePeers("coop "+tool, peerVals)
	if err != nil {
		return 2, err
	}
	providerArgs := dropDashDash(args)
	return a.runAgentCommandInBox(append(append([]string{}, a.defaultCmd(tool)...), providerArgs...), tool, peers, providerArgs)
}

// launchPreset runs an orchestration preset interactively (`coop <preset>`): its lead agent
// leads the session, its roles seed the run (routing contract, role models/credentials,
// wrappers). The who-runs positional named the preset, so there's no target to fold in — the
// lead ladder's first entry supplies the lead's model/account (applyPreset). --peer still adds
// ad-hoc read-only peers on top of the preset's own consult roles.
func (a *app) launchPreset(p *preset.Preset, args []string) (int, error) {
	tool := p.Lead().Provider
	args, err := a.takeNetworkFlags(args)
	if err != nil {
		return 2, err
	}
	if args, err = a.takeExposureFlags("coop "+p.Name, args); err != nil {
		return 2, err
	}
	if a.mode.Restricted() {
		// A preset's roles run from inside the box, which a restricted run gives no credentials
		// or wrappers to. The lead alone can run restricted, as a plain target.
		return 2, fmt.Errorf("a preset runs its roles from the box, which a %s run has none of — run its lead directly: coop %s --%s", a.mode, tool, a.mode)
	}
	peerVals, args, err := extractPeer(a.launchCommand(), args)
	if err != nil {
		return 2, err
	}
	if err := a.applyPinnedPreset(p, tool); err != nil {
		return 2, err
	}
	a.nudgeIfUnauthed(tool)
	peers, err := a.resolvePeers("coop "+p.Name, peerVals)
	if err != nil {
		return 2, err
	}
	providerArgs := dropDashDash(args)
	return a.runAgentCommandInBox(append(append([]string{}, a.defaultCmd(tool)...), providerArgs...), tool, peers, providerArgs)
}

// nudgeIfUnauthed prints one heads-up (TTY only, never blocks) when the account this run will use
// isn't signed in — so a first `coop claude` names the fix instead of failing opaquely inside the
// box. It stays a WARNING: the launch continues, because an agent CLI may still find its own
// credentials, and only a run that truly requires one refuses (needsAccountErr).
func (a *app) nudgeIfUnauthed(tool string) {
	if !ui.IsTerminal(os.Stdin) {
		return
	}
	if !box.ProfileAuthed(a.cfg, tool, a.cfg.ActiveProfile(tool)) {
		warnRows(titleName(tool)+" is not signed in", [2]string{"Sign in:", "coop login " + tool})
	}
}

// selectRunProfile points cfg at the credential profile chosen with the target's @account for a
// run of tool (a no-op when profile is ""). It requires a stored profile or the effective env-only
// default — a typo otherwise silently creates an empty husk dir (box.Run pre-creates the active
// profile), the very clutter `coop credentials rm` cleans up — and notes (without blocking) one
// that isn't signed in.
// Shared by every agent-launch path: launchAgent, launchPreset, cmdACP.
func (a *app) selectRunProfile(tool, profile string) error {
	if profile == "" {
		return nil
	}
	if !slices.Contains(box.EffectiveProfiles(a.cfg, tool), profile) {
		return noAccountErr(tool, profile)
	}
	if !box.ProfileAuthed(a.cfg, tool, profile) {
		warnRows(fmt.Sprintf("%s is not signed in", profile),
			[2]string{"Sign in:", agents.LoginCommand(tool + "@" + profile)})
	}
	a.cfg.SetActiveProfile(tool, profile)
	return nil
}

// selectRunModel points cfg at the model chosen by a run target (a no-op when model is "").
// Deliberately unvalidated: model ids churn faster than coop releases, so the
// agent CLI stays the source of truth — a bad id fails loudly in the agent's own error.
// Shared by every agent-launch path: launchAgent, launchPreset, cmdACP, and the fork paths.
func (a *app) selectRunModel(tool, model string) {
	if model != "" {
		a.cfg.SetActiveModel(tool, model)
	}
}

// selectRunEffort applies a single run's explicit reasoning effort (the target's /effort) to
// tool's top tier, mirroring selectRunModel. Empty is a no-op (the agent's default stands).
func (a *app) selectRunEffort(tool, effort string) {
	if effort != "" {
		a.cfg.SetActiveEffort(tool, effort)
	}
}

// applyOneOff applies a single run's decomposed one-off (model, account) to tool: model may
// carry a model@account shortcut (matching a preset ladder entry), and credential pins the
// account. Both empty is a no-op — the preset/default stands. It's the single-run analog of
// the loop's oneOffLadder; a bad shape (e.g. an account given in both the model's @ and
// credential) errors.
func (a *app) applyOneOff(tool, model, credential, effort string) error {
	a.selectRunEffort(tool, effort) // effort rides with the model but can be set even when model/account aren't
	rungs, err := oneOffLadder(model, credential, effort)
	if err != nil {
		return err
	}
	if rungs == nil {
		return nil
	}
	t := rungs[0]
	if err := a.selectRunProfile(tool, t.Account()); err != nil {
		return err
	}
	a.selectRunModel(tool, t.Model)
	return nil
}

// extractPeer pulls every --peer <target> (repeatable) out of a run's args — each value is one
// peer the lead may consult read-only on hard calls (see box.RunSpec.Peers). A valueless occurrence
// is refused in the shared block, naming the command it came from. `--`-aware. The one --peer parser
// for every command (the retired --consult spelling is now just an unknown flag).
func extractPeer(command string, args []string) (peers, rest []string, err error) {
	return extractRepeatable(command, args, "--peer",
		command+" --peer <target> [--peer <target>...]", command+" --peer codex")
}

// extractRepeatable collects every `--flag <value>` occurrence (repeatable) out of args, in
// order, returning the values and the remaining args. A valueless occurrence (a typo, or a bare
// flag) is refused in the shared block, with example as the spelling that works. Stops at `--` — everything after is the agent's
// own, forwarded verbatim (so an agent's OWN --peer still reaches it).
func extractRepeatable(command string, args []string, flag, usage, example string) (vals, rest []string, err error) {
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			return vals, append(rest, args[i:]...), nil
		}
		if args[i] == flag || strings.HasPrefix(args[i], flag+"=") {
			v, n, _, e := flagValue(args, i, flag)
			if e != nil {
				return nil, nil, ui.MissingRepeatableOptionValue(flag, command, usage, example)
			}
			vals = append(vals, v)
			i += n - 1
			continue
		}
		rest = append(rest, args[i])
	}
	return vals, rest, nil
}

// dropDashDash removes the first "--" from args. coop uses "--" to mark the end of ITS own flags;
// the separator must not reach the agent. Without this, `coop claude -- -p "x"` runs claude with
// `-- -p "x"` — the agent reads everything after `--` as positional, so `-p` stops being a flag
// (and `coop codex -- --profile w` never reaches codex's own --profile). It's stripped only here,
// after every coop-flag extractor has run, since those need the `--` to know where coop's flags end.
func dropDashDash(args []string) []string {
	for i, a := range args {
		if a == "--" {
			out := append([]string{}, args[:i]...)
			return append(out, args[i+1:]...)
		}
	}
	return args
}

// defaultCmd is the agent's autonomous interactive command; an unknown name runs as a
// raw passthrough (so `coop npm test` still works).
func (a *app) defaultCmd(tool string) []string {
	if ag, ok := agents.Get(tool); ok {
		return ag.Interactive(a.cfg)
	}
	return []string{tool}
}

// loginUsage is `coop login`'s syntax, shared by its missing- and extra-argument refusals.
const loginUsage = "coop login <agent>[@<account>]"

func (a *app) cmdLogin(args []string) (int, error) {
	// The account rides the target (coop login claude@work).
	// The agent is required — bare `coop login` must not silently default to one (it would open a
	// browser and block); name it explicitly, like the help shows. A stray extra arg is a typo,
	// not a second target, so reject it rather than silently ignore.
	if len(args) == 0 {
		return 2, ui.MissingArgument("agent", "coop login", loginUsage)
	}
	// An option-shaped token stays an OPTION error — login accepts none, so point at its help
	// rather than calling a flag a stray positional.
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			return 2, unknownOptionErr(arg, "coop login", nil)
		}
	}
	if len(args) > 1 {
		return 2, ui.UnexpectedArgument(args[1], "coop login", loginUsage)
	}
	t, err := agents.ParseTarget(args[0])
	if err != nil {
		return 2, targetUsage(err, "coop login", loginUsage)
	}
	// login authenticates an account; a :model in the target has no meaning here.
	if t.Model != "" {
		return 2, &ui.UsageError{
			Headline: `"coop login" does not take a model`,
			Rows:     [][2]string{{"Usage:", loginUsage}, {"Help:", "coop help login"}},
		}
	}
	acct, err := singleAccount(t, "coop login")
	if err != nil {
		return 2, err
	}
	return a.loginTo(t.Provider, acct)
}

// flagValue extracts the value of a value-bearing flag at args[i], handling both
// `--flag value` and `--flag=value`. ok reports whether args[i] is this flag at all;
// consumed is how many tokens it spans (1 or 2). It errors when the value is missing — the
// flag is the last token, its value is another flag (a leading '-'), or `--flag=` is empty —
// so a typo'd flag fails loudly instead of silently falling back to a default. Values for
// coop's own flags never start with '-', so treating a '-' next token as "missing" is safe.
func flagValue(args []string, i int, flag string) (val string, consumed int, ok bool, err error) {
	switch a := args[i]; {
	case a == flag:
		if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
			return "", 0, true, fmt.Errorf("%s needs a value", flag)
		}
		return args[i+1], 2, true, nil
	case strings.HasPrefix(a, flag+"="):
		if v := strings.TrimPrefix(a, flag+"="); v != "" {
			return v, 1, true, nil
		}
		return "", 0, true, fmt.Errorf("%s needs a value", flag)
	}
	return "", 0, false, nil
}

// validProfileName keeps a target's @account name to a single safe path segment, so it cannot
// traverse or collide outside the agent's profiles/ vault (no '/', '\', '..',
// '.', empty, or leading '-'). Login is the path that CREATES the dir from the name, so it's the
// gate; runs/select/rm/default already require an existing profile.
func validProfileName(name string) bool {
	if name == "" || name == "." || name == ".." || strings.HasPrefix(name, "-") {
		return false
	}
	return !strings.ContainsAny(name, "/\\")
}

var (
	readLoginSecret      = ui.ReadSecret
	loginInputIsTerminal = func() bool { return ui.IsTerminal(os.Stdin) }
)

// loginTo runs an agent's sign-in flow in the box; its token persists in the agent's
// config dir for the chosen credential. Shared by `coop login <provider>[@<account>]` and
// `coop <provider>[@<account>] login`.
func (a *app) loginTo(tool, profile string) (int, error) {
	ag, ok := agents.Get(tool)
	if !ok {
		return 2, unknownAgentErr(tool, "coop login")
	}
	if profile == "" {
		// A bare `coop login claude` refreshes the profile your runs actually USE — the marked
		// default — not a profile literally named "default". Targeting the literal name both
		// re-authed the wrong slot (runs kept using the marked profile's expired token) and
		// kept re-creating a husk "default" dir the user had deleted.
		profile = a.cfg.DefaultProfileOf(tool)
	}
	// Validate the profile name (a static arg) before the environment checks below, so a traversal
	// name like "../../x" can't escape the vault and fails the same way piped or at a tty.
	if !validProfileName(profile) {
		return 2, &ui.UsageError{
			Headline: fmt.Sprintf("Invalid account name %q", agents.DisplayTarget(profile)),
			Cause:    "Use one name, without \":\", \"@\", \"/\", or \"\\\\\".\nThe name cannot be \".\", \"..\", or start with \"-\".",
			Rows:     [][2]string{{"Help:", "coop help login"}},
		}
	}
	// Login is interactive — it prompts for a paste code or API key (reading the tty directly). Refuse a
	// non-terminal stdin up front rather than blocking forever on a piped/redirected run.
	if !loginInputIsTerminal() {
		return 2, &ui.UsageError{
			Headline: `"coop login" needs an interactive terminal`,
			Cause:    "Run it directly in your terminal to complete sign-in.",
			Rows:     [][2]string{{"Help:", "coop help login"}},
		}
	}
	// Named credentials live under profiles/, so create that storage before activation.
	if profile != config.DefaultProfile {
		if err := box.EnsureProfilesDir(a.cfg, tool); err != nil {
			return -1, err
		}
	}
	a.cfg.SetActiveProfile(tool, profile)
	loginHandoff(tool, profile)
	hostCredential := ag.HostCredential()
	if hostCredential.Declared() {
		return a.loginWithHostCredential(ag, profile)
	}
	previousLogin := a.loginProvider
	a.loginProvider = tool
	defer func() { a.loginProvider = previousLogin }()
	code, err := a.runInBox(ag.Login(a.cfg), tool, nil) // mounts only the agent being logged in to
	switch {
	case errors.Is(err, context.Canceled):
		// The human stopped the provider's flow. Say only that: whether a token was written is
		// the provider's business, and claiming either way would be a guess.
		loginStopped()
		return code, err
	case err != nil || code != 0:
		return code, err // the box already reported how the command ended
	}
	// Exiting zero is not proof: the provider may have been dismissed without writing a usable
	// credential, and a green ✓ over that would send the user into a failing run.
	loginResult(tool, profile, box.ProfileCredentialReady(a.cfg, tool, profile, time.Now()))
	return 0, nil
}

func (a *app) loginWithHostCredential(ag agents.Agent, profile string) (int, error) {
	spec := ag.HostCredential()
	if !spec.Valid() {
		return -1, fmt.Errorf("%s has an invalid host credential declaration", ag.DisplayName())
	}
	if spec.Instructions != "" {
		ui.Note("%s\n", spec.Instructions)
	}
	secret, err := readLoginSecret(spec.Prompt)
	if err != nil {
		return -1, err
	}
	defer clear(secret)
	if err := box.SaveHostCredential(a.cfg, ag, profile, secret); err != nil {
		return -1, err
	}
	loginResult(ag.Name(), profile, box.ProfileCredentialReady(a.cfg, ag.Name(), profile, time.Now()))
	return 0, nil
}

// loginHandoff names the sign-in before host setup. Provider guidance belongs at launch.
func loginHandoff(tool, profile string) {
	ui.Note("Signing in to %s\n", loginTargetName(tool, profile))
}

func (a *app) loginStartingNotice(tool string) string {
	if a.loginProvider != tool || tool == "" {
		return ""
	}
	return fmt.Sprintf("Follow %s's sign-in instructions below", titleName(tool))
}

// loginResult reports the sign-in only when a usable credential was actually persisted; ready=false
// says exactly that much — the provider exited, and coop could not confirm it.
func loginResult(tool, profile string, ready bool) {
	title := titleName(tool)
	if !ready {
		warnRows(fmt.Sprintf("%s exited, but Coop could not confirm sign-in", title),
			[2]string{"Accounts:", "coop credentials " + tool})
		return
	}
	ui.Note("")
	ui.OK("Signed in to %s%s", title, loginAccountSuffix(tool, profile))
	ui.Note("\nStart %s:\n  %s", title, "coop "+loginTargetName(tool, profile))
}

// loginStopped is coop's whole say after an interrupted provider flow.
func loginStopped() { ui.Note("\nSign-in stopped.") }

// loginTargetName is the target a person would type for this account: the bare agent for its
// built-in default slot, agent@account for a named one.
func loginTargetName(tool, profile string) string {
	if profile == config.DefaultProfile {
		return tool
	}
	return tool + "@" + profile
}

// loginAccountSuffix names the account only when it is a named one — "Signed in to Claude" for the
// default slot, "Signed in to Claude as work" for a second subscription.
func loginAccountSuffix(tool, profile string) string {
	if profile == config.DefaultProfile {
		return ""
	}
	return " as " + profile
}

// cmdPrompt prints a compact, single-line status of this repo for embedding in a shell prompt, a
// tmux status bar, or a menubar: task-queue counts and fork/loop activity, "·"-separated,
// non-zero segments only — nothing when idle, so an embedding prompt stays clean. It is READ-ONLY
// and does only cheap local reads (the task dirs + fork pidfiles, plus one git-root lookup) — never
// a per-fork git shell-out and never docker — so it's safe to run on every prompt redraw. It takes
// no arguments and never errors out loud (a prompt must not spew): an unresolvable repo prints
// nothing.
func (a *app) cmdPrompt(args []string) (int, error) {
	if err := rejectArgs("prompt", args); err != nil {
		return 2, err
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return 0, nil // not in a resolvable repo → stay quiet
	}
	var c tasks.TaskCounts
	if queues, qerr := tasks.TaskQueues(a.cfg, repo, nil); qerr == nil {
		hosts := make([]string, len(queues))
		for i, q := range queues {
			hosts[i] = filepath.Join(repo, q)
		}
		c, _, _ = tasks.QueueProgress(hosts)
	}
	// Fork activity from a dir listing + pidfiles — no git, so it stays prompt-cheap.
	names, _ := forkspace.Names(repo) // prompt decoration is deliberately best-effort
	looping := 0
	for _, n := range names {
		if forkspace.RunningPid(repo, n) > 0 {
			looping++
		}
	}
	// One extra bounded git call, and only when you sign by default: is HEAD unsigned (a box commit
	// not yet signed)? A nudge to run `coop sign` before a protected remote rejects the push.
	signWarn := false
	if forkspace.WantsSigning() {
		signWarn = headUnsigned(repo)
	}
	if line := promptLine(c, len(names), looping, signWarn); line != "" {
		fmt.Println(line)
	}
	return 0, nil
}

// promptLine builds coop prompt's compact status line from the counts: non-zero segments only,
// "·"-separated, returning "" when everything is idle so an embedding prompt shows nothing.
func promptLine(c tasks.TaskCounts, forks, looping int, signWarn bool) string {
	var seg []string
	if c.Todo > 0 {
		seg = append(seg, fmt.Sprintf("%d todo", c.Todo))
	}
	if c.Doing > 0 {
		seg = append(seg, fmt.Sprintf("%d in progress", c.Doing))
	}
	if c.Blocked > 0 {
		seg = append(seg, fmt.Sprintf("%d blocked", c.Blocked))
	}
	// Running loops qualify the fork count rather than standing beside it: a loop runs IN a fork,
	// so two separate numbers read as two separate populations.
	if forks > 0 {
		row := ui.Count(forks, "fork")
		if looping > 0 {
			row += fmt.Sprintf(" (%d running)", looping)
		}
		seg = append(seg, row)
	}
	if signWarn {
		seg = append(seg, "unsigned commit")
	}
	return strings.Join(seg, " · ")
}
