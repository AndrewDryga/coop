---
name: session-store-connection-pragmas
description: the session store's per-connection pragmas (busy_timeout, foreign_keys) ride in the DSN, because the driver silently replaces a connection whose call a context interrupted
subsystem: session
sources: [internal/session/store.go, internal/session/store_test.go]
updated: 2026-11-21
---

`session.Store` keeps one `database/sql` connection (`SetMaxOpenConns(1)`), but that connection is
not permanent. modernc.org/sqlite reports a connection unusable once a context interrupted its
call (`sqlite3_is_interrupted`), and `database/sql` then opens a fresh one. Any pragma that is
per-connection must therefore be part of the DSN, which `openStore` builds with `_pragma`
entries; a one-off `db.Exec("PRAGMA …")` at open only reaches the first connection.

Before 2026-11-21 `busy_timeout` and `foreign_keys` were set that way. One store call cancelled by
its context mid-statement (a short probe timeout under load was enough) left the store with
busy_timeout 0, so the next write behind another writer failed at once with
`database is locked (5) (SQLITE_BUSY)`, and with foreign keys off. `journal_mode=WAL` is a
property of the file, so it survives a reconnect. `TestInterruptedCallKeepsConnectionPragmas`
pins the contract.

The driver's busy handler counts SQLite's nominal retry delays, not wall time, so on a slow
machine a write waits at least `busyTimeout` (5 s) before giving up.

## Changelog
- 2026-11-21 — created after `TestTurnStartupCheckpointsSurviveWriterContention` failed under load; verified against modernc.org/sqlite v1.62.0 `conn.usable` and `busy.go`.
