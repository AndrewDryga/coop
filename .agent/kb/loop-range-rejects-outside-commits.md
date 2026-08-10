---
name: loop-range-rejects-outside-commits
description: any commit landing while an iteration runs joins its range; it only rejects that iteration's completion when its Coop-Task trailer names a task this iteration's authority could touch — an untouched one is tolerated and journaled instead. The per-worktree ref-authority lock (fork_loop.go) closes the narrower race where HEAD moves between a validated headAfter and much-later authority consumption
subsystem: loop
sources: [internal/tasks/audit.go, internal/tasks/refauthority.go, internal/tasks/queue.go, internal/cli/commands.go, internal/cli/fork_loop.go, internal/cli/sign.go, internal/cli/fork_merge.go]
updated: 2026-08-10
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
snapshot — captured at the very start of `runIteration`, so it is queue state the box could not have
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

`lockLoopCheckout` (`internal/cli/fork_loop.go`) now makes this impossible: `a.loop` takes an
exclusive flock per checkout before touching any queue state, so the second loop fails fast and
names the holding pid. It is keyed on the **resolved worktree path**, never the repo name — a fork
fleet hands each loop its own `ws`, so concurrent forks keep separate locks and stay parallel.
Serializing the fleet would defeat forks; isolate the state instead.

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
name — a fork fleet stays parallel). It is held ONLY across the validate→finalize→consume window,
never the box run — holding it for a whole iteration would serialize the fleet and defeat forks,
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
loop's own completion, just driven by a human instead of a box). Unlike `lockLoopCheckout`'s bare
flock, `LockRefAuthority` is proved after the fact the way task-lease authority already is
(`lockLeaseAuthorityWith`, `internal/tasks/lease.go`): open, flock, then `fstat` the held descriptor against a
fresh `lstat` of the name, so a lock file removed and recreated underfoot can never leave two
controllers each holding an "exclusive" lock on a different inode.

`internal/sessionsvc` (the ACP `coop run` session engine) was audited for the same shape but is
out of scope here: it lands work through its own isolated per-session forkspace workspace, never the
shared parent checkout this lock protects — the same reason a fork's own rebase doesn't need it. If a
future change gives it a land-onto-parent step, that step needs this same lock; it currently has none.

See [[task-authority-model]] for the full four-authority map and the lock-ordering invariant (ref
authority is acquired before lease authority, never the reverse) this ref-authority window is half of.

## Changelog
- 2026-08-01 — created: a host commit made during an iteration rejected that iteration's completed task; documents that the range is a time window and that single-writer covers commits.
- 2026-08-03 — two concurrent loops in one checkout were observed rejecting each other's completions (the per-task lease cannot catch it); added `lockLoopCheckout` and recorded why the lock is keyed per worktree so fork fleets stay parallel.
- 2026-08-09 — closed a separate race `lockLoopCheckout` doesn't cover: HEAD could move between an iteration's own validated `headAfter` and its much-later authority consumption. Added the per-worktree `lockRefAuthority` (short validate→finalize→consume window only) and made every host-side ref mutator — `signUnpushed`, fork-merge's parent fast-forward, and the audit-reopen completion path — take it too, documented the parallel-controller contract above.
- 2026-08-09 — narrowed the rejection itself: `unbindableTasks` used to reject on ANY foreign in-range binding; it now rejects only when that binding names a task this iteration's authority could touch (the finished/leased/reopened/queue-changed set, a rewritten binding, or an already-archived task via the new `completionWindowSet.baselineDoneIDs()`), and tolerates + journals the rest. Fixes the exact 2026-08-01 incident this card was created for — an untouched, never-archived foreign binding no longer costs a finished task — while the adversarial matrix (own task, a second completed/reopened task, an archived-but-untouched task, a rewritten binding, multi-value, invalid) still refuses exactly as before. First landed without `baselineDoneIDs`; `TestProviderScriptedLoopReviewProcess/worker_cannot_forge_review_authority_for_an_archived_task` caught the gap at the e2e level (a forged binding for an untouched archived task was wrongly tolerated) before this card's own unit matrix would have, since the unit tests pass `touched` directly and so never exercised how commands.go actually builds it. Rewrote the rejection-semantics paragraphs above to match; the parallel-controller contract section is unchanged.
- 2026-08-10 — sources repointed: `controller.go` moved whole to `internal/tasks/audit.go`,
  `tasks.go` to `internal/tasks/queue.go`, `completionwindow.go` to `internal/tasks/completion.go`,
  and the `lockRefAuthority`/`enterRefAuthorityWindow` slice out of `fork_loop.go` (which otherwise
  stays in `internal/cli`, `lockLoopCheckout` unmoved) into `internal/tasks/refauthority.go` — the
  2026-08 tasks/lease/completion-audit extraction. Facts unchanged; added the [[task-authority-model]]
  cross-reference for the newly-documented lock-ordering invariant.
