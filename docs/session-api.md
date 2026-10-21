# co:op worker API

A trusted controller supplies the code, context and execution settings. co:op runs models in
isolation and keeps their workspaces. The worker can share a VM with its controller or run on a
separate VM. The protocol can serve different products. Ryker uses it, but the protocol contains
no Ryker-specific terms.

## Connect

```bash
coop sessions connect --controller https://controller.example --token-file /run/secrets/coop-token
```

This is the only command that starts a worker. It starts a private local service, called the
daemon, and a connector that sends requests to your controller. You do not need to open an
inbound TCP port. There is no worker JSON file or local policy YAML. Use the optional
`--state <path>` to choose where the worker stores its private data. Use the optional
`--ca-file <path>` to trust a private TLS CA; public HTTPS uses the system trust store.

The single-use token identifies the worker to the controller. co:op saves the identity it can
renew in the state directory, then removes the token file. To reconnect, use the same controller
and state directory without `--token-file`. That saved identity works only with the original
controller.

The default state directory is `~/.local/state/coop/sessions`, and the daemon listens on the
internal Unix socket at `<state>/control.sock`. Each connector starts its own daemon and refuses
to reuse one that is already running. Stop the existing connection before reconnecting. To run
several workers at once, give each a separate `--state` directory. Use a process supervisor to
keep the worker running. Ctrl-C stops both the connector and the daemon it started.

A worker needs Git, Git LFS and a supported container runtime. The bundled worker image includes
Git LFS. On a native worker, install `git-lfs` with your package manager.

A worker never builds or runs a repository's `.agent/Dockerfile` for a job. You configure the
worker's image separately from repository code. Jobs use the worker's base image
(`COOP_BASE_IMAGE`), except that filtered jobs use co:op's locked client image. Install the tools
your jobs need in the base image. A normal-mode job also installs the tools listed in the
repository's `.tool-versions` when its box starts.

