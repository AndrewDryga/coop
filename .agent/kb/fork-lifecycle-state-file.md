---
name: fork-lifecycle-state-file
description: one file (<repo>-forks/.coop/<name>.pid) holds four fork lifecycle states, and only pid+start-token — never file age — may decide that its owner is gone
subsystem: fork
sources: [internal/cli/fork_loop.go, internal/cli/fork_merge.go, internal/processidentity/identity.go]
updated: 2026-08-09
---
Every fork's whole process lifecycle lives in ONE small file, `<repo>-forks/.coop/<name>.pid`, read
and written through `forkWorkerState` (`internal/cli/fork_loop.go`) — never by hand. Four shapes,
with a leading `owner-v1` header (a file without it is legacy state, refused for scoped container
cleanup):

| state | wire form | means |
| --- | --- | --- |
| worker | `<pid>\n<token>` | a detached loop is running (or crashed); signalable |
| reap-pending | `reap-pending` (± identity) | `fork stop` is mid-cleanup; its exact-label box reap still must run |
| reservation | `start-claim\n<pid>\n<token>` | a coop process claimed the fork and has NOT forked a worker yet |
| launched reservation | `start-launched\n<pid>\n<token>` | that coop forked a worker but has not yet recorded its identity |

The identity in a *reservation* is the **claiming coop process**, not a worker. Nothing may ever
signal it: `forkStop` zeroes it and falls into the tombstone path, and `forkRunningPid` returns 0 for
it, or status/watch views would report the starting CLI as a running loop.

## The two crash windows, and why only one of them recovers

`detachForkLoop` holds the per-fork flock across claim → fork → record, so a surviving reservation
means its owner DIED holding it. That's still not enough to reclaim: the death may have happened
after `cmd.Start()`, leaving a real worker out there that self-stamps its pid seconds later (it seeds
the fork's queues before `writeForkPid`). Reclaiming that would put two loops on one worktree — the
exact disaster the O_EXCL claim exists to prevent. So the reservation is written in two phases, and
only the pre-fork one is reclaimable; `start-launched` refuses and routes to `coop fork stop`.

## The one liveness rule

`ownerProvablyDead` is the single definition, and it takes only a `processIdentity`: **Gone** (kernel
says the pid is dead) or **Mismatch** (the pid now belongs to a different process). A live Match and
an unreadable identity (Unknown — no stable token, or `/proc`+sysctl unreadable) are BOTH unproven
and fail closed. There is no mtime, no age, no "it's been a while" anywhere in this subsystem, and
adding one is a rule violation, not an optimization: file age cannot distinguish a wedged host from a
busy one. `forkStateOwner` wraps it for callers that only ask "may I touch this fork?" — an
unreadable or malformed file, an ownerless tombstone, and a launched-but-unrecorded worker all answer
"held".

## Where recovery is allowed to act

- `claimForkPidUnlocked` — a provably dead pre-fork reservation is reclaimed loudly (`ui.Warn`), and
  a new reservation is written in its place. Everything else refuses.
- `recoverInterruptedRebase` (`internal/cli/fork_merge.go`) — leftover `rebase-merge`/`rebase-apply`
  in the fork's clone (a coop killed mid-land; `rebaseForkOntoParent` only aborts a rebase that
  FAILED) is `git rebase --abort`ed, but only when `forkStateOwner` says nobody holds the fork,
  because the abort resets that worktree. In practice `mergeOne` already refuses a fork with ANY
  lifecycle state, so this guard is the defense-in-depth one — keep it anyway.

A dead-WORKER state (not a reservation) is never auto-cleared: it may still own an orphaned box, and
only `coop fork stop` reaps that by owner label.

## Changelog
- 2026-08-09 — created while adding the two crash-window recoveries (reclaimable reservations, leftover-rebase abort); verified against fork_loop.go's state parser/claim path and fork_merge.go's rebase path.
