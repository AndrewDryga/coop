---
name: worker-connector
description: the outbound worker journals every controller command before it runs, resends results until acknowledged, moves workspaces only as digest-verified bounded bundles, and never falls back to local execution
subsystem: worker
sources: [internal/cli/session_connect.go, internal/cli/session_cmd.go, internal/workerconnector/connector.go, internal/workerconnector/executor.go, internal/workerconnector/journal.go, internal/workerconnector/receipt_page.go, internal/workerconnector/create_origins.go, internal/workerconnector/http_transport.go, internal/workerconnector/identity.go, internal/workerconnector/redirect_test.go, internal/workerconnector/event_streams.go, internal/workerconnector/unixapi.go, internal/workerproto/protocol.go, internal/workerproto/checkpoint_manifest.go, internal/sessionsvc/checkpoint.go, internal/sessionsvc/http.go, internal/sessionsvc/review.go, internal/sessionsvc/worker_connector_test.go, docs/session-api.md, docs/examples/worker.json, internal/workerconnector/storage.go, internal/workerproto/session_evidence.go, internal/sessionsvc/evidence.go]
updated: 2026-09-11
---

`coop sessions connect --config <path>` connects this machine to a fleet controller. Its
shape is a poll loop, not a server: the connector opens a single outbound mutual-TLS stream to the
controller, maps only versioned commands (`create_session`, `submit_turn`, `fence_operation`,
`reconcile_operation`, the workspace ensure/checkpoint pair, …) onto the owner-private Unix API,
and never listens on TCP or accepts a shell command (`internal/cli/session_connect.go`).

The traps the code does not make obvious:

- **Poll references are opaque correlation, not worker identity.** Config validation, connector
  construction and polling share one formatter. IDs up to 230 bytes keep the legacy
  `poll:<ID>:<sequence>` shape, reserving all 20 uint64 digits. Longer IDs use
  `poll-sha256:<SHA256(ID)>:<sequence>`, a disjoint namespace so hash-like short IDs cannot collide.
  The full Worker.ID is still validated and advertised. Exact response echo remains mandatory
  before receipt deletion, scan advancement or command execution; persistence semantics are unchanged.
- **Redirects are not controller authority.** The shared production identity-client factory
  removes response Location before Go's client can parse or follow it, including malformed URLs
  that otherwise leak into errors before CheckRedirect. The original status and body remain for
  bounded handling; same-origin redirects are refused too. Enrollment, renewal, polls and every
  artifact/checkpoint transfer use that factory; a failed poll preserves receipt/cursor custody.
- **Enrollment is not restart.** Identity begins absent, is generated privately and consumes its
  bootstrap token only after publication. Every restart must retain both identity and the entire
  journal, not only command receipts. A malformed/expired identity never falls back to enrollment.
  The committed JSON example is loaded by tests; real TLS plus Unix-service integration rebuilds
  the transport/executor/connector before receipt and event ACKs, proving disk recovery rather
  than reuse of one in-memory identity manager. Test-only CA helpers never load production trust.
- **Journal before execute; the receipt is the authority.** `journal.begin` publishes a receipt
  before the executor touches the daemon: a redelivered command with the same digest replays the
  stored result, a changed payload under the same id is `ErrCommandConflict`. Receipts are published
  whole (temp file + exclusive link) because `pending()` decodes every receipt at the top of each
  poll — a torn one would stop the worker from polling at all.
- **Receipts page by count and actual wire bytes, never expiry.** ACK/result units rotate across
  restarts using a durable cursor advanced only after a matching validated response. Cursor write
  failure still permits that response's ACKs and commands. Deferred results precede activity;
  structurally readable but unsendable legacy results retain unchanged custody, report their
  command identity and yield to healthy siblings. Arbitrary journal corruption still fails closed.
  Custody and HTTP share non-HTML-escaping JSON encoding so bounded raw payloads do not expand;
  command digests and replay comparison keep their original encoder for compatibility.
- **Results repeat until acknowledged.** Terminal results are resent on subsequent pages until the
  controller acknowledges them. A successful asynchronous create carries an operation, not a
  session: its metadata-only origin must be durable before receipt deletion. The fair activity scan
  resolves only that original key and operation ID, verifies a succeeded CreateRemoteSession/session
  resource, then fsyncs the stream before marking its origin bound. Uncertain operations remain
  pending. Only exact event ACKs advance cursors; lookup/activity errors cannot block response commands.
- **A transport timeout does not erase a review.** A RunReview may finish on the service context
  after its HTTP request expires. Its uncertain command receipt remains immutable. The existing
  reconcile_operation command enriches only a succeeded RunReview with the public saved dossier,
  read through the session-bound reviews endpoint; it never resumes a gate or exports raw
  Operation.Result. The controller must enqueue this read on the original placement.
- **Generation markers outlive activity.** Bound/failed origins prevent old receipts from resurrecting
  a discarded or superseded stream. Legacy v1 streams remain readable; an acknowledged terminal
  legacy stream stays dormant if it is the only generation floor. Short metadata transitions use a
  nonblocking journal lock; network lookups happen outside it and recheck origin identity when binding.
  A visible rename is not durable proof after a failed directory sync: retries re-sync before receipt
  release, stream adoption or event publication. A corrupt origin suppresses its corresponding stream
  and reports an error while healthy siblings continue; it never enables legacy fallback.
