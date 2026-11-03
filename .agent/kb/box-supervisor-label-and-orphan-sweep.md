---
name: box-supervisor-label-and-orphan-sweep
description: Every box records the host process supervising it, and only a provably dead one authorizes a reap — scoped to the workspace that launched it
subsystem: box
sources: [internal/cli/acp_cmd.go, internal/cli/build_cmd.go, internal/cli/modelscache.go, internal/config/runfiles.go, internal/testutil/dockersock/dockersock.go, internal/box/sweep.go, internal/box/services_owner.go, internal/box/services.go, internal/box/filtered_services.go, internal/forkctl/rm.go, internal/box/run.go, internal/box/open_broker.go, internal/box/reap.go, internal/box/mcp_broker.go, internal/cli/boxsweep.go, internal/cli/tempreap.go, internal/cli/doctor.go, internal/runtime/runtime.go, internal/runtime/mounts.go, internal/runtime/docker.go, internal/box/native_open.go, internal/cli/testdata/providerfixture/main.go, internal/runtime/compose.go, internal/box/hostid.go, internal/box/main_test.go, internal/processidentity/identity.go]
updated: 2026-11-03
---
A box outlives the coop that started it whenever that coop dies by SIGKILL: `--rm` is the docker
CLIENT's promise, no Go `defer` runs, and no PID 1 inside the box can help (that limit, and why a
death signal cannot fix it, is [[box-orphans-survive-pdeathsig]]). Cleanup is therefore **pull-only**
— some LATER invocation has to decide "nobody owns this box" — and until 2026-08-09 nothing in a
container's labels said who would have removed it.

`internal/box/run.go` now stamps every launch with

    coop.host = v2:<workspace-scope>:<host>:<pid>:<start-token>

- **workspace-scope** — `sha256(canonical path)[:12]`, of `spec.PolicyRepo` when set, else
  `spec.Repo`. The policy repo comes first because a review/gate box mounts a *disposable* candidate
  tree that no later run could ever scan; scoping it to the durable repo keeps its orphan reachable.
- **host** — `thisHost` (`box/hostid.go`): `sha256(id + "\n" + PID namespace)[:12]`. A pid means
  something only where it was read. Before v2, one Docker daemon serving two hosts, or a host and a
  devcontainer, at the same repository path let each side's sweep read the other's live supervisors
  as gone and remove their boxes and service volumes. The id is random, made once in
  `~/.local/state/coop/host-id.json` (`{"version":1,"id":…}`; a test binary uses `serviceStateRoot`'s
  temp root), written in full and then linked into place so racing first runs agree, and never
  replaced. It is not `/etc/machine-id` or `IOPlatformUUID`: x/sys has no `gethostuuid`, so macOS
  would need IOKit or an `ioreg` subprocess, and images often lack or share a machine id. Its folder
  already holds the task leases, whose pids assume one host, and `authorityProtection` keeps it out
  of every box. It is read with `safefile.ReadRegular` (at most 4 KiB, never through a link, never
  blocking on a FIFO). The namespace is `/proc/self/ns/pid` on Linux (`processidentity.PIDNamespace`),
  none on macOS. No id or no namespace → an **empty label**, and a `HostIDError` (its file or folder
  and a one-sentence fix) that `coop doctor` shows; nothing else would.
- **pid + start-token** — `os.Getpid()` and `processidentity.StartToken`, i.e. the process that runs
  `docker run`. No stable token → an **empty label**: a label nobody can verify later is worse than
  none.

`ownerLabels` sets `coop.host` even when it is empty, as `runServiceLabels` does: an agent can write
`.agent/Dockerfile`, and Docker keeps a label the image bakes in unless the run sets it. The native
broker takes only the owner labels with a value (`nativeBrokerLabels`): an exact `DockerRef` refuses
an empty one, and coop builds that image itself.

A v1 label (`v1:<scope>:<pid>:<token>`, written by builds before this change) is still read. A coop
on a host (macOS, or Linux in the initial PID namespace, `processidentity.InitialPIDNamespace`)
judges it as before, since it has no host field, unless its token is another platform's
(`processidentity.Native`), which only another host could have written. A coop inside a container
judges none (`legacyLabelsAreLocal`): it cannot tell its own v1 labels from another container's on
the same daemon, whose live pids look gone from its namespace, so it lists them as another host's.
The reader retires on the "Box supervisor label" row of [[one-upgrade-for-old-state]], taking
`legacyLabelsAreLocal` with it.

