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
**Coop** runs them in a disposable container that mounts only the repo you're working
on, shadows its secrets, and can't reach your home dir, SSH keys, or other projects.
One command, installed once; the same box drives **Claude, Codex, and Gemini**.

```bash
cd ~/code/some-repo && coop claude     # a sandboxed Claude, brakes off, secrets hidden
```

It's the working tooling behind two write-ups:
[Running an AI coding agent you can't trust](https://dryga.com/blog/untrusted-ai-coding-agent/)
(the sandbox) and [One brain, two agents](https://dryga.com/blog/os-for-coding-agents/)
(the queue, hooks, and the foreman that runs it unattended).

---

## Contents

- [Install](#install) · [Happy path](#happy-path-5-commands) · [Quickstart](#quickstart) · [Command reference](#command-reference)
- [The sandbox](#the-sandbox) — what's mounted · secrets shadowed · git identity · `coop doctor`
- [Forks](#forks-hand-off-work-like-a-pr) — open · review · land work like a contractor's PR
- [Agents & config](#agents--config) — authentication · instructions · MCP servers
- [Fusion](#fusion-a-governed-council) — a council of models that argues before it commits
- [Drive it from Zed (ACP)](#drive-it-from-zed-acp)
- [Run it unattended](#run-it-unattended) — the loop · the `.agent/` folder · a fleet
- [Project toolchain & services](#project-toolchain--services) — `.tool-versions` · `Dockerfile.agent` · services
- [Configuration](#configuration) · [Troubleshooting](#troubleshooting) · [Layout & development](#layout--development)

---

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/AndrewDryga/coop/main/install.sh | sh
```

Downloads the prebuilt `coop` binary for your OS/arch into `~/.local/bin` — no Go, no
clone. If a container runtime is present, the installer also builds the sandbox image
and runs `coop doctor`; otherwise do that once yourself:

```bash
coop build && coop doctor
```

**Requirements:** a container runtime — Apple
[`container`](https://github.com/apple/container) (macOS 26+), Docker, or Podman,
auto-detected. `coop` itself is a single static binary with no other dependencies.

**Staying current:** re-run the one-liner to upgrade the **binary**;
[`coop update`](#keeping-the-box-current) rebuilds the **box image** fresh, pulling the
latest agent CLIs and ACP adapters (they ship features often) plus a newer base.

<details><summary><b>Other ways to install</b></summary>

```bash
go install github.com/AndrewDryga/coop@latest                              # with Go
git clone https://github.com/AndrewDryga/coop && cd coop && make install   # from source
```
</details>

<details><summary><b>Verifying a download</b></summary>

`install.sh` verifies automatically: when [cosign](https://github.com/sigstore/cosign)
is on your `PATH` it checks `checksums.txt`'s keyless Sigstore signature (so the
checksum file itself is trusted, not just internally consistent) and aborts on failure;
otherwise it compares the archive's SHA-256 and prints that the signature was not
verified. To verify by hand, set `VER` and `ASSET` for your platform — e.g.
`VER=v0.1.0 ASSET=coop_0.1.0_darwin_arm64.tar.gz`:

```bash
base="https://github.com/AndrewDryga/coop/releases/download/$VER"
curl -fsSLO "$base/$ASSET"
curl -fsSLO "$base/checksums.txt"
curl -fsSLO "$base/checksums.txt.bundle"

# 1. checksums.txt is signed by the release workflow (keyless Sigstore):
cosign verify-blob checksums.txt \
  --bundle checksums.txt.bundle \
  --certificate-identity-regexp '^https://github.com/AndrewDryga/coop/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

# 2. the archive matches the now-trusted checksum (Linux: sha256sum -c -):
awk -v f="$ASSET" '$2==f{print $1"  "f}' checksums.txt | shasum -a 256 -c -
```
</details>

## Happy path (5 commands)

From nothing to a sandboxed agent landing reviewed work:

```bash
curl -fsSL https://raw.githubusercontent.com/AndrewDryga/coop/main/install.sh | sh
# ^ installs the binary, and (if a runtime is present) builds the box + runs coop doctor

cd ~/code/your-repo          # 1. any git repo
coop doctor                  # 2. prove isolation holds (builds the box first if needed)
coop login claude            # 3. authenticate once (token persists; paste-code, no browser needed)
coop claude                  # 4. a sandboxed agent, brakes off, your secrets shadowed
coop fork feature claude     # 5. or hand off a branch: agent works in a throwaway clone…
coop fork review feature     #    …you review the diff…
coop fork merge feature      #    …and land it (rebased onto your branch, signed if you sign)
```

If `coop doctor` says the image isn't built, run `coop build` once. Stuck on any step?
See [Troubleshooting](#troubleshooting).

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

Point it at a repo and go. Each agent launches with its own "don't stop to ask" flags
(`--dangerously-skip-permissions`, `--dangerously-bypass-approvals-and-sandbox`,
`--yolo`), all inside the same sandbox. The worst an off-the-rails agent can do is
trash one repo you can restore from git.

Anything after the agent name is **passed through** to it, on top of those flags — so
`coop claude --continue` resumes Claude's last session, still sandboxed.

## Command reference

Every command runs against the repo in your current directory. `-h`/`--help` works on
any of them.

**Run an agent**

| Command | What it does |
|---|---|
| `coop claude` · `codex` · `gemini` `[args]` | a sandboxed agent — its autonomous flags, plus any args you add |
| `coop fusion [agent]` | a [governed council](#fusion-a-governed-council): that agent leads, the other two advise |
| `coop <agent> --consult` | [opt-in second opinion](#second-opinions---consult) — may ask authed peers on hard calls |
| `coop run -- <cmd>` | run any command in the box (raw — none of coop's agent flags) |
| `coop shell` | a shell in the box, to look around |
| `coop acp [agent\|fusion]` | run as an [ACP](#drive-it-from-zed-acp) agent over stdio (for Zed) |
| `coop login <agent>` | [authenticate](#authentication) an agent (token persists in the config dir) |

**Forks** — hand off work like a PR ([details](#forks-hand-off-work-like-a-pr))

| Command | What it does |
|---|---|
| `coop fork <name> [agent] [--new]` | open or re-enter a [secrets-free fork](#forks-hand-off-work-like-a-pr) + run an agent (re-entry resumes the session; `--new` resets) |
| `coop fork <name> <agent> --loop --tasks <path> [-d]` | loop a tasks file unattended in the fork (`-d`/`--detach` backgrounds it) |
| `coop fork ls` | list this repo's forks: agent, branch, state, tasks done/total, change size, last activity |
| `coop fork review <name> [--tool\|--open]` | brief + diff; `--tool` = your `git difftool`, `--open` = your editor |
| `coop fork merge <name> [--all] [--yes]` | rebase the fork onto your branch and land it (`--all` = the whole fleet; `--yes` confirms non-interactively) |
| `coop fork rm <name> [--force]` | discard a fork (refuses unmerged/dirty work without `--force`) |
| `coop fork open <name>` · `path <name>` | open the fork in your editor · print its filesystem path |
| `coop fork <name> acp [agent]` | drive the fork's [sandboxed agent from Zed](#drive-a-fork-from-zed-acp) over ACP |
| `coop fork logs [name] [-f]` · `stop <name>` | tail a loop log (no name = all) · stop a detached loop |

**Run unattended** ([details](#run-it-unattended))

| Command | What it does |
|---|---|
| `coop loop [agent] [--debug-on-fail]` | work [`.agent/TASKS.md`](#the-loop) unattended until done, then audit (`claude` default; `codex`/`gemini` too); `--debug-on-fail` opens a box shell on an iteration failure |
| `coop fork <name> <agent> --loop --tasks <path>` | loop [one fork](#a-fleet) on a tasks file (`-d` detaches) |
| `coop fleet up` · `down` · `split <n>` | drive a [declared fleet](#a-fleet) from `.agent/fleet` (list with `coop fork ls`) |
| `coop status` | fleet roll-up — per fork: running/idle, tasks done/total, blockers, diff size, the task it's on |
| `coop tasks list` · `lint` · `add "<title>"` · `split <n>` | inspect/validate `.agent/TASKS.md` — `lint` flags stale `[w]` claims and tasks that aren't [self-contained](#the-loop); `split` carves it into self-contained slices |

**Set up & maintain**

| Command | What it does |
|---|---|
| `coop init [--stack asdf]` | [scaffold](#project-toolchain--services) the queue, hooks, skills (and optionally a toolchain) |
| `coop up` · `down [-v]` | start/stop [sibling services](#services) (Postgres, Redis) for this repo |
| `coop build` · `update` | build the box image · [rebuild it fresh](#keeping-the-box-current) (latest agents/adapters) |
| `coop doctor` | [prove isolation](#prove-it-coop-doctor) — attack the box and check it holds |
| `coop check-secrets` | scan the [visible working tree](#secrets-never-enter-the-box) for committed secrets by content (exit 1 on a hit) |
| `coop help` · `version` | print help · print the version |

## The sandbox

The repo is bind-mounted into the box at the **same path it has on your machine**, so
the agent edits real files and you see them live in your editor. Everything else —
your home dir, SSH keys, the rest of the disk — simply isn't in the container.

### Secrets never enter the box

`.env`, `*.tfvars`, `*.pem`, `secrets/`, `.ssh`, and friends are **shadowed**: an empty
`tmpfs` over secret directories, a read-only empty file over secret files. Templates
(`*.example`, `*.sample`, `*.template`) stay visible so the agent can still see the
shape of your config. The default patterns are compiled in (see
`internal/box/secrets.go`).

That default list is a **denylist of well-known names** — it can't know your repo holds
a `config/credentials.yaml` or a committed `prod.yml` full of secrets. Hide those by
adding a **`.coopignore`** at the repo root, one pattern per line (`#` comments and
blank lines ignored):

```gitignore
prod.yml                 # basename — matched at any depth
config/credentials.yaml  # a slash makes it a repo-relative path
data/*.csv               # path globs work (filepath.Match; no **)
vault/                   # a directory — its contents are hidden whole
```

Patterns extend the defaults; they never un-hide. Templates (`*.example` and friends)
always stay visible, even if a `.coopignore` line would match them. A `.coopignore` also
works in any **subdirectory** (like `.gitignore`): it's scoped to that subtree — basename
patterns match at any depth under it, path patterns are relative to it — so a monorepo's
sub-teams can keep folder-local rules next to their code. A denylist can never be
exhaustive, so prefer gitignoring real secrets (gitignored files never enter a
[fork](#forks-hand-off-work-like-a-pr) either) and use `.coopignore` for anything that
must live in the working tree. Prove your setup with [`coop doctor`](#prove-it-coop-doctor).

Shadowing hides secrets by **filename**, but a token can hide *inside* an ordinary file.
`coop check-secrets` scans the files the box can actually see (everything not shadowed)
for secret-looking **content** — provider token shapes and high-entropy values — and
reports each as `file:line`, exiting non-zero on a hit so it works as a pre-flight or CI
check. It deliberately ignores already-shadowed files (the box never sees them) — if it
flags something you keep on purpose, hide that file with a `.coopignore` entry. Forks land
through the same content scan ([`coop fork merge`](#forks-hand-off-work-like-a-pr)).

### Your git identity, not the box's

The box has no `~/.gitconfig` of its own, so coop mounts a curated one into every run:
your global **`user.name`/`user.email`** (commits are authored as you), your global
**gitignore** (`core.excludesfile`), and **`commit.gpgsign=false`** — the box holds no
signing key, so without that a global `gpgsign=true` would fail every commit. Your key
never enters the box; commits made inside it are unsigned. (Forks can still land
**signed** — see [Landing](#land-it-rebase-gate-sign).)

### Prove it: `coop doctor`

```bash
coop doctor
```

`doctor` plants a fake secret, launches the box, and checks **from inside** that the
secret is unreachable and unwritable — then checks on the host that a fork carries
neither the secret nor a pushable remote. Run it anytime, especially after changing
config.

## Forks: hand off work like a PR

A **fork** is a throwaway local clone of your repo, handed to an agent instead of your
working tree. It's an extra layer of isolation (the agent never touches your checkout)
and the unit of parallelism (run several at once). Because a fork's `origin` is a local
path, **the agent has nowhere to push** — and gitignored secrets were never committed,
so they don't come along. You stay the reviewer and the only one who lands anything.

The lifecycle mirrors a contractor's PR: **open → work → review → land**.

```bash
coop fork perf codex     # open: clone into ../<repo>-forks/perf, run codex there
coop fork perf           # re-enter: continues codex's last session by default
coop fork ls             # list your forks (branch, changes, state, last activity)
coop fork review perf    # review: a brief, then the diff
coop fork merge perf     # land: rebase onto your branch, then close the fork
coop fork rm perf        # or discard it (refuses unmerged/dirty work without --force)
```

`coop fork <name>` opens a new fork or re-enters an existing one (`coop clone` is a
back-compat alias). The agent defaults to `claude`; pass `codex` or `gemini` to pick
the model. A fork inherits your git identity, signing key, and global gitignore from
the parent — so the agent can commit *as you* and ignores the same noise you do.

### Re-entry resumes the session

A fork **remembers the agent it was created with**, so `coop fork perf` after
`coop fork perf codex` re-enters with *codex*, not a silent fallback to claude (pass an
agent to switch; `coop fork ls` shows each fork's agent). The agent's session history
persists too, so re-entry **continues its last conversation** instead of starting fresh
— scoped to that fork (claude `--continue`, gemini `--resume latest`, and codex by the
session whose recorded cwd is the fork). It falls back to a fresh session when none
exists. Force one with `--new`; `--fresh` recreates the whole fork.

### Review — in your terminal or your IDE

`coop fork review <name>` opens with a **brief** — the commits, the files changed, and
the agent's own `.agent/LOG.md` reasoning — then shows the diff in your pager. No setup
needed. To review in an IDE instead:

| | |
|---|---|
| `--stat` | brief only, skip the diff |
| `--open` | open the fork as a folder in your editor and review via its SCM panel |
| `--tool` | open each changed file in your GUI difftool |

<details>
<summary><b><code>--open</code></b> — register your editor</summary>

coop opens the fork directory with the **first** of these that's set:

1. **`COOP_EDITOR`** — a coop-only override
2. **`git config core.editor`** — your normal git editor (local config beats global)
3. an auto-detected GUI editor on `PATH`: `cursor`, `code`, `zed`, `idea`, `subl`
4. `$VISUAL` / `$EDITOR`

Detection (step 3) is just a best-effort fallback — with both `code` and `zed`
installed it picks `code`. To choose, set one of the first two:

```bash
git config --global core.editor "zed --wait"   # your standard git editor (git commit uses it too)
export COOP_EDITOR="zed"                        # coop-only; overrides core.editor
```

coop runs the editor command **verbatim**, so `core.editor = "zed --wait"` blocks the
terminal until you close the window — that's what `--wait` is for. If you'd rather the
command return immediately, set `COOP_EDITOR` without `--wait` (e.g. `COOP_EDITOR=zed`).
</details>

<details>
<summary><b><code>--tool</code></b> — register your difftool</summary>

`--tool` runs `git difftool`, which opens each changed file in whatever
`git config diff.tool` points at. Register one once:

```bash
# VS Code
git config --global diff.tool vscode
git config --global difftool.vscode.cmd 'code --wait --diff "$LOCAL" "$REMOTE"'

# Zed
git config --global diff.tool zed
git config --global difftool.zed.cmd 'zed --wait --diff "$LOCAL" "$REMOTE"'
```

JetBrains, Meld, Beyond Compare, vimdiff, etc. work the same way — see
`git difftool --tool-help` for the tools git already knows.
</details>

<details>
<summary><b><code>COOP_REVIEW_CMD</code></b> — wire any tool</summary>

For full control, set `COOP_REVIEW_CMD`. coop runs it via `sh -c` from the parent repo
with `$COOP_FORK_PATH`, `$COOP_FORK_NAME`, and `$COOP_REVIEW_REF` in the environment, so
you can launch a TUI, a script, anything:

```bash
export COOP_REVIEW_CMD='cd "$COOP_FORK_PATH" && lazygit'
```
</details>

### Land it: rebase, gate, sign

`coop fork merge <name>` **rebases** the fork onto your current branch and
fast-forwards — linear history, no merge commits. The rebase happens on the host, where
your key lives, so if you sign commits (`commit.gpgsign=true`) the landed commits are
**signed with your key** even though the box committed them unsigned. Then `git push` —
the only step the agents can't do.

Set **`COOP_GATE`** (e.g. `COOP_GATE="make check"`) and every merge re-runs that gate in
the box on the *rebased* tree, rolling back if it goes red — the machine gate behind
your human review. A **policy check** also flags secret-looking or oversized files and
**scans each changed file's content** for real credentials (provider token shapes —
AWS/OpenAI/Anthropic/GitHub/Slack/… — plus high-entropy values on secret-named keys), so
a token committed inside an ordinary file is caught even though its filename is innocuous
(override with `--force`). Merge refuses if your own tree is dirty.

Merging lands work and then offers to delete the fork, so it asks first. In a
**non-interactive** shell (CI, a pipe) there's no one to answer, and merge refuses
rather than landing on the default — pass **`--yes`** (`-y`) to confirm landing and
removal up front. `--yes` also skips the prompts interactively.

### Drive a fork from Zed (ACP)

`coop fork <name> acp [agent]` fronts a fork as an [ACP](#drive-it-from-zed-acp) agent
over stdio; `coop acp [agent]` does the same for the project in your current directory.

To get coop agents into Zed's **agent panel**, register them **once in your Zed user
settings** — `agent_servers` is a *user-level* setting, so Zed rejects it in a project's
`.zed/settings.json`. Because `coop acp` resolves the repo from its working directory,
**one set of entries covers every coop project you open, forks included**: open a fork
with `coop fork open <name>` and the same agents resolve to it.

```jsonc
// ~/.config/zed/settings.json
{
  "agent_servers": {
    "coop · claude": { "command": "coop", "args": ["acp", "claude"] },
    "coop · codex":  { "command": "coop", "args": ["acp", "codex"] },
    "coop · gemini": { "command": "coop", "args": ["acp", "gemini"] }
  }
}
```

Zed's secure-by-default **Worktree Trust** still applies: click the Restricted-Mode
shield once to trust a project before its agents launch. Resuming a prior conversation
rides on ACP's (still-stabilizing) `session/load`, which the editor drives.

## Agents & config

One box, three agents. Each reads its config and credentials from
`~/.config/coop/agents/<name>/`, mounted into the box at `~/.claude`, `~/.codex`, and
`~/.gemini`. That directory lives outside any repo, so credentials never land in git —
edit those files on the host and they take effect in the box.

### Authentication

```bash
coop login claude     # interactive login; the token persists in agents/claude/
coop login codex      # device-code login (the box has no browser for an OAuth redirect)
coop login gemini     # or it logs in on first use
```

…or use API keys — drop them in the env file, passed into every box:

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
Claude's skills directory. A real (non-symlink) instruction file you already have is
left untouched. A shared `~/.config/coop/agents/INSTRUCTIONS.md` is also wired into each
agent's global instruction path.

### MCP servers, defined once

Drop your MCP servers into `~/.config/coop/agents/mcp.json` (the standard
`{ "mcpServers": { ... } }` shape) and **all three** agents pick them up:

```bash
cp agents/mcp.json.example ~/.config/coop/agents/mcp.json   # then edit
```

`coop` wires that one file into each agent's native mechanism on launch: Claude via
`--mcp-config`, Gemini merged into its `settings.json`, Codex converted to
`[mcp_servers.*]` in its `config.toml`. The Gemini/Codex versions are generated
read-only on top of your existing config (pure Go, no extra tooling) — your own files
are never touched, and servers from `mcp.json` win on a name clash.

## Fusion: a governed council

One model **leads** (the *governor*) and does the real work; the other two **advise
read-only**; the leader **synthesizes** the best of all three. A council that argues
before it commits beats any of its members working alone — the synthesized answer
outperforms even the single strongest model on its own, **Fable 5 included**. You stop
betting the run on one model's blind spots. It's a mode like any other agent —
interactive, headless, or in Zed:

```bash
coop fusion                    # the default governor leads (COOP_FUSION_GOVERNOR); the others advise
coop fusion claude             # claude leads instead
coop fusion claude -- -p "Design the retry strategy"   # headless; args after -- pass to the leader
```

**No extra service or protocol** behind it: the leader is just that agent running
normally (it edits, runs the gate, streams), plus a fusion instruction injected **into
the leader's instruction file only**. For a non-trivial question it consults its peers
from its shell — read-only and in parallel:

```bash
claude -p --permission-mode plan "<question>"   # read-only: returns its approach, never edits
gemini --approval-mode plan -p   "<question>"
codex  exec -s read-only         "<question>"
```

It then merges the strongest parts, resolves disagreements by verification, and
proceeds. Because the instruction lands only on the leader, the peers it spawns read
their normal instructions and never recurse into a council of their own. Each
consultation is two extra read-only runs, so it's for decisions and hard problems, not
every keystroke — the leader is told to skip the council for trivial steps.

**In Zed:** add one entry per leader and pick who governs from the agent dropdown:

```json
"agent_servers": {
  "coop fusion (codex)":  { "command": "coop", "args": ["acp", "fusion", "codex"] },
  "coop fusion (claude)": { "command": "coop", "args": ["acp", "fusion", "claude"] },
  "coop fusion (gemini)": { "command": "coop", "args": ["acp", "fusion", "gemini"] }
}
```

### Second opinions (`--consult`)

Outside fusion, add `--consult` to a normal run — `coop claude --consult` (or
`codex`/`gemini`; in Zed, `coop acp claude --consult`) — for a lighter version of the
same idea: on a genuinely hard or risky call the agent **may** consult its peers
read-only and in parallel to catch blind spots, then decide. It's **off by default**
and, unlike fusion, optional — no synthesis mandate, not for routine work. It only names
peers that are **authenticated**: if no other agent is logged in, nothing is injected.
And it's scoped to the agent you launched, so peers it spawns never recurse.

## Drive it from Zed (ACP)

The box can act as an [ACP](https://agentclientprotocol.com) agent, so you steer the
sandboxed agent from Zed's own agent panel — **Zed is the cockpit, the box stays the
cage**. Four steps:

**1. Install** (once). The [install one-liner](#install) puts `coop` on your `PATH` and
builds the image with the ACP adapters baked in. Check it resolves:

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

> GUI apps don't always inherit your shell's `PATH`. If Zed can't find `coop`, use the
> absolute path from step 1 as `command`.

**4. Use it.** Open the agent panel, pick **coop** from the dropdown, and start a
thread. Zed launches `coop acp <agent>` with the project as cwd; the agent runs in the
box, edits your files over ACP, and you approve its tool calls in Zed (or let them run —
the box is the boundary).

Under the hood `coop acp [claude|codex|gemini|fusion]` runs the matching adapter
(`@agentclientprotocol/claude-agent-acp`, `@zed-industries/codex-acp`, `gemini --acp`)
inside the box over stdio. The repo mounts at its **real host path** — the same path
`coop` and `coop loop` use — so Zed's absolute paths resolve *and* the session history
lines up: a thread you started with `coop loop` is there to resume in Zed.

> **Services** work too — if the repo has a `compose.agent.yml`, run `coop up` first and
> the ACP box joins the same network.
> **Custom images** must carry the ACP adapters: `coop init` scaffolds them in; for an
> older/hand-written `Dockerfile.agent`, add `@agentclientprotocol/claude-agent-acp` and
> `@zed-industries/codex-acp` to its `npm install -g` line (else `coop acp` fails with
> `codex-acp: not found`).

## Run it unattended

### The loop

```bash
coop init             # scaffold AGENTS.md, the .agent/ working folder, and the hooks
# ...fill in .agent/TASKS.md with checkbox tasks...
coop loop             # disposable agents work the queue until it's done, then audit
coop loop codex       # …or pick the model: claude (default), codex, or gemini
```

`loop` starts a **fresh agent per iteration** (no context rot), works unchecked `[ ]`
items, and won't quit while any remain. Pass `claude`/`codex`/`gemini` to choose the
model (default `claude`); `COOP_LOOP_CMD` still overrides the whole iteration command if
you need something custom. When the queue empties, a fresh auditor
re-checks every `[x]` against the git log and reopens anything that doesn't hold up.
`init` also installs a `Stop` hook (won't let a session end with work outstanding) and a
fast Claude commit-gate hook. Because those are Claude-only, `init` *also* installs a
tracked git **pre-commit** gate (`.githooks/pre-commit`) and points `core.hooksPath` at
it, so the format check runs for *every* committer — Codex, Gemini, and a plain
`git commit` — and rides along on a fresh clone. A custom `core.hooksPath` is left
untouched; skip the gate once with `git commit --no-verify`.

If the model hits a **rate or usage limit** mid-run, the loop doesn't treat it as a
failure: it reads the reset time from the agent's own output, waits it out with a
countdown, and resumes the same item once the limit clears — so an overnight run rides
through the daily cap instead of burning retries against it.

`init` also installs generic **workflow skills** into `.claude/skills/` (shared with
Codex): `/spec` a multi-file change, `/work` it step-by-step against the gate, `/sweep`
to drain `.agent/TASKS.md`, `/investigate` to root-cause a failure, and `/verify-api`
before calling anything you're unsure of. Edit them freely — `init` won't overwrite a
skill you've changed.

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

Run several models at once, each looping unattended in its own [fork](#forks-hand-off-work-like-a-pr).
Split the work into separate tasks files and hand each fork one with `--tasks`:

```bash
coop fork perf codex  --loop -d --tasks .agent/TASKS.perf.md   # codex loops the perf file, detached
coop fork deps gemini --loop -d --tasks .agent/TASKS.deps.md   # gemini takes the deps file
coop fork docs claude --loop -d --tasks .agent/TASKS.docs.md   # claude takes the docs

coop status            # fleet at a glance: progress (done/total), blockers, the task each is on
coop fork ls           # who's running, how big the diff, last activity
coop fork logs -f      # tail every fork at once (compose-style, prefixed)
coop fork stop perf    # halt one; coop fork logs perf -f to watch just it
```

`--loop` needs `--tasks <path>` — the file is **explicit**, never inferred from the
fork name, so a fork and its tasks file can be named independently. It seeds the fork's
queue from that file (once — a resumed loop keeps its own progress) and runs the loop
with the chosen model; `-d` (`--detach`) backgrounds it, capturing output to
`../<repo>-forks/.coop/<name>.log`. When one finishes,
[review and land it](#forks-hand-off-work-like-a-pr) like a PR, then `git push`. Add
agents until *review*, not generation, is your bottleneck.

**Land the whole fleet at once** with `coop fork merge --all` — a revalidating rebase
*queue*: it rebases each fork onto the result of the last and re-runs `COOP_GATE`, so a
"green" fork can't ride in against a base an earlier landing already changed. It stops at
the first conflict or red gate, leaving the rest untouched.

**Declare the fleet once** in `.agent/fleet`, one fork per line as
`<name> [agent] <tasks-path>` (agent defaults to `claude`; the path is relative to the
repo root):

```
perf  codex  .agent/TASKS.perf.md
deps  gemini .agent/TASKS.deps.md
docs         .agent/TASKS.docs.md
```

Then `coop fleet up` starts them all detached, `coop fork ls` shows the board, and
`coop fleet down` stops them. To bootstrap that file, `coop fleet split <n>` mechanically
round-robins your `.agent/TASKS.md` into `.agent/TASKS.slice<n>.md` files **and writes a
matching `.agent/fleet`** with each slice's explicit path (use an agent for *semantic*
slicing). It won't clobber a fleet you've already written — it prints the lines to
reconcile instead.

## Project toolchain & services

Real projects need a language toolchain (Elixir, Go, …) and stateful services
(Postgres, Redis). The agent does **not** install those at runtime — that's slow,
non-reproducible, and dies with the container. Declare them once instead. This is the
Dev Containers + Compose model, minus the ceremony.

### `.tool-versions` — zero config

If your repo pins versions in a `.tool-versions` (asdf), the base box provisions that
toolchain **at runtime** — resolved from the working dir up the tree, or
`~/.tool-versions` — and caches it in a shared volume. So a repo with *just* a
`.tool-versions` (no `Dockerfile.agent`, no scaffolding) gets its toolchain with zero
setup:

```bash
cd ~/code/phoenix-app   # has a .tool-versions
coop claude             # provisions elixir/erlang/node/… from it, then runs the agent
```

The first install of a new toolchain can be slow (e.g. Erlang compiles), then it's
reused across runs and repos. Set `COOP_NO_ASDF=1` (in `agents/env`) to skip it. For a
**baked, fully-reproducible** image instead, `coop init --stack asdf` scaffolds an asdf
`Dockerfile.agent` that installs the same `.tool-versions` at build time.

### `Dockerfile.agent` — a per-project image

```bash
coop init --stack asdf   # writes an asdf Dockerfile.agent (from .tool-versions) + compose.agent.yml
coop build               # builds it, tagged coop-<repo-name> — its own image
```

A repo with its own `Dockerfile.agent` gets its **own** image tag, so projects never
collide, and every `coop`, `coop loop`, `coop fork`, `coop acp` in that repo uses it.
The scaffolded one is the **asdf** image — it bakes in the exact `.tool-versions`
toolchain (versions live there, not in the Dockerfile). For anything more exotic,
hand-write a `Dockerfile.agent` (see the box contract below). When the agent needs a new
system package, add it to the `RUN` line and `coop build` again — the dependency
*graduates into the image* instead of being installed each run. If you change
`Dockerfile.agent` or `.tool-versions` but forget to rebuild, `coop` notices on the next
run and reminds you to `coop build` (it records the image's inputs at build time).

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

If a repo already has a `.devcontainer/`, reuse its image as your base and add the agent
layer on top:

```dockerfile
FROM your-devcontainer-image          # the team's source of truth for the env
RUN npm install -g @anthropic-ai/claude-code @openai/codex @google/gemini-cli \
      @agentclientprotocol/claude-agent-acp @zed-industries/codex-acp \
 && git config --system --add safe.directory '*'
USER <the devcontainer's non-root user>
# If that user's home isn't /home/node, run with COOP_HOME_IN_BOX=/home/<user>.
```

Division of labour: **devcontainer = what's in the environment** (toolchain, features,
reproducibility); **Coop = running an untrusted agent in it safely** (secret shadowing,
the container boundary, the queue + foreman). Don't lean on the devcontainer as the
security boundary — by itself it mounts your whole workspace (`.env` included), and a
malicious project under `--dangerously-skip-permissions` can exfiltrate `~/.claude`. The
shadowing and the box are what Coop adds on top.
</details>

### Services

```bash
coop up        # starts Postgres + Redis from compose.agent.yml, waits until healthy
coop claude    # the box reaches them by name (db, redis)
coop down -v   # stop services and wipe their throwaway data
```

Services run as their own containers on a private network the box joins — connect with
e.g. `DATABASE_URL=postgres://postgres:postgres@db:5432/app_dev` (put it in `agents/env`).
The agent never installs or hosts a database, so it can't corrupt one, and `coop down -v`
resets to a clean slate. A shared `coop-cache` volume at `~/.cache` keeps disposable runs
from re-downloading the world.

### Keeping the box current

```bash
coop update    # rebuild the image fresh: latest agent CLIs + ACP adapters + base
```

**Stable vs fresh.** `coop build` is the *stable* path: it pins the base image to a
specific Node digest, so a rebuild gets the same OS/runtime every time, and the cache
holds the agent CLIs steady between builds. `coop update` is the *fresh* path: it floats
the base back to the `node:24` tag and rebuilds with `--pull --no-cache`, so the base and
the agent CLIs + ACP adapters all jump to latest (they ship features often). To move the
pinned base permanently, bump `pinnedNodeImage` in `internal/box/image.go`.

For a **fully reproducible** image, also pin the tool versions: set
`COOP_AGENT_PACKAGES` to exact specs and `coop build`, e.g.
`COOP_AGENT_PACKAGES="@anthropic-ai/claude-code@1.2.3 @openai/codex@1.0.0 …"` (the full
list is in `internal/agent/*.go`).

## Configuration

Set via environment variables, or `~/.config/coop/coop.conf` (`KEY=VALUE` lines, same
names — the environment wins over the file). Toggles default **on**; set `0`/`false` to
turn them off.

**Box & runtime**

| Var | Default | |
|---|---|---|
| `COOP_RUNTIME` | auto | `container` / `docker` / `podman` |
| `COOP_IMAGE` | (auto) | force a specific image (overrides `Dockerfile.agent` detection) |
| `COOP_BASE_IMAGE` | `coop-box` | the shared base image tag |
| `COOP_AGENT_PACKAGES` | (latest) | pin the global agent + ACP npm specs for a reproducible `coop build` |
| `COOP_REPO` | (git toplevel) | the repo to operate on, overriding cwd detection |
| `COOP_WORKDIR` | (real path) | where the repo mounts in the box |
| `COOP_HOME_IN_BOX` | `/home/node` | where auth + instructions mount in the box |
| `COOP_RUN_ARGS` | — | extra args passed straight to the container runtime |
| `COOP_PIDS` | `4096` | box pids-limit (fork-bomb cap); `0`/`unlimited`/empty turns it off |
| `COOP_MEMORY` · `COOP_CPUS` | — | box memory / CPU caps (e.g. `4g`, `2`); unset by default |
| `COOP_NO_NEW_PRIVILEGES` | `1` | `--security-opt no-new-privileges` on the box |
| `COOP_NO_ASDF` | (off) | skip runtime `.tool-versions` provisioning |
| `COOP_NETWORK` · `COOP_CACHE` | `1` | join the services network · mount the cache volume |
| `COOP_SERVICES_NET` | (auto) | services network to join (let a fleet share one db) |

The resource/privilege caps (`COOP_PIDS` / `COOP_MEMORY` / `COOP_CPUS` /
`COOP_NO_NEW_PRIVILEGES`) apply on **docker and podman**; Apple's `container` CLI differs,
so they're skipped there for now.

**Agents & config**

| Var | Default | |
|---|---|---|
| `COOP_CONFIG_DIR` | `~/.config/coop/agents` | per-agent auth + settings folder |
| `COOP_<AGENT>_CMD` (e.g. `COOP_CLAUDE_CMD`) | autonomous default | override an agent's base command |
| `COOP_FUSION_GOVERNOR` | `codex` | default leader for `coop fusion` |
| `COOP_MCP_FILE` | `<config>/mcp.json` | the one MCP source of truth |
| `COOP_SHELL` | `bash` | the shell `coop shell` opens |

**Forks & loop**

| Var | Default | |
|---|---|---|
| `COOP_GATE` | — | gate re-run in the box before a fork merge lands (e.g. `make check`) |
| `COOP_EDITOR` | (detected) | editor for `coop fork review --open` |
| `COOP_REVIEW_CMD` | — | full override for `coop fork review` (`sh -c`) |
| `COOP_LOOP_CMD` | — | override the loop's per-iteration command |

Command-valued settings — `COOP_GATE`, `COOP_LOOP_CMD`, `COOP_RUN_ARGS`, and the
`COOP_<AGENT>_CMD` overrides — are split into `argv` with **shell quoting** (single/double
quotes group, `\` escapes), but **no shell runs** them (no globbing or `$VAR`). So quotes
group as you'd expect — `COOP_GATE='bash -lc "make check && make lint"'` is three args, not
five — but a bare `&&`/`|`/`$VAR` is a literal argument: wrap those in `bash -lc "…"`.
(`COOP_REVIEW_CMD` is the exception — it *is* run via `sh -c`.)

## Troubleshooting

| Symptom | Fix |
|---|---|
| **"no container runtime found"** | Install Apple [`container`](https://github.com/apple/container) (macOS 26+), Docker, or Podman, then `coop build && coop doctor`. Force one with `COOP_RUNTIME=docker`. |
| **"image … isn't built — run 'coop build'"** | `coop build` (shared base), or `coop build` in a repo with a `Dockerfile.agent` (its own image). `coop doctor` builds it too. |
| **Login hangs or "usage limit reached"** | `coop login <agent>` re-runs the sign-in (paste-code, no browser). Hit a subscription limit? It resets on a schedule — wait, or `coop login` into another account. The unattended loop waits out the reset on its own. |
| **Agent seems stuck / a detached loop won't quit** | `coop fork logs <name> -f` to watch it; `coop fork stop <name>` to stop a detached loop. A foreground run is just Ctrl-C. |
| **"permission denied" writing `~/.cache` / build or test caches** | The shared cache volume initialized root-owned. Recreate it: `docker volume rm coop-cache` (or your runtime's equivalent), then `coop build`. |
| **`go`/`gofmt`: "No version is set for command go"** | The box provisions toolchains from `.tool-versions` via asdf — add the toolchain there (e.g. `golang 1.26.4`) so it's installed and shimmed. Set `COOP_NO_ASDF=1` to skip provisioning. |
| **Zed (ACP) can't find the agent** | Zed must launch `coop` from a shell where it's on `PATH` (the installer puts it in `~/.local/bin`). Point Zed's ACP command at the absolute path if needed, and confirm `coop acp <agent>` runs in a terminal first. |
| **A merge refuses** | Dirty tree → commit/stash first. Policy flagged a secret/large file → review, then `--force`. Non-interactive shell → pass `--yes`. Gate (`COOP_GATE`) went red on the rebased tree → it rolled back; fix and re-run. |
| **Secrets still visible / a custom secret isn't hidden** | Run `coop doctor` to see what's shadowed. Add repo-specific paths to a `.coopignore` (see [Secrets never enter the box](#secrets-never-enter-the-box)). |
| **"box image is stale … run 'coop build'"** | You changed `Dockerfile.agent` or `.tool-versions` since the image was built. `coop build` to rebuild; the warning clears once the image matches. |

## Layout & development

A single static Go binary plus a config folder. A repo you work on optionally carries a
`Dockerfile.agent` (its toolchain) and `compose.agent.yml` (its services):

```
main.go             entrypoint
internal/agent/     one file per coding agent (claude/codex/gemini): commands, resume, MCP, defaults, packages
internal/box/       the engine: secret-shadowing mounts, git env, image selection, container run
internal/fusion/    the council: peer commands + the governor instruction
internal/mcp/       one mcp.json → Claude / Gemini / Codex native configs (pure Go, no Python)
internal/scaffold/  `coop init` templates + the workflow skills (embedded in the binary)
internal/cli/       command dispatch, grouped help, the fork lifecycle, doctor
internal/config·runtime·ui/   settings · runtime detection · terminal output
agents/             example config (env.example, mcp.json.example); copied to ~/.config/coop on install
install.sh          the curl one-liner: download the prebuilt binary onto PATH
```

```bash
make build     # build ./coop
make check     # gofmt + vet + staticcheck + unit tests (what CI runs; no Docker needed)
make doctor    # the integration check — proves isolation, needs a runtime
```

`.tool-versions` pins the Go toolchain (`golang 1.26.4`), so an asdf user — and coop's
own box — gets the right `go`/`gofmt` automatically.

The security-critical logic — secret enumeration (`internal/box/mounts.go`) and run-arg
assembly (`internal/box/run.go`) — is pure and unit-tested without a runtime; `coop
doctor` proves the whole thing end-to-end against the real box.
</content>
