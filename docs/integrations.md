# Editors, MCP and controllers

Use this reference to connect an editor over ACP, share MCP servers across agents, or run co:op
workers for a controller. For a shorter introduction, see the guide's
[Editors](https://coop.dryga.com/docs.html#editors) and
[MCP and remote sessions](https://coop.dryga.com/docs.html#mcp-remote) sections.

## Zed and other ACP editors

You can use Zed or another [ACP](https://agentclientprotocol.com) editor to work with an agent
running in a co:op box. You send prompts from the editor's agent panel; co:op runs the agent in
the box and handles account rotation. The steps below use Zed.

### Set up Zed

1. Install co:op. The [install one-liner](../README.md#install) puts `coop` on your `PATH` and
   builds an image with the ACP adapters included. Check that your shell can find it:

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

If you do not specify a target, co:op checks Claude, Codex, Gemini and Grok in that order and
starts the first signed-in provider with its default account. You can change the provider or
choose a preset in the toolbar. If none is signed in, run `coop login <agent>` and reconnect
the editor.

To choose the starting model, reasoning effort and account, add a target to `args` in the form
`provider[:model][/effort][@account]`. Set these in the target rather than the editor's separate
defaults. For Claude with a Codex peer, use `["acp","claude:opus/xhigh","--peer","codex"]`.
For Claude alone on a named account, use `["acp","claude:opus/xhigh@work"]`. The toolbar shows
your target's choices, and you can still change them during the thread.

### Remote repositories

For a repository on another machine, use Zed Remote Development. Open the repository through
Zed's SSH remote workflow, then register `command: "coop"`, `args: ["acp", "<target>"]` there.
The editor, terminal and ACP process then use the same remote filesystem and absolute paths.

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

SSH verifies the host, authenticates the connection, encrypts traffic and carries the process's
input and output. co:op does not open an ACP TCP listener. Use this command-over-SSH setup only
if Zed can resolve the same absolute repository paths reported by the remote ACP process. If
the machines use different paths, use native Remote Development.

### What runs in the box

Zed starts `coop acp` in the project directory, optionally with a target or preset.
`coop acp [<target|preset>]` starts the selected provider's ACP adapter in the box. The adapter
communicates over standard input and output, and the provider edits your files through ACP.

| Provider | Adapter in the box |
| --- | --- |
| Claude | `@agentclientprotocol/claude-agent-acp` |
| Codex | `@agentclientprotocol/codex-acp` |
| Gemini | `gemini --acp` |
| Grok | `grok agent stdio` |

In a normal writable session, co:op mounts the repository at its real host path. This is the
same path used by `coop` and `coop loop`, so Zed's absolute file paths resolve correctly.

Tool calls do not ask for permission. co:op runs editor sessions in yolo mode, regardless of the
provider's settings, and ignores the editor's permission `mode` setting. For requests handled
inside the box, the box provides isolation. ACP can also ask the editor to access files and
terminals on your computer; see the file and terminal restrictions below.

Editor threads store their complete native home in
`~/.config/coop/agents/<agent>/acp-homes/<repository-key>/home/`. Accounts for the same provider
share that repository's home. You can switch accounts during a thread without losing the
conversation or exposing another project's threads.

Remote sessions use the same account-independent layout in their private state. Sessions from
`coop <agent>` and `coop loop` use separate storage.

co:op runs a proxy between the editor and the box to manage the session.

### The toolbar

The toolbar changes when you activate a preset. Neither version has a permission-mode menu.

A plain session shows the Preset, Provider and Account menus:

- Changing accounts or other choices within the same provider keeps the conversation. The
  provider's session storage is shared across accounts.
- Changing providers creates a new session. co:op carries over the message text and one-line
  descriptions of tool use as a best effort; tool payloads do not transfer.
- If you switch during a turn, co:op sends the current prompt again to the new target.
- The model menu starts with the target's `:model` or your configured default. You can change it.

With a preset active, the toolbar shows only the Preset menu. The preset's fallback list
chooses the provider, model, effort, account and roles. co:op accepts saved Provider and Account
choices from the editor but does not apply them.

Select None to choose a provider or account yourself. Both menus return: Provider shows the
provider currently in use, and Account is set to Auto.

### Usage limits

If a provider reports a usage limit during a turn, co:op suppresses the error, switches to your
next signed-in account and sends the prompt again. It updates the account dropdown to match.
If no account is available, co:op posts
`Waiting for account "<name>" on <Provider> to reset its usage limit at <Mon 15:04 MST> (in MM:SS). Your message will send automatically.`
It sends your prompt when the limit resets.

### Restarts

A box can stop during `coop build` or `coop update`, when it runs out of memory, or when Docker
restarts. co:op keeps the editor connected, starts another box and repeats the ACP handshake.
This also works for a thread that has not received a message yet. Supervision is always on.

### Dev servers

Set `serve.ports` in [`.agent/project.yaml`](box.md#dev-servers-in-your-browser) to publish the
box's ports. The thread shows the stable `http://localhost:<port>` URLs you can open.

### Tracing a session

To record a session's ACP traffic, run `touch ~/.config/coop/acp-debug` or set
`COOP_ACP_TRACE=1` in the agent server's environment. Each `coop acp` server then appends traffic
between the editor and box to `~/.config/coop/acp-trace-<pid>.log`.

Creating the file also turns tracing on for servers that are already running, so you can use
it after a problem appears. The trace contains prompts and file contents; treat it as sensitive.
See [Troubleshooting](https://coop.dryga.com/docs.html#troubleshooting).

### Filtered networking

With [filtered networking](networking.md), the editor offers only compatible providers and
presets that can run in full under those network rules.

| Provider | Filtered ACP works with |
| --- | --- |
| Claude | its supported provider-native credentials, or an API key |
| Codex | its supported provider-native credentials, or an API key |
| Gemini | an AI Studio API key or importable plain OAuth account; not Vertex |
| Grok | its OAuth login |

co:op's broker keeps API keys and native OAuth grants outside the box. You can choose either
an API-key account or a subscription account for a supported provider. Switching accounts starts
a new broker without changing the repository's editor home. Vertex `GOOGLE_API_KEY` is refused
in every network mode.

Switching providers never widens the session's network access. Explicitly asking for an
unsupported provider or preset still fails before launch.

### Forks

To work in a [fork](forks.md) from the editor, start the adapter with `coop fork <name> acp`.
The editor works the same way, but the agent edits the throwaway clone rather than your working
tree. You review and land the result locally without pushing the fork. Uncommitted, Git-ignored
secrets are not copied, but secrets already in Git history can remain.

The command starts the agent the fork was created with. Name a target to start another, as in
`coop fork <name> acp claude:opus`.

Configure this command with the parent project open in Zed. co:op maps that project directory
to the fork inside the box. The fork, provider and account stay fixed, so the usual ACP Provider
and Preset menus are hidden. You can still use any model and effort choices the adapter offers.
If the editor is open on an unrelated directory, co:op refuses the session.

### Services and custom images

Services work with ACP too. With open networking, run `coop up` for the repo's
`.agent/compose.yml`, and the ACP box joins the same network. With filtered networking, request
and approve access to each service in the project's network rules. co:op then starts those
services on a separate network for the editor session.

A custom image must include the ACP adapters. Building on co:op's box with
`FROM ${COOP_BASE_IMAGE}`, as `coop init` sets up, includes them automatically. If you use another
base image, install them as shown in [box.md](box.md#your-own-image-agentdockerfile).
Filtered runs must inherit co:op's client base and keep its client versions. Without the
required adapter, `coop acp` fails with an error such as `codex-acp: not found`.

### Files and terminals on your computer

ACP also lets the agent ask the editor to access your computer directly. Requests such as
`fs/read_text_file`, `fs/write_text_file` and `terminal/*` run through the editor, outside the
box. co:op forwards these requests to Zed without filtering them. Claude's adapter normally uses
this client-side `fs` access.

When you use ACP, your editor's file and terminal restrictions are part of the security
boundary. A prompt-injected agent could ask the editor to read or write an absolute path outside
the repo on your computer.

`coop claude` and `coop loop` do not have this editor channel. These additional file and
terminal risks apply when you control an agent from an editor over ACP.

## MCP servers

Define MCP servers in one shared file for all agents. `coop init` creates an empty
`~/.config/coop/agents/mcp.json` using the standard `{ "mcpServers": { ... } }` format. It connects
no servers until you add them:

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

Set `COOP_MCP_FILE` to choose the shared file; the default is `mcp.json`. Keep it outside
repositories, companion repositories, agent credential homes and ACP session stores. co:op
mounts those directories in full, so a file there could expose its contents to the box. co:op
refuses a `COOP_MCP_FILE` in any of them before launch, even if it is empty or does not exist yet.
The file must also:

- be a regular file, not a symlink or special file
- be 4 MiB or smaller
- have a canonical path without `..`

Gemini's native `settings.json` must follow the same file rules. co:op reads it and generates
the box's version on every Gemini launch. It reads Codex's native `config.toml` on every Codex
launch, and Grok's native config when the shared file is active. If an input is unsafe or cannot
be read, co:op stops that launch instead of ignoring the problem.

### How each agent gets them

At launch, `coop` passes the shared server definitions to each agent in its native format:

| Agent | Where the servers go |
| --- | --- |
| Claude | `--mcp-config` |
| Gemini | merged into its `settings.json` |
| Codex, Grok | converted to their provider-specific `[mcp_servers.*]` tables in `config.toml` |

co:op generates these configs in Go, with no extra tools. It mounts them read-only over the
existing configs inside the box and leaves your source files unchanged.

When the shared file is active, it is the only source of MCP server definitions. co:op leaves
out the native Codex and Grok `[mcp_servers.*]` tables.

If your native TOML uses dotted, quoted or inline `mcp_servers` entries, remove them or move
them into the shared file. co:op refuses those forms before launch because removing them would
require rewriting the rest of your config.

Remote sessions use the same `mcp.json` without extra setup. Their ACP adapters accept no flags,
so co:op passes server definitions directly to the session rather than using Claude's
`--mcp-config`. ACP accepts headers, not environment-variable names. co:op therefore reads
`bearer_token_env_var` and supplies an `Authorization` header.

Before changing private session state, co:op resolves and validates the file. The adapter gets
only the bytes captured during that check. If the active file is unsafe or ambiguous, the turn
stops before its child process starts.

### Tokens and headers

Every agent passes a command server's `env` block (`github` above) to that server unchanged.
The values are literal strings; `$VAR` is not expanded.

To keep a token out of `mcp.json`, point `bearer_token_env_var` at a variable (`sentry` above),
and put the value in the env file:

```bash
echo 'SENTRY_TOKEN=…' >> ~/.config/coop/agents/env
```

Every agent supports an HTTP server's `headers`. Claude and Grok turn `bearer_token_env_var`
into an Authorization header using the environment variable's value.

For Codex, a literal value becomes a static header. A value containing exactly one
`${VARIABLE}` becomes an environment-variable reference: Codex reads the secret itself, keeping
it out of the generated config. Codex cannot represent a mixed value such as `Bearer ${TOKEN}`.
Use `bearer_token_env_var` for that form.

### SSE servers

The agents handle a legacy SSE server (`"type": "sse"`) differently:

| Agent | What happens |
| --- | --- |
| Grok | Grok speaks legacy SSE, and co:op carries the declaration through. |
| Claude, Gemini | They receive the declaration and decide for themselves. |
| Codex | co:op stops before launch and names the unsupported server. Codex's client would silently treat the SSE declaration as streamable HTTP, while its ACP adapter rejects the whole session. |

The Codex refusal applies wherever Codex takes part, including as a `--peer` of another lead.

### Network modes

#### Filtered runs

With `--egress filtered`, header values must be literal for every agent. co:op builds the
approved network policy from those literal values, so it rejects environment references such
as `${…}` before launch.

In a filtered run, co:op's credential broker keeps the `bearer_token_env_var` token outside the
box and sends it to the server. The box gets a modified config that points to a loopback
listener and uses a replacement variable, `COOP_MCP_TOKEN_<n>`. That replacement works only for
this server during this run.

The original variable in co:op's env file (`SENTRY_TOKEN` above) never reaches a filtered box,
even if that box does not load MCP. The ACP adapter receives only the replacement value, not
the real token.

A legacy SSE server with a bearer token needs a broader allowed route: `GET` on its stream
path and `POST` on any clean path at the same host. The token still stays outside the box, and
co:op reports this compatibility mode at launch. Prefer a streamable HTTP URL when the server
provides one; that limits the route to an exact path.

#### Open runs

For supported MCP servers, an open run also keeps secrets outside the box. co:op starts a
small, unprivileged helper container on the box's network and removes it when the run ends.
The helper holds each server's credential and accepts only a replacement value valid for that
run, as the filtered broker does. The box's config points to `coop-broker` and uses
`COOP_MCP_TOKEN_<n>`.

The box talks to the helper over plain HTTP on its container network. If the project has no
network of its own, it uses the default bridge. The replacement value protects access to the
credential on that network. At launch, co:op lists the servers covered by the helper.

If the helper cannot handle a server through one route, the server still works but its secret
stays inside the box. co:op reports the server's name and reason at launch. This happens for:

- a server whose secret is in two places
- a URL that is not plain `https` on port 443
- every server, when the box has no network of its own to reach a helper on (Apple's `container`
  runtime, `--network none`, `--network container:…`)

Remote sessions with open networking use the same helper. Their adapters get replacement
values for supported servers. Servers the helper cannot handle use the same fallbacks above.

#### Offline runs

An offline run (`--egress none`) omits all remote MCP servers because it cannot reach them,
and reports this at launch. Local command servers still work. Tokens for the omitted remote
servers stay outside the box.

### Playwright

The image includes Chromium's system libraries. On first use, the example downloads the
Playwright package and browser, then keeps the browser in the cache volume. In a filtered run,
you must approve the download destinations and any sites the browser needs to visit. If a
download is blocked, review the reported destination before granting access.

The browser runs with `--headless --no-sandbox`, relying on the box for isolation.

## Controllers and workers

A controller is your platform that assigns jobs to co:op workers. Start an outbound worker on
the controller's VM or a separate VM:

```bash
coop sessions connect --controller https://controller.example --token-file /run/secrets/coop-token
```

The controller supplies jobs with fixed repositories, context, model targets, network settings
and execution limits. co:op authenticates the controller, fetches complete Git/LFS working trees
and authorized submodules, and runs the models in isolation. It saves session state so it
persists between runs.

You do not need a local policy catalog or worker configuration file. Any controller can use the
same protocol; Ryker is one example.

A private Unix API manages sessions, turns in first-in-first-out order, inspection, checkpoints,
review, publication, cancellation, budgets and explicit cleanup. It stays on the host: it
neither listens on TCP nor enters a model sandbox.

After each turn, co:op removes the box unless the job uses a warm-idle timeout to keep it ready.
If cleanup fails, co:op retries in the background. A parked session alone does not confirm that
its containers are gone. Companion repositories are read-only snapshots of exact pinned versions.

With a fresh grant scoped to the repository, the trusted host can push a fixed, reviewed
candidate and open a draft PR. The model never receives the GitHub credential.

`coop sessions doctor --json` checks the service that `connect` started. Before you upgrade a
worker, stop it and run `coop sessions check` with the new build; see
[Upgrading a worker](session-api.md#upgrading-a-worker).

If an old database has duplicate prompts in retry receipts, stop the worker and run:

```bash
coop sessions compact --backup /safe/path/session-before-compact.sqlite
```

Choose a new backup file outside the state root and treat it as sensitive. Compaction preserves
the original turns and prompts. Follow the API's
[backup and recovery instructions](session-api.md#compacting-old-turn-receipts).

The [worker API reference](session-api.md) has the full detail. Its [Connect](session-api.md#connect)
section covers the worker command.
