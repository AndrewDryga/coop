---
name: ci-free-checks
description: Make-owned CI partitions, conservative CHANGELOG routing and status-only required check
subsystem: development
sources: [Makefile, .github/workflows/ci.yml, .github/workflows/release.yml, release_test.go, tools/ci_scope.py, tools/test_ci_scope.py, tools/test_make_checks.py]
updated: 2026-10-14
---

`make check` is the union of static, unit, scripted-process and race owners. CI runs those
Make targets on at most four isolated runners; ordinary unit and race package batches are capped
at four.
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

Read-only CI caches modules and compiled objects, not behavioral results (`-count=1`). Release
reusable calls stay broad; the signing publisher remains uncached and fresh, importing no CI artifacts.
Local fixtures prove routing and refusal behavior, not hosted CI, signing or publication.
Related: [[local-development-checks]], [[release-qualification]].

## Changelog
- 2026-10-14 — created from the approved setup simplification; verified Make owners, raw-Git
  routing, actual aggregate execution and publisher isolation against the listed sources.
