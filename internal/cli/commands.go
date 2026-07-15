package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/acpproxy"
	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/fusion"
	"github.com/AndrewDryga/coop/internal/loopcfg"
	"github.com/AndrewDryga/coop/internal/preset"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/scaffold"
	"github.com/AndrewDryga/coop/internal/ui"
)

// resolveImage resolves the repo and its image, verifying the image is built.
func (a *app) resolveImage() (repo, img string, err error) {
	if err := a.ensureRuntime(); err != nil { // the choke point for box commands not eager-detected in dispatch (fork/fleet)
		return "", "", err
	}
	repo, err = box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return "", "", err
	}
	img = box.ImageForRepo(repo, a.cfg.BaseImage, a.cfg.ImageOverride)
	if !box.ImageExists(a.rt, img) {
		return "", "", fmt.Errorf("image %q not built — run 'coop build'", img)
	}
	return repo, img, nil
}

// runInBox runs a command in the box against the current repo with the default
// homes/network/cache toggles (the common interactive path). agent names the agent
// being driven (claude/codex/gemini) so its credentials are mounted and, with named peers,
// it gets the second-opinion directive plus exactly those peers' credentials. Pass
// "" for raw commands (coop run/shell) that aren't an agent session — they mount no
// agent credentials.
func (a *app) runInBox(cmd []string, agent string, peers []agents.Target) (int, error) {
	repo, img, err := a.resolveImage()
	if err != nil {
		return -1, err
	}
	lead := ""
	if len(peers) > 0 || (a.preset != nil && agent != "") {
		lead = agent // a preset makes the agent a lead too: its routing contract mounts via ConsultLead
	}
	pre := gitOut(repo, "rev-parse", "HEAD")
	code, err := box.Run(a.cfg, a.rt, box.RunSpec{
		Image: img, Repo: repo, Cmd: cmd, Agent: agent, ConsultLead: lead, Peers: peers, Preset: a.preset,
		Homes: a.cfg.Homes, Network: a.cfg.Network, Cache: a.cfg.Cache, Serve: true,
	})
	// An interactive/run box makes unsigned commits; sign what THIS session produced on exit so a
	// protected remote accepts them. Best-effort, session-scoped, skipped for a dirty tree.
	a.signOnBoxExit(repo, pre, false)
	return code, err
}

func (a *app) cmdRun(args []string) (int, error) {
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
	// The head is a target: provider[:model][@account]. The model/account ride it —
	// --model/--credential are retired.
	t, err := agents.ParseTarget(target)
	if err != nil {
		return 2, err
	}
	tool := t.Provider
	peerVals, args, err := extractPeer(args)
	if err != nil {
		return 2, err
	}
	// `coop claude login` reads as "log in to claude" — route it to the sign-in flow like
	// `coop login claude`; the account rides the target (`coop claude@work login`).
	if len(args) >= 1 && args[0] == "login" {
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
	return a.runInBox(append(append([]string{}, a.defaultCmd(tool)...), dropDashDash(args)...), tool, peers)
}

// launchPreset runs an orchestration preset interactively (`coop <preset>`): its lead agent
// leads the session, its roles seed the run (routing contract, role models/credentials,
// wrappers). The who-runs positional named the preset, so there's no target to fold in — the
// lead ladder's first entry supplies the lead's model/account (applyPreset). --peer still adds
// ad-hoc read-only peers on top of the preset's own consult roles.
func (a *app) launchPreset(p *preset.Preset, args []string) (int, error) {
	tool := p.LeadAgent
	peerVals, args, err := extractPeer(args)
	if err != nil {
		return 2, err
	}
	a.applyPreset(p, tool)
	a.nudgeIfUnauthed(tool)
	peers, err := a.resolvePeers("--peer", peerVals)
	if err != nil {
		return 2, err
	}
	return a.runInBox(append(append([]string{}, a.defaultCmd(tool)...), dropDashDash(args)...), tool, peers)
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
// run of tool (a no-op when profile is ""). It requires the profile to already exist — a typo
// otherwise silently creates an empty husk dir (box.Run pre-creates the active profile), the very
// clutter `coop credentials rm` cleans up — and notes (without blocking) one that isn't signed in.
// Shared by every agent-launch path: launchAgent, cmdFusion, cmdACP.
func (a *app) selectRunProfile(tool, profile string) error {
	if profile == "" {
		return nil
	}
	if !slices.Contains(a.cfg.Profiles(tool), profile) {
		return fmt.Errorf("%s has no account %q — sign in first: coop login %s@%s", tool, profile, tool, profile)
	}
	if !box.ProfileAuthed(a.cfg, tool, profile) {
		ui.Info("note: %s account %q isn't signed in — run: coop login %s@%s", tool, profile, tool, profile)
	}
	a.cfg.SetActiveProfile(tool, profile)
	return nil
}

// selectRunModel points cfg at the model chosen with --model for a run of tool (a no-op when
// model is ""). Deliberately unvalidated: model ids churn faster than coop releases, so the
// agent CLI stays the source of truth — a bad id fails loudly in the agent's own error.
// Shared by every agent-launch path: launchAgent, cmdFusion, cmdACP, and the fork paths.
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
	ladder, err := oneOffLadder(model, credential)
	if err != nil {
		return err
	}
	if ladder == nil {
		return nil
	}
	t := ladder[0]
	if err := a.selectRunProfile(tool, t.Account()); err != nil {
		return err
	}
	a.selectRunModel(tool, t.Model)
	return nil
}

// extractPeer pulls every --peer <target> (repeatable) out of a run's args — each value is one
// peer the lead may consult read-only on hard calls (fusion's whole council; the opt-in second
// opinion on every other surface — see box.RunSpec.Peers). A valueless occurrence errors with
// the repeatable form. `--`-aware. The one --peer parser for every command (the retired --consult
// spelling is now just an unknown flag).
func extractPeer(args []string) (peers, rest []string, err error) {
	return extractRepeatable(args, "--peer", "name each peer: --peer <agent> [--peer <agent> …]")
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
	// The account rides the target now (coop login claude@work); --credential is retired.
	// The agent is required — bare `coop login` must not silently default to one (it would open a
	// browser and block); name it explicitly, like the help shows. A stray extra arg is a typo,
	// not a second target, so reject it rather than silently ignore.
	if len(args) == 0 {
		return 2, fmt.Errorf("usage: coop login <%s>[@account]", strings.Join(agents.Names(), "|"))
	}
	if len(args) > 1 {
		return 2, fmt.Errorf("unexpected argument %q (usage: coop login <%s>[@account])", args[1], strings.Join(agents.Names(), "|"))
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

// validProfileName keeps a credential profile name to a single safe path segment, so a name passed
// to --credential can't traverse or collide outside the agent's profiles/ vault (no '/', '\', '..',
// '.', empty, or leading '-'). Login is the path that CREATES the dir from the name, so it's the
// gate; runs/select/rm/default already require an existing profile.
func validProfileName(name string) bool {
	if name == "" || name == "." || name == ".." || strings.HasPrefix(name, "-") {
		return false
	}
	return !strings.ContainsAny(name, "/\\")
}

// loginTo runs an agent's sign-in flow in the box; its token persists in the agent's
// config dir for the chosen credential. Shared by `coop login <provider>[@account]` and
// `coop <agent> login [--credential <name>]`.
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
	// A named profile needs the profiles/ layout; EnsureProfilesDir also migrates a
	// pre-existing flat login into profiles/default the first time, so it isn't orphaned.
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

// acpCommand maps an agent to its ACP adapter command inside the box.
func acpCommand(cfg *config.Config, tool string) ([]string, bool) {
	if ag, ok := agents.Get(tool); ok {
		return ag.ACP(cfg), true
	}
	return nil, false
}

// cmdACP runs the box as an ACP agent over stdio: the repo mounts at its real
// host path (so the editor's absolute paths resolve, and the session history
// matches `coop`/`coop loop` — see resolveWorkdir) and no tty is allocated. The
// explicit Workdir forces the real path even if COOP_WORKDIR is set.
//
// `coop acp fusion [governor]` fronts the governor's adapter as a normal ACP
// agent (so Zed drives it like any other) but wired for fusion: it consults its
// peers read-only and synthesizes (see cmdFusion). Add one Zed agent_servers
// entry per governor to switch which model leads.
// defaultACPProvider picks the provider for a bare `coop acp` (no positional target, no preset
// lead): the first signed-in agent, or "" when none is signed in. ACP-only — the editor toolbar's
// provider dropdown can switch it live, so an implicit default is safe here where `coop claude`/
// `coop loop` are deliberately strict (no dropdown to correct a wrong guess).
func defaultACPProvider(cfg *config.Config) string {
	if authed := box.AuthedAgents(cfg); len(authed) > 0 {
		return authed[0]
	}
	return ""
}

func (a *app) cmdACP(args []string) (int, error) {
	// The ACP proxy is ALWAYS in the path: it's coop's control point for the editor session —
	// restart resilience, plus rewriting the session so coop owns the toolbar (yolo, model default,
	// coop's plain/preset toolbar selectors). The OUTER process validates the args (fail fast), then
	// supervises; the INNER (COOP_ACP_INNER=1) runs the box.
	inner := args // the args the supervisor re-execs as `coop acp <inner>`; the inner re-parses them
	peerVals, args, err := extractPeer(args)
	if err != nil {
		return 2, err
	}
	// Resolve the --peer peers HERE, before the outer/inner split — so an editor's
	// agent_servers entry with a bad peer (unknown/unauthed, or an @account) fails fast in the
	// OUTER process, not silently later inside the box.
	peers, err := a.resolvePeers("--peer", peerVals)
	if err != nil {
		return 2, err
	}
	// The positional who-runs slot pins the session: a TARGET (provider[:model][/effort][@account],
	// so an editor's agent_servers entry runs ["acp","claude:opus@work"]) OR a PRESET NAME (routing +
	// role wiring; its lead is the agent — or governor, under fusion). Parsed BEFORE the inner
	// env-override block so a preset-rotation rung (COOP_ACP_TARGET) still wins over the launch-time
	// model/account. fusion is a keyword (a governor slot follows), not itself a provider/preset.
	model, profile, effort := "", "", ""
	tool, toolSet := "", false // no implicit default; an empty tool falls to the required-provider error below
	governor := ""
	presetName := ""
	consumed := 0 // positional tokens accounted for (the agent, plus a governor under fusion)
	isFusion := len(args) > 0 && args[0] == "fusion"
	// takeWho classifies a positional who slot: a target folds its model/effort/account in and sets
	// the provider; a preset name is captured for loadRunPreset below. Shared by the agent and the
	// fusion-governor slot so both accept a preset.
	takeWho := func(who string, provider *string) error {
		if !isTargetHead(who) {
			presetName = who
			return nil
		}
		t, terr := agents.ParseTarget(who)
		if terr != nil {
			return terr
		}
		*provider = t.Provider
		toolSet = true
		if terr := foldTarget(t, &model, &profile); terr != nil {
			return terr
		}
		effort = t.Effort
		return nil
	}
	switch {
	case isFusion:
		consumed = 1
		governor, toolSet = "", false // named explicitly (or via a preset lead) — no implicit default
		if len(args) > 1 {
			if terr := takeWho(args[1], &governor); terr != nil {
				return 2, terr
			}
			consumed = 2
		}
	case len(args) > 0:
		if terr := takeWho(args[0], &tool); terr != nil {
			return 2, terr
		}
		consumed = 1
	}
	// Reject leftover tokens rather than silently ignore them (loop/fork do the same) — the ACP
	// adapter takes no extra args, so `coop acp claude foo`/`--nope` is a mistake worth surfacing.
	if leftover := args[consumed:]; len(leftover) > 0 {
		return 2, fmt.Errorf("coop acp: unexpected argument %q (usage: coop acp [claude|codex|gemini|grok|fusion [governor]][:model][@account] | <preset>)", leftover[0])
	}
	// A running ACP session can switch its credential/preset/provider via coop's selector; the
	// supervisor re-execs the inner box with the resolved spawn target in the env
	// (COOP_ACP_TARGET, wire grammar) plus the preset whose roles mount (COOP_ACP_PRESET). The
	// target is the COMPLETE spawn intent — provider, model, account are taken from it verbatim
	// (empty slots mean the provider's defaults), so a provider switch or a cross-provider preset
	// rung fully replaces the launch identity instead of leaking the old lead's model/account.
	if os.Getenv("COOP_ACP_INNER") != "" {
		if ps := os.Getenv("COOP_ACP_PRESET"); ps != "" {
			presetName = ps
		}
		if tv := os.Getenv("COOP_ACP_TARGET"); tv != "" {
			t, terr := agents.ParseTarget(tv)
			if terr != nil {
				return 2, fmt.Errorf("COOP_ACP_TARGET: %v", terr)
			}
			tool, toolSet = t.Provider, true
			governor = t.Provider // under fusion the same switch retargets the governor
			model, effort, profile = t.Model, t.Effort, t.Account()
		}
	}
	p, err := a.loadRunPreset(presetName)
	if err != nil {
		return 2, err
	}
	if isFusion {
		governor = presetLeadAgent(p, governor, toolSet)
		if governor == "" {
			return 2, errors.New("coop acp fusion: name the governor — coop acp fusion <agent> (or a preset name, whose lead governs)")
		}
		if !fusion.Valid(governor, agents.Names()) {
			return 2, fmt.Errorf("unknown governor %q — use %s", governor, agentChoices())
		}
		if err := fusionLadderGuard(p, governor); err != nil {
			return 2, err
		}
		tool = governor
	} else {
		tool = presetLeadAgent(p, tool, toolSet)
		// A bare `coop acp` (no provider, no preset lead) defaults to the first signed-in provider
		// instead of erroring: the editor toolbar's provider dropdown can switch it live, so an
		// implicit default is safe HERE — unlike `coop claude`/`coop loop`, which stay strict since
		// there's no dropdown to correct a wrong guess. Nothing signed in falls through to the error.
		if tool == "" {
			tool = defaultACPProvider(a.cfg)
		}
	}
	if !agents.Valid(tool) {
		return 2, errors.New("coop acp: no provider named and none signed in — run 'coop login <agent>' (claude|codex|gemini|grok), or name one: coop acp claude")
	}
	// Fail a bad credential fast, in the outer process, before spawning anything (the inner's
	// applyOneOff does the real selection).
	if profile != "" && !slices.Contains(a.cfg.Profiles(tool), profile) {
		return 2, fmt.Errorf("%s has no account %q — sign in first: coop login %s@%s", tool, profile, tool, profile)
	}
	// The outer process owns the editor stream via the proxy; it builds coop's control layer (the
	// toolbar rewrite + preset/plain selectors) and re-execs `coop acp <inner>` (COOP_ACP_INNER
	// set) to run the box, the current selection carried in the env. The inner falls through to box.Run.
	if os.Getenv("COOP_ACP_INNER") == "" {
		repo, _ := box.ResolveRepo(a.cfg.RepoOverride)
		ctrlModel := model
		if ctrlModel == "" {
			ctrlModel = a.cfg.ModelFor(tool)
		}
		ctrlEffort := effort
		if ctrlEffort == "" {
			ctrlEffort = a.cfg.EffortFor(tool)
		}
		// Ports the inner box will publish (.agent/project.yaml serve), reported to the editor once per
		// session. Deterministic host ports (project.HostPort), so these match what box.Run binds. Only
		// when egress is open — otherwise nothing publishes, so nothing to announce.
		var serveURLs []string
		if a.cfg.Egress == "open" {
			if pj, err := project.Load(repo); err == nil {
				for _, port := range pj.Serve.Ports {
					serveURLs = append(serveURLs, fmt.Sprintf("box :%d → http://localhost:%d", port, project.HostPort(repo, port)))
				}
			}
		}
		sel := acpSelection{Account: profile, Preset: presetName}
		if sel.Account == "" && sel.Preset == "" {
			sel.Account = a.cfg.ActiveProfile(tool)
		}
		if toolSet {
			sel.Provider = tool
		}
		ctrl := newACPControl(a.cfg, tool, ctrlModel, ctrlEffort, repo, sel, a.acpPresetNames(repo), serveURLs, isFusion)
		return a.cmdACPSupervise(inner, ctrl)
	}
	a.applyPreset(p, tool)
	if err := a.applyOneOff(tool, model, profile, effort); err != nil {
		return 2, err
	}
	// Built AFTER the model selection: gemini's ACP command is its own binary and carries
	// the resolved model as a flag. tool passed agents.Valid above, so this can't miss.
	cmd, _ := acpCommand(a.cfg, tool)
	repo, img, err := a.resolveImage()
	if err != nil {
		return -1, err
	}
	lead := "" // named peers (or a preset) opt the session into the second-opinion directive
	if len(peers) > 0 || a.preset != nil {
		lead = tool // a preset's routing contract mounts via ConsultLead too
	}
	// ACP speaks to an editor over stdio, not a human, so run quiet: Quiet drops coop's
	// own progress lines, and COOP_QUIET tells the box to provision the toolchain silently.
	extra := []string{"-e", "COOP_QUIET=1"}
	// Under a supervisor, give the box a deterministic identity: --cidfile lets the supervisor
	// tear it down by id even before its labels are queryable (see cmdACPSupervise's stop()).
	if cid := os.Getenv("COOP_ACP_CIDFILE"); cid != "" {
		extra = append(extra, "--cidfile", cid)
	}
	return box.Run(a.cfg, a.rt, box.RunSpec{
		// A supervisor (which reconnects the box) passes COOP_ACP_SUPERVISOR; that tags
		// the box so build/update can restart it and the supervisor can kill exactly it.
		Image: img, Repo: repo, Workdir: repo, Cmd: cmd, ForceNoTTY: true, Agent: tool, Serve: true,
		SupervisorID:   os.Getenv("COOP_ACP_SUPERVISOR"),
		FusionGovernor: governor, ConsultLead: lead, Peers: peers, Preset: a.preset, Quiet: true,
		ExtraArgs: extra,
		Homes:     a.cfg.Homes, Network: a.cfg.Network, Cache: a.cfg.Cache,
	})
}

// acpResumeState is the whole handoff a `coop acp` supervisor carries across a SIGHUP re-exec: the
// proxy's session state + the controller's selection. JSON-serialized to a 0600 temp file whose path
// rides COOP_ACP_RESUME_STATE into the re-exec'd process.
type acpResumeState struct {
	Proxy acpproxy.Snapshot `json:"proxy"`
	Ctrl  ctrlSnapshot      `json:"ctrl"`
}

// writeResumeState JSON-encodes the handoff to a 0600 temp file (CreateTemp is 0600) and returns its
// path — the setup lines it carries are sensitive, so it's owner-only and removed after one read.
func writeResumeState(st acpResumeState) (string, error) {
	data, err := json.Marshal(st)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "coop-acp-resume-*.json")
	if err != nil {
		return "", err
	}
	if _, werr := f.Write(data); werr != nil {
		f.Close()
		os.Remove(f.Name())
		return "", werr
	}
	if cerr := f.Close(); cerr != nil {
		os.Remove(f.Name()) // a flush failure on close still wrote bytes — don't leave the setup lines in /tmp
		return "", cerr
	}
	return f.Name(), nil
}

// readResumeState reads + REMOVES the handoff file (consumed once, so a stale file can't resurrect on
// a later crash-respawn) and unsets the env var so the child boxes don't inherit it.
func readResumeState(path string) (acpResumeState, error) {
	defer os.Remove(path)
	os.Unsetenv("COOP_ACP_RESUME_STATE")
	var st acpResumeState
	data, err := os.ReadFile(path)
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(data, &st)
}

// cmdACPSupervise serves the editor on stdio and runs the real `coop acp <rest>` as a
// child (COOP_ACP_INNER set so the child runs the box, not another supervisor). When
// the child's container dies, acpproxy starts a new child and replays the ACP
// handshake, so the editor never sees a disconnect (see internal/acpproxy).
func (a *app) cmdACPSupervise(rest []string, ctrl *acpControl) (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 1, fmt.Errorf("acp --supervise: %w", err)
	}
	inner := append([]string{"acp"}, rest...)
	// A per-supervisor id, stamped on this supervisor's boxes (coop.sup=<id>) so it can
	// kill exactly its own box(es) on teardown — not other agents' supervised boxes.
	superID, err := newSupervisorID()
	if err != nil {
		return 1, err
	}

	// A SIGHUP re-exec left us its state: restore the controller's selection and hand the proxy
	// snapshot to Run so the editor's live threads are re-established on the first (fresh) box. A
	// missing/corrupt file degrades to a fresh start (new threads still work).
	var resume *acpproxy.Snapshot
	if path := os.Getenv("COOP_ACP_RESUME_STATE"); path != "" {
		if st, rerr := readResumeState(path); rerr == nil {
			ctrl.restore(st.Ctrl)
			resume = &st.Proxy
			acpproxy.Trace("resumed from re-exec: %d session(s)", len(st.Proxy.Sessions))
		} else {
			fmt.Fprintf(os.Stderr, "coop acp: resume state unreadable (%v) — starting fresh\n", rerr)
		}
	}
	// SIGHUP → a graceful reload (re-exec the freshly-built binary in place). SIGTERM/SIGINT stay
	// STOP (below), so coop is always stoppable.
	reload := make(chan struct{}, 1)
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go func() {
		<-hup
		select {
		case reload <- struct{}{}:
		default:
		}
	}()

	// Keep a box warm per OTHER signed-in provider so a provider switch swaps to a hot adapter
	// (proxy replay only) instead of cold-booting one (~5s). Behind the factory: a miss cold-spawns,
	// so correctness is unaffected. COOP_ACP_WARM=0 opts out (a low-RAM escape hatch).
	warm := os.Getenv("COOP_ACP_WARM") != "0"
	pool := newWarmPool(warm, func(provider string) (*acpproxy.Child, error) {
		return a.spawnBox(context.Background(), self, inner, superID, ctrl, agents.Target{Provider: provider}, "", true, os.Stderr)
	})
	factory := func(ctx context.Context) (*acpproxy.Child, error) {
		t, psName, ok := ctrl.spawnTarget()
		if bareProviderSwitch(t, psName, ok) {
			if c := pool.checkout(t.Provider); c != nil {
				go pool.refill(t.Provider) // keep it hot for a repeat switch
				return c, nil
			}
		}
		child, cerr := a.spawnBox(ctx, self, inner, superID, ctrl, t, psName, ok, os.Stderr)
		if bareProviderSwitch(t, psName, ok) && cerr == nil {
			go pool.refill(t.Provider)
		}
		return child, cerr
	}
	// Fan the other providers' boxes out in the background — the active one is spawned by Run's first
	// factory call, so startup latency is unchanged.
	if warm {
		go func() {
			others := ctrl.spawnableProviders(ctrl.leadProvider())
			for _, prov := range others {
				pool.refill(prov)
			}
			acpproxy.Trace("warmed %d provider(s)", len(others))
		}()
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	defer pool.reap() // Stop held warm boxes on any exit path; the label sweep still reaps their containers
	err = acpproxy.RunWith(ctx, os.Stdin, os.Stdout, factory, ctrl.hooks(), acpproxy.RunOpts{Resume: resume, Reload: reload})
	// A SIGHUP reload: write the combined state to a 0600 temp file and re-exec THIS binary in place —
	// same PID + fd 0/1/2, so the editor's transport never breaks. Run's reload path already stopped
	// the box; reap the warm boxes here (execve replaces the image, so deferred reap won't run) and
	// skip the label sweep — the re-exec'd process regenerates its own superID and owns the next box.
	if snap, ok := acpproxy.ReloadSnapshot(err); ok {
		pool.reap()
		// Sweep any box still labelled with THIS superID before exec — a warm spawn that was mid-flight
		// (reap only stops boxes already parked) would otherwise reparent to init and never be reaped
		// (the re-exec'd process uses a fresh superID). Safe here: Run already stopped the active box
		// and no new box is spawned until after exec, so nothing we need is swept.
		a.rt.KillByLabel(box.LabelSupervisor, superID)
		path, werr := writeResumeState(acpResumeState{Proxy: *snap, Ctrl: ctrl.snapshot()})
		if werr != nil {
			return 1, fmt.Errorf("acp reload: %w", werr)
		}
		if xerr := syscall.Exec(self, os.Args, append(os.Environ(), "COOP_ACP_RESUME_STATE="+path)); xerr != nil {
			os.Remove(path)
			return 1, fmt.Errorf("acp reload: exec %s: %w", self, xerr)
		}
		return 0, nil // unreachable — execve replaced the process on success
	}
	// Final teardown sweep, once, when the whole supervised session ends: a per-generation Stop
	// removes only its own box (by cidfile), so the last live generation — or a box orphaned by a
	// swap — is cleaned up here by this supervisor's id. (Doing this per-generation would kill the
	// just-spawned next box, which shares the id, fork-bombing the supervisor on the first resume.)
	a.rt.KillByLabel(box.LabelSupervisor, superID)
	if err != nil && !errors.Is(err, context.Canceled) {
		return 1, err
	}
	return 0, nil
}

func newSupervisorID() (string, error) {
	idbuf := make([]byte, 8)
	if _, err := rand.Read(idbuf); err != nil {
		return "", err
	}
	return hex.EncodeToString(idbuf), nil
}

const acpCleanupTimeout = 5 * time.Second

func cleanACPChildEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, item := range env {
		key, _, _ := strings.Cut(item, "=")
		switch key {
		case "COOP_ACP_INNER", "COOP_ACP_SUPERVISOR", "COOP_ACP_TARGET", "COOP_ACP_PRESET", "COOP_ACP_CIDFILE", "COOP_ACP_RESUME_STATE":
			continue
		}
		out = append(out, item)
	}
	return out
}

// bareProviderSwitch reports whether a spawn target is a plain provider switch at default
// account/model/effort — a bare Target{Provider} — the slow, common case the warm pool covers. A
// pinned target or preset spawns cold (rare; correctness is unaffected).
func bareProviderSwitch(t agents.Target, psName string, ok bool) bool {
	return ok && psName == "" && t.Model == "" && t.Effort == "" && len(t.Accounts) == 0
}

// spawnBox execs a `coop acp` inner box for the given spawn target and wraps it as an acpproxy.Child
// — the ONE spawn path for the live factory, warm-pool prewarm, and short-lived model probe, so each
// gets the same credentials, process isolation, and teardown.
func (a *app) spawnBox(ctx context.Context, self string, inner []string, superID string, ctrl *acpControl, t agents.Target, psName string, hasTarget bool, stderr io.Writer) (*acpproxy.Child, error) {
	provider := t.Provider
	if provider == "" && ctrl != nil {
		provider = ctrl.leadProvider()
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, err
	}
	cidDir, cidPath := "", ""
	env := append(cleanACPChildEnv(os.Environ()), "COOP_ACP_INNER=1", "COOP_ACP_SUPERVISOR="+superID)
	if hasTarget {
		if ctrl != nil { // model probes use a bare provider target and need no reset/preset wait
			if psName != "" {
				ctrl.waitForPresetRung(ctx)
			} else if acct := t.Account(); acct != "" {
				ctrl.waitForReset(ctx, acct)
			}
		}
		if psName != "" {
			env = append(env, "COOP_ACP_PRESET="+psName)
		}
		env = append(env, "COOP_ACP_TARGET="+t.String())
		acpproxy.Trace("spawn box on target=%s preset=%s", t.String(), psName)
	}
	if a.rt.SupportsCIDFile() {
		if d, derr := os.MkdirTemp("", "coop-acp-cid-"); derr == nil {
			cidDir = d
			cidPath = filepath.Join(d, "cid")
			env = append(env, "COOP_ACP_CIDFILE="+cidPath)
		}
	}
	cmd := exec.Command(self, inner...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, stderr
	// Own process group: a plain Process.Kill() reaps only the inner `coop` and orphans its
	// `docker run` grandchild; killing the whole group (-pgid) reaches the run client too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		if cidDir != "" {
			os.RemoveAll(cidDir)
		}
		return nil, err
	}
	inR.Close()  // the child holds the read end now
	outW.Close() // ...and the write end; outR sees EOF when the child exits
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()
	stop := func() {
		// Kill the whole process group and close its pipes first, so cleanup cannot wait forever behind
		// a wedged run client. Then remove ONLY this generation's box by cidfile under a fresh bound —
		// no label sweep here: every generation shares this supervisor's id.
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		inW.Close()
		outR.Close()
		if cidPath != "" {
			if cid, rerr := os.ReadFile(cidPath); rerr == nil {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), acpCleanupTimeout)
				_ = a.rt.RemoveContainerContext(cleanupCtx, strings.TrimSpace(string(cid)))
				cancel()
			}
		}
		if cidDir != "" {
			os.RemoveAll(cidDir)
		}
	}
	return &acpproxy.Child{In: inW, Out: outR, Stop: stop, Provider: provider}, nil
}

