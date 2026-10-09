---
name: isolated-fork-publication
description: optional independent execution, exact private review, semantic parent preconditions and durable forward-only publication
subsystem: forkspace
sources: [internal/cli/fork_isolated.go, internal/forkspace/generation.go, internal/forkspace/isolated.go, internal/forkspace/isolated_admission_test.go, internal/forkspace/gitobserve.go, internal/forkspace/gitview_config.go, internal/forkspace/independent_tree.go, internal/forkspace/lfs.go, internal/forkspace/lfs_storage.go, internal/box/isolated_launch.go, internal/box/isolated_services.go, internal/forkctl/isolated_candidate.go, internal/forkctl/isolated_parent.go, internal/forkctl/isolated_land.go, internal/forkctl/isolated_commands.go, internal/forkctl/testhelpers_test.go]
updated: 2026-10-09
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
index flags, introduced-path collisions and uncertain locks. Sync custody and journal before
object import; read-tree updates files/index, verified LFS hydration follows, then target-ref CAS.
Path/mode-framed raw byte checksums include symlink targets and recursive child semantic state.
Persist the verified publication checksum before CAS; both confirmation and coherent replay require
it. LF/CRLF and LFS pointer/payload equivalence never erase external physical byte changes.
An exact fully hydrated no-effect publication binds the original checksum; pointer-only LFS parents
take the normal journaled hydration path, even without new commits. Missing proof refuses recovery.
This is roll-forward, not atomic unchanged-parent publication. Only exact unchanged or coherent
published state replays; partial/foreign states retain custody and never reset/stash/delete locks.

Task authority always binds original source HEAD/tree. Kept-fork boundaries record that original
HEAD, not rebased publication. A v2 source receipt also binds publication HEAD/tree; only its
presence in current parent ancestry qualifies a privately rewritten source for removal/recreation.
Pending/malformed journals refuse removal; direct original-source ancestry remains sufficient.
Coherent replay verifies hydrated files/cache before finalization. Tracked-byte comparison also
qualifies Git's native checkout conversions through a fresh non-executing observation, so trusted
CRLF attributes do not fail only after read-tree has already changed the parent.
Checkout-only views project safe system/global built-in conversion values before local settings;
trusted drivers, includes themselves and external attribute paths never enter the execution config.
Native default LFS stores include common metadata for linked worktrees/gitfiles; custom storage
is explicitly refused before publication. Private attribute enumeration builds a pinned index.
GUI/custom review previews are retained and never merge input; direct `fork open` is untrusted.
Custom tools run at the captured base comparison while COOP_FORK_PATH points to a separate exact
published tree, preserving HEAD...COOP_REVIEW_REF without handing over the model workspace.

## Changelog
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
