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
// subcommand. See .agent/kb/rules/bare-subcommand-shows-help.md.
func groupHelp(cmd string) (int, error) {
	if h, ok := commandHelp[cmd]; ok {
		printTopicHelp(cmd, h)
		return 0, nil
	}
	return 2, fmt.Errorf("no help registered for command group %q", cmd)
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
	// One blank line separates who this is from the usage contract; the FIRST RUN
	// line and every group below keep their own spacing.
	fmt.Fprint(&b, "\nUsage: coop <command> [<args>...]\n")
	// A newcomer (no agent signed in) gets the day-one order up front. Pure-local check (no runtime),
	// so `coop help` still works before Docker exists — same state-aware style as the up/down rows below.
	if !ref && !anyAgentSignedIn(cfg) {
		fmt.Fprintf(&b, "\n%s  set up in order:  coop build → coop login <agent> → coop doctor\n", p.Bold("FIRST RUN"))
	}

	group("AGENTS")
	row("coop <target>", "agent target in a box")
	row("coop <preset>", "run a preset interactively (its lead leads)")
	row("coop acp <target|preset>", "serve as an editor agent (ACP; e.g. Zed)")
	row("coop <target> --peer <target>...", "start with read-only peer agents")
	row("coop <target> --readonly", "read the repo, write only to scratch")
	row("coop <target> --bare", "Q&A only: no repo, no context, no tools")

	group("CREDENTIALS, MODELS & PRESETS")
	row("coop login <agent>", "sign in an agent (a subscription)")
	row("coop credentials [<agent>]", "the accounts Coop can use")
	row("coop models [<agent>]", "the model menu per agent")
	row("coop presets [<preset>]", "orchestration recipes (lead + roles)")

	group("THE BOX")
	row("coop run -- <cmd...>", "run a raw command in the box")
	row("coop shell", "an interactive shell in the box")

	group("FORKS — review and land work like a PR")
	row("coop fork <name>", "open/re-enter; run a target or preset")
	row("coop fork ls", "list this repo's forks")
	row("coop fork review <name>", "show a fork's review dossier + diff")
	row("coop fork merge <name>", "rebase the fork onto your branch and land it")
	row("coop fork merge --all", "rebase and land every fork")
	row("coop fork logs [<name>]", "tail a fork's loop log (no name: every fork)")
	row("coop fork rm <name>", "discard a fork")
	row("coop fork stop <name>", "stop a detached loop")
	row("coop fork open <name>", "open the fork in your editor")
	row("coop fork path <name>", "print the fork's filesystem path")

	group("UNATTENDED")
	row("coop loop [<target|preset>]", "work the queue(s) until done, then sign off")

	group("TASKS — a folder-per-task queue in .agent/tasks/")
	row("coop tasks ls", "show the queue, grouped by state")
	row("coop tasks watch [--json]", "canonical tasks + every sandbox, live")
	row("coop tasks add \"<title>\"", "add a task (then claim/block/unblock/done)")
	row("coop tasks decisions", "what's blocked on a decision (-i to answer)")
	row("coop tasks flags", "tasks that changed what runs on your machine")
	row("coop context", "compile the docs relevant to touched paths")
	row("coop backlog", "park unscheduled ideas; promote when ready")

	group("SESSIONS — LOCAL REMOTE-SESSION CONTROLLER")
	row("coop sessions serve", "run the session controller on Unix")
	row("coop sessions doctor", "check the session controller Unix socket")
	row("coop sessions policies", "print trusted policy digests for workers")
	row("coop sessions compact", "back up and compact turn retry receipts")
	row("coop worker connect", "outbound worker: join a fleet controller")

	group("SERVICES — the box's .agent/compose.yml sidecars")
	// `coop up`/`down` act on this repo's .agent/compose.yml — always NAME the file (it's what makes
	// the rows obvious), listing its real services when present, and dim the pair when there's none to
	// act on. Repo resolution is best-effort (help runs anywhere; outside a repo, or with no compose).
	repo, _ := box.ResolveRepo(cfg.RepoOverride)
	switch {
	case ref: // the reference form is machine-independent: no per-repo service list
		row("coop up", "start the .agent/compose.yml services")
		row("coop down", "stop the .agent/compose.yml services")
	case len(scaffold.ComposeServiceNames(box.ComposeFile(repo, repo))) > 0:
		services := scaffold.ComposeServiceNames(box.ComposeFile(repo, repo))
		row("coop up", "start the .agent/compose.yml services ("+strings.Join(services, ", ")+")")
		row("coop down", "stop the .agent/compose.yml services")
	default:
		dimRow("coop up", "none in .agent/compose.yml yet")
		dimRow("coop down", "stop the .agent/compose.yml services")
	}

	group("SAFETY — prove the box holds, catch committed secrets")
	row("coop doctor", "attack the box, prove isolation holds")
	row("coop check-secrets", "scan the working tree for committed secrets")
	row("coop net", "control network access, see what runs reach")

	group("SETUP & MAINTENANCE")
	row("coop init [--stack asdf]", "scaffold queue, hooks, skills, agent dirs")
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

// anyAgentSignedIn reports whether any agent has a signed-in default or named credential.
// Pure-local (reads the config dir + env file, no runtime), so `coop help` can key its
// FIRST RUN hint on it without breaking the runtime-free help path.
func anyAgentSignedIn(cfg *config.Config) bool {
	for _, agent := range agents.Names() {
		for _, p := range box.EffectiveProfiles(cfg, agent) {
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
	b.WriteString("\n" + sourceTreeConformance + "\n")
	return b.String()
}

// runHelp is `coop run`'s page. It's deliberately NOT in commandHelp: the dispatch's help check
// is `--`-blind, so `coop run -- --help` (which must run --help in the box) would otherwise print
// this instead. cmdRun (which honors --) and helpForCommand print it directly.
const runHelp = `coop run — run a raw command in the box.

  Usage: coop run [--readonly|--bare] -- <cmd...>

  Everything after -- runs verbatim in the sandbox — same mounts, secret-shadowing,
  and network as an agent. "coop run echo hi" works too; use -- when the command has
  flags coop would otherwise read (e.g. coop run -- npm test --watch).

  --readonly and --bare run the command under the restricted profile of the same-named
  agent runs ('coop help claude'): a read-only root, the repo read-only or absent, scratch
  in memory only. The deterministic way to prove what such a run can and cannot write.`

const sourceTreeConformance = `SOURCE-TREE CONFORMANCE

  From a source checkout, 'make check' is the blocking no-credential gate. Focused
  deterministic targets are 'make provider-scripted-e2e', 'make acp-scripted-e2e',
  and 'make live-process-control'. Real isolation checks are 'make doctor' and
  'make review-writes-e2e'.

  'make provider-live-e2e', 'make provider-resume-live-e2e',
  'make provider-loop-live-e2e', 'make provider-consult-live-e2e', and
  'make acp-e2e' are opt-in because they use installed CLIs, configured credentials,
  and real quota. Strict '-all' forms, request counts, summaries, and triage are in
  README.md under Layout & development.`

// agentHelp is `coop help <agent>` — how to run THAT agent, in its own words: the command a
// person types, four examples, the coop flags read before a `--`, and where its models and
// accounts live. It documents coop's OWN flags because `coop <agent> --help` forwards to the
// agent's real CLI. Every line is generated from the adapter (name, effort support, restricted
// modes), so a new agent gets its page for free and none of them advertises a flag it refuses —
// see .agent/kb/rules/agents-are-one-file.md. Orchestration lives in `coop help presets`, the
// target grammar in `coop help models`; neither is restated here.
func agentHelp(name string) string {
	ag, ok := agents.Get(name)
	if !ok {
		return ""
	}
	title := titleName(name)
	target := name + "[:<model>]"
	example := name + ":" + ag.ExampleModel()
	account := example + "@work"
	if agents.SupportsEffort(ag) {
		target += "[/<effort>]"
		account = example + "/high@work"
	}
	restricted := restrictedModesOffered(ag)

	var b strings.Builder
	fmt.Fprintf(&b, "coop %s — run %s in a sandboxed box\n\n", name, title)
	fmt.Fprintf(&b, "Usage:\n  coop %s[@<account>] [options] [-- <%s-args>...]\n\n", target, name)
	fmt.Fprintf(&b, "Examples\n  coop %s\n  coop %s\n  coop %s\n  coop %s -- --help\n\n", name, example, account, name)

	options := [][2]string{{"--peer <target>", "start with a read-only peer agent; repeat to add more"}}
	if restricted {
		options = append(options,
			[2]string{"--readonly", "mount the repository read-only"},
			[2]string{"--bare", "run without the repository, project context, or tools"})
	}
	options = append(options, [2]string{"--", "pass all remaining arguments directly to " + title})
	b.WriteString("Options\n")
	b.WriteString(helpRows(options, 2))
	if restricted {
		b.WriteString("\n  --readonly and --bare cannot be combined or used with peers.\n")
	}

	b.WriteString("\nModels and accounts\n")
	b.WriteString(helpRows([][2]string{
		{"coop models " + name, "list " + title + " models"},
		{"coop credentials " + name, "list " + title + " accounts"},
		{"coop login " + name, "sign in to " + title},
	}, 3))
	b.WriteString("\nFor a guide to using multiple models and providers together:\n  coop help presets")
	return b.String()
}

// helpRows renders a two-column block: every description starts past the widest command cell,
// measured on plain text (see .agent/kb/rules/no-color-in-width-fields.md). gap is that block's
// own column gap — the approved page sets the flags tight (2) and the commands airier (3),
// because a flag column is read down and a command column is read across.
func helpRows(rows [][2]string, gap int) string {
	w := 0
	for _, r := range rows {
		if n := utf8.RuneCountInString(r[0]); n > w {
			w = n
		}
	}
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "  %s%s%s\n", padRight(r[0], w), strings.Repeat(" ", gap), r[1])
	}
	return b.String()
}

// restrictedModesOffered asks the adapter the same question a launch asks — an agent whose CLI
// has no proven switch refuses ModeReadOnly — so help never advertises a mode that would be
// rejected the moment someone typed it.
func restrictedModesOffered(ag agents.Agent) bool {
	_, err := ag.RestrictedCommand(agents.ModeReadOnly, nil)
	return err == nil
}

// commandHelp is the focused text for `coop <cmd> --help`, per subcommand. fork has its
// own richer forkHelp, run has runHelp, and the agents use agentHelp (their `--help` forwards to
// the agent's own CLI). Each value's first line is the synopsis.
var commandHelp = map[string]string{
	"worker": `coop worker — connect one private Coop daemon to Responder.

  Usage: coop worker connect --config <absolute-path>

  The connector opens one outbound mutual-TLS HTTPS poll stream and maps only
  versioned Responder commands to the owner-private Coop Unix API. It journals
  each command before execution and resends terminal results until Responder
  durably acknowledges them. It never opens an inbound TCP port or accepts a
  generic shell command. Populate policy_digests and policy_authority_digests
  from the exact output of 'coop sessions policies --json'.`,
	"sessions": `coop sessions — serve and inspect local remote sessions.

  Usage: coop sessions serve [--state <path>] [--policies <path>] [--socket <path>]
         coop sessions doctor [--socket <path>] [--json]
         coop sessions policies [--policies <path>] [--json]
         coop sessions compact [--state <path>] --backup <path>

  'serve' owns the state root and exposes the v1 JSON API over an owner-only Unix
  socket. It never listens on TCP. 'doctor' checks only that Unix socket and exits
  nonzero when the service is unavailable or unready. 'policies' validates the
  trusted policy file and prints the immutable policy and model-independent authority
  digests a fleet worker must advertise; JSON output contains policy_file,
  policy_digests, and policy_authority_digests. 'compact' stops
  if the state root is active, writes and verifies a new SQLite backup, replaces
  legacy full-turn retry receipts with prompt-free receipts, checks integrity,
  then vacuums reclaimed pages. It never overwrites the backup path.

  A policy's target: takes one target or a LIST of up to 4 — a fallback ladder, and
  it may cross providers: [codex:gpt-5.6-sol/xhigh@oncall, claude@oncall]. A rate
  limit moves the session to the next free rung and re-delivers the same turn; when
  every rung is cooling the turn fails 'rate_limited' for the client to retry.

  Defaults:
    state    ~/.local/state/coop/sessions
    policies ~/.config/coop/session-policies.yaml
    socket   <state>/control.sock`,
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
  range containing a merge commit. Signing runs in an isolated linked worktree,
  verifies that commit trees are unchanged, then compare-and-swaps the branch ref;
  staged, unstaged, and untracked files in your active checkout remain untouched.
  'coop loop' signs each cycle automatically when you sign by default
  (commit.gpgsign=true).`,
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

  Usage: coop shell [--readonly|--bare]

  A shell in the sandbox at your repo — same mounts, secret-shadowing, and
  network as an agent run. Exit to return. --readonly and --bare open it under
  the restricted profile of the same-named agent runs ('coop help claude').`,

	"login": `coop login <agent> — sign in to an agent (token persists in the config dir).

  Usage: coop login <agent>[@<account>]

  Runs the agent's sign-in (paste a code, no browser). Re-run any time to
  refresh or switch accounts — e.g. after a usage limit.

  @account signs in a second (or third) account under a name, so one agent can
  hold several subscriptions: coop login claude@work. An unattended loop rotates
  across all of them when one is rate limited (a bare model in a preset's lead agent:
  ladder fans out over every account). Without @account the sign-in targets the default.`,

	"credentials": `coop credentials — list stored credentials; a path grammar edits one.

  Usage: coop credentials [<agent> [<credential>]]
         coop credentials <agent> <credential> default
         coop credentials <agent> <credential> rm

  A CREDENTIAL is one stored account/login — a rate-limit slot. Orchestration
  recipes are PRESETS; see coop help presets.
  Each token narrows: no args lists every agent, an agent lists its credentials
  (which one runs by default, when each was last refreshed), a credential shows
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
  any agent launch: 'coop claude@work', 'coop claude@work --peer codex', and
  'coop acp claude@work' (so an editor entry can pin an account).`,

	"models": `coop models [<agent>] — the model menu per agent.

  Usage: coop models [<claude|codex|gemini|grok>] [--refresh]

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
  'coop claude:opus --peer codex', 'coop loop claude:haiku',
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

	"acp": `coop acp <target|preset> — serve as an ACP agent over stdio (for editors).

  Usage: coop acp <target|preset> [--peer <target>...]

  Speaks the Agent Client Protocol on stdin/stdout. Point your editor's ACP
  command at e.g. ["acp","claude"] — one entry per target or preset.
  The initial target is required; credential order is never launch intent. The
  PROVIDER dropdown can still switch a plain session live. coop always proxies
  the session, so the editor stays connected across a box restart (a rebuild/OOM)
  — it reconnects and replays the handshake, no lost session.

  coop owns the editor's toolbar: it runs the agent in yolo mode (the box is the
  sandbox, so no permission prompts), defaults the model dropdown to coop's model,
  drops the permission-mode and subagent pickers, and gives a normal session three
  dropdowns — PRESET (the recipe), PROVIDER (who runs), and ACCOUNT (the lead's login).
  An active preset is different: only PRESET renders because its ladder owns the provider,
  model, effort, account, and roles. Persisted Provider/Account sets while those controls
  are hidden are acknowledged and ignored. Selecting None returns the normal Provider and
  Account controls with the effective provider and Account set to Auto; select None first
  whenever you want to choose a Provider or Account manually.
  In a plain session, an account or same-provider switch is transparent — the conversation is
  preserved (a shared, credential-independent session store). A PROVIDER switch (picking another
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
  preset (its lead is the agent; see 'coop help presets').

  --peer <target>... lets the session ask NAMED peers for a read-only second opinion
  (repeatable; only those peers' credentials are mounted) — the orchestrator pattern,
  from your editor. Preset consult roles use the same read-only wrapper and keep their
  role names, target ladders, and personas.

  --bare serves a Q&A agent with no repository, no project context and no tools: the
  shared base image under the restricted profile of 'coop <target> --bare', a target only
  (no preset, no --peer), and no supervisor or toolbar — there is nothing to switch. Its
  no-tools switch rides the ACP session/new the client sends, so it is what 'coop sessions
  serve' launches for a bare policy; an editor entry gets the same box. --readonly is not
  offered here: a fork fronts read-only with 'coop fork <name> acp <target> --readonly'.

  To make plain provider switching near-instant, coop keeps a box warm per OTHER signed-in
  provider (spawned in the background at session start), so a switch pays only the ACP
  replay, not a container + adapter cold-boot. Set COOP_ACP_WARM=0 to disable
  prewarming (one fewer idle box per signed-in provider) on a low-RAM machine.

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

	"presets": `coop presets — configure multiple models and providers to work together

Usage:
  coop presets                 list presets
  coop presets <name>          show a preset
  coop presets init [<name>]   create a preset (default: frontier)
  coop help <name>             explain a preset

Create a preset
  coop presets init
  coop presets init review

Run a preset
  coop frontier
  coop loop frontier
  coop acp frontier
  coop fork risky frontier --loop

How to define a preset

  A preset is a YAML file that defines:
  - One lead agent.
  - Optional roles for focused work.
  - Optional prompt extensions.

  When you run a preset, Coop starts the lead and tells it which roles are
  available and when to use them. Each role runs with its configured agent,
  model, mode, and prompt. The lead combines their work into the final result.

  Syntax:

    agent:   presets use Coop's standard model, effort, account, and
             automatic-rotation syntax. For details, see:
               coop help models

    mode:    controls how a role works
               native    runs inside the lead agent's session
               consult   provides read-only advice from another agent
               delegate  edits files for the lead; never commits; runs one at a time

    when:    tells the lead when to use a role

    prompt:  adds custom instructions to Coop's generated instructions for the
             lead or role

    If the lead does not support native roles, they run as consult roles.

  Where presets live:
    Project   .agent/presets/<name>/preset.yaml
    Global    ~/.config/coop/presets/<name>/preset.yaml

  A project preset overrides a global preset with the same name.`,

	"tasks": `coop tasks — drive the task queue (a folder per task under .agent/tasks/).

  Usage: coop tasks [--tasks <path>]... <command>

  ls [--all] [--todo|--in-progress|--blocked|--done]
                   list tasks by state, with counts (recent done capped; --all shows all). Pass one
                   or more state flags to show only those. Task ids link to the folder — click to open.
  watch [--json]   live board: canonical tasks + every active sandbox (auto-exits when done);
                   --json prints the same project snapshot once for automation; a queue
                   that cannot be read exits 1 instead of counting as drained
  add [--project <name>] "<title>"
                   scaffold a task folder in todo (or fill it inline: --context/--acceptance/--approach/--subtask)
  claim <id> [--as <label>] [--pid <n>] [--force]
                   claim a task before you start it (todo -> in_progress); an agent's claim binds to
                   its process (--as names it, --pid picks it, --force takes over a live claim)
  release <id>     hand back a claim without finishing it (stays in_progress; the loop can adopt it)
  lease <id> [--as <label>] [--pid <n>] [-- <command...>]
                   hold the task's work lock — the one a loop iteration holds — for a command's
                   lifetime, or until the bound process (your agent, else the task's claimant,
                   or --pid) exits or the task moves; ls/watch show 'busy <label>' and a loop
                   in this checkout skips the task meanwhile ('done' and 'block' run by the
                   same agent stop its own holder first)
  block <id>       park it on a decision (-> blocked) and write a decision.md stub
  unblock <id>     move it back to todo; add "<answer>" to record in decision.md
  done <id>        move it to done (the archive)
  path <id>        print a task's resolved folder path
  rm <id>          delete a task folder; --all-done clears the done archive
  decisions [-i]   list open decisions; -i walks them one by one to answer (records + unblocks)
  flags [<id>] [--ack]
                   tasks whose commits changed files that run on YOUR machine — git hooks and
                   attributes, editor/agent settings and hooks, compose files, the Makefile — with
                   the reason each one matters; the board marks such a task until you read the
                   diff and acknowledge it with --ack (the sandbox contains the box, not your tools)
  lint             check the tree (blocked<->decision.md, no status field, ...; exits 1)
  queues           print each configured queue's path, one per line (for scripts and the sweep guard)

  A claim made without a terminal (an agent's tool call) is bound to the claiming process — coop's
  parent, or the nearest non-shell ancestor — so 'claimed by codex (pid 812)' says who holds the
  task, 'owner process gone' says that process died, a second claim by another live process is
  refused, and 'coop loop --preflight' releases claims whose process is gone. A claim made at a
  terminal is a person's: bound to nothing, and never released by the loop. 'coop tasks lease'
  adds the live half for an agent working outside the loop: the same kernel lock a loop iteration
  holds, dying with the holder, so 'busy codex' can never outlive the process it names.

  A task's state is its directory — 00_todo/ 10_in_progress/ 50_blocked/ 99_done/, the
  numeric prefix just sorts 'ls' in lifecycle order — so each transition is a folder move.
  Removing tasks is a MANUAL step: the loop and skills only ever move a finished task to
  done, never delete it, so 'coop tasks rm --all-done' is how you prune the archive.
  Defaults to .agent/tasks/ — or, in a monorepo, every subproject's .agent/tasks listed under
  'subprojects:' in .agent/project.yaml, so you never hand-maintain COOP_TASKS. Override with
  --tasks or COOP_TASKS. Paths are repo-relative. With several queues (a monorepo), ls, lint, and decisions
  (including -i) roll up across all of them, and the id commands (claim/block/unblock/done/
  rm) find their task in whichever queue holds it — erroring only when an id matches in
  more than one queue. In a monorepo, add requires
  --project root|<subproject> so creation picks an explicit queue; a nested member takes its
  full path (terraform/environments/va1) or just its last segment (va1) when no other member
  ends in the same one. With raw --tasks overrides, add still needs a single --tasks because it
  creates into one queue. Copied queue slicing is retired: parallel fork loops schedule distinct
  tasks directly from the same canonical queue.`,

	"backlog": `coop backlog — park the genuinely LARGE as task folders (.agent/tasks/xx_backlog/).

  Usage: coop backlog [--tasks <path>]... [ls | add "<title>" | rm <id> | promote <id>]

  (bare)           list the backlog drawer
  add "<title>"    capture an idea (--context/--acceptance/--approach/--subtask fill it inline)
  promote <id>     move it into 00_todo/ when it's ready to work (then coop tasks claim)
  rm <id>          drop an idea (--yes skips the confirm)

  The backlog is the SAME task-folder format as the queue, in an xx_backlog/ drawer that lives
  OUTSIDE the lifecycle — the loop, the Stop hook, and 'coop tasks' all ignore it, so an idea
  sits here with no nagging until you promote it (a folder move, not a rewrite). This drawer is
  for work one iteration couldn't finish, or that needs a spec or a decision before anyone can
  start; everything else goes straight to the queue ('coop tasks add'), and a close call belongs
  in the queue too. Defaults to .agent/tasks/ — or, in a monorepo, every subproject's queue (see
  coop tasks): ls rolls up across them and rm/promote find the item in whichever queue holds it,
  while add needs a single --tasks.`,

	"context": `coop context — compile the committed docs relevant to a scope (instructions + rules + KB).

  Usage: coop context [--changed] [--task <id> [--tasks <path>...]] [--json | --rendered] [<path>...]

  Selects which committed docs an agent needs for the paths in play — canonical
  AGENTS.md/CLAUDE.md (always, whole) plus the .agent/project.yaml 'context.routes'
  whose globs match — so a session carries less than the whole repo's instructions.

  Scope is DETERMINISTIC (never inferred from a prompt), from any of:
    paths...         explicit repo-relative paths
    --changed        paths git reports changed (staged, unstaged, untracked)
    --task <id>      the paths a queued task declares (a 'paths:' frontmatter list)
    (current subproject, when run inside one)

  Task IDs use the same exact-then-unique-fragment matching as coop tasks. Ambiguous
  matches are errors; --tasks <path> selects a queue for --task (repeatable). Explicit
  queues override COOP_TASKS, which otherwise overrides project-derived queues.

  Output: a report of each file + the route that selected it; --json for the same
  as data; --rendered to print the compiled content itself (canonical first, whole).
  A route include that is missing or escapes the repo is an error; canonical files
  are discovered, never truncated. Config comes from the committed project.yaml (so a
  fork inherits the parent's routes); scope comes from the fork's own tree.

  Define routes in .agent/project.yaml:
    context:
      routes:
        - paths: ["portal/**", "**/*.ex"]
          include: [.agent/kb/portal.md]`,

	"check-secrets": `coop check-secrets — scan the working tree for committed secrets, by content.

  Usage: coop check-secrets [--include-ignored]

  Scans for token shapes and high-entropy values, reporting file:line. Exits
  non-zero on a hit, for use as a pre-flight or CI check. Hide a flagged file
  with .coopignore.

  By default it scans the commit-candidate files (tracked + untracked; gitignored
  excluded) — including a file coop shadows from the box by name (an id_ed25519,
  a *.pem): the box never sees it, but a push would commit it. A 'coop
  run'/'shell'/'loop' mounts the WHOLE tree, though, so a gitignored-but-not-
  shadowed file is still visible to the agent — pass --include-ignored to scan
  the full visible tree too (deps/build dirs and shadowed files git would not
  commit are still skipped). A .coopignore entry silences a file in both modes.`,

	"loop": `coop loop [<target|preset>] — work the task queue until done, then sign off.

  Usage: coop loop [<target|preset>] [--tasks <path>]... [--peer <target>...] [--max-tasks <n>] [--preflight] [--no-mcp] [--debug-on-fail] [--egress <mode>] [--allow-domain <domain>]...

  A fresh agent per iteration works the todo tasks; when the queue empties, a DEMANDING
  signoff pass (a senior reviewer's bar) re-checks each shipped task — goal met (every
  acceptance criterion + subtask), standards followed (AGENTS.md + .agent/kb/rules, no scope
  creep), the FAILURE path tested, the change polished (docs/CHANGELOG updated), plus
  bookkeeping — then runs the repo's gate ONCE across the whole repo, reopening anything
  short of "merge with no changes". If the signoff reopened work, the loop drains and
  signs off AGAIN, repeating until a signoff reopens nothing (verified done) or the round
  cap is hit — then the task it keeps reopening is blocked for a human (exit 3), not
  reported as done. A later review or verify pass that leaves work actionable exits 1 so
  automation cannot mistake it for verified done. The cap SCALES with the batch: half the tasks worked this run, clamped
  to [3, loop.yaml signoff.rounds] (default 5) — a small batch still gets a few tries, a big
  overnight batch can't ping-pong one stuck task forever. On a rate limit it rotates to the
  next target in its agent: ladder, or waits out the reset when all are limited.

  Every review closes with one AUDIT EVIDENCE line per subject and a structured PASS/FAIL receipt
  naming the exact sorted task IDs it proposes reopening. By default the whole repository,
  including task queues, is read-only. Coop validates the complete proposal, acquires host task
  authority, and applies exact-subject reopens. A successful process with malformed structured
  output gets one immediate full-review retry over the same subjects with a fixed format
  correction; both attempts are recorded. A malformed second verdict, lifecycle churn,
  interruption, failed process, or out-of-scope proposal mutates no task. writes: repo permits
  source fixes, but task lifecycle is still host-applied.

  Completion requires exactly one Coop-Task binding in the current iteration range and exactly
  one binding for that task reachable from HEAD. Reopened work must amend or rewrite the original
  task commit; a second bound commit is rejected and the task is restored to in-progress.

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
  field = the built-in default. (coop init scaffolds a commented loop.yaml.) The launch announces
  the exact loop.yaml snapshot the run derives from — a short sha256 digest, or an explicit
  absent/built-in-defaults state — and pins it for the whole run; ladders, prompts, caps, and
  writes are one coherent read. A mid-run edit never hot-reloads: before each later box launch
  coop compares the on-disk bytes with that snapshot and warns once per new digest that the run
  keeps its startup config — restart the loop to apply the change.

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
  the rungs on a rate limit. A rung without a model uses COOP_<AGENT>_MODEL, then a model
  baked into COOP_<AGENT>_CMD, then the agent CLI's own default — so overnight runs can
  grind on a cheaper model.

  --peer <target>... lets each iteration ask NAMED peers for a read-only second opinion
  (repeatable; coop-consult on PATH, only those peers' credentials mounted) — the
  orchestrator pattern running unattended. Off by default: it widens each box's
  credential scope to exactly the named peers. Also on fork loops:
  coop fork <name> <target> --loop --peer codex --peer gemini.

  On macOS, coop holds a caffeinate assertion for the run so the machine doesn't
  idle-sleep mid-drain and stall an overnight loop (COOP_CAFFEINATE=0 to disable).
  Set COOP_SPINNER=0 to freeze live spinners and suppress the fast repaint ticker while
  debugging or recording the terminal.

  Every attempt is supervised for SILENCE, not slowness: 10m to its first model action,
  30m between recognized actions, 2h on any one foreground tool. Only the provider's own
  structured stream feeds those clocks — never CPU or process names — and an open tool
  suspends the idle one, so long reasoning and a slow gate finish untouched (a provider
  whose stream reports no tool calls gets a single conservative 2h post-progress budget
  instead). Silence past a deadline kills that attempt alone: any completion it wrote is
  restored, the task stays actionable, and a fresh attempt starts — on the next rung when
  the ladder has one — while three in a row on one stage stops the run instead of
  churning. The warning names the deadline that fired and the silence it observed. There
  is no off switch: it is what stops one wedged provider CLI from holding an overnight
  drain, its task lease, and its credential until you notice.

  Ctrl-C is a soft interrupt: the current iteration finishes its completion binding,
  host signing, and mandatory between/protected audit, then exits 130 before final
  signoff or another claim. Press Ctrl-C again to stop now (tearing the running box
  down). (A detached fork has no terminal — stop it with 'coop fork stop'.)

  Defaults to .agent/tasks/ — or, in a monorepo, every queue named by the top-level
  .agent/project.yaml ('subprojects:' + the root's own), so one loop drains all the
  components' work with no setup. Repeat --tasks (or set COOP_TASKS) to override the
  set; the loop keeps going while any queue has unfinished work. The whole repo is
  mounted either way.

  A fork loop does not copy or mount those queues. The host assigns one canonical task at a
  time and exposes only that task through an execution projection. Stop/crash keeps the exact
  assignment resumable; projected done becomes a reviewed fork candidate, and the canonical
  task reaches done only when that exact candidate lands. Multiple forks may therefore share
  the same queue without duplicate work. --tasks on a fork selects one canonical queue; it does
  not create a fork-local queue.

  --max-tasks <n>   work at most N selected tasks, counting each only after it reaches done
                    or blocked following retries and its immediate audit; then pause
                    successfully before another task or final signoff; N must be positive,
                    and an empty actionable queue starts no box
  --preflight       run the pre-loop tidy: coop itself returns blocked/ tasks whose decision
                    now has an answer to todo — host-side, no box; an agent runs only for a
                    loop.yaml preflight.prompt cleanup (default it on with preflight.enabled;
                    --no-preflight overrides). Makes no code changes or commits.
  --no-mcp          run this loop's boxes without the shared MCP config (the committed form
                    is loop.yaml mcp: false)
  --debug-on-fail   on a failure at a terminal, open a box shell, then retry
                    on exit (a no-op in unattended runs)
  --egress <mode>   open | filtered | none for this run ('coop help net'). Restricted egress
                    is admitted ONCE at the start: the operator's grants, the project's
                    approved requests, and the core endpoints of every rung the run may
                    rotate onto are frozen into one policy every iteration, review and
                    pre-flight box launches under. Refusals print between iterations and
                    once more at the end; 'coop net runs' has every run
  --allow-domain    an exact name this run may reach over TLS 443 (repeatable)
  --egress-rules    a file of universal rules for this run only

  Exit codes: 0 = queue verified done or an intentional --max-tasks pause; 1 = failure;
  2 = usage; 3 = stopped with a task blocked on a human decision (including one the
  review kept reopening past the round cap) — resolve with 'coop tasks decisions', then
  re-run; 130 = interrupted before queue verification. So cron/CI can branch without
  parsing output.

  loop.yaml work.command overrides the per-iteration command.`,

	"up": `coop up — start the repo's sibling services so the box can reach them by name.

  Usage: coop up

  Brings up the services in .agent/compose.yml on coop's network. The final
  status names the exact resolved Compose services, in Compose order; an agent
  in the box reaches each one by that hostname. Stop them with: coop down`,

	"down": `coop down [-v] — stop the repo's sibling services.

  Usage: coop down [-v | --volumes]

  -v, --volumes   also remove the services' volumes (their data)`,

	"init": `coop init — set up Coop in this project

Usage:
  coop init
  coop init [--stack asdf] [--services <service,...>] [--agents <agent,...>|all]

Coop sets up

- Shared instructions and skills for all your AI agents.

- A file-based task system — agents can pick up work, save progress, and hand
  it to another agent or session.

- Commit checks — Coop verifies formatting for the languages it detects or
  you choose.

- Project settings for sandboxed runs, including filtered internet access.

- An optional starter Docker Compose file for supporting services; you can
  extend it with anything else your project needs.

Without options, Coop detects what it can and asks before initializing Git,
adding formatting checks, or adding Postgres or Redis.

Options
  --agents <list>    set up Claude, Codex, Gemini, or all
                     default: agents you are signed in to

  --services <list>  add Postgres, Redis, or both

  --stack asdf       install tools from .tool-versions in the Coop box

You can run coop init again at any time.
Coop keeps your existing project files and adds anything missing.`,

	"net": `coop net — control network access and see what happened.

  Usage: coop net [<command>]

  ACCESS — control what new runs can reach
    coop net                  show this project's current access
    coop net approve          review and approve this project's network access
    coop net check <url>      can a new run reach it? (--run <run>: that run)
    coop net forget           drop this project's approval (--project <path>)

  RUNS — inspect recorded network activity
    coop net runs             show recent runs (--all, --all-projects)
    coop net inspect [<run>]  show connections and blocked access (--json)
    coop net explain <host>   explain why access was blocked (--run <run>)
    coop net watch [<run>]    follow an active run until its record is sealed
    coop net export <run>     export a shareable record (destinations withheld)

  REPAIR — normally automatic
    coop net setup            prepare or recheck this host now
    coop net recover [<run>]  retry interrupted cleanup now

  A filtered box reaches its agent's provider and the websites and services this
  project asked for — nothing else. Every other destination is blocked at the
  gateway, not inside the box; the box is told its policy up front, and the run
  ends with what it reached and what was blocked. A loop admits ONCE and every
  iteration, review and pre-flight box runs under that one frozen policy.

  Ask for it on a launch, or make it the project's default:

    coop claude --egress filtered
    coop run --egress filtered --allow-domain example.com -- curl https://example.com

  A project asks for websites and services in .agent/project.yaml:

    box:
      egress: filtered
      egress_rules:
        - to: {domain: "docs.example.com"}    # TLS, exact or *.wildcard
          protocol: tls
          ports: [443]

  That is a REQUEST. 'coop net approve' shows it against what is already
  approved and, once you confirm at a terminal, stores your decision outside
  the repository — so editing or deleting the file cannot widen access, and an
  unattended run can never approve itself. Approvals apply to NEW runs; boxes
  already running keep the policy they launched with. 'forget' takes one back.

  Runs are named by any unique prefix of their id — the eight characters
  'coop net runs' shows are enough. 'inspect' with no run reads the newest one
  recorded for this project; 'explain' with no run finds the newest time that
  host was blocked here and, when the evidence proves the name, port and
  protocol, prints the exact rule to paste. Unknown means unknown: a metric
  nobody measured is never shown as zero. Setup and recovery run on their own;
  the REPAIR verbs are for doing it now. Protocols, ports and what is refused
  by design are in docs/networking.md.`,

	"doctor": `coop doctor — prove the box's isolation: attack it, inside and from the host.

  Usage: coop doctor

  Runs the escape/leak checks — secret shadowing, network limits, host reach,
  the fork handoff — and prints a pass/fail report. Probes the image this
  repo's boxes run: its per-project image when built, else the shared base
  image, else a stock alpine stand-in (which skips the USER/toolchain checks
  and says so). Honors COOP_RUNTIME.`,

	"build": `coop build — build the box image (stable, pinned).

  Usage: coop build

  Builds the shared base, or a per-project image if the repo has a
  .agent/Dockerfile — pinning versions for reproducibility. Re-run after
  changing .agent/Dockerfile or .tool-versions. For the latest, use coop update.

  New runs use the fresh image automatically. Editor sessions (coop acp) are
  restarted onto it transparently — they reconnect, so you don't lose the session.
  Other running boxes (a loop or an interactive agent session) keep the old image
  until they next start.`,

	"update": `coop update — self-update coop, then rebuild the box image fresh.

  Usage: coop update [--self-only | --box-only | --check]

  First replaces the coop binary when GitHub has a newer release — its versioned
  archive is verified against the release checksum locally, then swapped in
  atomically so replacing the running binary is safe. Then rebuilds the image like
  coop build but --pull --no-cache and unpinned, so the node base and agent CLIs
  jump to latest. Use coop build for a reproducible image. Supervised editor
  sessions are restarted onto the new image transparently, same as build.

    --self-only   update just the coop binary, skip the image rebuild
    --box-only    rebuild just the image, skip the self-update (the old behavior)
    --check       dry-run: report the binary vs the latest release and the box
                  image's build age/staleness, changing nothing (no runtime needed)

  A dev/source build, an already-current or newer binary, or a coop installed
  somewhere unwritable (a package-manager prefix) skips the self-update with a note.

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
  zsh:  coop completion zsh > "${fpath[1]}/_coop"
        then add 'source "${fpath[1]}/_coop"' AFTER compinit in ~/.zshrc. Existing
        file-only installs must add this source line; it enables command-local nocorrect
        for coop arguments without changing Zsh correction for other commands.

  Completes commands and verbs, and — via a hidden 'coop __complete' — live values
  (fork names, task ids, credentials), all from local reads.`,
}

// printCommandHelp prints one subcommand's focused help: synopsis line bolded, body as-is,
// then a pointer to the full command list.
func printCommandHelp(text string) {
	printHelpPage(text)
	fmt.Println("\nRun 'coop help' for all commands.")
}

// printHelpPage prints a page that ENDS ITSELF: synopsis line bolded, body as-is, and no
// all-commands footer. A page that closes with its own pointer ("coop help presets") has already
// told the reader where to go next; the generic footer would be a second, weaker answer.
func printHelpPage(text string) {
	p := ui.For(os.Stdout) // stdout view — keep pipes clean
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		fmt.Println(p.Bold(text[:i]) + text[i:])
	} else {
		fmt.Println(p.Bold(text))
	}
}

// selfContainedHelp are the commandHelp pages that end with their own pointer, so they print
// without the all-commands footer. Membership travels with the page's last line.
var selfContainedHelp = map[string]bool{"presets": true}

// printTopicHelp is the ONE way a static page reaches the terminal, so `coop presets --help`,
// `coop help presets` and a bare group can never disagree about the footer.
func printTopicHelp(cmd, text string) {
	if selfContainedHelp[cmd] {
		printHelpPage(text)
		return
	}
	printCommandHelp(text)
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
