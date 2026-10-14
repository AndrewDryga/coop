---
name: live-provider-evidence-is-optional
description: saved live-provider reports never require a release waiver or implicit model spending
scope: agent-workflow
sources: [Makefile, internal/agent/qualification.go, internal/agent/qualification_gate_test.go, tools/qualify/main.go, tools/qualify/main_test.go, .github/workflows/release.yml, .agent/skills/release/SKILL.md]
check: go test ./tools/qualify -run TestSavedProviderReportIsOptionalAndNeverLoadsHostConfig
updated: 2026-10-14
---

# Live-provider evidence is optional, not release permission

Ordinary checks and releases make no model calls and never depend on a current saved
live-provider report or a release-number allowlist. Missing or stale evidence must remain
visible as unverified, not become a pass, a waiver request or invented compatibility proof.

Keep locked-client integrity and strict synthetic evidence tests. An explicit saved-report
verifier must refuse missing, malformed, incomplete or mismatched evidence without reading
host credentials or starting a runtime. Live tests stay explicit and bounded by their stated
scope/billing. This does not relax local network, kernel, identity or credential admission.

The mechanical check covers the optional report/strict refusal boundary. Default workflow
ownership and absence of model calls also require Make/workflow review.

## Changelog
- 2026-10-14 — user rejected repeated per-version waivers and unnecessary provider spending.
  Swept the record validator, recorder, default Make gate, release job, release skill and both
  qualification KB cards. Removed the version allowlist and stored-record admission test;
  retained strict synthetic validation and unchanged historical evidence.