// agentChoices lists the registered agents for a "use one of …" error, from the registry so a
// new agent is offered without editing the string. Sorted (agents.Names()), comma-separated.
func agentChoices() string { return strings.Join(agents.Names(), ", ") }

// cmdFusion runs a council: the governor agent (a leading `claude|codex|gemini`, else
// COOP_FUSION_GOVERNOR) runs normally — it edits and does the real work — while a fusion
// instruction injected into its instruction file tells it to consult its two peers
// read-only and synthesize. It behaves like `coop <agent>`: `coop fusion claude` opens
// claude interactively; trailing `<args>` pass through to the governor.
func (a *app) cmdFusion(args []string) (int, error) {
	// --model/--credential are retired — pin the governor in its target (coop fusion
	// claude:opus@work); the peers keep their own defaults. `--`-aware, so the
	// governor's OWN flags (codex's --profile) still pass through after a `--`.
	// The council is named EXPLICITLY with --peer (repeatable).
	peerVals, args, err := extractPeer(args)
	if err != nil {
		return 2, err
	}
	peers, err := a.resolvePeers("--peer", peerVals)
	if err != nil {
		return 2, err
	}
	// The governor slot is a target OR a preset name (parseGovernor classifies the leading
	// positional). Its model + account fold into this run's one-off selection (the peers keep
	// their own); a preset's lead governs when no target is named, and its role routing rides
	// along with the council directive.
	governor, model, profile, effort, presetName, rest, govSet, err := a.parseGovernor(args)
	if err != nil {
		return 2, err
	}
	p, err := a.loadRunPreset(presetName)
	if err != nil {
		return 2, err
	}
	// Fusion needs a council: at least one --peer, OR a preset that supplies consult roles.
	// No implicit "consult everyone signed in" — the peers participate only when named.
	if len(peers) == 0 && (p == nil || !p.HasConsult()) {
		return 2, errors.New("fusion needs its council — name each peer: coop fusion <governor> --peer <agent> [--peer <agent> …]")
	}
	governor = presetLeadAgent(p, governor, govSet)
	if governor == "" {
		return 2, errors.New("coop fusion: name the governor — coop fusion <agent> --peer <agent>… (or a preset name, whose lead governs)")
	}
	if !fusion.Valid(governor, agents.Names()) {
		return 2, fmt.Errorf("unknown governor %q — use %s", governor, agentChoices())
	}
	if err := fusionLadderGuard(p, governor); err != nil {
		return 2, err
	}
	a.applyPreset(p, governor)
	if err := a.applyOneOff(governor, model, profile, effort); err != nil {
		return 2, err
	}
	repo, img, err := a.resolveImage()
	if err != nil {
		return -1, err
	}
	// The governor's autonomous default command, plus any extra args you pass through.
	cmd := append(append([]string{}, a.defaultCmd(governor)...), dropDashDash(rest)...)
	council := make([]string, 0, len(peers))
	for _, pt := range peers {
		council = append(council, pt.String())
	}
	desc := strings.Join(council, " + ")
	if desc == "" {
		desc = "the preset's roles"
	}
	ui.Info("fusion: %s governs; peers %s consulted read-only", governor, desc)
	pre := gitOut(repo, "rev-parse", "HEAD")
	code, err := box.Run(a.cfg, a.rt, box.RunSpec{
		Image: img, Repo: repo, Cmd: cmd, Agent: governor, FusionGovernor: governor, Peers: peers, Preset: a.preset,
		Homes: a.cfg.Homes, Network: a.cfg.Network, Cache: a.cfg.Cache,
	})
	a.signOnBoxExit(repo, pre, false)
	return code, err
}

