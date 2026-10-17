---
name: ci-free-checks
description: Make-owned CI partitions, per-owner caches and a status-only required check
subsystem: development
sources: [Makefile, .github/workflows/ci.yml, .github/workflows/release.yml, release_test.go]
updated: 2026-10-17
---

CI runs each Make owner in the Makefile's `CI_OWNERS` on its own runner: static, unit (with only
`TEST_HOST_PROGRAMS` and go on PATH, `make test-minimal-path`, so a test that needs jq, node,
shellcheck or any other program must skip and name it), the scripted provider suite as two shards (loop tests and the rest, with process control), and the
race suite as three (every package but `internal/sessionsvc`, then that package in two shards).
A shard pair's `-run` and `-skip` share one pattern, so no test falls outside both; a release
test keeps each side non-empty. Ordinary unit and race package batches stay capped at four.
Local `make check` runs every owner but `check-unit` ([[local-development-checks]]).
Docker doctor and review-writes remain unconditional runtime jobs. No live provider calls.

Each main push, pull request and release call runs every job; none is path-filtered or routed.
The literal required `check` is a status-only `always()` job whose shell compares
`join(needs.*.result, ' ')` with one `success` per needed job, so a failed, cancelled, skipped
or missing job fails it, and adding a job without updating that string fails closed. Release
tests run the actual YAML step against each of those states.

Read-only CI caches modules and compiled objects, not behavioral results (`-count=1`). Each free
owner restores its own cache, keyed on reviewed Go inputs and the commit, from the newest main
push; only a main push saves one, after its checks pass. One shared setup-go key had let the first
owner to finish decide what every other restored, so the race owners rebuilt everything. Release
reusable calls run the same jobs; the signing publisher remains uncached and fresh, importing no
CI artifacts. Local tests prove workflow shape and refusal behavior, not hosted CI, signing or
publication. Related: [[local-development-checks]], [[release-qualification]].

## Changelog
- 2026-10-17 — `check-unit` runs the unit tests with only the required host programs on PATH and
  CGO_ENABLED=0, the release's build (`make test-minimal-path`); the race owners keep the full
  PATH, so optional-program tests still run there. The owner kept CI's plain pass for testing the
  build users run (decision on 2026-10-15-stop-running-every-unit-test-twice-in-make-check).
  Inventory and lists: CONTRIBUTING.md, the Makefile's `TEST_HOST_PROGRAMS`.
- 2026-10-15 — deleted the CHANGELOG-only path (scope job, tools/ci_scope.py, metadata job,
  `make check-metadata`): it never fired, since a release pushes its finalize and reopen commits
  together, and CI is free. The Python aggregate became shell over needs.*.result. The Make
  recipe tests went too; their static facts (owner union, no race constraint, shard sides) moved
  to release_test.go.
- 2026-10-15 — race and process owners sharded (CI_OWNERS), per-owner caches. The 13-minute race
  job was CI's critical path: sessionsvc alone ran 557 s, starting 3.5 minutes in. Release tests
  read the matrix from CI_OWNERS and hold each owner's cache steps; Make tests hold shard sides.
- 2026-10-14 — created from the approved setup simplification; verified Make owners, raw-Git
  routing, actual aggregate execution and publisher isolation against the listed sources.
