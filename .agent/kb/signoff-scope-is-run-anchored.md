---
name: signoff-scope-is-run-anchored
description: the signoff reviews a run-anchored folder-diff subject list — re-anchor the baseline ONLY on a receipt-consistent round, or reworked reopens silently escape the next review
subsystem: loop
sources: [internal/loop/loop.go, internal/loop/signoff.go, internal/loop/pending_review.go, internal/tasks/pending_review.go, internal/tasks/completion.go, internal/cli/sign.go, internal/loop/changes.go, internal/tasks/cmd.go]
updated: 2026-09-14
---

The signoff pass does NOT review all of `99_done/` (that dir holds every prior run's history until a
human prunes it). Its subjects are a folder diff: `newlyFinished(reviewBaseline, doneTaskDirs(hosts))`,
with `reviewBaseline` anchored at run start. Empty diff ⇒ the pass is skipped outright — a fresh
`coop loop` on an already-drained queue runs no review box.

The trap is WHERE the baseline re-anchors. It moves forward only after a round whose structured
PASS/FAIL receipt names the exact sorted done-to-actionable task-id delta, and it is taken
from the POST-review done set (the reopened tasks have already left `99_done/`, so when their rework
re-enters it they show up in the next round's diff). Two wrong placements that fail silently:

- Re-anchoring on the lost-verdict path: the untrusted round's whole subject set must be reviewed
  again; a moved baseline would diff it away and the re-run would review nothing.
- Re-anchoring from the PRE-review snapshot (e.g. `soSnap`): reopened tasks are still in that done
  set, so their rework diffs to nothing and ships unreviewed.
- Re-anchoring over a CONCURRENT host completion: a parallel session may `coop tasks done` an
  unrelated task while the review box runs. The review's completion window tolerates it (the
  window records the review's exact subject ids; a host-receipted non-subject completion is
  concurrent activity, not the review's mutation — everything else still fails closed), and
  `FinishReview` returns those ids so `reviewBaselineAfterVerdict` excludes them too. Absorbing
  them into the baseline would ship a human-completed task with no review round ever seeing it.
  Final verify returns to signoff when it observes one. If the controller crashes first, startup
  replay returns the journal's concurrent ids and excludes them from the new run baseline.

Accepted tasks now remain review debt across a graceful pause, hard process death, task limit, or
interrupted signoff/verification. The host writes a versioned pending record under task authority
before publishing the completion receipt. That record pins the exact task generation and receipt,
raw unique commit binding, workspace/branch/queue identities, run base, review ladder/prompts/write
policy, round and verification phase. Startup validates this bounded cohort before unrelated work,
resumes only those subjects, then continues the current queue under a fresh cohort after the older
debt settles. It never scans historical `99_done/` entries to guess what needs review.

Signing is a second write-ahead boundary: while the ref-authority lock is held, Coop records the
exact old-to-new commit map before the branch compare-and-swap. A crash after the ref update may
rebind pending evidence only through that host-private map. Tree-equivalent arbitrary rewrites,
wrong branches, changed queue/task generations, archive mutations and malformed records retain the
debt and fail closed. Receipt-consistent acceptance writes a reviewed-generation tombstone before
clearing the active record, so an explicit historical import cannot repeatedly review and bless the
same generation. Pre-ledger archives remain unknown unless the operator names an exact receipt-valid
task with `coop loop --review-task <id>`.

A batch review may reopen several tasks whose commits depend on one another. Repairing an older
task replays a later sibling commit under a new SHA even when that sibling's reviewed change is
identical. Coop rebinds both the sibling's pending-review binding and its active audit-reopen record
from the same host-validated rewrite. Host signing does the same for every subject in the cohort,
and startup can finish both updates from the signing journal. After a review or verification phase
transition, the loop reloads the cohort before more work; otherwise an accepted task whose record
was cleared remains a stale rebind target. Updating only one authority—or only the persisted state
without its in-memory view—leaves valid sibling work unable to resume.

If the controller stops after an audit-reopened task rewrites its commit but before completion
bookkeeping refreshes that task's pending binding, startup may recover only when the still-active
audit generation validates the exact changed subject and complete descendant replay. It advances
the audit record first; a second startup can then repair a still-old pending binding from that
host-owned current authority.

When the review round cap parks a task, its `decision.md` describes that current cap. A resolved
decision from an earlier block is replaced rather than shown as the action for a different block;
prior reasoning belongs in the task log.

Completed between audits add a second, separate run-scoped handoff: `auditEvidenceStore` keeps only
receipt-consistent per-task summaries, caps their prompt size, and drops them when signoff reopens a
task. The receipt verdict is host-bound; gate and finding text is reviewer-reported context only, so
signoff still independently inspects the task and runs the gate.

Work windows use a related but deliberately distinct rule. They journal the exact assigned task id.
When another baseline archive leaves `99_done/`, it is tolerated only if its host-only departure
record contains the exact nonce from that window's baseline completion receipt. `coop tasks claim`
creates that record under the task authority flock before clearing the receipt; raw folder moves,
stale or forged nonce records, missing receipts, duplicate assigned ids, and the assigned subject
itself all fail closed. The same journal fields drive crash replay, so an interrupted work stage gets
the live-stage verdict rather than a broader recovery exception.

Human deletion is the explicit exception to the missing-folder fail-closed rule. Completion-window
creation holds the index lock across its DONE snapshot and journal registration; every confirmed
CLI task deletion takes that same index lock first, then the task's persistent authority flock.
While both remain held, deletion removes the id from every completion-window policy field, clears
the receipt plus auxiliary records, and removes the folder. A live authority flock refuses deletion
without partially purging the journal. A free flock from a killed controller permits cleanup, but
its inode remains so later claimants cannot bypass the lock through a new file. Box-side or manual
folder deletion still has no such authority and remains a hard audit failure. A subject-scoped
review keeps that scope in a separate journal bit when deletion removes its final subject id; this
distinguishes it from an intentionally subject-free preflight/verify window, so later host-valid
completions still replay as concurrent activity.

Startup recovery must also distinguish a task newly entering `99_done/` from metadata changes to a
task already present in the durable window baseline. It locks every changed task's host completion
authority, then classifies all overlapping windows from the locked receipt generation. An
unreceipted arrival was never an accepted archive and is restored even if a later window observed
more provider-written metadata. A host-receipted arrival does not hide a later same-generation
baseline mutation: that archive remains done and its stale authority is cleared. A fresh receipt
can supersede a crash-persisted marker or an uncertain busy baseline unless another baseline
already carries the same generation and proves a later mutation. Recovery re-fingerprints locked
tasks before classification and retries if provider-written metadata changed during lock handoff.

Before changing lifecycle state, recovery persists per-window mutation markers and the ids of
baseline tasks it will restore. Those records are the crash boundary: a cleared nonce cannot be
reinterpreted as a new completion, and a reboot between overlapping-window retirements cannot
misreport recovery's own done-to-actionable move. Task authority stays locked through receipt
clearing, lifecycle repair, and journal retirement. Deterministic stale windows are consumed, one
actionable integrity error names mutated archives, and the next startup is clean.

Related: [[task-state-is-the-folder]].

## Changelog
- 2026-09-14 — documented cohort-wide pending and audit-authority rebinding across an older task
  repair and the following host signing rewrite, plus mandatory reload after review phase changes;
  added the audit-authorized crash boundary after live Emisar rework exposed it. Verified against
  the loop call sites and focused sibling, signing, two-phase startup recovery, and current review
  cap decision tests.
- 2026-09-13 — replaced the process-local crash tradeoff with exact host-private pending cohorts,
  signing-rewrite recovery, reviewed-generation tombstones and explicit bounded legacy import.
- 2026-08-10 — sources repointed: `completionwindow.go`/`taskcmd.go` moved to
  `internal/tasks/completion.go`/`internal/tasks/cmd.go` (the 2026-08 tasks/lease/completion-audit
  extraction); `finishReview` is now exported `tasks.FinishReview`. Facts unchanged.
- 2026-07-26 — documented authority-locked startup classification, overlapping-window precedence,
  and durable mutation/recovered-departure markers for crash-safe recovery.
- 2026-07-26 — documented CLI-authoritative task deletion and live-lease refusal; verified against
  `internal/cli/taskcmd.go` and the completion-window restart fixture.
- 2026-07-26 — documented nonce-bound concurrent host reopens in work completion windows; verified
  against `internal/cli/completionwindow.go`, `internal/cli/controller.go`, and the scripted loop
  process fixture.
- 2026-07-25 — review completion windows became subject-scoped (parallel human `coop tasks done`
  no longer kills the run); documented the concurrent-completion baseline exclusion.
- 2026-07-14 — documented the bounded audit-evidence handoff and rechecked its replacement/drop
  semantics against `internal/cli/commands.go` and `internal/cli/loopchanges.go`.
- 2026-07-14 — updated the receipt contract from a count to an exact verdict + task-id delta and
  re-verified the baseline placement against `internal/cli/commands.go`.
- 2026-07-13 — created with the run-scoped signoff change (verified against loop()'s round logic in internal/cli/commands.go).
- 2026-08-10 — sources repointed for the loop-engine extraction: the round logic is now
  `internal/loop/loop.go` and the baseline/subject bookkeeping (`newlyFinished`, `doneTaskDirs`,
  `reviewBaselineAfterVerdict`) is `internal/loop/signoff.go`; `loopchanges.go` → `changes.go`.
  Re-verified the baseline placement — it still re-anchors only on a receipt-consistent round.
