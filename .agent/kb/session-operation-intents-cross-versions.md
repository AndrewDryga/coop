---
name: session-operation-intents-cross-versions
description: persisted operation intents survive upgrades; replay requires proven frozen job authority and never reconstructs it from retired local policies
subsystem: session-api
sources: [internal/sessionsvc/service.go, internal/sessionsvc/review.go, internal/session/store.go]
updated: 2026-09-30
---

operations.result doubles as the write-ahead intent for a running cross-process operation.
Its JSON is durable even when the Go struct is private. Validate authority before filesystem
work, indexing or gate execution; missing proof becomes operation_uncertain, never a panic,
an indefinitely running row, or inferred replacement authority.

Create has one frozen intent: canonical JobDocument/JobDigest, task reference, deterministic
session/fork identities, the session store ID and the admitted endpoint binding. Sources have
already been privately staged. There is no policy snapshot, target-ladder normalization or
second source-pinning phase.
A retry checks the exact saved document and staging receipt before creating/reusing the workspace.
The create store transaction compares the captured intent before publishing the session result.
It also publishes the session's owner-store binding in that same transaction. A pre-upgrade
intent without a store ID may have published a v1 fork reservation before its session row existed;
replay marks it uncertain and leaves its workspace untouched rather than inventing v2 ownership.
An ambiguous generation or reservation publication keeps the new intent running so the same owner
can repeat the durability barrier; failing the operation there would strand a reserved fork.
For an already-existing workspace, that retry classification requires visible, valid generation
binding or a reservation for the exact session/store owner. A broken physical anchor or a refused
reservation with no matching record fails the create operation while leaving the workspace intact
for explicit recovery; it must not leave a Running operation that can never succeed.

Historical policy-only create intents cannot execute after the cutover. Existing sessions and
evidence are preserved, but replay never turns an old policy name into a new controller grant.
Version-1 job documents also lack the v2 check, environment and resource authority: new creates,
replayed creates, turns and recovered reviews refuse them rather than reconstructing setup from
the current repository or worker config. Recovered reviews prove the bound v2 job before any gate can run.
Retired pre-binding sessions may still carry active-turn runtime receipts. Startup excludes both
current quarantine and retired quarantine from ordinary turn reaping/reconciliation, which requires
workspace authority; a separate exact-run-label/private-ACP pass proves runtime capacity without
replaying old work or changing those durable turn rows.

Prefer: respond-async changes HTTP waiting, not custody. An exact retry coalesces onto the same
operation. Correlated failures retain the operation ID while public errors redact paths/secrets.
Startup and the watchdog reconcile other stale operations rather than guessing external effects;
a stranded reserved row can fail as interrupted admission because no effect was attempted.

## Changelog
- 2026-09-30 — rechecked replay against strict v2 JobSpec decoding; v1 has no complete
  work/review authority and remains inspectable history only.
- 2026-09-30 — checked retired pre-binding active turns at restart and documented their separate
  runtime-only proof; ordinary workspace-dependent replay remains denied.
- 2026-09-29 — rechecked existing-workspace adoption against generation anchors and reservation
  records; only visibly valid owner authority remains replayable after an adoption error.
- 2026-09-27 — checked create replay and store transaction sources; added the stable owner-store
  intent and atomic row binding, with fail-closed handling for historical in-flight creates.
- 2026-09-26 — removed obsolete policy/target normalization and two-phase pinning notes;
  reviewed current create CAS and added a regression denying jobless recovered review execution.
- 2026-09-05 — aligned replay fixtures with exact freshness custody.
- 2026-08-12 — documented durable intents and interrupted-operation reconciliation.