The decision table (`box.SurveyOrphanBoxes`), applied per container:

| recorded supervisor                            | action                     |
|------------------------------------------------|----------------------------|
| this host's pid gone, or reused (different token) | reap — the ONLY reapable |
| alive and matching                              | untouched                  |
| unreadable identity (`Inspect` → Unknown)       | untouched (fails closed)   |
| another workspace's scope                       | untouched, unless a session-service run's (`coop.run=session-*`): judged like this workspace's |
| another host or PID namespace: a v2 host field, another OS's start token, or a v1 label seen by a coop inside a container | untouched; doctor lists it under "Boxes from other computers" |
| a v2 label while this coop cannot name its host (`thisHost` failed) | REPORTED, never reaped |
| no label (pre-upgrade), or a value this version can't parse | REPORTED, never reaped |
| any value on a container carrying a Compose project label | REPORTED, never reaped |

This is the fork lifecycle's `OwnerProvablyDead` doctrine (`internal/forkspace/state.go`) applied to an
identity carried by the container instead of a pidfile. **Never** container age, image, or name — the
sweep has no such input, and adding one would resurrect the bug the label exists to kill.

`box.ReapOrphanBoxes` removes by the dead supervisor's own exact label (`RemoveByLabels`), the same
plumbing `coop fork stop` uses, so the removal can only reach containers carrying that identity AND
that scope.

A `coop=broker` credential broker helper carries its box's `ownerLabels`, and is judged by the same
proof on its OWN label, with or without its box. The open run's MCP broker (`box/open_broker.go`)
runs with `--rm` and an open stdin pipe, so it exits and goes the moment its coop does; the native
broker (`box/native_open.go`, `coop-native-*`) is created without `--rm`, so a coop killed after
creating it leaves it behind, holding its `coop-network` image, while its box leaves. The four found
on one Mac on 2026-10-15 were never started and came from session runs (`coop.run=session-*`) whose
remote-fork workspaces were already gone, which no workspace-scoped sweep reached. So the survey
also reads `coop.run`, and a session run's box or broker is judged from any workspace by the same
proof (this host's supervisor, provably dead); another workspace's container is otherwise left
alone, and is never listed as this workspace's other-host or unattributed box.
`SurveyOrphanBoxes` lists every container carrying the `coop` label in ONE `ps --format` call
(`runtime.ContainerLabels` with an empty value matches any value), reading each one's kind,
supervisor and Compose labels, so a survey that finds nothing still costs exactly one runtime call:
the scripted process E2Es pin that count. A broker is removed by `{coop=broker, coop.host=<dead>}`
but not counted in the launch's "Removed N boxes" line, and a broker with a Compose label is never
removed. The open broker's private directory (`coop-broker-*`, holding the run's real credentials)
is a `tempPrefixes` entry, so `ReapOrphanTempEntries` clears one a kill left behind.
`SurveyOrphanBoxes` returns the dead supervisors' brokers beside the boxes; `coop doctor` reports
the boxes only. The same listing yields `OtherHost`: this workspace's boxes, brokers and per-run
services that another host supervises, which no sweep will ever remove, with `OtherHostProjects`
for their volumes. Doctor (`reportOrphanSurvey`, which the approved transcripts 18n and 18o render
through) lists them with the exact `rm -f` and `volume rm` commands, and `HostError` under "Box
cleanup". It runs from `app.sweepOrphanBoxes` (memoized once per repo per process) at the entry
points that already reap: loop start, fork start, and `coop build`/`update`'s recycle (which runs
only when the build replaced an image, so an unchanged build sweeps nothing). Failure there
is deliberately silent — the
command's own box work reports a broken runtime loudly, and Apple's `container` CLI has no label
inspection at all, so it would otherwise print on every start. `coop doctor` is the reporting
surface: the count and the container ids, outside its pass/fail tally, reaping nothing.

