# co:op worker API

A trusted controller supplies the code, the context and the execution settings. co:op owns
isolated model execution and durable workspaces. The worker can share a VM with its controller,
or run on a separate VM. The protocol is product-neutral. Ryker is a consumer of it and is not
part of its vocabulary.

## Connect

```bash
coop sessions connect --controller https://controller.example --token-file /run/secrets/coop-token
```

This is the only command that starts a worker. It starts the private local service and connects
out to the controller. No inbound TCP port is required. There is no worker JSON file and no local
policy YAML. Use the optional `--state <path>` to choose private storage, and the optional
`--ca-file <path>` to trust a private TLS CA. Public HTTPS uses the system trust store.

The single-use token decides the worker's identity. co:op saves the renewable identity under the
state directory, and removes the token file only after saving it. To reconnect, use the same
controller and state directory without `--token-file`. A saved identity can't be reused with a
different controller.

The default state directory is `~/.local/state/coop/sessions`. The internal Unix socket is
`<state>/control.sock`. Run the command under a process supervisor. Ctrl-C stops the connection
and any local service the command started. It doesn't stop an already-running service that it
reused.

A worker needs Git, Git LFS and a supported container runtime. The bundled worker image includes
Git LFS. On a native worker, install `git-lfs` with your package manager.

A worker never builds or runs a repository's own `.agent/Dockerfile` for a job. That file is
repository code, and it is not worker configuration. Jobs run in the worker's base image
(`COOP_BASE_IMAGE`). A job with filtered networking runs co:op's locked client image instead. Put
what jobs need in the base image. A normal-mode job still provisions the repository's
`.tool-versions` when its box starts.

