---
name: native-broker-lease-reads
description: the host keeps leasing the held access through a passing renewal failure; VM lease reads use fresh descriptors and the existing load deadline, never a cached grant
subsystem: box
sources: [internal/box/native_broker.go, internal/networkgateway/native_runtime.go, internal/networkgateway/native_document_test.go, internal/networkgateway/native_broker.go, internal/networkgateway/native_broker_test.go, internal/networkgateway/native_broker_diagnostic_test.go, internal/safefile/safefile.go]
updated: 2026-10-16
---

The host atomically publishes private access snapshots, but a Docker VM bind mount can briefly
return a missing leaf or incomplete JSON. Three fresh attempts separated by 50ms were insufficient:
a public-synthetic Linux VM reader failed after three missing reads while its host publisher kept
renaming valid files. Live session diagnostics independently observed missing and incomplete reads;
the original session's first-stop category was not retained and cannot be attributed retroactively.

`decodeNativeDocument` reopens the strict root/file on each attempt, retrying only missing or
syntactically incomplete input at a cancellable 50ms cadence. Its retry/admission deadline is the
earlier of the caller's deadline and one second, including background/config callers. This does
not make a blocking filesystem syscall cancellable. Complete schema errors and other I/O errors
refuse immediately. Cancellation is checked before reads and before admitting a decoded document.

`NativeBroker.current` still validates fresh binding, revision, expiry and credential integrity.
Revocation, rollback, same-revision mutation, exhaustion and invalid snapshots permanently stop
protected traffic and active streams. The previous credential is never forwarded on read failure;
a stopped broker cannot revive. Partial fields are never decoded or inherited between attempts.

The host publisher (`nativeRun.serve`) writes a lease every 5 s, at most a minute and never past
the access's own expiry. It renews only once the access has under 2 minutes left, so a renewal
outage gets about 2 minutes before the access expires and `publish` revokes it. The per-run backoff
tries again 5, 10, 20 and 40 s apart; its 1-minute cap never binds before the access expires. A
passing failure keeps the held access. The ticks between attempts read the account under its lock,
so a host sign-in or removal revokes at the next tick that reads, without a provider call. Only a
busy account lock leaves no fresh record: `served` is published again, but for at most 2 minutes
after the last locked read, because a tick that cannot read cannot see a sign-in or removal.

The protected account broker can stop while the outer gateway and host publisher remain healthy.
A local CONNECT then returns403 `native account unavailable` before any upstream dial; zero
gateway denials and a currently valid lease do not identify the original account stop. Helper
stderr retains one fixed `native_broker_stopped` category with revision, attempt and duration
counters. It is emitted after protected cancellation/connection teardown, outside the broker
mutex; a blocked log sink cannot delay revocation. Credentials, accounts, raw documents and
arbitrary I/O/parser errors are never logged. Document callers still receive the generic error.

## Changelog
- 2026-10-16 — the host publisher keeps leasing the held access through a passing renewal failure
  and retries on backoff instead of revoking (task
  2026-10-16-let-the-native-broker-retry-a-failed-renewal-wit). Recorded the 2-minute renewal
  window that bounds outage tolerance and the 2-minute bound on re-serving through a busy lock;
  verified with the `TestNativeBroker` synctest family and mutation runs of the loop.
- 2026-10-12 — retained first-stop diagnostics after a new live refusal with healthy helpers
  and an advancing lease. Instrumented native tools/later follow-up and100000 production VM
  reads/validations passed; the original initiating category remains unknown, not fixed by those
  passes. Blocked-log regression verifies teardown precedes emission; snapshot denial predicates
  and the existing load budget are unchanged.
- 2026-10-11 — replaced the premature three-read cutoff with the existing load budget; tested
  fourth-read convergence, shorter deadlines, background bounds, cancellation and unchanged denials.
