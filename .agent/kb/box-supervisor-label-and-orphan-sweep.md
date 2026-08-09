---
name: box-supervisor-label-and-orphan-sweep
description: Every box records the host process supervising it, and only a provably dead one authorizes a reap — scoped to the workspace that launched it
subsystem: box
sources: [internal/box/sweep.go, internal/box/run.go, internal/cli/boxsweep.go, internal/cli/doctor.go, internal/runtime/runtime.go]
updated: 2026-08-09
---
A box outlives the coop that started it whenever that coop dies by SIGKILL: `--rm` is the docker
CLIENT's promise, no Go `defer` runs, and no PID 1 inside the box can help (that limit, and why a
death signal cannot fix it, is [[box-orphans-survive-pdeathsig]]). Cleanup is therefore **pull-only**
— some LATER invocation has to decide "nobody owns this box" — and until 2026-08-09 nothing in a
container's labels said who would have removed it.

`internal/box/run.go` now stamps every launch with

    coop.host = v1:<workspace-scope>:<pid>:<start-token>

- **workspace-scope** — `sha256(canonical path)[:12]`, of `spec.PolicyRepo` when set, else
  `spec.Repo`. The policy repo comes first because a review/gate box mounts a *disposable* candidate
  tree that no later run could ever scan; scoping it to the durable repo keeps its orphan reachable.
- **pid + start-token** — `os.Getpid()` and `processidentity.StartToken`, i.e. the process that runs
  `docker run`. No stable token → **no label at all**: a label nobody can verify later is worse than
  none.

The decision table (`box.SurveyOrphanBoxes`), applied per container:

| recorded supervisor                            | action                     |
|------------------------------------------------|----------------------------|
| pid gone, or pid reused (different token)       | reap — the ONLY reapable   |
| alive and matching                              | untouched                  |
| unreadable identity (`Inspect` → Unknown)       | untouched (fails closed)   |
| another workspace's scope                       | untouched                  |
| no label (pre-upgrade), or a value this version can't parse | REPORTED, never reaped |

This is the fork lifecycle's `OwnerProvablyDead` doctrine (`internal/forkspace/state.go`) applied to an
identity carried by the container instead of a pidfile. **Never** container age, image, or name — the
sweep has no such input, and adding one would resurrect the bug the label exists to kill.

`box.ReapOrphanBoxes` removes by the dead supervisor's own exact label (`RemoveByLabels`), the same
plumbing `coop fork stop` uses, so the removal can only reach containers carrying that identity AND
that scope. It runs from `app.sweepOrphanBoxes` (memoized once per repo per process, since
`fleet up` starts N forks through one app) at the entry points that already reap: loop start, fork
start, `fleet up`, `coop build`/`update`'s recycle. Failure there is deliberately silent — the
command's own box work reports a broken runtime loudly, and Apple's `container` CLI has no label
inspection at all, so it would otherwise print on every start. `coop doctor` is the reporting
surface: count, ids, and the label evidence, outside its pass/fail tally, reaping nothing.

Traps:
- The sweep adds ONE runtime `ps` to loop/fork/fleet/build. The scripted process E2Es pin exact
  runtime call counts, so those suites pass `sweepsOrphanBoxes` (see `assertDirectRunContract`); a
  command that grows any OTHER runtime call still fails the count.
- The scripted fixture must answer `inspect --format {{json .Config.Labels}}` (it does, as the
  `labels` command) or the sweep silently degrades to "unattributable" in every E2E.
- A fork's boxes are scoped to the FORK's workspace, not the parent repo, so `coop fork <name>` from
  the parent never sweeps them; the fork's own loop start does, and `coop fork stop` still owns the
  exact-owner reap.

## Changelog
- 2026-08-09 — created with the supervisor label + sweep (verified against `internal/box/sweep_test.go`'s decision table and the scripted loop/fork E2Es).
