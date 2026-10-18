---
name: signoff-scope-is-run-anchored
description: the signoff reviews a run-anchored folder-diff subject list — re-anchor the baseline ONLY on a receipt-consistent round, or reworked reopens silently escape the next review
subsystem: loop
sources: [internal/loop/loop.go, internal/loop/signoff.go, internal/loop/pending_review.go, internal/tasks/pending_review.go, internal/tasks/completion.go, internal/cli/sign.go, internal/cli/cli.go, internal/loop/changes.go, internal/tasks/cmd.go]
updated: 2026-10-18
---

The signoff pass does NOT review all of `99_done/` (that dir holds every prior run's history until a
human prunes it). Its subjects are a folder diff: `newlyFinished(reviewBaseline, doneTaskDirs(hosts))`,
with `reviewBaseline` anchored at run start. Empty diff ⇒ the pass is skipped outright — a fresh
`coop loop` on an already-drained queue runs no review box.

The trap is WHERE the baseline re-anchors. It moves forward only after a round whose structured
PASS/FAIL receipt names the exact sorted done-to-actionable task-id delta, and it is computed,
never rescanned: `reviewBaselineAfterVerdict` (internal/loop/signoff.go) takes the previous baseline
plus the round's subjects, minus the reopened ids (they have left `99_done/`, so when their rework
re-enters it they show up in the next round's diff) and minus concurrent completions. Three wrong
placements that fail silently:

- Re-anchoring on the lost-verdict path: the untrusted round's whole subject set must be reviewed
  again; a moved baseline would diff it away and the re-run would review nothing.
- Re-anchoring from the PRE-review done set (the queue as it stood before the review box ran):
  reopened tasks are still in it, so their rework diffs to nothing and ships unreviewed.
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
before publishing the completion receipt. The record names its subject by queue, task id and archive
generation with its receipt, and keeps the workspace, branch, host-recorded run base, review
ladder/prompts/write policy, round and verification phase. It holds no commit ID. Startup validates
this bounded cohort before unrelated work, resumes only those subjects, then continues the current
queue under a fresh cohort after the older debt settles. It never scans historical `99_done/`
entries to guess what needs review.

A review finds each subject's commit by its `Coop-Task` trailer, only in base..HEAD as git reads it:
the commits reachable from HEAD and not from the host-recorded run base. A box writes commit
messages, so a trailer the base already had never counts. The base need not stay an ancestor: the
recovery recipe for a commit that lost its folder move amends the run's start commit, and a signing
pass may re-sign it (requiring ancestry broke both in the scripted loop suite). Loading, enrolling,
starting a round and clearing a subject each require exactly one such commit. A dropped or
duplicated commit, a wrong branch, a changed queue or task generation, an archive mutation or a
malformed record keeps the debt and starts no reviewer. A missing or duplicated commit is a
`PendingReviewCommitsError`, which the CLI draws as a failure block naming the repair (restore one
commit, or drop the extra one or squash them, since it may be forged) and the command to run next;
for several it lists each one's short ID and author (control, direction, line-separator and invisible
characters shown as `?`), because only the person can tell a planted commit from their own.
After a rebase onto pulled work, those commits fall in the range too. Signing rewrites commit IDs
but keeps every trailer, so no signing pass has to report what it replaced.

One exception widens the search, and never for box work: a host-receipted completion whose trailer
is not in base..HEAD because the task was committed before the run, such as another session's
`coop tasks done` during a review or between boxes, or one the startup window replay finds.
Enrollment then takes the task's one commit in the whole branch and stores that commit's parent as
the subject's own `Base`, anchored the way `--review-task` anchors an import; zero or several such
commits refuse it. A box's own completion already passed the range check, so its commit is always in
range. Refusing this case failed the human's command and stopped the run, and after a controller
death the window replay failed at every start
(`TestProviderScriptedLoopPendingReviewTakesAHostCompletionFromBeforeTheRun`). Every new enrollment
needs its trailer on exactly one commit in the whole branch, as the box completion check already
demands: a box may plant a commit naming a task it does not hold (tolerated with a warning), and
binding that one would leave the human's own commit unreviewed
(`TestPendingReviewRefusesAHostCompletionBesideAPlantedCommit`). The signoff and verification change
block and diff add each anchored commit (`withEarlierReviewCommits`), so the reviewer sees what its
verdict accepts and a run that changed nothing else still verifies it instead of skipping a no-op.

