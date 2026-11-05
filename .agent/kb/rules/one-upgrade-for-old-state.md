---
name: one-upgrade-for-old-state
description: an old on-disk format is read for one upgrade step, then refused with the state version that converts it; check a worker with the new build before swapping it
scope: architecture
sources: [internal/session/records.go, internal/session/schema.go, internal/session/check.go, internal/session/operation_result.go, internal/workerconnector/journal.go, internal/workerconnector/event_streams.go, internal/workerconnector/create_origins.go, internal/forkspace/generation.go, internal/forkspace/reservation.go, internal/tasks/candidate.go, internal/tasks/candidate_rounds.go, internal/tasks/owner.go, internal/networkstate/authority.go, internal/networkstate/approval_withdrawal.go, internal/networkstate/old_records.go, internal/networkstate/qualification.go, internal/box/sweep.go, internal/processidentity/identity.go, internal/cli/session_cmd.go, internal/oneupgrade_test.go, docs/session-api.md, CHANGELOG.md, MIGRATING.md, internal/tasks/pending_review.go, internal/forkspace/state.go, internal/forkspace/immutable_primary.go, internal/sessionsvc/primary_snapshot_check.go]
sources: [internal/session/records.go, internal/session/schema.go, internal/session/check.go, internal/session/operation_result.go, internal/workerconnector/journal.go, internal/workerconnector/event_streams.go, internal/workerconnector/create_origins.go, internal/forkspace/generation.go, internal/forkspace/reservation.go, internal/tasks/candidate.go, internal/tasks/candidate_rounds.go, internal/tasks/owner.go, internal/networkstate/authority.go, internal/networkstate/approval_withdrawal.go, internal/networkstate/qualification.go, internal/box/sweep.go, internal/box/native_account.go, internal/box/native_home.go, internal/processidentity/identity.go, internal/cli/session_cmd.go, internal/oneupgrade_test.go, docs/session-api.md, CHANGELOG.md, MIGRATING.md, internal/tasks/pending_review.go, internal/forkspace/state.go, internal/forkspace/immutable_primary.go, internal/sessionsvc/primary_snapshot_check.go]
check: "go test ./internal -run TestOneUpgradeWaypoints"
updated: 2026-11-04
---

# Read old on-disk state for one upgrade step, then refuse it by name

