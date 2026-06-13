# Changelog

## 1.5.0

- `agent acp [claude|codex|gemini]` — run the box as an ACP agent over stdio, so
  editors like Zed can drive the sandboxed agent. Mounts the repo at its real
  host path (so the editor's absolute paths resolve), attaches stdin without a
  pty, and keeps secrets shadowed. Point Zed's `agent_servers` at
  `command: "agent", args: ["acp", "claude"]`.
- The default image now bakes in the ACP adapters (`@zed-industries/claude-code-acp`,
  `@zed-industries/codex-acp`; Gemini's is built in) and trusts any git worktree
  (`safe.directory '*'`) so git works on the host-path mount.

## 1.4.0

- Ship generic workflow skills — `/plan`, `/work`, `/batch`, `/verify-api` — under
  `skills/`. `agent init` installs them into the repo's shared `.claude/skills/`
  (Codex gets them via the symlink), without clobbering ones you've edited, and
  the generated `AGENTS.md` points the agent at them. Adapted from the production
  set (emisar), with the repo-specific parts (Iron Laws, Elixir contexts) removed.

## 1.3.0

- `agent init` now scaffolds the full `.agent/` working set — `TASKS.md`,
  `BACKLOG.md`, `LOG.md`, `PENDING_DECISIONS.md`, `IDEAS.md`, `rules/` — and the
  generated `AGENTS.md` documents each one's role, so the canonical manual that's
  re-injected after a compaction tells the agent how to use them. Matches the
  layout used in production (emisar). Everything but `rules/` stays git-ignored.

## 1.2.0

- `agent dispatch <name>` — the fleet unit: clone into an isolated workspace,
  seed it with that agent's queue slice (`.agent/TASKS.<name>.md`), run the loop.
- `agent init` now wires the tool-neutral setup: `CLAUDE.md` and `GEMINI.md`
  symlink to the canonical `AGENTS.md`, and `.codex/skills` shares
  `.claude/skills`. Real (non-symlink) instruction files are never clobbered.
- Explicit `AGENT_IMAGE` now overrides image selection (lets a dispatched clone
  reuse its origin's image); `AGENT_SERVICES_NET` lets a fleet share one db/redis.
- Source-guarded entrypoint (`main`) so the script is unit-testable; added
  `test/unit.sh`, a `Makefile`, and CI (shellcheck + unit tests).

## 1.1.0

- Three agents in one box: `claude`, `codex`, `gemini`, each with autonomous
  defaults; `agent login <agent>`.
- Per-agent auth + settings in `agents/<agent>/`, mounted into the box; `agents/env`
  for API keys; one `agents/INSTRUCTIONS.md` wired into all three agents' native
  instruction paths, with per-agent overrides taking precedence.
- Per-project environments: `agent init --stack <elixir|python|go|node>` writes a
  `Dockerfile.agent` (toolchain) and `compose.agent.yml` (services); `agent up`/
  `down` run sibling Postgres/Redis the box reaches by name; per-project image tags.

## 1.0.0

- The box: `agent` runs a sandboxed agent on the current repo, with repo secrets
  shadowed out of reach (tmpfs over secret dirs, read-only decoys over secret files).
- `agent doctor` proves isolation by attacking it; `agent clone` hands off a
  secrets-free workspace with no reachable remote.
- The autonomous loop: `agent loop` works `.agent/TASKS.md` with disposable
  sessions and an audit pass; `agent init` scaffolds the queue + Stop/commit hooks.
