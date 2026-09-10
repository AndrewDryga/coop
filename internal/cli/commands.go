package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	"github.com/AndrewDryga/coop/internal/scaffold"
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
	flags, rest, err := extractNetworkFlags(args)
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
	code, err := box.Run(a.cfg, a.rt, spec)
	// An interactive/run box makes unsigned commits; sign what THIS session produced on exit so a
	// protected remote accepts them. Best-effort, session-scoped, skipped for a dirty tree.
	a.signOnBoxExit(repo, pre, false)
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

func (a *app) cmdRun(args []string) (int, error) {
	args, err := a.takeNetworkFlags(args)
	if err != nil {
		return 2, err
	}
	if args, err = a.takeExposureFlags(args); err != nil {
		return 2, err
	}
	// Intercept the meta cases before entering the box. We can't lean on the dispatch's --help
	// handling here: it's `--`-blind, so it would mistake `coop run -- --help` (run --help in the
	// box) for a help request. Honor -- ourselves.
	if len(args) > 0 && args[0] == "--" {
		args = args[1:] // everything after -- runs verbatim
	} else if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		printCommandHelp(runHelp) // not forwarded to the box, where it would exec `--help` and crash
		return 0, nil
	}
	if len(args) == 0 {
		// `coop run` runs a raw command; it does not default to an agent (use `coop claude`).
		return 2, errors.New("usage: coop run -- <cmd...>")
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
		return 2, err
	}
	tool := t.Provider
	args, err = a.takeNetworkFlags(args)
	if err != nil {
		return 2, err
	}
	if args, err = a.takeExposureFlags(args); err != nil {
		return 2, err
	}
	peerVals, args, err := extractPeer(args)
	if err != nil {
		return 2, err
	}
	// `coop claude login` reads as "log in to claude" — route it to the sign-in flow like
	// `coop login claude`; the account rides the target (`coop claude@work login`).
	if len(args) >= 1 && args[0] == "login" {
		if a.mode.Restricted() {
			return 2, fmt.Errorf("'coop %s login' signs in on the host; it takes no --%s", tool, a.mode)
		}
		acct, aerr := singleAccount(t)
		if aerr != nil {
			return 2, aerr
		}
		if len(args) > 1 {
			return 2, fmt.Errorf("unexpected argument %q after 'coop %s login'", args[1], tool)
		}
		return a.loginTo(tool, acct)
	}
	if err := a.applyRunTarget(t); err != nil {
		return 2, err
	}
	a.nudgeIfUnauthed(tool)
	peers, err := a.resolvePeers("--peer", peerVals)
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
	if args, err = a.takeExposureFlags(args); err != nil {
		return 2, err
	}
	if a.mode.Restricted() {
		// A preset's roles run from inside the box, which a restricted run gives no credentials
		// or wrappers to. The lead alone can run restricted, as a plain target.
		return 2, fmt.Errorf("a preset runs its roles from the box, which a %s run has none of — run its lead directly: coop %s --%s", a.mode, tool, a.mode)
	}
	peerVals, args, err := extractPeer(args)
	if err != nil {
		return 2, err
	}
	if err := a.applyPinnedPreset(p, tool); err != nil {
		return 2, err
	}
	a.nudgeIfUnauthed(tool)
	peers, err := a.resolvePeers("--peer", peerVals)
	if err != nil {
		return 2, err
	}
	providerArgs := dropDashDash(args)
	return a.runAgentCommandInBox(append(append([]string{}, a.defaultCmd(tool)...), providerArgs...), tool, peers, providerArgs)
}