Files get a separate daily sweep (`cli.startTempReap` → `box.ReapOrphanTempEntries` for TMPDIR,
then `box.ReapOrphanRunfiles` for the private runfiles root): coop-prefixed entries that are over an
hour old AND mounted by no box (a run's folder must also pass `runFolderUnused`, below). It
starts once per process from `runBox` (direct, editor and fork launches) and `collectOrphanBoxes`
(loop and fork starts, and a build's recycle near its end);
doctor probes, eval trials and fork merge gates launch boxes without it. It stamps
`BoxHome/temp-reap` only after a pass whose mount listing succeeded (`box.ErrMountsUnknown` marks
one that did not): a goroutine dies with its process, stamping first let a short command use up the
day, and a pass that could not list removed nothing, so the next launch retries it.
`MountSourcesByLabel` skips a box removed between `ps` and its inspect only when the same listing,
run again, no longer shows its id.

A listing is one daemon's word. The runtime CLI follows DOCKER_HOST or the current docker context,
and another daemon's boxes are simply missing from it, so after `docker context use other` their
folders looked unused. Each run's folder (`coop-run-*`, the only kind current code makes that a box
mounts) therefore names its daemon in a `daemon` file. The launch writes the ID its own probe
returned (`runtime.DaemonID`, `info --format {{.ID}}`, in run.go) before it starts any service,
broker or box, and writes it again just before the box starts (`recordDaemonAtStart`): the CLI
starts the box on whichever daemon the context selects by then, and a switch while services or
brokers start would otherwise leave the folder naming a daemon that never sees the box. A
start-time probe that names no daemon writes an empty record. A native-credential run records its
owner's bound daemon (`owner.docker.Info().ID`) instead and skips the start-time probe, since that
box starts through the bound endpoint. The record is never removed once written, and a write that
fails stops the launch: an absent record means the run started no container.

The folder also names its owner, the coop process that made it, in an `owner` file written before
anything else goes in (`recordRunOwner`, the supervisor label's `v2:` form plus a newline that marks
a complete record; none when `supervisorLabelValue` is empty). The setup between the folder and the
first daemon record writes credentials, MCP configs and hooks and can take minutes, so before
2026-11-03 a run killed there left a folder no record named. One ~4-hour burst of killed runs on
2026-10-17 left 1,256 such folders on the owner's Mac (born before the binary that recorded
daemons, all with native credentials), and nothing removed them.

`runFolderUnused` is the judgment, after the hour and mount checks. A folder that names its owner
goes only when that owner is provably gone from this host (`supervisorProvablyDead`) AND its run
started no container (no daemon record), started its box on the pinned daemon, or ended at least a
day before this host's last boot (`processidentity.BootTime`; the day covers a clock set forward
after boot). An owner-less folder, as every earlier Coop made, goes when it names the pinned daemon
or ended a day before the last boot. No process outlives a reboot, nor does a box on a daemon that
can mount this host's files. A live owner, another host's, an incomplete or unreadable record, or a
box on another or an unnamed daemon keeps the folder.

A `coop acp` editor session's tool handoff folder (`coop-acp-mcp-*`, `NewSessionMCPHandoffDir` in
mcp_broker.go) lives in the same root and is judged by the same rule (`ownedFolderPrefixes`): it
names the session as its owner the same way and never records a daemon, so it goes once that session
is provably gone. The session removes it when its child stops; a killed session left 35 of them on
the owner's Mac by 2026-11-03. Those name no owner, so each goes only once it predates the last
boot by a day.

The pass (`cli.reapTemp`) pins the daemon the current context selects (`FreezeCompose`) when a
runfiles root exists: pinning makes a client folder there, and a host without one has no run
folders to judge. A frozen runtime's `MountSourcesByLabel` lists through that daemon's socket,
whatever the context says meanwhile, and fails (`ErrMountsUnknown`) unless that daemon still
answers there at the end; reading the ID before and after an unpinned listing missed a switch and
back. Only the frozen daemon's listing can prove a box gone, so with no pinned daemon a run folder
goes only on its owner and boot evidence, though the rest is still swept: `bindDocker` binds only a
local unix socket, so Docker over TCP or SSH never pins, and a `COOP_RUNTIME` Coop does not
recognize names no daemon.
Apple's container sweeps nothing at all: `FreezeCompose` returns it unchanged, its
`MountSourcesByLabel` fails, and the TMPDIR pass stops on `ErrMountsUnknown` before it removes or
stamps anything. The ID, not the endpoint: `default` and `orbstack` reach one daemon on the
developer's Mac, and Docker Desktop and OrbStack take turns behind /var/run/docker.sock. The
TMPDIR pass sweeps only kinds no box mounts (audit snapshots, test leases, earlier versions' broker
and Compose client folders); the box files earlier versions left there name no daemon, so the
sweep leaves them.

