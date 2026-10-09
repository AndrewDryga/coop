---
name: doctor-report-accounting
description: how `coop doctor` counts — the 37 checks, the outcomes a row can have, and why a failed probe adds one failure plus the checks it was carrying
subsystem: doctor
sources: [internal/cli/doctor.go, internal/cli/doctor_report.go, internal/cli/doctor_checks.go]
updated: 2026-10-09
---

`coop doctor` is the one command whose point IS the ledger: the person asked coop to perform
checks, so every check it performed is printed. (A passive inspection elsewhere shows facts, not a
list of successful lookups.)

The inventory is a table, not prose: `doctorSecretChecks`, `doctorHostChecks`,
`doctorCredentialChecks` and `doctorCloneChecks` in doctor_checks.go carry each check's stable id
and its two labels (held / did not hold). The probes emit `RESULT PASS|FAIL <id>`; the wording
lives only in the table, so a reworded label cannot change what was measured. 37 ordinary checks
on a real image, a Docker runtime and a readable process cap.

Seven row outcomes (doctor_report.go), because the easy version lies:
- `doctorPass` / `doctorFail` — it ran.
- `doctorSkip` — deliberately not checked HERE (Alpine fallback, an unreadable limit).
- `doctorNotApplied` / `doctorDisabled` — this runtime does not apply the limit / somebody
  switched it off. Different statements, different verdict clauses.
- `doctorProbeFail` — the probe died: ONE failure plus `covers` checks counted as uncompleted, so
  the totals still add to 37 instead of shrinking into something that reads clean.
- `doctorUnrun` — checks a failure elsewhere prevented; neither performed nor skipped.
- `doctorNote` — the abandoned-box survey, outside the tally entirely (host hygiene, not a hole in
  the isolation doctor attacks).

`doctorSections` opens the six sections in print order, and both `cmdDoctor` and the approved-report
fixtures go through it — a check cannot land in one section for a real run and another for the
transcript that is supposed to prove it. Rendering is a pure function of the measurements, which is
why `internal/cli/testdata/approved/18*.txt` pins every report without a container runtime.

A fallback run that performed what it could still exits 0; it just never claims full isolation.

The synthetic project in `buildFixture` is mounted for a probe that may run as a different UID.
It explicitly sets root/subdirectory traversal and seeded-file read modes after creation;
`MkdirAll` and `WriteFile` alone inherit the caller's umask and can leave a private-umask
fixture unreadable inside the box.

Credential fixtures start owner-private (0700 directories, 0600 files); the selected provider has
valid inert OAuth and identity input with distant expiry, imported through fresh host sign-in
before launch. Ordinary launch projects only a public broker selector. The in-box probe requires readable native
state, rejects host canary bytes (read errors fail closed), and checks peer directories are absent.
Offline runs deliberately have no broker selector. On Linux, the Alpine
fallback credential probe runs as the fixture owner's UID/GID: root without DAC_OVERRIDE cannot
traverse a foreign-owned private bind. A real image keeps its configured USER so doctor still
detects incompatible image ownership. Docker Desktop has a different host UID mapping.

## Changelog
- 2026-10-09 — reproduced stale legacy fixtures failing native custody checks. Updated fixture,
  broker/grant and Grok isolation checks, offline behavior and approved report totals; retained
  real-image USER and added independent fixture/probe refusal regressions.
- 2026-09-30 — exact-source release CI exposed the Alpine fallback's Linux owner mismatch;
  scoped probe argv and its regression preserve real-image USER and private credential modes.
- 2026-09-25 — a private-umask Ubuntu gate exposed doctor fixture files/subdirectories masked
  to owner-only despite a world-traversable root. Explicit fixture modes and nested-directory
  regression restore a readable non-owner probe without changing report semantics.
- 2026-09-17 — Podman removed as a runtime; the hardened-runtime count is Docker's.
- 2026-09-11: created with the check tables, the row outcomes and the counting rule. Verified
  against doctor.go, doctor_report.go and the approved 18a–18k fixtures.
