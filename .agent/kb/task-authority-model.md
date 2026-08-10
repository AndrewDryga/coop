---
name: task-authority-model
description: four separate authorities decide who may act on a task/checkout — claim (durable, human-released), lease (kernel flock, one iteration), checkout (kernel flock, one loop run), ref (kernel flock, one validate-then-consume window) — never merge them
subsystem: tasks
sources: [internal/cli/tasklease.go, internal/cli/fork_loop.go, internal/cli/controller.go, internal/cli/taskcmd.go]
updated: 2026-08-09
---
Coop has FOUR separate authorities over a task and its checkout. Each answers a different question,
each is held a different length of time, and each fails differently — conflating any two of them is
the bug this card exists to prevent re-deriving.

| # | Authority | Mechanism | Where | Held for | Kind |
|---|-----------|-----------|-------|----------|------|
| 1 | **Claim** | durable record, `<key>.owner.json` | `tasklease.go` (`readTaskOwnerRecord`/`writeTaskOwnerRecord`/`removeTaskOwnerRecord`, :694-802); written by `claimTaskOwnerRecord`, `taskcmd.go:466` | until a HUMAN releases it — never | record |
| 2 | **Lease** | kernel flock, `<key>.lock` | `tasklease.go` (`lockLeaseAuthority`, `tryTaskLease`, :1309) | one loop iteration | process lock |
| 3 | **Checkout** | kernel flock, `.locks/loop-<sha>.lock` | `lockLoopCheckout`, `fork_loop.go:60` | one whole `coop loop` run | process lock |
| 4 | **Ref** | kernel flock, `.locks/ref-<sha>.lock` | `lockRefAuthority`/`enterRefAuthorityWindow`, `fork_loop.go:127,221` | validate→finalize→consume only (short) | process lock |

The invariant all four protect: **exactly one writer acts on a task, and on the checkout, at a
time — and when that can't be proven, refuse rather than act on unvalidated state.** Two more
narrower locks exist beside these (the interactive session lock, `commands.go:92` /
`lockSessionProducer`, `fork_loop.go:244`, which serializes only a session-discovering adapter like
codex; and the completion-window index lock, `completionwindow.go`, an internal integrity journal) —
real, but scoped to one adapter or one bookkeeping structure rather than "who may act on this task,"
which is why they aren't in the headline four.

## The two KINDS of authority — this is the whole design

Authorities 2-4 are **process locks**: a kernel flock, bound to an open file description, that dies
with the process holding it. An unheld flock is definitionally unleased — `observeHeldTaskLease`
(`tasklease.go`) states this outright: "An unlocked or missing lock is unleased even if a crashed
controller left stale metadata behind; only a HELD lock means another controller owns the work." PID,
run-id, and heartbeat are recovery evidence and UI only, never authority (`tasklease.go:73-74`).

Authority 1 (claim) is deliberately NOT a process lock, because `coop tasks claim` exits
immediately — there is no process left to hold anything. A human's ownership has to outlive the
command that asserted it, so it is a **durable record** instead: written once, read by every future
scan, and cleared only by an explicit act. This is why a claim can NEVER be inferred stale by mtime,
PID liveness, or heartbeat age (unlike a lease, which a heartbeat CAN mark `leaseStalled` for display,
though even a stalled lease still blocks nobody but its own kernel lock) — a timeout can't distinguish
"went quiet for a good reason" from "abandoned," and guessing wrong is the 2026-07-25 dogfood incident
this authority exists to fix: the loop adopted a task a founder had claimed, because claim held no
lease and left no record, so `assignLoopTaskOnly` (`controller.go:3362`) saw a free flock and took it.

## How claim and lease interact — claim wins unconditionally

`skipOwnedCandidate` (`controller.go:3343`) checks the claim record BEFORE `tryTaskLease` even runs,
for every in-progress AND todo candidate: an owned task is skipped like a busy lease, never leased,
regardless of whether its lease actually is free. This ordering matters for the race between a human
claiming a todo task and the loop scanning the same instant: `tasksFolderMove`'s claim path
(`taskcmd.go`) writes the owner record BEFORE it moves the folder, and rolls the record back if the
move then loses the race — so the record is visible to any concurrent scan from the instant a claim
begins, not from whenever its folder move happens to land. The record is cleared by `done`
(`completeTrustedTask`, `controller.go:292`, so every caller — the interactive verb, fork-merge
reconciliation — gets it for free), `block` (`tasksFolderBlock`), `unblock`
(`moveBlockedAuditUnblock`), and the explicit `coop tasks release <id>` (`tasksFolderRelease`,
`taskcmd.go:544`) — nothing else. `coop tasks ls` tags an owned in-progress row "claimed by `<user>`"
in place of its lease label (`inProgressMarker`, `taskcmd.go:1642`) — the label the row would
otherwise carry, "unleased," would be true but actively misleading for a claimed task.

## Why checkout and ref are separate short-lived locks, not one big one

`lockLoopCheckout` stops two `coop loop` processes from sharing a worktree at all — held for the
WHOLE run. `lockRefAuthority` is a narrower, MUCH shorter lock inside a single loop's own iteration,
covering only the gap between reading a validated `headAfter` and actually consuming task authority
for it (finalizing, writing the receipt, clearing an audit-reopen record) — see
[[loop-range-rejects-outside-commits]] for the full incident and mechanism. Widening either lock to
cover the other's job would either serialize a fork fleet that must stay parallel (checkout is keyed
per resolved WORKTREE path, never the repo name, exactly for this reason — see
[[isolate-state-dont-serialize]]) or hold a lock for an entire box run when only a few filesystem
operations actually need it exclusive.

## Where the durable ones live

Claim's record sits beside the lease authority's OWN durable siblings — `<key>.json` (lease
metadata), `<key>.reopen.json` (audit-reopen), `<key>.departure.json` (trusted-done departure) — all
in the same host-only, non-cache registry described by
[[task-authority-registry-is-durable-state]]. Same `leaseAuthorityKey` derivation
(`sha256(resolved-repo-root + NUL + task-id)`), same `atomicWriteTaskFile`, same strict
"reject a body whose TaskID doesn't match" validation. `coop tasks rm` removes it alongside every
other per-task record (`removeTaskFolderAndRecords`, `taskcmd.go`) so a deleted task's claim can't
outlive the task itself. See [[task-state-is-the-folder]] for the repo-local queue these authorities
sit beside but never replace — the folder is still the only source of a task's lifecycle STATE; these
four decide who may act on it.

## Changelog
- 2026-08-09 — created, closing the two open rows in this map's original sketch (spec.md of task
  2026-07-25-design-durable-ownership-for-human-claims-and-lo): claim (this task, the 2026-07-25
  dogfood incident) and ref (landed same day as b91b51a, `2026-07-26-serialize-live-head-authority`).
  Lease and checkout were already real; interactive-session and completion-window locks noted as
  real but out of the headline four (narrower scope: one adapter / one bookkeeping journal, not
  "who may act on this task").
