---
name: native-broker-lease-reads
description: the host renews up to 10 minutes ahead, runs on one account take turns retrying, and the held access stays leased through a passing failure; VM lease reads use fresh descriptors and the existing load deadline, never a cached grant
subsystem: box
sources: [internal/box/native_broker.go, internal/box/native_account.go, internal/box/account_renewal.go, internal/networkgateway/native_runtime.go, internal/networkgateway/native_document_test.go, internal/networkgateway/native_broker.go, internal/networkgateway/native_broker_test.go, internal/networkgateway/native_broker_diagnostic_test.go, internal/safefile/safefile.go]
updated: 2026-10-17
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
the access's own expiry. It renews the access planning read once under 10 minutes of it are left
(`nativeRenewalLead`, well under Gemini's hour, the shortest access a provider issues), and each
later access from half the lifetime it had when the run first saw it, between 2 and 10 minutes
(`renewalLead`), so one issued for little more than the lead is not renewed at every tick. A box
rides out an outage of about 9 minutes: its last retry comes 40 to 60 s before expiry, after which
`publish` revokes. The renewed access only has to outlast 2 minutes (`nativeRenewalDeadline`, the
coverage check in every adapter's `Renew`); passing the lead as that deadline would refuse, and so
revoke, an access issued for less than the lead. The per-run backoff tries again 5, 10, 20 and 40 s
apart, then every minute. A passing failure keeps the held access. The ticks between attempts read
the account under its lock, so a host sign-in or removal revokes at the next tick that reads,
without a provider call. Only a busy account lock leaves no fresh record: `served` is published
again, but for at most 2 minutes after the last locked read, because a tick that cannot read cannot
see a sign-in or removal.

Runs on one account take turns (`retry.json`, account_renewal.go). An exchange that fails for a
passing reason records, under the lock and for that exact record, when it ended and a wait of 5 s
doubling to a minute; any publication ends it and removal deletes it. A renewal while the access
still covers the deadline waits it out (`errRenewalDeferred`, retryable) and holds the lock only to
read; one the deadline needs never waits. Both are judged once the lock is held, since getting it
can take 30 s; a wait that ends more than a minute from then was written before the host clock
stepped back and is ignored, as is one that cannot be read. Before this, every run retried on its
own schedule and held the lock through exchanges that can hang 20 s, so with several boxes the
others could not read for over 2 minutes and lost valid access.

Traps. A launch (`planNativeAccounts`, `prepare`) renews only short of the deadline, because a
failed renewal there fails the launch. An access that cannot be renewed stays in use until it stops
covering the deadline. Gemini's renewal once handed back an access that still covered the deadline
unchanged, which made every renewal ahead a no-op.

The protected account broker can stop while the outer gateway and host publisher remain healthy.
A local CONNECT then returns403 `native account unavailable` before any upstream dial; zero
gateway denials and a currently valid lease do not identify the original account stop. Helper
stderr retains one fixed `native_broker_stopped` category with revision, attempt and duration
counters. It is emitted after protected cancellation/connection teardown, outside the broker
mutex; a blocked log sink cannot delay revocation. Credentials, accounts, raw documents and
arbitrary I/O/parser errors are never logged. Document callers still receive the generic error.

## Changelog
- 2026-10-17 — renewal starts with a 10-minute lead, separate from the 2-minute deadline the adapters
  check, and later accesses renew at half their lifetime (task
  2026-10-16-start-native-credential-renewal-well-before-the). Review found the lead let several
  boxes on one account starve each other of the lock (6 boxes, 2 revoked); added the shared retry
  wait. Recorded the launch exception, the unrenewable access and the old Gemini no-op; verified
  with the `TestNativeBroker` synctest family (outage inside and past the lead, refusal inside it,
  issued lifetimes, six boxes on one hung provider) and mutation runs of each guard. Re-review: the
  wait is ignored when it ends over a minute ahead (clock stepped back) and judged once the lock is
  held; cases for a wait inside the deadline, after a lock wait, from a clock step, corrupt, after
  removal and after a new sign-in.
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
