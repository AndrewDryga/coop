# Coop worker API

A trusted controller supplies code, context and execution settings; Coop owns isolated model
execution and durable workspaces. The worker can share a VM with its controller or run on a
separate VM. The protocol is product-neutral: Ryker is a consumer, not part of its vocabulary.

## Connect

```bash
coop sessions connect --controller https://controller.example --token-file /run/secrets/coop-token
```

This is the only worker startup command. It starts the private local service and connects
outbound to the controller; no inbound TCP port is required. There is no worker JSON file or
local policy YAML. Optional `--state <path>` chooses private storage, and `--ca-file <path>`
trusts a private TLS CA. Public HTTPS uses the system trust store.

The single-use token determines the worker identity. Coop saves the renewable identity under
the state directory and removes the token file only after saving it. Reconnect using the same
controller and state directory without `--token-file`. A saved identity cannot be reused with
a different controller.

Default state: `~/.local/state/coop/sessions`. The internal Unix socket is
`<state>/control.sock`. Run the command under a process supervisor. Ctrl-C stops the connection
and any local service it started. It does not stop an already-running service it reused.

Workers need Git, Git LFS and a supported container runtime. The bundled worker image includes
Git LFS; on a native worker install `git-lfs` with your package manager.

A repository's own `.agent/Dockerfile` is repository code, not worker configuration, so a
worker never builds or runs it for a job. Jobs run in the worker's base image
(`COOP_BASE_IMAGE`); a job with filtered networking runs Coop's locked client image instead.
Put what jobs need in the base image; a normal-mode job still provisions the repository's
`.tool-versions` when its box starts.

```bash
coop sessions doctor
coop sessions doctor --socket /var/lib/coop-sessions/control.sock --json
```

The local socket and state are private to the worker OS account. Never mount that socket into
a model sandbox or expose it through an unauthenticated proxy. Outbound connectivity alone
does not authenticate the remote controller: TLS verifies the destination before Coop sends
an enrollment token or accepts work.

## Session authority

The controller supplies one immutable job with the source, model targets, execution mode,
network access and resource limits. Coop validates and saves it before creating a workspace.
Retries must carry the same configuration; restart uses the saved configuration. Repository
instructions and model output cannot widen it. Old session history remains readable, but an
old row without verifiable execution authority must not resume model work.

Coop gives each session store one stable ID inside `session.sqlite`. A new fork reservation binds
that store ID, the session ID, and the fork's exact generation. The store ID moves with the
database, so relocating `--state` does not change ownership. Only the owning session service may
discard the fork after checking its journal, session binding and workspace safety conditions;
`coop fork rm --force` is not an override. Old reservations without a store ID stay visible and
protected from generic fork removal, but Coop cannot infer their owner from a matching session
ID in some current store. Such sessions remain readable and require explicit offline recovery,
not automatic relabeling or age-based deletion.

The trusted worker fetches repository data directly from GitHub using a short-lived,
repository-scoped grant supplied by the controller. The model receives the full working tree,
including verified LFS payloads and every explicitly authorized recursive submodule. Tokens never
enter model mounts, repository configuration or exported results. Repository files cannot redirect
the worker's authenticated fetches. Missing objects, unauthorized submodules and unsupported LFS
pointer formats fail creation rather than leaving an incomplete tree.

### Workspace storage


Every hello carries an optional `storage` object: the worker's own account of the volume its fork
workspaces land on, taken from the daemon's [`GET /v1/storage`](#health) and forwarded
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

`disposable_bytes` is what a discard could still return; `protected_bytes` is what this worker is
holding on purpose, including the hardlinked repository baseline the forks share. A `null`
`unattributed_bytes` means the worker found storage it could not attribute, or could not finish
measuring — treat it as unknown, never as zero. `allocation` is `refused` with a `refusal_reason` of
`reserve_exhausted` or `protected_storage_exceeds_budget` while this worker will not accept a new
workspace; placement and cleanup of work it already has continue either way.

The watermarks are USED-byte levels and the reserve is a free-byte floor. By default they are
derived from the measured capacity: a reserve of 5%, allocation closing when free space falls under
one reserve, and reopening only once two reserves are free — the hysteresis is what stops a worker
from flapping after every reclaimed workspace. A worker that cannot measure its volume, or whose
daemon predates this endpoint, simply omits the object and keeps polling.

An unbound generation or clean workspace is not proof of abandonment: another session store or an
unfinished create may own it. Coop reports it as protected in `/v1/storage` and leaves it for manual
inspection. Automatic maintenance only finishes removals already staged by an authorized discard.
An interrupted removal is resumable: the workspace is renamed into an owner-private staging
directory before any deletion, and no discard reports success until those bytes are gone.

### Restart and recovery

Stop the connector with `SIGINT` or `SIGTERM`. Preserve the identity file and the entire journal
directory across restarts, together with the same worker/workspace configuration. A restart loads
the existing identity rather than enrolling again; certificate renewal is automatic over mutual TLS.

Startup, under the exclusive state lock, reclaims only unpublished source stages, transfer temps
and host-only Git credential files. Published sources and journaled response bodies are retained.
Run managed workers under a service manager that stops the whole process group or cgroup: a host
Git/LFS child surviving a forced parent kill can keep deleted blocks open until it exits.

Commands are journaled before execution. Unacknowledged terminal results are resent after restart.
Redelivery of a command with a recorded result reuses its receipt rather than executing it again;
reusing that command ID with a different payload is refused. An asynchronous
session creation can have its result acknowledged while creation is still running: the journal
retains its original operation identity until session activity can be bound. Event cursors advance
only after exact acknowledgements, so unacknowledged events replay and acknowledged events do not.
Do not prune journal files or keep only the `commands` directory when moving or backing up a worker.

If a review outlives its request, its uncertain transport receipt stays unchanged.
Read the saved operation through `GET /v1/operations?key=<original-key>` and, once successful,
fetch its retained review dossier. These are ordinary API requests; neither lookup reruns the gate.

