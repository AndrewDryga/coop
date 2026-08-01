---
name: loop-range-rejects-outside-commits
description: any commit landing while an iteration runs joins its range; one carrying another task's Coop-Task trailer rejects that iteration's completion
subsystem: loop
sources: [internal/cli/controller.go, internal/cli/commands.go]
updated: 2026-08-01
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

Recovery is cheap and does not need history surgery: the rejected task is restored to
`10_in_progress/` with its commit still in history, the loop's startup now warns that its commit is
already reachable (with the depth), and the next iteration resumes it — amending in place while that
commit is still HEAD.

## Changelog
- 2026-08-01 — created: a host commit made during an iteration rejected that iteration's completed task; documents that the range is a time window and that single-writer covers commits.
