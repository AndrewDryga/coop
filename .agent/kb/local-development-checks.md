---
name: local-development-checks
description: explicit local check scopes, reverse-consumer guard and conservative broad fallback
subsystem: development
sources: [Makefile, internal/local_checks_test.go, tools/test_make_checks.py, AGENTS.md, CONTRIBUTING.md, .agent/project.yaml]
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

## Changelog
- 2026-10-14 — added explicit scopes and import/fixture ownership guard. Real Make fixtures cover
  each accepted scope, invalid broad fallback, owner errors and formatter failure/output.
  Canonical/embedded workflow skills remain byte-identical and generic for other projects.
