# Local remote-session API

Coop can be driven by a trusted local service without a TTY or CLI-output parsing. The service
creates one Coop fork per session, submits persistent conversation turns, inspects changes, runs a
read-only review, and closes or explicitly discards the fork.

This is a generic local control plane. Slack, incident routing, authorization, audit storage,
GitHub publication, signing, merging, and deployment remain outside Coop.

## Boundary

`coop sessions serve` exposes HTTP/JSON over one Unix socket. It never listens on TCP.

```text
trusted local service
  -> owner-only Unix socket
  -> durable Coop session controller
  -> one Coop fork and private ACP state per session
  -> boxed ACP child, cold per turn or bounded warm by operator policy
```

The socket and state files are mode `0600`; their directories are mode `0700`. The daemon refuses
a symlink state root, socket, or socket parent and takes an exclusive lock on its state root. One
daemon owns a state root at a time.

This authorizes the Unix account, not an application identity. Any compromised process already
running as the Coop operator can normally read that user's repositories and state without using
this API. A hostile caller needs a separate OS identity and an authenticated broker. Do not expose
the socket through a TCP proxy or mount it into an untrusted container.

## Configure

The operator owns the policy file. A request selects a policy name; it cannot provide a repository,
command, host path, target, credential, mount, environment variable, image, MCP definition, egress
mode, or runtime argument.

Default path: `~/.config/coop/session-policies.yaml`

The policy file must be a real regular file owned by the daemon user (or root) and not writable by
group or world. Existing path components leading to it must be real directories, owned by the daemon
user (or root), and not writable by group or world. Coop rejects symlinks in the policy path.

```yaml
version: 1
policies:
  emisar-observe:
    repository: /srv/repos/emisar
    remote: origin
    branch: main
    companions:
      - name: coop
        repository: /srv/repos/coop
        remote: origin
        branch: main
      - name: responder
        repository: /srv/repos/responder
    target: [codex:gpt-5.6/medium@oncall, claude@oncall]
    project_env: false
    project_mcp: false
    repository_read_only: true
    egress:
      mode: filtered
      rules:
        - to: {domain: docs.example.com}
          protocol: tls
          ports: [443]
      export_destinations: false
    max_turns: 100
    max_queued_turns: 20
    max_queued_bytes: 1048576
    turn_timeout: 1h
    warm_idle_timeout: 15m
    max_patch_bytes: 1048576
```

The parser rejects unknown fields and requires:

- `mode`: optional execution mode, `normal` (the default, and every policy written before the
  key existed), `readonly`, or `bare`. It is fixed at creation, bound into both digests and
  persisted with the session, so an edit rotates new sessions and never changes an existing one.
  Both restricted modes run the box under the restricted filesystem profile of `coop <target>
  --readonly` / `--bare`: a read-only container root, run-private in-memory scratch discarded with
  the box, every host path read-only, nothing project-defined loaded, and the provider seeded with
  the access-only projection of its login. `readonly` pins and forks the repository exactly as a
  normal policy does and mounts the fork and its companions read-only (`repository_read_only` is
  implied). `bare` mounts nothing: it names no repository, remote, branch or companion, runs no
  Git and creates no workspace, and it implies `project_env: false` and `project_mcp: false`. The
  provider is started under the same switches the CLI proves — no repository or home extensions
  in either mode, and in `bare` no tool at all (`claude --tools ""` plus a stated no-tools system
  prompt) — sent on the ACP `session/new` the way the adapter reads them; only `claude` has a
  proven switch, so every rung of a restricted policy's ladder must be `claude`. Refused by name:
  a bare policy with any repository-shaped key, `repository_read_only`, `project_env: true` or
  `project_mcp: true`; a readonly policy without a repository; either with `warm_idle_timeout`
  (each turn is a fresh box) or `egress.mode: filtered` (the profile is not qualified under a
  gateway; a readonly policy whose project resolves to filtered is refused at load, and at create
  if the approval changed since). A restricted session keeps no provider history: every turn is a
  fresh native session, so a turn's prompt must carry whatever earlier context it needs, and
  `require_semantic_validation` is refused because there is no native session to re-prompt (a
  schema-invalid structured result is regenerated from the admitted prompt, up to the same three
  attempts). A bare session takes no `source` and no `responder_binding`, at create or on a
  turn. A legacy `repository_read_only: true` policy is not a readonly policy: it keeps its normal
  mode and its writable output root;
- `repository`: the absolute, canonical root of an existing Git worktree (omitted for `mode: bare`);
- `remote` and `branch`: optional, paired fields that make Coop fetch and pin the exact current
  remote branch commit without switching, pulling, resetting, or otherwise changing the local
  checkout; a refresh failure stops session creation rather than falling back to stale `HEAD`;
- `companions`: at most 32 uniquely named absolute, canonical Git worktree roots; aliases use
  lowercase letters, numbers, hyphens, or underscores and cannot be `primary`; each companion may
  configure its own paired `remote` and `branch`;
- `target`: an ACP-capable target, or a list of up to 4 of them forming a fallback ladder; each
  rung names at most one credential, and no rung may repeat another;
