---
name: task-authority-model
description: four separate authorities decide who may act on a task/checkout — claim (durable, human-released), lease (kernel flock, one iteration), checkout (kernel flock, one loop run), ref (kernel flock, one validate-then-consume window) — never merge them
subsystem: tasks
sources: [internal/tasks/lease.go, internal/tasks/refauthority.go, internal/tasks/audit.go, internal/tasks/cmd.go, internal/loop/lock.go]
updated: 2026-08-10
---
Coop has FOUR separate authorities over a task and its checkout. Each answers a different question,
each is held a different length of time, and each fails differently — conflating any two of them is
the bug this card exists to prevent re-deriving. This is also `internal/tasks`'s package doc — its
own top-of-file comment (`audit.go`) points back here rather than repeating it.

| # | Authority | Mechanism | Where | Held for | Kind |
|---|-----------|-----------|-------|----------|------|
| 1 | **Claim** | durable record, `<key>.owner.json` | `internal/tasks/lease.go` (`ReadTaskOwnerRecord`/`writeTaskOwnerRecord`/`removeTaskOwnerRecord`, :743-802); written by `claimTaskOwnerRecord`, `internal/tasks/cmd.go:466` | until a HUMAN releases it — never | record |
| 2 | **Lease** | kernel flock, `<key>.lock` | `internal/tasks/lease.go` (`lockLeaseAuthority`, `TryTaskLease`, :1309) | one loop iteration | process lock |
| 3 | **Checkout** | kernel flock, `.locks/loop-<sha>.lock` | `lockLoopCheckout`, `internal/loop/lock.go:25` | one whole `coop loop` run | process lock |
| 4 | **Ref** | kernel flock, `.locks/ref-<sha>.lock` | `LockRefAuthority`/`EnterRefAuthorityWindow`, `internal/tasks/refauthority.go:36,134` | validate→finalize→consume only (short) | process lock |

The invariant all four protect: **exactly one writer acts on a task, and on the checkout, at a
time — and when that can't be proven, refuse rather than act on unvalidated state.** Two more
narrower locks exist beside these (the interactive session lock, `internal/cli/commands.go:82` /
`lockSessionProducer`, `internal/cli/commands.go:94`, which serializes only a session-discovering
adapter like codex; and the completion-window index lock, `internal/tasks/completion.go`, an internal
integrity journal) — real, but scoped to one adapter or one bookkeeping structure rather than "who may
act on this task," which is why they aren't in the headline four.

Authorities 1, 2, and 4 live together in `internal/tasks` (the 2026-08 extraction that moved the
folder-task model, the lease/authority registry, and the trusted completion audit out of
`internal/cli` — one package, not the `internal/authority`+`internal/taskq` pair a first pass
considered, because the audit half reads several of these types' unexported fields directly and a
pair split would have forced a much larger export surface for no functional gain; see that task's
`spec.md` if the pair is ever revisited). Authority 3 (checkout) and the two narrower locks stay in
`internal/cli`'s `fork_loop.go`, deferred fork/loop-engine material — `lockLoopCheckout` has exactly
one caller (`commands.go`) and no mover file ever called it, unlike `lockRefAuthority`, which
`internal/tasks/queue.go`'s own `done` verb takes directly (the forcing fact that moved authority 4
with the rest: leaving it in `cli` would have required the new package to import back into it, which
`internal/importdag_test.go`'s invariant 1 forbids).

## The two KINDS of authority — this is the whole design

Authorities 2-4 are **process locks**: a kernel flock, bound to an open file description, that dies
with the process holding it. An unheld flock is definitionally unleased — `observeHeldTaskLease`
(`internal/tasks/lease.go:1454`) states this outright: "An unlocked or missing lock is unleased even
if a crashed controller left stale metadata behind; only a HELD lock means another controller owns
the work." PID, run-id, and heartbeat are recovery evidence and UI only, never authority
(`internal/tasks/lease.go:73-74`).

Authority 1 (claim) is deliberately NOT a process lock, because `coop tasks claim` exits
immediately — there is no process left to hold anything. A human's ownership has to outlive the
command that asserted it, so it is a **durable record** instead: written once, read by every future
scan, and cleared only by an explicit act. This is why a claim can NEVER be inferred stale by mtime,
PID liveness, or heartbeat age (unlike a lease, which a heartbeat CAN mark `leaseStalled` for display,
though even a stalled lease still blocks nobody but its own kernel lock) — a timeout can't distinguish
"went quiet for a good reason" from "abandoned," and guessing wrong is the 2026-07-25 dogfood incident
this authority exists to fix: the loop adopted a task a founder had claimed, because claim held no
lease and left no record, so `AssignLoopTaskOnly` (`internal/tasks/audit.go:3379`) saw a free flock
and took it.

## How claim and lease interact — claim wins unconditionally

`skipOwnedCandidate` (`internal/tasks/audit.go:3360`) checks the claim record BEFORE `TryTaskLease`
even runs, for every in-progress AND todo candidate: an owned task is skipped like a busy lease, never
leased, regardless of whether its lease actually is free. This ordering matters for the race between a
human claiming a todo task and the loop scanning the same instant: `tasksFolderMove`'s claim path
(`internal/tasks/cmd.go:481`) writes the owner record BEFORE it moves the folder, and rolls the record
back if the move then loses the race — so the record is visible to any concurrent scan from the
instant a claim begins, not from whenever its folder move happens to land. The record is cleared by
`done` (`CompleteTrustedTask`, `internal/tasks/audit.go:309`, so every caller — the interactive verb,
fork-merge reconciliation — gets it for free), `block` (`tasksFolderBlock`), `unblock`
(`moveBlockedAuditUnblock`), and the explicit `coop tasks release <id>` (`tasksFolderRelease`,
`internal/tasks/cmd.go:544`) — nothing else. `coop tasks ls` tags an owned in-progress row "claimed by
`<user>`" in place of its lease label (`inProgressMarker`, `internal/tasks/cmd.go:1642`) — the label
the row would otherwise carry, "unleased," would be true but actively misleading for a claimed task.

