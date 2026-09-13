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
	// note is a sentence that belongs to the group above it rather than to one command, set off by a
	// blank line and starting at the command column so it reads as part of the group, not a row.
	note := func(text string) { fmt.Fprintf(&b, "\n  %s\n", text) }
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
	row("coop <"+strings.Join(agents.Names(), "|")+">", "start "+ui.List(providers, "or"))
	row("coop <preset>", "run agents together using a preset")
	row("coop <target> --peer <target>...", "start with read-only peer agents")
	// A newcomer meets the word "target" here, in three rows that all use it. Saying what it can be
	// once, with one example of the longest form, is shorter than three rows that each explain it.
	note("A target names an agent, a preset, or a model: codex:gpt-5.6-luna/xhigh")

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
	"tasks", "tasks ls", "tasks add", "tasks claim", "tasks release", "tasks lease",
	"tasks block", "tasks unblock", "tasks done", "tasks path", "tasks queues",
	"tasks decisions", "tasks lint", "tasks rm", "tasks watch",
	"backlog", "backlog ls", "backlog add", "backlog promote", "backlog rm",
	"context", "loop",
	"fork", "fork acp", "fork ls", "fork review", "fork merge", "fork rm",
	"fork stop", "fork logs", "fork path", "fork open",
	"up", "down",
	"doctor", "net", "net runs", "net inspect", "net check", "net blocked", "net approve",
	"net watch", "net export", "net forget", "net setup", "net recover", "check-secrets", "sign",
	"init", "build", "update", "version",
	"acp", "sessions", "sessions serve", "sessions doctor", "sessions policies", "sessions compact", "sessions connect",
	"prompt", "completion")

