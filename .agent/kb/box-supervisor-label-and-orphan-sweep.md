---
name: box-supervisor-label-and-orphan-sweep
description: Every box records the host process supervising it, and only a provably dead one authorizes a reap — scoped to the workspace that launched it
subsystem: box
sources: [internal/box/sweep.go, internal/box/services_owner.go, internal/box/services.go, internal/box/filtered_services.go, internal/forkctl/rm.go, internal/box/run.go, internal/box/open_broker.go, internal/box/reap.go, internal/cli/boxsweep.go, internal/cli/tempreap.go, internal/cli/doctor.go, internal/runtime/runtime.go, internal/runtime/mounts.go, internal/runtime/docker.go]
updated: 2026-10-16
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
| any value on a container carrying a Compose project label | REPORTED, never reaped |

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
surface: the count and the container ids, outside its pass/fail tally, reaping nothing.

Files get a separate daily sweep (`cli.startTempReap` → `box.ReapOrphanTempEntries`): coop-prefixed
entries in TMPDIR and the private runfiles root that are over an hour old AND mounted by no box. It
starts once per process from `runBox` (direct, editor and fork launches), the readonly/bare launches,
and `collectOrphanBoxes` (loop and fork starts, and a build's recycle near its end); doctor probes,
eval trials and fork merge gates launch boxes without it. It stamps `BoxHome/temp-reap` only after a
pass whose mount listing succeeded (`box.ErrMountsUnknown` marks one that did not): a goroutine dies
with its process, stamping first let a short command use up the day, and a pass that could not list
removed nothing, so the next launch retries it. `MountSourcesByLabel` skips a box removed between
`ps` and its inspect only when the same listing, run again, no longer shows its id. Trap: a
`coop-docker-client-*` directory is in use for a whole run yet never mounted, so it is reaped only
from a directory no other user can write (on a shared /tmp another user could recreate a live run's
Docker config), and `Docker.Close` accepts one already removed.

**A killed run's own Compose services.** A loop's or editor session's private project
(`ComposeProjectFor(workspace, owner)`, `<ComposeProject(workspace)>-run-<8 hex>`) stayed behind
with its volumes and network whenever the run was killed: only a loop's clean exit tore it down, and
an editor session's never did. Coop's own override (`runServiceLabels`; the filtered path's override
carries the same block) now gives every service `coop=service` and a `coop.host` label naming the
process whose exit ends the run, scoped to the workspace that names the project
(`serviceSupervisor`): the loop itself, or an editor session's supervisor, the parent of the inner
children that come and go while the session's project stays. The override is merged last, so a label
the project's file planted is replaced, with an empty host when the process has no stable identity
(checked with real Compose, map and list forms; Compose also replaces a planted
`com.docker.compose.project`). The loop/fork/build start sweep's `ReapOrphanServices` lists every
`coop=service` container in ONE `ps --format` call (each line a JSON array, so a label value cannot
forge a line) and removes a project only when its label's scope is this workspace, its supervisor is
provably dead and its name is exactly one of this workspace's per-run names (`runProject`), by exact
labels, then the project's volumes; `ReapOrphanNetworks` takes the network once nothing is attached.
Each sweep step has its own time budget. `coop fork rm` runs the same reap for the fork's workspace,
which no later sweep reaches. Coop never labels the development project `coop up` runs, and the
sweep never takes it, whatever its file plants, even when a checkout's name makes its project name
look like a run's. Coop never puts a Compose label on a box, so `SurveyOrphanBoxes` reports a
`coop=box` container that carries one instead of reaping it: a development service whose file
planted a dead supervisor, or a box whose image carries the label. Removal still matches by the
exact `coop.host` value, so it also relies on a box being unable to learn its own supervisor's host
pid and start token (no environment variable or mounted file carries them). Services started before
this label stay untouched (remove those with `docker compose -p <project> down -v`).

Post-build recycling is a separate authority boundary. It uses a bounded, error-reporting running
container query to preserve the old-image notice, then `RemoveByLabel` to restart supervised boxes.
A true no-match is success; query or partial-removal failure makes `build`/`update` fail with the
already-built image called out explicitly. Do not reintroduce best-effort count/kill helpers there.

Traps:
- The sweep adds TWO runtime `ps` listings to loop/fork/build (boxes, then project services). The
  scripted process E2Es pin exact runtime call counts, so those suites pass `sweepsOrphanBoxes` (see
  `assertDirectRunContract`); a command that grows any OTHER runtime call still fails the count. The daily temp sweep would add
  more when due; `procharness.NewLayout` stamps `temp-reap` fresh so it never is there.
- The scripted fixture must answer `inspect --format {{json .Config.Labels}}` (it does, as the
  `labels` command) or the sweep silently degrades to "unattributable" in every E2E.
- A fork's boxes are scoped to the FORK's workspace, not the parent repo, so `coop fork <name>` from
  the parent never sweeps them; the fork's own loop start does, and `coop fork stop` still owns the
  exact-owner reap.
- Never label a per-run project with the pid of the process that happens to run `up`. Compose hashes
  labels into each service's configuration, so a label that changes between an editor session's
  inner children recreates the services on every start, and a child's exit would let a sweep take a
  live session's services and volumes.

## Changelog
- 2026-10-16 — the box survey skips Compose containers, so labels a development project's file plants
  never reach the box sweep (task 2026-10-16-keep-planted-coop-labels-on-development-services).
- 2026-10-16 — per-run Compose services carry the label of the loop or editor-session supervisor that
  owns them; the loop/fork/build start sweep removes a provably dead run's services and volumes (task
  2026-10-15-clean-up-per-run-services-that-killed-loops-leav).
- 2026-10-15 — added the daily temp sweep: it rides the listed launch paths, stamps only after a
  successful mount listing, skips a provably vanished box, and reaps Compose and (only from a
  private temp dir) Docker client dirs; verified against `internal/cli/tempreap_test.go`,
  `internal/box/reap_test.go` and `mounts_test.go`.
- 2026-09-19 — the reap also covers an open run's `coop=broker` helper, by the same supervisor label.
- 2026-09-03 — replaced post-build's silent count/kill path with bounded diagnostic queries and
  exact-label removal; verified the pull-only orphan sweep remains intentionally best-effort.
- 2026-08-25 — removed Fleet from the current sweep-entry and runtime-call inventory after the
  command family was retired; direct loop/fork/build entry points keep the same scoped reap.
- 2026-08-09 — created with the supervisor label + sweep (verified against `internal/box/sweep_test.go`'s decision table and the scripted loop/fork E2Es).
