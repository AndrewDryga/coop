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
	"github.com/AndrewDryga/coop/internal/project"
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

// renderHelp renders the top-level command menu. ref=true is the DETERMINISTIC reference form for
// docs/`coop help --all`: forced no-color, no state-aware GET STARTED block, and the generic
// service rows with no per-project names or dimming — so the output is identical on every machine
// (gendocs -check depends on it). ref=false is the live, state-aware terminal view.
func renderHelp(cfg *config.Config, ref bool) string {
	p := ui.For(os.Stdout) // help is a stdout view — gate color on stdout so a pipe stays clean
	if ref {
		p = ui.Palette{} // forced plain: the reference must be byte-identical regardless of the terminal
	}
	return renderMenu(p, cfg, ref)
}

// renderMenu is renderHelp with its palette supplied, so a test can prove both what a terminal
// shows (the dimmed rows) and what a pipe does (the same columns, no escapes).
func renderMenu(p ui.Palette, cfg *config.Config, ref bool) string {
	var b strings.Builder
	// A group heading is an UPPERCASE name plus one short explanation after an em dash; only the
	// name is bold, so the explanation reads as ordinary prose. Where it helps a newcomer find the
	// feature, the explanation NAMES the file or directory the commands act on.
	group := func(name, about string) { fmt.Fprintf(&b, "\n%s — %s\n", p.Bold(name), about) }
	// row keeps a column gap even when a command is long, so a description never glues to it.
	// Width is counted in runes, not bytes, so a command with a "…" doesn't shift its column.
	pad := func(cmd string) string {
		gap := 34 - utf8.RuneCountInString(cmd)
		if gap < 2 {
			gap = 2
		}
		return cmd + strings.Repeat(" ", gap)
	}
	row := func(cmd, desc string) { fmt.Fprintf(&b, "  %s%s\n", pad(cmd), desc) }
	// dimRow is row for a command with nothing to act on in this project yet (`coop up` with no
	// services): the WHOLE line recedes, so the available next action beside it stands out. The gap
	// is computed on plain text and dimmed after, so the column still aligns — and Dim is a no-op
	// when color is off, so a pipe keeps the same text.
	dimRow := func(cmd, desc string) { fmt.Fprintf(&b, "  %s\n", p.Dim(pad(cmd)+desc)) }

	providers := make([]string, 0, len(agents.Names()))
	for _, name := range agents.Names() {
		providers = append(providers, titleName(name))
	}
	if ref { // the reference omits the build version — its bytes must not depend on the tag/commit
		fmt.Fprintf(&b, "%s — run a coding agent all night long in a box it can't escape.\n", p.Bold("coop"))
	} else {
		fmt.Fprintf(&b, "%s %s — run a coding agent all night long in a box it can't escape.\n", p.Bold("coop"), resolveVersion())
	}
	// One blank line separates who this is from the usage contract; the GET STARTED
	// block and every group below keep their own spacing.
	fmt.Fprint(&b, "\nUsage: coop <command> [<args>...]\n")
	// A newcomer with no usable account gets the two commands that start the day, in order. Pure-local
	// check (no runtime), so `coop help` still works before Docker exists — same state-aware style as
	// the service rows below.
	if !ref && !anyAgentSignedIn(cfg) {
		group("GET STARTED", "sign in, then start an agent")
		row("coop login <agent>", "sign in to "+ui.List(providers, "or"))
		row("coop <"+strings.Join(agents.Names(), "|")+">", "start your agent")
	}

	group("THE BOX", "an isolated environment for running commands in this project")
	row("coop run -- <command>", "run a command in the box")
	row("coop shell", "open a shell in the box")

	group("RUN AGENTS", "work with a coding agent or a team of agents")
	row("coop <agent>", "start "+ui.List(providers, "or"))
	row("coop <preset>", "run agents together using a preset")
	row("coop <target> --peer <target>...", "start with read-only peer agents")

	group("ACCOUNTS, MODELS & PRESETS", "choose the accounts and models your agents use")
	row("coop login <agent>", "sign in to an agent")
	row("coop credentials [<agent>]", "show your signed in accounts")
	row("coop models [<agent>]", "show available models")
	row("coop presets [<name>]", "show your presets")

	group("TASKS", "each task is a folder in .agent/tasks/")
	row("coop tasks ls", "show tasks grouped by status")
	row("coop tasks watch [--json]", "follow task progress")
	row("coop tasks add \"<title>\"", "add a task")
	row("coop tasks decisions", "show tasks waiting for your decision")
	row("coop backlog", "save ideas for later")
	row("coop context [<path>...]", "show which instructions and docs apply to selected files")

	group("LOOPS", "work through tasks automatically; configure the steps in .agent/loop.yaml")
	row("coop loop [<agent|preset>]", "work through the project's task queue")

	group("FORKS", "work in separate project copies, then review and merge the changes")
	row("coop fork <name> <agent|preset>", "work in a separate copy of this project")
	row("coop fork ls", "show this project's forks")
	row("coop fork review <name>", "review a fork's changes")
	row("coop fork merge <name>", "merge a fork's changes into this branch")
	row("coop fork merge --all", "merge all forks into this branch")
	row("coop fork logs [<name>]", "show loop logs for one or all forks")
	row("coop fork rm <name>", "delete a fork")
	row("coop fork stop <name>", "stop a fork's background loop")
	row("coop fork open <name>", "open a fork in your editor")
	row("coop fork path <name>", "print a fork's directory path")

	// The section NAMES this project's services and Compose file, so a newcomer knows what `coop up`
	// would start and where to add more. With no services to act on, the up/down pair is dimmed and
	// only the setup row stays bright. Repo resolution is best-effort: help runs anywhere, and an
	// unreadable configuration keeps the generic explanation rather than claiming there are none.
	compose, services, known := projectServices(cfg, ref)
	if len(services) > 0 {
		group("SERVICES", ui.List(services, "and")+", defined in "+compose)
	} else {
		group("SERVICES", "databases and other services defined in "+compose)
	}
	row("coop init --services", "choose services to add")
	serviceRow := row
	if !ref && known && len(services) == 0 {
		serviceRow = dimRow
	}
	serviceRow("coop up", "start services from "+compose)
	serviceRow("coop down", "stop services from "+compose)

	group("SECURITY & ISOLATION", "control access, check isolation, and protect secrets")
	row("coop doctor", "check that the box's isolation works")
	row("coop net", "show and manage this project's network access")
	row("coop check-secrets", "check project files for exposed secrets")
	row("coop sign", "sign unpushed commits with your host key")

	group("SETUP & MAINTENANCE", "set up projects and manage Coop on this machine")
	row("coop init", "set up Coop in this project")
	row("coop build", "rebuild the box")
	row("coop update", "update Coop and the box")
	row("coop version", "show the installed Coop version")

	group("INTEGRATIONS", "use Coop with other tools and your shell")
	row("coop help acp", "connect Coop to your editor")
	row("coop help sessions", "let other applications start and manage agents")
	row("coop help prompt", "add Coop status to your shell prompt or tmux")
	row("coop help completion", "enable tab completion in Bash or Zsh")

	// The closing actions belong to no section, so they start at column zero.
	fmt.Fprint(&b, "\nHelp and examples: coop help <command>\nDocumentation: https://coop.dryga.com\n")
	return b.String()
}