When a durable format changes, the build that changes it still reads or converts the version
before it. A later build drops that reader once the known stores are confirmed past the step:
`coop sessions check` on each worker (Ryker's inventory names them), not an open-ended wait for a
fleet to drain. It then refuses the older state with the state version whose builds convert it:
"open it once with a coop build that uses session schema 25, then upgrade". Never ignore, reset
or guess older state instead.

Name the waypoint by state version (the session schema, a record's `version`), not by release.
Ryker deploys untagged builds that follow main, so "schema 25" identifies a build and "v11" does
not. A release name may follow as a hint.

**Why:** the owner chose this on 2026-10-15 (audit decision 3, option A). The session schema
ladder grew from 13 to 25 steps while it waited for a fleet drain that never came, and nothing
told an operator which build could still read old state. On 2026-09-28 a worker upgrade
converted a Ryker worker's store to schema 25 when it opened it. The old build refuses newer
schemas, so the worker had no way back.

**How to apply:**
- Before you swap a worker's coop, run `coop sessions check` with the NEW build while the service
  is stopped. It writes nothing. Exit 0 means the build uses the data as it is, 3 means it will
  convert it (back up the stopped state root to keep a way back), 4 means it refuses it, and 1
  means the check could not finish. See docs/session-api.md.
- A format change bumps its version constant, still reads or converts the previous version, and
  updates the table below and `coop sessions check` in the same commit: the check counts a record
  its reader accepts as current, so a reader that also accepts the older version must make the
  check report that version separately.
- Give every new durable format a `version` from its first write. An unversioned file, such as
  the worker identity or a job-source receipt, gets one before its shape changes.
- To retire an old reader:
  1. Confirm the waypoint: the retiring build's `coop sessions check` exits 0 or 3 on every known
     store. If the check does not read that state yet, add it to the check first.
  2. Raise the oldest-version constant and refuse older versions with the waypoint message.
  3. Update the row below, and remove only the conversion code the raised constant retires. A new
     session store is built by running every migration from schema 1, so retiring session steps
     first makes Open create new stores at the current schema directly.
  4. Announce it in CHANGELOG.md under `## Unreleased` with a bold headline that names the state,
     the oldest version still converted, and the build that converts older data. Add a
     MIGRATING.md section with the exact steps. For example:
     `**Session data older than schema 25 is no longer converted.** Run coop sessions check
     with this release before you upgrade a worker. If it names an older schema, open the data
     once with a coop build that uses schema 25 (v10.1.2 to v11.3.0) first.`
- The pre-v11 provider logins and history were retired on 2026-11-04, by the owner's decision and
  not by a version: the last two rows below. They have no state version; the refusal names the
  newest build that still converts them (v12.1.0).

## Waypoints

Formats Ryker's controller keeps, such as workspace checkpoint bundles, need its inventory rather
than a worker's `coop sessions check`, which never sees them.

A row whose oldest version is above its first has retired the older readers: Coop refuses that
older state by name and leaves it as it is. "Converting build" is the build that brings older data
to the current version; for a retired row it is the last build that still does, and the refusal
names it.

| State | Where | Current | Oldest read | Older versions today | Converting build |
|---|---|---|---|---|---|
| Primary snapshot receipt | `<state>/job-sources/<key>/repository-forks/<fork>/.git/coop-primary-snapshot.json` | 1 | 1 | none exist; independent forks have no receipt | none |
| Companion snapshot receipt | `<state>/companion-snapshots/<key>/receipt.json` | 1 | 1 | none exist | none |
| Session database schema | `<state>/session.sqlite` | 26 | 26 | 1 to 25 are refused by name when the service opens the store, and `coop sessions check` exits 4; the file is left as it is | a build at or before commit 29133b0e (schema 26) |
| Turn retry receipts | rows in `<state>/session.sqlite` | 1 | unversioned | read in place | `coop sessions compact` (v10.1.2 and later) |
| Worker command receipts | `<state>/connector/commands/` | 1 | 1 | none exist | none |
| Worker event streams | `<state>/connector/event-streams/` | 1 | 1 | no older version, but a version 1 stream with no create origin can remain from before origins; kept as a dormant generation floor, and `coop sessions check` cannot tell it apart | none |
| Worker create origins | `<state>/connector/create-origins/` | 1 | 1 | no older version, but a version 1 origin with an empty `operation_id` can remain from a receipt that proved only the session; read in place | none |
| Fork generation | `~/.local/state/coop/forks/<key>/<name>.generation.json` | 3 | 3 | 1 and 2 are refused by name: the fork is neither unbound nor free, and `coop fork stop` reports the refusal | v12.1.0 (start the stopped fork once) |
| Fork reservation | `~/.local/state/coop/forks/<key>/reservations/` | 2 | 2 | 1 is refused by name on every reservation read, so the fork name stays blocked | none: recover it offline |
| Fork review candidate | `~/.local/state/coop/forks/<key>/<name>.<generation>.candidate.json` | 2 | 2 | a bare version 1 file is refused by name, and so is a land whose candidate pointer is gone | v12.1.0 (review or merge the fork once) |
| Task owner record | `~/.local/state/coop/task-leases/v1/<key>.owner.json` | 2 | 2 | 1 is refused by name, so the task stays claimed | v12.1.0 (`coop tasks release`) |
| Pending final review | `~/.local/state/coop/task-leases/v1/<queue>-<key>.pending-review.json` | 2 | 2 | 1 is refused by name | v12.1.0 |
| Completion fingerprint | `tree_format` in the `*.windows.json` and `*.pending-review.json` records beside them | 1 | 1 | a fingerprint with none, and a window's singular `allowed_done_departure`, are refused by name | v12.1.0 |
| Detached worker record | `~/.local/state/coop/forks/<key>/<name>.pid` | owner-v2 | owner-v2 | `owner-v1` (no generation) is refused by name; `coop fork stop` prints the steps that clear it | none: stop the worker by hand |
| Network approval | `~/.local/state/coop/network/approval-<id>.json` | 4 | 4 | 1 to 3 are filed under the keyed `<id>` of v11.3.0 and earlier: not read, and the project they could belong to is refused by name until `coop net forget --project <path>` removes them (or you delete the file, if the project is gone) | `coop approve` (builds after v11.3.0) |
| Network withdrawal | `~/.local/state/coop/network/withdrawn-<id>.json` | 2 | 2 | version 1 (v11.3.0 and earlier, under the keyed `<id>`) is not read: the project it could belong to is refused by name, so the barrier holds, until `coop net forget --project <path>` removes it or you delete the file | Coop v12.1.0 |
| Network setup record | `~/.local/state/coop/network/qualification-<id>.json` | 2 | 2 | refused by name: run `coop net setup` | `coop net setup` |
| Network execution | `~/.local/state/coop/network/execution-<id>.json` | 2 | 2 | version 1 is not read: the project it names is refused by name, and the run is not cleaned up | Coop v12.1.0 |
| Isolated fork generation | `~/.local/state/coop/forks/<key>/<name>.generation.json` | 4 | 4 | none exist | none |
| Fork records folder | `~/.local/state/coop/forks/<key>/` | private folder | private folder | `<repo>-forks/.coop/` holding anything but lock files is refused by name and never moved or removed | v12.1.0 (first use of the project) |
| Execution fallback registry | `~/.local/state/coop/executions/v1/<key>/` | not written | not read | every entry is reported by path and blocks fork mutation; the folder stays fenced from boxes, and the service-launch locks beside it are unaffected | v12.1.0 |
| Service approval | `~/.local/state/coop/service-approvals/` | 5 | 5 | 4 and below grant nothing and label nothing; the files they covered stay hidden until `coop up` shows the review | `coop up` re-approves |
| Filesystem custody record | `~/.local/state/coop/filesystem/coop-fs-*/record.json` | 1 | 1 | a record with a helper image and no controller (builds of 2026-11-01 and 2026-11-02) is refused by name and left as it is, so its protection holds; the share-check `Probes` field is still read | v12.1.0 (retires it) |
| Account renewal journal | `~/.config/coop/credentials/<account>/renewal.json` | 1 | 1 | a kept answer with no `received_at` (Coop v11.3.0 and earlier) is refused by name and left as it is | v12.1.0, or `coop login` |
| Linux process start token | pid files, labels, records | `linux-proc-v2:` | `linux-proc-v2:` | `linux-proc-v1:` has no PID namespace and reads as no known shape: Unknown, never a match and never proof of death | none: the record stays |
| Workspace checkpoint bundle | kept by Ryker's controller | 2 | 1 | decoded so restore can refuse it by name; only a checkpoint file in the worker's own state is still served | none needed |
| Box supervisor label | `coop.host` on boxes, their brokers and per-run services | 2 | 2 | a v1 label reads as unknown: reported, never removed | none: labels are never rewritten |
| Pre-v11 provider login | `~/.config/coop/<provider>/profiles/<account>/` and `host-credentials/<account>/` | `credentials/<account>/authority.json` | refused | refused by name for an account with no `credentials/<account>/` folder: run `coop login <provider>@<account>`; the old folders are never read or deleted | v12.1.0 and earlier |
| Pre-v11 provider history | `~/.config/coop/<provider>/profiles/<account>/`, `acp-sessions/` and flat `acp-threads/*.json` | per-repository `native-homes/` and `acp-homes/` | not imported | an import an older build started and never finished is refused by name at launch; history and thread bindings never imported stay in the old folders unread | v12.1.0 and earlier |

`<state>` is the session state root (`--state`, default `~/.local/state/coop/sessions`). A worker's
job forks live under it too, in `<state>/job-sources/<key>/repository-forks/`. Fork records are in
`~/.local/state/coop/forks/<key>/`, where `<key>` is the SHA-256 of the project's real path in the
spelling the disk stores. A network record's `<id>` for a project is the SHA-256 of its real path;
up to v11.3.0 it was keyed with the store's `owner.key`, which a lost key could not recover. Retiring
the keyed names (`internal/networkstate/old_records.go`) was done without that count: the store
itself finds any approval below version 4, withdrawal below 2 or execution below 2 that could belong
to the project being used, and refuses that project by name (`OldRecordsError`); a file whose name
provably belongs to another project (neither its plain nor its keyed id) is left alone. The `check:` test
pins each row's Current and Oldest read to the constants named in `waypointConstants` in
`internal/oneupgrade_test.go`. A row whose oldest version has no constant yet pins only Current.
The box supervisor label's versions are the string prefixes `v2` and `v1` (`internal/box/sweep.go`),
so its row pins neither, and the fork records folder, the execution fallback registry, the detached
worker record, the filesystem custody record, the renewal journal and the start token have no
integer version, so their rows pin neither too. The box label's waypoint was every Coop that shares
a Docker daemon, not a store; v1 retired on 2026-11-04 once Ryker's workers (image
`ryker-coop:70e1ded2-065f1a07`, which has d01e1ca7) and this Mac's Coop all wrote v2.
`legacyLabelsAreLocal` stays, since network recovery judges a run record naming no host with it.

## Changelog
- 2026-11-04 — retired the v1 box supervisor label, the version 4 service approval hint, the Linux
  start token v1, the renewal journal's undated kept answer, and the custody record written before
  the shared filesystem controller (task 2026-11-04-drop-the-old-host-state-readers). Each is refused by name or reads as
  unknown, and is left as it is. Rows added for the custody record, the renewal journal and the
  start token. Left for later: the custody record's `Probes` field (four live records), the
  settings-folder run files and records (`~/.config/coop` still holds some).
- 2026-11-04 — retired the session schema 1 to 25 conversion floor and the schema 26 cost backfill
  (task 2026-11-04-drop-the-old-session-store-row-readers): `OldestSchemaVersion` is 26 and the row
  above pins it. The v1 to v25 DDL ladder stays, because a new store is built by running it. Also
  retired the readers for a stored session with no saved job, a blank or removed
  (`readonly`, `bare`) execution mode or a blank network mode: such a session is quarantined at
  start and named in the log, and only a `retire_quarantined` discard removes its record. A
  read-only fork with no primary snapshot receipt is refused by name. Left: worker event streams
  with no create origin and create receipts or origins with a session ID and no operation (the
  Worker event streams and Worker create origins rows still describe them), because connector tests
  and live generation replay lean on them.
- 2026-11-04 — retired the network approval (1 to 3, keyed names), withdrawal (1) and execution (1)
  readers (task 2026-11-04-drop-the-old-network-record-readers). `networkstate.projectIdentity`
  refuses a project by name while the store holds such a record that could be its own, so a
  withdrawal still bars its project; `coop net forget --project` removes the approval and withdrawal
  files and files the current barrier first. Setup-check snapshots written without a marker are no
  longer marked for removal and stay on disk.
- 2026-11-04 — retired the pre-v11 provider login cutover and history import (owner decision; the
  2026-12-31 date was dropped): rows for both. A login only the old folders hold, and an unfinished
  import, are refused by name; the old folders stay untouched. Listed in `waypointConstants` with
  empty pins, since neither has a version.
- 2026-11-04 — retired the readers for fork generations 1 and 2, fork reservation 1, review candidate 1
  (and the land recovery that rebuilt one), task owner record 1, pending final review 1, completion
  fingerprints without `tree_format`, a window's singular `allowed_done_departure`, `owner-v1` worker
  records, the discard replay of assignments an interrupted discard did not journal, the execution
  fallback registry, and the move of `<repo>-forks/.coop` (task
  2026-11-04-drop-the-old-fork-and-task-record-readers). Each is refused by name and left as it is;
  rows updated, and rows added for the completion fingerprint and the detached worker record.
- 2026-11-04 — the isolated-fork source receipt no longer reads version 1: no commit wrote one, so a
  stray one is refused as invalid. It stays out of the table, having only one format.
- 2026-11-04 — corrected three "Older versions today" cells against the code: event streams and create
  origins can hold older records under version 1, and restore refuses a version 1 checkpoint bundle.
- 2026-11-03 — added the version 1 immutable primary receipt and stopped-worker inventory of
  current workspaces and staged discards. Current-format checks use the runtime reader; unknown
  versions remain protected. Swept version constants through TestOneUpgradeWaypoints.
- 2026-10-28 — advanced the current session schema to 26 for accumulated turn costs; retained schema 25 historical waypoint examples and the existing conversion floor. Swept all version constants through TestOneUpgradeWaypoints.
- 2026-10-19 — network records are named by plain hashes (task
  2026-10-15-simplify-network-record-keys-and-setup-records): rows for the network withdrawal (2,
  moved from 1 on lookup) and the setup record (2, older refused by name); the network approval is 4,
  and 1 to 3 under the old name refuse their project until approved or forgotten. `coop sessions
  check` does not read the network store.
- 2026-10-19 — the execution fallback registry is no longer written, only read for one upgrade
  (task 2026-10-19-drop-the-fallback-execution-registry-now-that-fo); its row pins nothing.
- 2026-10-19 — fork records moved from `<repo>-forks/.coop/` to `~/.local/state/coop/forks/<key>/`
  (task 2026-10-15-fence-other-projects-fork-records-inside-a-box-m): the four fork rows' Where,
  and a row for the folder itself, whose old place is moved in once and never read again.
- 2026-10-18 — added the network execution record: version 2 drops the per-resource create/start
  journal (task 2026-10-15-replace-the-network-cleanup-journal-with-a-label), version 1 is still
  read. `coop sessions check` does not read the network store.
- 2026-10-17 — the pending final-review row also notes that a version 1 record whose commit
  predates its run base is anchored on load, as version 2 enrollment anchors one.
- 2026-10-17 — the pending final-review row: a version 1 record awaiting verification has no
  reviewed-content identity, so it returns to signoff once instead of being verified.
- 2026-10-16 — added the pending final-review record: version 2 drops the commit IDs version 1
  pinned, and version 1 is still read (task 2026-10-15-find-a-task-s-commits-by-its-coop-task-trailer-i).
  `coop sessions check` does not read task-lease records, so it has nothing to report for it.
- 2026-10-17 — row for the box supervisor label (v2, still reads v1; task
  2026-10-16-name-the-host-in-the-supervisor-label-so-a-share). Its versions are string prefixes,
  so `waypointConstants` lists it with empty pins; swept the table with TestOneUpgradeWaypoints.
- 2026-10-16 — review fixes: a state root with neither a database nor a lock is refused (a mistyped
  volume name makes an empty one); -wal/-shm must be regular files; a record declaring version 0 is
  damaged, not old; a format change must update the check; rows for isolated fork generations, service
  approvals and workspace checkpoint bundles; how a reader is retired, stated without the drain.
- 2026-10-15 — created from the owner's audit decision 3 (option A). Inventoried versioned
  durable state across session, workerconnector, forkspace, forkctl, tasks, networkstate and the
  provider homes. Ten formats went into the table. Unversioned files and single-format records
  stay out of it until they change. The isolated-fork source receipt (version 2, reads 1) has no
  constants yet, so it stays out too. Added `OldestSchemaVersion`, the shared refusal in
  `migrate`, and `coop sessions check`. No migration was removed. Swept the table with
  TestOneUpgradeWaypoints: 0 violations.
