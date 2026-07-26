---
name: signoff-scope-is-run-anchored
description: the signoff reviews a run-anchored folder-diff subject list — re-anchor the baseline ONLY on a receipt-consistent round, or reworked reopens silently escape the next review
subsystem: loop
sources: [internal/cli/commands.go, internal/cli/completionwindow.go, internal/cli/loopchanges.go]
updated: 2026-07-26
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
  `finishReview` returns those ids so `reviewBaselineAfterVerdict` excludes them too. Absorbing
  them into the baseline would ship a human-completed task with no review round ever seeing it.
  Final verify returns to signoff when it observes one. If the controller crashes first, startup
  replay returns the journal's concurrent ids and excludes them from the new run baseline.

Accepted tradeoff, by design: done tasks from a previous CRASHED run (completed but never signed
off) are history to the new run and are not re-reviewed — there is no reviewed-marker to tell them
apart, and they passed their own iteration.

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

Related: [[task-state-is-the-folder]].

## Changelog
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