An unavailable daemon or controller is reported and retried; it does not authorize local execution
or receipt deletion. A malformed or expired saved identity fails closed instead of silently
re-enrolling, even if an enrollment token is present. Check file ownership, configured trust,
clock and controller renewal status, then follow the controller operator's identity-recovery
procedure. Do not delete the identity or journal as a routine reconnect fix.

### Session inspection evidence

A controller may ask a worker for one bounded, versioned account of a session it hosts, through
the `get_session_evidence` command. The connector maps it onto a plain owner-private
`GET /v1/sessions/{id}/evidence` (see the `session evidence` endpoint below) and forwards the
daemon's answer verbatim; it selects nothing, derives nothing and adds no disclosure of its own.

The object carries the session's frozen network posture and what its runs were observed doing,
plus the host-approved task bound into its workspace as the task folder stands at capture. It
never carries a credential, a host path, a packet body, or — unless the session job set
`egress.export_destinations: true` — a destination name. The agent-written `state.md` is bounded
and withheld whole when it scans as carrying a secret.

Every section states its own availability, so a control plane can tell an unreadable registry from
a session that observed nothing: an unavailable section names its cause, a filtered session that
has not run reports `no_run`, and a session that never ran filtered reports `not_filtered`. A
daemon answer that does not satisfy the contract fails the command with `invalid_session_evidence`
rather than being forwarded.

The worker advertises `session-evidence` version `1` only after the running local daemon publishes
`session_evidence_versions` in `GET /v1/capabilities`, independently of the other two capabilities.
A configured claim cannot override that check, and a worker whose daemon predates the endpoint
simply does not advertise it — which is what lets a controller say "this build does not export
evidence" instead of showing an empty network.

One sealed filtered run's refusals also reach the controller as the existing `network` session
event, grouped and capped by the daemon exactly as `coop net` prints them, with the same
destination projection.

## Request rules

Examples below use curl's Unix-socket support:

```bash
SOCKET="$HOME/.local/state/coop/sessions/control.sock"
curl --unix-socket "$SOCKET" http://localhost/healthz
```

Every mutation is `POST` with:

```text
Content-Type: application/json
Idempotency-Key: <globally unique caller-owned key>
```

The key and content type must each occur exactly once. Mutation URLs accept no query parameters.
Ordinary mutation bodies are limited to 128 KiB; turn-submission bodies are limited to 12 MiB so
they can carry bounded input artifacts. Every body is decoded as exactly one JSON value with
unknown fields rejected. Prompts are limited to 256 KiB of valid UTF-8 without NUL.

Use a stable key for one logical action. Repeating the exact method, key, and canonical body returns
the recorded result without repeating the action. Reusing a key with a different method or body
returns `idempotency_conflict`.

After a lost response, retry the same request with the same key. If an earlier response supplied an
operation ID, `GET /v1/operations/{operation_id}` reports its state, error code, and resource ID.
When a mutation response may have been lost after admission, `GET /v1/operations?key=<idempotency-key>`
recovers the same public projection by the caller's exact bounded idempotency key. The query accepts
one `key` only and never returns the key, request hash, private intent, or result body.
Operation lookup deliberately omits the stored request, private result, prompts, native provider
IDs, and host paths.

Successful turn mutations store a compact public replay receipt rather than a second copy of the
turn prompt and private execution controls. Upgraded daemons can still read older full-turn
receipts. An operator can rewrite those old receipts only during an explicit maintenance window:

```bash
coop sessions compact --state <state-root> --backup <new-backup.sqlite>
```

The command takes the controller's exclusive state lock, refuses an existing backup path or one
inside the state root, verifies
the backup before one transactional rewrite, checks database integrity, and only then vacuums the
database. It never changes canonical turn rows and never runs automatically during startup.
The owner-only backup retains the legacy prompt copies and is sensitive. Delete it only after the
compacted controller and its ordinary backup cycle are verified.
To restore, stop the controller, move `session.sqlite` and both possible `-wal`/`-shm` sidecars
aside together, install the backup as owner-only `session.sqlite`, restart, and verify with
`coop sessions doctor` before removing the rollback set.

When an owner revokes authority after durably preparing a session create or turn submit but cannot
know whether the request crossed the socket, it must not replay the mutation merely to discover a
resource to stop. `POST /v1/operations/fence` uses the **target mutation's** idempotency key and a
strict envelope containing its exact typed request:

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

Only `CreateRemoteSession` and `SubmitTurn` are fenceable. If the target operation is absent or only
reserved, Coop records it as failed with `operation_fenced`; an exact later mutation therefore cannot
execute. If admission already won, the fence returns that existing operation and its resource for
ordinary close/cancel reconciliation. The target request may be as large as a turn submission plus
the bounded envelope. A lookup miss or elapsed client timeout is never absence proof.

Session creation can be admitted without holding the HTTP request open. Send
`Prefer: respond-async` on `POST /v1/sessions`; Coop durably records the create intent and returns
`202 Accepted` with an `operation` in `running` or terminal state. Poll
`GET /v1/operations/{operation_id}` until it succeeds, then fetch the returned session resource.
The create continues under the daemon lifetime if the client disconnects. An exact replay with the
same key coalesces onto the same operation. Callers that omit the preference retain the synchronous
response for compatibility.

Session lifecycle, budget, and cancellation requests carry `expected_revision`. Read the current
session before acting and treat `revision_conflict` as a request to reconcile, not as permission to
guess a new action. A validation decision instead compares `candidate_sha256`, because its authority
is the exact unpublished candidate rather than the broader session revision.

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

