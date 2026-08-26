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
    max_turns: 100
    max_queued_turns: 20
    max_queued_bytes: 1048576
    turn_timeout: 1h
    warm_idle_timeout: 15m
    max_patch_bytes: 1048576
```

The parser rejects unknown fields and requires:

- `repository`: the absolute, canonical root of an existing Git worktree;
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
- `max_turns`: `1..10000`;
- `max_queued_turns`: `1..1000`;
- `max_queued_bytes`: `1..67108864`;
- `turn_timeout`: positive and no longer than 24 hours;
- `warm_idle_timeout`: optional, positive, and no longer than one hour;
- `max_patch_bytes`: `1..1048576`.

Every rung's credential must already be authenticated. A rung that omits `@credential` resolves to
that provider's current default when Coop loads the policy. Presets are not supported.

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
client needs to know: a rung on the same provider keeps the conversation, but a cross-provider hop
drops the native transcript, because the new provider cannot load the previous one's session. A
client that wants continuity across a hop re-seeds it from its own durable context.

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
cross-provider rewind clears the previous provider's native transcript. The field governs one
admission decision and cannot be combined with a positive `min_target_index`. Omitting it preserves
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
consumption, and tears down the child and run-labeled box before parking.

With `warm_idle_timeout`, Coop can prepare the authenticated ACP connection before the first turn
and reuse that exact process and native session across serialized turns. The daemon retains at most
20 warm sessions. Expiry, cancellation, failure, close, discard, or daemon shutdown stops the
process, removes its run-labeled box and services, and deletes projected credentials. Provider-native
history remains durable, so the next cold child can load the exact session again.

On restart, queued turns that were never sent remain eligible. A turn waiting for semantic
validation keeps its unpublished candidate and can still be accepted or rejected by digest. A turn
interrupted after send intent is terminalized as interrupted and is not silently replayed. Provider-declared projected credential
files left by a process crash are removed before workers start; provider-native history is retained.

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

To edit an existing GitHub pull request, the trusted caller may also send
`"pull_request":{"number":514,"head_commit":"<exact full commit>"}`. Coop derives
`refs/pull/514/head` through the policy's configured remote, requires it to equal the supplied
commit, starts the generated bound branch at that commit, and records the pull-request binding.
The caller cannot choose a repository, remote, or arbitrary ref. The session creation base is the
merge base with the policy branch so review covers the complete existing pull-request change.

With `Prefer: respond-async`, this endpoint returns only the public operation and status 202. The
operation's successful `resource_type` is `session`; `resource_id` is then safe to fetch through
`GET /v1/sessions/{session_id}`. Without that preference, it waits and returns the historical
operation-plus-session response.

| Method | Path | Body/query |
| --- | --- | --- |
| `POST` | `/v1/sessions` | `policy`, `task`, optional `pull_request.number` + `pull_request.head_commit` |
| `GET` | `/v1/sessions?limit=100` | `limit` is `1..1000` |
| `GET` | `/v1/sessions/{session_id}` | none |
| `POST` | `/v1/sessions/{session_id}/prepare` | `expected_revision`; policy must enable warm execution |

The public session includes IDs, target, policy digest, primary base commit, optional immutable
pull-request number/ref/head binding, companion aliases,
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
| `POST` | `/v1/sessions/{session_id}/turns` | `expected_revision`, `prompt`; optional `artifacts`, `min_target_index`, `rewind_target`, `output_contract` |
| `GET` | `/v1/sessions/{session_id}/turns?after=0&limit=100` | ordinal cursor |
| `GET` | `/v1/sessions/{session_id}/turns/{turn_id}` | none |
| `GET` | `/v1/sessions/{session_id}/turns/{turn_id}/artifacts/{artifact_id}` | raw generated image |
| `POST` | `/v1/sessions/{session_id}/turns/{turn_id}/cancel` | `expected_revision` |
| `POST` | `/v1/sessions/{session_id}/turns/{turn_id}/validation` | `candidate_sha256`, `verdict`; `accept` forbids `violations`, `reject` requires 1–20 |

Validation decisions do not carry `expected_revision`; `candidate_sha256` provides the optimistic
concurrency check against the exact unpublished candidate being accepted or rejected.

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

Public events contain identity, sequence, turn ID, type, version, timestamp, and the event's own
payload: what happened, never anything that names the host — not a repository path, not a native
session id, not a tool's results. Each payload is capped at 256 KiB and a page is bounded by total
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
```

Activity events narrate the interior of a turn — what the model did, as against what Coop decided —
and are always sequenced before the turn's own terminal event, so a caller that stops polling at
`turn.completed` has already seen them:

```text
tool.started            # tool_call_id, title, kind, input
tool.completed          # tool_call_id, title, kind, status
model.plan              # entries
model.thought           # text
permission.decided      # tool_call_id, title, outcome, option_id, option_kind
activity.elided         # dropped, reason
provider.backoff        # attempt, target, next_target, retry_after_seconds, reset_at, all_limited_until
provider.alive          # frames, bytes
```

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
- for an existing-PR session, immutable `pull_request_tree`, allowing callers to distinguish new
  content from empty commits or commit-and-revert history above the admitted PR head;
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
| `409` | idempotency, revision, state, queue, budget, resume, uncertainty, or discard conflict |
| `413` | ordinary request body exceeds 128 KiB, or turn submission exceeds 12 MiB |
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
crash or upgrade, and a parked-session sweep retries cleanup every minute without racing an active
turn. Explicit session discard remains responsible for deleting the workspace volumes and network.

A completed turn is completed even when that teardown fails: the janitor's retry is the guarantee,
and a slow container runtime must not convert a finished answer into an error. Cleanup failure is
logged with its bounded cause; a turn that itself failed carries the cleanup cause joined with its
  own error.
