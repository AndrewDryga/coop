---
name: gate-is-one-recipe
description: "a new check goes in `make check`; CI installs the tools and calls that target, never its own step list"
scope: agent-workflow
sources: [Makefile, .github/workflows/ci.yml]
check: "none"
updated: 2026-08-09
---

# There is ONE gate recipe: `make check`

Add a check to the `check` target in the `Makefile` — never as a step in `.github/workflows/ci.yml`.
CI's check job is setup plus `make check`, so what a laptop, a box, and CI run is the same list by
construction. A tool the gate needs is REQUIRED: missing, it fails with the one-line command that
installs it, and a version-pinned tool fails when the local build isn't the pin. Never soft-skip.

**Why:** the two were maintained as separate step lists and drifted in BOTH directions — the race
detector, `go build ./...`, ShellCheck and a pinned Staticcheck ran only in CI, while cast hygiene,
the rules-card validator, the maintenance-tool tests and the tagged process-control races ran only
on a laptop. A race regression could not fail locally; a malformed rules card could not fail CI. The
local Staticcheck also skipped itself when it wasn't installed — the check silently stopped existing
on the machine where the code was written, and main rotted red with no local gate able to see it.

**How to apply:**
- New check → a Makefile target, added to `check`'s prerequisites. Order it so cheap and common
  failures come first and the slowest step (`-race`) runs last.
- Needs a container runtime? That's the one exception: its own CI job, plus a Makefile comment on
  `check` saying it's CI-only and why — `make check` stays runtime-independent.
- Pin a tool's version ONCE, in the Makefile, and have CI read it (`make -s staticcheck-version`)
  instead of repeating the literal.
- A check that only ever ran locally has never been proven against a clean checkout: before wiring
  it into CI, run it on `git archive HEAD` output, where gitignored working state doesn't exist.

Related: [[static-bounded-supervision]].

## Changelog
- 2026-08-09 — created, from closing the divergence it describes (`make check` is now the union and
  CI's check job is `make check`). Swept both sides: 4 CI-only steps and 4 local-only targets were
  merged; the tracked-checkout sweep also caught `small-work-to-the-queue` naming a gitignored
  source, which would have turned CI red the moment `rules-check` ran there.
