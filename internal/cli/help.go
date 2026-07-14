package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/scaffold"
	"github.com/AndrewDryga/coop/internal/ui"
)

// helpRequested reports whether args contains -h or --help.
func helpRequested(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false // everything after -- is passthrough to the agent, not a request for coop's help
		}
		if a == "-h" || a == "--help" {
			return true
		}
	}
	return false
}

func printHelp(cfg *config.Config) { fmt.Print(helpText(cfg)) }

// groupHelp prints a command group's focused help (its commandHelp entry) and returns exit
// 0 — the response to a bare `coop <group>` with no subcommand. A group must never answer a
// missing subcommand with an `unknown <group> command ""` error; that path is for a *mistyped*
// subcommand. See .agent/rules/bare-subcommand-shows-help.md.
func groupHelp(cmd string) (int, error) {
	if h, ok := commandHelp[cmd]; ok {
		printCommandHelp(h)
		return 0, nil
	}
	printHelp(config.Load())
	return 0, nil
}

// helpText renders the top-level command reference: one command per line, grouped, with a
// pointer to per-command help. Flags, sub-verbs, and examples live in `coop <cmd> --help`
// and the README, so this stays a clean, scannable overview.
func helpText(cfg *config.Config) string { return renderHelp(cfg, false) }

// renderHelp renders the top-level command reference. ref=true is the DETERMINISTIC reference form
// for docs/`coop help --all`: forced no-color, no state-aware FIRST RUN hint, and the canonical
// (compose-less) up/down rows — so the output is identical on every machine (gendocs -check depends
// on it). ref=false is the live, state-aware terminal view.
func renderHelp(cfg *config.Config, ref bool) string {
	var b strings.Builder
	p := ui.For(os.Stdout) // help is a stdout view — gate color on stdout so a pipe stays clean
	if ref {
		p = ui.Palette{} // forced plain: the reference must be byte-identical regardless of the terminal
	}
	group := func(label string) { fmt.Fprintf(&b, "\n%s\n", p.Bold(label)) }
	// row keeps a column gap even when a command is long, so a description never glues to it.
	// Width is counted in runes, not bytes, so a command with a "…" doesn't shift its column.
	row := func(cmd, desc string) {
		gap := 34 - utf8.RuneCountInString(cmd)
		if gap < 2 {
			gap = 2
		}
		fmt.Fprintf(&b, "  %s%s%s\n", cmd, strings.Repeat(" ", gap), desc)
	}
	// dimRow is row for a command not available in this repo right now (e.g. `coop up` with no
	// .agent/compose.yml) — the whole line recedes (gap computed on plain text, then dimmed, so the
	// command column still aligns). Dim is a no-op when color is off, so a pipe keeps the text.
	dimRow := func(cmd, desc string) {
		gap := 34 - utf8.RuneCountInString(cmd)
		if gap < 2 {
			gap = 2
		}
		fmt.Fprintf(&b, "  %s\n", p.Dim(cmd+strings.Repeat(" ", gap)+desc))
	}

	if ref { // the reference omits the build version — its bytes must not depend on the tag/commit
		fmt.Fprintf(&b, "%s — run a coding agent all night long in a box it can't escape.\n", p.Bold("coop"))
	} else {
		fmt.Fprintf(&b, "%s %s — run a coding agent all night long in a box it can't escape.\n", p.Bold("coop"), resolveVersion())
	}
	fmt.Fprint(&b, "Usage: coop <command> [args]\n")
	// A newcomer (no agent signed in) gets the day-one order up front. Pure-local check (no runtime),
	// so `coop help` still works before Docker exists — same state-aware style as the up/down rows below.
	if !ref && !anyAgentSignedIn(cfg) {
		fmt.Fprintf(&b, "\n%s  set up in order:  coop build → coop login <agent> → coop doctor\n", p.Bold("FIRST RUN"))
	}

	group("AGENTS")
	row("coop <agent>[:model][@account]", "a sandboxed agent (the target grammar)")
	row("coop <preset>", "run a preset interactively (its lead leads)")
	row("coop fusion <gov> --peer <a>…", "one leads, named peers advise")
	row("coop acp <agent|fusion|preset>", "serve as an editor agent (ACP; e.g. Zed)")
	row("coop <agent> --peer <peer>…", "a read-only second opinion, named peers")

	group("CREDENTIALS, MODELS & PRESETS")
	row("coop login <agent>", "sign in an agent (a subscription)")
	row("coop credentials [agent]", "stored credentials + which are signed in")
	row("coop models [agent]", "the model menu per agent")
	row("coop presets [name]", "orchestration recipes (lead + roles)")

	group("THE BOX")
	row("coop run -- <cmd...>", "run a raw command in the box")
	row("coop shell", "an interactive shell in the box")

	group("FORKS — review and land work like a PR")
	row("coop fork <name> [target]", "open or re-enter a fork and run an agent")
	row("coop fork ls", "list this repo's forks")
	row("coop fork review <name>", "show a fork's review dossier + diff")
	row("coop fork merge <name>", "rebase the fork onto your branch and land it")
	row("coop fork logs [name]", "tail a fork's loop log (no name: every fork)")
	row("coop fork rm <name>", "discard a fork")
	row("coop fork stop <name>", "stop a detached loop")
	row("coop fork open <name>", "open the fork in your editor")
	row("coop fork path <name>", "print the fork's filesystem path")

	group("UNATTENDED")
	row("coop loop [agent]", "work the queue(s) until done, then sign off")
	row("coop fleet init", "write the .agent/fleet.yaml template")
	row("coop fleet up", "start every fork's loop, detached")
	row("coop fleet down", "stop the fleet's running loops")
	row("coop fleet watch", "live dashboard of every fork's progress")
	row("coop fleet prune", "drop forks no longer in the fleet file")

	group("TASKS — a folder-per-task queue in .agent/tasks/")
	row("coop tasks ls", "show the queue, grouped by state")
	row("coop tasks watch", "live board of the queue + active forks")
	row("coop tasks add \"<title>\"", "add a task (then claim/block/unblock/done)")
	row("coop tasks decisions", "what's blocked on a decision (-i to answer)")
	row("coop backlog", "park unscheduled ideas; promote when ready")

	group("SERVICES — the box's .agent/compose.yml sidecars")
	// `coop up`/`down` act on this repo's .agent/compose.yml — always NAME the file (it's what makes
	// the rows obvious), listing its real services when present, and dim the pair when there's none to
	// act on. Repo resolution is best-effort (help runs anywhere; outside a repo, or with no compose).
	repo, _ := box.ResolveRepo(cfg.RepoOverride)
	switch {
	case ref: // the reference form is machine-independent: no per-repo service list
		row("coop up", "start the .agent/compose.yml services")
		row("coop down", "stop the .agent/compose.yml services")
	case len(scaffold.ComposeServiceNames(box.ComposeFile(repo))) > 0:
		services := scaffold.ComposeServiceNames(box.ComposeFile(repo))
		row("coop up", "start the .agent/compose.yml services ("+strings.Join(services, ", ")+")")
		row("coop down", "stop the .agent/compose.yml services")
	default:
		dimRow("coop up", "none in .agent/compose.yml yet")
		dimRow("coop down", "stop the .agent/compose.yml services")
	}

	group("SAFETY — prove the box holds, catch committed secrets")
	row("coop doctor", "attack the box, prove isolation holds")
	row("coop check-secrets", "scan the working tree for committed secrets")

	group("SETUP & MAINTENANCE")
	row("coop init [--stack asdf]", "scaffold the queue, hooks, skills, subagents")
	row("coop build", "build the box image (stable, pinned)")
	row("coop update", "self-update coop, then rebuild the box")
	row("coop completion <shell>", "shell tab-completion (bash, zsh)")
	row("coop sign", "re-sign unpushed commits with your host key")
	row("coop prompt", "a one-line status for a shell prompt / tmux")
	row("coop help", "this help")
	row("coop version", "print the version")

	fmt.Fprint(&b, "\nRun 'coop help <command>' or 'coop <command> --help' for a command's details —\n"+
		"for an agent (claude/codex/gemini/grok), --help is the agent's own.\n")
	if ref { // machine-independent footer for the reference/manual (no host-specific paths)
		fmt.Fprint(&b, "\nConfig  coop.conf (COOP_CONF), or COOP_* env vars\nAuth    the config dir (COOP_CONFIG_DIR)\nDocs    https://coop.dryga.com\n")
	} else {
		fmt.Fprintf(&b, "\nConfig  %s, or COOP_* env vars\nAuth    %s\nDocs    https://coop.dryga.com\n",
			tildeify(filepath.Join(cfg.BoxHome, "coop.conf")), tildeify(cfg.ConfigDir))
	}

	return b.String()
}