- `project_env` and `project_mcp`: optional booleans, defaulting to `true`, that let a policy omit
  the daemon's shared environment or MCP configuration while retaining the selected provider
  credential and trusted instructions;
- `repository_read_only`: optional boolean, defaulting to `false`. When true, the primary isolated
  fork is mounted read-only for every provider turn. Use it for conversation, watch, and
  investigation policies whose repository access is evidence-only; writable engineering policies
  must leave it false. The value is bound into the policy digest and persisted with the session, so
  changing the policy rotates rather than widening an existing session;
- `egress`: optional restricted-networking authority for this policy's sessions. `mode` is
  required when the block is present and is one of `open`, `filtered`, or `none`; `rules` uses the
  same grammar as a repository's `box.egress_rules` and requires `mode: filtered`;
  `export_destinations` defaults to `false`. The block is explicit operator authority: it is bound
  into both digests, resolved once when the session is created, and frozen on the session row, so a
  later edit applies to new sessions only. A policy with no `egress` block is not a request to
  widen access — the project's remembered posture still decides, exactly as it does for a direct
  launch in that repository. When the policy and the remembered posture disagree, creation is
  refused with `network_unavailable`; the API cannot reconcile that, an operator must
  (`coop approve`). Filtered creation also requires a completed `coop net setup` on the host
  and the repository's `box.egress_rules` to be inside its approved envelope. Beyond the rules you
  write, admission adds only what the session's own box will have: the selected targets' provider
  endpoints, and the HTTP hosts of the shared MCP configuration unless `project_mcp: false`
  withheld that file. Nothing a request carries becomes a grant, so a policy whose sessions call
  back to a Responder endpoint must allow that host itself — otherwise the callback is refused at
  the gateway like any other unlisted destination;
- `max_turns`: `1..10000`;
- `max_queued_turns`: `1..1000`;
- `max_queued_bytes`: `1..67108864`;
- `turn_timeout`: positive and no longer than 24 hours;
- `warm_idle_timeout`: optional, positive, and no longer than one hour;
- `max_patch_bytes`: `1..1048576`.

Every rung's credential must already be authenticated. A rung that omits `@credential` resolves to
that provider's current default when Coop loads the policy. Presets are not supported.

An optional top-level `storage:` block replaces the workspace-storage limits the daemon otherwise
derives from the measured volume:

```yaml
storage:
  reserve_bytes: 26843545600
  high_watermark_bytes: 510027366400
  low_watermark_bytes: 483183820800
  disposable_budget_bytes: 10737418240
  protected_budget_bytes: 483183820800
  grace_window: 15m
  measure_interval: 5m
  max_reclaim_per_pass: 4
```