// parseGovernor classifies the leading who-runs positional: a TARGET
// (provider[:model][/effort][@account]) is the governor, a non-target bare word is a PRESET NAME
// (its lead governs — resolved by the caller's loadRunPreset). Only the FIRST leading positional
// is the who; everything else passes through to the governor. explicit reports whether a governor
// TARGET was named (so a preset's lead only fills the default); model/profile carry the governor
// target's model + single account for the one-off selection.
func (a *app) parseGovernor(args []string) (governor, model, profile, effort, presetName string, rest []string, explicit bool, err error) {
	tookGov := false // no implicit default — the governor is named explicitly (or via a preset lead)
	took := func() bool { return tookGov || presetName != "" || len(rest) > 0 }
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--":
			return governor, model, profile, effort, presetName, append(rest, args[i+1:]...), tookGov, nil // everything after passes through
		case !took() && isTargetHead(args[i]):
			// Only the FIRST leading target is the governor: `coop fusion claude:opus/high@work`
			// (matches `coop acp fusion …`). A second agent token passes through to the governor
			// (not silently swallowed as the governor).
			t, terr := agents.ParseTarget(args[i])
			if terr != nil {
				return governor, model, profile, effort, presetName, rest, tookGov, terr
			}
			governor, tookGov = t.Provider, true
			if terr := foldTarget(t, &model, &profile); terr != nil {
				return governor, model, profile, effort, presetName, rest, tookGov, terr
			}
			effort = t.Effort
		case !took() && !strings.HasPrefix(args[i], "-"):
			// The FIRST leading non-target bare word is a preset name (the who slot). Its lead
			// governs; loadRunPreset (the caller) validates it exists.
			presetName = args[i]
		default:
			rest = append(rest, args[i])
		}
	}
	return governor, model, profile, effort, presetName, rest, tookGov, nil
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
	a.recycleBoxes()
	return 0, nil
}

