# Editors, MCP and controllers

Use this reference to connect an editor over ACP, share MCP servers across agents, or run co:op
workers for a controller. For a shorter introduction, see the guide's
[Editors](https://coop.dryga.com/docs.html#editors) and
[MCP and remote sessions](https://coop.dryga.com/docs.html#mcp-remote) sections.

## Zed and other ACP editors

Use Zed or another [ACP](https://agentclientprotocol.com) editor to work with an agent running in
a co:op box. You send prompts from the editor's agent panel, and co:op runs the agent in the box
and handles account rotation.

### Set up Zed

1. Install co:op. The [install one-liner](install.md) puts `coop` on your `PATH` and
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

Without a target, co:op checks Claude, Codex, Gemini and Grok in that order and starts the first
signed-in provider with its default account. You can change the provider or choose a preset in
the toolbar. If no provider is signed in, run `coop login <agent>` and reconnect the editor.

To choose the starting model, reasoning effort and account, add a target to `args` in the form
`provider[:model][/effort][@account]`, such as `["acp","claude:opus/xhigh@work"]`. Set these in
the target, not in the editor's separate defaults. The toolbar shows your target's choices, and
you can still change them during the thread.

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

SSH carries the process's input and output, and co:op doesn't open a TCP listener for ACP. Use
this setup only if Zed can resolve the same absolute repository paths that the remote ACP process
reports. If the machines use different paths, use native Remote Development.

### What runs in the box

Zed runs `coop acp [<target|preset>]` in the project directory. That starts the selected
provider's ACP adapter in the box, and the adapter talks ACP to the editor over stdio.

| Provider | Adapter in the box |
| --- | --- |
| Claude | `@agentclientprotocol/claude-agent-acp` |
| Codex | `@agentclientprotocol/codex-acp` |
| Gemini | `gemini --acp` |
| Grok | `grok agent stdio` |

In a normal writable session, co:op mounts the repository at its real host path, so Zed's absolute
file paths resolve correctly.

Tool calls don't ask for permission. co:op runs editor sessions in yolo mode, regardless of the
provider's settings, and ignores the editor's permission `mode` setting. The box isolates requests
handled inside it, but an agent can also use ACP to ask the editor to access files and terminals
on your computer. See the file and terminal restrictions below.

Editor threads store the agent's complete native home in
`~/.config/coop/agents/<agent>/acp-homes/<repository-key>/home/`. Accounts for the same provider
share that repository's home, so you can switch accounts during a thread without losing the
conversation or exposing another project's threads. Sessions from `coop <agent>` and `coop loop`
use separate storage.

### The toolbar

A plain session shows the Preset, Provider and Account menus:

- Changing accounts or other choices within the same provider keeps the conversation.
- Changing providers creates a new session. co:op tries to carry over the message text and
  one-line descriptions of tool use, but not tool payloads.
- If you switch during a turn, co:op sends the current prompt again to the new target.
- The model menu starts with the target's `:model` or your configured default, and you can change
  it.

With a preset active, the toolbar shows only the Preset menu. The preset's fallback list chooses
the provider, model, effort, account and roles, and co:op doesn't apply saved Provider and Account
choices from the editor.

Select None to choose a provider or account yourself. Both menus return: Provider shows the
provider currently in use, and Account is set to Auto.

### Usage limits

If a provider reports a usage limit during a turn, co:op hides the error, switches to your next
signed-in account and sends the prompt again, updating the account dropdown to match. If no
account is available, it posts
`Waiting for account "<name>" on <Provider> to reset its usage limit at <Mon 15:04 MST> (in MM:SS). Your message will send automatically.`
and sends your prompt when the limit resets.

### Restarts

A box can stop during `coop build` or `coop update`, when it runs out of memory, or when Docker
restarts. co:op always keeps the editor connected and starts another box, even for a thread that
hasn't received a message yet.

### Dev servers

Set `serve.ports` in [`.agent/project.yaml`](box.md#dev-servers-in-your-browser) to publish the
box's ports. The thread shows the stable `http://localhost:<port>` URLs you can open and tells you
when a server starts. After a switch, the URLs reach the box the thread is using.

### Tracing a session

To record a session's ACP traffic, run `touch ~/.config/coop/acp-debug` or set
`COOP_ACP_TRACE=1` in the agent server's environment. Each `coop acp` server then appends traffic
between the editor and box to `~/.local/state/coop/acp-trace-<pid>.log`.

Creating the file also turns tracing on for servers that are already running, so you can use
it after a problem appears. The trace contains prompts and file contents, so treat it as
sensitive. See [Troubleshooting](https://coop.dryga.com/docs.html#troubleshooting).

### Filtered networking

With [filtered networking](networking.md), the editor offers only compatible providers, and only
presets that can run in full under those network rules.

| Provider | Filtered ACP works with |
| --- | --- |
| Claude | its own supported credentials, or an API key |
| Codex | its own supported credentials, or an API key |
| Gemini | an AI Studio API key or importable plain OAuth account, but not Vertex |
| Grok | its OAuth login |

co:op refuses a Vertex `GOOGLE_API_KEY` in every network mode. Its broker keeps API keys and native
OAuth grants outside the box.

Switching providers never widens the session's network access. If you explicitly ask for an
unsupported provider or preset, the request fails before launch.

### Forks

To work in a [fork](forks.md) from the editor, start the adapter with `coop fork <name> acp`. The
agent edits the throwaway clone, not your working tree, and you review and land the result locally
without pushing the fork. co:op doesn't copy uncommitted, Git-ignored secrets into the fork, but
secrets already in Git history can remain.

The command starts the agent the fork was created with. Name a target to start another, as in
`coop fork <name> acp claude:opus`.

Configure this command with the parent project open in Zed, and co:op maps that directory to the
fork inside the box. If the editor is open on an unrelated directory, co:op refuses the session.
The fork, provider and account stay fixed, so co:op hides the usual ACP Provider and Preset menus.
You can still use any model and effort choices the adapter offers.

### Services and custom images

With open networking, run `coop up` for the repo's `.agent/compose.yml`, and the ACP box joins the
same network. With filtered networking, request and approve access to each service in the project's
network rules, and co:op starts those services on a separate network for the editor session.

A custom image must include the ACP adapters. If you build on co:op's box with
`FROM ${COOP_BASE_IMAGE}`, as `coop init` sets up, the image already has them. If you use another
base image, install them as shown in [box.md](box.md#your-own-image-agentdockerfile). For filtered
runs, the image must inherit co:op's client base and keep its client versions. Without the
required adapter, `coop acp` fails with an error such as `codex-acp: not found`.

### When the project box is not ready

With filtered networking, your editor connects even if the project's box can't start yet. The
thread says why, and anything you send waits. The agent doesn't run until the box is ready, so it
never works without your project's tools.

- If your build files haven't changed since you last ran `coop build`, co:op rebuilds the box by
  itself.
- If they're new or changed, co:op waits for you, because an agent can edit them and a build runs
  them with ordinary networking. Review them, then run `coop build --egress filtered` in the
  project.
- If the project has services and no network rules for them, the thread says to run `coop approve`,
  which lists the services and offers to add their rules. Or set `box.network: false` to start
  without services.
- If the project asks for network access you haven't approved, the thread says to run
  `coop approve`. Until then, the editor talks to a box that sees your project read-only and
  reaches only your agent's provider, and none of the rules the project asks for apply.
- If `.agent/project.yaml` doesn't load, the thread says what's wrong and where. The editor talks
  to that same box, and nothing in the file applies. Once you fix the file, anything new it asks
  for still waits for `coop approve`.
- If the box exits as it starts for any other reason, such as a service that changed since you
  approved it, the thread says so and the agent log has the reason. co:op tries again every 30
  seconds and whenever the project changes.
- If the box starts but doesn't answer within a minute, co:op stops it, says so in the thread and
  keeps trying. The time before it starts has its own limit of 5 minutes, not counting time the
  box reports waiting for another box. At that limit, the thread says the box took too long and
  co:op keeps trying.

When the box can start, the session moves onto it and sends what was waiting. You can cancel a
waiting message as usual. You don't need to reconnect, with one exception: a session started with
filtered networking can't move to a project that ends up with unrestricted access or no network at
all, so the thread asks you to reconnect.

If even the waiting box can't start, as happens when Docker has stopped, the session ends and the
thread says why. A session you start with `--egress offline` doesn't wait on a broken
`.agent/project.yaml`, because the waiting box would have a network. The editor's log says what's
wrong.

### Files and terminals on your computer

ACP lets an agent ask the editor to work on your computer directly, outside the box. Requests such
as `fs/read_text_file`, `fs/write_text_file` and `terminal/*` run through the editor, and a login
button can run a command there.

A filtered session blocks these requests. co:op doesn't tell the agent the editor can handle them,
refuses any such request from the box, hides the login buttons and refuses to open a link the box
chose. The agent can still ask you a question in the editor. It works on the project inside the
box as usual, so it sees what's saved on disk, not your unsaved edits.

Zed loads an image in an agent's message by itself. A remote image would send data to an address
outside the network rules, and a project path can be a symlink the box made, which Zed follows on
your computer. A filtered session blocks both: co:op turns any image in what the box writes into a
plain link, which you have to click. Only an image carried in the message as image data still
shows. In a code block, a Markdown image and a line that starts like a Markdown link definition,
such as `[key: string]: number;`, show a backslash.

An open or offline session forwards these requests to Zed without filtering them, so your editor's
file and terminal restrictions are part of the security boundary. A prompt-injected agent could
ask the editor to read or write an absolute path outside the repo on your computer. In a test,
Gemini's adapter wrote project files this way when the editor offered it, while Claude's and
Codex's adapters did their work inside the box.

`coop claude` and `coop loop` don't have this editor channel.

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

Set `COOP_MCP_FILE` to choose the shared file. The default is `mcp.json`. Keep it outside
repositories, companion repositories, agent credential homes and ACP session stores. co:op mounts
those directories in full, which would expose the file to the box, so it refuses a `COOP_MCP_FILE`
in any of them before launch, even if the file is empty or doesn't exist yet. The file must also:

- be a regular file, not a symlink or special file
- be 4 MiB or smaller
- have a canonical path without `..`

Gemini's native `settings.json` must follow the same file rules. co:op reads it on every Gemini
launch, Codex's native `config.toml` on every Codex launch, and Grok's native config when the
shared file is active. If one of these files is unsafe or can't be read, co:op stops that launch.

### How each agent gets them

At launch, `coop` passes the shared server definitions to each agent in its native format:

| Agent | Where the servers go |
| --- | --- |
| Claude | `--mcp-config` |
| Gemini | merged into its `settings.json` |
| Codex, Grok | converted to their provider-specific `[mcp_servers.*]` tables in `config.toml` |

co:op mounts the generated configs read-only over the existing ones inside the box and leaves your
source files unchanged.

When the shared file is active, it's the only source of MCP server definitions, and co:op leaves
out the native Codex and Grok `[mcp_servers.*]` tables.

co:op refuses dotted, quoted or inline `mcp_servers` entries in your native TOML before launch, so
remove them or move them into the shared file.

Remote sessions use the same `mcp.json` without extra setup. co:op checks it before changing any
session state, and the adapter gets only the copy it checked. If the active file is unsafe or
ambiguous, the turn stops before its child process starts.

### Tokens and headers

Every agent passes a command server's `env` block (`github` above) to that server unchanged.
The values are literal strings, so `$VAR` is not expanded.

To keep a token out of `mcp.json`, point `bearer_token_env_var` at a variable (`sentry` above),
and put the value in the env file:

```bash
echo 'SENTRY_TOKEN=…' >> ~/.config/coop/agents/env
```

Every agent supports an HTTP server's `headers`. Claude and Grok turn `bearer_token_env_var`
into an Authorization header using the environment variable's value.

For Codex, a literal value becomes a static header, and a value containing exactly one
`${VARIABLE}` becomes an environment-variable reference, which keeps the secret out of the
generated config. Codex can't represent a mixed value such as `Bearer ${TOKEN}`, so use
`bearer_token_env_var` for that form.

### SSE servers

The agents handle a legacy SSE server (`"type": "sse"`) differently:

| Agent | What happens |
| --- | --- |
| Grok | Grok supports legacy SSE, and co:op passes the declaration through. |
| Claude, Gemini | They receive the declaration and decide for themselves. |
| Codex | co:op stops before launch and names the unsupported server. |

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
even if that box doesn't load MCP. The ACP adapter receives only the replacement value, not
the real token.

A legacy SSE server with a bearer token needs a broader allowed route: `GET` on its stream
path and `POST` on any clean path at the same host. The token still stays outside the box, and
co:op reports this compatibility mode at launch. Prefer a streamable HTTP URL when the server
provides one, because that limits the route to an exact path.

#### Open runs

For supported MCP servers, an open run also keeps secrets outside the box. co:op starts a small,
unprivileged helper container on the box's network and removes it when the run ends. Like the
filtered broker, the helper holds each server's credential and accepts only a replacement value
valid for that run. The box's config points to `coop-broker` and uses `COOP_MCP_TOKEN_<n>`.

The box talks to the helper over plain HTTP on its container network, or on the default bridge if
the project has no network of its own. The replacement value protects access to the credential on
that network. At launch, co:op lists the servers the helper covers.

If the helper can't handle a server through one route, the server still works but its secret
stays inside the box. co:op reports the server's name and reason at launch. This happens for:

- a server whose secret is in two places, or whose secret header is anything other than literal
  text followed by one variable reference
- a URL that is not plain `https` on port 443
- every server, when the box has no network of its own to reach a helper on (Apple's `container`
  runtime, `--network none`, `--network container:…`)

Remote sessions with open networking use the same helper and the same fallbacks.

#### Offline runs

An offline run (`--egress offline`) omits all remote MCP servers because it can't reach them,
and reports this at launch. Local command servers still work. Tokens for the omitted remote
servers stay outside the box.

### Playwright

The image includes Chromium's system libraries. On first use, the example downloads the
Playwright package and browser, then keeps the browser in the cache volume. In a filtered run,
you must approve the download destinations and any sites the browser needs to visit. If a
download is blocked, review the reported destination before granting access.

The browser runs with `--headless --no-sandbox` and relies on the box for isolation.

## Controllers and workers

A controller is your platform that assigns jobs to co:op workers. Start a worker, which connects
out to the controller, on the controller's VM or a separate VM:

```bash
coop sessions connect --controller https://controller.example --token-file /run/secrets/coop-token
```

The controller supplies jobs with fixed repositories, context, model targets, network settings
and execution limits. co:op authenticates the controller, fetches complete Git/LFS working trees
and authorized submodules, runs the models in isolation and saves session state between runs.

You don't need a local policy catalog or worker configuration file. Any controller can use the
same protocol, and Ryker is one example.

A private Unix API manages sessions, turns in first-in-first-out order, inspection, checkpoints,
review, publication, cancellation, budgets and explicit cleanup. It stays on the host, doesn't
listen on TCP and doesn't enter a model sandbox.

After each turn, co:op removes the box unless the job uses a warm-idle timeout to keep it ready.
If cleanup fails, it retries in the background. A parked session alone doesn't confirm that its
containers are gone. Companion repositories are read-only snapshots of exact pinned versions.

With a fresh grant limited to the repository, the trusted host can push a fixed, reviewed
candidate and open a draft PR. The model never receives the GitHub credential.

`coop sessions doctor --json` checks the service that `connect` started. Before you upgrade a
worker, stop it and run `coop sessions check` with the new build. See
[Upgrading a worker](session-api.md#upgrading-a-worker).

If an old database has duplicate prompts in retry receipts, stop the worker and run:

```bash
coop sessions compact --backup /safe/path/session-before-compact.sqlite
```

Choose a new backup file outside the state root and treat it as sensitive. Compaction preserves
the original turns and prompts. Follow the API's
[backup and recovery instructions](session-api.md#compacting-old-turn-receipts).

The [worker API reference](session-api.md) has the full detail. Its
[Connect](session-api.md#connect) section covers the worker command.
