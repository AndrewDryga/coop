---
name: worker-connector
description: the outbound worker journals every controller command before it runs, resends results until acknowledged, moves workspaces only as digest-verified bounded bundles, and never falls back to local execution
subsystem: worker
sources: [internal/cli/worker_cmd.go, internal/workerconnector/connector.go, internal/workerconnector/executor.go, internal/workerconnector/journal.go, internal/workerconnector/event_streams.go, internal/workerconnector/unixapi.go, internal/workerproto/checkpoint_manifest.go, internal/sessionsvc/checkpoint.go]
updated: 2026-09-05
---

`coop worker connect --config <absolute-path>` runs one private Coop daemon as a fleet worker. Its
shape is a poll loop, not a server: the connector opens a single outbound mutual-TLS stream to the
controller, maps only versioned commands (`create_session`, `submit_turn`, `fence_operation`,
`reconcile_operation`, the workspace ensure/checkpoint pair, …) onto the owner-private Unix API,
and never listens on TCP or accepts a shell command (`internal/cli/worker_cmd.go`).

The traps the code does not make obvious:

- **Journal before execute; the receipt is the authority.** `journal.begin` publishes a receipt
  before the executor touches the daemon: a redelivered command with the same digest replays the
  stored result, a changed payload under the same id is `ErrCommandConflict`. Receipts are published
  whole (temp file + exclusive link) because `pending()` decodes every receipt at the top of each
  poll — a torn one would stop the worker from polling at all.
- **Results ride every poll until acknowledged.** Terminal results are resent until the controller
  acknowledges them; the activity-stream ledger (`event_streams.go`) advances only from exact
  acknowledgements and binds only from a successful `create_session` result. Under the connector's
  `Prefer: respond-async` that result carries an operation, not a session — the binding gap is a
  queued task, not a feature.
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
- 2026-09-05 — verified checkpoint restore tracking and binding order against the real service;
  regression covers mixed committed/staged/binary/untracked work and rejected bundles.
- 2026-09-05 — created during the pre-release audit, against the sources listed.
