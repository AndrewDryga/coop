---
name: mcp-authority-projection
description: one validated shared snapshot fans out to native configs, direct command args, nested wrappers, and ACP without widening credential scope
subsystem: box
sources: [internal/mcp/mcp.go, internal/mcp/broker.go, internal/box/mcp_broker.go, internal/agent/agent.go, internal/agent/claude.go, internal/agent/codex.go, internal/agent/gemini.go, internal/agent/grok.go, internal/box/auth.go, internal/box/run.go, internal/box/mcp_env.go, internal/box/taskchannel.go, internal/consult/wrapper.go, internal/preset/wrapper.go, internal/sessionsvc/acp.go]
updated: 2026-09-19
---

`COOP_MCP_FILE` is one host authority, but a box has four different consumers. `box.Run` captures
and validates it once, then calls each credential-scoped adapter's `MCP` transform before mutating
any agent home. Generated native configs are immutable mounts; an outer ordinary CLI gets
`MCPConfig.CommandArgs`; a peer or runnable preset role gets adapter-owned
`NestedCommandEnv` consumed by its consult/delegate fragments; an ACP adapter receives servers
through `ACPMCPServers` on `session/new` and `session/load` — codex-acp 1.7 needs them there as well as
in `config.toml` and deduplicates the two routes itself (`internal/agent/codex.go`). These are projections of the same snapshot,
not separate sources.

Every projection crosses the same host-file boundary before parsing. Shared and native config must
be regular files opened with nonblocking, no-follow semantics; Coop validates and reads the same
descriptor and retains at most 4 MiB. Only a path absent at the initial observation is inert. A
symlink, FIFO, directory, open/read failure, replacement with a special file, or content over the
limit fails before home mutation or provider launch. `box.Run` proves the configured shared source
is outside all wholesale mounts before opening it, so a provider-writable repo file cannot block or
consume memory on the way to an overlap refusal. It checks both the configured spelling and the
resolved target plus every resolved path prefix, and rejects raw `..` components whose meaning
could change across a parent symlink; cleaning such a path before resolution is not a containment
proof. Snapshot capture uses the validated resolved path, so a later parent-symlink retarget cannot
change which file the run reads.

Remote-session projection crosses that boundary before changing its private config tree. It rejects
an authority inside the agent-exposed primary/companion workspaces, selected credential profile, or
private session state, copies only the captured bytes, and treats adapter rendering errors as
credential-projection failures before the ACP child starts. Missing and server-empty authorities
remain intentionally inert; malformed or ambiguous active configuration never degrades into a
tool-free answer.

`mcp.BindTaskTools` is a second coop-owned binding beside `BindResponderState`: for a loop work box
it merges the reserved `coop-tasks` STDIO server (`{"command":"socat","args":["STDIO","UNIX-CONNECT:
/coop/tasks/mcp.sock"]}`) into the same validated snapshot before the per-run artifact is written, so
every projection — claude `--mcp-config`, codex TOML, gemini settings, ACP `session/new` — carries it
unchanged. Unlike responder-state it needs no bearer token: the socket is mounted only into this run's
box, so the mount is the authority. See [[in-box-task-channel]] for why the transport is a
helper-container socket and not HTTP.

