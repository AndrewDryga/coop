---
name: transport-bounds-do-not-abort-valid-work
description: "bound retained state and single payloads, never the cumulative volume of valid work"
scope: architecture
sources: [internal/acpproxy/proxy.go, internal/consult/wrapper.go]
check: "go test ./internal/consult -run TestConsultWrapperBoundsDefaultToUnlimited"
updated: 2026-08-25
---

# Transport bounds must not become task-completion limits

Bound retained state and individual untrusted payloads, not the cumulative volume of valid data that
is processed incrementally and discarded. A transport guard must not terminate legitimate long work
when cancellation, deadlines, per-frame limits, durable-output limits, and runtime isolation already
bound the actual resources.

If a host cannot retain an intermediate stream, compact, spill, aggregate, or discard that stream
while preserving the live task. Do not turn a host implementation detail into an arbitrary retry or
turn count that users must discover after their work fails.

## Changelog
- 2026-08-25 — source and check paths moved from `internal/fusion` to `internal/consult`; the
  bounds contract and test are unchanged.
- 2026-08-02 — created
- 2026-08-06 — card metadata added (format v1); body unchanged
- 2026-08-09 — graduated from `check: none`: `TestConsultWrapperBoundsDefaultToUnlimited`
  (internal/consult/instructions_test.go) asserts COOP_CONSULT_STREAM_LIMIT and COOP_CONSULT_TIMEOUT
  each default to 0 (unlimited) two ways — the rendered wrapper's own `:-N` fallback, and the
  wrapper actually run with no override — on both the capture path (bounded_capture, the reply/
  diagnostics spool) and the stream path (the peer's own process, bounded by consult_timeout).
  Negative-proof: reverting either fallback (`:-1800` seconds, `:-1048576` bytes) fails the test;
  reverted after confirming. Added the wrapper source to sources — the two 2026-08-03
  violations (6c89f91, a085b29) both lived there, not in the original acpproxy source. Swept the
  current tree: both defaults already comply (0 pre-existing violations). The provider watchdog's
  attempt ceiling (525ad91) is explicitly out of scope, per loop-provider-watchdog.md's own
  changelog — untouched by this check.
