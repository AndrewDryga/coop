<div align="center">

# Coop

<img src=".github/assets/coop.png" alt="Coop" width="180">

**Run a coding agent on your real repos every day — in a box it can't escape, with your secrets out of its reach.**

[![CI](https://github.com/AndrewDryga/coop/actions/workflows/ci.yml/badge.svg)](https://github.com/AndrewDryga/coop/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/AndrewDryga/coop?sort=semver)](https://github.com/AndrewDryga/coop/releases/latest)
[![Go Report Card](https://goreportcard.com/badge/github.com/AndrewDryga/coop)](https://goreportcard.com/report/github.com/AndrewDryga/coop)
[![License](https://img.shields.io/github/license/AndrewDryga/coop)](LICENSE)

</div>

Coding agents are most useful with the brakes off (`--dangerously-skip-permissions`,
`--yolo`) — and that's exactly when you don't want them loose on your laptop.
**Coop** runs them in a disposable container that mounts only the repo you're
working on, shadows its secrets, and can't reach your home dir, SSH keys, or other
projects. One `coop` command, installed once; the same box drives Claude, Codex,
and Gemini.

It's the working tooling behind two write-ups:
[Running an AI coding agent you can't trust](https://dryga.com/blog/untrusted-ai-coding-agent/)
(the sandbox) and [One brain, two agents](https://dryga.com/blog/os-for-coding-agents/)
(the queue, hooks, and the foreman that runs it unattended).

---

## Contents

- [Install](#install)
- [Quickstart](#quickstart)
- [Command reference](#command-reference)
- [The sandbox](#the-sandbox) — secrets shadowed · `coop doctor` · fork handoff
- [Agents & config](#agents--config) — auth · instructions · MCP
- [Fusion: a governed council](#fusion-a-governed-council)
- [Drive it from Zed (ACP)](#drive-it-from-zed-acp)
- [Run it unattended](#run-it-unattended) — the loop · `.agent/` · a fleet
- [Project toolchain & services](#project-toolchain--services) — `.tool-versions` · `Dockerfile.agent` · devcontainers
- [Configuration](#configuration)
- [Layout & development](#layout--development)

---

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/AndrewDryga/coop/main/install.sh | sh
```

Downloads the prebuilt `coop` binary for your OS/arch into `~/.local/bin` — no Go,
no clone. If a container runtime is present, the installer also builds the sandbox
image and runs `coop doctor`; otherwise do it once yourself:

```bash
coop build && coop doctor
```

**Requirements:** a container runtime — Apple
[`container`](https://github.com/apple/container) (macOS 26+), Docker, or Podman —
auto-detected. `coop` itself is a single static binary with no other dependencies.

**Staying current:** re-run the one-liner to upgrade the **binary**;
[`coop update`](#keeping-the-box-current) rebuilds the **box image** fresh, pulling
the latest agent CLIs and ACP adapters (they ship features often) plus a newer base.

<details><summary><b>Other ways to install</b></summary>

```bash
go install github.com/AndrewDryga/coop@latest                              # with Go
git clone https://github.com/AndrewDryga/coop && cd coop && make install   # from source
```
</details>

## Quickstart

```bash
cd ~/code/some-repo

coop claude           # sandboxed Claude — no permission prompts, secrets shadowed
coop codex            # same box, Codex instead
coop gemini           # ...or Gemini
coop fusion           # a council: one model leads, the other two advise, then it synthesizes
coop shell            # a shell in the box, to look around
coop run -- npm test  # run any command in the box
```

That's it — point it at a repo and go. Each agent launches with its own
"don't stop to ask" flags (`--dangerously-skip-permissions`,
`--dangerously-bypass-approvals-and-sandbox`, `--yolo`), all inside the same
sandbox. The worst an off-the-rails agent can do is trash one repo you can restore
from git.

## Command reference

| Command | What it does |
|---|---|
| `coop` | print help — running an agent is explicit (`coop claude`) |
| `coop claude` · `codex` · `gemini` `[args]` | a sandboxed agent — its autonomous flags, plus any args you add (e.g. `coop claude --continue`) |
| `coop fusion [--governor g]` | a [governed council](#fusion-a-governed-council): `g` leads, the other two advise |
| `coop run -- <cmd>` | run any command in the box |
| `coop shell` | a shell in the box |
| `coop login <agent>` | [authenticate](#authentication) an agent (persists in the config dir) |
| `coop acp [agent\|fusion]` | run as an [ACP](#drive-it-from-zed-acp) agent over stdio (for Zed) |
| `coop loop` | [work `.agent/TASKS.md`](#the-loop) unattended until done, then audit |
| `coop dispatch <name>` | [a fleet unit](#a-fleet): fork + that agent's queue slice + loop |
| `coop fork <name> [agent]` | open/resume a [secrets-free fork](#fork-a-clone) + run an agent in it |
| `coop fork ls\|review\|merge\|rm` | [list · diff · merge back · discard](#fork-a-clone) your forks |
| `coop init [--stack asdf]` | [scaffold](#project-toolchain--services) the queue, hooks, skills (+ a toolchain) |
| `coop up` · `down` | start/stop [sibling services](#services) (Postgres, Redis) for this repo |
| `coop build` · `update` | build the box image · [rebuild it fresh](#keeping-the-box-current) (latest agents/adapters) |
| `coop doctor` | [prove isolation](#prove-it-coop-doctor) — attack the box and check it holds |
| `coop help` · `version` | — |

## The sandbox

The repo is bind-mounted into the box at the **same path it has on your machine**;
the agent edits real files and you see them live in your editor. Everything else —
your home dir, SSH keys, the rest of the disk — simply isn't in the container.

### Secrets never enter the box

`.env`, `*.tfvars`, `*.pem`, `secrets/`, `.ssh`, and friends are **shadowed**: an
empty `tmpfs` over secret directories, a read-only empty file over secret files.
Templates (`*.example`, `*.sample`, `*.template`) stay visible so the agent can
still see the shape of your config. The shadow / keep-visible patterns are compiled
in (see `internal/box/secrets.go`).

### Prove it: `coop doctor`

```bash
coop doctor
```

`doctor` plants a fake secret, launches the box, and checks **from inside** that the
secret is unreachable and unwritable — then checks on the host that a fork carries
neither the secret nor a pushable remote. Run it anytime, especially after changing
config.

### Fork a clone

For an extra layer (or to run several agents at once), hand the agent a throwaway
**fork** — a local clone of your repo — instead of your working tree. A fork has no
gitignored secrets (they were never committed) and its `origin` is a local path, so
the agent has nowhere to push. Treat it like a contractor's PR: open, review, merge,
close.

```bash
coop fork perf codex     # fork into ../<repo>-forks/perf; run codex there
coop fork perf           # later: re-enter — continues your last session by default
coop fork perf --new     # …or re-enter with a fresh session
coop fork ls             # your forks: branch, changes, last activity
coop fork review perf    # fetch the fork's branch + show the diff
coop fork merge perf     # rebase it onto your branch (linear), then close it
coop fork rm perf        # discard (refuses unmerged/dirty work without --force)
```

The agent (`claude` by default; pass `codex`/`gemini` to pick the model) works in
the fork; you stay the reviewer and the only one who can push. `coop clone` is a
back-compat alias for `coop fork`. The agent's session history persists in the fork,
so re-entering **continues your last conversation by default** (claude `--continue`,
gemini `--resume latest`, and codex by the session whose recorded `cwd` is that fork) —
scoped to the fork, falling back to a fresh session when none exists. Pass `--new` to
start fresh.

`coop fork review` shows a brief (commits, files, the agent's reasoning) then the diff
in your pager — no setup. To review in an IDE instead: `--tool` hands the diff to your
configured GUI difftool (`git config diff.tool` — VS Code, JetBrains, Meld, vim…),
`--open` opens the fork in your editor (`$COOP_EDITOR`, else a detected
`code`/`cursor`/`zed`/`idea`/`subl`, else `$VISUAL`/`$EDITOR`) so you review via its
SCM panel, and **`COOP_REVIEW_CMD`** overrides it entirely (run via `sh -c` with
`$COOP_FORK_PATH` / `$COOP_FORK_NAME` / `$COOP_REVIEW_REF` set).

## Agents & config

One box, three agents. Each reads its config and credentials from
`~/.config/coop/agents/<name>/`, mounted into the box at `~/.claude`, `~/.codex`,
and `~/.gemini`. That directory lives outside any repo, so credentials never land
in git. Edit those files on the host; they take effect in the box.

### Authentication

```bash
coop login claude     # interactive login; the token persists in agents/claude/
coop login codex      # device-code login (the box has no browser for an OAuth redirect)
coop login gemini     # or it logs in on first use
```

…or use API keys — drop them in the env file, which is passed into every box:

```bash
echo 'ANTHROPIC_API_KEY=sk-…'  >> ~/.config/coop/agents/env
echo 'GEMINI_API_KEY=AIza…'    >> ~/.config/coop/agents/env
```

On first run the box pre-answers each agent's setup prompts — Claude's
theme/folder-trust/bypass warnings, Codex's "trust this directory?", and Gemini's
folder-trust — so a fresh install goes straight from login to work. (The box is the
sandbox, so trusting the one mounted repo is the intended posture.)

### Instructions, one source of truth

`coop init` wires a tool-neutral setup so every agent reads the same instructions:
`CLAUDE.md` and `GEMINI.md` symlink to a canonical `AGENTS.md`, and Codex shares
Claude's skills directory. A real (non-symlink) instruction file you already have
is left untouched. A shared `~/.config/coop/agents/INSTRUCTIONS.md` is also wired
into each agent's global instruction path.

### MCP servers, defined once

Drop your MCP servers into `~/.config/coop/agents/mcp.json` (the standard
`{ "mcpServers": { ... } }` shape) and **all three** agents pick them up:

```bash
cp agents/mcp.json.example ~/.config/coop/agents/mcp.json   # then edit
```

`coop` wires that one file into each agent's native mechanism on launch: Claude via
`--mcp-config`, Gemini merged into its `settings.json`, Codex converted to
`[mcp_servers.*]` in its `config.toml`. The Gemini/Codex versions are generated
read-only on top of your existing config — your own files are never touched, and
servers from `mcp.json` win on a name clash.

## Fusion: a governed council

One model **leads** (the *governor*) and does the real work; the other two
**advise read-only**; the leader **synthesizes** the best of all three. A council
that argues before it commits beats any of its members working alone — the
synthesized answer outperforms even the single strongest model on its own,
**Fable 5 included**. You stop betting the run on one model's blind spots. It's a
mode like any other agent — interactive, headless, or in Zed:

```bash
coop fusion                    # codex leads (COOP_FUSION_GOVERNOR); claude + gemini advise
coop fusion --governor claude  # claude leads instead
coop fusion -- -p "Design the retry strategy"   # headless; args pass to the leader
```

**No extra service or protocol** behind it: the leader is just that agent running
normally (it edits, runs the gate, streams), plus a fusion instruction injected
**into the leader's instruction file only**. For a non-trivial question it consults
its peers from its shell — read-only and in parallel:

```bash
claude -p --permission-mode plan "<question>"   # read-only: returns its approach, never edits
gemini --approval-mode plan -p   "<question>"
codex  exec -s read-only         "<question>"
```

It then merges the strongest parts, resolves disagreements by verification, and
proceeds. Because the instruction lands only on the leader, the peers it spawns read
their normal instructions and never recurse into a council of their own. Each
consultation is two extra read-only runs, so it's for decisions and hard problems,
not every keystroke — the leader is told to skip the council for trivial steps.

**In Zed:** add one entry per leader and pick the one you want from the agent
dropdown to switch who governs:

```json
"agent_servers": {
  "coop fusion (codex)":  { "command": "coop", "args": ["acp", "fusion", "codex"] },
  "coop fusion (claude)": { "command": "coop", "args": ["acp", "fusion", "claude"] },
  "coop fusion (gemini)": { "command": "coop", "args": ["acp", "fusion", "gemini"] }
}
```

### Second opinions (`--consult`)

Outside fusion, add `--consult` to a normal run — `coop claude --consult` (or
`codex`/`gemini`; in Zed, `coop acp claude --consult`) — for a lighter version of
the same idea: on a genuinely hard or risky call the agent **may** consult its peers
read-only and in parallel to catch blind spots, then decide. It's **off by default**
and, unlike fusion, optional — no synthesis mandate, not for routine work. It only
names peers that are **authenticated**: if no other agent is logged in, nothing is
injected. And it's scoped to the agent you launched, so peers it spawns read their
normal instructions and never recurse into a council of their own.

## Drive it from Zed (ACP)

The box can act as an [ACP](https://agentclientprotocol.com) agent, so you steer
the sandboxed agent from Zed's own agent panel — **Zed is the cockpit, the box stays
the cage**. Four steps:

**1. Install** (once). The [install one-liner](#install) puts `coop` on your `PATH`
and builds the image with the ACP adapters baked in. Check it resolves:

```bash
command -v coop      # e.g. /Users/you/.local/bin/coop
```

**2. Authenticate** the agent you'll use (see [Authentication](#authentication)):

```bash
coop login claude    # or codex / gemini
```

**3. Register it in Zed.** In the agent panel use **Add Custom Agent**, or edit
`settings.json` directly:

```jsonc
{
  "agent_servers": {
    "coop": {
      "type": "custom",
      "command": "coop",           // absolute path if Zed's PATH lacks ~/.local/bin
      "args": ["acp", "claude"],   // or "codex" / "gemini" / "fusion"
      "env": {}
    }
  }
}
```

> GUI apps don't always inherit your shell's `PATH`. If Zed can't find `coop`, use
> the absolute path from step 1 as `command`.

**4. Use it.** Open the agent panel, pick **coop** from the dropdown, and start a
thread. Zed launches `coop acp <agent>` with the project as cwd; the agent runs in
the box, edits your files over ACP, and you approve its tool calls in Zed (or let
them run — the box is the boundary).

Under the hood `coop acp [claude|codex|gemini|fusion]` runs the matching adapter
(`@agentclientprotocol/claude-agent-acp`, `@zed-industries/codex-acp`,
`gemini --acp`) inside the box over stdio. The repo mounts at its **real host
path** — the same path `coop` and `coop loop` use — so Zed's absolute paths resolve
*and* the session history lines up: a thread you started with `coop loop` is there
to resume in Zed.

> **Services** work too — if the repo has a `compose.agent.yml`, run `coop up`
> first and the ACP box joins the same network.
> **Custom images** must carry the ACP adapters: `coop init` scaffolds them
> in; for an older/hand-written `Dockerfile.agent`, add
> `@agentclientprotocol/claude-agent-acp` and `@zed-industries/codex-acp` to its
> `npm install -g` line (else `coop acp` fails with `codex-acp: not found`).

## Run it unattended

### The loop

```bash
coop init             # scaffold AGENTS.md, the .agent/ working folder, and the hooks
# ...fill in .agent/TASKS.md with checkbox tasks...
coop loop             # disposable agents work the queue until it's done, then audit
```

`loop` starts a **fresh agent per iteration** (no context rot), works unchecked
`[ ]` items, and won't quit while any remain. When the queue empties, a fresh
auditor re-checks every `[x]` against the git log and reopens anything that doesn't
hold up. `init` also installs a `Stop` hook (won't let a session end with work
outstanding) and a fast commit-gate hook.

If the model hits a **rate or usage limit** mid-run, the loop doesn't treat it as a
failure: it reads the reset time from the agent's own output, waits it out with a
countdown, and resumes the same item once the limit clears — so an overnight run
rides through the daily cap instead of burning retries against it.

`init` also installs generic **workflow skills** into `.claude/skills/` (shared with
Codex): `/spec` a multi-file change, `/work` it step-by-step against the gate,
`/sweep` to drain `.agent/TASKS.md`, and `/verify-api` before calling anything you're
unsure of. Edit them freely — `init` won't overwrite a skill you've changed.

### The `.agent/` working folder

`init` creates a tool-neutral working folder the agent reads back on every boot (and
after each compaction). Everything here is local working state and git-ignored —
**except `rules/`**, the shared knowledge base, which is committed.

| File | What it's for |
|---|---|
| `TASKS.md` | the work queue — `[ ]` todo · `[w]` claimed · `[x]` done+gated+committed · `[B]` blocked (the loop reads only this) |
| `BACKLOG.md` | work found *outside* the current task — captured, not auto-worked; a human promotes items into `TASKS.md` |
| `LOG.md` | the agent's chain-of-thought (what + why), so intent survives a compaction |
| `PENDING_DECISIONS.md` | anything needing a human call — decision, options, recommendation; the task goes `[B]` |
| `IDEAS.md` | product ideas, never auto-implemented — a human moves one into `TASKS.md` first |
| `rules/` | the taste knowledge base — corrections graduate into rules here (the one committed part) |

### A fleet

Split the queue into per-agent slices (`.agent/TASKS.<name>.md`) and hand each one
to a different model, each looping unattended in its own fork:

```bash
coop fork perf codex  --loop -d    # codex loops the perf slice, detached
coop fork deps gemini --loop -d    # gemini takes the deps slice
coop fork docs claude --loop -d    # claude takes the docs

coop fork ls            # who's running, how big the diff, last activity
coop fork logs -f       # tail every fork at once (compose-style, prefixed)
coop fork stop perf     # halt one; coop fork logs perf -f to watch just it
```

`--loop` seeds the fork from its `TASKS.<name>.md` slice and runs the loop with the
chosen model (claude `-p`, codex `exec`, gemini `-p`); `-d` detaches it (logs are
captured to `../<repo>-forks/.coop/<name>.log`). When one finishes, review and merge
it like a contractor PR (`coop fork review <name>` · `coop fork merge <name>`), then
`git push` — the only step the agents can't do. Add agents until *review*, not
generation, is your bottleneck. (`coop dispatch <name> [agent]` is the one-liner
equivalent: fork + slice + loop, foreground.)

Landing **rebases** the fork onto your current branch and fast-forwards — linear
history, no merge commits. **Land the whole fleet at once** with `coop fork merge
--all` — a revalidating rebase *queue*: it rebases each fork onto the result of the
last, so a "green" fork can't ride in against a base an earlier landing already
changed. Set **`COOP_GATE`** (e.g. `COOP_GATE="make check"`) and every `coop fork
merge` re-runs that gate in the box on the rebased tree, rolling back if it goes red —
the machine gate behind your human review. It stops at the first conflict or red gate,
leaving the rest untouched.

**Declare the fleet once** in `.agent/fleet` (one fork per line, `<name> [agent]`):

```
perf  codex
deps  gemini
docs  claude
```

Then `coop fleet up` starts them all detached, `coop fleet ls` shows the board, and
`coop fleet down` stops them. `coop fleet split <n>` mechanically round-robins your
`.agent/TASKS.md` into per-fork slices to get started (use an agent for *semantic*
slicing). Two guards make review trustworthy: `coop fork review <name>` opens with a
**brief** — commits, files, and the agent's own `.agent/LOG.md` reasoning — before the
diff; and `coop fork merge` runs a **policy check** that blocks secret-looking or
oversized files from sneaking in (override with `--force`).

## Project toolchain & services

Real projects need a language toolchain (Elixir, Go, …) and stateful services
(Postgres, Redis). The agent does **not** install those at runtime — that's slow,
non-reproducible, and dies with the container. Declare them once instead. This is
the Dev Containers + Compose model, minus the ceremony.

### `.tool-versions` — zero config

If your repo pins versions in a `.tool-versions` (asdf), the base box provisions
that toolchain **at runtime** — resolved from the working dir up the tree, or
`~/.tool-versions` — and caches it in a shared volume. So a repo with *just* a
`.tool-versions` (no `Dockerfile.agent`, no scaffolding) gets its toolchain with
zero setup:

```bash
cd ~/code/phoenix-app   # has a .tool-versions
coop claude             # provisions elixir/erlang/node/… from it, then runs the agent
```

The first install of a new toolchain can be slow (e.g. Erlang compiles), then it's
reused across runs and repos. Set `COOP_NO_ASDF=1` (in `agents/env`) to skip it.
For a **baked, fully-reproducible** image instead, `coop init` (or
`coop init --stack asdf`) scaffolds an asdf `Dockerfile.agent` that installs the
same `.tool-versions` at build time.

### `Dockerfile.agent` — a per-project image

```bash
coop init --stack asdf   # writes an asdf Dockerfile.agent (from .tool-versions) + compose.agent.yml
coop build               # builds it, tagged coop-<repo-name> — its own image
```

A repo with its own `Dockerfile.agent` gets its **own** image tag, so projects never
collide, and every `coop`, `coop loop`, `coop fork`, `coop acp` in that repo uses
it. The scaffolded one is the **asdf** image — it bakes in the exact `.tool-versions`
toolchain (versions live there, not in the Dockerfile). For anything more exotic,
hand-write a `Dockerfile.agent` (see the box contract below). When the agent needs a
new system package, add it to the `RUN` line and `coop build` again — the dependency
*graduates into the image* instead of being installed each run.

<details><summary><b>The box contract (build any base)</b></summary>

An image is a valid agent box when:

1. **It runs as a non-root user** — Claude Code refuses `--dangerously-skip-permissions` as root.
2. **That user's home is `/home/node`** — the `agents/` auth mounts land at `$HOME/.claude`, `$HOME/.codex`, `$HOME/.gemini`. (Different base? Set `COOP_HOME_IN_BOX=/home/<user>`.)
3. **`claude`, `codex`, `gemini` are on `PATH`** (so it needs Node) — plus the ACP adapters if you want `coop acp`.
4. **`git config --system --add safe.directory '*'`** — git works on the host-owned bind mount (which lives at the repo's real path, not a fixed `/workspace`).

coop sets the working directory itself, so no `WORKDIR` is required. A skeleton:

```dockerfile
FROM <your-language-base>
RUN <install your toolchain> \
 && npm install -g @anthropic-ai/claude-code @openai/codex @google/gemini-cli \
      @agentclientprotocol/claude-agent-acp @zed-industries/codex-acp \
 && git config --system --add safe.directory '*' \
 && id -u node >/dev/null 2>&1 || useradd -m -u 1000 -s /bin/bash node
USER node
```

(If the base lacks Node, install it first — the asdf template uses NodeSource.)
</details>

<details><summary><b>Reusing an existing devcontainer</b></summary>

If a repo already has a `.devcontainer/`, reuse its image as your base and add the
agent layer on top:

```dockerfile
FROM your-devcontainer-image          # the team's source of truth for the env
RUN npm install -g @anthropic-ai/claude-code @openai/codex @google/gemini-cli \
      @agentclientprotocol/claude-agent-acp @zed-industries/codex-acp \
 && git config --system --add safe.directory '*'
USER <the devcontainer's non-root user>
# If that user's home isn't /home/node, run with COOP_HOME_IN_BOX=/home/<user>.
```

Division of labour: **devcontainer = what's in the environment** (toolchain,
features, reproducibility); **Coop = running an untrusted agent in it safely**
(secret shadowing, the container boundary, the queue + foreman). Don't lean on the
devcontainer as the security boundary — by itself it mounts your whole workspace
(`.env` included), and a malicious project under `--dangerously-skip-permissions`
can exfiltrate `~/.claude`. The shadowing and the box are what Coop adds on top.
</details>

### Services

```bash
coop up        # starts Postgres + Redis from compose.agent.yml, waits until healthy
coop claude    # the box reaches them by name (db, redis)
coop down -v   # stop services and wipe their throwaway data
```

Services run as their own containers on a private network the box joins — connect
with e.g. `DATABASE_URL=postgres://postgres:postgres@db:5432/app_dev` (put it in
`agents/env`). The agent never installs or hosts a database, so it can't corrupt
one, and `coop down -v` resets to a clean slate. A shared `coop-cache` volume at
`~/.cache` keeps disposable runs from re-downloading the world.

### Keeping the box current

```bash
coop update    # rebuild the image fresh: latest agent CLIs + ACP adapters + base
```

`coop build` is cache-bound and won't pull newer package versions; `coop update`
rebuilds with `--pull --no-cache` and prints the resulting versions. The agent CLIs
and ACP adapters ship features often, so this is the easy way to stay current.

## Configuration

Set via env vars, or `~/.config/coop/coop.conf` (`KEY=VALUE` lines, same names —
the environment wins over the file):

| Var | Default | |
|---|---|---|
| `COOP_IMAGE` | (auto) | force a specific image (overrides `Dockerfile.agent` detection) |
| `COOP_BASE_IMAGE` | `coop-box` | the shared base image tag |
| `COOP_RUNTIME` | auto | `container` / `docker` / `podman` |
| `COOP_FUSION_GOVERNOR` | `codex` | default leader for `coop fusion` |
| `COOP_CONFIG_DIR` | `~/.config/coop/agents` | per-agent auth + settings folder |
| `COOP_HOME_IN_BOX` | `/home/node` | where auth + instructions mount in the box |
| `COOP_CLAUDE_CMD` · `COOP_CODEX_CMD` · `COOP_GEMINI_CMD` | autonomous defaults | per-agent command |
| `COOP_NETWORK` · `COOP_CACHE` | `1` | join services network / mount cache volume |
| `COOP_NO_ASDF` | (off) | skip runtime `.tool-versions` provisioning |
| `COOP_SERVICES_NET` | (auto) | services network to join (let a fleet share one db) |
| `COOP_WORKDIR` | (real path) | override where the repo mounts in the box |

## Layout & development

A single static Go binary plus a config folder. A repo you work on optionally
carries a `Dockerfile.agent` (its toolchain) and `compose.agent.yml` (its services):

```
main.go             entrypoint
internal/box/       the engine: secret-shadowing mounts, image selection, container run
internal/fusion/    the council: peer commands + the governor instruction
internal/mcp/       one mcp.json → Claude / Gemini / Codex native configs (no Python)
internal/scaffold/  `coop init` templates + the workflow skills (embedded in the binary)
internal/cli/       command dispatch, grouped help, doctor
internal/config·runtime·ui/   settings · runtime detection · terminal output
agents/             example config (env.example, mcp.json.example); copied to ~/.config/coop on install
skills/             the workflow skills (spec · work · sweep · verify-api), also embedded
install.sh          the curl one-liner: download the prebuilt binary onto PATH
```

```bash
make build     # build ./coop
make check     # gofmt + vet + staticcheck + unit tests (what CI runs; no Docker needed)
make doctor    # the integration check — proves isolation, needs a runtime
```

The security-critical logic — secret enumeration (`internal/box/mounts.go`) and
run-arg assembly (`internal/box/run.go`) — is pure and unit-tested without a
runtime; `coop doctor` proves the whole thing end-to-end against the real box.
