---
name: worker-connector
description: the outbound worker journals every controller command before it runs, resends results until acknowledged, streams large API bodies under the same command identity, and never falls back to local execution
subsystem: worker
sources: [internal/cli/session_connect.go, internal/cli/session_cmd.go, internal/workerconnector/connector.go, internal/workerconnector/first_download_waits.go, internal/workerconnector/executor.go, internal/workerconnector/bodies.go, internal/workerconnector/journal.go, internal/workerconnector/receipt_page.go, internal/workerconnector/create_origins.go, internal/workerconnector/http_transport.go, internal/workerconnector/identity.go, internal/workerconnector/redirect_test.go, internal/workerconnector/event_streams.go, internal/workerconnector/unixapi.go, internal/workerconnector/capabilities.go, internal/workerproto/protocol.go, internal/workerproto/job.go, internal/workerproto/checkpoint_manifest.go, internal/sessionsvc/checkpoint.go, internal/sessionsvc/checkpoint_repository.go, internal/sessionsvc/checkpoint_restore.go, internal/sessionsvc/checkpoint_storage.go, internal/workerconnector/temporary.go, internal/sessionsvc/http.go, internal/sessionsvc/review.go, internal/sessionsvc/worker_connector_test.go, docs/session-api.md, internal/workerconnector/storage.go, internal/workerproto/session_evidence.go, internal/sessionsvc/evidence.go, internal/sessionsvc/capacity.go, internal/sessionsvc/activity.go, internal/workerconnector/activity_narration.go]
updated: 2026-10-05
---

`coop sessions connect --controller <https-url> --token-file <path>` connects this machine to a fleet controller. Its
shape is a poll loop, not a server: the connector opens a single outbound mutual-TLS stream to the
controller and forwards protocol-v2 `api_request` envelopes to the owner-private Unix API.
Method, relative path, allowed headers and JSON or streamed body carry every API endpoint without
product-specific commands. The connector never listens on TCP or accepts a shell command (`internal/cli/session_connect.go`).

The traps the code does not make obvious:

- **Capacity follows live runtime custody.** The daemon's `/v1/capacity` measures four shared
  active/warm slots; creation's Git concurrency is separate. Admission reserves before leasing a
  turn or starting its timeout. Warm expiry holds the session lock through teardown, and failed
  process/box cleanup retains its slot. Unknown startup runtime custody advertises busy,
  including quarantine and record-only retirement; a separate exact-run-label/private-ACP proof
  can release capacity without granting workspace authority. Deleting a session record is not proof.
  Every session that survives a restart starts unproven and capacity stays zero until the last one
  is proven, so `Start` drains that backlog at once (`drainHistoricalRuntimes`: the janitor's
  bounded batches back-to-back until a pass proves nothing); the one-minute ticker only retries
  what failed. Two proofs a minute had left a restarted worker with 17 parked sessions busy for
  about nine minutes.
- **Jobs are not worker advertisements.** Protocol v2 rejects the retired policy digests and
  repository catalog. Capabilities describe daemon features; each immutable job authorizes its
  own settings and exact source. `job-setup:2` is advertised only when the running daemon proves
  it can decode v2 complete setup. Source selection has no separate advertised capability.
- **Poll references are opaque correlation, not worker identity.** Identity validation, connector
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
  Real TLS plus Unix-service integration rebuilds
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
- **Large bodies do not pause heartbeats.** Run owns a bounded FIFO and one execution goroutine;
  polling remains live during source downloads, request preparation and response uploads. Only
  identical command redelivery renews its in-memory lease. Expiry cancels preparation and both
  lease and placement are checked again before forwarding a mutation. Shutdown waits for execution
  and refuses new admission at the poll loop, queued start and executor entry. A successful poll
  still applies acknowledgements even if cancellation races its response; a ready completion or
  timer must not restart a command after that acknowledgement deletes its receipt.
- **A create waiting on a first download is held, quietly.** A repository's first download (its
  whole history) runs in the background, and a create that needs it fails with
  `errJobSourceDownloading`, which has no result, so the controller redelivers it on every poll.
  `firstDownloadWaits` (first_download_waits.go) skips staging it for 5 s, doubling to 2 min;
  polling goes on meanwhile, so redelivery keeps renewing the controller's lease. Run reports the
  wait once, when it begins, not on every try. Tests that drive Run should use synctest and few
  polls, because every poll fsyncs the journal; a fake API needs `Forward` too, or a create is
  rejected before it reaches the stager.
- **Response uploads resume from saved bytes.** Binary and large JSON bodies are spooled privately,
  hashed and journaled before upload. A lost upload acknowledgement resends that exact spool after
  restart, without rerunning the API. The controller's command-result acknowledgement releases it.
  Request bodies are verified before forwarding with their exact Content-Length.
- **HTTP outcomes stay HTTP outcomes.** A successfully transported 409 is a successful transport
  receipt containing status 409, not a succeeded session operation. Controllers must interpret the
  HTTP status and body. Review reconciliation reads the saved operation and dossier explicitly;
  the connector no longer makes hidden per-method follow-up calls.
