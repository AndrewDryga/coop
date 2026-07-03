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
// being driven (claude/codex/gemini) so its credentials are mounted and, with consult,
// it gets the second-opinion directive plus its authenticated peers' credentials. Pass
// "" for raw commands (coop run/shell) that aren't an agent session — they mount no
// agent credentials.
func (a *app) runInBox(cmd []string, agent string, consult bool) (int, error) {
	repo, img, err := a.resolveImage()
	if err != nil {
		return -1, err
	}
	lead := ""
	if consult || (a.preset != nil && agent != "") {
		lead = agent // a preset makes the agent a lead too: its routing contract mounts via ConsultLead
	}
	return box.Run(a.cfg, a.rt, box.RunSpec{
		Image: img, Repo: repo, Cmd: cmd, Agent: agent, ConsultLead: lead, Preset: a.preset,
		Homes: a.cfg.Homes, Network: a.cfg.Network, Cache: a.cfg.Cache,
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
	return a.runInBox(args, "", false) // raw command runner — not an agent session
}

// launchAgent runs a named agent: its autonomous default command, with any extra CLI
// args you pass appended — so `coop claude --continue` keeps coop's autonomy + MCP
// flags and just adds yours. The agents' autonomous flags are global, so this is safe
// even before subcommands (e.g. `coop codex resume --last`). coop's own --consult and
// --profile are stripped first so they aren't forwarded to the agent.
func (a *app) launchAgent(tool string, args []string) (int, error) {
	consult, args := extractConsult(args)
	// `coop claude login` reads as "log in to claude", not "prompt claude with the
	// word login" — route it to the sign-in flow like `coop login claude` (honoring
	// `--profile`, e.g. `coop claude login --profile work`).
	if len(args) >= 1 && args[0] == "login" {
		profile, rest, err := extractProfile(args[1:])
		if err != nil {
			return 2, err
		}
		if len(rest) > 0 {
			return 2, fmt.Errorf("unexpected argument %q after 'coop %s login'", rest[0], tool)
		}
		return a.loginTo(tool, profile)
	}
	// `coop claude --credential work` runs on a chosen credential (one account/login;
	// --profile is the legacy spelling); coop consumes the flag so it isn't forwarded. It's
	// read only before a `--`, so an agent's own --profile (e.g. codex's config.toml
	// profile) is still reachable as `coop codex -- --profile <name>`.
	profile, args, err := extractRunProfile(args)
	if err != nil {
		return 2, err
	}
	// `coop claude --model opus` picks the model for this run, beating the profile/agent
	// defaults (see config.ModelFor). Consumed like --profile — read only before a `--` —
	// though forwarding it would usually work too, since the adapters skip appending a
	// second --model when one is already present.
	model, args, err := extractRunModel(args)
	if err != nil {
		return 2, err
	}
	// `coop claude --preset frontier` loads the orchestration preset: its roles seed the
	// run (routing contract, role models/credentials, wrappers); `coop <agent>` names the
	// lead explicitly, so the preset's lead.agent never overrides the command's own.
	presetName, args, err := extractRunPreset(args)
	if err != nil {
		return 2, err
	}
	p, err := a.loadRunPreset(presetName)
	if err != nil {
		return 2, err
	}
	a.applyPreset(p, tool)
	if err := a.applyOneOff(tool, model, profile); err != nil {
		return 2, err
	}
	a.nudgeIfUnauthed(tool)
	return a.runInBox(append(append([]string{}, a.defaultCmd(tool)...), dropDashDash(args)...), tool, consult)
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

// selectRunProfile points cfg at the credential profile chosen with --profile for a run of tool
// (a no-op when profile is ""). It requires the profile to already exist — a typo otherwise
// silently creates an empty husk dir (box.Run pre-creates the active profile), the very clutter
// `coop credentials rm` cleans up — and notes (without blocking) one that exists but isn't signed in.
// Shared by every agent-launch path: launchAgent, cmdFusion, cmdACP.
func (a *app) selectRunProfile(tool, profile string) error {
	if profile == "" {
		return nil
	}
	if !slices.Contains(a.cfg.Profiles(tool), profile) {
		return fmt.Errorf("%s has no credential %q — sign in first: coop login %s --credential %s", tool, profile, tool, profile)
	}
	if !box.ProfileAuthed(a.cfg, tool, profile) {
		ui.Info("note: %s credential %q isn't signed in — run: coop login %s --credential %s", tool, profile, tool, profile)
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

// applyOneOff applies a single run's --model/--credential to tool: --model may carry a
// model@account shortcut (the only pair spelling, matching a preset ladder entry), and
// --credential pins the account. Both empty is a no-op — the preset/default stands. It's
// the single-run analog of the loop's oneOffLadder; a bad shape (e.g. an account given in
// both --model's @ and --credential) errors.
func (a *app) applyOneOff(tool, model, credential string) error {
	ladder, err := oneOffLadder(model, credential)
	if err != nil {
		return err
	}
	if ladder == nil {
		return nil
	}
	t := ladder[0]
	if err := a.selectRunProfile(tool, t.Credential); err != nil {
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

// extractConsult opts a normal run into the second-opinion directive — letting the agent
// consult its authenticated peers read-only on hard calls (see box.RunSpec.ConsultLead).
func extractConsult(args []string) (consult bool, rest []string) {
	return extractBoolFlag(args, "--consult")
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
	profile, rest, err := extractProfile(args)
	if err != nil {
		return 2, err
	}
	// The agent is required — bare `coop login` must not silently default to one (it would open a
	// browser and block); name it explicitly, like the help shows. A stray extra arg is a typo,
	// not a second target, so reject it rather than silently ignore.
	if len(rest) == 0 {
		return 2, fmt.Errorf("usage: coop login <%s> [--credential <name>]", strings.Join(agents.Names(), "|"))
	}
	if len(rest) > 1 {
		return 2, fmt.Errorf("unexpected argument %q (usage: coop login <%s> [--credential <name>])", rest[1], strings.Join(agents.Names(), "|"))
	}
	return a.loginTo(rest[0], profile)
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

// retiredProfileFlagErr is the tombstone for the pre-v3 --profile spelling: v3 keeps one
// canonical flag, so the old name fails loudly with the rewrite instead of living as an alias.
func retiredProfileFlagErr() error {
	return errors.New("--profile was renamed to --credential in v3 — same value, new name (an agent's OWN --profile still passes through after a --)")
}

// rejectRetiredProfileFlag errors when the retired --profile spelling appears before a "--"
// (after it, the token belongs to the agent and passes through untouched). Without this the
// dead flag would silently forward INTO the agent CLI, which is worse than an alias.
func rejectRetiredProfileFlag(args []string) error {
	for _, x := range args {
		if x == "--" {
			return nil
		}
		if x == "--profile" || strings.HasPrefix(x, "--profile=") {
			return retiredProfileFlagErr()
		}
	}
	return nil
}

// extractProfile pulls coop's own `--credential <name>` (or `--credential=<name>`; the
// plural is an accepted spelling) out of login args, returning the chosen credential
// ("" if absent — the caller resolves the agent's MARKED default, not one literally
// named "default") and the remaining args. It lets a login target one of several stored
// accounts. A flag with no value is an error, not a silent fall-back.
func extractProfile(args []string) (profile string, rest []string, err error) {
	if err := rejectRetiredProfileFlag(args); err != nil {
		return "", nil, err
	}
	for i := 0; i < len(args); i++ {
		matched := false
		for _, flag := range []string{"--credential", "--credentials"} {
			if v, n, ok, e := flagValue(args, i, flag); ok {
				if e != nil {
					return "", nil, e
				}
				profile = v
				i += n - 1
				matched = true
				break
			}
		}
		if !matched {
			rest = append(rest, args[i])
		}
	}
	return profile, rest, nil
}

// extractRunProfile pulls coop's own --credential <name> (or --credential=<name>; the
// plural is an accepted spelling) out of an agent RUN's args, returning the chosen
// credential ("" if none) and the remaining args. It stops at a "--" separator and
// forwards everything after it verbatim — so an agent's own --profile is still reachable
// as `coop codex -- --profile <name>`; BEFORE the --, the retired --profile spelling
// errors with the rewrite. A flag with no value is an error, not a silent fall-back.
func extractRunProfile(args []string) (profile string, rest []string, err error) {
	if err := rejectRetiredProfileFlag(args); err != nil {
		return "", nil, err
	}
	return extractRunValue(args, "--credential", "--credentials")
}

// extractRunModel pulls coop's own --model <name> (or --model=<name>) out of an agent RUN's
// args, `--`-aware like extractRunProfile — so `coop codex -- --model x` still reaches codex's
// own flag untouched. A --model with no value is an error, not a silent no-op.
func extractRunModel(args []string) (model string, rest []string, err error) {
	return extractRunValue(args, "--model")
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
// to --profile can't traverse or collide outside the agent's profiles/ vault (no '/', '\', '..',
// '.', empty, or leading '-'). Login is the path that CREATES the dir from the name, so it's the
// gate; runs/select/rm/default already require an existing profile.
func validProfileName(name string) bool {
	if name == "" || name == "." || name == ".." || strings.HasPrefix(name, "-") {
		return false
	}
	return !strings.ContainsAny(name, "/\\")
}

// loginTo runs an agent's sign-in flow in the box; its token persists in the agent's
// config dir for the chosen profile. Shared by `coop login [agent] [--profile p]` and
// `coop <agent> login [--profile p]`.
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
	return a.runInBox(ag.Login(a.cfg), tool, false) // mounts only the agent being logged in to
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
func (a *app) cmdACP(args []string) (int, error) {
	// --supervise keeps the editor's connection alive across a container restart by
	// running the normal `coop acp` as a child and proxying ACP through acpproxy. The
	// COOP_ACP_INNER guard makes the child run the box directly (no recursion).
	supervise, args := extractSupervise(args)
	if supervise && os.Getenv("COOP_ACP_INNER") == "" {
		return a.cmdACPSupervise(args)
	}
	consult, args := extractConsult(args)
	// --profile pins this ACP session to one credential profile — so an editor can point a
	// "claude (work)" agent_servers entry at ["acp","claude","--credential","work"]. Read before the
	// tool token; an agent's own --profile still passes through after a `--`.
	profile, args, err := extractRunProfile(args)
	if err != nil {
		return 2, err
	}
	// --model pins the session's model the same way, so an editor entry can run e.g.
	// ["acp","claude","--model","opus"]. Applied before acpCommand builds the adapter
	// command, so gemini's (its own binary) carries the flag; claude's separate adapter
	// binary picks it up via ModelEnv in box.Run instead.
	model, args, err := extractRunModel(args)
	if err != nil {
		return 2, err
	}
	// --preset: routing + role wiring for the editor session; the preset's lead is the
	// default agent (or governor, under fusion) when none is named.
	presetName, args, err := extractRunPreset(args)
	if err != nil {
		return 2, err
	}
	p, err := a.loadRunPreset(presetName)
	if err != nil {
		return 2, err
	}
	tool, toolSet := agents.Default(), false
	consumed := 0 // positional tokens accounted for (the agent, plus a governor under fusion)
	if len(args) > 0 {
		tool, toolSet = args[0], true
		consumed = 1
	}
	governor := ""
	if tool == "fusion" {
		governor, toolSet = a.cfg.FusionGovernor, false
		if len(args) > 1 {
			governor, toolSet = args[1], true
			consumed = 2
		}
		governor = presetLeadAgent(p, governor, toolSet)
		if !fusion.Valid(governor, agents.Names()) {
			return 2, fmt.Errorf("unknown governor %q — use claude, codex, or gemini", governor)
		}
		tool = governor
	} else {
		tool = presetLeadAgent(p, tool, toolSet)
	}
	if !agents.Valid(tool) {
		return 2, errors.New("usage: coop acp [claude|codex|gemini|fusion [governor]]")
	}
	// Reject leftover tokens rather than silently ignore them (loop/fork do the same) — the ACP
	// adapter takes no extra args, so `coop acp claude foo`/`--nope` is a mistake worth surfacing.
	if leftover := args[consumed:]; len(leftover) > 0 {
		return 2, fmt.Errorf("coop acp: unexpected argument %q (usage: coop acp [claude|codex|gemini|fusion [governor]] [--credential <name>] [--model <model>] [--preset <name>])", leftover[0])
	}
	a.applyPreset(p, tool)
	if err := a.applyOneOff(tool, model, profile); err != nil {
		return 2, err
	}
	// Built AFTER the model selection: gemini's ACP command is its own binary and carries
	// the resolved model as a flag. tool passed agents.Valid above, so this can't miss.
	cmd, _ := acpCommand(a.cfg, tool)
	repo, img, err := a.resolveImage()
	if err != nil {
		return -1, err
	}
	lead := "" // --consult opts into the second-opinion directive (a no-op under fusion)
	if consult || a.preset != nil {
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
		Image: img, Repo: repo, Workdir: repo, Cmd: cmd, ForceNoTTY: true, Agent: tool,
		SupervisorID:   os.Getenv("COOP_ACP_SUPERVISOR"),
		FusionGovernor: governor, ConsultLead: lead, Preset: a.preset, Quiet: true,
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
func (a *app) cmdACPSupervise(rest []string) (int, error) {
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

	factory := func(_ context.Context) (*acpproxy.Child, error) {
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
			// Remove the box by its deterministic cidfile id first — works even mid-startup, before
			// labels exist; `rm -f` stops it too. Then kill the whole process group (inner coop +
			// its run client), the label backstop for any box that did get labelled, and the pipes.
			if cidPath != "" {
				if cid, rerr := os.ReadFile(cidPath); rerr == nil {
					a.rt.RemoveContainer(strings.TrimSpace(string(cid)))
				}
			}
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			a.rt.KillByLabel(box.LabelSupervisor, superID)
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
	if err := acpproxy.Run(ctx, os.Stdin, os.Stdout, factory); err != nil && !errors.Is(err, context.Canceled) {
		return 1, err
	}
	return 0, nil
}

// cmdFusion runs a council: the governor agent (a leading `claude|codex|gemini`, else
// COOP_FUSION_GOVERNOR) runs normally — it edits and does the real work — while a fusion
// instruction injected into its instruction file tells it to consult its two peers
// read-only and synthesize. It behaves like `coop <agent>`: `coop fusion claude` opens
// claude interactively; trailing `<args>` pass through to the governor.
func (a *app) cmdFusion(args []string) (int, error) {
	// --profile picks the governor's credential profile, like a plain `coop <agent>` run; read it
	// before governor parsing so the governor's own --profile is still reachable after a `--`.
	profile, args, err := extractRunProfile(args)
	if err != nil {
		return 2, err
	}
	// --model picks the governor's model, same shape (`coop fusion claude --model opus`);
	// the peers keep their own profile/agent defaults.
	model, args, err := extractRunModel(args)
	if err != nil {
		return 2, err
	}
	// --consult is a documented no-op for fusion (a council always consults its peers). Strip it so it
	// isn't leaked into the governor's own CLI as an unknown flag.
	_, args = extractConsult(args)
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
	governor, rest, govSet := a.parseGovernor(args)
	governor = presetLeadAgent(p, governor, govSet)
	if !fusion.Valid(governor, agents.Names()) {
		return 2, fmt.Errorf("unknown governor %q — use claude, codex, or gemini", governor)
	}
	a.applyPreset(p, governor)
	if err := a.applyOneOff(governor, model, profile); err != nil {
		return 2, err
	}
	repo, img, err := a.resolveImage()
	if err != nil {
		return -1, err
	}
	// The governor's autonomous default command, plus any extra args you pass through.
	cmd := append(append([]string{}, a.defaultCmd(governor)...), dropDashDash(rest)...)
	ui.Info("fusion: %s governs; peers %s consulted read-only", governor,
		strings.Join(fusion.Peers(governor, agents.Names()), " + "))
	return box.Run(a.cfg, a.rt, box.RunSpec{
		Image: img, Repo: repo, Cmd: cmd, Agent: governor, FusionGovernor: governor, Preset: a.preset,
		Homes: a.cfg.Homes, Network: a.cfg.Network, Cache: a.cfg.Cache,
	})
}

// parseGovernor takes a leading `claude|codex|gemini` token as the governor (else
// COOP_FUSION_GOVERNOR); everything else passes through to the governor. explicit
// reports whether the command named one (so a --preset's lead only fills the default).
func (a *app) parseGovernor(args []string) (governor string, rest []string, explicit bool) {
	governor = a.cfg.FusionGovernor
	tookGov := false
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--":
			return governor, append(rest, args[i+1:]...), tookGov // everything after passes through
		case !tookGov && len(rest) == 0 && agents.Valid(args[i]):
			// Only the FIRST leading agent name is the governor: `coop fusion claude` (matches
			// `coop acp fusion claude`); otherwise the default / COOP_FUSION_GOVERNOR. A second
			// agent token passes through to the governor (not silently swallowed as the governor).
			governor, tookGov = args[i], true
		default:
			rest = append(rest, args[i])
		}
	}
	return governor, rest, tookGov
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
		Cmd:       []string{"sh", "-c", "npm ls -g --depth=0 2>/dev/null | grep -iE 'claude|codex|gemini|acp' || true"},
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

// loopAgent picks the agent for `coop loop [claude|codex|gemini]` (default claude),
// erroring on any unexpected token.
func loopAgent(args []string) (agent string, explicit bool, err error) {
	agent = agents.Default()
	for _, x := range args {
		if !agents.Valid(x) {
			return "", false, fmt.Errorf("coop loop: unexpected argument %q (usage: coop loop [%s] [--tasks <path>] [--model <model>] [--preset <name>] [--consult] [--preflight|--no-preflight] [--debug-on-fail])", x, strings.Join(agents.Names(), "|"))
		}
		if explicit {
			return "", false, fmt.Errorf("coop loop: more than one agent given (%q and %q) — name just one", agent, x)
		}
		agent, explicit = x, true
	}
	return agent, explicit, nil
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
	credential, rest, err := extractRunProfile(rest)
	if err != nil {
		return 2, err
	}
	presetName, rest, err := extractRunPreset(rest)
	if err != nil {
		return 2, err
	}
	agent, model, agentSet, consult, debugOnFail, preflight, err := parseLoopArgs(rest, a.cfg.Preflight)
	if err != nil {
		return 2, err
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	// --preset: the preset's lead agent is the default (an explicit agent still wins), its
	// roles seed the run, and its models ladder becomes the rotation (below explicit flags).
	p, err := a.loadRunPreset(presetName)
	if err != nil {
		return 2, err
	}
	agent = presetLeadAgent(p, agent, agentSet)
	a.applyPreset(p, agent)
	a.applyLoopModel(agent) // COOP_LOOP_MODEL → the fallback tier (below a ladder target's model)
	queues, err := taskQueues(a.cfg, repo, flags)
	if err != nil {
		return 2, err
	}
	// The rotation ladder (model-first): a one-off --model/--credential wins; else the preset
	// lead's models; else the default (agent model across all signed-in accounts). expandLadder
	// turns it into the concrete (model, account) targets the loop cycles on rate limits.
	ladder, err := oneOffLadder(model, credential)
	if err != nil {
		return 2, err
	}
	if ladder == nil && p != nil && agent == p.LeadAgent {
		ladder = p.LeadModels
	}
	rot, err := a.buildRotation(agent, ladder)
	if err != nil {
		return -1, err
	}
	img := box.ImageForRepo(repo, a.cfg.BaseImage, a.cfg.ImageOverride)
	return a.loop(repo, img, agent, "", rot, queues, nil, consult, debugOnFail, preflight) // local loop: no fork label
}

// applyLoopModel puts COOP_LOOP_MODEL in the fallback tier — the loop's standing default
// model, used when a rotation entry carries no model of its own (a bare `models: [work]`
// or the no-preset default). It sits below a ladder target's model and below an explicit
// --model, and above the account's mark. Shared by `coop loop` and the fork loops.
func (a *app) applyLoopModel(agent string) {
	if a.cfg.LoopModel != "" {
		a.cfg.SetFallbackModel(agent, a.cfg.LoopModel)
	}
}

// parseLoopArgs pulls the --model <m>, --consult, --debug-on-fail, and
// --preflight/--no-preflight flags out of `coop loop` args; what remains must be at most
// one agent name. preflight defaults to def (COOP_PREFLIGHT) and the flags override it.
func parseLoopArgs(args []string, def bool) (agent, model string, agentSet, consult, debugOnFail, preflight bool, err error) {
	preflight = def
	var rest []string
	for i := 0; i < len(args); i++ {
		if v, n, ok, e := flagValue(args, i, "--model"); ok {
			if e != nil {
				return "", "", false, false, false, false, e
			}
			model = v
			i += n - 1
			continue
		}
		switch x := args[i]; x {
		case "--consult":
			consult = true
		case "--debug-on-fail":
			debugOnFail = true
		case "--debug": // v3: renamed to --debug-on-fail
			note, _ := removedCommandNote("loop --debug")
			return agent, model, agentSet, consult, debugOnFail, preflight, errors.New(note)
		case "--preflight":
			preflight = true
		case "--no-preflight":
			preflight = false
		default:
			rest = append(rest, x)
		}
	}
	agent, agentSet, err = loopAgent(rest)
	return agent, model, agentSet, consult, debugOnFail, preflight, err
}

// loopWorkPrompt and loopAuditPrompt name the queue dir(s) the iteration works as ABSOLUTE
// in-box paths (the box's working dir is repo, bind-mounted at its real path). A relative
// ".agent/tasks" resolves fine for claude/codex (cwd-relative), but gemini's read_file rejects
// a relative path — so the queues (and AGENTS.md) are named absolute for every agent. With
// several queues (a monorepo's per-component trees) they're all listed so the agent works the union.
func loopWorkPrompt(repo string, queues []string) string {
	return fmt.Sprintf("Read %s and the task queue(s) %s, then work the queue per the protocol. A task is a folder under a queue dir and its state is its directory (named with a sort prefix): 00_todo/ · 10_in_progress/ · 50_blocked/ · 99_done/. `coop` is NOT installed in this box, so you change a task's state by MOVING its folder between those dirs yourself — that move IS the state change; do not try to run `coop`. First, if a task is already in 10_in_progress/, a previous attempt was interrupted before it committed: read that task's state.md (its resume note — where it stopped, the next action, traps), then run `git status` and `git diff` to see its uncommitted work, and continue it (or discard the partial work with `git restore`/`git checkout` and redo it if off-track) until done. Otherwise pick the next task in 00_todo/ and claim it by moving its folder into 10_in_progress/. As you work, keep that task's state.md current — a small, overwritten snapshot of the status, what is done, the next action, and any traps — refreshed before each commit and before you pause; append your reasoning to its log.md. Read a file before you edit it — an edit to a file you haven't read is rejected and wastes a turn (don't survey with `cat` then edit). Do the work, run the gate, commit your work, then move its folder into 99_done/. If you hit a one-way-door decision, move its folder into 50_blocked/ and fill in its decision.md. Always update state.md as your final step, leaving it reflecting the finished state (do not blank it). Work exactly ONE task per run: take the task you claimed to done — or to blocked — then STOP without claiming or starting another, even if 00_todo/ still has tasks. The loop re-invokes you in a fresh box with fresh context for the next one; draining the whole queue in a single run is the loop's job, not yours.",
		filepath.Join(repo, "AGENTS.md"), absJoin(repo, queues))
}

func loopAuditPrompt(repo string, queues []string) string {
	return fmt.Sprintf("Audit: for every task folder in the 99_done/ of the queue(s) %s, verify its gate passes and a commit implementing it exists in the git log. `coop` is NOT installed in this box, so reopen any that fail by moving its folder back to 10_in_progress/ yourself, and note what is missing in its log.md. Do not fix anything yourself.", absJoin(repo, queues))
}

// loopPreflightPrompt is the one-shot cleanup pass run before the work loop when
// --preflight / COOP_PREFLIGHT is set: it resolves answered blockers, but works no task and
// changes no code (these files are git-ignored, so nothing is committed).
func loopPreflightPrompt(repo string, queues []string) string {
	return fmt.Sprintf("Pre-flight cleanup ONLY — do NOT work any task, write code, run the gate, or commit. Read %s and the queue(s) %s. `coop` is NOT installed in this box, so act by moving task folders yourself. Then, for each task in a 50_blocked/ dir, if its decision.md now has a filled-in Resolution, unblock it by moving its folder to 00_todo/. Leave every 00_todo/ and 10_in_progress/ task untouched; change no code and make no commits.",
		filepath.Join(repo, "AGENTS.md"), absJoin(repo, queues))
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
// in_progress/ both empty), then (unless a custom COOP_LOOP_CMD is set) runs a one-shot audit
// pass over the results. A model rate/usage limit is not a failure: the loop waits for the
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

// consult opts every iteration into the second-opinion directive: the box mounts the authed
// peers' credentials and the coop-consult wrapper, so an unattended lead can ask codex/gemini
// on hard calls — the orchestrator pattern running headless. Off by default: it widens the
// credential scope, so mounting peers into every loop box stays a deliberate choice.
func (a *app) loop(repo, img, agent, forkName string, rot *rotation, queues []string, sink io.Writer, consult, debugOnFail, preflight bool) (int, error) {
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
	custom := a.cfg.LoopCmd
	// Claude on a TTY streams its activity as JSON we decode into live lines; other agents, a
	// custom COOP_LOOP_CMD, or a non-terminal (pipe/CI/fork log) keep plain text output. The
	// stream-json marker in the command is what runIteration keys the decoder off.
	stream := agent == "claude" && len(custom) == 0 && ui.IsTerminal(os.Stdout) && ui.IsTerminal(os.Stderr)
	work, audit := loopWorkPrompt(repo, queues), loopAuditPrompt(repo, queues)
	// iterCmd builds one iteration's command: a raw COOP_LOOP_CMD override if set,
	// otherwise the chosen agent's headless form carrying the work/audit prompt.
	iterCmd := func(prompt string) []string {
		if len(custom) > 0 {
			return custom
		}
		cmd := a.agentLoopCmd(agent, prompt)
		if stream {
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
				ui.Info("⏸ finishing this iteration, then stopping — Ctrl-C again to stop now")
			},
			func() {
				ui.Info("■ stopping now")
				cancel()
			})
	}

	// Pre-flight: one best-effort housekeeping pass before working the queue — unblock any
	// task whose decision.md now has a filled-in Resolution. It works no task and deletes
	// nothing: done tasks are pruned only by a human (`coop tasks rm --all-done`), never
	// by an agent. Opt-in (--preflight / COOP_PREFLIGHT); skipped under a custom COOP_LOOP_CMD
	// (not the agent's headless form). Best-effort like the audit pass — a failure never blocks work.
	if preflight && len(custom) == 0 {
		ui.Info("pre-flight: resolving answered blockers")
		_, _, _ = a.runIteration(iterCtx, repo, img, agent, forkName, iterCmd(loopPreflightPrompt(repo, queues)), hosts, sink, consult)
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
	fails, waits, completed, stalls := 0, 0, 0, 0
	settledBaseline := c0.Done + c0.Blocked       // "settled" = tasks out of the actionable set (done OR blocked)
	prevHead := gitOut(repo, "rev-parse", "HEAD") // a commit between iterations is progress too (see below)
	for n := 1; ; {
		// A first Ctrl-C (soft stop) that arrived between iterations — or that woke a wait
		// below — stops here, before the next task is claimed.
		if softStop.Load() {
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
		// Run this iteration on the pool's active target — its credential (the mount and the
		// agent command both resolve cfg.AgentDir) and its model, if the target carries one.
		a.applyTarget(agent, rot)
		// The active profile is shown on the model line (streamjson) — don't repeat it on the banner.
		ui.Info("%s", progressBanner(n, c, active))
		code, out, err := a.runIteration(iterCtx, repo, img, agent, forkName, iterCmd(work), hosts, sink, consult)
		// A second Ctrl-C canceled iterCtx and tore the box down mid-iteration — stop now.
		if iterCtx != nil && iterCtx.Err() != nil {
			break
		}
		// A first Ctrl-C during this iteration: it ran to completion, so stop before the next
		// (don't fall through to the retry/wait accounting).
		if softStop.Load() {
			break
		}
		action, wait, resetAt := decideIteration(code, err, out, time.Now(), &fails, &waits)
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
			// A clean iteration that neither finishes NOR blocks a task AND commits nothing means
			// the agent keeps continuing an in_progress task it can't complete — bail after maxStalls
			// rather than loop forever. But a commit IS progress on a big task (a genuinely stuck loop
			// spins WITHOUT committing), as is blocking a one-way door — so don't count either as a stall.
			after, _ := queueProgress(hosts)
			settled := after.Done + after.Blocked
			head := gitOut(repo, "rev-parse", "HEAD")
			if head != "" && head != prevHead {
				prevHead, settledBaseline, stalls = head, settled, 0
			} else if newBase, newStalls, stop := progressStall(settled, settledBaseline, stalls); stop {
				return code, fmt.Errorf("no task finished, blocked, or committed in %d iterations — stopping (stuck on %q?)", maxStalls, active)
			} else {
				settledBaseline, stalls = newBase, newStalls
			}
		case actWait:
			// A rate/usage limit is expected on long runs. With more than one profile in
			// the pool, switch to another subscription and retry immediately; otherwise wait
			// for the reset. Either way the same iteration is retried, not burned.
			if rot.rotates() {
				a.rotateOnLimit(agent, rot, resetAt, &waits, wake)
			} else {
				sleepForLimit(wait, resetAt, wake)
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
	// audit pass and the drain summary — the queue isn't done, the user asked to stop.
	if softStop.Load() || (iterCtx != nil && iterCtx.Err() != nil) {
		cf, _ := queueProgress(hosts)
		fmt.Fprintln(os.Stderr, ui.Bold(ui.Yellow(fmt.Sprintf("■ stopped by request — %d/%d done", cf.Done, cf.total()))))
		return 0, nil
	}
	if len(custom) == 0 {
		ui.Info("queue empty — running audit pass")
		_, _, _ = a.runIteration(iterCtx, repo, img, agent, forkName, iterCmd(audit), hosts, sink, consult)
	}
	// Re-read the queue AFTER the audit: it may have reopened done tasks into 10_in_progress/. The
	// audit runs only once the work loop drained the queue, so anything now actionable was reopened
	// just now — the banner must not claim success. (The old check saw 00_todo/ only and missed
	// reopens, which land in 10_in_progress/.)
	cf, _ := queueProgress(hosts)
	fmt.Fprintln(os.Stderr, loopClosingBanner(cf, completed))
	return loopExitCode(cf), nil
}

// loopExitCode is the machine-readable companion to loopClosingBanner so cron/fleet/CI can branch on
// the loop's outcome without parsing stderr prose: 3 when the loop stopped with work blocked on a
// human decision and nothing else actionable, 0 otherwise — verified done, or an audit reopen, which
// stays 0 by design (see the reopened-banner task). Failures (1) and usage errors (2) surface from
// their own call sites, not here.
func loopExitCode(cf taskCounts) int {
	if cf.Todo+cf.Doing == 0 && cf.Blocked > 0 {
		return 3
	}
	return 0
}

// loopClosingBanner picks the loop's final line from the post-audit queue counts: reopened work
// (todo, or reopened into in_progress) and tasks blocked on a human decision are NOT "done", so only
// a truly drained queue earns the green "verified done". Pure, so the outcomes are unit-tested
// without running the loop.
func loopClosingBanner(cf taskCounts, completed int) string {
	switch {
	case cf.Todo+cf.Doing > 0:
		return ui.Bold(ui.Yellow(fmt.Sprintf(
			"⚠ audit reopened %s — run 'coop loop' to work them", ui.Count(cf.Todo+cf.Doing, "task"))))
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
// live bar watches for task progress. In a fully interactive run the agent's output is funneled
// into the scroll history above a sticky progress bar (a Docker-build-style live view);
// otherwise it goes straight to the terminal unchanged.
func (a *app) runIteration(ctx context.Context, repo, img, agent, forkName string, cmd, hosts []string, sink io.Writer, consult bool) (code int, output string, err error) {
	tail := &tailWriter{max: 64 << 10}
	live := ui.IsTerminal(os.Stdout) && ui.IsTerminal(os.Stderr)

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
	// --consult makes each iteration a consult lead: box.Run then mounts the authed peers'
	// credentials, the coop-consult wrapper, and the second-opinion directive. A preset
	// does the same with ITS roles: the routing contract mounts via ConsultLead.
	lead := ""
	if consult || a.preset != nil {
		lead = agent
	}
	code, err = box.Run(a.cfg, a.rt, box.RunSpec{
		Image: img, Repo: repo, Cmd: cmd, Agent: agent, Batch: true, ForkName: forkName, ConsultLead: lead, Preset: a.preset,
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
