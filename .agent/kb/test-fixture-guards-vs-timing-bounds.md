---
name: test-fixture-guards-vs-timing-bounds
description: a test wait that guards a broken fixture is generous (testutil/wait, 60 s); a tight wall-clock bound is reserved for timing that IS the behavior under test, and then attributes its phases
subsystem: testing
sources: [internal/testutil/wait/wait.go, internal/box/runtime_init_e2e_test.go, internal/cli/fork_cmd_test.go, internal/forkctl/testhelpers_test.go, internal/forkctl/supervise_test.go, internal/consult/instructions_test.go, internal/box/run_test.go, internal/runtime/runtime_test.go, internal/sessionsvc/service_test.go]
updated: 2026-09-10
---

Two kinds of waits look alike in a test and fail alike on a loaded host, but mean opposite things.

**A fixture guard** waits for a subprocess to write its marker, a goroutine to reach a lock, a
container to start — "did the fixture get to step X". Timing is not the behavior under test, so
the deadline exists only to turn a hung fixture into a failure instead of a hang. It must be
generous: a healthy run pays nothing (the poll returns as soon as the condition holds), while a
tight constant (2–5 s was common) fails whenever the gate's race suite runs beside a Docker-heavy
suite — the 2026-09-06 inventory: `TestSpawnBoxExportsEmptyPresetSelection`, the fork lifecycle
lock waiters, `TestConsultWrapperLockSerializesAndRecoversAfterOwnerExit`,
`TestRunCanceledAfterServicesUpTearsDownAttemptedReviewCompose`,
`TestRemoveByLabelAppleContainerExactMatch`, `waitForSessionTest`. All of them passed alone. They
now use `internal/testutil/wait` (`wait.For`, `wait.ForFile`, `wait.Deadline` = 60 s, 5 ms poll),
whose failure names the missing event and the deadline.

**A timing bound** is the behavior: a coop-owned consult must be reaped in seconds and never sit
through the 30 s drain window; a forwarder must not delay a failed provider by more than a
moment. Those stay tight — and attribute what they measure, so a slow run says which phase
consumed the time. The stranded-consult subtest stamps the provider's exit inside the box and
reports "provider exit → container exit" separately from the whole run (whose 20 s bound also
counts container create/teardown on a starved runtime). The release audit's rule holds: never
fix such a failure by raising its threshold; reproduce under controlled load first
(`/tmp/coop-audit/consult-load.sh` did: 2.1–3.0 s under quiet, CPU and I/O load).

A third shape hides in a shell fixture that must ACKNOWLEDGE a signal: a POSIX shell defers a trap
until its foreground command completes, so `trap ... TERM; while :; do sleep 10; done` answers a
TERM only if the sleep dies from the same group signal. Any schedule where the child survives it
(the bare-pid fallback, a signal landing between fork and exec) defers the acknowledgement for the
whole sleep — past a 3 s TERM→KILL grace — and the KILL erases the evidence the test asserts.
Write such a fixture as `sleep 10 & wait $!`: `wait` is interruptible by a trap, so the
acknowledgement never depends on the child (`internal/forkctl/supervise_test.go`, the fork-stop
worker). And record how the fixture process actually ended (`worker:exit exit status 0` versus
`signal: killed`) so a failure shows signal-versus-exit evidence instead of a timestamp to guess
from.

## Changelog
- 2026-09-10 — added the deferred-trap shape after `TestForkStopReapsBoxAfterWorkerExit` failed
  once in a loaded gate (worker gone at 3.23 s, no acknowledgement); modelled deterministically
  with a child that ignores TERM (`signal: killed` at 3.25 s), fixed by backgrounding the sleep,
  20 race repetitions green (task make-fork-stop-term-acknowledgement-deterministic).
- 2026-09-06 — created with `internal/testutil/wait` (task make-the-wall-clock-test-deadlines-load-aware-or) after the release-audit requalification separated the two kinds.
