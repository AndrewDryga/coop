---
name: session-cost-accounting
description: logical-turn cost accumulates explicit native-session counter deltas across attempts and provider epochs
subsystem: session
sources: [internal/session/store.go, internal/session/turn_usage.go, internal/session/cost_migration.go, internal/session/cost_test.go, internal/session/cost_migration_test.go, internal/sessionsvc/acp.go, internal/sessionsvc/service.go, internal/sessionsvc/cost_test.go]
updated: 2026-10-28
---

`Turn.Usage.CostUSD` is accumulated logical-turn spend, including pending validation.
The session cursor is the latest accounted cumulative cost in its current native epoch.
`RecordTurnUsage` atomically adds only the counter delta and advances that cursor; repeated
observations add nothing. Explicit zero is known; absent/null reports leave cost unknown.
A lower counter starts a numeric reset; a native-session rotation explicitly clears the cursor
while retaining the logical turn's earlier spend. Tokens remain reported turn totals.

The ACP runner checkpoints every observation before validation or provider rotation, including
updates received before prompt failure/cancellation. A failed checkpoint retains the raw report
for bounded retry or atomic failure settlement. If neither can persist it, the unsettled-usage
error prevents generic cancellation/failure from falsely closing the turn. Reserved cancellation
requests are replayed unchanged; accumulated cost is never resubmitted as a raw native counter.

Schema 26 normalizes retained pending raw counters using the original baseline and reset-event
ordering. A session-wide reset newer than every cost-bearing terminal event proves an idle cursor
stale. Pending observations after that reset supply a new cursor; earlier candidates retain their
old-epoch delta. Ambiguous terminal histories and immutable operation receipts remain unchanged:
missing or overwritten historical provider observations cannot be reconstructed.

## Changelog
- 2026-10-28 — recorded transactional attempt accounting, native epoch changes, persistence-failure settlement and conservative schema 26 migration; checked real SQLite and ACP regressions.
