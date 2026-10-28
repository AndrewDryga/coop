---
name: quality-tool-reviews
description: measured complexity, duplication and mutation findings need coverage and ownership context
subsystem: development
sources: [internal/loop/loop.go, internal/loop/review.go, internal/loop/telemetry.go, internal/tasks/registry_file.go, internal/ladder/limit.go, internal/ladder/limit_test.go, internal/ladder/ladder_test.go, internal/agent/credential_file.go, internal/box/filtered_mounts.go, internal/testutil/nativedocker/fixture.go, internal/ui/tty_darwin.go, internal/ui/tty_linux.go]
updated: 2026-10-28
---

The October 2026 Crapper/Dryer/Mutator trial produced useful review inputs, not a new gate.
Trial revisions were Crapper `6fd92c4`, Dryer `66ff6d2` and Mutator `a5a0cb8`.
Its full evidence lives in the local archived task `2026-10-28-trial-crapper-dryer-and-mutator-on-coop`.
Use [[design-for-real-requirements]] when deciding what to change.

Go coverage from ordinary package tests misses separately built subprocesses and some dependent
package execution. Missing coverage does not establish dead code. Crapper also scans inactive
platform files unless given native `go list` inputs; keep source and coverage scopes explicit.
Its pinned grammar did not parse Go `new(value)` initializers in the docs generator, so check
parser diagnostics before relying on a report. Failed coverage tests remain failures even when
they produce useful counters.

Mutator's trial revision could not match Go import-path profile keys to absolute source paths:
it skipped every mutation and exited zero until the module prefix was normalized. Preserve raw
coverage and statement/hit identity when adapting paths. Inspect test diagnostics: compilation
failures and timeouts count as kills upstream. Include relevant consumer tests; a package-only
survivor can already be guarded by a caller. Equivalent clamp mutations in `LimitWait` cannot
change observable behavior; realistic retry timing and classification mutants can.

Dryer normalizes values and names, so exact structural matches include unrelated getters and
platform ioctl implementations. The native-Docker fixture parser intentionally interprets emitted
flags independently of production `networkMountPlan`; sharing those would weaken the test oracle.
Credential renewal wrappers already share deadline/lock policy in `lockCredentialRefresh` and keep
provider ownership; extracting the remaining wrappers adds callback/sentinel plumbing without
fixing demonstrated drift.

The adopted boundaries instead share actual policy: `readForkRegistryFile` owns bounded,
no-follow, single-link bytes while callers retain typed validation; `readRunRecords` owns the
telemetry readers' best-effort recovery and caps; `resolveReviewRotations` owns ordered fallback
construction. Inherited review stages share rotation pointers, including cooldown and advancement.
Current and resumed-cohort ladders are built before the loop's frozen network admission and move
together only at the existing cohort handoff. A lower complexity number alone is not a redesign.

## Changelog
- 2026-10-28 — recorded the measured tool limitations and deliberate retain/share decisions;
  adopted real rate-limit regressions and the three shared policy boundaries, preserving
  independent oracles, provider ownership, frozen admission and inherited rotation state.
