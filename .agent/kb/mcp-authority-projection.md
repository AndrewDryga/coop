---
name: mcp-authority-projection
description: one validated shared snapshot fans out to native configs, direct command args, nested wrappers, and ACP without widening credential scope
subsystem: box
sources: [internal/mcp/mcp.go, internal/agent/agent.go, internal/agent/claude.go, internal/box/auth.go, internal/box/run.go, internal/consult/wrapper.go, internal/preset/wrapper.go, internal/sessionsvc/acp.go]
updated: 2026-08-26
---

`COOP_MCP_FILE` is one host authority, but a box has four different consumers. `box.Run` captures
and validates it once, then calls each credential-scoped adapter's `MCP` transform before mutating
any agent home. Generated native configs are immutable mounts; an outer ordinary CLI gets
`MCPConfig.CommandArgs`; a peer or runnable preset role gets adapter-owned
`NestedCommandEnv` consumed by its consult/delegate fragments; an ACP adapter receives servers
through `ACPMCPServers` during protocol initialization. These are projections of the same snapshot,
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

Credential scope is not proof of command consumption. `credentialScope` answers whose login may be
mounted, while `nestedAgentCommand` answers whether an explicit peer or consult/delegate/degraded
native role can actually spawn that provider CLI. The raw snapshot mount exists only for an outer
ordinary command or such a nested consumer. In particular, a plain Claude ACP run does not gain the
ordinary CLI's `--mcp-config` mount; `claude-agent-acp` uses the protocol projection instead.

The adapter owns both halves of nested wiring. Claude declares the trusted in-box snapshot path in
`MCPConfig.NestedCommandEnv` and its fresh consult, resumed consult, and delegate fragments all add
`--mcp-config` from that variable. `box.Run` appends this trusted environment after the user env
file, so ambient configuration cannot redirect the wrapper to another file. A future adapter that
adds ordinary `CommandArgs` must decide whether its nested commands need an equivalent environment;
mounting the raw snapshot for every scoped credential is not the fallback.

## Changelog
- 2026-08-26 — routed remote ACP session projection through the same canonical snapshot and made
  active-adapter errors fail before child launch
- 2026-08-26 — centralized shared/native host reads behind the bounded regular-file boundary and
  moved box source-isolation validation before snapshot capture
- 2026-08-26 — created after the v9 range review found that Claude's direct command received the
  shared snapshot while Claude peers and preset roles silently did not
