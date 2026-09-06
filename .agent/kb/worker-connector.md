---
name: worker-connector
description: the outbound worker journals every controller command before it runs, resends results until acknowledged, moves workspaces only as digest-verified bounded bundles, and never falls back to local execution
subsystem: worker
sources: [internal/cli/worker_cmd.go, internal/workerconnector/connector.go, internal/workerconnector/executor.go, internal/workerconnector/journal.go, internal/workerconnector/receipt_page.go, internal/workerconnector/create_origins.go, internal/workerconnector/http_transport.go, internal/workerconnector/identity.go, internal/workerconnector/redirect_test.go, internal/workerconnector/event_streams.go, internal/workerconnector/unixapi.go, internal/workerproto/protocol.go, internal/workerproto/checkpoint_manifest.go, internal/sessionsvc/checkpoint.go, internal/sessionsvc/http.go, internal/sessionsvc/worker_connector_test.go, docs/worker.md, docs/examples/worker.json]
updated: 2026-09-06
---

`coop worker connect --config <absolute-path>` runs one private Coop daemon as a fleet worker. Its
shape is a poll loop, not a server: the connector opens a single outbound mutual-TLS stream to the
controller, maps only versioned commands (`create_session`, `submit_turn`, `fence_operation`,
`reconcile_operation`, the workspace ensure/checkpoint pair, …) onto the owner-private Unix API,
and never listens on TCP or accepts a shell command (`internal/cli/worker_cmd.go`).

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

## Changelog
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
