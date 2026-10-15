---
name: ci-free-checks
description: Make-owned CI partitions, conservative CHANGELOG routing and status-only required check
subsystem: development
sources: [Makefile, .github/workflows/ci.yml, .github/workflows/release.yml, release_test.go, tools/ci_scope.py, tools/test_ci_scope.py, tools/test_make_checks.py]
updated: 2026-10-15
---

CI runs each Make owner in the Makefile's `CI_OWNERS` on its own runner: static, unit, the
scripted provider suite as two shards (loop tests and the rest, with process control), and the
race suite as three (every package but `internal/sessionsvc`, then that package in two shards).
A shard pair's `-run` and `-skip` share one pattern, so no test falls outside both; the Make
tests keep each side non-empty. Ordinary unit and race package batches stay capped at four.
Local `make check` runs every owner but `check-unit` ([[local-development-checks]]).
Docker doctor and review-writes remain unconditional runtime jobs. No live provider calls.

Only an ordinary single-commit main push, called directly by ci.yml on main, may select
`check-metadata`. The actual HEAD must match the event SHA; actual commit headers must have
exactly the event's before SHA as sole parent. Reading commit headers rather than traversal
avoids forged `info/grafts` ancestry. The full raw NUL-delimited diff must be one changed regular
100644 `CHANGELOG.md` file, with differing complete nonzero blob IDs. Renames, modes, other paths,
merges, multi-commit pushes, shallow/missing objects, tags, PRs and unknown inputs go broad.
Git calls disable ambient configuration, replacement objects and external diff/textconv helpers.

The selector emits fixed mode/reason strings, never filenames or Git errors. Jobs are not
path-filtered. The literal required `check` is a status-only `always()` job: it accepts only
complete successful broad or metadata outcomes, with scope and both runtime jobs successful
and the unselected owner skipped. Metadata success explicitly says broad free checks did not run.
Tests execute the actual YAML aggregate against positive, missing, failed and skipped states.

Read-only CI caches modules and compiled objects, not behavioral results (`-count=1`). Each free
owner restores its own cache, keyed on reviewed Go inputs and the commit, from the newest main
push; only a main push saves one, after its checks pass. One shared setup-go key had let the first
owner to finish decide what every other restored, so the race owners rebuilt everything. Release
reusable calls stay broad; the signing publisher remains uncached and fresh, importing no CI artifacts.
Local fixtures prove routing and refusal behavior, not hosted CI, signing or publication.
Related: [[local-development-checks]], [[release-qualification]].

## Changelog
- 2026-10-15 — race and process owners sharded (CI_OWNERS), per-owner caches. The 13-minute race
  job was CI's critical path: sessionsvc alone ran 557 s, starting 3.5 minutes in. Release tests
  read the matrix from CI_OWNERS and hold each owner's cache steps; Make tests hold shard sides.
- 2026-10-14 — created from the approved setup simplification; verified Make owners, raw-Git
  routing, actual aggregate execution and publisher isolation against the listed sources.
