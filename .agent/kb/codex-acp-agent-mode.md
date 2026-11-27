---
name: codex-acp-agent-mode
description: Codex ACP must select full-access mode explicitly because session config does not override its per-turn sandbox policy
subsystem: acp
sources: [internal/agent/codex.go, internal/agent/agent_test.go, internal/box/run.go, internal/sessionsvc/acp.go]
updated: 2026-11-27
---

`codex-acp` derives every turn's approval and sandbox policies from its selected agent mode.
Its default `agent` mode explicitly requests `on-request` plus `workspace-write`, so Codex tries
to launch bubblewrap inside Coop's container even when `CODEX_CONFIG` or `config.toml` asks for a
different sandbox. That nested namespace is unavailable by design.

`codexAgent.ACP` therefore scopes `INITIAL_AGENT_MODE=agent-full-access` to the adapter process
(`internal/agent/codex.go`). This selects `never` plus `dangerFullAccess` before the first prompt.
Do not move the setting into persistent profile config or a Responder-specific path: it is a
provider adapter invariant shared by ordinary ACP and session-backed ACP.

This removes Codex's inner defense in depth, not Coop's boundary. The outer box still owns the
fork mount, credential scope, secret shadowing, capability restrictions, and network policy
(`internal/box/run.go`). Preserve those controls and keep managed sessions credential-poor.

The session daemon starts the adapter directly. After creating or loading a native thread, it
applies the selected model and reasoning effort and checks the adapter’s reply before sending the
first prompt. Warm turns on that thread retain the selection. A rejected selection fails the turn.

## Changelog
- 2026-11-27 - recorded who selects the model after a remote turn used the adapter default
- 2026-07-27 - created after a live Responder turn proved the default mode fails at bubblewrap and
  the process-scoped full-access mode executes tools inside the existing Coop boundary
