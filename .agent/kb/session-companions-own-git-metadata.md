---
name: session-companions-own-git-metadata
description: companion boxes mount only the pinned workspace, so each snapshot must own usable Git metadata rather than point back to its source checkout
subsystem: sessions
sources: [internal/sessionsvc/companion.go, internal/cli/fork_cmd.go, internal/box/run.go]
updated: 2026-08-11
---

# Session companions own their Git metadata

`companionRepositoryMounts` projects only the pinned companion workspace at
`/coop/repositories/<alias>` (`internal/box/run.go:724-727`); it deliberately does not expose the
operator's source checkout or Git configuration. A linked worktree therefore cannot function in
the box: its `.git` file points to an absolute worktree-admin directory under the unmounted source
repository.

Session ACP runs enter that box through `forkACP`, not the ordinary direct-run path. The session
service passes its policy-owned bindings in `COOP_SESSION_COMPANIONS`; `forkACP` must parse that
trusted value and copy it into `RunSpec.CompanionRepositories` before launch. Keeping the handoff at
that host-only boundary prevents the ACP request surface from supplying mount paths.

`createSessionCompanion` builds an empty repository in an identity-pinned private staging directory.
It enumerates reachable object IDs without lazy fetches, then sums their logical sizes through
`cat-file --batch-check`, stopping at 1 GiB. If any historical object is unavailable locally or the
sum exceeds the bound, the trusted source writes only the pinned commit and complete tree and marks
the commit as a shallow boundary. Otherwise it writes the complete history pack. Coop records the
selected mode, detaches at the pin, removes its reflog, verifies, and atomically publishes the
snapshot (`internal/sessionsvc/companion.go:101-448`). The bound exists because a production source
with years of history turned session creation into a multi-gigabyte pack that still had not
completed after ten minutes. Companion commands ignore host global and system Git configuration and
never lazy-fetch from repository configuration: after Coop's trusted policy-owned remote refresh,
the pinned commit and complete tree must already be local or session creation fails closed. This
also prevents `.gitattributes` from invoking a host-configured LFS or custom filter during checkout
and verification. Checkout ignores host attribute files as well, so operator-level normalization
cannot rewrite snapshot bytes.

Verification requires self-contained metadata, the exact persisted commit, mode-consistent history,
detached HEAD, a clean tree, no refs, remotes, reflogs, or host-backed object alternate, and the
commit-bound ownership marker before discard (`internal/sessionsvc/companion.go:470-825`). Cleanliness
is computed with a private temporary Git directory, config, and index that reads only the verified
companion object database and worktree. It ignores submodule recursion but separately streams the
pinned index's gitlinks and requires each placeholder to remain a real empty directory. Legacy
linked snapshots remain recognizable only so sessions created by older Coop binaries can be safely
discarded during rollout.

## Changelog
- 2026-08-11 — created after a live Responder companion mount exposed a host-only linked-worktree pointer; verified the mount, staged creation, object materialization, and discard checks against both sources
- 2026-08-11 — bounded repositories above 1 GiB to the pinned commit and tree after live deployment showed reachable-history packing could run for more than ten minutes; explicit operator approval retained full history for smaller repositories
- 2026-08-11 — replaced packed disk usage with no-lazy-fetch logical sizing after external deltas and partial promisor history proved the original boundary could undercount or hydrate unbounded data; reverified creation and verification ranges
- 2026-08-11 — isolated companion Git commands from host global and system configuration after a live Blitz checkout invoked Git LFS against a remote-less snapshot; added a hostile-filter regression and reverified code ranges
- 2026-08-11 — moved cleanliness checks into an isolated temporary Git repository and disabled host attribute files during checkout after review found mutable local config, info attributes, and operator attributes could execute filters or rewrite bytes
- 2026-08-11 — retained gitlink cleanliness without submodule recursion after rules review found that ignoring submodules could otherwise hide populated or deleted submodule paths during discard
- 2026-08-11 — documented and regression-tested the fork-ACP environment-to-RunSpec handoff after live repository-set sessions received companion metadata but launched boxes without the mounts
