---
name: loop-range-rejects-outside-commits
description: any commit landing while an iteration runs joins its range; it only rejects that iteration's completion when its Coop-Task trailer names a task this iteration's authority could touch — an untouched one is tolerated and journaled instead. The per-worktree ref-authority lock closes the narrower race where HEAD moves between a validated headAfter and much-later authority consumption
subsystem: loop
sources: [internal/tasks/audit.go, internal/tasks/wait.go, internal/tasks/refauthority.go, internal/tasks/queue.go, internal/tasks/pending_review.go, internal/tasks/session_claim.go, internal/taskmcp/taskmcp.go, internal/taskmcp/tools.go, internal/loop/loop.go, internal/loop/completion.go, internal/loop/lock.go, internal/cli/sign.go, internal/forkctl/merge.go]
updated: 2026-11-06
---
`coop loop` validates a completion over the commits between the iteration's starting HEAD and the
proposed HEAD. That range is a *time* window on one branch, not a set of commits the agent authored
— so **anything committed to the repo while an iteration is running joins it**, including work done
by a human or another agent on the host.

`unbindableTasks` rejects the whole completion when an in-range commit carries a `Coop-Task` trailer
for a task outside the finished set (the `!allowed[id] && inRange` guard) — but only when that foreign
id names a task this iteration's authority consumption could actually touch: the finished set itself,
the leased task id, the audit-reopen record's task, any task whose queue state this same completion
window observed change (`windows.AuditDoneCandidates`/`windows.Departures()`), or any task ALREADY
archived before the box ever ran (`windows.BaselineDoneIDs()`, the completion window's baseline
snapshot — captured at the very start of `runIterationWithMode`, so it is queue state the box could not have
influenced either). All of it is host-side knowledge the box cannot influence — never anything the box
itself could write, like a commit author or timestamp. That is deliberate: it stops an iteration from
smuggling in a binding it does not own — completing a second task, reopening an archived one while
leased for only one, or forging an EXTRA commit for a task that's already done and was never touched
at all. That last case is easy to miss: an archived task's history is meant to be *closed*, so it stays
protected even when its folder never moves during the iteration — corrupting a closed record needs no
folder move, only a trailer. A foreign binding that instead REWRITES a task's already-landed commit
(its old sha falls out of the reachable set between base and head) rejects too, unconditionally,
regardless of touched: that can only happen via a rebase or amend that reparents another task's
history, i.e. altering content it doesn't own without ever moving that task's queue folder. Anything
else — a binding for a task this iteration never leased, finished, reopened, moved, or that was ever
archived — is tolerated: `ReportToleratedForeignBindings` warns live (`ui.Warn`) and journals a note to
the named task's own `log.md` instead of rejecting, and that task's own reachable-binding count already
refuses to let its real next completion silently ride on a binding it did not itself just create.

Observed 2026-08-01: a host-side commit landed during a running iteration for a task that iteration
never touched, and the agent's finished, committed, 32-minute task was rejected with "the new commit
range and reachable HEAD each need exactly one commit with one parseable `Coop-Task: <id>` trailer per
task". The task's own commit was HEAD and its trailer parsed correctly — the rejection came entirely
from the foreign, untouched binding. That exact shape — a binding for a task this queue never tracked,
or a live task nobody leased or moved — is now tolerated instead of destroying the completion (fixed
2026-08-09; the regression is pinned by `TestUnbindableTasksTouchedSetNarrowsForeignBindings` in
`internal/tasks/audit_test.go`, which cannot pass against the pre-narrowing signature). An ALREADY-ARCHIVED
task's binding is a narrower exception that still rejects — proven end to end by
`TestProviderScriptedLoopReviewProcess/worker_cannot_forge_review_authority_for_an_archived_task`
(`scripted_loop_review_process_e2e_test.go`), which caught a first cut of this fix that forgot
`baselineDoneIDs` and tolerated a forged archived-task binding it should have rejected.

