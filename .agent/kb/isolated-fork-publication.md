---
name: isolated-fork-publication
description: optional independent execution, exact private review, semantic parent preconditions and durable forward-only publication
subsystem: forkspace
sources: [internal/cli/fork_isolated.go, internal/forkspace/generation.go, internal/forkspace/state.go, internal/forkspace/isolated.go, internal/forkspace/isolated_admission_test.go, internal/forkspace/gitobserve.go, internal/forkspace/gitview_config.go, internal/forkspace/independent_tree.go, internal/forkspace/lfs.go, internal/forkspace/lfs_storage.go, internal/box/isolated_launch.go, internal/box/isolated_services.go, internal/forkctl/isolated_candidate.go, internal/forkctl/isolated_parent.go, internal/forkctl/isolated_land.go, internal/forkctl/isolated_commands.go, internal/forkctl/isolated_dirty.go, internal/forkctl/testhelpers_test.go]
updated: 2026-11-06
---

`--isolated` is opt-in; ordinary shared-write behavior stays trusted collaboration. Host-private
v4 generation/anchor names bind mode and initial base. Re-entry cannot downgrade or upgrade an
ordinary generation. Strong host Git admission refuses missing/redirected/shared metadata before
operational view recovery, including calls from nested directories.

An enumerated optional metadata entry can disappear before its Info read when Git removes a
maintenance lock. Admission retries the entire validation at most three times, never skips the
entry or exempts lock files. Each attempt rechecks redirectors, required real roots and ownership.
Root/traversal/permission failures are immediate; persistent churn refuses. A complete successful
pass is not an atomic snapshot: callers still fence writers. Raw publication fixture commits
disable automatic maintenance, as trusted production Git commands already do.

Execution owns objects, initialized unchanged submodules and verified offline LFS payloads.
Parent checkout/admin/common paths cannot enter through aliases, shared inodes, volume backing
paths or services. Operator/service binds and existing volumes require inspectable contents: a daemon-only
path absent on the host is unknown, not an empty volume. Exact creator-owned private volumes retain
their explicit exception. Host-selected shared writable homes/data and outside parent writers remain
trusted extensions, not lifetime independence established by a prelaunch scan.
Ordinary selected cache/asdf volumes use a distinct shared-content trust grant, only after plain
local/no-options daemon provenance; never infer ownership from their names. Parent/private aliases
are still checked, and host-readable contents still get the IPC/inode scan. Opaque shared contents
remain trusted extensions; operator/service mounts do not acquire this waiver.

Fresh `ObserveGit` snapshots cannot reconcile cached HEAD or refresh the real index. Source
clone/fetch uses fresh views too. Operational views are **not read-only**: their cached newer
HEAD can recover into the real store. Isolated review uses immutable private custody and a
separate disposable gate carrying the captured parent ref and original mount boundary. Inspect
every incoming commit; `--force` cannot bypass risk/nested-Git/automatic-surface policy.

v2 journals retain all strong lands, even non-task ones. Parent binding is semantic: canonical
checkout/admin/common paths, target, HEAD/tree, logical index and tracked bytes. Reject hidden
index flags, introduced-path collisions and uncertain locks. A refusal that leaves the parent as it
was comes before the journal exists: mergeIsolatedLocked checks path collisions before the question
and the box checks, and advanceIsolatedLand writes the journal itself, under the ref lock, once the
collision and parent checks pass and just before the object import. Such a refusal leaves no journal,
so the fork shows no merge and the next merge asks and checks again. A land that starts from a
journal keeps it on any refusal. Sync custody and journal before object import; read-tree updates
files/index, verified LFS hydration follows, then target-ref CAS.
Path/mode-framed raw byte checksums include symlink targets and recursive child semantic state.
Persist the verified publication checksum before CAS; both confirmation and coherent replay require
it. LF/CRLF and LFS pointer/payload equivalence never erase external physical byte changes.
An exact fully hydrated no-effect publication binds the original checksum; pointer-only LFS parents
take the normal journaled hydration path, even without new commits. Missing proof refuses recovery.
This is roll-forward, not atomic unchanged-parent publication. Only exact unchanged or coherent
published state replays; partial/foreign states retain custody and never reset/stash/delete locks.

