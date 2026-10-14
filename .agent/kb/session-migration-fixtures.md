---
name: session-migration-fixtures
description: historical SQLite fixtures own frozen DDL and rows, independent of current migrations; test clocks and IDs are private store state
subsystem: session
sources: [internal/session/legacy_schema_test.go, internal/session/migration_test.go, internal/session/schema.go, internal/session/store.go, internal/session/store_test.go, internal/session/compact.go]
updated: 2026-10-14
---

Historical session databases are built from test-owned literal DDL increments1–24,
frozen from260ccd2, plus handwritten rows containing only that version's columns.
`TestHistoricalSchemaFixturesStayFrozen` pins their count and combined byte digest.
Do not build an old fixture through current `Open`, `migrate` or `schemaV*` constants:
that can make a broken migration redefine the database it is supposed to repair.
When adding a schema version, capture the previous released shape independently,
then deliberately update the frozen fixture and its digest.

The real migration owner opens each legacy file, checks saved authority and defaults,
then writes through today's store. Source-binding, compact/backup, rollback and
newer-version refusal tests remain separate storage contracts.

Production `Open(root)` has no public fixture options. Tests set the private clock and
ID generator only after a real open; compaction retains its private store options.
This does not replace SQLite, fsync, migration or lock behavior with test doubles.

## Changelog
- 2026-10-14 — separated historical fixture DDL from production constants; a renamed
  production migration column fails the v1 owner and exact restoration passes. Removed
  test-only public clock/ID options after checking all callers and private compaction.
