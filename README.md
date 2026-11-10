<div align="center">

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="brand/assets/coop-lockup.svg">
  <img src="brand/assets/coop-lockup-light.svg" alt="co:op" width="420">
</picture>

Run your AI agents in a sandbox, with your secrets, API keys and untrusted websites out of reach. Then give them a task queue, and let them work through it overnight.

[![CI](https://github.com/AndrewDryga/coop/actions/workflows/ci.yml/badge.svg)](https://github.com/AndrewDryga/coop/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/AndrewDryga/coop?sort=semver)](https://github.com/AndrewDryga/coop/releases/latest)
[![Go Report Card](https://goreportcard.com/badge/github.com/AndrewDryga/coop)](https://goreportcard.com/report/github.com/AndrewDryga/coop)
[![License](https://img.shields.io/github/license/AndrewDryga/coop)](LICENSE)

</div>

Coding agents do their best work unleashed, but that's a gamble with your secrets and your code.
co:op runs the agent in a sandbox (aka box) so it sees one folder: your project. The folder
keeps its real path, so the agent works well with your
[editor](#editors-mcp-and-your-own-platform). Your SSH keys, cloud logins and other repos aren't in
the box, and its network is restricted. co:op supports Claude Code, Codex, Gemini CLI and Grok.

```bash
cd ~/code/your-repo && coop claude
```

The [website](https://coop.dryga.com) shows how it works, and the
[guide](https://coop.dryga.com/docs.html) walks through every feature.

## Why a box

Agents do what they read in code, docs and issues, even when an attacker wrote it:

- A comment hidden in a README can tell your agent to post your SSH key in an issue. In the sandbox
  there is no key to post.
- A package the agent installs can try to send your `.env` to a stranger's server. In the sandbox
  `.env` cannot be read, and with filtered networking, the default, that server can't be reached.

And sometimes the agent just gets it wrong. An agent cleaning up can run `rm -rf tests/ plan/ ~/`. With the sandbox, that doesn't cause damage any more.

Supply chain attacks are common too. The sandbox keeps the agent from installing anything on your
computer or reading anything outside your project, and the filtered network keeps it from reaching
servers you haven't approved. Your Anthropic API keys, for example, are never mounted into the
sandbox, so they stay safe.

## Install

```bash
curl -fsSL https://coop.dryga.com/install.sh | sh
```

The script downloads the `coop` binary for your system into `~/.local/bin`. It needs `curl`, `tar`,
and `sha256sum` or `shasum`. If Docker is available, it also builds the box image and runs
`coop doctor`. If not, install Docker and then run:

```bash
coop build --egress open && coop doctor
```

co:op runs on macOS and Linux with Docker. The `coop` binary is static.

To build from source, run `git clone https://github.com/AndrewDryga/coop && cd coop && make install`.

<a name="verifying-a-download"></a>
To check a download yourself, follow [Verifying a download](https://coop.dryga.com/install.html#verifying-a-download).

## Updating

We ship updates frequently. `coop update` updates the binary, then uses the installed version to prepare the image for this
project's network mode.

## Agent skill

Teach your agent to set up Coop, run sandboxed commands, delegate work to forks,
and manage task queues:

```bash
npx skills add AndrewDryga/coop --skill coop
```

This installs only the [Coop product skill](skills/coop/SKILL.md). The Coop CLI
and Docker are installed separately.

## Quickstart

```bash
coop login claude          # 1. sign in, once per account
cd ~/code/your-repo        # 2. any git repo
coop init                  # 3. set up the project: settings, task queue, hooks
coop claude                # 4. start the agent in the box
```

`coop init` sets up the task queue, hooks, shared instructions and skills, and the
selected [agent directories](https://coop.dryga.com/agents.html#instructions). It configures the agents you're signed
in to, so sign in first.

Each agent starts with its own "don't stop to ask" flag on (`--dangerously-skip-permissions`,
`--dangerously-bypass-approvals-and-sandbox`, `--yolo`, `--permission-mode bypassPermissions`).

An agent that goes off the rails works on the repo you gave it, which you can restore from Git. The
rest of your computer stays out of reach. You can also use `coop fork` to give the agent its own
copy of your repo, so you can keep working in yours without risking your local changes. See
[forks](https://coop.dryga.com/forks.html).

Arguments after the agent's name go to the agent, so `coop claude --continue` resumes Claude's last
session in the box. When Claude exits, co:op prints the command that resumes that session, such as
`coop claude --resume <id>`. On every agent, `-p "…"` runs one prompt and exits.

co:op answers `--help`, `-h` and a leading `help` or `login` itself, and takes its own options
such as `--peer` and `--egress`. Put `--` before them to pass them to the agent, as in
`coop claude -- --help`.

Other ways in:

```bash
coop codex                                    # the same box, with Codex
coop claude --peer codex --peer gemini        # read-only second opinions for hard calls
coop shell                                    # a shell in the box, to look around
coop run -- npm test                          # any command in the box
```

## What stays out of the box

- The box mounts one folder: your project. Your home folder, SSH keys, cloud logins and other repos
  aren't in it.
- Secret files like `.env` stay on your computer, and the box cannot read their contents.
  `coop check-secrets` finds secrets inside your other files, and `.coopignore` hides more paths.
- Networking is filtered by default: the box reaches your AI provider and the domains you approve.
  Everything else is blocked, so your code can't go anywhere you haven't approved.
- Each repository has its own agent history and settings, so the box can't see your other projects'
  conversations. Provider credentials stay outside the box.
- There are no Git credentials inside. The agent commits in your name, and you decide what leaves
  your computer.

`coop doctor` checks secret hiding, host access, credential isolation and offline networking
against the real box on your computer. `coop net setup` checks that this computer can enforce
filtered networking, and the first filtered run does that check for you. Read more in
[the guide](https://coop.dryga.com/docs.html#sandbox), [the box](https://coop.dryga.com/box.html) and
[networking](https://coop.dryga.com/networking.html).

## Hand off the work

### A task queue that works overnight

Each task is a folder in `.agent/tasks`, written so an agent can start without asking questions.
`coop loop` works through the queue on its own, with a fresh agent for every task. A final review
reopens unfinished work. A task that needs your decision waits for you, and the loop moves on.

```bash
coop tasks add "Add a /health endpoint" \
  --context "The load balancer needs a cheap liveness check." \
  --acceptance "GET /health returns 200 with {\"ok\":true}, and a test covers it." \
  --approach "Add the route beside the others, then a handler test." \
  --subtask "Route and handler" --subtask "Test"
coop loop claude
```

See [the loop](https://coop.dryga.com/loop.html).

### Forks

A fork gives an agent its own copy of your repo, so you can keep working in yours. You review the
fork like a pull request. When you merge it, co:op rebases it onto your branch and runs your checks
before your branch moves.

```bash
coop fork api codex --loop -d     # work the queue in a fork, in the background
coop fork review api --stat       # its commits and changed files
coop fork merge api               # rebase, run your checks, merge
```

See [forks](https://coop.dryga.com/forks.html).

### Accounts, models and presets

Sign in to more than one account, and a loop that hits a usage limit moves to the next account or
model and keeps going. Editor sessions switch the same way. You pick the model, effort and account
in the target, as in `coop claude:opus/xhigh@work`.

A preset is a team of models in one file: a lead, its fallbacks, and the roles it consults or hands
work to. `coop presets init` writes a starter team you can change, and `coop loop frontier` runs it.

See [agents, accounts and presets](https://coop.dryga.com/agents.html).

`coop usage` shows your account limits and estimates your usage from activity on this computer.
Optional [provider reports](https://coop.dryga.com/usage-reports.html) show API spend separately.

## Your project's tools and services

If your repo pins versions in `.tool-versions`, `coop init` writes an `.agent/Dockerfile` that
installs them, and `coop build` builds it into the box's image. For anything more, edit or write
`.agent/Dockerfile` yourself. Postgres, Redis and anything else in your Compose file run beside
the box, each in its own container. With filtered networking they share a private network with
no direct internet. `coop up` starts them, `coop down` stops them and keeps their data, and
`coop down --delete-volumes` also deletes their data.

See [the box](https://coop.dryga.com/box.html).

## Editors, MCP and your own platform

Steer the sandboxed agent from Zed, or any editor that speaks ACP, with account rotation built in.
Define MCP servers once, outside your repo, and co:op makes them available to every supported
agent. You can also connect co:op workers to your own platform, which hands out the jobs. Models
never see your GitHub credentials.

See [editors, MCP and controllers](https://coop.dryga.com/integrations.html) and the [session API](https://coop.dryga.com/session-api.html).

## Evaluations

Evaluations show which model, preset or loop recipe works best on repeatable tasks. A separate
verifier grades each result, so the model never grades its own work. Start with a free preview,
which starts no container and spends nothing:

```bash
coop eval run core codex --timeout 35m --dry-run
```

See [evaluations](https://coop.dryga.com/evals.html).

## Documentation

[The docs](https://coop.dryga.com/docs.html) have a menu and search, and they cover every feature:

- Get started: [overview](https://coop.dryga.com/docs.html), [install](https://coop.dryga.com/install.html),
  [first sandbox run](https://coop.dryga.com/sandbox-guide.html) and
  [first overnight run](https://coop.dryga.com/overnight-guide.html).
- Sandbox: [files, images and services](https://coop.dryga.com/box.html) and
  [networking](https://coop.dryga.com/networking.html).
- Agents: [accounts, models and presets](https://coop.dryga.com/agents.html),
  [editors and MCP](https://coop.dryga.com/integrations.html) and
  [usage and billing](https://coop.dryga.com/usage-reports.html).
- Hand off work: [task queues and loops](https://coop.dryga.com/loop.html),
  [forks and review](https://coop.dryga.com/forks.html) and
  [evaluations](https://coop.dryga.com/evals.html).
- Reference: [commands](https://coop.dryga.com/cli.html),
  [configuration](https://coop.dryga.com/configuration.html),
  [the worker API](https://coop.dryga.com/session-api.html) and
  [upgrading](https://coop.dryga.com/migrating.html).
- What changed: [the changelog](CHANGELOG.md).
- The background, in two write-ups:
  [Running an AI coding agent you can't trust](https://dryga.com/blog/untrusted-ai-coding-agent/)
  (the sandbox) and [One brain, two agents](https://dryga.com/blog/os-for-coding-agents/) (the
  queue, the hooks and the loop that runs it unattended).

## Build and test

co:op is one static Go binary. `make install` builds it into `~/.local/bin`, and `make check` runs
nearly every check CI runs. [CONTRIBUTING.md](CONTRIBUTING.md) covers the source layout, the test
layers and the one CI pass that `make check` leaves out.

## License

MIT. See [LICENSE](LICENSE).
