# agents/ — auth & settings, one folder per agent

This folder is mounted into the sandbox so each CLI finds its config and
credentials here — and only here. Nothing leaks to the rest of your machine,
and you can read or edit any of it without entering the box.

| You edit here | Mounted in the box at | Holds |
|---|---|---|
| `claude/` | `~/.claude` | Claude Code `settings.json`, login |
| `codex/`  | `~/.codex`  | `config.toml`, `auth.json` |
| `gemini/` | `~/.gemini` | Gemini `settings.json`, login |
| `env`     | (env vars)  | API keys, passed to every launch |
| `INSTRUCTIONS.md` | all three native paths | one shared instruction file |

The per-agent folders are created automatically the first time you run `agent`.

## Shared instructions (one file, every agent)

```bash
cp INSTRUCTIONS.md.example INSTRUCTIONS.md   # then edit
```

`INSTRUCTIONS.md` is wired into each agent's native global path — `~/.claude/
CLAUDE.md`, `~/.codex/AGENTS.md`, `~/.gemini/GEMINI.md` — so all three agents
follow the same machine-level guidance without you copying it three times.

A **per-agent override wins**: drop a `CLAUDE.md` in `claude/` (or `AGENTS.md`
in `codex/`, `GEMINI.md` in `gemini/`) and that agent uses it instead. Project-
specific rules still belong in the repo's own `CLAUDE.md`/`AGENTS.md`.

## Authenticate (two ways)

**API keys** — the simplest:

```bash
cp env.example env      # then fill in the keys you use
```

**Interactive login** — writes a token into the matching folder, which persists:

```bash
agent login codex       # or: agent login claude / agent login gemini
```

## Override settings

Drop the tool's normal user-level config into its folder and it takes effect in
the box:

- `claude/settings.json` — Claude Code settings
- `codex/config.toml` — Codex config (model, approval defaults, ...)
- `gemini/settings.json` — Gemini settings

Everything here except the templates is gitignored. Don't commit credentials.
