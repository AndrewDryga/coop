---
name: native-broker-lease-reads
description: VM lease reads use fresh descriptors and the existing load deadline, never a cached grant
subsystem: box
sources: [internal/box/native_broker.go, internal/networkgateway/native_runtime.go, internal/networkgateway/native_document_test.go, internal/networkgateway/native_broker.go, internal/networkgateway/native_broker_test.go, internal/networkgateway/native_broker_diagnostic_test.go, internal/safefile/safefile.go]
updated: 2026-10-12
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

The protected account broker can stop while the outer gateway and host publisher remain healthy.
A local CONNECT then returns403 `native account unavailable` before any upstream dial; zero
gateway denials and a currently valid lease do not identify the original account stop. Helper
stderr retains one fixed `native_broker_stopped` category with revision, attempt and duration
counters. It is emitted after protected cancellation/connection teardown, outside the broker
mutex; a blocked log sink cannot delay revocation. Credentials, accounts, raw documents and
arbitrary I/O/parser errors are never logged. Document callers still receive the generic error.

## Changelog
- 2026-10-12 — retained first-stop diagnostics after a new live refusal with healthy helpers
  and an advancing lease. Instrumented native tools/later follow-up and100000 production VM
  reads/validations passed; the original initiating category remains unknown, not fixed by those
  passes. Blocked-log regression verifies teardown precedes emission; snapshot denial predicates
  and the existing load budget are unchanged.
- 2026-10-11 — replaced the premature three-read cutoff with the existing load budget; tested
  fourth-read convergence, shorter deadlines, background bounds, cancellation and unchanged denials.
