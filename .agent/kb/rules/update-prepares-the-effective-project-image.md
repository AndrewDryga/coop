---
name: update-prepares-the-effective-project-image
description: updates use installed definitions and effective project mode; missing build readiness comes before slow launch setup
scope: box
sources: [internal/cli/build_cmd.go, internal/cli/selfupdate.go, internal/cli/project_build.go, internal/box/project_preparation.go, internal/box/network_admission.go]
check: "go test ./internal/cli -run 'TestUpdateHandsOffToTheVerifiedInstalledExecutable|TestUpdateHandoffFailureNeverBuildsWithOldDefinitions|TestUpdateActuallyExecutesTheInstalledRelease|TestUpdateBoxUsesTheEffectiveNetworkMode|TestUpdateCheckDoesNotReportOrdinaryImageForFilteredProject'"
updated: 2026-10-11
---

# Prepare what the next project launch will use

A successful update prepares the effective project's image using the installed binary's
definitions. Do not build an unused ordinary image for a filtered project, or build with
pre-update embedded definitions after replacing the executable. Preserve a truthful partial
failure when the binary is installed but image preparation fails.

Check project build readiness before costly network qualification and mount/secret scanning.
When eligible, offer the explicit host next action under [[restricted-project-builds-are-explicit]];
never turn a readiness miss into automatic ordinary-network execution.

**Why:** the owner reported a seven-minute successful update followed by network qualification
and secret scanning, only to be told to run another build: "why ... have user do obvious next step?"

## Changelog
- 2026-10-11 — swept build/update, launch admission and dry-run status. Fixed ordinary/filtered
  mismatch, old-process rebuilding, late readiness and misleading filtered dry-run metadata;
  the check includes a real disposable executable handoff; the canonical gate also covers
  pre-qualification refusal.
