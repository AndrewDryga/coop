# Changelog

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
