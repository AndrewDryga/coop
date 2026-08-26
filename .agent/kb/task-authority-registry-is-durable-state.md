---
name: task-authority-registry-is-durable-state
description: host-global completion trust lives in ~/.local/state/coop/task-leases, never a cache dir; v9 refuses populated retired state and every authority flock rechecks its inode
subsystem: tasks
sources: [internal/tasks/lease.go, internal/tasks/completion.go, internal/tasks/audit.go, internal/sessionsvc/http.go]
updated: 2026-08-26
---
Everything that decides whether a task is *really* finished lives OUTSIDE the repo, in one
host-global registry: the `<sha>.lock` files whose kernel flock makes one controller the single
writer for a task (and which carry the completion receipt in their bytes), `<sha>.json` lease
metadata, `<sha>.reopen.json` audit-reopen authority, `<sha>.departure.json` trusted-done
departures, and `<sha>.windows.json`, the completion-window journal crash recovery replays.
Filenames are `sha256(resolved-repo-queue-root + "\x00" + task-id)`, so the registry is
location-independent: moving the registry itself never renames a record.

The same registry is the sole iteration-lease store. Coop does not mirror its flock or heartbeat
into a task's provider-writable `tmp/`; observation and recovery consult only the host records.

**It must not be a cache dir.** It was `os.UserCacheDir()/coop/task-leases/v1` until 2026-08-09.
A cache is OS-deletable by contract (macOS purges `~/Library/Caches` under pressure; cleaners empty
it), and a purge breaks trust two ways: mid-run it unlinks a lock file whose fd is still flocked, so
the next `openLeaseAuthorityRecord` recreates the name as a NEW inode and two controllers each hold
an "exclusive" lock on a different one — silently; between runs it erases receipts and reopen
authority, degrading crash recovery to restore-and-redo. `leaseAuthorityRoot`
(`internal/tasks/lease.go`) now resolves `$HOME/.local/state/coop/task-leases/v1` on both darwin
and linux, the same durable family as the session store's `defaultSessionStateRoot`
(`internal/sessionsvc/http.go`) — NOT XDG-configurable, matching that precedent exactly.

**V9 does not adopt or read retired authority.** When the durable root exists,
`OpenLeaseAuthorityRoot` opens it without even resolving the old cache path. When it is absent, Coop
checks the retired path before creating anything: missing or an empty real directory is safe, while
any entry, path-shape anomaly, lookup failure, or read failure refuses without mutation. The guard
does not interpret, merge, move, copy, or delete old records. Stop every older Coop process first:
an old binary does not participate in the guard and can write the cache path after it was checked.
Move the whole registry using the crash-safe same-filesystem or staged cross-filesystem procedure in
`MIGRATING.md`; never expose a partial current `v1` directory, because once it exists it is the sole
authority and the old path is intentionally ignored.

**Every authority flock is proved after the fact.** Take authority locks through
`lockLeaseAuthority` / `lockLeaseAuthorityForAudit` (`internal/tasks/lease.go`), never a bare `syscall.Flock`
on an `openLeaseAuthority` result: they open → lock → `fstat(fd)` vs a fresh `lstat(name)` → on a
mismatch drop the lock and retry on the inode that answers the name now, bounded by
`leaseAuthorityIdentityAttempts`, then `errLeaseAuthorityIdentity`. flock binds a process to an
INODE, never to a name; without the recheck a deleted-underfoot lock is silently non-exclusive.
`openLeaseAuthority` survives for unlocked reads and tests only. See [[task-state-is-the-folder]]
for the repo-local queue, which did NOT move.

## Changelog
- 2026-08-26 — v9 removed automatic cache-root adoption; current state is resolved first, and a
  populated or unreadable retired root now refuses without mutation until an operator migrates the
  whole directory with all older processes stopped
- 2026-08-25 — made the registry the sole iteration-lease authority and heartbeat store; removed
  the provider-visible task-local lock/metadata mirror and its completion-audit fallback
- 2026-08-10 — sources repointed again: `tasklease.go`/`completionwindow.go`/`controller.go` moved
  out of `internal/cli` into `internal/tasks` (`lease.go`/`completion.go`/`audit.go`) whole — the
  2026-08 tasks/lease/completion-audit extraction. The registry's on-disk format and every fact here
  are unchanged (another move-only extraction); see [[task-authority-model]] for the full
  four-authority map this registry backs.
- 2026-08-09 — sources repointed: the sessions service moved out of `internal/cli/session_*.go` into `internal/sessionsvc/`; the facts here are unchanged (a move-only extraction).
- 2026-08-09 — created: registry moved out of `os.UserCacheDir()` to the session store's state-root family, with one-shot locked adoption and a post-flock inode identity recheck at every authority lock site.
