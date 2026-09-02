---
name: task-watch-shows-work-not-workspace-ownership
description: tasks watch renders task-affecting activity, never an idle workspace reservation by itself
scope: cli-output
sources: [internal/tasks/watch.go, internal/tasks/watch_test.go]
check: go test ./internal/tasks -run 'TestReservationOnlyForkIsNotTaskWatchActivity|TestReservedForkWithAssignmentRendersTaskActivity|TestTaskWatchKeepsForkWorkVisibleWithoutAnExecution|TestTaskWatchRendersSandboxAsLabeledBlock'
updated: 2026-09-02
---

# Show work in tasks watch, not idle workspace ownership

`coop tasks watch` answers what task work is happening. A remote-session reservation alone is
durable ownership of a reusable workspace, not activity: it does not render a `forks` row and does
not make an otherwise empty board visible. Keep the reservation in the project JSON snapshot and
all lifecycle checks. If that same fork has an assignment, worker startup, detached loop, cleanup,
candidate, land, or execution, show the task-affecting state without exposing the session owner id.

**Why:** After the watcher filled its lower half with idle `remote-* · remote-session remote_*`
rows, the user said, "I don't see why forks should be shown in tasks watch" (2026-09-02). Those
opaque ownership ids hid the task queue without describing work.

**How to apply:** Treat snapshot collection and terminal presentation as separate boundaries.
Preserve every reservation in `ReadProjectSnapshot`/`--json`; in the terminal renderer, use one
shared activity predicate that excludes reservation-only state. Render executions under
`sandboxes`, and keep standalone fork rows only for lifecycle states that affect task progress.

## Changelog

- 2026-09-02 — created after sweeping every `snapshot.Forks` and `fork.Reservation` consumer.
  The two terminal visibility predicates and reservation-first label were the only presentation
  violations; lifecycle status, workspace cleanup, and JSON snapshot consumers remain unchanged.
