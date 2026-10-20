---
name: native-credential-cutover
description: existing native writer locks keep their inode during private normalization; uncertain legacy grants remain in custody
subsystem: credentials
sources: [internal/box/native_cutover.go, internal/box/account_authority.go, internal/box/native_writer_lock_test.go, internal/box/authority_mounts.go, internal/box/native_mount_paths_test.go, internal/runtime/mounts.go, internal/runtime/mounts_test.go, internal/agent/grok.go, internal/agent/gemini.go]
updated: 2026-10-20
---

`fenceLegacyAccount` holds the account lease, inventories existing credential mounts and locks
the adapter-declared native writer files before retiring legacy grants. Grok declares
`auth.json.lock`; its native PID file may legitimately have mode 0644.

Only a regular, current-user-owned, single-link writer-lock FD is eligible for normalization.
Coop acquires the nonblocking exclusive flock first, rechecks named root/inode identity and
metadata, then tightens that same inode to 0600 (`legacyAccountFence.lockWriter`). A busy,
replaced, linked, foreign-owned or nonregular lock refuses without chmod. Replacing the lock
would strand a native writer on another inode. Lock bytes do not change.

Writer-lock fixtures set their intended permissions with `Chmod` after creation: `WriteFile`
and `Mkfifo` modes are filtered by the inherited umask. Otherwise a restrictive umask makes the
0644 normalization case start private and makes unchanged unsafe locks look modified.

This exception is for adapter-declared legacy writer locks only, which live in a legacy profile
folder a box could reach. Canonical credential custody (authority.json, the renewal journal,
cutover.json) sits in folders no box mounts, so it is read plainly (`config.ReadRecord`) under a
plain flock, with no per-file owner, link or inode checks. Two rules keep the plain flock sound:
each folder of a custody path is refused as a link (`ConfigDir/<provider>` was a box mount before
2026-07), and `coop credentials rm` deletes an account's native homes, custody lock files and all,
only under `lockAccountHomes`, which launches hold shared while they work in one. The renewal
journal's kept answer is the one record still synced to disk (`keepRenewal`): it can be the only
copy of a single-use grant.
Unknown encrypted Gemini legacy caches have no proven storage identity and are retained
unchanged for explicit host recovery; presence in `coop credentials` is not cutover readiness.

Docker bind inventory freezes the endpoint and rechecks daemon identity after enumeration,
around each inspect batch and at return, including empty results. Full IDs are inspected in
32-container streams containing only ID, lifecycle status and mounts, each record bounded to
the existing 1 MiB mount limit. Missing/duplicate/mismatched/malformed/overflowed records,
failed commands or cancellation return no partial public inventory. Read-only and stopped
containers remain visible. `BindMount.Stopped` is true only when every observed user of a
source is created/exited; any live observation prevents that conclusion across batches.

An unrelated stopped container may name a removed temporary directory. Only confirmed
created/exited sources may resolve missing tails from their nearest existing ancestor.
`resolveAuthorityPath` refuses existing dangling aliases before ascending; native checks use
inode-aware `authorityPathsOverlap`. Missing credential descendants still overlap. Unresolved
live/paused/restarting/dead binds refuse: they can retain a deleted alias's inode. Unknown
paths, permissions and symlink loops are never inferred harmless or dropped from inventory.

## Changelog
- 2026-10-20 — made writer-lock fixture modes explicit, including same-inode normalization,
  refusal without chmod and replacement custody; verified under umask 077 and 022.
- 2026-10-18 — canonical custody is read plainly under a plain flock (task
  2026-10-15-read-coop-s-host-only-records-with-one-plain-hel); the legacy writer-lock checks stay.
- 2026-10-09 — traced the real Grok startup refusal to an owned 0644 PID lock; verified
  busy/replacement/link/type/ownership refusal and same-inode/byte normalization in tests.
- 2026-10-09 — replaced serial container inspection with complete bounded batches and retained
  lifecycle evidence, including mixed-state duplicate sources and mid-pass daemon replacement.
- 2026-10-09 — traced remaining native startup refusal to nine removed source paths; retained
  stopped-only missing-tail comparison and live/alias/permission/credential overlap refusals.
