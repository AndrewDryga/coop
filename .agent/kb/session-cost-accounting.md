---
name: session-cost-accounting
description: logical-turn cost accumulates explicit native-session counter deltas across attempts and provider epochs
subsystem: session
sources: [internal/session/store.go, internal/session/turn_usage.go, internal/session/cost_test.go, internal/sessionsvc/acp.go, internal/sessionsvc/service.go, internal/sessionsvc/cost_test.go]
updated: 2026-11-04
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

Schema 26 normalized retained pending raw counters using the original baseline and reset-event
ordering. That conversion is gone: a store below schema 26 is refused by name (build at or before
29133b0e converts it), and a new store builds straight to 26 with no costs to convert. Historical
operation receipts stay unchanged: missing or overwritten provider observations cannot be
reconstructed.

## Changelog
- 2026-11-04 — removed the schema 26 cost backfill (`cost_migration.go`); the schema floor is 26 (task 2026-11-04-drop-the-old-session-store-row-readers).
- 2026-10-28 — recorded transactional attempt accounting, native epoch changes, persistence-failure settlement and conservative schema 26 migration; checked real SQLite and ACP regressions.
