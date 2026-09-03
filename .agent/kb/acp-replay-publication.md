---
name: acp-replay-publication
description: ACP replacement sessions become authoritative as one generation before held editor work is released
subsystem: acp
sources: [internal/acpproxy/proxy.go, internal/acpproxy/proxy_test.go]
updated: 2026-09-03
---

Across SIGHUP, the proxy serializes exactly one initialize request plus explicit editor, adapter,
and provider identity for every session. A legacy setup tape or a session missing either native
identity is consumed as an invalid handoff and starts fresh; restore never guesses. Replay sends
`session/load` only when the replacement child names the same provider. A different or unnamed
provider gets `session/new`, so a foreign native id is never probed.

The editor session id is durable; each provider-owned native id is generation-scoped. Replay sorts
the durable sessions, negotiates all load/new results off to the side, rejects duplicate native
bindings, then swaps the complete binding set atomically (`replayAt`, `duplicateReplayBinding`, and
`swapChildAt` in `internal/acpproxy/proxy.go`). A provider switch therefore
uses direct `session/new`, never probes a foreign id, and only successful bindings publish.

Prompts arriving after a switch acknowledgement wait behind `restarting` until settings, recreation
hooks, and config updates are complete; the final release revalidates each session
(`releaseRestartHeld` in `internal/acpproxy/proxy.go`). Close/delete during either candidate replay
or the post-swap tail retires local state and sends a generation-checked lifecycle cleanup, so a late
native update cannot resurrect the editor thread (`forwardClientControlled` and
`writeLifecycleCleanup` in `internal/acpproxy/proxy.go`).

## Changelog
- 2026-09-03 - collapsed SIGHUP setup state to one initialize frame, made restored adapter/provider
  identity mandatory, removed providerless loading, and reverified replay plus process-control tests
- 2026-07-15 - created from the replay-publication and lifecycle portions of acp-scripted-e2e
