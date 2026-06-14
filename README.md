# Coop

<img src=".github/assets/coop.png" alt="Coop" width="200">

[![CI](https://github.com/AndrewDryga/coop/actions/workflows/ci.yml/badge.svg)](https://github.com/AndrewDryga/coop/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/AndrewDryga/coop?sort=semver)](https://github.com/AndrewDryga/coop/releases/latest)
[![Go Report Card](https://goreportcard.com/badge/github.com/AndrewDryga/coop)](https://goreportcard.com/report/github.com/AndrewDryga/coop)
[![License](https://img.shields.io/github/license/AndrewDryga/coop)](LICENSE)

Run a coding agent on your real repos every day, in a box it can't escape and
with your secrets out of its reach. One `coop` command, installed once.

It's the working tooling behind two write-ups that explain the *why* and the *how*:
[Running an AI coding agent you can't trust](https://dryga.com/blog/untrusted-ai-coding-agent/)
covers the sandbox; [One brain, two agents](https://dryga.com/blog/os-for-coding-agents/)
covers the queue, the hooks, and the foreman that runs it unattended.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/AndrewDryga/coop/main/install.sh | sh
```

Downloads the prebuilt `coop` binary for your OS/arch into `~/.local/bin` — no Go,
no clone. With a container runtime present the installer also builds the sandbox
image and runs `coop doctor`; otherwise do it once yourself:

```bash
coop build && coop doctor
```

`coop` is a single static binary with no runtime dependencies. It needs a
container runtime — Apple [`container`](https://github.com/apple/container)
(macOS 26+), Docker, or Podman — auto-detected. Re-run the one-liner to upgrade.

<details><summary><b>Other ways to install</b></summary>

```bash
go install github.com/AndrewDryga/coop@latest                              # with Go
git clone https://github.com/AndrewDryga/coop && cd coop && make install   # from source
```
</details>

## Daily use

```bash
cd ~/code/some-repo

coop                  # sandboxed `claude`, no permission prompts, secrets shadowed
coop codex            # the same box, but Codex (autonomous flags)
coop gemini           # ...or Gemini
coop fusion           # a council: one model leads, the other two advise, leader synthesizes
coop shell            # a shell in the box, to look around
coop run -- npm test  # run anything in the box
```

One box, three agents — each launches with its own "don't stop to ask" flags
(`--dangerously-skip-permissions`, `--dangerously-bypass-approvals-and-sandbox`,
`--yolo`), all inside the same sandbox. Or run all three at once with
[`coop fusion`](#fusion-a-governed-council), which beats any of them solo.

## Authentication & settings

Each agent reads its config and credentials from
`~/.config/coop/agents/<name>/`, mounted into the box at `~/.claude`,
`~/.codex`, `~/.gemini`. Edit those files on the host; they take effect in the
box. Two ways to authenticate:

```bash
coop login codex                                # device-code login (no browser); token persists
echo 'ANTHROPIC_API_KEY=sk-…' >> ~/.config/coop/agents/env   # ...or use API keys
```

The config dir lives outside any repo, so credentials never land in git. See
`agents/README.md` for the layout. On first run the box pre-answers each agent's
setup prompts — Claude's theme/folder-trust/bypass warnings, Codex's "trust this
directory?", and Gemini's folder-trust — so a fresh install goes straight from
login to work (the box is the sandbox). And `coop login codex` uses the
device-code flow, since the box has no browser for the usual OAuth redirect.

### MCP servers, defined once

Drop your MCP servers into `~/.config/coop/agents/mcp.json` (the standard
`{ "mcpServers": { ... } }` shape) and all three agents pick them up:

```bash
cp agents/mcp.json.example ~/.config/coop/agents/mcp.json   # then edit
```

`coop` wires that one file into each agent's native mechanism on launch: Claude
via `--mcp-config`, Gemini merged into its `settings.json`, Codex converted to
`[mcp_servers.*]` in its `config.toml`. The Gemini/Codex versions are generated
read-only on top of your existing config, so your own files are never touched and
servers from `mcp.json` win on a name clash.

Your repo is mounted into the box at the **same path it has on your machine**;
the agent edits real files and you see them in your editor. (Mounting at the real
path, rather than a fixed `/workspace`, keeps the agent's working directory — and
so its session history — identical across `coop`, `coop loop`, and `coop acp`, so
a thread you started looping shows up when you open the repo in Zed.) Everything
else — your home dir, SSH keys, the rest of the disk — isn't there. The worst an
off-the-rails agent can do is trash one repo you can restore from git.

### Secrets never enter the box

`.env`, `*.tfvars`, `*.pem`, `secrets/`, `.ssh`, and friends are shadowed: an
empty `tmpfs` over secret directories, a read-only empty file over secret files.
Templates (`*.example`, `*.sample`, `*.template`) stay visible. Verify it
yourself anytime:

```bash
coop doctor
```

It plants a fake secret, launches the box, and checks from *inside* that the
secret is unreachable and unwritable — then checks on the host that a clone
carries neither the secret nor a pushable remote. Run it after changing config.

### Hand off a clone, not your working tree

```bash
coop clone perf       # fresh clone in ../<repo>-agents/perf, agent runs there
```

The clone has no gitignored secrets (they were never committed) and its `origin`
is a local path — the agent has nowhere to push. When it's done:

```bash
git fetch ../<repo>-agents/perf perf:review/perf && git diff @...review/perf
```

### Leave it running

```bash
coop init             # scaffold AGENTS.md, the .agent/ working folder, and the hooks
# ...fill in .agent/TASKS.md with checkbox tasks...
coop loop             # disposable agents work the queue until it's done, then audit
```

`loop` starts a fresh agent per iteration (no context rot), works unchecked
`[ ]` items, and won't quit while any remain. When the queue empties, a fresh
auditor re-checks every `[x]` against the git log and reopens anything that
doesn't hold up. If the model hits a **rate or usage limit** mid-run, the loop
doesn't treat it as a failure: it reads the reset time from the agent's own
output, waits it out (with a countdown), and resumes the same item once the limit
clears — so an overnight run rides through the daily cap instead of burning
retries against it. `init` also installs a `Stop` hook (won't let a session end
with work outstanding) and a fast commit-gate hook.

It also wires the tool-neutral setup so every agent reads one source of truth:
`CLAUDE.md` and `GEMINI.md` symlink to the canonical `AGENTS.md`, and Codex
shares Claude's skills directory. A real (non-symlink) instruction file you
already have is left untouched.

And it installs a set of generic **workflow skills** into `.claude/skills/`
(shared with Codex): `/spec` a multi-file change, `/work` it step-by-step against
the gate, `/sweep` to drain `.agent/TASKS.md` unattended, and `/verify-api`
before calling anything you're unsure of. Edit them freely — `init` won't
overwrite a skill you've changed.

### The `.agent/` working folder

`init` creates a tool-neutral working folder the agent reads back on every boot
(and after each compaction). Everything here is local working state and
git-ignored — **except `rules/`**, the shared knowledge base, which is committed.

| File | What it's for |
|---|---|
| `TASKS.md` | the work queue — four states (`[ ]` todo · `[w]` claimed · `[x]` done+gated+committed · `[B]` blocked); the loop reads only this |
| `BACKLOG.md` | work discovered *outside* the current task — captured so it's not lost, but not auto-worked; a human promotes items into `TASKS.md` |
| `LOG.md` | the agent's chain-of-thought (what + why), so intent survives a compaction |
| `PENDING_DECISIONS.md` | anything needing a human call — the decision, the options, the agent's recommendation; the task goes `[B]` |
| `IDEAS.md` | product ideas, never auto-implemented — a human moves one into `TASKS.md` first |
| `rules/` | the taste knowledge base — corrections graduate into rules here (the one committed part) |

The generated `AGENTS.md` documents these too, so the canonical manual that's
re-injected after a compaction tells the agent how to use them.

### A fleet

Split the queue into per-agent slices and run several at once — each gets its
own clone, branch, and loop:

```bash
# .agent/TASKS.perf.md, .agent/TASKS.deps.md — independent items
coop dispatch perf > perf.log 2>&1 &
coop dispatch deps > deps.log 2>&1 &
```

Each clone is isolated (no shared working copy, no shared remote). Review and
merge the branches like contractor PRs. Add agents until *review*, not
generation, is your bottleneck.

### Drive it from Zed (ACP)

The box can act as an [ACP](https://agentclientprotocol.com) agent, so you steer
the sandboxed agent from Zed's own agent panel — Zed is the cockpit, the box
stays the cage. Connect it in four steps:

**1. Install** (once). The [install one-liner](#install) puts `coop` on your
`PATH`; `coop build` builds the image with the ACP adapters baked in. Check it
resolves:

```bash
command -v coop      # e.g. /Users/you/.local/bin/coop
```

**2. Authenticate** the agent you'll use — once; the token persists in
`agents/<agent>/`:

```bash
coop login claude    # or: coop login codex   (gemini logs in on first use)
```

**3. Register it in Zed.** In the agent panel, use **Add Custom Agent** (it
writes the entry for you), or edit `settings.json` directly:

```jsonc
{
  "agent_servers": {
    "coop": {
      "type": "custom",
      "command": "coop",          // absolute path if Zed's PATH lacks ~/.local/bin
      "args": ["acp", "claude"],   // or "codex" / "gemini"
      "env": {}
    }
  }
}
```

GUI apps don't always inherit your shell's `PATH`. If Zed can't find `coop`,
use the absolute path from step 1 as `command`.

**4. Use it.** Open Zed's agent panel, pick **coop** from the agent
dropdown, and start a thread. For each project window Zed launches
`coop acp claude` with that project as the cwd; the agent runs in the box, edits
your files over ACP, and you approve its tool calls in Zed (or let them run — the
box is the boundary).

Under the hood, `coop acp [claude|codex|gemini]` runs the matching adapter
(`@agentclientprotocol/claude-agent-acp`, `@zed-industries/codex-acp`, `gemini --acp`)
inside the box over stdio: the repo is mounted at its **real host path** — the
same path `coop` and `coop loop` use — so Zed's absolute paths resolve *and* the
session history lines up (a thread you started with `coop loop` is there to resume
in Zed); stdin is attached without a pty (ACP is JSON-RPC over stdio), and your
secrets stay shadowed.

Notes:
- **Services work too.** If the repo has a `compose.agent.yml`, `coop up` first
  — the ACP box joins the same network, so the agent reaches `db`/`redis` by name.
- **Stack images need the adapters.** A repo with its own `Dockerfile.agent`
  runs in *that* image, which doesn't ship the ACP adapters by default — add
  `@agentclientprotocol/claude-agent-acp` (and `@zed-industries/codex-acp`) to its
  `npm install -g` line if you want ACP there.

## Fusion: a governed council

One model **leads** (the *governor*) and does the real work; the other two
**advise read-only**; the leader **synthesizes** the best of all three. A council
that argues before it commits beats any of its members working alone — the
synthesized answer outperforms even the single strongest model on its own,
**Fable 5 included**. You stop betting the run on one model's blind spots. It's a
mode like any other agent — interactive, headless, or in Zed:

```bash
coop fusion                    # codex leads (COOP_FUSION_GOVERNOR); claude + gemini advise
coop fusion --governor claude  # claude leads instead; codex + gemini advise
coop fusion -- -p "Design the retry strategy"   # headless; args pass to the leader
```

There's no extra service or protocol behind it: the leader is just that agent
running normally (so it edits, runs the gate, and streams), plus a fusion
instruction injected **into the leader's instruction file only**. For a
non-trivial question it consults its peers from its shell — read-only and in
parallel:

```
claude -p --permission-mode plan "<question>"   # read-only: returns its approach, never edits
gemini -p --approval-mode plan   "<question>"
codex  exec -s read-only          "<question>"
```

It then merges the strongest parts, resolves disagreements by verification, and
proceeds. Because the instruction lands only on the leader, the peers it spawns
read their normal instructions and never recurse into a council of their own.

**In Zed:** add one `agent_servers` entry per leader and pick the one you want
from the agent dropdown to switch who governs:

```json
"agent_servers": {
  "coop fusion (codex)":  { "command": "coop", "args": ["acp", "fusion", "codex"] },
  "coop fusion (claude)": { "command": "coop", "args": ["acp", "fusion", "claude"] },
  "coop fusion (gemini)": { "command": "coop", "args": ["acp", "fusion", "gemini"] }
}
```

The leader runs as a normal ACP agent, so Zed drives it like any other — it just
consults its peers along the way.

Each consultation is two extra read-only runs, so fusion is for decisions and
hard problems, not every keystroke — the leader is told to skip the council for
trivial steps.

## Project dependencies: toolchain in the image, services alongside

Real projects need a language toolchain (Elixir, Python, …) and stateful
services (Postgres, Redis). The agent does **not** install those at runtime —
that's slow, non-reproducible, and dies with the container. You declare them
once instead:

```bash
coop init --stack elixir   # writes Dockerfile.agent (toolchain) + compose.agent.yml (db, redis)
coop build                 # bakes Elixir + the agent CLIs into this repo's own image
coop up                    # starts Postgres + Redis, waits until healthy
coop                       # the box has Elixir AND reaches db/redis by name
coop down -v               # stop services and wipe their throwaway data
```

- **Toolchain → the image.** A repo with its own `Dockerfile.agent` gets its own
  image tag, so projects never collide. Stacks: `elixir`, `python`, `go`, `node`
  (or edit the generated file). When the agent needs a new system package, it
  rebuilds the image — the dependency *graduates into the Dockerfile* instead of
  being re-installed every run.
- **Services → sibling containers.** Postgres and Redis run as their own
  containers on a private network the box joins; connect with e.g.
  `DATABASE_URL=postgres://postgres:postgres@db:5432/app_dev` (put it in
  `agents/env`). The agent never installs or hosts a database, so it can't
  corrupt one — and `coop down -v` resets to a clean slate.
- **Caches stay warm.** A shared `coop-cache` volume is mounted at `~/.cache`
  so disposable runs don't re-download the world.

This is the Dev Containers + Compose model, minus the ceremony.

### How `Dockerfile.agent` works

Drop a `Dockerfile.agent` at a repo's root (usually via `coop init --stack`).
From then on, in that repo:

- `coop build` builds it and tags it `coop-<repo-name>` — its **own** image,
  so a project's toolchain never clobbers the shared `coop-box` base.
- every `coop`, `coop codex`, `coop loop`, `coop clone` uses that image
  automatically (detected by the file's presence).
- need a new system package? add it to the `RUN` line and `coop build` again —
  the dependency *graduates into the image* instead of being installed each run.

The box has a small contract. An image is a valid agent box when:

1. **It runs as a non-root user** — Claude Code refuses `--dangerously-skip-permissions` as root.
2. **That user's home is `/home/node`** — the `agents/` auth mounts and
   `INSTRUCTIONS.md` land at `$HOME/.claude`, `$HOME/.codex`, `$HOME/.gemini`.
   (Different base? Set `COOP_HOME_IN_BOX=/home/<user>` to match.)
3. **`claude`, `codex`, `gemini` are on `PATH`** (so it needs Node).
4. **`git config --system --add safe.directory /workspace`** — git works on the
   host-owned bind mount.
5. **`WORKDIR /workspace`.**

Distilled to a skeleton you can build any stack on:

```dockerfile
FROM <your-language-base>
RUN <install your toolchain> \
 && npm install -g @anthropic-ai/claude-code @openai/codex @google/gemini-cli \
 && git config --system --add safe.directory /workspace \
 && id -u node >/dev/null 2>&1 || useradd -m -u 1000 -s /bin/bash node
USER node
WORKDIR /workspace
```

(If the base lacks Node, install it first — the `--stack` templates use NodeSource.)

### Reusing an existing devcontainer

If a repo already has a `.devcontainer/`, don't duplicate the toolchain — reuse
its image as your base and add the agent layer on top:

```dockerfile
FROM your-devcontainer-image          # the team's source of truth for the env
RUN npm install -g @anthropic-ai/claude-code @openai/codex @google/gemini-cli \
 && git config --system --add safe.directory /workspace
USER <the devcontainer's non-root user>
WORKDIR /workspace
# If that user's home isn't /home/node, run with COOP_HOME_IN_BOX=/home/<user>.
```

The division of labour: **devcontainer = what's in the environment**
(toolchain, features, reproducibility); **Coop = running an untrusted agent
in it safely** (secret shadowing, the VM boundary, the queue + foreman). Don't
lean on the devcontainer as the security boundary — by itself it mounts your
whole workspace (`.env` included), and upstream docs warn that under
`--dangerously-skip-permissions` a malicious project can exfiltrate `~/.claude`.
The shadowing and VM are what the box adds on top.

## Configure

Env vars, or `~/.config/coop/coop.conf` (`KEY=VALUE` lines, same names —
environment wins over the file):

| Var | Default | |
|---|---|---|
| `COOP_IMAGE` | (auto) | force a specific image (overrides `Dockerfile.agent` detection) |
| `COOP_BASE_IMAGE` | `coop-box` | the shared base image tag |
| `COOP_RUNTIME` | auto | `container` / `docker` / `podman` |
| `COOP_CONFIG_DIR` | `~/.config/coop/agents` | per-agent auth + settings folder |
| `COOP_HOME_IN_BOX` | `/home/node` | where auth + instructions mount in the box |
| `COOP_CLAUDE_CMD` · `COOP_CODEX_CMD` · `COOP_GEMINI_CMD` | autonomous defaults | per-agent command |
| `COOP_NETWORK` · `COOP_CACHE` | `1` | join services network / mount cache volume |
| `COOP_SERVICES_NET` | (auto) | services network to join (let a fleet share one db) |

The shadow / keep-visible patterns are compiled in (see `internal/box/secrets.go`).

## Layout

A single static Go binary plus a config folder. A repo you work on optionally
carries a `Dockerfile.agent` (its toolchain) and `compose.agent.yml` (its
services):

```
main.go             entrypoint
internal/box/       the engine: secret-shadowing mounts, image selection, container run
internal/mcp/       one mcp.json → Claude / Gemini / Codex native configs (no Python)
internal/scaffold/  `coop init` templates + the workflow skills (embedded in the binary)
internal/cli/       command dispatch, grouped help, doctor
internal/config·runtime·ui/   settings · runtime detection · terminal output
agents/             example config (env.example, mcp.json.example); copied to
                    ~/.config/coop/agents on install
skills/             the workflow skills (spec · work · sweep · verify-api), also embedded
install.sh          the curl one-liner: download the prebuilt binary onto PATH
Makefile            build · install · test · lint · doctor · check
```

## Development

```bash
make build     # build ./coop
make check     # gofmt + vet + staticcheck + unit tests (what CI runs; no Docker needed)
make doctor    # the integration check — proves isolation, needs a runtime
```

The security-critical logic — secret enumeration (`internal/box/mounts.go`) and
run-arg assembly (`internal/box/run.go`) — is pure and unit-tested without a
runtime; `coop doctor` proves the whole thing end-to-end against the real box.
