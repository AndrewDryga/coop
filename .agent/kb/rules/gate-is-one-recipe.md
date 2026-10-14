---
name: gate-is-one-recipe
description: "Make owns the broad check union and its CI partitions, never a second workflow check list"
scope: agent-workflow
sources: [Makefile, .github/workflows/ci.yml, release_test.go, tools/test_make_checks.py, tools/ci_scope.py, tools/test_ci_scope.py]
check: "go test ./. -run 'TestReleaseModuleHygiene|TestReleaseWorkflowQualification|TestReleaseCIAggregateRejectsFalseGreenDependencyStates'"
updated: 2026-10-14
---

# There is ONE gate recipe: `make check`

Add a check to a broad partition of `check` in the `Makefile` — never as a separate step in
`.github/workflows/ci.yml`. CI runs `check-static`, `check-unit`, `check-process` and `check-race`
on isolated runners; local `make check` is their union. The literal required `check` status runs
after failures too and refuses incomplete selected checks or runtime jobs.
A tool the gate needs is REQUIRED: missing, it fails with the one-line command that
installs it, and a version-pinned tool fails when the local build isn't the pin. Never soft-skip.

Local iteration uses Make-owned explicit scopes, not a second check list in agent prompts.
`make check-local SCOPE=<owner>` checks actual docs/rules or an owner plus its reviewed reverse
consumers; missing, unknown, wildcard or multiword scope takes the broad gate. The independent
scope test reads tagged/test/root/tool imports and observed subprocess owners. New consumers
must expand the reviewed constants or use broad checks, never silently escape local proof.
Shared foundations, modules, toolchains, harnesses and workflows require broad proof.

**Why:** the two were maintained as separate step lists and drifted in BOTH directions — the race
detector, `go build ./...`, ShellCheck and a pinned Staticcheck ran only in CI, while cast hygiene,
the rules-card validator, the maintenance-tool tests and the tagged process-control races ran only
on a laptop. A race regression could not fail locally; a malformed rules card could not fail CI. The
local Staticcheck also skipped itself when it wasn't installed — the check silently stopped existing
on the machine where the code was written, and main rotted red with no local gate able to see it.

**How to apply:**
- New check → a Makefile target, added to one of `check`'s partitions. Order it so cheap and common
  failures come first and the slowest step (`-race`) runs last.
- Needs a container runtime? That's the one exception: its own CI job, plus a Makefile comment on
  `check` saying it's CI-only and why — `make check` stays runtime-independent.
- Pin a tool's version ONCE, in the Makefile or its dedicated dependency graph, and have CI use
  the Makefile install target instead of repeating the literal. If compatibility requires a
  transitive dependency pin, verify that pin too; the analyzer's version alone is insufficient.
- A check that only ever ran locally has never been proven against a clean checkout: before wiring
  it into CI, run it on `git archive HEAD` output, where gitignored working state doesn't exist.

Only a proven single regular CHANGELOG edit in an ordinary single-parent main push may use
the explicit metadata owner. PRs, tags, reusable calls, merges, multi-commit pushes and unknown
inputs remain broad. Report the narrower coverage honestly. Never reuse low-privilege caches
or artifacts in the signing publisher. Related: [[ci-free-checks]], [[static-bounded-supervision]].

## Changelog
- 2026-10-14 — swept Make, both workflows, contributor guide and development/release KB. Real
  Make execution proves partition union, required tool calls and failure propagation; actual
  workflow aggregate execution rejects failed/skipped/missing states. Git fixtures cover
  parent history, modes and caller contexts. Publisher isolation remains unchanged.
- 2026-10-14 — added scoped local iteration without changing the broad free gate. Swept Make,
  AGENTS, project gate, contributor guide and root/embedded workflow skills; preserved broad
  fork landing and actual required tools. Real Make fixtures prove docs/rules execute only
  their owners and invalid scopes take the full gate; an AST guard checks reverse consumers.
- 2026-10-09 — Go1.27.2 introduced V5 export data beyond Staticcheck0.8.1's published importer.
  Swept host/CI/base/filtered/project-image installs: one embedded alternate module pins the
  upstream-compatible importer, and the gate rejects the old same-version binary. No check skipped.
- 2026-08-09 — created, from closing the divergence it describes (`make check` is now the union and
  CI's check job is `make check`). Swept both sides: 4 CI-only steps and 4 local-only targets were
  merged; the tracked-checkout sweep also caught `small-work-to-the-queue` naming a gitignored
  source, which would have turned CI red the moment `rules-check` ran there.
- 2026-08-09 — validate-on-write backfill: independently re-verified. .github/workflows/ci.yml's
  `check` job is setup (checkout, setup-go, install the pinned staticcheck) then `run: make check`
  (line 34) — no separate step list. The two other jobs (`doctor` → `make box-runtime-e2e`,
  `review-writes` → `make review-writes-e2e`) are the rule's one documented exception
  (container-runtime-dependent), and Makefile:94 names both by hand right next to `check:`. 0
  violations.
- 2026-09-03 — routed the doctor job's duplicated build plus `./coop doctor` commands through the
  existing `make doctor` target. Re-verified that the Docker/Podman matrix, runtime environment,
  and separate init-process contract remain in CI; added a focused wiring regression.
