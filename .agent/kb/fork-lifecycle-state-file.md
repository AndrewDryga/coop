---
name: fork-lifecycle-state-file
description: one generation-bound owner-v2 file names a fork's detached worker by pid+start token; the parent writes it under the lock its child must take first; unreadable bytes stay held and only pid+start-token — never file age — may decide that worker is gone
subsystem: fork
sources: [internal/forkspace/state_root.go, internal/forkspace/legacy_records.go, internal/box/authority_mounts.go, internal/forkspace/forkspace.go, internal/forkspace/create.go, internal/forkspace/state.go, internal/forkspace/generation.go, internal/forkspace/execution.go, internal/forkctl/supervise.go, internal/forkctl/merge.go, internal/forkctl/status.go, internal/tasks/snapshot.go, internal/tasks/watch.go, internal/cli/cli.go, internal/cli/fork_cmd.go, internal/sessionsvc/workspace.go, internal/processidentity/identity.go]
updated: 2026-10-19
---
A fork's detached loop is recorded in ONE small file, `<name>.pid` in the project's fork records
folder (`forkspace.StateDir`, `~/.local/state/coop/forks/<key>/`), read and
written only through `forkspace.WorkerState` (`internal/forkspace/state.go`). It has one shape:
`owner-v2\ngeneration=<id>\n<pid>\n<token>\n` — the worker's pid, its kernel start token (a pid
reused after a crash or a reboot belongs to another process) and the fork generation it serves.
`owner-v1` (no generation) is still parsed so an old worker can be stopped. The file exists only
while a worker runs or after one died; there is no start or cleanup state, because the per-fork
flock already covers both windows.

## Reading it

