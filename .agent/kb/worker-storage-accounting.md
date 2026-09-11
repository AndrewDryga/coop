---
name: worker-storage-accounting
description: the worker measures allocated blocks and charges a hardlinked baseline once, stages a discard by renaming before deleting, and closes allocation on used-byte watermarks with a free-byte reserve underneath
subsystem: worker
sources: [internal/forkspace/usage.go, internal/forkspace/discard_stage.go, internal/sessionsvc/storage.go, internal/sessionsvc/workspace.go, internal/workerproto/storage.go, internal/workerconnector/storage.go, docs/session-api.md]
updated: 2026-09-11
---

A worker accounts for its own disk so a control plane can bound workspace growth. Four things about
it are not obvious from the code.

**Only exclusive bytes are reclaimable.** `git clone` of a local path hardlinks the parent's object
files, so measuring each fork on its own charges the same baseline to every one of them and reports
a fork root far larger than the volume holds. `forkspace.UsageScan` measures a whole root under one
accounting: an inode with `Nlink > 1` is charged exactly once, to whichever tree reaches it first,
and lands in `SharedBytes`. `ExclusiveBytes()` — the single-link remainder — is what discarding
actually returns, so it is the number every category total uses
(`internal/sessionsvc/storage.go`, `StorageTotals`). The shared remainder is reported once as
`baseline_shared_bytes` and counted as retained, because the parent checkout keeps those links
alive. Measurement is Lstat only and reads no file body, which is what lets it account the private
per-session ACP state without touching its contents.

**A discard renames before it deletes.** `forkspace.StageWorkspaceDiscardLocked` moves the proven
inode into `<repo>-forks/.discarding/<name>.<16 hex>` — one atomic same-filesystem rename — before
anything is removed. Deleting in place was not resumable: a crash partway through left a directory
that still occupied the fork's name but had no `.git`, so every later plan refused it and its bytes
were stranded for good. After the rename the fork name is free and the remains are unambiguously
coop's garbage that `PurgeStagedDiscards` finishes without re-proving anything. The staging
directory is a SIBLING of the fork workspaces rather than a child of `.coop` so the accounting can
measure control state and mid-removal garbage as two separate trees. No discard reports success
until `confirmSessionWorkspaceRemoved` sees the bytes gone.

**The watermarks and the reserve measure different things.** `high_watermark_bytes` and
`low_watermark_bytes` are USED-byte levels; `reserve_bytes` is a FREE-byte floor. Allocation closes
at or above the high one and reopens only at or below the low one, so reclaiming a single workspace
cannot flap the gate. The defaults track the reserve (close under one reserve free, reopen at two)
rather than a "percent full" line on purpose: a worker does not own the whole volume, and refusing
every session because unrelated host data fills the disk is not this policy's job. An unreadable
statfs fails OPEN — turning a monitoring failure into a fleet outage is worse than missing one
refusal — and publishes no storage object at all.

**Unknown is never zero.** An unreadable subtree, an unattributable directory, or a scan past its
bounds sets `Usage.Unknown`, which becomes `totals.unknown` and publishes
`storage.unattributed_bytes` as `null`. The orphan scan reclaims only what it can prove — coop's own
generation record, no session naming it, no reservation, no live worker or sandbox activity, older
than the reclaim age, and a plan that passes the ordinary clean/merged discard fences. A generation
only seconds old is treated as a create still in flight, because
`ensureSessionWorkspaceContext` writes the workspace before the session row exists.

## Changelog
- 2026-09-11 — `docs/worker.md` folded into `docs/session-api.md` with the `coop sessions connect`
  consolidation; the workspace-storage section lives there now.
- 2026-09-11 — created with the worker-side byte accounting, pressure check and owned-orphan scan.
