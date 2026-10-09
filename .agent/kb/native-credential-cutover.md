---
name: native-credential-cutover
description: existing native writer locks keep their inode during private normalization; uncertain legacy grants remain in custody
subsystem: credentials
sources: [internal/box/native_cutover.go, internal/box/account_authority.go, internal/box/native_writer_lock_test.go, internal/runtime/mounts.go, internal/runtime/mounts_test.go, internal/agent/grok.go, internal/agent/gemini.go]
updated: 2026-10-09
---

`fenceLegacyAccount` holds the account lease, inventories existing credential mounts and locks
the adapter-declared native writer files before retiring legacy grants. Grok declares
`auth.json.lock`; its native PID file may legitimately have mode 0644.

Only a regular, current-user-owned, single-link writer-lock FD is eligible for normalization.
Coop acquires the nonblocking exclusive flock first, rechecks named root/inode identity and
metadata, then tightens that same inode to 0600 (`legacyAccountFence.lockWriter`). A busy,
replaced, linked, foreign-owned or nonregular lock refuses without chmod. Replacing the lock
would strand a native writer on another inode. Lock bytes do not change.

This exception is for adapter-declared legacy writer locks only. Canonical credential and
authority files still require the unchanged owner-private, regular, single-link validator.
Unknown encrypted Gemini legacy caches have no proven storage identity and are retained
unchanged for explicit host recovery; presence in `coop credentials` is not cutover readiness.

Docker bind inventory freezes the endpoint and rechecks daemon identity after enumeration,
around each inspect batch and at return, including empty results. Full IDs are inspected in
32-container streams containing only ID, lifecycle status and mounts, each record bounded to
the existing 1 MiB mount limit. Missing/duplicate/mismatched/malformed/overflowed records,
failed commands or cancellation return no partial public inventory. Read-only and stopped
containers remain visible. `BindMount.Stopped` is true only when every observed user of a
source is created/exited; any live observation prevents that conclusion across batches.

## Changelog
- 2026-10-09 — traced the real Grok startup refusal to an owned 0644 PID lock; verified
  busy/replacement/link/type/ownership refusal and same-inode/byte normalization in tests.
- 2026-10-09 — replaced serial container inspection with complete bounded batches and retained
  lifecycle evidence, including mixed-state duplicate sources and mid-pass daemon replacement.