// recycleBoxes restarts supervised boxes after a rebuild so they reconnect on the new
// image — a coop acp supervisor replays the ACP handshake, so the editor doesn't
// notice. New runs use the fresh image anyway (containers are anonymous). Other
// running boxes (loops, forks, an un-supervised session) are left alone; SIGKILLing
// them would lose work, and they pick up the new image when they next start.
func (a *app) recycleBoxes() {
	total := a.rt.CountByLabel(box.LabelKey, box.LabelBox)
	supervised := a.rt.CountByLabel(box.LabelSupervised, box.LabelOn)
	if n := a.rt.KillByLabel(box.LabelSupervised, box.LabelOn); n > 0 {
		ui.Info("restarted %s onto the new image", ui.Count(n, "supervised session"))
	}
	if others := total - supervised; others > 0 {
		ui.Info("%s still on the old image until restarted", ui.Count(others, "other running container"))
	}
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
	if err := a.ensureRuntime(); err != nil {
		return -1, err
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	ui.Info("updating the box: newer base image + latest agent CLIs and ACP adapters")
	if err := box.Build(a.rt, a.cfg, repo, true, resolveVersion()); err != nil {
		return -1, err
	}
	a.recycleBoxes()
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
	switch {
	case !releaseVersion(cur):
		ui.Note("coop %s is a dev/source build (self-update doesn't apply); the latest release is v%s", cur, l)
	case versionLess(c, l):
		ui.Note("coop v%s → v%s available — run 'coop update'", c, l)
	default:
		ui.OK("coop v%s is up to date", c)
	}

	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		ui.Note("(not in a repo — skipped the box image check)")
		return 0, nil
	}
	img := box.ImageForRepo(repo, a.cfg.BaseImage, a.cfg.ImageOverride)
	if at, ok := box.ImageBuildAge(a.cfg, img); ok {
		when := "today"
		if days := int(time.Since(at).Hours() / 24); days > 0 {
			when = ui.Count(days, "day") + " ago"
		}
		ui.Note("box image %s: built %s", img, when)
	}
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
	if err := a.rt.EnsureDaemon(); err != nil {
		return -1, err
	}
	if a.rt.Name == "container" {
		return -1, errors.New("the Apple 'container' runtime has no compose yet — use Docker or Podman for services")
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	file := box.ComposeFile(repo)
	if file == "" {
		return -1, errors.New("no .agent/compose.yml — run 'coop init --services postgres,redis' to scaffold one")
	}
	proj := box.ServicesProject(repo)
	rel, _ := filepath.Rel(repo, file)
	ui.Info("starting services from %s (waiting until healthy)", rel)
	if err := box.EnsureServices(a.rt, repo, os.Stdout, os.Stderr); err != nil {
		return -1, err
	}
	ui.Info("up on network %s_default — the box reaches them by name (db, redis, ...)", proj)
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
	if err := a.rt.EnsureDaemon(); err != nil {
		return -1, err
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	file := box.ComposeFile(repo)
	if file == "" {
		return -1, errors.New("no .agent/compose.yml here — nothing to bring down")
	}
	proj := box.ServicesProject(repo)
	cargs := []string{"compose", "-p", proj, "-f", file, "down"}
	if volumes {
		cargs = append(cargs, "--volumes")
	}
	return a.rt.Run(os.Stdin, os.Stdout, os.Stderr, cargs...)
}

// scaffoldableAgents are the agents with a per-agent dir `coop init` can scaffold (grok reads the
// root AGENTS.md, no dir of its own).
var scaffoldableAgents = []string{"claude", "codex", "gemini"}

// scaffoldAgentSet resolves which per-agent dirs `coop init` scaffolds: the --agents list when given
// ("all" → every scaffoldable agent; else the named ones, kept to the scaffoldable set), else the
// agents you're signed in to. Empty (no --agents, none signed in) → .agent/ only — a box synthesizes
// a missing agent's skills from .agent/ on demand, so the un-scaffolded agents still work.
func scaffoldAgentSet(cfg *config.Config, flag string, flagSet bool) []string {
	pick := func(names []string) []string {
		var out []string
		for _, n := range names {
			if slices.Contains(scaffoldableAgents, n) && !slices.Contains(out, n) {
				out = append(out, n)
			}
		}
		return out
	}
	if flagSet {
		if strings.TrimSpace(flag) == "all" {
			return append([]string{}, scaffoldableAgents...)
		}
		return pick(strings.FieldsFunc(flag, func(r rune) bool { return r == ',' || r == ' ' }))
	}
	return pick(box.AuthedAgents(cfg))
}

func (a *app) cmdInit(args []string) (int, error) {
	stack := ""
	var services []string
	servicesSet := false
	agentsFlag := ""
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
			services, servicesSet = parseServices(v), true
			i += n - 1
			continue
		}
		if v, n, ok, e := flagValue(args, i, "--agents"); ok {
			if e != nil {
				return 2, e
			}
			agentsFlag, agentsSet = v, true
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
	// Detect the repo's stack(s) for the commit gate; if nothing's detected and we're at a
	// terminal, ask rather than guess — coop never imposes a check the repo doesn't use.
	langs := scaffold.DetectStacks(repo)
	if len(langs) == 0 && ui.IsTerminal(os.Stdin) {
		langs = promptGateLangs(os.Stdin)
	}
	// Sibling services (db/redis) are opt-in — coop doesn't add a compose file a project may
	// not want. Ask at a terminal unless --services already said.
	if !servicesSet && ui.IsTerminal(os.Stdin) {
		services = promptServices(os.Stdin)
	}
	// Which per-agent dirs to scaffold: `--agents` if given (a name list, or "all"), else the agents
	// you're signed in to. Others aren't clutter you delete later — a box synthesizes a missing
	// agent's skills from .agent/ on demand.
	agentDirs := scaffoldAgentSet(a.cfg, agentsFlag, agentsSet)
	if err := scaffold.Init(repo, stack, langs, agentDirs); err != nil {
		return 0, err
	}
	if err := scaffold.WriteCompose(repo, services); err != nil {
		return 0, err
	}
	if err := a.writeMCPStub(); err != nil {
		return 0, err
	}
	// One "coop:" anchor closes the dim per-file log; then the optional Docker-box guidance
	// (only when the repo has its own Docker and no Dockerfile.agent yet); then the actions you
	// need to take next stand on their own — derived from what actually landed, not a fixed script.
	ui.Info("scaffolded into %s", repo)
	if len(agentDirs) > 0 {
		ui.Info("per-agent dirs: %s — missing artifacts synthesize in-box from .agent/ on demand", strings.Join(agentDirs, ", "))
	} else {
		ui.Info("no agents signed in — scaffolded .agent/ only; sign in and run, or `coop init --agents claude,codex`")
	}
	// Monorepo: detect member dirs (each with a .agent/), record them in the root .agent/project.yaml
	// so coop aggregates their task queues, and give each member a project.yaml if it lacks one. A
	// single repo still gets a project.yaml template. Never clobbers an existing file.
	subs := scaffold.DetectSubprojects(repo)
	if _, err := scaffold.WriteProject(repo, subs); err != nil {
		return 0, err
	}
	for _, s := range subs {
		// Members get only the minimal set — their task queue + backlog + project.yaml — since they
		// share the root's AGENTS.md, skills, rules, hooks, and box.
		if err := scaffold.InitSubproject(filepath.Join(repo, s)); err != nil {
			return 0, err
		}
	}
	if len(subs) > 0 {
		ui.Info("monorepo: %d member(s) (%s) — .agent/project.yaml aggregates their task queues", len(subs), strings.Join(subs, ", "))
		// A re-init keeps an existing project.yaml; flag any detected members it doesn't list yet.
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
	scaffold.SuggestDocker(repo)
	ui.Steps(initNextSteps(repo, services)...)
	return 0, nil
}

// initNextSteps is the short list of actions to run after scaffolding, built from what landed: a
// build step when there's a Dockerfile.agent, a `coop up` when sibling services were added, and
// always the edit-then-loop step. Assembled here (not in scaffold) so the whole list is shown in
// one block.
func initNextSteps(repo string, services []string) []string {
	var steps []string
	// coop runs forks and the loop on top of git (worktrees, rebase-merge); a repo that
	// isn't initialized yet needs that first, so lead with it.
	if !pathExists(filepath.Join(repo, ".git")) {
		steps = append(steps, "`git init`  (coop's forks and loop need a git repo)")
	}
	if fileExists(filepath.Join(repo, "Dockerfile.agent")) {
		steps = append(steps, "review Dockerfile.agent, then `coop build`")
	}
	if len(services) > 0 {
		steps = append(steps, fmt.Sprintf("`coop up`  (starts %s for the box)", strings.Join(services, " + ")))
	}
	steps = append(steps, "`coop tasks add \"<title>\"`, then `coop loop`")
	return steps
}

// writeMCPStub seeds an empty shared mcp.json — coop's one MCP source of truth, translated to
// each agent — at the global config path if absent, so there's an obvious, correctly-shaped file
// to drop servers into. An empty (no-server) file is inactive (see Config.MCPActive), so the stub
// changes no run until you add a server. Never clobbers an existing config.
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

// parseServices reads a --services value (comma/space-separated) into known service names,
// dropping blanks, "none", and unknowns.
func parseServices(s string) []string {
	var out []string
	for _, tok := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return r == ',' || r == ' ' }) {
		if tok != "none" && slices.Contains(scaffold.ComposeServices, tok) && !slices.Contains(out, tok) {
			out = append(out, tok)
		}
	}
	return out
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

// parseLoopArgs resolves `coop loop`'s leading who-runs positional — a TARGET
// (provider[:model][/effort][@account,…]) OR a PRESET NAME (validated by cmdLoop's loadRunPreset) —
// and its flags. Model + account come from the target (`--model`/`--credential` are retired);
// `--peer`/`--tasks` are pre-extracted by cmdLoop. hasTarget is false and presetName "" when no
// positional was given (a loop.yaml work.agent then supplies the lead).
func parseLoopArgs(args []string, def bool) (t agents.Target, hasTarget bool, presetName string, debugOnFail, preflight, noMCP bool, maxTasks int, err error) {
	preflight = def
	t, hasTarget, presetName, rest, err := takeHeadWho(args)
	if err != nil {
		return agents.Target{}, false, "", false, preflight, false, 0, err
	}
	for i := 0; i < len(rest); i++ {
		x := rest[i]
		switch x {
		case "--debug-on-fail":
			debugOnFail = true
		case "--preflight":
			preflight = true
		case "--no-preflight":
			preflight = false
		case "--no-mcp":
			noMCP = true
		case "--max-tasks":
			if maxTasks > 0 {
				return t, hasTarget, presetName, debugOnFail, preflight, noMCP, maxTasks, errors.New("coop loop: --max-tasks may be specified only once")
			}
			if i+1 >= len(rest) {
				return t, hasTarget, presetName, debugOnFail, preflight, noMCP, 0, errors.New("coop loop: --max-tasks requires a positive integer")
			}
			i++
			n, convErr := strconv.Atoi(rest[i])
			if convErr != nil || n <= 0 {
				return t, hasTarget, presetName, debugOnFail, preflight, noMCP, 0, fmt.Errorf("coop loop: --max-tasks must be a positive integer, got %q", rest[i])
			}
			maxTasks = n
		default:
			return t, hasTarget, presetName, debugOnFail, preflight, noMCP, maxTasks, fmt.Errorf("coop loop: unexpected argument %q (usage: coop loop [<agent>[:model][/effort][@account,…] | <preset>] [--tasks <path>] [--peer <agent>]… [--max-tasks <n>] [--preflight|--no-preflight] [--no-mcp] [--debug-on-fail])", x)
		}
	}
	return t, hasTarget, presetName, debugOnFail, preflight, noMCP, maxTasks, nil
}

func (a *app) cmdLoop(args []string) (int, error) {
	flags, rest, err := extractTasksFlags(args)
	if err != nil {
		return 2, err
	}
	peerVals, rest, err := extractPeer(rest)
	if err != nil {
		return 2, err
	}
	peers, err := a.resolvePeers("--peer", peerVals)
	if err != nil {
		return 2, err
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	// .agent/loop.yaml is the committed loop config; a bad file fails fast, before any box work.
	lc, err := loopcfg.Load(repo)
	if err != nil {
		return 2, err
	}
	// preflight defaults to loop.yaml preflight.enabled; --preflight/--no-preflight override.
	t, hasTarget, presetName, debugOnFail, preflight, noMCP, maxTasks, err := parseLoopArgs(rest, lc.Preflight.Enabled)
	if err != nil {
		return 2, err
	}
	// --no-mcp: this one run mounts no MCP anywhere (the committed form is loop.yaml `mcp: false`,
	// honored inside loop() so fork loops get it too). Blanking MCPFile is the single switch every
	// downstream check keys off (Config.MCPActive) — claude's --mcp-config and the generated
	// codex/gemini configs all stay out of the boxes.
	if noMCP {
		a.cfg.MCPFile = ""
	}
	// A positional preset name: its lead agent leads, its roles seed the run, and its models
	// ladder becomes the rotation. A positional target instead pins the one-off ladder.
	p, err := a.loadRunPreset(presetName)
	if err != nil {
		return 2, err
	}
	// .agent/loop.yaml work.agent is the committed default work ladder — used ONLY when the launch
	// names no positional who (no target and no preset). Its rungs are targets or preset names (a preset
	// rung runs the loop under that preset: its roles + lead ladder, exhausted before the next rung);
	// the first rung sets the lead agent.
	var workLadder []agents.Target
	workAgent := ""
	if !hasTarget && p == nil && len(lc.Work.Agent) > 0 {
		workAgent, p, workLadder, err = a.resolveWorkAgent(lc.Work.Agent)
		if err != nil {
			return 2, err
		}
	}
	agent := presetLeadAgent(p, t.Provider, hasTarget)
	if agent == "" {
		agent = workAgent // loop.yaml work.agent's first rung supplied the lead
	}
	if agent == "" { // provider required — no positional who (target/preset), no loop.yaml work.agent
		return 2, noProviderErr("loop")
	}
	a.applyPreset(p, agent)
	queues, err := taskQueues(a.cfg, repo, flags)
	if err != nil {
		return 2, err
	}
	// The rotation ladder: the positional target (its model + account ladder) wins; else the
	// loop.yaml work.agent ladder; else the preset lead's ladder; else the default (agent model
	// across all signed-in accounts). expandLadder turns it into concrete one-account rungs.
	var ladder []agents.Target
	switch {
	case hasTarget:
		ladder = []agents.Target{t}
	case len(workLadder) > 0:
		ladder = workLadder
	case p != nil && agent == p.LeadAgent:
		ladder = p.LeadLadder
	}
	rot, err := a.buildRotation(agent, ladder)
	if err != nil {
		return -1, err
	}
	img := box.ImageForRepo(repo, a.cfg.BaseImage, a.cfg.ImageOverride)
	return a.loop(repo, img, agent, "", rot, queues, nil, peers, debugOnFail, preflight, maxTasks) // local loop: no fork label
}

// resolveWorkAgent turns a .agent/loop.yaml work.agent ladder into the lead agent, an optional
// preset to apply (the FIRST preset rung — its roles wire the run), and the concrete target ladder
// to rotate: each preset rung expands to its lead ladder (nested — exhausted before the next rung),
// each target rung is itself. The first rung sets the lead agent. A bad preset name errors.
func (a *app) resolveWorkAgent(rungs []string) (agent string, p *preset.Preset, ladder []agents.Target, err error) {
	rs, err := loopcfg.Rungs(rungs)
	if err != nil {
		return "", nil, nil, err
	}
	for _, r := range rs {
		if r.Preset != "" {
			pr, perr := a.loadRunPreset(r.Preset)
			if perr != nil {
				return "", nil, nil, fmt.Errorf("work.agent: %w", perr)
			}
			if agent == "" {
				agent = pr.LeadAgent
			}
			if p == nil {
				p = pr // apply the first preset rung's roles for the run
			}
			ladder = append(ladder, pr.LeadLadder...)
			continue
		}
		if agent == "" {
			agent = r.Target.Provider
		}
		ladder = append(ladder, *r.Target)
	}
	return agent, p, ladder, nil
}

// reviewLadder parses a review stage's raw .agent/loop.yaml agent: rungs into targets, PRESERVING
// provider, model, effort, and account (and every fallback rung) — dropping only preset rungs, since
// a once-per-stage review takes targets, not a rotation of presets. It replaces the old stepModel,
// which kept only (model, effort) off the first rung and discarded the provider — so a claude-led
// run's `codex:…` signoff resolved to `claude --model <a-codex-model>`, an invalid combination, and
// the cross-vendor reviewer the config promised was never actually run.
func reviewLadder(rungs []string) ([]agents.Target, error) {
	rs, err := loopcfg.Rungs(rungs)
	if err != nil {
		return nil, err
	}
	var ladder []agents.Target
	for _, r := range rs {
		if r.Target != nil {
			ladder = append(ladder, *r.Target)
		}
	}
	return ladder, nil
}

// reviewRotation builds a review stage's own rotation from its ladder, so the stage runs on the
// configured provider/model/effort/account and rotates its OWN fallback rungs on a rate limit —
// exactly like the work loop. An empty (or preset-only) ladder falls back to def: between → signoff
// → the work rotation, so an unconfigured stage still reviews on the work target.
func (a *app) reviewRotation(rungs []string, workAgent string, def *rotation) (*rotation, error) {
	ladder, err := reviewLadder(rungs)
	if err != nil {
		return nil, err
	}
	if len(ladder) == 0 {
		return def, nil
	}
	return a.buildRotation(workAgent, ladder)
}

// runReview runs one review stage (signoff or between) on its OWN rotation — the configured
// provider, model, effort, and account — and fails CLOSED. A rate limit rotates the stage's ladder
// (or waits) and retries; a launch error or a nonzero, non-limit exit is retried within a small
// budget, and if the stage still can't run it returns an error so the caller can't mistake "nothing
// reopened" for "reviewed and accepted". A user interrupt (ctx canceled) returns no error — not a
// review failure. Returns the completed review's output so the caller can read its reopen receipt.
// Local counters keep review trouble out of the work loop's stop accounting.
type iterationCmdBuilder func(agent, prompt string) (cmd []string, streaming bool)

func reviewMountPolicy(writes loopcfg.ReviewWrites, queues []string) (bool, []string) {
	if writes.RepositoryWritable() {
		return false, nil
	}
	return true, queues
}

func (a *app) runReview(ctx context.Context, repo, img string, rev *rotation, forkName, prompt, activity string, iterCmd iterationCmdBuilder, hosts []string, writes loopcfg.ReviewWrites, sink io.Writer, peers []agents.Target, wake <-chan struct{}) (string, *iterResult, error) {
	var fails, waits, retries int
	for {
		agent := a.applyTarget(rev)
		cmd, streaming := iterCmd(agent, prompt) // build after rotation so argv matches this provider
		readOnly, writable := reviewMountPolicy(writes, hosts)
		code, out, res, err := a.runIteration(ctx, repo, img, agent, forkName, cmd, streaming, hosts, readOnly, writable, sink, peers, activity)
		if ctx != nil && ctx.Err() != nil {
			return "", nil, nil // user interrupt — the caller handles stopping, not a review failure
		}
		switch action, wait, resetAt := decideIteration(code, err, out, time.Now(), &fails, &waits, &retries); action {
		case actContinue:
			return out, res, nil
		case actWait:
			if rev.rotates() {
				a.rotateOnLimit(rev, resetAt, &waits, wake)
			} else {
				sleepForLimit(wait, resetAt, wake)
			}
		case actRetryNow:
			sleepOrWake(wait, wake)
		case actRetry:
			if fails > maxLoopFailures {
				return "", nil, fmt.Errorf("review stage failed %d times — stopping (a review that can't run is never an accept)", fails)
			}
			sleepOrWake(10*time.Second, wake)
		case actStop:
			return "", nil, fmt.Errorf("review stage failed %d times — stopping (a review that can't run is never an accept)", fails)
		}
	}
}

type reviewReceipt struct {
	verdict  string
	reopened []string
}

// reviewReopenReceipt parses the strict terminal receipt emitted by every review. Old count-only
// receipts are deliberately rejected: only the exact task ids can bind the verdict to the queue
// delta and distinguish review work from unrelated actionable tasks.
func reviewReopenReceipt(output string) (reviewReceipt, bool) {
	const prefix = "REVIEW COMPLETE — "
	lines := strings.Split(output, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, prefix) {
			return reviewReceipt{}, false
		}
		parts := strings.Split(strings.TrimPrefix(line, prefix), " — reopened: ")
		if len(parts) != 2 || (parts[0] != "PASS" && parts[0] != "FAIL") {
			return reviewReceipt{}, false
		}
		var ids []string
		if parts[1] != "none" {
			ids = strings.Split(parts[1], ",")
			if slices.Contains(ids, "") || !slices.IsSorted(ids) {
				return reviewReceipt{}, false
			}
			if slices.ContainsFunc(ids, func(id string) bool { return strings.ContainsAny(id, " \t\r\n") }) {
				return reviewReceipt{}, false
			}
			for j := 1; j < len(ids); j++ {
				if ids[j] == ids[j-1] {
					ids = nil
					break
				}
			}
			if ids == nil {
				return reviewReceipt{}, false
			}
		}
		if (parts[0] == "PASS") != (len(ids) == 0) {
			return reviewReceipt{}, false
		}
		return reviewReceipt{verdict: parts[0], reopened: ids}, true
	}
	return reviewReceipt{}, false
}

func reopenVerdictLost(receipt reviewReceipt, haveReceipt bool, actual, subjects []string) bool {
	if !haveReceipt || !slices.Equal(receipt.reopened, actual) {
		return true
	}
	for _, id := range receipt.reopened {
		if !slices.Contains(subjects, id) {
			return true
		}
	}
	return false
}

// protectedAuditVerdict makes the exceptional between pass fail closed. Ordinary configured
// audits keep their historical warn-and-continue behavior; a protected audit must both run and
// leave a receipt consistent with the queue before another task can trust the edited gate.
func protectedAuditVerdict(protected, interrupted bool, reviewErr error, output string, actual, subjects []string) error {
	if !protected {
		return nil
	}
	if reviewErr != nil {
		return fmt.Errorf("could not run: %w", reviewErr)
	}
	if interrupted {
		return nil
	}
	receipt, ok := reviewReopenReceipt(output)
	if reopenVerdictLost(receipt, ok, actual, subjects) {
		return fmt.Errorf("verdict inconsistent: review reported %s but task delta was %s", receiptClaim(receipt, ok), receiptIDs(actual))
	}
	return nil
}

// receiptClaim renders a review's verdict and exact ids for a compact diagnostic.
func receiptClaim(receipt reviewReceipt, ok bool) string {
	if !ok {
		return "no receipt"
	}
	return fmt.Sprintf("%s reopening %s", receipt.verdict, receiptIDs(receipt.reopened))
}

func receiptIDs(ids []string) string {
	if len(ids) == 0 {
		return "none"
	}
	return strings.Join(ids, ",")
}

// loopWorkPrompt and loopSignoffPrompt name the queue dir(s) the iteration works as ABSOLUTE
// in-box paths (the box's working dir is repo, bind-mounted at its real path). A relative
// ".agent/tasks" resolves fine for claude/codex (cwd-relative), but gemini's read_file rejects
// a relative path — so the queues (and AGENTS.md) are named absolute for every agent. With
// several queues (a monorepo's per-component trees) they're all listed so the agent works the union.
// The contract is REFERENCED, not re-read: every agent auto-loads its instruction file (the
// CLAUDE.md→AGENTS.md symlink / AGENTS.md / GEMINI.md), so an unconditional "Read AGENTS.md" made
// each iteration re-read ~2K tokens already in its context and burn a tool turn doing it — the
// conditional keeps the fallback for a repo where the auto-load didn't happen.
func loopWorkPrompt(repo string, queues []string, assignedID, agent string, peers []agents.Target, p *preset.Preset) string {
	instructions := strings.Join([]string{
		"The project contract is your instruction file, normally already loaded in your context — read %s only if its content is not.",
		"Read the task queue(s) %s, then work the queue per the protocol. A task is a folder under a queue dir and its state is its directory (named with a sort prefix): 00_todo/ · 10_in_progress/ · 50_blocked/ · 99_done/.",
		"`coop` is NOT installed in this box, so you change a task's state by MOVING its folder between those dirs yourself — that move IS the state change; do not try to run `coop`.",
		"Work task %s, already claimed in 10_in_progress/. Read that assigned task's task.md and state.md (its resume note — where prior work stopped, the next action, and traps), then run `git status` and `git diff` to find any uncommitted work; continue it, or discard partial work with `git restore`/`git checkout` and redo it if off-track.",
		"As you work, keep that task's state.md current — a small, overwritten snapshot of the status, what is done, the next action, and any traps — refreshed before each commit and before you pause; append your reasoning to its log.md.",
		"Put disposable but resumable scratch work (temporary worktrees, patches, generated files) under the assigned task's tmp/ directory; it survives interruption and blocked transitions but Coop removes it on completion. Before finishing, promote anything a reviewer or future maintainer needs to the task's durable artifacts/ directory.",
		"Read a file before you edit it — an edit to a file you haven't read is rejected and wastes a turn (don't survey with `cat` then edit).",
		"Do the work, run the gate, then commit your work — END the commit message with a trailer line `Coop-Task: <task-id>` (the task id is its folder name), so the harness can bind the commit to the task, resume correctly if interrupted, and reconcile the queue after a fork merge.",
		"When you cite that commit in state.md or log.md, name it by its `Coop-Task: <task-id>` trailer (or the task id), NOT its SHA — coop re-signs your commit on the host after this run, which rewrites its SHA, so a written-down SHA goes stale.",
		"AFTER the commit, refresh state.md one last time while the task is still in 10_in_progress/: preserve the useful Done so far and Traps, set Status to complete, and set Next action to none. Then move its folder into 99_done/ as the final filesystem action; write nothing more inside that task folder after the move. Coop also enforces those lifecycle fields host-side before review.",
		"If you hit a one-way-door decision, move its folder into 50_blocked/ and fill in its decision.md.",
		"If you SPOT a SEPARATE task while working (not part of this one), do NOT fold it into your commit: a simple, ready fix → create its folder in 00_todo/ with a task.md whose acceptance you can state in a line (a later iteration works it); a big one that needs a spec → create it under xx_backlog/ instead (the backlog is only for the big/not-yet-ready, never small stuff).",
		"Work exactly ONE task per run: take the assigned task to done — or to blocked — then STOP without claiming or starting another, even if 00_todo/ still has tasks. The loop re-invokes you in a fresh box with fresh context for the next one; draining the whole queue in a single run is the loop's job, not yours.",
	}, " ")
	return loopPeerCapabilities(agent, peers, p) + "\n\n" + fmt.Sprintf(instructions,
		filepath.Join(repo, "AGENTS.md"), absJoin(repo, queues), assignedID)
}

func loopPeerCapabilities(agent string, peers []agents.Target, p *preset.Preset) string {
	var consults, delegates []string
	if p != nil {
		for _, role := range append(p.Consults(), p.DegradedNativeRoles(agent)...) {
			consults = append(consults, role.Name)
		}
		for _, role := range p.Delegates() {
			delegates = append(delegates, role.Name)
		}
	} else {
		for _, peer := range peers {
			if peer.Provider != agent {
				consults = append(consults, peer.String())
			}
		}
	}
	if len(consults) == 0 && len(delegates) == 0 {
		return "Runtime capabilities: no peer wrappers are mounted. `coop-consult` and `coop-delegate` are unavailable; do not invoke or probe them."
	}
	parts := make([]string, 0, 2)
	if len(consults) > 0 {
		parts = append(parts, fmt.Sprintf("`coop-consult` is available for these configured read-only targets only: %s", strings.Join(consults, ", ")))
	} else {
		parts = append(parts, "`coop-consult` is unavailable")
	}
	if len(delegates) > 0 {
		parts = append(parts, fmt.Sprintf("`coop-delegate` is available for these configured write-capable roles only: %s", strings.Join(delegates, ", ")))
	} else {
		parts = append(parts, "`coop-delegate` is unavailable; do not invoke it")
	}
	return "Runtime capabilities: " + strings.Join(parts, ". ") + ". Do not assume any other peers or preset roles are mounted."
}

// defaultSignoffPrompt is the built-in signoff pass: a senior
// reviewer's bar over work done unattended overnight — per task under review it checks the goal is
// met, the repo's standards are followed, the failure path is tested, and the change is polished,
// then runs the repo's gate ONCE across the whole repo (not per task) — reopening anything short of
// "merge with no changes" but never fixing task code itself (the work loop does that next round).
// The tasks under review are the header loopSignoffPrompt prepends (what this run completed — NOT
// all of 99_done/, which persists until a human prunes it); the fixed context footer
// (reviewContextFooter) supplies the queue paths + the "coop isn't installed, move folders
// yourself" mechanics, so this text stays static and unit-testable.
const defaultSignoffPrompt = "Review pass — you are the SENIOR REVIEWER for work done unattended overnight. Make sure every shipped task is CORRECT, meets its stated goal, follows this repo's standards, and is genuinely polished — not merely \"the gate is green.\" You do NOT fix code or make commits: when something falls short you REOPEN the task with a SPECIFIC, actionable note, and the work loop fixes it next round. Be demanding — the bar is work you'd merge with no changes.\n" +
	"For EVERY task listed above (its folder is in 99_done/):\n" +
	"1. Meets its goal — read its task.md and the diff of its commit (git log/git show). Does the work satisfy EVERY acceptance criterion and cover every subtask? If any is unmet or a subtask was skipped, reopen it.\n" +
	"2. Follows the standards — it obeys AGENTS.md and every rule in .agent/rules, matches the surrounding code's style, and adds NO scope creep: no unrequested features or knobs, no unrelated refactors, no churn. Reopen violations.\n" +
	"3. Tested for real — it has tests that exercise the FAILURE/edge path, not just the happy path, and they actually cover the new behavior. Reopen thin or missing tests.\n" +
	"4. Polished — no debug prints, commented-out or dead code, leftover TODO/FIXME, or stray files; comments say why, not what; a user-visible change updated the docs/README/CHANGELOG. Reopen anything unpolished.\n" +
	"5. Bookkeeping — a commit implementing it exists in git log (find it by its Coop-Task: <task-id> trailer, NOT by any SHA the notes cite — coop re-signs commits on the host, so their SHAs change and a stale SHA in a note is EXPECTED, not a defect to reopen), a final state.md is present, and the queue is internally consistent (no id in two state dirs, no half-moved folder). Status must be complete and Next action must be none. If those lifecycle values are the ONLY defect, repair them in place in 99_done/ and continue the review; do NOT reopen implementation work for metadata alone. Task-local tmp/ was disposable and has been removed before this review; any evidence that needed to survive completion belongs in artifacts/.\n" +
	"Then ONCE across the WHOLE repo (not per task), run the repo's gate (per AGENTS.md). If it fails, reopen the responsible task(s) — the most-recently-done whose commit plausibly caused it — with the failure.\n" +
	"Reopen a task by MOVING its folder back to 10_in_progress/, writing in its log.md exactly what's wrong and what \"done\" requires, and refreshing state.md to a reopened Status plus one concrete Next action — and do it THE MOMENT you decide, before reviewing the next task: a review session can be cut at any turn boundary, and a verdict that exists only as prose is silently lost. Never batch reopens for the end, and never park verdicts behind background subagents you wait on — work still running when your turn ends dies with it. Change no task code; make no commits."

// loopSignoffPrompt is the end-of-loop signoff pass's prompt: a header naming the tasks under
// review (what this run completed since the last accepted round — the loop computes it as a folder
// diff, so the reviewer never re-derives its subjects from 99_done/, which persists until a human
// prunes it), then the built-in senior review, then the optional .agent/loop.yaml signoff.prompt
// APPEND (extra project checks — never a replacement), then a fixed context footer with the
// concrete queue paths and reopen mechanics.
func loopSignoffPrompt(repo string, queues []string, appendPrompt string, finished []string) string {
	var b strings.Builder
	b.WriteString("The task(s) this run completed since the last accepted review — the ONLY tasks to review this pass:\n")
	for _, f := range finished {
		b.WriteString("  - " + f + "\n")
	}
	b.WriteString("\n")
	b.WriteString(defaultSignoffPrompt)
	if s := strings.TrimSpace(appendPrompt); s != "" {
		b.WriteString("\n\nAlso apply these project-specific checks, reopening any task that fails one:\n" + s)
	}
	b.WriteString("\n\n")
	b.WriteString(reviewContextFooter(repo, queues))
	return b.String()
}

// reviewContextFooter is appended to every review prompt (override or default) so the mechanics
// never depend on the base text: the absolute in-box queue path(s), the AGENTS.md path, and the
// reminder that `coop` is NOT installed here — a task is reopened by MOVING its folder back to
// 10_in_progress/, not by running coop. It also carries the execute-immediately rule: a limit
// resume or failover restarts the agent process mid-review, killing background subagents and
// dropping any reopen decided but not yet written to the queue as a folder move.
func reviewContextFooter(repo string, queues []string) string {
	return fmt.Sprintf("Context: the task queue(s) are at %s and the project contract is %s. `coop` is NOT installed in this box. A stale Status/Next action in an otherwise complete done task is metadata-only: repair those fields in place and do not reopen it. For any substantive defect, reopen the task by MOVING its folder back to 10_in_progress/ yourself (do not run `coop`), note what was missing in log.md, and refresh state.md with a reopened Status and one concrete Next action. Execute every substantive reopen IMMEDIATELY as you decide it (move the folder, then write the note and state) — never batch reopens for the end and never leave them waiting on background work: an interrupted session loses any verdict not yet written to the queue.",
		absJoin(repo, queues), filepath.Join(repo, "AGENTS.md")) +
		" You are the authoritative review for this stage: do NOT invoke the review-board skill or spawn another review board. When evidence is missing, do focused read-only investigation yourself (inspect the code, tests, history, or run a targeted verifier)." +
		" When you are completely finished, end your reply with exactly one receipt line and nothing after it: `REVIEW COMPLETE — PASS — reopened: none` if every subject passed, or `REVIEW COMPLETE — FAIL — reopened: <id1>,<id2>` listing every task you moved, sorted by task ID with no spaces. The loop compares the verdict and exact IDs against the named review subjects and folders that actually moved; a missing, malformed, or mismatched receipt is re-run — never batch or defer a reopen past this line." +
		" GATE INTEGRITY: a task that changed a gate-defining file — the Makefile/gate, .agent/project.yaml, .agent/loop.yaml, .claude/hooks/, or CI — could be passing by WEAKENING its own checker (removing an assertion, relaxing the gate, disabling a hook). Scrutinize any such change and REOPEN the task if the gate was weakened rather than the code fixed; a green gate the candidate loosened is not a pass."
}

const auditEvidencePrompt = "Before the final receipt, write exactly one compact evidence line for EACH audit subject: `AUDIT EVIDENCE — <id> — gate: <test actually run, or not run with why> — findings: <unresolved findings, or none>`. Put those lines immediately before the receipt, one per task and with no duplicates. This preserves observations for final signoff; it does not decide acceptance."

// loopBetweenPrompt is the opt-in per-task audit run after each completed task. A header names
// the task(s) the last iteration moved to done — the audit's subject, computed at fire time so
// the between.prompt never has to make the agent GUESS "the most recent" from folder mtimes.
// Then the .agent/loop.yaml between.prompt (SET, not appended — between has no built-in;
// loopcfg.Load requires it when between.enabled), then the same fixed context footer with the
// queue paths and reopen mechanics. It reviews the just-completed task and may reopen it — the
// loop reworks it first.
func loopBetweenPrompt(repo string, queues []string, setPrompt string, finished, gateHits []string) string {
	var b strings.Builder
	if len(finished) > 0 {
		b.WriteString("The task(s) the last iteration just completed — the ONLY subject of this audit:\n")
		for _, f := range finished {
			b.WriteString("  - " + f + "\n")
		}
		b.WriteString("\n")
	}
	if len(gateHits) > 0 {
		b.WriteString("PROTECTED CHANGE: this iteration edited gate-defining file(s) — " + strings.Join(gateHits, ", ") +
			". Before anything else, verify the change did NOT weaken the checker (remove/relax an assertion, disable a hook, loosen the gate) to make the task pass; reopen it if it did.\n\n")
	}
	b.WriteString(strings.TrimSpace(setPrompt))
	b.WriteString("\n\n")
	b.WriteString(auditEvidencePrompt)
	b.WriteString("\n\n")
	b.WriteString(reviewContextFooter(repo, queues))
	return b.String()
}

const defaultProtectedBetweenPrompt = "Audit ONLY the protected gate change named above. Verify from the committed diff and an independent gate run that it preserves or strengthens enforcement rather than removing an assertion, disabling a hook, or relaxing what counts as green. Reopen the task with the concrete weakness if it does not pass that bar."

// betweenAuditSetPrompt keeps ordinary between-task review opt-in, while making a completed task's
// protected gate edit earn an immediate audit even when between.enabled is false. An unconfigured
// protected audit uses the signoff target (betweenRot's existing fallback) and this built-in prompt.
func betweenAuditSetPrompt(configured bool, setPrompt string, gateFiles []string) (string, bool) {
	if configured {
		return strings.TrimSpace(setPrompt), true
	}
	if len(gateFiles) == 0 {
		return "", false
	}
	return defaultProtectedBetweenPrompt, true
}

func shouldRunBetweenAudit(iterationSucceeded, auditAvailable, protected bool) bool {
	return protected || (iterationSucceeded && auditAvailable)
}

// doneTaskDirs maps every done task's id → its folder across the queue(s). The between audit
// diffs a before/after snapshot of it to name exactly which task(s) an iteration finished.
func doneTaskDirs(hosts []string) map[string]string {
	out := map[string]string{}
	for _, h := range hosts {
		for _, t := range readTaskTree(h) {
			if t.State == stateDone {
				out[t.ID] = t.Dir
			}
		}
	}
	return out
}

// newlyFinished returns "id — dir" lines (sorted by id) for tasks done now but not before —
// what the last iteration completed, and so what the between audit is about.
func newlyFinished(before, now map[string]string) []string {
	var out []string
	for id, dir := range now {
		if _, ok := before[id]; !ok {
			out = append(out, id+" — "+dir)
		}
	}
	slices.Sort(out)
	return out
}

// taskIDsOf strips the " — dir" suffix off newlyFinished lines — the bare ids, for the banner.
func taskIDsOf(finished []string) []string {
	out := make([]string, len(finished))
	for i, f := range finished {
		out[i], _, _ = strings.Cut(f, " — ")
	}
	return out
}

// defaultSignoffRounds is the built-in work→signoff round ceiling when .agent/loop.yaml
// signoff.rounds is unset.
const defaultSignoffRounds = 5

// signoffRounds is the work→signoff round ceiling: .agent/loop.yaml signoff.rounds when set (>0),
// else the built-in default of 5. signoffRoundCap scales it by the batch.
func signoffRounds(lc *loopcfg.Config) int {
	if lc.Signoff.Rounds > 0 {
		return lc.Signoff.Rounds
	}
	return defaultSignoffRounds
}

// blockReopenedTasks parks the exact tasks reopened by the capped signoff round into 50_blocked/
// with a decision.md; unrelated actionable work is left untouched, and the capped loop exits 3
// (blocked on a human) instead of spinning or claiming a false "done".
// The loop runs on the host, where coop's own task helpers are available, so it moves the folders
// directly. Best-effort: a move/write failure is surfaced and skipped, never fatal — the closing
// banner still reports the honest count.
func blockReopenedTasks(hosts, reopened []string, rounds int) {
	for _, host := range hosts {
		for _, t := range readTaskTree(host) {
			if !slices.Contains(reopened, t.ID) || (t.State != stateTodo && t.State != stateInProgress) {
				continue
			}
			if err := moveTaskDir(host, t, stateBlocked); err != nil {
				ui.Warn("could not block %s: %v", t.ID, err)
				continue
			}
			writeReviewBlockDecision(filepath.Join(host, stateBlocked, t.ID, "decision.md"), t.ID, t.Title, rounds)
		}
	}
}

// writeReviewBlockDecision drops a decision.md explaining that the review kept reopening this task
// past the round cap, so a human knows why it's parked — unless one already exists (don't clobber a
// prior note). Best-effort; mirrors the `coop tasks block` stub shape.
func writeReviewBlockDecision(path, id, title string, rounds int) {
	if fileExists(path) {
		return
	}
	body := fmt.Sprintf("# Decision: the review keeps reopening %q after %d rounds\n\n"+
		"**Blocks:** this task (`%s`).\n\n"+
		"**The decision:** The unattended loop drained the queue and the signoff pass reopened this "+
		"task %d times without it converging — the work loop can't get it to a state the review "+
		"accepts. A human needs to look at why (a gate it can't make green, a spec gap, a flaky test) "+
		"before it goes back in the queue.\n\n"+
		"**Recommendation:** Read the review's reopen notes in this task's log.md, fix the underlying "+
		"issue (or split/redefine the task), then `coop tasks unblock %s`.\n\n"+
		"---\n\n"+
		"**Resolution:** <!-- HUMAN: your answer here, then: coop tasks unblock %s -->\n",
		title, rounds, id, rounds, id, id)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		ui.Warn("could not write decision.md for %s: %v", id, err)
	}
}

// loopPreflightPrompt frames the CUSTOM pre-loop cleanup (loop.yaml preflight.prompt) — the
// built-in job, unblocking answered decisions, runs host-side in unblockResolved, so a box (and
// its tokens) spins up only for these extra instructions. The guardrails still bound them:
// cleanup only, no task work, no code, no commits (the queue files are git-ignored anyway).
func loopPreflightPrompt(repo string, queues []string, customPrompt string) string {
	return fmt.Sprintf("Pre-flight cleanup ONLY — do NOT work any task, write code, run the gate, or commit. The project contract is your instruction file, normally already loaded in your context — read %s only if its content is not. The queue(s) are at %s. `coop` is NOT installed in this box, so move task folders between the queue's state dirs ONLY as the cleanup instructions below direct — never start working a task's content. Change no code and make no commits.\n\nThe cleanup to do: %s",
		filepath.Join(repo, "AGENTS.md"), absJoin(repo, queues), strings.TrimSpace(customPrompt))
}

// absJoin renders queues (repo-relative) as a comma-separated list of absolute in-box paths.
func absJoin(repo string, queues []string) string {
	abs := make([]string, len(queues))
	for i, q := range queues {
		abs[i] = filepath.Join(repo, q)
	}
	return strings.Join(abs, ", ")
}

// loop works the .agent/tasks queue unattended until nothing actionable remains (todo/ and
// in_progress/ both empty), then (unless a custom work.command is set) runs a signoff pass over the
// results; if the review reopens anything, the loop drains and reviews again until a review reopens
// nothing (accepted) or the round cap (config.MaxReviewRounds) is hit, which blocks the stuck task
// for a human. A model rate/usage limit is not a failure: the loop waits for the
// reset — parsed from the agent's own output when possible — and retries, so a long run
// survives the limit. A task left in in_progress/ by an interrupted iteration is continued (the
// work prompt points the next agent at its uncommitted partial work), not stranded; a
// run that completes no task for maxStalls iterations stops rather than spinning.
// forkName is non-empty only for a detached fork loop — it labels each iteration's box so
// `coop fork stop` can tear the container down by label (see RunSpec.ForkName); the local
// `coop loop` passes "".
// watchInterrupt turns a stream of interrupt signals into the loop's two-stage stop: the first
// signal calls onSoft (finish the current iteration, then stop before the next), the second calls
// onHard (stop now). Pulled out of loop() so the escalation is unit-testable with a plain channel;
// it returns when the channel is closed (loop() stops delivery and closes it on exit).
func watchInterrupt(sig <-chan os.Signal, onSoft, onHard func()) {
	if _, ok := <-sig; !ok {
		return
	}
	onSoft()
	if _, ok := <-sig; !ok {
		return
	}
	onHard()
}

// loopInterruptInfo prints a stop notice. On the plain line-oriented path it starts on a fresh
// line, because an interactive terminal may echo Ctrl-C as literal ^C at the current cursor
// without advancing it — without the leading newline, coop's notice is glued to that echo (or to
// a partial agent line). While the loop's live bar is up, the region positions lines itself (and
// wipes the echo on its next repaint), and a raw newline would desync the region's cursor
// bookkeeping — so there the notice goes through ui alone.
func loopInterruptInfo(msg string) {
	if !ui.LiveActive() {
		fmt.Fprintln(os.Stderr)
	}
	ui.Info("%s", msg)
}

type loopTaskLimit struct {
	max       int
	settled   int
	currentID string
	lastID    string
	lastState string
}

func (l *loopTaskLimit) enabled() bool { return l.max > 0 }

func (l *loopTaskLimit) scope() string {
	if !l.enabled() {
		return ""
	}
	return l.currentID
}

func (l *loopTaskLimit) assign(id string) {
	if l.enabled() && l.currentID == "" {
		l.currentID = id
	}
}

// observe counts the selected task only after its post-iteration audit has left it done or blocked.
// A reopened task stays selected; reaching the limit retains the last task for the closing banner.
func (l *loopTaskLimit) observe(snapshot map[string]string) (bool, error) {
	if l.scope() == "" {
		return false, nil
	}
	state, ok := snapshot[l.currentID]
	if !ok {
		return false, fmt.Errorf("task-limited run lost task %s from the queue — inspect `coop tasks` before retrying", l.currentID)
	}
	if state != stateDone && state != stateBlocked {
		return false, nil
	}
	l.settled++
	l.lastID, l.lastState = l.currentID, state
	if l.settled >= l.max {
		return true, nil
	}
	l.currentID = ""
	return false, nil
}

// consult opts every iteration into the second-opinion directive: the box mounts the authed
// peers' credentials and the coop-consult wrapper, so an unattended lead can ask codex/gemini
// on hard calls — the orchestrator pattern running headless. Off by default: it widens the
// credential scope, so mounting peers into every loop box stays a deliberate choice.
func (a *app) loop(repo, img, agent, forkName string, rot *rotation, queues []string, sink io.Writer, peers []agents.Target, debugOnFail, preflight bool, maxTasks int) (int, error) {
	hosts := make([]string, len(queues)) // the queues' absolute host paths
	for i, q := range queues {
		hosts[i] = filepath.Join(repo, q)
	}
	// A queue is a directory (.agent/tasks), so check for one with isTaskDir — fileExists is
	// false for a directory and used to reject every folder queue, so the loop never ran.
	if !slices.ContainsFunc(hosts, isTaskDir) {
		return -1, fmt.Errorf("no task queue found (%s) — run 'coop init' or pass --tasks", strings.Join(queues, ", "))
	}
	// .agent/loop.yaml is the committed loop config (prompts, per-step models, settings). A bad file
	// fails the run here, before any box work. Absent → an empty config (all built-in defaults).
	lc, err := loopcfg.Load(repo)
	if err != nil {
		return 1, err
	}
	// loop.yaml `mcp: false` runs EVERY stage's box without the shared MCP config — the schemas
	// ride at the front of each model request, so a drain that doesn't need those tools shouldn't
	// pay for them each iteration. Sitting here (not cmdLoop) it covers fork loops too. Blanking
	// MCPFile is the one switch everything downstream keys off (Config.MCPActive); the loop owns
	// this process, so nothing else reads the config after it. Caveat: a verify: pass whose e2e
	// depends on MCP tooling needs mcp left on — repo-local e2e via bash is unaffected.
	if lc.MCPDisabled() {
		a.cfg.MCPFile = ""
	}
	custom := lc.Work.Command
	limit := loopTaskLimit{max: maxTasks}
	// A task-limited run with no actionable work is a pure host-side no-op: it does not need an
	// image and must not launch a configured preflight agent. Its built-in preflight may first
	// unblock answered decisions, since that is host-only and can make work actionable.
	preflightBuiltinRan := false
	if limit.enabled() && preflight && len(custom) == 0 {
		ui.Info("pre-flight: resolving answered blockers")
		if ids := unblockResolved(hosts); len(ids) > 0 {
			ui.Info("pre-flight: unblocked %s — resolution filled in", strings.Join(ids, ", "))
		}
		preflightBuiltinRan = true
	}
	if limit.enabled() {
		cf, _ := queueProgress(hosts)
		if cf.Todo+cf.Doing == 0 {
			fmt.Fprintln(os.Stderr, loopTaskLimitBanner(cf, limit))
			return loopExitCode(cf), nil
		}
	}
	if !box.ImageExists(a.rt, img) {
		return -1, fmt.Errorf("image %q not built — run 'coop build'", img)
	}
	// Iterations run Batch (box.Run stays quiet), so surface image staleness once here —
	// an overnight drain on a month-old box is exactly where a stale nudge earns its line.
	for _, nudge := range box.StalenessNudges(a.cfg, repo, img) {
		ui.Info("%s", nudge)
	}
	// Hold a sleep inhibitor for the whole run so an unattended overnight drain isn't stalled by
	// the machine idle-sleeping (caffeinate on macOS; see armKeepAwake). Released when loop returns.
	defer armKeepAwake(a.cfg)()
	// On a TTY every built-in provider streams JSON that coop decodes into the same live lines.
	// A custom work.command or non-terminal (pipe/CI/fork log) keeps plain text output. This is
	// decided per iteration because a cross-provider rotation can swap the active agent.
	tty := ui.IsTerminal(os.Stdout) && ui.IsTerminal(os.Stderr)
	// signoff.prompt APPENDS to the built-in senior review (it never replaces it).
	health := newLoopHealth() // per-task risk signals (reopens, gate edits, untagged) accumulated across the run
	audits := newAuditEvidenceStore()
	// The signoff pass (end-of-loop) and between-tasks audits both run only under the signoff-aware
	// agent form, not a custom work.command. Ordinary between review is opt-in; a completed task that
	// changed a protected gate path gets the narrow built-in audit even when it is off.
	betweenEnabled := len(custom) == 0 && lc.Between.Enabled
	// Per-stage signoff/between rotations from .agent/loop.yaml — each runs on its OWN configured
	// provider/model/effort/account and rotates its own fallback ladder on a limit (NOT a model name
	// pasted onto the work provider). An unset stage falls back: between → signoff → the work loop.
	signoffRot, err := a.reviewRotation(lc.Signoff.Agent, agent, rot)
	if err != nil {
		return 2, fmt.Errorf("signoff agent: %w", err)
	}
	betweenRot, err := a.reviewRotation(lc.Between.Agent, agent, signoffRot)
	if err != nil {
		return 2, fmt.Errorf("between agent: %w", err)
	}
	verifyEnabled := len(custom) == 0 && lc.Verify.Enabled
	verifyRot, err := a.reviewRotation(lc.Verify.Agent, agent, signoffRot) // unset → the signoff model
	if err != nil {
		return 2, fmt.Errorf("verify agent: %w", err)
	}
	// A per-run id keys this run's telemetry file (.agent/runs/<runid>.jsonl) — one JSON-Lines
	// record per stage, so the harness's own behavior (which target ran, reopen/retry counts) is
	// measurable. Best-effort throughout; a telemetry hiccup never touches the work.
	ridb := make([]byte, 8)
	_, _ = rand.Read(ridb)
	runid := hex.EncodeToString(ridb)
	a.runID = runid // boxes get it as COOP_RUN_ID so a consult peer can log its usage for the cost digest
	a.streamSeq, a.streamOff = 0, false
	// iterCmd builds one iteration's command: a raw work.command override if set,
	// otherwise the chosen agent's headless form carrying the work/signoff prompt.
	iterCmd := func(iterAgent, prompt string) ([]string, bool) {
		var cmd []string
		if len(custom) == 0 {
			cmd = a.agentLoopCmd(iterAgent, prompt)
		}
		return iterationCommand(iterAgent, cmd, custom, tty)
	}
	// Soft interrupt for any foreground loop that owns a terminal — a plain `coop loop` OR a
	// foreground `coop fork <name> --loop`: the first Ctrl-C finishes the current iteration then
	// stops before the next; a second stops now (tears the box down). A DETACHED fork worker has
	// no stdin tty (its stdin is /dev/null) and is stopped by `coop fork stop` (SIGTERM), so the
	// tty check below leaves it out — it keeps the plain, uninterruptible box and that SIGTERM
	// teardown is untouched. We watch SIGINT only. iterCtx stays nil otherwise.
	var softStop atomic.Bool
	wake := make(chan struct{}) // closed on the first Ctrl-C so any in-progress wait returns at once
	var iterCtx context.Context
	if ui.IsTerminal(os.Stdin) {
		ctx, cancel := context.WithCancel(context.Background())
		iterCtx = ctx
		defer cancel()
		sig := make(chan os.Signal, 2)
		signal.Notify(sig, os.Interrupt)
		defer func() { signal.Stop(sig); close(sig) }()
		go watchInterrupt(sig,
			func() {
				softStop.Store(true)
				close(wake)
				loopInterruptInfo("⏸ finishing this iteration, then stopping — Ctrl-C again to stop now")
			},
			func() {
				loopInterruptInfo("■ stopping now")
				cancel()
			})
	}

	// Pre-flight: one best-effort housekeeping pass before working the queue. The built-in job —
	// return every blocked task whose decision.md now has a filled-in Resolution to todo — is
	// mechanical, so the HOST does it directly: no box, no model, no tokens, and the same bar as
	// `coop tasks unblock` (decisionResolved), so preflight and the CLI never disagree. It works
	// no task and deletes nothing: done tasks are pruned only by a human (`coop tasks rm
	// --all-done`), never by an agent. Opt-in (preflight.enabled / --preflight); skipped under a
	// custom work.command (not the agent's headless form).
	if preflight && len(custom) == 0 {
		if !preflightBuiltinRan {
			ui.Info("pre-flight: resolving answered blockers")
			if ids := unblockResolved(hosts); len(ids) > 0 {
				ui.Info("pre-flight: unblocked %s — resolution filled in", strings.Join(ids, ", "))
			}
		}
		// An agent runs only for a CUSTOM cleanup (loop.yaml preflight.prompt) — extra instructions
		// that need judgment. Best-effort like the signoff pass — a failure never blocks work.
		if s := strings.TrimSpace(lc.Preflight.Prompt); s != "" {
			pfStart, pfHead := time.Now(), gitOut(repo, "rev-parse", "HEAD")
			pfCmd, streaming := iterCmd(agent, loopPreflightPrompt(repo, queues, s))
			pfCode, pfOut, _, pfErr := a.runIteration(iterCtx, repo, img, agent, forkName, pfCmd, streaming, hosts, false, nil, sink, peers, "preflight")
			a.recordStage(repo, runid, "preflight", rot.active(), pfStart, pfCode, 0, 0, pfHead, hosts, nil, nil, nil, nil)
			prev := rot.active()
			if wait, until, limited := rememberPreflightLimit(rot, pfCode, pfErr, pfOut, time.Now()); limited {
				if wait > 0 {
					ui.Info("all %d targets are rate limited after pre-flight — waiting for the soonest reset", len(rot.targets))
					sleepForLimit(wait, until, wake)
					rot.clearExpired(time.Now())
				} else {
					ui.Info("pre-flight target %q rate limited — starting work on %q", prev, rot.active())
				}
			}
		}
	}
	label := strings.Join(queues, ", ")
	c0, _ := queueProgress(hosts)
	stopHint := "Ctrl-C to stop"
	if limit.enabled() {
		stopHint = fmt.Sprintf("at most %s, then pause", ui.Count(limit.max, "task"))
	} else if iterCtx != nil {
		stopHint = "Ctrl-C to stop after this task, again to stop now"
	}
	if len(custom) == 0 {
		ui.Info("starting unattended loop on %s with %s — %d/%d done (%s)", label, agent, c0.Done, c0.total(), stopHint)
	} else {
		ui.Info("starting unattended loop on %s — %d/%d done (%s)", label, c0.Done, c0.total(), stopHint)
	}
	if rot.rotates() {
		ui.Info("rotating %d targets on rate limit: %s", len(rot.targets), strings.Join(rot.members(), ", "))
	}
	fails, waits, retries, completed, stalls := 0, 0, 0, 0, 0
	settledBaseline := c0.Done + c0.Blocked       // "settled" = tasks out of the actionable set (done OR blocked)
	prevHead := gitOut(repo, "rev-parse", "HEAD") // a commit between iterations is progress too (see below)
	loopStartHead := prevHead                     // for the end-of-run signing sweep (catches any straggler cycle)
	// The signoff reviews only what THIS RUN completed: anchoring to the pre-run done set keeps
	// 99_done/'s history (pruned only by a human) out of every round's subject list.
	reviewBaseline := doneTaskDirs(hosts)
	// Loop-until-accepted: drain the work queue, run the signoff pass, and if it reopened
	// anything, drain and sign off AGAIN — repeating until a signoff reopens nothing (accepted) or
	// the round cap is hit (block the stuck task for a human). The cap scales with the batch —
	// clamp(tasks worked/2, 3, signoff.rounds) — so a big overnight batch can't ping-pong one
	// stuck task forever while a tiny batch still gets a few tries (computed per round from the run's
	// completed count; the hard ceiling bounds it). A custom work.command has no signoff pass.
	for signoffRound := 1; ; signoffRound++ {
		for n := 1; ; {
			// A first Ctrl-C (soft stop) that arrived between iterations — or that woke a wait
			// below — stops here, before the next task is claimed; a second (hard) Ctrl-C that
			// canceled iterCtx during a between-tasks audit stops here too, before respawning a box.
			if softStop.Load() || (iterCtx != nil && iterCtx.Err() != nil) {
				break
			}
			reached, limitErr := limit.observe(queueSnapshot(hosts))
			if limitErr != nil {
				return 1, limitErr
			}
			if reached {
				break
			}
			// Point cfg at this iteration's target before leasing: the provider/target in metadata
			// identifies the owning controller, while flock remains the actual authority.
			agent = a.applyTarget(rot)
			target := rot.active()
			// Select and host-claim one authoritative task before the box starts. The returned task
			// drives both the banner and prompt, so the model cannot guess a different "next" task.
			assignment, assignErr := assignLoopTaskOnly(hosts, taskLeaseOwner{
				RunID: a.runID, PID: os.Getpid(), Provider: agent, Target: target.String(),
			}, limit.scope())
			if assignErr != nil {
				return 1, assignErr
			}
			if assignment.Outcome == assignmentUnavailable {
				// Foreign-held work is not a drained queue. Do not sign off a batch another live
				// controller still owns; its kernel lock will make the task adoptable on death.
				ui.Info("no task lease available — %s; stopping without signoff", assignment.Busy)
				return 0, nil
			}
			if assignment.Outcome == assignmentDrained {
				if limit.scope() != "" {
					continue // the selected task settled between scans; observe and count its final state
				}
				break
			}
			c, assigned, lease := assignment.Counts, assignment.Task, assignment.Lease
			limit.assign(assigned.Item.ID)
			if lease.legacy {
				ui.Info("adopting unleased in-progress task %s", assigned.Item.ID)
			}
			// Snapshot which tasks are done BEFORE the iteration, so the between audit can name
			// exactly what this iteration finished (the diff), not guess "the most recent".
			var doneBefore map[string]string
			if len(custom) == 0 {
				doneBefore = doneTaskDirs(hosts)
			}
			// The active profile is shown on the model line (streamjson) — don't repeat it on the banner.
			active := assigned.Item.Title
			ui.Info("%s · owned by %s", progressBanner(n, c, active), agent)
			// Informed resume: if an in_progress task already carries a landed Coop-Task commit (a crash
			// after commit before the folder-move, or a review reopen), prepend a line telling the agent
			// to disambiguate and act — instead of blindly redoing it. Empty prefix → prompt unchanged.
			work := loopWorkPrompt(repo, queues, assigned.Item.ID, agent, peers, a.preset)
			iterWork := work
			if pre := a.resumePrefixFor(repo, assigned.Item.ID); pre != "" {
				iterWork = pre + "\n\n" + work
			}
			snapBefore := queueSnapshot(hosts)
			iterStart, iterHead := time.Now(), gitOut(repo, "rev-parse", "HEAD")
			cmd, streaming := iterCmd(agent, iterWork)
			code, out, res, err := a.runIteration(iterCtx, repo, img, agent, forkName, cmd, streaming, hosts, false, nil, sink, peers, active)
			// The worker may have moved the task to done. Stop metadata updates and release the
			// inode lock BEFORE its tmp cleanup can remove lease.lock.
			if releaseErr := lease.release(); releaseErr != nil {
				return 1, fmt.Errorf("release task lease %s: %w", assigned.Item.ID, releaseErr)
			}
			// The worker moves its own folder in-box. Finalize every done task before a stop, retry,
			// between audit, or signoff can consume the archive; a prior failed write/cleanup is retried
			// here, not stranded (a per-iteration delta would miss it after a re-run).
			if cleanupErr := finalizeFinishedTasks(hosts); cleanupErr != nil {
				return 1, fmt.Errorf("%w — completion was not accepted; fix the obstruction and re-run `coop loop`", cleanupErr)
			}
			// A second Ctrl-C canceled iterCtx and tore the box down mid-iteration — stop now.
			if iterCtx != nil && iterCtx.Err() != nil {
				break
			}
			action, wait, resetAt := decideIteration(code, err, out, time.Now(), &fails, &waits, &retries)
			// Completion integrity is a hard boundary. Fresh work must bind inside this iteration's
			// commit range; only a no-HEAD-change folder repair may use reachable history. Reject and
			// restore before signing or review so neither can bless unbindable work.
			finished := finishedTasks(snapBefore, queueSnapshot(hosts))
			headAfter := gitOut(repo, "rev-parse", "HEAD")
			missing := untrailered(repo, iterHead, headAfter, finished)
			if len(missing) > 0 {
				restoreErr := restoreUnbindableCompletions(hosts, missing)
				return 1, unbindableCompletionError(missing, restoreErr)
			}
			// Verifier trust boundary (first step): a task that edited a gate-defining file could be
			// passing by WEAKENING its own checker. Detect it host-side and warn; the review footer and
			// the between-audit note tell the reviewer to scrutinize it (a hard dual-run gate is a follow-up).
			gateHits := protectedGateChanges(repo, iterHead, headAfter)
			if len(gateHits) > 0 {
				ui.Warn("this iteration edited gate-defining file(s) %s — the review must confirm the gate wasn't weakened to pass", strings.Join(gateHits, ", "))
			}
			health.noteIteration(finished, gateHits, missing) // for the signoff/verify context + the closing digest
			// Host signing rewrites commit SHAs. Do it before recording successful work so telemetry and
			// every reviewer name the final commits rather than the unsigned pre-rebase heads.
			if action == actContinue && wantsSigning() {
				if signed, serr := a.signUnpushed(repo, iterHead); serr != nil {
					ui.Warn("could not sign this cycle's commits: %v — left unsigned", serr)
				} else if signed > 0 {
					ui.Info("signed %s with your host key", ui.Count(signed, "commit"))
				}
				headAfter = gitOut(repo, "rev-parse", "HEAD")
			}
			a.recordStage(repo, runid, "work", rot.active(), iterStart, code, retries, 0, iterHead, hosts, finished, missing, gateHits, res)
			// Review a just-completed task now when a successful iteration has ordinary between
			// review configured OR its complete run-bound diff touched the gate. Protected completion
			// is checked even when the worker exited nonzero, so a retry cannot hand a changed checker
			// to the next task before the mandatory audit runs.
			if len(custom) == 0 {
				if finishedDirs := newlyFinished(doneBefore, doneTaskDirs(hosts)); len(finishedDirs) > 0 {
					finishedIDs := taskIDsOf(finishedDirs)
					stepChanges := loopChanges(repo, loopStartHead, headAfter).forTasks(finishedIDs)
					auditGateFiles := protectedGateFiles(append(stepChanges.gateFiles(), gateHits...))
					setPrompt, auditAvailable := betweenAuditSetPrompt(betweenEnabled, lc.Between.Prompt, auditGateFiles)
					protectedAudit := len(auditGateFiles) > 0
					runAudit := shouldRunBetweenAudit(action == actContinue, auditAvailable, protectedAudit)
					if runAudit {
						if protectedAudit && !betweenEnabled {
							ui.Info("protected-change audit — reviewing %s", strings.Join(finishedIDs, ", "))
						} else {
							ui.Info("between-tasks audit — reviewing %s", strings.Join(finishedIDs, ", "))
						}
						prompt := loopBetweenPrompt(repo, queues, substituteLoopVars(setPrompt, stepChanges, health), finishedDirs, auditGateFiles) + stepChanges.reviewBlock(health)
						// An ordinary configured audit preserves its historical warn-and-continue behavior.
						// A protected audit is mandatory: failure or a missing/mismatched receipt stops
						// before another task can trust the changed gate.
						btStart, btHead := time.Now(), gitOut(repo, "rev-parse", "HEAD")
						btSnap := queueSnapshot(hosts)
						btExit := 0
						stage := "between audit"
						if protectedAudit {
							stage = "protected audit"
						}
						btOut, btRes, rerr := a.runReview(iterCtx, repo, img, betweenRot, forkName, prompt, reviewActivity(stage, finishedIDs), iterCmd, hosts, lc.Between.Writes, sink, peers, wake)
						if rerr != nil {
							ui.Warn("between audit could not run for %s: %v — left unaudited", strings.Join(finishedIDs, ", "), rerr)
							btExit = 1
						}
						reopenedIDs := reopenedBySignoff(btSnap, queueSnapshot(hosts))
						a.recordStage(repo, runid, "between", betweenRot.active(), btStart, btExit, 0, len(reopenedIDs), btHead, hosts, nil, nil, auditGateFiles, btRes)
						interrupted := iterCtx != nil && iterCtx.Err() != nil
						if verdictErr := protectedAuditVerdict(protectedAudit, interrupted, rerr, btOut, reopenedIDs, finishedIDs); verdictErr != nil {
							return 1, fmt.Errorf("protected-change audit for %s: %w — stopped before another task could trust the changed gate; inspect the task and re-run `coop loop`", strings.Join(finishedIDs, ", "), verdictErr)
						}
						if rerr == nil && !interrupted {
							audits.capture(finishedIDs, reopenedIDs, protectedAudit, btOut)
							audits.drop(reopenedIDs)
						}
					}
				}
			}
			// A first Ctrl-C lets completion binding, host signing, and the mandatory between/protected
			// audit finish, then skips retries and the final signoff. The exit remains interrupted (130),
			// because an intentionally incomplete batch is not queue verification.
			if softStop.Load() {
				break
			}
			// --debug-on-fail: on a non-rate-limit failure, open an interactive box shell
			// (same repo/image) to inspect, then retry — instead of the auto-retry/stop.
			if (action == actRetry || action == actStop) && debugOnFail && ui.IsTerminal(os.Stdin) {
				ui.Info("iteration failed — opening a debug shell in the box (exit it to retry; Ctrl-C to stop)")
				a.debugShell(repo, img, agent)
				fails = 0 // the developer intervened; don't count this toward the stop cap
				continue
			}
			switch action {
			case actContinue:
				completed++
				n++
				// A clean iteration that neither finishes/blocks a task NOR commits means the agent keeps
				// continuing an in_progress task it can't complete — advanceStall bails after maxStalls
				// rather than loop forever (a commit or a block still counts as progress).
				var stop error
				prevHead, settledBaseline, stalls, stop = a.advanceStall(repo, hosts, prevHead, settledBaseline, stalls, active)
				if stop != nil {
					return code, stop
				}
			case actWait:
				// A rate/usage limit is expected on long runs. With more than one profile in
				// the pool, switch to another subscription and retry immediately; otherwise wait
				// for the reset. Either way the same iteration is retried, not burned.
				if rot.rotates() {
					// Advancing the rotation is the point — the loop head re-derives the agent
					// from rot (applyTarget), so the returned name would go unread here.
					a.rotateOnLimit(rot, resetAt, &waits, wake)
				} else {
					sleepForLimit(wait, resetAt, wake)
				}
			case actRetryNow:
				if wait > 0 {
					ui.Info("iteration reached model output limit (%d/%d) — resuming in %s", retries, maxOutputRetries, wait)
					sleepOrWake(wait, wake)
				} else {
					ui.Info("iteration reached model output limit — resuming immediately")
				}
			case actRetry:
				ui.Info("iteration failed (%d/%d) — retrying in 10s", fails, maxLoopFailures)
				sleepOrWake(10*time.Second, wake)
			case actStop:
				if waits > maxLimitWaits {
					return code, fmt.Errorf("still rate limited after %d waits — stopping", maxLimitWaits)
				}
				return code, fmt.Errorf("iteration failed %d times since the last success — stopping", fails)
			}
		}
		// A requested stop (soft: the current iteration finished; hard: it was torn down) skips the
		// signoff pass and the drain summary — the queue isn't done, the user asked to stop.
		if softStop.Load() || (iterCtx != nil && iterCtx.Err() != nil) {
			cf, _ := queueProgress(hosts)
			fmt.Fprintln(os.Stderr, loopInterruptedBanner(cf))
			return loopInterruptedExitCode, nil
		}
		if limit.enabled() {
			cf, _ := queueProgress(hosts)
			fmt.Fprintln(os.Stderr, loopTaskLimitBanner(cf, limit))
			if limit.settled == 0 {
				return loopExitCode(cf), nil
			}
			return 0, nil
		}
		// A custom work.command isn't the signoff-aware agent form, so it gets no signoff pass —
		// today's behavior: drain the queue, then report.
		if len(custom) > 0 {
			break
		}
		// Scale the cap to this run's batch (completed tasks), clamped to [3, signoff.rounds].
		maxSignoffRounds := signoffRoundCap(completed, signoffRounds(lc))
		// The round's subjects: what entered done/ since the last accepted round (for round 1, since
		// the run started) — a folder diff, so it also catches a completion with no commit. Nothing
		// new means nothing to review: skip the pass instead of burning a box on 99_done/'s history.
		subjects := newlyFinished(reviewBaseline, doneTaskDirs(hosts))
		if len(subjects) == 0 {
			ui.Info("signoff — nothing newly completed to review, skipping")
			break
		}
		ui.Info("queue empty — running signoff (round %d/%d)", signoffRound, maxSignoffRounds)
		// The signoff runs on signoff.agent's OWN target — a stronger, usually different-vendor model
		// reviews the work loop's output — and fails CLOSED: if it can't run after retries, stop loudly
		// rather than let "nothing reopened" read as an accepting signoff.
		soStart, soHead := time.Now(), gitOut(repo, "rev-parse", "HEAD")
		// Hand the signoff the run's change context (per task, bound by the Coop-Task trailer) + health,
		// so a prompt like "e2e the affected features" resolves against a concrete list. Rebuilt each
		// round because the range (loopStartHead..HEAD) grows as reopened work lands.
		soSnap := queueSnapshot(hosts)
		cs := loopChanges(repo, loopStartHead, soHead)
		signoff := loopSignoffPrompt(repo, queues, substituteLoopVars(lc.Signoff.Prompt, cs, health), subjects) + audits.signoffBlock(taskIDsOf(subjects)) + cs.reviewBlock(health)
		signoffOut, soRes, serr := a.runReview(iterCtx, repo, img, signoffRot, forkName, signoff, reviewActivity("signoff", taskIDsOf(subjects)), iterCmd, hosts, lc.Signoff.Writes, sink, peers, wake)
		if serr != nil {
			return 1, serr
		}
		// A stop that landed during the signoff pass is honored before the next round is decided.
		if softStop.Load() || (iterCtx != nil && iterCtx.Err() != nil) {
			cf, _ := queueProgress(hosts)
			fmt.Fprintln(os.Stderr, loopInterruptedBanner(cf))
			return loopInterruptedExitCode, nil
		}
		// Derive the review's exact done-to-actionable delta once. Other actionable tasks can exist
		// independently of this review and must not affect its telemetry, health, or outcome.
		reopenedIDs := reopenedBySignoff(soSnap, queueSnapshot(hosts))
		health.noteReopen(reopenedIDs)
		a.recordStage(repo, runid, "signoff", signoffRot.active(), soStart, 0, 0, len(reopenedIDs), soHead, hosts, nil, nil, nil, soRes)
		// Guard against a lost verdict (the 2026-07-10 incident): a signoff that DECIDES reopens as
		// prose but never moves the folders — its subagents interrupted, or it batched them past the
		// end — would leave the queue empty and read as "accepted". The review must end with a
		// structured receipt; if its ids disagree with the folders that actually moved (or the receipt
		// is missing entirely), the round is treated as interrupted and
		// re-run within the cap, or — at the cap — the loop exits loudly rather than claim a false done.
		receipt, ok := reviewReopenReceipt(signoffOut)
		if reopenVerdictLost(receipt, ok, reopenedIDs, taskIDsOf(subjects)) {
			if signoffRound >= maxSignoffRounds {
				return 3, fmt.Errorf("signoff verdict inconsistent after %d rounds: review reported %s but task delta was %s — verdicts may have been lost, a human should look", maxSignoffRounds, receiptClaim(receipt, ok), receiptIDs(reopenedIDs))
			}
			ui.Warn("signoff review inconsistent (reported %s, task delta %s) — re-running the round", receiptClaim(receipt, ok), receiptIDs(reopenedIDs))
			continue
		}
		audits.drop(reopenedIDs)
		// This round's verdict is consistent — re-anchor the baseline to the post-review done set, so
		// the next round reviews only what re-enters done/ (reworked reopens, new completions), never
		// a task this round just accepted. The lost-verdict path above deliberately keeps the old
		// baseline: an untrusted round's whole subject set is reviewed again.
		reviewBaseline = doneTaskDirs(hosts)
		switch signoffRoundOutcome(signoffRound, maxSignoffRounds, len(reopenedIDs) > 0) {
		case signoffContinue:
			ui.Info("signoff reopened %s — draining again", ui.Count(len(reopenedIDs), "task"))
			continue
		case signoffCapReached:
			// The work loop couldn't get these tasks to a state the signoff accepts within the cap —
			// park them for a human rather than spin or claim a false "done" (exit 3 via loopExitCode).
			ui.Info("signoff still reopening after %d rounds — blocking %s for a human", maxSignoffRounds, ui.Count(len(reopenedIDs), "task"))
			blockReopenedTasks(hosts, reopenedIDs, maxSignoffRounds)
		}
		// signoffAccepted (nothing reopened) or signoffCapReached (just blocked) → the loop is done.
		break
	}
	// Verify: an optional FINAL pass over the whole run's changes — its prompt (verify.prompt) says
	// what, typically "e2e-test the affected features". It runs after the signoff accepted the batch,
	// on its own model, with the run's change context injected; best-effort, and it may reopen a task
	// whose e2e it can't get to pass (surfaced in the closing digest + exit). Skipped on a custom
	// work.command or a requested stop.
	if verifyEnabled && !softStop.Load() && (iterCtx == nil || iterCtx.Err() == nil) {
		cs := loopChanges(repo, loopStartHead, gitOut(repo, "rev-parse", "HEAD"))
		if cs.empty() {
			ui.Info("verify pass — nothing changed this run, skipping")
		} else {
			ui.Info("verify pass — e2e the affected features (%s)", strings.Join(cs.subsystems, ", "))
			vPrompt := substituteLoopVars(lc.Verify.Prompt, cs, health) + cs.reviewBlock(health) + "\n\n" + reviewContextFooter(repo, queues)
			vStart, vHead := time.Now(), gitOut(repo, "rev-parse", "HEAD")
			vExit := 0
			verifyIDs := make([]string, 0, len(cs.tasks))
			for _, task := range cs.tasks {
				verifyIDs = append(verifyIDs, task.id)
			}
			verifyActivity := reviewActivity("verify", verifyIDs)
			if len(verifyIDs) == 0 {
				verifyActivity = "verify: unbound changes"
			}
			_, vRes, verr := a.runReview(iterCtx, repo, img, verifyRot, forkName, vPrompt, verifyActivity, iterCmd, hosts, lc.Verify.Writes, sink, peers, wake)
			if verr != nil {
				ui.Warn("verify pass could not run: %v — the affected features went un-e2e'd", verr)
				vExit = 1
			}
			a.recordStage(repo, runid, "verify", verifyRot.active(), vStart, vExit, 0, 0, vHead, hosts, nil, nil, nil, vRes)
		}
	}
	// End-of-run signing sweep: normally a no-op (per-cycle signing already covered each iteration),
	// but it catches any straggler — a commit from a previously interrupted run, or a preflight
	// commit — so the whole run's range is signed before you push. Best-effort.
	if wantsSigning() && len(custom) == 0 {
		if signed, serr := a.signUnpushed(repo, loopStartHead); serr != nil {
			ui.Warn("end-of-run signing sweep failed: %v — some commits may be unsigned (run `coop sign`)", serr)
		} else if signed > 0 {
			ui.Info("signed %s with your host key", ui.Count(signed, "commit"))
		}
	}
	cf, _ := queueProgress(hosts)
	// A human-facing digest above the verdict banner: what shipped (per task + areas), what's blocked,
	// and any task the run flagged — so you see what to review/e2e at a glance.
	if len(custom) == 0 {
		cost := costFromRecords(readStageRecords(repo, runid), readPeerRecords(repo, runid))
		if digest := loopChanges(repo, loopStartHead, gitOut(repo, "rev-parse", "HEAD")).humanDigest(health, blockedTaskIDs(hosts), cost); digest != "" {
			fmt.Fprintln(os.Stderr, digest)
		}
		// Done folders accumulate until a human prunes them (agents never delete) — and a big
		// 99_done/ taxes every future run: each iteration's box lists it, and it's the haystack a
		// crash-resume scan walks. Past a threshold, say so once, at close.
		if nudge := pruneNudge(cf.Done); nudge != "" {
			fmt.Fprintln(os.Stderr, nudge)
		}
	}
	fmt.Fprintln(os.Stderr, loopClosingBanner(cf, completed))
	return loopExitCode(cf), nil
}

// rememberPreflightLimit carries a failed custom pre-flight's provider limit into the work
// rotation. A successful pre-flight may legitimately discuss limits, and output exhaustion is
// resumable rather than a provider limit, so neither changes target selection.
func rememberPreflightLimit(r *rotation, code int, runErr error, out string, now time.Time) (wait time.Duration, until time.Time, limited bool) {
	if runErr == nil && code == 0 {
		return 0, time.Time{}, false
	}
	hint := detectLimit(out, now)
	if !hint.limited || hint.outputLimited {
		return 0, time.Time{}, false
	}
	wait, until = r.onLimit(hint.resetAt, 1, now)
	return wait, until, true
}

// doneNudgeThreshold is how many done task folders accumulate before the loop's close suggests
// pruning. Agents never delete tasks, so without a nudge the pile only grows.
const doneNudgeThreshold = 10

// pruneNudge is the one-line prune suggestion once done/ has accumulated past the threshold; ""
// below it. The command is named, never run — pruning destroys state, so it stays the human's call.
func pruneNudge(done int) string {
	if done < doneNudgeThreshold {
		return ""
	}
	return fmt.Sprintf("  %s accumulated in 99_done/ — after you review and push, prune with 'coop tasks rm --all-done'",
		ui.Count(done, "done task folder"))
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
	var c taskCounts
	if queues, qerr := taskQueues(a.cfg, repo, nil); qerr == nil {
		hosts := make([]string, len(queues))
		for i, q := range queues {
			hosts[i] = filepath.Join(repo, q)
		}
		c, _ = queueProgress(hosts)
	}
	// Fork activity from a dir listing + pidfiles — no git, so it stays prompt-cheap.
	names := forkNames(repo)
	looping := 0
	for _, n := range names {
		if forkRunningPid(repo, n) > 0 {
			looping++
		}
	}
	// One extra bounded git call, and only when you sign by default: is HEAD unsigned (a box commit
	// not yet signed)? A nudge to run `coop sign` before a protected remote rejects the push.
	signWarn := false
	if wantsSigning() {
		signWarn = headUnsigned(repo)
	}
	if line := promptLine(c, len(names), looping, signWarn); line != "" {
		fmt.Println(line)
	}
	return 0, nil
}

// promptLine builds coop prompt's compact status line from the counts: non-zero segments only,
// "·"-separated, returning "" when everything is idle so an embedding prompt shows nothing.
func promptLine(c taskCounts, forks, looping int, signWarn bool) string {
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

// advanceStall updates the loop's stall bookkeeping after a clean iteration and reports whether to
// stop. Progress is a task SETTLING (done or blocked) OR a new commit — a genuinely stuck loop keeps
// continuing an in_progress task it can't finish AND commits nothing, so after maxStalls such
// iterations it returns a stop error rather than looping forever. It returns the updated
// (prevHead, settledBaseline, stalls); a new commit resets the stall count and rebaselines.
func (a *app) advanceStall(repo string, hosts []string, prevHead string, settledBaseline, stalls int, active string) (string, int, int, error) {
	after, _ := queueProgress(hosts)
	settled := after.Done + after.Blocked
	head := gitOut(repo, "rev-parse", "HEAD")
	if head != "" && head != prevHead {
		return head, settled, 0, nil
	}
	newBase, newStalls, stop := progressStall(settled, settledBaseline, stalls)
	if stop {
		return prevHead, settledBaseline, stalls, fmt.Errorf("no task finished, blocked, or committed in %d iterations — stopping (stuck on %q?)", maxStalls, active)
	}
	return prevHead, newBase, newStalls, nil
}

// loopExitCode is the machine-readable companion to loopClosingBanner so cron/fleet/CI can branch on
// the loop's outcome without parsing stderr prose: 3 when the loop stopped with work blocked on a
// human decision and nothing else actionable (including a task the review kept reopening past the
// round cap), 0 otherwise — verified done. Failures (1) and usage errors (2) surface from their own
// call sites, not here.
func loopExitCode(cf taskCounts) int {
	if cf.Todo+cf.Doing == 0 && cf.Blocked > 0 {
		return 3
	}
	return 0
}

// loopClosingBanner picks the loop's final line from the post-review queue counts: reopened work
// (todo, or reopened into in_progress) and tasks blocked on a human decision are NOT "done", so only
// a truly drained queue earns the green "verified done". With loop-until-accepted the loop normally
// exits either accepted (nothing reopened) or with the stuck task blocked, but the reopened branch
// stays as a defensive fallback (e.g. a custom work.command run). Pure, so the outcomes are
// unit-tested without running the loop.
func loopClosingBanner(cf taskCounts, completed int) string {
	switch {
	case cf.Todo+cf.Doing > 0:
		return ui.Bold(ui.Yellow(fmt.Sprintf(
			"⚠ signoff reopened %s — run 'coop loop' to work them", ui.Count(cf.Todo+cf.Doing, "task"))))
	case cf.Blocked > 0:
		// Tasks parked in 50_blocked/ on a human decision are NOT done — don't report success.
		return ui.Bold(ui.Yellow(fmt.Sprintf(
			"stopped — %d/%d done, %d blocked on a decision; resolve them (coop tasks decisions), then re-run",
			cf.Done, cf.total(), cf.Blocked)))
	default:
		msg := fmt.Sprintf("✓ queue verified done — %d/%d", cf.Done, cf.total())
		if completed > 0 {
			msg += fmt.Sprintf(" in %d iterations", completed)
		}
		return ui.Bold(ui.Green(msg))
	}
}

const loopInterruptedExitCode = 130

func loopInterruptedBanner(cf taskCounts) string {
	return ui.Bold(ui.Yellow(fmt.Sprintf("■ interrupted before queue verification — %d/%d done; run 'coop loop' to resume", cf.Done, cf.total())))
}

func loopTaskLimitBanner(cf taskCounts, limit loopTaskLimit) string {
	if limit.settled == 0 {
		if cf.Blocked > 0 {
			return ui.Bold(ui.Yellow(fmt.Sprintf("■ task-limited run idle — no actionable task; %d blocked on a decision; no box started", cf.Blocked)))
		}
		return ui.Bold(ui.Green("✓ task-limited run idle — no actionable task; no box started"))
	}
	last := fmt.Sprintf("last: %s %s", limit.lastID, stateLabel(limit.lastState))
	if limit.settled >= limit.max {
		noun := "tasks"
		if limit.max == 1 {
			noun = "task"
		}
		msg := fmt.Sprintf("task limit reached — %d/%d %s settled (%s); paused before another task or final signoff", limit.settled, limit.max, noun, last)
		if limit.lastState == stateBlocked {
			return ui.Bold(ui.Yellow("■ " + msg))
		}
		return ui.Bold(ui.Green("✓ " + msg))
	}
	return ui.Bold(ui.Green(fmt.Sprintf("✓ task-limited run paused — %d/%d tasks settled (%s); no actionable task remains; final signoff not run", limit.settled, limit.max, last)))
}

// debugShell opens an interactive shell in the box against the same repo/image as the
// loop iteration, so --debug-on-fail can inspect the failed state. The box is disposable
// per iteration, so this is a fresh shell in the same context, not the failed container.
func (a *app) debugShell(repo, img, agent string) {
	_, _ = box.Run(a.cfg, a.rt, box.RunSpec{
		Image: img, Repo: repo, Cmd: []string{a.cfg.Shell}, Agent: agent,
		Homes: a.cfg.Homes, Network: a.cfg.Network, Cache: a.cfg.Cache,
	})
}

const progressPoll = 2 * time.Second // how often the live bar re-reads the queue while an iteration runs

// iterationCommand adds streaming flags only to coop's known headless forms on a TTY. Claude's
// existing form appends them after the prompt; the other CLIs require their trailing prompt token
// (or -p/value pair) to remain last.
func iterationCommand(agent string, cmd, custom []string, tty bool) ([]string, bool) {
	if len(custom) > 0 {
		return custom, false
	}
	adapter, ok := agents.Get(agent)
	if !ok {
		return cmd, false
	}
	stream := adapter.Stream()
	if !tty || stream.Format == agents.StreamNone || len(stream.Flags) == 0 {
		return cmd, false
	}
	return spliceBeforeTrailing(cmd, stream.Flags, stream.TrailingArgs), true
}

func spliceBeforeTrailing(cmd, insert []string, trailing int) []string {
	if len(insert) == 0 {
		return cmd
	}
	at := len(cmd) - trailing
	if at < 0 {
		at = 0
	}
	result := make([]string, 0, len(cmd)+len(insert))
	result = append(result, cmd[:at]...)
	result = append(result, insert...)
	return append(result, cmd[at:]...)
}

// runIteration runs one boxed command in batch mode, teeing its output to the terminal while
// capturing the tail so a rate-limit notice can be detected. hosts are the queue files the
// live bar watches task counts while its explicit activity remains fixed. On interactive terminals
// the agent's output is funneled into the scroll history above a sticky progress bar (a
// Docker-build-style live view). Non-terminal output goes straight to the destination unchanged.
func (a *app) runIteration(ctx context.Context, repo, img, agent, forkName string, cmd []string, streaming bool, hosts []string, repoReadOnly bool, repoWritablePaths []string, sink io.Writer, peers []agents.Target, activity string) (code int, output string, res *iterResult, err error) {
	tail := &tailWriter{max: 64 << 10}
	live := loopBarSupported(os.Getenv("TERM_PROGRAM"), ui.IsTerminal(os.Stdout), ui.IsTerminal(os.Stderr))

	termOut, termErr := io.Writer(os.Stdout), io.Writer(os.Stderr)
	var bar *loopBar
	var funnel *lineWriter
	if live {
		region := ui.NewRegion(os.Stderr, func() int { return ui.TermWidth(os.Stderr) })
		c0, _ := queueProgress(hosts)
		bar = newLoopBar(region, time.Now(), c0, activity)
		funnel = &lineWriter{fn: bar.history} // agent/loop lines scroll above the bar
		termOut, termErr = funnel, funnel
		// Route coop's own status lines (ui.Info etc. — from here AND box.Run's startup: "shadowed",
		// "starting sibling services") through the bar too, so they scroll above it instead of
		// overprinting it. Deferred clear restores plain stderr once the iteration's bar is gone.
		ui.SetLiveSink(bar.history)
		defer ui.SetLiveSink(nil)
	}

	outWs := []io.Writer{termOut}
	errWs := []io.Writer{termErr, tail}
	if sink != nil { // fork loops also capture to ../<repo>-forks/.coop/<name>.log
		outWs = append(outWs, sink)
		errWs = append(errWs, sink)
	}
	// A built-in loop command on a TTY emits its provider's streaming JSON. Decode it into human
	// activity lines, feeding only narration and terminal errors to the rate-limit tail.
	rawTrace, renderedTrace, closeTrace := a.iterationStreamTrace(repo, agent, streaming)
	defer closeTrace()
	if renderedTrace != nil {
		outWs = append(outWs, renderedTrace)
	}
	var stdoutW io.Writer
	var dec iterationStreamDecoder
	if streaming {
		dec = newIterationStreamDecoder(agent, io.MultiWriter(outWs...), tail, a.cfg.ActiveProfile(agent), box.Workdir(a.cfg, repo), a.cfg.ModelFor(agent))
	}
	if dec != nil {
		stdoutW = dec
		if rawTrace != nil {
			stdoutW = io.MultiWriter(rawTrace, dec)
		}
	} else {
		plainWs := append(outWs, tail)
		if rawTrace != nil {
			plainWs = append([]io.Writer{rawTrace}, plainWs...)
		}
		stdoutW = io.MultiWriter(plainWs...)
	}
	var stderrW io.Writer = io.MultiWriter(errWs...)
	var stderrFilter *stderrLineFilter
	switch dec.(type) {
	case *codexStreamDecoder:
		stderrFilter = newCodexStderrFilter(stderrW)
	case *geminiStreamDecoder:
		stderrFilter = newGeminiStderrFilter(stderrW)
	}
	if stderrFilter != nil {
		stderrW = stderrFilter
	}

	var wg sync.WaitGroup
	var stop chan struct{}
	if live {
		stop = make(chan struct{})
		wg.Add(1)
		go func() { defer wg.Done(); monitorProgress(hosts, stop, bar) }()
		if ui.SpinnerEnabled() {
			wg.Add(1)
			go func() { defer wg.Done(); spinLoop(bar, stop) }()
		}
	}
	// Named --peer peers make each iteration a consult lead: box.Run then mounts exactly
	// those peers' credentials, the coop-consult wrapper, and the second-opinion directive. A
	// preset does the same with ITS roles: the routing contract mounts via ConsultLead.
	lead := ""
	if len(peers) > 0 || a.preset != nil {
		lead = agent
	}
	code, err = box.Run(a.cfg, a.rt, box.RunSpec{
		Image: img, Repo: repo, Cmd: cmd, Agent: agent, Batch: true, ForkName: forkName, ConsultLead: lead, Peers: peers, Preset: a.preset, RunID: a.runID,
		RepoReadOnly: repoReadOnly, RepoWritablePaths: repoWritablePaths,
		Homes: a.cfg.Homes, Network: a.cfg.Network, Cache: a.cfg.Cache,
		Stdout: stdoutW,
		Stderr: stderrW,
		Ctx:    ctx,
	})
	if live {
		close(stop)
		wg.Wait() // no goroutine repaints the region after this, so the teardown below is clean
	}
	if dec != nil {
		dec.flush()                // before tail.String(): final events must reach the rate-limit tail
		res = dec.lastIterResult() // result cost/turns/tokens (nil if none landed), for telemetry
	}
	if stderrFilter != nil {
		if flushErr := stderrFilter.flush(); err == nil {
			err = flushErr
		}
	}
	if live {
		funnel.flush()
		bar.stop()
	}
	return code, tail.String(), res, err
}

// monitorProgress watches the queue while an iteration runs and pushes count changes into the live
// bar. The activity is owned by runIteration and cannot drift to another queue item when a task
// moves; only done/blocked/total counts are monitored.
func monitorProgress(hosts []string, stop <-chan struct{}, bar *loopBar) {
	t := time.NewTicker(progressPoll)
	defer t.Stop()
	last, _ := queueProgress(hosts) // the bar was built with this baseline
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			last = updateLoopBarCounts(hosts, last, bar)
		}
	}
}

func updateLoopBarCounts(hosts []string, last taskCounts, bar *loopBar) taskCounts {
	// c.total()==0 while we had a baseline is a torn read (a folder caught mid-move) — a
	// running loop always has tasks; keep the last good counts rather than blink to 0/0.
	c, _ := queueProgress(hosts)
	if c != last && (c.total() > 0 || last.total() == 0) {
		bar.setCounts(c)
		return c
	}
	return last
}

func reviewActivity(stage string, subjects []string) string {
	if len(subjects) == 0 {
		return stage
	}
	prefix := stage + ": "
	suffix := ""
	if len(subjects) > 1 {
		suffix = fmt.Sprintf(" +%d", len(subjects)-1)
	}
	budget := progressActivityWidth - len([]rune(prefix+suffix))
	if budget < 1 {
		return truncate(stage, progressActivityWidth)
	}
	return prefix + truncate(subjects[0], budget) + suffix
}
