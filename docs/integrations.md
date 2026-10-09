# Editors, MCP and controllers

This page is the reference for steering co:op from an editor over ACP, sharing MCP servers with
every agent, and running co:op as a worker for a controller. The guide's
[Editors](https://coop.dryga.com/docs.html#editors) and
[MCP and remote sessions](https://coop.dryga.com/docs.html#mcp-remote) sections give the short
version.

## Zed and other ACP editors

Steer the sandboxed agent from Zed, or any editor that speaks [ACP](https://agentclientprotocol.com),
with account rotation built in. The box acts as an ACP agent: you work in the editor's agent panel,
and the agent keeps running in the box. The steps below use Zed.

### Set up Zed

1. Install co:op once. The [install one-liner](../README.md#install) puts `coop` on your `PATH` and
   builds the image with the ACP adapters baked in. Check that it resolves:

```bash
command -v coop      # e.g. /Users/you/.local/bin/coop
```

2. Sign in to the agent you'll use. See [Authentication](agents.md#authentication).

```bash
coop login claude    # or codex / gemini / grok
```

3. Register co:op in Zed. In the agent panel, use Add Custom Agent, or edit `settings.json`
   directly:

```jsonc
{
  "agent_servers": {
    "coop": {
      "type": "custom",
      "command": "coop",                                    // absolute path if Zed's PATH lacks ~/.local/bin
      "args": ["acp"]                                       // automatic startup; choose in the live toolbar
    },
    "coop · second opinion": {
      "type": "custom",
      "command": "coop",
      "args": ["acp", "claude:opus/xhigh", "--peer", "codex"] // named peer; the lead's model/effort ride the target
    }
  }
}
```

4. Open the agent panel, pick `coop` from the dropdown and start a thread.

GUI apps don't always inherit your shell's `PATH`. If Zed can't find `coop`, use the absolute path
from step 1 as `command`.

### Pick the provider, model and account

Without a target, co:op starts the first signed-in provider in Claude, Codex, Gemini, Grok order,
using its default account. That choice isn't pinned: select a provider or preset in the toolbar. If
no provider is signed in, run `coop login <agent>` and reconnect the editor.

To pin the model, reasoning effort and account, put a target inside `args`, in the form
`provider[:model][/effort][@account]`. Use the target, not the editor's own per-option defaults. A
Claude session with a Codex peer is `["acp","claude:opus/xhigh","--peer","codex"]`. A solo run is
`["acp","claude:opus/xhigh@work"]`. The toolbar menus come up showing the target, and you can
still switch them mid-thread.

### Remote repositories

For a repository on another machine, prefer Zed Remote Development. Open the repository through
Zed's SSH remote workflow, and register the same `command: "coop"`, `args: ["acp", "<target>"]`
there. The editor, terminal, repository and ACP process then share one remote filesystem and its
exact absolute paths.

If the editor must stay local, its custom-agent command can be SSH itself:

```jsonc
{
  "agent_servers": {
    "coop · remote": {
      "type": "custom",
      "command": "ssh",
      "args": ["-T", "dev@coop-host", "cd /srv/projects/my-repo && exec coop acp claude"]
    }
  }
}
```

SSH supplies host identity, authentication, encryption and process transport. co:op adds no ACP
TCP listener. Use the command-over-SSH form only when Zed can resolve the same absolute repository
paths that the remote ACP process reports. When the two hosts use different paths, native Remote
Development is the reliable choice.

### What runs in the box

Zed launches `coop acp` (optionally with a target or preset) with the project as its working
directory. `coop acp [<target|preset>]` runs the selected provider's ACP adapter inside the box
over stdio, and the provider edits your files over ACP.

| Provider | Adapter in the box |
| --- | --- |
| Claude | `@agentclientprotocol/claude-agent-acp` |
| Codex | `@agentclientprotocol/codex-acp` |
| Gemini | `gemini --acp` |
| Grok | `grok agent stdio` |

In normal writable sessions, the repository mounts at its real host path, the same path `coop` and
`coop loop` use, so Zed's absolute paths resolve.

Tool calls never ask for permission. co:op runs every editor session in yolo mode, whatever the
provider's own settings say, and ignores any editor permission `mode` setting. The box is the
boundary, so permission prompts would only slow the agent down.

Editor threads keep their own complete native home in
`~/.config/coop/agents/<agent>/acp-homes/<repository-key>/home/`. Accounts for that provider share
this repository's home, so switching accounts mid-thread keeps the conversation without exposing
another project's threads. Remote sessions use the same account-independent layout in their private state.
Sessions from `coop <agent>` or `coop loop` aren't in it.

co:op's proxy sits between the editor and the box and owns the session.

### The toolbar

The toolbar has one shape for a plain session and another for an active preset. Neither has a
permission-mode menu.

A plain session shows the Preset, Provider and Account menus:

- Switching the account, or switching within the same provider, keeps the conversation. It goes
  through the shared session store, which doesn't depend on the credential.
- Switching the provider re-creates the session. The thread carries over approximately, on a
  best-effort basis: message text plus one-line tool narration, without tool payloads.
- A switch made mid-turn sends the in-flight prompt again, so the turn completes on the new target
  instead of failing with an error.
- The model menu defaults to the target's `:model` or your config, and you can still switch it.

An active preset shows only the Preset menu. Its ladder owns the provider, model, effort, account
and roles, and co:op acknowledges and ignores saved Provider and Account choices. Selecting None
brings Provider and Account back, with Provider set to the effective provider and Account set to
Auto. Select None first whenever you want to choose either one yourself.

### Usage limits

When a turn hits the provider's limit, co:op hides the error, rotates to your next signed-in
account, sends your prompt again and moves the dropdown. The turn completes on the backup
credential. If no account is free, co:op posts
`Waiting for account "<name>" on <Provider> to reset its usage limit at <Mon 15:04 MST> (in MM:SS). Your message will send automatically.`
and sends your prompt when the limit lifts.

### Restarts

A box can die from `coop build` or `coop update`, an out-of-memory kill or a Docker restart. The
editor stays connected: co:op respawns the box and replays the handshake, so even a thread you
haven't messaged yet survives. Supervision is always on.

### Dev servers

With `serve.ports` in [`.agent/project.yaml`](box.md#dev-servers-in-your-browser), the thread announces the stable
`http://localhost:<port>` URLs where the box's ports are published.

### Tracing a session

To trace a session that misbehaves, run `touch ~/.config/coop/acp-debug`, or set
`COOP_ACP_TRACE=1` in the agent server's env. Every `coop acp` server then appends the ACP traffic
between the editor and the box to `~/.config/coop/acp-trace-<pid>.log`.

The sentinel file works on a server that's already running, so you can turn tracing on after a
session has gone wrong. The trace carries prompts and file contents, so treat it as sensitive. See
[Troubleshooting](https://coop.dryga.com/docs.html#troubleshooting).

### Filtered networking

A session with [filtered networking](networking.md) offers only compatible providers and complete
presets.

| Provider | Filtered ACP works with |
| --- | --- |
| Claude | its supported provider-native credentials, or an API key |
| Codex | its supported provider-native credentials, or an API key |
| Gemini | an AI Studio API key or importable plain OAuth account; not Vertex |
| Grok | its OAuth login |

API keys and native OAuth grants stay outside the workload behind co:op's broker. A provider's
API-key and subscription accounts can both be offered; switching selects a fresh broker without
changing that repository's editor home. Vertex `GOOGLE_API_KEY` is refused in every network mode.

Switching providers never widens the session's network access. Explicitly asking for an
unsupported provider or preset still fails before launch.

### Forks

To steer a [fork](forks.md) from the editor instead of your working tree, point the adapter at it
with `coop fork <name> acp`. It's the same editor flow, but the agent works in the throwaway clone.
There's nothing to push, your secrets never came along, and you still review and land the work.

The command starts the agent the fork was created with. Name a target to start another, as in
`coop fork <name> acp claude:opus`.

For a writable local session, configure that command while the parent project is open in Zed.
co:op maps the editor's project directory to the fork inside the box. The fork, provider and
account stay fixed, and the plain ACP Provider and Preset menus don't appear. Any model and effort
choices the adapter offers stay available. co:op refuses an editor opened on an unrelated
directory instead of quietly sending its session to the fork. With `--readonly`, open the fork
itself in the editor.

### Services and custom images

Services work with ACP too. If the repo has a `.agent/compose.yml`, run `coop up` first, and the
ACP box joins the same network.

Custom images must carry the ACP adapters. An image built on co:op's box
(`FROM ${COOP_BASE_IMAGE}`, as `coop init` scaffolds) inherits them. An image on another base
installs them itself, as the box contract's skeleton in [box.md](box.md#your-own-image-agentdockerfile) shows. Otherwise
`coop acp` fails with `codex-acp: not found`.

### Files and terminals on your computer

ACP has a second channel. The agent can send `fs/read_text_file`, `fs/write_text_file` and
`terminal/*` requests to the editor, and the editor serves them on your computer, outside the box.
co:op's proxy forwards them to Zed unfiltered. Claude's adapter uses client-side `fs` in normal
operation.

So over ACP, the box's isolation is only as strong as your editor's own file and terminal sandbox.
A prompt-injected agent could ask the editor to read or write an absolute path on your computer,
outside the repo.

`coop claude` and `coop loop` have no such channel, so there the box is the only boundary that
matters. This applies only when you drive an agent from an editor over ACP.

## MCP servers

Define your MCP servers once, in one file, and every agent picks them up. `coop init` seeds an
empty `~/.config/coop/agents/mcp.json` in the standard `{ "mcpServers": { ... } }` shape. An empty
file wires up nothing. Add your servers to it:

```json
{
  "mcpServers": {
    "playwright": {
      "command": "npx",
      "args": ["-y", "@playwright/mcp@latest", "--headless", "--no-sandbox"]
    },
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

### Where the file lives

`COOP_MCP_FILE` names the shared file; `mcp.json` is the default. Keep it outside repositories,
companion repositories, agent credential homes and ACP session stores. co:op mounts those
directories wholesale, so it refuses a `COOP_MCP_FILE` inside one before launch, even when the
file is empty or doesn't exist yet. The file must also:

- be a regular file, not a symlink or special file
- be 4 MiB or smaller
- have a canonical path without `..`

Gemini's native `settings.json` follows the same file rules, and co:op projects it on every Gemini
launch. co:op reads Codex's native `config.toml` on every Codex launch, and Grok's native config
only when the shared file is active. An unsafe or unreadable input stops the affected launch instead
of being ignored.

### How each agent gets them

On launch, `coop` wires the one file into each agent's native mechanism:

| Agent | Where the servers go |
| --- | --- |
| Claude | `--mcp-config` |
| Gemini | merged into its `settings.json` |
| Codex, Grok | converted to their provider-specific `[mcp_servers.*]` tables in `config.toml` |

The generated versions are laid read-only on top of your existing config, in pure Go with no extra
tooling. Generation never edits the source files.

With the shared file active, co:op omits the native Codex and Grok `[mcp_servers.*]` tables, so the
shared file is the only server authority. Remove other native TOML spellings, such as dotted,
quoted or inline `mcp_servers`, or move them into the configured shared file. co:op refuses them
before launch, because removing them would mean rewriting the rest of your config.

A remote session is the exception. Its ACP adapter takes no flags, so `--mcp-config` never reaches
Claude there. co:op hands the same servers to the session directly, and resolves
`bearer_token_env_var` into an `Authorization` header on the way, because ACP carries headers, not
env-var names. co:op resolves and validates the file before changing private session state, then
hands the adapter only the bytes it captured. An unsafe or ambiguous active file stops the turn
before its child starts. There's nothing to configure: it's the same `mcp.json`.

### Tokens and headers

An `env` block on a command server (`github` above) reaches that server under every agent,
verbatim. Its values are literal strings, with no `$VAR` substitution.

To keep a token out of `mcp.json`, point `bearer_token_env_var` at a variable (`sentry` above),
and put the value in the env file:

```bash
echo 'SENTRY_TOKEN=…' >> ~/.config/coop/agents/env
```

An HTTP server's `headers` work for every agent. Claude and Grok receive `bearer_token_env_var` as
an environment-expanded Authorization header. Codex receives a literal header value as a static
header. It receives a value that is exactly one `${VARIABLE}` as the variable's name, so Codex
resolves a secret referenced that way and the secret never lands in the generated config. A value
that mixes text with a reference (`Bearer ${TOKEN}`) is the one shape Codex can't express. Use
`bearer_token_env_var` for that.

### SSE servers

Transport matters for one agent. This is what each agent does with a legacy SSE server
(`"type": "sse"`):

| Agent | What happens |
| --- | --- |
| Grok | Grok speaks legacy SSE, and co:op carries the declaration through. |
| Claude, Gemini | They receive the declaration and decide for themselves. |
| Codex | co:op refuses the server before launch and names it. Codex's client would accept the SSE declaration and then treat the server as streamable HTTP without saying so, and its ACP adapter refuses the whole session. |

The Codex refusal applies wherever Codex takes part, including as a `--peer` of another lead.

### Network modes

#### Filtered runs

Under `--egress filtered`, a header value may not reference the environment at all, for any agent.
The approved network policy is compiled from literal definitions, so co:op refuses `${…}` there
before launch.

A `bearer_token_env_var` token stays outside a filtered box. co:op's credential broker holds it and
sends it upstream. The box's copy of the file points the server at a loopback listener and names a
co:op-owned stand-in variable, `COOP_MCP_TOKEN_<n>`, that is valid only for that server and run.

Your own variable in co:op's env file (`SENTRY_TOKEN` above) never reaches a filtered box, whether
or not the box loads MCP. A session's ACP adapter is handed the stand-in, never the token.

A bearer server on legacy SSE gets a wider route: `GET` on its stream path and `POST` on any clean
path of the same host. Its token still stays outside the box. The launch names this compatibility
support. Prefer the server's streamable HTTP URL when there is one, since it uses an exact-path
route.

#### Open runs

An open run keeps its MCP secrets outside the box too. co:op starts one small helper container
beside the box. The helper is unprivileged, sits on the box's own network and is removed when the
run ends. It holds each secret-bearing server's credential and answers only to a per-run stand-in,
exactly as the filtered broker does. The box's copy of the file points the server at `coop-broker`
and names `COOP_MCP_TOKEN_<n>`.

The box reaches the helper over plain HTTP on its container network, which is the default bridge
when your project has no network of its own. That network doesn't keep the credential yours; the
stand-in does. The launch says which servers the helper covers.

A server that no single route can carry keeps working exactly as before, with its secret in the
box, and the launch says which server and why. That happens for:

- a server whose secret is in two places
- a URL that is not plain `https` on port 443
- every server, when the box has no network of its own to reach a helper on (Apple's `container`
  runtime, `--network none`, `--network container:…`)

Read-only sessions on open networking use the same helper. Their adapters receive stand-ins for
the servers it covers, with the same fallbacks for servers it can't broker.

#### Offline runs

An offline run (`--egress none`) leaves out every remote MCP server, since it couldn't answer
without internet, and says so at launch. Local (command) servers still work, and the remote
servers' tokens stay out of the box.

### Playwright

The example's Playwright server works in the box with no extra setup. Chromium's system libraries
are baked into the image, and the browser binary downloads to the cache volume on first use. The
server runs `--headless --no-sandbox`, because the box is already the sandbox.

## Controllers and workers

Connect co:op workers to a controller: your own platform, which hands out the jobs. Run one
outbound worker, on the controller's VM or a separate VM:

```bash
coop sessions connect --controller https://controller.example --token-file /run/secrets/coop-token
```

The controller supplies immutable jobs: exact repositories and context, model targets, network
settings and execution limits. co:op authenticates the controller, fetches complete Git/LFS and
authorized submodule working trees, runs models in isolation and keeps durable session state. You
need no local policy catalog or worker configuration file. The same generic protocol works with
any controller; Ryker is one consumer.

The private Unix API owns sessions, FIFO turns, inspection, checkpoints, review, publication,
cancellation, budgets and explicit cleanup. It never listens on TCP or enters a model sandbox. By
default a parked session has no box; jobs with a warm-idle timeout may keep a prepared runtime.
Companion repositories are exact pinned read-only snapshots. The trusted host can push an immutable
reviewed candidate and create a draft PR with a fresh repository-scoped grant. Models never receive
the GitHub credential.

`coop sessions doctor --json` checks the service that `connect` started.

If an old database has duplicate prompts in retry receipts, stop the worker and run:

```bash
coop sessions compact --backup /safe/path/session-before-compact.sqlite
```

The backup must be a new file outside the state root, and it stays sensitive. Canonical turns and
prompts are preserved. Follow the API's
[backup and recovery instructions](session-api.md#compacting-old-turn-receipts).

The [worker API reference](session-api.md) has the full detail. Its [Connect](session-api.md#connect)
section covers the worker command.