Task authority always binds original source HEAD/tree. Kept-fork boundaries record that original
HEAD, not rebased publication. The private rebase replays only source after that boundary and
compares it only with parent commits since the last publication, through two throwaway merges: the
upstream joins the boundary and the parent HEAD, the rebased branch joins the source and the
publication head (creation base before any). A source commit the parent already has (a person's
cherry-pick) is skipped rather than conflicting with later parent edits, while one that redoes a
change the parent reverted is not matched against the parent's copy of published source. The source receipt, version 2 only, also binds publication HEAD/tree;
only its presence in current parent ancestry qualifies a privately rewritten source for removal/recreation.
Pending/malformed journals refuse removal; direct original-source ancestry remains sufficient.
Coherent replay verifies hydrated files/cache before finalization. Tracked-byte comparison also
qualifies Git's native checkout conversions through a fresh non-executing observation, so trusted
CRLF attributes do not fail only after read-tree has already changed the parent.
Checkout-only views project safe system/global built-in conversion values before local settings;
trusted drivers, includes themselves and external attribute paths never enter the execution config.
Native default LFS stores include common metadata for linked worktrees/gitfiles; custom storage
is explicitly refused before publication. Private attribute enumeration builds a pinned index.
GUI/custom review previews are retained and never merge input; direct `fork open` is untrusted.
Candidates, previews and the source receipt are named by the fork's generation
(forkspace.IsolatedRecordPrefix), so deleting the fork sweeps exactly its own, and its log, in
forkspace.Destroy and orphan recovery, never without a valid generation. The lock file stays: flock
locks the file, so removing it under a waiting command would let two commands hold one fork's lock.
Refusals are the shared block (`ui.CommandFailed`): `isolatedRefusal` for the fork's commits, which
always says `--force` cannot bypass the check, and `parentRefusal` for a detached, locked or dirty
parent (`parentDirtyError`: staged or changed files, conflicts, assume-unchanged and skip-worktree
marks, a submodule's own), which adds that the journal is kept when the land came from one. A dirty
parent met from a journal keeps the recovery text instead, since it may be that publication's own
half-made state and no one should be told to commit or stash it. Wrapping such an error hides its
text, since the dispatcher draws only the block.
Custom tools run at the captured base comparison while COOP_FORK_PATH points to a separate exact
published tree, preserving HEAD...COOP_REVIEW_REF without handing over the model workspace.

## Changelog
- 2026-11-06 — a parent with uncommitted changes, conflicts or hidden index flags is refused in the
  same block, naming the files and the exact command that clears them (task
  2026-11-06-put-the-isolated-fork-s-dirty-parent-refusals-in).
- 2026-11-06 — isolated refusals became blocks that name the cause and the next step, and `fork ls`
  marks isolation (task 2026-11-06-word-isolated-fork-refusals-and-previews-plainly).
- 2026-11-06 — a merge refused before it changed the parent (an untracked file in the way) left its
  journal, so the fork showed as merging and the next merge finished it unasked. The journal is now
  written just before the first change to the parent, and deleting a fork removes its previews,
  source receipt and log (task 2026-11-06-stop-a-refused-isolated-fork-merge-from-staying).
- 2026-11-06 — the private rebase skips source commits the parent already has, so a fork whose
  task a person took with `coop tasks done --from-fork` still merges cleanly (task
  2026-11-05-let-the-owner-complete-a-fork-held-task-whose-co).
- 2026-11-04 — the source receipt reader dropped version 1, which no commit wrote; a stray one fails
  closed on both the boundary and the removal path.
- 2026-10-09 — Git 2.55 release CI lost a detached maintenance lock between enumeration and Info.
  Added bounded complete revalidation with disappearance, new-unsafe-metadata, required-root,
  permission and persistent-churn regressions. Pinned fixture auto-maintenance; real Git trace
  proves no maintenance child. One-pass admission and unpinned fixture negative controls fail.
- 2026-10-08 — created from independent admission/publication implementation and failure tests.
- 2026-10-08 — real custom-tool reproduction caught base files masquerading as the publication
  preview; separate independent retained copies now qualify proposed bytes and comparison identity.
- 2026-10-08 — board regressions proved native CRLF post-checkout rejection, false kept-source
  removal refusal and daemon-only volume IPC exposure; qualify native bytes, retain exact publication
  receipts and refuse unobservable existing-volume contents.
- 2026-10-08 — reproduced dropped source cutoffs after parent resets, default-cache refusal,
  global-only CRLF settings and byte-equivalent external edits across CAS/replay. Bound cutoffs to
  current publication ancestry, distinguished selected shared cache trust, projected safe effective
  EOL data and journaled exact original/publication byte digests with denial/no-effect regressions.
