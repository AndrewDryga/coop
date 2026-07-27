# External incident responder service

Status: reference architecture, 2026-07-26

This document preserves the product and security design for building a Slack-based incident
responder on top of Coop. It is deliberately not a Coop feature specification. Slack, incident
lifecycle, Emisar-specific behavior, organization identity, and GitHub publication belong in a
separate service or in Emisar.

The implemented Coop boundary is documented in
[Local remote-session API](session-api.md). It exposes a local, transport-neutral session/fork API
and contains no Slack or incident concepts.

## Executive decision

Build three layers with separate authority:

```text
Slack and incident system
        |
        v
External responder service or Emisar
  - incident and channel lifecycle
  - Slack identity and authorization
  - durable Slack inbox/outbox
  - prompt framing and response rendering
  - Emisar response mode and approvals
  - GitHub draft PR publication
        |
        v
Generic Coop session API
  - execution policy
  - isolated fork and box
  - durable conversation turns
  - agent resume, cancellation, and parking
  - changes, review identities, and bounded binary patch
```

Coop never receives Slack tokens, channel IDs as authority, incident semantics, GitHub credentials,
or permission to merge. The responder never starts an agent directly, chooses arbitrary mounts or
credentials, or treats the box as a trusted control plane.

This is a logical API/authority separation, not a same-host Unix privilege boundary in v1. A
responder running as Coop's Unix user can bypass an owner-only socket and read that user's files if
compromised. Treat that process as a trusted local caller, sandbox it from unrelated filesystem and
process access where practical, and do not claim malicious-caller isolation until a distinct-UID
authenticated boundary or outbound worker protocol exists.

## Job to be done

For every declared incident:

1. Create or discover one dedicated Slack incident channel.
2. Create one canonical investigation thread in that channel.
3. Start one isolated Coop session/fork for each selected repository.
4. Let authorized responders hold a real-time, multi-turn conversation with the agent for hours or
   days without keeping an agent box alive while idle.
5. Surface only bounded status, questions or terminal responses, approval notices, review evidence,
   and draft PR links.
6. Let the agent investigate through Emisar under explicit response authority.
7. Preserve exact evidence, code changes, and operation IDs through restarts and ambiguous network
   outcomes.
8. Close the conversation without deleting work. Destructive cleanup is a later, separately
   authorized retention operation.

The first product slice is manual, observe-only incident investigation in an existing channel. It
does not need automatic channel creation, production mutation, or automatic PR publication to prove
the conversational and isolation model.

## Ownership matrix

| Concern | Owner |
| --- | --- |
| Slack app installation, tokens, scopes, Socket Mode | Responder/Emisar |
| Slack workspace, channel, thread, user identity | Responder/Emisar |
| Incident declaration, deduplication, severity, lifecycle | Incident system/Emisar |
| Operator and approver authorization | Responder/Emisar |
| Durable Slack input, output, actions, retries | Responder/Emisar |
| Trusted responder task frame and output policy | Responder/Emisar |
| Repository allowlist and execution policy | Coop operator |
| Fork, box, provider session, turn queue, budgets | Coop |
| Git diff, gate, review identities, bounded patch | Coop |
| Fleet identity, policy, approval, execution, audit | Emisar |
| GitHub App, branch push, draft PR | Responder/Emisar |
| Commit signing, merge, landing | Human/host workflow |

## Deployment choices

### Standalone responder beside Coop

Best for the first internal deployment:

- One service runs as a separate supervised process under the restricted Coop operator identity
  because Coop v1's `0600` socket authorizes that same UID.
- It connects to Slack through outbound Socket Mode.
- It calls Coop through the owner-only local Unix socket.
- It stores incident/Slack state in PostgreSQL, or SQLite only for a deliberately single-host
  prototype.
- Slack and GitHub secrets stay in its secret store, not in Coop or the box.

This is the shortest path to validating user experience and the box/model boundary. The service is
trusted with the local Coop account: process hardening such as a read-only filesystem view, explicit
path allowlist, no-new-privileges, and systemd sandboxing reduces accidental exposure but does not
turn a shared UID into a strong security boundary. A dedicated OS identity requires a later
peer-credential/token-authorized proxy or Coop transport design and must not be implied by v1.

### Emisar-owned control plane

Best if AI SRE becomes an Emisar product:

- Emisar owns Slack OAuth installations, tenant identity, incident records, approvals, audit links,
  and GitHub App installations.
- A customer-side responder worker connects outbound to Emisar and invokes the local Coop socket.
- The worker never accepts public inbound traffic and never exposes the Coop socket remotely.
- Emisar sends bounded, authenticated work envelopes referencing a preconfigured Coop execution
  policy. The worker returns typed events and reviewed artifacts.

This adds multi-tenancy, remote worker enrollment, delivery, and artifact transport. Those are
Emisar/service features, not additions to Coop's v1 local session API.

### Recommendation

Start standalone and keep the service contract compatible with later Emisar ownership. Do not put
Slack code in Coop as an intermediate shortcut. Do not expose Coop's local API on TCP merely to
avoid building an authenticated outbound worker later.

## Canonical identities

Never use a Slack timestamp, channel name, fork name, or human title as a global identity.

```text
tenant_id
slack_installation_id
slack_enterprise_id          nullable; Enterprise org installs rejected in v1
slack_team_id
slack_channel_id
slack_root_thread_ts
incident_source
incident_source_id
incident_id
coop_session_id
coop_external_ref
coop_turn_id
coop_event_sequence
emisar_operation_id
github_installation_id
github_repository_id
github_pull_request_id
```

The durable mapping is:

```text
incident_id
  -> (team_id, channel_id, canonical_root_ts)
  -> one or more (repository_id, Coop session_id, fork/review state)
  -> submitted turns and rendered Slack messages
  -> Emisar operations and approvals
  -> GitHub publications
```

Slack thread identity is `(slack_installation_id, channel_id, root_ts)`, never `thread_ts` alone.
`slack_installation_id` is non-null and already binds tenant, optional enterprise, and team.
V1 accepts workspace installations only and rejects organization-wide Enterprise Grid installs
until enterprise/team routing and authorization are explicitly implemented.

For one repository, one incident occurrence/repository-binding version maps to one Coop session/fork.
A multi-repository incident maps to multiple Coop sessions coordinated by one external incident
conversation. V1 must not mount several repositories into one ambiguous fork or let concurrent
repository agents all narrate into Slack without an external coordinator.

Reopening never reopens a closed Coop session. The responder appends a new repository-binding
version and creates a new session/fork from the then-current allowed base, while preserving the prior
binding/session/fork as immutable history. If retained unpublished work may still matter, reopening
enters `blocked` until an operator decides whether to publish it separately, summarize it as
untrusted evidence for the new session, or abandon it. The new session never silently adopts or
overwrites the old provider history.

## Incident lifecycle

### Alert is not incident

Do not create a Slack channel or Coop session for every alert. Alert storms, repeated evaluations,
symptom alerts, and correlated downstream failures would create unbounded channels and boxes while
appearing to provide coverage.

An incident source of truth must first deduplicate, correlate, and promote alerts into a stable
declared `incident_id`. Suitable owners include:

- Emisar, if it gains a first-class incident record.
- incident.io or Rootly.
- PagerDuty or another incident system with a durable incident identity.
- A small standalone incident controller for an internal deployment.

Grafana or Alertmanager posting to Slack is useful evidence delivery, but it is not an incident
lifecycle or channel-per-incident contract.

### Source incident and responder states

```text
source_incident_state        provider value plus normalized declared | active | mitigated |
                             resolved | closed | cancelled | unknown
source_version/event_id      exact provider ordering and reconciliation cursor

responder_workflow_state     unbound | provisioning_channel | provisioning_session |
                             investigating | waiting_operator | waiting_emisar_approval |
                             reviewing | ready_for_review | publishing_draft |
                             parked | blocked | failed | closed
response_authority           investigate | request_containment
```

