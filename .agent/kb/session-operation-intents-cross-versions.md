---
name: session-operation-intents-cross-versions
description: running session operations survive binary upgrades, so persisted intent JSON needs explicit compatibility normalization before replay
subsystem: session-api
sources: [internal/sessionsvc/service.go, internal/session/store.go]
updated: 2026-08-12
---

`operations.result` is also the write-ahead intent for a running cross-process operation
(`internal/session/store.go`). A daemon restart can therefore replay intent bytes written by an
older binary through current `sessionsvc` code. Those bytes are a durable wire format even when
the Go structs were originally package-private.

Compatibility must be handled before filesystem work or slice indexing. In particular, create
intents written before target ladders contain `Policy.Target` and no `Policy.Targets`; replay
normalizes that single target into a one-rung ladder. Missing or malformed authority data moves
the operation to `uncertain` and fails closed. It must never panic the HTTP handler or leave the
operation indefinitely `running`.

Create admission now intentionally has two durable intent phases. The first records the complete
operator policy, deterministic session/workspace identities, and optional governed pull-request
binding before remote Git resolution starts. The create worker resolves immutable repository pins,
then advances that exact running intent with an optimistic compare-and-swap before creating any
workspace. A restart may safely resume either phase because pinning is read-only and workspace
creation is deterministic. No other mutation receives this treatment: startup and the periodic
watchdog make arbitrary stale running operations `uncertain` instead of guessing whether an
external side effect happened. A stranded `reserved` row has no attempted side effect; the same
reconciler terminalizes it as a failed interrupted admission rather than leaving it invisible.

Clients may request this lifecycle with `Prefer: respond-async`, but the intent format and recovery
rules belong to the operation record, not HTTP. An exact idempotent replay coalesces onto the same
operation, and operation-correlated errors must preserve the operation ID while keeping internal
paths and secrets out of the public projection.

## Changelog
- 2026-08-12 — documented the admitted-to-pinned create-intent transition, bounded async worker,
  and general stale-operation reconciliation.
- 2026-08-12 — created after recovering an Aug 7 production create intent that crossed the
  single-target to target-ladder schema change.