The generated native configs are also where the MANAGED-CLIENT defaults live, so codex's
`config.toml` and gemini's `settings.json` overlays exist for every box that mounts that home,
shared MCP or not (`box.Run` asks each in-scope adapter's `MCP` even with `MCPFile` blanked). Codex's
opens with `mcp.CodexManagedDefaults` — `check_for_update_on_startup = false`, `analytics.enabled =
false`, the three `otel.*` exporters `"none"` — then the host profile's bytes verbatim minus the
keys the box owns (and minus native `[mcp_servers.*]` only when shared MCP is active; without it the
profile's own servers stay authoritative); the removal is proven by re-parse and a spelling the
textual strip cannot remove (quoted or array table) is refused by name, like the MCP forms. Gemini's
forces `general.enableAutoUpdate`, `general.enableAutoUpdateNotification` and
`privacy.usageStatisticsEnabled` to false beside the file-filtering override. Grok uses its own
`mcp.GenerateGrok` server writer and gets no managed block: its CLI does not know Codex's keys.
Grok accepts literal HTTP headers and expands `${VAR}` in header values, so the renderer translates
`bearer_token_env_var` to Authorization while returning the required name for the same private
environment capture Gemini uses. Codex takes headers too, in two exclusive tables: a literal value
goes in `http_headers`, and a value that is exactly one `${VARIABLE}` goes in `env_http_headers` as
the variable's NAME, so the secret is resolved by the client and never written into the file
(qualified with `codex mcp get` against codex-cli 0.153.4, which lists both tables beside
`bearer_token_env_var` for one server). Its `RequiredEnv` therefore covers every referenced name,
not only the bearer one — the client drops an unset header variable SILENTLY, so nothing downstream
would report the miss. A value that mixes text with a reference is refused: `http_headers` is sent
verbatim, so it would travel upstream as the literal characters.

Transport is the other asymmetry. Grok speaks legacy SSE for real (`grok mcp add -t sse` writes
`type = "sse"`, which the renderer now carries through instead of dropping), and Claude's and
Gemini's clients decide for themselves. Codex's client accepts an SSE declaration and then drives
the server as streamable HTTP with no warning, and `@agentclientprotocol/codex-acp` throws
`invalidRequest` on one — which fails `session/new` entirely, so a single legacy server takes down a
session carrying working ones. Coop therefore refuses SSE for Codex on BOTH paths, naming the
server; the ACP guard lives in `codexAgent.ACPMCPServers`, where provider-specific knowledge
belongs.
The mounts are read-only, so a client cannot persist a setting change from inside the box; the host
profile is never written by a projection (`EnsureDefaults` alone writes it, for first-run prompts).
Claude's controls are environment, not a file (`claudeAgent.BoxEnv`). Why the traffic is stopped at
the client rather than granted or hidden: [[provider-bundles-carry-function-not-chatter]].

Sign-in is deliberately not a coding session. `RunSpec.Login` selects `Agent.LoginConfig`, skips
the shared MCP snapshot and project mount, and mounts only the selected credential home plus its
managed login controls. Gemini keeps user `settings.json` writable so native Google selection can
survive its relaunch; separate immutable system settings retain managed defaults and deny MCP via
`mcp.allowed: []`, while `--extensions none` prevents extension activation. Native system merges
do not replace a user's server map with an empty map, and v0.59's remote-admin merge supersedes
local `admin` controls: neither is a substitute for the empty allowlist. Project services, ports,
instructions and preset/peer tools do not belong in login. Network admission still applies.

Gemini's native schema rejects canonical `bearer_token_env_var`. Its renderer converts that field
to `headers.Authorization: "Bearer ${NAME}"` and returns the required variable names; it never
stores token values in settings. Before a provider launch, `captureRequiredMCPEnv` resolves those
names from the assembled, credential-scoped runtime environment and freezes nonempty values in a
private temporary env file, removing their explicit argv overrides. Missing or blank values stop
the launch: Gemini itself leaves a missing placeholder literal, so interpolation alone is not a
denial check. Normal, ACP and nested Gemini all use this boundary. This check happens after setup,
before the provider; it does not promise that no daemon or sidecar was started.

**Filtered runs broker bearer servers** (`box/mcp_broker.go`, `mcp/broker.go`). Every
secret-bearing server `mcp.SecretServers` finds becomes a route of the run's credential broker after
the provider routes (`planMCPRoutes`; listener i, route name `mcp-<i>`, kind `mcp`: exact URL path,
POST/GET/DELETE, no response-header timeout). A secret is a `bearer_token_env_var` or ONE header
that is literal text then one `${VAR}` (anything else is refused by server name); under filtered,
admission already refused `${`, so only bearer servers get here. `SecretServer.Bearer` tells a
`bearer_token_env_var` from an `Authorization: Bearer ${X}` header written out — they read the same
on the wire but are rewritten in different places. A header the gateway would reject
(`networkgateway.MCPSecretHeader`) is refused HERE, by server name; an ACP adapter takes inline
headers only, so `acpServer` resolves every `${VAR}` in a header from the same environment the
bearer token is read from and DROPS a server whose reference has no value — a stand-in left
unresolved would 401 that server for the whole session. The rewrite happens ONCE, before any projection:
`mcp.RouteThroughBroker` points each server at `http://127.0.0.1:<port><path>` and renames its
variable to `COOP_MCP_TOKEN_<i>` (renaming is required — two servers sharing one operator variable
need two stand-ins), so generated configs, claude's file and a session's ACP list all follow it. The
operator's variables — every name the CONFIGURED file references, read from the source even for a
box that loads no MCP (`mcpScrubNames`) — are dropped from a filtered box's env, and the
`COOP_MCP_TOKEN_` prefix is reserved in operator input (`ReadValidatedSnapshot`). An SSE bearer
server, a missing token or one set through `-e` refuses by name. The pinned claude IGNORES
`bearer_token_env_var` (captured 2026-09-19: no Authorization at all) and expands `${VAR}` in a
header, so claude's mount is `mcp.ClaudeView` — the snapshot with each bearer turned into
`Authorization: Bearer ${NAME}` — in every non-restricted mode (restricted modes mount no MCP file). A filtered SESSION child writes its lead adapter's
final ACP list to the daemon-named `COOP_SESSION_MCP_HANDOFF` file before its container starts; the
daemon renders nothing pre-spawn for it, reads the file once after `initialize` (bound to the run
id), and refuses the turn without it. A filtered session that withholds MCP drops the source's token
names from its private env copy.

**Offline runs drop every remote server** (`box.Run`, `sessionsvc` `captureSessionMCP`). A box on
`--network none` cannot reach one, so `mcp.WithoutRemoteServers` removes each server with a `url`
before any projection, `mcpScrub`'s names leave the env (and a `-e` of one is refused, as under
filtered), and the network section names what was left out. An offline session's private copy is
written without them and without the Responder binding. Its daemon-side `session/new` render reads
that copy, so it needs no handoff. Residual: only names the configured file references are scrubbed
— a leftover token elsewhere in the env file still rides a filtered box's env.

Credential scope is not proof of command consumption. `credentialScope` answers whose login may be
mounted, while `nestedAgentCommand` answers whether an explicit peer or a consult/delegate role can
actually spawn that provider CLI (a native role runs in the lead's own session and spawns none).
Claude's snapshot mount exists only for an outer ordinary command or such a nested consumer. In particular, a plain Claude ACP run does not gain the
ordinary CLI's `--mcp-config` mount; `claude-agent-acp` uses the protocol projection instead.

The adapter owns both halves of nested wiring. Claude declares the trusted in-box snapshot path in
`MCPConfig.NestedCommandEnv` and its fresh consult, resumed consult, and delegate fragments all add
`--mcp-config` from that variable. `box.Run` appends this trusted environment after the user env
file, so ambient configuration cannot redirect the wrapper to another file. A future adapter that
adds ordinary `CommandArgs` must decide whether its nested commands need an equivalent environment;
mounting the raw snapshot for every scoped credential is not the fallback.

## Changelog
- 2026-09-19 — planning reads `mcp.SecretServers` (bearer or one `prefix${VAR}` header); the rewrite
  turns a header secret into `prefix${COOP_MCP_TOKEN_<i>}` where it is written.
- 2026-09-19 — offline runs drop every remote server (`mcp.WithoutRemoteServers`, one rewrite before
  every projection), scrub their token names and name them at launch; an offline session's private
  copy is written without them, so its box and its session/new both follow.
- 2026-09-19 — filtered runs broker bearer MCP servers (one rewrite before every projection, stand-in
  variables, source-derived scrub, session handoff); claude's mount became `mcp.ClaudeView` because
  the pinned claude ignores `bearer_token_env_var`.
- 2026-09-19 — native preset roles are never demoted to consults any more; nestedAgentCommand line re-verified.
- 2026-09-18 — Codex's header support corrected: it takes `http_headers` and `env_http_headers`
  beside `bearer_token_env_var`, qualified against the pinned 0.153.4 binary, so the card's
  "refuses a shared server that declares headers" claim is retired. Added the SSE asymmetry: Grok's
  declaration is carried through, Codex's is refused on both the file and ACP paths because its
  client rewrites it silently and its ACP adapter fails the whole session.
- 2026-09-15 — split Grok's native HTTP authentication from Codex: verified Grok header and
  `${VAR}` support against the installed CLI documentation, then recorded Codex's upfront refusal
  and Grok's captured bearer projection.
- 2026-09-13 — isolated native login controls from coding projections; verified Gemini 0.59 schema,
  allowlist and save/relaunch persistence; documented bearer projection and runtime capture.
- 2026-09-10 — the codex and gemini overlays are always-on carriers of the managed-client defaults
  (no update check, no analytics/telemetry export); codex's keeps the profile's native MCP tables
  when shared MCP is inactive; grok moved to `GenerateGrok` so codex's keys never reach its file.
- 2026-09-10 — added the second coop-owned binding, `mcp.BindTaskTools`: the box's `coop-tasks` STDIO
  task server merges into the same snapshot before the artifact write, tokenless because the socket is
  run-private. See in-box-task-channel.
- 2026-08-26 — routed remote ACP session projection through the same canonical snapshot and made
  active-adapter errors fail before child launch
- 2026-08-26 — centralized shared/native host reads behind the bounded regular-file boundary and
  moved box source-isolation validation before snapshot capture
- 2026-08-26 — created after the v9 range review found that Claude's direct command received the
  shared snapshot while Claude peers and preset roles silently did not
- 2026-09-05 — ACP servers travel on session/new and session/load, not initialize; recorded the codex-acp 1.7 dual route (46a2500).