The incident system remains authoritative for incident lifecycle. The responder workflow records
only what this automation is doing and never writes back or infers `contained`, `resolved`, or
`closed` from an agent response. Mapping provider states to normalized states is versioned and
reconciled against the exact source record.

### Incident-source delivery

Automatic incident intake needs its own durable authenticated boundary; Slack Socket Mode only
covers Slack events.

- Prefer an incident-provider webhook delivered to the Emisar/control-plane ingress. Verify the
  signature over the raw body, tenant/installation, timestamp/replay window, event type, and size
  before admission.
- Persist the provider delivery and request digest before acknowledging a webhook. Deduplicate by
  `(tenant_id, source, source_event_id)` and retain provider ordering/version metadata.
- If the provider offers only polling or a durable event stream, persist a per-tenant cursor and
  advance it only in the same transaction that records admitted events.
- A standalone outbound-only worker must poll a provider API or receive authenticated work from the
  control plane. Do not quietly add an unauthenticated public endpoint beside Coop.
- Reconciliation periodically fetches exact provider incident IDs/versions and repairs missed,
  duplicated, reordered, reopened, resolved, and deleted records.

### Channel and session reconciliation

Handle each case idempotently:

- Incident declaration delivered twice.
- Channel already created but response was lost.
- Canonical thread posted but binding was not stored.
- Channel creation succeeds but bot invite fails.
- Coop session creation times out after the fork may have been created.
- Incident is renamed without changing its ID.
- Channel is renamed, archived, unarchived, or converted.
- Incident reopens after the Coop session was closed.
- Incident is cancelled or deleted.
- Orphan channel exists without a session.
- Orphan Coop session exists without a channel binding.

Creation recovers through the incident provider and Coop idempotency records. It never retries an
unknown mutation using a new key.

### Channel creation authority

Prefer the existing incident system as channel owner. This avoids granting the responder app broad
`channels:manage` or `groups:write` scopes.

If the responder must create channels itself:

- Separate channel-lifecycle authority from the conversational bot token where practical.
- Allow only one configured workspace and naming policy initially.
- Make public/private policy explicit.
- Invite a configured responder group and the bot deterministically.
- Persist channel ID before subsequent actions.
- Reconcile `name_taken`, permission failures, rate limits, and ambiguous timeouts.
- Never infer channel creation authority from a model response.

## Slack conversation model

### One canonical thread

Each incident channel contains one bot-created canonical investigation thread. The channel remains
available for human coordination, lifecycle bots, timelines, and unrelated work.

Only these enter the agent conversation:

- The initial trusted incident frame assembled by the responder.
- Authorized human replies inside the canonical thread.
- Explicitly selected, allowlisted alert updates wrapped as untrusted evidence.

Do not forward all channel traffic. Do not ingest arbitrary bot posts, entire history, reactions,
edits, files, unfurls, or links by default.

Deterministic command results such as status, changes, review, close, and publication remain in the
Slack/UI and audit transcript; they are not silently added to model context. When a human wants the
agent to act on one, the service constructs a new bounded instruction containing only validated
identifiers/enums plus explicitly labeled untrusted text.

### Summon behavior

For the manual MVP:

1. An authorized operator mentions the bot in an allowed channel.
2. The normalized admitted top-level Slack message record creates a durable synthetic manual
   incident UUID. The service's first bot reply becomes the canonical root, bound idempotently to
   `(installation_id, channel_id, message_ts)` and that UUID. The human summon is preserved as the
   first authorized input but is not itself used as a mutable identity.
3. The service creates the Coop session idempotently from the durable incident/repository binding.
4. Follow-up replies inside that thread need no mention.
5. Messages outside the canonical thread are ignored unless they are a new explicit summon.
6. A summon is "the same incident" only when it is inside the bound canonical thread or carries an
   exact authorized incident-provider/manual UUID selected through deterministic UI. Otherwise it
   creates a distinct manual incident; title/text similarity never merges incidents.

For declared incidents, the incident lifecycle creates the binding; no mention is required to start
the initial observe-only investigation.

### Multiple responders

- Every accepted message records the Slack user ID and mapped organization identity.
- One Coop turn runs at a time.
- Later replies are placed in a visible queue rather than cancelling the active turn.
- The service may debounce consecutive messages from the same operator for a short configured
  window before submission. Coop itself never coalesces turns.
- Queue position and capacity are visible.
- A responder can stop the active turn without discarding queued instructions unless the command
  explicitly says so.

### Edits and deletions

- Editing a message that has not been admitted updates the queued instruction if revision and
  identity still match.
- Editing an already admitted message does not rewrite history or replay the turn. The bot asks for
  a new corrective reply.
- Deleting a message marks the rendered record deleted but does not pretend an admitted instruction
  or side effect never happened.
- Bot/system edits of status messages are outbox operations and never become new input.

### Deterministic commands

Commands are parsed and executed by the service, never sent to the model. Text commands match the
entire trimmed message, case-sensitively, after Slack mention-token normalization:

```text
!coop status
!coop stop
!coop extend <positive integer minutes>
!coop changes
!coop review
!coop close
!coop publish <exact Coop review ID>
```

The parser is anchored; substrings, prose such as "please close", quoted/code-block text, edits, and
natural-language approximations are not commands. An invalid message beginning with `!coop` fails
closed as a control error rather than entering model context.

`status`, `changes`, and `review` may execute after authorization. `stop` binds the active turn ID at
action-record creation. `extend` binds the requested amount and current policy cap. `close` and
`publish` only open a deterministic confirmation action; they do not execute from text alone. The
confirmation record binds actor, incident, session revision, queue state, expiry, and nonce.
Publication additionally binds the exact review ID, candidate base/head/tree, patch digest,
`publishable: true`, and current GitHub publication state.

`discard`, `merge`, `sign`, arbitrary shell, policy changes, credential changes, and break-glass
operations are absent from Slack in the initial product.

Every command reauthorizes the actor and is bound to tenant, workspace, channel, thread, incident,
session, expected state/revision, expiry, and a one-time action ID.

## Slack app

### Connection

Use Socket Mode:

- Outbound WebSocket; no public Events API endpoint.
- App-level token has only `connections:write`.
- Reconnect with bounded exponential backoff and jitter.
- Handle Slack disconnect warnings and refresh requests.
- Maintain health state separately from agent/session health.
- Socket Mode removes ingress requirements; it does not provide application authorization,
  durable delivery, or idempotency.

### Initial bot scopes

Exact Slack capabilities change, so verify the manifest against current Slack documentation before
implementation. The expected narrow set is:

- `app_mentions:read`
- `chat:write`
- `channels:history` for public incident channels the app can access
- `groups:history` for private incident channels the app is explicitly invited to
- `commands` only if a slash command is actually shipped
- `assistant:write` only if Slack's native assistant status/thread APIs are used
- `files:read` only after safe attachment ingest is implemented
- `files:write` only if long safe reports are uploaded rather than linked elsewhere
- `users:read` only if required for identity display/reconciliation

Do not request scopes for future features. In particular, omit channel-management, broad search,
usergroup mutation, direct-message history, admin, and workspace-wide file scopes unless a shipped
and reviewed feature requires them.

### Events

Expected subscriptions:

- `app_mention`
- `message.channels`
- `message.groups`

Slack may deliver one mentioned message through both `app_mention` and `message.*` with different
event IDs. `app_mention` exclusively owns summon admission. Ignore `message.channels`/
`message.groups` events for a message containing the responder mention, and enforce a normalized
business uniqueness key `(installation_id, channel_id, message_ts, human_message)` before routing.
Event-ID/envelope dedupe remains necessary for redelivery of each transport event.

Filter before routing:

- Ignore the responder's own bot ID.
- Ignore message subtypes unless explicitly supported.
- Ignore bot messages by default.
- Allow trusted lifecycle/alert bots only by exact app/bot IDs and only as untrusted evidence.
- Ignore messages outside allowed workspaces/channels and the canonical thread.
- Deny Slack Connect/external channels and guest/external users in v1.

