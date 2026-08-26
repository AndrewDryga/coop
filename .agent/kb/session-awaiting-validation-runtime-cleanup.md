---
name: session-awaiting-validation-runtime-cleanup
description: awaiting-validation owns durable candidate authority but no live provider runtime; startup and the bounded janitor reap runtime state without deciding the candidate
subsystem: sessions
sources: [internal/session/store.go, internal/sessionsvc/acp.go, internal/sessionsvc/service.go]
updated: 2026-08-26
---

# Awaiting validation is durable authority, not runtime authority

A schema-valid semantic candidate stays `awaiting_validation` with the session activity `running`
and its turn active. That deliberately blocks the FIFO until the caller accepts or rejects the exact
digest. It does not authorize the ACP child, run-labeled container, projected credentials, or
services to remain alive.

The ACP runner stages the candidate before deferred teardown. Once staging succeeds, a teardown
error is a warning: it cannot hide or fail the durable candidate. Startup lists
`starting`/`running`/`awaiting_validation` turns and reaps their exact runtime before serving;
reconciliation still terminalizes only interrupted `starting`/`running` turns. The bounded idle
runtime janitor retries awaiting-candidate cleanup under the per-session runtime lock and records a
successful proof against session revision/update plus turn/digest. Acceptance, rejection,
cancellation, or replacement therefore invalidates the proof naturally.

Acceptance, rejection, and awaiting-turn cancellation take the same runtime lock and require an
exact successful reap (or that matching proof) before changing durable state. A cleanup failure
leaves the candidate awaiting and terminally fails that idempotent operation; after runtime
recovery the caller rereads the candidate and submits its still-current decision with a fresh key.

Cleanup is host-runtime work only. It must not call the validation operation, change the candidate,
publish the assistant message, alter usage/cost or artifacts, or clear the native session binding.

## Changelog
- 2026-08-26 — made candidate decisions themselves a cleanup barrier so an immediate accept,
  reject, or cancel cannot erase the janitor's only ownership signal or race a replacement worker.
- 2026-08-26 — created after closing the gap where a crash or teardown failure after candidate
  staging could leave a credential-bearing runtime outside both startup recovery and the parked
  session janitor.
