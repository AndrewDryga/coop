---
name: session-migration-fixtures
description: historical SQLite fixtures own frozen DDL and rows, independent of current migrations; test clocks and IDs are private store state
subsystem: session
sources: [internal/session/legacy_schema_test.go, internal/session/migration_test.go, internal/session/schema.go, internal/session/store.go, internal/session/store_test.go, internal/session/compact.go, internal/session/check_test.go]
updated: 2026-11-04
---

Historical session databases are built from test-owned literal DDL increments 1–25,
with versions 1–24 frozen from 260ccd2 and version 25 captured before schema 26.
Handwritten rows contain only their version's columns.
`TestHistoricalSchemaFixturesStayFrozen` pins the original prefix and version 25 separately.
Do not build an old fixture through current `Open`, `migrate` or `schemaV*` constants:
that can make a broken migration redefine the database it is supposed to repair.
When adding a schema version, capture the previous released shape independently,
then deliberately update the frozen fixture and its digest.

Since 2026-11-04 the floor is schema 26, so the fixtures no longer feed a conversion. They build
stopped stores below the floor: `TestOpenRefusesEveryOlderSchemaByNameAndLeavesTheStoreAlone`
proves Open refuses each by name and changes no byte, and the check tests use them for
`StoreTooOld`. The v1–v25 DDL ladder still builds a new store, so it stays. Rollback and
newer-version refusal tests remain separate storage contracts.

Production `Open(root)` has no public fixture options. Tests set the private clock and
ID generator only after a real open; compaction retains its private store options.
This does not replace SQLite, fsync, migration or lock behavior with test doubles.

## Changelog
- 2026-11-04 — schema floor raised to 26: the historical-version conversion tests became refusal tests; the cost backfill tests went with the backfill.
- 2026-10-28 — froze version 25 independently; schema 26 fixtures exercise pending cost normalization, proven idle epoch resets, ambiguous-history preservation and unchanged receipts.
- 2026-10-17 — spacing in "increments 1–24" and "from 260ccd2" (and the same comment in
  legacy_schema_test.go).
- 2026-10-14 — separated historical fixture DDL from production constants; a renamed
  production migration column fails the v1 owner and exact restoration passes. Removed
  test-only public clock/ID options after checking all callers and private compaction.
