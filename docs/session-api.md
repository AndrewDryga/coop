# co:op worker API

A trusted controller supplies the code, context and execution settings, and co:op runs models in
isolation and keeps their workspaces. The worker can share a VM with its controller or run on a
separate VM. Ryker uses this protocol, but it can serve other products and has no Ryker-specific
terms.

## Connect

```bash
coop sessions connect --controller https://controller.example --token-file /run/secrets/coop-token
```

This is the only command that starts a worker. It starts a private local service, the daemon, and a
connector that sends requests to your controller, so you don't need to open an inbound TCP port.
The worker has no JSON file or local policy YAML. Add `--state <path>` to choose where the worker
stores its private data, and `--ca-file <path>` to trust a private TLS CA. Public HTTPS uses the
system trust store.

The single-use token identifies the worker to the controller. co:op saves a renewable identity in
the state directory, then removes the token file. To reconnect, use the same controller and state
directory without `--token-file`. The saved identity works only with the original controller.

The default state directory is `~/.local/state/coop/sessions`, whatever `XDG_STATE_HOME` says, so
every co:op process on the machine finds the same worker. The daemon listens on the internal Unix
socket at `<state>/control.sock`. Each connector starts its own daemon and refuses to reuse a
running one, so stop the existing connection before reconnecting. To run several workers at once,
give each a separate `--state` directory. Use a process supervisor to keep the worker running.
Ctrl-C stops both the connector and the daemon it started.

A worker needs Git, Git LFS and a supported container runtime. The bundled worker image includes
Git LFS. On a native worker, install `git-lfs` with your package manager.

Jobs use the worker's base image (`COOP_BASE_IMAGE`) unless the controller grants a project
runtime. That grant can select a project Dockerfile and an isolated Compose test stack. The worker
builds the toolchain from its private frozen source and gives Work and review the same image.
Filtered jobs use co:op's locked client image and refuse custom controller toolchains. An open-network
job without a project runtime runs in an image with the tools the repository lists in
`.tool-versions`, built once on the worker and reused.

