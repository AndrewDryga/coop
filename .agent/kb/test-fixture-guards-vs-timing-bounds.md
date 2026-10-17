---
name: test-fixture-guards-vs-timing-bounds
description: a test wait that guards a broken fixture is generous (testutil/wait, 60 s); a tight wall-clock bound is reserved for timing that IS the behavior under test, and then attributes its phases
subsystem: testing
sources: [Makefile, internal/testutil/wait/wait.go, internal/testutil/procharness/harness_test.go, internal/box/runtime_init_e2e_test.go, internal/cli/fork_cmd_test.go, internal/forkctl/testhelpers_test.go, internal/forkctl/supervise_test.go, internal/forkctl/supervise.go, internal/cli/acp_cmd.go, internal/cli/acp_execution_test.go, internal/acpproxy/scripted_matrix_e2e_test.go, internal/consult/instructions_test.go, internal/consult/capture_ownership_test.go, internal/box/run_test.go, internal/runtime/runtime_test.go, internal/sessionsvc/acp_test.go, internal/sessionsvc/service_test.go, internal/sessionsvc/helpers_test.go, internal/sessionsvc/storage_test.go, tools/lifecyclebench/cases.go, tools/lifecyclebench/cases_test.go, internal/agent/usage_report_test.go]
updated: 2026-10-17
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

A fourth: an injected production deadline is a timing bound in disguise when the code runs a REAL
subprocess under it. `TestRepositoryFetchMayOutliveRemoteIdentityLookup` handed production a 250 ms
lookup budget and then let it run a real `rev-parse` plus a real `ls-remote` inside that budget —
two process startups that fit on a quiet host and not on a loaded one. Observe the budget on the
injected runner's contexts instead (`fetchDeadline − lookupDeadline ≥ fetchTimeout − lookupTimeout`
is monotonic arithmetic that holds under any load), keep the real Git transfer for what it proves
(the object arrived), and model slow startup with a PATH shim (`sleep 0.3; exec git`) to show the
old test fails and the new one does not (`internal/sessionsvc/source_test.go`).

**Production liveness bounds meet the same load.** A production deadline that only guards a
wedged runtime is not a speed requirement, and a fake runtime that is a shell script makes a test
race it. Fork stop's exact-label reap had 3 s, and under make check a busy daemon (and the test's
shell runtime) missed it, so `coop fork stop` failed with "fix the container runtime". It is now
30 s, like the temp reap. Where the bound sits on a latency path, keep it and inject the test's
slack instead: `reapACPChildBoxes` takes its timeout, the provider switch passes 5 s, and its
test passes `wait.Deadline`.

**A terminal result is not a pending fixture.** Session tests use `sessionOperationReached` to
report an unexpected failed/uncertain operation with its id, error code and detail immediately.
The generous wait still guards reserved/running operations; extending it cannot repair a recorded
storage refusal. Ordinary workspace fixtures explicitly configure `Config.StorageLimits` through
`newSessionServiceWithTestStorage`: test-volume watermarks and a small reserve, but normal grace
and measurement intervals. The focused storage helper's nanosecond interval is not an ordinary
fixture default. Production limits and dedicated reserve/adoption tests stay independent.

**Two process listings are not one snapshot.** A fixture that starts a background sleep and then a
foreground sleep may add the latter between listings, hiding an excluded leader in the row count.
Publish one child's PID, keep the leader in builtin `wait`, and assert those known identities.
Do not compare dynamic status/elapsed columns or loosen production cleanup assertions.

**A child marker is not Docker inventory readiness.** Docker 28.0.4 starts the task before
setting Running and checkpointing its query snapshot (Moby `daemon/start.go:222-235`);
`docker ps` reads that snapshot. The signal-forwarding runtime test waits for both the child
marker and exactly one matching running container before canceling. Query errors, duplicate
matches and premature exit still fail; TERM receipt and zero containers after return stay
strict. Its controlled initial-empty-inventory case fails the former one-shot assertion.

**A transient reader needs a barrier, not a longer poll.** The consult slow-drain fixture keeps
its peer waiting until the host has frozen a capture reader. Only then does the peer produce its
reply and publish a marker; the unchanged eight-second freeze starts after that marker. Without
this ordering, a small reply can drain before the first process listing. Failure cleanup kills
the exact group and joins the command's output writer before inspecting its capture.

**macOS's /dev/fd fails now and then under concurrent lookups.** stat(2) on `/dev/stdin` (devfs
`/dev/fd/0`) answers "Bad file descriptor" for an open descriptor every few thousand calls while
many processes resolve `/dev/fd` at once, as a full `go test ./...` makes them do (a CPU hog alone
does not). stat on the descriptor itself never goes through devfs: `stat -c %i -` (GNU) or
`stat -f %i` with no operand (BSD) reads stdin's inode, and `stat … <&8` another descriptor's.
The consult and delegate lost-EOF stubs find their FIFO that way.

