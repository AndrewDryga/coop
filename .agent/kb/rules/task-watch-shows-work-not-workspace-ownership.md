---
name: task-watch-shows-work-not-workspace-ownership
description: tasks watch renders task-affecting activity, never an idle workspace reservation by itself
scope: cli-output
sources: [internal/tasks/watch.go, internal/tasks/watch_test.go, internal/tasks/snapshot.go, internal/tasks/dir.go, internal/tasks/priority_test.go, internal/tasks/loop_note.go, internal/box/service_launch_wait.go, internal/tasks/owner.go, internal/tasks/session_claim.go, internal/cli/session_tasks.go, internal/tasks/lease.go, internal/forkctl/meta.go, internal/forkspace/gitcommand.go, internal/loop/rotation.go]
check: go test ./internal/tasks -run 'TestReservationOnlyForkIsNotTaskWatchActivity|TestReservedForkWithAssignmentRendersTaskActivity|TestTaskWatchKeepsForkWorkVisibleWithoutAnExecution|TestTaskWatchDropsTheStandaloneBoxInventory|TestTaskWatchHidesTodoClaims|TestTaskWatchOmitsClaimProcessIDs|TestTaskWatchCompactQueueLabels|TestTaskWatchSaysWhatAForkIsDoingWithItsTask|TestTaskWatchListsAForkOnceOnItsTaskRow|TestTaskWatchShowsDecisionsAndTodaysWork|TestTaskWatchCountsAPersonsClaimAsWork|TestTaskWatchBoardMatchesTheApprovedLayout|TestTaskWatchEdgeCaseRows|TestTaskWatchPlacesEveryFactByOneRule|TestTaskWatchCapsTheListsNeverTheWork|TestForkStopReasonReadsTheRecordThenTheLog|TestTaskWatchMultiQueueBoardMatchesTheApprovedLayout|TestTaskWatchNamesEveryRowsQueueOnlyWhenThereAreSeveral|TestTaskWatchShowsAForksMergeOnce|TestTaskWatchNamesTheFullTargetTheWorkerRecorded|TestTaskWatchFillsTheTerminalHeight|TestTaskWatchHeaderShortensInsteadOfBeingCut|TestTaskWatchTitleUsesAvailableTerminalWidth|TestTaskWatchLinksTitles|TestTaskWatchNamesEveryTaskOfAForkTheSameWay|TestTaskWatchShowsAForkLoopsErrorOnItsOwnLines|TestTaskWatchGroupsStoppedTasksByTheirFix|TestTaskWatchLiveFrameFitsTheTerminal|TestTaskWatchRowColors|TestLoopNoteSaysAStopOrAWaitOnlyWhileItIsTrue|TestTheSnapshotReadsTheNoteOfTheLoopThatHeldTheTask|TestTaskWatchSaysWhileATasksBoxWaitsToStartItsServices|TestTaskWatchShowsAForksRecordedLoopFailure|TestASessionClaimNamesTheSessionsTarget|TestTaskWatchListsUpNextInTheOrderTheLoopClaims'
updated: 2026-11-06
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
board reads the fork's recorded failure or the loop's own `✗` line in its log, never an agent's
indented, quoted tool failure, and none after the loop stopped for a decision). Stopped tasks that
share a fix are one group with the fix printed once after its last row; a task a person stopped
reads `stopped by you` with a dim `‖`, and a usage-limit wait or a box waiting to start its
services is a yellow Now row, each from a record the loop or box keeps only while it is true
(never from its narration); a fork task finished
before its loop stopped for another task's decision waits under Needs you, never Stopped. The live
board never runs past the terminal's height. A command is never cut; one that does
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

Up next lists todo tasks in the order the loop claims them: the loop's own `claimOrder` over the
queues in their configured order (priority, then queue, then folder name), never a sort of the
board's own. A high or low task is marked after its title as `coop tasks ls` marks it.

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

- 2026-11-06 — Up next listed todo tasks by folder name across all queues while the loop claims
  by priority, then queue by queue, so the board could name another next task. It now ranks them
  with the loop's `claimOrder` (`claimRanks`) and marks `High priority` / `Low priority`. Swept
  the other todo orderings: `coop tasks ls`, `QueueState` and `TaskTreeCounts` already use
  `byPriority`; a fork loop's candidates order several queues by path, not configuration order
  (left for its own task). Added TestTaskWatchListsUpNextInTheOrderTheLoopClaims.
- 2026-11-05 — a claim an agent session makes through its task tools records the session's full
  target (`TaskOwnerRecord.ActorTarget`, from `loop.RecordedTarget` over the session's config), and
  the board names the claim by it. Swept the claim writers: the CLI's `coop tasks claim` (a person,
  or `--as`) records none and keeps the person's name or label, since nothing tells Coop that
  agent's target and a preset's lead is a fallback, not one target. Added TestASessionClaimNamesTheSessionsTarget.
- 2026-11-05 — a fork's recorded loop failure is snapshot data (`ProjectForkSnapshot.LoopError`,
  `loop_error` in `--json`), set only while nothing runs the fork's loop, and the board's stop reason
  reads it there. Swept the readers of `.coop/loop-error`: `forkStopReason` no longer opens it, and
  `coop fork ls` keeps its own. Added TestTaskWatchShowsAForksRecordedLoopFailure.
- 2026-11-05 — the spec's last three edge cases: a loop run's note beside its leases (`NoteLoopRun`:
  stopped by a person at the signal, a usage-limit wait until a time; the next run replaces it, and
  only the run that last held a task speaks for it) and a box's `launch-waiters/` note. Swept every
  usage-limit sleep in internal/loop: the work wait and the rotation's all-limited wait (reviews
  share it) note it; the preflight wait and a single-target review's wait hold no task in progress.
  `requestStop` is the loop's only stop path, for Ctrl-C and coop fork stop's SIGTERM alike. A stopped-by-you row shares a group with other rows that one command moves on; a group's
  `+ N more` names what they share only when every row shares it.

- 2026-11-05 — round three, from the coordinator's look at the real boards: the log fallback read
  an agent's failed `make` (`  ✗ "sh -c …" (exit 2): …`) as the fork loop's error although the loop
  had stopped for a decision, and 11 stopped tasks scrolled the header off a 24-row terminal.
  The fallback reads only the loop's own column-0 `✗` lines and none after `Stopped for your
  decision`; the fork's finished task reads `done in fork <name>, waiting for the fork's other task`
  under Needs you; a wrapped error never leaves one short word on a line; Stopped and Needs you
  share the height after Now (three rows each, Up next two), and stopped tasks group by their fix.
  Swept every `lastLoggedError` and `failureLines` caller (watch.go only) and every section's
  height accounting (`sectionShown`). Added the grouping and terminal-fit tests to the check.

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