### Slack AI UI

Slack-native status is an optional presentation improvement. The durable contract remains ordinary
stored messages and Coop events; v1 posts one terminal response rather than token streaming. If
assistant APIs are unavailable or restricted by plan/surface, fall back to one posted status message
updated in place and one terminal thread reply.

Do not expose tool titles merely because the Slack API offers a status field. Use a small controlled
status vocabulary.

## Durable Slack delivery

Socket Mode and the Events API are at-least-once/best-effort delivery surfaces. The service must
provide the durable boundary absent from the audited bridges.

### Inbound path

1. Receive Slack envelope.
2. Validate installation, tenant, event type, IDs, size, and basic timestamp bounds.
3. Begin database transaction.
4. Insert the unique envelope/event/action record or load its existing result.
5. Commit the inbox record.
6. ACK the Socket Mode envelope.
7. A worker leases the inbox item and performs authorization/routing.
8. Reserve the Coop operation/turn using a deterministic idempotency key.
9. Persist the resulting binding before marking the inbox item complete.

If the database is unavailable, do not ACK and silently drop. Let Slack retry. Keep the
persist-before-ACK transaction short enough for Slack's acknowledgement window.

Useful unique boundaries:

```text
(slack_installation_id, envelope_id)
(slack_installation_id, event_id)
(slack_installation_id, channel_id, message_ts, normalized_message_kind)
(slack_installation_id, service_action_nonce)
(tenant_id, incident_source, incident_source_id)
(slack_installation_id, channel_id, canonical_root_ts)
(coop_daemon_id, globally_unique_coop_idempotency_key)
(coop_session_id, coop_event_sequence)
(emisar_credential_lineage, emisar_operation_id)
(github_installation_id, repository_id, publication_key)
```

`envelope_id` deduplicates Socket Mode delivery. Interactive Slack payloads do not provide a generic
`action_payload_id`; the service embeds a cryptographically random one-time nonce in `actions.value`,
stores only its digest, and binds it to actor, tenant, thread, resource state, and expiry.

### Coop admission

Derive stable opaque Coop values:

```text
external_ref    responder incident/repository binding ID
create key      responder:create:<binding UUID>
turn key        responder:turn:<turn submission UUID>
cancel key      responder:cancel:<action UUID>
review key      responder:review:<action UUID>
close key       responder:close:<action UUID>
```

For debounced input, freeze the `turn_submission` first: commit its ordered relay-input membership,
canonical request digest, and UUID, then reserve the Coop operation from that UUID. A crash before
freeze may continue collection; a crash after freeze cannot add/remove inputs and always recovers
the same Coop key/request.

If a Coop request times out, replay the exact HTTP mutation with the same idempotency key. That
returns the recorded result without repeating the work. If the responder already received the Coop
operation ID, it may first inspect `GET /v1/operations/{operation_id}`. Do not retry with a new key.
A returned `operation_uncertain` is a visible blocked/reconciliation state, not permission to
guess.

### Outbound path

1. Consume Coop events by durable cursor.
2. Transform only allowed event types into a sanitized render model.
3. Commit an outbox item, intended Slack target, content digest, and source event sequence.
4. Send or update Slack.
5. Persist Slack message timestamp and terminal result.

On an ambiguous Slack post/update timeout, reconcile the known thread and prior outbox state before
retrying. Slack does not provide a general exactly-once posting guarantee. Use a verified
Slack-supported opaque message metadata marker when the selected API/event surface preserves and
returns it. A content digest or text/time search is not proof that a particular outbox item was
posted. If exact correlation is unavailable, leave the item `uncertain` for operator/reconciler
decision instead of automatically risking a duplicate. Never drop the source event from the durable
audit.

Respect Slack `Retry-After`, isolate poison outbox items, and prevent one failed channel from
blocking unrelated incidents.

### Ordering

- One worker owns each incident binding at a time.
- Coop event sequence determines source order.
- Outbox ordering is preserved per canonical thread, not globally.
- A terminal message cannot appear before its preceding status/approval state is committed.
- Reconnect or leader changes resume from durable cursors.

## External data model

### Installation

```text
id/tenant_id/enterprise_id/team_id
bot_user_id/app_id
encrypted token references
installed_by/installed_at
scopes/manifest_version
state                       active | disabled | revoked | uninstalling
last_socket_health
```

### Incident-source event

```text
id/tenant_id/source/source_event_id
source_incident_id/source_version/source_occurred_at
request_digest/signature_identity
payload_ciphertext_ref
state                       received | leased | applied | duplicate | rejected | failed
attempt_count/lease_owner/lease_expires_at/next_attempt_at
received/acked/applied_at
```

The inbox record is committed before webhook acknowledgement or polling-cursor advancement.

### Incident binding

```text
id/tenant_id
incident_source/source_id
occurrence/version
source_incident_state/source_version/source_event_id
normalized_incident_state/severity
responder_workflow_state/response_authority
enterprise_id/team_id/channel_id/root_ts
responder_profile
repository bindings
created/updated/resolved/closed_at
last_reconciled_at
```

### Repository binding

```text
id/incident_binding_id
version/supersedes_repository_binding_id
repository_id
coop_execution_policy
coop_external_ref/session_id
session_state/last_event_sequence
latest_review_id/source_head/source_tree/candidate_head/candidate_tree
github_publication_id
```

### Relay input

```text
id/installation_id
envelope_id/event_id/action_id
team/channel/root/message timestamps
slack_user_id/mapped_operator_id
kind
payload_digest
authorization_result
state                       received | leased | submitted | completed | rejected | failed
received/acked/processed_at
```

Store the minimum Slack content required for operation and audit, encrypted where appropriate, with
an explicit retention policy.

### Turn submission

```text
id/repository_binding_id
coop_session_id
request_digest/globally_unique_idempotency_key
state                       collecting | reserved | submitted | completed | uncertain | failed
attempt_count/lease_owner/lease_expires_at
coop_operation_id/turn_id
created/submitted/completed_at
```

### Turn input membership

```text
turn_submission_id/relay_input_id
input_ordinal
admitted_message_revision
```

This many-to-one ledger makes debounce/coalescing deterministic. A Slack edit can replace content
only while the submission remains `collecting`; after reservation it becomes a new corrective input.

### Coop operation

```text
id/repository_binding_id/coop_daemon_id
kind                        create | turn | cancel | extend | review | close |
                            discard_plan | discard
request_digest/globally_unique_idempotency_key
expected_revision/expected_head
state                       reserved | submitted | succeeded | failed | uncertain
coop_resource_type/resource_id
attempt_count/lease_owner/lease_expires_at/next_attempt_at
response_digest/error_code
created/submitted/resolved_at
```

Every Coop mutation, including automatic lifecycle work without a Slack message, gets a row. The
responder recovers by replaying the exact request with the same key and records the returned Coop
operation ID for later state lookup.

### Outbox

```text
id/incident_binding_id
source_type/source_id/source_sequence
render_type
target_channel/root_ts
render_schema_version/content_digest
encrypted_render_payload_ref
correlation_marker_digest
slack_message_ts
state                       pending | sending | sent | uncertain | failed
attempt_count/next_attempt_at
created/sent_at
```

`encrypted_render_payload_ref` points to the exact bounded sanitized Slack API payload committed
before delivery. Retries replay those bytes rather than re-rendering with changed templates,
sanitizers, or expired source events. Retain it through terminal outbox reconciliation and the
configured audit window; its digest/schema version remain after payload expiry.

### Operator action

```text
id/tenant/workspace/channel/thread
incident/session/turn/review/publication IDs
actor IDs
action/digest
expected_state/revision/head
expires_at
decided_at/result
one_time_nonce_digest
```

Button visibility is never authority. The service reloads and validates this record and the current
actor/state before action.

### Emisar operation/approval