// anyAgentSignedIn reports whether any agent has a signed-in credential — its default/flat login or a
// named profile. Pure-local (reads the config dir + env file, no runtime), so `coop help` can key its
// FIRST RUN hint on it without breaking the runtime-free help path.
func anyAgentSignedIn(cfg *config.Config) bool {
	for _, agent := range agents.Names() {
		if box.ProfileAuthed(cfg, agent, cfg.DefaultProfileOf(agent)) {
			return true
		}
		for _, p := range cfg.Profiles(agent) {
			if box.ProfileAuthed(cfg, agent, p) {
				return true
			}
		}
	}
	return false
}

// RenderManual is the entire CLI reference as ONE deterministic, plain-text document: the reference-
// form top-level overview, then every command's page in a stable order. It's the single source shared
// by `coop help --all`, docs/cli.md, and site/llms.txt — so terminal, docs, and the offline reference
// are provably identical (tools/gendocs -check enforces it). Plain (ui.Palette{}) and state-free, so
// its bytes never depend on the terminal, the repo's compose file, the config paths, or logins.
func RenderManual(cfg *config.Config) string {
	var b strings.Builder
	b.WriteString(renderHelp(cfg, true))
	b.WriteString("\n" + strings.Repeat("=", 78) + "\n\n")
	b.WriteString(forkHelpText(ui.Palette{}) + "\n")
	b.WriteString(runHelp + "\n")
	for _, name := range topLevelCommands { // stable order; fork/run have their own pages above
		if h := commandHelp[name]; h != "" {
			b.WriteString("\n" + h + "\n")
		}
	}
	return b.String()
}

// runHelp is `coop run`'s page. It's deliberately NOT in commandHelp: the dispatch's help check
// is `--`-blind, so `coop run -- --help` (which must run --help in the box) would otherwise print
// this instead. cmdRun (which honors --) and helpForCommand print it directly.
const runHelp = `coop run — run a raw command in the box.

  Usage: coop run -- <cmd...>

  Everything after -- runs verbatim in the sandbox — same mounts, secret-shadowing,
  and network as an agent. "coop run echo hi" works too; use -- when the command has
  flags coop would otherwise read (e.g. coop run -- npm test --watch).`