Every read of its bytes goes through `ReadWorkerState`, forkctl's included; `ClearPidIfMine`
compares the same bounded bytes, and `NeedsStop` only asks whether the name exists. Records an
older build kept in `<repo>-forks/.coop`, where a box whose mount held that folder (another
project's, through a nested repository or a non-git parent folder) could put anything at the name,
move in as they are, so `OpenStateFile` opens it with `O_NOFOLLOW|O_NONBLOCK`: a
link fails with ELOOP, a socket with ENXIO (Linux) or EOPNOTSUPP (macOS), and fstat must say
regular. The record read then stops past 4 KiB (a real record is about 150 bytes). Each refusal
matches `ErrNotAStateFile` and names what Coop found ("… is a named pipe", "… is 1048576 bytes,
larger than any record"); it is deliberately not an `*os.PathError`. The fork stays held
(`RunningPid` is 0, `StateOwner` and `NeedsStop` hold it), and commands print the guarded recovery
below with `ls -ld` as the inspection: `sed` would wait on a FIFO, and `ls -l` lists a folder's
contents. Its remove step is `rmdir` for a folder, `rm` otherwise. An I/O failure stays an
`*os.PathError`, which is how `CheckWorkerStateFormat`, `requireNoWorkerLocked` and `ForkStop` tell
it from a record Coop cannot own.

## Start: the parent writes, the child confirms

`DetachForkLoop` (`internal/forkctl/supervise.go`) holds the flock from its check until the child's
record exists. `requireNoWorkerLocked` refuses while any record is present: a live worker "already
has a loop running"; any other record (a dead worker's, or one Coop cannot verify) goes through
`coop fork stop` first. Then it
starts `coop fork <name> <who> --loop --_detached`, writes the child's pid, token and generation
(`forkspace.WritePid`) and only then unlocks; a failed write kills and reaps the child. The child's
first act (`forkspace.ConfirmWorker`, called from `forkCreate`) is to take the same flock and go on
only if the record names its own pid and start token. A parent that died before writing, or a stop
that removed the record first, leaves nothing to confirm, so the child exits before it touches
metadata, the queue, the workspace or the runtime; `cli.Main` also skips update and temp
housekeeping for this re-exec. A confirmed child defers `ClearPidIfMine`, which removes only its
byte-exact record and gives up, rather than wait, while stop holds the flock.

## Stop

`ForkStop` holds the flock throughout. It signals only a verified live worker (TERM, then KILL, to
its session group, re-checking the token before each signal) and refuses an unverifiable identity
with the manual `ps`/`kill` recovery. A record another operating system wrote (a coop in a Linux
container sharing the folder with macOS, or the reverse) is refused before the runtime is touched,
naming the system whose coop can check that pid (`forkForeignWorker`); a detached start refuses it
the same way. Then it reaps the boxes labelled with the fork's exact owner, generation and worker
flag, retires provably dead `fork-loop` sandbox records of that generation after their own exact
runtime reap, and removes the file last. Any failure leaves the record as it was: a dead worker's
record is the retry handle, so a new start refuses it and the next stop finishes. Live, unknown,
other-system, foreign-generation and foreground/editor records stay held. With no file, stop still
reaps provably dead execution records of the exact generation. Nothing else clears a dead worker's
record: it may still own an orphaned box.

`coop fork ls` (`gatherForkStatus`, `internal/forkctl/status.go`) lists a fork whose record another
system wrote as `elsewhere`, "background loop started on Linux", ahead of `unknown` and `cleanup`:
the loop's own sandbox record reads as unverifiable here too, and stop cleans nothing while the
record stands. Its `--json` names the system in `detached_system` (present only then); the record
itself never makes `cleanup_pending` true, though a dead local sandbox of the same fork still does,
and the listing shows `elsewhere` until the record goes. The task snapshot (`ReadProjectSnapshot`,
`internal/tasks/snapshot.go`) asks the same `forkspace.WorkerElsewhere` and agrees: the fork gets
`DetachedSystem` (`detached_system`), never `CleanupPending`, so `coop tasks watch` shows
"background loop started on Linux" and its `--json` matches fork ls. Neither counts that loop as
running, so it does not hold a drained board open.

## Unsupported state never becomes an identity

`ParseWorkerState` accepts only `owner-v1`/`owner-v2` records naming a pid above 1 and a start
token of a known shape (`processidentity.Stable`; another system's token passes and is judged
Foreign). Headerless or malformed bytes fail for a missing supported owner header, a bad pid or a
missing or unknown token, and `Marshal` refuses the same, so no writer publishes what the readers
refuse. A pid without its token cannot say which kernel it was read on, so a record like that is
malformed rather than a worker stop could prove gone. An unknown `owner-*` header keeps a distinct
unsupported-version error. Neither becomes a `WorkerState`.
`forkctl.CheckWorkerStateFormat` refuses before start, recreate, merge, removal or stop probes the
runtime or changes workspace metadata, and the locked start and stop parse again. `RunningPid`
returns 0 for such bytes while `NeedsStop` and `StateOwner` stay held; Coop never infers or signals
a pid from invalid bytes. The start reservations and cleanup markers earlier builds wrote
(`start-claim`, `start-launched`, `reap-pending`; last released in 11.3.0) now read as malformed,
and there is no reader for them. A `reap-pending` record can name a worker that still runs, so the
refusal (`workerStateFormatError`) prints the guarded recovery in order: list the fork's loops
with `pgrep -fl -- '[f]ork <name> .*--_detached'` (the bracket keeps pgrep's own shell out), stop
only one whose working directory is this repository (the command line names no repository),
escalating TERM to KILL, `rm --` the file, then `coop fork stop <name>`, whose no-file path
reaps the box through the dead worker's activity record. Skipping the check stays guarded: that path refuses while the activity record is live, and
`BeginExecution` refuses a second loop controller for the fork.

## The one liveness rule

`forkspace.OwnerProvablyDead` is the single definition, over a `forkspace.ProcessIdentity`:
**Gone** (the kernel says the pid is dead) or **Mismatch** (the pid now belongs to another process).
A live Match, an unreadable identity (Unknown: no stable token, or `/proc` and sysctl unreadable)
and a token of the other platform's shape (Foreign, decided before any pid is probed: a Linux pid
means nothing to macOS's kernel, nor the reverse; `processidentity.Native`) are all unproven and
fail closed. A Linux token names the PID namespace its pid was read in (`linux-proc-v2:<inode>:`),
and one from another namespace, a container's sharing the folder with the host or the reverse, is
Foreign too, refused as "started on another container or host"; a v1 token an earlier coop wrote
names none and is judged as before (`processidentity.Same` compares the two shapes). `ProcessIdentityOf` is `processidentity.Inspect` asked through this package's
`SignalPID` and `ReadProcStartToken` (`processidentity.Kernel`), so fork tests can stub liveness
and the fork lifecycle, claims, leases, network records and the box sweep share one answer: a pid
of 1 or below, or a token of no known shape, is Unknown before any pid is asked, never Gone. There
is no mtime, age or "it's been a while" anywhere in this
subsystem, and adding one is a rule violation, not an optimization: file age cannot tell a wedged
host from a busy one. `forkspace.StateOwner` wraps it for "may I touch this fork?": only a missing
record or a provably dead worker answers "not held". `recoverInterruptedRebase`
(`internal/forkctl/merge.go`) aborts a crashed land's leftover rebase only then, because the abort
resets that worktree; `mergeOne` already refuses any fork with a record, so this guard is defense
in depth — keep it.

## Who owns which half

The FILE is `internal/forkspace`'s: paths, the flock (`LockState`, `LockStateContext`,
`TryLockState`), the log open (`OpenLog`), the wire format (`WorkerState`, `ParseWorkerState`,
`Marshal`, `Read/WriteWorkerState`, `RemoveWorkerState`, `WritePid`, `ConfirmWorker`,
`ClearPidIfMine`) and the identity doctrine
(`ProcessIdentityOf`, `OwnerProvablyDead`, `StateOwner`, `RunningPid`, `NeedsStop`,
`WorkerElsewhere`). It is a leaf —
no runtime, no `ui` — so fork commands and the sessions service (which checks `NeedsStop` and takes
the lock through `LockState` and `LockStateContext`) read one contract. SUPERVISION is
`internal/forkctl`'s: `requireNoWorkerLocked`, `DetachForkLoop`, `recordStartedFork`, `ForkStop`
and `ForkContainerOwner`. If it decides what to do
to a process or a container, or prints, it is supervision.

## Around the file

- Fork records live in Coop's private folder, `~/.local/state/coop/forks/<key>/` (`StateDir`;
  `<key>` is the SHA-256 of the project's real path in the spelling the disk stores,
  `fsidentity.StoredPath`, so `~/projects/APP` and `~/Projects/app` on a Mac share one folder),
  which no box can mount. `<repo>-forks` holds
  only workspaces and `.discarding`, and a box may mount it. The records folder is a real `0700`
  directory. New lifecycle, generation, execution, reservation, assignment, candidate, proposal,
  land, discard, lock and log records are `0600`; opening an existing lock or log also tightens its
  mode. A fork's anchor is a hard link, so a fork needs the forks folder and the private folder on
  one disk (`requireOneDisk` refuses otherwise, naming both).
- Builds up to v11.3.0 kept the records in `<repo>-forks/.coop`, which a box could write. The first
  open by a later build moves them (`MoveLegacyForkRecords`, internal/forkspace/legacy_records.go,
  which every box launch also runs before the box starts): the one time Coop trusts the old place.
  It opens the forks folder and `.coop` following no link (`openForkHome`), so a box cannot point it
  at another project's records, and a link or other non-folder at either place is nothing to move:
  the project gets its empty folder and its box launches go on, while fork commands in the forks
  folder refuse it; copies each record into folders it makes in a staging folder, so a
  box that held an old folder or file open keeps writing the old copy; moves generation anchors
  (each must stay one file with its workspace marker) and logs; publishes with a no-replace rename;
  then removes the old folder, unless anything had to stay in it (an entry past the bounds, a
  special file), which keeps it whole. One move runs at a time (`forks/.moving.lock`). A project with
  nothing to move gets an empty folder at once, so a `.coop` planted later is never read. On
  another disk an anchor cannot move: it stays, and so does the whole old folder, for an older
  build. Trap: a project nobody has opened since the upgrade still has its old place open to a box.
- `<repo>-forks` still matters: fork commands work in it by name. A writable mount that contains a
  project's own forks folder is refused (`checkAuthoritySource`, `forkHomes`), as it was while
  `.coop` lived there, and `Destroy` removes a fork through the folder's no-follow handle.
- `openStateDir` (behind `EnsureStateDir`) opens the private root, `forks` and the project's folder
  without following a link, and the lock, the log and the worker record are created, renamed and
  removed only relative to that handle (`openStateFile`, the readers' open with a folder handle). A
  lock or log name that holds a link, pipe, folder, socket or device is refused as
  `ErrNotAStateFile` before any mode change; start and stop then say to remove it.
- The host-owned `<name>.generation.json` binds name and generation to a private two-link anchor
  ([[identity-fences-compare-the-inode]]). Workers, runtime labels, executions, assignments,
  candidates and cleanup all carry that exact identity, so `--fresh` cannot rebind a stale
  namesake. Generation publication is crash-replayable: a session-create retry adopts a published
  record with its anchor and workspace. Reservation rollback retires the generation before deleting the workspace.
- Workspace creation is all-or-nothing before generation authority exists: `SetupContext` keeps the
  clone only once `HEAD` is on the requested branch, Git identity and signing settings have
  propagated, and `.coop/` plus the root identity marker are excluded. Discovery treats a missing
  workspace or state root as empty and returns any other directory error; only prompt decoration
  and shell completion hide it.
- Merge holds the flock across its final check, fetch, rebase, gate, land and reconciliation, then
  takes it again for post-land removal. Removal re-checks the land's approval, worktree status
  (untracked files and every populated submodule included, since a child's ignore settings can hide
  nested changes) and parent ancestry under the lock, before and after stopping writable sidecars;
  index flags that hide changes prevent automatic cleanup. Bulk land-and-remove uses the same
  default-No `DestroyGate`, and `--yes` does not override changed work.
- A missing fork log is an ordinary not-yet-produced state. Multi-fork reads keep every readable
  stream and fail for any unreadable one; a follower's failure prints at once while the others run.
  `streamLog` opens a log through the same `OpenStateFile` but caps no size: it holds at most
  64 KiB of a line (`logChunkLimit`), so a longer line prints in pieces, and in the prefixed
  multi-fork view each piece ends with a newline so another fork never lands mid-line. A log that is
  not a regular file gets "remove it", not the permissions advice: a log only holds output.

**Folders in the forks tree open without waiting.** `forkspace.OpenFolder` opens a folder with
`O_DIRECTORY|O_NONBLOCK` and, unless told to follow, `O_NOFOLLOW`, so a FIFO or link where the
records folder, the forks folder or the staged-discard folder belongs fails at once; the listings
(`Names`, `LifecycleNames`, `StagedDiscards`) and the folder syncs use it. Only the fork home's
parent, the project's own parent folder, may be reached through a link (`confirmForkHomeEntry`).
forkctl's `syncIsolatedPath` opens files and folders under the records folder with
`O_NONBLOCK|O_NOFOLLOW`.

**Removal rechecks the fork once its services stop.** `DestroyFork(rt, repo, name, force)` is rm's
and `--fresh`'s removal, under the fork's lifecycle lock. A service with a writable bind can write
to the fork until it stops, after the earlier cleanliness check, so unless `force` it keeps the fork
when `stopForkServices` returned a failure, and runs `ForkWorktreeClean` (recursive through
populated submodules) once more before `forkspace.Destroy`. Merge's `destroyLandedFork` keeps its
own after-teardown `validateRemoval` and treats teardown as best effort.

**Every fork record is written through the records folder's own handle.**
`forkspace.OpenStateRoot(repo, sub, create)` opens the records folder, or a folder below it (several
components), by descriptor without
following a link at any step (made owner-only when `create`), then opens
it as an `os.Root` and requires the two to be the same folder, so every open, rename and removal
through the root stays there even if a box swaps the name afterwards. `CreateTempIn`,
`MkdirTempIn` and `PublishNoReplace` (hard link, then unlink: never replaces) work through that
root. A record's file is synced before its rename; no folder is: directory fsyncs and their
"visible but not yet synced" retries guarded only against power loss, which coop's threat model
leaves out (owner decision, 2026-10-15). `folderChecked` is the tests' scheduling point where a
box could swap a folder Coop just checked. Generation records, the reservation and execution registries, forkctl's land
journal, isolated records and candidate folders, and tasks' assignment, proposal and candidate
registries, discard intent and projection snapshots all use it (tasks' `openStateRoot` takes a
path under `StateDir`). `.discarding`, in the forks folder, is opened the same way from that
folder's handle (`openStagedDiscardDir`), and `StageWorkspaceDiscardLocked` moves the workspace with a
no-replace `renameat` between the two folders' descriptors.

## Changelog
- 2026-10-19 — a planted link at the forks folder or at the old `.coop` is nothing to move, so box
  launches no longer fail on one (task 2026-10-19-keep-box-launches-working-when-a-project-s-forks).
- 2026-10-19 — the records key uses the disk's own spelling (fsidentity.StoredPath, remembered per
  process for an existing folder) (task 2026-10-18-key-coop-s-per-project-host-records-by-the-folde).
- 2026-10-19 — fork records moved from `<repo>-forks/.coop` into `~/.local/state/coop/forks/<key>/`,
  copied once by the first open or box launch, following no link; forks need one disk (task
  2026-10-15-fence-other-projects-fork-records-inside-a-box-m). Review fixes: copy instead of rename
  (held handles), no-follow opens, an empty folder when nothing moves, a writable mount holding the
  forks folder refused, `Destroy` through a handle. Dropped the trap about by-path `.coop` writers.
- 2026-10-18 — directory fsyncs and their retries removed; the race tests swap at `folderChecked`
  (task 2026-10-15-drop-directory-fsyncs-and-sync-retry-ladders).
- 2026-10-18 — staging a discard goes through the forks folder's and .discarding's descriptors (task
  2026-10-18-stage-a-fork-discard-through-the-forks-folder-s).
- 2026-10-18 — the remaining `.coop` writers go through `OpenStateRoot` (task
  2026-10-17-route-the-remaining-coop-writers-through-the-no).
- 2026-10-18 — rm and --fresh keep a fork whose teardown failed or that gained work while its
  services stopped (task 2026-10-17-recheck-a-fork-s-submodule-work-after-its-servic).
- 2026-10-18 — folder opens in the forks tree no longer wait on a planted FIFO or follow a link
  (task 2026-10-16-open-the-forks-tree-s-folders-without-waiting-on).
- 2026-10-18 — Linux start tokens name their PID namespace (v2), and a token from another namespace
  is Foreign (task 2026-10-17-treat-a-linux-fork-record-written-in-another-pid).
- 2026-10-17 — the task snapshot reads another system's record through the same check as
  `coop fork ls`, now `forkspace.WorkerElsewhere` (it was forkctl's `workerElsewhere`): the fork
  gets `DetachedSystem` and `CleanupPending` stays false, in the generation-bound and name-only
  branches alike, so `coop tasks watch` shows "background loop started on <system>" where it showed
  cleanup-pending, and `gatherForkStatus` no longer sets the snapshot's flag aside. Retired the
  trap. Re-verified against state.go, status.go, snapshot.go and watch.go
  (task 2026-10-17-stop-coop-tasks-watch-from-reporting-a-fork-loop).
- 2026-10-17 — one identity check: `ProcessIdentityOf` delegates to `processidentity.Inspect`
  through `processidentity.Kernel`, and `ProcessIdentity` is an alias of `processidentity.State`
  under the old names. The copies differed for a pid of 1 or below and for a missing pid with no
  token or one of no known shape: forkspace said Gone, so `coop fork stop` reaped the fork's boxes
  and deleted such a record. Both are now Unknown, and `ParseWorkerState`/`Marshal` refuse a record
  without a stable token, so stop prints the guarded malformed-record recovery instead of a
  "verify, then retry stop" that could never pass. No coop writes such a record (`WritePid` has
  refused one since 2026-07-16). Re-verified against state.go, execution.go, supervise.go and
  identity.go (task 2026-10-17-read-a-process-start-token-through-one-check-not).
- 2026-10-17 — `coop fork ls` lists a fork whose record another system wrote as `elsewhere`,
  naming that system in its words and in `--json`'s `detached_system`, never as cleanup or
  running; it ranks above unknown and cleanup. Before, it read "cleanup needed", or "status
  unavailable" beside the loop's own sandbox record, with `cleanup_pending` true. Recorded the task
  snapshot's `CleanupPending`, which `coop tasks watch` still reports, as a trap. Re-verified against
  status.go, ls.go and snapshot.go (task 2026-10-17-show-a-fork-whose-loop-record-another-os-wrote-a).
- 2026-10-17 — a record whose start token is the other platform's shape is
  `ProcessIdentityForeign`, decided before any pid is probed, and never provably dead: `ForkStop`
  and a detached start refuse it by name and keep it, and `killGroup`/`waitForExit` proceed only on
  a Match. Before, a Linux record read on macOS (or the reverse) looked gone or reused, so stop
  reaped the fork's boxes and deleted the record of a loop that could still run. Re-verified against
  state.go and supervise.go (task 2026-10-17-stop-judging-a-fork-pidfile-s-process-dead-becau).
- 2026-10-17 — the lock, the log and the worker record are written only through a handle each call opens on
  `.coop`, opened with the forks folder without following a link: lock and log opens refuse
  anything but a regular file before their fchmod, the record is renamed and removed with
  renameat/unlinkat, and `.coop`'s own chmod goes through its descriptor. forkctl's `OpenForkLog`
  became `forkspace.OpenLog`, stop clears the record with `forkspace.RemoveWorkerState`, and start
  and stop say to remove whatever stands at a refused lock. Recorded the remaining by-path `.coop`
  writers as a trap. Re-verified against state.go,
  supervise.go and fork_cmd.go (task 2026-10-16-create-fork-lock-files-without-following-a-plant).
- 2026-10-17 — the record and the fork log open through `forkspace.OpenStateFile` (no link
  followed, no FIFO waited on, a regular file only), and the record read stops past 4 KiB; anything
  else is `ErrNotAStateFile`, named by what Coop found. forkctl stopped reading the record's bytes
  itself, so "read only through `WorkerState`" holds. The refusal's inspection is `ls -ld`, never
  `sed`. Re-verified against state.go and supervise.go
  (task 2026-10-16-read-a-fork-s-worker-pidfile-without-waiting-on).
- 2026-10-15 — replaced the four lifecycle states (worker, reap-pending, start reservation,
  launched reservation) with the worker record alone (audit EDGE-11, owner decision 3.5). The flock
  already spanned claim, start and record, so the reservations only guarded a parent dying in that
  window; now the child confirms its own record under the same flock and exits when it is missing.
  Stop writes no marker: a dead worker's record is the retry handle. Kept start tokens, the
  malformed/unsupported refusals and token re-checks before each signal. Retired forms read as
  malformed, and the refusal prints the guarded recovery. Fork logs open with `O_APPEND`, so a
  refused child of a start that died before its record cannot overwrite a newer start's output.
  Removed `PublishReservedWorker`, `ClaimStateFor`, the claim/clear helpers, the
  `--_detached=<reservation>` payload and the task snapshot's `starting` flag. Re-verified against
  state.go, supervise.go, fork_cmd.go, cli.go and sessionsvc's `NeedsStop`, `LockState` and
  `LockStateContext` uses.
- 2026-10-10 — actual detached-loop stop left a dead sandbox record after its box was gone. Reverified stop/registry ownership and added exact child cleanup before success; live, unknown, foreign-generation and foreground/editor retention regressed.
- 2026-09-23 — generation v3 replaced allocator metadata with a private hardlink anchor. Stopped
  v1/v2 records migrate only after branch/origin verification and absence of executions,
  reservations and land intent; active or ambiguous state fails closed. Publication and session
  rollback preserve an exact retry point across post-rename sync and cleanup interruptions, and
  migration refuses redirected common Git metadata.
- 2026-09-18 — the generation binds the workspace inode only (generation.go ValidateGenerationWorkspace,
  OpenGenerationWorkspaceRoot): comparing the device had stopped every fork from opening after a reboot.
- 2026-09-05 — post-land deletion retains the successful land's open inode, generation and commit
  approval through confirmation, and rechecks clean state after sidecar teardown. Regressed legacy,
  task-candidate and journal-replay success, late work, identity replacement and inspection errors.
  Added nested-submodule proofs for ignore/untracked settings, hidden index bits, inactive modules
  and missing/redirected Git roots; status alone does not establish safe removal.
- 2026-09-03 — removed the headerless pre-v8 classification and migration procedure after a live
  inventory found 32 fork-state directories and zero worker PID files. Headerless records now use
  the generic malformed fail-closed path; owner-v1/v2 and future-version handling remain.
- 2026-09-03 — removed the unused generation-less `ClaimState` wrapper and re-verified that every
  reservation creator uses `ClaimStateFor` with the fork's immutable generation
- 2026-09-03 — documented honest missing/error fork-log states and independent follow failure
  reporting
- 2026-09-03 — documented and re-verified all-or-nothing workspace setup and fail-closed fork
  discovery; prompt/completion remain the explicit best-effort display exceptions
- 2026-09-03 — documented and re-verified owner-only fork control roots and records across
  forkspace, forkctl, and canonical task-authority writers
- 2026-08-28 — current workers moved to owner-v2 and carry the immutable workspace generation;
  added exact-generation execution publication and clarified owner-v1 as stop-only migration state.
- 2026-08-26 — made the launched-reservation handoff byte-exact and atomic. The re-exec child now
  publishes before every host mutation and only while the inherited reservation still owns the
  lifecycle file; stop-winning cleanup is terminal, while parent publication is idempotent.
- 2026-08-26 — removed the pre-v8 parsed state model and its supervision branches. Only
  `owner-v1` is decoded; headerless and unknown-version records remain byte-exact, held, and
  side-effect-free behind the command preflight and merge lifecycle lock. Corrected the wire table
  to include its header.
- 2026-08-25 — removed Fleet from the current contract inventory and corrected the supervision
  owner to `internal/forkctl`; direct fork commands and sessions still share the same leaf state.
- 2026-08-10 — supervision and the land moved OUT of `internal/cli` into the new
  `internal/forkctl` control plane (`fork_loop.go` → `supervise.go`, `fork_merge.go` → `merge.go`);
  the contract itself is unchanged and still lives in `internal/forkspace`. The one cli residue that
  matters here: `lockLoopCheckout`/`lockSessionProducer` stayed in `internal/cli/fork_loop.go` — they
  are LOOP locks, not fork lifecycle state, and the loop-engine extraction owns them next.
- 2026-08-09 — the state file moved to the new `internal/forkspace` leaf (with the workspace paths,
  the name rule, and clone/destroy) so the sessions service can import the contract instead of
  taking a 12-method interface; supervision stayed in cli. Identifiers re-verified against
  `internal/forkspace/state.go`; the wire format is byte-identical (`TestForkWorkerStateWireFormat`
  moved with it, to `internal/forkspace/state_test.go`).
- 2026-08-09 — created while adding the two crash-window recoveries (reclaimable reservations, leftover-rebase abort); verified against fork_loop.go's state parser/claim path and fork_merge.go's rebase path.
- 2026-08-10 — the loop-engine extraction finished that residue: `lockLoopCheckout` is now
  `internal/loop/lock.go`, `lockSessionProducer` moved beside its wrapper in
  `internal/cli/commands.go`, `runForkLoop` folded into `internal/cli/fork_cmd.go`, and
  `internal/cli/fork_loop.go` is retired. The state-file contract in `internal/forkspace` is
  unchanged; re-verified.