```text
id/incident_binding_id/repository_binding_id
emisar_account_id/credential_lineage
operation_id/run_id/approval_id
runner_ref/pack_ref/action_ref/target_digest
request_digest/response_authority
state                       proposed | pending_approval | running | succeeded |
                            denied | expired | failed | uncertain
attempt_count/lease_owner/lease_expires_at/next_reconcile_at
approval_url/expires_at
result_digest/error_code
created/updated/resolved_at
```

### GitHub publication

```text
id/repository_binding_id
installation_id/repository_id/coop_review_id
candidate_base/head/tree/patch_digest
request_digest/publication_key
branch_name/expected_remote_head/pull_request_id
state                       reserved | importing | pushing | creating_pr |
                            draft_ready | uncertain | blocked | failed
attempt_count/lease_owner/lease_expires_at/next_reconcile_at
result_digest/error_code
created/updated/published_at
```

Each ledger has an immutable request digest, stable provider/resource IDs, bounded attempts, a
crash-recoverable lease, explicit `uncertain`, and its own reconciliation cursor. Do not infer one
system's success from another system's message.

## Authorization and identity

### Defaults

- Empty workspace allowlist means deny all.
- Empty channel allowlist means deny all.
- Empty operator allowlist means deny all.
- Workspace/channel membership is not authorization.
- Bot messages are denied unless exact bot/app ID and message purpose are configured.
- Slack Connect, guests, external members, and unmapped identities are denied initially.

### Per-action authorization

Reauthorize:

- initial summon;
- every conversational reply;
- status, stop, extend, changes, review, close, and publish;
- every button/menu/modal submission;
- Emisar approval/denial;
- incident containment mode changes;
- GitHub publication.

Authorization evaluates tenant, workspace, channel, incident role, mapped organization identity,
response mode, action, resource state, expiry, and separation-of-duty rules.

### Slack-to-Emisar identity

Slack user IDs are not Emisar identities. Establish a durable enrollment/link:

```text
tenant_id
slack_team_id/slack_user_id
emisar_account_id/user_id
verified_at/by
state/revoked_at
```

For MVP, post a deep link to the authenticated Emisar approval page. The user completes approval
under Emisar's existing identity and policy. Slack buttons are a later feature requiring server-side
identity revalidation, anti-replay binding, exact action digests, expiry, and complete audit.

## Prompt and untrusted data

### Trusted frame

The external responder preset owns:

- job and recovery criteria;
- response authority mode;
- incident/repository binding;
- required evidence/uncertainty format;
- allowed human boundaries;
- output policy;
- source-controlled fix expectations;
- instruction that Slack, alert, log, tool, repository, and web content are data, not instructions.

The preset selects a Coop execution policy by name. It cannot override the policy's repository,
credentials, MCP, image, egress, mounts, network, or caps.

### Input construction

Do not concatenate an alert directly as the agent's top-level task. Use a fixed shape:

```text
Trusted task:
<responder-owned incident task and authority>

Incident metadata:
<validated server-generated IDs, normalized enums, and timestamps only>

Untrusted incident fields:
<title, description, service display name, labels, annotations, URLs, and provider summaries>

Untrusted Slack message:
actor_ref: <opaque mapped reference>
message_ts: <timestamp>
content:
<exact bounded user text>

Untrusted alert evidence:
source: <allowlisted source>
observed_at: <timestamp>
content:
<bounded payload>
```

Delimiters and prompt wording do not eliminate prompt injection. Security comes from fixed
execution policy, least privilege, domain policy, human boundaries, and controlled egress.
Only IDs whose provenance is verified, normalized enums, and validated timestamps belong in trusted
structure. Provider-controlled display strings remain untrusted even when they arrived from the
incident API rather than Slack.

### History

- Coop preserves the agent's native conversation.
- The service does not fetch entire Slack channel history.
- New operators receive a bounded service-generated incident summary rather than silently injecting
  all old chat.
- Message edits/deletes are represented as later facts, not history rewrites.
- Sensitive Slack content is minimized before submission. Deleting the responder's copy does not
  erase text already persisted in Coop's prompt record, provider-native history, filesystem/backups,
  or an external model provider. Retention must describe those copies separately. Do not promise
  end-to-end deletion until Coop/provider-session erasure and provider data-retention contracts
  exist and have been verified.

### Attachments

Attachments are disabled in the initial MVP. When added:

1. Reauthorize the message and channel.
2. Fetch the file in the trusted responder service.
3. Never include a Slack bot bearer token or authorization header in a Coop prompt or review.
4. Apply size, MIME, archive-depth, malware, content, and decompression limits.
5. Store a content-addressed sanitized artifact in the responder's approved store.
6. Submit only a bounded text extraction to Coop, labeled as untrusted data. Coop v1 has no generic
   attachment/artifact endpoint and never accepts a host path or arbitrary URL.
7. Redact/log only metadata.

Do not automatically fetch model-generated or user-posted URLs. Disable Slack link unfurling on
model-generated output to prevent URL-based exfiltration.

## User-visible output

### Allowed

- One coalesced controlled status message.
- Terminal assistant response or question for each turn.
- Queue and budget state changes.
- Existing Emisar approval notice and authenticated link.
- Bounded changes/review summary.
- Draft PR or retained review link.
- Clear failure, blocked, parked, exhausted, and closed notices.

### Suppressed

- Hidden reasoning or chain of thought.
- Raw ACP frames.
- Raw tool names, arguments, output, or execution transcripts.
- Complete prompts or system instructions.
- Environment variables, credentials, cookies, headers, or tokens.
- Raw box stdout/stderr and build logs.
- Unbounded telemetry or diffs.

### Rendering

- Post the one terminal Coop assistant response at turn end. Safe controlled activity events may
  update one status message; raw ACP/model deltas are not an external rendering contract.
- The final stored assistant message is authoritative.
- Escape unintended Slack mentions, especially `@channel`, `@here`, and user/group mentions.
- Disable link unfurls.
- Sanitize Markdown and code blocks.
- Enforce message length. Put a redacted long report in an approved artifact store and link it rather
  than splitting raw output into message spam.
- Include evidence references and uncertainty, not internal reasoning.
- Never run a model-based "is this a question?" classifier. A terminal assistant message may end in
  a question; the next authorized human reply starts another turn.

### Status vocabulary

```text
provisioning
queued
investigating
waiting_on_operator
waiting_on_emisar_approval
reviewing
ready_for_review
publishing_draft
parked
budget_exhausted
blocked
failed
closed
```

Status must reflect durable service/Coop state. Silence must not look like coverage.

## Emisar integration

### Initial authority

Start in `investigate` mode:

- An observe-scoped Emisar identity is preconfigured in the Coop execution policy or delivered
  through an approved broker.
- The external caller never submits the raw key.
- The agent follows the incident responder skill and Emisar's returned continuations.
- Observational remote actions still pass Emisar trust, policy, signing, runner validation,
  redaction, and audit.
- No direct SSH, cloud CLI, copied credentials, or fallback shell is introduced by the Slack layer.

### Response modes

```text
investigate     observation only
contain         explicitly delegated reversible mitigation in exact scope
break_glass     separate exact time-bounded human procedure
deploy          human-owned permanent-fix deployment
```

Slack conversation does not silently expand the mode. A user asking a question during investigation
does not authorize containment. A contain request must be explicit, mapped to an authorized
operator, and bounded to incident scope.

`response_authority` is service workflow state, not a way to mutate an existing Coop session's
immutable execution policy or credentials. Preferred design: the observe/request-scoped box may
submit a bounded action proposal, while Emisar holds all execution authority server-side and runs
only the exact human-approved operation; the box credential never becomes a mutation credential. If
Emisar cannot support that brokered proposal contract, create a separate explicitly authorized Coop
session under a distinct containment policy and hand it a bounded evidence summary. Never replace
credentials or widen MCP authority inside the investigation session.

### Approvals

