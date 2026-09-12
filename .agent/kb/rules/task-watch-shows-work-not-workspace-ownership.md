---
name: task-watch-shows-work-not-workspace-ownership
description: tasks watch renders task-affecting activity, never an idle workspace reservation by itself
scope: cli-output
sources: [internal/tasks/watch.go, internal/tasks/watch_test.go, internal/tasks/snapshot.go, internal/tasks/owner.go]
check: go test ./internal/tasks -run 'TestReservationOnlyForkIsNotTaskWatchActivity|TestReservedForkWithAssignmentRendersTaskActivity|TestTaskWatchKeepsForkWorkVisibleWithoutAnExecution|TestTaskWatchDropsTheStandaloneBoxInventory|TestTaskWatchHidesTodoClaims|TestTaskWatchOmitsClaimProcessIDs|TestTaskWatchCompactQueueLabels'
updated: 2026-09-12
---

# Show work in tasks watch, not idle workspace ownership

`coop tasks watch` answers what task work is happening. A remote-session reservation alone is
durable ownership of a reusable workspace, not activity: it does not render a `forks` row and does
not make an otherwise empty board visible. Keep the reservation in the project JSON snapshot and
all lifecycle checks. If that same fork has an assignment, worker startup, detached loop, cleanup,
candidate, land, or execution, show the task-affecting state without exposing the session owner id.

A running box is not a second inventory either. The board carries no per-execution block: a box or
session that belongs to a task shows on that task's own row (`← fork`), and one that does not has
nothing to say to somebody reading the queue. The execution data stays in the snapshot, where
activity accounting, auto-exit and `--json` read it.

TODO rows never show a claim or fork attribution. Watch labels omit process IDs; an active task
still names its owner and reports when that owner's process has stopped. Keep diagnostic ownership
in `--json` and retain stored claims: a claim can exist in TODO while claim/release is transitioning.

Task rows show only the queue's scope: omit the word `queue`, trim the exact `/.agent/tasks`
suffix, and omit the root `.agent/tasks` label entirely. An absent label means root; do not
replace it with `root` or leave an empty separator. Preserve custom queue names and diagnostic
paths. Compute title width after shortening the suffix so titles use the space saved.

**Why:** After the watcher filled its lower half with idle `remote-* · remote-session remote_*`
rows, the user said, "I don't see why forks should be shown in tasks watch" (2026-09-02). Those
opaque ownership ids hid the task queue without describing work.
On 2026-09-12 the user added, "if task is in TODO don't show claims" and "no need to show pid in
watch" after stopped claims obscured queued task titles.
The same day: "do not show the 'queue' word here, takes too much space", "trim standard suffix",
and "dont show · root, if there is no name it's root".

**How to apply:** Treat snapshot collection and terminal presentation as separate boundaries.
Preserve every reservation in `ReadProjectSnapshot`/`--json`; in the terminal renderer, use one
shared activity predicate that excludes reservation-only state. Do not render executions at all,
and keep standalone fork rows only for lifecycle states that affect task progress.

## Changelog

- 2026-09-12 — swept watch row rendering, source summaries, snapshot labels and loop task scopes
  (watch.go, snapshot.go, loop/report.go). Only task rows repeated `queue` and the standard path
  suffix; fixed here. Source summaries still identify their queues, diagnostic paths stay exact,
  and loop task scopes already omit root. Regression covers every visible task state, nested and
  custom queues, lookalike suffixes, active leases and the shortened suffix width budget.
- 2026-09-12 — swept watch, snapshot, owner labels and task-list markers. Fixed TODO attribution
  and watch PID noise; list already gates claims on in-progress state. Shared diagnostic labels
  and authority remain intact. Regressions cover TODO human/fork claims and the real watch path
  with live, stopped and unbound owners, including unchanged JSON and stored claims.
- 2026-09-11 — the `sandboxes` block is gone with the CLI design's task-family revision; the same
  boundary now holds all the way (snapshot keeps every execution, the terminal shows tasks). The
  card's check follows the renamed regression test.
- 2026-09-02 — created after sweeping every `snapshot.Forks` and `fork.Reservation` consumer.
  The two terminal visibility predicates and reservation-first label were the only presentation
  violations; lifecycle status, workspace cleanup, and JSON snapshot consumers remain unchanged.