Remaining leak, until the host's next reboot plus a day: a killed run whose box was on another or
an unnamed daemon (Docker over TCP or SSH, a context switch, a daemon reset that changed its ID),
and an owner-less folder from since the last boot. `coop doctor` counts run folders over a day old
whose owner is gone or unrecorded (`box.LeftoverRunFolders`, read-only, no runtime call) under
"Leftover run folders", outside the tally. Trap: a shared or network home would let one host's
reboot rule judge another host's owner-less folders; owner records name their host and are never
judged that way.

Trap: a `coop-docker-client-*` directory (`runtime.bindDocker`) is in use for a whole run yet never
mounted. It lives in the runfiles root beside the run files and `coop-compose-client-*`, made only
once the daemon qualifies. One helper owns that root: `config.PrivateRunfiles` creates it for runs
and, with a nil Config, for the runtime (which has none; Load sets BoxHome to RootDir), and
`config.ExistingRunfiles` finds it for the sweep. Either refuses a runfiles that is a link or
someone else's; the Coop home above it may be a link. Removing a live client folder there is
harmless (Docker reads a missing one as empty; `Docker.Close` accepts it) and no other user can
recreate it, so only `ReapOrphanRunfiles` looks for the prefix. A TMPDIR sweep never does, not even
for earlier versions' leftovers: on a shared /tmp another user could recreate a live run's Docker
config. Binding tests get a config root of their own from `dockersock.ServeHandler`, never the
developer's ~/.config/coop.

**A killed run's own Compose services.** A loop's or editor session's private project
(`ComposeProjectFor(workspace, owner)`, `<ComposeProject(workspace)>-run-<8 hex>`) stayed behind
with its volumes and network whenever the run was killed: only a loop's clean exit tore it down. An
editor session now tears its own down too (`removeACPServices`, after its boxes stop): at the
supervisor's exit, and on a reload for the generation that is ending, since the next generation runs
under a new supervisor id with a new project. It never removes one while an inner child that
supervisor id started still runs (`acpChildGroups`: a process group counts as gone only on the
kernel's ESRCH), after a box sweep that failed, or for an empty id, the development project.
`StopServicesForOwner` removes the volumes even when the networks could not go, under the caller's
budget: once the containers are gone no sweep finds the project again. A model probe
(`fetchACPModelCatalog`) starts no services in open mode (`autoUpServices` skips the probe role),
and a filtered probe, whose admitted grants it must start, removes its own project afterwards
(`cleanupModelProbe`). Coop's own override (`runServiceLabels`; the filtered path's override
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
- "Inside a container" means any Linux PID namespace but the initial one (`pid:[4026531836]`).
  OrbStack's Docker engine itself runs in a child namespace, so even a `--pid=host` container there
  reads another; Coop on OrbStack machines, WSL2 or LXC counts as containerized and judges no v1
  label, which only leaves more for doctor to list.
- The sweep adds TWO runtime `ps` listings to loop/fork/build (every coop container for boxes and
  brokers, then project services). The
  scripted process E2Es pin exact runtime call counts, so those suites pass `sweepsOrphanBoxes` (see
  `assertDirectRunContract`); a command that grows any OTHER runtime call still fails the count.
  The daily temp sweep would add more when due; `procharness.NewLayout` stamps `temp-reap` fresh so
  it never is there.
- The scripted fixture must accept the survey's bare `label=coop` filter (`parsePS`: a key with no
  value matches the label's presence) or the sweep silently fails in every E2E.
- A launch's daemon probe is `info --format {{.ID}}` (`DaemonID`), not bare `info`, and a box that
  starts through the CLI makes it twice, at setup and just before the start, so a direct box run is
  four runtime invocations in the scripted E2Es. The scripted fixture accepts exactly that form
  (`daemonIDProbe`), holds host setup on it and prints an ID. An answer without an ID falls back to
  bare `info` (a CLI may print a blank one for a daemon it cannot reach), so a fixture that printed
  none would add a call per probe. Every other caller keeps `EnsureDaemon`'s bare `info`: test
  fakes answer `info --format` with a platform.
- A fork's boxes are scoped to the FORK's workspace, not the parent repo, so `coop fork <name>` from
  the parent never sweeps them; the fork's own loop start does, and `coop fork stop` still owns the
  exact-owner reap.
- Never label a per-run project with the pid of the process that happens to run `up`. Compose hashes
  labels into each service's configuration, so a label that changes between an editor session's
  inner children recreates the services on every start, and a child's exit would let a sweep take a
  live session's services and volumes.
- The host field fails closed, not open. A coop in a container gets a new PID namespace, so a new
  field, each time the container starts, and nothing ever removes what a run killed before that
  restart left behind; doctor lists it as another host's. A coop with another `$HOME` has another id
  and leaves this one's leftovers alone too. Two hosts sharing one state folder (a network home, a
  cloned disk) share the id, so only the PID namespace still tells their containers apart; two such
  hosts in their initial namespaces collide, as their task leases already do.
