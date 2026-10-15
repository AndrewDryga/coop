---
name: gate-is-one-recipe
description: "Make owns the broad check union and its CI partitions, never a second workflow check list"
scope: agent-workflow
sources: [Makefile, .github/workflows/ci.yml, release_test.go]
check: "go test ./. -run 'TestReleaseModuleHygiene|TestReleaseLocalGateRunsEveryCIOwner|TestReleaseShardPatternsSplitTheirSuites|TestReleaseWorkflowQualification|TestReleaseCIAggregateRejectsFalseGreenDependencyStates'"
updated: 2026-10-16
---

# There is ONE gate recipe: `make check`

Add a check to a broad owner of `check` in the `Makefile` — never as a separate step in
`.github/workflows/ci.yml`. CI runs each owner in the Makefile's `CI_OWNERS` on an isolated
runner; the workflow's matrix is that list, which the release test holds. Local `make check` runs
the same owners but `check-unit`, the static one first and the test owners side by side: the race
owners run every test `check-unit` runs, under the race detector. The literal required `check`
status runs after failures too and refuses a failed, cancelled or skipped check or runtime job.
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
- New check → a Makefile target, added to one of `check`'s owners. Cheap and common failures belong
  to the static owner, which runs first; a new test owner joins `CI_OWNERS` and `GATE_TEST_OWNERS`.
- A slow package or suite becomes two shards with one pattern, `-run` in one and `-skip` in the
  other, so no test can fall outside both; a release test keeps each side non-empty.
- `make check` may leave out `check-unit` only while no Go file has a race build constraint
  (a release test holds that). Never drop a check from CI to speed up the local gate.
- Needs a container runtime? That's the one exception: its own CI job, plus a Makefile comment on
  `check` saying it's CI-only and why — `make check` stays runtime-independent.
- Pin a tool's version ONCE, in the Makefile or its dedicated dependency graph, and have CI use
  the Makefile install target instead of repeating the literal. If compatibility requires a
  transitive dependency pin, verify that pin too; the analyzer's version alone is insufficient.
- A check that only ever ran locally has never been proven against a clean checkout: before wiring
  it into CI, run it on `git archive HEAD` output, where gitignored working state doesn't exist.

CI runs every owner on each main push, pull request and release; no path filter or changed-file
route narrows it. Never reuse low-privilege caches or artifacts in the signing publisher.
Related: [[ci-free-checks]], [[static-bounded-supervision]].

## Changelog
- 2026-10-16 — Coop's box images dropped their Go tools stage, so the analyzer module moved to
  `tools/staticcheck.mod`. Swept the pins: the Makefile owns both and CI still installs through
  `make install-staticcheck` and `make -s govulncheck-version`. One mirror remains:
  `.agent/Dockerfile` repeats the govulncheck version (a Dockerfile cannot read Make), and
  `TestOwnDockerfileInstallsTheGatePins` holds it and the module path equal to the Makefile's.
- 2026-10-15 — deleted the CHANGELOG-only metadata path (it never fired; CI is free) and the
  Make recipe tests, by the owner's decision. The required status is now shell over
  needs.*.result, and release_test.go holds the owner union, race-constraint and shard facts the
  Python tests held. Swept Make, ci.yml, release tests, CONTRIBUTING, AGENTS, README, the release
  skill and the CI, local and release KB cards; AGENTS, README and the skill named neither.
- 2026-10-15 — the race and process owners became shards (CI_OWNERS, seven runners) and each
  owner got its own CI cache: CI's race job took 13 minutes beside 5-7 for the rest. Local
  `make check` (52 minutes) runs the test owners four at a time and skips the plain unit pass
  the race owners repeat, in a build cache of its own. Swept Make, both workflows' tests,
  CONTRIBUTING and the CI, local and release KB cards.
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
