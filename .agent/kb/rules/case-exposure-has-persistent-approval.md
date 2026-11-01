---
name: case-exposure-has-persistent-approval
description: remember reviewed case-exposed secret paths through coop approve without granting future paths or policy writes
scope: security
sources: [internal/box/caseapproval.go, internal/box/mounts.go, internal/cli/net_approve.go, internal/cli/services_cmd.go, internal/runtime/docker_objects.go, internal/runtime/docker_lifecycle.go]
check: "go test ./internal/box -run 'TestCaseExposureApprovalPersistsOnlyReviewedPaths|TestCaseExposureApprovalIncludesServiceOnlyPaths'"
updated: 2026-11-01
---

# Remember reviewed case exposure through coop approve

A user who accepts secret-file exposure on a case-insensitive disk can save that decision
with `coop approve`. Bind it to the host-owned repository identity and exact reviewed paths;
new paths require another review. Keep canonical decoys. Do not grant alternate-spelling
writes to `.coopignore` through this secret-file approval. Show paths during review and a
short count during launch. Check both agent and service mount plans.

Docker creation and observation must support the mount plans case protection produces.
Bounds on inspection must not strand a successfully created ordinary box.

**Why:** the user reported Coop was unusable: repeating
`COOP_ALLOW_CASE_EXPOSED_SECRETS=1` was annoying, and the resulting launch failed inspection.
They explicitly requested removal of the feature or persistent approval via `coop approve`.

## Changelog
- 2026-11-01 — swept agent launch, service launch, approval UI, spelling budgets and Docker
  create/inspect limits. Added exact-path approval, case-only review, both launch checks, and
  aligned Docker bounds. The failed live container had 1,780 mounts and 772,918 bytes of mount
  JSON against a 256 KiB inspection limit. Tests cover persistence, new-path refusal,
  copied/replaced identity, cancellation, and successful 2,200-mount creation and inspection.
