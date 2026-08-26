---
name: fork-lifecycle-state-file
description: one owner-v1 file holds four fork lifecycle states; unsupported formats stay held and only pid+start-token — never file age — may decide a current owner is gone
subsystem: fork
sources: [internal/forkspace/state.go, internal/forkctl/supervise.go, internal/forkctl/merge.go, internal/processidentity/identity.go]
updated: 2026-08-26
---
Every fork's whole process lifecycle lives in ONE small file, `<repo>-forks/.coop/<name>.pid`, read
and written through `forkspace.WorkerState` (`internal/forkspace/state.go`) — never by hand. Four
current shapes. Every one has a leading `owner-v1` header:

| state | wire form | means |
| --- | --- | --- |
| worker | `owner-v1\n<pid>\n<token>` | a detached loop is running (or crashed); signalable |
| reap-pending | `owner-v1\nreap-pending\n` (optionally followed by identity) | `fork stop` is mid-cleanup; its exact-owner box reap still must run |
| reservation | `owner-v1\nstart-claim\n<pid>\n<token>` | a coop process claimed the fork and has NOT forked a worker yet |
| launched reservation | `owner-v1\nstart-launched\n<pid>\n<token>` | that coop forked a worker but has not yet recorded its identity |

The identity in a *reservation* is the **claiming coop process**, not a worker. Nothing may ever
signal it: `forkStop` zeroes it and falls into the tombstone path, and `forkspace.RunningPid` returns
0 for it, or status/watch views would report the starting CLI as a running loop.

## Who owns which half

The FILE is `internal/forkspace`'s: paths (`StateDir`/`PidPath`/`LockPath`/`LogPath`), the flock
(`LockState`/`TryLockState`), the wire format (`WorkerState`, `ParseWorkerState`, `Marshal`,
`Read/WriteWorkerState`, `WritePid`, `ClaimState`, `ClearPidIfMine`), and the identity doctrine
(`ProcessIdentityOf`, `OwnerProvablyDead`, `StateOwner`, `RunningPid`, `NeedsStop`). It is a leaf —
no runtime, no `ui` — so direct fork commands and the sessions service read one contract.

SUPERVISION is `internal/forkctl`'s: `claimForkPid`/`claimForkPidUnlocked` and
`clearForkClaimUnlocked` (the start protocol, which WARNS on a reclaim), `detachForkLoop`,
`recordStartedFork`, `forkStop`'s signalling/`waitForExit`/box reap, and `forkContainerOwner`. Rule
of thumb: if it decides what to do to a PROCESS or a CONTAINER, or prints, it is supervision.

## Unsupported state never becomes an identity

`ParseWorkerState` classifies before body parsing. A headerless numeric PID or first-line
`reap-pending` record is pre-v8 evidence; an unknown `owner-*` header belongs to another version.
Neither becomes a `WorkerState`. `forkctl.CheckWorkerStateFormat` applies the user-facing refusal
before start/recreate, merge, removal, or stop can probe the runtime or change workspace metadata,
and locked start/stop parse again against races. Merge holds that same lifecycle flock across its
final check, fetch, rebase, gate, land, and reconciliation, then reacquires it for post-land removal.
The exact file remains authoritative: `RunningPid` returns 0, while `NeedsStop` and `StateOwner`
stay held. Recovery must never prepend a header; `MIGRATING.md` owns the stop-with-v8 and verified
manual procedure.

## The two crash windows, and why only one of them recovers