- **Bytes move only as verified bundles.** Input artifacts and workspace checkpoints are fetched
  through the authenticated transfer, bounded, and digest-verified before any filesystem mutation;
  member paths are canonical base64 bytes, and the restore writer refuses `.git` components and
  symlinked parents. Restore applies the base-relative tracked patch to both index and worktree:
  plain `git apply` loses the tracking identity of additions and fails exact verification. This
  restores the logical candidate, not its source commit history or staged/unstaged split. Durable
  task binding follows exact patch, untracked-file and task-projection verification. Durable
  command records never carry raw bytes.
- **No local fallback.** Every command is a private-API call; nothing executes work directly or
  reads a shared filesystem when the daemon is unreachable — the error is reported and the
  controller redelivers.
- **Activity has two privacy boundaries.** The owner-private events API retains bounded tool
  evidence, but outbound `publicActivityPayload` deliberately strips free-form fields. Adding
  local narration alone does not make it visible to a fleet controller. Tool `path_context`
  crosses only after independent validation: up to 16 lexical project-relative paths, outside
  or unknown warnings without absolute paths, no checkout root. This is display metadata, not
  symlink containment authority. Late path evidence belongs to the completion, not a rewritten
  start event. The reconnect/ACK regression proves this metadata survives durable delivery.

## Changelog
- 2026-09-11 — `coop worker` is RETIRED; the workflow is `coop sessions connect --config <path>`
  (`internal/cli/session_connect.go`). It validates the configuration first, then reuses a ready
  local service or starts an owned one, proving readiness through the same `/readyz` probe
  `coop sessions doctor` uses. A service that is LISTENING but not ready is not absent: its socket
  is never unlinked and no second service starts. A race on the state root resolves through
  `internal/session`'s exclusive state lock — the loser re-probes and reuses the winner. Ctrl-C
  stops the connector and only a service this invocation created. Two optional strict-JSON fields,
  `session_state_dir` and `session_policy_path`, name which local service a configuration is about;
  `coop_socket` must resolve inside the state directory, and the state root is never inferred from
  the socket's parent. No policy file is ever generated. Docs folded into `docs/session-api.md`.
- 2026-09-11 — a controller can read one session's bounded inspection evidence through
  `get_session_evidence` (a plain owner-private GET the connector forwards verbatim) and the
  `network` session event now crosses `publicActivityPayload` as bounded grouped refusals. The
  `session-evidence:1` capability is advertised only on live daemon proof, so an older worker's
  silence never reads as an empty network. See [[session-evidence-export]].
- 2026-09-11 — hello carries an OPTIONAL `storage` object (`internal/workerproto/storage.go`), read from the daemon's owner-private `GET /v1/storage` by `LiveStorage` and forwarded verbatim. Any failure — old daemon, failed measurement, object that fails its own contract — publishes nothing rather than a wrong number, so an unmeasurable disk never stops a poll. See [[worker-storage-accounting]].
- 2026-09-11 — `create_session` carries a `source` selector; the connector keeps its OWN bounded
  copy of that union (`sourceSelector`, `internal/workerconnector/executor.go`) because this
  package may import only `secretscan` and `workerproto`, refuses a malformed one with
  `invalid_command` before any daemon call, and advertises `repository-source-selector:1` only on
  live daemon proof, independently of `repository-freshness:2`. See [[session-source-selection]].
- 2026-09-10 — live Responder QA exposed a completed 54-second review stranded behind a
  30-second worker timeout. Added read-only result reconciliation and explicit empty evidence
  arrays; HTTP and worker regressions prove no repeated gate and preserved session identity.
- 2026-09-06 — review caught URI-looking names masquerading as relative paths and edit
  previews dropping path evidence. Reject schemes before and after normalization; retain
  bounded typed paths before diff truncation, independently of the owner-private preview.
- 2026-09-06 — traced missing project paths through both activity projections; added bounded
  path metadata and verified HTTP bytes plus outbound restart/ACK while retaining raw-field privacy.
- 2026-09-06 — create_session forwards policy_digest/authority_digest as expected_policy_digest/expected_authority_digest; the daemon fences them at intent capture (`fenceExpectedPolicyDigests`, `internal/sessionsvc/service.go`) with `policy_digest_mismatch`. `decodeAPIError` now reads the daemon's wrapped `{"error":{...}}` shape; before, every daemon refusal surfaced as `http_error`.
- 2026-09-06 — target-side placement fence: `Execute` refuses a still-leased `submit_turn`/`ensure_workspace` whose generation is below the journal's create origin for the session ref with a definite `placement_superseded` failure (`internal/workerconnector/executor.go`); reads and cleanup for the old generation stay allowed.
- 2026-09-06 — reproduced a valid 244-byte ID failing at poll 1000000; bounded the shared formatter
  and covered the full protocol ID range, maximum sequence, disjoint identities and wrong-echo custody.
- 2026-09-06 — reproduced TLS enrollment redirecting a synthetic token to plaintext, then fenced
  the shared identity-client factory; exercised every transport operation and retained poll custody.
- 2026-09-06 — added operator enrollment/recovery guide and real TLS/Unix integration with
  identity persistence, command redelivery, ACK-before-completion and event replay/ACK checks.
- 2026-09-05 — restored async-create activity through real service/Unix API ACK-before-completion and
  restart tests; verified origin identity, generation/tombstone, directory-sync failure and fair-scan
  recovery without resetting cursors or changing receipt identity.
- 2026-09-05 — bounded receipt paging and wire encoding verified through real HTTP with count/byte
  pressure, restart, partial ACKs, expired custody, legacy results and publication/response failures.
- 2026-09-05 — verified checkpoint restore tracking and binding order against the real service;
  regression covers mixed committed/staged/binary/untracked work and rejected bundles.
- 2026-09-05 — created during the pre-release audit, against the sources listed.
