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

## Changelog
- 2026-08-12 — created after recovering an Aug 7 production create intent that crossed the
  single-target to target-ladder schema change.
