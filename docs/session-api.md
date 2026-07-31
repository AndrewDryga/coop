# Local remote-session API

Coop can be driven by a trusted local service without a TTY or CLI-output parsing. The service
creates one Coop fork per session, submits persistent conversation turns, inspects changes, runs a
read-only review, and closes or explicitly discards the fork.

This is a generic local control plane. Slack, incident routing, authorization, audit storage,
GitHub publication, signing, merging, and deployment remain outside Coop. See
[External responder service](external-responder-service.md) for one consumer architecture.

## Boundary

`coop sessions serve` exposes HTTP/JSON over one Unix socket. It never listens on TCP.

```text
trusted local service
  -> owner-only Unix socket
  -> durable Coop session controller
  -> one Coop fork and private ACP state per session
  -> short-lived boxed ACP child for each turn
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
    companions:
      - name: coop
        repository: /srv/repos/coop
      - name: responder
        repository: /srv/repos/responder
    target: codex:gpt-5.6/medium@oncall
    max_turns: 100
    max_queued_turns: 20
    max_queued_bytes: 1048576
    turn_timeout: 1h
    max_patch_bytes: 1048576
```

The parser rejects unknown fields and requires:

- `repository`: the absolute, canonical root of an existing Git worktree;
- `companions`: at most 32 uniquely named absolute, canonical Git worktree roots; aliases use
  lowercase letters, numbers, hyphens, or underscores and cannot be `primary`;
- `target`: one ACP-capable target and at most one account;
- `max_turns`: `1..10000`;
- `max_queued_turns`: `1..1000`;
- `max_queued_bytes`: `1..67108864`;
- `turn_timeout`: positive and no longer than 24 hours;
- `max_patch_bytes`: `1..1048576`.

The selected account must already be authenticated. If the target omits `@account`, Coop resolves
and stores the operator's current default account when it loads the policy. Multi-provider presets
are not supported.

Repository, target, and limits are immutable session fields. `policy_digest` identifies those
resolved fields. Explicit non-secret box settings from the daemon's Coop configuration and the
repository's trusted box policy control the child. Raw runtime arguments, task queues, and merge
gates are not forwarded into a turn.

On session creation Coop pins each companion at its current commit and creates a detached, clean
snapshot worktree under the owner-private session state root. The agent sees only read-only mounts
at `/coop/repositories/<alias>` plus
`COOP_COMPANION_REPOSITORIES_JSON` containing aliases, in-box paths, and commits. The primary
repository remains the current working directory and is the only writable, reviewable tree.
Companion host paths are never returned by the API. Discard verifies and removes both the primary
fork and every owned companion snapshot.

The daemon's `mcp.json`, `env`, and `INSTRUCTIONS.md` are copied into private session state only
while a turn runs, then removed with the projected provider credential. This lets an operator run
the daemon under a dedicated least-privilege Coop configuration, for example an observe-only Emisar
MCP credential, without mounting the shared provider home. These files apply to every policy served
by that daemon; use a separate state root/socket and dedicated Coop configuration for a distinct
authority tier. API callers cannot replace or select them.

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
Bodies are limited to 128 KiB, must contain exactly one JSON value, and reject unknown fields.
Prompts are further limited to 64 KiB of valid UTF-8 without NUL.

Use a stable key for one logical action. Repeating the exact method, key, and canonical body returns
the recorded result without repeating the action. Reusing a key with a different method or body
returns `idempotency_conflict`.

After a lost response, retry the same request with the same key. If an earlier response supplied an
operation ID, `GET /v1/operations/{operation_id}` reports its state, error code, and resource ID.
Operation lookup deliberately omits the stored request, private result, prompts, native provider
IDs, and host paths.

State-changing requests carry `expected_revision`. Read the current session before acting and treat
`revision_conflict` as a request to reconcile, not as permission to guess a new action.

## Lifecycle

```text
create -> open/parked
          -> queued -> starting -> running -> completed|failed|cancelled|interrupted
          -> parked
          -> exhausted when the turn budget is consumed
          -> closed
          -> discard plan -> discarded
```

One worker owns a session. Turns are a bounded durable FIFO and only one runs at a time. Coop starts
one short-lived `coop fork <name> acp <target>` child for a turn, resumes the exact recorded native
session, records only its terminal assistant message for public consumption, and tears down the
child and run-labeled box before parking.