**Every attempt is checked, not only a completing one.** The same refusal runs at the end of every
ordinary work attempt (`tasks.AttemptBindingsRefused`, called in `internal/loop/loop.go` right after
the completion window audit), whether or not the attempt completed its task; the assigned task's own
trailers pass there, and their count stays the completion check's job. Without it a box could forge
or rewrite a binding in an attempt that then fails, and every later attempt's range would start
above that commit. A refused attempt stops the run with the same "Task completion rejected" text,
after returning the task to in progress (from done or blocked) with the rejection note.

**A wait cannot hide a late commit.** `tasks_wait` refuses a task with a binding, and the loop's
`WaitBase` hook records the commit the wait starts from before the task moves; the worker can still
commit after the tool returns. After the worker exits, `checkTaskWait` judges only what came after
that commit. One binding a task this attempt never touched is another session's and passes, as
everywhere in this card; anything else refuses the wait, and so does a binding for the waiting task
anywhere, a rewrite of the wait's base, or history Coop cannot read. Commits before the wait are the
ordinary binding check's, since people and other agents commit in this checkout while loops run.
That check runs first and its sentences join the refusal, so a forged binding is named. A task
handed back without the tool is checked from the attempt's start. All of this runs before retry
handling, failed attempts and background handoffs included. The box can write the queue, so a queue
Coop cannot read stops the run instead of reading as "no wait", and a failed completion scan takes
the wait back. `RestoreRejectedWait` returns the task to in progress, clears `waits_on`, keeps its
commits, `tmp/` and the worker's own resume step, and logs why.