**A persisted receipt is not a joined writer.** An asynchronous session operation records its
failure before writing its diagnostic. Observing that terminal receipt does not make an injected
`bytes.Buffer` safe to read or ensure the log exists yet. Stop and join the service before reading
the capture (`TestSessionServiceLogsSanitizedOperationFailureWithCorrelationID`); a buffer mutex
alone would still permit an empty-log assertion. Keep correlation and redaction assertions intact.

The lifecycle benchmark's timeout-cleanup fixtures publish readiness only after their PID,
sample label and printed network identity exist. Their injected launch (the bench's `start`) waits
for that marker before the unchanged 0.5 s case timeout starts. A controlled 0.75 s startup delay
reproduces the old empty-cleanup failure and passes with the handshake; production cleanup and
timeouts stay unchanged. A readiness failure still reaps the fixture process.

A filesystem readiness marker does not mean the host consumed queued terminal output. Drain
already-readable PTY bytes before sending terminal Ctrl-C: the signal flushes unread bytes, which
may contain the only printed network-run identity. `TestInterruptReadsWhatTheTerminalHoldsFirst`
proves it on a real terminal with no timing at all: the identity is written, the bench's side is
seen readable, Ctrl-C is typed, and the check waits for the terminal's `^C` echo, which follows its
flush. On macOS, whose Ctrl-C discards output the bench has not read, it fails every run without
the drain. Linux discards only output still on its way to the bench's side (by reading its tty
code, not a run), so there the test passes either way.

**Marker existence is not complete content.** Shell redirection creates a PGID marker before
`printf` writes the bytes. Write a private sibling and rename it only after completion, or have
the fixture write a ready marker after the content and wait for that marker (the Google token
helper in `internal/agent/usage_report_test.go`), so the fixture's existence waiter cannot read
an empty identity. The controlled ACP marker test holds `printf` before its write; the final
marker must remain absent until release, then contain the exact PGID. Report joined helper output
on failure and reap its resident group on every assertion path. The editor-stdin and
stable-leader assertions remain unchanged.

## Changelog
- 2026-10-17 — macOS's /dev/fd answers stat with EBADF now and then under concurrent lookups
  (reproduced outside coop: 4 to 6 in 9,600 stats from 32 shells); the consult and delegate lost-EOF
  stubs stat their stdin descriptor directly instead (task
  2026-10-17-make-the-consult-wrapper-capture-tests-hold-unde).
- 2026-10-17 — the Google token cancellation test read its helper's pid file empty in 17 of 200
  runs: `wait.ForFile` saw the file the redirection had just created. The helper now writes a
  ready marker after the pid and the test waits for it; 200 plain and 50 race runs green, with the
  cancellation and reap assertions unchanged (task 2026-10-17-fix-testreportgoogletokencancellationreapshelper).
- 2026-10-16 — the lifecycle bench and its fixtures are Go (tools/lifecyclebench); the readiness
  handshake is the bench's start seam, and the queued-terminal-output check uses a real PTY and the
  `^C` echo instead of a patched select (task 2026-10-15-port-the-lifecycle-and-services-benchmarks-to-go).
- 2026-10-15 — dropped internal/acpctl/process_live_test.go from sources (the live process registry was removed).
- 2026-10-14 — runtime audit replaced ambient skip/path failure/invalid-request controls with
  hermetic selection, real clock health and valid lease/foreign-identity positives. TERM children
  publish readiness after their trap; kernel wake/lock fixtures join before teardown. Native
  offline probes freeze daemon/image identity and inherit cancellation; sibling curl terminal
  frames separate transport errors from denied access, with host liveness rechecked afterwards.
- 2026-10-11 — tagged ACP stdin checks exposed an empty PGID: fixture redirection created the
  readiness marker before writing its bytes. Child diagnostics confirmed two failures in 100
  runs; a controlled paused-write regression failed old ordering. Publish the complete marker
  by same-directory rename, join/reap the resident helper even on an assertion failure, and
  retain exact stdin, group, liveness and refusal assertions without longer timing bounds.
- 2026-10-11 — full gate exposed a cancellation benchmark false success after Ctrl-C flushed its
  unread identity. Controlled scheduling failed before pre-cancel nonblocking drain; kept signal,
  timing and ownership assertions unchanged. This is benchmark capture, not product teardown.
- 2026-10-10 — the canonical gate's whole session-service race package reached 20 m with its
  active test only 1 s old. That exact test passed alone in 4.784 s; the unchanged complete race
  package then passed in 1220.664 s (399 serial tests). Raised only the race aggregate guard to 30 m,
  roughly 50% contention headroom. Plain 20 m, -race, -p 4, operation deadlines and all
  assertions remain unchanged. Swept both recipes and the provider-scripted guard; no other
  bound needed a change. Successful unchanged-package cache reuse is not an uncached timing run.
