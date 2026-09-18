---
name: identity-fences-compare-the-inode
description: a persisted "same directory" fence records the device but compares only the inode — a reboot renumbers volumes, and five fences broke on it; a source guard now fails any new device comparison
subsystem: tasks
sources: [internal/tasks/identity.go, internal/tasks/completion.go, internal/tasks/lease.go, internal/networkstate/authority.go, internal/networkstate/artifact.go, internal/forkspace/generation.go, internal/sessionsvc/workspace.go, internal/device_fence_test.go]
updated: 2026-09-18
---

Coop fences many things to "the exact directory that was there": a task folder an owner claimed, a
completion a window or review captured, a project a network approval covers, a run's artifact
directory, a fork's workspace, a session's discard plan. Each records the directory's device and
inode. **Compare only the inode.** A volume gets its device number when it is mounted — macOS for
every APFS volume, Linux for dm/LVM, btrfs subvolumes, overlay and network mounts — so a reboot
changes it while the folder, its inode and its contents are untouched. Every fence that compared the
device broke at the host's next reboot: tasks could not be closed (6d92825a), completion windows
called untouched archives mutations and pending reviews lost their receipt (f554c94e), approved
projects looked "replaced" and post-reboot recovery refused to clean a run up (1f8df1b7), forks
stopped opening and session discard plans went stale (this card's commit).

The fence keeps its teeth without the device: a directory recreated at the path is a new inode, a
file-level copy is a new inode, and the durable ids (TaskID, generation) catch recreation. The
device only ever added "an identical inode on another volume", which takes mounting a filesystem at
the path — an actor who could already rewrite the state being fenced.

How it is held: `TaskGeneration.SameInstanceAs` (task instances compare through
`sameTaskInstance`), `CompletionFingerprint.Matches`/`sameArchive` (non-comparable: its live walk
forbids `==`), `sessionWorkspaceIdentity.sameDirectory` (non-comparable by a blank `[0]func()` field)
and inode-only checks in `checkDirectory`, `openPrivateDirectory` and the fork generation.
`TestIdentityFencesNeverCompareTheDevice` (internal/device_fence_test.go) parses internal/ and fails
on any `==`/`!=` between a device value (named `Dev`/`dev` or ending in "device") and a non-literal,
except its two allowlisted, reasoned cases — a record's own fields captured together, and two live
stats taken together — and fails on a stale entry. It cannot see a device inside a struct compare or
a reflect.DeepEqual; the records that still compare whole task instances with `==` compare two copies
of one capture, which is why TaskGeneration stays comparable. Keep recording the device — it is
diagnosis, and old completion trees hashed it (see [[canonical-fork-task-authority]]).

A digest that only lives inside one process (the network approval review MAC) may keep the device:
a reboot can only make it ask for a fresh review.

## Changelog
- 2026-09-18 — created with the last two fences (fork generation, session discard plans) and the
  source guard; swept internal/: after the five fixes, the guard finds only its two allowlisted cases.