- **Generation markers outlive activity.** Bound/failed origins prevent old receipts from resurrecting
  a discarded or superseded stream. Legacy v1 streams remain readable; an acknowledged terminal
  legacy stream stays dormant if it is the only generation floor. Short metadata transitions use a
  nonblocking journal lock; network lookups happen outside it and recheck origin identity when binding.
  A visible rename is not durable proof after a failed directory sync: retries re-sync before receipt
  release, stream adoption or event publication. A corrupt origin suppresses its corresponding stream
  and reports an error while healthy siblings continue; it never enables legacy fallback.
- **Checkpoints preserve complete custody.** V2 streams raw typed Git objects and LFS payloads,
  including intermediate new history, plus the final tracked tree, untracked files and task state.
  Quarantine proves all bytes and task authority before live replacement. Exact HEAD survives;
  the staged/unstaged split does not. GNU tar supports large members with bounded memory and
  metadata; physical headers, padding and terminators are checked without hidden tar extensions.
  V1 remains readable history, never a patch-only execution fallback. Changed historical gitlinks
  require separate child custody and are refused. Paths cannot cross `.git` or symlinked parents.
- **Restore holds a durable runtime fence.** The private body is fsynced before the Running intent.
  Turns and competing restores are refused until same-operation recovery completes; a new key
  cannot overwrite a bound session. Capture shares atomic turn admission exclusion. Operation-owned
  staging is reclaimed only while its operation lock is idle; final capture/restore artifacts
  carry private local session ownership for discard. Historical unmappable artifacts remain intact.
  Disk pressure cancels new checkpoint allocation while retaining an interrupted restore's body.
- **No local fallback.** Every command is a private-API call; nothing executes work directly or
  reads a shared filesystem when the daemon is unreachable — the error is reported and the
  controller redelivers.
- **Activity has two privacy boundaries.** The owner-private events API retains bounded tool
  evidence with absolute paths. Outbound `publicActivityPayload` narrates a tool's title, input
  and result, thoughts, progress and plan, each bounded, secret-scanned and with the checkout
  root replaced (`activity_narration.go`). Only the daemon knows that root: it writes
  `checkout_root` on every narrated event (`sessionsvc/activity.go` `narratedActivity`), and the
  worker reads it and never copies it out. The worker once read the root from
  `path_context.root`, which the daemon never wrote, so every absolute path reached the
  controller; `sessionsvc/testdata/narrated_activity.json` now pins the daemon's real events and
  both packages' tests use it. Tool `path_context` crosses only after independent validation: up
  to 16 lexical project-relative paths, outside or unknown warnings without absolute paths. This
  is display metadata, not symlink containment authority. Late path evidence belongs to the
  completion, not a rewritten start event. The reconnect/ACK regression proves this metadata
  survives durable delivery.

## Changelog
- 2026-10-05 — the checkout root never reached the worker's narration; the daemon now writes
  `checkout_root` on narrated events (task 2026-10-05-check-whether-worker-activity-events-leak-the-ch).
- 2026-10-04 — documented the first-download hold (d019c807) and its once-per-wait report
  (task 2026-10-04-a-create-waiting-on-a-repository-s-first-downloa), verified against connector.go.
- 2026-10-01 — Go1.27's default JSONv2 leaves JavaScript escaping enabled when
  SetEscapeHTML(false) is used. The wire-only encoder now explicitly disables both HTML
  and JavaScript escaping with v1 semantic options; receipt bounds and canonical identity
  encoders remain unchanged. Unicode-separator custody/restart and maximum-wire controls
  reproduce the regression without models.
- 2026-09-30 — exact-tag/full-main race gates exposed receipt acknowledgement racing shutdown;
  deterministic cancellation-in-poll regression and repeated transfer/lease tests verify the
  admission guards without weakening the existing one-download/replay contract.
- 2026-09-30 — verified that the connector's job-setup advertisement follows live daemon
  `job_spec_versions:[2]` proof; an older daemon cannot silently accept incomplete v1 jobs.
- 2026-09-30 — separated quarantine execution denial from runtime-capacity uncertainty. Startup
  seeds quarantined and retired sessions into the immediate historical drain; exact run-label and
  private credential proof releases capacity while leaving their workspace and native history
  untouched. Retired active turns bypass ordinary workspace-dependent startup reaping.
- 2026-09-28 — Ryker's worker restarted with 17 parked sessions advertised busy for 5+ minutes
  (janitor: 2 proofs per 1-minute tick; capacity zero while any is pending). Start now drains the
  backlog immediately with the same per-session proofs; a pass that proves nothing stops it.
- 2026-09-27 — replaced patch-only checkpoint execution with streamed v2 Git/LFS custody,
  verified quarantine restore, durable admission fence, cancellation and exact-owned cleanup.
- 2026-09-27 — replaced hard-coded free capacity with daemon accounting. Focused admission,
  warm-expiry, failed-cleanup and cancellation tests passed, including race detection.
- 2026-09-27 — removed policy/repository advertisements and the retired source-selector capability;
  both protocol decoders reject old fields and mismatched worker versions. Focused wire tests passed.
- 2026-09-26 — replaced the semantic dispatcher and specialized binary transports with one generic
  API tunnel. Added streamed-body retry custody and continued polling during execution. Owning
  package gates passed; renewal, stale-placement and saved-response regressions passed under race detection.
- 2026-09-26 — removed worker JSON and local policy-file setup. Connect uses explicit controller,
  token, optional state and CA flags; identity.json binds the saved certificate to its controller.
  CLI startup and TLS/restart tests migrated. The generic API and publication cutover remains in progress.
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