- 2026-10-09 — race CI missed a consult reader after its immediate peer had already exited.
  Added a release/produced handshake and joined cleanup, retaining the eight-second slow-drain
  assertion and unchanged production budget.
- 2026-10-09 — release CI observed child readiness then zero running matches. Confirmed the
  invalid ordering assumption against the exact runner's Docker 28.0.4 source and reproduced
  it by withholding the first inventory response. Added bounded readiness, retaining signal
  and cleanup assertions; no production code or runtime deadline changed.
- 2026-10-08 — reproduced the lifecycle-tool cleanup assertion failing with a controlled 0.75 s
  child-start delay before its injected 0.5 s timeout. Synchronized readiness in the two shared
  timeout-cleanup cases and retained exact owned-resource/PID assertions; delayed-start regression
  and owning tool tests qualify the fix (task 2026-10-08-investigate-lifecycle-bench-timeout-cleanup-fixt).
- 2026-10-04 — four load flakes from the 2026-10-02/04 gates, classified and fixed (task
  2026-10-02-stop-three-tests-timing-out-under-full-gate-load). forkctl and cli fork stop: the
  production reap's 3 s is a wedged-runtime guard, now 30 s. The ACP child reap test: the 5 s
  stays on the switch path, and the test injects wait.Deadline. The scripted ACP rate-limit
  case: its 10 s whole-fixture context is a fixture guard, now wait.Deadline.
- 2026-09-30 — ACP hang cancellation now arms on a logged prompt, not the log's
  creation before initialization. Controlled interrupted-write and delayed-reader
  regressions both failed old production: delivered prompts missed cancellation,
  and a fixed 10 ms shutdown sleep lost queued notifications. The runner now sends
  bounded cancellation on both paths and reuses bounded EOF drain before signalling;
  larger restricted/filtered grace is preserved. Fixture goroutines are joined.
- 2026-09-30 — profiled 379 serial `internal/sessionsvc` tests: 516.818 s alone, with the
  slowest test at 23.44 s and the 14 tests over five seconds totaling 157.2 s. Three other
  green plain runs took 507–523 s; one loaded gate hit Go's default 600 s package timeout
  without a failed assertion. Swept the plain and race recipes, then aligned the plain
  whole-package guard with race at 20 minutes; test and production operation deadlines remain
  unchanged. The full plain target passed under two added CPU loads in 712 s, with sessionsvc
  at 533.460 s. Broad fixture sharing or parallelization would risk isolation for unmeasured gain.
- 2026-09-27 — the 360-test session-service package passed plain tests in 517 seconds but hit Go's default ten-minute package timeout under race instrumentation. The active subtest had run only one second and its entire family passed separately under race in 14 seconds; an earlier gate expired in a different test. Set the race target's whole-package guard to 20 minutes without changing any operation deadline or behavioral assertion. Swept both ordinary and race recipes; package bounds are distinct from fixture waits.
- 2026-09-25 — verified the large ACP image-turn fixture guards against this rule: three 15-second guards expired only under the two-core Linux parallel race gate; all three tests passed three isolated race runs in 6–8 seconds. Reused the shared 60-second guard for all five large-image turn paths, leaving artifact assertions and production timeouts intact.
- 2026-09-22 — the canonical race gate caught a log-buffer read before its background writer
  completed. Joined the service before inspection; swept both session-service logger captures,
  leaving the synchronous HTTP request fixture unchanged.
- 2026-09-22 — reproduced the process-group exclusion fixture race in the canonical race gate
  and focused repetitions; synchronized one child and asserted exact leader/child identities.
- 2026-09-13 — reproduced an ordinary session fixture refusing allocation under the host's 24.7 GB
  production reserve. Added explicit test storage configuration and terminal-operation diagnostics;
  preserved production-default and reserve/refusal/recovery coverage instead of changing host policy
  or waiting for impossible success.
- 2026-09-10 — added the injected-deadline shape after the fetch-may-outlive-lookup regression blew
  its 250 ms budget in a loaded gate; now observed on the seam, fails instantly against a shared
  deadline and two near-misses, 20 focused runs and 3 package runs green (task
  make-repository-fetch-timeout-regression-independent).
- 2026-09-10 — added the deferred-trap shape after `TestForkStopReapsBoxAfterWorkerExit` failed
  once in a loaded gate (worker gone at 3.23 s, no acknowledgement); modelled deterministically
  with a child that ignores TERM (`signal: killed` at 3.25 s), fixed by backgrounding the sleep,
  20 race repetitions green (task make-fork-stop-term-acknowledgement-deterministic).
- 2026-09-06 — created with `internal/testutil/wait` (task make-the-wall-clock-test-deadlines-load-aware-or) after the release-audit requalification separated the two kinds.