Every field is required when the block is present. The reserve and the two watermarks are one
ordered policy — see [`GET /v1/storage`](#health) for what each one means — and a high watermark
written without a low one would defer an incoherent configuration to the first time the volume
filled up. `coop sessions serve` refuses to start on a block that cannot hold, and on numbers that
do not fit the volume it measures the daemon publishes no storage advertisement and reports the
mismatch in `/v1/storage`. Omit the block and the daemon keeps one reserve of 5% free, closing
allocation under it and reopening once two reserves are free.

## Target ladders

A `target` list is an ordered fallback ladder, and it may be cross-provider:

```yaml
    target: [codex:gpt-5.6-sol/xhigh@oncall, claude@oncall]
```

Sessions start on the first rung. When a provider rate limits an in-flight turn, Coop marks that
rung as cooling, moves the session to the next rung that is not, and delivers the same turn again —
so a usage limit costs a retry rather than the turn. Only a proven rate limit rotates: an
expired credential, a protocol error, or limit wording inside the model's own answer all surface
as the failure they are.

Sessions that survive a policy edit keep their fallback: the ladder applies whenever the
session's current target is one of the current policy's rungs, even when the rest of the policy
(and so its digest) has changed. Rotation only ever moves a session between rungs the operator
currently names, and only when the session already sits on one; a session whose rung was removed
keeps its pinned target and does not rotate. Teardown never requires the digest to match at all —
closing and discarding a drifted session works, with the dirty and unmerged guards intact, so a
policy edit cannot orphan the workspaces its old sessions own.

A rotation is durable. `target` on the session becomes the rung now in use, and a
`session.target_rotated` event carries `from`, `to`, and `native_session_reset`. The last is what a
client needs to know: model or effort changes on the same provider and credential account preserve
the native session. Changing either the provider or account clears the native-session binding,
because the new target cannot load the previous account's session. A client that wants continuity
across that reset re-seeds it from its own durable context.

### Starting above the first rung

One turn may name the rung it is delivered on:

```bash
curl --unix-socket "$SOCKET" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: responder:turn:01J...' \
  -d '{"expected_revision":4,"prompt":"Re-deliver the corrected answer.","min_target_index":1}' \
  http://localhost/v1/sessions/remote_.../turns
```

`min_target_index` is a zero-based index into the policy's `target` ladder, and it governs that one
turn: the turn is delivered no lower than that rung. A session already on that rung or above does
not move — it is a floor, not a seat assignment. Absent, or zero (which is rung zero), is the
ordinary turn, unchanged.

It is a floor for the whole turn, not a starting hint. Rotation continues upward from it exactly as
it would otherwise, and when every rung at or above it is cooling the turn fails with `rate_limited`
naming the rung rather than falling back below it. A corrected answer re-delivered by the model that
produced the answer being corrected would reach the client looking exactly like an honored
escalation, so the client owns that retry.

The move is durable in the same way a rotation is: `target` on the session becomes that rung, a
`session.target_rotated` event carries `from`, `to`, and `native_session_reset`, and later turns
start there unless the ladder moves again. Nothing is narrated as a backoff, because nothing was
throttled.

An index that names no rung of the session's current ladder is refused at admission with
`invalid_request`, naming how many rungs there are. A policy edited between admission and delivery
is caught at delivery instead, where the turn fails with `invalid_session_target`.

### Rewinding one turn to the first rung

An ordinary turn inherits the session's durable current target. That is normally the desired
continuity, but it cannot recover an escalated session when every provider at or above that rung is
limited and a lower rung is healthy. Such a retry can set `rewind_target: true`:

```bash
curl --unix-socket "$SOCKET" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: responder:turn:01J...:fallback' \
  -d '{"expected_revision":5,"prompt":"Continue on the healthy fallback.","rewind_target":true}' \
  http://localhost/v1/sessions/remote_.../turns
```

The turn starts on rung zero before any provider receives the prompt. The move becomes the
session's durable target and publishes the ordinary `session.target_rotated` event; a
rewind that changes provider or account clears the previous native-session binding. The field governs
one admission decision and cannot be combined with a positive `min_target_index`. Omitting it preserves
the existing behavior and request hash.

A rejection that is not a rate limit still fails the turn as `acp_protocol_error`, but its detail
now carries the adapter's own message — normalised to one bounded line — instead of a fixed
"ACP request was rejected" that named neither the cause nor the fix.

When every rung is cooling the turn fails with `rate_limited`, whose detail names the soonest
reset. Coop does not hold a queued turn waiting for one: the client owns retry and its own backoff.
Cooldowns live only in the running controller — a restart resumes on the session's stored rung and
re-probes the others.

Repository, target ladder, and limits are immutable session fields. `policy_digest` identifies those
resolved fields; a one-rung ladder digests exactly as the equivalent pre-ladder policy did. Explicit non-secret box settings from the daemon's Coop configuration and the
repository's trusted box policy control the child. Raw runtime arguments, task queues, and merge
gates are not forwarded into a turn.

`authority_digest` separately identifies the model-independent authority: repository source,
companions, project environment and MCP projection, repository write mode, and provider account
selection. It excludes policy name, model, reasoning effort, and resource budgets. Controllers may
therefore require conversational, standard, and deep policies to share one authority digest while
still pinning each policy's full `policy_digest`.

A create may pin either or both: `expected_policy_digest` and `expected_authority_digest` on
`POST /v1/sessions`. Admission compares each against the daemon's current resolution of the named
policy and refuses with `policy_digest_mismatch` (409) before any intent is journaled or a
workspace exists, so a daemon restarted with a changed same-name policy cannot run a create that
was authorized against the old one. A fleet worker forwards the digests its command was pinned to;
a direct client that pins nothing is unchanged.

Neither digest covers the network reach that policy text RESOLVES to on the serving host: the
project's remembered approval, the provider core bundles and the trusted MCP hosts all feed it, so
two daemons with identical policy files and different approvals advertise the same authority and
grant different access. The daemon therefore resolves each policy's effective network fingerprint
when it loads its policies and publishes it in `GET /v1/capabilities` and `coop sessions policies`.
A policy whose network it cannot resolve — nothing approved for its project, no completed `coop net
setup`, a rule this release cannot enforce — is refused at load with its reason rather than served
unfenced. Resolving writes nothing: no approval, no published snapshot, no owner key.

A create may pin that value as `expected_network_fingerprint`. The daemon resolves the policy again
— freshly, because an approval edited since it published the value is exactly what this catches —
and refuses with `network_fingerprint_mismatch` (409) before any intent is journaled or a workspace
exists. The refusal names the fingerprint the policy resolves to now and `coop approve` as the
thing that changed on the host. An open or offline policy publishes its mode and no fingerprint,
so pinning one for such a policy is itself a mismatch. The published value is resolved when you
ask for it, so approving a change on the host changes what callers are told without restarting the
daemon; a policy the host cannot resolve at that moment publishes its mode and no fingerprint.

On session creation Coop resolves all configured repositories concurrently. A repository with
`remote` and `branch` is pinned to that remote branch's exact commit; otherwise Coop preserves the
legacy local-`HEAD` behavior. Remote refresh imports only the immutable commit object and does not
move local branches, update tracking refs, write `FETCH_HEAD`, or touch working-tree changes. Coop
then creates each companion as a detached, clean snapshot with self-contained Git metadata under
the owner-private session state root. Repositories whose pinned history contains at most 1 GiB of
logical object data retain that complete reachable history. Above that bound—or when a partial
clone cannot prove its complete history is locally available—the companion becomes a one-commit
shallow snapshot containing the exact pinned commit and its complete tree. This keeps large-session
creation bounded by the checked-out revision instead of the source repository's lifetime while
preserving historical Git operations for smaller repositories. Selection and materialization do not
lazy-fetch from repository-configured promisor remotes: the trusted policy remote refresh must leave
the pinned commit and complete tree available locally, or session creation fails. The agent sees only
read-only mounts at `/coop/repositories/<alias>` plus
`COOP_COMPANION_REPOSITORIES_JSON` containing aliases, in-box paths, and commits. The primary
repository remains the current working directory and is the only writable, reviewable tree.
Companion creation and verification ignore host global and system Git configuration and host
attribute files, so repository attributes cannot invoke host-configured LFS, smudge, clean, or
process filters and operator attributes cannot rewrite checkout bytes. Verification computes status
with an isolated Git directory, config, and index backed only by the pinned companion objects.
Gitlink placeholders are checked directly and must remain real empty directories, avoiding
submodule config execution without weakening discard's modified-snapshot rejection.
Companion host paths are never returned by the API. Discard verifies and removes both the primary
fork and every owned companion snapshot.

Unless the session policy disables them, the daemon's `mcp.json` and `env` are copied into private
session state with `INSTRUCTIONS.md` only while a turn runs, then removed with the projected
provider credential. This lets an operator run
the daemon under a dedicated least-privilege Coop configuration, for example an observe-only Emisar
MCP credential, without mounting the shared provider home. These files apply to every policy served
by that daemon; use a separate state root/socket and dedicated Coop configuration for a distinct
authority tier. API callers cannot replace or select them. MCP uses the same canonical-path,
regular-file, 4 MiB, and ambiguity checks as an ordinary box; Coop captures it before changing
private session state, and an unsafe active authority fails the turn before the ACP child starts.

## Run

Run the daemon under a process supervisor:

```bash
coop sessions serve
```

Paths can be overridden:

```bash
coop sessions serve \
  --state /var/lib/coop-sessions \
  --policies /etc/coop/session-policies.yaml \
  --socket /var/lib/coop-sessions/control.sock
```

Defaults:

| Item | Path |
| --- | --- |
| State | `~/.local/state/coop/sessions` |
| Policy | `~/.config/coop/session-policies.yaml` |
| Socket | `<state>/control.sock` |

The process stays in the foreground and handles `SIGINT` and `SIGTERM`. Shutdown stops HTTP
admission, cancels workers, waits for their cleanup, closes the durable store, and removes only the
socket inode created by that process.

Check it:

```bash
coop sessions doctor
coop sessions doctor --json
coop sessions doctor --socket /var/lib/coop-sessions/control.sock
```

`doctor` exits nonzero when either `/healthz` or `/readyz` fails.

## Connect this machine to a remote controller

`coop sessions connect` connects this machine's local session service to a compatible fleet
controller. It makes outbound mutual-TLS HTTPS requests; it does not open an inbound TCP port. The
controller sends versioned commands, not arbitrary shell commands. Provider work still runs through
the local service and its trusted policies, never as a connector-local fallback.

```bash
coop sessions connect --config /etc/coop/worker.json
```

One command is enough: it validates the configuration first, then uses the ready local service, or
starts one when none is running. A service that is listening but not ready is not absent — its
socket is left alone and no second service is started. Ctrl-C stops the connection and, only if this
invocation created it, the service it started; a service that was already running is left alone.

Which local service a configuration is about comes from two optional fields, so autostart is never a
guess:

| Field | Default |
| --- | --- |
| `session_state_dir` | `~/.local/state/coop/sessions` |
| `session_policy_path` | `~/.config/coop/session-policies.yaml` |

`coop_socket` is still required and must sit inside the resolved `session_state_dir`: the socket is
the service's front door, and pointing it at a service that stores its sessions somewhere else would
connect a controller to a machine whose policies nobody checked. The state root is never inferred
from the socket's parent directory, and no session policy file is ever generated.

To run the local service on its own instead — for a supervisor that manages the two separately —
see [Run](#run) above; `coop sessions serve` is unchanged.

Export the policy digests from the same file the service loads:

```bash
coop sessions policies --policies /etc/coop/session-policies.yaml --json
```

Copy `policy_digests` and `policy_authority_digests` from that output into the worker configuration.
Keep the service's policies, worker advertisements and controller placement authority aligned. The
fleet operator supplies the worker/workspace identities, controller origin, trusted CA, enrollment
token and expected sandbox/repository advertisements. Do not derive those values from the example or
substitute a different digest that merely has the right length.

### Configure and enroll

Start with [worker.json](examples/worker.json), saving your completed configuration as
`/etc/coop/worker.json`. The example passes configuration validation, but its hostname, hashes,
policy name, paths and capacity are demonstration values, not deployment authority.

The format is strict JSON: no comments, unknown fields, trailing documents or environment-variable
expansion. Use literal absolute paths, not `~` or `$HOME` in JSON. In particular:

- `responder_url` is the controller's HTTPS origin, without an API endpoint, query or fragment.
  Serve its worker API routes directly, or rewrite internally at your proxy. The connector refuses
  HTTP redirects, including same-origin redirects; it does not forward credentials or transfer
  bytes to a redirect destination.
- `ca_file` contains exactly one trusted CA certificate in PEM format.
- `enrollment_token_file` is an owner-private regular file, at most 128 bytes. Its token must be
  32–128 bytes with no embedded whitespace. Obtain it through your controller's enrollment process;
  do not place the token in command arguments or the JSON file.
- `identity_file` starts absent. Do not precreate an empty file: the connector generates the key
  and saves its certificate bundle here during enrollment, with mode `0600`.
- `journal_dir` is persistent, owner-private storage, not a cache or disposable directory.
- `session_state_dir` and `session_policy_path` are optional and name the LOCAL session service this
  configuration is about, so `coop sessions connect` knows exactly which service it would start.
  Omitted, the documented defaults apply. `coop_socket` must resolve inside `session_state_dir`.
- `repositories`, `capabilities` and `capacity` are deployment advertisements. Populate them to
  match the controller's contract and actual worker resources. Coop only advertises its
  implementation capabilities — `repository-freshness` version `2` and
  `repository-source-selector` version `1` — after the running local daemon proves each one in
  `GET /v1/capabilities`; putting either in JSON cannot override that check, and a configured
  claim is removed. The two are versioned independently, so a partially upgraded fleet advertises
  only what each daemon actually supports and a controller can withhold source-selecting work
  from workers that do not have it.

Keep the configuration and enrollment file private (`0600`) and their containing directories
owned by the daemon/connector user. That user must be able to create the identity and journal,
and remove the enrollment token after use. Do not copy another worker's identity or journal.

A `create_session` command may carry a `source` selector beside its policy and digests
(`{"kind":"default"}`, `{"kind":"branch","name":…}`, `{"kind":"pull_request","number":…}` with an
optional `expected_head_commit`, or `{"kind":"commit","sha":…}`). The connector validates its
shape and bounds, forwards it unchanged to the daemon — which owns the authority decision — and
refuses a malformed one with `invalid_command` before any daemon call. A create fence carries the
same request, selector included, so it occupies the create's exact ledger identity. See
the endpoint reference below for what each selector resolves to.

Run it under your process supervisor:

```bash
coop sessions connect --config /etc/coop/worker.json
```

The first poll enrolls using the supplied token, saves the worker-owned identity, consumes the
local token file, then polls with its client certificate. Successful polls are quiet. Verify
enrollment and worker eligibility in your controller, not just by checking that the process lives.

`poll_interval_ms` must be 100–60000 and `request_timeout_ms` 100–300000. The example polls each
second with a 30-second request timeout. `renew_before_seconds` is 60–86400; omitted or zero
defaults to one hour. Choose a renewal window shorter than your controller's certificate lifetime.

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

The worker also reclaims fork storage it can prove is garbage: its own generation record, no session
naming it, no reservation, no live worker or sandbox activity, older than the reclaim age, and a
clean tree fully contained by its parent. Everything else — a dirty workspace, an unmerged branch, a
directory with no coop generation record — is reported in `/v1/storage` and left alone. An
interrupted removal is resumable: the workspace is renamed into an owner-private staging directory
before any deletion, and no discard reports success until those bytes are physically gone.

### Restart and recovery

Stop the connector with `SIGINT` or `SIGTERM`. Preserve the identity file and the entire journal
directory across restarts, together with the same worker/workspace configuration. A restart loads
the existing identity rather than enrolling again; certificate renewal is automatic over mutual TLS.

Commands are journaled before execution. Unacknowledged terminal results are resent after restart.
Redelivery of a command with a recorded result reuses its receipt rather than executing it again;
reusing that command ID with a different payload is refused. An asynchronous
session creation can have its result acknowledged while creation is still running: the journal
retains its original operation identity until session activity can be bound. Event cursors advance
only after exact acknowledgements, so unacknowledged events replay and acknowledged events do not.
Do not prune journal files or keep only the `commands` directory when moving or backing up a worker.

If a review outlives its request, its uncertain transport receipt stays unchanged.
The existing `reconcile_operation` command returns the saved public operation/review
envelope once that exact review succeeds; pending or failed operations still return
their operation metadata. This reads the completed result and never reruns the gate.
The daemon and connector must both support completed-review lookup.

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
never carries a credential, a host path, a packet body, or — unless the session policy set
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

The API cannot merge, sign, push, publish a pull request, mutate the parent ref, run an arbitrary
host command, or return a host workspace path.

## Endpoints

### Health

| Method | Path | Result |
| --- | --- | --- |
| `GET` | `/healthz` | `{"healthy":true}` |
| `GET` | `/readyz` | `{"ready":true}` after controller startup |
| `GET` | `/v1/capabilities` | `{"repository_freshness_receipt_versions":[2],"session_evidence_versions":[1],"policies":{"<name>":{"mode":"filtered","fingerprint":"<64 hex>"}}}` for caller-side protocol negotiation and network placement |
| `GET` | `/v1/storage` | this daemon's own workspace-storage accounting: `storage` (the object a fleet controller reads), `budget`, `totals`, `roots`, `forks` and `problems` |

The `policies` map is each served policy's network reach as this daemon resolved it against this
host; an open or offline policy reports its mode with no fingerprint. It is published because a
caller cannot compute it — host approval feeds it — and a create pins it as
`expected_network_fingerprint`.

`/v1/storage` measures allocated blocks, never apparent size, and it never opens a file — so it can
account credential-bearing private session state without reading any of it. A fork's git objects are
hardlinked from the checkout it was cloned from, so every total is the storage that would actually
be returned by removing that content, with the multiply-linked baseline reported once in
`totals.baseline_shared_bytes` instead of charged to each fork. A measurement that could not see
everything reports `totals.unknown` and publishes `storage.unattributed_bytes` as `null`: unknown is
not zero. A full tree walk is too expensive for every caller, so the answer is re-measured at most
once per `budget.measure_seconds` and `storage.measured_at` carries its age.

`forks[]` names each directory under a fork root, its category, and the evidence for it. Only
`disposable`, `owned_orphan` and `staged_discard` are storage anything may reclaim; `active`,
`grace`, `protected` (a running worker, registered sandbox activity, an interrupted land, canonical
task authority, a quarantined session, or uncommitted work), `control` and `unattributed` are all
refusals with a reason. A directory with no coop generation record is `unattributed`: it is
reported, and it is never deleted on a guess.

Under storage pressure the daemon refuses a NEW workspace with `storage_unavailable` and names its
cause. It does not refuse anything else: recovery of a workspace that already exists, cleanup,
discard and every read keep working, and nothing protected is deleted to make room. The refusal
closes at `storage.high_watermark_bytes` of USED space and reopens only under
`storage.low_watermark_bytes`, so reclaiming one workspace cannot flap it open and shut.
`storage.reserve_bytes` is the free-space floor underneath both, and new work never spends it.
None of this bounds what an already-running task writes inside its own workspace.

The outbound worker connector reports `repository-freshness` capability version `2`,
`repository-source-selector` version `1` and `session-evidence` version `1` only after the session
daemon on its configured Unix socket returns the matching versions in `GET /v1/capabilities`
(`repository_freshness_receipt_versions`, `repository_source_selector_versions`,
`session_evidence_versions`). The three are
versioned independently, so a daemon that resolves freshness but not source selectors advertises
only the first and receives no selector-bound work, and a control plane can tell a worker that
does not export inspection evidence from a session that genuinely observed nothing. The connector removes any configured claim to
either and drops the advertised capability again if live proof is unavailable. Responder therefore
negotiates the exact worker and daemon currently serving a placed session during rolling upgrades.

### Sessions

Create:

```bash
curl --unix-socket "$SOCKET" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: responder:create:01J...' \
  -H 'Prefer: respond-async' \
  -d '{"policy":"emisar-observe","task":"incident/repository binding 01J..."}' \
  http://localhost/v1/sessions
```

The `task` is a bounded opaque external reference, not a shell command or authority-bearing
configuration.

#### Selecting a source

A create may name which source INSIDE the policy's already-authorized repository the session
starts from, with one bounded `source` selector:

```json
{"kind":"default"}
{"kind":"branch","name":"feature/payments"}
{"kind":"pull_request","number":514}
{"kind":"commit","sha":"0123456789abcdef0123456789abcdef01234567"}
```

Omitting `source` means `{"kind":"default"}`. The caller cannot name a repository, filesystem
path, remote, URL or raw ref: Coop derives every ref from the operator policy plus that one value.
A branch name is validated with Git's own ref rules and derives only `refs/heads/<name>`; a pull
request number derives only `refs/pull/<number>/head`; a commit must be a complete lowercase
40- or 64-character object id. Trusted ingress may add `expected_head_commit` to a pull-request
selector as host-owned evidence of the head it observed — a push racing session creation then
fails closed instead of silently changing the approved source. A policy with no configured
`remote` is intentionally local: it keeps local semantics for its own default and refuses
`branch`, `pull_request` and `commit`.

For a non-default selection Coop pins the configured default branch head AND the selected head,
uses their merge base as the session's creation base, and starts the generated bound branch at the
selected commit — so review covers the complete inherited change and the baseline always
represents divergence from the configured default branch. A source with no common history is
refused with `invalid_request` before any workspace exists, rather than reviewed against an
invented ancestor.

A branch, pull request or default selection is proven with `git ls-remote --exit-code --refs` on
its exact derived ref, followed by an object fetch only when the cache lacks it. An exact commit
has no advertised ref, so the remote is contacted on every create; note that `git fetch <remote>
<oid>` answers success WITHOUT contacting the server when the object is already in the local
object database, so a commit that is already cached is additionally anchored to the remote's
current advertisement of `refs/heads/*` and `refs/pull/*/head`. A commit that exists only in the
local cache is therefore refused. Hosted Git must serve object ids that are not ref tips
(`uploadpack.allowReachableSHA1InWant`, which GitHub sets) for commit selection to resolve
anything below a tip; a server that refuses fails the create closed.

The create response and the public session carry the immutable version-1 binding:

```json
{
  "version": 1,
  "kind": "branch",
  "requested": {"kind": "branch", "name": "feature/payments"},
  "remote_identity": "origin",
  "default_ref": "refs/heads/main",
  "default_commit": "<full object id>",
  "selected_ref": "refs/heads/feature/payments",
  "selected_commit": "<full object id>",
  "base_commit": "<merge base>",
  "admitted_tree": "<tree object id>",
  "resolved_at": "<UTC timestamp>"
}
```

`selected_ref` is `null` for an exact commit, which advertises none; for the default source the
selected and default identities are equal. A pull-request binding adds `pull_request_number` and,
when the caller supplied one, `pull_request_expected_head`. The binding never carries a remote URL
or credential. It is resolved and journaled into the durable create intent BEFORE the workspace is
created, so a lost response, a daemon restart, or the same idempotency key replays the exact
commits instead of landing on a branch that moved in between; the same key with a DIFFERENT
selector is a different request and conflicts.

With `Prefer: respond-async`, this endpoint returns only the public operation and status 202. The
operation's successful `resource_type` is `session`; `resource_id` is then safe to fetch through
`GET /v1/sessions/{session_id}`. Without that preference, it waits and returns the historical
operation-plus-session response.

| Method | Path | Body/query |
| --- | --- | --- |
| `POST` | `/v1/sessions` | `policy`, `task`, optional `source`, optional `expected_policy_digest` / `expected_authority_digest` / `expected_network_fingerprint` |
| `GET` | `/v1/sessions?limit=100` | `limit` is `1..1000` |
| `GET` | `/v1/sessions/{session_id}` | none |
| `POST` | `/v1/sessions/{session_id}/prepare` | `expected_revision`; policy must enable warm execution |

The public session includes IDs, target, policy digest, its execution `mode` (`normal`,
`readonly` or `bare`; a session created before modes existed reads `normal`), the exact
`project_env`, `project_mcp`, and
`repository_read_only` authority flags, its frozen `network` posture (`{"mode":"filtered",
"fingerprint":"<64 hex>"}`, or just `{"mode":"open"}`), primary base commit, its immutable `source`
binding, companion aliases, and one version-2 repository freshness receipt per
configured alias. Each receipt contains the requested revision, immutable fetched revision,
sanitized remote identity, UTC fetch time, and stale-base status. The primary receipt also carries
`workspace_base_revision`: normally the fetched base head, or the exact merge base for a
non-default selected source. A non-default selection adds one receipt named `source`; a default
selection needs none, because the `primary` receipt already proves the same ref and object. Legacy sessions expose explicit unavailable freshness instead; a caller requiring current
source evidence must fail closed or replace that session rather than infer freshness. The remaining
session fields include
in-box paths and pinned commits, generated fork name, revision, state, activity, queue/budget
counters, event cursor, and timestamps. It excludes host repository and workspace paths, native
session ID, prompts, credentials, environment, caller-defined mounts, and runtime data.

A `bare` session has no workspace: `fork_name` and `base_commit` are empty, its freshness is
`unavailable` because there is no repository to be fresh about, and every repository-specific
operation — `GET .../changes`, `POST .../checkpoint`, `POST .../workspace`,
`POST .../workspace/restore`, and `POST .../review` — refuses it with `invalid_session_state`
(409) and `session has no workspace: its policy is bare` (review answers with its own bound-fork
refusal). Turns, events, budget, cancel, close, and the discard plan/discard pair work as for any
session; a bare discard removes the session's private state and retires the record, since there is
no fork to remove.

### Turns

Submit:

```bash
curl --unix-socket "$SOCKET" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: responder:turn:01J...' \
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
before the turn completes, so generated charts do not appear as repository changes.

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
session policy set `egress.export_destinations: true`.

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
  -H 'Idempotency-Key: responder:extend:01J...' \
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
bounded page. `patch_limit` cannot exceed the session policy's `max_patch_bytes`; offsets are
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

`projection` states the disclosure scope. It is `destinations-withheld` unless the session policy
set `egress.export_destinations: true`, in which case it is `destinations-included` and the rule
texts and observed names are present. The daemon owns that projection; the outbound worker forwards
exactly what the daemon answered, through the four read commands `get_network`,
`get_network_connections`, `get_network_explanation` and `get_network_receipt` — each one a plain
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
  -H 'Idempotency-Key: responder:review:01J...' \
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
- `patch_truncated`, complete `patch_digest` and `patch_bytes`, an opaque
  `patch_artifact_id`, `publishable`, and stable not-publishable reason codes.

The preview is base64 in JSON. A truncated preview is a transport condition and does not by itself
make the review unpublishable. The complete owner-private artifact is capped at 64 MiB and is
available only through
`GET /v1/operations/{patch_artifact_id}/review-patch`. The response is raw `text/x-diff`, with the
review digest as its ETag. Consumers must verify its size and SHA-256 digest before use.

Reviews may outlive the requesting connection. Look up the original operation key,
then retrieve a succeeded review with
`GET /v1/sessions/{session_id}/reviews/{operation_id}`. It returns the same public
`operation` and `review` envelope without starting or resuming a gate. Another
session, a non-review operation, and an unfinished review are refused. Both
`policy_findings` and `not_publishable_reasons` are arrays, including when empty.

`publishable` is evidence about this exact candidate, not permission to push or merge. It is false
for conflict, no/failed gate, startup failure, policy findings, parent or source movement, active
fork ownership, or an unavailable/oversized complete artifact. An external publisher applies the
verified artifact to the exact `parent_head` in an isolated checkout and verifies that the resulting
tree equals `candidate_tree`. It must stop on any mismatch and owns all GitHub credentials,
branching, secret scans, commit creation, and draft-PR idempotency. For an existing pull request,
the publisher must also compare-and-swap the recorded repository/ref at the recorded head before
pushing and verify that the same pull request points to the resulting exact commit afterward.

### Close and discard

Close:

```bash
curl --unix-socket "$SOCKET" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: responder:close:01J...' \
  -d '{"expected_revision":15}' \
  http://localhost/v1/sessions/remote_.../close
```

Close requires no active or queued turns and preserves the fork.

Plan a discard only after close:

```bash
curl --unix-socket "$SOCKET" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: responder:discard-plan:01J...' \
  -d '{"expected_revision":16}' \
  http://localhost/v1/sessions/remote_.../discard-plan
```

The plan captures exact session revision, workspace inode, branch, head, status digest, running
state, dirty state, and unmerged state. By default dirty or unmerged work is refused. A caller that
has separate authority to destroy it must explicitly set `accept_dirty` and/or `accept_unmerged`
when creating the plan.

Execute that exact plan:

```bash
curl --unix-socket "$SOCKET" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: responder:discard:01J...' \
  -d '{"plan_operation_id":"op_..."}' \
  http://localhost/v1/sessions/remote_.../discard
```

Discard first proves that the plan belongs to the path session. It then compares all captured state,
refuses a stale/replaced/running workspace, deletes the fork workspace and private ACP state, and
leaves a durable discarded session tombstone. A failed comparison does not delete anything.

A session the daemon quarantined at start — a record from before fork generations were persisted,
or one whose generation record or workspace is gone — can be neither planned nor discarded this
way, because Coop cannot prove it owns the workspace. Retire the record instead:

```bash
curl --unix-socket "$SOCKET" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: responder:retire:01J...' \
  -d '{"retire_quarantined":true,"expected_revision":16}' \
  http://localhost/v1/sessions/remote_.../discard
```

This tombstones the session row only: queued turns are exhausted, a turn that was active when the
daemon lost authority stays in history as it was, and the workspace, sidecar services, and private
ACP state stay on disk for the operator to inspect and remove. A session that is not quarantined
is refused with `invalid_session_state`.

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
| `409` | idempotency, operation fence, revision, state, queue, budget, resume, uncertainty, discard, policy digest, or network fingerprint conflict |
| `413` | ordinary request body exceeds 128 KiB, or turn submission exceeds 12 MiB |
| `500` | internal failure; host paths and raw internal errors are suppressed |
| `503` | readiness is not ready, or repository/runtime/network/storage authority is temporarily unavailable |

`storage_unavailable` is a retryable `503`: this worker's volume is under its configured pressure
limits, so it will not create another workspace until space is returned or, when the cause is
`protected storage exceeds budget`, until the protected data itself goes. Read `/v1/storage` to see
which bytes are held and why. Place the session on another worker or clear the storage; do not
retry in a tight loop and do not delete protected workspaces to satisfy the quota.

`network_unavailable` is a `503` an operator has to clear, not a retryable one: the host has no
approval for the project, no completed `coop net setup`, or the named policy disagrees with the
project's remembered posture. Creation refuses rather than falling back to an open session. A
create that pinned `expected_network_fingerprint` reports it for the same reason: a pin nobody can
confirm is not a pin.

`network_fingerprint_mismatch` is a `409` with the same fix: the policy still resolves, but to a
different reach than the caller was authorized against, because an approval on this host changed.
Nothing was journaled, so the caller re-reads the fingerprint (from the refusal, or from
`coop sessions policies`) and places again.

Treat `operation_uncertain` and `turn.interrupted` as reconciliation states. Never retry a mutation
under a new key merely because its result is unknown.

`RunReview` is the narrow read-only exception: after Coop captures immutable source, parent, and
policy identities, replaying the exact request under the same key resumes that frozen review under
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
| Other running operation at restart | Coop marks it `operation_uncertain`; reconcile rather than replaying under a new key |
| Operation remains running after startup | The periodic watchdog resumes safe creates and marks stale ambiguous mutations uncertain |
| Turn interrupted after send intent | Surface interrupted; do not submit the same human input automatically |
| Active turn must stop | Use the idempotent cancel endpoint, then reconcile the terminal turn |
| Session should stop costing runtime | Wait for park; no agent or Compose service container remains between turns |
| Incident is over | Close; retain the fork for review or an explicit later discard |
| Review patch is truncated | Do not publish; reduce/split the change or use a separate human host workflow |

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
- GitHub or other publication machinery;
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
