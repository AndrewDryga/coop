---
name: identity-fences-compare-the-inode
description: inode and timestamp reuse is real; durable fork and network roots use private hardlink anchors, while tasks and destructive plans combine logical identity, semantic checks and live pins
subsystem: security
sources: [internal/fsidentity/anchor.go, internal/fsidentity/link_unix.go, internal/forkspace/generation.go, internal/networkstate/project_anchor.go, internal/networkstate/authority.go, internal/box/serviceanchor.go, internal/box/authority_mounts.go, internal/tasks/identity.go, internal/tasks/completion.go, internal/sessionsvc/workspace.go, internal/sessionsvc/service.go, internal/device_fence_test.go]
updated: 2026-09-23
---

Coop often needs to answer “is this still the object I authorized?” Linux overlayfs disproved the
old shortcut: in Cloud Shell, deleting and immediately recreating a directory reused its device,
inode, birth time, ctime, mtime, mount ID and link count. Neither another allocator field nor a
newer timestamp is durable identity.

Use the boundary's real authority instead of one universal tuple:

- **Fork generations, project network approvals, and service-file grants survive process restarts and authorize future
  sandbox access.** They use `fsidentity.Binding`: a `0600` marker inside the protected root is a
  hard link to a randomly named file in Coop's owner-private state. Validation opens both names
  without following links and requires the expected body, owner, mode, exactly two links and
  `os.SameFile`. The private link keeps that file inode allocated; copying the marker bytes cannot
  recreate the link. A missing, replaced or third-linked marker fails closed.
- **Canonical tasks use logical identity.** Random QueueID and TaskID values travel through owner,
  assignment, projection, candidate and land records. The folder inode remains a useful
  replacement signal, not standalone authority; ordinary recreation gets a new TaskID even if the
  allocator immediately reuses its inode. Mutations also hold the task authority lock and use
  rooted or pinned handles. Completion fingerprints add ctime, a complete tree digest and receipt
  state.
- **Remote-session discard plans are semantic snapshots.** They bind the session revision and
  anchored fork generation/reservation, then compare branch, HEAD, status, dirty/unmerged consent
  and a live pinned directory immediately before staging deletion. Their recorded inode is one
  stale-plan signal, not the proof by itself. Cleanup can replay either exact crash prefix after
  the workspace is gone: generation still present/reservation gone, or generation gone/reservation
  still present. A replacement of either record is still stale, never cleanup permission.
- **Live locks and scans may compare inode metadata.** A held file descriptor prevents that open
  inode from being recycled while a lock-name recheck runs. Storage hardlink accounting compares
  device+inode only during one live walk. Neither case persists a future authorization decision.

The hardlink marker creates one extra sandbox boundary. A box may mount the exact anchored project,
but not a source path below it: an existing writable box can swap that path after host validation
and before the runtime resolves the bind, even when the resulting mount is read-only. It may not
mount a writable ancestor that could rename the project and transplant the marker, and it may never
see any ancestor or descendant of the private anchor state—even read-only. Named volumes are
inspected for the same relationships; opaque inherited volumes and custom volume drivers are
refused. Prospective network, fork, execution-registry and launch-lock paths are protected before
they exist. Filtered launches repeat the mount proof under their launch lock immediately before
creating the agent. The generated run files and run-private task volume are the only explicit
allowlist.

Marker and private anchor must be on one filesystem with hardlink support. Failure is actionable
and has no timestamp fallback. The markers are exact Git exclusions, but a manual `git clean -x`
can still remove them: network access then needs a fresh `coop approve`; a damaged fork remains
non-authoritative and may need deliberate removal. Coop's own checkpoint restore preserves the fork
marker.

Publication ordering is part of the identity contract. `fsidentity.Create` syncs the private name's
directory before linking and syncing the public marker, so a durable marker can never outlive its
only recovery name. A fork generation whose record rename became visible before a directory-sync
error keeps its anchor and workspace for retry. Retirement removes the anchor before unlinking its
record; session rollback and discard likewise retire exact generation authority before deleting the
workspace, and accept only exact prefix-completed retries.

Migration is explicit. Allocator-based network approvals load only so the UI can request a fresh
review; they never grant. A v1/v2 fork record migrates only while stopped, with no execution or
land intent, after the branch and absolute canonical origin are verified. Ordinary forks require
no reservation; a remote session may retain its exact same-generation, same-owner reservation
during startup migration so a valid session is not quarantined merely for upgrading. The record
is atomically replaced only after the anchor exists. Its Git exclude write also proves the real
project `.git` or linked-worktree backlink and refuses a repository-controlled `commondir` redirect.
Active or ambiguous legacy state is left untouched. A network review reuses an intact binding
without interrupting boxes; creating, replacing, or moving one re-reads policy while holding the
exclusive cross-box launch lock. Commit reuses only the reviewed exact binding.

Device numbers remain diagnostic and are not compared across reboots.
`TestIdentityFencesNeverCompareTheDevice` guards that rule. This design does not claim protection
from a malicious process already running as the host user: that actor can alter Coop's owner-private
records directly. The relevant attacker is repository/model code confined to the box.

## Changelog
- 2026-09-23 — included service approval's third hardlink authority and the exact-reservation
  exception for stopped legacy remote-session migration. Moved empty-policy network checkouts
  keep a pending review barrier rather than silently resolving to the default posture.
- 2026-09-23 — reproduced complete allocator-metadata reuse on Linux overlayfs; replaced the
  rejected birth-time design with private hardlink anchors for durable fork/network roots, retained
  logical/semantic/live-pin authority for tasks and session plans, added the prospective/revalidated
  mount boundary, and made publication, transition and cleanup crash-replay ordering explicit.
- 2026-09-18 — created after removing reboot-unstable device comparisons from persisted fences.
