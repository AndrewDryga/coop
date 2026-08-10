---
name: loop-range-rejects-outside-commits
description: any commit landing while an iteration runs joins its range and can reject that iteration's completion; the per-worktree ref-authority lock (fork_loop.go) closes the narrower race where HEAD moves between a validated headAfter and much-later authority consumption
subsystem: loop
sources: [internal/cli/controller.go, internal/cli/commands.go, internal/cli/fork_loop.go, internal/cli/sign.go, internal/cli/fork_merge.go, internal/cli/tasks.go]
updated: 2026-08-09
---
`coop loop` validates a completion over the commits between the iteration's starting HEAD and the
proposed HEAD. That range is a *time* window on one branch, not a set of commits the agent authored
— so **anything committed to the repo while an iteration is running joins it**, including work done
by a human or another agent on the host.

`unbindableTasks` rejects the whole completion when an in-range commit carries a `Coop-Task` trailer
for a task that is not in the finished set (the `!allowed[id] && inRange` guard). That is deliberate:
it stops an iteration from smuggling in a binding it does not own. But it also means an unrelated,
perfectly good commit made on the host mid-iteration **destroys that iteration's completion**.

Observed 2026-08-01: a host-side commit landed during a running iteration, and the agent's finished,
committed, 32-minute task was rejected with "the new commit range and reachable HEAD each need
exactly one commit with one parseable `Coop-Task: <id>` trailer per task". The task's own commit was
HEAD and its trailer parsed correctly — the rejection came entirely from the foreign binding.

So the single-writer rule for a loop's checkout covers **commits**, not just file edits. While a loop
is running against a repo, do not commit to it from the host. If work is unavoidable, stop the loop
first — and stop it at an iteration boundary, because a hard kill between the agent's commit and its
folder move leaves the task in the state [[loop-resume-never-rewrites-history]] describes.

## The worst source of a foreign commit is a second loop

A human committing mid-iteration is the obvious case; the expensive one is **two `coop loop`
processes sharing a checkout**. Each works a *different* task, so the per-task lease in
`completionwindow.go` never fires — it only stops two loops claiming the SAME task. Both commit,
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

The fix is `lockRefAuthority` (`fork_loop.go`), keyed and located exactly like `lockLoopCheckout` (the
resolved worktree path, `$ConfigDir/.locks/ref-<sha>.lock`, never the repo name — a fork fleet stays
parallel). It is held ONLY across the validate→finalize→consume window, never the box run — holding it
for a whole iteration would serialize the fleet and defeat forks, exactly the property
`lockLoopCheckout`'s own per-worktree keying protects. `enterRefAuthorityWindow` acquires it and, as
the FIRST action inside, re-reads `HEAD` and compares it against the `headAfter` validation already
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
(`completeTrustedTask`'s reopened branch has the identical read-`HEAD`-then-consume-later shape as the
loop's own completion, just driven by a human instead of a box). Unlike `lockLoopCheckout`'s bare
flock, `lockRefAuthority` is proved after the fact the way task-lease authority already is
(`lockLeaseAuthorityWith`, `tasklease.go`): open, flock, then `fstat` the held descriptor against a
fresh `lstat` of the name, so a lock file removed and recreated underfoot can never leave two
controllers each holding an "exclusive" lock on a different inode.

`internal/sessionsvc` (the ACP `coop run` session engine) was audited for the same shape but is
out of scope here: it lands work through its own isolated per-session forkspace workspace, never the
shared parent checkout this lock protects — the same reason a fork's own rebase doesn't need it. If a
future change gives it a land-onto-parent step, that step needs this same lock; it currently has none.

## Changelog
- 2026-08-01 — created: a host commit made during an iteration rejected that iteration's completed task; documents that the range is a time window and that single-writer covers commits.
- 2026-08-03 — two concurrent loops in one checkout were observed rejecting each other's completions (the per-task lease cannot catch it); added `lockLoopCheckout` and recorded why the lock is keyed per worktree so fork fleets stay parallel.
- 2026-08-09 — closed a separate race `lockLoopCheckout` doesn't cover: HEAD could move between an iteration's own validated `headAfter` and its much-later authority consumption. Added the per-worktree `lockRefAuthority` (short validate→finalize→consume window only) and made every host-side ref mutator — `signUnpushed`, fork-merge's parent fast-forward, and the audit-reopen completion path — take it too, documented the parallel-controller contract above.
