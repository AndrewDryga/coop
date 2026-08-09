---
name: task-authority-registry-is-durable-state
description: host-global completion trust lives in ~/.local/state/coop/task-leases, never a cache dir; adoption off the old cache path is one-shot and every authority flock rechecks its inode
subsystem: tasks
sources: [internal/cli/tasklease.go, internal/cli/completionwindow.go, internal/cli/controller.go, internal/sessionsvc/http.go]
updated: 2026-08-09
---
Everything that decides whether a task is *really* finished lives OUTSIDE the repo, in one
host-global registry: the `<sha>.lock` files whose kernel flock makes one controller the single
writer for a task (and which carry the completion receipt in their bytes), `<sha>.json` lease
metadata, `<sha>.reopen.json` audit-reopen authority, `<sha>.departure.json` trusted-done
departures, and `<sha>.windows.json`, the completion-window journal crash recovery replays.
Filenames are `sha256(resolved-repo-queue-root + "\x00" + task-id)`, so the registry is
location-independent: moving the registry itself never renames a record.

**It must not be a cache dir.** It was `os.UserCacheDir()/coop/task-leases/v1` until 2026-08-09.
A cache is OS-deletable by contract (macOS purges `~/Library/Caches` under pressure; cleaners empty
it), and a purge breaks trust two ways: mid-run it unlinks a lock file whose fd is still flocked, so
the next `openLeaseAuthorityRecord` recreates the name as a NEW inode and two controllers each hold
an "exclusive" lock on a different one — silently; between runs it erases receipts and reopen
authority, degrading crash recovery to restore-and-redo. `leaseAuthorityRoots`
(`internal/cli/tasklease.go`) now resolves `$HOME/.local/state/coop/task-leases/v1` on both darwin
and linux, the same durable family as the session store's `defaultSessionStateRoot`
(`internal/sessionsvc/http.go`) — NOT XDG-configurable, matching that precedent exactly.

**Adoption is one-shot, never a fallback reader.** `adoptLegacyLeaseAuthorityRoot` runs only when
the durable root is absent and the cache root is present, under a blocking flock on
`.adopt.lock` in the new root's parent, re-proving both conditions after it gets the lock. Same
volume: whole-dir `rename(2)`. Cross-volume (EXDEV): per-record fsync-then-rename into a
`.v1.adopting` staging dir which is itself renamed into place, then the cache root is removed — so a
crash leaves either the untouched cache root or a COMPLETE durable root, never half a registry.
Non-regular and dot-prefixed entries are skipped: they are not records. The trap when upgrading: a
process ALREADY RUNNING the old binary holds its locks on file descriptors, not paths, so it keeps
the old inodes and is not mutually exclusive with a new-binary process until it exits. Let in-flight
runs finish before upgrading.

**Every authority flock is proved after the fact.** Take authority locks through
`lockLeaseAuthority` / `lockLeaseAuthorityForAudit` (`tasklease.go`), never a bare `syscall.Flock`
on an `openLeaseAuthority` result: they open → lock → `fstat(fd)` vs a fresh `lstat(name)` → on a
mismatch drop the lock and retry on the inode that answers the name now, bounded by
`leaseAuthorityIdentityAttempts`, then `errLeaseAuthorityIdentity`. flock binds a process to an
INODE, never to a name; without the recheck a deleted-underfoot lock is silently non-exclusive.
`openLeaseAuthority` survives for unlocked reads and tests only. See [[task-state-is-the-folder]]
for the repo-local queue, which did NOT move.

## Changelog
- 2026-08-09 — sources repointed: the sessions service moved out of `internal/cli/session_*.go` into `internal/sessionsvc/`; the facts here are unchanged (a move-only extraction).
- 2026-08-09 — created: registry moved out of `os.UserCacheDir()` to the session store's state-root family, with one-shot locked adoption and a post-flock inode identity recheck at every authority lock site.
