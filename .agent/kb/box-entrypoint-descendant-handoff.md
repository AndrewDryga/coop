---
name: box-entrypoint-descendant-handoff
description: Supervised loop/review boxes authenticate forwarder exemptions and hand off live detached jobs
subsystem: box
sources: [internal/box/image.go, internal/box/run.go, internal/loop/loop.go, internal/loop/ratelimit.go]
updated: 2026-08-10
---

`RunSpec.SuperviseDescendants` is set only for loop and review iterations. In that mode
`coop-entry` launches the provider and each sidecar forwarder in separate sessions, then scans
`/proc` after a successful provider exit. It treats all non-zombie processes as agent-owned except
PID 1, the entrypoint's transient direct children, and a forwarder session whose leader still has
the recorded PID plus Linux start token. PPID or session walks alone are insufficient because a
provider can double-`setsid` and reparent its work.

The supervisor performs one short quiescence rescan before accepting a clean exit, because a
detached leader can replace its short-lived launcher between `/proc` reads. A seen job that exits
before the deadline returns exit 190; a job still live at the deadline is sent TERM then KILL and
returns 191. A failed provider terminates any remaining agent-owned jobs before preserving its real
exit. The entrypoint remaps raw provider use of either reserved code to ordinary failure.

Host loop and review handling records the reserved codes separately as `background_drained` and
`background_timeout`. It restores a premature work completion or discards a review receipt, then
bounds consecutive fresh attempts at three without charging ordinary process-failure or no-progress
strikes. Interactive boxes retain the normal entrypoint `exec` path.

## Changelog

- 2026-07-26 — created after adding supervised descendant handoff; verified against the listed sources.

- 2026-08-10 — sources repointed: the loop engine moved out of `internal/cli` into `internal/loop` (`ratelimit.go` whole,
  `commands.go`'s handoff retry into `loop.go`/`review.go`); re-verified, both unchanged.
