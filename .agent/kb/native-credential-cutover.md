---
name: native-credential-cutover
description: existing native writer locks keep their inode during private normalization; uncertain legacy grants remain in custody
subsystem: credentials
sources: [internal/box/native_cutover.go, internal/box/account_authority.go, internal/box/native_writer_lock_test.go, internal/agent/grok.go, internal/agent/gemini.go]
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

## Changelog
- 2026-10-09 — traced the real Grok startup refusal to an owned 0644 PID lock; verified
  busy/replacement/link/type/ownership refusal and same-inode/byte normalization in tests.
