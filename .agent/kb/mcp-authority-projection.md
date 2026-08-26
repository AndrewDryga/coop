---
name: mcp-authority-projection
description: one validated shared snapshot fans out to native configs, direct command args, nested wrappers, and ACP without widening credential scope
subsystem: box
sources: [internal/mcp/mcp.go, internal/agent/agent.go, internal/agent/claude.go, internal/box/auth.go, internal/box/run.go, internal/consult/wrapper.go, internal/preset/wrapper.go]
updated: 2026-08-26
---

`COOP_MCP_FILE` is one host authority, but a box has four different consumers. `box.Run` captures
and validates it once, then calls each credential-scoped adapter's `MCP` transform before mutating
any agent home. Generated native configs are immutable mounts; an outer ordinary CLI gets
`MCPConfig.CommandArgs`; a peer or runnable preset role gets adapter-owned
`NestedCommandEnv` consumed by its consult/delegate fragments; an ACP adapter receives servers
through `ACPMCPServers` during protocol initialization. These are projections of the same snapshot,
not separate sources.

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
- 2026-08-26 — created after the v9 range review found that Claude's direct command received the
  shared snapshot while Claude peers and preset roles silently did not
