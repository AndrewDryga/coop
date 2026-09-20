---
name: box-supervisor-label-and-orphan-sweep
description: Every box records the host process supervising it, and only a provably dead one authorizes a reap — scoped to the workspace that launched it
subsystem: box
sources: [internal/box/sweep.go, internal/box/run.go, internal/box/open_broker.go, internal/cli/boxsweep.go, internal/cli/doctor.go, internal/runtime/runtime.go]
updated: 2026-09-19
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
that scope.

A dead supervisor's `coop=broker` MCP credential broker helper (`box/open_broker.go`, same
`ownerLabels`, holding that run's MCP secrets) is removed with its box — but only once a box of that
supervisor is already proven orphaned, so a sweep that finds nothing still costs exactly ONE
listing: the scripted process E2Es pin that count. Ordinarily the helper never gets that far: it
runs with an open stdin pipe, so it exits and `--rm` removes it the moment its coop does. Its
private directory (`coop-broker-*`, holding the run's real credentials) is a `tempPrefixes` entry,
so `ReapOrphanTempEntries` clears one a kill left behind. `SurveyOrphanBoxes`, and so `coop doctor`,
reports boxes only. It runs from `app.sweepOrphanBoxes` (memoized once per repo per process) at the entry
points that already reap: loop start, fork start, and `coop build`/`update`'s recycle. Failure there
is deliberately silent — the
command's own box work reports a broken runtime loudly, and Apple's `container` CLI has no label
inspection at all, so it would otherwise print on every start. `coop doctor` is the reporting
surface: count, ids, and the label evidence, outside its pass/fail tally, reaping nothing.

Post-build recycling is a separate authority boundary. It uses a bounded, error-reporting running
container query to preserve the old-image notice, then `RemoveByLabel` to restart supervised boxes.
A true no-match is success; query or partial-removal failure makes `build`/`update` fail with the
already-built image called out explicitly. Do not reintroduce best-effort count/kill helpers there.

Traps:
- The sweep adds ONE runtime `ps` to loop/fork/build. The scripted process E2Es pin exact
  runtime call counts, so those suites pass `sweepsOrphanBoxes` (see `assertDirectRunContract`); a
  command that grows any OTHER runtime call still fails the count.
- The scripted fixture must answer `inspect --format {{json .Config.Labels}}` (it does, as the
  `labels` command) or the sweep silently degrades to "unattributable" in every E2E.
- A fork's boxes are scoped to the FORK's workspace, not the parent repo, so `coop fork <name>` from
  the parent never sweeps them; the fork's own loop start does, and `coop fork stop` still owns the
  exact-owner reap.

## Changelog
- 2026-09-19 — the reap also covers an open run's `coop=broker` helper, by the same supervisor label.
- 2026-09-03 — replaced post-build's silent count/kill path with bounded diagnostic queries and
  exact-label removal; verified the pull-only orphan sweep remains intentionally best-effort.
- 2026-08-25 — removed Fleet from the current sweep-entry and runtime-call inventory after the
  command family was retired; direct loop/fork/build entry points keep the same scoped reap.
- 2026-08-09 — created with the supervisor label + sweep (verified against `internal/box/sweep_test.go`'s decision table and the scripted loop/fork E2Es).
