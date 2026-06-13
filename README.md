# agent-box

Run a coding agent on your real repos every day, in a box it can't escape and
with your secrets out of its reach. One `agent` command, installed once.

It's the working tooling behind two write-ups:
[running an agent you can't trust](https://dryga.com/blog/untrusted-ai-coding-agent/)
and [an OS for autonomous agents](https://dryga.com/blog/os-for-coding-agents/).

## Install

```bash
./install.sh
```

Symlinks `agent` into `~/.local/bin`, builds the `agent-box` image (Node +
Claude Code + Codex + Gemini), and runs `agent doctor` to prove the sandbox
holds. After that, `agent` works from any repo. Needs a container runtime —
Apple [`container`](https://github.com/apple/container) (macOS 26+), Docker, or
Podman — auto-detected.

## Daily use

```bash
cd ~/code/some-repo

agent                  # sandboxed `claude`, no permission prompts, secrets shadowed
agent codex            # the same box, but Codex (autonomous flags)
agent gemini           # ...or Gemini
agent shell            # a shell in the box, to look around
agent run -- npm test  # run anything in the box
```

One box, three agents — each launches with its own "don't stop to ask" flags
(`--dangerously-skip-permissions`, `--dangerously-bypass-approvals-and-sandbox`,
`--yolo`), all inside the same sandbox.

## Authentication & settings

Each agent reads its config and credentials from `agents/<name>/`, mounted into
the box at `~/.claude`, `~/.codex`, `~/.gemini`. Edit those files on the host;
they take effect in the box. Two ways to authenticate:

```bash
cp agents/env.example agents/env   # 1. drop in API keys, or...
agent login codex                  # 2. interactive login; token persists in agents/codex/
```

See `agents/README.md`. Everything there except the templates is gitignored, so
credentials never land in git.

Your repo is mounted at `/workspace`; the agent edits real files and you see
them in your editor. Everything else — your home dir, SSH keys, the rest of the
disk — isn't there. The worst an off-the-rails agent can do is trash one repo
you can restore from git.

### Secrets never enter the box

`.env`, `*.tfvars`, `*.pem`, `secrets/`, `.ssh`, and friends are shadowed: an
empty `tmpfs` over secret directories, a read-only empty file over secret files.
Templates (`*.example`, `*.sample`, `*.template`) stay visible. Verify it
yourself anytime:

```bash
agent doctor
```

It plants a fake secret, launches the box, and checks from *inside* that the
secret is unreachable and unwritable — then checks on the host that a clone
carries neither the secret nor a pushable remote. Run it after changing config.

### Hand off a clone, not your working tree

```bash
agent clone perf       # fresh clone in ../<repo>-agents/perf, agent runs there
```

The clone has no gitignored secrets (they were never committed) and its `origin`
is a local path — the agent has nowhere to push. When it's done:

```bash
git fetch ../<repo>-agents/perf perf:review/perf && git diff @...review/perf
```

### Leave it running

```bash
agent init             # scaffold AGENTS.md, the .agent/ working folder, and the hooks
# ...fill in .agent/TASKS.md with checkbox tasks...
agent loop             # disposable agents work the queue until it's done, then audit
```

`loop` starts a fresh agent per iteration (no context rot), works unchecked
`[ ]` items, and won't quit while any remain. When the queue empties, a fresh
auditor re-checks every `[x]` against the git log and reopens anything that
doesn't hold up. `init` also installs a `Stop` hook (won't let a session end
with work outstanding) and a fast commit-gate hook.

It also wires the tool-neutral setup so every agent reads one source of truth:
`CLAUDE.md` and `GEMINI.md` symlink to the canonical `AGENTS.md`, and Codex
shares Claude's skills directory. A real (non-symlink) instruction file you
already have is left untouched.

And it installs a set of generic **workflow skills** into `.claude/skills/`
(shared with Codex): `/plan` a multi-file change, `/work` it step-by-step against
the gate, `/batch` to drain `.agent/TASKS.md` unattended, and `/verify-api`
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
agent dispatch perf > perf.log 2>&1 &
agent dispatch deps > deps.log 2>&1 &
```

Each clone is isolated (no shared working copy, no shared remote). Review and
merge the branches like contractor PRs. Add agents until *review*, not
generation, is your bottleneck.

### Drive it from Zed (ACP)

The box can act as an [ACP](https://agentclientprotocol.com) agent, so you steer
the sandboxed agent from Zed's own agent panel — Zed is the cockpit, the box
stays the cage. Connect it in four steps:

**1. Install and build** (once). `./install.sh` puts `agent` on your `PATH` and
builds the image with the ACP adapters baked in. Check it resolves:

```bash
command -v agent      # e.g. /Users/you/.local/bin/agent
```

**2. Authenticate** the agent you'll use — once; the token persists in
`agents/<agent>/`:

```bash
agent login claude    # or: agent login codex   (gemini logs in on first use)
```

**3. Register it in Zed.** In the agent panel, use **Add Custom Agent** (it
writes the entry for you), or edit `settings.json` directly:

```jsonc
{
  "agent_servers": {
    "agent-box": {
      "type": "custom",
      "command": "agent",          // absolute path if Zed's PATH lacks ~/.local/bin
      "args": ["acp", "claude"],   // or "codex" / "gemini"
      "env": {}
    }
  }
}
```

GUI apps don't always inherit your shell's `PATH`. If Zed can't find `agent`,
use the absolute path from step 1 as `command`.

**4. Use it.** Open Zed's agent panel, pick **agent-box** from the agent
dropdown, and start a thread. For each project window Zed launches
`agent acp claude` with that project as the cwd; the agent runs in the box, edits
your files over ACP, and you approve its tool calls in Zed (or let them run — the
box is the boundary).

Under the hood, `agent acp [claude|codex|gemini]` runs the matching adapter
(`@zed-industries/claude-code-acp`, `@zed-industries/codex-acp`, `gemini --acp`)
inside the box over stdio: the repo is mounted at its **real host path** (not
`/workspace`) so Zed's absolute paths resolve, stdin is attached without a pty
(ACP is JSON-RPC over stdio), and your secrets stay shadowed.

Notes:
- **Services work too.** If the repo has a `compose.agent.yml`, `agent up` first
  — the ACP box joins the same network, so the agent reaches `db`/`redis` by name.
- **Stack images need the adapters.** A repo with its own `Dockerfile.agent`
  runs in *that* image, which doesn't ship the ACP adapters by default — add
  `@zed-industries/claude-code-acp` (and `@zed-industries/codex-acp`) to its
  `npm install -g` line if you want ACP there.

## Project dependencies: toolchain in the image, services alongside

Real projects need a language toolchain (Elixir, Python, …) and stateful
services (Postgres, Redis). The agent does **not** install those at runtime —
that's slow, non-reproducible, and dies with the container. You declare them
once instead:

```bash
agent init --stack elixir   # writes Dockerfile.agent (toolchain) + compose.agent.yml (db, redis)
agent build                 # bakes Elixir + the agent CLIs into this repo's own image
agent up                    # starts Postgres + Redis, waits until healthy
agent                       # the box has Elixir AND reaches db/redis by name
agent down -v               # stop services and wipe their throwaway data
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
  corrupt one — and `agent down -v` resets to a clean slate.
- **Caches stay warm.** A shared `agentbox-cache` volume is mounted at `~/.cache`
  so disposable runs don't re-download the world.

This is the Dev Containers + Compose model, minus the ceremony.

### How `Dockerfile.agent` works

Drop a `Dockerfile.agent` at a repo's root (usually via `agent init --stack`).
From then on, in that repo:

- `agent build` builds it and tags it `agentbox-<repo-name>` — its **own** image,
  so a project's toolchain never clobbers the shared `agent-box` base.
- every `agent`, `agent codex`, `agent loop`, `agent clone` uses that image
  automatically (detected by the file's presence).
- need a new system package? add it to the `RUN` line and `agent build` again —
  the dependency *graduates into the image* instead of being installed each run.

The box has a small contract. An image is a valid agent box when:

1. **It runs as a non-root user** — Claude Code refuses `--dangerously-skip-permissions` as root.
2. **That user's home is `/home/node`** — the `agents/` auth mounts and
   `INSTRUCTIONS.md` land at `$HOME/.claude`, `$HOME/.codex`, `$HOME/.gemini`.
   (Different base? Set `AGENT_HOME_IN_BOX=/home/<user>` to match.)
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
# If that user's home isn't /home/node, run with AGENT_HOME_IN_BOX=/home/<user>.
```

The division of labour: **devcontainer = what's in the environment**
(toolchain, features, reproducibility); **agent-box = running an untrusted agent
in it safely** (secret shadowing, the VM boundary, the queue + foreman). Don't
lean on the devcontainer as the security boundary — by itself it mounts your
whole workspace (`.env` included), and upstream docs warn that under
`--dangerously-skip-permissions` a malicious project can exfiltrate `~/.claude`.
The shadowing and VM are what the box adds on top.

## Configure

Env vars, or `~/.config/agent-box/agent.conf` (sourced — set the same names):

| Var | Default | |
|---|---|---|
| `AGENT_IMAGE` | (auto) | force a specific image (overrides `Dockerfile.agent` detection) |
| `AGENT_BASE_IMAGE` | `agent-box` | the shared base image tag |
| `AGENT_RUNTIME` | auto | `container` / `docker` / `podman` |
| `AGENT_CONFIG_DIR` | `agents/` | per-agent auth + settings folder |
| `AGENT_HOME_IN_BOX` | `/home/node` | where auth + instructions mount in the box |
| `AGENT_CLAUDE_CMD` · `AGENT_CODEX_CMD` · `AGENT_GEMINI_CMD` | autonomous defaults | per-agent command |
| `SECRET_GLOBS` · `ALLOW_GLOBS` | see `bin/agent` | shadow / keep-visible patterns |
| `AGENT_NETWORK` · `AGENT_CACHE` | `1` | join services network / mount cache volume |
| `AGENT_SERVICES_NET` | (auto) | services network to join (let a fleet share one db) |

## Layout

Everything is one self-contained script plus a config folder. A repo you work on
optionally carries a `Dockerfile.agent` (its toolchain) and `compose.agent.yml`
(its services):

```
bin/agent      the CLI: claude · codex · gemini · login · acp · run · shell · clone
               · dispatch · up · down · loop · init · doctor · build
agents/        per-agent auth + settings (claude/ codex/ gemini/ env), gitignored
skills/        generic workflow skills (plan · work · batch · verify-api) init installs
Dockerfile     reference base image (agent build has a built-in copy too)
install.sh     symlink onto PATH, build, verify
Makefile       install · test · doctor · lint · check
test/unit.sh   unit tests for the pure logic (no runtime needed)
```

## Development

```bash
make check     # shellcheck + unit tests (what CI runs; no Docker needed)
make doctor    # the integration check — proves isolation, needs a runtime
```

The script is source-guarded, so `test/unit.sh` loads its functions and tests
the pure logic (secret enumeration, image selection, naming) directly.