// projectServices reads what the SERVICES section may state as fact: this project's Compose path
// (the configured one, else the default) and the service names declared in it. known=false means
// coop could not read a service list — no project, or a file it cannot parse — and the section must
// then keep its generic explanation and undimmed rows rather than treat unknown as "none". The
// reference form ignores the machine entirely, so its bytes are the same everywhere.
func projectServices(cfg *config.Config, ref bool) (compose string, services []string, known bool) {
	if ref {
		return project.DefaultCompose, nil, false
	}
	repo, err := box.ResolveRepo(cfg.RepoOverride)
	if err != nil {
		return project.DefaultCompose, nil, false
	}
	compose = project.ComposePath(repo)
	file := box.ComposeFileAt(repo, compose)
	if file == "" { // no Compose file at all: a project with no services yet
		return compose, nil, true
	}
	names := scaffold.ComposeServiceNames(file)
	return compose, names, len(names) > 0
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

// manualOrder is the order the complete reference presents its pages — the menu's own order, so a
// reader who scanned the menu finds each page where they expect it: the box, the agents it runs,
// the accounts and models they use, the work queues, loops and forks, this project's services, the
// security checks, setup, and the integrations last. Every public command is here; one that isn't
// is drift TestManualCoversEveryCommand catches. Presets are resolved BY NAME (`coop help
// frontier`), never added here: the manual's bytes must not depend on the host.
var manualOrder = append(append([]string{"run", "shell"}, agents.Names()...),
	"login", "credentials", "credentials default", "credentials rm", "credentials account",
	"models", "presets init", "presets",
	"tasks", "backlog", "context", "loop", "fork",
	"up", "down",
	"doctor", "net", "net runs", "net inspect", "net check", "net blocked", "net approve",
	"net watch", "net export", "net forget", "net setup", "net recover", "check-secrets", "sign",
	"init", "build", "update", "version",
	"acp", "sessions", "worker", "prompt", "completion")

// manualPage is one command's page for the manual, in plain text: run and fork have their own
// renderers, a registered agent's page is generated from its adapter, and everything else is its
// commandHelp entry. It returns "" for a command with no page, which the manual refuses.
func manualPage(name string) string {
	switch {
	case name == "run":
		return runHelp
	case name == "fork":
		return forkHelpText(ui.Palette{})
	case agents.Valid(name):
		return agentHelp(name)
	}
	return commandHelp[name]
}

// manualSeparator rules off one page from the next in the complete reference.
var manualSeparator = strings.Repeat("=", 78)

// RenderManual is the entire CLI reference as ONE deterministic, plain-text document: the
// reference-form menu, then every command's page in manualOrder, each behind the same separator.
// It's the single source shared by `coop help --all`, docs/cli.md, and site/llms.txt — so terminal,
// docs, and the offline reference are provably identical (tools/gendocs -check enforces it). Plain
// (ui.Palette{}) and state-free, so its bytes never depend on the terminal, this project's Compose
// file, the config paths, or which accounts are signed in. Contributor build/test guidance is NOT
// here: it lives in README.md, where a contributor looks, not in the user's command reference.
func RenderManual(cfg *config.Config) string {
	var b strings.Builder
	b.WriteString(renderHelp(cfg, true))
	for _, name := range manualOrder {
		page := strings.TrimRight(manualPage(name), "\n")
		if page == "" {
			continue // covered by TestManualCoversEveryCommand; never a silent hole at runtime
		}
		b.WriteString("\n" + manualSeparator + "\n\n" + page + "\n")
	}
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
	fmt.Fprintf(&b, "EXAMPLES\n  coop %s\n  coop %s\n  coop %s\n  coop %s -- --help\n\n", name, example, account, name)

	options := [][2]string{{"--peer <target>", "start with a read-only peer agent; repeat to add more"}}
	if restricted {
		options = append(options,
			[2]string{"--readonly", "mount the repository read-only"},
			[2]string{"--bare", "run without the repository, project context, or tools"})
	}
	options = append(options, [2]string{"--", "pass all remaining arguments directly to " + title})
	b.WriteString("OPTIONS\n")
	b.WriteString(helpRows(options, 2))
	if restricted {
		b.WriteString("\n  --readonly and --bare cannot be combined or used with peers.\n")
	}

	b.WriteString("\nMODELS AND ACCOUNTS\n")
	b.WriteString(helpRows([][2]string{
		{"coop models " + name, "list " + title + " models"},
		{"coop credentials " + name, "list " + title + " accounts"},
		{"coop login " + name, "sign in to " + title},
	}, 2))
	b.WriteString("\nFor a guide to using multiple models and providers together:\n  coop help presets")
	return b.String()
}

// helpRows renders a two-column block: every description starts past the widest command cell,
// measured on plain text (see .agent/kb/rules/no-color-in-width-fields.md). gap is that block's
// own column gap, two spaces wherever the approved pages align a column of commands or flags
// against their descriptions.
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

	"login": `coop login — sign in to an agent

Usage: coop login <agent>[@<account>]

AGENTS
  claude  codex  gemini  grok

EXAMPLES
  coop login claude
  coop login codex@work

ACCOUNTS
  Without @account, signs in to the agent's default account.
  Use a name such as @work to keep a separate login.

  Show accounts: coop credentials
  Start an agent: coop claude`,

	"credentials": `coop credentials — show and manage your agent accounts

Usage:
  coop credentials                            show all accounts
  coop credentials <agent>                    show one agent's accounts
  coop credentials <agent> <account>          show an account
  coop credentials <agent> <account> default  use it by default
  coop credentials <agent> <account> rm       remove it

USE AN ACCOUNT
  coop claude@work
  coop login claude@work

  The default account is marked with *.
  Account removal options: coop help credentials rm
  Models and automatic rotation: coop help models`,

	"credentials default": `coop credentials <agent> <account> default — choose the default account

Usage: coop credentials <agent> <account> default

  New runs try this account first. Add @account to use only one account.

EXAMPLE
  coop credentials codex work default

  Accounts: coop credentials codex`,

	"credentials rm": `coop credentials <agent> <account> rm — remove a saved account

Usage: coop credentials <agent> <account> rm [--yes]

OPTIONS
  -y, --yes  skip confirmation

  Removes the saved login and its local session history.
  Choose another default before removing the current default account.

EXAMPLE
  coop credentials codex old-work rm`,

	"credentials account": `coop credentials <agent> <account> — show an account

Usage: coop credentials <agent> <account>

EXAMPLE
  coop credentials codex personal

  Use by default: coop help credentials default
  Remove account: coop help credentials rm`,

	"models": `coop models — list models available to each agent

Usage:
  coop models [claude|codex|gemini|grok] [--refresh]

LIST MODELS
  coop models                   all agents
  coop models claude            Claude only
  coop models claude --refresh  refresh Claude models list now

USE A MODEL
  Put :model after the agent name. This works anywhere Coop accepts an agent.

  coop claude:opus
  coop loop claude:haiku
  coop fork risky claude:opus --loop

  You can use any model accepted by the agent, even if it is not listed.

SET REASONING EFFORT
  Add /effort after the model, or directly after the agent to use its default model.

  coop codex:gpt-6-astra/high
  coop codex/high

  Claude, Codex, and Grok support reasoning effort. Gemini does not.

CHOOSE AN ACCOUNT
  Add @account after the model.

  coop claude:opus@work
  coop credentials

FULL SYNTAX
  coop <provider>:<model>/<effort>@<account>
  coop codex:gpt-6-astra/high@personal

SET A DEFAULT MODEL
  export COOP_CLAUDE_MODEL=opus

AUTOMATIC ROTATION
  Loops and presets can try models in order when one is rate-limited.
  Leave off @account to let Coop also try another signed-in account.
  Add @account when you want to use only that account.

  coop loop claude:opus
  coop loop claude:opus@work

  For more details:
    coop help presets
    coop help loop`,

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

	"presets init": `coop presets init — create a preset you can edit

Usage: coop presets init [<name>]

  Creates the frontier template in .agent/presets/.
  The default name is frontier. Existing presets are left unchanged.

EXAMPLES
  coop presets init
  coop presets init review

  Learn how presets work: coop help presets`,

	"presets": `coop presets — configure multiple models and providers to work together

Usage:
  coop presets                list presets
  coop presets <name>         show a preset
  coop presets init [<name>]  create a preset (default: frontier)
  coop help <name>            explain a preset

CREATE A PRESET
  coop presets init
  coop presets init review

RUN A PRESET
  coop frontier               start an interactive session with the lead agent
  coop loop frontier          work through tasks with this preset
  coop acp frontier           use this preset in your editor
  coop fork risky frontier --loop

HOW TO DEFINE A PRESET

  A preset is a YAML file that defines:
  - One lead agent.
  - Optional roles for focused work.
  - Optional prompt extensions.

  When you run a preset, Coop starts the lead and tells it which roles are
  available and when to use them. Each role runs with its configured agent,
  model, mode, and prompt. The lead combines their work into the final result.

  Syntax:

    agent:   presets use Coop’s standard model, effort, account, and
             automatic-rotation syntax. For details, see:
               coop help models

    mode:    controls how a role works
               native    runs inside the lead agent’s session
               consult   provides read-only advice from another agent
               delegate  edits files for the lead; never commits; runs one at a time

    when:    tells the lead when to use a role

    prompt:  adds custom instructions to Coop’s generated instructions for the
             lead or role

    If the lead does not support native roles, they run as consult roles.

  Where presets live:
    Project  .agent/presets/<name>/preset.yaml
    Global   ~/.config/coop/presets/<name>/preset.yaml

  A project preset overrides a global preset with the same name.`,

	"tasks": `coop tasks — manage work in .agent/tasks/

Usage: coop tasks [--tasks <path>]... [<command>]

WORK
  ls             list tasks (also the default)
  add "<title>"  create a task
  claim <id>     start working on a task
  release <id>   return a task to todo with its handoff notes
  block <id>     ask for a decision before continuing
  unblock <id> ["<answer>"] record a decision and return the task to todo
  done <id>                move completed work to the archive

REVIEW
  watch           watch task progress
  decisions [-i]  show questions waiting for your answer
  lint            check task files for missing or inconsistent information

MANAGE
  lease <id>  reserve a task while an agent works outside a loop
  path <id>   print a task's folder path
  queues      print the configured queue paths
  rm <id>     delete a task

TASK STATES
  todo         ready for an agent
  in progress  being worked on
  blocked      waiting for a decision
  done         completed and archived

GET STARTED
  coop tasks add "Fix login retries"
  coop loop claude

  A task folder holds its instructions, progress and decisions.
  Most commands accept a full task ID or a unique part of it.

FOR AI AGENTS
  Create a task with its instructions and checklist:

  coop tasks add "Fix login retries" \
    --context "An expired session retries forever." \
    --acceptance "An expired session returns to sign-in." \
    --approach "Stop retrying after an authentication failure." \
    --subtask "Test an expired session."

  Block a task with a complete decision request:

  coop tasks block choose-storage \
    --question "Where should uploads be stored?" \
    --option "A — Local disk: simplest, tied to one machine." \
    --option "B — Object storage: shared across machines." \
    --recommendation "B — production runs on more than one machine."

  Get its folder without parsing a listing: coop tasks path <id>
  Before releasing a task, update its state.md with a complete handoff.

PROJECTS
  Coop includes queues listed in .agent/project.yaml.
  Use --tasks <path> to select a queue; repeat it to include several.
  When adding a task to a project with subprojects, choose --project <name>.

  Command options: coop help tasks <command>`,

	"tasks ls": `coop tasks ls — list tasks and their progress

Usage: coop tasks ls [--tasks <path>]... [<options>]

OPTIONS
  --todo         show tasks ready to start
  --in-progress  show tasks being worked on
  --blocked      show tasks waiting for a decision
  --done         show completed tasks
  --all          show the full completed archive

  Combine state options to show more than one state.
  By default, completed tasks are limited to the five most recent.

EXAMPLES
  coop tasks ls --in-progress --blocked
  coop tasks ls --done --all
  coop tasks ls --tasks web/.agent/tasks`,

	"tasks add": `coop tasks add — create a task for you or an agent

Usage: coop tasks add "<title>" [<options>]

OPTIONS
  --project <name>     choose a subproject, or root
  --tasks <path>       choose one task queue
  --context <text>     explain the problem and why it matters
  --acceptance <text>  describe what must be true when finished
  --approach <text>    describe how to tackle the work
  --subtask <text>     add a checklist item; repeat for more items

  Without the text options, Coop creates a template for you to fill in.
  If you use any text option, include --context, --acceptance and --approach.
  Repeat a text option to add another paragraph.

EXAMPLES
  coop tasks add "Fix login retries"
  coop tasks add --project web "Fix login retries"
  coop tasks add "Fix login retries" \
    --context "An expired session retries forever." \
    --acceptance "An expired session returns to sign-in." \
    --approach "Stop retrying after an authentication failure." \
    --subtask "Test an expired session."`,

	"tasks claim": `coop tasks claim — take responsibility for a task

Usage: coop tasks claim <id> [--tasks <path>]... [<options>]

OPTIONS
  --as <label>  name the person or agent working on it
  --pid <n>     bind the claim to an existing process
  --force       take over a claim held by another live process

  Moves a todo task to in progress. An agent's claim normally follows its
  process; a claim made at a terminal stays until you release or finish it.

EXAMPLES
  coop tasks claim login-retries
  coop tasks claim login-retries --as codex --pid 812

  Hand it back: coop tasks release <id>`,

	"tasks release": `coop tasks release — return a task to the queue for another agent

Usage: coop tasks release <id> [--tasks <path>]...

Before releasing, update state.md with the work completed, checks run, and
the next steps. Include anything another agent needs to continue where you left off.

Returns the task to todo and removes your claim. Progress and handoff notes are kept.
A task being worked on by another process or assigned to a fork cannot be released.

EXAMPLE
  coop tasks release login-retries`,

	"tasks lease": `coop tasks lease — reserve a task while work is running

Usage: coop tasks lease <id> [--tasks <path>]... [<options>] [-- <command>...]

OPTIONS
  --as <label>  name the working agent
  --pid <n>     hold the reservation while this process is alive
  --            run a command and release the reservation when it exits

  Claim the task first. Other loops skip it while this reservation is held.
  Without a command, Coop follows your agent's process or the task's claimant.
  Without a process to follow, it waits until the task moves or you press Ctrl-C.

EXAMPLES
  coop tasks lease login-retries -- make check
  coop tasks lease login-retries --as codex --pid 812`,

	"tasks block": `coop tasks block — pause a task that needs a decision

Usage: coop tasks block <id> [--tasks <path>]... [options]

OPTIONS
  --question <text>        explain the decision that is needed
  --option <text>          describe a choice and its tradeoff; repeat for more choices
  --recommendation <text>  recommend a choice and explain why

Without these options, creates decision.md for you to fill in.
If you use any of them, include a question, at least one option, and a recommendation.
Existing answers and decision notes are never overwritten.

FOR AI AGENTS
  coop tasks block choose-storage \
    --question "Where should uploads be stored?" \
    --option "A — Local disk: simplest, tied to one machine." \
    --option "B — Object storage: shared across machines." \
    --recommendation "B — production runs on more than one machine."

ANSWER A QUESTION
  coop tasks unblock choose-storage "Use object storage."`,

	"tasks unblock": `coop tasks unblock — record a decision and return a task to todo

Usage: coop tasks unblock <id> ["<answer>"] [--tasks <path>]...

  Provide your answer here, or fill in Resolution in decision.md first.
  Coop keeps the decision with the task so the next agent can continue.

EXAMPLE
  coop tasks unblock login-retries "Return to sign-in after the first failure."

  Questions waiting for you: coop tasks decisions`,

	"tasks done": `coop tasks done — move completed work to the archive

Usage: coop tasks done <id> [--tasks <path>]...

  Finish the checks and commit the task's work first.
  Coop moves the task to done and removes its temporary files.
  The task's instructions, progress and saved evidence stay in the archive.

EXAMPLE
  coop tasks done login-retries`,

	"tasks path": `coop tasks path — print a task's folder path

Usage: coop tasks path <id> [--tasks <path>]...

EXAMPLE
  coop tasks path login-retries`,

	"tasks queues": `coop tasks queues — print task queue paths

Usage: coop tasks queues [--tasks <path>]...

  Prints one absolute path per line. By default, includes the project's queues.

EXAMPLE
  coop tasks queues`,

	"tasks decisions": `coop tasks decisions — show questions waiting for your answer

Usage: coop tasks decisions [--tasks <path>]... [-i]

OPTIONS
  -i, --interactive  read and answer each question interactively

EXAMPLES
  coop tasks decisions
  coop tasks decisions -i
  coop tasks unblock login-retries "Return to sign-in after the first failure."`,

	"tasks lint": `coop tasks lint — check task files for missing or inconsistent information

Usage: coop tasks lint [--tasks <path>]...

  Checks the task folders, required sections and decision state.
  Returns a nonzero exit status when it finds a problem.

EXAMPLE
  coop tasks lint`,

	"tasks rm": `coop tasks rm — permanently delete task folders

Usage: coop tasks rm <id> [--tasks <path>]... [--yes]
       coop tasks rm --all-done [--tasks <path>]... [--yes]

OPTIONS
  --all-done  delete every completed task in the selected queues
  -y, --yes   skip confirmation

  Deletes task instructions, progress and saved evidence. Git commits remain.
  To archive finished work, use coop tasks done instead.

EXAMPLES
  coop tasks rm abandoned-task
  coop tasks rm --all-done`,

	"tasks watch": `coop tasks watch — watch task progress

Usage: coop tasks watch [--tasks <path>]... [--json]

OPTIONS
  --json  print one snapshot for scripts

  Shows task progress and any problems that need attention.
  The live view closes when the queue is done and its activity has stopped.
  Press Ctrl-C to leave the view. In a pipe, Coop prints one static snapshot.

EXAMPLES
  coop tasks watch
  coop tasks watch --tasks web/.agent/tasks`,

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

	"check-secrets": `coop check-secrets — check project files for exposed secrets

Usage: coop check-secrets [--include-ignored]

Checks tracked and untracked files, plus Coop's task files and notes.
Possible secrets are reported by file and line; their values stay hidden.

OPTIONS
  --include-ignored  also check other Git-ignored files the box can read

FALSE POSITIVES
  Add the entries printed by a scan to .coopsecretsignore in your project root.
  Each entry needs a reason. You can edit many entries together in that file.

  IDs stay the same when line numbers change. A changed value or file path is checked again.
  Remove an entry to check that finding again.

  .coopsecretsignore skips exact findings in this check.
  .coopignore hides files from the box. They do different jobs.

Returns a nonzero status if possible secrets remain or the scan cannot finish.`,

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

	"up": `coop up — start this project's services

Usage:
  coop up

Starts services from .agent/compose.yml and waits for them to be ready.
Uses the Compose path configured in .agent/project.yaml when different.
Agents reach each service by its Compose name. Starting again is safe.

An agent box must not be running in this project when services start.
Requires Docker or Podman with Compose support.

If a service asks to read a secret file, Coop asks at a terminal before
allowing it. Otherwise the service receives an empty file. Approval applies
to the reviewed Compose file and must be repeated if that file changes.

Add services:  coop init --services
Stop services: coop down`,

	"down": `coop down — stop this project's services

Usage: coop down [--delete-volumes] [--yes]

Stops and removes this project's Compose containers and networks.
Stored data is kept unless you use --delete-volumes.
Uses .agent/compose.yml, or the configured Compose path.

OPTIONS
  --delete-volumes  also permanently delete service volumes and their data
  -y, --yes         skip the volume-deletion confirmation

Coop names the volumes before asking; press Enter to cancel.
Without a terminal, deletion requires --yes.
External volumes and files mounted from the project are kept.

Start again: coop up`,

	"init": `coop init — set up Coop in this project

Usage:
  coop init
  coop init [--stack asdf] [--services [<service,...>]] [--agents <agent,...>|all]

COOP SETS UP

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

OPTIONS
  --agents <list>       set up Claude, Codex, Gemini, or all
                       default: agents you are signed in to

  --services [<list>]   add Postgres, Redis, or both

  --stack asdf          install tools from .tool-versions in the Coop box

You can run coop init again at any time.
Coop keeps your existing project files and adds anything missing.`,

	// The net family page and its ten leaf pages are APPROVED transcripts: the exact bytes are
	// pinned in internal/cli/testdata/approved/20*.txt. The family page groups the verbs by the
	// job a person came with — what new runs may reach, what recorded runs did, the repair coop
	// normally does itself — and ends with the one workflow nobody guesses (edit the YAML, then
	// approve). Every leaf page is reached as `coop net <verb> --help` and `coop help net <verb>`.
	"net": `coop net — control network access and see what happened

ACCESS — control what new runs can reach
  coop net                  show this project's current access
  coop net approve          review and approve requested changes
  coop net check <url>      check access
  coop net forget           withdraw this project's network approval

RUNS — inspect recorded network activity
  coop net runs             show recent runs
  coop net inspect [<run>]  show connections and blocked access
  coop net blocked <host>   find blocked connections and how to allow them
  coop net watch [<run>]    follow network activity
  coop net export <run>     export a shareable record

REPAIR — normally automatic
  coop net setup            prepare or recheck this host now
  coop net recover [<run>]  retry interrupted cleanup now

HOW TO ADD A NETWORK RULE

1. Edit .agent/project.yaml and add the destination under box.egress_rules:

   box:
     egress: filtered
     egress_rules:
       - to: {domain: "docs.example.com"}
         protocol: tls
         ports: [443]

2. Review and approve the changes:

   coop net approve

Command options: coop help net <command>`,

	"net runs": `coop net runs — show recorded network runs

Usage: coop net runs [--all | --all-projects] [--json]

OPTIONS
  --all           show every run for this project
  --all-projects  show every project's runs on this host
  --json          print the records for scripts

  By default, shows this project's 25 newest runs.
  Use a run's ID or a unique prefix with other network commands.

EXAMPLE
  coop net inspect e644f07a`,

	"net inspect": `coop net inspect — show a run's connections and blocked traffic

Usage: coop net inspect [<run>] [--json]

OPTIONS
  --json  print the full technical record

  Without a run ID, shows this project's newest recorded run.

EXAMPLES
  coop net inspect
  coop net inspect e644f07a`,

	"net check": `coop net check — check whether network rules allow a connection

Usage: coop net check <url-or-host> [<options>]
       coop net check <ip> --run <run> --protocol <tcp|udp> --port <n>
       coop net check <ip> --run <run> --icmp

OPTIONS
  --run <run>               use that run's rules
  --port <n>                choose a port; defaults to the URL's port or 443
  --protocol <tls|tcp|udp>  choose the connection type
  --icmp                    check an ICMP echo request to an IP address
  --json                    print the answer for scripts

  Without --run, checks this project's approved access for a new run.
  Hostnames use TLS. IP addresses require a recorded run and connection type.

EXAMPLES
  coop net check https://example.com
  coop net check example.com --run e644f07a`,

	"net blocked": `coop net blocked — find blocked connections and how to allow them

Usage: coop net blocked <host> [--run <run>] [--json]

OPTIONS
  --run <run>  look only in this run
  --json       print the evidence for scripts

  Without --run, finds the newest matching block in this project's runs.
  Shows a rule you can copy when the record proves the destination and port.
  An exact event ID can replace the host when --run is supplied.

EXAMPLES
  coop net blocked registry.npmjs.org
  coop net blocked registry.npmjs.org --run e644f07a`,

	"net approve": `coop net approve — review and approve this project's network request

Usage: coop net approve

  Shows what .agent/project.yaml asks for, compared with your last approval.
  Confirm the changes to allow new runs to use them.
  If nothing changed, there is nothing to approve.

  Existing boxes keep the rules they started with.
  Run this command in your terminal.

See current access: coop net`,

	"net watch": `coop net watch — follow a run's network activity

Usage: coop net watch [<run>] [--json]

OPTIONS
  --json  stream one JSON record per update

  Without an ID, selects this project's only unfinished run.
  Shows new connections, blocks and problems, then the final report.

  Press Ctrl-C to stop watching. The run continues.

EXAMPLE
  coop net watch e644f07a`,

	"net export": `coop net export — export a run's network record

Usage: coop net export <run> [--include-addresses]

OPTIONS
  --include-addresses  include remote hostnames and IP addresses

  Writes JSON to stdout. Remote hostnames and IP addresses are hidden by default.
  The run must have a final record.

EXAMPLE
  coop net export e644f07a > network-record.json`,

	"net forget": `coop net forget — withdraw this project's network approval

Usage: coop net forget [--project <path>]

Use this when you no longer trust an approval saved on this machine.
To change network rules, edit .agent/project.yaml and run coop net approve.

OPTIONS
  --project <path>  choose another project, including a folder that was deleted

New runs wait for approval again. The project file is not changed.
Existing boxes, recorded runs, and this host's network setup are kept.
Run this command in your terminal.

Restore approval:
  coop net approve`,

	"net setup": `coop net setup — prepare this host for filtered networking

Usage: coop net setup

  Prepares the network images and checks that allowed access works and
  blocked access stays blocked.

  Coop does this automatically when a filtered run needs it.
  Run it yourself to check or prepare the host ahead of time.

Requires Docker to be running.`,

	"net recover": `coop net recover — clean up interrupted network runs

Usage: coop net recover [<run>]

  Removes containers and temporary volumes left by an interrupted run.
  Without an ID, checks every run waiting for cleanup on this host.
  Running boxes and resources Coop cannot safely identify are left alone.

  Coop normally retries this cleanup automatically.

EXAMPLE
  coop net recover e644f07a`,

	"doctor": `coop doctor — check that the box's isolation works

Usage:
  coop doctor

Runs checks in a temporary project and reports what passed, failed, or
could not be checked. Your files and credentials are not the test data.

Checks secret hiding, host access, privileges, offline networking,
task access, credential isolation, settings permissions, and fork handoff.

Uses this project's built image, or the shared image if available.
If neither exists, uses Alpine and names the checks it cannot perform.
Run coop build, then coop doctor to test the Coop image.

Requires Git and a running container runtime. Honors COOP_RUNTIME.
Lists abandoned boxes but does not remove them.`,

	"build": `coop build — build the Coop box image

Usage:
  coop build

Builds the shared image, or this project's image when it has a box
Dockerfile. The default path is .agent/Dockerfile; use box.dockerfile in
.agent/project.yaml to select another path.

Run after changing the box Dockerfile or .tool-versions.
Builds use the configured versions and available cache.
Use coop update --box-only to fetch newer components and rebuild fresh.

New runs use the rebuilt image. Supervised editor sessions restart and
reconnect. Other running boxes use the old image until their next start.
Requires a running container runtime.`,

	"update": `coop update — update Coop and rebuild the box image

Usage:
  coop update [--self-only | --box-only | --check]

Updates the Coop binary when a newer release is available, then rebuilds
the box with newer base-image components, agent CLIs, and editor adapters.
Downloaded releases are checked before replacing the binary.

OPTIONS
  --self-only  update only the Coop binary; no container runtime needed
  --box-only   rebuild only the box image, fetching newer components
  --check      report available updates and local build records; change nothing

Choose at most one option. --check does not need a container runtime.
Building the box requires a running container runtime.

Development builds and binaries newer than the latest release are kept.
A binary that cannot be replaced is an error; update it with the tool
that installed it. The box rebuild can still finish independently.

New runs use the rebuilt image. Supervised editor sessions restart and
reconnect. Other running boxes use the old image until their next start.

Coop checks for new releases once a day during terminal use.
Set COOP_NO_UPDATE_CHECK=1 to turn off those notices.`,

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
// without the all-commands footer. Membership travels with the page's last line. Every task page
// ends with its own next step (the family page points at its per-command pages; each command page
// ends with its examples or the command that follows it), so the family is listed as a prefix.
var selfContainedHelp = map[string]bool{
	"presets": true, "models": true, "init": true, "tasks": true, "net": true,
	"login": true, "credentials": true,
}

// selfContained reports whether cmd's page ends itself. A `<family> <command>` page inherits its
// family's answer, so a new task page needs no second registration.
func selfContained(cmd string) bool {
	if selfContainedHelp[cmd] {
		return true
	}
	family, _, ok := strings.Cut(cmd, " ")
	return ok && selfContainedHelp[family]
}

// printTopicHelp is the ONE way a static page reaches the terminal, so `coop presets --help`,
// `coop help presets` and a bare group can never disagree about the footer.
func printTopicHelp(cmd, text string) {
	if selfContained(cmd) {
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