- `thisHost` runs once per process, so a test cannot swap its id; the sweeps take the host as an
  argument (`surveyOrphanBoxes`, `reapOrphanServices`, `supervisorLabelFor`) for the no-id tests.
  Test binaries keep the id under `ServiceStateRootEnv`, which the box, cli and forkctl `TestMain`s
  point at a fresh temp root.

## Changelog
- 2026-11-03 — editor tool handoff folders (`coop-acp-mcp-*`) name their owner and join the runfiles
  sweep under the run folder rule; `TestReapRunfilesJudgesAnEditorHandoffFolderByItsOwner` keeps a live
  session's folder.
- 2026-11-03 — a build's recycle (and the sweep it starts) runs only for an image the build
  replaced, and restarts only the supervised boxes running it (`recycleBoxes` in build_cmd.go).
- 2026-11-03 — run folders name their owner first, the daemon record is never removed, and the sweep
  judges owner, daemon and the last boot (`runFolderUnused`); doctor counts leftovers. Verified with
  `TestReapRunfilesJudgesARunFolderByItsOwnerAndDaemon` and three mutations of the rule (task
  2026-11-03-reap-leftover-per-run-folders-in-the-coop-config).
- 2026-10-18 — a session-service run's boxes and brokers are judged from any workspace (task
  2026-10-16-remove-a-dead-session-service-run-s-containers-b).
- 2026-10-17 — the read-only and bare launches are gone: `runBox` and `collectOrphanBoxes` are the
  sweep's only starters and run.go the only launch that records its daemon.
- 2026-10-17 — v2 labels name the host and PID namespace (`thisHost`); a sweep never judges another
  host's pid, and v1 labels keep their meaning for a coop on a host while a coop in a container
  judges none. Doctor lists another host's containers and a host id
  it cannot read; the id is read bounded and link-free; `coop.host` is always set (task
  2026-10-16-name-the-host-in-the-supervisor-label-so-a-share).
- 2026-10-17 — a supervisor label whose start token another operating system wrote (a Linux coop
  sharing the daemon with macOS) reads as Foreign and is left alone; it was reaped as dead before
  (task 2026-10-17-stop-judging-a-fork-pidfile-s-process-dead-becau).
- 2026-10-17 — the daily sweep judges a run's folder only by the daemon its box started on: launches
  record the daemon ID in the folder at setup and again as the box starts (the bound one for native
  runs), the pass pins one daemon for its whole listing, the TMPDIR pass sweeps only kinds no box
  mounts, and a run folder no pass can prove unused leaks (no record, Docker over TCP or SSH, a
  daemon that never lists again); Apple's container sweeps nothing (task
  2026-10-15-tie-the-daily-temp-sweep-to-the-docker-daemon-th; verified against `reap_test.go`,
  `tempreap_test.go`, `mounts_test.go` and `runtime_test.go`).
- 2026-10-16 — model probes start no services in open mode; a filtered probe removes its own project.
- 2026-10-16 — an editor session removes its own per-run services at exit and on reload, never while one
  of its inner children runs (task 2026-10-16-editor-sessions-remove-their-own-services-when-t).
- 2026-10-16 — brokers are judged on their own supervisor label, so a native broker whose box is
  gone is removed too; the survey reads every coop container in one `ps --format` listing, with no
  per-container inspect (task 2026-10-15-sweep-orphan-helper-containers-and-superseded-im).
- 2026-10-16 — Docker client folders moved from TMPDIR to the private runfiles root; the private-TMPDIR
  special case is gone and only ReapOrphanRunfiles reaps them; the root's one helper is
  internal/config/runfiles.go (a linked Coop home is fine, a linked or foreign runfiles is refused);
  fake-daemon tests get their own XDG_CONFIG_HOME.
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