One worker owns a session. Turns are a bounded durable FIFO and only one runs at a time. Without
`warm_idle_timeout`, Coop starts one short-lived `coop fork <name> acp <target>` child for a turn,
resumes the exact recorded native session, records only its terminal assistant message for public
consumption, and tears down the child and run-labeled box before parking. A `readonly` session's
child is `coop fork <name> acp <target> --readonly` and a `bare` session's is
`coop acp <target> --bare`; neither resumes a native session (the restricted box keeps none), and
a bare box is reaped by its run receipt alone, having no fork or project registry behind it.

With `warm_idle_timeout`, Coop can prepare the authenticated ACP connection before the first turn
and reuse that exact process and native session across serialized turns. The daemon retains at most
20 warm sessions. Expiry, cancellation, failure, close, discard, or daemon shutdown stops the
process, removes its run-labeled box and services, and deletes projected credentials. Provider-native
history remains durable, so the next cold child can load the exact session again.

On restart, queued turns that were never sent remain eligible. A turn waiting for semantic
validation keeps its unpublished candidate and can still be accepted or rejected by digest. Its
provider process, run-labeled box, projected credentials, and services are disposable: startup and
the bounded periodic cleaner reap them without accepting, rejecting, or changing the candidate. A
turn interrupted after send intent is terminalized as interrupted and is not silently replayed.
Provider-native history is retained.

An optional `output_contract` carries the exact schema bytes and their digest:

```json
{
  "json_schema": {"type":"object","required":["answer"]},
  "sha256": "<lowercase SHA-256 hex of the exact json_schema bytes>",
  "require_semantic_validation": true
}
```

When `output_contract` is present, `json_schema` and `sha256` are required, the schema is capped at
256 KiB, and admission compiles the schema only after the digest matches. Coop persists that exact
contract, gives it to the model, and validates the final assistant bytes before completion. Invalid
JSON or a schema mismatch is repaired in the same native session. Without semantic validation,
three invalid provider responses fail the turn with `output_contract_failed` and publish no
assistant message.

Set `require_semantic_validation: true` when the caller also owns checks that depend on external
frozen state. After schema validation, Coop exposes the exact unpublished bytes and SHA-256 digest
as `turn.candidate` with state `awaiting_validation`. The durable
`validation_candidate_sha256` remains visible after the decision so a caller can reconcile a lost
HTTP response. The caller sends one idempotent decision to:

```text
POST /v1/sessions/<session>/turns/<turn>/validation
{"candidate_sha256":"<digest>","verdict":"accept"}
```

or rejects it with `verdict: "reject"` and 1–20 `violations`; each violation and the combined list
are capped at 4 KiB. Acceptance forbids violations,
copies only the stored candidate into `assistant_message`, and returns a durable
`validation_receipt`. Rejection re-prompts the same native session under the same logical turn.
Semantic review allows up to three schema-valid candidates; each semantic round separately allows
up to three provider responses to repair malformed or schema-invalid output, so a schema repair
does not spend a caller-review attempt.

Close is non-destructive. It preserves the fork, conversation state, events, and reviews. Discard is
a separate two-step compare-and-swap action available only for a closed, idle session.

The API can publish an explicitly authorized reviewed candidate as a draft pull request. It
cannot merge, sign, mutate the local parent ref, run an arbitrary host command, or return a host
workspace path.

## Endpoints

### Health

| Method | Path | Result |
| --- | --- | --- |
| `GET` | `/healthz` | `{"healthy":true}` |
| `GET` | `/readyz` | `{"ready":true}` after controller startup |
| `GET` | `/v1/capabilities` | `{"controller_tools_versions":[1],"repository_freshness_receipt_versions":[2],"session_evidence_versions":[1]}` for caller-side protocol negotiation |
| `GET` | `/v1/capacity` | current shared active/warm runtime slots; the connector forwards this measurement in each heartbeat |
| `GET` | `/v1/storage` | this daemon's own workspace-storage accounting: `storage` (the object a fleet controller reads), `budget`, `totals`, `roots`, `forks` and `problems` |

Runtime capacity is four shared slots for active and warm model processes. The session, turn and
workspace counts describe that same pool, not independent allocations. A queued turn waits before
its model timeout starts. Failed teardown retains its slot until cleanup succeeds; unresolved
startup runtime custody reports busy with zero free slots. A failed capacity read also reports
busy, without preventing the worker from polling for reads and cleanup.

`/v1/storage` measures allocated blocks, never apparent size, and it never opens a file — so it can
account credential-bearing private session state without reading any of it. A fork's git objects are
hardlinked from the checkout it was cloned from, so every total is the storage that would actually
be returned by removing that content, with the multiply-linked baseline reported once in
`totals.baseline_shared_bytes` instead of charged to each fork. A measurement that could not see
everything reports `totals.unknown` and publishes `storage.unattributed_bytes` as `null`: unknown is
not zero. A full tree walk is too expensive for every caller, so the answer is re-measured at most
once per `budget.measure_seconds` and `storage.measured_at` carries its age.

`forks[]` names each directory under a fork root, its category, and the evidence for it. Only
`disposable` and `staged_discard` have an authorized reclamation path; `active`,
`grace`, `protected` (a running worker, registered sandbox activity, an interrupted land, canonical
task authority, a quarantined session, or uncommitted work), `control` and `unattributed` are all
refusals with a reason. A directory with no coop generation record is `unattributed`: it is
reported, and it is never deleted on a guess.

Under storage pressure the daemon refuses a NEW workspace with `storage_unavailable` and names its
cause. Cleanup, discard and reads keep working, and nothing protected is deleted to make room.
Checkpoint capture and restore allocate additional copies, including during recovery, so their
separate pressure guard waits above the reserve while keeping interrupted restores fenced. The refusal
closes at `storage.high_watermark_bytes` of USED space and reopens only under
`storage.low_watermark_bytes`, so reclaiming one workspace cannot flap it open and shut.
`storage.reserve_bytes` is the free-space floor underneath both, and new work never spends it.
None of this bounds what an already-running task writes inside its own workspace.