A signoff verdict covers the content it reviewed, not just the trailer. Each round start stores as
`Reviewed` a count and SHA-256 digest of the identities (change tree, author, message; signing keeps
all three) of every commit in the subject's review range up to HEAD, the window a reviewer of the
run saw: audit rework adds its repair as a commit with no trailer, and an attempt that exits before
any range check can rewrite an untrailered commit on either side of the subject's own. Loading
sends a subject awaiting verification back to signoff when any of those commits changed,
disappeared or moved, and clearing does the same for any subject, instead of refusing; commits
added after the round are not covered. The loop's `requeueChangedReviews` then drops the task from
the review baseline so the next round reviews it again
(`TestPendingReviewVerdictCoversTheCommitsAfterTheTasksOwn`, `...TheRunBeforeTheTasksCommit`), and
that round keeps counting toward the round cap (`nextRoundAfterPending`): only a completion another
session landed restarts the count, or a rework that keeps changing an accepted task's window would
ping-pong forever. Each change is a diff against its parent, so the digest starts with the tree the
window's first commit applies to; without it, `git rebase --onto base~1 base` dropped the run's
start commit, changed HEAD's tree and still matched (`...TheTreeTheRunStartsFrom`). Every field is
length-prefixed: NUL separators once let two different histories share a fingerprint, since git
stores NUL in an author line (`TestPendingReviewDigestKeepsFieldsApart`); with the prefixes, unusual
author lines read like any other. The window must be linear, as audit authority already requires.
A round reads its subjects, its window and the reviewer's change set from one commit, which
`BeginPendingReviewRound` returns; it refuses a subject whose commit is outside the window, and the
review refuses to start anywhere else. Another session's box that moved HEAD between those reads
could otherwise get a verdict whose digest covers commits nobody reviewed
(`TestPendingReviewRoundReadsOneHead`, `TestReviewVerdictRefusesAHeadOtherThanTheRounds`). Loading
and clearing read their two histories from one commit too.
Without this, a later attempt that amended an accepted commit and exited nonzero (so no range check
ran), a rewrite across a Ctrl-C, or a host completion during a review let verification clear code
no reviewer saw (`TestPendingReviewRejectsArchiveAndHistoryMutation/history`, and end to end
`TestProviderScriptedLoopPendingReviewReviewsAChangedAcceptedCommitAgain`).
Version 1 records also pinned commit IDs; they are still read, and the next write stores version 2.
A version 1 record awaiting verification has no `Reviewed`, so it goes back to signoff once, and
one whose commit predates its run base is anchored on load, once, as a new enrollment would be.
Clearing only removes the record: a crash before that reviews the subject again, and
`coop loop --review-task <id>` on an already reviewed task asks for another review. Pre-ledger
archives remain unknown unless the operator names an exact receipt-valid task with `--review-task`.
Older builds also left `*.pending-review-reviewed.json`, `*.pending-review-signing.json` and
`*.pending-review-workspace` files in the registry; nothing reads them now.

A batch review may reopen several tasks whose commits depend on one another. Pending debt names
tasks, so one task's rework cannot invalidate a sibling's. Audit-reopen authority does record commit
IDs: accepted rework of one reopened task replays every commit after its own, and a signing pass
replaces unsigned ones. `CarryReopenedReviews` moves a sibling's authority to current history only
when it held at the old head and the sibling's commits, found again by trailer, still mean exactly
what was reviewed, the reworked task's own entry being the one allowed change. The loop calls it
after accepted rework, and every host signing pass calls `CarryReopenedReviewsAfterSigning` after
its compare-and-swap. Nothing is journaled: a stop between a signing swap and its carry leaves the
authority stale, and the next lease parks that task with recovery guidance. After a review or
verification phase transition, the loop reloads the cohort before more work.

If the controller stops after a reopened task's rework rewrote its commit but before completion, the
next load accepts that rework the way completion would (exact changed subject, complete descendant
replay, same raw parent) and moves the authority to it without spending the generation. It then
carries the other reopened tasks across that rework in the same load, because afterwards nothing
shows the rework any more.

When the review round cap parks a task, its `decision.md` describes that current cap. A resolved
decision from an earlier block is replaced rather than shown as the action for a different block;
prior reasoning belongs in the task log.

Completed between audits add a second, separate run-scoped handoff: `auditEvidenceStore` keeps only
receipt-consistent per-task summaries, caps their prompt size, and drops them when signoff reopens a
task. The receipt verdict is host-bound; gate and finding text is reviewer-reported context only, so
signoff still independently inspects the task and the worker-reported gate results; it runs no
checks itself and reopens a task to request one.

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
- 2026-10-18 — a round reads its subjects, window and change set from one commit and the review
  must start there (task 2026-10-17-read-a-pending-review-s-subjects-and-window-from).
- 2026-10-17 — the baseline is no longer a POST-review rescan: `reviewBaselineAfterVerdict` computes
  it from the prior baseline and the round's subjects, minus reopened and concurrent ids; the gone
  `soSnap` local (a0946fa2) is now described rather than named, and "two wrong placements" counts
  the three listed. Verified against internal/loop/signoff.go and internal/loop/loop.go.
- 2026-10-17 — the several-commit listing shows every character `safetext.Hidden` names as `?`
  (direction overrides, line separators and invisible characters too, not only controls).
- 2026-10-17 — fourth review: the digest starts with the tree the window starts from, a requeued
  task keeps counting toward the round cap, and the control-character refusal is gone (length
  prefixes suffice; the refusal's listing still escapes authors).
- 2026-10-17 — third review: the verdict window starts at the subject's review base (the whole run,
  not just from its commit), its digest length-prefixes every field and refuses control characters
  in author lines, and a several-commit refusal lists each commit's short ID and author.
- 2026-10-17 — second review: a verdict covers every commit from the subject's to HEAD at round
  start (untrailered audit repairs included); new enrollment needs the trailer on one commit in the
  whole branch; version 1 records from before their run base are anchored on load; anchored commits
  join the review's change block and diff.
- 2026-10-17 — review fixes: a host-receipted completion committed before the run is anchored at
  its one commit in the branch (the human's `coop tasks done` and window replay no longer fail),
  and a round stores the reviewed content identity so verification never clears a commit that
  changed after signoff; the found-N refusal is a drawn failure block. Verified against
  pending_review.go, loop.go, cli.go and their unit and scripted tests.
- 2026-10-16 — pending records hold no commit IDs (version 2, version 1 still read): a review finds
  each subject's commit by trailer between the host-recorded base and HEAD and needs exactly one.
  Deleted the signing journal, workspace marker, reviewed-generation tombstones and pending-binding
  rebinds; a reopened sibling's audit authority is carried after accepted rework, after rework a
  stopped controller left, and after every signing pass (task
  2026-10-15-find-a-task-s-commits-by-its-coop-task-trailer-i). Verified against pending_review.go,
  loop.go, sign.go and their tests.
- 2026-10-15 — signoff never runs the gate itself: it inspects worker-reported results and reopens a
  task to request a check (corrected while making loop.yaml request `make check` once per run).
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
