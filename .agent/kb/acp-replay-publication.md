---
name: acp-replay-publication
description: ACP replacement sessions become authoritative as one generation before held editor work is released
subsystem: acp
sources: [internal/acpproxy/proxy.go, internal/acpproxy/proxy_test.go, internal/acpproxy/cancel.go, internal/acpproxy/cancel_test.go, internal/acpproxy/factory.go, internal/acpproxy/factory_test.go, internal/acpctl/cancel.go, internal/acpctl/cancel_test.go]
updated: 2026-09-12
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

Cancellation shares the admission/publication control mutex. A prompt held for replay or target
settings is locally completed as cancelled; a prompt admitted to the current child remains the
adapter's responsibility. Reserved replay IDs are not wire-active prompts. The controller clears
quota resend intent while retaining session/model caches and visible conversation history. Removing
a held prompt must not remove its target-setting gate, and releasing a force queue must not race
cancellation. `scripted_cancel_e2e_test.go` also covers cancel during quota wait followed by switching.

A replacement factory may be waiting for quota with no child to stop. Each factory attempt owns a
context cancelled by a newer selection, reload, or disconnect. A successful child's context remains
alive until its retirement; cancelling it immediately on factory return would kill live resources.
Natural EOF retires those resources before entering the replacement factory, not after its possible
quota wait. The launcher checks cancellation after quota waits and before launching a replacement process.

## Changelog
- 2026-09-12 - documented cancellation ownership and interruptible factory waits; verified held/active cancellation, quota-switch scripts, and replacement-context barriers
- 2026-09-03 - collapsed SIGHUP setup state to one initialize frame, made restored adapter/provider
  identity mandatory, removed providerless loading, and reverified replay plus process-control tests
- 2026-07-15 - created from the replay-publication and lifecycle portions of acp-scripted-e2e
