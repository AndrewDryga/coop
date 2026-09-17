---
name: doctor-report-accounting
description: how `coop doctor` counts — the 35 checks, the outcomes a row can have, and why a failed probe adds one failure plus the checks it was carrying
subsystem: doctor
sources: [internal/cli/doctor.go, internal/cli/doctor_report.go, internal/cli/doctor_checks.go]
updated: 2026-09-17
---

`coop doctor` is the one command whose point IS the ledger: the person asked coop to perform
checks, so every check it performed is printed. (A passive inspection elsewhere shows facts, not a
list of successful lookups.)

The inventory is a table, not prose: `doctorSecretChecks`, `doctorHostChecks`,
`doctorCredentialChecks` and `doctorCloneChecks` in doctor_checks.go carry each check's stable id
and its two labels (held / did not hold). The probes emit `RESULT PASS|FAIL <id>`; the wording
lives only in the table, so a reworded label cannot change what was measured. 35 ordinary checks
on a real image, a Docker runtime and a readable process cap.

Seven row outcomes (doctor_report.go), because the easy version lies:
- `doctorPass` / `doctorFail` — it ran.
- `doctorSkip` — deliberately not checked HERE (Alpine fallback, an unreadable limit).
- `doctorNotApplied` / `doctorDisabled` — this runtime does not apply the limit / somebody
  switched it off. Different statements, different verdict clauses.
- `doctorProbeFail` — the probe died: ONE failure plus `covers` checks counted as uncompleted, so
  the totals still add to 35 instead of shrinking into something that reads clean.
- `doctorUnrun` — checks a failure elsewhere prevented; neither performed nor skipped.
- `doctorNote` — the abandoned-box survey, outside the tally entirely (host hygiene, not a hole in
  the isolation doctor attacks).

`doctorSections` opens the six sections in print order, and both `cmdDoctor` and the approved-report
fixtures go through it — a check cannot land in one section for a real run and another for the
transcript that is supposed to prove it. Rendering is a pure function of the measurements, which is
why `internal/cli/testdata/approved/18*.txt` pins every report without a container runtime.

A fallback run that performed what it could still exits 0; it just never claims full isolation.

## Changelog
- 2026-09-17 — Podman removed as a runtime; the hardened-runtime count is Docker's.
- 2026-09-11: created with the check tables, the row outcomes and the counting rule. Verified
  against doctor.go, doctor_report.go and the approved 18a–18k fixtures.