`detachForkLoop` holds the per-fork flock across claim → fork → record, so a surviving reservation
means its owner DIED holding it. That's still not enough to reclaim: the death may have happened
after `cmd.Start()`, leaving a real worker out there that self-stamps its pid seconds later (it seeds
the fork's queues before `forkspace.WritePid`). Reclaiming that would put two loops on one worktree —
the exact disaster the O_EXCL claim exists to prevent. So the reservation is written in two phases,
and only the pre-fork one is reclaimable; `start-launched` refuses and routes to `coop fork stop`.

## The one liveness rule

`forkspace.OwnerProvablyDead` is the single definition, and it takes only a
`forkspace.ProcessIdentity`: **Gone** (kernel says the pid is dead) or **Mismatch** (the pid now
belongs to a different process). A live Match and an unreadable identity (Unknown — no stable token,
or `/proc`+sysctl unreadable) are BOTH unproven and fail closed. There is no mtime, no age, no "it's
been a while" anywhere in this subsystem, and adding one is a rule violation, not an optimization:
file age cannot distinguish a wedged host from a busy one. `forkspace.StateOwner` wraps it for
callers that only ask "may I touch this fork?" — an unreadable or malformed file, an ownerless
tombstone, and a launched-but-unrecorded worker all answer "held".

## Where recovery is allowed to act

- `claimForkPidUnlocked` (`internal/forkctl/supervise.go`) — a provably dead pre-fork reservation is
  reclaimed loudly (`ui.Warn`), and a new reservation is written in its place. Everything else
  refuses. It lives OUTSIDE forkspace precisely BECAUSE it warns: forkspace prints nothing.
- `recoverInterruptedRebase` (`internal/forkctl/merge.go`) — leftover `rebase-merge`/`rebase-apply`
  in the fork's clone (a coop killed mid-land; `rebaseForkOntoParent` only aborts a rebase that
  FAILED) is `git rebase --abort`ed, but only when `forkspace.StateOwner` says nobody holds the fork,
  because the abort resets that worktree. In practice `mergeOne` already refuses a fork with ANY
  lifecycle state, so this guard is the defense-in-depth one — keep it anyway.

A dead-WORKER state (not a reservation) is never auto-cleared: it may still own an orphaned box, and
only `coop fork stop` reaps that by owner label.

## Changelog
- 2026-08-26 — removed the pre-v8 parsed state model and its supervision branches. Only
  `owner-v1` is decoded; headerless and unknown-version records remain byte-exact, held, and
  side-effect-free behind the command preflight and merge lifecycle lock. Corrected the wire table
  to include its header.
- 2026-08-25 — removed Fleet from the current contract inventory and corrected the supervision
  owner to `internal/forkctl`; direct fork commands and sessions still share the same leaf state.
- 2026-08-10 — supervision and the land moved OUT of `internal/cli` into the new
  `internal/forkctl` control plane (`fork_loop.go` → `supervise.go`, `fork_merge.go` → `merge.go`);
  the contract itself is unchanged and still lives in `internal/forkspace`. The one cli residue that
  matters here: `lockLoopCheckout`/`lockSessionProducer` stayed in `internal/cli/fork_loop.go` — they
  are LOOP locks, not fork lifecycle state, and the loop-engine extraction owns them next.
- 2026-08-09 — the state file moved to the new `internal/forkspace` leaf (with the workspace paths,
  the name rule, and clone/destroy) so the sessions service can import the contract instead of
  taking a 12-method interface; supervision stayed in cli. Identifiers re-verified against
  `internal/forkspace/state.go`; the wire format is byte-identical (`TestForkWorkerStateWireFormat`
  moved with it, to `internal/forkspace/state_test.go`).
- 2026-08-09 — created while adding the two crash-window recoveries (reclaimable reservations, leftover-rebase abort); verified against fork_loop.go's state parser/claim path and fork_merge.go's rebase path.
- 2026-08-10 — the loop-engine extraction finished that residue: `lockLoopCheckout` is now
  `internal/loop/lock.go`, `lockSessionProducer` moved beside its wrapper in
  `internal/cli/commands.go`, `runForkLoop` folded into `internal/cli/fork_cmd.go`, and
  `internal/cli/fork_loop.go` is retired. The state-file contract in `internal/forkspace` is
  unchanged; re-verified.