## Why checkout and ref are separate short-lived locks, not one big one

`lockLoopCheckout` stops two `coop loop` processes from sharing a worktree at all — held for the
WHOLE run. `LockRefAuthority` is a narrower, MUCH shorter lock inside a single loop's own iteration,
covering only the gap between reading a validated `headAfter` and actually consuming task authority
for it (finalizing, writing the receipt, clearing an audit-reopen record) — see
[[loop-range-rejects-outside-commits]] for the full incident and mechanism. Widening either lock to
cover the other's job would either serialize a fork fleet that must stay parallel (checkout is keyed
per resolved WORKTREE path, never the repo name, exactly for this reason — see
[[isolate-state-dont-serialize]]) or hold a lock for an entire box run when only a few filesystem
operations actually need it exclusive.

## Lock ordering when more than one authority is held at once: ref before lease, always

The only place two of these authorities nest today is `ReconcileQueueAfterMerge`
(`internal/tasks/audit.go`, the fork-merge reconciliation path): it takes the **ref** authority
(`LockRefAuthority`) for the whole reconciliation, then calls `CompleteTrustedTask` for each landed
task, which takes the **lease** authority (`lockLeaseAuthority`) internally. So **ref authority is
acquired before lease authority, never the reverse.** Authority 3 (checkout) never nests with either —
it is held for a whole `coop loop` run, one level up, before any task-level authority is ever
considered, so it is never acquired simultaneously with a decision that also needs claim/lease/ref.

This ordering is implicit in the code today (which function calls which), not enforced — a future
edit that acquired lease authority first and then tried to take ref authority inside it would compile
fine and might never deadlock in a single-controller test, only under real contention. No `check:` is
mechanized for it yet: doing so properly needs a call-graph scan in the shape of
`internal/importdag_test.go` (which function reachable from a lease-held region calls
`LockRefAuthority`), which is a genuinely separate piece of tooling, not a same-commit addition to
this move. Tracked instead as a queued follow-up (`coop tasks add`) rather than guessed at here.

## Where the durable ones live

Claim's record sits beside the lease authority's OWN durable siblings — `<key>.json` (lease
metadata), `<key>.reopen.json` (audit-reopen), `<key>.departure.json` (trusted-done departure) — all
in the same host-only, non-cache registry described by
[[task-authority-registry-is-durable-state]]. Same `leaseAuthorityKey` derivation
(`sha256(resolved-repo-root + NUL + task-id)`), same `AtomicWriteTaskFile`, same strict
"reject a body whose TaskID doesn't match" validation. `coop tasks rm` removes it alongside every
other per-task record (`removeTaskFolderAndRecords`, `internal/tasks/cmd.go:1240`) so a deleted
task's claim can't outlive the task itself. See [[task-state-is-the-folder]] for the repo-local queue
these authorities sit beside but never replace — the folder is still the only source of a task's
lifecycle STATE; these four decide who may act on it.

## Changelog
- 2026-08-10 — **the 2026-08 authority-package extraction**: authorities 1 (claim), 2 (lease), and 4
  (ref) — plus the folder-task model and the trusted completion audit — moved out of `internal/cli`
  (`tasklease.go`, `taskcmd.go`, `tasks.go`, `taskwatch.go`, `backlog.go`, `completionwindow.go`,
  `controller.go`, and the `fork_loop.go` ref-authority slice) into `internal/tasks` as ONE package
  (~20.5k lines, the largest of the 2026-08 extractions). Every path and file:line citation in this
  card updated to match. Authority 3 (checkout) and the two narrower locks (interactive session,
  completion-window index) stay in `internal/cli`, unmoved — `lockLoopCheckout` has no mover-file
  caller, unlike `lockRefAuthority`, whose caller inside the new package (`tasks.go`'s `done` verb)
  is what forced authority 4 to move now rather than later. Added the "lock ordering" section above
  (ref acquired before lease, never the reverse) as a documented invariant for the first time — it
  was already true of the code, just never written down; no mechanized `check:` yet, queued as a
  follow-up. This card is now also `internal/tasks`'s package doc (its own file header points here).
- 2026-08-09 — created, closing the two open rows in this map's original sketch (spec.md of task
  2026-07-25-design-durable-ownership-for-human-claims-and-lo): claim (this task, the 2026-07-25
  dogfood incident) and ref (landed same day as b91b51a, `2026-07-26-serialize-live-head-authority`).
  Lease and checkout were already real; interactive-session and completion-window locks noted as
  real but out of the headline four (narrower scope: one adapter / one bookkeeping journal, not
  "who may act on this task").
- 2026-08-10 — line citations refreshed for the loop-engine extraction: `lockLoopCheckout` moved to
  `internal/loop/lock.go:25` and `lockSessionProducer` landed beside its one wrapper in
  `internal/cli/commands.go:94` (`internal/cli/fork_loop.go` is retired). Both bodies are unchanged,
  and the four-authority model is untouched.
