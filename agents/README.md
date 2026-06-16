# agents/ — auth & settings, one folder per agent

Your live config lives at **`~/.config/coop/agents/`** (override with
`COOP_CONFIG_DIR`). The copy in this repo holds the examples (`env.example`,
`mcp.json.example`, …); `install.sh` seeds the config dir from it on first run.
Edit the files under `~/.config/coop/agents/` — they're mounted into the
sandbox so each CLI finds its config and credentials there, and only there.
Nothing leaks to the rest of your machine, and you can edit any of it without
entering the box.

| You edit here | Mounted in the box at | Holds |
|---|---|---|
| `claude/` | `~/.claude` | Claude Code `settings.json`, login |
| `codex/`  | `~/.codex`  | `config.toml`, `auth.json` |
| `gemini/` | `~/.gemini` | Gemini `settings.json`, login |
| `env`     | (env vars)  | API keys, passed to every launch |
| `INSTRUCTIONS.md` | all three native paths | one shared instruction file |
| `mcp.json` | all three native MCP configs | MCP servers, defined once |

The per-agent folders are created automatically the first time you run `coop`.

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

## MCP servers (one file, every agent)

Define your MCP servers once in `mcp.json` and all three agents pick them up:

```bash
cp mcp.json.example mcp.json   # then edit
```

It's the standard `{ "mcpServers": { ... } }` shape:

```json
{
  "mcpServers": {
    "context7": { "command": "npx", "args": ["-y", "@upstash/context7-mcp"] },
    "github": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-github"],
      "env": { "GITHUB_PERSONAL_ACCESS_TOKEN": "ghp_replace_with_your_token" }
    },
    "sentry": {
      "type": "http",
      "url": "https://mcp.sentry.dev/mcp",
      "bearer_token_env_var": "SENTRY_TOKEN"
    }
  }
}
```

`coop` wires it into each agent's native mechanism on every launch:

- **Claude** — passed directly with `--mcp-config` (needs no extra tooling).
- **Gemini** — merged into `~/.gemini/settings.json`.
- **Codex** — converted to `[mcp_servers.*]` in `~/.codex/config.toml`.

The Gemini and Codex versions are generated **read-only**, merged on top of
whatever you already keep in `gemini/settings.json` and `codex/config.toml` — your
files are never modified, and servers from `mcp.json` win on a name clash.
Generating those two is built into `coop` — pure Go, no extra runtime needed.

### Environment variables

**Set them on a stdio server** with an `env` block of `KEY: value` pairs (see
`github` above). coop passes them through to that server under all three agents —
verbatim, so the values are literal strings (no `$VAR` substitution).

**Reference a secret instead of pasting it** — for a Codex HTTP server, name the
variable that holds its token with `"bearer_token_env_var": "SENTRY_TOKEN"` (see
`sentry`) and put the value in the `env` file:

```bash
echo 'SENTRY_TOKEN=…' >> env     # the env file is loaded into every box
```

coop loads that file into the box on every launch, so Codex reads the token from the
environment at run time and it never appears in `mcp.json`. `mcp.json` is gitignored
regardless, so any token you do inline stays on your machine.

## Authenticate (two ways)

**API keys** — the simplest:

```bash
cp env.example env      # then fill in the keys you use
```

**Interactive login** — writes a token into the matching folder, which persists:

```bash
coop login codex       # or: coop login claude / coop login gemini
```

## Override settings

Drop the tool's normal user-level config into its folder and it takes effect in
the box:

- `claude/settings.json` — Claude Code settings
- `codex/config.toml` — Codex config (model, approval defaults, ...)
- `gemini/settings.json` — Gemini settings

Everything here except the templates is gitignored. Don't commit credentials.