**The refusal names what broke it.** `touched` maps each id to why it counts (`touchedTasks` in
`internal/loop/completion.go`: moved to done during this attempt, already done before it started,
under review rework, the attempt's own task), and `scanTaskBindings` walks the whole range, writing
one sentence per offending commit — its short id, the task ids it carries (escaped and cut, since a
box writes trailers) and the reason — capped at five plus a count. A count failure is explained by
`tasks.UnbindableTaskProblems` (no new commit, an older binding plus a new one, several new ones).
The failure box, the `RestoreQueuedCompletion` log note and the in-box `tasks_complete` refusal all
quote them, so the next attempt does not mistake another task's commit for its own mistake.

**A host lifecycle move cannot be told apart from a box's, so it is not tolerated.** The tempting
fix for the operator case is to tolerate a binding for a task the host CLI closed. There is no
host-only evidence for that: a loop box's `tasks_complete` on another, unclaimed task runs
`CompleteBoxTask` → `completeTrustedTask`, the very path `coop tasks done` takes, and writes the same
lease completion receipt; the trailer and its commit are box-writable too. Tolerating a receipted
task would let a box plant a binding and close that task itself. Note also that one commit with two
`Coop-Task` trailers (the 2026-11-05 incident's `d47340b0`) is refused before `touched` is consulted,
so no provenance rule would have saved that completion. The run's
recorded base (`loopStartHead`) also counts as a base for rewrites, so a binding dropped or rewritten
below the attempt's start, outside any checked attempt, still refuses a trailer that replaces it.
Audit rework (a lease holding host audit-reopen authority) gets the same check. Only a rewrite of
its task's commit, which an ordinary record allows, may replace the bindings of the commits after it
by replaying them, and semantic replay checks that (`TestAuditReworkRejectionNamesAForgedBinding`,
`TestAuditReworkAttemptBindingsAreChecked`). `TestProviderScriptedLoopPendingReviewRejectsAForgerThatFailsFirst`
proves it end to end: the forging attempt fails, and the loop stops there instead of retrying above
the forged commit.

Pending final review reads trailers under the same rule: it finds a subject's commit only between
the run base the host recorded and HEAD, and needs exactly one there, so a trailer in older history
never counts and a forged second one in range keeps the review from starting
([[signoff-scope-is-run-anchored]]). The one exception is a host-receipted completion, never box
work, whose task was committed before the run: it is anchored just before its single commit in the
whole branch. Any new enrollment needs the trailer on exactly one commit in the whole branch, like
the box completion check's reachable count: a commit a box planted for a task it does not hold is
tolerated here with a warning, and binding it would leave the human's own commit unreviewed. An
iteration that forges one and completes its own task is already rejected by the check above,
because the pending subject was archived before it ran.
`TestProviderScriptedLoopPendingReviewRejectsAForgedSubjectCommit` proves both halves end to end:
the forging iteration is rejected, and the next loop counts two commits and starts no reviewer.

**One binding per task is checked when a task closes, not when a review fails on it.** A review
reopen needs exactly one reachable binding for its subject, so a second binding silently accepted
at close loses a later review's verdict on that task. A task committed after it with more no longer
matters: the review sets commits its exact replay refuses aside (`captureReviewReopen`).
`completeTrustedTask` (`coop tasks done`, and a box's `tasks_complete` on another task) refuses a
task with more than one with `ExtraBindingsError`, and the loop's startup names every queued task
that already has more (`tasks.ExtraBindings`) before any review runs. `coop tasks claim` and
`coop tasks unblock` warn earlier still (`boundTaskNote`): a task already bound gets its commit
named and the rule that further work is an unbound commit naming the task in its body, since a
partial commit made before a decision is the binding a person taking the task up again misses.

The single-writer rule for a loop's checkout still covers **commits**, not just file edits — for two
narrower reasons now instead of one. A commit that touches a task this iteration also leases,
finishes, reopens, or moves still destroys that iteration's completion exactly as before. And even a
genuinely unrelated commit lands in a shared, possibly-dirty tree the running box did not expect, so
it is still worth avoiding. While a loop is running against a repo, do not commit to it from the host.
If work is unavoidable, stop the loop first — and stop it at an iteration boundary, because a hard
kill between the agent's commit and its folder move leaves the task in the state
[[loop-resume-never-rewrites-history]] describes.

## The worst source of a foreign commit is a second loop

A human committing mid-iteration is the obvious case; the expensive one is **two `coop loop`
processes sharing a checkout**. Each works a *different* task, so the per-task lease in
`internal/tasks/completion.go` never fires — it only stops two loops claiming the SAME task. Both commit,
each one's range then contains the other's `Coop-Task` binding, and **both** completions are
rejected. Observed 2026-08-03: `7769ead7` (10:21:41) and `33cb8a41` (10:27:05), two different
tasks, both rejected and reopened after they were finished.

`lockLoopCheckout` (`internal/loop/lock.go`) now makes this impossible: `loop.Control.Run` takes an
exclusive flock per checkout before touching any queue state, so the second loop fails fast and
names the holding pid. It is keyed on the **resolved worktree path**, never the repo name — each
direct fork hands its loop its own `ws`, so concurrent forks keep separate locks and stay parallel.
Serializing parallel forks would defeat them; isolate the state instead.

Recovery is cheap and does not need history surgery: the rejected task is restored to
`10_in_progress/` with its commit still in history, the loop's startup now warns that its commit is
already reachable (with the depth), and the next iteration resumes it — amending in place while that
commit is still HEAD.

## The parallel-controller contract: ref authority is a SEPARATE, short lock

`lockLoopCheckout` stops two loops from sharing a checkout at all, so it can't be the fix for a
narrower race one loop already has internally: `headAfter := gitOut(repo, "rev-parse", "HEAD")` is
read once an iteration's box exits, the completion is validated over `iterHead..headAfter`, and then
— several filesystem operations later — task authority is actually CONSUMED for it: the completion is
finalized, its receipt is written, and (for host-authorized rework) the single-use audit-reopen record
is removed. Nothing serialized the ref across that gap. An interactive `coop run`, a host-side signing
rewrite, `coop fork merge` landing onto the same parent, or a human commit can all move `HEAD` in that
window — none of them go through `lockLoopCheckout` (only another `coop loop` does) — so a reviewed
generation could be consumed while unvalidated history was already current. The invariant this closes:
**authority is consumed only over history that was validated; when that can't be proven, refuse.**

The fix is `LockRefAuthority` (`internal/tasks/refauthority.go`), keyed and located exactly like
`lockLoopCheckout` (the resolved worktree path, `$ConfigDir/.locks/ref-<sha>.lock`, never the repo
name — parallel forks stay parallel). It is held ONLY across the validate→finalize→consume window,
never the box run — holding it for a whole iteration would serialize parallel forks and defeat them,
exactly the property `lockLoopCheckout`'s own per-worktree keying protects. `EnterRefAuthorityWindow`
acquires it and, as the FIRST action inside, re-reads `HEAD` and compares it against the `headAfter` validation already
trusted: a mismatch (or an unreadable HEAD) fails closed before anything is consumed — no receipt, no
audit-reopen generation removed, the task restored to `10_in_progress/` with its commit intact, exactly
as this card's ordinary foreign-commit case resumes.

Because the box run itself is deliberately excluded, EVERY host-side path that mutates the SAME
worktree's ref outside a box has to take this same lock, or it could land inside another controller's
window and make it refuse work that was actually fine (coop tripping its own compare-and-swap):
`signUnpushed`'s `update-ref` (the loop's per-cycle signing sweep, `coop sign`, and a `coop run`
session's exit-time signing all funnel through it), `fastForwardParent`'s `merge --ff-only` (`coop
fork merge` landing onto the parent — the fork's OWN rebase stays unlocked, since that happens in the
fork's isolated worktree, a different lock key entirely), and the host-authorized completion of
audit-reopened work reached via `coop tasks done` and fork-merge's queue reconciliation
(`CompleteTrustedTask`'s reopened branch has the identical read-`HEAD`-then-consume-later shape as the
loop's own completion, just driven by a human instead of a box). Like `lockLoopCheckout`, it is a
plain flock through `config.(*Config).TryLock` (`$ConfigDir/.locks/ref-<sha>.lock`): nothing unlinks
`.locks` files, so the inode the name answers is the one every controller contends for.

`internal/sessionsvc` (the ACP `coop run` session engine) was audited for the same shape but is
out of scope here: it lands work through its own isolated per-session forkspace workspace, never the
shared parent checkout this lock protects — the same reason a fork's own rebase doesn't need it. If a
future change gives it a land-onto-parent step, that step needs this same lock; it currently has none.

See [[task-authority-model]] for the four-authority map and workflow-specific lock ordering. The
loop holds its iteration lease before entering the short ref window; host completion takes ref
authority before attempting task-completion authority.

## Changelog
- 2026-11-06 — claim and unblock name a task's existing binding (task
  2026-11-06-catch-a-second-coop-task-binding-before-a-task-c).
- 2026-11-06 — audit-rework attempts get the attempt binding scan; a review reopen no longer needs
  one binding for every later task (task 2026-11-06-let-a-later-review-reopen-a-task-whose-record-mo).
- 2026-11-06 — documented the late-commit wait refusal: judged from the commit `tasks_wait`
  recorded, another session's bindings tolerated, forged bindings named, unreadable queues fail
  closed; pinned by `TestLoopRejectsCommitsAfterTaskWait`, `TestLoopKeepsAWaitPastAnotherSessionsCommit`
  and `TestCheckTaskWaitJudgesWhatWasCommittedAfterTheWait`. Corrected the old universal lock-order
  claim against `task-authority-model` and the loop caller.
- 2026-11-05 — host completion refuses a second binding and loop startup lists multi-bound tasks
  (task 2026-11-05-refuse-a-second-coop-task-binding-when-it-is-mad).
- 2026-11-05 — the refusal names each offending commit, task id and reason, and the retry command
  (task 2026-11-05-name-the-foreign-commit-when-a-loop-rejects-a-co). Kept rejecting host-closed
  tasks' bindings: a box's `tasks_complete` on another task writes the same receipt `coop tasks done`
  does (`CompleteBoxTask`), so no host-only evidence tells the two apart; recorded above.
- 2026-10-18 — the ref-authority lock dropped its post-flock inode re-check and shares
  `config.TryLock` with the loop and session locks (task 2026-10-15-read-coop-s-host-only-records-with-one-plain-hel).
- 2026-10-18 — the refusal runs at the end of every ordinary attempt, not only a completing one, and
  checks rewrites against the run's recorded base too; named the fail-first forger e2e.
- 2026-10-17 — the snapshot is taken in `runIterationWithMode`, which `runIteration` calls.
- 2026-10-17 — the 2026-08-10 entry names the exported `LockRefAuthority`/`EnterRefAuthorityWindow`
  the move produced (internal/tasks/refauthority.go).
- 2026-10-17 — a new pending-review enrollment needs the trailer on one commit in the whole branch,
  so a planted commit cannot stand in for the human's own (second review of the same task).
- 2026-10-17 — noted the host-receipted exception: a task committed before the run and completed by
  another session is anchored at its one commit in the branch (review fix for the same task).
- 2026-10-16 — pending final review now finds commits by trailer too, only in its host-recorded
  range; noted it beside the foreign-binding check, which is unchanged, and named the e2e that
  forges a pending subject's trailer.
- 2026-08-25 — replaced Fleet terminology in the current concurrency contract with direct parallel
  forks; the per-worktree lock scope and ordering are unchanged.
- 2026-08-01 — created: a host commit made during an iteration rejected that iteration's completed task; documents that the range is a time window and that single-writer covers commits.
- 2026-08-03 — two concurrent loops in one checkout were observed rejecting each other's completions (the per-task lease cannot catch it); added `lockLoopCheckout` and recorded why the lock is keyed per worktree so fork fleets stay parallel.
- 2026-08-09 — closed a separate race `lockLoopCheckout` doesn't cover: HEAD could move between an iteration's own validated `headAfter` and its much-later authority consumption. Added the per-worktree `lockRefAuthority` (short validate→finalize→consume window only) and made every host-side ref mutator — `signUnpushed`, fork-merge's parent fast-forward, and the audit-reopen completion path — take it too, documented the parallel-controller contract above.
- 2026-08-09 — narrowed the rejection itself: `unbindableTasks` used to reject on ANY foreign in-range binding; it now rejects only when that binding names a task this iteration's authority could touch (the finished/leased/reopened/queue-changed set, a rewritten binding, or an already-archived task via the new `completionWindowSet.baselineDoneIDs()`), and tolerates + journals the rest. Fixes the exact 2026-08-01 incident this card was created for — an untouched, never-archived foreign binding no longer costs a finished task — while the adversarial matrix (own task, a second completed/reopened task, an archived-but-untouched task, a rewritten binding, multi-value, invalid) still refuses exactly as before. First landed without `baselineDoneIDs`; `TestProviderScriptedLoopReviewProcess/worker_cannot_forge_review_authority_for_an_archived_task` caught the gap at the e2e level (a forged binding for an untouched archived task was wrongly tolerated) before this card's own unit matrix would have, since the unit tests pass `touched` directly and so never exercised how commands.go actually builds it. Rewrote the rejection-semantics paragraphs above to match; the parallel-controller contract section is unchanged.
- 2026-08-10 — sources repointed: `controller.go` moved whole to `internal/tasks/audit.go`,
  `tasks.go` to `internal/tasks/queue.go`, `completionwindow.go` to `internal/tasks/completion.go`,
  and the `lockRefAuthority`/`enterRefAuthorityWindow` slice out of `fork_loop.go` (which otherwise
  stays in `internal/cli`, `lockLoopCheckout` unmoved) into `internal/tasks/refauthority.go`
  (exported there as `LockRefAuthority`/`EnterRefAuthorityWindow`) — the
  2026-08 tasks/lease/completion-audit extraction. Facts unchanged; added the [[task-authority-model]]
  cross-reference for the newly-documented lock-ordering invariant.
- 2026-08-10 — sources repointed for the loop-engine extraction: `lockLoopCheckout` moved to
  `internal/loop/lock.go` and the range validation it guards to `internal/loop/loop.go`;
  `internal/cli/fork_loop.go` is retired. The lock is still keyed on the resolved worktree path and
  still taken before any queue state is touched — re-verified against the moved file.
