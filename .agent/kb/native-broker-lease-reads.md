---
name: native-broker-lease-reads
description: VM lease reads use fresh descriptors and the existing load deadline, never a cached grant
subsystem: box
sources: [internal/box/native_broker.go, internal/networkgateway/native_runtime.go, internal/networkgateway/native_document_test.go, internal/networkgateway/native_broker.go, internal/networkgateway/native_broker_test.go, internal/safefile/safefile.go]
updated: 2026-10-11
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

## Changelog
- 2026-10-11 — replaced the premature three-read cutoff with the existing load budget; tested
  fourth-read convergence, shorter deadlines, background bounds, cancellation and unchanged denials.
