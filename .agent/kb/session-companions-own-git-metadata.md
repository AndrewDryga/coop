---
name: session-companions-own-git-metadata
description: companion boxes mount only the pinned workspace, so each snapshot must own usable Git metadata rather than point back to its source checkout
subsystem: sessions
sources: [internal/sessionsvc/companion.go, internal/sessionsvc/companion_snapshot.go, internal/sessionsvc/primary_snapshot.go, internal/forkspace/immutable_primary.go, internal/sessionsvc/companion_snapshot_gc.go, internal/sessionsvc/job_tree.go, internal/forkspace/git.go, internal/forkspace/lfs.go, internal/forkspace/lfs_status.go, internal/sessionsvc/service.go, internal/cli/fork_cmd.go, internal/box/run.go]
updated: 2026-11-04
---

# Session companions own their Git metadata

`companionRepositoryMounts` projects only the pinned companion workspace at
`/coop/repositories/<alias>` (internal/box/run.go); it deliberately does not expose the
operator's source checkout or Git configuration. A linked worktree therefore cannot function in
the box: its `.git` file points to an absolute worktree-admin directory under the unmounted source
repository.

Session ACP runs enter that box through `forkACP`, not the ordinary direct-run path. The session
service passes its frozen job bindings in `COOP_SESSION_COMPANIONS`; `forkACP` must parse that
trusted value and copy it into `RunSpec.CompanionRepositories` before launch. Keeping the handoff at
that host-only boundary prevents the ACP request surface from supplying mount paths.

`createSessionCompanionContext` copies the pinned commit's whole history with
`GitCopyPinnedHistoryContext`, as session submodule clones do. Its source is a trusted Git view of
the already verified private job cache. A fresh `git init --template=` repository runs
`pack-objects --revs --window=0` itself, reading the cache's objects through a one-command
`GIT_ALTERNATE_OBJECT_DIRECTORIES`, and keeps each object's stored form in one pack of its own;
`rev-list --objects` then proves the history whole without the source. Only the pin's history
crosses: no other branch, unreachable object, template, executable source configuration,
hardlinked object, persistent alternate or lazy network fetch. A shallow source, or one whose
object directory path holds the path separator that splits an alternate list, is cloned as
primary and review workspaces are (they keep the non-local clone for its remote-tracking refs),
then loses the clone's refs and remote. Creation detaches at the pin, removes reflogs, records
ownership and full-history markers, verifies, and atomically publishes the identity-pinned
staging directory. Missing objects fail closed.

The non-local clone it replaced searched objects for deltas again across the cache's packs and
re-hashed every object on receipt. On a tenant worker one 6.2 GB, two-pack history took 293 s of a
337 s companion preparation (`pack-objects` alone 167 s, `index-pack` alone 134 s); the copy
takes 34 s there, checkout 4.5 s. Git never retries a delta between two objects stored whole in
the same pack, so the regression fixture puts near-identical blobs in two packs. The former 1 GiB pack planner and shallow-creation fallback are gone, and verification accepts
only a `full` history marker.

Checkout and verification ignore host attribute files and executable Git drivers. Recursive
submodules come from the controller's exact authorized graph, never repository-supplied URLs.
`job_tree.go` copies each child into its own Git directory before publishing the workspace.
Primary, companion and review clones share this materialization path. A missing child, redirected
path, changed gitlink or dirty nested tree fails closed; parent status alone is insufficient proof
because the trusted Git view deliberately does not recurse into mutable child configuration.

Verification requires self-contained metadata, the exact persisted commit, mode-consistent history,
detached HEAD, a clean tree, no refs, remotes, reflogs, or host-backed object alternate, and the
commit-bound ownership marker before discard. Cleanliness
is computed with a private temporary Git directory, config, and index that reads only the verified
companion object database and worktree. It ignores submodule recursion but separately streams the
pinned index's gitlinks and independently verifies populated children's exact HEAD and cleanliness.
An empty submodule folder still counts as clean, because an agent can empty one in a running box.
A companion whose `.git` is a file, a linked worktree, is refused by verify and by
`removeSessionCompanion`, which never runs `git worktree remove` on a source repository.
New workspaces receive full LFS payloads and independently owned object storage at every recursive
level; clean companion replay rehydrates any pointers before exposing the workspace again.
The isolated check never enables repository filters; it treats a worktree
file as the committed representation only when a canonical LFS v1 pointer in the pinned index names
that exact byte count and SHA-256, the pinned attributes name the `lfs` filter, and the modes match.
Pointer and attribute reads use the private metadata, so source replacement refs and filter config
cannot change the verdict. Hashing observes request cancellation. Malformed pointers, changed bytes
or modes, non-regular files, and every other status record remain dirty. Shared streaming status
normalization removes verified LFS records before the caller's retained-output bound is applied.

Private-index commands must run directly with the freshly created host-owned metadata/config.
Passing their environment into `GitCommandWithEnv` would replace that index with the model's real
index: an inspection's `read-tree` would then overwrite staged work. Regression tests compare the
model's index bytes before and after inspection, including staged edits and hidden index flags.

Current companion creation first materializes a versioned immutable cache entry under
`<state>/companion-snapshots`. Its key includes the authenticated worker identity's public
controller/workspace namespace, the controller-scoped repository identity, selected commit and
submodule descriptors. Freshness timestamps stay in each source receipt and do not duplicate
content. A missing enrollment identity uses the local state-root namespace for local sessions.
A worker identity file from another controller is refused by the connector before enrollment;
explicit re-enrollment also changes this cache namespace.

