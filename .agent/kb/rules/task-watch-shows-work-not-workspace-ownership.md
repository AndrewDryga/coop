---
name: task-watch-shows-work-not-workspace-ownership
description: tasks watch renders task-affecting activity, never an idle workspace reservation by itself
scope: cli-output
sources: [internal/tasks/watch.go, internal/tasks/watch_test.go, internal/tasks/snapshot.go, internal/tasks/owner.go, internal/forkctl/meta.go]
check: go test ./internal/tasks -run 'TestReservationOnlyForkIsNotTaskWatchActivity|TestReservedForkWithAssignmentRendersTaskActivity|TestTaskWatchKeepsForkWorkVisibleWithoutAnExecution|TestTaskWatchDropsTheStandaloneBoxInventory|TestTaskWatchHidesTodoClaims|TestTaskWatchOmitsClaimProcessIDs|TestTaskWatchCompactQueueLabels|TestTaskWatchSaysWhatAForkIsDoingWithItsTask|TestTaskWatchListsAForkOnceOnItsTaskRow|TestTaskWatchShowsDecisionsAndTodaysWork|TestTaskWatchCountsAPersonsClaimAsWork|TestTaskWatchBoardMatchesTheApprovedLayout|TestTaskWatchEdgeCaseRows|TestTaskWatchPlacesEveryFactByOneRule|TestTaskWatchCapsTheListsNeverTheWork|TestForkStopReasonReadsTheRecordThenTheLog'
updated: 2026-11-05
---

# Show work in tasks watch, not idle workspace ownership

`coop tasks watch` answers what task work is happening. A remote-session reservation alone is
durable ownership of a reusable workspace, not activity: it does not render a `forks` row and does
not make an otherwise empty board visible. Keep the reservation in the project JSON snapshot and
all lifecycle checks. If that same fork has an assignment, detached loop, cleanup, candidate, land,
or execution, show the task-affecting state without exposing the session owner id.

A running box is not a second inventory either. The board carries no per-execution block: a box or
session that belongs to a task shows on that task's own rows, and one that does not has nothing to
say to somebody reading the queue. The execution data stays in the snapshot, where activity
accounting, auto-exit and `--json` read it. The board's sections are Now, Stopped, Needs you and Up
next. A task being worked or stopped has a detail line in one fixed order,
`<done>/<total> · <agent>[ in fork <name>] · <state>`, and a stopped one a fix line with the complete
commands that move it on (never "see the logs": the board reads the fork's recorded failure or its
log itself). A fork is named only as `in fork <name>` there, nothing is right-aligned, and headers
carry no reason text. The `forks` lines are for forks with no task row. Now and Stopped are what the
header counts as working and stopped.

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

- 2026-11-05 — the owner approved a new layout (task 2026-11-05-lay-out-coop-tasks-watch-as-now-
  stopped-needs-yo, spec.md): Now / Stopped / Needs you / Up next, a detail line under each task
  being worked or stopped, a fix line under each stopped one, and `in fork <name>` as the only place
  a fork is named. The `← fork · paused · coop fork logs` suffix, the question under blocked tasks
  and the closing `Answer decisions` line are gone. Swept watch.go's row, header and section
  renderers and the homepage scene (tools/gendocs); `coop tasks ls`, `coop fork ls` and
  `coop tasks decisions` keep their own wording. Added the layout, edge-case, placement, cap and
  stop-reason tests to the check.
- 2026-11-05 — the owner, looking at a board that said `4 in progress` when one task was being
  worked and three forks had died at start, asked how to improve it. The header now splits working
  from paused and counts today's finished tasks; fork rows carry their status and next command, so a
  fork with a task row is no longer repeated under `forks` (`N assigned tasks` replaced
  `N assignment(s)`); blocked tasks lead with their question. Swept every frame and row renderer in
  watch.go; `coop tasks ls` and `coop prompt` keep their own counts.

- 2026-10-17 — a fork loop another operating system started is task work, not cleanup: its row
  reads `background loop started on <system>`, as `coop fork ls` does, where it read
  `cleanup-pending`. Added that state to TestTaskWatchKeepsForkWorkVisibleWithoutAnExecution and
  re-ran the card's check; the other fork states were unchanged.
- 2026-10-15 — dropped "worker startup" from the board states: the fork lifecycle lost its start
  reservation, so the snapshot's `starting` flag, the board's `starting` row and the watch's startup
  guard are gone. A detached loop shows as running once its start returns. Re-ran the card's check.
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
