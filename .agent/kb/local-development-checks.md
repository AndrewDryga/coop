---
name: local-development-checks
description: explicit local check scopes, reverse-consumer guard and conservative broad fallback
subsystem: development
sources: [Makefile, internal/local_checks_test.go, release_test.go, runtime_selection_test.go, live_selection_test.go, AGENTS.md, CONTRIBUTING.md, .agent/project.yaml, internal/testutil/procharness/binaries.go, internal/testutil/procharness/stdio.go, internal/testutil/procharness/capture.go, internal/testutil/gitrepo/committed.go, .github/workflows/ci.yml]
updated: 2026-10-17
---

`make check-local SCOPE=<owner>` has explicit rules/docs and six Go scopes: agent, runtime,
sessions (session + sessionsvc), tasks (internal/tasks only), connector (workerconnector) and cli.
Go scopes include reviewed reverse consumers, capped unit tests and both-platform/all-tag vet.
Tagged live suites are compiled, not run. For the fastest edit loop, use the owning named test
with `go test ... -run ...`; reverse-consumer batches can still be substantial.

Missing/unknown/wildcard/multiword scopes take the full free `make check` gate. There is no
automatic changed-file selector. Shared foundations, build tags, modules, toolchains, harnesses
and workflows require broad proof. Fork landing retains `gate: make check`.

The independent structural guard reads all tagged/test/root/tool imports, attributes standalone
fixtures to their owner, and holds observed dynamic consumers: ACP builds the root program;
CLI's network fixture builds box/testdata/mcpprobe. Missing/deleted consumers/fixtures fail.
It validates reviewed Make constants; it never chooses checks at runtime. Keep those source
ownership facts current when a subprocess caller changes.

Docs/rules run actual docs, alignment, site and KB checks; they do not run Docker, race,
process benchmarks or live models. A local scope pass is not the completed broad candidate gate.

No scope and no `make check` owner needs a container runtime. CI's doctor job runs `make doctor`
and `make box-runtime-e2e`, its review-writes job `make review-writes-e2e`; the filtered-network
suite, `make network-runtime-e2e`, is never in `make check` and not yet in CI ([[restricted-networking]]).
Runtime targets pick tests by -run pattern, so `runtime_selection_test.go` (in `make check`) fails
on a boxruntimee2e, networkruntimee2e, reviewwritee2e or acpe2e test no target runs: none whose own
-tags build its file, that lists its package and whose pattern matches it. A subprocess helper
takes its parent's prefix. `live_selection_test.go` holds live-process-control the same way
([[provider-live-e2e]]).

CI runs the Make-owned broad owners (`CI_OWNERS`) on isolated runners ([[ci-free-checks]]).
`make check` runs the static owner, then the others but `check-unit` four at a time, each into
its own log under `.agent/gate/`, with a Go build cache of its own (another session once cleared
the shared one under a running gate). The race owners stand in for `check-unit` there: no Go file
has a race build constraint, and a release test holds that along with the owner lists.
Module/build caches reuse compilation, not behavioral test results: child-built or read inputs
can be invisible to Go's result cache, so unit, race and scoped tests use `-count=1`.

Scripted CLI/ACP fixture builds reuse immutable bytes only within one package test process.
The TestMain owns a private cache root and freezes the Go executable/environment before cases
change PATH/HOME. Canonical source root, package and normalized build tags distinguish entries;
builds with different tags never alias. Every case gets a new writable executable inode.
Do not change source/build inputs during m.Run or treat this as persistent cross-version caching.

ACP/task helpers own their stdout reader independently of Cmd.Wait; final buffered frames cannot
be lost when the child is reaped. Cleanup is registered before readiness and joins readers and
the one Wait receipt. Shared capture drains concurrently and restores streams on panic/Goexit,
but callers must still own process-global streams serially. This is not a production lock.

Review fixtures copy an in-memory empty Git seed into independent .git files; no mutable refs,
worktrees, databases, credentials or task authority are shared. Cold initialization/LFS/hydration
owners still build their real inputs. Pure log cap/rune tests avoid full review staging; the real
large-output pager, HTTP, session custody and failed-review owners remain.

## Changelog
- 2026-10-17 — runtime_selection_test.go holds every runtime-tagged test to a target's -run pattern.
  Its first run found three no target ran: two took an existing prefix, and the task-channel test,
  which fits none, got `make task-channel-e2e`. A target counts only for files its own -tags build
  (&&, && ! and || fixtures), and recipes read past blank and comment lines as GNU Make does.
- 2026-10-17 — says which runtime suites CI's jobs run and that `make network-runtime-e2e` runs only
  by hand; checked against .github/workflows/ci.yml and the Makefile.
- 2026-10-15 — the fixture-build cache note no longer names the retired cooplivetest tag.
- 2026-10-15 — deleted the Make recipe tests (tools/test_make_checks.py), which ran real Make with
  stub binaries; release_test.go now holds the owner lists, the race-constraint fact and shard
  sides, and the CI metadata path is gone.
- 2026-10-15 — `make check` runs its test owners side by side without the plain unit pass, in its
  own build cache: the 52-minute local gate spent a quarter of its time repeating the race
  owners' tests without -race. Real Make fixtures cover the order-free union and failure.
- 2026-10-14 — verified the broad Make union, CI partitions and uncached behavioral test results;
  real Make fixtures cover every partition, independent tool calls and failure propagation.
- 2026-10-14 — added explicit scopes and import/fixture ownership guard. Real Make fixtures cover
  each accepted scope, invalid broad fallback, owner errors and formatter failure/output.
  Canonical/embedded workflow skills remain byte-identical and generic for other projects.
- 2026-10-14 — package-owned binary/seed reuse and abort-safe stdio/capture/lease helpers.
  Queued-frame, missing-close, missing-cleanup, output-limit and rune mutations fail the keepers.
  Production signal grace is unchanged; one real tool escalation owner remains.