The cache is copied independently from the mutable source with the checks above. Per-session
projections share only sealed working-tree, object and LFS payload inodes. Each owns its index,
HEAD, config and refs, and every model/service mount remains read-only. Writable primary forks
never use these links. Session paths and frozen network authority remain unchanged. The kernel's
hardlinks are durable references: cache eviction cannot remove bytes a session still holds, and
closing one session only releases its own references. Private cache projection stages are never
mounted; reuse/GC removes an interrupted stage under the same snapshot lock.

Cache GC keeps referenced or unproven entries, considers unused entries after an hour and evicts
unreferenced entries sooner under pressure or above the disposable-byte budget. A directory-bound
versioned receipt plus a durable eviction marker makes interrupted deletion resumable. GC atomically
retires the authenticated directory out of the reusable key namespace and syncs its parent before
removing payloads. It removes ownership last; a crash after that can leave only an empty directory,
which nonrecursive removal can reclaim without authority over unknown contents. An individual
entry whose ownership/cleanliness proof exceeds the 30-second pass remains retained and reported;
the rotating cursor prevents it from starving other entries. Session history and old independent companion copies keep their existing custody and cleanup rules.

Frozen read-only controller primary forks now use the same immutable cache/projection machinery
under a distinct materialization key. Their root set includes selected, admitted default and base
commits; private metadata restores their fork branch, origin and admitted tracking refs without
checkout or reset. Writable primaries still use independent clones. Task binding, checkpoint,
restore and publication already refuse readonly sessions; review rebases an independent scratch.
A versioned, inode-bound primary marker is durable before publication. Retries of a marked fork
verify only; historical independent copies keep their hydration path. Missing provenance cannot
adopt shared payloads as an independent copy (the separately proven generation anchor is excluded).
The marker denies native mutation and actual writable mounts even before generation/reservation
publication, including ancestor aliases and child paths. Unknown marker versions remain protected
through discard planning, revalidation, staging and purge. Stopped-worker checks inventory them.
No filesystem reflink support is required: private state on both Btrfs and ext4 shares actual inodes,
and the existing allocated-block accountant counts each payload once.

## Changelog
- 2026-11-04 — linked companions and bounded, shallow or missing history markers are refused now; the owner accepted a code proof and every store on this Mac had none. The empty-submodule check stays, since a running box can produce one.
- 2026-11-04 — corrected the linked-snapshot line: nothing reaches one, so it is not a rollout path.
- 2026-11-03 — extend immutable payload sharing to frozen read-only primaries; close unreserved native-launch and unknown-receipt cleanup gaps.
- 2026-11-03 — share verified immutable companion payloads while keeping per-session metadata, mounts and custody; bounded cache reclamation uses durable filesystem references.
- 2026-10-17 — dropped the run.go line numbers after `companionRepositoryMounts`, which the sentence
  already names (re-verified its `/coop/repositories/<alias>` read-only bind in
  internal/box/run.go).
- 2026-10-17 — replaced the companion's and session submodules' non-local clone with a pinned
  history copy after a tenant's Work sessions waited six minutes for one 6.2 GB companion. Tests
  cover the two-pack delta regression, pin-only ownership with no file written into the source,
  the shallow boundary, a source path holding the separator, and refusal of a missing object.
- 2026-09-27 — added verified offline LFS hydration and clean replay, shared streaming status,
  and private-index non-mutation proof. Full binaries, zero-size files, hostile filters and nested
  companion/review trees are covered; unsupported pointer forms fail explicitly, not silently.
- 2026-09-27 — replaced duplicate companion pack planning with the shared self-contained pinned
  clone; retained historical read-only verification. Added recursive source materialization and
  dirty-child review/checkpoint/discard refusal. Focused tests prove full nested primary/companion
  trees, review-gate access, mutation detection and preservation after a stale discard.
- 2026-08-11 — created after a live Responder companion mount exposed a host-only linked-worktree pointer; verified the mount, staged creation, object materialization, and discard checks against both sources
- 2026-08-11 — bounded repositories above 1 GiB to the pinned commit and tree after live deployment showed reachable-history packing could run for more than ten minutes; explicit operator approval retained full history for smaller repositories
- 2026-08-11 — replaced packed disk usage with no-lazy-fetch logical sizing after external deltas and partial promisor history proved the original boundary could undercount or hydrate unbounded data; reverified creation and verification ranges
- 2026-08-11 — isolated companion Git commands from host global and system configuration after a live Blitz checkout invoked Git LFS against a remote-less snapshot; added a hostile-filter regression and reverified code ranges
- 2026-08-11 — moved cleanliness checks into an isolated temporary Git repository and disabled host attribute files during checkout after review found mutable local config, info attributes, and operator attributes could execute filters or rewrite bytes
- 2026-08-11 — retained gitlink cleanliness without submodule recursion after rules review found that ignoring submodules could otherwise hide populated or deleted submodule paths during discard
- 2026-08-11 — documented and regression-tested the fork-ACP environment-to-RunSpec handoff after live repository-set sessions received companion metadata but launched boxes without the mounts
- 2026-08-18 — reverified discard against a live legacy blitz-flutter companion; recorded the cryptographic LFS-smudge equivalence that lets old clean snapshots retire without executing their filters
