---
name: native-credential-cutover
description: existing native writer locks keep their inode during private normalization; uncertain legacy grants remain in custody
subsystem: credentials
sources: [internal/box/native_cutover.go, internal/box/account_authority.go, internal/box/native_writer_lock_test.go, internal/box/authority_mounts.go, internal/box/native_mount_paths_test.go, internal/runtime/mounts.go, internal/runtime/mounts_test.go, internal/agent/grok.go, internal/agent/gemini.go]
updated: 2026-11-04
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

On Docker Desktop and OrbStack the daemon's bind sources are VM paths. `hostlessKernelMount`
skips only the exact roots `/sys`, `/sys/fs/cgroup` and `/sys/fs/bpf`, and only on a host that has
no `/sys`: a path below a root can reach another filesystem (bpffs links, new mount points).
`/proc` stays refused even as a root: through `/proc/<pid>/root` a container allowed to inspect the
VM's processes (netdata adds the ptrace capability) reaches every filesystem the VM sees, its view
of host folders included. Trees of ordinary files (`/lib/modules`, `/var/lib/docker`, `/run` and
its Docker socket) stay refused when a running container binds them: they can hold links or
mounts, and a named volume bound to a host folder is mounted under `/var/lib/docker`. A stopped
container's missing source under the VM's view of host folders (OrbStack's documented `/mnt/mac`,
Docker Desktop's `/host_mnt`) is judged as the host path it names (`hostShareSource`), without
asking which runtime runs: on a host without that view, the mapping can only refuse more. A
running one with such a source still refuses.

## Changelog
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
