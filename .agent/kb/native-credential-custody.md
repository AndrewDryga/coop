---
name: native-credential-custody
description: canonical credential custody is read plainly under a flock; a login only the pre-v11 folders hold is refused by name; launch mounts that overlap protected state are refused
subsystem: credentials
sources: [internal/box/account_authority.go, internal/box/account_renewal.go, internal/box/authority_mounts.go, internal/box/native_account.go]
updated: 2026-11-17
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

A launch's mount sources are checked against Coop's protected host state (`authority_mounts.go`).
`resolveAuthorityPath` resolves each source, rebuilding only a missing tail and refusing an existing
dangling alias, and `authorityPathsOverlap` compares the results by path and by inode, so a mount
that reaches protected state through another name is refused even read-only. Unknown paths,
permission errors and symlink loops are errors, never read as harmless.

## Changelog
- 2026-11-17 — dropped the Docker listing and stopped-container paragraphs: 925253ae deleted
  `listDockerPatiently` and the container custody check. What remains is the flock, `lockAccountHomes`,
  `keepRenewal`, `refuseLegacyLogin` and the launch-mount overlap check. Sources updated.
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