// agentHelp is `coop help <agent>`: it documents coop's OWN wrapper flags (the ones coop consumes
// before a --), since `coop <agent> --help` forwards to the agent's real CLI. Kept short per
// help-output-style — the detail is in coop credentials / coop models.
const agentHelp = `coop <agent> — run a sandboxed coding agent (claude, codex, gemini, or grok).

  Usage: coop <agent>[:model][/effort][@account] [coop flags] [-- <agent args>]
         coop <preset>   (run an orchestration preset interactively — its lead leads)

  The agent is a TARGET — a provider, an optional :model, an optional reasoning /effort,
  an optional @account:
    claude, claude:opus, claude:opus/xhigh, claude@work, claude:opus/high@work
  In the SAME who-runs slot, a bare word that names a preset runs that recipe instead —
  coop frontier (its lead + roles; see coop help presets). A run names one, never both.

  These flags are coop's own, read before a -- (everything after -- goes to the agent):
    --peer <peer>…       a read-only second opinion from NAMED peers (repeatable); each
                         <peer> is a target: --peer codex:gpt-5.5 --peer gemini
    --                   pass the rest verbatim to the agent, e.g. coop claude -- --help

  Sign in first with 'coop login <agent>'. For the agent's own flags: coop <agent> -- --help.`

// commandHelp is the focused text for `coop <cmd> --help`, per subcommand. fork has its
// own richer forkHelp, run has runHelp, and the agents use agentHelp (their `--help` forwards to
// the agent's own CLI). Each value's first line is the synopsis.
var commandHelp = map[string]string{
	"sign": `coop sign — re-sign this branch's unpushed commits with your host key.

  Usage: coop sign [--from <ref>]

  Box commits are made UNSIGNED — no signing key ever enters a box — so a remote
  that requires signed commits (a protected main, like many projects) rejects
  work a loop or an interactive box produced. 'coop sign' re-signs them on the
  HOST, where your signing key lives, using your GLOBAL git signing config (so a
  poisoned repo can't point gpg.program at a planted binary).

  The range is the UNPUSHED one — @{upstream}..HEAD — git's own rule for what is
  safe to rewrite: it never touches pushed history and never pushes. With no
  upstream, pass --from <ref> (e.g. the commit you last pushed). It refuses a
  dirty tree and a range containing a merge commit. 'coop loop' signs each cycle
  automatically when you sign by default (commit.gpgsign=true).`,
	"prompt": `coop prompt — one compact status line for a shell prompt, tmux, or menubar.

  Usage: coop prompt

  Prints this repo's state on ONE line — task counts and fork/loop activity, with a
  compact separator between non-zero segments. Nothing prints when the queue is empty
  and no forks exist, so an embedding prompt stays clean.

  Read-only and cheap: it reads the task dirs + fork pidfiles (plus one git-root
  lookup) — never a per-fork git call and never docker — so it's safe to run on
  every prompt redraw. Wire it into your shell prompt (a starship custom command)
  or tmux, e.g. set -g status-right '#(cd #{pane_current_path}; coop prompt)'.`,

	"shell": `coop shell — open an interactive shell in the box.

  Usage: coop shell

  A shell in the sandbox at your repo — same mounts, secret-shadowing, and
  network as an agent run. Exit to return.`,

	"login": `coop login <agent> — sign in to an agent (token persists in the config dir).

  Usage: coop login <agent>[@account]

  Runs the agent's sign-in (paste a code, no browser). Re-run any time to
  refresh or switch accounts — e.g. after a usage limit.

  @account signs in a second (or third) account under a name, so one agent can
  hold several subscriptions: coop login claude@work. An unattended loop rotates
  across all of them when one is rate limited (a bare model in a preset's lead agent:
  ladder fans out over every account). Without @account the sign-in targets the default.`,

	"credentials": `coop credentials — list stored credentials; a path grammar edits one.

  Usage: coop credentials [agent [credential]]
         coop credentials <agent> <credential> default
         coop credentials <agent> <credential> rm

  A CREDENTIAL is one stored account/login — a rate-limit slot. (Orchestration
  recipes are PRESETS; see coop help presets. 'coop profiles' was the pre-v3 name.)
  Each token narrows: no args lists every agent, an agent lists its credentials
  (signed in? default? how long since its token last rotated?), a credential shows
  its detail, and a trailing attribute reads or writes one property of it. A credential is one subscription; add more
  with 'coop login <agent>@<name>', then an unattended loop rotates across them on
  a rate limit (a bare model in a preset's lead agent: ladder). The model is a separate
  axis — set it inline (claude:opus) or in a preset, never on a credential.

  default                mark this credential as what a plain 'coop <agent>' runs,
                         and the account a loop's rotation starts on. A mark you
                         set — the listing shows it first, tagged (default).
  rm                     delete the credential (its login token and session
                         history). Set a different default first if you're
                         removing the marked one.

  Run on a specific account without changing the default — put it in the target on
  any agent launch: 'coop claude@work', 'coop fusion claude@work --peer codex', and
  'coop acp claude@work' (so an editor entry can pin an account).`,

	"models": `coop models [agent] — the model menu per agent.

  Usage: coop models [claude|codex|gemini|grok] [--refresh]

  A block per agent: its models and when that list was last refreshed. A fresh per-agent
  cache shows the agent's real list; a never- (or stale-) refreshed list is the curated
  static examples — model ids churn, so ANY id the agent's CLI accepts works either way
  (coop never validates a model id). A model is an axis of its own — set it inline in the
  target (claude:opus) or in a preset's lead agent: ladder, never on a credential.

  Plain 'coop models' is instant and never needs the container runtime — it only reads
  the cache. '--refresh' runs grok/codex's native catalog CLI on the host ('grok models',
  'codex debug models') and asks claude/gemini's ACP adapter in a short-lived credential-
  scoped box. Normal 'coop acp' sessions also refresh claude/gemini opportunistically.
  Refresh is best-effort: an unavailable CLI/runtime, timeout, or parse error falls back
  to the last cache or the static list, noted on that block — it never errors or hangs.

  Pick per run inline in the target on any launch: 'coop claude:fable',
  'coop fusion claude:opus --peer codex', 'coop loop claude:haiku',
  'coop fork risky claude:opus --loop', 'coop acp claude:sonnet'.

  Precedence: the target's :model > the active rotation entry's model (a loop stepping
  through a preset's lead agent: ladder, or loop.yaml work.agent) > COOP_<AGENT>_MODEL
  (agent-wide) > a model baked into COOP_<AGENT>_CMD > the agent CLI's own default.
  An account rides the SAME target (claude:opus@work). coop never validates a model
  id — a bad one fails in the agent's own error.

  Reasoning effort is a sibling axis, set with /effort in the same target
  (claude:opus/xhigh, codex/high): low, medium, high, xhigh, or max — coop passes the
  level through and the agent's CLI validates it (claude, codex, and grok have it; gemini
  has none, so a /effort on gemini errors). One axis carries both — a target and
  COOP_<AGENT>_MODEL take model[/effort] (e.g. opus/high), so there is no separate effort
  var. Precedence mirrors the model: the target's /effort > a rotation rung's effort > those.`,

	"acp": `coop acp <agent|fusion|preset> — serve as an ACP agent over stdio (for editors).

  Usage: coop acp <agent>[:model][/effort][@account] | <preset>          [--peer <peer>…]
         coop acp fusion <governor>[:model][/effort][@account] | <preset> [--peer <peer>…]

  Speaks the Agent Client Protocol on stdin/stdout. Point your editor's ACP
  command at e.g. ["acp","claude"] — one entry per agent or governor. A bare
  ["acp"] (no agent) works too: it starts on your first signed-in provider and the
  PROVIDER dropdown below switches it live; name one to pin it. coop always
  proxies the session, so the editor stays connected across a box restart (a
  rebuild/OOM) — it reconnects and replays the handshake, no lost session.

  coop owns the editor's toolbar: it runs the agent in yolo mode (the box is the
  sandbox, so no permission prompts), defaults the model dropdown to coop's model,
  drops the permission-mode and subagent pickers, and leads with three dropdowns
  mirroring the target grammar — PROVIDER (who runs), ACCOUNT (the lead's login), and
  PRESET (the recipe); one selection underneath, so changing one refreshes the others.
  On a preset the native model/effort knobs are hidden — the preset's ladder and roles
  pin them.
  An account or same-provider switch is transparent — the conversation is preserved (a
  shared, credential-independent session store). A PROVIDER switch (picking another
  signed-in agent, or a cross-provider preset rung rotating in on a rate limit)
  re-creates the session on the new agent and carries the conversation BEST-EFFORT:
  coop prepends the thread so far — message text plus one-line tool narration, no tool
  payloads, labeled approximate — budgeted by COOP_ACP_CARRY_TOKENS (default 200000;
  trim it below the smallest window you switch into). A switch made MID-TURN re-sends
  the in-flight prompt once the new box is up, so the turn completes on the new target
  instead of erroring. On a rate limit it auto-rotates across the ladder's rungs —
  accounts, models, providers.

  The target pins the session's agent, model, and account — an editor can run two
  entries on different ones, e.g. ["acp","claude:opus@work"].

  A bare preset name in the who-runs slot runs the session under that orchestration
  preset (its lead is the agent — or governor, under fusion; see 'coop help presets').

  --peer <peer>… lets the session ask NAMED peers for a read-only second opinion
  (repeatable; only those peers' credentials are mounted) — the orchestrator pattern,
  from your editor.

  To make provider switching near-instant, coop keeps a box warm per OTHER signed-in
  provider (spawned in the background at session start), so a switch pays only the ACP
  replay, not a container + adapter cold-boot. Set COOP_ACP_WARM=0 to disable it (one
  fewer idle box per signed-in provider) on a low-RAM machine.

  Picking up a rebuilt coop WITHOUT restarting your editor: send the running server
  SIGHUP — 'pkill -HUP -f "coop acp"'. It re-execs the freshly-installed binary in
  place (same process, same stdio), tears down its box, and re-establishes your open
  threads against a fresh box on the new binary — the editor never sees a disconnect.
  (SIGTERM/SIGINT still STOP it; only SIGHUP reloads. A box restart already picks up
  box-side changes, so SIGHUP is for supervisor-side changes to coop itself.)

  Debugging a misbehaving session: set COOP_ACP_TRACE=1 in the editor's server env, or
  create ~/.config/coop/acp-debug, and coop appends the editor<->box ACP wire to
  ~/.config/coop/acp-trace-<pid>.log (the sentinel works on an already-running server).
  The log is size-capped and auto-rotated so it can't grow unbounded; it holds prompts
  and file contents, so treat it as sensitive.`,

	"fusion": `coop fusion <governor> --peer <agent>… — one agent leads, named peers advise, it synthesizes.

  Usage: coop fusion <governor>[:model][/effort][@account] | <preset> --peer <agent>… [args...]

  The governor is a REQUIRED target — name it (or let a preset's lead govern); there is
  no implicit default. Its :model, /effort, and @account fold into this run; the peers keep
  their own. Peers advise read-only; only the governor writes. Lighter, opt-in variant:
  coop <agent> --peer <peer>…

  --peer <agent> names a council member (repeatable, at least one required — or a
  preset that supplies consult roles): coop fusion claude --peer codex:gpt-5.5 --peer gemini.
  Each <peer> is a target; only the named peers' credentials mount. There is no implicit
  "consult everyone signed in".

  A bare preset name in the governor slot loads an orchestration preset: its lead governs,
  and its role routing rides along with the council directive ('coop help presets').

  Like coop <agent>, it forwards extra args to the governor — the leading who-runs slot
  picks the governor (a target) or a preset; anything after passes through (or after a --).`,

	"presets": `coop presets — YAML orchestration recipes under .agent/presets/<name>/.

  Usage: coop presets [name]        list them, or show one recipe in full
         coop presets init [name]   scaffold the frontier template (default name: frontier)

  A PRESET is a runtime recipe: which agent leads, and which roles it can route
  work to — each role an agent: target or fallback list + routing hints. The lead's
  agent: is a target, or a fallback ladder (even CROSS-PROVIDER, [claude:fable, codex:gpt-5.6-sol]):
  a bare provider:model runs on EVERY signed-in account (rotating on a rate limit),
  provider:model@account pins one. On a loop it rotates the ladder top-to-bottom, running each
  rung's own agent; a single run uses the first entry (also the default agent). Accounts are your
  local logins (see coop credentials) — presets name models, not secrets.
  Cross-provider rungs rotate on the loop AND on ACP sessions (ACP re-creates the session on
  the new provider and carries the thread best-effort as text); fusion refuses such a ladder
  (one governor per council).

  Run one by NAMING it in the who-runs slot:
    coop <name>
    coop loop <name>
    coop fusion <name>
    coop acp <name>
    coop fork <fork> <name> --loop
  A fleet can name it per fork in .agent/fleet.yaml (agent: <name>). A target
  (claude:opus@work) in that same slot runs the agent directly instead.

  .agent/presets/frontier/preset.yaml:

    lead:
      # a target, or a ladder (cross-provider ok) — frontier models at xhigh effort
      agent: [claude:claude-fable-5/xhigh, codex:gpt-5.6-sol/xhigh]
      prompt: roles/lead.md                 # Optional Markdown, appended to the generated contract.
    roles:                                  # consult/delegate agent: may be a fallback list; default accounts
      thinker:                              # native Claude subagent — deep thinking in-session
        mode: native
        agent: claude:claude-opus-4-8/xhigh # model + effort ride agent: (generates coop-thinker)
        when: [architecture, debugging, code-review]
        prompt: roles/thinker.md            # its system prompt (or set subagent: <name> to reuse one)
      critic:                               # read-only peer via coop-consult
        mode: consult
        agent: [codex:gpt-5.6-sol/xhigh, grok:grok-4.5/high]
        when: [plan-review, security]
      fast:                                 # write-capable delegate via coop-delegate
        mode: delegate
        agent: [gemini:gemini-3.5-flash, codex:gpt-5.4-mini]
        when: [boilerplate, bulk-edits, test-scaffolding]
        commit: never                       # the delegate edits; the LEAD reviews, gates, commits
        concurrent: never                   # delegate runs are serialized

  coop generates the lead's routing contract from this — each role, when to use it, and
  its ROLE-ADDRESSED invocation (@coop-<role>, coop-consult <role>, coop-delegate <role>)
  — and mounts the wrappers. Markdown prompt files (roles/lead.md, roles/<name>.md)
  append to the generated text, never replace it. A native role generates a coop-<role>
  Claude subagent in the box from itself (its model + when + prompt) — never written to
  your repo; set subagent: <name> to reference an existing .claude/agents/ subagent
  instead. Consult/delegate ladders advance once per target only after a failed command
  proves a rate limit; ordinary failures stay visible. A delegate advances only from a
  clean worktree when the limited rung left every file and Git history unchanged. A consult
  remembers its successful rung and transcript for --continue. Providers without mounted
  credentials are skipped; every available rung's credential home is mounted in the lead box.
  The role's prompt (if any) is its persona.
  Native roles run inside the lead's session, so
  under a codex/gemini/grok lead they degrade to exactly such a consult (same model + persona),
  coop-consult <role> instead of @coop-<role>.
  A delegate may edit the worktree but must not commit — coop-delegate fails loud if HEAD
  moved — and the lead owns the diff review, the gate, and the commit. Model ids: coop models.
  Scaffold one: coop presets init.

  WHERE presets live — two locations, repo wins: a preset resolves first from the
  repo's .agent/presets/<name>/, then from a per-user global dir ~/.config/coop/presets/
  (COOP_PRESETS_DIR overrides it), so a recipe like frontier applies across every repo
  without symlinking. A repo preset shadows a same-named global one (repo wins wholesale,
  no merging); coop presets tags a global-sourced one (global). init scaffolds into the
  repo — author a global preset by hand (or copy one there).`,

	"fleet": `coop fleet — run a declarative fleet of forks from .agent/fleet.yaml.

  Usage: coop fleet <init|up|down|watch|prune>

  init           write a .agent/fleet.yaml template
  up             start every fork in the fleet, looping its tasks, detached
  down           stop the fleet's running loops
  watch          live dashboard of every fork's progress (auto-exits when the fleet's
                 done; Ctrl-C anytime). Task-centric view: coop tasks watch
  prune          remove forks no longer in the fleet file (kept: running, dirty, or
                 unmerged — pass --force to remove those too)

  up and down take --prune (with optional --force) to prune in the same step.

  .agent/fleet.yaml is a forks: map — each fork needs tasks: (the tree that seeds its
  loop) and agent: — the who-runs, either a TARGET (provider[:model][/effort][@account];
  give each fork a DIFFERENT account so they run in parallel) or a PRESET NAME (its lead +
  ladder drive the fork). A fork takes ONE account — a full rotation ladder lives in a preset:

    forks:
      core:
        tasks: .agent/tasks.core
        agent: frontier
      perf:
        agent: codex:gpt-5.5@work
        tasks: .agent/tasks.perf

  List forks: coop fork ls`,

	"tasks": `coop tasks — drive the task queue (a folder per task under .agent/tasks/).

  Usage: coop tasks [--tasks <path>]... <command>

  ls [--all]       list tasks by state, with counts (recent done capped; --all shows all)
  watch            live board: the queue + any active forks, deduped by id (auto-exits when done).
                   Per-fork fleet view: coop fleet watch
  add [--project <name>] "<title>"
                   scaffold a task folder in todo (or fill it inline: --context/--acceptance/--approach/--subtask)
  claim <id>       claim a task before you start it (todo -> in_progress)
  block <id>       park it on a decision (-> blocked) and write a decision.md stub
  unblock <id>     move it back to todo (claim it to start); add "<answer>" to record in decision.md
  done <id>        move it to done (the archive)
  path <id>        print a task's resolved folder path
  rm <id>          delete a task folder; --all-done (alias: clear) clears the done archive
  decisions [-i]   list open decisions; -i walks them one by one to answer (records + unblocks)
  lint             check the tree (blocked<->decision.md, no status field, ...; exits 1)
  split <n>        slice the todo tasks into n copy-trees (.agent/tasks.sliceN); loop one fork per slice
  queues           print each configured queue's path, one per line (for scripts and the sweep guard)

  A task's state is its directory — 00_todo/ 10_in_progress/ 50_blocked/ 99_done/, the
  numeric prefix just sorts 'ls' in lifecycle order — so each transition is a folder move.
  Removing tasks is a MANUAL step: the loop and skills only ever move a finished task to
  done, never delete it, so 'coop tasks rm --all-done' is how you prune the archive.
  Defaults to .agent/tasks/ — or, in a monorepo, every subproject's .agent/tasks listed under
  'subprojects:' in .agent/project.yaml, so you never hand-maintain COOP_TASKS. Override with
  --tasks or COOP_TASKS. Paths are repo-relative. With several queues (a monorepo), ls, lint, and decisions
  (including -i) roll up across all of them, and the id commands (claim/block/unblock/done/
  rm) find their task in whichever queue holds it — erroring only when an id matches in
  more than one queue (split slices share ids with their source). In a monorepo, add requires
  --project root|<subproject> so creation picks an explicit queue; with raw --tasks overrides,
  add and split still need a single --tasks because they create into a queue.`,

	"backlog": `coop backlog — park the BIG or not-yet-ready as task folders (.agent/tasks/xx_backlog/).

  Usage: coop backlog [--tasks <path>]... [ls | add "<title>" | rm <id> | promote <id>]

  (bare)           list the backlog drawer
  add "<title>"    capture an idea (--context/--acceptance/--approach/--subtask fill it inline)
  promote <id>     move it into 00_todo/ when it's ready to work (then coop tasks claim)
  rm <id>          drop an idea (--yes skips the confirm)

  The backlog is the SAME task-folder format as the queue, in an xx_backlog/ drawer that lives
  OUTSIDE the lifecycle — the loop, the Stop hook, and 'coop tasks' all ignore it, so an idea
  sits here with no nagging until you promote it (a folder move, not a rewrite). This drawer is
  for work that needs a spec, a decision, or real scoping first — a simple, ready task you found
  goes straight to the queue ('coop tasks add'), never here. Defaults to .agent/tasks/ — or, in
  a monorepo, every subproject's queue (see coop tasks): ls rolls up across them and rm/promote
  find the item in whichever queue holds it, while add needs a single --tasks.`,

	"check-secrets": `coop check-secrets — scan the working tree for committed secrets, by content.

  Usage: coop check-secrets [--include-ignored]

  Scans for token shapes and high-entropy values, reporting file:line. Exits
  non-zero on a hit, for use as a pre-flight or CI check. Hide a flagged file
  with .coopignore.

  By default it scans the commit-candidate files (tracked + untracked; gitignored
  excluded). A 'coop run'/'shell'/'loop' mounts the WHOLE tree, though, so a
  gitignored-but-not-shadowed file is still visible to the agent — pass
  --include-ignored to scan the full visible tree too (deps/build dirs and
  shadowed files are still skipped).`,

	"loop": `coop loop [agent] — work the task queue until done, then sign off.

  Usage: coop loop [<agent>[:model][/effort][@account,…] | <preset>] [--tasks <path>]... [--peer <peer>…] [--preflight] [--no-mcp] [--debug-on-fail]

  A fresh agent per iteration works the todo tasks; when the queue empties, a DEMANDING
  signoff pass (a senior reviewer's bar) re-checks each shipped task — goal met (every
  acceptance criterion + subtask), standards followed (AGENTS.md + .agent/rules, no scope
  creep), the FAILURE path tested, the change polished (docs/CHANGELOG updated), plus
  bookkeeping — then runs the repo's gate ONCE across the whole repo, reopening anything
  short of "merge with no changes". If the signoff reopened work, the loop drains and
  signs off AGAIN, repeating until a signoff reopens nothing (verified done) or the round
  cap is hit — then the task it keeps reopening is blocked for a human (exit 3), not
  reported as done. The cap SCALES with the batch: half the tasks worked this run, clamped
  to [3, loop.yaml signoff.rounds] (default 5) — a small batch still gets a few tries, a big
  overnight batch can't ping-pong one stuck task forever. On a rate limit it rotates to the
  next target in its agent: ladder, or waits out the reset when all are limited.

  Every review closes with a structured PASS/FAIL receipt naming the exact sorted task IDs it
  reopened. Coop compares it with the review's done-to-actionable folder delta and named subjects;
  unrelated queue work is ignored, while a missing, malformed, or mismatched receipt fails closed.

  One committed .agent/loop.yaml configures every step (preflight/work/between/signoff/verify),
  each with its own agent: model ladder and prompt — between is the per-task reviewer, signoff the
  final review, verify an optional post-signoff pass that e2e-tests the affected features. Prompts
  never REPLACE a coop built-in: signoff.prompt APPENDS to its senior review; between.prompt,
  verify.prompt, and preflight.prompt SET their pass. Ordinary between review is opt-in and has no
  built-in prompt, but a completed task that edits a gate-defining file always gets an immediate
  protected audit; it uses between.agent/prompt when configured, otherwise the signoff target and
  a focused built-in prompt. Preflight's built-in tidy is coop itself, run host-side, so its prompt
  is the optional agent cleanup on top. The review
  passes are handed the run's CHANGE CONTEXT — every task completed this loop, by its Coop-Task
  trailer, with the files it touched — so "e2e the affected features" resolves against a concrete
  list; place it inline with {loop.changes} / {loop.tasks} / {loop.affected}. signoff.rounds is the
  round cap, preflight.enabled the pre-loop cleanup, work.command a raw per-iteration override, and
  mcp: false runs every stage's box without the shared MCP config — the servers' tool schemas ride
  at the front of every model request, so a drain that never uses those tools shouldn't pay for
  them each iteration (leave it on if a verify: pass depends on MCP tooling). A missing file or
  field = the built-in default. (coop init scaffolds a commented loop.yaml.)

  Each step's agent: is a ladder of TARGET (provider[:model][/effort][@account]) or PRESET-NAME
  rungs: signoff.agent runs the final review on its own, typically STRONGER model (the cheap
  work loop does the work, a capable model signs it off), between.agent the per-task audit, and
  work.agent the work rotation when the launch names no target and no preset.

  A preset in the who-runs slot runs the loop under that orchestration preset: its lead is
  the agent, its lead agent: ladder is the rotation, and each iteration gets the preset's role
  routing + wrappers ('coop help presets'). With no preset, the loop rotates the agent's
  default model across all signed-in accounts.

  The target is a one-off ladder for this run (no preset needed): a bare provider
  (claude) fans the agent's default model across all signed-in accounts, claude:opus
  pins the model, claude@work,personal is an explicit account ladder — the loop rotates
  the rungs on a rate limit. Below a rung's own model sits the account's marked default
  ('coop models'), then COOP_<AGENT>_MODEL — so overnight runs can grind on a cheaper model.

  --peer <peer>… lets each iteration ask NAMED peers for a read-only second opinion
  (repeatable; coop-consult on PATH, only those peers' credentials mounted) — the
  orchestrator pattern running unattended. Off by default: it widens each box's
  credential scope to exactly the named peers. Also on fork loops:
  coop fork <name> <target> --loop --peer codex --peer gemini.

  On macOS, coop holds a caffeinate assertion for the run so the machine doesn't
  idle-sleep mid-drain and stall an overnight loop (COOP_CAFFEINATE=0 to disable).
  Set COOP_SPINNER=0 to freeze live spinners and suppress the fast repaint ticker while
  debugging or recording the terminal.

  Ctrl-C is a soft stop: the current iteration finishes and commits, then the loop
  stops before claiming the next task. Press Ctrl-C again to stop now (tearing the
  running box down). (A detached fork has no terminal — stop it with 'coop fork stop'.)

  Defaults to .agent/tasks/ — or, in a monorepo, every queue named by the top-level
  .agent/project.yaml ('subprojects:' + the root's own), so one loop drains all the
  components' work with no setup. Repeat --tasks (or set COOP_TASKS) to override the
  set; the loop keeps going while any queue has unfinished work. The whole repo is
  mounted either way.

  --preflight       run the pre-loop tidy: coop itself returns blocked/ tasks whose decision
                    now has an answer to todo — host-side, no box; an agent runs only for a
                    loop.yaml preflight.prompt cleanup (default it on with preflight.enabled;
                    --no-preflight overrides). Makes no code changes or commits.
  --no-mcp          run this loop's boxes without the shared MCP config (the committed form
                    is loop.yaml mcp: false)
  --debug-on-fail   on a failure at a terminal, open a box shell, then retry
                    on exit (a no-op in unattended runs)

  Exit codes: 0 = queue verified done; 1 = failure; 2 = usage; 3 = stopped with a task
  blocked on a human decision (including one the review kept reopening past the round cap)
  — resolve with 'coop tasks decisions', then re-run. So cron/CI can branch without parsing
  output.

  loop.yaml work.command overrides the per-iteration command.`,

	"up": `coop up — start the repo's sibling services so the box can reach them by name.

  Usage: coop up

  Brings up the services in .agent/compose.yml on coop's network; an agent in
  the box reaches db/redis/... by hostname. Stop them with: coop down`,

	"down": `coop down [-v] — stop the repo's sibling services.

  Usage: coop down [-v | --volumes]

  -v, --volumes   also remove the services' volumes (their data)`,

	"init": `coop init [--stack asdf] — scaffold coop's working set into the repo.

  Usage: coop init [--stack asdf] [--services postgres,redis] [--agents claude,codex|all]

  Per-agent dirs (.claude/.codex/.gemini) are scaffolded only for the agents you're
  signed in to — or the --agents list ("all" for every one). A repo you keep on
  only .agent/ still works: a box synthesizes missing skills from .agent/skills on
  demand, and Claude also gets fallback settings + hooks from .agent/claude.

  Writes AGENTS.md, the .agent/ queue, the Claude + git commit hooks, the
  workflow skills, and two starter subagents for the orchestrator pattern —
  .claude/agents/deep-reasoner.md (pinned to Opus, for reasoning-heavy phases)
  and fast-worker.md (pinned to Sonnet, for mechanical work) — so a lead on a
  bigger model spends its tokens on planning and synthesis (see the README's
  "orchestrator pattern"). The commit hooks' format gate matches the repo's stack —
  detected from go.mod / *.tf / mix.exs / Cargo.toml or .tool-versions (gofmt,
  terraform fmt, mix format, cargo fmt). With nothing detected the gate is left
  neutral (it imposes no checks); at a terminal it asks which gate to add. A
  .tool-versions (or --stack asdf) also scaffolds an asdf Dockerfile.agent.
  Sibling services (db/redis) are opt-in: at a terminal it asks which to add as a
  .agent/compose.yml — none by default, or pass --services. If the repo already has
  its own Docker and no Dockerfile.agent yet, it suggests how to build the box on it.
  Also seeds an empty ~/.config/coop/agents/mcp.json (the shared MCP source of truth,
  inert until you add a server) so there's an obvious place to declare MCP servers.
  Writes the TOP-LEVEL .agent/project.yaml (committed): in a monorepo — detected by
  direct child dirs that are themselves coop projects (each with a .agent/) — it lists
  them under 'subprojects:' so coop aggregates their task queues automatically (no
  COOP_TASKS). coop init scaffolds each member with ONLY its own task queue + backlog
  (it shares the root's AGENTS.md/.claude); the member's queue is for its own work, the
  root's for changes spanning members. A single repo gets a project.yaml template with
  commented serve/subprojects examples. The .gitignore ignores .agent/ state at any
  depth (**/.agent/*) and commits knowledge + adapters — rules/skills/presets/claude/
  loop.yaml — at any depth too (a large member MAY add its own), keeping only project.yaml
  top-level. Never clobbers existing files.`,

	"doctor": `coop doctor — prove the box's isolation: attack it, inside and from the host.

  Usage: coop doctor

  Runs the escape/leak checks — secret shadowing, network limits, host reach,
  the fork handoff — and prints a pass/fail report. Honors COOP_RUNTIME.`,

	"build": `coop build — build the box image (stable, pinned).

  Usage: coop build

  Builds the shared base, or a per-project image if the repo has a
  Dockerfile.agent — pinning versions for reproducibility. Re-run after
  changing Dockerfile.agent or .tool-versions. For the latest, use coop update.

  New runs use the fresh image automatically. Editor sessions (coop acp) are
  restarted onto it transparently — they reconnect, so you don't lose the session.
  Other running boxes (a loop or an interactive agent session) keep the old image
  until they next start.`,

	"update": `coop update — self-update coop, then rebuild the box image fresh.

  Usage: coop update [--self-only | --box-only | --check]

  First replaces the coop binary with the latest GitHub release — fetched and
  verified the same way the installer does (checksum + cosign), then swapped in
  atomically so replacing the running binary is safe. Then rebuilds the image like
  coop build but --pull --no-cache and unpinned, so the node base and agent CLIs
  jump to latest. Use coop build for a reproducible image. Supervised editor
  sessions are restarted onto the new image transparently, same as build.

    --self-only   update just the coop binary, skip the image rebuild
    --box-only    rebuild just the image, skip the self-update (the old behavior)
    --check       dry-run: report the binary vs the latest release and the box
                  image's build age/staleness, changing nothing (no runtime needed)

  A dev/source build, an already-current binary, or a coop installed somewhere
  unwritable (a package-manager prefix) skips the self-update with a note.

  Once a day, any TTY command also checks for a newer release in the background
  and mentions it after the command's output; COOP_NO_UPDATE_CHECK=1 turns that
  notice off.`,

	"version": `coop version — print coop's version and exit.

  Usage: coop version

  Prints coop's build version (git tag or commit). Takes no arguments; -v and
  --version are aliases.`,

	"completion": `coop completion <bash|zsh> — print a shell completion script.

  Usage: coop completion <bash|zsh>

  bash: coop completion bash > ~/.local/share/bash-completion/completions/coop
  zsh:  coop completion zsh  > "${fpath[1]}/_coop"   (then restart your shell)

  Completes commands and verbs, and — via a hidden 'coop __complete' — live values
  (fork names, task ids, credentials), all from local reads.`,
}

// printCommandHelp prints one subcommand's focused help: synopsis line bolded, body as-is,
// then a pointer to the full command list.
func printCommandHelp(text string) {
	p := ui.For(os.Stdout) // stdout view — keep pipes clean
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		fmt.Println(p.Bold(text[:i]) + text[i:])
	} else {
		fmt.Println(p.Bold(text))
	}
	fmt.Println("\nRun 'coop help' for all commands.")
}

// tildeify shortens a path under the home dir to ~/… for readable help.
func tildeify(path string) string {
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, path); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(filepath.Join("~", rel))
		}
	}
	return path
}