- Emisar policy is authoritative.
- The agent cannot approve its own action.
- Coop's fixed ACP sandbox acknowledgement is unrelated to production authorization.
- A `pending_approval` result is stored with immutable Emisar account, operation, run, action/pack/
  runner refs, target digest, risk/policy reason, expiry, and approval URL.
- MVP posts the Emisar deep link, lets the current agent turn return a terminal pending notice, and
  parks the Coop box. A responder worker monitors/reconciles the same Emisar operation/run without
  consuming an agent turn. When it reaches an authoritative terminal state, the service submits one
  new idempotent Coop turn with validated IDs/status enums and any untrusted result text labeled as
  evidence.
- Denial stops that path. It is not a reason to use a different credential, new operation ID, wider
  target, SSH, or another tool.
- Later Slack approval buttons call an Emisar operator endpoint only after reauthorizing mapped
  identity and refetching the unchanged pending request.

### Idempotency

Slack/relay idempotency and Emisar operation idempotency are distinct:

```text
Slack event/action ID
  -> responder inbox and Coop turn idempotency
  -> agent chooses an Emisar action
  -> Emisar operation_id governs that remote mutation/recovery
```

An ambiguous Emisar mutation is recovered through the same operation ID. Never repeat the action
because the Slack or Coop turn was interrupted.

### Evidence

Every incident conclusion should preserve:

- incident ID and response mode;
- exact observed timestamps;
- runner, pack, action, run, approval, and operation IDs;
- relevant deployment/commit IDs;
- facts, hypotheses, contradicting evidence, and confidence;
- recovery criteria;
- temporary action and rollback;
- source-controlled fix and verification status.

Slack shows the bounded useful subset. Emisar and the incident record retain authoritative execution
and audit details.

## GitHub draft PR publication

### Authority

Publication belongs to the responder/Emisar service, never Coop or the box.

- Use a repository-scoped GitHub App.
- Mint short-lived installation tokens in the publisher.
- Minimum practical permissions normally include Contents write to push the branch and Pull requests
  write to create/update a PR. There is no true "PR-only PAT" that can push code without repository
  content authority.
- Never put the GitHub token, App private key, or installation token in the Coop session, box
  environment, prompt, event, artifact, or logs.
- Always publish a draft.
- Never approve, merge, sign, delete branches, or bypass branch protection.

### Input

Publish only from an immutable Coop review:

- exact allowed repository identity;
- creation base and current target base;
- source fork head/tree and the separately reviewed candidate head/tree;
- Coop review `publishable: true` with no not-publishable reasons;
- green configured gate against the exact candidate;
- clean committed state;
- policy/risk findings;
- secret scan;
- the complete non-truncated binary patch returned inline by Coop, with a responder-owned digest.

Coop v1 returns that patch as base64 bytes in the review JSON; it does not expose a bundle or generic
artifact endpoint. The responder stores the exact reviewed bytes and digest. The publisher checks
out the exact `parent_head` in an isolated checkout, applies the binary patch, and verifies that the
resulting tree equals `candidate_tree` before creating a new publication commit and pushing with the
GitHub App. It does not read arbitrary host paths. A conflict, failed/no gate, startup error,
truncated/missing patch, apply failure, or identity mismatch blocks Slack-triggered publication. A
human may use a separate established host workflow for an exceptional ungated review; the responder
does not encode a hidden no-gate bypass.

### Publication idempotency

Use a durable publication key:

```text
(GitHub installation, repository ID, Coop review ID)
```

Handle:

- push succeeded but response was lost;
- branch exists;
- PR created but response was lost;
- draft PR already exists;
- target base moved;
- patch/base/tree mismatch;
- branch changed by a human;
- previous publication of an older review;
- rate limit or GitHub outage.

Use a deterministic safe branch prefix plus opaque binding ID. Never force-push over human changes.
If the remote branch no longer matches the recorded published head, stop or create a new versioned
branch. If the target base moved after review, request a new Coop review/candidate; the publisher
does not rebase or claim the old gate covers a new base.

### PR content

The draft PR includes:

- incident link/ID and bounded impact;
- evidence-based root cause or current hypothesis;
- exact reviewed commit and tree;
- tests/gate result;
- known risks and unresolved questions;
- temporary mitigation and rollback when relevant;
- deployment and verification still owned by humans;
- generated-content disclosure where required.

Secret-scan and redact the title, body, commits, diff, and attached artifacts. A green test gate does
not prove a diff is safe to publish.

### Signing

Box commits are unsigned by design. Publishing them for review does not claim host signing.
Organizations must choose one landing policy:

- review unsigned commits, then host-side Coop signing before landing;
- squash/recreate a signed landing commit through the normal protected workflow;
- use a separately designed attestation/publication identity.

The responder does not silently re-sign, rewrite, or merge.

## Credentials and network

### Responder service

May hold, through a real secret manager:

- Slack app/bot tokens;
- database credentials;
- GitHub App private key/installation authority;
- Emisar control-plane service credentials needed for tenant/incident integration.

Tokens are encrypted at rest where applicable, rotated, never logged, and invalidated on Slack app
uninstall or tenant disconnect.

### Coop host

Owns:

- LLM/provider credential accounts;
- execution-policy mapping;
- allowed repository paths;
- box image and runtime policy;
- MCP configuration and an observe-scoped Emisar identity where configured.

The Coop daemon may read the operator's source account configuration, but each remote session gets
private provider-native and ACP state. Coop's provider adapter projects only declared read-only
configuration and per-turn ephemeral authentication. The shared provider/account home, interactive
history, and other incidents' session directories are never mounted into the box.

### Box

Receives only what the selected Coop policy deliberately projects:

- one repository fork;
- one session-private provider/ACP state root and one ephemeral provider identity projection;
- the configured MCP connection;
- bounded network/resource policy.

It receives no Slack token, GitHub credential, responder database credential, Docker socket, host
signing key, or incident-management administrative credential.

## Audit, privacy, and retention

Slack is a user interface, not the audit system of record.

The responder stores:

- who instructed what;
- authorization decision;
- incident/channel/session/turn bindings;
- deterministic command/action;
- Coop operation and event references;
- Emisar operation/run/approval references;
- rendered outbound digest and Slack message timestamp;
- publication/review identities and outcome.

Minimize and encrypt raw message content. Define retention separately for:

- Slack message bodies;
- normalized incident record;
- Coop prompts/provider session history;
- assistant responses;
- diffs/review artifacts;
- audit metadata;
- GitHub publications.

Closing an incident does not immediately delete evidence. Retention cleanup is an explicit
reconciled background policy. Coop close preserves the fork; later discard uses Coop's exact-state
plan/confirmation and never serves as an error-recovery shortcut.

An append-only application log improves reconstruction but is not tamper-proof merely because it is
append-only. Emisar remains authoritative for its operations; GitHub remains authoritative for
published refs; Coop remains authoritative for its fork/review evidence.

## Capacity, budgets, and backpressure

Apply independent caps:

- per tenant/workspace;
- per incident channel;
- per repository/execution policy;
- total active Coop sessions;
- total running turns;
- queued turns and bytes;
- turns/session;
- per-turn and total active wall time;
- provider/API spend where observable;
- idle time before close;
- retained forks/artifacts.

When capacity is exhausted:

- persist the incident/request;
- post a visible `queued`, `holding`, or `budget_exhausted` state;
- expose queue position when stable;
- allow an authorized `extend` only within organizational and Coop policy caps;
- do not silently drop or imply an investigation is running;
- do not create extra sessions to bypass a limit.

Budget extension changes the responder record and invokes Coop's idempotent `ExtendBudget`.
Authorization to chat does not automatically include authority to increase spend.

## Failure and recovery

