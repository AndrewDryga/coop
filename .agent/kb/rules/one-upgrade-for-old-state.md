---
name: one-upgrade-for-old-state
description: an old on-disk format is read for one upgrade step, then refused with the state version that converts it; check a worker with the new build before swapping it
scope: architecture
sources: [internal/session/records.go, internal/session/schema.go, internal/session/check.go, internal/session/operation_result.go, internal/workerconnector/journal.go, internal/workerconnector/event_streams.go, internal/workerconnector/create_origins.go, internal/forkspace/generation.go, internal/forkspace/reservation.go, internal/tasks/candidate.go, internal/tasks/candidate_rounds.go, internal/tasks/owner.go, internal/networkstate/authority.go, internal/box/sweep.go, internal/processidentity/identity.go, internal/cli/session_cmd.go, internal/oneupgrade_test.go, docs/session-api.md, CHANGELOG.md, MIGRATING.md, internal/tasks/pending_review.go]
check: "go test ./internal -run TestOneUpgradeWaypoints"
updated: 2026-10-17
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
- The pre-v11 provider history import is retired by date, not by version: coop stops importing
  it after 2026-12-31.

## Waypoints

Formats Ryker's controller keeps, such as workspace checkpoint bundles, need its inventory rather
than a worker's `coop sessions check`, which never sees them.

Nothing has been retired under this rule yet, so every oldest version is still the first one.
"Converting build" is the build that brings older data to the current version.

| State | Where | Current | Oldest read | Older versions today | Converting build |
|---|---|---|---|---|---|
| Session database schema | `<state>/session.sqlite` | 25 | 1 | converted when the service opens the store | schema 25 (v10.1.2 and later) |
| Turn retry receipts | rows in `<state>/session.sqlite` | 1 | unversioned | read in place | `coop sessions compact` (v10.1.2 and later) |
| Worker command receipts | `<state>/connector/commands/` | 1 | 1 | none exist | none |
| Worker event streams | `<state>/connector/event-streams/` | 1 | 1 | none exist | none |
| Worker create origins | `<state>/connector/create-origins/` | 1 | 1 | none exist | none |
| Fork generation | `<repo>-forks/.coop/<name>.generation.json` | 3 | 1 | read; converted when the stopped fork next starts (isolated forks write 4) | v10.1.2 and later |
| Fork reservation | `<repo>-forks/.coop/reservations/` | 2 | 1 | held: keeps the fork name blocked until recovered offline | none |
| Fork review candidate | `<repo>-forks/.coop/<name>.<generation>.candidate.json` | 2 | 1 | read; adopted into round history at the next change | v10.1.2 and later |
| Task owner record | `~/.local/state/coop/task-leases/v1/<key>.owner.json` | 2 | 1 | read in place, never rewritten | none needed |
| Pending final review | `~/.local/state/coop/task-leases/v1/<queue>-<key>.pending-review.json` | 2 | 1 | read without its commit IDs (one awaiting verification returns to signoff once; one whose commit predates its run base is anchored on load); the next write stores 2 | builds after v11.3.0 |
| Network approval | `~/.local/state/coop/network/approval-<id>.json` | 3 | 1 | read, but grants nothing | `coop approve` (v10.1.2 and later) |
| Isolated fork generation | `<repo>-forks/.coop/<name>.generation.json` | 4 | 4 | none exist | none |
| Service approval | `~/.local/state/coop/service-approvals/` | 5 | 4 | read; a new approval writes 5 | `coop up` re-approves |
| Workspace checkpoint bundle | kept by Ryker's controller | 2 | 1 | read | none needed |
| Box supervisor label | `coop.host` on boxes, their brokers and per-run services | 2 | 1 | judged as before by a Coop on a host, by none inside a container; an older Coop sharing the Docker daemon still writes them | none: labels are never rewritten |

`<state>` is the session state root (`--state`, default `~/.local/state/coop/sessions`). A worker's
job forks live under it too, in `<state>/job-sources/<key>/repository-forks/`. The `check:` test
pins each row's Current and Oldest read to the constants named in `waypointConstants` in
`internal/oneupgrade_test.go`. A row whose oldest version has no constant yet pins only Current.
The box supervisor label's versions are the string prefixes `v2` and `v1` (`internal/box/sweep.go`),
so its row pins neither. Its waypoint is every Coop that shares a Docker daemon, not a store:
retire v1 once those all write v2. Retiring it also removes `legacyLabelsAreLocal`, and a v1
label then reads as unknown: reported, never removed.

## Changelog
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
