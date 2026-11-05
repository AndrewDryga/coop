---
name: native-credential-custody
description: canonical credential custody is read plainly under a flock; a login only the pre-v11 folders hold is refused by name; Docker listings are patient and fail closed
subsystem: credentials
sources: [internal/box/account_authority.go, internal/box/authority_mounts.go, internal/box/native_account.go, internal/box/docker_listing.go, internal/box/docker_listing_test.go, internal/runtime/mounts.go, internal/runtime/mounts_test.go]
updated: 2026-11-04
---

Canonical credential custody (authority.json, the renewal journal) sits in folders no box mounts,
so it is read plainly (`config.ReadRecord`) under a plain flock, with no per-file owner, link or
inode checks. Two rules keep the plain flock sound: each folder of a custody path is refused as a
link (`ConfigDir/<provider>` was a box mount before 2026-07), and `coop credentials rm` deletes an
account's native homes, custody lock files and all, only under `lockAccountHomes`, which launches
hold shared while they work in one. The renewal journal's kept answer is the one record still synced
to disk (`keepRenewal`): it can be the only copy of a single-use grant.

A login only the old `profiles/<account>` or `host-credentials/<account>` folders hold is not read
or moved (`refuseLegacyLogin`): the launch, `coop usage` and `coop credentials` name
`coop login <provider>@<account>`, and the old folders are left alone.

Docker bind inventory freezes the endpoint and rechecks daemon identity after enumeration,
around each inspect batch and at return, including empty results. Full IDs are inspected in
32-container streams containing only ID, lifecycle status and mounts, each record bounded to
the existing 1 MiB mount limit. Missing/duplicate/mismatched/malformed/overflowed records,
failed commands or cancellation return no partial public inventory. Read-only and stopped
containers remain visible. `BindMount.Stopped` is true only when every observed user of a
source is created/exited; any live observation prevents that conclusion across batches.

Docker is shared, so the inventory and the label check around it (`listDockerPatiently`) ask up to
three times within 45 seconds. A pass can fail on load alone or on a container removed between `ps`
and `inspect`. A launch runs several of these checks, and a slow listing delays every one of them.
Only a complete pass counts. If none completes the launch refuses, and a failed listing is never
read as an empty one. `safeModelProbeCause` finds that refusal by its text, so change them together.

An unrelated stopped container may name a removed temporary directory. Only confirmed
created/exited sources may resolve missing tails from their nearest existing ancestor.
`resolveAuthorityPath` refuses existing dangling aliases before ascending; native checks use
inode-aware `authorityPathsOverlap`. Missing credential descendants still overlap. Unresolved
live/paused/restarting/dead binds refuse: they can retain a deleted alias's inode. Unknown
paths, permissions and symlink loops are never inferred harmless or dropped from inventory.

## Changelog
- 2026-11-04 — removed the pre-v11 cutover (writer fence, legacy writer locks, the Gemini encrypted
  cache refusal, the stopped-container and VM-share mount checks that only the fence used); renamed
  from native-credential-cutover. Docker listing patience stays.
- 2026-11-04 — a slow or churning Docker no longer refuses a launch after 5 seconds; both refusals
  say what happened in plain words. 47 containers list in 0.15 seconds on a quiet host.
- 2026-11-03 — a stopped container's `/mnt/mac/...` or `/host_mnt/...` source is mapped to the
  host path before the overlap check; it used to pass as an unrelated missing path. `/proc` was
  considered and kept refused: its `/proc/<pid>/root` links reach the VM's view of host folders.
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
