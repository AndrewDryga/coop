---
name: session-awaiting-validation-runtime-cleanup
description: every started turn retains its exact runtime receipt through teardown; awaiting-validation owns durable candidate authority but no live provider runtime
subsystem: sessions
sources: [internal/session/store.go, internal/sessionsvc/acp.go, internal/sessionsvc/service.go, internal/forkspace/generation.go, internal/forkspace/reservation.go]
updated: 2026-09-05
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

Successful accept/reject operations wake the session FIFO after releasing runtime and operation
locks, including successful receipt replay. The candidate worker may already have exited, so its
old drain loop is not a wakeup guarantee. When MaxTurns is reached, final rejection exhausts queued
turns and their inputs, queue counters and session budget in the same transaction as the failed
turn before scheduling.

Cleanup is host-runtime work only. It must not call the validation operation, change the candidate,
publish the assistant message, alter usage/cost or artifacts, or clear the native session binding.

Schema v15 stores `turns.runtime_run_id` before an ordinary or borrowed-warm child starts. The
receipt survives terminal success and candidate staging until exact process/box cleanup succeeds;
the terminal turn therefore remains discoverable by cleanup even though it has left the ordinary
running-turn query. A cleanup failure is warned and retained—it never rewrites a successful answer
or semantic candidate into provider failure. Cleanup clears only the same exact runtime ID, so a
replacement child cannot lose its ownership evidence.

A generationless bound session from an older schema is not enough authority to clean anything.
Startup may adopt it only when the current generation already has an exact remote-session
reservation for that session ID, proving a partial upgrade rather than path reuse. Without that
proof the session is quarantined: interrupted-turn reconciliation excludes it transactionally,
workers and janitors skip it, and every workspace/runtime/destructive API fails before touching the
fork. Read-only durable session and turn history remains available for manual recovery.

## Changelog
- 2026-09-05 — verified semantic decision wakeups with exited/unwinding workers and replay;
  final rejection now preserves normal transactional MaxTurns exhaustion and queued-input cleanup.
- 2026-08-28 — made legacy generation adoption require an exact pre-existing session reservation;
  unproven sessions remain byte-stable and quarantined from runtime and workspace operations.
- 2026-08-28 — persisted exact ordinary and borrowed-warm runtime IDs through terminal transitions;
  successful answers remain successful while session cleanup retries the exact leftover runtime.
- 2026-08-26 — made candidate decisions themselves a cleanup barrier so an immediate accept,
  reject, or cancel cannot erase the janitor's only ownership signal or race a replacement worker.
- 2026-08-26 — created after closing the gap where a crash or teardown failure after candidate
  staging could leave a credential-bearing runtime outside both startup recovery and the parked
  session janitor.