Before the worker accepts filtered jobs, run [`coop net setup`](networking.md#fleet-workers-and-docker-in-docker)
as the worker's OS user, using the same Docker endpoint and network state directory. This prepares
and checks the networking setup. The session API requires those checks to remain valid; it does
not build the networking images itself. Installing Docker or accepting JobSpec version 2 is not
enough to show that the worker can enforce filtered networking.

To check the local session service:

```bash
coop sessions doctor
coop sessions doctor --socket /var/lib/coop-sessions/control.sock --json
```

The local socket and state are private to the worker's OS account. Never mount the socket into a
model sandbox or expose it through an unauthenticated proxy. Connecting out to a controller does
not, by itself, establish that the controller is trusted. co:op verifies the TLS connection before
sending an enrollment token or accepting work.

## Session authority

Your controller supplies one job that fixes the source, model targets, repository access, network
access and resource limits for the session. co:op checks and saves it before creating a workspace.
Retries must use that same configuration, and restarts use the saved copy. Repository instructions
and model output cannot grant more access. You can still read old session history, but co:op will
not resume model work for an old session whose authorization it cannot verify.

co:op saves a stable ID for each session store in `session.sqlite`. When it reserves a fork, it
records which store and session own that specific instance of the fork. Moving the database with
`--state` preserves the store ID and ownership. Only the session service that owns the fork can
discard it, after checking its journal, the session's link to the fork and the workspace safety
conditions. `coop fork rm --force` cannot bypass these checks.

Old fork reservations without a store ID remain visible, but ordinary fork removal cannot delete
them. A matching session ID in another store is not enough to prove who owns them. You can read
these sessions, but recovering them requires an explicit procedure with the service stopped.
co:op does not assign them to a new owner automatically or delete them because they are old.

The worker fetches repository data directly from GitHub using short-lived credentials that your
controller grants for that repository. The model receives the complete working tree, including
verified LFS files and every explicitly authorized submodule, including nested submodules. Tokens
never enter the model's mounts, repository configuration or exported results. Repository files
cannot redirect these authenticated downloads. If objects are missing, a submodule is unauthorized
or an LFS pointer format is unsupported, creation fails instead of leaving an incomplete tree.

### Workspace storage

The worker can include a `storage` object in each hello message to report disk space on the
volume that holds its fork workspaces. The connector reads this object from the daemon's
[`GET /v1/storage`](#health) endpoint and forwards it unchanged.

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

- `disposable_bytes` is the disk space a discard could still free.
- `protected_bytes` is the disk space the worker must keep, including the repository data shared
  by forks through hard links.
- A `null` `unattributed_bytes` means the worker could not identify what uses some of the space,
  or could not finish measuring. Treat it as unknown, never as zero.
- `allocation` is `refused` when the worker will not accept a new workspace. `refusal_reason`
  explains whether the cause is `reserve_exhausted` or `protected_storage_exceeds_budget`.
  The worker can still handle existing workspaces and clean them up.

The high and low watermarks set limits on used disk space. The reserve is the amount of space
that must stay free. By default, co:op calculates all three from the volume's measured capacity:

- It reserves 5% of the volume.
- It stops accepting new workspaces when free space falls below that reserve.
- It accepts new workspaces again only when twice that reserve is free.

Using different thresholds to stop and resume avoids repeatedly accepting and refusing work as
cleanup frees space. If the worker cannot measure its volume, or its daemon is too old to provide
this endpoint, it leaves out the storage object and keeps polling.

A workspace may look unused because it has no recorded session binding or no uncommitted changes.
That does not mean it is abandoned: another session store or an unfinished create operation may
own it. co:op marks it as protected in `/v1/storage` and leaves it for manual inspection.

Automatic cleanup only finishes removals that an authorized discard has already started. Before
deleting a workspace, co:op moves it into a private staging directory belonging to its owner. If
removal is interrupted, cleanup can resume it. Discard reports success only after the data has
been removed.

### Restart and recovery

Stop the connector with `SIGINT` or `SIGTERM`. Across restarts, keep the identity file and the
entire journal directory, together with the same worker and workspace configuration. A restart
loads the existing identity and doesn't enroll again. Certificate renewal is automatic over
mutual TLS.

At startup, co:op locks the state directory and removes source staging data that has not been
published, temporary transfer files and host-only Git credential files. It keeps published sources
and response bodies saved in the journal.

Configure the service manager to stop the worker's whole process group or cgroup. Killing only
the parent can leave a Git or LFS child running. That child may keep deleted files open, so their
disk space is not freed until it exits.

co:op records each command in the journal before running it. After a restart, it resends finished
results that the controller has not acknowledged. If the controller sends a completed command
again, co:op returns the saved result without running it again. It refuses to reuse a command ID
with a different payload.

An asynchronous create request can be acknowledged while the session is still being created.
The journal keeps the original operation ID until co:op can associate the resulting session's
activity with it. Event cursors advance only when the controller acknowledges the exact events:
unacknowledged events are sent again, and acknowledged events are not. When moving or backing up a
worker, keep the entire journal, including more than just the `commands` directory.

A review may continue after its request loses the connection. The saved delivery result remains
uncertain, even if the review later finishes. Look up the operation with
`GET /v1/operations?key=<original-key>`. Once it succeeds, fetch the saved review report.
Neither request runs the gate again.

If the daemon or controller is unavailable, the connector reports the problem and retries. It
does not run work locally or delete saved results to get past the failure. If the saved identity
is malformed or expired, the worker stops connecting. It does not enroll again automatically,
even when an enrollment token is available. Check file ownership, trusted certificates, the clock
and the controller's renewal status, then follow the controller operator's identity-recovery
procedure. Do not delete the identity or journal as a routine reconnect fix.

### Upgrading a worker

A new co:op build may need to convert the worker's session data when it first opens it. After
conversion, the previous build cannot read that data. Each build can perform only one upgrade
step. If the data is too old, the build stops and tells you which session schema to upgrade
through first. Before replacing the worker, check its data with the new build, whether you are
upgrading by hand or with a script:

1. Wait for the commands the worker is carrying out, then stop the worker.
2. Run the check with the new build or image, against the worker's state directory:

   ```bash
   coop sessions check --state /var/lib/coop/sessions
   ```

   In a container, run it from the new image with the worker's volumes mounted, as the worker's
   own user.
3. Act on its exit status:

| Exit | Meaning | Next step |
| --- | --- | --- |
| `0` | The new build can use the data unchanged, or the service has initialized its storage without saving any sessions yet | Start the new build |
| `3` | The new build will convert the data, and older builds can't open it afterwards | To keep a way back, copy the stopped state directory aside, then start the new build |
| `4` | The data is too old for this build to convert, or was written by a newer build | Restart the old build. If the data is too old, first upgrade through a build that uses the session schema named in the message. |
| `1` | The check could not finish: the service is still running, the path has no session data, or some data cannot be read | Fix the reported problem and check again. A directory that no service has used cannot pass, even if it is empty. Check for a mistyped `--state` or a new volume. A worker that has never run has no data to check. |
| `2` | The arguments are invalid | Fix the command |

The command checks both the session database and the connector's command journal. It does not
write to the state directory or create SQLite's `-wal` and `-shm` files. While checking, it holds
a shared state lock to prevent a service from starting and converting the data. If a service is
already running against that directory, the check refuses to read it. Stop the service first so
the data cannot change during the check.

This extra cleanup is needed only if the worker ran a development build from co:op's `main`
branch between 2026-09-15 and 2026-09-23. No released version created these files. Those builds
left a `.service-launch.lock` file in each execution-registry folder. Current builds refuse to
launch in the affected repositories and report the file as an unsupported registry entry.
Stop the worker, then run these commands as its OS user to delete the files and any folders left
empty:

```bash
find ~/.local/state/coop/executions/v1 -mindepth 2 -maxdepth 2 -type f -name .service-launch.lock -delete
find ~/.local/state/coop/executions/v1 -mindepth 1 -maxdepth 1 -type d -empty -delete
```

### Session inspection evidence

Your controller can request a report about a session hosted by the worker. The report has a
versioned format and size limits. Send an `api_request` command with the payload
`{"method":"GET","path":"/v1/sessions/<id>/evidence"}` (see the
[`session evidence`](#session-evidence) endpoint below). The connector forwards the daemon's
answer unchanged. It does not choose fields, calculate extra values or reveal additional data.

The report includes the network settings saved for the session, the activity observed during its
runs and the approved task assigned to its workspace. The task data comes from its folder when
the report is captured. Credentials, host paths and packet bodies are excluded. Destination names
appear only if the session job set `egress.export_destinations: true`. The agent-written
`state.md` has a size limit and is withheld in full if the scan flags a likely secret.

Each section says whether its data was available. Your controller can distinguish a failed read
from a session with no observed activity:

- An unavailable section names its cause.
- A filtered session that has not run reports `no_run`.
- A session that never ran filtered reports `not_filtered`.

Before sending the report, the daemon checks that it follows the evidence format and disclosure
rules. If the report fails that check, the endpoint returns `500 internal_error` instead.

The worker reports support for `session-evidence` version `1` only when its running daemon lists
`session_evidence_versions` in `GET /v1/capabilities`. It checks this independently of the other
capabilities; configuration cannot override the result. An older daemon without the endpoint does
not report support. Your controller can then show that evidence export is unavailable, rather
than displaying an empty network report.

The controller also receives blocked requests and alerts from filtered runs through the existing
`network` session event. The daemon uses the same grouping, limits and destination-disclosure
rules as `coop net`.

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

Choose one key for each action and keep using that key when you retry it. If the method, key and
normalized request body match, co:op returns the saved result without running the action again.
Reusing a key with a different method or body returns `idempotency_conflict`.

After a lost response, retry the same request with the same key. If an earlier response supplied
an operation ID, `GET /v1/operations/{operation_id}` reports its state, error code and resource
ID.

If the worker may have accepted a request but you did not receive its response, look it up with
`GET /v1/operations?key=<idempotency-key>`. Supply the exact idempotency key within its size
limit; the query accepts only one `key`. The response contains the public operation information.
It excludes the key, request hash, saved execution instructions, result body, stored request,
private result, prompts, native provider IDs and host paths.

### Compacting old turn receipts

When a turn request succeeds, co:op saves a small public result that it can return if you retry.
It does not duplicate the prompt or private execution settings in that result. Newer daemons can
still read older results that stored the full turn. You can compact those older records during a
planned maintenance window:

```bash
coop sessions compact --state <state-root> --backup <new-backup.sqlite>
```

The command locks the state directory so the daemon cannot use it during maintenance. The backup
path must not exist or be inside the state root. After verifying the backup, the command rewrites
the old results in one transaction, checks database integrity and vacuums the database to reclaim
space. It does not change the turn records themselves and never runs automatically at startup.

The backup still contains the old prompt copies, so only its owner can access it. Keep it until
you have verified both the compacted database and its normal backups.

To restore:

1. Stop the daemon.
2. Move `session.sqlite` and both possible `-wal` and `-shm` sidecars aside together.
3. Install the backup as an owner-only `session.sqlite`.
4. Restart.
5. Verify with `coop sessions doctor` before deleting the files you moved aside.

### Fencing an uncertain mutation

Your controller may withdraw permission for a session create or turn submission after saving the
request, without knowing whether the worker received it. Do not resend the request just to find
out what to stop: that could start work you no longer want. Fence the request instead, which
blocks it unless the worker has already accepted it.

Call `POST /v1/operations/fence` with the original request's idempotency key and an envelope
containing its exact method and request:

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

- If the worker has not accepted the operation, including when it has only reserved it, co:op
  records a failure with `operation_fenced`. Sending that same request later cannot run it.
- If the worker already accepted the operation, the fence returns it and its resource. Inspect
  that result and use the normal close or cancel request to stop the work.

The enclosed request can be as large as a turn submission, with a separate size limit for the
envelope. A lookup that finds nothing, or a client timeout, does not prove that the request never
reached the worker.

### Asynchronous session create

To create a session without keeping the HTTP request open, send `Prefer: respond-async` on
`POST /v1/sessions`. co:op saves the request and returns `202 Accepted` with an `operation`
that is still `running` or has already finished. Poll `GET /v1/operations/{operation_id}`
until it succeeds, then fetch the session it identifies.

Creation continues in the daemon if the client disconnects. Retrying the identical request with
the same key returns the same operation. Without the preference header, the request waits for
creation to finish.

### Revision checks

Session lifecycle, budget and cancellation requests include `expected_revision`. Read the session
before acting. If you get `revision_conflict`, read it again and decide whether your action is
still appropriate. Do not simply guess a new revision or action. Validation decisions check
`candidate_sha256` instead: they approve or reject one exact unpublished answer, regardless of
other changes to the session.

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

Each session belongs to one worker. The worker saves turns in a queue with a size limit and runs
them one at a time, in the order received. The queue survives restarts.

Without `limits.warm_idle_timeout_ms`, co:op starts a new `coop fork <name> acp <target>` process
for each turn. That process resumes the provider session saved for this co:op session. co:op
publishes only the final assistant message, then stops the process and removes its box before
marking the session as parked.

A job with `repository_read_only: true` uses the same kind of process. The fork is mounted
read-only, and files generated by a turn go into a writable output directory in the worker's
private state.

With `limits.warm_idle_timeout_ms`, co:op can authenticate the ACP connection before the first
turn and keep the same process and provider session for later turns. Turns still run one at a
time. The daemon keeps at most four sessions warm, and each occupies one of the four runtime
slots while idle.

When the idle timeout expires, a turn is cancelled or fails, the session closes or is discarded,
or the daemon shuts down, co:op stops the process and removes its box, services and temporary
credential files. The provider's saved conversation history remains, so a new process can resume
the same session.

When co:op stops a turn's runtime, it also removes the workspace's Compose service containers,
identified by their project and working-directory labels. A successful warm turn keeps its
runtime and services until warm-session cleanup. The service volumes remain after cleanup, so
later turns can restart services with the same development data.

After a crash or upgrade, daemon startup repeats cleanup for historical sessions. A background
check retries idle-runtime cleanup every minute, with a limit on each pass and without interfering
with an active turn. It also removes the runtime and temporary credential files for a turn
awaiting validation, while leaving the candidate unchanged. Only an explicit session discard
removes the workspace's volumes and network.

A turn that has completed successfully stays successful if cleanup fails. The background cleaner
retries and logs the cleanup error, with a limit on its length. If the turn itself failed, its
error includes the cleanup failure too.

On restart:

- Queued turns that were never sent to the provider remain in the queue.
- A turn waiting for your validation keeps its unpublished candidate. You can still accept or
  reject it by digest. Startup and the periodic cleaner remove its provider process, box,
  temporary credential files and services without deciding or changing the candidate.
- If co:op had recorded its intent to send a turn before the interruption, that turn is marked
  interrupted. It is not sent again automatically.
- The provider's saved conversation history remains.

Closing a session keeps its fork, conversation state, events and reviews. To delete the
workspace, use the separate two-step discard process: make a plan, then apply it only if the
session still matches that plan. Only a closed, idle session can be discarded.

The API can publish an explicitly authorized, reviewed candidate as a draft pull request. It
can't merge, sign, change the local parent ref, run an arbitrary host command or return a host
workspace path.

### Structured output

Use the optional `output_contract` to require an answer that follows a JSON schema. It contains
the exact schema bytes and their digest:

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
- co:op checks that the schema matches the digest before compiling it to validate responses.

co:op saves the contract, gives it to the model and checks the final answer against it. If the
answer is not valid JSON or does not match the schema, co:op asks the model to fix it in the same
provider session. Without semantic validation, three invalid responses fail the turn with
`output_contract_failed`. No assistant message is published.

Set `require_semantic_validation: true` when your controller must also check the answer against
external data fixed for this request. After the answer passes schema validation, co:op returns
its exact unpublished bytes and SHA-256 digest as `turn.candidate`, and sets the turn to
`awaiting_validation`. The saved `validation_candidate_sha256` remains visible after your
decision, so you can check what happened if the HTTP response is lost. Send one decision with an
idempotency key to:

```text
POST /v1/sessions/<session>/turns/<turn>/validation
{"candidate_sha256":"<digest>","verdict":"accept"}
```

To reject the candidate instead, send `verdict: "reject"` with 1 to 20 `violations`. Each
violation is capped at 4 KiB, and so is the combined list.

An acceptance must not include violations. co:op copies the stored candidate into
`assistant_message` and returns a saved `validation_receipt`. A rejection asks the model to try
again in the same provider session, as part of the same turn.

Your controller can review up to three candidates that pass the schema check. For each candidate,
the model has up to three responses to produce valid output. Responses that fail the schema check
do not use up one of your controller's three review attempts.

## Endpoints

### Health

| Method | Path | Result |
| --- | --- | --- |
| `GET` | `/healthz` | `{"healthy":true}` |
| `GET` | `/readyz` | `{"ready":true}` after daemon startup |
| `GET` | `/v1/capabilities` | `{"job_spec_versions":[2],"controller_tools_versions":[1],"repository_freshness_receipt_versions":[2],"session_evidence_versions":[1],"review_commit_message_versions":[1]}`. Check these versions before choosing which protocol features to use. |
| `GET` | `/v1/capacity` | Current shared active and warm runtime slots. The connector forwards this measurement in the hello it sends with every poll. |
| `GET` | `/v1/storage` | Disk use measured by this daemon: `storage` (the summary for a fleet controller), `budget`, `totals`, `roots`, `forks` and `problems` |

The worker has four slots for model processes, shared by active turns and idle warm sessions.
The session, turn and workspace counts all describe those same slots; do not add them together.
Time spent waiting in the queue does not count toward a turn's model timeout. If cleanup fails,
the process keeps its slot until cleanup succeeds. At startup, the worker reports busy with zero
free slots until it has checked which runtimes it owns.

For a quarantined or retired session, cleanup frees a slot only after co:op confirms that no
runtime remains with that session's exact labels. This does not make the workspace safe to run
again. If co:op cannot read capacity, it reports busy. The worker still polls for requests that
read data or clean up existing work.

`/v1/storage` measures the disk blocks allocated to files, rather than the file sizes reported
by applications. It reads file metadata without opening file contents, so it can count private
session data that contains credentials without reading those credentials.

Forks can share Git objects with their source checkout through hard links. Storage totals count
the space that deleting the content would actually free. Shared repository data is counted once
in `totals.baseline_shared_bytes`, rather than once for every fork.

An incomplete measurement reports `totals.unknown` and sets `storage.unattributed_bytes`
to `null`. This means unknown, never zero. To avoid scanning the full tree for every request,
co:op measures it at most once per `budget.measure_seconds`.
`storage.measured_at` lets callers check how old the measurement is.

`forks[]` lists each directory under a fork root, its category and the evidence used to classify
it. Only `disposable` and `staged_discard` can be removed through the authorized cleanup process.
For `active`, `grace`, `protected`, `control` and `unattributed`, the response explains why
removal is refused.

A directory is `protected` when it belongs to a running worker or registered sandbox activity,
a merge was interrupted, the task system still needs it, a session is quarantined,
or work is uncommitted. Without a co:op record identifying that fork instance, the directory is
`unattributed`. co:op reports it for inspection instead of guessing that it can be deleted.

When disk space is too low, the daemon refuses new workspaces with `storage_unavailable` and
explains the cause. Reads, cleanup and discard still work. co:op does not delete protected data
to make room.

Checkpoint capture and restore need space for extra copies, including during recovery. They have
a separate storage check that waits for enough space above the reserve. While a restore is
interrupted, the session remains blocked from running work.

The worker stops creating workspaces when disk use reaches `storage.high_watermark_bytes`.
It starts accepting them again only when use drops below `storage.low_watermark_bytes`. The gap
prevents repeated stops and starts as cleanup frees space. New work must leave
`storage.reserve_bytes` free. These checks do not limit how much data an already-running task
writes in its workspace.

Before reporting support for `job-setup:2`, `repository-freshness:2`, `session-evidence:1`,
`controller-tools:1` or `review-commit-message:1`, the connector checks the running daemon's
`GET /v1/capabilities` response for the matching version. If it cannot verify a capability,
it leaves it out of the next hello. The controller can distinguish an unsupported evidence
export from a supported report with no observed activity.

Worker protocol v2 reports supported features, runtime capacity and optional disk measurements.
It does not distribute lists of policies or repositories. Each controller job specifies its own
exact code and execution settings, which cannot change after submission.

### Sessions

Your controller creates `create-session.json` with these fields:

- `task`: your external task reference; co:op does not interpret its value.
- `job`: a version-2 JobSpec.
- `expected_job_digest`: the SHA-256 digest of the job's canonical representation.
- `controller_tools` (optional): connects the session to your controller's authenticated tool endpoint.

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

The job selects model targets, network rules, queue and turn limits, and the complete work and
review setup. Its `mode` is always `"normal"`; set `repository_read_only: true` for a job that must
not change its repository. A job that asks for the removed `readonly` or `bare` mode is refused with
`invalid_request`. Besides the existing required fields, version 2 requires:

```json
{
  "environment": {"CI": "1"},
  "check": {"argv": ["make", "test"], "environment": {"DATABASE_URL": "postgres://test-db/app"}},
  "resources": {"cpu_millis": 2000, "memory_bytes": 4294967296, "pids": 512}
}
```

These fields must be present even when the environment maps and check argv are empty.

The job's environment variables are passed only to sandbox processes. Review uses those variables
plus `check.environment`; the check's values override duplicate names. Names must be valid shell
identifiers. `COOP_*`, provider credential variables, model variables and adapter-owned
variables are reserved.

Values must be UTF-8, with no line breaks or leading or trailing whitespace. This prevents
processing an environment file from changing a value that co:op accepted. Use these variables for
job settings, not for model or GitHub credentials.

The check is a list of command arguments, passed directly to the process. It is not interpreted as
a shell command; invoke a shell explicitly if you need shell syntax. Leaving both `check.argv`
and the check environment empty means no check will run, so the review cannot be published.

| Field | Meaning | Accepted range |
| --- | --- | --- |
| `cpu_millis` | CPU, in thousandths of one core | 10 to 128000 |
| `memory_bytes` | Memory, in bytes | 6 MiB to 1 TiB |
| `pids` | The per-container process cap | 1 to 65536 |

Every job must set positive, finite CPU, memory and process limits. If a requested limit exceeds
the worker's configured maximum, or the runtime cannot enforce it, co:op refuses the job. It does
not silently lower the requested value.

co:op saves the normalized job before creating a workspace and refuses retries that change it.
Task text and repository settings cannot change what the worker is allowed to run. Your controller
may use repository defaults, but it must choose and include every setting before submission.
Work and review containers use that same saved configuration, with review-only environment
variables added for the check. There is no local policy name, policy catalog, separate source
selector or worker JSON file.

Send version-2 jobs only to workers that report `job-setup:2`. A new connector can still be
connected to an old daemon, so the connector checks the running daemon before reporting support.
Test your JobSpec encoder against the shared digest examples in `internal/workerproto/job_test.go`.
Remote reviews use the submitted job's check, not the worker's `COOP_GATE` or the repository's
`gate:`.

co:op does not fill in missing execution settings for v1 jobs. Historical sessions and
evidence stay readable, but their turns, reviews and interrupted creates can't run until
they are resubmitted as v2 jobs.

#### Frozen repository source

Before creating the job, your controller resolves the requested default branch, named branch, PR
or commit to exact commits. `job.source` identifies the GitHub repository by its stable ID and
slug. It records the default, selected and base commits, the approved tree and the information
that binds the job to that source. It also lists every authorized submodule, including nested
ones, with the exact repository, commit and tree. Do not include a host checkout path, remote URL
or GitHub token in this document.

The worker gets short-lived credentials for the authorized repository through its authenticated
connection to the controller. It downloads Git and LFS data directly from GitHub and verifies the
complete working tree. Repository files such as `.gitmodules` and `.lfsconfig` cannot redirect
those authenticated requests. The job fails if history or LFS files are missing, submodule commit
references do not match, or disk space is insufficient. A job with `source: null` uses an
isolated empty workspace.

#### Session endpoints

The public session response identifies the fixed repository source, `job_ref`, `job_digest`,
model target, execution mode, project exposure settings, saved network settings and repository
freshness. It also includes companion aliases, base commits, the generated fork name, revision,
state, activity, queue and budget counts, the event cursor and timestamps. It excludes host paths,
provider state, prompts and credentials. You can inspect historical sessions whose jobs can no
longer be verified, but you cannot resume model work in them.

With `Prefer: respond-async`, creation returns HTTP 202 and an operation to poll. Once it
succeeds with `resource_type: session`, fetch the session it identifies. Without the preference
header, creation waits and returns the operation and session together.

| Method | Path | Body/query |
| --- | --- | --- |
| `POST` | `/v1/sessions` | `task`, `job`, `expected_job_digest`; optional `controller_tools` |
| `GET` | `/v1/sessions?limit=100` | `limit` is `1..1000` |
| `GET` | `/v1/sessions/{session_id}` | none |
| `POST` | `/v1/sessions/{session_id}/prepare` | `expected_revision`; the job must turn on warm execution |
| `POST` | `/v1/sessions/{session_id}/workspace` | `expected_revision`, `task`; the session must be open, writable and unused |

You can still read a session created in the removed `readonly` or `bare` mode, but its job no
longer passes validation. A new turn returns `invalid_session_state`. Close and discard the old
session instead of trying to run it again.

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

The response includes the `operation` and `session`. The session's `workspace_task` contains
`queue_id`, `task_id`, `id`, `offer_ref` and `draft_sha256`. co:op generates and saves random
values for `queue_id` and `task_id`; `id` is the task's folder name. `draft_sha256` is the
SHA-256 digest of the task as co:op encoded it. Binding the task increases the session revision by
one and records a `workspace.task_bound` event.

The session must be open, writable and parked, with no active, queued or previously used turns.
Otherwise, the request fails with `invalid_session_state`. Changing a task that the workspace
already holds under the same `offer_ref` also returns that error.

A stale `expected_revision` returns `revision_conflict`. An invalid task returns
`invalid_request`, as does submitting a different task to a session that is already bound.
Repeating the exact binding leaves the session unchanged.

### Workspace checkpoints

| Method | Path | Input/output |
| --- | --- | --- |
| `POST` | `/v1/sessions/{id}/checkpoint` | JSON: `session_ref`, `expected_revision`, `placement_generation`, `repository_ref`; returns operation and descriptor |
| `GET` | `/v1/operations/{id}/checkpoint-bundle` | Streamed tar, exact `Content-Length`, SHA-256 `ETag` |
| `POST` | `/v1/sessions/{id}/workspace/restore` | Binary body plus `X-Coop-Expected-Revision` and base64-JSON `X-Coop-Workspace-Checkpoint` headers |

To capture a checkpoint, the session must be idle, writable and bound to a task. To restore one,
create an unused replacement session with the same authorized base and task offer. Both requests
require an idempotency key. Restore uses the content type
`application/vnd.coop.workspace-checkpoint.v2+tar`, and its Content-Length must match the
descriptor.

A v2 checkpoint streams Git objects with their types and verified LFS files. It includes new
commits, the final tracked tree, untracked files and task state. Restoring it recreates the exact
HEAD and working tree, but not the original split between staged and unstaged changes.

Changed submodule commits need a separate authorized process to retain and verify their data.
Checkpoints therefore refuse them, including intermediate submodule changes that were later
reverted.

Checkpoint processing limits memory use and metadata size, but does not impose a 64-MiB limit
on the data. co:op checks disk space and cancellation throughout the transfer and Git operations.
These checks can stop the operation when space is low; they cannot prevent other host processes
from filling the filesystem.

co:op checks every archive entry and the task's identity in a separate area before changing live
files. It saves the complete request body before marking the restore as running. If restore is
interrupted, the session cannot run work, even after a restart. Restore resumes from the saved
body. Retry the same operation; do not send a new key for a session that is already bound.

Capturing a checkpoint also reserves the runtime so a model turn cannot change files during
capture. Restoring a checkpoint does not restore permission to publish. Review the candidate
again before publishing it.

Discard removes checkpoint artifacts only after verifying that the local session owns them. It
also resolves any interrupted restores for that session. You can still read historical v1
artifacts, but cannot restore them: their patch-only format does not preserve enough information
to verify the complete Git and LFS data.

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
| `GET` | `/v1/sessions/{session_id}/turns?after=0&limit=100` | Continue after the numbered turn given by the cursor |
| `GET` | `/v1/sessions/{session_id}/turns/{turn_id}` | none |
| `GET` | `/v1/sessions/{session_id}/turns/{turn_id}/artifacts/{artifact_id}` | raw generated image |
| `POST` | `/v1/sessions/{session_id}/turns/{turn_id}/cancel` | `expected_revision` |
| `POST` | `/v1/sessions/{session_id}/turns/{turn_id}/validation` | `candidate_sha256`, `verdict`; `accept` forbids `violations`, `reject` requires 1 to 20 |

Validation decisions do not include `expected_revision`. Instead, co:op checks
`candidate_sha256` to ensure you are accepting or rejecting the exact unpublished answer you
reviewed.

Before accepting, rejecting or cancelling a turn awaiting validation, co:op stops and removes
the provider runtime and temporary credential files that produced its candidate. If it cannot
confirm cleanup, the API returns `503 session_cleanup_error`. The candidate remains
`awaiting_validation`, and your decision has not taken effect.

That failed operation is finished: retrying its idempotency key returns the same failure. After
the background cleaner or an operator fixes cleanup, fetch the current candidate and submit your
decision with a new idempotency key.

The public turn response excludes the prompt, idempotency data, `min_target_index` and
`rewind_target`. You can see the effect of those last two settings in the session's `target`
and `session.target_rotated` event. For a completed turn, `assistant_message` is the response
to show the user. co:op does not publish hidden reasoning, raw tool calls, raw ACP frames or box
logs in that response.

A completed turn includes `usage` when the adapter reports it. The fields are `input_tokens`,
`cached_input_tokens`, `output_tokens`, `reasoning_tokens` and, when available, the provider's
`cost_usd`. Check `cost_recorded` to distinguish a reported zero cost from a missing cost.

ACP reports the cumulative cost of the provider session. co:op calculates this turn's share
before returning it, including after daemon restarts or adapter-process resets. Fresh and cached
input tokens are reported separately because providers charge different rates for them; combining
them before calculating cost would overestimate the bill. If the adapter reports no usage, co:op
leaves the object out. Missing measurements are never represented as zero usage or free work.

A completed turn may include `output_artifacts` for images the agent generated or saved in the
turn's `.coop-output/<turn_id>` directory. Each record has an opaque ID, a safe filename, the
media type, a SHA-256 digest and the exact byte count. The record does not embed the image.
Download it through the raw endpoint, which returns the saved file with matching `Content-Type`,
`Content-Length` and `ETag` headers. The saved file cannot change.

co:op captures image blocks and embedded image resources from ACP tool updates in output order,
naming them `generated-1.<ext>`, `generated-2.<ext>` and so on. Image bytes do not count toward
the text transcript limit.

co:op inspects text tool updates one frame at a time, then discards them. A long turn therefore
does not fail just because its tool updates add up to a large transcript. Separate limits still
apply to assistant text, each incoming frame, the turn's duration and saved output artifacts.

Only PNG, JPEG, WebP and GIF are accepted. A turn returns at most five images. Images from tool
updates and files in the turn's directory share that limit, and a repeated image is kept once. A
file in that directory without a `.png`, `.jpg`, `.jpeg`, `.webp` or `.gif` extension is ignored.
co:op rejects symlinks, special files, mismatched content, a sixth image, a file over 8 MiB, or
more than 8 MiB in total.

co:op removes the temporary output directory before completing the turn, so generated charts do
not become repository changes. In a read-only repository session, the container still sees the
path `.coop-output/...`, but its files are stored in co:op's private session state on the host.
The repository itself stays mounted read-only.

Cancellation first asks ACP to cancel the turn. co:op then stops the turn's process group and
removes its box, using the run's labels to identify it. This does not undo actions already taken
in external systems.

### Events

```bash
curl --unix-socket "$SOCKET" \
  'http://localhost/v1/sessions/remote_.../events?after=41&limit=100'
```

The events endpoint returns a JSON array ordered by `sequence`, which increases within each
session. Save the last sequence you processed. After a disconnect, request `after=<sequence>`
to continue from that point.

Events stored privately for the worker's owner include the identity, sequence, turn ID, type,
version, timestamp and payload. Tool evidence has size limits but may include filesystem paths,
arguments, results and diffs. Events describing tool starts and finishes, thoughts, progress,
plans and permission decisions also carry `checkout_root`. The connector uses that checkout
path to remove it before forwarding the event.

The connector forwards a filtered version of activity events to the controller. It includes tool
titles, inputs and results, plus the model's thoughts, progress and plan, with the session's host
checkout path removed. Each field has a size limit:

| Field | Limit |
| --- | --- |
| Titles | 1 KiB |
| Text | 8 KiB |
| Tool fields | 16 KiB. Larger fields are sent as a `preview` marked `truncated`. |
| One event | 64 KiB |

If a field appears to contain a secret, the connector withholds the entire field. The event's
`withheld` object identifies the field and reason, such as `likely GitHub token`. When tool
input is withheld, the event still identifies the MCP server and tool. The structured
`path_context` includes only project-relative paths and scope warnings, never the host checkout
root.

Each event payload is limited to 256 KiB. A page also has a total byte limit, in addition to its
`limit` on event count. If events are large, the endpoint returns fewer of them on the page
rather than cutting an event short.

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

co:op records a `network` event for blocked requests when a filtered run's network record is
finalized. It can also report alerts, such as a mismatch in network permissions or incomplete
evidence. A run with neither refusals nor alerts produces no event; this is not a heartbeat for
every turn. The payload has these limits:

- Destinations are grouped and capped, with an `omitted_destinations` count.
- Alerts are capped.
- `evidence_id` names the retained event that `coop net blocked` can open.

It follows the same disclosure scope as the network routes. A destination appears only when the
session job set `egress.export_destinations: true`.

`workspace.task_bound` records a [task binding](#task-binding). Its payload holds `offer_ref`,
`draft_sha256`, `queue_id`, `task_id` and `id`, the values in the session's `workspace_task`. A
checkpoint restore appends it too, with `base_commit` and `restored: true` added.

`output_contract.rejected` records a rejected candidate within a turn. When a provider response
fails the schema check, co:op records `attempt`, the contract's `sha256` and an `error` with
a size limit. When your controller sends a `reject` verdict, the event instead includes
`attempt`, `candidate_sha256`, `semantic: true` and your `violations`.

The connector forwards lifecycle events without their payloads, except for `network` events.

#### Activity events

Activity events describe what the model does during a turn. co:op records them before the event
that ends the turn, so a client that stops polling at `turn.completed` has already received
that turn's activity events:

```text
tool.started            # tool_call_id, title, kind, input, path_context
tool.completed          # tool_call_id, title, kind, status, input, output, content, locations, terminal_exit, path_context
model.plan              # entries
model.thought           # text
model.progress          # outward commentary text, not private reasoning
permission.decided      # tool_call_id, title, outcome, option_id, option_kind
activity.elided         # dropped, reason
provider.backoff        # attempt, target, next_target, retry_after_seconds, reset_at, all_limited_until
provider.alive          # frames, bytes, last_kind
```

When the provider reports it, `terminal_exit` contains the command's numeric `exit_code`, its
termination `signal` or both. These fields are separate from the output, so truncating an output
preview does not remove the exit result.

When a tool provides structured file information, `path_context` includes `basis: "lexical"`
and up to 16 `paths` entries. Each entry uses a JSON-pointer `source` to identify where the
information came from. Its `scope` is `project`, `outside` or `unknown`. Only project-relative
files receive a `path`.

The host checkout root is never exported. `partial: true` means some path information was left
out. Paths are classified by their spelling; this does not check where symlinks lead or decide
whether access is allowed. co:op does not guess missing paths from tool titles or shell commands.
A later tool update can add paths to the completion event without changing the start event.

co:op extracts paths before shortening large diffs into previews. URI-shaped paths are marked
unknown, rather than treated as checkout files. Before forwarding an event, the connector checks
the path fields again, drops entries it cannot verify and marks the context `partial`. It also
applies the size limits and secret scan described under [Events](#events) to titles and inputs.

`provider.backoff` reports a confirmed provider rate limit. co:op emits at most one such event
for each configured model target in a turn.

- `attempt` numbers the events from 1 within the turn.
- `target` identifies the model target that reached its rate limit.
- `retry_after_seconds` says how long that target must wait. co:op uses the provider's reset time
  when available, or its own limited backoff otherwise.
- If co:op switches to another target, the event includes `next_target`.
- If every target is limited, the last backoff includes `all_limited_until` instead.

Use these events to distinguish a turn waiting for a rate limit from one that has stopped
responding. Waiting can still be expected progress; silence alone is not a reason to cancel it.

`provider.alive` helps when the provider CLI retries internally. In that case, co:op's own
model-selection logic never sees the rate limit, so it cannot emit a backoff event. The CLI may
keep sending ACP frames without making tool calls or producing other activity events.

This event confirms that the worker is still receiving data. `frames` and `bytes` count all
ACP frames received for the current turn's prompt. Compare these cumulative counts, not the
timestamp, to distinguish new activity from a repeated event.

`last_kind` identifies the latest frame as its JSON-RPC method, a session-update type after a
colon, or `response`. It contains no frame content. Repeated events with the same kind tell you
what the provider is still sending.

co:op sends this event at most once per minute, and only when it has not already reported other
activity in that interval. Normal activity and long tool calls do not get duplicate reports.
If the provider sends no frames, there is no event. Your client still needs a timeout for turns
that go completely silent.

A turn fails with `acp_quiet` if the model sends nothing for 15 minutes after its tools have
answered. Only the model's text, thoughts, plans and tool calls count as output; liveness events
do not. This timeout does not apply before the first tool finishes or while a tool is running.
The error reports the silent duration and the provider's last frame kind. You may retry the turn;
this error reports a silent model, not a failed project task.

If the ACP process exits before returning an answer, the turn fails with `acp_process_error`.
The detail includes its exit status or signal and a summary of stderr: line and byte counts, a
digest for grouping similar failures, and fixed categories such as rate limit, authentication,
network or crash. Digits and hexadecimal runs are masked before calculating the digest. The
stderr text itself never leaves co:op.

### Budget

Extend a session's turn budget:

```bash
curl --unix-socket "$SOCKET" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: controller:extend:01J...' \
  -d '{"expected_revision":9,"additional_turns":20}' \
  http://localhost/v1/sessions/remote_.../budget
```

You can extend the turn budget only up to co:op's global `max_turns` limit. Your controller must
decide who may authorize an extension and how much to spend. co:op enforces the numeric limit and
prevents the same request from being applied twice.

### Changes

```bash
curl --unix-socket "$SOCKET" \
  http://localhost/v1/sessions/remote_.../changes
```

`GET /v1/sessions/{session_id}/changes` returns:

- the immutable `base_commit`, the current `fork_head` and `fork_tree`, and the current
  `parent_head`;
- for a repository-backed session with a source binding, the fixed `admitted_source_tree`.
  Compare it with the current tree to distinguish changed content from empty commits or changes
  that were committed and later reverted;
- paths classified as committed, staged, unstaged, untracked or conflicted;
- ahead and behind counts, and base-to-head divergence counts;
- a page of the binary patch, subject to a size limit;
- `patch_digest`, the exact `patch_bytes`, `patch_offset`, `patch_next_offset` and
  `patch_has_more`.

To fetch the next patch page, call
`GET /v1/sessions/{session_id}/changes?patch_offset=<next>&patch_limit=<bytes>`. `patch_limit`
cannot exceed the job's `max_patch_bytes`, and offsets cannot exceed 1 GiB. Check `patch_digest`
as you page through the result. If it changes, start again at offset zero so you do not combine
pages from different patches. The legacy `truncated` field is true whenever the response
contains less than the complete patch.

The JSON response encodes `patch`, `path_bytes` and `old_path_bytes` as base64. `patch_bytes`
is an integer giving the complete patch's size. A valid UTF-8 filename also appears in `path`;
filenames containing arbitrary bytes are preserved in `path_bytes`. If Git fails, the endpoint
returns an error, never an empty change list.

The changes endpoint can inspect uncommitted work. Review requires a clean, committed workspace.

### Network

```bash
curl --unix-socket "$SOCKET" http://localhost/v1/sessions/remote_.../network
curl --unix-socket "$SOCKET" http://localhost/v1/sessions/remote_.../network/connections
curl --unix-socket "$SOCKET" http://localhost/v1/sessions/remote_.../network/explanations/<event_id>
curl --unix-socket "$SOCKET" http://localhost/v1/sessions/remote_.../network/receipt
```

These four endpoints only read data. They do not probe a gateway or approve, grant or expand
network access. The session API cannot grant network permissions.

`GET /v1/sessions/{session_id}/network` returns:

- the session's frozen `mode` and `fingerprint`;
- the `requested` and `effective` rule texts;
- `current`: the newest run's retained observation summary, or `{"status":"no run yet"}`;
- an `alerts` list with a size limit.

`GET /v1/sessions/{session_id}/network/receipt` combines the records for all of the session's
runs into one versioned report. It tracks `finality` separately from `completeness`. The report
becomes `final` only after the session is closed and every run's record is final. A `final`
report can still be `partial` if some evidence is missing.

The worker saves these records in the owner's registry, so both endpoints keep working after
container cleanup. A session that never used filtered networking returns
`{"available":false,"reason":"..."}` rather than an empty record.

`GET /v1/sessions/{session_id}/network/connections` returns the collector's live connection
records for the newest run, subject to a row limit. The response includes `status` to describe
freshness, using the same values as the summary, plus `as_of` and `detail_truncated`. Clients
request samples when needed; the worker does not need to save a sample every second. Before the
first run, the endpoint returns `{"status":"no run yet","connections":[]}`.

`GET /v1/sessions/{session_id}/network/explanations/{event_id}` returns one saved refusal from
this session's runs. It includes the event, the reason, where the evidence came from and whether
co:op drafted a proposed rule. Each run retains a limited number of events. If the requested
event has aged out, the endpoint returns `{"available":false,"reason": "event_not_retained"}`.
That response does not mean the event never existed.

`projection` states what the response discloses. It is `destinations-withheld` unless the
session job set `egress.export_destinations: true`. With that setting, it is
`destinations-included`, and the response includes rule texts and observed names.
The daemon controls this disclosure. The connector makes ordinary GET requests to the endpoints
above and forwards each answer unchanged. It cannot choose different destinations, rules or
disclosure settings.

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

This endpoint returns a versioned report for a controller's session-inspection page, with limits
on how much data it includes. It covers:

- the network settings approved for the session;
- activity observed during its newest run;
- the combined network record for all its runs;
- the approved task assigned to its workspace, read from the current task folder.

The endpoint reads saved evidence without probing a gateway or starting a runtime. It cannot
grant additional access and accepts no query parameters, so callers cannot change what it
discloses. The daemon checks the report against its format and disclosure rules before sending
it. If the report fails that check, the endpoint returns `500 internal_error` instead. The
connector forwards it unchanged through an `api_request` command.

Each section reports whether its data was available. This lets you distinguish a failed read
from a successful read that found no activity:

| Section | Status words |
| --- | --- |
| `network.access` | `captured`, `not_filtered`, `unavailable` |
| `network.observation` | `observed`, `no_run`, `not_filtered`, `unavailable` |
| `network.receipt` | `available`, `not_filtered`, `unavailable` |
| `task` | `bound`, `unbound`, `unavailable` |
| `task.snapshot.state_note` | `captured`, `absent`, `withheld` |

An `unavailable` section includes a `reason`. The network settings (`mode`, `fingerprint`)
come from the saved session record and cannot change. If the host cannot read the network
registry, those settings remain available while the affected sections report that they could not
be read. Before its first run, a filtered session reports `no_run` and a `provisional` receipt
with `run_count` `0`, rather than an empty activity counter.

Counters are unsigned decimal strings so JavaScript clients can preserve values above 2^53
without rounding them. A `null` counter means the metric was not measured, never that it was
`0`.

Each response list has a size limit. The report counts omitted entries in `omitted_denials`,
`omitted_connections`, `omitted_alerts` and `omitted_run_references`. If a run blocked a
thousand names, the response stays within its limit and tells you how many entries it left out.

The report follows the network endpoints' `projection` setting. Destination names appear only
with `destinations-included`. When a name is withheld, the refusal includes
`destination_withheld`, so callers can distinguish a hidden name from a missing one.

`task` includes the fixed task identity saved for the session and a snapshot of its folder at
the time the report is captured:

- its state;
- the same `state_sha256` used for the task data in a workspace checkpoint;
- the checklist labels corresponding to the checkpoint's completion flags;
- a list of files and their digests;
- the agent-written `state.md`.

The note has a size limit and is withheld in full if the scan flags a likely secret. The worker
keeps the current snapshot, not a history of task transitions. If your controller needs that
history, save successive snapshots. If a bound task's folder is missing, the report returns
`unavailable` while preserving the task's identity. It does not return an empty task.

| Method | Path | Body/query |
| --- | --- | --- |
| `GET` | `/v1/sessions/{session_id}/evidence` | none |

### Review

```bash
curl --unix-socket "$SOCKET" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: controller:review:01J...' \
  -d '{"expected_revision":12,"commit_message":"Add per-worker memory metrics"}' \
  http://localhost/v1/sessions/remote_.../review
```

Use the optional `commit_message` to set the reviewed commit's message, up to 4,096 bytes. If
you omit it, co:op uses `Reviewed changes`. It also adds the `Coop-Review: <operation id>`
trailer. The message is saved in the reviewed commit and stays the same when that commit is
published.

To review a session, it must be parked and either open or exhausted, with no queued turns and a
clean, committed fork. co:op records the exact source and parent commits and trees, then prepares
a temporary candidate based on the current parent `HEAD`. It runs the saved job's check against
that candidate and returns:

- the creation base, source, parent and candidate commit and tree identities;
- the saved PR number, ref and head, when the session came from an existing PR;
- the rebase and check results, including the startup error text in `gate_error` if startup failed;
- `gate_output`: the check command, its `exit_code` if it returned one, the number of saved
  output bytes and whether all output was kept. `complete` confirms it was; `incomplete`
  explains what is missing; `lost` means no output was kept;
- policy findings, subject to a size limit;
- a size-limited inline preview of the patch from the parent tree to the candidate tree;
- `patch_truncated`, `candidate_retained`, `publishable` and stable not-publishable reason codes.

The saved job's `check.argv` is the only remote review command. The worker's `COOP_GATE`, the
parent's `gate:` and candidate changes can't replace it. The first two still apply to ordinary
local fork merges.

The check runs in an isolated container using the session's saved network mode and, for filtered
runs, the exact saved network permissions. It ignores the repository's `box:` settings and the
worker's runtime arguments, environment file and MCP configuration. It uses the saved work
environment plus check-only overrides, with the same CPU, memory and process limits as model
turns. It also uses the same worker-configured image, or locked filtered client image, as the
turns. It never builds or uses an image from the job repository's Dockerfile.

The check can create ignored build files in its temporary checkout. If it changes the reviewed
source, the result cannot be published. Publication is also blocked when no check is configured,
the check fails or cannot start.

The JSON response encodes the patch preview as base64. A shortened preview does not prevent
publication. co:op saves the exact candidate commit and verified LFS objects in private host
storage, separate from the model's workspace, so publication does not depend on transferring the
full patch or fitting it under a patch-size limit.

For the same saved inputs, co:op creates the same single candidate commit on the reviewed parent,
before running the check. `source_head` records the model's original commit before that candidate
was prepared.

co:op scans the changes between the parent and candidate for secrets, including large files,
binary Git objects and LFS files. If LFS attributes changed, it also scans the resulting LFS
files. If any content cannot be read, review fails; unreadable content is not assumed safe.

`publishable` tells you whether this exact candidate passed the requirements for publication.
It does not grant permission to push or merge. It is false when there is:

- a conflict;
- no configured check, or a check that failed;
- a startup failure;
- findings from policy checks;
- a changed parent or source;
- a remaining fork-worker record, even if its process has exited;
- no changes to publish.

`candidate_retained` tells you whether co:op saved the exact candidate, independently of whether
the check passed. A candidate can be saved even if the check is missing or fails. Saving it does
not authorize publication.

To publish, use the saved `candidate_head` unchanged, obtain a separate controller grant and
update the destination ref only if it still matches the expected value. Never reconstruct the
result from a patch or the model's later workspace. Discarding a session deletes its saved
candidates, so keep sessions whose reviewed changes still need to be published.

A review can continue after the requesting connection ends. Look up its original operation key.
Once it succeeds, fetch `GET /v1/sessions/{session_id}/reviews/{operation_id}` to get the saved
`operation` and `review` response. This does not start or resume the check. The endpoint refuses
requests for another session, an operation that is not a review, or an unfinished review.
`policy_findings` and `not_publishable_reasons` are always arrays, even when empty.

#### Gate output

The review keeps the gate's stdout, stderr and co:op startup messages as printed,
whether the gate passes or fails. Your controller can read failures directly without
asking someone to relay the output. Read it page by page with
`GET /v1/sessions/{session_id}/reviews/{operation_id}/gate-output?cursor={cursor}`,
starting without a cursor:

```json
{"output":"1 test, 1 failure\n…","next_cursor":"1048576","bytes":1843200,"complete":true}
```

Each page contains at most 1 MiB of output and ends at a character boundary. Invalid UTF-8 bytes
appear as U+FFFD replacement characters. `next_cursor` is `null` on the last page. `bytes`
reports the total amount of saved output. When `complete: false`, `incomplete` explains what
is missing. co:op keeps at most the first 64 MiB, so a check that prints continuously cannot fill
the disk with its output.

If no output was saved, the response is `{"lost":"<why>"}`. This can happen because no check
ran, writing the output failed, or the session was discarded. Output can be read only through the
session that owns the review. Discarding that session removes the output.

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

Leave `expected_head` empty to require a new branch. To update a previously authorized PR,
provide its number and the exact commit you expect it to point to. The operation returns `202`
while running. Use the operation key to check its state, then fetch
`GET /v1/sessions/{session_id}/publications/{operation_id}` for the completed `operation` and
`publication` response. Retry with the same key and identical request.

The result is one of:

- `published`, with the exact commit, tree and PR result;
- `conflict`, with the verified existing PR and its observed remote head;
- `refused`, with a stable error code.

The worker requests fresh credentials from your authenticated controller at
`POST /v1/coop-workers/jobs/{job_ref}/publication-grants`. The request identifies the session,
job digest, review operation, original command key, repository and exact publication request.
Your controller returns that same repository identity, a `token`, its `expires_at` time and
the GitHub App's `actor_id`.

Your controller decides whether publication is approved; co:op does not require a product-specific
approval format. A `403` permanently refuses this operation. Temporary failures can be retried.

The worker sends Git and LFS data directly from the saved candidate to GitHub. It creates a
draft PR or updates the exact existing PR owned by the App. If a person has already marked that
PR ready for review, co:op preserves that choice.

If a push or PR response is lost, co:op checks the remote result before retrying. A candidate
cannot be discarded while publication is active. Later edits in the model workspace cannot change
what gets published.

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

The discard plan records the session revision, the specific fork instance and its reservation,
and the workspace's inode, branch, head, status digest and running, dirty and unmerged states.
The inode helps detect a replaced directory, but does not prove ownership on its own. Before
deleting anything, co:op holds a reference to the live directory and checks that all recorded
state still matches.

By default, co:op refuses to discard uncommitted or unmerged work. If your controller has separate
permission to destroy that work, set `accept_dirty`, `accept_unmerged` or both when creating
the plan.

Execute that exact plan:

```bash
curl --unix-socket "$SOCKET" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: controller:discard:01J...' \
  -d '{"plan_operation_id":"op_..."}' \
  http://localhost/v1/sessions/remote_.../discard
```

Discard first checks that the plan belongs to the session named in the request path. It then
compares the current workspace with every recorded detail in the plan. If the plan is stale, the
workspace was replaced or work is running, nothing is deleted. Otherwise, co:op deletes the fork
workspace and private ACP state, and saves a record that the session was discarded.

At startup, the daemon blocks sessions whose workspace ownership it cannot verify. This includes
old session records without a store-owner binding, v1 reservations, and sessions whose fork record
or workspace is missing. These sessions are quarantined: you cannot create or apply a discard
plan for them because co:op cannot prove it owns the workspace. You can retire the session record
instead:

```bash
curl --unix-socket "$SOCKET" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: controller:retire:01J...' \
  -d '{"retire_quarantined":true,"expected_revision":16}' \
  http://localhost/v1/sessions/remote_.../discard
```

This marks the session record as retired without deleting its workspace:

- Queued turns are marked exhausted.
- A turn that was active when the daemon lost ownership remains in history without changes.
- The workspace, fork reservation, sidecar services and provider's ACP history are retained.

A session that is not quarantined is refused with `invalid_session_state`.

Retiring a record does not show that its runtime has stopped. Separately, the daemon looks for
boxes with the exact `coop.run` labels derived from the store's session and turn IDs. It removes
matching boxes and temporary credential files in its private ACP state. If a check fails or
finishes only partly, the daemon retries and reports busy until cleanup is fully verified. These
checks continue after a restart.

Once runtime cleanup is verified, the worker can report free capacity again. It does not take
ownership of or delete the unowned workspace. Quarantined sessions still cannot run and need
explicit retirement or recovery. If capacity remains busy, inspect the worker's runtime-cleanup
warning; a retired session record is not proof that its runtime stopped.

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

The error includes `operation_id` when co:op accepted an operation before it failed. The public
API leaves out private internal details.

The daemon also writes a JSON diagnostic to stderr, with a size limit. It includes the operation
ID, method, resource identity, error code and a detail with sensitive information removed.
Repository and state-directory paths are replaced, and lines containing secrets are removed.
Use the operation ID to connect the client error, saved operation and supervisor log without
opening SQLite directly.

Common status mapping:

| HTTP | Meaning |
| --- | --- |
| `400` | Invalid request, or a field exceeds its allowed limit |
| `404` | session, turn, or operation not found |
| `409` | idempotency, operation fence, revision, state, queue, budget, resume, uncertainty, or discard conflict |
| `413` | ordinary request body exceeds 128 KiB, or turn submission exceeds 12 MiB |
| `500` | internal failure; host paths and raw internal errors are suppressed |
| `503` | The worker is not ready, or it temporarily cannot verify or use the required repository, runtime, network or storage access |

`storage_unavailable` is a retryable `503`. The worker has reached its configured disk-space
limits and will not create another workspace until space is freed. If `storage.refusal_reason`
is `protected_storage_exceeds_budget`, the protected data itself must be reduced before new
workspaces can be accepted. Read `/v1/storage` to see what is using space and why it is retained.
Place the session on another worker or free storage through the appropriate cleanup process.
Do not retry in a tight loop or delete protected workspaces just to meet the limit.

`network_unavailable` is a `503`. The worker cannot enforce, or verify that it can enforce,
the session's saved network permissions. Check its container runtime and network setup. The
worker stops execution instead of falling back to open networking. Do not change a saved job to
bypass the refusal.

For `operation_uncertain` or `turn.interrupted`, inspect the saved operation and resulting
session state to find out what happened. An unknown result is not a reason to submit the same
action with a new key: that could run it twice.

`RunReview` is a limited exception because it only reviews a fixed candidate. Once co:op has
saved the exact source, parent and job, repeating the request with the same key resumes that
review under its original operation ID. It does not reread a moving branch to choose a new
candidate. If the saved instructions cannot be read or verified, the operation remains
`operation_uncertain`.

## Operational recovery

| Situation | Action |
| --- | --- |
| Client response lost | Replay the exact mutation with the same idempotency key |
| Permission is withdrawn while a prepared create or submit may be arriving | Fence the exact operation; if it was already accepted, close or cancel the resulting work |
| Socket reconnect | Resume event polling after the last sequence your controller saved as processed |
| Daemon already owns state | Stop or inspect that daemon; don't remove the lock |
| Stale socket after crash | Start the daemon with the same state directory; it acquires the state lock before removing the stale socket |
| Queued turn at restart | co:op resumes queued turns in the order received |
| Running session create at restart | co:op resumes creation automatically from the saved request |
| Reserved operation at restart | co:op records that the operation was interrupted before acceptance; it had not attempted any external action |
| Running checkpoint restore at restart | Resume from the saved request body while model work remains blocked; never submit a new key |
| Other running operation at restart | co:op marks it `operation_uncertain`; inspect what happened before deciding what to do. Do not resubmit it under a new key. |
| Operation remains running after startup | The background watchdog resumes creates that are safe to retry and marks stale operations uncertain when their outcome cannot be established |
| Turn interrupted after co:op recorded its intent to send it | Report that the turn was interrupted; do not submit the same user input automatically |
| Active turn must stop | Cancel it with an idempotency key, then read the turn to confirm how it ended |
| Session should stop using runtime resources | Wait until it is parked, then close it if it is warm. co:op removes containers after each non-warm turn, or when a warm session times out or closes. If cleanup fails, it retries in the background. A parked state alone does not prove cleanup finished. |
| Incident is over | Close the session and keep its fork for review, or discard it explicitly later |
| New co:op build for the worker | Stop the worker and run `coop sessions check` with the new build before you swap it in; see [Upgrading a worker](#upgrading-a-worker) |
| Review patch preview is truncated | This response has no further patch pages. Do not treat a shortened preview as a complete diff. Publication uses the saved reviewed candidate, never a patch rebuilt from the preview. |

Stop the daemon before backing up the entire state directory. If you restore a database without
its matching forks, an operator must resolve the mismatch. co:op will not assign ownership based
only on directory names.

## Consumer responsibilities

When building a production controller, you are responsible for:

- deciding who can connect and which actions they may authorize;
- reliably saving and delivering incoming and outgoing messages without processing duplicates;
- assigning stable, globally unique idempotency keys;
- saving event cursors and checking operations whose outcomes are uncertain;
- preparing prompts and removing sensitive output before sending it through your chosen transport;
- setting retention, audit, capacity and spending policies;
- approving GitHub publication and supplying short-lived credentials for the authorized repository;
- scanning for secrets before data leaves the host;
- supervising the worker and providing a way to stop it.

Never put chat-service tokens, GitHub credentials, Emisar administrative credentials or other
credentials that authorize publication into a co:op prompt or box.