The outbound connector reports `repository-freshness:2`, `session-evidence:1` and
`controller-tools:1` only after its local daemon proves the corresponding versions through
`GET /v1/capabilities`. Missing proof removes that capability from the next heartbeat.
The controller can distinguish an unsupported evidence export from a session that observed nothing.
Worker protocol v2 carries capabilities, capacity and optional storage measurements, not policy
or repository catalogs. Exact code and execution settings belong to each immutable controller job.

### Sessions

The controller creates `create-session.json` with `task` (an opaque external reference),
`job` (a version-1 JobSpec), and its canonical SHA-256 `expected_job_digest`.
Optional `controller_tools` binds the controller's authenticated tool endpoint.
See the shared [protocol fixture](../testdata/protocol/coop-worker-v2.json) and
[JobSpec definition](../internal/workerproto/job.go) for exact fields and validation.

```bash
curl --unix-socket "$SOCKET" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: controller:create:01J...' \
  -H 'Prefer: respond-async' \
  --data-binary @create-session.json \
  http://localhost/v1/sessions
```

The job selects model targets, execution mode, project environment/MCP exposure, network
rules and queue/turn limits. Coop freezes it before creating a workspace; a changed retry is
refused. Job settings cannot exceed worker hard limits. Task text is not execution authority.
There is no local policy name, policy catalog, separate source selector or worker JSON file.

#### Frozen repository source

The controller resolves the requested default branch, branch, PR or commit before creating
the job. `job.source` identifies the GitHub repository by immutable ID and slug, and contains
the exact default/selected/base commits, admitted tree and source binding. It also lists every
authorized recursive submodule with its exact repository, commit and tree. No host checkout
path, remote URL or GitHub token belongs in this document.

The trusted worker obtains short-lived repository-scoped grants through its authenticated
controller connection, fetches Git/LFS directly from GitHub and verifies the complete working
tree. Untrusted `.gitmodules` and `.lfsconfig` cannot redirect authenticated requests.
Missing history, missing LFS payloads, mismatched gitlinks or insufficient disk fail closed.
A job with `source: null` uses an isolated empty workspace (or no workspace in bare mode).

The public session carries the immutable source binding, `job_ref`, `job_digest`, target,
execution mode, project exposure flags, frozen network posture and repository freshness.
It also includes companion aliases, base commits, generated fork name, revision, state,
activity, queue/budget counters, event cursor and timestamps. Host paths, provider state,
prompts and credentials are private. Historical sessions without verifiable jobs remain
inspectable but cannot resume model work.

With `Prefer: respond-async`, creation returns status 202 and the operation. Poll that operation;
a successful `resource_type: session` identifies the session to fetch. Without the preference,
creation waits and returns the operation and session together.

| Method | Path | Body/query |
| --- | --- | --- |
| `POST` | `/v1/sessions` | `task`, `job`, `expected_job_digest`; optional `controller_tools` |
| `GET` | `/v1/sessions?limit=100` | `limit` is `1..1000` |
| `GET` | `/v1/sessions/{session_id}` | none |
| `POST` | `/v1/sessions/{session_id}/prepare` | `expected_revision`; the job must enable warm execution |

A bare session has no workspace. Changes, workspace binding, checkpoint, restore and review
refuse it. Turns, events, budget, cancel, close and discard remain available.

### Workspace checkpoints

| Method | Path | Input/output |
| --- | --- | --- |
| `POST` | `/v1/sessions/{id}/checkpoint` | JSON: `session_ref`, `expected_revision`, `placement_generation`, `repository_ref`; returns operation and descriptor |
| `GET` | `/v1/operations/{id}/checkpoint-bundle` | Streamed tar, exact `Content-Length`, SHA-256 `ETag` |
| `POST` | `/v1/sessions/{id}/workspace/restore` | Binary body plus `X-Coop-Expected-Revision` and base64-JSON `X-Coop-Workspace-Checkpoint` headers |

Capture requires an idle writable session with a bound task. Restore requires an unused
replacement session with the same authorized base and task offer. All mutations need an
idempotency key. Restore's content type is
`application/vnd.coop.workspace-checkpoint.v2+tar`; its Content-Length must equal the descriptor.

V2 streams raw typed Git objects and verified LFS payloads, including new committed history,
the final tracked tree, untracked files and task state. It restores the exact HEAD and working
tree, not the original staged/unstaged split. Changed submodule commits require separate
authorized custody and are refused, including intermediate changes later reverted.
Memory and metadata are bounded; there is no 64-MiB data cap. Disk-pressure checks and
cancellation apply throughout transfer and Git work. These are pressure protection, not a
filesystem quota against other host writers.

Every member and task binding is verified in quarantine before live files change. Coop retains
the exact body before marking restore Running; incomplete restores refuse execution across
restart and resume from that body. Retry the same operation, never a new key against an already
bound session. Capture holds the runtime too, so it cannot race a model turn. A restored
checkpoint does not inherit a publication approval: review the candidate again before publishing.

Discard removes only checkpoint artifacts whose local session ownership is proven and settles
interrupted restores of that discarded session. Historical v1 artifacts remain readable;
their patch-only format cannot prove full Git/LFS custody, so v1 restore is explicitly refused.

### Turns

Submit:

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
| `POST` | `/v1/sessions/{session_id}/turns/{turn_id}/validation` | `candidate_sha256`, `verdict`; `accept` forbids `violations`, `reject` requires 1–20 |

Validation decisions do not carry `expected_revision`; `candidate_sha256` provides the optimistic
concurrency check against the exact unpublished candidate being accepted or rejected. Before an
accept, reject, or awaiting-turn cancellation changes durable state, Coop reaps the provider
runtime and projected credentials that produced the candidate. If that proof fails, the API returns
`503 session_cleanup_error` and leaves the exact candidate `awaiting_validation`; it has not applied
the verdict. The failed idempotent operation is terminal, so after the janitor or operator restores
runtime cleanup, fetch the still-current candidate and submit the same decision with a fresh
idempotency key. Replaying the failed key returns the same failure.