On restart, queued turns that were never sent remain eligible. A turn interrupted after send intent
is terminalized as interrupted and is not silently replayed. Provider-declared projected credential
files left by a process crash are removed before workers start; provider-native history is retained.

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

### Sessions

Create:

```bash
curl --unix-socket "$SOCKET" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: responder:create:01J...' \
  -d '{"policy":"emisar-observe","task":"incident/repository binding 01J..."}' \
  http://localhost/v1/sessions
```

The `task` is a bounded opaque external reference, not a shell command or authority-bearing
configuration.

| Method | Path | Body/query |
| --- | --- | --- |
| `POST` | `/v1/sessions` | `policy`, `task` |
| `GET` | `/v1/sessions?limit=100` | `limit` is `1..1000` |
| `GET` | `/v1/sessions/{session_id}` | none |

The public session includes IDs, target, policy digest, primary base commit, companion aliases,
in-box paths and pinned commits, generated fork name, revision, state, activity, queue/budget
counters, event cursor, and timestamps. It excludes host repository and workspace paths, native
session ID, prompts, credentials, environment, caller-defined mounts, and runtime data.

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
| `POST` | `/v1/sessions/{session_id}/turns` | `expected_revision`, `prompt` |
| `GET` | `/v1/sessions/{session_id}/turns?after=0&limit=100` | ordinal cursor |
| `GET` | `/v1/sessions/{session_id}/turns/{turn_id}` | none |
| `POST` | `/v1/sessions/{session_id}/turns/{turn_id}/cancel` | `expected_revision` |

A public turn excludes its prompt and idempotency data. A completed turn's `assistant_message` is
the user-facing response. Coop does not publish hidden reasoning, raw tool calls, raw ACP frames, or
box logs.

Cancellation asks ACP to cancel, then stops and reaps the exact process group and run-labeled box.
It does not claim that external side effects were reversed.

### Events

```bash
curl --unix-socket "$SOCKET" \
  'http://localhost/v1/sessions/remote_.../events?after=41&limit=100'
```

Events are returned as a JSON array ordered by monotonically increasing per-session `sequence`.
Persist the last processed sequence and request `after=<sequence>` after a disconnect.

Public events contain identity, sequence, turn ID, type, version, and timestamp. Payloads are
deliberately omitted. On `assistant.message` or a terminal turn event, fetch the referenced turn.
On a session/activity event, fetch the session. This prevents internal event payloads from becoming
an accidental public output channel.

Event types:

```text
session.created
session.state_changed
turn.queued
turn.started
activity.changed
assistant.message
turn.completed
turn.failed
turn.cancelled
turn.interrupted
budget.exhausted
session.parked
session.closed
workspace.discarded
```

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

- immutable `base_commit`, current `fork_head`, and current `parent_head`;
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

`publishable` is evidence about this exact candidate, not permission to push or merge. It is false
for conflict, no/failed gate, startup failure, policy findings, parent or source movement, active
fork ownership, or an unavailable/oversized complete artifact. An external publisher applies the
verified artifact to the exact `parent_head` in an isolated checkout and verifies that the resulting
tree equals `candidate_tree`. It must stop on any mismatch and owns all GitHub credentials,
branching, secret scans, commit creation, and draft-PR idempotency.

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

## Errors

Errors have one shape:

```json
{
  "error": {
    "code": "revision_conflict",
    "detail": "expected revision 4, current revision 5"
  }
}
```

Common status mapping:

| HTTP | Meaning |
| --- | --- |
| `400` | invalid or over-bounds request |
| `404` | session, turn, or operation not found |
| `409` | idempotency, revision, state, queue, budget, resume, uncertainty, or discard conflict |
| `413` | request body exceeds 128 KiB |
| `500` | internal failure; host paths and raw internal errors are suppressed |
| `503` | readiness endpoint is not ready |

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
| Socket reconnect | Resume event polling after the last committed sequence |
| Daemon already owns state | Stop or inspect that daemon; do not remove the lock |
| Stale socket after crash | Start the daemon with the same state root; it removes only a socket after acquiring the state lock |
| Queued turn at restart | Coop resumes it in FIFO order |
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
crash or upgrade, and a parked-session sweep retries cleanup every minute without racing an active
turn. Explicit session discard remains responsible for deleting the workspace volumes and network.