// manualPage is one command's page for the manual, in plain text: run and fork have their own
// renderers, a registered agent's page is generated from its adapter, and everything else is its
// commandHelp entry. It returns "" for a command with no page, which the manual refuses.
func manualPage(name string) string {
	switch {
	case name == "run":
		return runHelp
	case name == "fork":
		return forkHelpText("")
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
const runHelp = `coop run — run a command in the box

Usage:
  coop run [options] -- <command> [<args>...]

EXAMPLES
  coop run -- npm test
  coop run -- curl https://example.com
  coop run --readonly --egress none -- git status

OPTIONS
  --readonly  mount the repository read-only
  --bare      run without the repository or project context

  --egress <mode>        choose filtered, open, or none
  --allow-domain <host>  allow TLS access to this host on port 443
                          repeat to allow more hosts
  --egress-rules <path>   load network rules from a file

  Put Coop options before --. Everything after it belongs to your command.
  --readonly and --bare cannot be combined.
  These two modes require Docker and --egress open or none.

NETWORK ACCESS
  filtered  allow traffic permitted by approved network rules
  open      allow unrestricted internet access
  none      block internet access

For more details see:
  coop help net`

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

// helpRowsAt is helpRows with a MINIMUM description column, for a page whose rows are generated
// from a value (a fork's name) but whose approved bytes were set with a fixed column: a short name
// keeps the approved alignment, a long one pushes the column out rather than colliding with it.
func helpRowsAt(rows [][2]string, column int) string {
	w := column - 4 // the 2-space indent plus helpRows' own 2-space gap
	for _, r := range rows {
		if n := utf8.RuneCountInString(r[0]); n > w {
			w = n
		}
	}
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "  %s  %s\n", padRight(r[0], w), r[1])
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
	"sessions": `coop sessions — let other applications start and manage agents

Usage: coop sessions <command>

COMMANDS
  connect   connect this machine to a remote controller
  serve     run the local session service on its own
  doctor    check whether the local service is ready
  policies  show the policies that control allowed sessions
  compact   back up session data and reduce its disk usage

CONNECT THIS MACHINE
  coop sessions connect --config /path/to/worker.json

  Uses the running local service, or starts it if needed.
  The configuration selects the controller and the sessions it is allowed to request.

COMMAND HELP
  coop help sessions connect
  coop help sessions serve
  coop help sessions doctor
  coop help sessions policies
  coop help sessions compact`,

	"sessions connect": `coop sessions connect — connect this machine to a remote controller

Usage: coop sessions connect --config <path>

OPTIONS
  --config <path>  JSON file with this machine's connection and session settings

CONFIGURATION
  Selects the controller, certificates, repositories, and allowed session policies.
  Uses the ready local service, or starts one when none is running.

  Default session policies: ~/.config/coop/session-policies.yaml
  Default session storage:  ~/.local/state/coop/sessions

  Review session configurations:
    coop sessions policies

RUNNING
  Leave this command running. Connection failures are retried automatically.
  Press Ctrl-C to stop connecting. A service started by this command also stops;
  a service that was already running is left alone.`,

	"sessions serve": `coop sessions serve — start the local session service

Usage:
  coop sessions serve [options]

OPTIONS
  --state <path>     directory containing session data
  --policies <path>  file defining the allowed sessions
  --socket <path>    Unix socket used by local clients

DEFAULT PATHS
  Session data  ~/.local/state/coop/sessions
  Policies      ~/.config/coop/session-policies.yaml
  Socket        <session data>/control.sock

RUNNING THE SERVICE
  Leave this command running. Press Ctrl-C to stop it.

  Check it from another terminal:
  coop sessions doctor

  To connect it to a remote controller:
  coop help sessions connect`,

	"sessions doctor": `coop sessions doctor — check the local session service

Usage:
  coop sessions doctor [options]

OPTIONS
  --socket <path>  check this Unix socket
  --json           print the result as JSON

EXAMPLES
  coop sessions doctor
  coop sessions doctor --socket /path/to/control.sock

  The command succeeds when the service responds and is ready for sessions.`,

	"sessions policies": `coop sessions policies — show remote session configurations

Usage: coop sessions policies [options]

Each configuration names the project a remote application can use, its agents and
accounts, whether they can edit files, and the network rules they must follow.

OPTIONS
  --policies <path>  read configurations from this file
  --json            print verification data for remote applications as JSON

DEFAULT FILE
  ~/.config/coop/session-policies.yaml

EXAMPLES
  coop sessions policies
  coop sessions policies --json

To connect this machine to a remote controller:
  coop help sessions connect`,

	"sessions compact": `coop sessions compact — back up session data and reduce its disk usage

Usage:
  coop sessions compact --backup <path> [--state <path>]

OPTIONS
  --backup <path>  save a verified database backup to a new file
  --state <path>   use this session-data directory

EXAMPLE
  coop sessions compact --backup /path/to/session-backup.sqlite

BEFORE RUNNING
  Stop the session service.
  Choose a new backup file outside the session-data directory.
  Its parent directory must already exist.

  The backup contains private session data. Keep it protected.

DEFAULT SESSION DATA
  ~/.local/state/coop/sessions`,
	"sign": `coop sign — sign this branch's unpushed commits with your host key

Usage:
  coop sign [--from <ref>]

EXAMPLES
  coop sign
  coop sign --from origin/main

OPTIONS
  --from <ref>   sign commits after this Git reference

HOW IT WORKS
  By default, Coop signs commits after the branch's upstream reference.
  Without an upstream, use --from with the last commit you pushed.

  Signing changes commit IDs. It preserves the committed files and your
  working changes. The selected range must not contain merge commits.

  Coop uses your host's Git signing settings.

AUTOMATIC SIGNING
  When commit.gpgsign is enabled, Coop signs commits after ordinary agent runs
  and loop cycles. Fork commits are signed when you merge the fork.`,
	"prompt": `coop prompt — print a short project status for your shell or tmux

Usage:
  coop prompt

OUTPUT
  Shows task counts, forks, running fork loops, and an unsigned-commit marker.
  Empty counts are omitted. When there is nothing to show, it prints nothing.

EXAMPLE
  2 todo · 1 in progress · 1 blocked · 3 forks (2 running) · unsigned commit

TMUX
  Add this to ~/.tmux.conf:
  set -g status-right '#(cd "#{pane_current_path}" && coop prompt)'

SHELL PROMPTS
  Run coop prompt from your prompt's custom-command configuration.`,

	"shell": `coop shell — open a shell in the box

Usage:
  coop shell [options]

EXAMPLES
  coop shell
  coop shell --readonly --egress none

OPTIONS
  --readonly  mount the repository read-only
  --bare      open a shell without the repository or project context

  --egress <mode>        choose filtered, open, or none
  --allow-domain <host>  allow TLS access to this host on port 443
                          repeat to allow more hosts
  --egress-rules <path>   load network rules from a file

  --readonly and --bare cannot be combined.
  These two modes require Docker and --egress open or none.

USING THE SHELL
  The shell opens in your project inside the box.
  Type exit or press Ctrl-D to return to your host.

  With --bare, the project is not mounted.

NETWORK ACCESS
  filtered  allow traffic permitted by approved network rules
  open      allow unrestricted internet access
  none      block internet access

For more details see:
  coop help net`,

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

	"acp": `coop acp — connect your editor to an agent running in a Coop box

Usage:
  coop acp [<agent|preset>] [options]

EDITOR SETUP
  Set the editor's agent command to coop and its arguments to:

  ["acp"]
  ["acp", "claude"]
  ["acp", "codex:gpt-6-astra/high@personal"]
  ["acp", "frontier"]

  Your editor must support ACP, the Agent Client Protocol.

  Without an agent or preset, Coop starts the first signed-in provider in
  Claude, Codex, Gemini, Grok order, using its default account. This does not
  pin the choice: use the editor's selectors after connecting.
  No providers signed in? Run coop login <agent>, then reconnect your editor.

OPTIONS
  --peer <agent>          start a peer provider for read-only advice
                         repeat to add more peers
  --bare                 run without the repository, project context, or tools

  --egress <mode>        choose filtered, open, or none
  --allow-domain <host>  allow TLS access to this host on port 443
                         repeat to allow more hosts
  --egress-rules <path>   load network rules from a file

  --bare requires a single agent, without peers or a preset.
  It requires Docker and --egress open or none.

SWITCHING AGENTS
  Use the editor's Provider and Account controls to change who runs the session.
  A preset chooses its agents automatically. Select None in Preset to choose
  a provider or account yourself.

  Switching providers carries the conversation into a new agent session.
  Account changes keep the conversation with the same provider.

  With filtered networking, only compatible providers and presets are offered.
  Claude and Codex support it; Gemini and Grok currently require open networking.
  Switching never changes this session's network rules.

READ-ONLY WORK
  coop fork investigation acp claude --readonly

  Replace investigation with your fork's name.

TROUBLESHOOTING
  Set COOP_ACP_TRACE=1 in your editor's agent environment to record a trace.
  Trace files are saved as ~/.config/coop/acp-trace-<pid>.log and may contain
  your prompts and file contents.

  COOP_ACP_WARM=0 disables background boxes kept ready for provider switching.
  COOP_ACP_CARRY_TOKENS sets the conversation size carried between providers.

  After replacing the Coop binary, send SIGHUP to the specific ACP process
  to reload it. SIGINT and SIGTERM stop it.

RELATED HELP
  coop help models
  coop help presets
For more details see:
  coop help net`,

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

  Questions waiting for you: coop tasks decisions -i`,

	"tasks done": `coop tasks done — move completed work to the archive

Usage: coop tasks done <id> [--tasks <path>]...

  Finish the checks and commit the task's work first.
  Completion requires a nonempty, fully checked task checklist.
  Failed, unavailable or unrun required checks must stay open.
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

	"backlog": `coop backlog — save ideas that need more planning

Usage: coop backlog [--tasks <path>]... [<command>]

COMMANDS
  ls             list ideas (also the default)
  add "<title>"  save an idea
  promote <id>   move an idea to the task queue
  rm <id>        delete an idea

  Backlog items live in .agent/tasks/xx_backlog/.
  Agents leave them alone until you promote them.
  Put work that is ready to start directly in coop tasks.

EXAMPLES
  coop backlog add "Redesign account permissions"
  coop backlog promote account-permissions

  Command options: coop help backlog <command>`,

	"backlog ls": `coop backlog ls — list saved ideas

Usage: coop backlog ls [--tasks <path>]...

  By default, includes every configured project queue.
  Repeat --tasks to select several queues.

EXAMPLE
  coop backlog ls --tasks web/.agent/tasks`,

	"backlog add": `coop backlog add — save an idea for later

Usage: coop backlog add "<title>" [<options>]

OPTIONS
  --tasks <path>       choose one task queue
  --context <text>     explain the problem and why it matters
  --acceptance <text>  describe the intended result
  --approach <text>    describe a possible approach
  --subtask <text>     add a checklist item; repeat for more items

  Without the text options, Coop creates a template for you to fill in.
  If you use any text option, include --context, --acceptance and --approach.
  Repeat a text option to add another paragraph.

EXAMPLES
  coop backlog add "Redesign account permissions"
  coop backlog add "Redesign account permissions" --tasks web/.agent/tasks

Ready to start: coop backlog promote <id>`,

	"backlog promote": `coop backlog promote — move an idea to the task queue

Usage: coop backlog promote <id> [--tasks <path>]...

  Fill in its problem, completion criteria and approach before promoting it.
  The task becomes todo, where an agent or loop can pick it up.

EXAMPLE
  coop backlog promote account-permissions`,

	"backlog rm": `coop backlog rm — permanently delete a saved idea

Usage: coop backlog rm <id> [--tasks <path>]... [--yes]

OPTIONS
  -y, --yes  skip confirmation

  Deletes the idea's folder, including its notes and saved files.

EXAMPLE
  coop backlog rm account-permissions`,

	"fork acp": `coop fork <name> acp — use an existing fork from an ACP editor

Usage: coop fork <name> acp <target> [<options>]

OPTIONS
  --readonly       mount the fork read-only
  --peer <target>  start with read-only peer agents; repeatable

  Read-only mode has no peer agents, project hooks or MCP servers.
  The editor controls whether to start or resume a conversation.

EXAMPLE
  coop fork login acp claude:opus

  Create the fork first: coop fork login claude
  Editor setup: coop help acp`,

	"fork ls": `coop fork ls — show forks and their progress

Usage: coop fork ls [--json]

OPTIONS
  --json  print workspace details for scripts

  Shows each fork's agent, activity, tasks, changes and reported cost.

EXAMPLE
  coop fork ls`,

	"fork review": `coop fork review — inspect a fork before merging

Usage: coop fork review <name> [<options>]

OPTIONS
  --stat  show a change summary instead of the full diff
  --tool  open the diff with your global Git diff tool
  --open  open the fork in your editor
  --gate  test the fork after rebasing a temporary copy onto your current branch

  Shows commits, the agent's task notes and changes that need your attention.
  --gate leaves the fork and your working tree unchanged. It cannot be used
  with --open.

EXAMPLES
  coop fork review login
  coop fork review login --stat --gate

  Merge after review: coop fork merge login`,

	"fork merge": `coop fork merge — bring a fork's commits into your current branch

Usage: coop fork merge <name> [<options>]
       coop fork merge --all [<options>]

OPTIONS
  --all        merge eligible forks one at a time
  -f, --force  allow changes otherwise blocked by the merge policy
  -y, --yes    confirm merging and removal without prompting

  Review the diff first. Your working tree must be clean and the fork stopped.
  Coop rebases the commits, runs configured project checks and merges the result.
  --force does not bypass those checks.

  After merging, Coop asks before deleting a clean fork. With --all, one prompt
  covers the batch and removal. Forks with uncommitted work are kept.
  Removing a fork also removes its service containers and Docker volumes.

EXAMPLES
  coop fork review login
  coop fork merge login
  coop fork merge --all`,

	"fork rm": `coop fork rm — delete a fork and its local work

Usage: coop fork rm <name> [<options>]

OPTIONS
  -f, --force  stop active work and discard uncommitted or unmerged changes
  -y, --yes    skip confirmation

  Deleting a fork removes its files and local session data, plus its service
  containers and Docker volumes.
  Assigned tasks return to the project queue. Proposed tasks that have not been
  imported are discarded; tasks already imported into the project are kept.

EXAMPLES
  coop fork rm login
  coop fork rm login --force`,

	"fork stop": `coop fork stop — stop a fork's background loop

Usage: coop fork stop <name>

  Stops the worker and its running boxes. The fork's work and assigned task stay
  available so you can continue it later.

EXAMPLES
  coop fork stop login
  coop fork login claude --loop`,

	"fork logs": `coop fork logs — show a fork's loop output

Usage: coop fork logs [<name>] [--follow]

OPTIONS
  -f, --follow  keep showing new output

  Without a name, includes all forks and labels each line with its fork name.
  Press Ctrl-C to leave the log view; the loop keeps running.

EXAMPLES
  coop fork logs login
  coop fork logs login --follow
  coop fork logs --follow`,

	"fork path": `coop fork path — print a fork's folder path

Usage: coop fork path <name>

EXAMPLE
  coop fork path login`,

	"fork open": `coop fork open — open a fork in your editor

Usage: coop fork open <name>

  Uses COOP_EDITOR, your global Git editor or an available editor.

EXAMPLE
  coop fork open login`,

	"context": `coop context — show the instructions relevant to your work

Usage: coop context [<path>...] [<options>]

OPTIONS
  --changed       use files changed in Git
  --task <id>     use paths listed in a task's frontmatter
  --tasks <path>  choose a queue for --task; repeat for more queues
  --rendered      print the selected files' contents
  --json          print the selection for scripts

  Coop includes shared agent instructions and any matching context routes.
  Use paths relative to the repository. Inside a subproject, its path is included.
  You can combine explicit paths, --changed and --task.
  Choose either --rendered or --json for the output format.

EXAMPLES
  coop context internal/cli/help.go
  coop context --changed
  coop context --task login-retries --rendered

CONFIGURE ROUTES — .agent/project.yaml
  context:
    routes:
      - paths: ["web/**"]
        include: [.agent/kb/web.md]

  This includes .agent/kb/web.md when the selected work is under web/.`,

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

	"loop": `coop loop — let agents work through your task queue

Usage: coop loop [<target|preset>] [<options>]

HOW IT WORKS
  A fresh agent works on each task, then a final review checks the completed work.
  The review can reopen tasks. Coop continues until the work passes review or
  needs your decision.

EXAMPLES
  coop loop claude
  coop loop frontier
  coop loop codex --max-tasks 1

OPTIONS
  --tasks <path>         choose a task queue; repeat for more queues
  --peer <target>        start with read-only peer agents; repeat for more peers
  --max-tasks <n>        pause after N tasks finish or become blocked
  --preflight            resolve answered decisions and release stale claims first
  --no-preflight         skip that preparation
  --no-mcp               run without configured MCP servers
  --debug-on-fail        open a box shell after a failure, then retry when you exit
  --egress <mode>        internet access: filtered, open or none
  --allow-domain <name>  allow this exact domain over TLS on port 443; repeatable
  --egress-rules <file>  add this file's network rules for the run

  --max-tasks pauses before the final review. The limit must be positive.
  --debug-on-fail opens a shell only when running in a terminal.

STOP AND CONTINUE
  Press Ctrl-C to finish the current task attempt and its required review, then stop.
  Press it again to stop immediately. Run the same command to continue.
  For a background fork loop: coop fork stop <name>

CONFIGURE STEPS — .agent/loop.yaml
  preflight  optional preparation before starting
  work       the agent that works on each task
  between    optional review after each task
  signoff    final review of completed work
  verify     optional checks after the final review

  Set work.agent to choose the default when no target or preset is given.
  Review steps can each use their own agent and prompt. Signoff prompts add to
  Coop's built-in review. Between and verify prompts define those checks.
  Changes to this file apply when you restart the loop.

  Step options:
    preflight  enabled, prompt
    work       agent, command
    between    enabled, agent, prompt, writes
    signoff    agent, prompt, rounds, writes
    verify     enabled, agent, prompt, writes

  agent: accepts a list of targets or presets. See coop help models.
  writes: tasks keeps reviews read-only; repo lets the reviewer edit source files.
  Between and verify require a prompt when enabled.
  mcp: false disables MCP servers for every step.

NETWORK ACCESS
  The run's approved access covers its work and review agents for the entire loop.
  See coop help net for rules and approvals.

EXIT STATUS
  0    review and enabled final checks passed, or the task limit was reached
  1    the run failed, final checks failed, or work remains
  2    invalid command or configuration
  3    stopped for a decision
  130  interrupted before the final verdict`,

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

	"version": `coop version — print the installed Coop version

Usage:
  coop version

ALIASES
  coop -v
  coop --version`,

	"completion": `coop completion — print shell integration for Bash or Zsh

Usage:
  coop completion <bash|zsh>

BASH
  Save the completion script:
  mkdir -p ~/.config/coop
  coop completion bash > ~/.config/coop/completion.bash

  Add this to ~/.bashrc:
  source ~/.config/coop/completion.bash

ZSH
  Save the integration script:
  mkdir -p ~/.config/coop
  coop completion zsh > ~/.config/coop/completion.zsh

  Add this after compinit in ~/.zshrc:
  source ~/.config/coop/completion.zsh

  This also stops Zsh from suggesting corrections to Coop arguments such
  as claude and codex.

WHAT IT COMPLETES
  Commands, options, agents, models, accounts, presets, forks, and task IDs.`,
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
// without the all-commands footer. Membership travels with the page's last line. A whole family is
// listed when every one of its pages ends itself — the family page points at its per-command pages,
// and each command page ends with its examples or the command that follows it.
var selfContainedHelp = map[string]bool{
	"presets": true, "models": true, "init": true, "net": true,
	"login": true, "credentials": true,
	"tasks": true, "backlog": true, "context": true, "loop": true, "fork": true,
	"shell": true, "acp": true, "sign": true, "prompt": true, "version": true,
	"completion": true, "sessions": true,
	"up": true, "down": true, "doctor": true, "check-secrets": true, "build": true, "update": true,
}

// selfContained reports whether cmd's page ends itself. A `<family> <command>` page inherits its
// family's answer, so a new leaf page in a listed family needs no second registration.
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