A public turn excludes its prompt, its idempotency data, `min_target_index`, and `rewind_target` —
request data whose effect is published as the session's `target` and its
`session.target_rotated` event. A
completed turn's `assistant_message` is the user-facing response. Coop does not publish hidden
reasoning, raw tool calls, raw ACP frames, or box logs.

A completed turn carries `usage` when the adapter reported what it cost: `input_tokens`,
`cached_input_tokens`, `output_tokens`, `reasoning_tokens`, and provider-reported `cost_usd` when
available. `cost_recorded` distinguishes an exact zero from an adapter that reported no money.
ACP reports money as a cumulative session counter; Coop normalizes it into this turn's delta before
publishing it, including across daemon restarts and adapter-process resets. Cached input is reported
apart from fresh input because providers price the two differently, and a caller costing a merged
figure would overcharge itself. The object is omitted entirely when nothing was reported, so an
unmeasured turn stays distinguishable from a genuinely free one instead of reading as zeros.

A completed turn may include `output_artifacts` metadata for images generated by the agent or saved
under the turn-specific `.coop-output/<turn_id>` directory. Each record contains an opaque ID,
safe filename, media type, SHA-256 digest, and exact byte count, never inline bytes. The raw endpoint
returns that one immutable file with matching `Content-Type`, `Content-Length`, and `ETag` headers.
Typed image and embedded image-resource blocks nested in ACP tool updates are captured in output
order as `generated-1.<ext>`, `generated-2.<ext>`, and so on; their encoded bytes do not consume the
text transcript budget. Text tool updates are inspected one bounded frame at a time and discarded,
so their cumulative bytes do not terminate a legitimate long turn. Assistant text, each wire frame,
the turn deadline, and durable output artifacts remain independently bounded.
Only PNG, JPEG, WebP, and GIF are accepted. Coop rejects symlinks, special files, mismatched content,
more than four files, a file over 8 MiB, or more than 8 MiB total. The scratch directory is removed
before the turn completes, so generated charts do not appear as repository changes. For a legacy
read-only repository session the path is still `.coop-output/...` in the box, but its host source
lives in Coop's session state rather than beneath the checkout; the repository mount remains wholly
read-only.

Cancellation asks ACP to cancel, then stops and reaps the exact process group and run-labeled box.
It does not claim that external side effects were reversed.

### Events

```bash
curl --unix-socket "$SOCKET" \
  'http://localhost/v1/sessions/remote_.../events?after=41&limit=100'
```

Events are returned as a JSON array ordered by monotonically increasing per-session `sequence`.
Persist the last processed sequence and request `after=<sequence>` after a disconnect.

Owner-private events contain identity, sequence, turn ID, type, version, timestamp, and the event's
own payload. Bounded raw tool evidence can include filesystem paths, arguments, results, and diffs.
The separate outbound worker projection strips those raw fields; its structured `path_context`
contains only project-relative paths and scope warnings, never the host checkout root.
Each payload is capped at 256 KiB and a page is bounded by total
bytes as well as by `limit`, so a caller reading a chatty turn gets a short page rather than a
truncated one.

Lifecycle event types — what Coop itself decided:

```text
session.created
session.state_changed
turn.queued
turn.started
turn.awaiting_validation
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

`network` is appended after a filtered run seals, and only when that run hit the boundary — a quiet
run costs no event, so this stream carries refusals rather than a per-turn heartbeat. Its payload is
bounded by construction: destinations are grouped and capped with an `omitted_destinations` count,
alerts are capped, and `evidence_id` names the retained event `coop net blocked` can open. It
follows the same disclosure scope as the network routes, so a destination appears only when the
session job set `egress.export_destinations: true`.

Activity events narrate the interior of a turn — what the model did, as against what Coop decided —
and are always sequenced before the turn's own terminal event, so a caller that stops polling at
`turn.completed` has already seen them:

```text
tool.started            # tool_call_id, title, kind, input, path_context
tool.completed          # tool_call_id, title, kind, status, input, output, content, locations, path_context
model.plan              # entries
model.thought           # text
model.progress          # outward commentary text, not private reasoning
permission.decided      # tool_call_id, title, outcome, option_id, option_kind
activity.elided         # dropped, reason
provider.backoff        # attempt, target, next_target, retry_after_seconds, reset_at, all_limited_until
provider.alive          # frames, bytes
```

When structured filesystem evidence is available, `path_context` contains `basis: "lexical"`
and up to 16 `paths` entries. Each entry identifies its evidence with a JSON-pointer `source`,
a `scope` (`project`, `outside`, or `unknown`), and a `path` only for project-relative files.
The bound checkout root is not exported. `partial: true` means some path evidence was omitted.
These are lexical display facts, not symlink-safe containment or permission decisions. Missing
provider paths stay missing; titles and shell commands are not parsed to invent them. A later
tool update can add paths to its completion event without changing the original start event.
Path facts are extracted before large diff bodies become partial previews. URI-shaped paths are
reported as unknown, not interpreted as files inside the checkout.
The outbound worker projection separately validates these fields and still excludes raw titles,
commands, and tool arguments.

`provider.backoff` is how a throttled turn stays audible. One event per proven rate limit, which
the ladder bounds to one per rung: `attempt` numbers them from 1 within the turn, `target` is the
rung the provider limited, and `retry_after_seconds` is how long that rung is out — the provider's
own reset when it named one, the ladder's bounded backoff when it did not. A rotation also carries
`next_target`; the last backoff on an exhausted ladder carries `all_limited_until` instead. Without
it a turn crawling through 429s was indistinguishable from a dead one, and a client watching for
silence would cancel work that was making progress.

`provider.alive` covers the throttle Coop cannot see. A backoff is narrated when Coop's own ladder
acts on a limit; a provider CLI retrying 429s INSIDE itself never reaches the ladder, and that turn
streams frames while making no tool calls and producing no other events at all. The pulse says the
transport is still moving: `frames` and `bytes` count every ACP frame read for that turn's prompt,
cumulatively, so a client re-reading its cursor tells new progress from a redelivered event by the
numbers rather than the timestamp. It is emitted at most once a minute, and never in a window where
the turn already narrated something — an ordinary turn produces none, and a long tool call is not
described twice. A turn producing no frames at all still produces no events, which is what a client's
silent-turn deadline is for.

### Budget

```bash
curl --unix-socket "$SOCKET" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: controller:extend:01J...' \
  -d '{"expected_revision":9,"additional_turns":20}' \
  http://localhost/v1/sessions/remote_.../budget