| Failure | Required behavior |
| --- | --- |
| Slack disconnect | Reconnect with backoff; process durable inbox/outbox; no new agent prompt solely because of reconnect |
| Duplicate/out-of-order Slack event | Unique inbox record; route once; retain source ordering metadata |
| Database unavailable before ACK | Do not ACK; let Slack retry |
| Crash after ACK | Inbox already durable; worker resumes |
| Coop create/turn timeout | Look up the same idempotent operation; never use a new key |
| Coop daemon restart during queued turn | Replay durable queued turn through Coop recovery |
| Coop restart at/after prompt send intent | Mark interrupted/cleanup-required; do not replay uncertain prompt |
| Agent/provider crash | Surface failed/interrupted; preserve fork and events |
| Cancellation timeout | Coop cleanup-required; block new turns and deletion |
| Emisar unknown mutation result | Recover by the same Emisar operation ID |
| Emisar pending approval | Post authoritative link; park Coop turn; reconcile same run/operation externally; submit one new result turn |
| Emisar denial | Stop that path; no credential/tool bypass |
| Slack post timeout | Use verified opaque correlation if available; otherwise leave uncertain for explicit reconciliation; preserve source event |
| Slack 429 | Honor `Retry-After`; do not block unrelated channels |
| Git push/PR timeout | Reconcile branch/PR by publication key and exact SHA before retry |
| Channel archived/removed | Stop rendering; preserve session; reconcile or require operator decision |
| Incident closes with active turn | Request explicit stop or wait; then close preserving fork |
| Service restored from backup | Reconcile Slack, Coop operations/sessions, Emisar operations, and GitHub refs before workers resume |

## Security threats and controls

| Threat | Control |
| --- | --- |
| Prompt injection in Slack/alert/log | Fixed trusted frame, labeled untrusted data, least privilege, controlled egress |
| Malicious bot loops | Ignore own bot, deny bots by default, exact allowlisted app IDs |
| Unauthorized responder | Default-deny IDs and per-message authorization |
| Unauthorized button click | Reauthorize actor and bind nonce to exact tenant/thread/session/action/state/expiry |
| Slack Connect/external user | Deny in v1 |
| Slack token exfiltration | Token exists only in responder; never box/prompt/artifact |
| GitHub credential exfiltration | Short-lived App token only in publisher |
| Agent escapes to host | Coop box; no runtime socket, host key, or caller-selected mount/env |
| Cross-incident context | One Coop session/fork per incident/repository; exact canonical thread mapping |
| Cross-incident provider transcript | Session-private provider/ACP roots; no shared account home; canary isolation tests |
| Compromised same-UID responder | Treat as trusted v1 caller; process sandboxing is defense in depth; distinct-UID authenticated worker boundary required for hostile isolation |
| Duplicate turn after timeout | Durable responder and Coop idempotency lookup |
| Replay of production action | Emisar operation recovery; no blind resend |
| Model approves its own action | Emisar server-side policy/approval; agent cannot decide |
| Destructive cleanup | Close preserves; discard requires exact short-lived Coop plan |
| Secret in final response/diff/PR | Structured suppression, known-secret redaction, domain scan before Slack/publication |
| URL-based exfiltration | No automatic fetch; disabled Slack unfurls; domain policy |
| Poisoned repository hooks/config | Coop hardened Git/review operations and box isolation |
| Thought/tool leakage | Coop public event contract and responder renderer structurally omit them |

## Operations

### Health and readiness

Expose separately:

- database/schema readiness;
- Slack socket connected/refreshing/disconnected;
- inbox and outbox worker health/lag;
- Coop socket health and policy availability;
- incident reconciler lag;
- Emisar API/MCP health;
- GitHub publisher health;
- active/parked/cleanup-required sessions;
- pending approvals and oldest age.

The service may be live while not ready to admit new incidents.

### Metrics

Content-free metrics:

- Slack envelopes received, duplicates, ACK latency, reconnects, and rate limits;
- inbox/outbox depth, age, attempts, uncertain results, and poison items;
- incidents by state;
- session create/resume/close latency and errors;
- queued/running/interrupted turns;
- Coop event cursor lag;
- budget usage/exhaustion/extensions;
- unauthorized/expired/stale actions;
- Emisar run/approval outcomes and age by non-sensitive class;
- review/gate/publication outcomes;
- orphan bindings and reconciliation repairs.

Never label metrics with message text, prompts, tool output, tokens, repository paths containing
sensitive tenant data, or high-cardinality raw IDs.

### Logs

Structured logs include opaque IDs, state transitions, timings, retry classes, and error codes.
Redact tokens, authorization headers, cookies, prompts, Slack bodies, ACP frames, tool output, diffs,
and GitHub artifacts.

### Kill switches

Independent controls:

- disable new incident/session creation;
- disable automatic alert triggers;
- disable Emisar containment/mutations while retaining observation;
- disable GitHub publication;
- disconnect one Slack installation;
- stop one active turn;
- close one incident's session while preserving work;
- globally drain workers without discarding queued durable records.

Destructive fork discard is not an incident kill switch.

### Reconciliation

Periodic reconcilers own:

- incident-source inbox events, provider cursors, and source-version drift;
- declared incidents missing channels;
- channels missing canonical threads;
- bindings missing Coop sessions;
- Coop sessions/forks with missing bindings;
- stale queued/running turn state;
- uncertain/non-message Coop create, cancel, budget, review, close, and discard operations;
- pending Emisar operations/approvals;
- uncertain Slack outbox sends;
- uncertain GitHub branches/PRs;
- closed incidents with retained sessions beyond policy;
- discard plans/tombstones and retained artifacts.

Reconciliation uses exact IDs and immutable refs. It never infers ownership from names alone.

## Competitive research

Snapshot date: 2026-07-26. Re-verify projects before adopting code or making current activity
claims.

### OpenAB

Repository: <https://github.com/openabdev/openab>
Audited commit:
<https://github.com/openabdev/openab/tree/53061d696148106b2b7529f9d6c5dd802dff4545>

Useful:

- Slack Socket Mode with reconnect and watchdog.
- Per-thread FIFO and one active turn.
- Per-thread native ACP session mapping with `session/load` recovery.
- Per-thread creation mutex without holding a global pool lock during slow startup.
- Slack-native status/streaming with post/update fallback.
- Docker, Helm, Kubernetes, non-root/read-only-root/capability-drop packaging.

Reject:

