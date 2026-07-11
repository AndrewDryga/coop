package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
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
	return box.Run(a.cfg, a.rt, box.RunSpec{
		Image: img, Repo: repo, Cmd: cmd, Agent: agent, ConsultLead: lead, Peers: peers, Preset: a.preset,
		Homes: a.cfg.Homes, Network: a.cfg.Network, Cache: a.cfg.Cache, Serve: true,
	})
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

// launchAgent runs a named agent: its autonomous default command, with any extra CLI
// args you pass appended — so `coop claude --continue` keeps coop's autonomy + MCP
// flags and just adds yours. The agents' autonomous flags are global, so this is safe
// even before subcommands (e.g. `coop codex resume --last`). coop's own --consult and
// --credential are stripped first so they aren't forwarded to the agent.
func (a *app) launchAgent(target string, args []string) (int, error) {
	// The head is a target: provider[:model][@account]. The model/account ride it —
	// --model/--credential are retired.
	t, err := agents.ParseTarget(target)
	if err != nil {
		return 2, err
	}
	tool := t.Provider
	consultVals, args, err := extractConsult(args)
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
	if err := retiredTargetFlagErr(args); err != nil {
		return 2, err
	}
	// `coop claude --preset frontier` loads the orchestration preset: its roles seed the run
	// (routing contract, role models/credentials, wrappers); `coop <agent>` names the lead
	// explicitly, so the preset's lead.agent never overrides the command's own.
	presetName, args, err := extractRunPreset(args)
	if err != nil {
		return 2, err
	}
	p, err := a.loadRunPreset(presetName)
	if err != nil {
		return 2, err
	}
	a.applyPreset(p, tool)
	if err := a.applyRunTarget(t); err != nil {
		return 2, err
	}
	a.nudgeIfUnauthed(tool)
	peers, err := a.resolvePeers("--consult", consultVals)
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

// extractBoolFlag pulls one of coop's own bool flags out of an agent's args (so it isn't
// forwarded to the agent CLI) and reports whether it was present. Everything after a `--`
// is the agent's own args and is passed through verbatim.
func extractBoolFlag(args []string, flag string) (found bool, rest []string) {
	for i, a := range args {
		if a == "--" {
			return found, append(rest, args[i:]...)
		}
		if a == flag {
			found = true
			continue
		}
		rest = append(rest, a)
	}
	return found, rest
}

// extractConsult pulls every --consult <target> (repeatable) out of a normal/loop/fork-loop
// run's args — each value is one peer the lead may consult read-only on hard calls (see
// box.RunSpec.Peers). The OLD boolean --consult (no value) is retired (v3-clean): a valueless
// occurrence returns the rewrite. `--`-aware.
func extractConsult(args []string) (consult, rest []string, err error) {
	return extractRepeatable(args, "--consult", "name each peer: --consult <agent> [--consult <agent> …]")
}

// extractPeer pulls every --peer <target> (repeatable) out of fusion's args; each value is one
// council peer. Fusion requires ≥1 (checked by the caller). `--`-aware.
func extractPeer(args []string) (peers, rest []string, err error) {
	return extractRepeatable(args, "--peer", "name each peer: --peer <agent> [--peer <agent> …]")
}

// extractRepeatable collects every `--flag <value>` occurrence (repeatable) out of args, in
// order, returning the values and the remaining args. A valueless occurrence (the old boolean
// spelling, or a typo) errors, pointing at the repeatable form. Stops at `--` — everything after
// is the agent's own, forwarded verbatim (so an agent's OWN --consult/--peer still reaches it).
func extractRepeatable(args []string, flag, tombstone string) (vals, rest []string, err error) {
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			return vals, append(rest, args[i:]...), nil
		}
		if args[i] == flag || strings.HasPrefix(args[i], flag+"=") {
			v, n, _, e := flagValue(args, i, flag)
			if e != nil {
				return nil, nil, fmt.Errorf("%s takes a value now — %s", flag, tombstone)
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

// migrateFlatVaults retires any legacy flat credential vault into the named-profile layout at
// startup: for each agent whose <ConfigDir>/<agent>/ dir predates profiles/, box.EnsureProfilesDir
// moves the login into profiles/default so every read path can assume named profiles. It runs once
// (from Main) before anything reads a profile, is idempotent — a no-op once profiles/ exists — and
// best-effort: a rare rename failure is surfaced as a warning and nothing is deleted, so the flat
// login stays put and the user can retry or simply log in again. Agents never used (no <agent>/ dir)
// are skipped, so this doesn't leave an empty profiles/ behind for them.
func migrateFlatVaults(cfg *config.Config) {
	for _, name := range agents.Names() {
		base := filepath.Join(cfg.ConfigDir, name)
		if fi, err := os.Stat(base); err != nil || !fi.IsDir() {
			continue // never used this agent — nothing to migrate
		}
		if err := box.EnsureProfilesDir(cfg, name); err != nil {
			ui.Warn("could not migrate %s credentials into the profile layout: %v — log in again if %s stops authenticating", name, err, name)
		}
	}
}

func (a *app) cmdLogin(args []string) (int, error) {
	// The account rides the target now (coop login claude@work); --credential is retired.
	if err := retiredTargetFlagErr(args); err != nil {
		return 2, err
	}
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

// extractRunValue is the shared extractor behind extractRunProfile/extractRunModel: it pulls
// one of coop's own value-bearing flags (any of the given spellings) out of run args,
// stopping at "--" (everything after is the agent's, forwarded verbatim).
func extractRunValue(args []string, flags ...string) (val string, rest []string, err error) {
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			return val, append(rest, args[i:]...), nil
		}
		matched := false
		for _, flag := range flags {
			if v, n, ok, e := flagValue(args, i, flag); ok {
				if e != nil {
					return "", nil, e
				}
				val = v
				i += n - 1
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		rest = append(rest, args[i])
	}
	return val, rest, nil
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
	// The ACP proxy is ALWAYS in the path (not only under --supervise): it's coop's control point for
	// the editor session — restart resilience, plus rewriting the session so coop owns the toolbar
	// (yolo, model default, the credential/preset selector). The OUTER process validates the args
	// (fail fast), then supervises; the INNER (COOP_ACP_INNER=1) runs the box. --supervise is now the
	// default — strip and ignore it so an existing editor config that still passes it keeps working.
	_, args = extractSupervise(args)
	inner := args // the args the supervisor re-execs as `coop acp <inner>`; the inner re-parses them
	consultVals, args, err := extractConsult(args)
	if err != nil {
		return 2, err
	}
	// Resolve the --consult peers HERE, before the outer/inner split — so an editor's
	// agent_servers entry with a bad peer (unknown/unauthed, or an @account) fails fast in the
	// OUTER process, not silently later inside the box.
	peers, err := a.resolvePeers("--consult", consultVals)
	if err != nil {
		return 2, err
	}
	// --model/--credential are retired on this surface too — pin the session in the positional
	// target instead, so an editor's agent_servers entry runs ["acp","claude:opus@work"].
	if err := retiredTargetFlagErr(args); err != nil {
		return 2, err
	}
	// --preset: routing + role wiring for the editor session; the preset's lead is the
	// default agent (or governor, under fusion) when none is named.
	presetName, args, err := extractRunPreset(args)
	if err != nil {
		return 2, err
	}
	// The positional target pins the session's agent, model, and account. Parsed BEFORE the
	// inner env-override block so a preset-rotation rung (COOP_ACP_LEAD_MODEL/_CRED) still wins
	// over the launch-time model/account. fusion is a keyword (a governor target follows), not
	// itself a provider, so it isn't parsed as a target.
	model, profile, effort := "", "", ""
	tool, toolSet := "", false // no implicit default; an empty tool falls to the required-provider error below
	governor := ""
	consumed := 0 // positional tokens accounted for (the agent, plus a governor under fusion)
	isFusion := len(args) > 0 && args[0] == "fusion"
	switch {
	case isFusion:
		consumed = 1
		governor, toolSet = "", false // named explicitly (or via a --preset lead) — no implicit default
		if len(args) > 1 {
			t, terr := agents.ParseTarget(args[1])
			if terr != nil {
				return 2, terr
			}
			governor, toolSet = t.Provider, true
			if terr := foldTarget(t, &model, &profile); terr != nil {
				return 2, terr
			}
			effort = t.Effort
			consumed = 2
		}
	case len(args) > 0:
		t, terr := agents.ParseTarget(args[0])
		if terr != nil {
			return 2, terr
		}
		tool, toolSet = t.Provider, true
		if terr := foldTarget(t, &model, &profile); terr != nil {
			return 2, terr
		}
		effort = t.Effort
		consumed = 1
	}
	// Reject leftover tokens rather than silently ignore them (loop/fork do the same) — the ACP
	// adapter takes no extra args, so `coop acp claude foo`/`--nope` is a mistake worth surfacing.
	if leftover := args[consumed:]; len(leftover) > 0 {
		return 2, fmt.Errorf("coop acp: unexpected argument %q (usage: coop acp [claude|codex|gemini|grok|fusion [governor]][:model][@account] [--preset <name>])", leftover[0])
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
			return 2, errors.New("coop acp fusion: name the governor — coop acp fusion <agent> (or --preset <name>, whose lead governs)")
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
	// toolbar rewrite + credential/preset selector) and re-execs `coop acp <inner>` (COOP_ACP_INNER
	// set) to run the box, the current selection carried in the env. The inner falls through to box.Run.
	if os.Getenv("COOP_ACP_INNER") == "" {
		repo, _ := box.ResolveRepo(a.cfg.RepoOverride)
		ctrlModel := model
		if ctrlModel == "" {
			ctrlModel = a.cfg.ModelFor(tool)
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
		ctrl := newACPControl(a.cfg, tool, ctrlModel, profile, repo, a.acpPresetNames(repo), serveURLs, isFusion)
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

func extractSupervise(args []string) (supervise bool, rest []string) {
	return extractBoolFlag(args, "--supervise")
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
	idbuf := make([]byte, 8)
	if _, err := rand.Read(idbuf); err != nil {
		return 1, err
	}
	superID := hex.EncodeToString(idbuf)

	factory := func(ctx context.Context) (*acpproxy.Child, error) {
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
		// A deterministic container identity (a per-generation --cidfile) so teardown can remove
		// the box even before its labels are queryable — closing the startup race where Stop fires
		// after `docker run` begins but before the container is labelled. docker writes the id to
		// this file the moment the container is created. A fresh dir per generation avoids the
		// "name already in use" hazard across swaps, and `--cidfile` requires the path not to exist.
		cidDir, cidPath := "", ""
		env := append(os.Environ(), "COOP_ACP_INNER=1", "COOP_ACP_SUPERVISOR="+superID)
		// The current selection (a credential, a preset, or a provider) resolves to ONE spawn
		// target — provider, model, account in the wire grammar — each respawn re-reads it, so a
		// switch via coop's selector (or a preset-ladder rotation, cross-provider included) lands
		// on the new identity. Block first if the target is still rate-limit cooling (the
		// wait-for-reset paths) — sits before replay, so the replay startup grace is unaffected.
		if t, psName, ok := ctrl.spawnTarget(); ok {
			if psName != "" {
				env = append(env, "COOP_ACP_PRESET="+psName)
				ctrl.waitForPresetRung(ctx)
			} else if acct := t.Account(); acct != "" {
				ctrl.waitForReset(ctx, acct)
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
		cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, os.Stderr
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
			// Remove ONLY this generation's box, by its deterministic cidfile id — works even
			// mid-startup, before labels exist; `rm -f` stops it too. Then kill the whole process
			// group (inner coop + its run client) and the pipes. Deliberately NO label sweep here:
			// every generation shares this supervisor's id, so a swap that Stops the dead child would
			// also kill the just-spawned next box — see the final sweep after acpproxy.Run.
			if cidPath != "" {
				if cid, rerr := os.ReadFile(cidPath); rerr == nil {
					a.rt.RemoveContainer(strings.TrimSpace(string(cid)))
				}
			}
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			inW.Close()
			outR.Close()
			if cidDir != "" {
				os.RemoveAll(cidDir)
			}
		}
		return &acpproxy.Child{In: inW, Out: outR, Stop: stop}, nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	err = acpproxy.Run(ctx, os.Stdin, os.Stdout, factory, ctrl.hooks())
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
	if err := retiredTargetFlagErr(args); err != nil {
		return 2, err
	}
	// The council is named EXPLICITLY with --peer (repeatable). --consult is the normal-run
	// spelling; on fusion it's a mistake worth naming rather than leaking to the governor's CLI.
	for _, x := range args {
		if x == "--" {
			break
		}
		if x == "--consult" || strings.HasPrefix(x, "--consult=") {
			return 2, errors.New("coop fusion names its council with --peer, not --consult (e.g. coop fusion claude --peer codex --peer gemini)")
		}
	}
	peerVals, args, err := extractPeer(args)
	if err != nil {
		return 2, err
	}
	peers, err := a.resolvePeers("--peer", peerVals)
	if err != nil {
		return 2, err
	}
	// --preset: the preset's lead is the default governor (an explicit one still wins), and
	// its role routing rides along with the council directive.
	presetName, args, err := extractRunPreset(args)
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
	// The governor target names the agent; its model + account fold into this run's one-off
	// selection (the peers keep their own).
	governor, model, profile, effort, rest, govSet, err := a.parseGovernor(args)
	if err != nil {
		return 2, err
	}
	governor = presetLeadAgent(p, governor, govSet)
	if governor == "" {
		return 2, errors.New("coop fusion: name the governor — coop fusion <agent> --peer <agent>… (or --preset <name>, whose lead governs)")
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
	return box.Run(a.cfg, a.rt, box.RunSpec{
		Image: img, Repo: repo, Cmd: cmd, Agent: governor, FusionGovernor: governor, Peers: peers, Preset: a.preset,
		Homes: a.cfg.Homes, Network: a.cfg.Network, Cache: a.cfg.Cache,
	})
}

// parseGovernor takes a leading target (provider[:model][@account]) as the governor (else
// COOP_FUSION_GOVERNOR); everything else passes through to the governor. explicit reports
// whether the command named one (so a --preset's lead only fills the default); model/profile
// carry the governor target's model + single account for the one-off selection.
func (a *app) parseGovernor(args []string) (governor, model, profile, effort string, rest []string, explicit bool, err error) {
	tookGov := false // no implicit default — the governor is named explicitly (or via a --preset lead)
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--":
			return governor, model, profile, effort, append(rest, args[i+1:]...), tookGov, nil // everything after passes through
		case !tookGov && len(rest) == 0 && isTargetHead(args[i]):
			// Only the FIRST leading target is the governor: `coop fusion claude:opus/high@work`
			// (matches `coop acp fusion …`); otherwise the default / COOP_FUSION_GOVERNOR. A second
			// agent token passes through to the governor (not silently swallowed as the governor).
			t, terr := agents.ParseTarget(args[i])
			if terr != nil {
				return governor, model, profile, effort, rest, tookGov, terr
			}
			governor, tookGov = t.Provider, true
			if terr := foldTarget(t, &model, &profile); terr != nil {
				return governor, model, profile, effort, rest, tookGov, terr
			}
			effort = t.Effort
		default:
			rest = append(rest, args[i])
		}
	}
	return governor, model, profile, effort, rest, tookGov, nil
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
		return -1, errors.New("no compose.agent.yml — run 'coop init --services postgres,redis' to scaffold one")
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
	// later as an unrelated "no compose.agent.yml" — `coop down` takes only -v/--volumes.
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
		return -1, errors.New("no compose.agent.yml here — nothing to bring down")
	}
	proj := box.ServicesProject(repo)
	cargs := []string{"compose", "-p", proj, "-f", file, "down"}
	if volumes {
		cargs = append(cargs, "--volumes")
	}
	return a.rt.Run(os.Stdin, os.Stdout, os.Stderr, cargs...)
}

func (a *app) cmdInit(args []string) (int, error) {
	stack := ""
	var services []string
	servicesSet := false
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
		// An unknown token is a typo — error before doing any scaffold work, rather than
		// silently ignoring it and acting as if a flag were never passed.
		return 2, unknownErr("init flag", args[i], []string{"--stack", "--services"})
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
	if err := scaffold.Init(repo, stack, langs); err != nil {
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
	if lf := legacyTasksFile(filepath.Join(repo, tasksRoot)); lf != "" {
		rel := lf
		if r, err := filepath.Rel(repo, lf); err == nil {
			rel = r
		}
		ui.Warn("a legacy %s is present — v3 uses a folder per task and did NOT migrate it; convert it with MIGRATING.md", rel)
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

// promptServices asks (on a tty) which sibling services to scaffold into compose.agent.yml.
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

// parseLoopArgs resolves `coop loop`'s leading target (provider[:model][@account,…]) and its
// boolean flags. Model + account come from the target — `--model`/`--credential` are retired;
// `--consult`/`--tasks`/`--preset` are pre-extracted by cmdLoop. hasTarget is false when no
// positional was given (a preset then supplies the lead).
func parseLoopArgs(args []string, def bool) (t agents.Target, hasTarget, debugOnFail, preflight bool, err error) {
	preflight = def
	if err = retiredTargetFlagErr(args); err != nil {
		return agents.Target{}, false, false, preflight, err
	}
	t, hasTarget, rest, err := takeHeadTarget(args)
	if err != nil {
		return agents.Target{}, false, false, preflight, err
	}
	for _, x := range rest {
		switch x {
		case "--debug-on-fail":
			debugOnFail = true
		case "--debug": // v3: renamed to --debug-on-fail
			note, _ := removedCommandNote("loop --debug")
			return t, hasTarget, debugOnFail, preflight, errors.New(note)
		case "--preflight":
			preflight = true
		case "--no-preflight":
			preflight = false
		default:
			return t, hasTarget, debugOnFail, preflight, fmt.Errorf("coop loop: unexpected argument %q (usage: coop loop [%s][:model][@account] [--tasks <path>] [--preset <name>] [--consult <agent>]… [--preflight|--no-preflight] [--debug-on-fail])", x, strings.Join(agents.Names(), "|"))
		}
	}
	return t, hasTarget, debugOnFail, preflight, nil
}

func (a *app) cmdLoop(args []string) (int, error) {
	if len(args) > 0 && args[0] == "pool" { // v3: the persistent pool is gone — rotation lives in a preset
		note, _ := removedCommandNote("loop pool")
		return 2, errors.New(note)
	}
	flags, rest, err := extractTasksFlags(args)
	if err != nil {
		return 2, err
	}
	presetName, rest, err := extractRunPreset(rest)
	if err != nil {
		return 2, err
	}
	consultVals, rest, err := extractConsult(rest)
	if err != nil {
		return 2, err
	}
	peers, err := a.resolvePeers("--consult", consultVals)
	if err != nil {
		return 2, err
	}
	t, hasTarget, debugOnFail, preflight, err := parseLoopArgs(rest, a.cfg.Preflight)
	if err != nil {
		return 2, err
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	// --preset: its lead agent is the default (a positional target still wins), its roles seed
	// the run, and its models ladder becomes the rotation (below the positional target).
	p, err := a.loadRunPreset(presetName)
	if err != nil {
		return 2, err
	}
	agent := presetLeadAgent(p, t.Provider, hasTarget)
	if agent == "" { // provider required — no positional target, and no preset supplied a lead
		return 2, noProviderErr("loop")
	}
	a.applyPreset(p, agent)
	a.applyLoopModel(agent) // COOP_LOOP_MODEL → the fallback tier (below a ladder target's model)
	queues, err := taskQueues(a.cfg, repo, flags)
	if err != nil {
		return 2, err
	}
	// The rotation ladder: the positional target (its model + account ladder) wins; else the
	// preset lead's ladder; else the default (agent model across all signed-in accounts).
	// expandLadder turns it into the concrete one-account rungs the loop cycles on limits.
	var ladder []agents.Target
	if hasTarget {
		ladder = []agents.Target{t}
	} else if p != nil && agent == p.LeadAgent {
		ladder = p.LeadLadder
	}
	rot, err := a.buildRotation(agent, ladder)
	if err != nil {
		return -1, err
	}
	img := box.ImageForRepo(repo, a.cfg.BaseImage, a.cfg.ImageOverride)
	return a.loop(repo, img, agent, "", rot, queues, nil, peers, debugOnFail, preflight) // local loop: no fork label
}

// applyLoopModel puts COOP_LOOP_MODEL in the fallback tier — the loop's standing default
// model, used when a rotation entry carries no model of its own (a bare `models: [work]`
// or the no-preset default). It sits below a ladder target's model and below an explicit
// --model, and above the account's mark. Shared by `coop loop` and the fork loops.
func (a *app) applyLoopModel(agent string) {
	model, effort := a.cfg.LoopModelEffort() // COOP_LOOP_MODEL is model[/effort] — one var, both axes
	if model != "" {
		a.cfg.SetFallbackModel(agent, model)
	}
	if effort != "" {
		a.cfg.SetFallbackEffort(agent, effort)
	}
}

// withReviewModel runs fn with agent's model swapped to COOP_REVIEW_MODEL — so the review pass (and
// the between-tasks audit) can run on a stronger model than the cheaper work loop — then restores
// the prior top-tier model so the next work round rotates as before. It uses the top model tier so
// the review model wins for these iterations regardless of the work loop's rotation target; unset →
// no swap (the loop's model reviews). fn builds AND runs the iteration, so the model is in effect
// when the box command is assembled (the adapters read cfg.ModelFor at build time).
func (a *app) withReviewModel(agent string, fn func()) {
	model, effort := a.cfg.ReviewModelEffort() // COOP_REVIEW_MODEL is model[/effort]
	if model == "" && effort == "" {
		fn()
		return
	}
	prevM, prevE := a.cfg.ActiveModel(agent), a.cfg.ActiveEffort(agent)
	if model != "" {
		a.cfg.SetActiveModel(agent, model)
	}
	if effort != "" { // COOP_REVIEW_MODEL's /effort lets the review pass run at a different effort
		a.cfg.SetActiveEffort(agent, effort)
	}
	defer func() {
		a.cfg.SetActiveModel(agent, prevM)
		a.cfg.SetActiveEffort(agent, prevE)
	}()
	fn()
}

// parseLoopArgs pulls the --model <m>, --consult, --debug-on-fail, and
// --preflight/--no-preflight flags out of `coop loop` args; what remains must be at most
// one agent name. preflight defaults to def (COOP_PREFLIGHT) and the flags override it.
// loopWorkPrompt and loopReviewPrompt name the queue dir(s) the iteration works as ABSOLUTE
// in-box paths (the box's working dir is repo, bind-mounted at its real path). A relative
// ".agent/tasks" resolves fine for claude/codex (cwd-relative), but gemini's read_file rejects
// a relative path — so the queues (and AGENTS.md) are named absolute for every agent. With
// several queues (a monorepo's per-component trees) they're all listed so the agent works the union.
func loopWorkPrompt(repo string, queues []string) string {
	return fmt.Sprintf("Read %s and the task queue(s) %s, then work the queue per the protocol. A task is a folder under a queue dir and its state is its directory (named with a sort prefix): 00_todo/ · 10_in_progress/ · 50_blocked/ · 99_done/. `coop` is NOT installed in this box, so you change a task's state by MOVING its folder between those dirs yourself — that move IS the state change; do not try to run `coop`. First, if a task is already in 10_in_progress/, a previous attempt was interrupted before it committed: read that task's state.md (its resume note — where it stopped, the next action, traps), then run `git status` and `git diff` to see its uncommitted work, and continue it (or discard the partial work with `git restore`/`git checkout` and redo it if off-track) until done. Otherwise pick the next task in 00_todo/ and claim it by moving its folder into 10_in_progress/. As you work, keep that task's state.md current — a small, overwritten snapshot of the status, what is done, the next action, and any traps — refreshed before each commit and before you pause; append your reasoning to its log.md. Read a file before you edit it — an edit to a file you haven't read is rejected and wastes a turn (don't survey with `cat` then edit). Do the work, run the gate, commit your work, then move its folder into 99_done/. If you hit a one-way-door decision, move its folder into 50_blocked/ and fill in its decision.md. Always update state.md as your final step, leaving it reflecting the finished state (do not blank it). Work exactly ONE task per run: take the task you claimed to done — or to blocked — then STOP without claiming or starting another, even if 00_todo/ still has tasks. The loop re-invokes you in a fresh box with fresh context for the next one; draining the whole queue in a single run is the loop's job, not yours.",
		filepath.Join(repo, "AGENTS.md"), absJoin(repo, queues))
}

// defaultReviewPrompt is the built-in review pass when .agent/loop/review.md is absent: a senior
// reviewer's bar over work done unattended overnight — per done task it checks the goal is met, the
// repo's standards are followed, the failure path is tested, and the change is polished, then runs
// the repo's gate ONCE across the whole repo (not per task) — reopening anything short of "merge with
// no changes" but never fixing task code itself (the work loop does that next round). The fixed
// context footer (reviewContextFooter) supplies the queue paths + the "coop isn't installed, move
// folders yourself" mechanics, so this text stays static and unit-testable.
const defaultReviewPrompt = "Review pass — you are the SENIOR REVIEWER for work done unattended overnight. Make sure every shipped task is CORRECT, meets its stated goal, follows this repo's standards, and is genuinely polished — not merely \"the gate is green.\" You do NOT fix code or make commits: when something falls short you REOPEN the task with a SPECIFIC, actionable note, and the work loop fixes it next round. Be demanding — the bar is work you'd merge with no changes.\n" +
	"For EVERY task folder in 99_done/:\n" +
	"1. Meets its goal — read its task.md and the diff of its commit (git log/git show). Does the work satisfy EVERY acceptance criterion and cover every subtask? If any is unmet or a subtask was skipped, reopen it.\n" +
	"2. Follows the standards — it obeys AGENTS.md and every rule in .agent/rules, matches the surrounding code's style, and adds NO scope creep: no unrequested features or knobs, no unrelated refactors, no churn. Reopen violations.\n" +
	"3. Tested for real — it has tests that exercise the FAILURE/edge path, not just the happy path, and they actually cover the new behavior. Reopen thin or missing tests.\n" +
	"4. Polished — no debug prints, commented-out or dead code, leftover TODO/FIXME, or stray files; comments say why, not what; a user-visible change updated the docs/README/CHANGELOG. Reopen anything unpolished.\n" +
	"5. Bookkeeping — a commit implementing it exists in git log, a final state.md is present, and the queue is internally consistent (no id in two state dirs, no half-moved folder).\n" +
	"Then ONCE across the WHOLE repo (not per task), run the repo's gate (per AGENTS.md). If it fails, reopen the responsible task(s) — the most-recently-done whose commit plausibly caused it — with the failure.\n" +
	"Reopen a task by MOVING its folder back to 10_in_progress/ and writing in its log.md exactly what's wrong and what \"done\" requires — and do it THE MOMENT you decide, before reviewing the next task: a review session can be cut at any turn boundary, and a verdict that exists only as prose is silently lost. Never batch reopens for the end, and never park verdicts behind background subagents you wait on — work still running when your turn ends dies with it. Change no task code; make no commits."

// loopReviewPrompt is the end-of-loop review pass's prompt: the built-in senior review, then the
// optional .agent/loop.yaml review.prompt APPEND (extra project checks — never a replacement),
// then a fixed context footer with the concrete queue paths and reopen mechanics.
func loopReviewPrompt(repo string, queues []string, appendPrompt string) string {
	p := defaultReviewPrompt
	if s := strings.TrimSpace(appendPrompt); s != "" {
		p += "\n\nAlso apply these project-specific checks, reopening any task that fails one:\n" + s
	}
	return p + "\n\n" + reviewContextFooter(repo, queues)
}

// reviewContextFooter is appended to every review prompt (override or default) so the mechanics
// never depend on the base text: the absolute in-box queue path(s), the AGENTS.md path, and the
// reminder that `coop` is NOT installed here — a task is reopened by MOVING its folder back to
// 10_in_progress/, not by running coop. It also carries the execute-immediately rule: a limit
// resume or failover restarts the agent process mid-review, killing background subagents and
// dropping any reopen decided but not yet written to the queue as a folder move.
func reviewContextFooter(repo string, queues []string) string {
	return fmt.Sprintf("Context: the task queue(s) are at %s and the project contract is %s. `coop` is NOT installed in this box — reopen a task by MOVING its folder back to 10_in_progress/ yourself (do not run `coop`), and note what was missing in its log.md. Execute every reopen IMMEDIATELY as you decide it (move the folder, then write the note) — never batch reopens for the end and never leave them waiting on background work: an interrupted session loses any verdict not yet written to the queue.",
		absJoin(repo, queues), filepath.Join(repo, "AGENTS.md"))
}

// loopFilesTombstone returns a one-time warning when any RETIRED loop config file still exists —
// .agent/loop/{review,audit,between}.md or the legacy .agent/audit.md. Those knobs moved into one
// .agent/loop.yaml (review.prompt / preflight.prompt APPEND, between.prompt SETS), and coop NO
// LONGER reads the old files. Empty when none linger. loop() surfaces it once so an unmigrated repo
// isn't silently ignored. Pure (returns the string), so it's unit-testable.
func loopFilesTombstone(repo string) string {
	var found []string
	for _, rel := range []string{".agent/loop/review.md", ".agent/loop/audit.md", ".agent/loop/between.md", ".agent/audit.md"} {
		if fileExists(filepath.Join(repo, filepath.FromSlash(rel))) {
			found = append(found, rel)
		}
	}
	if len(found) == 0 {
		return ""
	}
	return "found retired loop config file(s) " + strings.Join(found, ", ") + " — loop settings now live in one .agent/loop.yaml (review.prompt/preflight.prompt append the built-ins; between.prompt sets the audit) and the old files are NO LONGER read; fold them into .agent/loop.yaml, then delete them"
}

// loopBetweenPrompt is the opt-in per-task audit run after each completed task. Its base is the
// .agent/loop.yaml between.prompt (SET, not appended — between has no built-in; loopcfg.Load
// requires it when between.enabled), then the same fixed context footer with the queue paths and
// reopen mechanics. It reviews the just-completed task and may reopen it — the loop reworks it first.
func loopBetweenPrompt(repo string, queues []string, setPrompt string) string {
	return strings.TrimSpace(setPrompt) + "\n\n" + reviewContextFooter(repo, queues)
}

// reviewRounds is the work→review round ceiling: .agent/loop.yaml review.rounds when set (>0),
// else the built-in default (config.MaxReviewRounds, which defaults to 5). reviewRoundCap scales
// it by the batch.
func reviewRounds(lc *loopcfg.Config, cfg *config.Config) int {
	if lc.Review.Rounds > 0 {
		return lc.Review.Rounds
	}
	return cfg.MaxReviewRounds
}

// blockReopenedTasks parks every task still reopened after the review round cap (anything left in
// todo/ or in_progress/ once the work loop drained the queue) into 50_blocked/ with a decision.md,
// so the capped loop exits 3 (blocked on a human) instead of spinning or claiming a false "done".
// The loop runs on the host, where coop's own task helpers are available, so it moves the folders
// directly. Best-effort: a move/write failure is surfaced and skipped, never fatal — the closing
// banner still reports the honest count.
func blockReopenedTasks(hosts []string, rounds int) {
	for _, host := range hosts {
		for _, t := range readTaskTree(host) {
			if t.State != stateTodo && t.State != stateInProgress {
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
		"**The decision:** The unattended loop drained the queue and the review pass reopened this "+
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

// loopPreflightPrompt is the one-shot cleanup pass run before the work loop when preflight is on:
// it resolves answered blockers, but works no task and changes no code (these files are git-ignored,
// so nothing is committed). The .agent/loop.yaml preflight.prompt APPENDS to this built-in.
func loopPreflightPrompt(repo string, queues []string, appendPrompt string) string {
	p := fmt.Sprintf("Pre-flight cleanup ONLY — do NOT work any task, write code, run the gate, or commit. Read %s and the queue(s) %s. `coop` is NOT installed in this box, so act by moving task folders yourself. Then, for each task in a 50_blocked/ dir, if its decision.md now has a filled-in Resolution, unblock it by moving its folder to 00_todo/. Leave every 00_todo/ and 10_in_progress/ task untouched; change no code and make no commits.",
		filepath.Join(repo, "AGENTS.md"), absJoin(repo, queues))
	if s := strings.TrimSpace(appendPrompt); s != "" {
		p += "\n\nAlso, as part of the cleanup: " + s
	}
	return p
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
// in_progress/ both empty), then (unless a custom COOP_LOOP_CMD is set) runs a review pass over the
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

// consult opts every iteration into the second-opinion directive: the box mounts the authed
// peers' credentials and the coop-consult wrapper, so an unattended lead can ask codex/gemini
// on hard calls — the orchestrator pattern running headless. Off by default: it widens the
// credential scope, so mounting peers into every loop box stays a deliberate choice.
func (a *app) loop(repo, img, agent, forkName string, rot *rotation, queues []string, sink io.Writer, peers []agents.Target, debugOnFail, preflight bool) (int, error) {
	hosts := make([]string, len(queues)) // the queues' absolute host paths
	for i, q := range queues {
		hosts[i] = filepath.Join(repo, q)
	}
	// A queue is a directory (.agent/tasks), so check for one with isTaskDir — fileExists is
	// false for a directory and used to reject every folder queue, so the loop never ran.
	if !slices.ContainsFunc(hosts, isTaskDir) {
		if len(hosts) > 0 {
			if lf := legacyTasksFile(hosts[0]); lf != "" {
				return -1, legacyMigrateErr(repo, lf, queues[0])
			}
		}
		return -1, fmt.Errorf("no task queue found (%s) — run 'coop init' or pass --tasks", strings.Join(queues, ", "))
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
	// .agent/loop.yaml is the committed loop config (prompts, per-step models, settings). A bad file
	// fails the run here, before any box work. Absent → an empty config (all built-in defaults).
	lc, err := loopcfg.Load(repo)
	if err != nil {
		return 1, err
	}
	custom := lc.Work.Command
	if len(custom) == 0 {
		custom = a.cfg.LoopCmd // TODO(loop.yaml): COOP_LOOP_CMD is retired next increment; loop.yaml work.command wins
	}
	// Claude on a TTY streams its activity as JSON we decode into live lines; other agents, a
	// custom work.command, or a non-terminal (pipe/CI/fork log) keep plain text output. Decided
	// per iteration in iterCmd (a cross-provider rotation can swap the active agent), keyed off the
	// stream-json marker runIteration finds in the command.
	tty := ui.IsTerminal(os.Stdout) && ui.IsTerminal(os.Stderr)
	// review.prompt APPENDS to the built-in senior review (it never replaces it).
	work, review := loopWorkPrompt(repo, queues), loopReviewPrompt(repo, queues, lc.Review.Prompt)
	// The review pass (end-of-loop) and the optional between-tasks audit both run only under the
	// review-aware agent form, not a custom work.command. The between audit is opt-in
	// (between.enabled + between.prompt); its prompt SETS the audit (between has no built-in).
	betweenEnabled := len(custom) == 0 && lc.Between.Enabled
	betweenPrompt := ""
	if betweenEnabled {
		betweenPrompt = loopBetweenPrompt(repo, queues, lc.Between.Prompt)
	}
	// Tombstone the retired .agent/loop/*.md (and legacy .agent/audit.md) once: those knobs moved
	// into .agent/loop.yaml and coop no longer reads the old files — don't silently ignore them.
	if len(custom) == 0 {
		if note := loopFilesTombstone(repo); note != "" {
			ui.Warn("%s", note)
		}
	}
	// iterCmd builds one iteration's command: a raw COOP_LOOP_CMD override if set,
	// otherwise the chosen agent's headless form carrying the work/review prompt.
	iterCmd := func(prompt string) []string {
		if len(custom) > 0 {
			return custom
		}
		cmd := a.agentLoopCmd(agent, prompt)
		if agent == "claude" && tty { // only claude streams JSON; recomputed per iteration (agent may rotate)
			cmd = append(cmd, "--output-format", "stream-json", "--verbose")
		}
		return cmd
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

	// Pre-flight: one best-effort housekeeping pass before working the queue — unblock any
	// task whose decision.md now has a filled-in Resolution. It works no task and deletes
	// nothing: done tasks are pruned only by a human (`coop tasks rm --all-done`), never
	// by an agent. Opt-in (--preflight / COOP_PREFLIGHT); skipped under a custom COOP_LOOP_CMD
	// (not the agent's headless form). Best-effort like the review pass — a failure never blocks work.
	if preflight && len(custom) == 0 {
		ui.Info("pre-flight: resolving answered blockers")
		_, _, _ = a.runIteration(iterCtx, repo, img, agent, forkName, iterCmd(loopPreflightPrompt(repo, queues, lc.Preflight.Prompt)), hosts, sink, peers)
	}
	label := strings.Join(queues, ", ")
	c0, _ := queueProgress(hosts)
	stopHint := "Ctrl-C to stop"
	if iterCtx != nil {
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
	// Loop-until-accepted: drain the work queue, run the review pass, and if the review reopened
	// anything, drain and review AGAIN — repeating until a review reopens nothing (accepted) or the
	// round cap is hit (block the stuck task for a human). The cap scales with the batch —
	// clamp(tasks worked/2, 3, COOP_MAX_REVIEW_ROUNDS) — so a big overnight batch can't ping-pong one
	// stuck task forever while a tiny batch still gets a few tries (computed per round from the run's
	// completed count; the hard ceiling bounds it). A custom COOP_LOOP_CMD has no review pass.
	for reviewRound := 1; ; reviewRound++ {
		for n := 1; ; {
			// A first Ctrl-C (soft stop) that arrived between iterations — or that woke a wait
			// below — stops here, before the next task is claimed; a second (hard) Ctrl-C that
			// canceled iterCtx during a between-tasks audit stops here too, before respawning a box.
			if softStop.Load() || (iterCtx != nil && iterCtx.Err() != nil) {
				break
			}
			// Surface queue progress + the task being worked, so a long run shows movement
			// instead of a bare counter (the same queueProgress `coop tasks` uses).
			c, active := queueProgress(hosts)
			// Keep going while anything is actionable — a todo/ task or an in_progress/ one an
			// interrupted iteration left mid-task. Stop only when both are empty (the rest is
			// done/ or blocked/), so a task in_progress when the box died is continued, not stranded.
			if c.Todo+c.Doing == 0 {
				break
			}
			// Run this iteration on the pool's active target — its provider (a cross-provider ladder
			// swaps the agent per rung), its credential (the mount + the agent command both resolve
			// cfg.AgentDir), and its model. applyTarget returns the active provider for THIS iteration.
			agent = a.applyTarget(rot)
			// The active profile is shown on the model line (streamjson) — don't repeat it on the banner.
			ui.Info("%s", progressBanner(n, c, active))
			code, out, err := a.runIteration(iterCtx, repo, img, agent, forkName, iterCmd(work), hosts, sink, peers)
			// A second Ctrl-C canceled iterCtx and tore the box down mid-iteration — stop now.
			if iterCtx != nil && iterCtx.Err() != nil {
				break
			}
			// A first Ctrl-C during this iteration: it ran to completion, so stop before the next
			// (don't fall through to the retry/wait accounting).
			if softStop.Load() {
				break
			}
			action, wait, resetAt := decideIteration(code, err, out, time.Now(), &fails, &waits, &retries)
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
				// Optional between-tasks audit: if this iteration moved a task to 99_done/ and
				// .agent/loop/between.md is present, review that just-completed task now (on the review
				// model). It may reopen it — the next inner iteration picks the reopened task back up and
				// reworks it before the loop moves on. Off (no extra iteration) unless the file exists.
				if betweenEnabled {
					if cNow, _ := queueProgress(hosts); cNow.Done > c.Done {
						ui.Info("between-tasks audit — reviewing the task just completed")
						a.withReviewModel(agent, func() {
							_, _, _ = a.runIteration(iterCtx, repo, img, agent, forkName, iterCmd(betweenPrompt), hosts, sink, peers)
						})
					}
				}
			case actWait:
				// A rate/usage limit is expected on long runs. With more than one profile in
				// the pool, switch to another subscription and retry immediately; otherwise wait
				// for the reset. Either way the same iteration is retried, not burned.
				if rot.rotates() {
					agent = a.rotateOnLimit(rot, resetAt, &waits, wake) // may swap the agent (cross-provider)
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
		// review pass and the drain summary — the queue isn't done, the user asked to stop.
		if softStop.Load() || (iterCtx != nil && iterCtx.Err() != nil) {
			cf, _ := queueProgress(hosts)
			fmt.Fprintln(os.Stderr, ui.Bold(ui.Yellow(fmt.Sprintf("■ stopped by request — %d/%d done", cf.Done, cf.total()))))
			return 0, nil
		}
		// A custom COOP_LOOP_CMD isn't the review-aware agent form, so it gets no review pass —
		// today's behavior: drain the queue, then report.
		if len(custom) > 0 {
			break
		}
		// Scale the cap to this run's batch (completed tasks), clamped to [3, COOP_MAX_REVIEW_ROUNDS].
		maxReviewRounds := reviewRoundCap(completed, reviewRounds(lc, a.cfg))
		ui.Info("queue empty — running review pass (round %d/%d)", reviewRound, maxReviewRounds)
		// The review pass runs on COOP_REVIEW_MODEL when set — a stronger model reviews the cheaper
		// work loop's output; unset → the loop's model. Restored after, so the next work round rotates as before.
		a.withReviewModel(agent, func() {
			_, _, _ = a.runIteration(iterCtx, repo, img, agent, forkName, iterCmd(review), hosts, sink, peers)
		})
		// A stop that landed during the review pass is honored before the next round is decided.
		if softStop.Load() || (iterCtx != nil && iterCtx.Err() != nil) {
			cf, _ := queueProgress(hosts)
			fmt.Fprintln(os.Stderr, ui.Bold(ui.Yellow(fmt.Sprintf("■ stopped by request — %d/%d done", cf.Done, cf.total()))))
			return 0, nil
		}
		// Re-read the queue AFTER the review: it may have reopened done tasks into 10_in_progress/.
		// The review runs only once the work loop drained the queue, so anything now actionable was
		// reopened just now — drain it again (loop-until-accepted), unless the round cap is hit.
		cf, _ := queueProgress(hosts)
		switch reviewRoundOutcome(reviewRound, maxReviewRounds, cf.Todo+cf.Doing > 0) {
		case reviewContinue:
			ui.Info("review reopened %s — draining again", ui.Count(cf.Todo+cf.Doing, "task"))
			continue
		case reviewCapReached:
			// The work loop couldn't get these tasks to a state the review accepts within the cap —
			// park them for a human rather than spin or claim a false "done" (exit 3 via loopExitCode).
			ui.Info("review still reopening after %d rounds — blocking %s for a human", maxReviewRounds, ui.Count(cf.Todo+cf.Doing, "task"))
			blockReopenedTasks(hosts, maxReviewRounds)
		}
		// reviewAccepted (nothing reopened) or reviewCapReached (just blocked) → the loop is done.
		break
	}
	cf, _ := queueProgress(hosts)
	fmt.Fprintln(os.Stderr, loopClosingBanner(cf, completed))
	return loopExitCode(cf), nil
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
	if line := promptLine(c, len(names), looping); line != "" {
		fmt.Println(line)
	}
	return 0, nil
}

// promptLine builds coop prompt's compact status line from the counts: non-zero segments only,
// "·"-separated, returning "" when everything is idle so an embedding prompt shows nothing.
func promptLine(c taskCounts, forks, looping int) string {
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
// stays as a defensive fallback (e.g. a custom COOP_LOOP_CMD run). Pure, so the outcomes are
// unit-tested without running the loop.
func loopClosingBanner(cf taskCounts, completed int) string {
	switch {
	case cf.Todo+cf.Doing > 0:
		return ui.Bold(ui.Yellow(fmt.Sprintf(
			"⚠ review reopened %s — run 'coop loop' to work them", ui.Count(cf.Todo+cf.Doing, "task"))))
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

// runIteration runs one boxed command in batch mode, teeing its output to the terminal while
// capturing the tail so a rate-limit notice can be detected. hosts are the queue files the
// live bar watches for task progress. On interactive terminals the agent's output is funneled
// into the scroll history above a sticky progress bar (a Docker-build-style live view).
// Non-terminal output goes straight to the destination unchanged.
func (a *app) runIteration(ctx context.Context, repo, img, agent, forkName string, cmd, hosts []string, sink io.Writer, peers []agents.Target) (code int, output string, err error) {
	tail := &tailWriter{max: 64 << 10}
	live := loopBarSupported(os.Getenv("TERM_PROGRAM"), ui.IsTerminal(os.Stdout), ui.IsTerminal(os.Stderr))

	termOut, termErr := io.Writer(os.Stdout), io.Writer(os.Stderr)
	var bar *loopBar
	var funnel *lineWriter
	if live {
		region := ui.NewRegion(os.Stderr, func() int { return ui.TermWidth(os.Stderr) })
		c0, a0 := queueProgress(hosts)
		bar = newLoopBar(region, time.Now(), c0, a0)
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
	// Claude's loop command (set by iterCmd on a TTY) emits stream-json; decode it into human
	// activity lines, feeding only the human text to tail so rate-limit detection still works.
	var stdoutW io.Writer
	var dec *streamDecoder
	if slices.Contains(cmd, "stream-json") {
		dec = newStreamDecoder(io.MultiWriter(outWs...), tail, agent, a.cfg.ActiveProfile(agent), box.Workdir(a.cfg, repo))
		stdoutW = dec
	} else {
		stdoutW = io.MultiWriter(append(outWs, tail)...)
	}

	var wg sync.WaitGroup
	var stop chan struct{}
	if live {
		stop = make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); monitorProgress(hosts, stop, bar) }()
		go func() { defer wg.Done(); spinLoop(bar, stop) }()
	}
	// Named --consult peers make each iteration a consult lead: box.Run then mounts exactly
	// those peers' credentials, the coop-consult wrapper, and the second-opinion directive. A
	// preset does the same with ITS roles: the routing contract mounts via ConsultLead.
	lead := ""
	if len(peers) > 0 || a.preset != nil {
		lead = agent
	}
	code, err = box.Run(a.cfg, a.rt, box.RunSpec{
		Image: img, Repo: repo, Cmd: cmd, Agent: agent, Batch: true, ForkName: forkName, ConsultLead: lead, Peers: peers, Preset: a.preset,
		Homes: a.cfg.Homes, Network: a.cfg.Network, Cache: a.cfg.Cache,
		Stdout: stdoutW,
		Stderr: io.MultiWriter(errWs...),
		Ctx:    ctx,
	})
	if live {
		close(stop)
		wg.Wait() // no goroutine repaints the region after this, so the teardown below is clean
	}
	if dec != nil {
		dec.flush() // before tail.String(): the last events must reach the rate-limit tail
	}
	if live {
		funnel.flush()
		bar.stop()
	}
	return code, tail.String(), err
}

// monitorProgress watches the queue while an iteration runs and pushes each task state change
// into the live bar — the agent moves task folders between state dirs as it works and the host
// sees those moves through the bind mount, so the bar's count and active task move live even
// while the agent's own output is still buffered. It returns when stop is closed.
func monitorProgress(hosts []string, stop <-chan struct{}, bar *loopBar) {
	t := time.NewTicker(progressPoll)
	defer t.Stop()
	last, _ := queueProgress(hosts) // the bar was built with this baseline
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			// c.total()==0 while we had a baseline is a torn read (a folder caught mid-move) — a
			// running loop always has tasks; keep the last good counts rather than blink to 0/0.
			if c, active := queueProgress(hosts); c != last && (c.total() > 0 || last.total() == 0) {
				bar.setProgress(c, active)
				last = c
			}
		}
	}
}