```

Extension is bounded by Coop's global `max_turns` limit. Authorization and spend policy belong to
the caller; Coop only enforces the numeric bound and idempotency.

### Changes

```bash
curl --unix-socket "$SOCKET" \
  http://localhost/v1/sessions/remote_.../changes
```

`GET /v1/sessions/{session_id}/changes` returns:

- immutable `base_commit`, current `fork_head` and `fork_tree`, and current `parent_head`;
- for a repository-backed session with a source binding, the immutable `admitted_source_tree`,
  allowing callers to distinguish new content from empty commits or commit-and-revert history
  above the admitted source — whichever kind it was;
- committed, staged, unstaged, untracked, and conflicted typed path records;
- ahead/behind and base-to-head divergence counts;
- a bounded binary patch page;
- `patch_digest`, exact `patch_bytes`, `patch_offset`, `patch_next_offset`, and
  `patch_has_more`.

Call
`GET /v1/sessions/{session_id}/changes?patch_offset=<next>&patch_limit=<bytes>` to read another
bounded page. `patch_limit` cannot exceed the session job's `max_patch_bytes`; offsets are
bounded to 1 GiB. Consumers must bind navigation to `patch_digest` and restart at offset zero if
the digest changes. The legacy `truncated` field is true whenever the response is not the complete
patch.

JSON encodes `patch` and every `*_bytes` field as base64. A normal UTF-8 path also appears in
`path`; an arbitrary byte path is preserved in `path_bytes`. Empty change lists are not used to
hide Git failures: failures return an error.

Changes may inspect dirty work. Review may not.

### Network

```bash
curl --unix-socket "$SOCKET" http://localhost/v1/sessions/remote_.../network
curl --unix-socket "$SOCKET" http://localhost/v1/sessions/remote_.../network/connections
curl --unix-socket "$SOCKET" http://localhost/v1/sessions/remote_.../network/explanations/<event_id>
curl --unix-socket "$SOCKET" http://localhost/v1/sessions/remote_.../network/receipt
```

All four are reads. Neither probes a gateway, and neither can grant, approve, or widen anything: the
session API has no path to network authority at all. `GET /v1/sessions/{session_id}/network`
returns the session's frozen `mode` and `fingerprint`, the `requested` and `effective` rule texts,
`current` — the newest run's retained observation summary, or `{"status":"no run yet"}` — and a
bounded `alerts` list. `GET /v1/sessions/{session_id}/network/receipt` aggregates every run the
session owned into one versioned receipt with independent `finality` and `completeness`: it is only
`final` once the session is closed and every run receipt is, and a `final` receipt may still be
honestly `partial`. Because the receipts are retained in the owner's own registry, both survive
container garbage collection. A session that never ran filtered answers
`{"available":false,"reason":"..."}` rather than an empty receipt.

`GET /v1/sessions/{session_id}/network/connections` is the live drilldown: the newest run's bounded
connection rows as the collector recorded them, with `status` (the same freshness word the summary
uses), `as_of` and `detail_truncated`. It is pull-based — a per-second sample belongs in nobody's
durable journal — and a session with no run yet answers `{"status":"no run yet","connections":[]}`.

`GET /v1/sessions/{session_id}/network/explanations/{event_id}` opens ONE retained refusal of this
session's own runs: the event, the reason, its provenance and whether a candidate rule was drafted.
An event that aged out of its run's bounded ring answers `{"available":false,"reason":
"event_not_retained"}`, which is not proof the id never existed.

`projection` states the disclosure scope. It is `destinations-withheld` unless the session job
set `egress.export_destinations: true`, in which case it is `destinations-included` and the rule
texts and observed names are present. The daemon owns that projection; the outbound worker forwards
exactly what the daemon answered through ordinary GET API requests — each one a plain
GET of the route above, with no destination, rule or disclosure scope of its own to choose.

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

One bounded, versioned account of a session for a control plane's inspection page: the network
posture it was admitted under, what its newest run was observed doing, the session-wide receipt,
and the host-approved task bound into its workspace as the task folder currently stands. It is a
read — no gateway probe, no runtime, no authority — and takes no query, so nothing about the
disclosure can be selected by the caller. The outbound worker fetches it through the
`get_session_evidence` command and forwards it verbatim after confirming it satisfies its own
contract; a daemon answer that does not is a definite `invalid_session_evidence` failure rather
than an object a controller has to guess its way through.

Every section states its own availability, because a section that could not be read and a section
with nothing in it lead an operator to opposite conclusions:

| Section | Status words |
| --- | --- |
| `network.access` | `captured`, `not_filtered`, `unavailable` |
| `network.observation` | `observed`, `no_run`, `not_filtered`, `unavailable` |
| `network.receipt` | `available`, `not_filtered`, `unavailable` |
| `task` | `bound`, `unbound`, `unavailable` |
| `task.snapshot.state_note` | `captured`, `absent`, `withheld` |

`unavailable` always carries a `reason`. The posture (`mode`, `fingerprint`) comes from the
immutable session row, so a registry this host cannot read leaves the posture known and every
section beside it explicitly unreadable. A filtered session that has not run yet reports
`no_run` and a `provisional` receipt with `run_count` `0` — never an empty counter.

Counters are unsigned decimal STRINGS so a value above 2^53 survives a JavaScript client, and a
`null` counter is a metric nobody measured, never `0`. Lists are bounded on the way out with the
drop counted (`omitted_denials`, `omitted_connections`, `omitted_alerts`,
`omitted_run_references`), so a run that denied a thousand names costs one bounded object that
still says how much it left behind. The same `projection` rule as the network routes applies: a
destination appears only under `destinations-included`, and a refusal whose name was withheld says
so with `destination_withheld` rather than reporting no name at all.

`task` carries the immutable binding from the session row plus the task folder at capture — its
state, the same `state_sha256` a workspace checkpoint records for its task projection, the
checklist labels the checkpoint's booleans stand for, the file inventory with digests, and the
agent-written `state.md`. The note is bounded and withheld whole when it scans as carrying a
secret. The worker keeps no transition ledger, so this is a snapshot: a control plane records
successive ones rather than asking the worker for a history it does not have. A bound task whose
folder has gone missing reports `unavailable` with its identity intact, never an empty task.

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

Review requires a parked open or exhausted session with no queued turns and a clean committed fork.
It snapshots exact source and parent commit/tree identities, prepares a disposable candidate
against current parent `HEAD`, runs the trusted parent gate read-only, and returns:

- creation base, source, parent, and candidate commit/tree identities;
- the immutable pull-request number/ref/head binding when the session came from an existing PR;
- rebase status and gate status;
- bounded policy findings;
- a bounded inline patch preview from parent tree to candidate tree;
- `patch_truncated`, `candidate_retained`, `publishable`, and stable not-publishable reason codes.

The preview is base64 in JSON. A truncated preview is a transport condition and does not by itself
make the review unpublishable. Coop retains the exact candidate commit and its verified LFS
objects in private host storage, independently of the model workspace. There is no full-patch
transfer or patch-size publication limit. The candidate is one deterministic commit on the
reviewed parent, created before the gate; `source_head` preserves the original model provenance.
Secret checks cover the actual parent-to-candidate delta, including large/binary Git blobs and
LFS payloads. Changing LFS attributes also scans the resulting LFS payloads. Read failures refuse
the review rather than treating unreadable content as safe.

Reviews may outlive the requesting connection. Look up the original operation key,
then retrieve a succeeded review with
`GET /v1/sessions/{session_id}/reviews/{operation_id}`. It returns the same public
`operation` and `review` envelope without starting or resuming a gate. Another
session, a non-review operation, and an unfinished review are refused. Both
`policy_findings` and `not_publishable_reasons` are arrays, including when empty.

`publishable` is evidence about this exact candidate, not permission to push or merge. It is false
for conflict, no/failed gate, startup failure, policy findings, parent or source movement, active
fork ownership, or an empty change. `candidate_retained` distinguishes custody from gate readiness:
a missing or failed gate can still leave an exact snapshot, but grants no publication authority.
Publication must use the retained `candidate_head` unchanged, with a separate controller grant
and compare-and-swap on the destination ref. It must never reconstruct the result from a patch
or from the model's later workspace. Explicit session discard removes its retained candidates;
controllers must keep sessions whose reviews still await publication.

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

An empty `expected_head` requires a new branch. Updating a previously authorized PR requires
its number and exact expected head. The operation returns `202` while running; reconcile its
key and read `GET /v1/sessions/{session_id}/publications/{operation_id}` for the completed
`operation` and `publication` envelope. A retry uses the same key and identical request.
The result is `published` with the exact commit/tree and PR receipt, `conflict` with a verified
existing PR and observed remote head, or `refused` with a stable error code.

The trusted host requests fresh credentials from the authenticated controller at
`POST /v1/coop-workers/jobs/{job_ref}/publication-grants`. The request binds the session, job
digest, review operation, original command key, repository identity and publication body.
The controller returns the same repository identity plus `token`, `expires_at` and its GitHub
App `actor_id`. A `403` permanently refuses this operation; temporary failures remain retryable.
The controller owns approval rules. No product-specific approval format is required by Coop.

Git and LFS bytes go directly from the private retained candidate to GitHub. Coop creates a
draft, or updates the exact App-owned PR while preserving a person's ready-for-review choice.
Lost push and PR responses reconcile before retrying. An active publication prevents discard
of its candidate; model workspace changes cannot alter the reviewed result.

### Close and discard

Close:

```bash
curl --unix-socket "$SOCKET" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: controller:close:01J...' \
  -d '{"expected_revision":15}' \
  http://localhost/v1/sessions/remote_.../close