Before the worker accepts filtered jobs, run
[`coop net setup`](networking.md#fleet-workers-and-docker-in-docker) as the worker's OS user, using
the same Docker endpoint and network state directory. This prepares and checks the networking setup.
The session API requires those checks to stay valid but doesn't build the networking images itself.
Installing Docker or accepting JobSpec version 2 isn't enough to show that the worker can enforce
filtered networking.

To check the local session service:

```bash
coop sessions doctor
coop sessions doctor --socket /var/lib/coop-sessions/control.sock --json
```

The local socket and state are private to the worker's OS account. Never mount the socket into a
model's box or expose it through an unauthenticated proxy. Connecting out to a controller doesn't
by itself prove that the controller can be trusted, so co:op verifies the TLS connection before it
sends an enrollment token or accepts work.

## Session authority

Your controller supplies one job, which sets the session's source, model targets, repository access,
network access and resource limits. co:op checks and saves it before creating a workspace. Retries
must use that same configuration, and restarts use the saved copy. Repository instructions and
model output can't grant more access. co:op doesn't resume model work for a session whose
authorization it can't verify.

co:op saves a stable ID for each session store in `session.sqlite`. When it reserves a fork, it
records which store and session own that specific instance of the fork, and moving the database
with `--state` preserves the store ID and ownership. Only the session service that owns the fork can
discard it, after checking its journal, the session's link to the fork and the workspace safety
conditions. `coop fork rm --force` can't bypass these checks.

Old fork reservations without a store ID remain visible, but ordinary fork removal can't delete
them, and a matching session ID in another store isn't enough to prove who owns them. You can read
these sessions, but recovering them requires an explicit procedure with the service stopped. co:op
doesn't assign them to a new owner automatically or delete them because they're old.

The worker fetches repository data directly from GitHub using short-lived credentials that your
controller grants for that repository. The model receives the complete working tree, with verified
LFS files and every explicitly authorized submodule, including nested ones. Tokens never enter the
model's mounts, repository configuration or exported results. If objects are missing, a submodule
is unauthorized or an LFS pointer format is unsupported, creation fails instead of leaving an
incomplete tree.

### Workspace storage

Each hello message can carry a `storage` object that reports disk space on the volume holding the
worker's fork workspaces. The connector reads it from the daemon's
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
- A `null` `unattributed_bytes` means the worker couldn't identify what uses some of the space,
  or couldn't finish measuring. Treat it as unknown, never as zero.
- `allocation` is `refused` when the worker won't accept a new workspace, and `refusal_reason` says
  whether the cause is `reserve_exhausted` or `protected_storage_exceeds_budget`. The worker can
  still handle existing workspaces and clean them up.

The high and low watermarks limit used disk space, and the reserve is the space that must stay
free. By default, co:op calculates all three from the volume's measured capacity:

- It reserves 5% of the volume.
- It stops accepting new workspaces when free space falls to that reserve or below.
- It accepts new workspaces again only when twice that reserve is free.

If the worker can't measure its volume, it leaves out the storage object and keeps polling.

A workspace with no recorded session binding or no uncommitted changes can look unused, but another
session store or an unfinished create operation may own it. co:op marks such a workspace as
protected in `/v1/storage` and leaves it for manual inspection.

Automatic cleanup only finishes removals that an authorized discard has already started. Before
deleting a workspace, co:op moves it into a private staging directory belonging to its owner, so
cleanup can resume an interrupted removal. Discard reports success only after the data has been
removed.

### Restart and recovery

Stop the connector with `SIGINT` or `SIGTERM`. Across restarts, keep the identity file, the entire
journal directory and the same worker and workspace configuration. A restart loads the existing
identity and doesn't enroll again. Certificate renewal is automatic over mutual TLS.

At startup, co:op locks the state directory and removes unpublished source staging data, temporary
transfer files and host-only Git credential files. It keeps published sources and response bodies
saved in the journal.

Configure the service manager to stop the worker's whole process group or cgroup. Killing only
the parent can leave a Git or LFS child running. That child may keep deleted files open, so their
disk space isn't freed until it exits.

co:op records each command in the journal before running it. After a restart, it resends finished
results that the controller hasn't acknowledged. If the controller sends a completed command
again, co:op returns the saved result without running it again. It refuses to reuse a command ID
with a different payload.

The controller can acknowledge an asynchronous create request while the session is still being
created, and the journal keeps the original operation ID until co:op can link the resulting
session's activity to it. An event cursor advances only when the controller acknowledges the exact
events, so co:op resends unacknowledged events and doesn't resend acknowledged ones. When you move
or back up a worker, keep the entire journal. The `commands` directory alone isn't enough.

A review may continue after its request loses the connection, and the saved delivery result stays
uncertain even if the review later finishes. Look up the operation with
`GET /v1/operations?key=<original-key>`. Once it succeeds, fetch the saved review report. Neither
request runs the check again.

If the daemon or controller is unavailable, the connector reports the problem and retries. It
doesn't run work locally or delete saved results to get past the failure. If the saved identity
is malformed or expired, the worker stops connecting and doesn't enroll again automatically, even
when an enrollment token is available. Check file ownership, trusted certificates, the clock and
the controller's renewal status, then follow the controller operator's identity-recovery
procedure. Don't delete the identity or journal as a routine reconnect fix.

Before you restart or replace a worker, stop new submissions at the controller, then run
`coop sessions quiescence` to see whether queued or running work remains:

- It reads every committed turn and operation, including the running service's write-ahead log,
  so it needs that service's existing database, WAL and index files.
- It never migrates session data. When you inspect the data from a new container image, mount the
  data read-only. That also stops SQLite from creating sidecar files if the service stops during the
  inspection.
- Zero counts mean no queued or running work remains. Still shut the service down gracefully so it
  can finish its runtime cleanup.
- Create and discard operations in the uncertain state stay unresolved and don't count as running
  work.

| Exit | Meaning |
| --- | --- |
| `0` | No queued or running work remains in the session store |
| `1` | The store is unavailable, damaged or uses an unsupported schema |
| `2` | The arguments are invalid |
| `3` | Work remains |

### Upgrading a worker

A new co:op build may need to convert the worker's session data when it first opens it, and the
previous build can't read that data afterwards. Each build can perform only one upgrade step. If
the data is older than schema 26, the build stops and names the build that converts it first.
Before replacing the worker, check its data with the new build:

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
| `1` | The check couldn't finish: the service is still running, the path has no session data, or some data can't be read | Fix the reported problem and check again. A directory that no service has used can't pass, even if it's empty. Check for a mistyped `--state` or a new volume. A worker that has never run has no data to check. |
| `2` | The arguments are invalid | Fix the command |

The command checks both the session database and the connector's command journal. It doesn't write
to the state directory or create SQLite's `-wal` and `-shm` files. While checking, it holds a
shared state lock to prevent a service from starting and converting the data. If a service is
already running against that directory, the check refuses to read it, so stop the service first.

You need this extra cleanup only if the worker ran a development build from co:op's `main` branch
between 2026-09-15 and 2026-09-23. Those builds left a `.service-launch.lock` file in each
execution-registry folder, and current builds refuse to launch in the affected repositories and
report the file as an unsupported registry entry. Stop the worker, then run these commands as its
OS user to delete the files and any folders left empty:

```bash
find ~/.local/state/coop/executions/v1 -mindepth 2 -maxdepth 2 -type f -name .service-launch.lock -delete
find ~/.local/state/coop/executions/v1 -mindepth 1 -maxdepth 1 -type d -empty -delete
```

### Session inspection evidence

Your controller can request a versioned, size-limited report about a session hosted by the worker.
Send an `api_request` command with the payload
`{"method":"GET","path":"/v1/sessions/<id>/evidence"}` (see the
[`session evidence`](#session-evidence) endpoint below). The connector forwards the daemon's
answer unchanged. It doesn't choose fields, calculate extra values or reveal additional data.

The report includes the network settings saved for the session, the activity observed during its
runs and the approved task assigned to its workspace. Credentials, host paths and packet bodies are
excluded. Destination names appear only if the session job set `egress.export_destinations: true`.

The worker reports support for `session-evidence` version `1` only when its running daemon lists
`session_evidence_versions` in `GET /v1/capabilities`. It checks this independently of the other
capabilities, and configuration can't override the result. If the daemon doesn't list it, the worker
doesn't report support, and your controller can show that evidence export is unavailable instead of
an empty network report.

The controller also receives blocked requests and alerts from filtered runs through the `network`
session event. The daemon uses the same grouping, limits and destination-disclosure rules as
`coop net`.

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
- Turn-submission bodies are limited to 12 MiB, so they can carry input artifacts.
- A turn takes at most 5 input `artifacts`, 8 MiB in total. More artifacts, or more bytes, return
  `invalid_request` ("turn has too many artifacts" or "turn artifact content exceeds its bound").
- Text input artifacts are preserved as complete files in the session mount. The model receives
  bounded manifests with their names, digests, byte counts and local paths, and can read or search
  them in portions across turns. Their contents are never appended wholesale to the model prompt.
- co:op decodes every body as exactly one JSON value and rejects unknown fields.
- Prompts are limited to 256 KiB of valid UTF-8 without NUL.

### Idempotency keys

Choose one key for each action and keep using that key when you retry it. If the method, key and
normalized request body match, co:op returns the saved result without running the action again.
Reusing a key with a different method or body returns `idempotency_conflict`.

After a lost response, retry the same request with the same key. If an earlier response supplied
an operation ID, `GET /v1/operations/{operation_id}` reports its state, error code and resource
ID.

If the worker may have accepted a request but you didn't receive its response, look it up with
`GET /v1/operations?key=<idempotency-key>`. Supply the exact idempotency key, within its size limit.
The query accepts only one `key`. The response contains the public operation information and
excludes the key, request hash, saved execution instructions, result body, stored request, private
result, prompts, native provider IDs and host paths.

### Compacting old turn receipts

When a turn request succeeds, co:op saves a small public result to return if you retry, without the
prompt or private execution settings. The daemon can still read older results that stored the full
turn, and you can compact those during a planned maintenance window:

```bash
coop sessions compact --state <state-root> --backup <new-backup.sqlite>
```

The command locks the state directory so the daemon can't use it during maintenance. The backup
path must not exist yet and must not be inside the state root. After verifying the backup, the
command rewrites the old results in one transaction, checks database integrity and vacuums the
database to reclaim space. It doesn't change the turn records themselves, and it never runs
automatically at startup.

The backup still contains the old prompt copies, so only its owner can access it. Keep it until
you've verified both the compacted database and its normal backups.

To restore:

1. Stop the daemon.
2. Move `session.sqlite` and both possible `-wal` and `-shm` sidecars aside together.
3. Install the backup as an owner-only `session.sqlite`.
4. Restart.
5. Verify with `coop sessions doctor` before deleting the files you moved aside.

### Fencing an uncertain mutation

Your controller may withdraw permission for a session create or turn submission after saving the
request, without knowing whether the worker received it. Don't resend the request just to find out
what to stop, because that could start work you no longer want. Fence it instead. A fence blocks
the request unless the worker has already accepted it.

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

- If the worker hasn't accepted the operation, including when it has only reserved it, co:op
  records a failure with `operation_fenced`. Sending that same request later can't run it.
- If the worker already accepted the operation, the fence returns it and its resource. Inspect
  that result and use the normal close or cancel request to stop the work.

The enclosed request can be as large as a turn submission, with a separate size limit for the
envelope. A lookup that finds nothing, or a client timeout, doesn't prove that the request never
reached the worker.

### Asynchronous session create

To create a session without keeping the HTTP request open, send `Prefer: respond-async` on
`POST /v1/sessions`. co:op saves the request and returns `202 Accepted` with an `operation`
that is still `running` or has already finished. Poll `GET /v1/operations/{operation_id}`
until it succeeds with `resource_type: session`, then fetch the session it identifies.

Creation continues in the daemon if the client disconnects. Retrying the identical request with
the same key returns the same operation. Without the preference header, the request waits for
creation to finish and returns the operation and session together.

### Revision checks

Session lifecycle, budget and cancellation requests include `expected_revision`. Read the session
before acting. If you get `revision_conflict`, read it again and decide whether your action is
still appropriate. Don't guess a new revision or action. Validation decisions don't include
`expected_revision`. They check `candidate_sha256` instead: they approve or reject one exact
unpublished answer, regardless of other changes to the session.

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
for each turn, which resumes the provider session saved for this co:op session. co:op publishes
only the final assistant message, then stops the process and removes its box before marking the
session as parked.

A job with `repository_read_only: true` uses the same kind of process. The fork is mounted
read-only, and files generated by a turn go into a writable output directory in the worker's
private state.

With `limits.warm_idle_timeout_ms`, you get a warm session: co:op can authenticate the ACP
connection before the first turn and keep the same process and provider session for later turns.
Turns still run one at a time. The daemon keeps at most four sessions warm, and each one occupies
one of the four runtime slots while idle.

co:op stops the process and removes its box, services and temporary credential files when any of
these happens:

- The idle timeout expires.
- A turn is cancelled or fails.
- The session closes or is discarded.
- The daemon shuts down.

The provider's saved conversation history remains, so a new process can resume the same session.

Stopping a turn's runtime also removes the workspace's Compose service containers, but a successful
warm turn keeps its runtime and services until warm-session cleanup. The service volumes remain
after cleanup, so later turns can restart services with the same development data. Only an explicit
session discard removes the workspace's volumes and network.

The daemon repeats cleanup at startup after a crash or upgrade, and a background check retries
idle-runtime cleanup every minute without interfering with an active turn. A turn that completed
successfully stays successful if cleanup fails, and the background cleaner retries and logs the
error. If the turn itself failed, its error includes the cleanup failure too.

On restart:

- Queued turns that were never sent to the provider remain in the queue.
- A turn waiting for your validation keeps its unpublished candidate, and you can still accept or
  reject it by digest. Startup and the periodic cleaner remove its provider process, box, temporary
  credential files and services without deciding or changing the candidate.
- If co:op had recorded its intent to send a turn before the interruption, that turn is marked
  interrupted and isn't sent again automatically.

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
answer isn't valid JSON or doesn't match the schema, co:op asks the model to fix it in the same
provider session. Without semantic validation, three invalid responses fail the turn with
`output_contract_failed`, and no assistant message is published.

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

Your controller can review up to three candidates that pass the schema check, and the model has up
to three responses to produce valid output for each one. Responses that fail the schema check don't
use up one of those review attempts.

## Endpoints

### Health

| Method | Path | Result |
| --- | --- | --- |
| `GET` | `/healthz` | `{"healthy":true}` |
| `GET` | `/readyz` | `{"ready":true}` after daemon startup |
| `GET` | `/v1/capabilities` | `{"job_spec_versions":[2],"controller_tools_versions":[1],"repository_freshness_receipt_versions":[2],"session_evidence_versions":[1],"review_commit_message_versions":[1]}`. Check these versions before choosing which protocol features to use. |
| `GET` | `/v1/capacity` | Current shared active and warm runtime slots. The connector forwards this measurement in the hello it sends with every poll. |
| `GET` | `/v1/storage` | Disk use measured by this daemon: `storage` (the summary for a fleet controller), `budget`, `totals`, `roots`, `forks` and `problems` |

The worker has four slots for model processes, shared by active turns and idle warm sessions. The
session, turn and workspace counts all describe those same slots, so don't add them together. Time
spent waiting in the queue doesn't count toward a turn's model timeout. If cleanup fails, the
process keeps its slot until cleanup succeeds. At startup, the worker reports busy with zero free
slots until it has checked which runtimes it owns.

For a quarantined or retired session, cleanup frees a slot only after co:op confirms that no
runtime remains with that session's exact labels. This doesn't make the workspace safe to run
again. If co:op can't read capacity, it reports busy. The worker still polls for requests that
read data or clean up existing work.

After a worker restart, an unused prepared read-only session may retain a warm-process receipt
whose owner cannot be checked here. co:op quarantines that session before admitting new work,
preserves its files and receipt, and checks its exact runtime labels separately. Create a fresh
session for new work; quarantine retirement preserves the old workspace and history.

`/v1/storage` measures the disk blocks allocated to files, not the file sizes that applications
report. It reads file metadata without opening file contents, so it can count private session data
without reading the credentials in it.

Forks can share Git objects with their source checkout through hard links. Storage totals count the
space that deleting the content would free, so shared repository data is counted once, in
`totals.baseline_shared_bytes`.

Read-only primary and companion repositories reuse verified snapshots that never change. Sessions
with the same authorized repository and pinned revisions share unchanged source files, Git objects
and LFS payloads, and storage totals count each payload once. Each session keeps its own Git
configuration, index and outputs, and a primary fork also keeps its own branch and the default and
base history its job approved. Private caches stay separate for each controller and workspace and
for each repository binding.

Only read-only jobs with a frozen source use this storage. Checkpoint, restore and writable jobs
keep independent payloads, and existing sessions with independent payloads stay as they are.

After a Docker-volume copy, a complete current snapshot receipt may name old inodes.
A new session preserves that cache under `.cache-held-*` and rebuilds from its authorized
frozen source. It never rewrites the old receipt or changes existing session projections.
Held bytes remain counted as protected storage. The upgrade check reports copied caches
as rebuildable and held copies as preserved; malformed or unsupported receipts still refuse.

Existing read-only primary workspaces whose current receipt names a copied inode are
quarantined at startup. Their files and completed-turn history stay intact. Explicit quarantine
retirement removes only session custody; it neither repairs ownership nor proves runtime cleanup.

A missing, damaged or newer ownership receipt stops co:op from reusing the shared data, and an
unsupported receipt also prevents destructive cleanup. Local fork commands and other writable mounts
can't take over a read-only primary whose creation was interrupted. The normal authorized discard
releases a primary's references to the shared data.

Cleanup can remove an unused repository snapshot after an hour, or sooner when disk space is low or
use is above the disposable-byte budget. A snapshot that a session references stays, so closing one
session can't invalidate another session's files.

Each cleanup pass has a 30-second limit. If cleanup can't prove an entry's ownership and cleanliness
in that time, it keeps and reports the entry, then moves on to other entries in later passes, so
large or slow entries can stay in place. Private ownership receipts let interrupted creation and
removal resume, and `coop sessions check` checks their format before an upgrade.

When free disk space falls to twice the default reserve, new source downloads and checkouts wait
before they allocate space, while verified source cache hits keep working. The wait keeps the same
saved worker command, resumes when space returns and doesn't use up a model attempt. A later job for
the same repository, commit, tree and submodules links the verified checkout and LFS files already
on disk, and keeps its own Git metadata and its record of how the source was resolved.

An incomplete measurement reports `totals.unknown` and sets `storage.unattributed_bytes` to `null`.
co:op measures the full tree at most once per `budget.measure_seconds`, and `storage.measured_at`
lets callers check how old the measurement is.

`forks[]` lists each directory under a fork root, its category and the evidence for that category.
Only `disposable` and `staged_discard` can be removed through the authorized cleanup process. For
`active`, `grace`, `protected`, `control` and `unattributed`, the response explains why removal is
refused.

A directory is `protected` in any of these cases:

- It belongs to a running worker or registered box activity.
- A merge was interrupted.
- The task system still needs it.
- Its session is quarantined.
- It holds uncommitted work.

Without a co:op record identifying that fork instance, the directory is `unattributed`, and co:op
reports it for inspection instead of guessing that it can be deleted.

When disk space is too low, the daemon refuses new workspaces with `storage_unavailable` and
explains the cause. Reads, cleanup and discard still work. co:op doesn't delete protected data
to make room.

Checkpoint capture and restore need space for extra copies, including during recovery. They have
a separate storage check that waits for enough space above the reserve. While a restore is
interrupted, the session remains blocked from running work.

The worker stops creating workspaces when disk use reaches `storage.high_watermark_bytes`, and
starts accepting them again only when use drops to `storage.low_watermark_bytes` or below. New work
must leave `storage.reserve_bytes` free. These checks don't limit how much data an already-running
task writes in its workspace.

Before reporting support for `job-setup:2`, `repository-freshness:2`, `session-evidence:1`,
`controller-tools:1`, `project-runtime:1` or `review-commit-message:1`, the connector checks the running daemon's
`GET /v1/capabilities` response for the matching version. If it can't verify a capability,
it leaves it out of the next hello.

Worker protocol v2 reports supported features, runtime capacity and optional disk measurements.
It doesn't distribute lists of policies or repositories. Each controller job specifies its own
exact code and execution settings, which can't change after submission. The shared
[protocol fixture](../testdata/protocol/coop-worker-v2.json) shows one worker poll and the
controller's reply.

### Sessions

Your controller creates `create-session.json` with these fields:

- `task`: your external task reference. co:op doesn't interpret its value.
- `job`: a version-2 JobSpec.
- `expected_job_digest`: the SHA-256 digest of the job's canonical representation.
- `controller_tools` (optional): connects the session to your controller's authenticated tool
  endpoint.

co:op refuses a job that is missing a required field, has an unknown field or would change when
co:op normalizes it. The error is `invalid_request`, and its detail names the problem.

```bash
curl --unix-socket "$SOCKET" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: controller:create:01J...' \
  -H 'Prefer: respond-async' \
  --data-binary @create-session.json \
  http://localhost/v1/sessions
```

The job selects model targets, network rules, queue and turn limits, and the complete work and
review setup. Its `mode` is always `"normal"`. Set `repository_read_only: true` for a job that must
not change its repository. A job that asks for `readonly` or `bare` mode is refused with
`invalid_request`. Besides the other required fields, version 2 requires:

```json
{
  "environment": {"CI": "1"},
  "check": {"argv": ["make", "test"], "environment": {"DATABASE_URL": "postgres://test-db/app"}},
  "resources": {"cpu_millis": 2000, "memory_bytes": 4294967296, "pids": 512}
}
```

These fields must be present even when the environment maps and check argv are empty.

co:op passes the job's environment variables only to processes in the box. Review uses those
variables plus `check.environment`, and the check's value wins when a name appears in both. Names
must be valid shell identifiers. `COOP_*`, provider credential variables, model variables and
adapter-owned variables are reserved.

Values must be UTF-8, with no line breaks or leading or trailing whitespace. Use these variables
for job settings, not for model or GitHub credentials.

The check is a list of command arguments, passed directly to the process and never interpreted as
a shell command. If you need shell syntax, run a shell explicitly. If both `check.argv` and the
check environment are empty, no check runs, so the review can't be published.

| Field | Meaning | Accepted range |
| --- | --- | --- |
| `cpu_millis` | CPU, in thousandths of one core | 10 to 128000 |
| `memory_bytes` | Memory, in bytes | 6 MiB to 1 TiB |
| `pids` | Maximum processes per container | 1 to 65536 |

Every job must set positive, finite CPU, memory and process limits. If a requested limit exceeds
the worker's configured maximum, or the runtime can't enforce it, co:op refuses the job instead of
silently lowering the value.

co:op refuses retries that change the saved job. Task text and repository settings can't change
what the worker is allowed to run. Your controller may use repository defaults, but it must choose
and include every setting before submission. The worker has no local policy name, policy catalog or
separate source selector.

Writable source jobs may additionally include an optional project runtime:

```json
"runtime": {
  "version": 1,
  "dockerfile": ".agent/Dockerfile",
  "compose": "dev/services.yml",
  "environment": {"PGHOST": "db"}
}
```

These are canonical repository-relative paths resolved by the controller at the frozen source.
An empty Dockerfile uses the shared image; an empty Compose path starts no services. Retained jobs
without this field keep their exact authority and digest. Send runtime-bearing jobs only to workers
advertising `project-runtime:1`, proven by `project_runtime_versions: [1]` on the local daemon.
The runtime's `environment` holds frozen project defaults, with a 64 KiB limit measured as escaped
JSON. Candidate `review.env` overrides those defaults. The job's root `environment` and
`check.environment` remain controller authority and override candidate values.

Work gets its own Compose project and default network, and services get finite job resource caps.
At least one service must join the default network; custom-only network topologies are refused
before startup. Service aliases and additional project networks are preserved. Required setup
failures stop the command. Host ports and globally named/external volumes are refused or
removed from the job setup. Named data volumes survive ordinary turns on the same worker;
anonymous volumes and tmpfs are disposable. Final discard removes owned containers, networks and
named volumes before deleting the workspace, using immutable labels even if project files changed.
A replacement worker reconstructs test services; checkpoints do not transfer database volumes.
A candidate's `review.compose` can replace the Work stack with a separate disposable review stack.


co:op computes the job digest itself. It writes the job's JSON the way Go's `encoding/json`
does: no extra whitespace, object keys in sorted order, and `&`, `<` and `>` inside strings
escaped as `\u0026`, `\u003c` and `\u003e`. The digest is the SHA-256 of those bytes, in
lowercase hex. If it differs from `expected_job_digest`, co:op refuses the session with
`invalid_request`.

To check your encoder, use the [digest test vectors](../testdata/protocol/job-digest-v2.json). Each
one gives a job, the exact bytes co:op takes the digest of, and the digest. The file also lists
values of `source.binding.resolved_at` that co:op refuses, such as a fraction with trailing zeros.

Send version-2 jobs only to workers that report `job-setup:2`. co:op doesn't fill in missing
execution settings for v1 jobs. Older sessions and their evidence stay readable, but their turns,
reviews and interrupted creates can't run until they're resubmitted as v2 jobs.

#### Frozen repository source

Before creating the job, your controller resolves the requested default branch, named branch, PR
or commit to exact commits. `job.source` identifies the GitHub repository by its stable ID and
slug, and records the default, selected and base commits, the approved tree and the information
that binds the job to that source. It also lists every authorized submodule, including nested
ones, with the exact repository, commit and tree. Don't include a host checkout path, remote URL
or GitHub token in this document.

The worker gets short-lived credentials through its authenticated connection to the controller,
then downloads Git and LFS data directly from GitHub and verifies the complete working tree.
Repository files such as `.gitmodules` and `.lfsconfig` can't redirect those authenticated requests.
The job fails if history or LFS files are missing, submodule commit references don't match, or
there isn't enough disk space. A job with `source: null` uses an isolated empty workspace.

#### Session endpoints

The public session response identifies the fixed repository source, `job_ref`, `job_digest`, model
target, execution mode, project exposure settings, saved network settings and repository freshness.
It also includes companion aliases, base commits, the generated fork name, revision, state,
activity, queue and budget counts, the event cursor and timestamps. It excludes host paths, provider
state, prompts and credentials.

| Method | Path | Body/query |
| --- | --- | --- |
| `POST` | `/v1/sessions` | `task`, `job`, `expected_job_digest`, and optional `controller_tools` |
| `GET` | `/v1/sessions?limit=100` | `limit` is `1..1000` |
| `GET` | `/v1/sessions/{session_id}` | none |
| `POST` | `/v1/sessions/{session_id}/prepare` | `expected_revision`. The job must turn on warm execution. |
| `POST` | `/v1/sessions/{session_id}/workspace` | `expected_revision`, `task`. The session must be open, writable and unused. |

A session stored in `readonly` or `bare` mode is quarantined at startup and never runs; a new turn
returns `invalid_session_state`. Retire its record as described under quarantine, or close and
discard it with a co:op build at or before commit 29133b0e.

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
`queue_id`, `task_id`, `id`, `offer_ref` and `draft_sha256`. `queue_id` and `task_id` are random
values co:op generates and saves, `id` is the task's folder name, and `draft_sha256` is the SHA-256
digest of the task as co:op encoded it. Binding the task increases the session revision by one and
records a `workspace.task_bound` event.

The session must be open, writable and parked, with no active, queued or previously used turns.
Otherwise, the request fails with `invalid_session_state`. Changing a task that the workspace
already holds under the same `offer_ref` also returns that error.

A stale `expected_revision` returns `revision_conflict`. An invalid task returns
`invalid_request`, as does submitting a different task to a session that is already bound.
Repeating the exact binding leaves the session unchanged.

### Workspace checkpoints

| Method | Path | Input/output |
| --- | --- | --- |
| `POST` | `/v1/sessions/{id}/checkpoint` | JSON: `session_ref`, `expected_revision`, `placement_generation`, `repository_ref`. Returns the operation and descriptor. |
| `GET` | `/v1/operations/{id}/checkpoint-bundle` | Streamed tar, exact `Content-Length`, SHA-256 `ETag` |
| `POST` | `/v1/sessions/{id}/workspace/restore` | Binary body plus `X-Coop-Expected-Revision` and base64-JSON `X-Coop-Workspace-Checkpoint` headers |

A credential found in newly exported content refuses capture with HTTP 409
`invalid_session_state`, including on replay of the same operation key. The workspace remains
intact and no checkpoint bundle is published; correct the content before requesting a new capture.

To capture a checkpoint, the session must be idle, writable and bound to a task. You can capture an
open, exhausted or closed session, but not a discarded one. Capture keeps a closed session closed
and can't restart model execution. To restore a checkpoint, create an unused replacement session
with the same authorized base and task offer. Both requests require an idempotency key. Restore uses
the content type `application/vnd.coop.workspace-checkpoint.v2+tar`, and its Content-Length must
match the descriptor.

A v2 checkpoint streams Git objects with their types and verified LFS files. It includes new
commits, the final tracked tree, untracked files and task state. Restoring it recreates the exact
HEAD and working tree, but not the original split between staged and unstaged changes.

Changed submodule commits need a separate authorized process to keep and verify their data, so
checkpoints refuse them, including intermediate submodule changes that were later reverted.

Checkpoint processing limits memory use and metadata size, but it sets no fixed limit on the data
itself. co:op checks disk space and cancellation throughout the transfer and Git operations, which
can stop the operation when space is low but can't prevent other host processes from filling the
filesystem.

co:op checks every archive entry and the task's identity in a separate area before changing live
files, and it saves the complete request body before marking the restore as running. If restore is
interrupted, the session can't run work, even after a restart. Restore resumes from the saved body.
Retry the same operation. Don't send a new key for a session that is already bound.

Capturing a checkpoint also reserves the runtime so a model turn can't change files during
capture. Restoring a checkpoint doesn't restore permission to publish. Review the candidate
again before publishing it.

Discard removes checkpoint artifacts only after verifying that the local session owns them, and it
resolves any interrupted restores for that session. You can still read older v1 artifacts, but you
can't restore them, because their patch-only format doesn't keep enough information to verify the
complete Git and LFS data.

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
| `POST` | `/v1/sessions/{session_id}/turns` | `expected_revision`, `prompt`. Optional: `artifacts`, `min_target_index`, `rewind_target`, `output_contract` |
| `GET` | `/v1/sessions/{session_id}/turns?after=0&limit=100` | Continue after the numbered turn given by the cursor |
| `GET` | `/v1/sessions/{session_id}/turns/{turn_id}` | none |
| `GET` | `/v1/sessions/{session_id}/turns/{turn_id}/artifacts/{artifact_id}` | raw generated image |
| `POST` | `/v1/sessions/{session_id}/turns/{turn_id}/cancel` | `expected_revision` |
| `POST` | `/v1/sessions/{session_id}/turns/{turn_id}/validation` | `candidate_sha256`, `verdict`. `accept` forbids `violations`, and `reject` requires 1 to 20. |

Before accepting, rejecting or cancelling a turn awaiting validation, co:op stops and removes
the provider runtime and temporary credential files that produced its candidate. If it can't
confirm cleanup, the API returns `503 session_cleanup_error`. The candidate remains
`awaiting_validation`, and your decision hasn't taken effect.

That failed operation is finished: retrying its idempotency key returns the same failure. After
the background cleaner or an operator fixes cleanup, fetch the current candidate and submit your
decision with a new idempotency key.

The public turn response excludes the prompt, idempotency data, `min_target_index` and
`rewind_target`. You can see the effect of those last two settings in the session's `target`
and `session.target_rotated` event. For a completed turn, `assistant_message` is the response
to show the user. co:op doesn't publish hidden reasoning, raw tool calls, raw ACP frames or box
logs in that response.

A completed turn includes `usage` when the adapter reports it. The fields are `input_tokens`,
`cached_input_tokens`, `output_tokens`, `reasoning_tokens` and, when available, the provider's
`cost_usd`. Check `cost_recorded` to distinguish a reported zero cost from a missing cost.

ACP reports the cumulative cost of the provider session, and co:op calculates this turn's share
before returning it, including after daemon restarts or adapter-process resets. Fresh and cached
input tokens are reported separately because providers charge different rates for them, and
combining them before calculating cost would overestimate the bill. If the adapter reports no
usage, co:op leaves the object out. Missing measurements are never represented as zero usage or
free work.

A completed turn may include `output_artifacts` for images the agent generated or saved in the
turn's `.coop-output/<turn_id>` directory. Each record has an opaque ID, a safe filename, the
media type, a SHA-256 digest and the exact byte count, but not the image itself. Download it
through the raw endpoint, which returns the saved file with matching `Content-Type`,
`Content-Length` and `ETag` headers. The saved file can't change.

co:op captures image blocks and embedded image resources from ACP tool updates in output order,
naming them `generated-1.<ext>`, `generated-2.<ext>` and so on. Image bytes don't count toward
the text transcript limit.

co:op inspects text tool updates one frame at a time and then discards them, so a long turn doesn't
fail just because its tool updates add up to a large transcript. Separate limits still apply to
assistant text, each incoming frame, the turn's duration and saved output artifacts.

Only PNG, JPEG, WebP and GIF are accepted. A turn returns at most five images. Images from tool
updates and files in the turn's directory share that limit, and a repeated image is kept once. A
file in that directory without a `.png`, `.jpg`, `.jpeg`, `.webp` or `.gif` extension is ignored.
co:op rejects symlinks, special files, mismatched content, a sixth image, a file over 8 MiB, or
more than 8 MiB in total.

co:op removes the temporary output directory before completing the turn, so generated charts don't
become repository changes. In a read-only repository session, the container still sees the
path `.coop-output/...`, but its files are stored in co:op's private session state on the host.

Cancellation first asks ACP to cancel the turn. co:op then stops the turn's process group and
removes its box. This doesn't undo actions already taken in external systems.

### Events

```bash
curl --unix-socket "$SOCKET" \
  'http://localhost/v1/sessions/remote_.../events?after=41&limit=100'
```

The events endpoint returns a JSON array ordered by `sequence`, which increases within each
session. Save the last sequence you processed, and after a disconnect, request `after=<sequence>`
to continue from that point.

Events stored privately for the worker's owner include the identity, sequence, turn ID, type,
version, timestamp and payload. Tool evidence has size limits but may include filesystem paths,
arguments, results and diffs. Events describing tool starts and finishes, thoughts, progress, plans
and permission decisions also carry `checkout_root`. The connector reads it to remove the checkout
path before forwarding the event.

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
`limit` on event count, and the endpoint returns fewer events instead of cutting one short.

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
evidence. A run with no refusals and no alerts produces no event, so this event isn't a heartbeat
for every turn. In the payload:

- Destinations are grouped and capped, with an `omitted_destinations` count.
- Alerts are capped.
- `evidence_id` names the saved event that `coop net blocked` can open.

The event follows the same disclosure rules as the network routes. A destination appears only when
the session job set `egress.export_destinations: true`.

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
preview doesn't remove the exit result.

When a tool provides structured file information, `path_context` includes `basis: "lexical"`
and up to 16 `paths` entries. Each entry uses a JSON-pointer `source` to identify where the
information came from. Its `scope` is `project`, `outside` or `unknown`. Only project-relative
files receive a `path`.

The host checkout root is never exported. `partial: true` means some path information was left out.
co:op classifies paths by their spelling. It doesn't check where symlinks lead, decide whether
access is allowed or guess missing paths from tool titles or shell commands. A later tool update can
add paths to the completion event without changing the start event.

co:op extracts paths before shortening large diffs into previews. It marks URI-shaped paths as
unknown and doesn't treat them as checkout files. Before forwarding an event, the connector checks
the path fields again, drops entries it can't verify and marks the context `partial`. It also
applies the size limits and secret scan described under [Events](#events) to titles and inputs.

`provider.backoff` reports a confirmed provider rate limit. co:op sends at most one such event for
each configured model target in a turn.

- `attempt` numbers the events from 1 within the turn.
- `target` identifies the model target that reached its rate limit.
- `retry_after_seconds` says how long that target must wait. co:op uses the provider's reset time
  when available, or its own limited backoff otherwise.
- If co:op switches to another target, the event includes `next_target`.
- If every target is limited, the last backoff includes `all_limited_until` instead.

Use these events to tell a turn waiting for a rate limit from one that has stopped responding.
Waiting can still be expected progress, so silence alone isn't a reason to cancel the turn.

`provider.alive` confirms that the worker is still receiving data. It helps when the provider CLI
retries a rate limit internally: co:op's model selection never sees that limit, so it can't send a
backoff event, and the CLI may keep sending ACP frames with no tool calls or other activity events.

`frames` and `bytes` count all ACP frames received for the current turn's prompt. Compare these
cumulative counts, not the timestamp, to tell new activity from a repeated event. `last_kind`
identifies the latest frame as its JSON-RPC method, a session-update type after a colon, or
`response`, and contains no frame content. Repeated events with the same kind tell you what the
provider is still sending.

co:op sends this event at most once per minute, and only when it hasn't already reported other
activity in that interval. If the provider sends no frames, there's no event, so your client still
needs a timeout for turns that go completely silent.

A turn fails with `acp_quiet` if the model sends nothing for 15 minutes after its tools have
answered. Only the model's text, thoughts, plans and tool calls count as output, and liveness events
don't. This timeout doesn't apply before the first tool finishes or while a tool is running. The
error reports the silent duration and the provider's last frame kind. You may retry the turn,
because this error reports a silent model and not a failed project task.

If the ACP process exits before returning an answer, the turn fails with `acp_process_error`. The
detail includes its exit status or signal and a summary of stderr: line and byte counts, a digest
for grouping similar failures, and fixed categories such as rate limit, authentication, network or
crash. co:op masks digits and hexadecimal runs before it calculates the digest. The stderr text
itself never leaves co:op.

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
  `parent_head`
- the fixed `admitted_source_tree`, for a repository-backed session with a source binding
- paths classified as committed, staged, unstaged, untracked or conflicted
- ahead and behind counts, and base-to-head divergence counts
- a page of the binary patch, with a size limit
- `patch_digest`, the exact `patch_bytes`, `patch_offset`, `patch_next_offset` and `patch_has_more`

Compare `admitted_source_tree` with the current tree to distinguish changed content from empty
commits or changes that were committed and later reverted.

To fetch the next patch page, call
`GET /v1/sessions/{session_id}/changes?patch_offset=<next>&patch_limit=<bytes>`. `patch_limit`
can't exceed the job's `max_patch_bytes`, and offsets can't exceed 1 GiB. Check `patch_digest` as
you page through the result. If it changes, start again at offset zero so you don't combine pages
from different patches. The `truncated` field is true whenever the response contains less than the
complete patch.

The JSON response encodes `patch`, `path_bytes` and `old_path_bytes` as base64. `patch_bytes` is an
integer giving the complete patch's size. A valid UTF-8 filename also appears in `path`. Filenames
containing arbitrary bytes are preserved in `path_bytes`. If Git fails, the endpoint returns an
error, never an empty change list.

### Network

```bash
curl --unix-socket "$SOCKET" http://localhost/v1/sessions/remote_.../network
curl --unix-socket "$SOCKET" http://localhost/v1/sessions/remote_.../network/connections
curl --unix-socket "$SOCKET" http://localhost/v1/sessions/remote_.../network/explanations/<event_id>
curl --unix-socket "$SOCKET" http://localhost/v1/sessions/remote_.../network/receipt
```

These four endpoints only read data. They don't probe a gateway or approve, grant or expand
network access, and the session API can't grant network permissions.

`GET /v1/sessions/{session_id}/network` returns:

- the session's fixed `mode` and `fingerprint`
- the `requested` and `effective` rule texts
- `current`: the newest run's saved observation summary, or `{"status":"no run yet"}`
- an `alerts` list with a size limit

`GET /v1/sessions/{session_id}/network/receipt` combines the records for all of the session's
runs into one versioned report. It tracks `finality` separately from `completeness`. The report
becomes `final` only after the session is closed and every run's record is final. A `final`
report can still be `partial` if some evidence is missing.

The worker saves these records in the owner's registry, so both endpoints keep working after
container cleanup. A session that never used filtered networking returns
`{"available":false,"reason":"..."}` instead of an empty record.

`GET /v1/sessions/{session_id}/network/connections` returns the collector's live connection records
for the newest run, with a row limit. The response includes `status` to describe freshness, using
the same values as the summary, plus `as_of` and `detail_truncated`. Before the first run, the
endpoint returns `{"status":"no run yet","connections":[]}`.

`GET /v1/sessions/{session_id}/network/explanations/{event_id}` returns one saved refusal from this
session's runs. It includes the event, the reason, where the evidence came from and whether co:op
drafted a proposed rule. Each run keeps a limited number of events. If the requested event has aged
out, the endpoint returns `{"available":false,"reason": "event_not_retained"}`, which doesn't mean
the event never existed.

`projection` states what the response discloses. It is `destinations-withheld` unless the
session job set `egress.export_destinations: true`. With that setting, it is
`destinations-included`, and the response includes rule texts and observed names.
The daemon controls this disclosure. The connector forwards each answer unchanged and can't choose
different destinations, rules or disclosure settings.

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

This endpoint returns a versioned, size-limited report for a controller's session-inspection page.
It covers:

- the network settings approved for the session
- activity observed during its newest run
- the combined network record for all its runs
- the approved task assigned to its workspace, read from the current task folder

The endpoint reads saved evidence without probing a gateway or starting a runtime. It can't grant
additional access and accepts no query parameters, so callers can't change what it discloses. The
daemon checks the report against its format and disclosure rules before sending it. If the report
fails that check, the endpoint returns `500 internal_error` instead.

Each section reports whether its data was available, so you can tell a failed read from a successful
read that found no activity:

| Section | Status words |
| --- | --- |
| `network.access` | `captured`, `not_filtered`, `unavailable` |
| `network.observation` | `observed`, `no_run`, `not_filtered`, `unavailable` |
| `network.receipt` | `available`, `not_filtered`, `unavailable` |
| `task` | `bound`, `unbound`, `unavailable` |
| `task.snapshot.state_note` | `captured`, `absent`, `withheld` |

An `unavailable` section includes a `reason`. A session that never ran filtered reports
`not_filtered`. The network settings (`mode`, `fingerprint`) come from the saved session record and
can't change. If the host can't read the network registry, those settings stay available while the
affected sections report that they couldn't be read. Before its first run, a filtered session
reports `no_run` and a `provisional` receipt with `run_count` `0`, not an empty activity counter.

Counters are unsigned decimal strings so JavaScript clients can preserve values above 2^53
without rounding them. A `null` counter means the metric was not measured, never that it was
`0`.

Each response list has a size limit, and the report counts omitted entries in `omitted_denials`,
`omitted_connections`, `omitted_alerts` and `omitted_run_references`.

The report follows the network endpoints' `projection` setting. Destination names appear only
with `destinations-included`. When a name is withheld, the refusal includes
`destination_withheld`, so callers can distinguish a hidden name from a missing one.

`task` includes the fixed task identity saved for the session and a snapshot of its folder at
the time the report is captured:

- its state
- the same `state_sha256` used for the task data in a workspace checkpoint
- the checklist labels that match the checkpoint's completion flags
- a list of files and their digests
- the agent-written `state.md`

The `state.md` note has a size limit and is withheld in full if the scan flags a likely secret. The
worker keeps the current snapshot, not a history of task transitions, so save successive snapshots
if your controller needs that history. If a bound task's folder is missing, the report returns
`unavailable` and keeps the task's identity instead of returning an empty task.

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

You can review a session only when all of these are true:

- It is parked.
- It is open or exhausted.
- It has no queued turns.
- Its fork is clean and committed.

co:op records the exact source and parent commits and trees, then prepares a temporary candidate
based on the current parent `HEAD`. It runs the saved job's check against that candidate and
returns:

- the creation base, source, parent and candidate commit and tree identities
- the saved PR number, ref and head, when the session came from an existing PR
- the rebase and check results, including the startup error text in `gate_error` if startup failed
- `gate_output`: the check command, its `exit_code` if it returned one, the number of saved output
  bytes and whether all output was kept (`complete` confirms it was, `incomplete` explains what is
  missing, and `lost` means no output was kept)
- policy findings, with a size limit
- a size-limited inline preview of the patch from the parent tree to the candidate tree
- `patch_truncated`, `candidate_retained`, `publishable` and stable not-publishable reason codes

The saved job's `check.argv` is the only remote review command. The worker's `COOP_GATE`, the
parent's `gate:` and candidate changes can't replace it. The first two still apply to ordinary
local fork merges.

The check runs in an isolated container using the session's saved network mode and, for filtered
runs, the exact saved network permissions. It ignores the repository's `box:` settings and the
worker's runtime arguments, environment file and MCP configuration. It uses the saved work
environment plus check-only overrides, with the same CPU, memory and process limits as model
turns. It also applies the candidate's [`review:` settings](configuration.md#review-checks).
Where the job and `review.env` set the same variable, the job's value wins. Project defaults in
`runtime.environment` apply below `review.env`. The check uses the
same granted project toolchain or worker-configured image as the turns. Without a project runtime,
it continues to use the worker-configured image, or locked filtered client image.

The check can create ignored build files in its temporary checkout. If it changes the reviewed
source, the result can't be published. Publication is also blocked when no check is configured, or
when the check fails or can't start.

When your controller authorizes publication, the change may edit CI workflows. Running repository
checks is part of the authorized task and doesn't need a separate human approval. The scan still
checks workflow contents for credentials and refuses files hidden from the box, changed
`.coopignore` files, automatic local hooks and new package installation scripts. A hook nested under
`.github/workflows/` still counts as a local execution finding. Local fork merges keep their own
protections. Publication still needs a fresh controller grant for the exact saved candidate and
destination, and repository CI permissions and environment protections also apply.

The JSON response encodes the patch preview as base64. A shortened preview doesn't prevent
publication, because co:op saves the exact candidate commit and verified LFS objects in private host
storage, separate from the model's workspace.

For the same saved inputs, co:op creates the same single candidate commit on the reviewed parent,
before running the check. `source_head` records the model's original commit before that candidate
was prepared.

co:op scans the changes between the parent and candidate for secrets, including large files, binary
Git objects and LFS files. If LFS attributes changed, it also scans the resulting LFS files. If any
content can't be read, review fails, because co:op doesn't assume that unreadable content is safe.

`publishable` tells you whether this exact candidate passed the requirements for publication.
It doesn't grant permission to push or merge. It is false when there is:

- a conflict
- no configured check, or a check that failed
- a startup failure
- findings from policy checks
- a changed parent or source
- a remaining fork-worker record, even if its process has exited
- no changes to publish

`candidate_retained` tells you whether co:op saved the exact candidate, which it can do even if the
check is missing or fails. Saving it doesn't authorize publication.

To publish, use the saved `candidate_head` unchanged, get a separate controller grant and update the
destination ref only if it still matches the expected value. Never reconstruct the result from a
patch or the model's later workspace. Discarding a session deletes its saved candidates, so keep
sessions whose reviewed changes still need to be published.

A review can continue after the requesting connection ends. Once its operation succeeds, fetch
`GET /v1/sessions/{session_id}/reviews/{operation_id}` for the saved `operation` and `review`
response. This doesn't start or resume the check. The endpoint refuses requests for another session,
an operation that isn't a review, or an unfinished review. `policy_findings` and
`not_publishable_reasons` are always arrays, even when empty.

#### Gate output

The review keeps the stdout and stderr of the gate (the check command) and co:op's startup messages
as printed, whether the gate passes or fails. Read the output page by page with
`GET /v1/sessions/{session_id}/reviews/{operation_id}/gate-output?cursor={cursor}`, starting without
a cursor:

```json
{"output":"1 test, 1 failure\n…","next_cursor":"1048576","bytes":1843200,"complete":true}
```

Each page contains at most 1 MiB of output and ends at a character boundary. Invalid UTF-8 bytes
appear as U+FFFD replacement characters. `next_cursor` is `null` on the last page. `bytes`
reports the total amount of saved output. When `complete: false`, `incomplete` explains what
is missing. co:op keeps at most the first 64 MiB.

If no output was saved, the response is `{"lost":"<why>"}`. This can happen because no check
ran, writing the output failed, or the session was discarded. Output can be read only through the
session that owns the review, and discarding that session removes it.

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

- `published`, with the exact commit, tree and PR result
- `conflict`, with the verified existing PR and its observed remote head
- `refused`, with a stable error code

The worker requests fresh credentials from your authenticated controller at
`POST /v1/coop-workers/jobs/{job_ref}/publication-grants`. The request identifies the session,
job digest, review operation, original command key, repository and exact publication request.
Your controller returns that same repository identity, a `token`, its `expires_at` time and
the GitHub App's `actor_id`.

Your controller decides whether publication is approved. co:op doesn't require a product-specific
approval format. A `403` permanently refuses this operation. Temporary failures can be retried.

The worker sends Git and LFS data directly from the saved candidate to GitHub, and creates a
draft PR or updates the exact existing PR owned by the App. If a person has already marked that
PR ready for review, co:op preserves that choice.

If a push or PR response is lost, co:op checks the remote result before retrying. A candidate
can't be discarded while publication is active. Later edits in the model workspace can't change
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

You can close a session only when it has no active or queued turns. Closing keeps the fork.

Plan a discard only after you close the session:

```bash
curl --unix-socket "$SOCKET" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: controller:discard-plan:01J...' \
  -d '{"expected_revision":16}' \
  http://localhost/v1/sessions/remote_.../discard-plan
```

The discard plan records the session revision, the specific fork instance and its reservation,
and the workspace's inode, branch, head, status digest and running, dirty and unmerged states.
The inode helps detect a replaced directory, but doesn't prove ownership on its own.

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

Discard first checks that the plan belongs to the session named in the request path, then compares
the current workspace with every recorded detail in the plan. If the plan is stale, the workspace
was replaced or work is running, nothing is deleted. Otherwise, co:op deletes the fork workspace and
private ACP state, and saves a record that the session was discarded.

At startup, the daemon blocks sessions whose workspace ownership it can't verify. This includes
old session records without a store-owner binding, v1 reservations, and sessions whose fork record
or workspace is missing. It also blocks a stored session this build no longer reads: one with no
saved job, no execution or network mode, or one created in the removed `readonly` or `bare` mode.
The log names the session. These sessions are quarantined: you can't create or apply a discard
plan for them because co:op can't prove it owns the workspace. You can retire the session record
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
- The workspace, fork reservation, sidecar services and provider's ACP history are kept.

A session that isn't quarantined is refused with `invalid_session_state`.

Retiring a record doesn't show that its runtime has stopped. Separately, the daemon looks for
boxes with the exact `coop.run` labels derived from the store's session and turn IDs, and removes
matching boxes and temporary credential files in its private ACP state. If a check fails or
finishes only partly, the daemon retries and reports busy until cleanup is fully verified. These
checks continue after a restart.

Once runtime cleanup is verified, the worker can report free capacity again. It doesn't take
ownership of or delete the unowned workspace. Quarantined sessions still can't run and need
explicit retirement or recovery. If capacity stays busy, inspect the worker's runtime-cleanup
warning.

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

The daemon also writes a size-limited JSON diagnostic to stderr, with the operation ID, method,
resource identity, error code and a detail with sensitive information removed: repository and
state-directory paths are replaced, and lines containing secrets are removed. Use the operation ID
to connect the client error, saved operation and supervisor log without opening SQLite directly.

Common status mapping:

| HTTP | Meaning |
| --- | --- |
| `400` | Invalid request, or a field exceeds its allowed limit |
| `404` | Session, turn or operation not found |
| `409` | Idempotency, operation fence, revision, state, queue, budget, resume, uncertainty or discard conflict |
| `413` | Ordinary request body exceeds 128 KiB, or turn submission exceeds 12 MiB |
| `500` | Internal failure. co:op leaves host paths and raw internal errors out of the response. |
| `503` | The worker is not ready, or it temporarily can't verify or use the required repository, runtime, network or storage access |

`storage_unavailable` is a retryable `503`. The worker has reached its configured disk-space limits
and won't create another workspace until space is freed. If `storage.refusal_reason` is
`protected_storage_exceeds_budget`, you must reduce the protected data itself before the worker can
accept new workspaces. Read `/v1/storage` to see what is using space and why it is kept. Place the
session on another worker or free storage through the appropriate cleanup process. Don't retry in a
tight loop or delete protected workspaces just to meet the limit.

`network_unavailable` is a `503`. The worker can't enforce, or verify that it can enforce,
the session's saved network permissions. Check its container runtime and network setup. The
worker stops execution instead of falling back to open networking. Don't change a saved job to
bypass the refusal.

For `operation_uncertain` or `turn.interrupted`, inspect the saved operation and resulting
session state to find out what happened. An unknown result isn't a reason to submit the same
action with a new key: that could run it twice.

`RunReview` is a limited exception because it only reviews a fixed candidate. Once co:op has
saved the exact source, parent and job, repeating the request with the same key resumes that
review under its original operation ID. It doesn't reread a moving branch to choose a new
candidate. If the saved instructions can't be read or verified, the operation remains
`operation_uncertain`.

## Operational recovery

| Situation | Action |
| --- | --- |
| Client response lost | Replay the exact mutation with the same idempotency key |
| Permission is withdrawn while a prepared create or submit may be arriving | Fence the exact operation. If it was already accepted, close or cancel the resulting work. |
| Socket reconnect | Resume event polling after the last sequence your controller saved as processed |
| Daemon already owns state | Stop or inspect that daemon. Don't remove the lock. |
| Stale socket after crash | Start the daemon with the same state directory. It acquires the state lock before removing the stale socket. |
| Queued turn at restart | co:op resumes queued turns in the order received |
| Running session create at restart | co:op resumes creation automatically from the saved request |
| Reserved operation at restart | co:op records that the operation was interrupted before acceptance. It hadn't attempted any external action. |
| Running checkpoint restore at restart | Resume from the saved request body while model work remains blocked. Never submit a new key. |
| Other running operation at restart | co:op marks it `operation_uncertain`. Inspect what happened before deciding what to do. Don't resubmit it under a new key. |
| Operation remains running after startup | The background watchdog resumes creates that are safe to retry. It marks stale operations uncertain when it can't determine their outcome. |
| Turn interrupted after co:op recorded its intent to send it | Report that the turn was interrupted. Don't submit the same user input automatically. |
| Active turn must stop | Cancel it with an idempotency key, then read the turn to confirm how it ended |
| Session should stop using runtime resources | Wait until it is parked, then close it if it is warm. co:op removes containers after each non-warm turn, or when a warm session times out or closes. If cleanup fails, it retries in the background. A parked state alone doesn't prove cleanup finished. |
| Incident is over | Close the session and keep its fork for review, or discard it explicitly later |
| New co:op build for the worker | Stop the worker and run `coop sessions check` with the new build before you swap it in. See [Upgrading a worker](#upgrading-a-worker). |
| Review patch preview is truncated | This response has no further patch pages. Don't treat a shortened preview as a complete diff. Publication uses the saved reviewed candidate, never a patch rebuilt from the preview. |

Stop the daemon before backing up the entire state directory. If you restore a database without
its matching forks, an operator must resolve the mismatch. co:op won't assign ownership based
only on directory names.

## Consumer responsibilities

When building a production controller, you are responsible for:

- deciding who can connect and which actions they may authorize
- reliably saving and delivering incoming and outgoing messages without processing duplicates
- assigning stable, globally unique idempotency keys
- saving event cursors and checking operations whose outcomes are uncertain
- preparing prompts and removing sensitive output before sending it through your chosen transport
- setting retention, audit, capacity and spending policies
- approving GitHub publication and supplying short-lived credentials for the authorized repository
- scanning for secrets before data leaves the host
- supervising the worker and providing a way to stop it

Never put chat-service tokens, GitHub credentials, Emisar administrative credentials or other
credentials that authorize publication into a co:op prompt or box.
