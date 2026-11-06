---
name: box-entrypoint-descendant-handoff
description: Supervised loop/review boxes authenticate forwarder exemptions and hand off live detached jobs
subsystem: box
sources: [internal/box/image.go, internal/box/run.go, internal/loop/loop.go, internal/loop/ratelimit.go]
updated: 2026-11-06
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

A daemon is not work that finishes (`daemon_jobs`): a job that left the provider's session with
stdin, stdout and stderr all on `/dev/null`, as a double-forked daemon such as `epmd -daemon`
leaves itself, or one named on a short list (`epmd`, `ssh-agent`, `gpg-agent`, `dbus-daemon`;
names only ever stop a job sooner). It never earns a handoff and never counts toward the drain. It
stays while other jobs run, since they may use it, and once only daemons are left they get 10
seconds, then TERM/KILL with a line naming them; with no other job seen the exit stays 0. Work in
the provider's session, or that still writes to a pipe or file (a `setsid setsid sh -c` job or a
client's detached background shell), keeps the full window.

Host loop and review handling records the reserved codes separately as `background_drained` and
`background_timeout`. It restores a premature work completion or discards a review receipt, then
bounds such fresh attempts at three for the task (a provider timeout between them does not reset
the count) without charging ordinary process-failure or no-progress strikes. At the bound the loop
stops with exit 1, never the reserved code. Interactive boxes retain the normal entrypoint `exec`
path.

## Changelog

- 2026-11-06 — daemons left behind are stopped after a 10s grace instead of drained for 900s, with no
  handoff (an emisar fork idled 15 minutes on a test's `epmd -daemon`, 2026-11-05).

- 2026-10-17 — the handoff bound is not reset by a timeout in between, and the loop stops at it with
  exit 1, not 190 or 191 (task 2026-10-17-exit-1-not-the-provider-s-own-code-when-coop-loo).
- 2026-07-26 — created after adding supervised descendant handoff; verified against the listed sources.

- 2026-08-10 — sources repointed: the loop engine moved out of `internal/cli` into `internal/loop` (`ratelimit.go` whole,
  `commands.go`'s handoff retry into `loop.go`/`review.go`); re-verified, both unchanged.