// nudgeIfUnauthed prints one heads-up (TTY only, never blocks) when the credential this run will use
// isn't signed in — so a first `coop claude` names the fix instead of failing opaquely inside the box.
func (a *app) nudgeIfUnauthed(tool string) {
	if !ui.IsTerminal(os.Stdin) {
		return
	}
	if !box.ProfileAuthed(a.cfg, tool, a.cfg.ActiveProfile(tool)) {
		ui.Info("%s isn't signed in — run 'coop login %s' (first run: coop build → coop login → coop doctor)", tool, tool)
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
		return fmt.Errorf("%s has no account %q — sign in first: coop login %s@%s", tool, profile, tool, profile)
	}
	if !box.ProfileAuthed(a.cfg, tool, profile) {
		ui.Info("note: %s account %q isn't signed in — run: coop login %s@%s", tool, profile, tool, profile)
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
// peer the lead may consult read-only on hard calls (see box.RunSpec.Peers). A valueless occurrence errors with
// the repeatable form. `--`-aware. The one --peer parser for every command (the retired --consult
// spelling is now just an unknown flag).
func extractPeer(args []string) (peers, rest []string, err error) {
	return extractRepeatable(args, "--peer", "name each peer: --peer <target> [--peer <target> ...]")
}

// extractRepeatable collects every `--flag <value>` occurrence (repeatable) out of args, in
// order, returning the values and the remaining args. A valueless occurrence (a typo, or a bare
// flag) errors, pointing at the repeatable form. Stops at `--` — everything after is the agent's
// own, forwarded verbatim (so an agent's OWN --peer still reaches it).
func extractRepeatable(args []string, flag, hint string) (vals, rest []string, err error) {
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			return vals, append(rest, args[i:]...), nil
		}
		if args[i] == flag || strings.HasPrefix(args[i], flag+"=") {
			v, n, _, e := flagValue(args, i, flag)
			if e != nil {
				return nil, nil, fmt.Errorf("%s takes a value — %s", flag, hint)
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

func (a *app) cmdLogin(args []string) (int, error) {
	// The account rides the target (coop login claude@work).
	// The agent is required — bare `coop login` must not silently default to one (it would open a
	// browser and block); name it explicitly, like the help shows. A stray extra arg is a typo,
	// not a second target, so reject it rather than silently ignore.
	if len(args) == 0 {
		return 2, fmt.Errorf("usage: coop login <%s>[@<account>]", strings.Join(agents.Names(), "|"))
	}
	if len(args) > 1 {
		return 2, fmt.Errorf("unexpected argument %q (usage: coop login <%s>[@<account>])", args[1], strings.Join(agents.Names(), "|"))
	}
	t, err := agents.ParseTarget(args[0])
	if err != nil {
		return 2, err
	}
	// login authenticates an account; a :model in the target has no meaning here.
	if t.Model != "" {
		return 2, fmt.Errorf("coop login takes no model — run: coop login %s@<account>", t.Provider)
	}
	acct, err := singleAccount(t)
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

// loginTo runs an agent's sign-in flow in the box; its token persists in the agent's
// config dir for the chosen credential. Shared by `coop login <provider>[@<account>]` and
// `coop <provider>[@<account>] login`.
func (a *app) loginTo(tool, profile string) (int, error) {
	ag, ok := agents.Get(tool)
	if !ok {
		return 2, unknownErr("agent", tool, agents.Names())
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
		return 2, fmt.Errorf("invalid credential name %q — use a single segment (no '/', '..', or leading '-')", profile)
	}
	// Login is interactive — it prompts for a paste code (reading the tty directly). Refuse a
	// non-terminal stdin up front rather than blocking forever on a piped/redirected run.
	if !ui.IsTerminal(os.Stdin) {
		return 2, errors.New("login needs an interactive terminal (it prompts for a paste code) — run it directly")
	}
	// Named credentials live under profiles/, so create that storage before activation.
	if profile != config.DefaultProfile {
		if err := box.EnsureProfilesDir(a.cfg, tool); err != nil {
			return -1, err
		}
	}
	a.cfg.SetActiveProfile(tool, profile)
	where := ""
	if profile != config.DefaultProfile {
		where = fmt.Sprintf(" (credential %s)", profile)
	}
	ui.Info("logging in to %s%s — credentials persist in %s/", tool, where, a.cfg.AgentDir(tool))
	return a.runInBox(ag.Login(a.cfg), tool, nil) // mounts only the agent being logged in to
}

func (a *app) cmdBuild(args []string) (int, error) {
	if err := rejectArgs("build", args); err != nil {
		return 2, err
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	if err := box.Build(a.rt, a.cfg, repo, false, resolveVersion()); err != nil {
		return -1, err
	}
	if err := a.recycleBoxes(repo); err != nil {
		return -1, fmt.Errorf("box image built, but old supervised boxes could not be recycled: %w — fix the container runtime and run 'coop build' again", err)
	}
	return 0, nil
}

// recycleBoxes restarts supervised boxes after a rebuild so they reconnect on the new
// image — a coop acp supervisor replays the ACP handshake, so the editor doesn't
// notice. New runs use the fresh image anyway (containers are anonymous). Other
// running boxes (loops, forks, an un-supervised session) are left alone; SIGKILLing
// them would lose work, and they pick up the new image when they next start.
// It reaps repo's orphans first, so a box nobody supervises is never counted (or reported to the
// user) as work still running on the old image. Every authoritative query and removal shares one
// deadline; the preceding image build has already succeeded if an error is returned.
const recycleBoxesTimeout = 10 * time.Second

func (a *app) recycleBoxes(repo string) error {
	a.sweepOrphanBoxes(repo)
	ctx, cancel := context.WithTimeout(context.Background(), recycleBoxesTimeout)
	defer cancel()
	running, err := a.rt.RunningContainerIDsByLabel(ctx, box.LabelKey, box.LabelBox)
	if err != nil {
		return fmt.Errorf("list running Coop boxes: %w", err)
	}
	supervised, err := a.rt.RunningContainerIDsByLabel(ctx, box.LabelSupervised, box.LabelOn)
	if err != nil {
		return fmt.Errorf("list supervised Coop boxes: %w", err)
	}
	n, err := a.rt.RemoveByLabel(ctx, box.LabelSupervised, box.LabelOn)
	if err != nil {
		return fmt.Errorf("remove supervised Coop boxes (%d removed before failure): %w", n, err)
	}
	if n > 0 {
		ui.Info("restarted %s onto the new image", ui.Count(n, "supervised session"))
	}
	if others := len(running) - len(supervised); others > 0 {
		ui.Info("%s still on the old image until restarted", ui.Count(others, "other running container"))
	}
	return nil
}

// cmdUpdate self-updates the coop binary to the latest release, then force-rebuilds
// the box image (--pull --no-cache) so the base image and the npm-installed agent CLIs
// + ACP adapters refresh to their latest, then reports the versions it landed on.
// --self-only does just the binary; --box-only does just the image (the old behavior).
func (a *app) cmdUpdate(args []string) (int, error) {
	selfOnly, boxOnly, check, err := parseUpdateFlags(args)
	if err != nil {
		return 2, err
	}
	if check {
		return a.cmdUpdateCheck()
	}

	// Self-update the binary first. A failed *check* (offline/rate limit) is soft and
	// must not block the box rebuild; a write or install failure is loud and exits
	// non-zero, but the box still rebuilds (it's independent) so the run isn't wasted.
	selfFailed := false
	if !boxOnly {
		if _, err := selfUpdate(os.Stdout); err != nil {
			var ce checkError
			switch {
			case selfOnly:
				return -1, err
			case errors.As(err, &ce):
				ui.Info("coop self-update: couldn't check for a newer release (%v) — continuing with the box", err)
			default:
				ui.Error("coop self-update failed: %v", err)
				selfFailed = true
			}
		}
		if selfOnly {
			return 0, nil
		}
	}

	// The box rebuild needs the runtime; --self-only returned above, so detect only here (not eagerly
	// in dispatch), keeping `coop update --self-only` usable on a box with no container runtime.
	repo, _, err := loadProject(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	if err := a.ensureRuntime(); err != nil {
		return -1, err
	}
	ui.Info("updating the box: newer base image + latest agent CLIs and ACP adapters")
	if err := box.Build(a.rt, a.cfg, repo, true, resolveVersion()); err != nil {
		return -1, err
	}
	if err := a.recycleBoxes(repo); err != nil {
		return -1, fmt.Errorf("box image updated, but old supervised boxes could not be recycled: %w — fix the container runtime and run 'coop update --box-only' again", err)
	}
	img := box.ImageForRepo(repo, a.cfg.BaseImage, a.cfg.ImageOverride)
	ui.Info("installed versions:")
	_, _ = box.Run(a.cfg, a.rt, box.RunSpec{
		Image: img, Repo: repo, Batch: true, Quiet: true,
		Cmd:       []string{"sh", "-c", "npm ls -g --depth=0 2>/dev/null | grep -iE '" + strings.Join(append(agents.Names(), "acp"), "|") + "' || true"},
		ExtraArgs: []string{"-e", "COOP_NO_ASDF=1"}, // skip the .tool-versions provision for a quick version print
	})
	if selfFailed {
		return 1, nil // box updated, binary didn't — signal the partial failure
	}
	return 0, nil
}

// parseUpdateFlags parses `coop update`'s own flags: --self-only (just the binary),
// --box-only (just the image), and --check (report, change nothing) — mutually exclusive.
func parseUpdateFlags(args []string) (selfOnly, boxOnly, check bool, err error) {
	for _, x := range args {
		switch x {
		case "--self-only":
			selfOnly = true
		case "--box-only":
			boxOnly = true
		case "--check":
			check = true
		default:
			return false, false, false, fmt.Errorf("update: unknown flag %q (usage: coop update [--self-only|--box-only|--check])", x)
		}
	}
	picked := 0
	for _, on := range []bool{selfOnly, boxOnly, check} {
		if on {
			picked++
		}
	}
	if picked > 1 {
		return false, false, false, errors.New("update: --self-only, --box-only, and --check are mutually exclusive")
	}
	return selfOnly, boxOnly, check, nil
}

// cmdUpdateCheck is `coop update --check`: report what an update WOULD do, changing
// nothing. The binary line needs one GitHub call; the box report reads only the local
// build stamps (no container runtime), so the dry-run works anywhere.
func (a *app) cmdUpdateCheck() (int, error) {
	cur := resolveVersion()
	latest, err := latestReleaseTag()
	if err != nil {
		return -1, err // latestReleaseTag's message already says what to do
	}
	c, l := normalizeVersion(cur), normalizeVersion(latest)
	switch relation := compareReleaseVersions(cur, latest); relation {
	case releaseInvalid:
		if releaseVersion(latest) {
			ui.Note("coop %s is a dev/source build (self-update doesn't apply); the latest release is v%s", cur, l)
			break
		}
		return -1, fmt.Errorf("GitHub returned an invalid latest release tag %q", latest)
	case releaseBehind:
		ui.Note("coop v%s → v%s available — run 'coop update'", c, l)
	case releaseEqual:
		ui.OK("coop v%s is up to date", c)
	case releaseAhead:
		ui.OK("coop v%s is newer than GitHub's latest release v%s; leaving it unchanged", c, l)
	default:
		ui.Note("coop %s is a dev/source build (self-update doesn't apply); the latest release is v%s", cur, l)
	}

	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		ui.Note("(not in a repo — skipped the box image check)")
		return 0, nil
	}
	img := box.ImageForRepo(repo, a.cfg.BaseImage, a.cfg.ImageOverride)
	at, built := box.ImageBuildAge(a.cfg, img)
	if !built {
		// No build stamp means this coop never built the image here; "current" would describe an
		// image `coop run` is about to refuse. The runtime is deliberately not consulted so the
		// dry-run keeps working without one.
		ui.Note("box image %s has no build record here — run 'coop build'", img)
		return 0, nil
	}
	when := "today"
	if days := int(time.Since(at).Hours() / 24); days > 0 {
		when = ui.Count(days, "day") + " ago"
	}
	ui.Note("box image %s: built %s", img, when)
	nudges := box.StalenessNudges(a.cfg, repo, img)
	for _, n := range nudges {
		ui.Note("%s", n)
	}
	if len(nudges) == 0 {
		ui.OK("box image %s is current", img)
	}
	return 0, nil
}

func (a *app) cmdUp(args []string) (int, error) {
	if err := rejectArgs("up", args); err != nil {
		return 2, err
	}
	repo, p, err := loadProject(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	if err := a.rt.EnsureDaemon(); err != nil {
		return -1, err
	}
	if a.rt.Name == "container" {
		return -1, errors.New("the Apple 'container' runtime has no compose yet — use Docker or Podman for services")
	}
	file := box.ComposeFileAt(repo, p.ComposeRel())
	if file == "" {
		return -1, fmt.Errorf("no %s — run 'coop init --services postgres,redis' to scaffold one", p.ComposeRel())
	}
	// Sidecars start only when no agent box is running in this project: a running agent could
	// swap a validated bind source for a link to a host path between coop's check and Docker
	// opening it, and this start is exactly the launch it would race.
	live, err := box.LiveBoxes(repo, "")
	if err != nil {
		return -1, err
	}
	if len(live) > 0 {
		return 1, fmt.Errorf("an agent box is running in this project (%s) — sidecars start only when none is; stop it or wait for it to finish, then retry: coop up", box.DescribeLiveBoxes(live))
	}
	proj := box.ComposeProject(repo)
	rel, _ := filepath.Rel(repo, file)
	// A service that legitimately needs a secret-looking file (a generated dev TLS key) gets it
	// only after a human at a terminal approves this exact compose content; a script or an agent
	// cannot answer for them, and the auto-start on box launch never asks — it warns and uses
	// decoys until someone runs this command and says yes.
	review, err := box.ReviewServiceSecrets(repo, file)
	if err != nil {
		return -1, fmt.Errorf("could not start services from %s: %w — fix the Compose file, then retry: coop up", rel, err)
	}
	if review != nil && ui.IsTerminal(os.Stdin) {
		ui.Warn("%s binds %s that look like secrets into its services:", rel, ui.Count(len(review.Files), "file"))
		for _, file := range review.Files {
			ui.Detail("%s — %s", file.Path, file.Reason())
		}
		ui.Detail("services get an empty file for each unless you approve; the approval lasts until %s changes", rel)
		if ui.Confirm("let the services read these files", false) {
			if err := review.Approve(); err != nil {
				return -1, fmt.Errorf("record the approval: %w", err)
			}
			ui.Info("approved — services read them until %s changes", rel)
		}
	}
	ui.Info("starting services from %s (waiting until healthy)", rel)
	services, err := box.EnsureServicesFile(a.rt, repo, file, os.Stdout, os.Stderr, box.ConfigExposureRoots(a.cfg)...)
	if err != nil {
		return -1, fmt.Errorf("could not start services from %s: %w — fix the Compose file or runtime, then retry: coop up", rel, err)
	}
	ui.Info("up on network %s_default — the box reaches %s by name", proj, strings.Join(services, ", "))
	return 0, nil
}

func (a *app) cmdDown(args []string) (int, error) {
	// Validate flags before any runtime/compose work, so a typo fails clearly here instead of
	// later as an unrelated "no .agent/compose.yml" — `coop down` takes only -v/--volumes.
	volumes := false
	for _, x := range args {
		if x != "-v" && x != "--volumes" {
			return 2, fmt.Errorf("unknown flag %q — coop down takes only -v/--volumes", x)
		}
		volumes = true
	}
	repo, p, err := loadProject(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	if err := a.rt.EnsureDaemon(); err != nil {
		return -1, err
	}
	file := box.ComposeFileAt(repo, p.ComposeRel())
	if file == "" {
		return -1, fmt.Errorf("no %s here — nothing to bring down", p.ComposeRel())
	}
	if err := box.DownServicesFile(a.rt, repo, file, volumes, os.Stdout, os.Stderr, box.ConfigExposureRoots(a.cfg)...); err != nil {
		return -1, err
	}
	return 0, nil
}

// scaffoldableAgents are the agents with a per-agent dir `coop init` can scaffold (grok reads the
// root AGENTS.md, no dir of its own).
var scaffoldableAgents = []string{"claude", "codex", "gemini"}

// parseExplicitList normalizes and de-duplicates a comma/space-separated closed list. The optional
// sentinel must stand alone and maps to sentinelValues ("none" → nil; "all" → every agent).
func parseExplicitList(flag, value string, valid []string, sentinel string, sentinelValues []string) ([]string, error) {
	tokens := strings.FieldsFunc(strings.ToLower(value), func(r rune) bool { return r == ',' || r == ' ' })
	choices := strings.Join(append(slices.Clone(valid), sentinel), ", ")
	if slices.Contains(tokens, sentinel) {
		if len(tokens) != 1 {
			return nil, fmt.Errorf("%s: %q must be used alone — choose from %s", flag, sentinel, choices)
		}
		return slices.Clone(sentinelValues), nil
	}
	var out []string
	for _, token := range tokens {
		if !slices.Contains(valid, token) {
			return nil, fmt.Errorf("%s: unknown value %q — choose from %s", flag, token, choices)
		}
		if !slices.Contains(out, token) {
			out = append(out, token)
		}
	}
	return out, nil
}

// scaffoldAgentSet resolves the implicit per-agent dirs from signed-in agents. Empty means
// .agent/ only; a box synthesizes a missing agent's skills from the shared source on demand.
func scaffoldAgentSet(cfg *config.Config) []string {
	var out []string
	for _, name := range box.AuthedAgents(cfg) {
		if slices.Contains(scaffoldableAgents, name) && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

func (a *app) cmdInit(args []string) (int, error) {
	stack := ""
	var services []string
	servicesSet := false
	var agentDirs []string
	agentsSet := false
	for i := 0; i < len(args); i++ {
		if v, n, ok, e := flagValue(args, i, "--stack"); ok {
			if e != nil {
				return 2, e
			}
			stack = v
			i += n - 1
			continue
		}
		if v, n, ok, e := flagValue(args, i, "--services"); ok {
			if e != nil {
				return 2, e
			}
			services, e = parseExplicitList("--services", v, scaffold.ComposeServices, "none", nil)
			if e != nil {
				return 2, e
			}
			servicesSet = true
			i += n - 1
			continue
		}
		if v, n, ok, e := flagValue(args, i, "--agents"); ok {
			if e != nil {
				return 2, e
			}
			agentDirs, e = parseExplicitList("--agents", v, scaffoldableAgents, "all", scaffoldableAgents)
			if e != nil {
				return 2, e
			}
			agentsSet = true
			i += n - 1
			continue
		}
		// An unknown token is a typo — error before doing any scaffold work, rather than
		// silently ignoring it and acting as if a flag were never passed.
		return 2, unknownErr("init flag", args[i], []string{"--stack", "--services", "--agents"})
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	// A re-init asks nothing. Every scaffold write is no-clobber, so on an already-initialized repo
	// an answer to either prompt below could not take effect — and `coop init` is run from inside a
	// repo often enough (after a coop upgrade, or just from a subdirectory) that interrogating the
	// user each time is pure friction. --services / --stack still work explicitly.
	already := scaffold.Initialized(repo)
	// Detect the repo's stack(s) for the commit gate; if nothing's detected and we're at a
	// terminal, ask rather than guess — coop never imposes a check the repo doesn't use.
	langs := scaffold.DetectStacks(repo)
	if len(langs) == 0 && !already && ui.IsTerminal(os.Stdin) {
		langs = promptGateLangs(os.Stdin)
	}
	// Sibling services (db/redis) are opt-in — coop doesn't add a compose file a project may
	// not want. Ask at a terminal unless --services already said.
	if !servicesSet && !already && ui.IsTerminal(os.Stdin) {
		services = promptServices(os.Stdin)
	}
	// Without an explicit --agents list, scaffold dirs for the signed-in agents. Others aren't
	// clutter you delete later — a box synthesizes a missing agent's skills from the repo's shared
	// source on demand.
	if !agentsSet {
		agentDirs = scaffoldAgentSet(a.cfg)
	}
	if err := scaffold.Init(repo, stack, langs, agentDirs); err != nil {
		return 0, err
	}
	if err := scaffold.WriteCompose(repo, services); err != nil {
		return 0, err
	}
	if err := a.writeMCPStub(); err != nil {
		return 0, err
	}
	// Monorepo: detect member dirs (each with a .agent/), record them in the root .agent/project.yaml
	// so coop aggregates their task queues, and give each member a project.yaml if it lacks one. A
	// single repo still gets a project.yaml template. Never clobbers an existing file.
	subs := scaffold.DetectSubprojects(repo)
	if _, err := scaffold.WriteProject(repo, subs); err != nil {
		return 0, err
	}
	for _, s := range subs {
		// Members get only the minimal set — their own task queue (plus a backlog drawer on demand)
		// — since they share the root's AGENTS.md, skills, rules, hooks, box, and its single
		// top-level project.yaml. A member never gets a project.yaml of its own.
		if err := scaffold.InitSubproject(repo, filepath.Join(repo, filepath.FromSlash(s))); err != nil {
			return 0, err
		}
	}
	if len(subs) > 0 {
		// A re-init keeps an existing project.yaml, so newly-added members were previously only
		// REPORTED and you had to list them by hand. Register them instead — an unlisted member is
		// a queue coop silently ignores, which is never what you wanted when you created it.
		added, err := scaffold.RegisterSubprojects(repo, subs)
		if err != nil {
			return 0, err
		}
		if len(added) > 0 {
			ui.Detail("registered in subprojects: %s", strings.Join(added, ", "))
		}
		ui.Detail("monorepo: %d member(s) (%s) — .agent/project.yaml aggregates their task queues", len(subs), strings.Join(subs, ", "))
		// Only if the edit couldn't be placed (a hand-restructured project.yaml) does the advisory
		// remain — coop never silently drops a member on the floor.
		if pj, err := project.Load(repo); err == nil {
			var missing []string
			for _, s := range subs {
				if !slices.Contains(pj.Subprojects, s) {
					missing = append(missing, s)
				}
			}
			if len(missing) > 0 {
				ui.Warn("add these to 'subprojects:' in .agent/project.yaml: %s", strings.Join(missing, ", "))
			}
		}
	}
	if len(agentDirs) > 0 {
		ui.Detail("per-agent dirs: %s — missing artifacts synthesize in-box from shared sources on demand", strings.Join(agentDirs, ", "))
	} else {
		ui.Detail("no agents signed in — scaffolded .agent/ only; sign in and run, or `coop init --agents claude,codex`")
	}
	// One "coop:" anchor closes the dim per-file log; then the optional Docker-box guidance
	// (only when the repo has its own Docker and no .agent/Dockerfile yet); then the actions you
	// need to take next stand on their own — derived from what actually landed, not a fixed script.
	if already {
		// Name the repo explicitly: `coop init` scaffolds the git ROOT, so run from a subdirectory
		// it acts somewhere other than where you're standing, and saying so is the whole message.
		ui.Info("already initialized at %s — anything missing above was added", repo)
	} else {
		ui.Info("scaffolded into %s", repo)
	}
	scaffold.SuggestDocker(repo)
	// "review .agent/Dockerfile, then `coop build`" is first-run advice. On a repo that has been
	// building for weeks it's noise at best and misleading at worst.
	if !already {
		ui.Steps(initNextSteps(repo, services)...)
	}
	return 0, nil
}

// initNextSteps is the short list of actions to run after scaffolding, built from what landed: a
// build step when there's a .agent/Dockerfile, a `coop up` when sibling services were added, and
// always the edit-then-loop step. Assembled here (not in scaffold) so the whole list is shown in
// one block.
func initNextSteps(repo string, services []string) []string {
	var steps []string
	// coop runs forks and the loop on top of git (worktrees, rebase-merge); a repo that
	// isn't initialized yet needs that first, so lead with it.
	if !pathExists(filepath.Join(repo, ".git")) {
		steps = append(steps, "`git init`  (coop's forks and loop need a git repo)")
	}
	if dfRel := project.DockerfilePath(repo); fileExists(filepath.Join(repo, dfRel)) {
		steps = append(steps, fmt.Sprintf("review %s, then `coop build`", dfRel))
	}
	if len(services) > 0 {
		steps = append(steps, fmt.Sprintf("`coop up`  (starts %s for the box)", strings.Join(services, " + ")))
	}
	steps = append(steps, "`coop tasks add \"<title>\"`, then `coop loop`")
	return steps
}

// writeMCPStub seeds an empty shared mcp.json — coop's one MCP source of truth, translated to
// each agent — at the global config path if absent, so there's an obvious, correctly-shaped file
// to drop servers into. The validated snapshot boundary treats an empty (no-server) file as inert,
// so the stub changes no run until you add a server. Never clobbers an existing config.
func (a *app) writeMCPStub() error {
	path := a.cfg.MCPFile
	if path == "" {
		return nil
	}
	if fileExists(path) {
		// mcp.json is the GLOBAL shared MCP config, not part of this repo's scaffold — when it already
		// exists `coop init` changed nothing, so say nothing. (A "kept existing mcp.json" line during a
		// fresh repo's init reads as if it were a repo file; the e2e review flagged it as misleading.)
		// The "wrote" line below still fires the one time init actually seeds it.
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte("{\n  \"mcpServers\": {}\n}\n"), 0o600); err != nil {
		return err
	}
	ui.Detail("wrote %s — add MCP servers under \"mcpServers\" to share them with every agent", path)
	return nil
}

// promptServices asks (on a tty) which sibling services to scaffold into .agent/compose.yml.
// Blank → none (coop adds no db/redis you didn't ask for); unknown tokens are ignored.
func promptServices(in io.Reader) []string {
	fmt.Fprintf(os.Stderr, "add sibling services for the box? [%s] (space-separated, blank for none): ",
		strings.Join(scaffold.ComposeServices, " "))
	sc := bufio.NewScanner(in)
	if !sc.Scan() {
		return nil
	}
	var chosen []string
	for _, tok := range strings.Fields(strings.ToLower(sc.Text())) {
		if slices.Contains(scaffold.ComposeServices, tok) && !slices.Contains(chosen, tok) {
			chosen = append(chosen, tok)
		}
	}
	return chosen
}

// promptGateLangs asks (on a tty) which commit format gate(s) to scaffold when coop couldn't
// detect a stack. Blank → none; unknown tokens are ignored. Reads one line from in.
func promptGateLangs(in io.Reader) []string {
	fmt.Fprintf(os.Stderr, "no stack detected — add a commit format gate? [%s] (space-separated, blank for none): ",
		strings.Join(scaffold.GateLangs, " "))
	sc := bufio.NewScanner(in)
	if !sc.Scan() {
		return nil
	}
	var chosen []string
	for _, tok := range strings.Fields(strings.ToLower(sc.Text())) {
		if slices.Contains(scaffold.GateLangs, tok) && !slices.Contains(chosen, tok) {
			chosen = append(chosen, tok)
		}
	}
	return chosen
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
		seg = append(seg, fmt.Sprintf("%d doing", c.Doing))
	}
	if c.Blocked > 0 {
		seg = append(seg, fmt.Sprintf("%d blocked", c.Blocked))
	}
	if looping > 0 {
		seg = append(seg, fmt.Sprintf("%d looping", looping))
	}
	if forks > 0 {
		word := "forks"
		if forks == 1 {
			word = "fork"
		}
		seg = append(seg, fmt.Sprintf("%d %s", forks, word))
	}
	if signWarn {
		seg = append(seg, "unsigned")
	}
	return strings.Join(seg, " · ")
}
