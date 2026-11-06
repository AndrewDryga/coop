---
name: task-watch-shows-work-not-workspace-ownership
description: tasks watch renders task-affecting activity, never an idle workspace reservation by itself
scope: cli-output
sources: [internal/tasks/watch.go, internal/tasks/watch_test.go, internal/tasks/snapshot.go, internal/tasks/owner.go, internal/tasks/lease.go, internal/forkctl/meta.go, internal/forkspace/gitcommand.go, internal/loop/rotation.go]
check: go test ./internal/tasks -run 'TestReservationOnlyForkIsNotTaskWatchActivity|TestReservedForkWithAssignmentRendersTaskActivity|TestTaskWatchKeepsForkWorkVisibleWithoutAnExecution|TestTaskWatchDropsTheStandaloneBoxInventory|TestTaskWatchHidesTodoClaims|TestTaskWatchOmitsClaimProcessIDs|TestTaskWatchCompactQueueLabels|TestTaskWatchSaysWhatAForkIsDoingWithItsTask|TestTaskWatchListsAForkOnceOnItsTaskRow|TestTaskWatchShowsDecisionsAndTodaysWork|TestTaskWatchCountsAPersonsClaimAsWork|TestTaskWatchBoardMatchesTheApprovedLayout|TestTaskWatchEdgeCaseRows|TestTaskWatchPlacesEveryFactByOneRule|TestTaskWatchCapsTheListsNeverTheWork|TestForkStopReasonReadsTheRecordThenTheLog|TestTaskWatchMultiQueueBoardMatchesTheApprovedLayout|TestTaskWatchNamesEveryRowsQueueOnlyWhenThereAreSeveral|TestTaskWatchShowsAForksMergeOnce|TestTaskWatchNamesTheFullTargetTheWorkerRecorded|TestTaskWatchFillsTheTerminalHeight|TestTaskWatchHeaderShortensInsteadOfBeingCut|TestTaskWatchTitleUsesAvailableTerminalWidth|TestTaskWatchLinksTitles|TestTaskWatchNamesEveryTaskOfAForkTheSameWay|TestTaskWatchShowsAForkLoopsErrorOnItsOwnLines'
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
next, each header with its count. Every row above Up next has a detail line in one fixed order,
`<queue> · [<done>/<total> · ][<agent>[ in fork <name>] · ]<state> for <duration>`, where the agent
is the full target the worker recorded (`provider:model/effort@account`, only the parts it has; one
target for every task of a fork), and
a stopped one a fix line with the complete commands that move it on (never "see the logs": the
board reads the fork's recorded failure or its log itself). A command is never cut; one that does
not fit goes on its own line. A fork's task names its fork only as `in fork <name>`; a fork's ready
or merging candidate is ONE row for the fork (`Merge fork <name> (N tasks)`, under Needs you with
`coop fork merge <name>` when ready, under Now while merging), never a row per task. Nothing is
right-aligned, and headers carry no reason text. The `forks` lines are for forks with no row. Now
and Stopped are what the header counts as working and stopped.

TODO rows never show a claim or fork attribution. Watch labels omit process IDs; an active task
still names its owner and reports when that owner's process has stopped. Keep diagnostic ownership
in `--json` and retain stored claims: a claim can exist in TODO while claim/release is transitioning.

A queue is named by the folder that holds it: omit the word `queue` and the `.agent` folder
(`portal/.agent/tasks` reads `portal`, `portal/.agent/tasks-b` reads `portal/tasks-b`). On a board
of several queues every row names its queue, first in its detail line or after an Up next title;
the project's own `.agent/tasks` is named by the project's folder name, never `root`. On a board
of one queue no row names a queue. Preserve custom queue names and diagnostic paths.

**Why:** After the watcher filled its lower half with idle `remote-* · remote-session remote_*`
rows, the user said, "I don't see why forks should be shown in tasks watch" (2026-09-02). Those
opaque ownership ids hid the task queue without describing work.
On 2026-09-12 the user added, "if task is in TODO don't show claims" and "no need to show pid in
watch" after stopped claims obscured queued task titles.
The same day: "do not show the 'queue' word here, takes too much space", "trim standard suffix",
and "dont show · root, if there is no name it's root".
On 2026-11-05 the owner's review of the real emisar board found a fork's three finished tasks as
three cut-off `ready to merge` rows while its merge was already running, rows from the root queue
naming no queue beside rows that did, and a `coop tasks release` command cut off mid-id.

**How to apply:** Treat snapshot collection and terminal presentation as separate boundaries.
Preserve every reservation in `ReadProjectSnapshot`/`--json`; in the terminal renderer, use one
shared activity predicate that excludes reservation-only state. Do not render executions at all,
and keep standalone fork rows only for lifecycle states that affect task progress.

## Changelog

- 2026-11-05 — the coordinator's review of round two: one agent form per fork and per task. Every
  lease now records the target the run used (`loop.RecordedTarget`: the rotation's target with a
  model or effort it left open filled in from config), a fork's loop notes it in `.coop/target`, a
  released lease leaves a `.holder.json` record of who last held the task, and the board names a
  fork by one target for all its tasks. A claim's state says `has ended · idle for <age>` (only
  file times are known); a fork loop's reported error reads `loop failed` with the error on up to
  two dim red lines. Swept the three lease writers (loop assignment, loop task tools, fork
  assignment; pinned by internal/loop TestRecordedTargetNamesWhatTheRunUsed); a claim's `--as` label
  (`lead`) carries no target and stays as written.
- 2026-11-05 — round two of the owner's layout (task 2026-11-05-show-fork-merges-once-name-every-
  row-s-queue-siz, spec.md), from the real emisar board: a fork's ready or merging candidate is one
  row; every row of a multi-queue board names its queue first, and the 2026-09-12 "dont show ·
  root" is now met by naming the project's own queue with the project folder name, so no row is
  ever labeled `root` and none goes unnamed; headers carry counts; Needs you and Up next fill the
  terminal height; commands are never cut and the header shortens; titles are terminal links; the
  agent is the full target from the lease record (a fork's from its loop's lease on its own copy of
  the task, else the fork's agent note). Swept watch.go's row, header, section and tail renderers,
  the homepage scene and its shapes (tools/gendocs). A fork task in final review or finished while
  its loop is gone and nothing was published now reads Stopped with the restart, where it read
  "ready to merge"; a live fork's task between two loop iterations reads `working for` from its
  loop's held lease, where it read `paused`. `coop tasks ls`, `coop fork ls` and `coop tasks
  decisions` keep their wording.
  Added the multi-queue, merge, target, height, header, width and link tests to the check.
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