```

Close requires no active or queued turns and preserves the fork.

Plan a discard only after close:

```bash
curl --unix-socket "$SOCKET" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: controller:discard-plan:01J...' \
  -d '{"expected_revision":16}' \
  http://localhost/v1/sessions/remote_.../discard-plan
```

The plan captures the exact session revision, anchored fork generation and reservation, plus the
workspace inode, branch, head, status digest, running state, dirty state, and unmerged state. The
inode is one stale-plan signal, not authority by itself; apply pins the live directory and rechecks
the complete snapshot before deletion. By default dirty or unmerged work is refused. A caller that
has separate authority to destroy it must explicitly set `accept_dirty` and/or `accept_unmerged`
when creating the plan.

Execute that exact plan:

```bash
curl --unix-socket "$SOCKET" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: controller:discard:01J...' \
  -d '{"plan_operation_id":"op_..."}' \
  http://localhost/v1/sessions/remote_.../discard
```

Discard first proves that the plan belongs to the path session. It then compares all captured state,
refuses a stale/replaced/running workspace, deletes the fork workspace and private ACP state, and
leaves a durable discarded session tombstone. A failed comparison does not delete anything.

A session the daemon quarantined at start — a historical row without an owner-store binding, a v1
reservation, or one whose generation record or workspace is gone — can be neither planned nor
discarded this way, because Coop cannot prove it owns the workspace. Retire the record instead:

```bash
curl --unix-socket "$SOCKET" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: controller:retire:01J...' \
  -d '{"retire_quarantined":true,"expected_revision":16}' \
  http://localhost/v1/sessions/remote_.../discard