- ACP permission picker
  [selects `allow_always` before `allow_once` and can fabricate it](https://github.com/openabdev/openab/blob/53061d696148106b2b7529f9d6c5dd802dff4545/crates/openab-core/src/acp/connection.rs#L16-L75).
- Empty channel/user allowlists mean allow all.
- One pod shares HOME, OAuth/PVC, workspace, network namespace, and credentials across threads.
- [Slack ACK occurs before durable dispatch](https://github.com/openabdev/openab/blob/53061d696148106b2b7529f9d6c5dd802dff4545/crates/openab-core/src/slack.rs#L884-L930);
  there is no event-ID dedupe, inbox, or outbox.
- Session key is only `slack:<thread_ts>`, omitting workspace and channel.
- Top-level bot alerts are not a complete trigger path.
- No channel creation, incident lifecycle, host review boundary, or durable operator relay audit.

Lesson: borrow transport/session-pool mechanics, not its authority or isolation model. Autoapproving
local ACP permissions is only defensible when the process is inside Coop's credential-poor box and
production authorization remains elsewhere.

### OpenACP

Packages:

- CLI
  [2026.518.2](https://registry.npmjs.org/@openacp/cli/-/cli-2026.518.2.tgz),
  integrity
  `sha512-6Zw5ft1STPq6hi66YOqVA43JD0W76p4DKr61k8wWuD2QN36dhGBylSeYOw5M8AijgEFNqlreIfr8PRrVcfBizA==`
- Slack adapter
  [2026.525.5](https://registry.npmjs.org/@openacp/slack-adapter/-/slack-adapter-2026.525.5.tgz),
  integrity
  `sha512-EY80vxSk34haVCd7VfjLtHjKg0ieR9SNYhILcnu5k34hjdWSlWePehC3JSC+kyX6xQf81PVKa3HDDtlUdidsiQ==`

The CLI's linked core source repository returned 404 during the audit. A GitHub web view for the
Slack adapter appeared intermittently, while anonymous Git access returned repository-not-found.
Treat source availability as unverified until a clean clone at an immutable commit succeeds. The npm
CLI metadata said AGPL-3.0 while packaged documentation/source badges said MIT.

Useful:

- Setup and session UX ideas.
- Durable session metadata and native ACP resume.
- Coalesced tool/status rendering.
- Permission expiry and default rejection.
- Terminal/chat handoff concept.

Reject:

- Slack adapter creates one private channel per session and ignores human thread replies.
- Documented slash-command surface does not match the shipped manifest/routing.
- Permission and archive/output actions do not reauthorize the clicking user.
- Default setup can allow all users.
- Agent and terminal processes run directly as the daemon OS user with no isolation.
- Slack/settings token files lack explicit restrictive mode in audited paths.
- Usage budgets and idle parking were documented but not enforced in the shipped release.
- Plugin installation runs package lifecycle scripts in process; provenance is insufficient for this
  authority boundary.

Lesson: UX breadth is not proof of a secure team execution model or even of shipped behavior.

### Hoomanity

Repository: <https://github.com/vaibhavpandeyvpz/hoomanity>
Audited commit:
<https://github.com/vaibhavpandeyvpz/hoomanity/tree/c2ccd4618a8567dcc0fd3924185fe6736d113637>

Useful:

- Small ACP relay that demonstrates Slack/Telegram/WhatsApp and local-model possibilities.
- Final-message-only output is quiet.
- Permission timeout defaults toward rejection.

Reject:

- [Slack session key is channel-only](https://github.com/vaibhavpandeyvpz/hoomanity/blob/c2ccd4618a8567dcc0fd3924185fe6736d113637/src/listeners/slack/mapper.ts#L31-L33),
  so unrelated threads share context and queue.
- [Agent starts with `shell:true` and the complete process environment](https://github.com/vaibhavpandeyvpz/hoomanity/blob/c2ccd4618a8567dcc0fd3924185fe6736d113637/src/core/agent-transport.ts#L28-L39).
- [Approval actions are not bound to an authorized Slack user/channel/session](https://github.com/vaibhavpandeyvpz/hoomanity/blob/c2ccd4618a8567dcc0fd3924185fe6736d113637/src/listeners/slack/actions.ts#L4-L57).
- Defaults include wildcard channel access and no mention requirement.
- Token config writes do not explicitly enforce `0600`.
- [Whole-file asynchronous JSON session persistence](https://github.com/vaibhavpandeyvpz/hoomanity/blob/c2ccd4618a8567dcc0fd3924185fe6736d113637/src/core/session-registry.ts#L74-L117)
  is not a durable concurrent ledger.

Lesson: platform breadth and local-model support do not compensate for missing identity, thread
isolation, secret containment, and crash semantics.

### slack-acp

Repository: <https://github.com/kfet/slack-acp>
Audited commit:
<https://github.com/kfet/slack-acp/tree/c03b0ba0d9ae075a0106ade42bc8d4930ff802c6>

Useful:

- Compact Go implementation.
- Correct `(channel, thread_ts)` local routing key and per-thread mutex.
- Stable per-thread work directories and path containment.
- One throttled Slack message with serialized writes and unfurls disabled.
- Setup verifies tokens and writes restrictive config/service units.

Reject:

- Current integration regression: Socket transport
  [strips the mention](https://github.com/kfet/slack-acp/blob/c03b0ba0d9ae075a0106ade42bc8d4930ff802c6/internal/slackproto/slackproto.go#L136-L148)
  before the handler
  [checks for it](https://github.com/kfet/slack-acp/blob/c03b0ba0d9ae075a0106ade42bc8d4930ff802c6/internal/handler/handler.go#L129-L146),
  so a real channel summon produces no ACP prompt or Slack output. Unit tests validate the two
  incompatible layers separately; there is no automated cross-layer E2E.
- [Child inherits the host environment](https://github.com/kfet/slack-acp/blob/c03b0ba0d9ae075a0106ade42bc8d4930ff802c6/cmd/slack-acp/main.go#L118-L122),
  including Slack/other credentials.
- Default ACP policy grants permissions, and
  [filesystem callbacks can reach arbitrary absolute host paths](https://github.com/kfet/acp-kit/blob/aa5fca9bcbb0201b7dc8fbf6a4c9a063a2f166ef/client/agent.go#L608-L644).
- Backfilled messages do not consistently apply user allowlists/deduplication.
- Thought chunks can be exposed unless configured.
- [A new message cancels the active turn](https://github.com/kfet/slack-acp/blob/c03b0ba0d9ae075a0106ade42bc8d4930ff802c6/internal/handler/handler.go#L149-L160)
  instead of entering a queue.

Lesson: preserve its routing/streaming patterns and add real contract E2E, but execution must stay
behind Coop.

### IncidentFox

Repository: <https://github.com/incidentfox/incidentfox>
Audited commit:
<https://github.com/incidentfox/incidentfox/tree/1b6ffad4551da1eef1f2ca6ec254e9dd1816b002>

Useful:

- One sandbox per investigation/thread.
- gVisor Kubernetes isolation.
- Warm pool for low startup latency.
- Per-sandbox identity and credential proxying so broad integration credentials need not enter the
  agent.
- Strong alignment with alert-to-investigation and SRE evidence collection.

Reject/qualify:

- Repository was archived at audit time.
- Production isolation/credential layer is BSL 1.1 rather than the core Apache license.
- Some auto-investigation routing is memory-resident/fire-and-forget.
- Auto-investigating every message in configured channels risks storms and bot loops.
- One Slack attachment path includes a bearer bot token in the request to the investigation service,
  contradicting the desired credential boundary.

Lesson: one sandbox per investigation and credential brokerage validate Coop's boundary. Do not
copy its lifecycle or token path uncritically.

### HolmesGPT

Repository: <https://github.com/HolmesGPT/holmesgpt>
Audited commit:
<https://github.com/HolmesGPT/holmesgpt/tree/8f9c4d08aa55805c155f15f1a3cc99c8a3fd53cd>

Useful:

- Active CNCF SRE engine and broad observability/tool integration.
- Fixed tool-side split between safe diagnostic tools and one human-approved mutation fallback.
- Scoped RBAC, verb/image/command allowlists, dangerous-flag blocks, `shell=False`, and timeouts.
- Noninteractive mode fails tools requiring approval rather than auto-approving.
- GitHub App option mints and refreshes short-lived installation tokens.

Qualify:

- It is an SRE reasoning/tool engine, not the durable Slack/fork conversation controller.
- Slack conversational behavior often arrives through adjacent Robusta/product surfaces.

Lesson: action risk belongs in trusted tool/server policy, not model classification. Failure-closed
automation and short-lived GitHub App credentials are the patterns to take.

### incident.io and Rootly

References:

- <https://incident.io/investigations>
- <https://docs.incident.io/incidents/declaring>
- <https://docs.rootly.com/incidents/creating-incidents/creating-incidents-via-slack>

Useful:

- Incident declaration and stable identity.
- Deduplication and channel lifecycle.
- Roles, timelines, actions, workflows, and postmortems.
- Existing Slack-native responder experience.

Lesson: use or integrate with incident lifecycle rather than rebuilding incident management merely
to feed Coop. Differentiate on trustworthy isolated execution, evidence, and reviewable fixes.

## Rollout

### Phase 0: contract and threat model

- Finalize incident source, identity mappings, responder preset, Coop policy, database, retention,
  and kill switches.
- Threat-model Slack input, external members, attachments, Emisar actions, GitHub publication, and
  box egress.
- Complete Coop generic API contract and source-boundary test.

Done when duplicate/ambiguous operations, authority boundaries, and denial cases have executable
tests before live Slack access.

### Phase 1: manual observe-only

- One Slack workspace.
- One repository and Coop execution policy.
- Trusted same-UID local responder with explicit process sandboxing limitations documented.
- Existing incident channel; manual mention creates canonical thread/session.
- Default-deny operators/channels.
- Durable inbox/outbox.
- One turn at a time, queue, status/stop/extend/close.
- Emisar observe-only.
- Changes/review link; no GitHub publication.

Done when an authorized responder can investigate, answer a follow-up after park/resume, inspect a
review, and close while unauthorized/duplicate/crash paths fail correctly.

### Phase 2: gated draft PR

- GitHub App publisher.
- Coop review patch reconstruction and candidate-tree verification.
- Secret scan and draft PR only.
- Idempotent branch/PR reconciliation.
- Publication kill switch.

### Phase 3: declared incidents and channels

- Integrate incident source of truth.
- Authenticated persist-before-ACK webhook intake, durable polling cursor, or authenticated
  control-plane delivery; no implicit unauthenticated ingress.
- Stable incident-to-channel/thread/session reconciliation.
- Alert correlation and trusted bot evidence.
- Capacity/backpressure notices.
- Reopen/resolve/archive workflows.

### Phase 4: containment and approvals

- Separate feature flag for Emisar contain mode.
- Brokered exact-operation approval with no investigation-session credential upgrade.
- Pending approval links first; park agent turns while approval is pending.
- Exact identity/action binding and approval reconciliation.
- No break glass or deployment authority.

### Phase 5: product/HA

- Multi-workspace/tenant isolation.
- Emisar-owned control plane and outbound customer worker.
- PostgreSQL HA, leader election, backups, restore/reconciliation.
- Multi-repository coordinator.
- Attachments and artifact policies only after dedicated security review.

Each phase has an independent rollback/kill switch. Do not bundle Slack rollout, production
mutation, and GitHub write authority into one launch.

## Verification

### End-to-end acceptance

```text
declare or manually summon
  -> idempotently bind channel/canonical thread
  -> create Coop session/fork
  -> run observe-only investigation
  -> post bounded terminal question
  -> authorize human reply
  -> resume exact parked session
  -> create committed fix
  -> get structured changes and immutable review
  -> publish gated draft PR (phase 2)
  -> resolve incident
  -> close session while preserving fork/evidence
```

### Unit and property tests

- ID normalization and under-scoped identity rejection.
- State machines and illegal transitions.
- Unique constraints and canonical idempotency hashes.
- Authorization on every message/action.
- Prompt framing and untrusted-data boundaries.
- Markdown/mention/unfurl sanitization.
- Redaction and secret detection.
- Branch naming and exact SHA validation.
- Retryable, terminal, policy, and unknown outcome classification.

### Integration and chaos tests

- Duplicate and out-of-order Slack events.
- Same mention delivered as both `app_mention` and `message.*` admits one manual incident/input.
- Duplicate, reordered, replayed, and signature-invalid incident-provider events; polling cursor
  crash before/after commit.
- Crash before and after inbox commit/ACK.
- Crash before and after Coop create/turn admission.
- Coop response lost after successful operation.
- Identical Coop operation retry after its first result advanced the session revision.
- Single-turn serialization and bounded queue.
- Crash before/after debounced turn-membership freeze preserves one request/key.
- Cancel, park, resume, budget exhaustion, and extension.
- Socket disconnect/refresh and Slack 429.
- Outbox restart replays the exact stored render bytes; ambiguous send without a supported marker
  remains uncertain.
- Exact full-message command parsing; partial/prose/code-block controls do not execute; close/publish
  require bound confirmation.
- Unauthorized, stale, expired, replayed, and cross-thread buttons.
- Guest, external, Slack Connect, deactivated, and unmapped users.
- Own-bot and trusted/untrusted bot loops.
- Malicious Slack Markdown, ANSI, URLs, files, archive bombs, and hidden instructions.
- Tool/log output prompt injection.
- Secret-bearing assistant output, diff, commit, review, and PR body.
- Emisar pending, denied, expired, ambiguous, and contract-changed actions.
- Pending approval parks the box and resumes through one new exact-operation result turn.
- Git push succeeds but response is lost.
- PR exists, branch collides, base moves, human changes branch.
- Database backup/restore with orphan reconciliation.
- Incident reopen appends a new repository binding/session and preserves the prior fork/history.
- Cross-incident/provider-home transcript and credential canaries remain unreadable from the box.

### Load tests

- Alert storm does not create one channel/session per alert.
- Per-tenant/repository fairness.
- Global and per-policy concurrency caps.
- Queue count/byte limits.
- Slow Slack and slow Coop event consumers.
- No silent loss and visible holding state.

### Boundary test

The Coop repository should mechanically reject Slack SDKs, Slack/incident domain types, Emisar
product profiles, and GitHub publication code from the generic session packages. The external
service should contract-test only the versioned generic API.

## Open decisions

These are intentionally not buried in implementation:

1. Standalone service or Emisar control-plane ownership after the internal MVP.
2. Incident source of truth and channel lifecycle owner.
3. Slack app per tenant/workspace and installation administration.
4. Public/private incident channels and future Slack Connect policy.
5. PostgreSQL and HA requirements for the first deployment.
6. Slack-to-organization and Slack-to-Emisar identity enrollment.
7. Existing-channel manual summon versus channel-per-declared-incident initial UX.
8. Multi-repository coordination and which agent narrates to the canonical thread.
9. Consecutive-message debounce versus always-one-message/one-turn.
10. Operator handoff and authority to stop/extend/close/publish.
11. Slack message/content, Coop session, fork, and artifact retention periods.
12. Local standalone worker versus authenticated outbound Emisar worker protocol.
13. Attachment and artifact transfer policy.
14. Emisar approval deep links versus later Slack buttons.
15. Status verbosity and Slack native assistant API use.
16. Spend source of truth and who may extend budgets.
17. GitHub branch update, unsigned commit, and landing/signing policy.
18. Whether channel creation uses the responder app or a separate incident-lifecycle integration.

## Explicit non-goals

- Slack support inside Coop.
- A public Coop network API.
- Automatic investigation for every raw alert/message.
- Replacing incident.io, Rootly, PagerDuty, or established incident lifecycle without a product
  decision.
- Giving Slack channel members implicit agent or production authority.
- Letting Slack text select repositories, images, mounts, credentials, MCP servers, egress, or
  budgets outside allowlists.
- Sending Slack/GitHub/database tokens into the box.
- Surfacing generic ACP permission prompts as production approval.
- Model-interpreted status/stop/extend/publish commands.
- Raw remote shell or break-glass access through chat.
- Automatic containment, deployment, signing, merge, or branch-protection bypass.
- Chain-of-thought, tool transcript, environment, or raw log streaming.
- Close-implies-delete or cleanup-on-error.
- One Coop session spanning multiple repositories.

## Near-misses to reject in review

- "Socket Mode means the bot is secure."
- "A `0600` same-user socket isolates Coop from a compromised responder."
- "Workspace or channel membership means the user is authorized."
- "`thread_ts` is globally unique."
- "A nullable enterprise ID is safe inside a uniqueness key."
- "Slack event IDs deduplicate `app_mention` against `message.*`."
- "ACK first, enqueue later."
- "Empty allowlist means everyone."
- "Retry with a new idempotency key after a timeout."
- "A PR-only PAT can push code without Contents write."
- "A visible button proves approval authority."
- "The word `publish` anywhere in a reply is a command."
- "Changing response mode can upgrade an existing Coop session's credential."
- "The model can decide whether its own action is safe."
- "All thread replies are trusted context."
- "Redacting only the final response is enough."
- "Posting raw tool output is transparency."
- "A content digest proves an uncertain Slack message was posted."
- "Deleting the responder database erases a prompt from provider-native history."
- "Closing a session should clean up the fork."
- "One incident always touches one repository."
- "A channel should be created for every alert."
- "A green gate means the diff is safe to publish."
