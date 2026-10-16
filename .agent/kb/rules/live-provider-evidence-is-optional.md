---
name: live-provider-evidence-is-optional
description: live provider suites run on demand; no gate, release or committed report depends on them
scope: agent-workflow
sources: [Makefile, .github/workflows/ci.yml, .github/workflows/release.yml, release_test.go, live_selection_test.go, .agent/skills/release/SKILL.md, CONTRIBUTING.md]
check: "go test ./. -run 'TestReleaseWorkflowQualification|TestTaggedTestsRunUnderLiveProcessControl'"
updated: 2026-10-16
---

# Live-provider evidence is optional, not release permission

Ordinary checks and releases make no model calls and never depend on a live-provider run, a
saved report, a waiver or a release-number allowlist. The live suites stay explicit, on-demand
Make targets bounded by their stated scope and billing. A missing live run stays visible as
unverified; it never becomes a pass, a waiver request or invented compatibility proof. Don't
commit a live-provider report or gate anything on one. Keep locked-client integrity tests. This
does not relax local network, kernel, identity or credential admission.

**Why:** the user rejected repeated per-version waivers and unnecessary provider spending
(2026-10-14), and the owner then retired the saved report (2026-10-15): recorded once, it went
stale with the next client pin and proved nothing about the clients a release ships.

**How to apply:** when a change needs live proof, run the smallest affected suite on demand and
report a run you could not make. The check holds the hosted boundary: CI and the release keep
their fixed free jobs (the release is only CI and its publisher), and no job inherits secrets; and
`live-process-control`, which runs inside `make check`, selects no `…LiveCompatibility` or
`…LiveChild` test. Review the rest: no other gate target runs a live suite, no untagged test needs a
provider, no workflow step reads a provider secret, and nothing reads a committed report.

## Changelog
- 2026-10-16 — `TestTaggedTestsRunUnderLiveProcessControl` now also fails when a
  `live-process-control` prefix selects a `…LiveCompatibility`/`…LiveChild` test (the target runs in
  `make check`); the check runs it beside the release workflow test.
- 2026-10-16 — deleted the saved report, its validator, `tools/qualify` and the
  `provider-status`, `provider-verify` and `provider-qualify` targets; the check moved from the
  deleted report test to the release workflow test. Swept the Makefile gate owners, both
  workflows, the release skill, CONTRIBUTING and the KB: 0 violations.
- 2026-10-14 — user rejected repeated per-version waivers and unnecessary provider spending.
  Swept the record validator, recorder, default Make gate, release job, release skill and both
  qualification KB cards. Removed the version allowlist and stored-record admission test;
  retained strict synthetic validation and unchanged historical evidence.