```

This tombstones the session row only: queued turns are exhausted, a turn that was active when the
daemon lost authority stays in history as it was, and the workspace, reservation, sidecar services,
and private ACP state stay on disk for the operator to inspect and remove. A session that is not
quarantined is refused with `invalid_session_state`.

Record-only retirement does not restore runtime capacity, including after restart: it provides no
proof that those processes stopped. Inspect and stop the old runtime before bringing up a worker
with a fresh `--state` directory; retain the old directory for its session history.

## Errors

Errors have one shape:

```json
{
  "error": {
    "code": "revision_conflict",
    "detail": "expected revision 4, current revision 5",
    "operation_id": "op_..."
  }
}
```

`operation_id` is present when Coop admitted an operation before the failure. Internal details
remain suppressed on the public API. The daemon emits a bounded JSON diagnostic to stderr with the
same operation ID, method, resource identity, error code, and a sanitized detail; repository and
state-root paths are replaced and secret-bearing lines are removed. Use the ID to correlate the
client error, operation record, and supervisor log without reading SQLite directly.

Common status mapping:

| HTTP | Meaning |
| --- | --- |
| `400` | invalid or over-bounds request |
| `404` | session, turn, or operation not found |
| `409` | idempotency, operation fence, revision, state, queue, budget, resume, uncertainty, or discard conflict |
| `413` | ordinary request body exceeds 128 KiB, or turn submission exceeds 12 MiB |
| `500` | internal failure; host paths and raw internal errors are suppressed |
| `503` | readiness is not ready, or repository/runtime/network/storage authority is temporarily unavailable |

`storage_unavailable` is a retryable `503`: this worker's volume is under its configured pressure
limits, so it will not create another workspace until space is returned or, when the cause is
`protected storage exceeds budget`, until the protected data itself goes. Read `/v1/storage` to see
which bytes are held and why. Place the session on another worker or clear the storage; do not
retry in a tight loop and do not delete protected workspaces to satisfy the quota.

`network_unavailable` is a `503`: the worker cannot enforce or prove the session's frozen network
authority. Inspect the worker's runtime and network readiness. The worker refuses execution rather
than falling back to open networking; do not alter a saved job to get past the refusal.

Treat `operation_uncertain` and `turn.interrupted` as reconciliation states. Never retry a mutation
under a new key merely because its result is unknown.

`RunReview` is the narrow read-only exception: after Coop captures immutable source, parent, and
job identities, replaying the exact request under the same key resumes that frozen review under
the same operation ID. It never recaptures moving repository state. Unreadable or invalid captured
intent remains `operation_uncertain`.

## Operational recovery

| Situation | Action |
| --- | --- |
| Client response lost | Replay the exact mutation with the same idempotency key |
| Stop races a prepared create/submit | Fence the exact operation; close/cancel only if admission already won |
| Socket reconnect | Resume event polling after the last committed sequence |
| Daemon already owns state | Stop or inspect that daemon; do not remove the lock |
| Stale socket after crash | Start the daemon with the same state root; it removes only a socket after acquiring the state lock |
| Queued turn at restart | Coop resumes it in FIFO order |
| Running session create at restart | Coop resumes its durable create intent automatically |
| Reserved operation at restart | Coop records an interrupted-admission failure; nothing external was attempted |
| Running checkpoint restore at restart | Resume its retained body under the execution fence; never submit a fresh key |
| Other running operation at restart | Coop marks it `operation_uncertain`; reconcile rather than replaying under a new key |
| Operation remains running after startup | The periodic watchdog resumes safe creates and marks stale ambiguous mutations uncertain |
| Turn interrupted after send intent | Surface interrupted; do not submit the same human input automatically |
| Active turn must stop | Use the idempotent cancel endpoint, then reconcile the terminal turn |
| Session should stop costing runtime | Wait for park; no agent or Compose service container remains between turns |
| Incident is over | Close; retain the fork for review or an explicit later discard |
| Review patch preview is truncated | Use paged inspection; publish the retained reviewed candidate, never reconstruct it from the preview |

Back up the entire state root while the daemon is stopped. Restoring a database without its
corresponding forks requires operator reconciliation; Coop does not infer ownership from names.

## Consumer responsibilities

A production consumer still needs:

- its own identity and authorization policy;
- durable inbound/outbound delivery and deduplication;
- stable globally unique idempotency keys;
- event cursors and reconciliation workers;
- prompt framing and output redaction appropriate to its transport;
- retention, audit, capacity, and spend policy;
- approval and short-lived repository credential grants for GitHub publication;
- secret scanning before data leaves the host;
- supervision and a kill switch.

Do not send a chat transport token, GitHub credential, Emisar administrative credential, or other
landing authority into a Coop prompt or box.

After every terminal turn, Coop removes the exact workspace-owned Compose service containers using
their project and working-directory labels. Volumes remain so the next turn can restart services
with durable development data. Daemon startup repeats this cleanup for historical sessions after a
crash or upgrade, and a bounded idle-runtime sweep retries cleanup every minute without racing an
active turn. The same sweep reaps an awaiting-validation turn's exact runtime and projected
credentials without changing its candidate. Explicit session discard remains responsible for
deleting the workspace volumes and network.

A completed turn is completed even when that teardown fails: the janitor's retry is the guarantee,
and a slow container runtime must not convert a finished answer into an error. Cleanup failure is
logged with its bounded cause; a turn that itself failed carries the cleanup cause joined with its
own error.
