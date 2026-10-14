---
name: local-development-checks
description: explicit local check scopes, reverse-consumer guard and conservative broad fallback
subsystem: development
sources: [Makefile, internal/local_checks_test.go, tools/test_make_checks.py, AGENTS.md, CONTRIBUTING.md, .agent/project.yaml, internal/testutil/procharness/binaries.go, internal/testutil/procharness/stdio.go, internal/testutil/procharness/capture.go, internal/testutil/gitrepo/committed.go]
updated: 2026-10-14
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

CI runs the four Make-owned broad partitions on isolated runners; `make check` remains their
union. Module/build caches reuse compilation, not behavioral test results: child-built or read
inputs can be invisible to Go's result cache, so unit, race and scoped tests use `-count=1`.
The narrow CI metadata path is separate from local scopes; see [[ci-free-checks]].

Scripted CLI/ACP fixture builds reuse immutable bytes only within one package test process.
The TestMain owns a private cache root and freezes the Go executable/environment before cases
change PATH/HOME. Canonical source root, package and normalized build tags distinguish entries;
ordinary and cooplivetest builds never alias. Every case gets a new writable executable inode.
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
- 2026-10-14 — verified the broad Make union, CI partitions and uncached behavioral test results;
  real Make fixtures cover every partition, independent tool calls and failure propagation.
- 2026-10-14 — added explicit scopes and import/fixture ownership guard. Real Make fixtures cover
  each accepted scope, invalid broad fallback, owner errors and formatter failure/output.
  Canonical/embedded workflow skills remain byte-identical and generic for other projects.
- 2026-10-14 — package-owned binary/seed reuse and abort-safe stdio/capture/lease helpers.
  Queued-frame, missing-close, missing-cleanup, output-limit and rune mutations fail the keepers.
  Production signal grace is unchanged; one real tool escalation owner remains.