Before accepting filtered jobs, run [`coop net setup`](networking.md#fleet-workers-and-docker-in-docker)
under the worker's OS user with the same Docker endpoint and network state directory. The session
API requires current qualification; it never builds the networking images itself. Use
`mode: "normal"` and `repository_read_only: true` for a filtered repository-knowledge job: the
separate `readonly` execution mode does not support filtered networking. Docker being present or
the worker accepting JobSpec version 2 does not prove filtered readiness.

To check the local session service:

```bash
coop sessions doctor
coop sessions doctor --socket /var/lib/coop-sessions/control.sock --json
```

The local socket and state are private to the worker's OS account. Never mount that socket into a
model sandbox. Never expose it through an unauthenticated proxy. Outbound connectivity alone
doesn't authenticate the remote controller. TLS verifies the destination before co:op sends an
enrollment token or accepts work.

## Session authority

Your controller supplies one immutable job. It holds the source, model targets, execution mode,
network access and resource limits. co:op validates and saves the job before it creates a
workspace. Retries must carry the same configuration. A restart uses the saved configuration.
Repository instructions and model output can't widen it. Old session history stays readable. An
old row without verifiable execution authority must not resume model work.

co:op gives each session store one stable ID inside `session.sqlite`. A new fork reservation
binds that store ID, the session ID and the fork's exact generation. The store ID moves with the
database, so relocating `--state` doesn't change ownership. Only the owning session service may
discard the fork, after checking its journal, session binding and workspace safety conditions.
`coop fork rm --force` is not an override.

Old reservations without a store ID stay visible and protected from generic fork removal. co:op
can't infer their owner from a matching session ID in some current store. Such sessions stay
readable. They need explicit offline recovery. co:op doesn't relabel them automatically or delete
them by age.

The trusted worker fetches repository data directly from GitHub. It uses a short-lived,
repository-scoped grant that the controller supplies. The model receives the full working tree,
including verified LFS payloads and every explicitly authorized recursive submodule. Tokens never
enter model mounts, repository configuration or exported results. Repository files can't redirect
the worker's authenticated fetches. Missing objects, unauthorized submodules and unsupported LFS
pointer formats make creation fail, so no incomplete tree is left behind.

### Workspace storage

Every hello carries an optional `storage` object. It is the worker's own account of the volume its
fork workspaces land on. It is taken from the daemon's [`GET /v1/storage`](#health) and forwarded
verbatim.

```json
{
  "version": 1,
  "measured_at": "2026-09-11T04:05:06Z",
  "capacity_bytes": 536870912000, "free_bytes": 107374182400, "reserve_bytes": 26843545600,
  "high_watermark_bytes": 510027366400, "low_watermark_bytes": 483183820800,
  "disposable_bytes": 8589934592,
  "protected_bytes": 21474836480,
  "unattributed_bytes": null,
  "allocation": "open",
  "refusal_reason": null
}
```

- `disposable_bytes` is what a discard could still return.
- `protected_bytes` is what this worker holds on purpose, including the hardlinked repository
  baseline the forks share.
- A `null` `unattributed_bytes` means the worker found storage it couldn't attribute, or couldn't
  finish measuring. Treat it as unknown, never as zero.
- `allocation` is `refused` while this worker won't accept a new workspace. `refusal_reason` is
  then `reserve_exhausted` or `protected_storage_exceeds_budget`. Placement and cleanup of work
  the worker already has continue either way.

The watermarks are levels of used bytes. The reserve is a floor of free bytes. By default, all
three are derived from the measured capacity:

- The reserve is 5%.
- Allocation closes when free space falls under one reserve.
- Allocation reopens only once two reserves are free.

This hysteresis stops a worker from flapping after every reclaimed workspace. A worker that can't
measure its volume, or whose daemon predates this endpoint, leaves the object out and keeps
polling.

An unbound generation or a clean workspace is not proof of abandonment. Another session store or
an unfinished create may own it. co:op reports it as protected in `/v1/storage` and leaves it for
manual inspection. Automatic maintenance only finishes removals that an authorized discard
already staged. An interrupted removal can resume. Before any deletion, the workspace is renamed
into an owner-private staging directory. No discard reports success until those bytes are gone.

### Restart and recovery

Stop the connector with `SIGINT` or `SIGTERM`. Across restarts, keep the identity file and the
entire journal directory, together with the same worker and workspace configuration. A restart
loads the existing identity and doesn't enroll again. Certificate renewal is automatic over
mutual TLS.

At startup, under the exclusive state lock, co:op reclaims only unpublished source stages,
transfer temps and host-only Git credential files. It keeps published sources and journaled
response bodies. Run managed workers under a service manager that stops the whole process group
or cgroup. If a forced kill takes down the parent, a host Git or LFS child that survives can keep
deleted blocks open until it exits.

Commands are journaled before they run. Terminal results that weren't acknowledged are sent again
after a restart. If a command that already has a recorded result is delivered again, its receipt
is reused and the command doesn't run again. Reusing that command ID with a different payload is
refused. An asynchronous session create can have its result acknowledged while the create is
still running. The journal keeps its original operation identity until session activity can be
bound. Event cursors advance only after exact acknowledgements. Unacknowledged events replay, and
acknowledged events don't. When you move or back up a worker, don't prune journal files, and
don't keep only the `commands` directory.

If a review outlives its request, its uncertain transport receipt stays unchanged. Read the saved
operation through `GET /v1/operations?key=<original-key>`. Once it has succeeded, fetch its
retained review dossier. These are ordinary API requests, and neither lookup reruns the gate.

An unavailable daemon or controller is reported and retried. That doesn't authorize local
execution or receipt deletion. A malformed or expired saved identity fails closed. It doesn't
silently re-enroll, even if an enrollment token is present. Check file ownership, configured
trust, the clock and the controller's renewal status. Then follow the controller operator's
identity-recovery procedure. Don't delete the identity or the journal as a routine reconnect fix.

### Session inspection evidence

Your controller may ask a worker for one bounded, versioned account of a session the worker hosts.
It sends an ordinary `api_request` command with the payload
`{"method":"GET","path":"/v1/sessions/<id>/evidence"}` (see the
[`session evidence`](#session-evidence) endpoint below). The connector forwards the daemon's answer
verbatim. It selects nothing, derives nothing and adds no disclosure of its own.

The object carries the session's frozen network posture and what its runs were observed doing. It
also carries the host-approved task bound into its workspace, as the task folder stands at
capture. It never carries a credential, a host path or a packet body. It carries a destination
name only if the session job set `egress.export_destinations: true`. The agent-written `state.md`
is bounded, and it is withheld whole when it scans as carrying a secret.

Every section states its own availability, so a control plane can tell an unreadable registry from
a session that observed nothing:

- An unavailable section names its cause.
- A filtered session that has not run reports `no_run`.
- A session that never ran filtered reports `not_filtered`.

The daemon checks each answer against the evidence contract before it serves it. An answer that
fails the check is never sent. The route returns `500 internal_error` instead.

The worker advertises `session-evidence` version `1` only after the running local daemon publishes
`session_evidence_versions` in `GET /v1/capabilities`. This is independent of the other three
capabilities. A configured claim can't override that check. A worker whose daemon predates the
endpoint doesn't advertise it. That is what lets a controller say "this build does not export
evidence" instead of showing an empty network.

The refusals of one sealed filtered run also reach the controller as the existing `network`
session event. The daemon groups and caps them exactly as `coop net` prints them, with the same
destination projection.

## Request rules

The examples below use curl's Unix-socket support:

```bash
SOCKET="$HOME/.local/state/coop/sessions/control.sock"
curl --unix-socket "$SOCKET" http://localhost/healthz
```

Every mutation is a `POST` with these headers:

```text
Content-Type: application/json
Idempotency-Key: <globally unique caller-owned key>
```

- `Idempotency-Key` and `Content-Type` must each occur exactly once.
- Mutation URLs accept no query parameters.
- Ordinary mutation bodies are limited to 128 KiB.
- Turn-submission bodies are limited to 12 MiB, so they can carry bounded input artifacts.
- A turn takes at most 5 input `artifacts`, 8 MiB in total. More artifacts, or more bytes, return
  `invalid_request` ("turn has too many artifacts" or "turn artifact content exceeds its bound").
- Every body is decoded as exactly one JSON value, and unknown fields are rejected.
- Prompts are limited to 256 KiB of valid UTF-8 without NUL.

### Idempotency keys

Use one stable key for one logical action. Repeating the exact method, key and canonical body
returns the recorded result, and the action doesn't run again. Reusing a key with a different
method or body returns `idempotency_conflict`.

After a lost response, retry the same request with the same key. If an earlier response supplied
an operation ID, `GET /v1/operations/{operation_id}` reports its state, error code and resource
ID.

If a mutation response may have been lost after admission, `GET /v1/operations?key=<idempotency-key>`
recovers the same public projection by your exact, bounded idempotency key. The query accepts one
`key` only. It never returns the key, the request hash, the private intent or the result body.
Operation lookup deliberately leaves out the stored request, the private result, prompts, native
provider IDs and host paths.

### Compacting old turn receipts

A successful turn mutation stores a compact public replay receipt. It doesn't store a second copy
of the turn prompt and private execution controls. Upgraded daemons can still read older
full-turn receipts. An operator can rewrite those old receipts only during an explicit
maintenance window:

```bash
coop sessions compact --state <state-root> --backup <new-backup.sqlite>
```

The command takes the daemon's exclusive state lock. It refuses a backup path that already
exists, or one inside the state root. It verifies the backup before one transactional rewrite,
checks database integrity, and only then vacuums the database. It never changes canonical turn
rows, and it never runs automatically during startup.

The owner-only backup keeps the legacy prompt copies and is sensitive. Delete it only after the
compacted database and its ordinary backup cycle are verified.

To restore:

1. Stop the daemon.
2. Move `session.sqlite` and both possible `-wal` and `-shm` sidecars aside together.
3. Install the backup as an owner-only `session.sqlite`.
4. Restart.
5. Verify with `coop sessions doctor` before you remove the rollback set.

### Fencing an uncertain mutation

An owner can revoke authority after it durably prepared a session create or turn submit, and then
be unable to know whether the request crossed the socket. In that case it must not replay the
mutation just to discover a resource to stop. It fences the target instead.
`POST /v1/operations/fence` uses the target mutation's idempotency key and a strict envelope that
contains the exact typed request:

```json
{
  "method": "SubmitTurn",
  "request": {
    "session_id": "remote_...",
    "expected_revision": 3,
    "prompt": "the exact frozen prompt"
  }
}
```

Only `CreateRemoteSession` and `SubmitTurn` can be fenced.

- If the target operation is absent or only reserved, co:op records it as failed with
  `operation_fenced`. An exact later mutation therefore can't run.
- If admission already won, the fence returns that existing operation and its resource, for
  ordinary close or cancel reconciliation.

The target request may be as large as a turn submission, plus the bounded envelope. A lookup miss
or an elapsed client timeout is never proof of absence.

### Asynchronous session create

A session create can be admitted without holding the HTTP request open. Send
`Prefer: respond-async` on `POST /v1/sessions`. co:op durably records the create intent and
returns `202 Accepted` with an `operation` in `running` or a terminal state. Poll
`GET /v1/operations/{operation_id}` until it succeeds, then fetch the returned session resource.
If the client disconnects, the create continues under the daemon's lifetime. An exact replay with
the same key coalesces onto the same operation. Callers that leave out the preference keep the
synchronous response, for compatibility.

### Revision checks

Session lifecycle, budget and cancellation requests carry `expected_revision`. Read the current
session before acting. Treat `revision_conflict` as a request to reconcile. It is not permission
to guess a new action. A validation decision compares `candidate_sha256` instead, because its
authority is the exact unpublished candidate rather than the broader session revision.

## Lifecycle

```text
create -> open/parked
          -> queued -> starting -> running -> awaiting_validation -> completed
                                      |                 -> queued (rejected candidate)
                                      -> failed|cancelled|interrupted
          -> parked
          -> exhausted when the turn budget is consumed
          -> closed
          -> discard plan -> discarded
```

One worker owns a session. Turns are a bounded, durable FIFO, and only one runs at a time.

Without `limits.warm_idle_timeout_ms`, co:op starts one short-lived `coop fork <name> acp <target>` child
for a turn. It resumes the exact recorded native session, and records only its terminal assistant
message for public consumption. It tears down the child and the run-labeled box before parking.

A `readonly` session's child is `coop fork <name> acp <target> --readonly`. A `bare` session's
child is `coop acp <target> --bare`. Neither resumes a native session, because the restricted box
keeps none. A bare box is reaped by its run receipt alone, since it has no fork or project
registry behind it.

With `limits.warm_idle_timeout_ms`, co:op can prepare the authenticated ACP connection before the first
turn. It reuses that exact process and native session across serialized turns. The daemon keeps
at most four warm sessions. Each one keeps one of the four shared runtime slots between turns.
Any of these stops the process, removes its run-labeled box and services, and deletes projected
credentials: expiry, cancellation, failure, close, discard or daemon shutdown. Provider-native
history stays durable, so the next cold child can load the exact session again.

After every terminal turn, co:op removes the exact workspace-owned Compose service containers,
using their project and working-directory labels. Volumes remain, so the next turn can restart
services with durable development data. Daemon startup repeats this cleanup for historical
sessions after a crash or upgrade. A bounded idle-runtime sweep retries cleanup every minute,
without racing an active turn. The same sweep reaps an awaiting-validation turn's exact runtime
and projected credentials, without changing its candidate. Deleting the workspace volumes and
network is still the job of an explicit session discard.

A completed turn stays completed even when that teardown fails. The janitor's retry is the
guarantee, and a slow container runtime must not turn a finished answer into an error. A cleanup
failure is logged with its bounded cause. A turn that itself failed carries the cleanup cause
joined with its own error.

On restart:

- Queued turns that were never sent stay eligible.
- A turn waiting for semantic validation keeps its unpublished candidate, and it can still be
  accepted or rejected by digest. Its provider process, run-labeled box, projected credentials
  and services are disposable. Startup and the bounded periodic cleaner reap them without
  accepting, rejecting or changing the candidate.
- A turn interrupted after send intent ends as interrupted, and it is not silently replayed.
- Provider-native history is kept.

Close is non-destructive. It keeps the fork, conversation state, events and reviews. Discard is a
separate, two-step compare-and-swap action. It is available only for a closed, idle session.

The API can publish an explicitly authorized, reviewed candidate as a draft pull request. It
can't merge, sign, change the local parent ref, run an arbitrary host command or return a host
workspace path.

### Structured output

An optional `output_contract` carries the exact schema bytes and their digest:

```json
{
  "json_schema": {"type":"object","required":["answer"]},
  "sha256": "<lowercase SHA-256 hex of the exact json_schema bytes>",
  "require_semantic_validation": true
}
```

When `output_contract` is present:

- `json_schema` and `sha256` are required.
- The schema is capped at 256 KiB.
- Admission compiles the schema only after the digest matches.

co:op saves that exact contract, gives it to the model, and validates the final assistant bytes
before completion. Invalid JSON or a schema mismatch is repaired in the same native session.
Without semantic validation, three invalid provider responses fail the turn with
`output_contract_failed`, and no assistant message is published.

Set `require_semantic_validation: true` when you also own checks that depend on external frozen
state. After schema validation, co:op exposes the exact unpublished bytes and their SHA-256 digest
as `turn.candidate`, with state `awaiting_validation`. The durable `validation_candidate_sha256`
stays visible after the decision, so you can reconcile a lost HTTP response. Send one idempotent
decision to:

```text
POST /v1/sessions/<session>/turns/<turn>/validation
{"candidate_sha256":"<digest>","verdict":"accept"}
```

To reject the candidate instead, send `verdict: "reject"` with 1 to 20 `violations`. Each
violation is capped at 4 KiB, and so is the combined list.

An accept forbids violations. It copies only the stored candidate into `assistant_message` and
returns a durable `validation_receipt`. A reject re-prompts the same native session under the same
logical turn.

Semantic review allows up to three schema-valid candidates. Each semantic round separately allows
up to three provider responses to repair malformed or schema-invalid output. So a schema repair
doesn't spend a caller-review attempt.

## Endpoints

### Health

| Method | Path | Result |
| --- | --- | --- |
| `GET` | `/healthz` | `{"healthy":true}` |
| `GET` | `/readyz` | `{"ready":true}` after daemon startup |
| `GET` | `/v1/capabilities` | `{"job_spec_versions":[2],"controller_tools_versions":[1],"repository_freshness_receipt_versions":[2],"session_evidence_versions":[1]}`, for caller-side protocol negotiation |
| `GET` | `/v1/capacity` | Current shared active and warm runtime slots. The connector forwards this measurement in the hello it sends with every poll. |
| `GET` | `/v1/storage` | This daemon's own workspace-storage accounting: `storage` (the object a fleet controller reads), `budget`, `totals`, `roots`, `forks` and `problems` |

Runtime capacity is four shared slots for active and warm model processes. The session, turn and
workspace counts describe that same pool. They are not independent allocations. A queued turn
waits before its model timeout starts. A failed teardown keeps its slot until cleanup succeeds.
Unresolved startup runtime custody reports busy, with zero free slots.

For a quarantined or retired historical session, cleanup may release capacity only after proving
that its exact session-owned runtime labels are gone. That doesn't make the workspace runnable. A
failed capacity read also reports busy. It doesn't stop the worker from polling for reads and
cleanup.

`/v1/storage` measures allocated blocks, never apparent size. It never opens a file, so it can
account for credential-bearing private session state without reading any of it.

A fork's git objects are hardlinked from the checkout it was cloned from. So every total is the
storage that removing that content would actually return. The multiply-linked baseline is
reported once, in `totals.baseline_shared_bytes`, instead of being charged to each fork.

A measurement that couldn't see everything reports `totals.unknown` and publishes
`storage.unattributed_bytes` as `null`, which means unknown and never zero. A full tree walk is
too expensive for every caller. So the answer is re-measured at most once per
`budget.measure_seconds`, and `storage.measured_at` carries its age.

`forks[]` names each directory under a fork root, its category and the evidence for it. Only
`disposable` and `staged_discard` have an authorized reclamation path. `active`, `grace`,
`protected`, `control` and `unattributed` are all refusals with a reason. `protected` covers a
running worker, registered sandbox activity, an interrupted land, canonical task authority, a
quarantined session or uncommitted work. A directory with no co:op generation record is
`unattributed`. It is reported, and it is never deleted on a guess.

Under storage pressure, the daemon refuses a new workspace with `storage_unavailable` and names
its cause. Cleanup, discard and reads keep working. Nothing protected is deleted to make room.
Checkpoint capture and restore allocate additional copies, including during recovery. So their
separate pressure guard waits above the reserve, while keeping interrupted restores fenced.

Allocation closes at `storage.high_watermark_bytes` of used space, and reopens only under
`storage.low_watermark_bytes`. So reclaiming one workspace can't flap it open and shut.
`storage.reserve_bytes` is the free-space floor underneath both, and new work never spends it.
None of this bounds what an already-running task writes inside its own workspace.

The outbound connector reports `job-setup:2`, `repository-freshness:2`, `session-evidence:1` and
`controller-tools:1` only after its local daemon proves the matching versions through
`GET /v1/capabilities`. Missing proof removes that capability from the next hello. The
controller can tell an unsupported evidence export from a session that observed nothing.

Worker protocol v2 carries capabilities, capacity and optional storage measurements. It carries
no policy or repository catalogs. Exact code and execution settings belong to each immutable
controller job.

### Sessions

Your controller creates `create-session.json` with these fields:

- `task`: an opaque external reference.
- `job`: a version-2 JobSpec.
- `expected_job_digest`: the job's canonical SHA-256.
- `controller_tools` (optional): binds the controller's authenticated tool endpoint.

For exact fields and validation, see the shared
[protocol fixture](../testdata/protocol/coop-worker-v2.json) and the
[JobSpec definition](../internal/workerproto/job.go).

```bash
curl --unix-socket "$SOCKET" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: controller:create:01J...' \
  -H 'Prefer: respond-async' \
  --data-binary @create-session.json \
  http://localhost/v1/sessions
```

The job selects model targets, execution mode, network rules, queue and turn limits, and the
complete work and review setup. Besides the existing required fields, version 2 requires:

```json
{
  "environment": {"CI": "1"},
  "check": {"argv": ["make", "test"], "environment": {"DATABASE_URL": "postgres://test-db/app"}},
  "resources": {"cpu_millis": 2000, "memory_bytes": 4294967296, "pids": 512}
}
```

These fields must be present even when the environment maps and check argv are empty.

The work environment reaches only sandbox processes. Review receives it plus `check.environment`,
whose values win for duplicate keys. Env names are ordinary shell identifiers. `COOP_*`, provider
credential, model and adapter-owned variables are reserved. Values must be UTF-8 without line
breaks or leading or trailing whitespace, so env-file processing can't change an accepted value.
Values are plain job settings. They are not a way to transfer model or GitHub credentials.

The check is a literal argv. It is not a shell string, so use an explicit shell command if you need
shell syntax. An empty `check.argv` and an empty check environment explicitly mean no check, and
that review is not publishable.

| Field | Meaning | Accepted range |
| --- | --- | --- |
| `cpu_millis` | CPU, in thousandths of one core | 10 to 128000 |
| `memory_bytes` | Memory, in bytes | 6 MiB to 1 TiB |
| `pids` | The per-container process cap | 1 to 65536 |

Every job needs finite positive caps. co:op refuses a job that exceeds the worker's configured
CPU, memory or PID ceiling. It also refuses a job whose runtime can't enforce these limits. It
never silently clamps a requested value.

co:op freezes the canonical document before creating a workspace. A changed retry is refused. Task
text and repository settings are not execution authority. Your controller may read repository
defaults, but it must resolve and include every chosen value before submission. The worker's
normal and review containers use the same frozen setup, and the review container adds the
review-only environment. There is no local policy name, policy catalog, separate source selector
or worker JSON file.

Send version-2 jobs only to workers that advertise `job-setup:2`. An old daemon may remain behind
a new connector, so the advertisement requires live daemon proof. Check your JobSpec encoder
against the shared digest vectors in `internal/workerproto/job_test.go`. Don't rely on the
worker's `COOP_GATE` or the repository's `gate:` for remote reviews.

No v1 job gains invented setup. Historical sessions and evidence stay readable. Their turns,
reviews and interrupted creates can't run until they are resubmitted as v2 jobs.

#### Frozen repository source

Your controller resolves the requested default branch, branch, PR or commit before creating the
job. `job.source` identifies the GitHub repository by immutable ID and slug. It contains the exact
default, selected and base commits, the admitted tree and the source binding. It also lists every
authorized recursive submodule, with its exact repository, commit and tree. No host checkout path,
remote URL or GitHub token belongs in this document.

The trusted worker obtains short-lived, repository-scoped grants through its authenticated
controller connection. It fetches Git and LFS data directly from GitHub, and verifies the complete
working tree. Untrusted `.gitmodules` and `.lfsconfig` can't redirect authenticated requests.
Missing history, missing LFS payloads, mismatched gitlinks or insufficient disk fail closed. A job
with `source: null` uses an isolated empty workspace, or no workspace in bare mode.

#### Session endpoints

The public session carries the immutable source binding, `job_ref`, `job_digest`, target,
execution mode, project exposure flags, frozen network posture and repository freshness. It also
includes companion aliases, base commits, the generated fork name, revision, state, activity,
queue and budget counters, the event cursor and timestamps. Host paths, provider state, prompts
and credentials are private. Historical sessions without verifiable jobs stay inspectable, but
they can't resume model work.

With `Prefer: respond-async`, creation returns status 202 and the operation. Poll that operation. A
successful `resource_type: session` identifies the session to fetch. Without the preference,
creation waits and returns the operation and the session together.

| Method | Path | Body/query |
| --- | --- | --- |
| `POST` | `/v1/sessions` | `task`, `job`, `expected_job_digest`; optional `controller_tools` |
| `GET` | `/v1/sessions?limit=100` | `limit` is `1..1000` |
| `GET` | `/v1/sessions/{session_id}` | none |
| `POST` | `/v1/sessions/{session_id}/prepare` | `expected_revision`; the job must turn on warm execution |
| `POST` | `/v1/sessions/{session_id}/workspace` | `expected_revision`, `task`; the session must be open, writable and unused |

A bare session has no workspace. Changes, workspace binding, checkpoint, restore and review refuse
it. Turns, events, budget, cancel, close and discard remain available.

#### Task binding

`POST /v1/sessions/{session_id}/workspace` writes one approved task into the session's workspace
and binds the session to it. Send it before the first turn. The body holds `expected_revision`
and a `task` object:

- `offer_ref`: your stable reference for the approved task. It is 1 to 256 bytes of letters,
  digits, `_`, `.`, `:` and `-`.
- `title`: one line, up to 120 bytes.
- `prompt`: up to 12,000 bytes.
- `success_checks`: 1 to 20 distinct items, each up to 1,000 bytes.
- `authority_limits`: up to 20 distinct items, each up to 500 bytes.
- `instruction_ref` (optional): one reference, with the same rules as `offer_ref`.
- `source_refs`: up to 20 distinct references, with the same rules as `offer_ref`.

Every string must be valid UTF-8, not blank and free of NUL. The encoded task is capped at
64 KiB. co:op adds the task to the workspace's task queue as a to-do task, with one subtask per
success check.

The response holds the `operation` and the `session`. The session's `workspace_task` then carries
`queue_id`, `task_id`, `id`, `offer_ref` and `draft_sha256`. `queue_id` and `task_id` are durable
random identities, and `id` is the task's folder name. `draft_sha256` is the SHA-256 of the task
as co:op encoded it. The revision goes up by one, and co:op appends a `workspace.task_bound`
event.

The session must be open, writable and unused: parked, with no active, queued or used turns.
Anything else fails with `invalid_session_state`, and so does a bare session. A changed task under
an `offer_ref` the workspace already holds fails the same way. A stale `expected_revision` returns
`revision_conflict`. An invalid task returns `invalid_request`, and so does a different task for a
session that is already bound. Repeating the exact binding returns the session unchanged.

### Workspace checkpoints

| Method | Path | Input/output |
| --- | --- | --- |
| `POST` | `/v1/sessions/{id}/checkpoint` | JSON: `session_ref`, `expected_revision`, `placement_generation`, `repository_ref`; returns operation and descriptor |
| `GET` | `/v1/operations/{id}/checkpoint-bundle` | Streamed tar, exact `Content-Length`, SHA-256 `ETag` |
| `POST` | `/v1/sessions/{id}/workspace/restore` | Binary body plus `X-Coop-Expected-Revision` and base64-JSON `X-Coop-Workspace-Checkpoint` headers |

Capture requires an idle, writable session with a bound task. Restore requires an unused
replacement session with the same authorized base and task offer. All mutations need an
idempotency key. Restore's content type is `application/vnd.coop.workspace-checkpoint.v2+tar`.
Its Content-Length must equal the descriptor.

A v2 checkpoint streams raw typed Git objects and verified LFS payloads. That includes new
committed history, the final tracked tree, untracked files and task state. Restore brings back the
exact HEAD and working tree. It doesn't bring back the original split between staged and
unstaged changes. Changed submodule commits require separate authorized custody. They are
refused, including intermediate changes that were later reverted.

Memory and metadata are bounded. There is no 64-MiB data cap. Disk-pressure checks and
cancellation apply throughout the transfer and the Git work. These checks are pressure protection.
They are not a filesystem quota against other host writers.

Every member and task binding is verified in quarantine before live files change. co:op retains
the exact body before it marks the restore Running. An incomplete restore refuses execution across
a restart, and resumes from that body. Retry the same operation. Never send a new key against an
already bound session. Capture holds the runtime too, so it can't race a model turn. A restored
checkpoint doesn't inherit a publication approval. Review the candidate again before publishing.

Discard removes only checkpoint artifacts whose local session ownership is proven. It also settles
interrupted restores of that discarded session. Historical v1 artifacts stay readable. Their
patch-only format can't prove full Git and LFS custody, so a v1 restore is explicitly refused.

### Turns

Submit a turn:

```bash
curl --unix-socket "$SOCKET" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: controller:turn:01J...' \
  -d '{"expected_revision":1,"prompt":"Investigate the failing readiness check."}' \
  http://localhost/v1/sessions/remote_.../turns
```

| Method | Path | Body/query |
| --- | --- | --- |
| `POST` | `/v1/sessions/{session_id}/turns` | `expected_revision`, `prompt`; optional `artifacts`, `min_target_index`, `rewind_target`, `output_contract` |
| `GET` | `/v1/sessions/{session_id}/turns?after=0&limit=100` | ordinal cursor |
| `GET` | `/v1/sessions/{session_id}/turns/{turn_id}` | none |
| `GET` | `/v1/sessions/{session_id}/turns/{turn_id}/artifacts/{artifact_id}` | raw generated image |
| `POST` | `/v1/sessions/{session_id}/turns/{turn_id}/cancel` | `expected_revision` |
| `POST` | `/v1/sessions/{session_id}/turns/{turn_id}/validation` | `candidate_sha256`, `verdict`; `accept` forbids `violations`, `reject` requires 1 to 20 |

Validation decisions don't carry `expected_revision`. `candidate_sha256` provides the optimistic
concurrency check against the exact unpublished candidate being accepted or rejected.

Before an accept, a reject or an awaiting-turn cancellation changes durable state, co:op reaps the
provider runtime and projected credentials that produced the candidate. If that proof fails, the
API returns `503 session_cleanup_error`. The exact candidate stays `awaiting_validation`, and the
verdict has not been applied. The failed idempotent operation is terminal. After the janitor or an
operator restores runtime cleanup, fetch the still-current candidate and submit the same decision
with a fresh idempotency key. Replaying the failed key returns the same failure.

A public turn excludes its prompt, its idempotency data, `min_target_index` and `rewind_target`.
The last two are request data whose effect is published as the session's `target` and its
`session.target_rotated` event. A completed turn's `assistant_message` is the user-facing
response. co:op doesn't publish hidden reasoning, raw tool calls, raw ACP frames or box logs.

A completed turn carries `usage` when the adapter reported what it cost. It holds `input_tokens`,
`cached_input_tokens`, `output_tokens`, `reasoning_tokens`, and provider-reported `cost_usd` when
available. `cost_recorded` tells an exact zero apart from an adapter that reported no money.

ACP reports money as a cumulative session counter. co:op normalizes it into this turn's delta
before publishing it, including across daemon restarts and adapter-process resets. Cached input is
reported apart from fresh input, because providers price the two differently. A caller costing a
merged figure would overcharge itself. When nothing was reported, the object is left out entirely.
An unmeasured turn then doesn't read as zeros, and stays distinguishable from a genuinely free
one.

A completed turn may include `output_artifacts` metadata for images that the agent generated, or
that were saved under the turn-specific `.coop-output/<turn_id>` directory. Each record contains an
opaque ID, a safe filename, the media type, the SHA-256 digest and the exact byte count. It never
contains inline bytes. The raw endpoint returns that one immutable file, with matching
`Content-Type`, `Content-Length` and `ETag` headers.

Typed image and embedded image-resource blocks nested in ACP tool updates are captured in output
order, as `generated-1.<ext>`, `generated-2.<ext>` and so on. Their encoded bytes don't consume the
text transcript budget. Text tool updates are inspected one bounded frame at a time and then
discarded, so their cumulative bytes don't end a legitimate long turn. Assistant text, each wire
frame, the turn deadline and durable output artifacts remain independently bounded.

Only PNG, JPEG, WebP and GIF are accepted. A turn returns at most five images. Images from tool
updates and files in the turn's directory share that limit, and a repeated image is kept once. A
file in that directory without a `.png`, `.jpg`, `.jpeg`, `.webp` or `.gif` extension is ignored.
co:op rejects symlinks, special files, mismatched content, a sixth image, a file over 8 MiB, or
more than 8 MiB in total.

The scratch directory is removed before the turn completes, so generated charts don't appear as
repository changes. For a legacy read-only repository session, the path in the box is still
`.coop-output/...`. Its host source lives in co:op's session state instead of beneath the
checkout. The repository mount stays wholly read-only.

Cancellation asks ACP to cancel. It then stops and reaps the exact process group and run-labeled
box. It doesn't claim that external side effects were reversed.

### Events

```bash
curl --unix-socket "$SOCKET" \
  'http://localhost/v1/sessions/remote_.../events?after=41&limit=100'
```

Events are returned as a JSON array, ordered by a monotonically increasing per-session `sequence`.
Persist the last sequence you processed, and request `after=<sequence>` after a disconnect.

Owner-private events contain identity, sequence, turn ID, type, version, timestamp and the event's
own payload. Bounded raw tool evidence can include filesystem paths, arguments, results and
diffs. Every narrated event (a tool's start and completion, a thought, progress, a plan, a
permission answer) also carries `checkout_root`, the session's checkout path, so the worker can
take it out of what it sends.

The separate outbound worker projection carries what the model did to its controller: a tool's
title, input and result, and the model's thoughts, progress and plan. The session's checkout root
is removed. Each field is bounded:

| Field | Limit |
| --- | --- |
| Titles | 1 KiB |
| Text | 8 KiB |
| Tool fields | 16 KiB. Past that, a field crosses as a `preview` marked `truncated`. |
| One event | 64 KiB |

A field that scans as carrying a likely secret is withheld whole. The event's `withheld` object
names the field and why (`likely GitHub token`). A withheld tool input still names its MCP server
and tool. The event's structured `path_context` contains only project-relative paths and scope
warnings, never the host checkout root.

Each payload is capped at 256 KiB. A page is bounded by total bytes as well as by `limit`, so a
caller reading a chatty turn gets a short page instead of a truncated one.

#### Lifecycle events

These event types record what co:op itself decided:

```text
session.created
workspace.task_bound
session.state_changed
turn.queued
turn.started
turn.awaiting_validation
output_contract.rejected
activity.changed
assistant.message
turn.completed
turn.failed
turn.cancelled
turn.interrupted
budget.exhausted
session.target_rotated
session.parked
session.closed
workspace.discarded
network                 # one sealed filtered run's outcome: run_id, grouped denials, alerts
```

`network` is appended after a filtered run seals, and only when that run hit the boundary. A
quiet run costs no event, so this stream carries refusals rather than a per-turn heartbeat. Its
payload is bounded by construction:

- Destinations are grouped and capped, with an `omitted_destinations` count.
- Alerts are capped.
- `evidence_id` names the retained event that `coop net blocked` can open.

It follows the same disclosure scope as the network routes. A destination appears only when the
session job set `egress.export_destinations: true`.

`workspace.task_bound` records a [task binding](#task-binding). Its payload holds `offer_ref`,
`draft_sha256`, `queue_id`, `task_id` and `id`, the values in the session's `workspace_task`. A
checkpoint restore appends it too, with `base_commit` and `restored: true` added.

`output_contract.rejected` records one rejected candidate inside a turn. co:op appends one for
each provider response that fails the schema, with `attempt`, the contract's `sha256` and a
bounded `error`. Your `reject` verdict appends one with `attempt`, `candidate_sha256`,
`semantic: true` and your `violations`.

Through the outbound worker, lifecycle events other than `network` arrive without their payload.

#### Activity events

Activity events narrate the interior of a turn: what the model did, as opposed to what co:op
decided. They are always sequenced before the turn's own terminal event. So a caller that stops
polling at `turn.completed` has already seen them:

```text
tool.started            # tool_call_id, title, kind, input, path_context
tool.completed          # tool_call_id, title, kind, status, input, output, content, locations, terminal_exit, path_context
model.plan              # entries
model.thought           # text
model.progress          # outward commentary text, not private reasoning
permission.decided      # tool_call_id, title, outcome, option_id, option_kind
activity.elided         # dropped, reason
provider.backoff        # attempt, target, next_target, retry_after_seconds, reset_at, all_limited_until
provider.alive          # frames, bytes
```

When available, `terminal_exit` carries a numeric `exit_code`, a `signal` or both, separately
from command output. So a truncated output preview doesn't erase the exit result.

When structured filesystem evidence is available, `path_context` contains `basis: "lexical"` and
up to 16 `paths` entries. Each entry identifies its evidence with a JSON-pointer `source`, a
`scope` (`project`, `outside` or `unknown`) and, only for project-relative files, a `path`.

The bound checkout root is not exported. `partial: true` means some path evidence was omitted.
These are lexical display facts. They are not symlink-safe containment or permission decisions.
Missing provider paths stay missing. Titles and shell commands are not parsed to invent them. A
later tool update can add paths to its completion event without changing the original start
event. Path facts are extracted before large diff bodies become partial previews. URI-shaped
paths are reported as unknown. They are not interpreted as files inside the checkout. The
outbound worker projection validates these fields again. It drops any path entry it can't verify
and marks the context `partial`. The title and input it forwards follow the bounds and secret scan
described under [Events](#events).

`provider.backoff` is how a throttled turn stays audible. There is one event per proven rate
limit, and the ladder bounds that to one per rung.

- `attempt` numbers the events from 1 within the turn.
- `target` is the rung the provider limited.
- `retry_after_seconds` is how long that rung is out: the provider's own reset when it named one,
  and the ladder's bounded backoff when it did not.
- A rotation also carries `next_target`.
- The last backoff on an exhausted ladder carries `all_limited_until` instead.

Without this event, a turn crawling through 429s was indistinguishable from a dead one. A client
watching for silence would cancel work that was making progress.

`provider.alive` covers the throttle co:op can't see. A backoff is narrated when co:op's own
ladder acts on a limit. A provider CLI that retries 429s inside itself never reaches the ladder.
That turn streams frames while it makes no tool calls and produces no other events at all.

The pulse says the transport is still moving. `frames` and `bytes` count every ACP frame read for
that turn's prompt, cumulatively. A client re-reading its cursor tells new progress from a
redelivered event by these numbers, not by the timestamp.

The pulse is emitted at most once a minute. It is never emitted in a window where the turn already
narrated something. An ordinary turn produces none, and a long tool call is not described twice.
A turn that produces no frames at all still produces no events. That is what a client's
silent-turn deadline is for.

### Budget

Extend a session's turn budget:

```bash
curl --unix-socket "$SOCKET" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: controller:extend:01J...' \
  -d '{"expected_revision":9,"additional_turns":20}' \
  http://localhost/v1/sessions/remote_.../budget
```

An extension is bounded by co:op's global `max_turns` limit. You own authorization and spend
policy, as the caller. co:op only enforces the numeric bound and idempotency.

### Changes

```bash
curl --unix-socket "$SOCKET" \
  http://localhost/v1/sessions/remote_.../changes
```

`GET /v1/sessions/{session_id}/changes` returns:

- the immutable `base_commit`, the current `fork_head` and `fork_tree`, and the current
  `parent_head`;
- for a repository-backed session with a source binding, the immutable `admitted_source_tree`.
  It lets you tell new content from empty commits or commit-and-revert history above the
  admitted source, whichever kind it was;
- committed, staged, unstaged, untracked and conflicted typed path records;
- ahead and behind counts, and base-to-head divergence counts;
- a bounded binary patch page;
- `patch_digest`, the exact `patch_bytes`, `patch_offset`, `patch_next_offset` and
  `patch_has_more`.

To read another bounded page, call
`GET /v1/sessions/{session_id}/changes?patch_offset=<next>&patch_limit=<bytes>`. `patch_limit`
can't exceed the session job's `max_patch_bytes`. Offsets are bounded to 1 GiB. You must bind
navigation to `patch_digest`, and restart at offset zero if the digest changes. The legacy
`truncated` field is true whenever the response is not the complete patch.

JSON encodes `patch`, `path_bytes` and `old_path_bytes` as base64. `patch_bytes` is a plain
integer: the byte size of the whole patch. A normal UTF-8 path also appears in `path`. An
arbitrary byte path is preserved in `path_bytes`. Empty change lists are never used to hide Git
failures. A failure returns an error.

Changes may inspect dirty work. Review may not.

### Network

```bash
curl --unix-socket "$SOCKET" http://localhost/v1/sessions/remote_.../network
curl --unix-socket "$SOCKET" http://localhost/v1/sessions/remote_.../network/connections
curl --unix-socket "$SOCKET" http://localhost/v1/sessions/remote_.../network/explanations/<event_id>
curl --unix-socket "$SOCKET" http://localhost/v1/sessions/remote_.../network/receipt
```

All four are reads. None of them probes a gateway, and none can grant, approve or widen anything.
The session API has no path to network authority at all.

`GET /v1/sessions/{session_id}/network` returns:

- the session's frozen `mode` and `fingerprint`;
- the `requested` and `effective` rule texts;
- `current`: the newest run's retained observation summary, or `{"status":"no run yet"}`;
- a bounded `alerts` list.

`GET /v1/sessions/{session_id}/network/receipt` aggregates every run the session owned into one
versioned receipt, with independent `finality` and `completeness`. The receipt is `final` only
once the session is closed and every run receipt is final. A `final` receipt may still be honestly
`partial`.

The receipts are retained in the owner's own registry, so both of these answers survive container
garbage collection. A session that never ran filtered answers `{"available":false,"reason":"..."}`
instead of an empty receipt.

`GET /v1/sessions/{session_id}/network/connections` is the live drilldown. It returns the newest
run's bounded connection rows as the collector recorded them, with `status` (the same freshness
word the summary uses), `as_of` and `detail_truncated`. It is pull-based, because a per-second
sample belongs in nobody's durable journal. A session with no run yet answers
`{"status":"no run yet","connections":[]}`.

`GET /v1/sessions/{session_id}/network/explanations/{event_id}` opens one retained refusal from
this session's own runs. It returns the event, the reason, its provenance and whether a candidate
rule was drafted. An event that aged out of its run's bounded ring answers
`{"available":false,"reason": "event_not_retained"}`. That answer is not proof that the ID never
existed.

`projection` states the disclosure scope. It is `destinations-withheld` unless the session job set
`egress.export_destinations: true`. In that case it is `destinations-included`, and the rule texts
and observed names are present. The daemon owns that projection. The outbound worker forwards
exactly what the daemon answered, through ordinary GET API requests. Each one is a plain GET of a
route above, with no destination, rule or disclosure scope of its own to choose.

| Method | Path | Body/query |
| --- | --- | --- |
| `GET` | `/v1/sessions/{session_id}/network` | none |
| `GET` | `/v1/sessions/{session_id}/network/connections` | none |
| `GET` | `/v1/sessions/{session_id}/network/explanations/{event_id}` | none |
| `GET` | `/v1/sessions/{session_id}/network/receipt` | none |

### Session evidence

```bash
curl --unix-socket "$SOCKET" http://localhost/v1/sessions/remote_.../evidence
```

This route returns one bounded, versioned account of a session, for a control plane's inspection
page. It covers:

- the network posture the session was admitted under;
- what its newest run was observed doing;
- the session-wide receipt;
- the host-approved task bound into its workspace, as the task folder currently stands.

It is a read: no gateway probe, no runtime, no authority. It takes no query, so the caller can't
select anything about the disclosure. The daemon checks the object against its own contract
before it answers. An object that fails the check is never sent, and the route returns
`500 internal_error` instead. The outbound worker forwards this route through an ordinary
`api_request` command, verbatim. The controller never gets an object it has to guess its way
through.

Every section states its own availability. A section that couldn't be read and a section with
nothing in it lead an operator to opposite conclusions:

| Section | Status words |
| --- | --- |
| `network.access` | `captured`, `not_filtered`, `unavailable` |
| `network.observation` | `observed`, `no_run`, `not_filtered`, `unavailable` |
| `network.receipt` | `available`, `not_filtered`, `unavailable` |
| `task` | `bound`, `unbound`, `unavailable` |
| `task.snapshot.state_note` | `captured`, `absent`, `withheld` |

`unavailable` always carries a `reason`. The posture (`mode`, `fingerprint`) comes from the
immutable session row. So a registry this host can't read leaves the posture known, and every
section beside it explicitly unreadable. A filtered session that hasn't run yet reports `no_run`
and a `provisional` receipt with `run_count` `0`. It never reports an empty counter.

Counters are unsigned decimal strings, so a value above 2^53 survives a JavaScript client. A
`null` counter is a metric nobody measured. It never means `0`.

Lists are bounded on the way out, with the drop counted in `omitted_denials`,
`omitted_connections`, `omitted_alerts` and `omitted_run_references`. A run that denied a thousand
names costs one bounded object, which still says how much it left behind.

The same `projection` rule as the network routes applies. A destination appears only under
`destinations-included`. A refusal whose name was withheld says so with `destination_withheld`,
instead of reporting no name at all.

`task` carries the immutable binding from the session row, plus the task folder at capture:

- its state;
- the same `state_sha256` that a workspace checkpoint records for its task projection;
- the checklist labels that the checkpoint's booleans stand for;
- the file inventory, with digests;
- the agent-written `state.md`.

The note is bounded, and it is withheld whole when it scans as carrying a secret. The worker keeps
no transition ledger, so this is a snapshot. A control plane records successive snapshots instead
of asking the worker for a history it doesn't have. A bound task whose folder has gone missing
reports `unavailable` with its identity intact. It never reports an empty task.

| Method | Path | Body/query |
| --- | --- | --- |
| `GET` | `/v1/sessions/{session_id}/evidence` | none |

### Review

```bash
curl --unix-socket "$SOCKET" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: controller:review:01J...' \
  -d '{"expected_revision":12}' \
  http://localhost/v1/sessions/remote_.../review
```

Review requires a parked open or exhausted session, with no queued turns and a clean, committed
fork. It snapshots the exact source and parent commit and tree identities. It prepares a
disposable candidate against the current parent `HEAD`. It runs the worker-owned gate on that
candidate, and returns:

- the creation base, source, parent and candidate commit and tree identities;
- the immutable pull-request number, ref and head binding, when the session came from an
  existing PR;
- the rebase status and gate status, with the start failure's own words in `gate_error`;
- `gate_output`: the gate's command, its `exit_code` when it ran to one, how many bytes of its
  output co:op kept, and whether that is all of it (`complete`; otherwise `incomplete` says why,
  or `lost` when nothing could be kept);
- bounded policy findings;
- a bounded inline patch preview from the parent tree to the candidate tree;
- `patch_truncated`, `candidate_retained`, `publishable` and stable not-publishable reason codes.

The saved job's `check.argv` is the only remote review command. The worker's `COOP_GATE`, the
parent's `gate:` and candidate changes can't replace it. The first two still apply to ordinary
local fork merges.

The gate runs in an isolated controller-job box, with the session's saved network mode (and the
exact saved filtered-network authority). It doesn't use the repository's `box:` settings, or the
worker's ambient runtime arguments, env file or MCP config. It receives the saved work environment
plus check-only overrides, and the same per-container CPU, memory and PID limits as model work.
Like the job's turns, it uses the worker-configured image (or the locked filtered client image).
It never uses an image built from the job repository's Dockerfile.

The gate may create ignored build output in its disposable checkout. Changing the reviewed source
makes the result unpublishable. No gate, a red gate or a startup error also prevents publication.

The preview is base64 in JSON. A truncated preview is a transport condition, and it doesn't by
itself make the review unpublishable. co:op retains the exact candidate commit and its verified
LFS objects in private host storage, independently of the model workspace. There is no full-patch
transfer or patch-size publication limit. The candidate is one deterministic commit on the
reviewed parent, created before the gate. `source_head` preserves the original model provenance.

Secret checks cover the actual parent-to-candidate delta, including large or binary Git blobs and
LFS payloads. Changing LFS attributes also scans the resulting LFS payloads. A read failure
refuses the review. Unreadable content is never treated as safe.

`publishable` is evidence about this exact candidate. It is not permission to push or merge. It is
false for:

- a conflict;
- no gate, or a failed gate;
- a startup failure;
- policy findings;
- parent or source movement;
- active fork ownership;
- an empty change.

`candidate_retained` distinguishes custody from gate readiness. A missing or failed gate can still
leave an exact snapshot, but that grants no publication authority. Publication must use the
retained `candidate_head` unchanged, with a separate controller grant and a compare-and-swap on
the destination ref. It must never reconstruct the result from a patch or from the model's later
workspace. An explicit session discard removes its retained candidates. Controllers must keep
sessions whose reviews still await publication.

A review may outlive the connection that requested it. Look up the original operation key. Then
retrieve a succeeded review with `GET /v1/sessions/{session_id}/reviews/{operation_id}`. It
returns the same public `operation` and `review` envelope, without starting or resuming a gate.
Another session, a non-review operation and an unfinished review are refused. Both
`policy_findings` and `not_publishable_reasons` are arrays, including when empty.

#### Gate output

Everything the gate printed is kept beside the review: stdout and stderr as they came, and what
co:op printed while starting it. It is kept on a green gate as on a red one. So your controller
can fix what failed without asking a person to relay it. Read it page by page with
`GET /v1/sessions/{session_id}/reviews/{operation_id}/gate-output?cursor={cursor}`, starting
without a cursor:

```json
{"output":"1 test, 1 failure\n…","next_cursor":"1048576","bytes":1843200,"complete":true}
```

A page holds at most 1 MiB of output and ends on a character boundary. Bytes that are not UTF-8
read as U+FFFD. `next_cursor` is `null` on the last page. `bytes` counts everything kept.
`complete: false` comes with `incomplete`, which says why. co:op keeps the first 64 MiB, so a gate
that prints without end can't fill the worker's disk.

When nothing was kept, the answer is `{"lost":"<why>"}`. That happens when no gate ran, the output
couldn't be written, or the session was discarded. Only the review's own session reads its
output, and discarding the session removes it.

### Publish a reviewed candidate

`POST /v1/sessions/{session_id}/reviews/{review_operation_id}/publish` uses a new
`Idempotency-Key` and this JSON body:

```json
{
  "authorization_ref": "approval:42",
  "candidate_head": "<exact reviewed commit>",
  "candidate_tree": "<exact reviewed tree>",
  "branch": "automation/fix",
  "base_branch": "main",
  "expected_head": "",
  "pull_request_number": 0,
  "title": "Fix retry handling",
  "body": "Reviewed changes and verification details."
}
```

An empty `expected_head` requires a new branch. Updating a previously authorized PR requires its
number and its exact expected head. The operation returns `202` while it runs. Reconcile its key,
and read `GET /v1/sessions/{session_id}/publications/{operation_id}` for the completed
`operation` and `publication` envelope. A retry uses the same key and an identical request.

The result is one of:

- `published`, with the exact commit and tree and the PR receipt;
- `conflict`, with a verified existing PR and the observed remote head;
- `refused`, with a stable error code.

The trusted host requests fresh credentials from the authenticated controller at
`POST /v1/coop-workers/jobs/{job_ref}/publication-grants`. The request binds the session, job
digest, review operation, original command key, repository identity and publication body. Your
controller returns the same repository identity plus `token`, `expires_at` and its GitHub App
`actor_id`. A `403` permanently refuses this operation. Temporary failures stay retryable. Your
controller owns the approval rules. co:op requires no product-specific approval format.

Git and LFS bytes go directly from the private retained candidate to GitHub. co:op creates a
draft, or updates the exact App-owned PR while preserving a person's ready-for-review choice. Lost
push and PR responses are reconciled before retrying. An active publication prevents discard of
its candidate. Model workspace changes can't alter the reviewed result.

### Close and discard

Close a session:

```bash
curl --unix-socket "$SOCKET" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: controller:close:01J...' \
  -d '{"expected_revision":15}' \
  http://localhost/v1/sessions/remote_.../close
```

Close requires no active or queued turns, and it preserves the fork.

Plan a discard only after close:

```bash
curl --unix-socket "$SOCKET" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: controller:discard-plan:01J...' \
  -d '{"expected_revision":16}' \
  http://localhost/v1/sessions/remote_.../discard-plan
```

The plan captures the exact session revision, the anchored fork generation and reservation, and
the workspace inode, branch, head, status digest, running state, dirty state and unmerged state.
The inode is one stale-plan signal. It is not authority by itself. Apply pins the live directory
and rechecks the complete snapshot before deletion.

By default, dirty or unmerged work is refused. A caller that has separate authority to destroy it
must explicitly set `accept_dirty`, `accept_unmerged` or both when creating the plan.

Execute that exact plan:

```bash
curl --unix-socket "$SOCKET" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: controller:discard:01J...' \
  -d '{"plan_operation_id":"op_..."}' \
  http://localhost/v1/sessions/remote_.../discard
```

Discard first proves that the plan belongs to the session in the path. It then compares all
captured state, and refuses a stale, replaced or running workspace. It deletes the fork workspace
and private ACP state, and leaves a durable tombstone for the discarded session. A failed
comparison doesn't delete anything.

The daemon quarantines some sessions at start: a historical row without an owner-store binding, a
v1 reservation, or one whose generation record or workspace is gone. A quarantined session can be
neither planned nor discarded this way, because co:op can't prove it owns the workspace. Retire
the record instead:

```bash
curl --unix-socket "$SOCKET" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: controller:retire:01J...' \
  -d '{"retire_quarantined":true,"expected_revision":16}' \
  http://localhost/v1/sessions/remote_.../discard
```

This tombstones the session row only:

- Queued turns are exhausted.
- A turn that was active when the daemon lost authority stays in history as it was.
- The workspace, reservation, sidecar services and provider-native ACP history stay on disk.

A session that is not quarantined is refused with `invalid_session_state`.

Retirement itself never proves that a runtime stopped. Independently, the daemon checks the exact
`coop.run` labels derived from that store's session and turn IDs. It removes any matching boxes,
and cleans projected credentials under its private ACP state. It retries after a failed or
partial check, and advertises busy until the whole proof succeeds, including after a restart.

A successful runtime proof restores capacity. It doesn't adopt or delete the unowned workspace.
Quarantined sessions remain unrunnable, and still require explicit retirement or recovery. If
capacity stays busy, inspect the worker's runtime-cleanup warning. Don't treat a tombstone as
proof.

## Errors

Every error has one shape:

```json
{
  "error": {
    "code": "revision_conflict",
    "detail": "expected revision 4, current revision 5",
    "operation_id": "op_..."
  }
}
```

`operation_id` is present when co:op admitted an operation before the failure. Internal details
stay suppressed on the public API.

The daemon emits a bounded JSON diagnostic to stderr. It has the same operation ID, the method, the
resource identity, the error code and a sanitized detail. Repository and state-root paths are
replaced, and secret-bearing lines are removed. Use the ID to correlate the client error, the
operation record and the supervisor log, without reading SQLite directly.

Common status mapping:

| HTTP | Meaning |
| --- | --- |
| `400` | invalid or over-bounds request |
| `404` | session, turn, or operation not found |
| `409` | idempotency, operation fence, revision, state, queue, budget, resume, uncertainty, or discard conflict |
| `413` | ordinary request body exceeds 128 KiB, or turn submission exceeds 12 MiB |
| `500` | internal failure; host paths and raw internal errors are suppressed |
| `503` | readiness is not ready, or repository/runtime/network/storage authority is temporarily unavailable |

`storage_unavailable` is a retryable `503`. This worker's volume is under its configured pressure
limits, so it won't create another workspace until space is returned. When
`storage.refusal_reason` is `protected_storage_exceeds_budget`, it waits until the protected data
itself goes. Read `/v1/storage` to see which bytes are held and why. Place the session on another
worker, or clear the storage. Don't retry in a tight loop, and don't delete protected workspaces
to satisfy the quota.

`network_unavailable` is a `503`. The worker can't enforce or prove the session's frozen network
authority. Inspect the worker's runtime and network readiness. The worker refuses execution rather
than falling back to open networking. Don't alter a saved job to get past the refusal.

Treat `operation_uncertain` and `turn.interrupted` as reconciliation states. Never retry a
mutation under a new key just because its result is unknown.

`RunReview` is the narrow, read-only exception. After co:op captures the immutable source, parent
and job identities, replaying the exact request under the same key resumes that frozen review
under the same operation ID. It never recaptures moving repository state. Unreadable or invalid
captured intent stays `operation_uncertain`.

## Operational recovery

| Situation | Action |
| --- | --- |
| Client response lost | Replay the exact mutation with the same idempotency key |
| Stop races a prepared create or submit | Fence the exact operation; close or cancel only if admission already won |
| Socket reconnect | Resume event polling after the last committed sequence |
| Daemon already owns state | Stop or inspect that daemon; don't remove the lock |
| Stale socket after crash | Start the daemon with the same state root; it removes only a socket after acquiring the state lock |
| Queued turn at restart | co:op resumes it in FIFO order |
| Running session create at restart | co:op resumes its durable create intent automatically |
| Reserved operation at restart | co:op records an interrupted-admission failure; nothing external was attempted |
| Running checkpoint restore at restart | Resume its retained body under the execution fence; never submit a fresh key |
| Other running operation at restart | co:op marks it `operation_uncertain`; reconcile rather than replaying under a new key |
| Operation remains running after startup | The periodic watchdog resumes safe creates and marks stale ambiguous mutations uncertain |
| Turn interrupted after send intent | Surface interrupted; don't submit the same human input automatically |
| Active turn must stop | Use the idempotent cancel endpoint, then reconcile the terminal turn |
| Session should stop costing runtime | Wait for park; without warm execution, no agent or Compose service container remains between turns, and a warm session keeps them until its idle timeout expires or you close it |
| Incident is over | Close; retain the fork for review or an explicit later discard |
| Review patch preview is truncated | Use paged inspection; publish the retained reviewed candidate, never reconstruct it from the preview |

Back up the entire state root while the daemon is stopped. Restoring a database without its
corresponding forks requires operator reconciliation. co:op doesn't infer ownership from names.

## Consumer responsibilities

A production consumer still needs:

- its own identity and authorization policy;
- durable inbound and outbound delivery, and deduplication;
- stable, globally unique idempotency keys;
- event cursors and reconciliation workers;
- prompt framing and output redaction that suit its transport;
- retention, audit, capacity and spend policy;
- approval and short-lived repository credential grants for GitHub publication;
- secret scanning before data leaves the host;
- supervision and a kill switch.

Don't send a chat transport token, GitHub credential, Emisar administrative credential or other
landing authority into a co:op prompt or box.
